// gitutil.go — the git object layer, written from scratch for this entry.
//
// No git binary and no third-party library: SHA-1 object ids, loose-object
// hashing, pkt-line framing, packfile v2 read and write, tree / commit /
// tag build and parse, and reachability over an in-memory object store.
//
// Design notes (see docs/DECISIONS.md in the reference tree):
//   - Packs are written as whole zlib objects only, never deltas (D1).
//     Deltas arriving from a client are still resolved on read, thin
//     packs included, so pushes from real git clients stay safe.
//   - Everything is in memory; a request never touches the disk (D2).
//   - inflateOne (D12) measures a zlib stream's length exactly with a
//     byte-counting reader: the flate decoder reads one byte at a time
//     from an io.ByteReader and stops at the stream's end, so the count
//     after the read is the consumed length. One inflate, no re-deflate,
//     no search — the same answer for any encoder, ours or a client's.

//   - Side-band frames carry 65,515 data bytes (D11): a pkt-line tops
//     out at 65,520 bytes including its 4-byte header, and the band
//     byte eats one more.

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

// ZeroOID is the all-zero object id: "no object", used for unborn refs.
const ZeroOID = "0000000000000000000000000000000000000000"

// oidLen is the length of a hex object id (40 chars = 20 raw bytes).
const oidLen = 40

// ObjectType is the kind of a git object: commit, tree, blob or tag.
type ObjectType string

const (
	ObjCommit ObjectType = "commit"
	ObjTree   ObjectType = "tree"
	ObjBlob   ObjectType = "blob"
	ObjTag    ObjectType = "tag"
)

// GitObject is one git object: its type plus its raw (uncompressed) bytes.
// The id is not stored here; it is always hashObject(Type, Data).
type GitObject struct {
	Type ObjectType
	Data []byte
}

var oidRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// isOid reports whether v looks like a lowercase hex object id.
func isOid(v string) bool { return oidRe.MatchString(v) }

// isOidValue is isOid for a value of unknown JSON type: only a string can
// be an id, anything else is not one.
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

// ---------------------------------------------------------------- pkt-line
//
// A pkt-line is a 4-hex-digit length (itself included) followed by the
// payload. "0000" is a flush packet, "0001" a delimiter packet.

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// pktLine frames one text line (usually ending in "\n") as a pkt-line.
func pktLine(text string) []byte {
	return pktData([]byte(text))
}

// pktData frames arbitrary bytes as one pkt-line.
func pktData(payload []byte) []byte {
	head := fmt.Sprintf("%04x", len(payload)+4)
	out := make([]byte, 0, len(payload)+4)
	out = append(out, head...)
	return append(out, payload...)
}

// pktItem is one element of a pkt-line stream being encoded.
type pktItem struct {
	kind int // 0 = text line, 1 = raw bytes, 2 = flush, 3 = delimiter
	s    string
	b    []byte
}

func pktStr(s string) pktItem { return pktItem{kind: 0, s: s} }
func pktRaw(b []byte) pktItem { return pktItem{kind: 1, b: b} }

var pktFlushItem = pktItem{kind: 2}
var pktDelimItem = pktItem{kind: 3}

// encodePktLines renders a list of lines, raw buffers, flush and delimiter
// markers into a single pkt-line byte stream.
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

// PktPacket is one decoded pkt-line: a data packet, a flush, or a delimiter.
type PktPacket struct {
	Kind    string // "data" | "flush" | "delim"
	Payload []byte
}

// decodePktLines decodes a whole pkt-line stream; any malformed framing is
// an error.
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
// including its 4-byte header, and the band byte takes one more (D11).
// An empty payload still produces one frame holding just the band byte,
// so the receiver sees the stream end rather than nothing at all.
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

// Identity is one author/committer line: name, email and a Unix timestamp.
// The timezone is parsed but deliberately dropped — identities here are
// always rendered in UTC (see formatIdentity).
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

// identityRe matches "Name <email> 1234567890 +0000". The name part is
// greedy, so a "<" inside the name still resolves to the last bracket pair.
var identityRe = regexp.MustCompile(`^(.*) <([^>]*)> (\d+) ([+-]\d{4})$`)

// parseIdentity parses one "Name <email> ts tz" line. A line that does not
// match is kept whole as the name, with no email and a zero timestamp.
func parseIdentity(line string) Identity {
	m := identityRe.FindStringSubmatch(line)
	if m == nil {
		return Identity{Name: line, Email: "", TS: 0}
	}
	ts, _ := strconv.ParseInt(m[3], 10, 64)
	return Identity{Name: m[1], Email: m[2], TS: ts}
}

// formatIdentity renders an identity the way git does, always in +0000.
func formatIdentity(id Identity) string {
	return fmt.Sprintf("%s <%s> %d +0000", id.Name, id.Email, id.TS)
}

// splitCommitHead splits raw commit bytes at the first blank line into the
// header block and the message. With no blank line the whole thing is the
// header and the message is empty.
func splitCommitHead(data []byte) (head, message string) {
	text := string(data)
	if i := strings.Index(text, "\n\n"); i >= 0 {
		return text[:i], text[i+2:]
	}
	return text, ""
}

// headerLines walks the lines of a header block, skipping continuation
// lines (those starting with a space belong to the previous header).
func headerLines(head string, fn func(line string)) {
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(line, " ") {
			continue
		}
		fn(line)
	}
}

// parseCommit parses a whole commit object: tree, parents, author,
// committer and message.
func parseCommit(data []byte) CommitInfo {
	head, message := splitCommitHead(data)
	c := CommitInfo{Message: message}
	headerLines(head, func(line string) {
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

// parseCommitLight is the hot-path commit parse: tree and parents only, no
// identity regexes and no message handling. Results are byte-identical to
// parseCommit for the two fields it returns. It scans with IndexByte
// instead of splitting, so it never allocates a line slice.
func parseCommitLight(data []byte) (tree string, parents []string) {
	head, _ := splitCommitHead(data)
	for start := 0; start <= len(head); {
		nl := strings.IndexByte(head[start:], '\n')
		var line string
		if nl < 0 {
			line = head[start:]
		} else {
			line = head[start : start+nl]
		}
		if len(line) > 0 && line[0] != ' ' {
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

// buildCommit renders a commit object: tree line, one parent line each,
// author, committer, a blank line, then the message verbatim.
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

// ---------------------------------------------------------------- trees

// TreeEntry is one entry of a tree object: mode ("100644", "40000", ...),
// name, and the hex id of the child object.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// parseTree parses raw tree bytes: repeating "<mode> <name>\0<20 raw bytes>".
// Entries come back in file order; callers that need git order sort them.
func parseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	pos := 0
	for pos < len(data) {
		space := bytes.IndexByte(data[pos:], ' ')
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
		entries = append(entries, TreeEntry{
			Mode: string(data[pos:space]),
			Name: string(data[space+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		pos = nul + 21
	}
	return entries, nil
}

// treeSortKey is the name git sorts on: directories compare as if their
// name ended in "/".
func treeSortKey(e TreeEntry) string {
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
		return treeSortKey(sorted[i]) < treeSortKey(sorted[j])
	})
	var out bytes.Buffer
	for _, e := range sorted {
		fmt.Fprintf(&out, "%s %s\x00", e.Mode, e.Name)
		raw, _ := hex.DecodeString(e.OID)
		out.Write(raw)
	}
	return out.Bytes()
}

// ---------------------------------------------------------------- tags

// TagInfo is the parsed form of an annotated tag object. Tagger is nil
// for a lightweight-style tag object with no tagger line.
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
	head, message := splitCommitHead(data)
	t := TagInfo{Type: ObjCommit, Message: message}
	headerLines(head, func(line string) {
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

// packTypeNum maps an object type to its packfile type number; packTypeName
// is the reverse lookup. Types 6 (OFS_DELTA) and 7 (REF_DELTA) only ever
// appear on read — this writer never emits them (D1).
var packTypeNum = map[ObjectType]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var packTypeName = map[int]ObjectType{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

// packObjectHeader encodes one entry header: the type number and the
// uncompressed size, size first in 4 low bits, then 7 bits per byte,
// little-endian, with the high bit set on every byte but the last.
func packObjectHeader(typeNum, size int) []byte {
	first := (size & 0x0f) | (typeNum << 4)
	rest := size >> 4
	if rest > 0 {
		first |= 0x80
	}
	header := []byte{byte(first)}
	for rest > 0 {
		b := rest & 0x7f
		rest >>= 7
		if rest > 0 {
			b |= 0x80
		}
		header = append(header, byte(b))
	}
	return header
}

// writePack renders a packfile v2 of whole objects only (no deltas, D1):
// "PACK", version 2, object count, one header + zlib stream per object,
// then the SHA-1 trailer over everything before it.
func writePack(objects []*GitObject) []byte {
	var body bytes.Buffer
	head := make([]byte, 12)
	copy(head[0:4], "PACK")
	putU32BE(head[4:8], 2)
	putU32BE(head[8:12], uint32(len(objects)))
	body.Write(head)
	for _, obj := range objects {
		body.Write(packObjectHeader(packTypeNum[obj.Type], len(obj.Data)))
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

// streamCounter is a reader over a byte slice that counts every byte the
// decoder pulls. It implements io.ByteReader, so compress/flate reads one
// byte at a time and stops exactly at the end of the deflate stream;
// pos after the read is therefore the exact number of input bytes the
// stream consumed. Read is present only so zlib.NewReader accepts the
// counter as an io.Reader — flate prefers ReadByte when it exists.
type streamCounter struct {
	b   []byte
	pos int
}

func (c *streamCounter) ReadByte() (byte, error) {
	if c.pos >= len(c.b) {
		return 0, io.EOF
	}
	v := c.b[c.pos]
	c.pos++
	return v, nil
}

func (c *streamCounter) Read(p []byte) (int, error) {
	if c.pos >= len(c.b) {
		return 0, io.EOF
	}
	n := copy(p, c.b[c.pos:])
	c.pos += n
	return n, nil
}

// inflateOne inflates exactly one zlib stream starting at pos in buf and
// returns the decoded bytes plus how many input bytes the stream used.
// A single inflate through the counting reader gives both: the decoder
// stops at the stream's end, so the counter holds the consumed length
// with no re-compression and no search, for any encoder.
func inflateOne(buf []byte, pos int) (data []byte, consumed int, err error) {
	if pos < 0 || pos >= len(buf) {
		return nil, 0, errors.New("pack zlib stream truncated")
	}
	ctr := &streamCounter{b: buf[pos:]}
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

// deltaReader walks a delta stream one byte at a time, tracking position
// so the error paths can say exactly what ran out.
type deltaReader struct {
	delta []byte
	pos   int
}

func (r *deltaReader) byte() (byte, error) {
	if r.pos >= len(r.delta) {
		return 0, errors.New("delta stream truncated")
	}
	b := r.delta[r.pos]
	r.pos++
	return b, nil
}

// varint reads a little-endian base-128 varint: 7 payload bits per byte,
// high bit set while more bytes follow.
func (r *deltaReader) varint() (int, error) {
	out := 0
	shift := uint(0)
	for {
		b, err := r.byte()
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

// applyDelta applies a git delta (OFS/REF payload) to its base object and
// returns the reconstructed object bytes. Opcode layout: a byte with the
// high bit set copies from the base (bits 0-3 select offset bytes, bits
// 4-6 select size bytes, a zero size means 0x10000); any other nonzero
// byte inserts that many literal bytes; opcode 0 is invalid. Both sizes
// are checked against the stream so a corrupt delta fails loudly instead
// of silently producing wrong bytes.
func applyDelta(base []byte, delta []byte) ([]byte, error) {
	r := &deltaReader{delta: delta}
	sourceSize, err := r.varint()
	if err != nil {
		return nil, err
	}
	targetSize, err := r.varint()
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
	for r.pos < len(delta) {
		op, err := r.byte()
		if err != nil {
			return nil, err
		}
		switch {
		case op&0x80 != 0:
			offset, size := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					b, err := r.byte()
					if err != nil {
						return nil, err
					}
					offset |= int(b) << (8 * uint(i))
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					b, err := r.byte()
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
			if r.pos+int(op) > len(delta) {
				return nil, errors.New("delta insert truncated")
			}
			out.Write(delta[r.pos : r.pos+int(op)])
			r.pos += int(op)
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

// readPackEntryHead parses one pack entry header at pos (type number and
// uncompressed size, see packObjectHeader) and returns where the entry
// body starts.
func readPackEntryHead(buf []byte, pos, end int) (typeNum, size, next int, err error) {
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

// readPackOfsOffset parses an OFS_DELTA base offset: the first byte holds
// 7 bits, and every continuation byte adds its 7 bits after shifting the
// accumulated value left by 7 and adding one — the encoding that keeps
// the offset strictly increasing with the byte length.
func readPackOfsOffset(buf []byte, pos, end int) (offset, next int, err error) {
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
// entries into whole objects. resolveBase supplies a base object the pack
// omits — that is what makes thin packs work: the base is already in the
// receiving store. The SHA-1 trailer is verified before any entry is
// trusted, and each entry's inflated size is checked against its header,
// so a corrupt pack fails here instead of poisoning the store.
func readPack(buf []byte, resolveBase func(oid string) *GitObject) (PackReadResult, error) {
	if len(buf) < 12 || string(buf[0:4]) != "PACK" {
		return PackReadResult{}, errors.New("not a packfile")
	}
	version := u32BE(buf[4:8])
	if version != 2 && version != 3 {
		return PackReadResult{}, fmt.Errorf("unsupported pack version %d", version)
	}
	if len(buf) < 32 {
		return PackReadResult{}, errors.New("packfile too short")
	}
	if sha1Hex(buf[:len(buf)-20]) != hex.EncodeToString(buf[len(buf)-20:]) {
		return PackReadResult{}, errors.New("packfile checksum mismatch")
	}
	count := u32BE(buf[8:12])
	objects := make(map[string]*GitObject)
	byOffset := make(map[int]*GitObject)
	deltaCount := 0
	pos := 12
	end := len(buf) - 20
	for i := uint32(0); i < count; i++ {
		entryStart := pos
		typeNum, size, next, err := readPackEntryHead(buf, pos, end)
		if err != nil {
			return PackReadResult{}, err
		}
		pos = next
		baseOffset := -1
		baseOid := ""
		switch typeNum {
		case 6: // OFS_DELTA: base is an earlier entry, by back-offset.
			var back int
			back, pos, err = readPackOfsOffset(buf, pos, end)
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
			resolved, err := applyDelta(base.Data, data)
			if err != nil {
				return PackReadResult{}, err
			}
			obj := &GitObject{Type: base.Type, Data: resolved}
			oid := hashObject(obj.Type, resolved)
			objects[oid] = obj
			byOffset[entryStart] = obj
			continue
		}
		typ, ok := packTypeName[typeNum]
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

// ---------------------------------------------------------------- object store

// ObjectStore is an in-memory object store: loose objects keyed by id,
// nothing on disk (D2). Put and PutObject are idempotent — storing an
// object that is already there is a no-op.
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

// ---------------------------------------------------------------- reachability

type walkResult struct {
	commits  map[string]bool
	boundary map[string]bool
}

// walkCommits walks commits from tips, breadth-first, and returns the ids
// reached plus the boundary commits — reached commits whose parents were
// NOT walked, because a stopAt boundary was hit or the depth limit ran
// out. maxDepth counts edges from the tips (0 = tips only); it applies
// only when hasMaxDepth is set.
func walkCommits(store *ObjectStore, tips []string, maxDepth int, hasMaxDepth bool, stopAt map[string]bool) walkResult {
	commits := make(map[string]bool)
	boundary := make(map[string]bool)
	type step struct {
		oid   string
		depth int
	}
	queue := make([]step, 0, len(tips))
	for _, tip := range tips {
		if isOid(tip) && !commits[tip] {
			queue = append(queue, step{oid: tip, depth: 0})
		}
	}
	for head := 0; head < len(queue); head++ {
		cur := queue[head]
		if commits[cur.oid] {
			continue
		}
		obj := store.Get(cur.oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		commits[cur.oid] = true
		if stopAt != nil && stopAt[cur.oid] {
			boundary[cur.oid] = true
			continue
		}
		_, parents := parseCommitLight(obj.Data)
		if hasMaxDepth && cur.depth >= maxDepth {
			if len(parents) > 0 {
				boundary[cur.oid] = true
			}
			continue
		}
		for _, p := range parents {
			if !commits[p] {
				queue = append(queue, step{oid: p, depth: cur.depth + 1})
			}
		}
	}
	return walkResult{commits: commits, boundary: boundary}
}

// expandTree adds a tree and everything under it to out, iteratively:
// subtrees are expanded, blobs (and anything else) are added as-is.
// A non-tree root is added but not expanded; a missing object is skipped.
func expandTree(store *ObjectStore, out map[string]*GitObject, root string) {
	stack := []string{root}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
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
				stack = append(stack, e.OID)
			} else {
				out[e.OID] = child
			}
		}
	}
}

// collectReachable returns every object reachable from commitOids:
// the commits and all their ancestors, their trees and blobs, and —
// for a tag id — the tag itself plus whatever it points at.
func collectReachable(store *ObjectStore, commitOids map[string]bool) map[string]*GitObject {
	out := make(map[string]*GitObject)
	commitStack := []string{}
	for oid := range commitOids {
		obj := store.Get(oid)
		if obj == nil {
			continue
		}
		switch obj.Type {
		case ObjCommit:
			commitStack = append(commitStack, oid)
		case ObjTag:
			out[oid] = obj
			tag := parseTag(obj.Data)
			switch tag.Type {
			case ObjCommit:
				commitStack = append(commitStack, tag.Object)
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
	for len(commitStack) > 0 {
		oid := commitStack[len(commitStack)-1]
		commitStack = commitStack[:len(commitStack)-1]
		obj := store.Get(oid)
		if obj == nil || obj.Type != ObjCommit || out[oid] != nil {
			continue
		}
		out[oid] = obj
		tree, parents := parseCommitLight(obj.Data)
		expandTree(store, out, tree)
		commitStack = append(commitStack, parents...)
	}
	return out
}

// collectCommitObjects returns the objects of exactly the given commits —
// each commit plus its tree and blobs — WITHOUT following parents. A tag
// id contributes the tag plus its target's objects (for a commit target,
// the target commit and its tree).
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

// mergeBase is the merge-base of two commits: the newest common ancestor,
// found by walking breadth-first from b and returning the first commit
// that is also an ancestor of a. Empty when the histories share nothing.
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

// ---------------------------------------------------------------- byte order

// putU32BE writes v as 4 big-endian bytes into b (which must hold 4).
func putU32BE(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

// u32BE reads 4 big-endian bytes as a uint32.
func u32BE(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
