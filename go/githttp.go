// githttp.go — git smart HTTP: the wire end of the forge.
//
// A client reaches a repository at /{owner}/{repo}.git. Two dialects are
// spoken here and the request itself picks one: a Git-Protocol header of
// "version=2" on upload-pack selects the v2 command protocol (ls-refs and
// fetch as named commands with delimited sections); anything else speaks
// v0 (a "# service=" advertisement followed by the classic want/have
// exchange). Receive-pack is v0 only.
//
// Everything served here comes from the in-memory store — no disk reads,
// no git binary, no network — and every pack written out is made of whole
// objects, never deltas. Packs arriving from a client may carry deltas;
// those are resolved on the way in by readPack.
//
// A request that cannot be parsed never takes the process down: the
// handler answers 400 and the repository is exactly as it was.

package main

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// forgeAgent is stamped into both capability advertisements so a client
// can tell what answered it.
const forgeAgent = "github-core-forge/1.0.0"

// uploadCapsV0 is what an upload-pack advertisement offers a v0 client.
// The symref always names the default branch.
const uploadCapsV0 = "side-band-64k thin-pack ofs-delta shallow multi_ack_detailed agent=" + forgeAgent + " symref=HEAD:refs/heads/" + defaultBranch

// receiveCapsV0 is what a receive-pack advertisement offers: per-ref
// status reporting, side-band progress, and ref deletion.
const receiveCapsV0 = "report-status side-band-64k delete-refs agent=" + forgeAgent

// ---------------------------------------------------------------- refs

// refLine is one ref as it will be advertised, with the peeled commit
// already resolved for annotated tags.
type refLine struct {
	name      string
	oid       string
	peeled    string
	hasPeeled bool
}

// followTag walks a chain of tag objects (bounded, so a pathological
// store cannot loop forever) and returns the commit at the end of it.
func followTag(repo *Repo, oid string) (string, bool) {
	target := oid
	for hops := 0; hops < 10; hops++ {
		obj := repo.Store.Get(target)
		if obj == nil || obj.Type != ObjTag {
			return "", false
		}
		tag := parseTag(obj.Data)
		if tag.Type == ObjCommit {
			return tag.Object, true
		}
		target = tag.Object
	}
	return "", false
}

// listRefs returns every ref in the repo, sorted by name, with peeled
// targets resolved up front.
func listRefs(repo *Repo) []refLine {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := make([]refLine, 0, len(names))
	for _, name := range names {
		oid := repo.Refs[name]
		peeled, ok := followTag(repo, oid)
		lines = append(lines, refLine{name: name, oid: oid, peeled: peeled, hasPeeled: ok})
	}
	return lines
}

// defaultTip is the default branch tip, or the zero id while the repo
// still has no commits.
func defaultTip(repo *Repo) string {
	if tip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
		return tip
	}
	return ZeroOID
}

// ---------------------------------------------------------------- advertisements

// v0Advertisement renders the "# service=" preamble plus the ref list
// for one of the two services. An empty repo advertises the zero id under
// the capabilities^{} pseudo-ref, which is how the capabilities still
// reach the client when there is nothing else to list.
func v0Advertisement(repo *Repo, service string) []byte {
	caps := uploadCapsV0
	if service == "git-receive-pack" {
		caps = receiveCapsV0
	}
	items := []pktItem{
		pktStr("# service=" + service + "\n"),
		pktFlushItem,
	}
	refs := listRefs(repo)
	if len(refs) == 0 {
		items = append(items, pktStr(ZeroOID+" capabilities^{}\x00"+caps+"\n"))
	} else {
		items = append(items, pktStr(defaultTip(repo)+" HEAD\x00"+caps+"\n"))
		for _, ref := range refs {
			items = append(items, pktStr(ref.oid+" "+ref.name+"\n"))
			if ref.hasPeeled {
				items = append(items, pktStr(ref.peeled+" "+ref.name+"^{}\n"))
			}
		}
	}
	items = append(items, pktFlushItem)
	return encodePktLines(items)
}

// v2Advertisement renders the v2 capability list. It never changes, so
// it is rendered once and reused.
var v2AdvertisementBody []byte

func v2Advertisement() []byte {
	if v2AdvertisementBody == nil {
		v2AdvertisementBody = encodePktLines([]pktItem{
			pktStr("version 2\n"),
			pktStr("agent=" + forgeAgent + "\n"),
			pktStr("ls-refs=unborn\n"),
			pktStr("fetch=shallow wait-for-done filter\n"),
			pktStr("object-format=sha1\n"),
			pktFlushItem,
		})
	}
	return v2AdvertisementBody
}

// containsStr reports whether the exact string is present in the list.
// Shared with the GraphQL and MCP surfaces, which use it for the same
// exact-match test on their own argument lists.
func containsStr(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- ls-refs

// lsRefsOptions is what a v2 ls-refs request asked for.
type lsRefsOptions struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
}

// parseLsRefsArgs reads the argument lines out of an already-decoded
// packet stream. The first data line is the command itself; agent and
// object-format lines are client chatter and are skipped.
func parseLsRefsArgs(packets []PktPacket) (lsRefsOptions, bool) {
	var opts lsRefsOptions
	seenCommand := false
	for _, packet := range packets {
		if packet.Kind != "data" {
			continue
		}
		line := string(packet.Payload)
		if !seenCommand {
			if !strings.HasPrefix(line, "command=ls-refs") {
				return opts, false
			}
			seenCommand = true
			continue
		}
		switch {
		case line == "peel\n":
			opts.peel = true
		case line == "symrefs\n":
			opts.symrefs = true
		case line == "unborn\n":
			opts.unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			opts.prefixes = append(opts.prefixes, strings.TrimSuffix(line[len("ref-prefix "):], "\n"))
		}
	}
	return opts, seenCommand
}

// serveLsRefs answers a v2 command=ls-refs request.
func serveLsRefs(s *server, repo *Repo, w *respWriter, packets []PktPacket) {
	opts, ok := parseLsRefsArgs(packets)
	if !ok {
		panic(fmt.Errorf("not an ls-refs command"))
	}
	// A ref is listed only when it matches at least one requested prefix;
	// with no prefixes everything is listed.
	visible := func(name string) bool {
		if len(opts.prefixes) == 0 {
			return true
		}
		for _, prefix := range opts.prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	symrefText := ""
	if opts.symrefs {
		symrefText = " symref-target:refs/heads/" + repo.DefaultBranch
	}
	var items []pktItem
	head := defaultTip(repo)
	refs := listRefs(repo)
	if len(refs) == 0 {
		if opts.unborn && visible("HEAD") {
			items = append(items, pktStr("unborn HEAD"+symrefText+"\n"))
		}
	} else {
		if visible("HEAD") {
			items = append(items, pktStr(head+" HEAD"+symrefText+"\n"))
		}
		for _, ref := range refs {
			if !visible(ref.name) {
				continue
			}
			line := ref.oid + " " + ref.name
			if opts.peel && ref.hasPeeled {
				line += " peeled:" + ref.peeled
			}
			items = append(items, pktStr(line+"\n"))
		}
	}
	items = append(items, pktFlushItem)
	s.writeResp(w, 200, "application/x-git-upload-pack-result", nil, encodePktLines(items))
}

// ---------------------------------------------------------------- fetch

// fetchPlan is the complete answer to one fetch: which haves the server
// acknowledges, which shallow boundaries the reply declares, and the
// exact set of objects the pack must carry.
type fetchPlan struct {
	acks      []string
	ready     bool
	nak       bool
	shallow   []string
	unshallow []string
	objects   map[string]*GitObject
}

// dedupSorted returns the list with duplicates removed, in sorted order.
// An empty list stays empty (never nil) so callers can range over it.
func dedupSorted(in []string) []string {
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

// planFetch works out, once, the shared answer for a v2 or a v0 fetch.
// Both protocol versions ask the same question — "what do I still need?"
// — and differ only in how the answer is framed on the wire.
func planFetch(repo *Repo, wants, haves, shallow []string, deepen *int) fetchPlan {
	store := repo.Store
	shallowSet := make(map[string]bool, len(shallow))
	for _, oid := range shallow {
		shallowSet[oid] = true
	}
	// Wants the server cannot resolve are dropped silently; the client
	// only ever asks for objects the advertisement showed it.
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
	held := collectCommitObjects(store, heldWalk.commits)
	for _, have := range haves {
		if obj := store.Get(have); obj != nil && obj.Type != ObjCommit {
			held[have] = obj
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
	sent := collectCommitObjects(store, sentWalk.commits)
	for _, want := range knownWants {
		if obj := store.Get(want); obj != nil && obj.Type != ObjCommit {
			for oid, extra := range collectCommitObjects(store, map[string]bool{want: true}) {
				sent[oid] = extra
			}
		}
	}
	// Never send what the client already holds.
	for oid := range held {
		delete(sent, oid)
	}

	// Shallow-info: new boundaries are the sent commits whose parents
	// were cut by the depth limit; an old shallow commit whose parents
	// are now all sent stops being shallow.
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
			allParentsSent := true
			for _, parent := range parents {
				if !sentWalk.commits[parent] {
					allParentsSent = false
					break
				}
			}
			if allParentsSent {
				unshallowOut = append(unshallowOut, old)
			}
		}
	}

	// Acknowledgements: a have that is an ancestor of (or equal to) a
	// want earns one ACK; the first such have is enough to declare the
	// negotiation done.
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
	return fetchPlan{
		acks:      acks,
		ready:     len(acks) > 0,
		nak:       len(acks) == 0,
		shallow:   dedupSorted(shallowOut),
		unshallow: dedupSorted(unshallowOut),
		objects:   sent,
	}
}

// objectsByID flattens an object map into a slice ordered by id, so the
// pack bytes are deterministic for the same object set.
func objectsByID(objects map[string]*GitObject) []*GitObject {
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

// joinBytes concatenates byte slices into one buffer, pre-sized so the
// copy runs once.
func joinBytes(parts [][]byte) []byte {
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

// parseDeepen turns a deepen argument into a depth. Anything that is not
// a positive finite number is ignored.
func parseDeepen(text string) *int {
	n, ok := jsNumber(strings.TrimSpace(text))
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return nil
	}
	d := int(n)
	return &d
}

// fetchArgs is the want/have/shallow/deepen set pulled out of a decoded
// v2 fetch request. The first data line is the command line; agent,
// object-format, thin-pack, no-progress, ofs-delta, filter, deepen-since,
// deepen-not and done lines carry no information this server acts on.
func fetchArgsV2(packets []PktPacket) (wants, haves, shallow []string, deepen *int, ok bool) {
	seenCommand := false
	for _, packet := range packets {
		if packet.Kind != "data" {
			continue
		}
		line := string(packet.Payload)
		if !seenCommand {
			seenCommand = true
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
			if d := parseDeepen(line[7:]); d != nil {
				deepen = d
			}
		}
	}
	return wants, haves, shallow, deepen, seenCommand
}

// serveFetchV2 answers a v2 command=fetch request: an acknowledgments
// section, an optional shallow-info section, then the packfile section,
// each delimited, and a final flush.
func serveFetchV2(s *server, repo *Repo, w *respWriter, packets []PktPacket) {
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
	wants, haves, shallow, deepen, ok := fetchArgsV2(packets)
	if !ok {
		panic(fmt.Errorf("fetch without a command line"))
	}
	plan := planFetch(repo, wants, haves, shallow, deepen)

	ackItems := []pktItem{pktStr("acknowledgments\n")}
	if plan.nak {
		ackItems = append(ackItems, pktStr("NAK\n"))
	}
	for _, ack := range plan.acks {
		ackItems = append(ackItems, pktStr("ACK "+ack+"\n"))
	}
	if plan.ready {
		ackItems = append(ackItems, pktStr("ready\n"))
	}
	ackItems = append(ackItems, pktDelimItem)
	sections := [][]byte{encodePktLines(ackItems)}
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		shallowItems := []pktItem{pktStr("shallow-info\n")}
		for _, oid := range plan.shallow {
			shallowItems = append(shallowItems, pktStr("shallow "+oid+"\n"))
		}
		for _, oid := range plan.unshallow {
			shallowItems = append(shallowItems, pktStr("unshallow "+oid+"\n"))
		}
		shallowItems = append(shallowItems, pktDelimItem)
		sections = append(sections, encodePktLines(shallowItems))
	}
	if len(plan.objects) > 0 {
		sections = append(sections, encodePktLines([]pktItem{pktStr("packfile\n")}))
		sections = append(sections, sideband(writePack(objectsByID(plan.objects))))
	}
	sections = append(sections, pktFlush)
	s.writeResp(w, 200, "application/x-git-upload-pack-result", nil, joinBytes(sections))
}

// fetchArgsV0 pulls want/have/shallow/deepen out of a v0 upload-pack
// request. The first want line carries the client's capabilities after
// the object id, so only its first field is read; the wants arrive in
// one section and the haves (plus done) in a second, separated by a
// flush, but both are read the same way.
func fetchArgsV0(packets []PktPacket) (wants, haves, shallow []string, deepen *int) {
	for _, packet := range packets {
		if packet.Kind != "data" {
			continue
		}
		line := string(packet.Payload)
		switch {
		case strings.HasPrefix(line, "want "):
			fields := strings.Fields(strings.TrimSpace(line[5:]))
			if len(fields) > 0 {
				wants = append(wants, fields[0])
			}
		case strings.HasPrefix(line, "have "):
			haves = append(haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			shallow = append(shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			if d := parseDeepen(line[7:]); d != nil {
				deepen = d
			}
		}
	}
	return wants, haves, shallow, deepen
}

// serveFetchV0 answers a v0 upload-pack POST. The framing is flatter
// than v2 — bare NAK/ACK lines, then shallow/unshallow lines, then the
// pack — but the negotiation underneath is the same.
func serveFetchV0(s *server, repo *Repo, body []byte, w *respWriter) {
	packets, err := decodePktLines(body)
	if err != nil {
		s.writeText(w, 400, "Bad Request: not a pkt-line stream")
		return
	}
	wants, haves, shallow, deepen := fetchArgsV0(packets)
	plan := planFetch(repo, wants, haves, shallow, deepen)
	var items []pktItem
	if plan.nak {
		items = append(items, pktStr("NAK\n"))
	}
	for _, ack := range plan.acks {
		items = append(items, pktStr("ACK "+ack+"\n"))
	}
	for _, oid := range plan.shallow {
		items = append(items, pktStr("shallow "+oid+"\n"))
	}
	for _, oid := range plan.unshallow {
		items = append(items, pktStr("unshallow "+oid+"\n"))
	}
	sections := [][]byte{encodePktLines(items)}
	if len(plan.objects) > 0 {
		sections = append(sections, sideband(writePack(objectsByID(plan.objects))))
	}
	sections = append(sections, pktFlush)
	s.writeResp(w, 200, "application/x-git-upload-pack-result", nil, joinBytes(sections))
}

// ---------------------------------------------------------------- receive-pack

// refCommand is one ref update from the client's pkt-line prefix: move
// ref from oldOID to newOID (a zero newOID means delete).
type refCommand struct {
	oldOID string
	newOID string
	ref    string
}

// refResult is one line of the per-ref status report.
type refResult struct {
	ref    string
	ok     bool
	reason string
}

// pushCommandPattern matches a single ref-update line.
var pushCommandPattern = regexp.MustCompile(`^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)\s*$`)

// writePushReport renders the side-band report: one "unpack ok" (or
// "unpack <detail>") line, then one "ok" / "ng" line per ref.
func writePushReport(s *server, w *respWriter, unpackOK bool, unpackDetail string, results []refResult) {
	var inner []pktItem
	if unpackOK {
		inner = append(inner, pktStr("unpack ok\n"))
	} else {
		inner = append(inner, pktStr("unpack "+unpackDetail+"\n"))
	}
	for _, result := range results {
		if result.ok {
			inner = append(inner, pktStr("ok "+result.ref+"\n"))
		} else {
			inner = append(inner, pktStr("ng "+result.ref+" "+result.reason+"\n"))
		}
	}
	out := joinBytes([][]byte{sideband(encodePktLines(inner)), pktFlush})
	s.writeResp(w, 200, "application/x-git-receive-pack-result", nil, out)
}

// serveReceivePack handles a git-receive-pack POST: pkt-line ref
// commands, then a packfile. The pack is imported first — objects land
// in the store even for refs that are ultimately refused, which matches
// git, where the objects are already on disk — then each command is
// applied in order under the fast-forward rule. The reply is a side-band
// report: one "unpack ok" line, then one "ok" / "ng" line per ref.
func serveReceivePack(s *server, repo *Repo, body []byte, w *respWriter) {
	// The command prefix ends at the first flush; everything after it
	// is the packfile, untouched.
	prefix, consumed, err := decodePktPrefix(body)
	if err != nil {
		s.writeText(w, 400, "Bad Request: not a pkt-line stream")
		return
	}
	var commands []refCommand
	firstLine := true
	for _, packet := range prefix {
		if packet.Kind == "flush" {
			break
		}
		if packet.Kind != "data" {
			continue
		}
		line := string(packet.Payload)
		if firstLine {
			// The first command line carries the client's capabilities
			// after a NUL byte; the command itself is before it.
			if nul := strings.IndexByte(line, 0); nul >= 0 {
				line = line[:nul]
			}
			firstLine = false
		}
		if m := pushCommandPattern.FindStringSubmatch(line); m != nil {
			commands = append(commands, refCommand{oldOID: m[1], newOID: m[2], ref: m[3]})
		}
	}
	packStart := consumed
	if packStart > len(body) {
		packStart = len(body)
	}
	if packBytes := body[packStart:]; len(packBytes) > 0 {
		result, err := readPack(packBytes, func(oid string) *GitObject { return repo.Store.Get(oid) })
		if err != nil {
			ng := make([]refResult, 0, len(commands))
			for _, cmd := range commands {
				ng = append(ng, refResult{ref: cmd.ref, ok: false, reason: "unpack failed"})
			}
			writePushReport(s, w, false, err.Error(), ng)
			return
		}
		for oid, obj := range result.Objects {
			repo.Store.PutObject(oid, obj)
		}
	}

	// Apply the commands one at a time. A mixed push applies the
	// fast-forwarding refs and refuses the rest — never all-or-nothing.
	results := make([]refResult, 0, len(commands))
	for _, cmd := range commands {
		results = append(results, applyRefCommand(repo, cmd))
	}
	writePushReport(s, w, true, "", results)
}

// applyRefCommand validates and applies a single ref command. Every
// update must fast-forward: the advertised old tip has to match the ref,
// and the new commit has to descend from it.
func applyRefCommand(repo *Repo, cmd refCommand) refResult {
	deny := func(reason string) refResult {
		return refResult{ref: cmd.ref, ok: false, reason: reason}
	}
	allow := func() refResult {
		return refResult{ref: cmd.ref, ok: true}
	}
	if !strings.HasPrefix(cmd.ref, "refs/") || !isOid(cmd.newOID) || !isOid(cmd.oldOID) {
		return deny("invalid ref")
	}
	current, hasCurrent := repo.Refs[cmd.ref]
	if cmd.newOID == ZeroOID {
		// Delete.
		if !hasCurrent {
			return deny("remote ref does not exist")
		}
		if cmd.oldOID != ZeroOID && cmd.oldOID != current {
			return deny("non-fast-forward")
		}
		delete(repo.Refs, cmd.ref)
		return allow()
	}
	newObj := repo.Store.Get(cmd.newOID)
	if newObj == nil {
		return deny("unable to resolve new ref")
	}
	if cmd.oldOID == ZeroOID {
		// Create.
		if hasCurrent {
			return deny("already exists")
		}
		repo.Refs[cmd.ref] = cmd.newOID
		return allow()
	}
	// Update.
	if !hasCurrent {
		return deny("remote ref does not exist")
	}
	if cmd.oldOID != current {
		return deny("non-fast-forward")
	}
	if cmd.newOID == cmd.oldOID {
		return allow()
	}
	switch newObj.Type {
	case ObjCommit:
		if !isAncestor(repo.Store, cmd.oldOID, cmd.newOID) {
			return deny("non-fast-forward")
		}
	case ObjTag:
		// Retargeting a ref at a tag is allowed; anything else is not.
	default:
		return deny("new ref is not a commit or tag")
	}
	repo.Refs[cmd.ref] = cmd.newOID
	return allow()
}

// ---------------------------------------------------------------- entry

var gitRoutePattern = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(.*)$`)

// handleGit routes one git smart-HTTP request. It returns true when the
// path is a git route at all (handled, even on 404); false when the path
// belongs to another surface. A malformed git request never takes the
// server down: it answers 400 and the repo survives.
func (s *server) handleGit(r *http.Request, w *respWriter, pathname string, search queryParams, body []byte) (handled bool) {
	m := gitRoutePattern.FindStringSubmatch(pathname)
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
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		s.writeText(w, 404, "Not Found")
		return true
	}
	defer func() {
		if rec := recover(); rec != nil {
			if !w.sent {
				s.writeText(w, 400, "Bad Request")
			}
		}
	}()
	if r.Method == "GET" && tail == "info/refs" {
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
	if r.Method == "POST" && tail == "git-upload-pack" {
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
				serveLsRefs(s, repo, w, packets)
			case strings.HasPrefix(command, "command=fetch"):
				serveFetchV2(s, repo, w, packets)
			default:
				panic(fmt.Errorf("unknown v2 command"))
			}
		} else {
			serveFetchV0(s, repo, body, w)
		}
		return true
	}
	if r.Method == "POST" && tail == "git-receive-pack" {
		serveReceivePack(s, repo, body, w)
		return true
	}
	s.writeText(w, 404, "Not Found")
	return true
}
