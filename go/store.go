// store.go — every scrap of forge state, held in memory.
//
// A Store is the whole world for this process: repositories with their git
// objects and refs, plus the collaboration layer on top (issues, labels,
// comments, pull requests). Nothing here touches disk; the trial lives and
// dies with the process, which is exactly what the perf workloads want —
// every read is a map lookup, every write a map store.
//
// Determinism is the other half of the design. Ids and numbers come from
// counters that only ever go up, and every timestamp is the trial epoch
// plus a monotonic sequence tick — one tick per thing created. No wall
// clock, no randomness, so replaying a trial rebuilds the same state.
//
// Labels need one extra piece of bookkeeping: Go maps iterate in random
// order, so each repo keeps its label names in insertion order alongside
// the map.

package main

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The viewer — the single authenticated user this forge knows — and the two
// other people who show up in seeded data. Constants, not lookups: the
// scenario seed is fixed and the bearer token is opaque, so the token's
// owner is known by construction.
const (
	viewerLogin = "adaiversen"
	viewerID    = 1000
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"

	teammateLogin = "quinnkeller"
	teammateID    = 1001

	strangerLogin = "olgabauer"
	strangerID    = 1002
)

const defaultBranch = "main"

// 2025-01-01T00:00:00Z, the trial epoch when MAKEFUN_TRIAL_NOW is unset.
const scenarioEpochSec = 1735689600

// trialEpochSec reads the trial clock's base from the environment. It
// accepts a plain unix second count, a millisecond count (anything over
// 1e12), or a date string in one of the common layouts; anything
// unparseable falls back to the scenario epoch.
func trialEpochSec() int64 {
	raw := os.Getenv("MAKEFUN_TRIAL_NOW")
	if raw == "" {
		return scenarioEpochSec
	}
	if num, ok := jsNumber(raw); ok && !isNaN(num) && num > 0 {
		if num > 1e12 {
			return int64(num / 1000)
		}
		return int64(num)
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999Z07:00",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.Unix()
		}
	}
	return scenarioEpochSec
}

func isNaN(f float64) bool { return f != f }

// viewerIdentity builds the git identity for the viewer at a given
// absolute timestamp.
func viewerIdentity(ts int64) Identity {
	return Identity{Name: viewerName, Email: viewerEmail, TS: ts}
}

// globalId mints an opaque id: the base64url encoding of a typed tag and
// its parts joined with colons, e.g. "R:5000" or "I:5000:3". Clients see
// only the opaque string; the server can decode it again without a table.
func globalId(tag string, parts ...any) string {
	var b strings.Builder
	b.WriteString(tag)
	for _, p := range parts {
		b.WriteByte(':')
		switch v := p.(type) {
		case string:
			b.WriteString(v)
		case int:
			b.WriteString(strconv.Itoa(v))
		case int64:
			b.WriteString(strconv.FormatInt(v, 10))
		case float64:
			b.WriteString(jsNumberString(v))
		default:
			b.WriteString(jsString(v))
		}
	}
	return base64urlEncode([]byte(b.String()))
}

type decodedGlobalId struct {
	Tag   string
	Parts []string
}

// decodeGlobalId reverses globalId. It returns nil for anything that is
// not a base64url-encoded string whose tag is a single uppercase letter.
func decodeGlobalId(id string) *decodedGlobalId {
	text := string(lenientBase64urlDecode(id))
	fields := strings.Split(text, ":")
	if len(fields) == 0 {
		return nil
	}
	tag := fields[0]
	if len(tag) != 1 || tag[0] < 'A' || tag[0] > 'Z' {
		return nil
	}
	return &decodedGlobalId{Tag: tag, Parts: fields[1:]}
}

// ---------------------------------------------------------------- types

type Label struct {
	Name        string
	Color       string
	Description string
}

type Comment struct {
	ID         int
	Body       string
	CreatedSeq int64
	UserLogin  string
}

type Issue struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string // "open" | "closed"
	Labels     []string
	Comments   []*Comment
	CreatedSeq int64
	UpdatedSeq int64
	// Memoized IssueJSONBuffer output (JSON plus the trailing newline
	// sendJson appends). Stays valid while the issue's visible fields are
	// unchanged; every mutation path clears it. The read workload fetches
	// one issue 2,000 times, so this skips a JSON encode per request.
	jsonCache []byte
}

type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Merged         bool
	MergedAt       any // string | nil
	MergeCommitSha any // string | nil
	HeadRef        string
	BaseRef        string
	HeadSha        string
	BaseSha        string
	CreatedSeq     int64
}

type Repo struct {
	ID              int
	Name            string
	Owner           string
	Description     string
	Private         bool
	DefaultBranch   string
	Store           *ObjectStore
	Refs            map[string]string
	Labels          map[string]*Label
	LabelOrder      []string
	Issues          []*Issue
	Pulls           []*Pull
	NextIssueNumber int
	NextPullNumber  int
	CreatedSeq      int64
	UpdatedSeq      int64
}

type FileChange struct {
	Path      string
	Status    string // "added" | "modified" | "changed"
	Additions int
	Deletions int
	HeadOid   any // string | nil
	BaseOid   any // string | nil
}

// ------------------------------------------------------- content helpers

// lineCounts counts added and removed lines between two blob bodies using
// a multiset diff: a line present in head but not (still) in base is an
// addition; a base line with no matching head line is a deletion. A final
// newline does not create a phantom empty line.
func lineCounts(base []byte, head []byte) (additions, deletions int) {
	splitLines := func(data []byte) []string {
		if data == nil {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines = lines[:n-1]
		}
		return lines
	}
	remaining := make(map[string]int)
	for _, line := range splitLines(base) {
		remaining[line]++
	}
	for _, line := range splitLines(head) {
		if remaining[line] > 0 {
			remaining[line]--
		} else {
			additions++
		}
	}
	for _, left := range remaining {
		deletions += left
	}
	return additions, deletions
}

// flattenTree walks a tree object depth-first and returns every blob it
// contains as path -> entry. Directories (mode 40000) are descended into,
// never listed. A missing or malformed tree flattens to nothing.
func flattenTree(store *ObjectStore, treeOid string, prefix string, out map[string]TreeEntry) map[string]TreeEntry {
	if out == nil {
		out = make(map[string]TreeEntry)
	}
	type pending struct {
		oid    string
		prefix string
	}
	stack := []pending{{oid: treeOid, prefix: prefix}}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		obj := store.Get(cur.oid)
		if obj == nil || obj.Type != ObjTree {
			continue
		}
		entries, err := parseTree(obj.Data)
		if err != nil {
			continue
		}
		// Push in reverse so entries come out in tree order.
		for i := len(entries) - 1; i >= 0; i-- {
			e := entries[i]
			path := e.Name
			if cur.prefix != "" {
				path = cur.prefix + "/" + e.Name
			}
			if e.Mode == "40000" {
				stack = append(stack, pending{oid: e.OID, prefix: path})
			} else {
				out[path] = TreeEntry{Mode: e.Mode, Name: e.Name, OID: e.OID}
			}
		}
	}
	return out
}

// diffTrees diffs two trees (by oid; "" means the empty tree) and returns
// the changed paths in sorted order. A path only in head is "added", only
// in base is "changed" (deleted), and in both with a different oid is
// "modified". Unchanged paths never appear.
func diffTrees(store *ObjectStore, baseTreeOid string, headTreeOid string) []FileChange {
	base := flattenTree(store, baseTreeOid, "", nil)
	head := flattenTree(store, headTreeOid, "", nil)
	paths := make([]string, 0, len(base)+len(head))
	seen := make(map[string]bool, len(base)+len(head))
	for p := range base {
		seen[p] = true
		paths = append(paths, p)
	}
	for p := range head {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	changes := make([]FileChange, 0, len(paths))
	for _, path := range paths {
		before, hasBefore := base[path]
		after, hasAfter := head[path]
		if hasBefore && hasAfter && before.OID == after.OID {
			continue
		}
		var baseData, headData []byte
		if hasBefore {
			if o := store.Get(before.OID); o != nil {
				baseData = o.Data
			}
		}
		if hasAfter {
			if o := store.Get(after.OID); o != nil {
				headData = o.Data
			}
		}
		added, removed := lineCounts(baseData, headData)
		change := FileChange{Path: path, Additions: added, Deletions: removed}
		switch {
		case !hasBefore:
			change.Status = "added"
			change.HeadOid = after.OID
		case !hasAfter:
			change.Status = "changed"
			change.BaseOid = before.OID
		default:
			change.Status = "modified"
			change.HeadOid = after.OID
			change.BaseOid = before.OID
		}
		changes = append(changes, change)
	}
	return changes
}

// ------------------------------------------------------------------ store

type Store struct {
	repos         map[string]*Repo
	nextRepoID    int
	nextIssueID   int
	nextCommentID int
	nextPullID    int
	seq           int64
	epochSec      int64
}

func NewStore() *Store {
	return &Store{
		repos:         make(map[string]*Repo),
		nextRepoID:    5000,
		nextIssueID:   1,
		nextCommentID: 1,
		nextPullID:    1,
		epochSec:      trialEpochSec(),
	}
}

// nextSeq hands out the next monotonic sequence tick. Every created thing
// takes one, and timestamps are epoch + tick.
func (s *Store) nextSeq() int64 {
	s.seq++
	return s.seq
}

func (s *Store) isoAt(seq int64) string {
	return time.Unix(s.epochSec+seq, 0).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// nowIso is "now" for served data: the current sequence tick rendered as
// an ISO timestamp. Deterministic within a trial.
func (s *Store) nowIso() string { return s.isoAt(s.seq) }

func (s *Store) identity() Identity {
	return viewerIdentity(s.epochSec + s.seq)
}

// --------------------------------------------------------- repositories

func (s *Store) repoKey(owner, name string) string { return owner + "/" + name }

func (s *Store) GetRepo(owner, name string) *Repo {
	if owner != viewerLogin {
		return nil
	}
	return s.repos[s.repoKey(owner, name)]
}

func (s *Store) GetRepoById(id int) *Repo {
	for _, repo := range s.repos {
		if repo.ID == id {
			return repo
		}
	}
	return nil
}

func (s *Store) CreateRepo(name string, description string, hasDescription bool, private bool, defaultBranchOpt string, hasDefaultBranch bool) *Repo {
	key := s.repoKey(viewerLogin, name)
	if _, exists := s.repos[key]; exists {
		return nil
	}
	seq := s.nextSeq()
	repo := &Repo{
		ID:              s.nextRepoID,
		Name:            name,
		Owner:           viewerLogin,
		Description:     description,
		Private:         private,
		DefaultBranch:   defaultBranch,
		Store:           NewObjectStore(),
		Refs:            make(map[string]string),
		Labels:          make(map[string]*Label),
		Issues:          []*Issue{},
		Pulls:           []*Pull{},
		NextIssueNumber: 1,
		NextPullNumber:  1,
		CreatedSeq:      seq,
		UpdatedSeq:      seq,
	}
	if !hasDescription {
		repo.Description = ""
	}
	if hasDefaultBranch {
		repo.DefaultBranch = defaultBranchOpt
	}
	s.nextRepoID++
	s.repos[key] = repo
	return repo
}

func (s *Store) DeleteRepo(owner, name string) bool {
	if owner != viewerLogin {
		return false
	}
	key := s.repoKey(owner, name)
	if _, ok := s.repos[key]; !ok {
		return false
	}
	delete(s.repos, key)
	return true
}

func (s *Store) ListViewerRepos() []*Repo {
	out := make([]*Repo, 0, len(s.repos))
	for _, repo := range s.repos {
		if repo.Owner == viewerLogin {
			out = append(out, repo)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedSeq < out[j].CreatedSeq })
	return out
}

// --------------------------------------------------------------- git refs

func (s *Store) refName(repo *Repo, branch string) string { return "refs/heads/" + branch }

func (s *Store) refTip(repo *Repo, ref string) (string, bool) {
	v, ok := repo.Refs[ref]
	return v, ok
}

// ResolveBranch turns a branch name or a full ref into its tip commit oid.
func (s *Store) ResolveBranch(repo *Repo, branchOrRef string) (string, bool) {
	ref := branchOrRef
	if !strings.HasPrefix(ref, "refs/") {
		ref = s.refName(repo, branchOrRef)
	}
	v, ok := repo.Refs[ref]
	return v, ok
}

func (s *Store) CreateBranch(repo *Repo, branch, sha string) bool {
	obj := repo.Store.Get(sha)
	if obj == nil || obj.Type != ObjCommit {
		return false
	}
	repo.Refs[s.refName(repo, branch)] = sha
	return true
}

// ReadFileAt returns the blob oid and contents of a path at a ref, or
// ok=false when the ref or the path does not exist.
func (s *Store) ReadFileAt(repo *Repo, ref, path string) (oid string, data []byte, ok bool) {
	tip, found := s.ResolveBranch(repo, ref)
	if !found {
		return "", nil, false
	}
	commit := repo.Store.Get(tip)
	if commit == nil || commit.Type != ObjCommit {
		return "", nil, false
	}
	tree, _ := parseCommitLight(commit.Data)
	entry, found := flattenTree(repo.Store, tree, "", nil)[path]
	if !found {
		return "", nil, false
	}
	blob := repo.Store.Get(entry.OID)
	if blob == nil {
		return "", nil, false
	}
	return entry.OID, blob.Data, true
}

// ListDirAt lists the entries of a directory at a ref. The root is listed
// when path is empty. Returns nil when the ref or the directory is missing.
type DirEntry struct {
	Name string
	Path string
	OID  string
	Type string // "file" | "dir"
}

func (s *Store) ListDirAt(repo *Repo, ref, path string) []DirEntry {
	tip, found := s.ResolveBranch(repo, ref)
	if !found {
		return nil
	}
	commit := repo.Store.Get(tip)
	if commit == nil || commit.Type != ObjCommit {
		return nil
	}
	treeOid, _ := parseCommitLight(commit.Data)
	if path != "" {
		sub, ok := s.subtreeAt(repo, treeOid, strings.Split(path, "/"))
		if !ok {
			return nil
		}
		treeOid = sub
	}
	obj := repo.Store.Get(treeOid)
	if obj == nil || obj.Type != ObjTree {
		return nil
	}
	entries, err := parseTree(obj.Data)
	if err != nil {
		return nil
	}
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		full := e.Name
		if path != "" {
			full = path + "/" + e.Name
		}
		kind := "file"
		if e.Mode == "40000" {
			kind = "dir"
		}
		out = append(out, DirEntry{Name: e.Name, Path: full, OID: e.OID, Type: kind})
	}
	return out
}

// subtreeAt descends from a tree oid through path parts and returns the
// oid of the directory at the end, or false if any part is missing or is
// not a directory.
func (s *Store) subtreeAt(repo *Repo, treeOid string, parts []string) (string, bool) {
	current := treeOid
	for _, part := range parts {
		obj := repo.Store.Get(current)
		if obj == nil || obj.Type != ObjTree {
			return "", false
		}
		entries, err := parseTree(obj.Data)
		if err != nil {
			return "", false
		}
		next := ""
		for _, e := range entries {
			if e.Name == part && e.Mode == "40000" {
				next = e.OID
				break
			}
		}
		if next == "" {
			return "", false
		}
		current = next
	}
	return current, true
}

// setPathInTree writes (content != nil) or deletes (content == nil) one
// path in a tree, creating intermediate directories as needed, and returns
// the oid of the new tree. A directory left empty by a delete is pruned.
func (s *Store) setPathInTree(store *ObjectStore, treeOid string, hasTree bool, parts []string, content []byte) string {
	var entries []TreeEntry
	if hasTree {
		if obj := store.Get(treeOid); obj != nil {
			entries, _ = parseTree(obj.Data)
		}
	}
	head, rest := parts[0], parts[1:]
	idx := -1
	for i, e := range entries {
		if e.Name == head {
			idx = i
			break
		}
	}
	replaceOrAppend := func(entry TreeEntry) {
		if idx >= 0 {
			entries[idx] = entry
		} else {
			entries = append(entries, entry)
		}
	}
	removeAt := func() {
		if idx >= 0 {
			entries = append(entries[:idx], entries[idx+1:]...)
		}
	}
	if len(rest) == 0 {
		if content == nil {
			removeAt()
		} else {
			replaceOrAppend(TreeEntry{Mode: "100644", Name: head, OID: store.Put(ObjBlob, content)})
		}
		return store.Put(ObjTree, buildTree(entries))
	}
	childOid, hasChild := "", false
	if idx >= 0 && entries[idx].Mode == "40000" {
		childOid, hasChild = entries[idx].OID, true
	}
	newChild := s.setPathInTree(store, childOid, hasChild, rest, content)
	empty := false
	if child := store.Get(newChild); child != nil && child.Type == ObjTree {
		kids, _ := parseTree(child.Data)
		empty = len(kids) == 0
	}
	switch {
	case empty:
		removeAt()
	default:
		replaceOrAppend(TreeEntry{Mode: "40000", Name: head, OID: newChild})
	}
	return store.Put(ObjTree, buildTree(entries))
}

type CommitFileResult struct {
	CommitSha string
	BlobSha   any // string | nil
	Created   bool
}

// CommitFile commits one file change on a branch. A branch that does not
// exist yet starts from the default branch tip; the default branch itself
// starts from nothing (the root commit). Returns nil when there is no base
// to build on, or the path is empty. Created is true only for the repo's
// very first commit.
func (s *Store) CommitFile(repo *Repo, branch, path string, content []byte, message string) *CommitFileResult {
	ref := s.refName(repo, branch)
	tip, hasTip := repo.Refs[ref]
	var baseTree string
	hasBaseTree := false
	var parents []string
	if hasTip {
		commit := repo.Store.Get(tip)
		if commit == nil || commit.Type != ObjCommit {
			return nil
		}
		baseTree, _ = parseCommitLight(commit.Data)
		hasBaseTree = true
		parents = append(parents, tip)
	} else if branch != repo.DefaultBranch {
		defaultTip, ok := repo.Refs[s.refName(repo, repo.DefaultBranch)]
		if !ok {
			return nil
		}
		commit := repo.Store.Get(defaultTip)
		if commit == nil || commit.Type != ObjCommit {
			return nil
		}
		baseTree, _ = parseCommitLight(commit.Data)
		hasBaseTree = true
		parents = append(parents, defaultTip)
	}
	parts := make([]string, 0, 4)
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	newTree := s.setPathInTree(repo.Store, baseTree, hasBaseTree, parts, content)
	identity := s.identity()
	commitData := buildCommit(CommitInfo{Tree: newTree, Parents: parents, Author: identity, Committer: identity, Message: message})
	commitSha := repo.Store.Put(ObjCommit, commitData)
	repo.Refs[ref] = commitSha
	var blobSha any
	if content != nil {
		blobSha = hashObject(ObjBlob, content)
	}
	return &CommitFileResult{CommitSha: commitSha, BlobSha: blobSha, Created: !hasTip && len(parents) == 0}
}

type CommitListEntry struct {
	Sha     string
	Message string
}

// ListCommits walks the commits reachable from a ref, newest first. The
// walk is breadth-first over parents, which matches first-parent order on
// the linear histories this forge produces.
func (s *Store) ListCommits(repo *Repo, branchOrRef string) []CommitListEntry {
	tip, found := s.ResolveBranch(repo, branchOrRef)
	if !found {
		return []CommitListEntry{}
	}
	out := []CommitListEntry{}
	seen := make(map[string]bool)
	queue := []string{tip}
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := repo.Store.Get(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		info := parseCommit(obj.Data)
		out = append(out, CommitListEntry{Sha: oid, Message: info.Message})
		queue = append(queue, info.Parents...)
	}
	return out
}

// ------------------------------------------------- issues / labels / comments

func (s *Store) CreateIssue(repo *Repo, title, body string, labels []string) *Issue {
	seq := s.nextSeq()
	issue := &Issue{
		ID:         s.nextIssueID,
		Number:     repo.NextIssueNumber,
		Title:      title,
		Body:       body,
		State:      "open",
		Labels:     append([]string{}, labels...),
		Comments:   []*Comment{},
		CreatedSeq: seq,
		UpdatedSeq: seq,
	}
	s.nextIssueID++
	repo.NextIssueNumber++
	repo.Issues = append(repo.Issues, issue)
	return issue
}

func (s *Store) GetIssue(repo *Repo, number int) *Issue {
	for _, i := range repo.Issues {
		if i.Number == number {
			return i
		}
	}
	return nil
}

func (s *Store) CreateLabel(repo *Repo, name, color string) *Label {
	if existing, ok := repo.Labels[name]; ok {
		return existing
	}
	label := &Label{Name: name, Color: color, Description: ""}
	repo.Labels[name] = label
	repo.LabelOrder = append(repo.LabelOrder, name)
	return label
}

func (s *Store) AddComment(repo *Repo, issue *Issue, body string) *Comment {
	comment := &Comment{ID: s.nextCommentID, Body: body, CreatedSeq: s.nextSeq(), UserLogin: viewerLogin}
	s.nextCommentID++
	issue.Comments = append(issue.Comments, comment)
	issue.jsonCache = nil
	return comment
}

// ------------------------------------------------------------- pull requests

func (s *Store) CreatePull(repo *Repo, title, body, headRef, baseRef string) *Pull {
	headSha, okH := s.ResolveBranch(repo, headRef)
	baseSha, okB := s.ResolveBranch(repo, baseRef)
	if !okH || !okB {
		return nil
	}
	seq := s.nextSeq()
	pr := &Pull{
		ID: s.nextPullID, Number: repo.NextPullNumber,
		Title: title, Body: body, State: "open",
		Merged: false, MergedAt: nil, MergeCommitSha: nil,
		HeadRef: headRef, BaseRef: baseRef,
		HeadSha: headSha, BaseSha: baseSha,
		CreatedSeq: seq,
	}
	s.nextPullID++
	repo.NextPullNumber++
	repo.Pulls = append(repo.Pulls, pr)
	return pr
}

func (s *Store) GetPull(repo *Repo, number int) *Pull {
	for _, p := range repo.Pulls {
		if p.Number == number {
			return p
		}
	}
	return nil
}

// treeOf resolves a commit oid to its root tree oid, or "" when the commit
// is missing.
func treeOf(store *ObjectStore, commitOid string) string {
	if obj := store.Get(commitOid); obj != nil {
		tree, _ := parseCommitLight(obj.Data)
		return tree
	}
	return ""
}

// threeWayTrees flattens the three trees a squash merge reasons about:
// the merge-base tree, the head tree, and the current base-tip tree.
func (s *Store) threeWayTrees(repo *Repo, pr *Pull) (base, head, tip map[string]TreeEntry) {
	baseTipSha, ok := s.ResolveBranch(repo, pr.BaseRef)
	if !ok {
		return nil, nil, nil
	}
	mergeBaseOid := mergeBase(repo.Store, pr.HeadSha, baseTipSha)
	base = flattenTree(repo.Store, treeOf(repo.Store, mergeBaseOid), "", nil)
	head = flattenTree(repo.Store, treeOf(repo.Store, pr.HeadSha), "", nil)
	tip = flattenTree(repo.Store, treeOf(repo.Store, baseTipSha), "", nil)
	return base, head, tip
}

// PullFiles lists the files changed between the merge-base tree and the
// head tree — the PR's diff.
func (s *Store) PullFiles(repo *Repo, pr *Pull) []FileChange {
	base := mergeBase(repo.Store, pr.HeadSha, pr.BaseSha)
	return diffTrees(repo.Store, treeOf(repo.Store, base), treeOf(repo.Store, pr.HeadSha))
}

// MergeConflicts answers whether a squash merge of this PR would collide:
// a path the head changed (or deleted) relative to the merge base that the
// base tip also changed differently. Returns true when the base ref cannot
// even be resolved.
func (s *Store) MergeConflicts(repo *Repo, pr *Pull) bool {
	base, head, tip := s.threeWayTrees(repo, pr)
	if base == nil {
		return true
	}
	for path, h := range head {
		b, hasB := base[path]
		if hasB && b.OID == h.OID {
			continue // head left this path alone
		}
		if t, hasT := tip[path]; hasT && (!hasB || t.OID != b.OID) && t.OID != h.OID {
			return true
		}
	}
	for path, b := range base {
		if _, hasH := head[path]; hasH {
			continue
		}
		// Deleted on head; a conflict only if the base tip changed it too.
		if t, hasT := tip[path]; hasT && t.OID != b.OID {
			return true
		}
	}
	return false
}

type MergeResult struct {
	Sha     string
	Message string
}

// MergePull squash-merges an open PR onto its base branch: the merge-base
// to head diff is applied onto the base tip tree, one commit is written,
// and the PR is closed as merged. Returns nil for a closed PR, a
// conflicting merge, or unresolvable refs.
func (s *Store) MergePull(repo *Repo, pr *Pull) *MergeResult {
	if pr.State != "open" {
		return nil
	}
	if s.MergeConflicts(repo, pr) {
		return nil
	}
	baseTipSha, ok := s.ResolveBranch(repo, pr.BaseRef)
	if !ok {
		return nil
	}
	base, head, _ := s.threeWayTrees(repo, pr)
	// Every path the head touched relative to the merge base, in sorted
	// order so the rebuild is deterministic.
	touched := make(map[string]bool)
	for path, h := range head {
		if b, hasB := base[path]; !hasB || b.OID != h.OID {
			touched[path] = true
		}
	}
	for path := range base {
		if _, hasH := head[path]; !hasH {
			touched[path] = true
		}
	}
	ordered := make([]string, 0, len(touched))
	for p := range touched {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)
	newTreeOid := treeOf(repo.Store, baseTipSha)
	for _, path := range ordered {
		var content []byte
		if h, hasH := head[path]; hasH {
			if o := repo.Store.Get(h.OID); o != nil {
				content = o.Data
			}
		}
		newTreeOid = s.setPathInTree(repo.Store, newTreeOid, true, strings.Split(path, "/"), content)
	}
	seq := s.nextSeq()
	identity := s.identity()
	message := "Merge pull request #" + strconv.Itoa(pr.Number) + " from " + pr.HeadRef + "\n\n" + pr.Title
	commitData := buildCommit(CommitInfo{
		Tree: newTreeOid, Parents: []string{baseTipSha},
		Author: identity, Committer: identity, Message: message,
	})
	sha := repo.Store.Put(ObjCommit, commitData)
	repo.Refs[s.refName(repo, pr.BaseRef)] = sha
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = s.isoAt(seq)
	pr.MergeCommitSha = sha
	return &MergeResult{Sha: sha, Message: message}
}

// -------------------------------------------------------------- JSON shapes

func (s *Store) RepoJSON(repo *Repo) map[string]any {
	openCount := 0
	for _, i := range repo.Issues {
		if i.State == "open" {
			openCount++
		}
	}
	return map[string]any{
		"id":                repo.ID,
		"name":              repo.Name,
		"full_name":         repo.Owner + "/" + repo.Name,
		"owner":             map[string]any{"login": repo.Owner, "id": viewerID, "type": "User"},
		"private":           repo.Private,
		"description":       repo.Description,
		"default_branch":    repo.DefaultBranch,
		"created_at":        s.isoAt(repo.CreatedSeq),
		"updated_at":        s.isoAt(repo.UpdatedSeq),
		"has_issues":        true,
		"has_pull_requests": true,
		"clone_url":         "https://example.invalid/" + repo.Owner + "/" + repo.Name + ".git",
		"html_url":          "https://example.invalid/" + repo.Owner + "/" + repo.Name,
		"open_issues_count": openCount,
	}
}

func (s *Store) IssueJSON(repo *Repo, issue *Issue) map[string]any {
	labels := make([]any, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		color, desc := "ededed", ""
		if label := repo.Labels[name]; label != nil {
			color, desc = label.Color, label.Description
		}
		labels = append(labels, map[string]any{"name": name, "color": color, "description": desc})
	}
	return map[string]any{
		"id":             issue.ID,
		"number":         issue.Number,
		"title":          issue.Title,
		"body":           issue.Body,
		"state":          issue.State,
		"labels":         labels,
		"user":           map[string]any{"login": viewerLogin, "id": viewerID, "type": "User"},
		"assignee":       nil,
		"comments":       len(issue.Comments),
		"created_at":     s.isoAt(issue.CreatedSeq),
		"updated_at":     s.isoAt(issue.UpdatedSeq),
		"repository_url": "/api/v3/repos/" + repo.Owner + "/" + repo.Name,
		"html_url":       "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
	}
}

// IssueJSONBuffer returns the exact bytes sendJson would write for this
// issue (JSON plus '\n'), memoized on the issue. Any mutation clears the
// cache first.
func (s *Store) IssueJSONBuffer(repo *Repo, issue *Issue) []byte {
	if issue.jsonCache == nil {
		issue.jsonCache = append(marshalJSON(s.IssueJSON(repo, issue)), '\n')
	}
	return issue.jsonCache
}

func (s *Store) PullJSON(repo *Repo, pr *Pull) map[string]any {
	return map[string]any{
		"id":               pr.ID,
		"number":           pr.Number,
		"title":            pr.Title,
		"body":             pr.Body,
		"state":            pr.State,
		"merged":           pr.Merged,
		"merged_at":        pr.MergedAt,
		"merge_commit_sha": pr.MergeCommitSha,
		"mergeable":        !s.MergeConflicts(repo, pr),
		"head":             map[string]any{"ref": pr.HeadRef, "sha": pr.HeadSha, "label": repo.Owner + ":" + pr.HeadRef},
		"base":             map[string]any{"ref": pr.BaseRef, "sha": pr.BaseSha, "label": repo.Owner + ":" + pr.BaseRef},
		"user":             map[string]any{"login": viewerLogin, "id": viewerID, "type": "User"},
		"created_at":       s.isoAt(pr.CreatedSeq),
		"updated_at":       s.isoAt(pr.CreatedSeq),
		"html_url":         "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(pr.Number),
	}
}

func (s *Store) LabelJSON(label *Label) map[string]any {
	return map[string]any{"name": label.Name, "color": label.Color, "description": label.Description}
}

func (s *Store) CommentJSON(comment *Comment) map[string]any {
	return map[string]any{
		"id":         comment.ID,
		"body":       comment.Body,
		"user":       map[string]any{"login": comment.UserLogin, "id": viewerID, "type": "User"},
		"created_at": s.isoAt(comment.CreatedSeq),
		"updated_at": s.isoAt(comment.CreatedSeq),
	}
}
