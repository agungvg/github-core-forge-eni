// gitutil.go — the git object layer, ported from gitutil.ts.
// No git binary, no library: SHA-1 object ids, loose-object hashing, packfile
// v2 read/write (whole objects; deltas resolved on read), pkt-line framing,
// tree/commit/tag build+parse, and reachability/ancestry over an in-memory
// object store.
//
// Differences from the TS original, all deliberate:
//   - inflateOne uses a counting io.ByteReader: Go's zlib reader consumes
//     exactly the true stream length (verified), so no echo re-deflate and
//     no binary search are needed. Same results, faster.
//   - writePack uses Go's compress/zlib (valid zlib stream; not byte-identical
//     to C zlib output, which no reader requires).

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const ZeroOID = "0000000000000000000000000000000000000000"
const oidLen = 40

type ObjectType string

const (
	ObjCommit ObjectType = "commit"
	ObjTree   ObjectType = "tree"
	ObjBlob   ObjectType = "blob"
	ObjTag    ObjectType = "tag"
)

type GitObject struct {
	Type ObjectType
	Data []byte
}

var oidRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func isOid(v string) bool { return oidRe.MatchString(v) }

// IsOidValue mirrors isOid for values of unknown JSON type.
func isOidValue(v any) bool {
	s, ok := v.(string)
	return ok && isOid(s)
}

// SHA-1 of `<type> <len>\0<data>` — the git object id.
func hashObject(typ ObjectType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------- pkt-line

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// One pkt-line carrying `text` (which normally ends with \n).
func pktLine(text string) []byte {
	payload := []byte(text)
	return pktData(payload)
}

// One pkt-line carrying raw bytes.
func pktData(payload []byte) []byte {
	out := make([]byte, 4, len(payload)+4)
	copy(out, fmt.Sprintf("%04x", len(payload)+4))
	return append(out, payload...)
}

// pktItem is one element of an encoded pkt-line stream.
type pktItem struct {
	kind int // 0 = string line, 1 = raw bytes, 2 = flush, 3 = delim
	s    string
	b    []byte
}

func pktStr(s string) pktItem { return pktItem{kind: 0, s: s} }
func pktRaw(b []byte) pktItem { return pktItem{kind: 1, b: b} }

var pktFlushItem = pktItem{kind: 2}
var pktDelimItem = pktItem{kind: 3}

// Encode a list of lines/buffers/flush/delim markers into one pkt-line stream.
func encodePktLines(items []pktItem) []byte {
	var out bytes.Buffer
	for _, it := range items {
		switch it.kind {
		case 2:
			out.Write(pktFlush)
		case 3:
			out.Write(pktDelim)
		case 0:
			out.Write(pktLine(it.s))
		default:
			out.Write(pktData(it.b))
		}
	}
	return out.Bytes()
}

type PktPacket struct {
	Kind    string // "data" | "flush" | "delim"
	Payload []byte
}

// Decode a whole pkt-line stream. Returns an error on any malformed framing.
func decodePktLines(buf []byte) ([]PktPacket, error) {
	packets, _, err := decodePktPrefix(buf)
	return packets, err
}

// Decode pkt-lines from the start of `buf` until the first flush packet
// (inclusive) and report how many bytes that prefix consumed. The bytes
// after the flush — a packfile, in receive-pack — are left untouched.
func decodePktPrefix(buf []byte) ([]PktPacket, int, error) {
	var packets []PktPacket
	pos := 0
	for pos < len(buf) {
		if pos+4 > len(buf) {
			return nil, 0, errors.New("pkt-line truncated header")
		}
		head := string(buf[pos : pos+4])
		if head == "0000" {
			packets = append(packets, PktPacket{Kind: "flush", Payload: []byte{}})
			return packets, pos + 4, nil
		}
		if head == "0001" {
			packets = append(packets, PktPacket{Kind: "delim", Payload: []byte{}})
			pos += 4
			continue
		}
		length, err := jsParseIntHex(head)
		if err != nil || length < 4 || pos+length > len(buf) {
			return nil, 0, fmt.Errorf("pkt-line bad length %s", jsonQuote(head))
		}
		packets = append(packets, PktPacket{Kind: "data", Payload: buf[pos+4 : pos+length]})
		pos += length
	}
	return packets, pos, nil
}

// Side-band-64k: wrap `payload` as band-1 pkt-lines, each <= 65520 bytes total.
func sideband(payload []byte) []byte {
	const chunk = 65515
	var out bytes.Buffer
	if len(payload) == 0 {
		out.Write(pktData([]byte{1}))
		return out.Bytes()
	}
	for off := 0; off < len(payload); off += chunk {
		end := off + chunk
		if end > len(payload) {
			end = len(payload)
		}
		framed := make([]byte, 0, 1+end-off)
		framed = append(framed, 1)
		framed = append(framed, payload[off:end]...)
		out.Write(pktData(framed))
	}
	return out.Bytes()
}

// ---------------------------------------------------------------- commits / trees / tags

type Identity struct {
	Name  string
	Email string
	TS    int64
}

type CommitInfo struct {
	Tree      string
	Parents   []string
	Author    Identity
	Committer Identity
	Message   string
}

var identityRe = regexp.MustCompile(`^(.*) <([^>]*)> (\d+) ([+-]\d{4})$`)

func parseIdentity(line string) Identity {
	m := identityRe.FindStringSubmatch(line)
	if m == nil {
		return Identity{Name: line, Email: "", TS: 0}
	}
	ts, _ := strconv.ParseInt(m[3], 10, 64)
	return Identity{Name: m[1], Email: m[2], TS: ts}
}

func formatIdentity(id Identity) string {
	return fmt.Sprintf("%s <%s> %d +0000", id.Name, id.Email, id.TS)
}

func parseCommit(data []byte) CommitInfo {
	text := string(data)
	split := strings.Index(text, "\n\n")
	head, message := text, ""
	if split >= 0 {
		head = text[:split]
		message = text[split+2:]
	}
	var tree string
	var parents []string
	author := Identity{}
	committer := Identity{}
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(line, " ") {
			continue // continuation of a multi-line header
		}
		if strings.HasPrefix(line, "tree ") {
			tree = strings.TrimSpace(line[5:])
		} else if strings.HasPrefix(line, "parent ") {
			parents = append(parents, strings.TrimSpace(line[7:]))
		} else if strings.HasPrefix(line, "author ") {
			author = parseIdentity(line[7:])
		} else if strings.HasPrefix(line, "committer ") {
			committer = parseIdentity(line[10:])
		}
	}
	return CommitInfo{Tree: tree, Parents: parents, Author: author, Committer: committer, Message: message}
}

// Hot-path commit parse: only the tree and parents, no identity regexes and
// no message handling. Byte-identical results for `tree`/`parents`.
func parseCommitLight(data []byte) (tree string, parents []string) {
	text := string(data)
	split := strings.Index(text, "\n\n")
	head := text
	if split >= 0 {
		head = text[:split]
	}
	start := 0
	for {
		nl := strings.Index(head[start:], "\n")
		var line string
		if nl < 0 {
			line = head[start:]
		} else {
			line = head[start : start+nl]
		}
		if len(line) > 0 && line[0] != 0x20 {
			if strings.HasPrefix(line, "tree ") {
				tree = strings.TrimSpace(line[5:])
			} else if strings.HasPrefix(line, "parent ") {
				parents = append(parents, strings.TrimSpace(line[7:]))
			}
		}
		if nl < 0 {
			break
		}
		start += nl + 1
	}
	return tree, parents
}

func buildCommit(c CommitInfo) []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, "tree %s\n", c.Tree)
	for _, p := range c.Parents {
		fmt.Fprintf(&sb, "parent %s\n", p)
	}
	fmt.Fprintf(&sb, "author %s\n", formatIdentity(c.Author))
	fmt.Fprintf(&sb, "committer %s\n", formatIdentity(c.Committer))
	sb.WriteString("\n")
	sb.WriteString(c.Message)
	return []byte(sb.String())
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func parseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	pos := 0
	for pos < len(data) {
		space := bytes.IndexByte(data[pos:], 0x20)
		if space < 0 {
			return nil, errors.New("tree entry has no mode")
		}
		space += pos
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			return nil, errors.New("tree entry truncated")
		}
		nul += space + 1
		if nul+21 > len(data) {
			return nil, errors.New("tree entry truncated")
		}
		mode := string(data[pos:space])
		name := string(data[space+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		pos = nul + 21
	}
	return entries, nil
}

// Git tree ordering: names sort as if directories carried a trailing '/'.
func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		an := sorted[i].Name
		if sorted[i].Mode == "40000" {
			an += "/"
		}
		bn := sorted[j].Name
		if sorted[j].Mode == "40000" {
			bn += "/"
		}
		return an < bn
	})
	var out bytes.Buffer
	for _, e := range sorted {
		fmt.Fprintf(&out, "%s %s\x00", e.Mode, e.Name)
		raw, _ := hex.DecodeString(e.OID)
		out.Write(raw)
	}
	return out.Bytes()
}

type TagInfo struct {
	Object  string
	Type    ObjectType
	Tag     string
	Tagger  *Identity
	Message string
}

func parseTag(data []byte) TagInfo {
	text := string(data)
	split := strings.Index(text, "\n\n")
	head, message := text, ""
	if split >= 0 {
		head = text[:split]
		message = text[split+2:]
	}
	var object, tag string
	typ := ObjCommit
	var tagger *Identity
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(line, " ") {
			continue
		}
		if strings.HasPrefix(line, "object ") {
			object = strings.TrimSpace(line[7:])
		} else if strings.HasPrefix(line, "type ") {
			typ = ObjectType(strings.TrimSpace(line[5:]))
		} else if strings.HasPrefix(line, "tag ") {
			tag = strings.TrimSpace(line[4:])
		} else if strings.HasPrefix(line, "tagger ") {
			id := parseIdentity(line[7:])
			tagger = &id
		}
	}
	return TagInfo{Object: object, Type: typ, Tag: tag, Tagger: tagger, Message: message}
}

func buildTag(t TagInfo) []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, "object %s\ntype %s\ntag %s\n", t.Object, t.Type, t.Tag)
	if t.Tagger != nil {
		fmt.Fprintf(&sb, "tagger %s\n", formatIdentity(*t.Tagger))
	}
	sb.WriteString("\n")
	sb.WriteString(t.Message)
	return []byte(sb.String())
}

// ---------------------------------------------------------------- packfile v2

var packTypeNum = map[ObjectType]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var packTypeName = map[int]ObjectType{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

// Write a packfile of whole objects only (no deltas) — valid pack v2.
func writePack(objects []*GitObject) []byte {
	var parts bytes.Buffer
	head := make([]byte, 12)
	copy(head[0:4], "PACK")
	putU32BE(head[4:8], 2)
	putU32BE(head[8:12], uint32(len(objects)))
	parts.Write(head)
	for _, obj := range objects {
		data := obj.Data
		typeNum := packTypeNum[obj.Type]
		size := len(data)
		first := (size & 0x0f) | (typeNum << 4)
		if size > 0x0f {
			first |= 0x80
		}
		header := []byte{byte(first)}
		size >>= 4
		for size > 0 {
			b := (size & 0x7f)
			if size > 0x7f {
				b |= 0x80
			}
			header = append(header, byte(b))
			size >>= 7
		}
		parts.Write(header)
		var zb bytes.Buffer
		zw := zlib.NewWriter(&zb)
		zw.Write(data)
		zw.Close()
		parts.Write(zb.Bytes())
	}
	body := parts.Bytes()
	sum := sha1.Sum(body)
	out := make([]byte, 0, len(body)+20)
	out = append(out, body...)
	out = append(out, sum[:]...)
	return out
}

// byteCounter feeds zlib exactly one stream: flate reads byte-by-byte from an
// io.ByteReader, so pos after the read is the exact consumed length.
type byteCounter struct {
	b   []byte
	pos int
}

func (c *byteCounter) ReadByte() (byte, error) {
	if c.pos >= len(c.b) {
		return 0, io.EOF
	}
	v := c.b[c.pos]
	c.pos++
	return v, nil
}

// Read satisfies io.Reader so zlib.NewReader accepts this counter; flate
// still prefers ReadByte when present, so pos stays the exact consumed
// length after the stream.
func (c *byteCounter) Read(p []byte) (int, error) {
	if c.pos >= len(c.b) {
		return 0, io.EOF
	}
	n := copy(p, c.b[c.pos:])
	c.pos += n
	return n, nil
}

// Inflate exactly one zlib stream starting at `pos`.
func inflateOne(buf []byte, pos int) (data []byte, consumed int, err error) {
	rem := len(buf) - pos
	if rem <= 0 {
		return nil, 0, errors.New("pack zlib stream truncated")
	}
	ctr := &byteCounter{b: buf[pos:]}
	zr, err := zlib.NewReader(ctr)
	if err != nil {
		return nil, 0, errors.New("pack zlib stream is corrupt")
	}
	data, err = io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, errors.New("pack zlib stream is corrupt")
	}
	return data, ctr.pos, nil
}

func applyDelta(base []byte, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int, error) {
		out := 0
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return 0, errors.New("delta varint truncated")
			}
			b := delta[pos]
			pos++
			out |= (int(b) & 0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return out, nil
	}
	sourceSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	targetSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	var chunks [][]byte
	written := 0
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			offset := 0
			size := 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					offset |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					size |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if offset+size > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			chunks = append(chunks, base[offset:offset+size])
			written += size
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, errors.New("delta insert truncated")
			}
			chunks = append(chunks, delta[pos:pos+int(op)])
			pos += int(op)
			written += int(op)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if written != targetSize {
		return nil, errors.New("delta target size mismatch")
	}
	return bytes.Join(chunks, nil), nil
}

type PackReadResult struct {
	Objects    map[string]*GitObject
	DeltaCount int
}

// Read a packfile v2/v3, resolving OFS_DELTA and REF_DELTA entries.
// `resolveBase` supplies a base object the pack omits (thin packs).
func readPack(buf []byte, resolveBase func(oid string) *GitObject) (PackReadResult, error) {
	if len(buf) < 12 || string(buf[0:4]) != "PACK" {
		return PackReadResult{}, errors.New("not a packfile")
	}
	version := u32BE(buf[4:8])
	if version != 2 && version != 3 {
		return PackReadResult{}, fmt.Errorf("unsupported pack version %d", version)
	}
	count := u32BE(buf[8:12])
	if len(buf) < 32 {
		return PackReadResult{}, errors.New("packfile too short")
	}
	expected := sha1Hex(buf[:len(buf)-20])
	actual := hex.EncodeToString(buf[len(buf)-20:])
	if expected != actual {
		return PackReadResult{}, errors.New("packfile checksum mismatch")
	}

	objects := make(map[string]*GitObject)
	byOffset := make(map[int]*GitObject)
	deltaCount := 0
	pos := 12
	end := len(buf) - 20
	for i := uint32(0); i < count; i++ {
		entryStart := pos
		if pos >= end {
			return PackReadResult{}, errors.New("packfile truncated")
		}
		b := buf[pos]
		pos++
		typeNum := (b >> 4) & 0x07
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if pos >= end {
				return PackReadResult{}, errors.New("packfile size varint truncated")
			}
			b = buf[pos]
			pos++
			size |= (int(b) & 0x7f) << shift
			shift += 7
		}
		var baseOid string
		var baseOffset int = -1
		if typeNum == 6 {
			if pos >= end {
				return PackReadResult{}, errors.New("packfile ofs-delta truncated")
			}
			b = buf[pos]
			pos++
			offset := int(b & 0x7f)
			for b&0x80 != 0 {
				if pos >= end {
					return PackReadResult{}, errors.New("packfile ofs-delta truncated")
				}
				b = buf[pos]
				pos++
				offset = ((offset + 1) << 7) | (int(b) & 0x7f)
			}
			baseOffset = entryStart - offset
		} else if typeNum == 7 {
			if pos+20 > end {
				return PackReadResult{}, errors.New("packfile ref-delta truncated")
			}
			baseOid = hex.EncodeToString(buf[pos : pos+20])
			pos += 20
		}
		data, consumed, err := inflateOne(buf, pos)
		if err != nil {
			return PackReadResult{}, err
		}
		pos += consumed
		if len(data) != size {
			return PackReadResult{}, errors.New("pack object size mismatch")
		}
		if typeNum == 6 || typeNum == 7 {
			deltaCount++
			var base *GitObject
			if baseOffset >= 0 {
				base = byOffset[baseOffset]
			} else if baseOid != "" {
				base = objects[baseOid]
				if base == nil && resolveBase != nil {
					base = resolveBase(baseOid)
				}
			}
			if base == nil {
				return PackReadResult{}, errors.New("pack delta base missing")
			}
			resolved, err := applyDelta(base.Data, data)
			if err != nil {
				return PackReadResult{}, err
			}
			typ := base.Type
			oid := hashObject(typ, resolved)
			obj := &GitObject{Type: typ, Data: resolved}
			objects[oid] = obj
			byOffset[entryStart] = obj
		} else {
			typ, ok := packTypeName[int(typeNum)]
			if !ok {
				return PackReadResult{}, fmt.Errorf("pack object type %d unknown", typeNum)
			}
			oid := hashObject(typ, data)
			obj := &GitObject{Type: typ, Data: data}
			objects[oid] = obj
			byOffset[entryStart] = obj
		}
	}
	return PackReadResult{Objects: objects, DeltaCount: deltaCount}, nil
}

// ---------------------------------------------------------------- object store

// An in-memory object store: loose objects keyed by id, nothing on disk.
type ObjectStore struct {
	Objects map[string]*GitObject
}

func NewObjectStore() *ObjectStore { return &ObjectStore{Objects: make(map[string]*GitObject)} }

func (s *ObjectStore) Has(oid string) bool { _, ok := s.Objects[oid]; return ok }

func (s *ObjectStore) Get(oid string) *GitObject { return s.Objects[oid] }

func (s *ObjectStore) Put(typ ObjectType, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := s.Objects[oid]; !ok {
		s.Objects[oid] = &GitObject{Type: typ, Data: data}
	}
	return oid
}

func (s *ObjectStore) PutObject(oid string, obj *GitObject) {
	if _, ok := s.Objects[oid]; !ok {
		s.Objects[oid] = obj
	}
}

type walkResult struct {
	commits  map[string]bool
	boundary map[string]bool
}

// Walk commits from `tips`: returns the commit ids reached, and the boundary
// commits — reached commits whose parents were NOT walked (depth limit, or a
// stopAt boundary). `maxDepth` counts edges from the tips (0 = tips only).
func walkCommits(store *ObjectStore, tips []string, maxDepth int, hasMaxDepth bool, stopAt map[string]bool) walkResult {
	commits := make(map[string]bool)
	boundary := make(map[string]bool)
	type item struct {
		oid   string
		depth int
	}
	var queue []item
	head := 0
	for _, tip := range tips {
		if isOid(tip) && !commits[tip] {
			queue = append(queue, item{oid: tip, depth: 0})
		}
	}
	for head < len(queue) {
		cur := queue[head]
		head++
		oid, depth := cur.oid, cur.depth
		if commits[oid] {
			continue
		}
		obj := store.Get(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		commits[oid] = true
		if stopAt != nil && stopAt[oid] {
			boundary[oid] = true
			continue
		}
		_, parents := parseCommitLight(obj.Data)
		if hasMaxDepth && depth >= maxDepth {
			if len(parents) > 0 {
				boundary[oid] = true
			}
			continue
		}
		for _, p := range parents {
			if !commits[p] {
				queue = append(queue, item{oid: p, depth: depth + 1})
			}
		}
	}
	return walkResult{commits: commits, boundary: boundary}
}

// Every object reachable from `commitOids` (commits, their trees/blobs, tags).
func collectReachable(store *ObjectStore, commitOids map[string]bool) map[string]*GitObject {
	out := make(map[string]*GitObject)
	var visitCommit func(oid string)
	var visitTree func(oid string)
	visitTree = func(oid string) {
		obj := store.Get(oid)
		if obj == nil || out[oid] != nil {
			return
		}
		out[oid] = obj
		if obj.Type != ObjTree {
			return
		}
		entries, err := parseTree(obj.Data)
		if err != nil {
			return
		}
		for _, e := range entries {
			child := store.Get(e.OID)
			if child == nil {
				continue
			}
			if child.Type == ObjTree {
				visitTree(e.OID)
			} else {
				out[e.OID] = child
			}
		}
	}
	visitCommit = func(oid string) {
		obj := store.Get(oid)
		if obj == nil || obj.Type != ObjCommit || out[oid] != nil {
			return
		}
		out[oid] = obj
		tree, parents := parseCommitLight(obj.Data)
		visitTree(tree)
		for _, p := range parents {
			visitCommit(p)
		}
	}
	for oid := range commitOids {
		obj := store.Get(oid)
		if obj == nil {
			continue
		}
		if obj.Type == ObjCommit {
			visitCommit(oid)
		} else if obj.Type == ObjTag {
			out[oid] = obj
			tag := parseTag(obj.Data)
			if tag.Type == ObjCommit {
				visitCommit(tag.Object)
			} else if tag.Type == ObjTree {
				visitTree(tag.Object)
			} else {
				target := store.Get(tag.Object)
				if target != nil {
					out[tag.Object] = target
				}
			}
		} else {
			out[oid] = obj
		}
	}
	return out
}

// The objects of exactly the given commits (commit + its tree + its blobs),
// WITHOUT following parents.
func collectCommitObjects(store *ObjectStore, commitOids map[string]bool) map[string]*GitObject {
	out := make(map[string]*GitObject)
	var visitTree func(oid string)
	visitTree = func(oid string) {
		obj := store.Get(oid)
		if obj == nil || out[oid] != nil {
			return
		}
		out[oid] = obj
		if obj.Type != ObjTree {
			return
		}
		entries, err := parseTree(obj.Data)
		if err != nil {
			return
		}
		for _, e := range entries {
			child := store.Get(e.OID)
			if child == nil {
				continue
			}
			if child.Type == ObjTree {
				visitTree(e.OID)
			} else {
				out[e.OID] = child
			}
		}
	}
	for oid := range commitOids {
		obj := store.Get(oid)
		if obj == nil {
			continue
		}
		if obj.Type == ObjCommit {
			out[oid] = obj
			tree, _ := parseCommitLight(obj.Data)
			visitTree(tree)
		} else if obj.Type == ObjTag {
			out[oid] = obj
			tag := parseTag(obj.Data)
			if tag.Type == ObjCommit {
				target := store.Get(tag.Object)
				if target != nil {
					out[tag.Object] = target
					tree, _ := parseCommitLight(target.Data)
					visitTree(tree)
				}
			} else if tag.Type == ObjTree {
				visitTree(tag.Object)
			} else {
				target := store.Get(tag.Object)
				if target != nil {
					out[tag.Object] = target
				}
			}
		} else {
			out[oid] = obj
		}
	}
	return out
}

// Is `ancestor` reachable from `descendant` by following parents?
func isAncestor(store *ObjectStore, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := make(map[string]bool)
	queue := []string{descendant}
	head := 0
	for head < len(queue) {
		oid := queue[head]
		head++
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := store.Get(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		_, parents := parseCommitLight(obj.Data)
		for _, p := range parents {
			if p == ancestor {
				return true
			}
			if !seen[p] {
				queue = append(queue, p)
			}
		}
	}
	return false
}

// The merge-base of two commits: the newest common ancestor (by walk order).
func mergeBase(store *ObjectStore, a, b string) string {
	ancestorsA := make(map[string]bool)
	queueA := []string{a}
	headA := 0
	for headA < len(queueA) {
		oid := queueA[headA]
		headA++
		if ancestorsA[oid] {
			continue
		}
		ancestorsA[oid] = true
		obj := store.Get(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		_, parents := parseCommitLight(obj.Data)
		queueA = append(queueA, parents...)
	}
	// BFS from b: first commit also in A is the merge base.
	seen := make(map[string]bool)
	queueB := []string{b}
	headB := 0
	for headB < len(queueB) {
		oid := queueB[headB]
		headB++
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if ancestorsA[oid] {
			return oid
		}
		obj := store.Get(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		_, parents := parseCommitLight(obj.Data)
		queueB = append(queueB, parents...)
	}
	return ""
}

func putU32BE(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

func u32BE(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
