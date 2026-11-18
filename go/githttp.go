// githttp.go — git smart HTTP: advertisements, ls-refs, fetch,
// receive-pack. Ported from git-http.ts. Protocol v2 when the client sends
// `Git-Protocol: version=2` on upload-pack, v0 otherwise. Whole-object
// packs on write; deltas resolved on read.

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

const agentName = "github-core-forge/1.0.0"
const uploadCaps = "side-band-64k thin-pack ofs-delta shallow multi_ack_detailed agent=" + agentName + " symref=HEAD:refs/heads/" + defaultBranch
const receiveCaps = "report-status side-band-64k delete-refs agent=" + agentName

// ---------------------------------------------------------------- fetch memo

// An upload-pack answer is a pure function of the request and the objects
// reachable from the wants. The clone workload issues 50 identical
// concurrent shallow clones, so the first builds the pack and the other 49
// are served from memory. Bounded (FIFO) so a long trial cannot grow it
// without limit.
type fifoCache struct {
	m     map[string][]byte
	order []string
	max   int
}

func newFifoCache(max int) *fifoCache {
	return &fifoCache{m: make(map[string][]byte), max: max}
}

func (c *fifoCache) get(key string) ([]byte, bool) {
	v, ok := c.m[key]
	return v, ok
}

func (c *fifoCache) put(key string, body []byte) {
	if _, ok := c.m[key]; ok {
		c.m[key] = body
		return
	}
	if len(c.m) >= c.max {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.m, oldest)
	}
	c.m[key] = body
	c.order = append(c.order, key)
}

var fetchCache = newFifoCache(16)
var advCache = newFifoCache(16)

// The v2 capability advertisement never changes; build it once.
var v2AdvBuf []byte

func fetchCacheKey(repo *Repo, wants, haves, shallow []string, deepen *int, v2 bool) string {
	head := repo.Refs["refs/heads/"+repo.DefaultBranch]
	if head == "" {
		head = ZeroOID
	}
	v2s := "0"
	if v2 {
		v2s = "2"
	}
	deepenS := ""
	if deepen != nil {
		deepenS = strconv.Itoa(*deepen)
	}
	return strings.Join([]string{
		strconv.Itoa(repo.ID), head, strconv.Itoa(len(repo.Store.Objects)), v2s, deepenS,
		strings.Join(wants, ","), strings.Join(haves, ","), strings.Join(shallow, ","),
	}, "|")
}

func refsFingerprint(repo *Repo) string {
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

// Follow a tag object chain to its commit, or "".
func peelTag(repo *Repo, oid string) (string, bool) {
	current := oid
	for depth := 0; depth < 10; depth++ {
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

type refLine struct {
	name      string
	oid       string
	peeled    string
	hasPeeled bool
}

func listRefs(repo *Repo) []refLine {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]refLine, 0, len(names))
	for _, name := range names {
		oid := repo.Refs[name]
		peeled, ok := peelTag(repo, oid)
		out = append(out, refLine{name: name, oid: oid, peeled: peeled, hasPeeled: ok})
	}
	return out
}

func headOid(repo *Repo) string {
	if v, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
		return v
	}
	return ZeroOID
}

// v0 advertisement body for a service, with the `# service=` preamble.
func v0Advertisement(repo *Repo, service string) []byte {
	cacheKey := "v0|" + service + "|" + refsFingerprint(repo)
	if hit, ok := advCache.get(cacheKey); ok {
		return hit
	}
	caps := uploadCaps
	if service == "git-receive-pack" {
		caps = receiveCaps
	}
	var lines []pktItem
	lines = append(lines, pktStr("# service="+service+"\n"), pktFlushItem)
	refs := listRefs(repo)
	if len(refs) == 0 {
		lines = append(lines, pktStr(ZeroOID+" capabilities^{}\x00"+caps+"\n"))
	} else {
		head := headOid(repo)
		lines = append(lines, pktStr(head+" HEAD\x00"+caps+"\n"))
		for _, ref := range refs {
			lines = append(lines, pktStr(ref.oid+" "+ref.name+"\n"))
			if ref.hasPeeled {
				lines = append(lines, pktStr(ref.peeled+" "+ref.name+"^{}\n"))
			}
		}
	}
	lines = append(lines, pktFlushItem)
	out := encodePktLines(lines)
	advCache.put(cacheKey, out)
	return out
}

// v2 capability advertisement for upload-pack.
func v2Advertisement() []byte {
	if v2AdvBuf == nil {
		v2AdvBuf = encodePktLines([]pktItem{
			pktStr("version 2\n"),
			pktStr("agent=" + agentName + "\n"),
			pktStr("ls-refs=unborn\n"),
			pktStr("fetch=shallow wait-for-done filter\n"),
			pktStr("object-format=sha1\n"),
			pktFlushItem,
		})
	}
	return v2AdvBuf
}

// ---------------------------------------------------------------- ls-refs (v2)

func handleLsRefs(s *server, repo *Repo, body []byte, w *respWriter, predecoded []PktPacket) {
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
	seenCommand := false
	for _, packet := range packets {
		if packet.Kind != "data" {
			continue
		}
		line := string(packet.Payload)
		if !seenCommand {
			if !strings.HasPrefix(line, "command=ls-refs") {
				panic(fmt.Errorf("not an ls-refs command"))
			}
			seenCommand = true
			continue
		}
		args = append(args, line)
	}
	if !seenCommand {
		panic(fmt.Errorf("ls-refs without a command line"))
	}
	cacheKey := "ls-refs|" + repo.DefaultBranch + "|" + refsFingerprint(repo) + "|" + strings.Join(args, ",")
	if cached, ok := advCache.get(cacheKey); ok {
		s.writeResp(w, 200, "application/x-git-upload-pack-result", nil, cached)
		return
	}
	peel := containsStr(args, "peel\n")
	symrefs := containsStr(args, "symrefs\n")
	unborn := containsStr(args, "unborn\n")
	var prefixes []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "ref-prefix ") {
			prefixes = append(prefixes, strings.TrimSuffix(arg[len("ref-prefix "):], "\n"))
		}
	}
	keep := func(name string) bool {
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

	var lines []pktItem
	head := headOid(repo)
	refs := listRefs(repo)
	if len(refs) == 0 {
		if unborn && keep("HEAD") {
			suffix := ""
			if symrefs {
				suffix = " symref-target:refs/heads/" + repo.DefaultBranch
			}
			lines = append(lines, pktStr("unborn HEAD"+suffix+"\n"))
		}
	} else {
		if keep("HEAD") {
			suffix := ""
			if symrefs {
				suffix = " symref-target:refs/heads/" + repo.DefaultBranch
			}
			lines = append(lines, pktStr(head+" HEAD"+suffix+"\n"))
		}
		for _, ref := range refs {
			if !keep(ref.name) {
				continue
			}
			line := ref.oid + " " + ref.name
			if peel && ref.hasPeeled {
				line += " peeled:" + ref.peeled
			}
			lines = append(lines, pktStr(line+"\n"))
		}
	}
	lines = append(lines, pktFlushItem)
	out := encodePktLines(lines)
	advCache.put(cacheKey, out)
	s.writeResp(w, 200, "application/x-git-upload-pack-result", nil, out)
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func isInf(f float64) bool {
	return math.IsInf(f, 0)
}

// ---------------------------------------------------------------- fetch

type fetchPlan struct {
	acks      []string
	ready     bool
	nak       bool
	shallow   []string
	unshallow []string
	objects   map[string]*GitObject
}

// Shared want/have/shallow/deepen computation for v2 and v0 fetch.
func planFetch(repo *Repo, wants, haves, shallow []string, deepen *int) fetchPlan {
	store := repo.Store
	shallowSet := make(map[string]bool)
	for _, sh := range shallow {
		shallowSet[sh] = true
	}
	var knownWants []string
	for _, w := range wants {
		if store.Get(w) != nil {
			knownWants = append(knownWants, w)
		}
	}

	// Held: everything reachable from the haves, stopping at the client's
	// shallow boundaries (their ancestors are NOT held).
	heldWalk := walkCommits(store, haves, 0, false, shallowSet)
	heldObjects := collectCommitObjects(store, heldWalk.commits)
	for _, have := range haves {
		obj := store.Get(have)
		if obj != nil && obj.Type != ObjCommit {
			heldObjects[have] = obj
		}
	}

	// Sent commits: walk from the wants, depth-limited when deepening.
	var sentWalk walkResult
	if deepen != nil {
		sentWalk = walkCommits(store, knownWants, *deepen-1, true, nil)
	} else {
		sentWalk = walkCommits(store, knownWants, 0, false, nil)
	}
	sentObjects := collectCommitObjects(store, sentWalk.commits)
	for _, want := range knownWants {
		obj := store.Get(want)
		if obj != nil && obj.Type != ObjCommit {
			extra := collectCommitObjects(store, map[string]bool{want: true})
			for oid, o := range extra {
				sentObjects[oid] = o
			}
		}
	}
	// Subtract what the client already holds.
	for oid := range heldObjects {
		delete(sentObjects, oid)
	}

	// Shallow-info: new boundaries are the sent commits whose parents were cut;
	// old shallow commits whose parents are now all sent become unshallow.
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
			allSent := true
			for _, p := range parents {
				if !sentWalk.commits[p] {
					allSent = false
					break
				}
			}
			if allSent {
				unshallowOut = append(unshallowOut, old)
			}
		}
	}

	// Acknowledgements: a have that is an ancestor of (or equal to) a want.
	var acks []string
	if len(haves) > 0 {
		fullWalk := walkCommits(store, knownWants, 0, false, nil)
		for _, h := range haves {
			if fullWalk.commits[h] {
				acks = append(acks, h)
				break
			}
		}
	}
	// Dedup + sort shallow/unshallow (JS: [...new Set(x)].sort()).
	shallowOut = dedupSort(shallowOut)
	unshallowOut = dedupSort(unshallowOut)
	return fetchPlan{
		acks:      acks,
		ready:     len(acks) > 0,
		nak:       len(acks) == 0,
		shallow:   shallowOut,
		unshallow: unshallowOut,
		objects:   sentObjects,
	}
}

func dedupSort(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	seen := make(map[string]bool)
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

func sortedPackObjects(objects map[string]*GitObject) []*GitObject {
	keys := make([]string, 0, len(objects))
	for k := range objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*GitObject, 0, len(keys))
	for _, k := range keys {
		out = append(out, objects[k])
	}
	return out
}

func handleFetchV2(s *server, repo *Repo, body []byte, w *respWriter, predecoded []PktPacket) {
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
	var wants, haves, shallow []string
	var deepen *int
	seenCommand := false
	for _, packet := range packets {
		if packet.Kind != "data" {
			continue
		}
		line := string(packet.Payload)
		if !seenCommand {
			if !strings.HasPrefix(line, "command=fetch") {
				panic(fmt.Errorf("not a fetch command"))
			}
			seenCommand = true
			continue
		}
		if strings.HasPrefix(line, "want ") {
			wants = append(wants, strings.TrimSpace(line[5:]))
		} else if strings.HasPrefix(line, "have ") {
			haves = append(haves, strings.TrimSpace(line[5:]))
		} else if strings.HasPrefix(line, "shallow ") {
			shallow = append(shallow, strings.TrimSpace(line[8:]))
		} else if strings.HasPrefix(line, "deepen ") {
			n, ok := jsNumber(strings.TrimSpace(line[7:]))
			if ok && !isNaN(n) && !isInf(n) && n > 0 {
				d := int(n)
				deepen = &d
			}
		}
	}
	if !seenCommand {
		panic(fmt.Errorf("fetch without a command line"))
	}
	cacheKey := fetchCacheKey(repo, wants, haves, shallow, deepen, true)
	if cached, ok := fetchCache.get(cacheKey); ok {
		s.writeResp(w, 200, "application/x-git-upload-pack-result", nil, cached)
		return
	}
	plan := planFetch(repo, wants, haves, shallow, deepen)

	var parts [][]byte
	var ackLines []pktItem
	ackLines = append(ackLines, pktStr("acknowledgments\n"))
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
	parts = append(parts, encodePktLines(ackLines))
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		var shallowLines []pktItem
		shallowLines = append(shallowLines, pktStr("shallow-info\n"))
		for _, oid := range plan.shallow {
			shallowLines = append(shallowLines, pktStr("shallow "+oid+"\n"))
		}
		for _, oid := range plan.unshallow {
			shallowLines = append(shallowLines, pktStr("unshallow "+oid+"\n"))
		}
		shallowLines = append(shallowLines, pktDelimItem)
		parts = append(parts, encodePktLines(shallowLines))
	}
	if len(plan.objects) > 0 {
		parts = append(parts, encodePktLines([]pktItem{pktStr("packfile\n")}))
		parts = append(parts, sideband(writePack(sortedPackObjects(plan.objects))))
	}
	parts = append(parts, pktFlush)
	out := joinBytes(parts)
	fetchCache.put(cacheKey, out)
	s.writeResp(w, 200, "application/x-git-upload-pack-result", nil, out)
}

func handleFetchV0(s *server, repo *Repo, body []byte, w *respWriter) {
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
		if strings.HasPrefix(line, "want ") {
			// slice(5, 45): the oid only.
			end := 45
			if len(line) < end {
				end = len(line)
			}
			wants = append(wants, strings.TrimSpace(line[5:end]))
		} else if strings.HasPrefix(line, "have ") {
			haves = append(haves, strings.TrimSpace(line[5:]))
		} else if strings.HasPrefix(line, "shallow ") {
			shallow = append(shallow, strings.TrimSpace(line[8:]))
		} else if strings.HasPrefix(line, "deepen ") {
			n, ok := jsNumber(strings.TrimSpace(line[7:]))
			if ok && !isNaN(n) && !isInf(n) && n > 0 {
				d := int(n)
				deepen = &d
			}
		}
	}
	cacheKey := fetchCacheKey(repo, wants, haves, shallow, deepen, false)
	if cached, ok := fetchCache.get(cacheKey); ok {
		s.writeResp(w, 200, "application/x-git-upload-pack-result", nil, cached)
		return
	}
	plan := planFetch(repo, wants, haves, shallow, deepen)
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
	var parts [][]byte
	parts = append(parts, encodePktLines(lines))
	if len(plan.objects) > 0 {
		parts = append(parts, sideband(writePack(sortedPackObjects(plan.objects))))
	}
	parts = append(parts, pktFlush)
	out := joinBytes(parts)
	fetchCache.put(cacheKey, out)
	s.writeResp(w, 200, "application/x-git-upload-pack-result", nil, out)
}

func joinBytes(parts [][]byte) []byte {
	total := 0
	for _, p := range parts {
		total += len(p)
	}
	out := make([]byte, 0, total)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ---------------------------------------------------------------- receive-pack

type pushCommand struct {
	oldOid string
	newOid string
	ref    string
}

type refResult struct {
	ref    string
	ok     bool
	reason string
}

func handleReceivePack(s *server, repo *Repo, body []byte, w *respWriter) {
	report := func(unpackOk bool, unpackDetail string, refs []refResult) {
		var inner []pktItem
		if unpackOk {
			inner = append(inner, pktStr("unpack ok\n"))
		} else {
			inner = append(inner, pktStr("unpack "+unpackDetail+"\n"))
		}
		for _, r := range refs {
			if r.ok {
				inner = append(inner, pktStr("ok "+r.ref+"\n"))
			} else {
				inner = append(inner, pktStr("ng "+r.ref+" "+r.reason+"\n"))
			}
		}
		payload := encodePktLines(inner)
		out := joinBytes([][]byte{sideband(payload), pktFlush})
		s.writeResp(w, 200, "application/x-git-receive-pack-result", nil, out)
	}

	prefix, consumed, err := decodePktPrefix(body)
	if err != nil {
		s.writeText(w, 400, "Bad Request: not a pkt-line stream")
		return
	}
	var commands []pushCommand
	first := true
	cmdRe := regexp.MustCompile(`^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)\s*$`)
	for _, packet := range prefix {
		if packet.Kind == "flush" {
			break
		}
		if packet.Kind != "data" {
			continue
		}
		line := string(packet.Payload)
		if first {
			if nul := strings.IndexByte(line, 0); nul >= 0 {
				line = line[:nul]
			}
			first = false
		}
		if m := cmdRe.FindStringSubmatch(line); m != nil {
			commands = append(commands, pushCommand{oldOid: m[1], newOid: m[2], ref: m[3]})
		}
	}
	packStart := consumed
	if packStart > len(body) {
		packStart = len(body)
	}
	packBytes := body[packStart:]
	if len(packBytes) > 0 {
		result, err := readPack(packBytes, func(oid string) *GitObject { return repo.Store.Get(oid) })
		if err != nil {
			var ng []refResult
			for _, c := range commands {
				ng = append(ng, refResult{ref: c.ref, ok: false, reason: "unpack failed"})
			}
			report(false, err.Error(), ng)
			return
		}
		for oid, obj := range result.Objects {
			repo.Store.PutObject(oid, obj)
		}
	}

	var results []refResult
	for _, cmd := range commands {
		current, hasCurrent := repo.Refs[cmd.ref]
		if !strings.HasPrefix(cmd.ref, "refs/") || !isOid(cmd.newOid) || !isOid(cmd.oldOid) {
			results = append(results, refResult{ref: cmd.ref, ok: false, reason: "invalid ref"})
			continue
		}
		if cmd.newOid == ZeroOID {
			// Delete.
			if !hasCurrent {
				results = append(results, refResult{ref: cmd.ref, ok: false, reason: "remote ref does not exist"})
			} else if cmd.oldOid != ZeroOID && cmd.oldOid != current {
				results = append(results, refResult{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
			} else {
				delete(repo.Refs, cmd.ref)
				results = append(results, refResult{ref: cmd.ref, ok: true})
			}
			continue
		}
		newObj := repo.Store.Get(cmd.newOid)
		if newObj == nil {
			results = append(results, refResult{ref: cmd.ref, ok: false, reason: "unable to resolve new ref"})
			continue
		}
		if cmd.oldOid == ZeroOID {
			// Create.
			if hasCurrent {
				results = append(results, refResult{ref: cmd.ref, ok: false, reason: "already exists"})
			} else {
				repo.Refs[cmd.ref] = cmd.newOid
				results = append(results, refResult{ref: cmd.ref, ok: true})
			}
			continue
		}
		// Update: old must match the tip, and the new commit must descend from it.
		if !hasCurrent {
			results = append(results, refResult{ref: cmd.ref, ok: false, reason: "remote ref does not exist"})
			continue
		}
		if cmd.oldOid != current {
			results = append(results, refResult{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
			continue
		}
		if cmd.newOid == cmd.oldOid {
			results = append(results, refResult{ref: cmd.ref, ok: true})
			continue
		}
		if newObj.Type == ObjCommit {
			if !isAncestor(repo.Store, cmd.oldOid, cmd.newOid) {
				results = append(results, refResult{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
				continue
			}
		} else if newObj.Type != ObjTag {
			results = append(results, refResult{ref: cmd.ref, ok: false, reason: "new ref is not a commit or tag"})
			continue
		}
		repo.Refs[cmd.ref] = cmd.newOid
		results = append(results, refResult{ref: cmd.ref, ok: true})
	}
	report(true, "", results)
}

// ---------------------------------------------------------------- entry point

var gitPathRe = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(.*)$`)

// Handle a git smart-HTTP request. Returns true when the path is a git route
// (handled, even on 404); false when it is not a git path at all.
func (s *server) handleGit(r *http.Request, w *respWriter, pathname string, search queryParams, body []byte) (handled bool) {
	m := gitPathRe.FindStringSubmatch(pathname)
	if m == nil {
		return false
	}
	owner, err1 := jsDecodeURIComponent(m[1])
	repoName, err2 := jsDecodeURIComponent(m[2])
	if err1 != nil || err2 != nil {
		// decodeURIComponent throws in TS -> app catch -> 404. Match that.
		s.writeJSON404(w)
		return true
	}
	rest := m[3]
	method := r.Method
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		s.writeText(w, 404, "Not Found")
		return true
	}
	defer func() {
		// A malformed git request must never take the server down.
		if rec := recover(); rec != nil {
			if !w.wrote {
				s.writeText(w, 400, "Bad Request")
			}
		}
	}()
	if method == "GET" && rest == "info/refs" {
		service, _ := search.Get("service")
		if service == "git-upload-pack" {
			if r.Header.Get("Git-Protocol") == "version=2" {
				s.writeResp(w, 200, "application/x-git-upload-pack-advertisement", nil, v2Advertisement())
			} else {
				s.writeResp(w, 200, "application/x-git-upload-pack-advertisement", nil, v0Advertisement(repo, "git-upload-pack"))
			}
			return true
		}
		if service == "git-receive-pack" {
			s.writeResp(w, 200, "application/x-git-upload-pack-advertisement", nil, v0Advertisement(repo, "git-receive-pack"))
			return true
		}
		s.writeText(w, 400, "Bad Request: unknown service")
		return true
	}
	if method == "POST" && rest == "git-upload-pack" {
		isV2 := r.Header.Get("Git-Protocol") == "version=2"
		if isV2 {
			// Peek at the first pkt-line: ls-refs vs fetch. Decode once here
			// and hand the packets to the handler instead of decoding twice.
			packets, err := decodePktLines(body)
			if err != nil {
				panic(err)
			}
			var text string
			if len(packets) > 0 && packets[0].Kind == "data" {
				text = string(packets[0].Payload)
			}
			if strings.HasPrefix(text, "command=ls-refs") {
				handleLsRefs(s, repo, body, w, packets)
			} else if strings.HasPrefix(text, "command=fetch") {
				handleFetchV2(s, repo, body, w, packets)
			} else {
				panic(fmt.Errorf("unknown v2 command"))
			}
		} else {
			handleFetchV0(s, repo, body, w)
		}
		return true
	}
	if method == "POST" && rest == "git-receive-pack" {
		handleReceivePack(s, repo, body, w)
		return true
	}
	s.writeText(w, 404, "Not Found")
	return true
}
