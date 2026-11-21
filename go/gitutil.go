// gitutil.go — the git object layer: ids, pkt-lines, packfiles, and the
// commit / tree / tag formats.
//
// This file is pure git mechanics. It knows nothing about HTTP,
// repositories, issues, or pull requests; the store and git-http layers
// drive it against an in-memory object map. No git binary, no network,
// no third-party packages, no disk, no clock, no randomness — the same
// inputs always produce the same bytes.
//
// The invariants it honours (the reasoning lives in docs/DECISIONS.md
// in the reference tree):
//   - Packs we write hold whole objects only. Delta search never runs
//     on the write path; deltas a client sends are resolved while
//     reading, thin packs included.
//   - A side-band data frame carries at most 65,515 payload bytes, so
//     a framed packet never passes git's 65,520-byte pkt-line ceiling.
//   - A zlib stream's length is measured with a counting reader: one
//     inflate yields both the object and the exact number of input
//     bytes it consumed, whatever encoder produced the stream.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ZeroOID is the all-zero id: "no object", used for unborn refs and for
// the old/new side of a ref creation or deletion.
const ZeroOID = "0000000000000000000000000000000000000000"

// ObjectType is the kind of a git object.
type ObjectType string

const (
	ObjCommit ObjectType = "commit"
	ObjTree   ObjectType = "tree"
	ObjBlob   ObjectType = "blob"
	ObjTag    ObjectType = "tag"
)

// GitObject is one git object: its kind plus its raw, uncompressed
// bytes. The id is never stored here; it is always hashObject(Type, Data).
type GitObject struct {
	Type ObjectType
	Data []byte
}

// isOid reports whether v looks like a lowercase hex object id: exactly
// 40 characters, each 0-9 or a-f.
func isOid(v string) bool {
	if len(v) != 40 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isOidValue is isOid for a value of unknown type: only a string can be
// an id, anything else is not one.
func isOidValue(v any) bool {
	s, ok := v.(string)
	return ok && isOid(s)
}

// hashObject is the git object id: SHA-1 over "<type> <len>\0<data>".
func hashObject(typ ObjectType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(string(typ)))
	h.Write([]byte(" "))
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// sha1Hex is the hex SHA-1 of raw bytes (pack trailers, checksums).
func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// ------------------------------------------------------------------ store

// ObjectStore is an in-memory object store: objects keyed by id, nothing
// on disk. Put and PutObject are idempotent — storing an object that is
// already there is a no-op.
type ObjectStore struct {
	Objects map[string]*GitObject
}

func NewObjectStore() *ObjectStore { return &ObjectStore{Objects: make(map[string]*GitObject)} }

func (s *ObjectStore) Has(oid string) bool { _, ok := s.Objects[oid]; return ok }

func (s *ObjectStore) Get(oid string) *GitObject { return s.Objects[oid] }

// Put hashes data as an object of the given type, stores it unless it is
// already there, and returns its id.
func (s *ObjectStore) Put(typ ObjectType, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := s.Objects[oid]; !ok {
		s.Objects[oid] = &GitObject{Type: typ, Data: data}
	}
	return oid
}

// PutObject stores an already-built object under a caller-supplied id,
// unless that id is already present.
func (s *ObjectStore) PutObject(oid string, obj *GitObject) {
	if _, ok := s.Objects[oid]; !ok {
		s.Objects[oid] = obj
	}
}

// ---------------------------------------------------------------- pkt-line
//
// A pkt-line is a 4-hex-digit length (itself included) followed by the
// payload. "0000" is a flush packet, "0001" a delimiter packet.

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// pktData frames arbitrary bytes as one pkt-line.
func pktData(payload []byte) []byte {
	head := fmt.Sprintf("%04x", len(payload)+4)
	out := make([]byte, 0, len(payload)+4)
	out = append(out, head...)
	return append(out, payload...)
}

// pktLine frames one text line (usually ending in "\n") as a pkt-line.
func pktLine(text string) []byte {
	return pktData([]byte(text))
}

// pktItem is one element of a pkt-line stream being encoded.
type pktItem struct {
	kind int // pktText, pktBytes, pktFlushKind or pktDelimKind
	s    string
	b    []byte
}

const (
	pktText = iota
	pktBytes
	pktFlushKind
	pktDelimKind
)

func pktStr(s string) pktItem { return pktItem{kind: pktText, s: s} }
func pktRaw(b []byte) pktItem { return pktItem{kind: pktBytes, b: b} }

var pktFlushItem = pktItem{kind: pktFlushKind}
var pktDelimItem = pktItem{kind: pktDelimKind}

// encodePktLines renders a list of lines, raw buffers, flush and
// delimiter markers into a single pkt-line byte stream.
func encodePktLines(items []pktItem) []byte {
	var out bytes.Buffer
	for _, it := range items {
		switch it.kind {
		case pktFlushKind:
			out.Write(pktFlush)
		case pktDelimKind:
			out.Write(pktDelim)
		case pktText:
			out.Write(pktLine(it.s))
		default:
			out.Write(pktData(it.b))
		}
	}
	return out.Bytes()
}

// PktPacket is one decoded pkt-line: a data packet, a flush, or a
// delimiter.
type PktPacket struct {
	Kind    string // "data" | "flush" | "delim"
	Payload []byte
}

// decodePktLines decodes a whole pkt-line stream; any malformed framing
// is an error.
func decodePktLines(buf []byte) ([]PktPacket, error) {
	packets, _, err := decodePktPrefix(buf)
	return packets, err
}

// decodePktPrefix decodes pkt-lines from the start of buf until the first
// flush packet (inclusive) and reports how many bytes that prefix used.
// Bytes after the flush — a packfile in receive-pack, for instance — are
// left for the caller. With no flush in the buffer, every packet is
// returned and the consumed count is the whole buffer.
func decodePktPrefix(buf []byte) ([]PktPacket, int, error) {
	var packets []PktPacket
	pos := 0
	for pos < len(buf) {
		if pos+4 > len(buf) {
			return nil, 0, errors.New("pkt-line truncated header")
		}
		head := string(buf[pos : pos+4])
		switch head {
		case "0000":
			packets = append(packets, PktPacket{Kind: "flush", Payload: []byte{}})
			return packets, pos + 4, nil
		case "0001":
			packets = append(packets, PktPacket{Kind: "delim", Payload: []byte{}})
			pos += 4
			continue
		}
		length, err := strconv.ParseUint(head, 16, 32)
		if err != nil || length < 4 || pos+int(length) > len(buf) {
			return nil, 0, fmt.Errorf("pkt-line bad length %q", head)
		}
		packets = append(packets, PktPacket{Kind: "data", Payload: buf[pos+4 : pos+int(length)]})
		pos += int(length)
	}
	return packets, pos, nil
}

// sideband wraps payload as band-1 pkt-lines (side-band-64k). Each frame
// holds at most 65,515 data bytes: a pkt-line is at most 65,520 bytes
// including its 4-byte header, and the band byte takes one more. An
// empty payload still produces one frame holding just the band byte, so
// the receiver sees the stream end rather than nothing at all.
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

// ---------------------------------------------------------------- commits

// Identity is one author/committer line: name, email and a Unix
// timestamp. The timezone is parsed but deliberately dropped — identities
// here are always rendered in UTC (see formatIdentity).
type Identity struct {
	Name  string
	Email string
	TS    int64
}

// CommitInfo is the parsed form of a commit object.
type CommitInfo struct {
	Tree      string
	Parents   []string
	Author    Identity
	Committer Identity
	Message   string
}

// identityTail checks the tail of an identity line — " <digits> <+|-><4
// digits>" and nothing else — and returns the timestamp when it matches.
func identityTail(tail string) (int64, bool) {
	if len(tail) < 2 || tail[0] != ' ' {
		return 0, false
	}
	rest := tail[1:]
	sp := strings.IndexByte(rest, ' ')
	if sp < 0 {
		return 0, false
	}
	digits, tz := rest[:sp], rest[sp+1:]
	if digits == "" || len(tz) != 5 || (tz[0] != '+' && tz[0] != '-') {
		return 0, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	for i := 1; i < 5; i++ {
		if tz[i] < '0' || tz[i] > '9' {
			return 0, false
		}
	}
	ts, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return ts, true
}

// parseIdentity parses one "Name <email> ts tz" line. The name part is
// greedy, so a "<" inside the name still resolves to the last bracket
// pair that leaves a well-formed tail. A line that does not match is
// kept whole as the name, with no email and a zero timestamp.
func parseIdentity(line string) Identity {
	end := len(line)
	for {
		lt := strings.LastIndex(line[:end], "<")
		if lt < 0 {
			break
		}
		if gt := strings.IndexByte(line[lt+1:], '>'); gt >= 0 {
			gt += lt + 1
			if lt > 0 && line[lt-1] == ' ' {
				if ts, ok := identityTail(line[gt+1:]); ok {
					return Identity{Name: line[:lt-1], Email: line[lt+1 : gt], TS: ts}
				}
			}
		}
		end = lt
	}
	return Identity{Name: line, Email: "", TS: 0}
}

// formatIdentity renders an identity the way git does, always in +0000.
func formatIdentity(id Identity) string {
	return fmt.Sprintf("%s <%s> %d +0000", id.Name, id.Email, id.TS)
}

// splitHeadBody splits raw commit/tag bytes at the first blank line into
// the header block and the message. With no blank line the whole thing
// is the header and the message is empty.
func splitHeadBody(data []byte) (head, message string) {
	text := string(data)
	if i := strings.Index(text, "\n\n"); i >= 0 {
		return text[:i], text[i+2:]
	}
	return text, ""
}

// scanHeaderLines calls fn for each line of a header block, skipping
// continuation lines (those starting with a space belong to the previous
// header).
func scanHeaderLines(head string, fn func(line string)) {
	for line := range strings.Lines(head) {
		line = strings.TrimSuffix(line, "\n")
		if strings.HasPrefix(line, " ") {
			continue
		}
		fn(line)
	}
}

// parseCommit parses a whole commit object: tree, parents, author,
// committer and message.
func parseCommit(data []byte) CommitInfo {
	head, message := splitHeadBody(data)
	c := CommitInfo{Message: message}
	scanHeaderLines(head, func(line string) {
		switch {
		case strings.HasPrefix(line, "tree "):
			c.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			c.Parents = append(c.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			c.Author = parseIdentity(line[7:])
		case strings.HasPrefix(line, "committer "):
			c.Committer = parseIdentity(line[10:])
		}
	})
	return c
}

// parseCommitLight is the hot-path commit parse: tree and parents only,
// no identity parsing and no message handling. Results are identical to
// parseCommit for the two fields it returns.
func parseCommitLight(data []byte) (tree string, parents []string) {
	head, _ := splitHeadBody(data)
	for line := range strings.Lines(head) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" || line[0] == ' ' {
			continue
		}
		if strings.HasPrefix(line, "tree ") {
			tree = strings.TrimSpace(line[5:])
		} else if strings.HasPrefix(line, "parent ") {
			parents = append(parents, strings.TrimSpace(line[7:]))
		}
	}
	return tree, parents
}

// buildCommit renders a commit object: tree line, one parent line each,
// author, committer, a blank line, then the message verbatim.
func buildCommit(c CommitInfo) []byte {
	var buf bytes.Buffer
	buf.WriteString("tree " + c.Tree + "\n")
	for _, p := range c.Parents {
		buf.WriteString("parent " + p + "\n")
	}
	buf.WriteString("author " + formatIdentity(c.Author) + "\n")
	buf.WriteString("committer " + formatIdentity(c.Committer) + "\n")
	buf.WriteString("\n")
	buf.WriteString(c.Message)
	return buf.Bytes()
}

// ------------------------------------------------------------------- trees

// TreeEntry is one entry of a tree object: mode ("100644", "40000", ...),
// name, and the hex id of the child object.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// parseTree parses raw tree bytes: repeating "<mode> <name>\0<20 raw
// bytes>". Entries come back in file order; callers that need git order
// sort them (buildTree does).
func parseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	pos := 0
	for pos < len(data) {
		rel := bytes.IndexByte(data[pos:], ' ')
		if rel < 0 {
			return nil, errors.New("tree entry has no mode")
		}
		space := pos + rel
		rel = bytes.IndexByte(data[space+1:], 0)
		if rel < 0 {
			return nil, errors.New("tree entry truncated")
		}
		nul := space + 1 + rel
		if nul+21 > len(data) {
			return nil, errors.New("tree entry truncated")
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[pos:space]),
			Name: string(data[space+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		pos = nul + 21
	}
	return entries, nil
}

// treeOrderKey is the name git sorts on: directories compare as if their
// name ended in "/".
func treeOrderKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

// buildTree renders tree bytes, sorting entries into git order first
// (stable, so equal keys keep their input order). The input slice is
// never mutated.
func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return treeOrderKey(sorted[i]) < treeOrderKey(sorted[j])
	})
	var out bytes.Buffer
	for _, e := range sorted {
		out.WriteString(e.Mode + " " + e.Name + "\x00")
		raw, _ := hex.DecodeString(e.OID)
		out.Write(raw)
	}
	return out.Bytes()
}

// -------------------------------------------------------------------- tags

// TagInfo is the parsed form of an annotated tag object. Tagger is nil
// for a tag object with no tagger line.
type TagInfo struct {
	Object  string
	Type    ObjectType
	Tag     string
	Tagger  *Identity
	Message string
}

// parseTag parses an annotated tag object. A missing type line defaults
// to commit, matching git's assumption for a tag with no explicit type.
func parseTag(data []byte) TagInfo {
	head, message := splitHeadBody(data)
	t := TagInfo{Type: ObjCommit, Message: message}
	scanHeaderLines(head, func(line string) {
		switch {
		case strings.HasPrefix(line, "object "):
			t.Object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			t.Type = ObjectType(strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "tag "):
			t.Tag = strings.TrimSpace(line[4:])
		case strings.HasPrefix(line, "tagger "):
			id := parseIdentity(line[7:])
			t.Tagger = &id
		}
	})
	return t
}

// buildTag renders a tag object: object, type and tag lines, an optional
// tagger line, a blank line, then the message verbatim.
func buildTag(t TagInfo) []byte {
	var buf bytes.Buffer
	buf.WriteString("object " + t.Object + "\n")
	buf.WriteString("type " + string(t.Type) + "\n")
	buf.WriteString("tag " + t.Tag + "\n")
	if t.Tagger != nil {
		buf.WriteString("tagger " + formatIdentity(*t.Tagger) + "\n")
	}
	buf.WriteString("\n")
	buf.WriteString(t.Message)
	return buf.Bytes()
}

// -------------------------------------------------------------- byte order

// storeU32BE writes v as 4 big-endian bytes into b (which must hold 4).
func storeU32BE(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

// loadU32BE reads 4 big-endian bytes as a uint32.
func loadU32BE(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// ------------------------------------------------------------ packfile v2

// packCodeOf maps an object type to its packfile type number; packKindOf
// is the reverse lookup. Types 6 (OFS_DELTA) and 7 (REF_DELTA) only ever
// appear on read — this writer never emits them.
var packCodeOf = map[ObjectType]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var packKindOf = map[int]ObjectType{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

// packEntryHeader encodes one entry header: the type number and the
// uncompressed size, size first in 4 low bits, then 7 bits per byte,
// little-endian, with the high bit set on every byte but the last.
func packEntryHeader(typeNum, size int) []byte {
	head := []byte{byte((typeNum << 4) | (size & 0x0f))}
	size >>= 4
	if size > 0 {
		head[0] |= 0x80
	}
	for size > 0 {
		b := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		head = append(head, b)
	}
	return head
}

// writePack renders a packfile v2 of whole objects only (no deltas):
// "PACK", version 2, object count, one header + zlib stream per object,
// then the SHA-1 trailer over everything before it.
func writePack(objects []*GitObject) []byte {
	var body bytes.Buffer
	head := make([]byte, 12)
	copy(head[0:4], "PACK")
	storeU32BE(head[4:8], 2)
	storeU32BE(head[8:12], uint32(len(objects)))
	body.Write(head)
	for _, obj := range objects {
		body.Write(packEntryHeader(packCodeOf[obj.Type], len(obj.Data)))
		zw := zlib.NewWriter(&body)
		zw.Write(obj.Data)
		zw.Close()
	}
	packed := body.Bytes()
	sum := sha1.Sum(packed)
	out := make([]byte, 0, len(packed)+20)
	out = append(out, packed...)
	out = append(out, sum[:]...)
	return out
}

// byteCounter is a reader over a byte slice that counts every byte the
// decoder pulls. It implements io.ByteReader, so compress/flate reads
// one byte at a time and stops exactly at the end of the deflate stream;
// pos after the read is therefore the exact number of input bytes the
// stream consumed. Read is present only so zlib.NewReader accepts the
// counter as an io.Reader — flate prefers ReadByte when it exists.
type byteCounter struct {
	data []byte
	pos  int
}

func (c *byteCounter) ReadByte() (byte, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	v := c.data[c.pos]
	c.pos++
	return v, nil
}

func (c *byteCounter) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := copy(p, c.data[c.pos:])
	c.pos += n
	return n, nil
}

// inflateAt inflates exactly one zlib stream starting at pos in buf and
// returns the decoded bytes plus how many input bytes the stream used.
// A single inflate through the counting reader gives both: the decoder
// stops at the stream's end, so the counter holds the consumed length
// with no re-compression and no search, for any encoder.
func inflateAt(buf []byte, pos int) (data []byte, consumed int, err error) {
	if pos < 0 || pos >= len(buf) {
		return nil, 0, errors.New("pack zlib stream truncated")
	}
	ctr := &byteCounter{data: buf[pos:]}
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

// deltaCursor walks a delta stream one byte at a time, tracking position
// so the error paths can say exactly what ran out.
type deltaCursor struct {
	data []byte
	pos  int
}

func (d *deltaCursor) next() (byte, error) {
	if d.pos >= len(d.data) {
		return 0, errors.New("delta stream truncated")
	}
	b := d.data[d.pos]
	d.pos++
	return b, nil
}

// varint reads a little-endian base-128 varint: 7 payload bits per byte,
// high bit set while more bytes follow.
func (d *deltaCursor) varint() (int, error) {
	out := 0
	shift := uint(0)
	for {
		b, err := d.next()
		if err != nil {
			return 0, errors.New("delta varint truncated")
		}
		out |= (int(b) & 0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			return out, nil
		}
	}
}

// applyDeltaPatch applies a git delta (OFS/REF payload) to its base
// object and returns the reconstructed object bytes. Opcode layout: a
// byte with the high bit set copies from the base (bits 0-3 select
// offset bytes, bits 4-6 select size bytes, a zero size means 0x10000);
// any other nonzero byte inserts that many literal bytes; opcode 0 is
// invalid. Both sizes are checked against the stream so a corrupt delta
// fails loudly instead of silently producing wrong bytes.
func applyDeltaPatch(base []byte, delta []byte) ([]byte, error) {
	d := &deltaCursor{data: delta}
	sourceSize, err := d.varint()
	if err != nil {
		return nil, err
	}
	targetSize, err := d.varint()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	// Pre-size the output, but cap the reservation: a corrupt delta could
	// claim an absurd target size, and the buffer grows on demand anyway.
	seed := targetSize
	if seed > 1<<20 {
		seed = 1 << 20
	}
	out := bytes.NewBuffer(make([]byte, 0, seed))
	for d.pos < len(delta) {
		op, err := d.next()
		if err != nil {
			return nil, err
		}
		switch {
		case op&0x80 != 0:
			offset, size := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					b, err := d.next()
					if err != nil {
						return nil, err
					}
					offset |= int(b) << (8 * uint(i))
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					b, err := d.next()
					if err != nil {
						return nil, err
					}
					size |= int(b) << (8 * uint(i))
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if offset+size > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			out.Write(base[offset : offset+size])
		case op != 0:
			if d.pos+int(op) > len(delta) {
				return nil, errors.New("delta insert truncated")
			}
			out.Write(delta[d.pos : d.pos+int(op)])
			d.pos += int(op)
		default:
			return nil, errors.New("delta opcode 0")
		}
	}
	if out.Len() != targetSize {
		return nil, errors.New("delta target size mismatch")
	}
	return out.Bytes(), nil
}

// PackReadResult is what readPack returns: every object in the pack,
// keyed by id, plus how many of them arrived as deltas.
type PackReadResult struct {
	Objects    map[string]*GitObject
	DeltaCount int
}

// readEntryHeader parses one pack entry header at pos (type number and
// uncompressed size, see packEntryHeader) and returns where the entry
// body starts.
func readEntryHeader(buf []byte, pos, end int) (typeNum, size, next int, err error) {
	if pos >= end {
		return 0, 0, 0, errors.New("packfile truncated")
	}
	b := buf[pos]
	pos++
	typeNum = (int(b) >> 4) & 0x07
	size = int(b & 0x0f)
	shift := uint(4)
	for b&0x80 != 0 {
		if pos >= end {
			return 0, 0, 0, errors.New("packfile size varint truncated")
		}
		b = buf[pos]
		pos++
		size |= (int(b) & 0x7f) << shift
		shift += 7
	}
	return typeNum, size, pos, nil
}

// readOfsDistance parses an OFS_DELTA base offset: the first byte holds
// 7 bits, and every continuation byte adds its 7 bits after shifting the
// accumulated value left by 7 and adding one — the encoding that keeps
// the offset strictly increasing with the byte length.
func readOfsDistance(buf []byte, pos, end int) (offset, next int, err error) {
	if pos >= end {
		return 0, 0, errors.New("packfile ofs-delta truncated")
	}
	b := buf[pos]
	pos++
	offset = int(b & 0x7f)
	for b&0x80 != 0 {
		if pos >= end {
			return 0, 0, errors.New("packfile ofs-delta truncated")
		}
		b = buf[pos]
		pos++
		offset = ((offset + 1) << 7) | (int(b) & 0x7f)
	}
	return offset, pos, nil
}

// readPack reads a packfile v2 (or v3), resolving OFS_DELTA and REF_DELTA
// entries into whole objects. resolveBase supplies a base object the
// pack omits — that is what makes thin packs work: the base is already
// in the receiving store. The SHA-1 trailer is verified before any entry
// is trusted, and each entry's inflated size is checked against its
// header, so a corrupt pack fails here instead of poisoning the store.
func readPack(buf []byte, resolveBase func(oid string) *GitObject) (PackReadResult, error) {
	if len(buf) < 12 || string(buf[0:4]) != "PACK" {
		return PackReadResult{}, errors.New("not a packfile")
	}
	version := loadU32BE(buf[4:8])
	if version != 2 && version != 3 {
		return PackReadResult{}, fmt.Errorf("unsupported pack version %d", version)
	}
	if len(buf) < 32 {
		return PackReadResult{}, errors.New("packfile too short")
	}
	if sha1Hex(buf[:len(buf)-20]) != hex.EncodeToString(buf[len(buf)-20:]) {
		return PackReadResult{}, errors.New("packfile checksum mismatch")
	}
	count := loadU32BE(buf[8:12])
	objects := make(map[string]*GitObject)
	byOffset := make(map[int]*GitObject)
	deltaCount := 0
	pos := 12
	end := len(buf) - 20
	for i := uint32(0); i < count; i++ {
		entryStart := pos
		typeNum, size, next, err := readEntryHeader(buf, pos, end)
		if err != nil {
			return PackReadResult{}, err
		}
		pos = next
		baseOffset := -1
		baseOid := ""
		switch typeNum {
		case 6: // OFS_DELTA: base is an earlier entry, by back-offset.
			var back int
			back, pos, err = readOfsDistance(buf, pos, end)
			if err != nil {
				return PackReadResult{}, err
			}
			baseOffset = entryStart - back
		case 7: // REF_DELTA: base is named by id.
			if pos+20 > end {
				return PackReadResult{}, errors.New("packfile ref-delta truncated")
			}
			baseOid = hex.EncodeToString(buf[pos : pos+20])
			pos += 20
		}
		data, consumed, err := inflateAt(buf, pos)
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
			switch {
			case baseOffset >= 0:
				base = byOffset[baseOffset]
			case baseOid != "":
				base = objects[baseOid]
				if base == nil && resolveBase != nil {
					base = resolveBase(baseOid)
				}
			}
			if base == nil {
				return PackReadResult{}, errors.New("pack delta base missing")
			}
			resolved, err := applyDeltaPatch(base.Data, data)
			if err != nil {
				return PackReadResult{}, err
			}
			obj := &GitObject{Type: base.Type, Data: resolved}
			oid := hashObject(obj.Type, resolved)
			objects[oid] = obj
			byOffset[entryStart] = obj
			continue
		}
		typ, ok := packKindOf[typeNum]
		if !ok {
			return PackReadResult{}, fmt.Errorf("pack object type %d unknown", typeNum)
		}
		obj := &GitObject{Type: typ, Data: data}
		oid := hashObject(typ, data)
		objects[oid] = obj
		byOffset[entryStart] = obj
	}
	return PackReadResult{Objects: objects, DeltaCount: deltaCount}, nil
}

// ------------------------------------------------------------ reachability

type walkResult struct {
	commits  map[string]bool
	boundary map[string]bool
}

// walkCommits walks commits from tips, breadth-first, and returns the
// ids reached plus the boundary commits — reached commits whose parents
// were NOT walked, because a stopAt boundary was hit or the depth limit
// ran out. maxDepth counts edges from the tips (0 = tips only); it
// applies only when hasMaxDepth is set.
func walkCommits(store *ObjectStore, tips []string, maxDepth int, hasMaxDepth bool, stopAt map[string]bool) walkResult {
	reached := make(map[string]bool)
	edge := make(map[string]bool)
	type node struct {
		oid   string
		depth int
	}
	queue := make([]node, 0, len(tips))
	for _, tip := range tips {
		if isOid(tip) && !reached[tip] {
			queue = append(queue, node{oid: tip, depth: 0})
		}
	}
	for head := 0; head < len(queue); head++ {
		cur := queue[head]
		if reached[cur.oid] {
			continue
		}
		obj := store.Get(cur.oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		reached[cur.oid] = true
		if stopAt != nil && stopAt[cur.oid] {
			edge[cur.oid] = true
			continue
		}
		_, parents := parseCommitLight(obj.Data)
		if hasMaxDepth && cur.depth >= maxDepth {
			if len(parents) > 0 {
				edge[cur.oid] = true
			}
			continue
		}
		for _, p := range parents {
			if !reached[p] {
				queue = append(queue, node{oid: p, depth: cur.depth + 1})
			}
		}
	}
	return walkResult{commits: reached, boundary: edge}
}

// expandTree adds a tree and everything under it to out, iteratively:
// subtrees are expanded, blobs (and anything else) are added as-is. A
// non-tree root is added but not expanded; a missing object is skipped.
func expandTree(store *ObjectStore, out map[string]*GitObject, root string) {
	pending := []string{root}
	for len(pending) > 0 {
		oid := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		obj := store.Get(oid)
		if obj == nil || out[oid] != nil {
			continue
		}
		out[oid] = obj
		if obj.Type != ObjTree {
			continue
		}
		entries, err := parseTree(obj.Data)
		if err != nil {
			continue
		}
		for _, e := range entries {
			child := store.Get(e.OID)
			if child == nil {
				continue
			}
			if child.Type == ObjTree {
				pending = append(pending, e.OID)
			} else {
				out[e.OID] = child
			}
		}
	}
}

// collectReachable returns every object reachable from commitOids: the
// commits and all their ancestors, their trees and blobs, and — for a
// tag id — the tag itself plus whatever it points at.
func collectReachable(store *ObjectStore, commitOids map[string]bool) map[string]*GitObject {
	out := make(map[string]*GitObject)
	var stack []string
	for oid := range commitOids {
		obj := store.Get(oid)
		if obj == nil {
			continue
		}
		switch obj.Type {
		case ObjCommit:
			stack = append(stack, oid)
		case ObjTag:
			out[oid] = obj
			tag := parseTag(obj.Data)
			switch tag.Type {
			case ObjCommit:
				stack = append(stack, tag.Object)
			case ObjTree:
				expandTree(store, out, tag.Object)
			default:
				if target := store.Get(tag.Object); target != nil {
					out[tag.Object] = target
				}
			}
		default:
			out[oid] = obj
		}
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		obj := store.Get(oid)
		if obj == nil || obj.Type != ObjCommit || out[oid] != nil {
			continue
		}
		out[oid] = obj
		tree, parents := parseCommitLight(obj.Data)
		expandTree(store, out, tree)
		stack = append(stack, parents...)
	}
	return out
}

// collectCommitObjects returns the objects of exactly the given commits
// — each commit plus its tree and blobs — WITHOUT following parents. A
// tag id contributes the tag plus its target's objects (for a commit
// target, the target commit and its tree).
func collectCommitObjects(store *ObjectStore, commitOids map[string]bool) map[string]*GitObject {
	out := make(map[string]*GitObject)
	for oid := range commitOids {
		obj := store.Get(oid)
		if obj == nil {
			continue
		}
		switch obj.Type {
		case ObjCommit:
			out[oid] = obj
			tree, _ := parseCommitLight(obj.Data)
			expandTree(store, out, tree)
		case ObjTag:
			out[oid] = obj
			tag := parseTag(obj.Data)
			switch tag.Type {
			case ObjCommit:
				if target := store.Get(tag.Object); target != nil {
					out[tag.Object] = target
					tree, _ := parseCommitLight(target.Data)
					expandTree(store, out, tree)
				}
			case ObjTree:
				expandTree(store, out, tag.Object)
			default:
				if target := store.Get(tag.Object); target != nil {
					out[tag.Object] = target
				}
			}
		default:
			out[oid] = obj
		}
	}
	return out
}

// commitParents returns the parent ids of a commit object, or nil when
// oid is missing or is not a commit.
func commitParents(store *ObjectStore, oid string) []string {
	obj := store.Get(oid)
	if obj == nil || obj.Type != ObjCommit {
		return nil
	}
	_, parents := parseCommitLight(obj.Data)
	return parents
}

// isAncestor reports whether ancestor is reachable from descendant by
// following parent links. A commit is its own ancestor.
func isAncestor(store *ObjectStore, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := make(map[string]bool)
	queue := []string{descendant}
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		for _, p := range commitParents(store, oid) {
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

// collectAncestors returns the set of ids reachable from start by
// following parents, including start itself even when it is not a commit.
func collectAncestors(store *ObjectStore, start string) map[string]bool {
	found := make(map[string]bool)
	queue := []string{start}
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		if found[oid] {
			continue
		}
		found[oid] = true
		queue = append(queue, commitParents(store, oid)...)
	}
	return found
}

// mergeBase is the merge-base of two commits: the newest common
// ancestor, found by walking breadth-first from b and returning the
// first commit that is also an ancestor of a. Empty when the histories
// share nothing.
func mergeBase(store *ObjectStore, a, b string) string {
	inA := collectAncestors(store, a)
	seen := make(map[string]bool)
	queue := []string{b}
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if inA[oid] {
			return oid
		}
		queue = append(queue, commitParents(store, oid)...)
	}
	return ""
}
