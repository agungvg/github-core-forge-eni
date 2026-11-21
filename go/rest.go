// rest.go — the GitHub REST v3 surface, mounted under /api/v3.
//
// Every answer is JSON with a non-empty X-GitHub-Request-Id, and every
// error is a JSON {"message": ...} object. The bearer token is checked
// before anything else: no token, or the wrong one, is a 401, and a
// repository owned by anyone other than the token's login simply does
// not exist for this caller, so it reads as a 404 rather than a 403.
//
// The dispatcher is a small tree walk. The request path is split into
// segments once; the top level claims the root, /meta and /user, the
// next level claims /user/repos, and everything below /repos/{owner}/
// {repo} is claimed by one sub-dispatcher per resource family
// (branches, git refs, commits, contents, labels, issues, pulls).
// A sub-dispatcher that does not recognise its tail returns false and
// the next one gets a try; when none claims it, the answer is a JSON
// 404. Only "/" and "/meta" exist outside the resource tree.
//
// The read workload fetches one issue two thousand times, so that one
// shape has a fast lane: a plain GET /repos/{o}/{r}/issues/{n} with
// no escapes in the path is served straight from the store's memoized
// issue bytes, skipping body parsing and segment decoding entirely.

package main

import (
	"encoding/base64"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

// jsonType is the content type of every REST reply. It is also the
// type the app-level JSON replies use.
const jsonType = "application/json; charset=utf-8"

// freshRequestID mints the next request id: "req-" plus the counter in
// hex. The counter only ever goes up, so ids are unique per process.
func (s *server) freshRequestID() string {
	n := atomic.AddInt64(&s.requestCounter, 1)
	return "req-" + strconv.FormatInt(n, 16)
}

// replyJSON serializes payload, appends the trailing newline, and
// writes it with the JSON content type, a request id, and any extra
// headers. A 204 or a nil payload writes an empty body instead.
func (s *server) replyJSON(w *respWriter, status int, payload any, extra map[string]string) {
	w.header("content-type", jsonType)
	w.header("x-github-request-id", s.freshRequestID())
	for k, v := range extra {
		w.header(k, v)
	}
	if status == 204 || payload == nil {
		w.header("content-length", "0")
		w.writeHeader(status)
		return
	}
	text := append(marshalJSON(payload), '\n')
	w.header("content-length", strconv.Itoa(len(text)))
	w.writeHeader(status)
	w.write(text)
}

// replyCached writes bytes that are already serialized (the memoized
// issue JSON, newline included) with the same headers replyJSON sets.
func (s *server) replyCached(w *respWriter, status int, body []byte) {
	w.header("content-type", jsonType)
	w.header("x-github-request-id", s.freshRequestID())
	w.header("content-length", strconv.Itoa(len(body)))
	w.writeHeader(status)
	w.write(body)
}

// replyErr answers with a JSON error object: {"message": ...}.
func (s *server) replyErr(w *respWriter, status int, msg string) {
	s.replyJSON(w, status, map[string]any{"message": msg}, nil)
}

// tokenOK reports whether the request carries the server's bearer
// token. With no token configured, nobody is authorized.
func (s *server) tokenOK(r *http.Request) bool {
	if s.token == "" {
		return false
	}
	return r.Header.Get("Authorization") == s.expectedAuth
}

// decodeRequestBody decodes a request body. An empty body is an empty
// object; anything that is not valid JSON is a failure the caller
// turns into a 400.
func decodeRequestBody(raw []byte) (any, bool) {
	if len(raw) == 0 {
		return map[string]any{}, true
	}
	v, err := parseJSON(raw)
	if err != nil {
		return nil, false
	}
	return v, true
}

// slicePage cuts one page out of a full list and, when more pages
// follow, builds the Link rel="next" value for it. Page size defaults
// to 30 and caps at 100; the page number defaults to 1.
func slicePage(items []any, q queryParams, r *http.Request, origPath string) ([]any, string) {
	perPage := intParam(q, "per_page", 30)
	if perPage > 100 {
		perPage = 100
	}
	pageNo := intParam(q, "page", 1)
	start := (pageNo - 1) * perPage
	lo, hi := jsSlice(len(items), float64(start), float64(start+perPage))
	out := items[lo:hi]
	if out == nil {
		out = []any{}
	}
	link := ""
	if start+perPage < len(items) {
		host := r.Host
		if host == "" {
			host = "localhost"
		}
		next := q.Clone()
		next.Set("per_page", strconv.Itoa(perPage))
		next.Set("page", strconv.Itoa(pageNo+1))
		link = "<http://" + host + origPath + "?" + next.String() + ">; rel=\"next\""
	}
	return out, link
}

// pageLinkHeader turns a pagination link into response headers, or no
// headers at all when there is no next page.
func pageLinkHeader(link string) map[string]string {
	if link == "" {
		return nil
	}
	return map[string]string{"link": link}
}

// matchIssueRead recognizes a plain GET /repos/{owner}/{repo}/issues/{n}
// path for the fast lane. Only plain segment characters are accepted —
// no escapes, no trailing slash — so the fast lane never decodes
// anything. It returns the owner, repo and issue number when the path
// matches.
func matchIssueRead(path string) (owner, repo string, number int, ok bool) {
	if !strings.HasPrefix(path, "/repos/") {
		return "", "", 0, false
	}
	segs := strings.Split(path, "/")
	if len(segs) != 6 || segs[4] != "issues" {
		return "", "", 0, false
	}
	digits := segs[5]
	if digits == "" || digits[0] == '0' {
		return "", "", 0, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return "", "", 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return "", "", 0, false
	}
	return segs[2], segs[3], n, true
}

// parseResourceNumber reads an issue or pull-request number out of a
// path segment the way the store counts them: a positive integer.
// Anything else is not a number at all, so the caller lets the route
// fall through to the unknown-route 404.
func parseResourceNumber(seg string) (int, bool) {
	f, valid := jsNumber(seg)
	if !valid || !jsIsInteger(f) || f <= 0 {
		return 0, false
	}
	return int(f), true
}

// tailName returns the last segment of a slash-separated path.
func tailName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// encodeB64 renders bytes as standard base64, as the contents API does.
func encodeB64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// restCall carries everything one route handler needs: the request, the
// writer, the parsed query, the JSON body as a record, and the path
// split into raw segments. unescape decodes one segment; a malformed
// escape panics, and the recover in handleRest turns that into a 500.
type restCall struct {
	srv    *server
	w      *respWriter
	r      *http.Request
	query  queryParams
	orig   string
	fields map[string]any
	parts  []string
}

// unescape URI-decodes one already-split path segment.
func (c *restCall) unescape(seg string) string {
	dec, err := jsDecodeURIComponent(seg)
	if err != nil {
		panic(err)
	}
	return dec
}

// field decodes the path segment at index i, or "" past the end.
func (c *restCall) field(i int) string {
	if i >= len(c.parts) {
		return ""
	}
	return c.unescape(c.parts[i])
}

// stateFilter resolves a ?state= filter: absent means "open"; a
// present-but-empty value means "match nothing", since no issue or
// pull request ever carries the empty state.
func (c *restCall) stateFilter() string {
	v, present := c.query.Get("state")
	if !present {
		return "open"
	}
	return v
}

// ascendingSort reports whether the list should run oldest-first:
// only an explicit direction=asc asks for that.
func (c *restCall) ascendingSort() bool {
	v, _ := c.query.Get("direction")
	return v == "asc"
}

// checkedLabels extracts the label names from a create-issue body and
// checks each one against the repo. Entries may be plain strings or
// {"name": ...} objects; empty names are skipped. It returns the list
// and the first unknown label name, or "" when every label exists.
func (c *restCall) checkedLabels(repo *Repo) ([]string, string) {
	raw, isArr := c.fields["labels"].([]any)
	if !isArr {
		return nil, ""
	}
	var out []string
	for _, item := range raw {
		var name string
		switch v := item.(type) {
		case string:
			name = v
		case map[string]any:
			name, _ = asStringValue(v["name"])
		default:
			continue
		}
		if name == "" {
			continue
		}
		if _, exists := repo.Labels[name]; !exists {
			return nil, name
		}
		out = append(out, name)
	}
	return out, ""
}

// handleRest routes one REST request. It reports whether the path sits
// under /api/v3 (handled, even on 404) so the app router knows whether
// to try the next surface.
func (s *server) handleRest(r *http.Request, w *respWriter, pathname string, search queryParams, rawBody []byte) (claimed bool) {
	trimmed := pathname
	if trimmed == "/api/v3" {
		trimmed = "/api/v3/"
	} else {
		trimmed = strings.TrimRight(trimmed, "/")
		if trimmed == "" {
			trimmed = "/"
		}
	}
	if !strings.HasPrefix(trimmed, "/api/v3") {
		return false
	}
	claimed = true
	restPath := trimmed[len("/api/v3"):]
	if restPath == "" {
		restPath = "/"
	}

	// Auth first: every API call carries the bearer token.
	if !s.tokenOK(r) {
		s.replyErr(w, 401, "Bad credentials")
		return true
	}

	// Fast lane: GET /repos/{owner}/{repo}/issues/{n}. This is the
	// read workload's shape, and it skips body parsing, segment
	// decoding, and JSON encoding by serving the memoized issue.
	if r.Method == "GET" && !strings.Contains(restPath, "%") {
		if owner, name, num, ok := matchIssueRead(restPath); ok {
			repo := s.store.GetRepo(owner, name)
			if repo == nil {
				s.replyErr(w, 404, "Not Found")
				return true
			}
			issue := s.store.GetIssue(repo, num)
			if issue == nil {
				s.replyErr(w, 404, "Not Found")
				return true
			}
			s.replyCached(w, 200, s.store.IssueJSONBuffer(repo, issue))
			return true
		}
	}

	// Body parsing for methods that carry one. Malformed JSON is a 400.
	var doc any = map[string]any{}
	if r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" || r.Method == "DELETE" {
		parsed, ok := decodeRequestBody(rawBody)
		if !ok {
			s.replyErr(w, 400, "Problems parsing JSON")
			return true
		}
		doc = parsed
	}

	// Any panic below (for example a bad escape in a segment) answers
	// 500, as long as nothing has been written yet.
	defer func() {
		if rec := recover(); rec != nil {
			if !w.sent {
				s.replyErr(w, 500, "Internal Server Error")
			}
		}
	}()

	var parts []string
	for _, seg := range strings.Split(restPath, "/") {
		if seg != "" {
			parts = append(parts, seg)
		}
	}
	call := &restCall{
		srv: s, w: w, r: r, query: search,
		orig: pathname, fields: asRecord(doc), parts: parts,
	}
	if call.serveTop(r.Method, restPath) {
		return true
	}
	if call.serveUserRepos(r.Method) {
		return true
	}
	if call.serveRepo(r.Method) {
		return true
	}
	s.replyErr(w, 404, "Not Found")
	return true
}

// serveTop answers the API root, /meta, and /user.
func (c *restCall) serveTop(method, path string) bool {
	if method != "GET" {
		return false
	}
	switch path {
	case "/":
		c.srv.replyJSON(c.w, 200,
			map[string]any{"current_user_url": "/api/v3/user", "rate_limit_url": "/api/v3/rate_limit"},
			map[string]string{"x-oauth-scopes": "repo, read:org"})
		return true
	case "/meta":
		c.srv.replyJSON(c.w, 200,
			map[string]any{"installed_version": "1.0.0", "verifiable_password_authentication": false}, nil)
		return true
	case "/user":
		c.srv.replyJSON(c.w, 200,
			map[string]any{"login": viewerLogin, "id": viewerID, "name": viewerName, "email": viewerEmail, "type": "User"}, nil)
		return true
	}
	return false
}

// serveUserRepos answers GET and POST /user/repos.
func (c *restCall) serveUserRepos(method string) bool {
	if len(c.parts) != 2 || c.parts[0] != "user" || c.parts[1] != "repos" {
		return false
	}
	s := c.srv
	switch method {
	case "GET":
		repos := s.store.ListViewerRepos()
		items := make([]any, 0, len(repos))
		for _, repo := range repos {
			items = append(items, s.store.RepoJSON(repo))
		}
		page, link := slicePage(items, c.query, c.r, c.orig)
		s.replyJSON(c.w, 200, page, pageLinkHeader(link))
		return true
	case "POST":
		name, ok := asStringValue(c.fields["name"])
		if !ok || name == "" {
			s.replyErr(c.w, 422, "name is required")
			return true
		}
		desc, hasDesc := asStringValue(c.fields["description"])
		priv, _ := c.fields["private"].(bool)
		repo := s.store.CreateRepo(name, desc, hasDesc, priv, "", false)
		if repo == nil {
			s.replyErr(c.w, 422, "Repository already exists")
			return true
		}
		s.replyJSON(c.w, 201, s.store.RepoJSON(repo), nil)
		return true
	}
	return false
}

// serveRepo answers everything under /repos/{owner}/{repo}. The repo
// is looked up once; a missing repo is a 404 here, whichever
// sub-resource was asked for.
func (c *restCall) serveRepo(method string) bool {
	if len(c.parts) < 3 || c.parts[0] != "repos" {
		return false
	}
	s := c.srv
	owner := c.field(1)
	name := c.field(2)
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		s.replyErr(c.w, 404, "Not Found")
		return true
	}
	tail := c.parts[3:]

	// The repo itself: read, update, delete.
	if len(tail) == 0 {
		switch method {
		case "GET":
			s.replyJSON(c.w, 200, s.store.RepoJSON(repo), nil)
			return true
		case "PATCH":
			if desc, ok := asStringValue(c.fields["description"]); ok {
				repo.Description = desc
			}
			if priv, ok := c.fields["private"].(bool); ok {
				repo.Private = priv
			}
			repo.UpdatedSeq = s.store.nextSeq()
			s.replyJSON(c.w, 200, s.store.RepoJSON(repo), nil)
			return true
		case "DELETE":
			s.store.DeleteRepo(owner, name)
			s.replyJSON(c.w, 204, nil, nil)
			return true
		}
		return false
	}

	if c.serveBranches(method, repo, tail) {
		return true
	}
	if c.serveRefs(method, repo, tail) {
		return true
	}
	if c.serveCommitList(method, repo, tail) {
		return true
	}
	if c.serveContents(method, repo, tail) {
		return true
	}
	if c.serveLabels(method, repo, tail) {
		return true
	}
	if c.serveIssues(method, repo, tail) {
		return true
	}
	if c.servePulls(method, repo, tail) {
		return true
	}
	return false
}

// serveBranches answers GET /branches and GET /branches/{branch}. The
// list puts the default branch first, then the rest alphabetically.
func (c *restCall) serveBranches(method string, repo *Repo, tail []string) bool {
	if len(tail) < 1 || tail[0] != "branches" {
		return false
	}
	s := c.srv
	if method == "GET" && len(tail) == 1 {
		var others []string
		for ref := range repo.Refs {
			if !strings.HasPrefix(ref, "refs/heads/") {
				continue
			}
			name := ref[len("refs/heads/"):]
			if name != repo.DefaultBranch {
				others = append(others, name)
			}
		}
		sort.Strings(others)
		ordered := make([]string, 0, len(others)+1)
		if _, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
			ordered = append(ordered, repo.DefaultBranch)
		}
		ordered = append(ordered, others...)
		out := make([]any, 0, len(ordered))
		for _, name := range ordered {
			out = append(out, map[string]any{"name": name, "commit": map[string]any{"sha": repo.Refs["refs/heads/"+name]}})
		}
		s.replyJSON(c.w, 200, out, nil)
		return true
	}
	if method == "GET" && len(tail) == 2 {
		branch := c.unescape(tail[1])
		sha, ok := repo.Refs["refs/heads/"+branch]
		if !ok {
			s.replyErr(c.w, 404, "Branch not found")
			return true
		}
		s.replyJSON(c.w, 200, map[string]any{"name": branch, "commit": map[string]any{"sha": sha}}, nil)
		return true
	}
	return false
}

// serveRefs answers GET /git/refs/heads/{branch} and POST /git/refs.
func (c *restCall) serveRefs(method string, repo *Repo, tail []string) bool {
	if len(tail) < 2 || tail[0] != "git" || tail[1] != "refs" {
		return false
	}
	s := c.srv
	if method == "GET" && len(tail) >= 4 && tail[2] == "heads" {
		bits := make([]string, 0, len(tail)-3)
		for _, seg := range tail[3:] {
			bits = append(bits, c.unescape(seg))
		}
		branch := strings.Join(bits, "/")
		sha, ok := repo.Refs["refs/heads/"+branch]
		if !ok {
			s.replyErr(c.w, 404, "Not Found")
			return true
		}
		s.replyJSON(c.w, 200, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"sha": sha, "type": "commit"}}, nil)
		return true
	}
	if method == "POST" && len(tail) == 2 {
		ref, okRef := asStringValue(c.fields["ref"])
		sha, okSha := asStringValue(c.fields["sha"])
		if !okRef || !okSha || ref == "" || sha == "" {
			s.replyErr(c.w, 422, "ref and sha are required")
			return true
		}
		obj := repo.Store.Get(sha)
		if obj == nil || obj.Type != ObjCommit {
			s.replyErr(c.w, 422, "sha is not a known commit")
			return true
		}
		repo.Refs[ref] = sha
		s.replyJSON(c.w, 201, map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"}}, nil)
		return true
	}
	return false
}

// serveCommitList answers GET /commits, optionally filtered by
// ?sha=<branch>. Each entry carries the message and the viewer as
// author and committer.
func (c *restCall) serveCommitList(method string, repo *Repo, tail []string) bool {
	if len(tail) != 1 || tail[0] != "commits" || method != "GET" {
		return false
	}
	s := c.srv
	branch, ok := c.query.Get("sha")
	if !ok {
		branch = repo.DefaultBranch
	}
	entries := s.store.ListCommits(repo, branch)
	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		out = append(out, map[string]any{
			"sha": entry.Sha,
			"commit": map[string]any{
				"message":   entry.Message,
				"author":    map[string]any{"name": viewerName, "email": viewerEmail},
				"committer": map[string]any{"name": viewerName, "email": viewerEmail},
			},
		})
	}
	s.replyJSON(c.w, 200, out, nil)
	return true
}

// serveContents answers GET/PUT/DELETE /contents/{path...}. A GET on a
// file returns its base64 content; a GET on a directory returns its
// entries. A PUT creates (201) or updates (200) a file, refusing a
// stale sha with 409; a DELETE removes a file and returns the deletion
// commit.
func (c *restCall) serveContents(method string, repo *Repo, tail []string) bool {
	if len(tail) < 1 || tail[0] != "contents" {
		return false
	}
	s := c.srv
	bits := make([]string, 0, len(tail)-1)
	for _, seg := range tail[1:] {
		bits = append(bits, c.unescape(seg))
	}
	path := strings.Join(bits, "/")
	switch method {
	case "GET":
		ref, ok := c.query.Get("ref")
		if !ok {
			ref = repo.DefaultBranch
		}
		if path != "" {
			if oid, data, found := s.store.ReadFileAt(repo, ref, path); found {
				s.replyJSON(c.w, 200, map[string]any{
					"type": "file", "encoding": "base64",
					"content": encodeB64(data), "sha": oid, "size": len(data),
					"name": tailName(path), "path": path,
				}, nil)
				return true
			}
		}
		if dir := s.store.ListDirAt(repo, ref, path); dir != nil {
			out := make([]any, 0, len(dir))
			for _, e := range dir {
				out = append(out, map[string]any{"type": e.Type, "name": e.Name, "path": e.Path, "sha": e.OID})
			}
			s.replyJSON(c.w, 200, out, nil)
			return true
		}
		s.replyErr(c.w, 404, "Not Found")
		return true
	case "PUT":
		message, _ := asStringValue(c.fields["message"])
		encoded, okEnc := asStringValue(c.fields["content"])
		if !okEnc {
			s.replyErr(c.w, 422, "content is required")
			return true
		}
		data := lenientBase64Decode(encoded)
		branch, okBranch := asStringValue(c.fields["branch"])
		if !okBranch {
			branch = repo.DefaultBranch
		}
		curOid, _, exists := s.store.ReadFileAt(repo, branch, path)
		sha, hasSha := asStringValue(c.fields["sha"])
		if exists && (!hasSha || sha != curOid) {
			s.replyErr(c.w, 409, "sha does not match the current blob")
			return true
		}
		res := s.store.CommitFile(repo, branch, path, data, message)
		if res == nil {
			s.replyErr(c.w, 404, "Branch not found")
			return true
		}
		status := 201
		if exists {
			status = 200
		}
		s.replyJSON(c.w, status, map[string]any{
			"content": map[string]any{"name": tailName(path), "path": path, "sha": res.BlobSha, "encoding": "base64"},
			"commit":  map[string]any{"sha": res.CommitSha},
		}, nil)
		return true
	case "DELETE":
		oid, _, exists := s.store.ReadFileAt(repo, repo.DefaultBranch, path)
		if !exists {
			s.replyErr(c.w, 404, "Not Found")
			return true
		}
		sha, hasSha := asStringValue(c.fields["sha"])
		if hasSha && sha != oid {
			s.replyErr(c.w, 409, "sha does not match the current blob")
			return true
		}
		message, _ := asStringValue(c.fields["message"])
		res := s.store.CommitFile(repo, repo.DefaultBranch, path, nil, message)
		if res == nil {
			s.replyErr(c.w, 404, "Not Found")
			return true
		}
		s.replyJSON(c.w, 200, map[string]any{"commit": map[string]any{"sha": res.CommitSha}}, nil)
		return true
	}
	return false
}

// serveLabels answers GET and POST /labels. Labels are repo-scoped
// and listed in creation order.
func (c *restCall) serveLabels(method string, repo *Repo, tail []string) bool {
	if len(tail) < 1 || tail[0] != "labels" {
		return false
	}
	s := c.srv
	if method == "GET" && len(tail) == 1 {
		out := make([]any, 0, len(repo.LabelOrder))
		for _, name := range repo.LabelOrder {
			out = append(out, s.store.LabelJSON(repo.Labels[name]))
		}
		s.replyJSON(c.w, 200, out, nil)
		return true
	}
	if method == "POST" && len(tail) == 1 {
		name, ok := asStringValue(c.fields["name"])
		if !ok || name == "" {
			s.replyErr(c.w, 422, "name is required")
			return true
		}
		color, okColor := asStringValue(c.fields["color"])
		if !okColor {
			color = "ededed"
		}
		label := s.store.CreateLabel(repo, name, color)
		s.replyJSON(c.w, 201, s.store.LabelJSON(label), nil)
		return true
	}
	return false
}

// serveIssues answers /issues: list and create, one issue by number
// (read and patch), and its comments.
func (c *restCall) serveIssues(method string, repo *Repo, tail []string) bool {
	if len(tail) < 1 || tail[0] != "issues" {
		return false
	}
	s := c.srv
	if len(tail) == 1 {
		switch method {
		case "GET":
			state := c.stateFilter()
			asc := c.ascendingSort()
			var matched []*Issue
			for _, issue := range repo.Issues {
				if state == "all" || issue.State == state {
					matched = append(matched, issue)
				}
			}
			sort.SliceStable(matched, func(i, j int) bool {
				if asc {
					return matched[i].CreatedSeq < matched[j].CreatedSeq
				}
				return matched[i].CreatedSeq > matched[j].CreatedSeq
			})
			items := make([]any, 0, len(matched))
			for _, issue := range matched {
				items = append(items, s.store.IssueJSON(repo, issue))
			}
			page, link := slicePage(items, c.query, c.r, c.orig)
			s.replyJSON(c.w, 200, page, pageLinkHeader(link))
			return true
		case "POST":
			title, ok := asStringValue(c.fields["title"])
			if !ok || title == "" {
				s.replyErr(c.w, 422, "title is required")
				return true
			}
			labels, bad := c.checkedLabels(repo)
			if bad != "" {
				s.replyErr(c.w, 422, "label "+bad+" does not exist")
				return true
			}
			body, _ := asStringValue(c.fields["body"])
			issue := s.store.CreateIssue(repo, title, body, labels)
			s.replyCached(c.w, 201, s.store.IssueJSONBuffer(repo, issue))
			return true
		}
		return false
	}
	num, okNum := parseResourceNumber(tail[1])
	if !okNum {
		return false
	}
	issue := s.store.GetIssue(repo, num)
	if issue == nil {
		s.replyErr(c.w, 404, "Not Found")
		return true
	}
	if len(tail) == 2 {
		switch method {
		case "GET":
			s.replyCached(c.w, 200, s.store.IssueJSONBuffer(repo, issue))
			return true
		case "PATCH":
			if state, ok := asStringValue(c.fields["state"]); ok && (state == "open" || state == "closed") {
				issue.State = state
			}
			if title, ok := asStringValue(c.fields["title"]); ok {
				issue.Title = title
			}
			if body, ok := asStringValue(c.fields["body"]); ok {
				issue.Body = body
			}
			issue.UpdatedSeq = s.store.nextSeq()
			issue.jsonCache = nil
			s.replyJSON(c.w, 200, s.store.IssueJSON(repo, issue), nil)
			return true
		}
		return false
	}
	if len(tail) >= 3 && tail[2] == "comments" {
		switch method {
		case "GET":
			out := make([]any, 0, len(issue.Comments))
			for _, comment := range issue.Comments {
				out = append(out, s.store.CommentJSON(comment))
			}
			s.replyJSON(c.w, 200, out, nil)
			return true
		case "POST":
			text, ok := asStringValue(c.fields["body"])
			if !ok {
				s.replyErr(c.w, 422, "body is required")
				return true
			}
			comment := s.store.AddComment(repo, issue, text)
			s.replyJSON(c.w, 201, s.store.CommentJSON(comment), nil)
			return true
		}
	}
	return false
}

// servePulls answers /pulls: list and create, one pull by number, its
// changed files, and the squash merge.
func (c *restCall) servePulls(method string, repo *Repo, tail []string) bool {
	if len(tail) < 1 || tail[0] != "pulls" {
		return false
	}
	s := c.srv
	if len(tail) == 1 {
		switch method {
		case "GET":
			state := c.stateFilter()
			items := make([]any, 0, len(repo.Pulls))
			for _, pr := range repo.Pulls {
				if state == "all" || pr.State == state {
					items = append(items, s.store.PullJSON(repo, pr))
				}
			}
			page, link := slicePage(items, c.query, c.r, c.orig)
			s.replyJSON(c.w, 200, page, pageLinkHeader(link))
			return true
		case "POST":
			title, okTitle := asStringValue(c.fields["title"])
			head, okHead := asStringValue(c.fields["head"])
			base, okBase := asStringValue(c.fields["base"])
			if !okBase {
				base = repo.DefaultBranch
			}
			if !okTitle || !okHead || title == "" || head == "" {
				s.replyErr(c.w, 422, "title and head are required")
				return true
			}
			if _, ok := s.store.ResolveBranch(repo, head); !ok {
				s.replyErr(c.w, 422, "head or base branch does not exist")
				return true
			}
			if _, ok := s.store.ResolveBranch(repo, base); !ok {
				s.replyErr(c.w, 422, "head or base branch does not exist")
				return true
			}
			for _, pr := range repo.Pulls {
				if pr.State == "open" && pr.HeadRef == head && pr.BaseRef == base {
					s.replyErr(c.w, 422, "A pull request already exists for this head and base")
					return true
				}
			}
			body, _ := asStringValue(c.fields["body"])
			pr := s.store.CreatePull(repo, title, body, head, base)
			if pr == nil {
				s.replyErr(c.w, 422, "head or base branch does not exist")
				return true
			}
			s.replyJSON(c.w, 201, s.store.PullJSON(repo, pr), nil)
			return true
		}
		return false
	}
	num, okNum := parseResourceNumber(tail[1])
	if !okNum {
		return false
	}
	pr := s.store.GetPull(repo, num)
	if pr == nil {
		s.replyErr(c.w, 404, "Not Found")
		return true
	}
	if len(tail) == 2 && method == "GET" {
		s.replyJSON(c.w, 200, s.store.PullJSON(repo, pr), nil)
		return true
	}
	if len(tail) == 3 && tail[2] == "files" && method == "GET" {
		files := s.store.PullFiles(repo, pr)
		out := make([]any, 0, len(files))
		for _, f := range files {
			out = append(out, map[string]any{
				"filename": f.Path, "status": f.Status,
				"additions": f.Additions, "deletions": f.Deletions,
				"changes": f.Additions + f.Deletions,
			})
		}
		s.replyJSON(c.w, 200, out, nil)
		return true
	}
	if len(tail) == 3 && tail[2] == "merge" && method == "PUT" {
		if pr.State != "open" {
			s.replyErr(c.w, 405, "Pull request is already closed")
			return true
		}
		merged := s.store.MergePull(repo, pr)
		if merged == nil {
			s.replyErr(c.w, 405, "This pull request cannot be merged: conflicts")
			return true
		}
		s.replyJSON(c.w, 200, map[string]any{"sha": merged.Sha, "merged": true, "message": merged.Message}, nil)
		return true
	}
	return false
}
