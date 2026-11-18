// store.go — the forge's whole state, in memory. Ported from store.ts.
// One Store holds every repository: its git objects and refs, its issues,
// labels, comments and pull requests. Everything is deterministic: ids and
// numbers come from counters, timestamps from MAKEFUN_TRIAL_NOW plus a
// sequence (never the wall clock), so a replayed trial leaves the same
// state byte-for-byte.
//
// One deliberate difference: labels keep an explicit insertion-order list
// (labelNames), because Go map iteration is random while JS Maps preserve
// insertion order.

package main

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const viewerLogin = "adaiversen"
const viewerID = 1000
const viewerName = "ada iversen"
const viewerEmail = "adaiversen@example.invalid"

const teammateLogin = "quinnkeller"
const teammateID = 1001

const strangerLogin = "olgabauer"
const strangerID = 1002

const defaultBranch = "main"
const scenarioEpochSec = 1735689600 // 2025-01-01T00:00:00Z

func trialEpochSec() int64 {
	raw := os.Getenv("MAKEFUN_TRIAL_NOW")
	if raw != "" {
		if asNum, ok := jsNumber(raw); ok && !isNaN(asNum) && asNum > 0 {
			if asNum > 1e12 {
				return int64(asNum / 1000)
			}
			return int64(asNum)
		}
		// Date.parse fallback: try common layouts.
		for _, layout := range []string{
			time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999Z07:00",
			"2006-01-02 15:04:05", "2006-01-02",
		} {
			if t, err := time.Parse(layout, raw); err == nil {
				return t.Unix()
			}
		}
	}
	return scenarioEpochSec
}

func isNaN(f float64) bool { return f != f }

// viewerIdentity mirrors viewerIdentity(ts).
func viewerIdentity(ts int64) Identity {
	return Identity{Name: viewerName, Email: viewerEmail, TS: ts}
}

// Opaque, deterministic global ids: base64url of a typed tag.
func globalId(tag string, parts ...any) string {
	strs := []string{tag}
	for _, p := range parts {
		switch v := p.(type) {
		case string:
			strs = append(strs, v)
		case int:
			strs = append(strs, strconv.Itoa(v))
		case int64:
			strs = append(strs, strconv.FormatInt(v, 10))
		case float64:
			strs = append(strs, jsNumberString(v))
		default:
			strs = append(strs, jsString(v))
		}
	}
	return base64urlEncode([]byte(strings.Join(strs, ":")))
}

type decodedGlobalId struct {
	Tag   string
	Parts []string
}

func decodeGlobalId(id string) *decodedGlobalId {
	raw := lenientBase64urlDecode(id)
	text := string(raw)
	parts := strings.Split(text, ":")
	tag := parts[0]
	if tag == "" || len(tag) != 1 || tag[0] < 'A' || tag[0] > 'Z' {
		return nil
	}
	return &decodedGlobalId{Tag: tag, Parts: parts[1:]}
}

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
	// Memoized issueJson serialization (with sendJson's trailing newline).
	// Valid while title/body/state/labels/comments/seqs are unchanged;
	// cleared by every mutation path. The read workload GETs one issue
	// 2,000 times, so this removes a JSON encode per request.
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

func lineCounts(base []byte, head []byte) (additions, deletions int) {
	var baseLines, headLines []string
	if base != nil {
		baseLines = strings.Split(string(base), "\n")
	}
	if head != nil {
		headLines = strings.Split(string(head), "\n")
	}
	// Drop the trailing empty element a final newline leaves behind.
	if len(baseLines) > 0 && baseLines[len(baseLines)-1] == "" {
		baseLines = baseLines[:len(baseLines)-1]
	}
	if len(headLines) > 0 && headLines[len(headLines)-1] == "" {
		headLines = headLines[:len(headLines)-1]
	}
	baseCount := make(map[string]int)
	for _, l := range baseLines {
		baseCount[l]++
	}
	for _, l := range headLines {
		if c := baseCount[l]; c > 0 {
			baseCount[l] = c - 1
		} else {
			additions++
		}
	}
	for _, c := range baseCount {
		deletions += c
	}
	return additions, deletions
}

// Flatten a tree (by oid) into path -> {oid, mode}.
func flattenTree(store *ObjectStore, treeOid string, prefix string, out map[string]TreeEntry) map[string]TreeEntry {
	if out == nil {
		out = make(map[string]TreeEntry)
	}
	obj := store.Get(treeOid)
	if obj == nil || obj.Type != ObjTree {
		return out
	}
	entries, err := parseTree(obj.Data)
	if err != nil {
		return out
	}
	for _, e := range entries {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			flattenTree(store, e.OID, path, out)
		} else {
			out[path] = TreeEntry{Mode: e.Mode, Name: e.Name, OID: e.OID}
		}
	}
	return out
}

func diffTrees(store *ObjectStore, baseTreeOid string, headTreeOid string) []FileChange {
	var base, head map[string]TreeEntry
	if baseTreeOid != "" {
		base = flattenTree(store, baseTreeOid, "", nil)
	} else {
		base = make(map[string]TreeEntry)
	}
	if headTreeOid != "" {
		head = flattenTree(store, headTreeOid, "", nil)
	} else {
		head = make(map[string]TreeEntry)
	}
	pathSet := make(map[string]bool)
	for p := range base {
		pathSet[p] = true
	}
	for p := range head {
		pathSet[p] = true
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var changes []FileChange
	for _, path := range paths {
		b, hasB := base[path]
		h, hasH := head[path]
		if hasB && hasH && b.OID == h.OID {
			continue
		}
		var baseData, headData []byte
		if hasB {
			if o := store.Get(b.OID); o != nil {
				baseData = o.Data
			}
		}
		if hasH {
			if o := store.Get(h.OID); o != nil {
				headData = o.Data
			}
		}
		additions, deletions := lineCounts(baseData, headData)
		status := "modified"
		if !hasB {
			status = "added"
		} else if !hasH {
			status = "changed"
		}
		var headOid, baseOid any
		if hasH {
			headOid = h.OID
		}
		if hasB {
			baseOid = b.OID
		}
		changes = append(changes, FileChange{Path: path, Status: status, Additions: additions, Deletions: deletions, HeadOid: headOid, BaseOid: baseOid})
	}
	if changes == nil {
		changes = []FileChange{}
	}
	return changes
}

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

// Monotonic sequence; every created thing takes the next one.
func (s *Store) nextSeq() int64 {
	s.seq++
	return s.seq
}

func (s *Store) isoAt(seq int64) string {
	return time.Unix(s.epochSec+seq, 0).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// "Now" as an ISO timestamp, deterministic within a trial.
func (s *Store) nowIso() string { return s.isoAt(s.seq) }

func (s *Store) identity() Identity {
	return viewerIdentity(s.epochSec + s.seq)
}

// ---------------------------------------------------------- repositories

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
	if _, exists := s.repos[s.repoKey(viewerLogin, name)]; exists {
		return nil
	}
	seq := s.nextSeq()
	repo := &Repo{
		ID:              s.nextRepoID,
		Name:            name,
		Owner:           viewerLogin,
		Description:     "",
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
	s.nextRepoID++
	if hasDescription {
		repo.Description = description
	}
	if hasDefaultBranch {
		repo.DefaultBranch = defaultBranchOpt
	}
	s.repos[s.repoKey(viewerLogin, name)] = repo
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
	var out []*Repo
	for _, repo := range s.repos {
		if repo.Owner == viewerLogin {
			out = append(out, repo)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedSeq < out[j].CreatedSeq })
	return out
}

// ---------------------------------------------------------- git refs

func (s *Store) refName(repo *Repo, branch string) string { return "refs/heads/" + branch }

func (s *Store) refTip(repo *Repo, ref string) (string, bool) {
	v, ok := repo.Refs[ref]
	return v, ok
}

// Resolve a branch name or full ref to its tip commit oid.
func (s *Store) ResolveBranch(repo *Repo, branchOrRef string) (string, bool) {
	if strings.HasPrefix(branchOrRef, "refs/") {
		v, ok := repo.Refs[branchOrRef]
		return v, ok
	}
	v, ok := repo.Refs[s.refName(repo, branchOrRef)]
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

// Read a file (blob) at a ref; nil when the ref/path does not exist.
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
	flat := flattenTree(repo.Store, tree, "", nil)
	entry, found := flat[path]
	if !found {
		return "", nil, false
	}
	blob := repo.Store.Get(entry.OID)
	if blob == nil {
		return "", nil, false
	}
	return entry.OID, blob.Data, true
}

// List a directory at a ref: nil when the ref/path does not exist.
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
		// Find the subtree at `path`.
		var walk func(tree string, parts []string) (string, bool)
		walk = func(tree string, parts []string) (string, bool) {
			if len(parts) == 0 {
				return tree, true
			}
			obj := repo.Store.Get(tree)
			if obj == nil || obj.Type != ObjTree {
				return "", false
			}
			entries, err := parseTree(obj.Data)
			if err != nil {
				return "", false
			}
			for _, e := range entries {
				if e.Name == parts[0] && e.Mode == "40000" {
					return walk(e.OID, parts[1:])
				}
			}
			return "", false
		}
		foundOid, ok := walk(treeOid, strings.Split(path, "/"))
		if !ok {
			return nil
		}
		treeOid = foundOid
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
		p := e.Name
		if path != "" {
			p = path + "/" + e.Name
		}
		typ := "file"
		if e.Mode == "40000" {
			typ = "dir"
		}
		out = append(out, DirEntry{Name: e.Name, Path: p, OID: e.OID, Type: typ})
	}
	return out
}

// Set (or delete, when content is nil) one path in a tree; returns the new tree oid.
func (s *Store) setPathInTree(store *ObjectStore, treeOid string, hasTree bool, parts []string, content []byte) string {
	var entries []TreeEntry
	if hasTree {
		if obj := store.Get(treeOid); obj != nil {
			entries, _ = parseTree(obj.Data)
		}
	}
	head := parts[0]
	rest := parts[1:]
	idx := -1
	for i, e := range entries {
		if e.Name == head {
			idx = i
			break
		}
	}
	if len(rest) == 0 {
		if content == nil {
			if idx >= 0 {
				entries = append(entries[:idx], entries[idx+1:]...)
			}
		} else {
			blobOid := store.Put(ObjBlob, content)
			if idx >= 0 {
				entries[idx] = TreeEntry{Mode: "100644", Name: head, OID: blobOid}
			} else {
				entries = append(entries, TreeEntry{Mode: "100644", Name: head, OID: blobOid})
			}
		}
	} else {
		var childTree string
		hasChild := false
		if idx >= 0 && entries[idx].Mode == "40000" {
			childTree = entries[idx].OID
			hasChild = true
		}
		newChild := s.setPathInTree(store, childTree, hasChild, rest, content)
		// An emptied subtree disappears; a fresh one appears.
		childObj := store.Get(newChild)
		childEmpty := false
		if childObj != nil && childObj.Type == ObjTree {
			childEntries, _ := parseTree(childObj.Data)
			childEmpty = len(childEntries) == 0
		}
		if childEmpty {
			if idx >= 0 {
				entries = append(entries[:idx], entries[idx+1:]...)
			}
		} else if idx >= 0 {
			entries[idx] = TreeEntry{Mode: "40000", Name: head, OID: newChild}
		} else {
			entries = append(entries, TreeEntry{Mode: "40000", Name: head, OID: newChild})
		}
	}
	return store.Put(ObjTree, buildTree(entries))
}

type CommitFileResult struct {
	CommitSha string
	BlobSha   any // string | nil
	Created   bool
}

// Commit a file change on `branch` (creating the branch from the default tip
// when it does not exist, creating the root commit when the repo is unborn).
// Returns nil when the branch base does not exist.
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
		// New branch starts from the default branch tip.
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
	var parts []string
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

// Commits reachable from a ref, newest first (first-parent walk, linear histories).
func (s *Store) ListCommits(repo *Repo, branchOrRef string) []CommitListEntry {
	tip, found := s.ResolveBranch(repo, branchOrRef)
	if !found {
		return []CommitListEntry{}
	}
	var out []CommitListEntry
	seen := make(map[string]bool)
	queue := []string{tip}
	head := 0
	for head < len(queue) {
		oid := queue[head]
		head++
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
	if out == nil {
		out = []CommitListEntry{}
	}
	return out
}

// ---------------------------------------------------------- issues / labels

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

// ---------------------------------------------------------- pull requests

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

// Files changed between the merge-base tree and the head tree.
func (s *Store) PullFiles(repo *Repo, pr *Pull) []FileChange {
	base := mergeBase(repo.Store, pr.HeadSha, pr.BaseSha)
	var baseTree string
	if base != "" {
		if obj := repo.Store.Get(base); obj != nil {
			baseTree, _ = parseCommitLight(obj.Data)
		}
	}
	var headTree string
	if obj := repo.Store.Get(pr.HeadSha); obj != nil {
		headTree, _ = parseCommitLight(obj.Data)
	}
	return diffTrees(repo.Store, baseTree, headTree)
}

// Would a squash merge of this PR conflict? (Three-way, merge-base vs head vs base tip.)
func (s *Store) MergeConflicts(repo *Repo, pr *Pull) bool {
	baseTipSha, ok := s.ResolveBranch(repo, pr.BaseRef)
	if !ok {
		return true
	}
	base := mergeBase(repo.Store, pr.HeadSha, baseTipSha)
	var baseTreeOid string
	if base != "" {
		if obj := repo.Store.Get(base); obj != nil {
			baseTreeOid, _ = parseCommitLight(obj.Data)
		}
	}
	var headTreeOid, tipTreeOid string
	if obj := repo.Store.Get(pr.HeadSha); obj != nil {
		headTreeOid, _ = parseCommitLight(obj.Data)
	}
	if obj := repo.Store.Get(baseTipSha); obj != nil {
		tipTreeOid, _ = parseCommitLight(obj.Data)
	}
	baseFlat := make(map[string]TreeEntry)
	if baseTreeOid != "" {
		baseFlat = flattenTree(repo.Store, baseTreeOid, "", nil)
	}
	headFlat := flattenTree(repo.Store, headTreeOid, "", nil)
	tipFlat := flattenTree(repo.Store, tipTreeOid, "", nil)
	for path, h := range headFlat {
		b, hasB := baseFlat[path]
		if hasB && b.OID == h.OID {
			continue // unchanged from base
		}
		t, hasT := tipFlat[path]
		if hasT && (!hasB || t.OID != b.OID) && t.OID != h.OID {
			return true // both sides changed it
		}
	}
	for path, b := range baseFlat {
		if _, hasH := headFlat[path]; hasH {
			continue // deleted on head
		}
		t, hasT := tipFlat[path]
		if hasT && t.OID != b.OID {
			return true // deleted on head, changed on base
		}
	}
	return false
}

type MergeResult struct {
	Sha     string
	Message string
}

// Squash-merge a PR onto its base branch. Returns nil when the PR cannot
// merge (conflict or missing refs).
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
	base := mergeBase(repo.Store, pr.HeadSha, baseTipSha)
	var baseTreeOid string
	if base != "" {
		if obj := repo.Store.Get(base); obj != nil {
			baseTreeOid, _ = parseCommitLight(obj.Data)
		}
	}
	var headTreeOid, tipTreeOid string
	if obj := repo.Store.Get(pr.HeadSha); obj != nil {
		headTreeOid, _ = parseCommitLight(obj.Data)
	}
	if obj := repo.Store.Get(baseTipSha); obj != nil {
		tipTreeOid, _ = parseCommitLight(obj.Data)
	}
	baseFlat := make(map[string]TreeEntry)
	if baseTreeOid != "" {
		baseFlat = flattenTree(repo.Store, baseTreeOid, "", nil)
	}
	headFlat := flattenTree(repo.Store, headTreeOid, "", nil)
	// Start from the base tip tree; apply every head-side change.
	newTreeOid := tipTreeOid
	changed := make(map[string]bool)
	for path, h := range headFlat {
		b, hasB := baseFlat[path]
		if hasB && b.OID == h.OID {
			continue
		}
		changed[path] = true
	}
	for path := range baseFlat {
		if _, hasH := headFlat[path]; !hasH {
			changed[path] = true // deleted on head
		}
	}
	// Apply changes path by path, rebuilding trees bottom-up.
	sortedChanged := make([]string, 0, len(changed))
	for p := range changed {
		sortedChanged = append(sortedChanged, p)
	}
	sort.Strings(sortedChanged)
	for _, path := range sortedChanged {
		h, hasH := headFlat[path]
		var content []byte
		if hasH {
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

// ---------------------------------------------------------- JSON shapes

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
		label := repo.Labels[name]
		color := "ededed"
		desc := ""
		if label != nil {
			color = label.Color
			desc = label.Description
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

// The exact bytes sendJson would write for this issue (JSON + '\n'),
// memoized on the issue. Any mutation clears `jsonCache` first.
func (s *Store) IssueJSONBuffer(repo *Repo, issue *Issue) []byte {
	if issue.jsonCache == nil {
		b := marshalJSON(s.IssueJSON(repo, issue))
		b = append(b, '\n')
		issue.jsonCache = b
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
