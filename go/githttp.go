// githttp.go — the git smart-HTTP surface: advertisements, ls-refs, fetch,
// and receive-pack, served under /{owner}/{repo}.git.
//
// The wire dialect is decided per request: a client that announces
// `Git-Protocol: version=2` on upload-pack gets the v2 command protocol,
// everything else speaks the v0 advertisement + want/have exchange. Packs
// are always written as whole objects (no delta search on the hot path);
// packs the client sends may contain deltas and are resolved on read.
//
// Responses here are pure functions of the repo state plus the request, so
// identical clone/fetch requests are memoized in small bounded caches —
// the clone workload fires fifty identical shallow clones at once, and
// forty-nine of them should never rebuild the same pack.

package main

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// agentID is stamped into both capability advertisements.
const agentID = "github-core-forge/1.0.0"

// uploadCapsV0 lists what this upload-pack offers a v0 client. The symref
// capability always points at the default branch.
const uploadCapsV0 = "side-band-64k thin-pack ofs-delta shallow multi_ack_detailed agent=" + agentID + " symref=HEAD:refs/heads/" + defaultBranch

// receiveCapsV0 lists what receive-pack offers: status reporting, side-band
// progress, and ref deletion.
const receiveCapsV0 = "report-status side-band-64k delete-refs agent=" + agentID

// ------------------------------------------------------------------ memo

// memo is a tiny FIFO cache: the newest entries stay, the oldest are evicted
// once the cap is reached. Keys are cheap fingerprints of (repo state,
// request), so a repeated request after the repo changed simply misses.
type memo struct {
	entries map[string][]byte
	order   []string
	cap     int
}

func newMemo(capacity int) *memo {
	return &memo{entries: make(map[string][]byte), cap: capacity}
}

func (m *memo) lookup(key string) ([]byte, bool) {
	body, ok := m.entries[key]
	return body, ok
}

func (m *memo) store(key string, body []byte) {
	if _, ok := m.entries[key]; ok {
		m.entries[key] = body
		return
	}
	if len(m.entries) >= m.cap {
		evict := m.order[0]
		m.order = m.order[1:]
		delete(m.entries, evict)
	}
	m.entries[key] = body
	m.order = append(m.order, key)
}

// fetchMemo answers repeated identical fetches; advertMemo answers repeated
// advertisements and ls-refs replies. Both are bounded so a long trial
// cannot grow them without limit.
var fetchMemo = newMemo(16)
var advertMemo = newMemo(16)

// v2AdvertisementBytes is built once: the v2 capability list never changes.
var v2AdvertisementBytes []byte

// fetchMemoKey fingerprints a fetch request. The head commit and the object
// count stand in for "the repo as it is right now": any push changes one of
// them, so a stale pack can never be served for a fresh repo.
func fetchMemoKey(repo *Repo, wants, haves, shallow []string, deepen *int, v2 bool) string {
	head := repo.Refs["refs/heads/"+repo.DefaultBranch]
	if head == "" {
		head = ZeroOID
	}
	version := "0"
	if v2 {
		version = "2"
	}
	deepenText := ""
	if deepen != nil {
		deepenText = strconv.Itoa(*deepen)
	}
	return strings.Join([]string{
		strconv.Itoa(repo.ID), head, strconv.Itoa(len(repo.Store.Objects)), version, deepenText,
		strings.Join(wants, ","), strings.Join(haves, ","), strings.Join(shallow, ","),
	}, "|")
}

// refsDigest is a stable fingerprint of every ref in the repo, used as a
// cache key for advertisements and ls-refs answers.
func refsDigest(repo *Repo) string {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, name := range names {
		sb.WriteString(name)
		sb.WriteString("=")
		sb.WriteString(repo.Refs[name])
		sb.WriteString(";")
	}
	return sb.String()
}

// ------------------------------------------------------------------ refs

// peelToCommit follows a tag object (through nested tags, bounded) down to
// the commit it annotates. It reports false for anything that is not a tag
// chain ending in a commit.
func peelToCommit(repo *Repo, oid string) (string, bool) {
	current := oid
	for hops := 0; hops < 10; hops++ {
		obj := repo.Store.Get(current)
		if obj == nil || obj.Type != ObjTag {
			return "", false
		}
		tag := parseTag(obj.Data)
		if tag.Type == ObjCommit {
			return tag.Object, true
		}
		current = tag.Object
	}
	return "", false
}

// advertisedRef is one ref line as it will appear on the wire, with the
// peeled commit precomputed for annotated tags.
type advertisedRef struct {
	name      string
	oid       string
	peeled    string
	hasPeeled bool
}

// advertisedRefs lists every ref in the repo, sorted by name, with peeled
// targets resolved up front so the advertisement builders stay simple.
func advertisedRefs(repo *Repo) []advertisedRef {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]advertisedRef, 0, len(names))
	for _, name := range names {
		oid := repo.Refs[name]
		peeled, ok := peelToCommit(repo, oid)
		out = append(out, advertisedRef{name: name, oid: oid, peeled: peeled, hasPeeled: ok})
	}
	return out
}

// tipOfDefault returns the default branch tip, or the zero oid for an
// unborn repo (no commits yet).
func tipOfDefault(repo *Repo) string {
	if tip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
		return tip
	}
	return ZeroOID
}

// v0Advertisement renders the `# service=` preamble plus the ref list for
// one of the two services. An empty repo advertises the zero oid under the
// `capabilities^{}` pseudo-ref so the capabilities still reach the client.
func v0Advertisement(repo *Repo, service string) []byte {
	cacheKey := "v0|" + service + "|" + refsDigest(repo)
	if hit, ok := advertMemo.lookup(cacheKey); ok {
		return hit
	}
	caps := uploadCapsV0
	if service == "git-receive-pack" {
		caps = receiveCapsV0
	}
	lines := []pktItem{
		pktStr("# service=" + service + "\n"),
		pktFlushItem,
	}
	refs := advertisedRefs(repo)
	if len(refs) == 0 {
		lines = append(lines, pktStr(ZeroOID+" capabilities^{}\x00"+caps+"\n"))
	} else {
		lines = append(lines, pktStr(tipOfDefault(repo)+" HEAD\x00"+caps+"\n"))
		for _, ref := range refs {
			lines = append(lines, pktStr(ref.oid+" "+ref.name+"\n"))
			if ref.hasPeeled {
				lines = append(lines, pktStr(ref.peeled+" "+ref.name+"^{}\n"))
			}
		}
	}
	lines = append(lines, pktFlushItem)
	out := encodePktLines(lines)
	advertMemo.store(cacheKey, out)
	return out
}

// v2Advertisement renders the v2 capability list, built once and reused.
func v2Advertisement() []byte {
	if v2AdvertisementBytes == nil {
		v2AdvertisementBytes = encodePktLines([]pktItem{
			pktStr("version 2\n"),
			pktStr("agent=" + agentID + "\n"),
			pktStr("ls-refs=unborn\n"),
			pktStr("fetch=shallow wait-for-done filter\n"),
			pktStr("object-format=sha1\n"),
			pktFlushItem,
		})
	}
	return v2AdvertisementBytes
}

// ---------------------------------------------------------------- ls-refs

// serveLsRefs answers a v2 `command=ls-refs` request. The caller has
// already decoded the pkt-lines once; pass them in so they are not decoded
// a second time.
func serveLsRefs(s *server, repo *Repo, body []byte, w *respWriter, predecoded []PktPacket) {
	var packets []PktPacket
	if predecoded != nil {
		packets = predecoded
	} else {
		var err error
		packets, err = decodePktLines(body)
		if err != nil {
			panic(err)
		}
	}
	var args []string
	sawCommand := false
	for _, packet := range packets {
		if packet.Kind != "data" {
			continue
		}
		line := string(packet.Payload)
		if !sawCommand {
			if !strings.HasPrefix(line, "command=ls-refs") {
				panic(fmt.Errorf("not an ls-refs command"))
			}
			sawCommand = true
			continue
		}
		args = append(args, line)
	}
	if !sawCommand {
		panic(fmt.Errorf("ls-refs without a command line"))
	}
	cacheKey := "ls-refs|" + repo.DefaultBranch + "|" + refsDigest(repo) + "|" + strings.Join(args, ",")
	if cached, ok := advertMemo.lookup(cacheKey); ok {
		s.writeResp(w, 200, "application/x-git-upload-pack-result", nil, cached)
		return
	}
	wantPeel := containsStr(args, "peel\n")
	wantSymrefs := containsStr(args, "symrefs\n")
	wantUnborn := containsStr(args, "unborn\n")
	var prefixes []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "ref-prefix ") {
			prefixes = append(prefixes, strings.TrimSuffix(arg[len("ref-prefix "):], "\n"))
		}
	}
	// A ref (or HEAD) is listed only when it matches at least one requested
	// prefix; with no prefixes everything is listed.
	visible := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	symrefSuffix := ""
	if wantSymrefs {
		symrefSuffix = " symref-target:refs/heads/" + repo.DefaultBranch
	}

	var lines []pktItem
	head := tipOfDefault(repo)
	refs := advertisedRefs(repo)
	if len(refs) == 0 {
		if wantUnborn && visible("HEAD") {
			lines = append(lines, pktStr("unborn HEAD"+symrefSuffix+"\n"))
		}
	} else {
		if visible("HEAD") {
			lines = append(lines, pktStr(head+" HEAD"+symrefSuffix+"\n"))
		}
		for _, ref := range refs {
			if !visible(ref.name) {
				continue
			}
			line := ref.oid + " " + ref.name
			if wantPeel && ref.hasPeeled {
				line += " peeled:" + ref.peeled
			}
			lines = append(lines, pktStr(line+"\n"))
		}
	}
	lines = append(lines, pktFlushItem)
	out := encodePktLines(lines)
	advertMemo.store(cacheKey, out)
	s.writeResp(w, 200, "application/x-git-upload-pack-result", nil, out)
}

// containsStr reports whether the exact argument line is present.
func containsStr(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ fetch

// negotiation is the outcome of want/have analysis: which haves the server
// acknowledges, which shallow boundaries the reply declares, and the exact
// set of objects the pack must carry.
type negotiation struct {
	acks      []string
	ready     bool
	nak       bool
	shallow   []string
	unshallow []string
	objects   map[string]*GitObject
}

// negotiateFetch computes, once, the shared answer for a v2 or v0 fetch.
// Both protocol versions ask the same question — "what do I still need?" —
// and differ only in how the answer is framed on the wire.
func negotiateFetch(repo *Repo, wants, haves, shallow []string, deepen *int) negotiation {
	store := repo.Store
	shallowSet := make(map[string]bool, len(shallow))
	for _, oid := range shallow {
		shallowSet[oid] = true
	}
	// Wants the server cannot resolve are dropped silently; the client only
	// ever asks for objects the advertisement showed it.
	var knownWants []string
	for _, want := range wants {
		if store.Get(want) != nil {
			knownWants = append(knownWants, want)
		}
	}

	// Held: everything reachable from the haves, stopping at the client's
	// shallow boundaries — their ancestors are NOT held, the client does
	// not have them.
	heldWalk := walkCommits(store, haves, 0, false, shallowSet)
	heldObjects := collectCommitObjects(store, heldWalk.commits)
	for _, have := range haves {
		if obj := store.Get(have); obj != nil && obj.Type != ObjCommit {
			heldObjects[have] = obj
		}
	}

	// Sent: the walk from the wants, cut short when the client asked to
	// deepen by a bounded number of commits.
	var sentWalk walkResult
	if deepen != nil {
		sentWalk = walkCommits(store, knownWants, *deepen-1, true, nil)
	} else {
		sentWalk = walkCommits(store, knownWants, 0, false, nil)
	}
	sentObjects := collectCommitObjects(store, sentWalk.commits)
	for _, want := range knownWants {
		if obj := store.Get(want); obj != nil && obj.Type != ObjCommit {
			for oid, extra := range collectCommitObjects(store, map[string]bool{want: true}) {
				sentObjects[oid] = extra
			}
		}
	}
	// Never send what the client already holds.
	for oid := range heldObjects {
		delete(sentObjects, oid)
	}

	// Shallow-info: new boundaries are the sent commits whose parents were
	// cut by the depth limit; an old shallow commit whose parents are now
	// all sent stops being shallow.
	var shallowOut, unshallowOut []string
	if deepen != nil || len(shallow) > 0 {
		for oid := range sentWalk.boundary {
			if sentWalk.commits[oid] {
				shallowOut = append(shallowOut, oid)
			}
		}
		for old := range shallowSet {
			if !sentWalk.commits[old] {
				continue
			}
			obj := store.Get(old)
			if obj == nil || obj.Type != ObjCommit {
				continue
			}
			_, parents := parseCommitLight(obj.Data)
			if len(parents) == 0 {
				continue
			}
			parentsSent := true
			for _, parent := range parents {
				if !sentWalk.commits[parent] {
					parentsSent = false
					break
				}
			}
			if parentsSent {
				unshallowOut = append(unshallowOut, old)
			}
		}
	}

	// Acknowledgements: a have that is an ancestor of (or equal to) a want
	// earns one ACK; the first such have is enough to declare readiness.
	var acks []string
	if len(haves) > 0 {
		fullWalk := walkCommits(store, knownWants, 0, false, nil)
		for _, have := range haves {
			if fullWalk.commits[have] {
				acks = append(acks, have)
				break
			}
		}
	}
	return negotiation{
		acks:      acks,
		ready:     len(acks) > 0,
		nak:       len(acks) == 0,
		shallow:   uniqueSorted(shallowOut),
		unshallow: uniqueSorted(unshallowOut),
		objects:   sentObjects,
	}
}

// uniqueSorted dedups a list and returns it in sorted order (empty stays
// empty, never nil, so callers can range over it freely).
func uniqueSorted(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// sortedObjects flattens an object map into a slice ordered by oid, so the
// pack bytes are deterministic for the same object set.
func sortedObjects(objects map[string]*GitObject) []*GitObject {
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]*GitObject, 0, len(keys))
	for _, key := range keys {
		out = append(out, objects[key])
	}
	return out
}

// parseFetchArgs pulls want/have/shallow/deepen lines out of a decoded
// packet stream. The first line is the command line and is skipped; the
// caller has already verified it. A deepen value must be a positive finite
// number, otherwise it is ignored.
func parseFetchArgs(packets []PktPacket) (wants, haves, shallow []string, deepen *int, sawCommand bool) {
	for _, packet := range packets {
		if packet.Kind != "data" {
			continue
		}
		line := string(packet.Payload)
		if !sawCommand {
			sawCommand = true
			continue
		}
		switch {
		case strings.HasPrefix(line, "want "):
			wants = append(wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			haves = append(haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			shallow = append(shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			if n, ok := jsNumber(strings.TrimSpace(line[7:])); ok && !math.IsNaN(n) && !math.IsInf(n, 0) && n > 0 {
				d := int(n)
				deepen = &d
			}
		}
	}
	return wants, haves, shallow, deepen, sawCommand
}

// serveFetchV2 answers a v2 `command=fetch` request: an acknowledgments
// section, an optional shallow-info section, then the packfile section,
// each delimited, and a final flush.
func serveFetchV2(s *server, repo *Repo, body []byte, w *respWriter, predecoded []PktPacket) {
	var packets []PktPacket
	if predecoded != nil {
		packets = predecoded
	} else {
		var err error
		packets, err = decodePktLines(body)
		if err != nil {
			panic(err)
		}
	}
	first := ""
	for _, packet := range packets {
		if packet.Kind == "data" {
			first = string(packet.Payload)
			break
		}
	}
	if !strings.HasPrefix(first, "command=fetch") {
		panic(fmt.Errorf("not a fetch command"))
	}
	wants, haves, shallow, deepen, sawCommand := parseFetchArgs(packets)
	if !sawCommand {
		panic(fmt.Errorf("fetch without a command line"))
	}
	cacheKey := fetchMemoKey(repo, wants, haves, shallow, deepen, true)
	if cached, ok := fetchMemo.lookup(cacheKey); ok {
		s.writeResp(w, 200, "application/x-git-upload-pack-result", nil, cached)
		return
	}
	plan := negotiateFetch(repo, wants, haves, shallow, deepen)

	var sections [][]byte
	ackLines := []pktItem{pktStr("acknowledgments\n")}
	if plan.nak {
		ackLines = append(ackLines, pktStr("NAK\n"))
	}
	for _, ack := range plan.acks {
		ackLines = append(ackLines, pktStr("ACK "+ack+"\n"))
	}
	if plan.ready {
		ackLines = append(ackLines, pktStr("ready\n"))
	}
	ackLines = append(ackLines, pktDelimItem)
	sections = append(sections, encodePktLines(ackLines))
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		shallowLines := []pktItem{pktStr("shallow-info\n")}
		for _, oid := range plan.shallow {
			shallowLines = append(shallowLines, pktStr("shallow "+oid+"\n"))
		}
		for _, oid := range plan.unshallow {
			shallowLines = append(shallowLines, pktStr("unshallow "+oid+"\n"))
		}
		shallowLines = append(shallowLines, pktDelimItem)
		sections = append(sections, encodePktLines(shallowLines))
	}
	if len(plan.objects) > 0 {
		sections = append(sections, encodePktLines([]pktItem{pktStr("packfile\n")}))
		sections = append(sections, sideband(writePack(sortedObjects(plan.objects))))
	}
	sections = append(sections, pktFlush)
	out := concat(sections)
	fetchMemo.store(cacheKey, out)
	s.writeResp(w, 200, "application/x-git-upload-pack-result", nil, out)
}

// serveFetchV0 answers a v0 upload-pack POST. The framing is flatter than
// v2 — bare NAK/ACK lines, then shallow/unshallow lines, then the pack —
// but the negotiation underneath is the same. Note the first want line
// carries the client's capabilities after a NUL, so only its first 40
// characters (the oid) are read.
func serveFetchV0(s *server, repo *Repo, body []byte, w *respWriter) {
	packets, err := decodePktLines(body)
	if err != nil {
		panic(err)
	}
	var wants, haves, shallow []string
	var deepen *int
	for _, packet := range packets {
		if packet.Kind != "data" {
			continue
		}
		line := string(packet.Payload)
		switch {
		case strings.HasPrefix(line, "want "):
			end := 45
			if len(line) < end {
				end = len(line)
			}
			wants = append(wants, strings.TrimSpace(line[5:end]))
		case strings.HasPrefix(line, "have "):
			haves = append(haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			shallow = append(shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			if n, ok := jsNumber(strings.TrimSpace(line[7:])); ok && !math.IsNaN(n) && !math.IsInf(n, 0) && n > 0 {
				d := int(n)
				deepen = &d
			}
		}
	}
	cacheKey := fetchMemoKey(repo, wants, haves, shallow, deepen, false)
	if cached, ok := fetchMemo.lookup(cacheKey); ok {
		s.writeResp(w, 200, "application/x-git-upload-pack-result", nil, cached)
		return
	}
	plan := negotiateFetch(repo, wants, haves, shallow, deepen)
	var lines []pktItem
	if plan.nak {
		lines = append(lines, pktStr("NAK\n"))
	}
	for _, ack := range plan.acks {
		lines = append(lines, pktStr("ACK "+ack+"\n"))
	}
	for _, oid := range plan.shallow {
		lines = append(lines, pktStr("shallow "+oid+"\n"))
	}
	for _, oid := range plan.unshallow {
		lines = append(lines, pktStr("unshallow "+oid+"\n"))
	}
	sections := [][]byte{encodePktLines(lines)}
	if len(plan.objects) > 0 {
		sections = append(sections, sideband(writePack(sortedObjects(plan.objects))))
	}
	sections = append(sections, pktFlush)
	out := concat(sections)
	fetchMemo.store(cacheKey, out)
	s.writeResp(w, 200, "application/x-git-upload-pack-result", nil, out)
}

// concat joins byte slices into one buffer, pre-sized so the copy runs once.
func concat(parts [][]byte) []byte {
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	out := make([]byte, 0, total)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

// ------------------------------------------------------------ receive-pack

// pushUpdate is one ref command from the client's pkt-line prefix:
// move ref from oldOid to newOid (newOid zero means delete).
type pushUpdate struct {
	oldOid string
	newOid string
	ref    string
}

// refOutcome is one line of the per-ref status report.
type refOutcome struct {
	ref    string
	ok     bool
	reason string
}

// pushCommandRe matches a single ref-update line.
var pushCommandRe = regexp.MustCompile(`^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)\s*$`)

// serveReceivePack handles a git-receive-pack POST: pkt-line ref commands,
// then a packfile. The pack is imported first (objects land in the store
// even for refs that are ultimately refused — that matches git, where the
// objects are already on disk), then each command is applied in order
// under the fast-forward rule. The reply is a side-band report: one
// `unpack ok` line, then one `ok` / `ng` line per ref.
func serveReceivePack(s *server, repo *Repo, body []byte, w *respWriter) {
	report := func(unpackOk bool, unpackDetail string, outcomes []refOutcome) {
		var inner []pktItem
		if unpackOk {
			inner = append(inner, pktStr("unpack ok\n"))
		} else {
			inner = append(inner, pktStr("unpack "+unpackDetail+"\n"))
		}
		for _, outcome := range outcomes {
			if outcome.ok {
				inner = append(inner, pktStr("ok "+outcome.ref+"\n"))
			} else {
				inner = append(inner, pktStr("ng "+outcome.ref+" "+outcome.reason+"\n"))
			}
		}
		out := concat([][]byte{sideband(encodePktLines(inner)), pktFlush})
		s.writeResp(w, 200, "application/x-git-receive-pack-result", nil, out)
	}

	// The command prefix ends at the first flush; everything after it is
	// the packfile, untouched.
	prefix, consumed, err := decodePktPrefix(body)
	if err != nil {
		s.writeText(w, 400, "Bad Request: not a pkt-line stream")
		return
	}
	var commands []pushUpdate
	first := true
	for _, packet := range prefix {
		if packet.Kind == "flush" {
			break
		}
		if packet.Kind != "data" {
			continue
		}
		line := string(packet.Payload)
		if first {
			// The first command line carries the client's capabilities
			// after a NUL byte; the command itself is before it.
			if nul := strings.IndexByte(line, 0); nul >= 0 {
				line = line[:nul]
			}
			first = false
		}
		if m := pushCommandRe.FindStringSubmatch(line); m != nil {
			commands = append(commands, pushUpdate{oldOid: m[1], newOid: m[2], ref: m[3]})
		}
	}
	packStart := consumed
	if packStart > len(body) {
		packStart = len(body)
	}
	if packBytes := body[packStart:]; len(packBytes) > 0 {
		result, err := readPack(packBytes, func(oid string) *GitObject { return repo.Store.Get(oid) })
		if err != nil {
			ng := make([]refOutcome, 0, len(commands))
			for _, cmd := range commands {
				ng = append(ng, refOutcome{ref: cmd.ref, ok: false, reason: "unpack failed"})
			}
			report(false, err.Error(), ng)
			return
		}
		for oid, obj := range result.Objects {
			repo.Store.PutObject(oid, obj)
		}
	}

	// Apply the commands, one at a time. A mixed push applies the
	// fast-forwarding refs and refuses the rest — never all-or-nothing.
	outcomes := make([]refOutcome, 0, len(commands))
	for _, cmd := range commands {
		outcomes = append(outcomes, applyPush(repo, cmd))
	}
	report(true, "", outcomes)
}

// applyPush validates and applies a single ref command. Every update must
// fast-forward: the advertised old tip has to match the ref, and the new
// commit has to descend from it.
func applyPush(repo *Repo, cmd pushUpdate) refOutcome {
	deny := func(reason string) refOutcome {
		return refOutcome{ref: cmd.ref, ok: false, reason: reason}
	}
	allow := func() refOutcome {
		return refOutcome{ref: cmd.ref, ok: true}
	}
	if !strings.HasPrefix(cmd.ref, "refs/") || !isOid(cmd.newOid) || !isOid(cmd.oldOid) {
		return deny("invalid ref")
	}
	current, hasCurrent := repo.Refs[cmd.ref]
	if cmd.newOid == ZeroOID {
		// Delete.
		if !hasCurrent {
			return deny("remote ref does not exist")
		}
		if cmd.oldOid != ZeroOID && cmd.oldOid != current {
			return deny("non-fast-forward")
		}
		delete(repo.Refs, cmd.ref)
		return allow()
	}
	newObj := repo.Store.Get(cmd.newOid)
	if newObj == nil {
		return deny("unable to resolve new ref")
	}
	if cmd.oldOid == ZeroOID {
		// Create.
		if hasCurrent {
			return deny("already exists")
		}
		repo.Refs[cmd.ref] = cmd.newOid
		return allow()
	}
	// Update.
	if !hasCurrent {
		return deny("remote ref does not exist")
	}
	if cmd.oldOid != current {
		return deny("non-fast-forward")
	}
	if cmd.newOid == cmd.oldOid {
		return allow()
	}
	switch newObj.Type {
	case ObjCommit:
		if !isAncestor(repo.Store, cmd.oldOid, cmd.newOid) {
			return deny("non-fast-forward")
		}
	case ObjTag:
		// Retargeting a ref at a tag is allowed; anything else is not.
	default:
		return deny("new ref is not a commit or tag")
	}
	repo.Refs[cmd.ref] = cmd.newOid
	return allow()
}

// ------------------------------------------------------------------ entry

var gitPathRe = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(.*)$`)

// handleGit routes one git smart-HTTP request. It returns true when the
// path is a git route at all (handled, even on 404); false when the path
// belongs to another surface. A malformed git request never takes the
// server down: it answers 400 and the repo survives.
func (s *server) handleGit(r *http.Request, w *respWriter, pathname string, search queryParams, body []byte) (handled bool) {
	m := gitPathRe.FindStringSubmatch(pathname)
	if m == nil {
		return false
	}
	owner, errOwner := jsDecodeURIComponent(m[1])
	repoName, errRepo := jsDecodeURIComponent(m[2])
	if errOwner != nil || errRepo != nil {
		// A bad escape throws in the reference server and its app-level
		// catch answers 404. Match that.
		s.writeJSON404(w)
		return true
	}
	tail := m[3]
	method := r.Method
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		s.writeText(w, 404, "Not Found")
		return true
	}
	defer func() {
		if rec := recover(); rec != nil {
			if !w.wrote {
				s.writeText(w, 400, "Bad Request")
			}
		}
	}()
	if method == "GET" && tail == "info/refs" {
		service, _ := search.Get("service")
		switch service {
		case "git-upload-pack":
			if r.Header.Get("Git-Protocol") == "version=2" {
				s.writeResp(w, 200, "application/x-git-upload-pack-advertisement", nil, v2Advertisement())
			} else {
				s.writeResp(w, 200, "application/x-git-upload-pack-advertisement", nil, v0Advertisement(repo, "git-upload-pack"))
			}
		case "git-receive-pack":
			s.writeResp(w, 200, "application/x-git-receive-pack-advertisement", nil, v0Advertisement(repo, "git-receive-pack"))
		default:
			s.writeText(w, 400, "Bad Request: unknown service")
		}
		return true
	}
	if method == "POST" && tail == "git-upload-pack" {
		if r.Header.Get("Git-Protocol") == "version=2" {
			// Decode the packet stream once here and hand it to the
			// command handler, so ls-refs/fetch never decode twice.
			// The first data line names the command.
			packets, err := decodePktLines(body)
			if err != nil {
				panic(err)
			}
			command := ""
			for _, packet := range packets {
				if packet.Kind == "data" {
					command = string(packet.Payload)
					break
				}
			}
			switch {
			case strings.HasPrefix(command, "command=ls-refs"):
				serveLsRefs(s, repo, body, w, packets)
			case strings.HasPrefix(command, "command=fetch"):
				serveFetchV2(s, repo, body, w, packets)
			default:
				panic(fmt.Errorf("unknown v2 command"))
			}
		} else {
			serveFetchV0(s, repo, body, w)
		}
		return true
	}
	if method == "POST" && tail == "git-receive-pack" {
		serveReceivePack(s, repo, body, w)
		return true
	}
	s.writeText(w, 404, "Not Found")
	return true
}
