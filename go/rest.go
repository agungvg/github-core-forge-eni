// rest.go — REST v3 under /api/v3.
//
// Every reply is application/json (never HTML) and carries a non-empty
// X-GitHub-Request-Id. Errors are JSON {"message": ...}. Auth is a bearer
// token; a missing or wrong token answers 401 before anything else, and a
// repo under another owner answers 404 — never 403 — because from the
// token's point of view that repo simply does not exist.
//
// Routing is table-free and deliberately boring: the path is split into
// segments once, then each section (root, user repos, one repo and its
// sub-resources) claims the shapes it owns. The read workload hammers one
// shape — GET a single issue — so that shape has a fast path that skips
// body parsing, segment decoding, and JSON encoding entirely.

package main

import (
	"encoding/base64"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

const jsonType = "application/json; charset=utf-8"

// sendCachedJson writes a body that is already serialized (the memoized
// issue JSON), with the same headers sendJson would attach.
func (s *server) sendCachedJson(w *respWriter, status int, body []byte) {
	n := atomic.AddInt64(&s.requestCounter, 1)
	w.header("content-type", jsonType)
	w.header("x-github-request-id", "req-"+strconv.FormatInt(n, 16))
	w.header("content-length", strconv.Itoa(len(body)))
	w.writeHeader(status)
	w.write(body)
}

// sendJson serializes a value, appends the trailing newline the reference
// server emits, and writes it with the JSON content type and a request id.
// A 204 or a nil body writes an empty body with content-length 0.
func (s *server) sendJson(w *respWriter, status int, body any, extra map[string]string) {
	n := atomic.AddInt64(&s.requestCounter, 1)
	w.header("content-type", jsonType)
	w.header("x-github-request-id", "req-"+strconv.FormatInt(n, 16))
	for k, v := range extra {
		w.header(k, v)
	}
	if status == 204 || body == nil {
		w.header("content-length", "0")
		w.writeHeader(status)
		return
	}
	text := append(marshalJSON(body), '\n')
	w.header("content-length", strconv.Itoa(len(text)))
	w.writeHeader(status)
	w.write(text)
}

// sendError answers with a JSON error object: {"message": ...}.
func (s *server) sendError(w *respWriter, status int, message string) {
	s.sendJson(w, status, map[string]any{"message": message}, nil)
}

// isAuthorized checks the bearer token against the one the server was
// started with. An empty server token authorizes nobody.
func (s *server) isAuthorized(r *http.Request) bool {
	if s.token == "" {
		return false
	}
	return r.Header.Get("Authorization") == s.expectedAuth
}

// parseBody decodes a request body. An empty body is an empty object; a
// body that is not valid JSON is a failure the caller turns into a 400.
func parseBody(raw []byte) (any, bool) {
	if len(raw) == 0 {
		return map[string]any{}, true
	}
	v, err := parseJSON(raw)
	if err != nil {
		return nil, false
	}
	return v, true
}

// paginate slices a full list into one page and, when more pages follow,
// builds the `Link: rel="next"` value for it. Page size defaults to 30
// and caps at 100.
func paginate(items []any, search queryParams, r *http.Request, pathname string) (page []any, link string) {
	perPage := intParam(search, "per_page", 30)
	if perPage > 100 {
		perPage = 100
	}
	pageNum := intParam(search, "page", 1)
	start := (pageNum - 1) * perPage
	lo, hi := jsSlice(len(items), float64(start), float64(start+perPage))
	page = items[lo:hi]
	if page == nil {
		page = []any{}
	}
	if start+perPage < len(items) {
		host := r.Host
		if host == "" {
			host = "localhost"
		}
		params := search.Clone()
		params.Set("per_page", strconv.Itoa(perPage))
		params.Set("page", strconv.Itoa(pageNum+1))
		link = "<http://" + host + pathname + "?" + params.String() + ">; rel=\"next\""
	}
	return page, link
}

// linkHeaders turns a pagination link into response headers, or no headers
// when there is no next page.
func linkHeaders(link string) map[string]string {
	if link == "" {
		return nil
	}
	return map[string]string{"link": link}
}

// singleIssuePath matches GET /repos/{owner}/{repo}/issues/{n} for the fast
// path. Only plain segment characters are allowed — no escapes, no
// trailing slash — so the fast path never has to decode anything.
var singleIssuePath = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/([1-9][0-9]*)$`)

// restCtx carries everything a route handler needs: the request, the
// writer, the parsed query, the JSON body as a record, and the path split
// into segments. decode reads one segment with URI decoding; a malformed
// escape panics, and the recover in handleRest turns that into a 500,
// matching the reference server.
type restCtx struct {
	srv      *server
	w        *respWriter
	r        *http.Request
	search   queryParams
	pathname string
	body     map[string]any
	segs     []string
}

func (c *restCtx) decode(i int) string {
	if i >= len(c.segs) {
		return ""
	}
	dec, err := jsDecodeURIComponent(c.segs[i])
	if err != nil {
		panic(err)
	}
	return dec
}

// handleRest routes one REST request. It returns true when the path is
// under /api/v3 (handled, even on 404) and false otherwise, so the app
// router can try the next surface.
func (s *server) handleRest(r *http.Request, w *respWriter, pathname string, search queryParams, rawBody []byte) bool {
	clean := pathname
	if pathname == "/api/v3" {
		clean = "/api/v3/"
	} else {
		clean = strings.TrimRight(pathname, "/")
		if clean == "" {
			clean = "/"
		}
	}
	if !strings.HasPrefix(clean, "/api/v3") {
		return false
	}
	method := r.Method
	path := clean[len("/api/v3"):]
	if path == "" {
		path = "/"
	}

	// Auth first: every API call carries the bearer token.
	if !s.isAuthorized(r) {
		s.sendError(w, 401, "Bad credentials")
		return true
	}

	// Fast path: GET /repos/{owner}/{repo}/issues/{n}. This is the read
	// workload's 2,000 sequential requests, and it skips body parsing,
	// segment decoding, and JSON encoding by serving the memoized issue.
	if method == "GET" && !strings.Contains(path, "%") {
		if m := singleIssuePath.FindStringSubmatch(path); m != nil {
			repo := s.store.GetRepo(m[1], m[2])
			if repo == nil {
				s.sendError(w, 404, "Not Found")
				return true
			}
			num, _ := strconv.Atoi(m[3])
			issue := s.store.GetIssue(repo, num)
			if issue == nil {
				s.sendError(w, 404, "Not Found")
				return true
			}
			s.sendCachedJson(w, 200, s.store.IssueJSONBuffer(repo, issue))
			return true
		}
	}

	// Body parsing for methods that carry one. Malformed JSON is a 400.
	var bodyValue any = map[string]any{}
	if method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE" {
		parsed, ok := parseBody(rawBody)
		if !ok {
			s.sendError(w, 400, "Problems parsing JSON")
			return true
		}
		bodyValue = parsed
	}

	// Any panic below (for example a bad escape in a segment) answers 500,
	// as the reference server's catch does.
	defer func() {
		if rec := recover(); rec != nil {
			if !w.wrote {
				s.sendError(w, 500, "Internal Server Error")
			}
		}
	}()

	var segs []string
	for _, seg := range strings.Split(path, "/") {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	ctx := &restCtx{
		srv: s, w: w, r: r, search: search,
		pathname: pathname, body: asRecord(bodyValue), segs: segs,
	}
	if ctx.routeRoot(method, path) {
		return true
	}
	if ctx.routeUserRepos(method) {
		return true
	}
	if ctx.routeRepo(method) {
		return true
	}
	s.sendError(w, 404, "Not Found")
	return true
}

// routeRoot answers the API root, /meta, and /user.
func (c *restCtx) routeRoot(method, path string) bool {
	if method != "GET" {
		return false
	}
	switch path {
	case "/":
		c.srv.sendJson(c.w, 200,
			map[string]any{"current_user_url": "/api/v3/user", "rate_limit_url": "/api/v3/rate_limit"},
			map[string]string{"x-oauth-scopes": "repo, read:org"})
		return true
	case "/meta":
		c.srv.sendJson(c.w, 200,
			map[string]any{"installed_version": "1.0.0", "verifiable_password_authentication": false}, nil)
		return true
	case "/user":
		c.srv.sendJson(c.w, 200,
			map[string]any{"login": viewerLogin, "id": viewerID, "name": viewerName, "email": viewerEmail, "type": "User"}, nil)
		return true
	}
	return false
}

// routeUserRepos answers GET and POST /user/repos.
func (c *restCtx) routeUserRepos(method string) bool {
	if len(c.segs) != 2 || c.segs[0] != "user" || c.segs[1] != "repos" {
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
		page, link := paginate(items, c.search, c.r, c.pathname)
		s.sendJson(c.w, 200, page, linkHeaders(link))
		return true
	case "POST":
		name, ok := asStringValue(c.body["name"])
		if !ok || name == "" {
			s.sendError(c.w, 422, "name is required")
			return true
		}
		desc, _ := asStringValue(c.body["description"])
		priv, _ := c.body["private"].(bool)
		repo := s.store.CreateRepo(name, desc, true, priv, "", false)
		if repo == nil {
			s.sendError(c.w, 422, "Repository already exists")
			return true
		}
		s.sendJson(c.w, 201, s.store.RepoJSON(repo), nil)
		return true
	}
	return false
}

// routeRepo answers everything under /repos/{owner}/{repo}. The repo is
// looked up once; a missing repo is a 404 here, whichever sub-resource was
// asked for.
func (c *restCtx) routeRepo(method string) bool {
	if len(c.segs) < 3 || c.segs[0] != "repos" {
		return false
	}
	s := c.srv
	owner := c.decode(1)
	repoName := c.decode(2)
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		s.sendError(c.w, 404, "Not Found")
		return true
	}
	rest := c.segs[3:]

	// The repo itself: read, update, delete.
	if len(rest) == 0 {
		switch method {
		case "GET":
			s.sendJson(c.w, 200, s.store.RepoJSON(repo), nil)
			return true
		case "PATCH":
			if description, ok := asStringValue(c.body["description"]); ok {
				repo.Description = description
			}
			if priv, ok := c.body["private"].(bool); ok {
				repo.Private = priv
			}
			repo.UpdatedSeq = s.store.nextSeq()
			s.sendJson(c.w, 200, s.store.RepoJSON(repo), nil)
			return true
		case "DELETE":
			s.store.DeleteRepo(owner, repoName)
			s.sendJson(c.w, 204, nil, nil)
			return true
		}
		return false
	}

	if c.routeBranches(method, repo, rest) {
		return true
	}
	if c.routeGitRefs(method, repo, rest) {
		return true
	}
	if c.routeCommits(method, repo, rest) {
		return true
	}
	if c.routeContents(method, repo, rest) {
		return true
	}
	if c.routeLabels(method, repo, rest) {
		return true
	}
	if c.routeIssues(method, repo, rest) {
		return true
	}
	if c.routePulls(method, repo, rest) {
		return true
	}
	return false
}

// routeBranches answers GET /branches and GET /branches/{branch}. The list
// puts the default branch first, then the rest alphabetically.
func (c *restCtx) routeBranches(method string, repo *Repo, rest []string) bool {
	if len(rest) < 1 || rest[0] != "branches" {
		return false
	}
	s := c.srv
	if method == "GET" && len(rest) == 1 {
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
		names := make([]string, 0, len(others)+1)
		if _, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
			names = append(names, repo.DefaultBranch)
		}
		names = append(names, others...)
		out := make([]any, 0, len(names))
		for _, name := range names {
			out = append(out, map[string]any{"name": name, "commit": map[string]any{"sha": repo.Refs["refs/heads/"+name]}})
		}
		s.sendJson(c.w, 200, out, nil)
		return true
	}
	if method == "GET" && len(rest) == 2 {
		branch := c.decode(4)
		sha, ok := repo.Refs["refs/heads/"+branch]
		if !ok {
			s.sendError(c.w, 404, "Branch not found")
			return true
		}
		s.sendJson(c.w, 200, map[string]any{"name": branch, "commit": map[string]any{"sha": sha}}, nil)
		return true
	}
	return false
}

// routeGitRefs answers GET /git/refs/heads/{branch} and POST /git/refs.
func (c *restCtx) routeGitRefs(method string, repo *Repo, rest []string) bool {
	if len(rest) < 2 || rest[0] != "git" || rest[1] != "refs" {
		return false
	}
	s := c.srv
	if method == "GET" && len(rest) >= 4 && rest[2] == "heads" {
		parts := make([]string, 0, len(rest)-3)
		for _, p := range rest[3:] {
			parts = append(parts, c.decodeSegment(p))
		}
		branch := strings.Join(parts, "/")
		sha, ok := repo.Refs["refs/heads/"+branch]
		if !ok {
			s.sendError(c.w, 404, "Not Found")
			return true
		}
		s.sendJson(c.w, 200, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"sha": sha, "type": "commit"}}, nil)
		return true
	}
	if method == "POST" && len(rest) == 2 {
		ref, okRef := asStringValue(c.body["ref"])
		sha, okSha := asStringValue(c.body["sha"])
		if !okRef || !okSha || ref == "" || sha == "" {
			s.sendError(c.w, 422, "ref and sha are required")
			return true
		}
		obj := repo.Store.Get(sha)
		if obj == nil || obj.Type != ObjCommit {
			s.sendError(c.w, 422, "sha is not a known commit")
			return true
		}
		repo.Refs[ref] = sha
		s.sendJson(c.w, 201, map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"}}, nil)
		return true
	}
	return false
}

// decodeSegment URI-decodes one already-split path segment, panicking on a
// malformed escape (handled by the recover in handleRest).
func (c *restCtx) decodeSegment(seg string) string {
	dec, err := jsDecodeURIComponent(seg)
	if err != nil {
		panic(err)
	}
	return dec
}

// routeCommits answers GET /commits, optionally filtered by ?sha=<branch>.
// Each entry carries the message and the viewer as author and committer.
func (c *restCtx) routeCommits(method string, repo *Repo, rest []string) bool {
	if len(rest) != 1 || rest[0] != "commits" || method != "GET" {
		return false
	}
	s := c.srv
	branch, ok := c.search.Get("sha")
	if !ok {
		branch = repo.DefaultBranch
	}
	commits := s.store.ListCommits(repo, branch)
	out := make([]any, 0, len(commits))
	for _, commit := range commits {
		out = append(out, map[string]any{
			"sha": commit.Sha,
			"commit": map[string]any{
				"message":   commit.Message,
				"author":    map[string]any{"name": viewerName, "email": viewerEmail},
				"committer": map[string]any{"name": viewerName, "email": viewerEmail},
			},
		})
	}
	s.sendJson(c.w, 200, out, nil)
	return true
}

// routeContents answers GET/PUT/DELETE /contents/{path...}. A GET on a file
// returns its base64 content; a GET on a directory returns its entries.
// A PUT creates (201) or updates (200) a file, refusing a stale sha with
// 409; a DELETE removes a file and returns the deletion commit.
func (c *restCtx) routeContents(method string, repo *Repo, rest []string) bool {
	if len(rest) < 1 || rest[0] != "contents" {
		return false
	}
	s := c.srv
	parts := make([]string, 0, len(rest)-1)
	for _, p := range rest[1:] {
		parts = append(parts, c.decodeSegment(p))
	}
	contentPath := strings.Join(parts, "/")
	switch method {
	case "GET":
		ref, ok := c.search.Get("ref")
		if !ok {
			ref = repo.DefaultBranch
		}
		if contentPath != "" {
			if oid, data, found := s.store.ReadFileAt(repo, ref, contentPath); found {
				s.sendJson(c.w, 200, map[string]any{
					"type": "file", "encoding": "base64",
					"content": b64encode(data), "sha": oid, "size": len(data),
					"name": baseName(contentPath), "path": contentPath,
				}, nil)
				return true
			}
		}
		if dir := s.store.ListDirAt(repo, ref, contentPath); dir != nil {
			out := make([]any, 0, len(dir))
			for _, e := range dir {
				out = append(out, map[string]any{"type": e.Type, "name": e.Name, "path": e.Path, "sha": e.OID})
			}
			s.sendJson(c.w, 200, out, nil)
			return true
		}
		s.sendError(c.w, 404, "Not Found")
		return true
	case "PUT":
		message, _ := asStringValue(c.body["message"])
		contentB64, okB64 := asStringValue(c.body["content"])
		if !okB64 {
			s.sendError(c.w, 422, "content is required")
			return true
		}
		content := lenientBase64Decode(contentB64)
		branch, okBranch := asStringValue(c.body["branch"])
		if !okBranch {
			branch = repo.DefaultBranch
		}
		existingOid, _, existing := s.store.ReadFileAt(repo, branch, contentPath)
		sha, hasSha := asStringValue(c.body["sha"])
		if existing {
			if !hasSha || sha != existingOid {
				s.sendError(c.w, 409, "sha does not match the current blob")
				return true
			}
		}
		result := s.store.CommitFile(repo, branch, contentPath, content, message)
		if result == nil {
			s.sendError(c.w, 404, "Branch not found")
			return true
		}
		status := 201
		if existing {
			status = 200
		}
		s.sendJson(c.w, status, map[string]any{
			"content": map[string]any{"name": baseName(contentPath), "path": contentPath, "sha": result.BlobSha, "encoding": "base64"},
			"commit":  map[string]any{"sha": result.CommitSha},
		}, nil)
		return true
	case "DELETE":
		oid, _, existing := s.store.ReadFileAt(repo, repo.DefaultBranch, contentPath)
		if !existing {
			s.sendError(c.w, 404, "Not Found")
			return true
		}
		sha, hasSha := asStringValue(c.body["sha"])
		if hasSha && sha != oid {
			s.sendError(c.w, 409, "sha does not match the current blob")
			return true
		}
		message, _ := asStringValue(c.body["message"])
		result := s.store.CommitFile(repo, repo.DefaultBranch, contentPath, nil, message)
		if result == nil {
			s.sendError(c.w, 404, "Not Found")
			return true
		}
		s.sendJson(c.w, 200, map[string]any{"commit": map[string]any{"sha": result.CommitSha}}, nil)
		return true
	}
	return false
}

// baseName returns the last segment of a slash-separated path.
func baseName(path string) string {
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

// routeLabels answers GET and POST /labels. Labels are repo-scoped and
// listed in creation order.
func (c *restCtx) routeLabels(method string, repo *Repo, rest []string) bool {
	if len(rest) < 1 || rest[0] != "labels" {
		return false
	}
	s := c.srv
	if method == "GET" && len(rest) == 1 {
		out := make([]any, 0, len(repo.LabelOrder))
		for _, name := range repo.LabelOrder {
			out = append(out, s.store.LabelJSON(repo.Labels[name]))
		}
		s.sendJson(c.w, 200, out, nil)
		return true
	}
	if method == "POST" && len(rest) == 1 {
		name, ok := asStringValue(c.body["name"])
		if !ok || name == "" {
			s.sendError(c.w, 422, "name is required")
			return true
		}
		color, okColor := asStringValue(c.body["color"])
		if !okColor {
			color = "ededed"
		}
		label := s.store.CreateLabel(repo, name, color)
		s.sendJson(c.w, 201, s.store.LabelJSON(label), nil)
		return true
	}
	return false
}

// listState resolves a ?state= filter the way the reference does: an
// absent parameter means "open"; a present-but-empty one means "match
// nothing" (no issue has the empty state).
func (c *restCtx) listState() string {
	state, _ := c.search.Get("state")
	if state == "" {
		if _, present := c.search.Get("state"); !present {
			state = "open"
		}
	}
	return state
}

// listDirection resolves a ?direction= filter: absent means "desc".
func (c *restCtx) listDirection() string {
	direction, _ := c.search.Get("direction")
	if direction == "" {
		if _, present := c.search.Get("direction"); !present {
			direction = "desc"
		}
	}
	return direction
}

// routeIssues answers /issues: list and create, one issue by number (read
// and patch), and its comments.
func (c *restCtx) routeIssues(method string, repo *Repo, rest []string) bool {
	if len(rest) < 1 || rest[0] != "issues" {
		return false
	}
	s := c.srv
	if len(rest) == 1 {
		switch method {
		case "GET":
			state := c.listState()
			direction := c.listDirection()
			matched := make([]*Issue, 0, len(repo.Issues))
			for _, issue := range repo.Issues {
				if state == "all" || issue.State == state {
					matched = append(matched, issue)
				}
			}
			sort.SliceStable(matched, func(i, j int) bool {
				if direction == "asc" {
					return matched[i].CreatedSeq < matched[j].CreatedSeq
				}
				return matched[i].CreatedSeq > matched[j].CreatedSeq
			})
			items := make([]any, 0, len(matched))
			for _, issue := range matched {
				items = append(items, s.store.IssueJSON(repo, issue))
			}
			page, link := paginate(items, c.search, c.r, c.pathname)
			s.sendJson(c.w, 200, page, linkHeaders(link))
			return true
		case "POST":
			title, ok := asStringValue(c.body["title"])
			if !ok || title == "" {
				s.sendError(c.w, 422, "title is required")
				return true
			}
			labels, badLabel := c.validatedLabels(repo)
			if badLabel != "" {
				s.sendError(c.w, 422, "label "+badLabel+" does not exist")
				return true
			}
			bodyStr, _ := asStringValue(c.body["body"])
			issue := s.store.CreateIssue(repo, title, bodyStr, labels)
			s.sendCachedJson(c.w, 201, s.store.IssueJSONBuffer(repo, issue))
			return true
		}
		return false
	}
	if len(rest) >= 2 {
		numberF, okNum := jsNumber(rest[1])
		if !okNum || !jsIsInteger(numberF) || numberF <= 0 {
			return false
		}
		issue := s.store.GetIssue(repo, int(numberF))
		if issue == nil {
			s.sendError(c.w, 404, "Not Found")
			return true
		}
		if len(rest) == 2 {
			switch method {
			case "GET":
				s.sendCachedJson(c.w, 200, s.store.IssueJSONBuffer(repo, issue))
				return true
			case "PATCH":
				if state, ok := asStringValue(c.body["state"]); ok && (state == "open" || state == "closed") {
					issue.State = state
				}
				if title, ok := asStringValue(c.body["title"]); ok {
					issue.Title = title
				}
				if bodyStr, ok := asStringValue(c.body["body"]); ok {
					issue.Body = bodyStr
				}
				issue.UpdatedSeq = s.store.nextSeq()
				issue.jsonCache = nil
				s.sendJson(c.w, 200, s.store.IssueJSON(repo, issue), nil)
				return true
			}
			return false
		}
		if len(rest) >= 3 && rest[2] == "comments" {
			switch method {
			case "GET":
				out := make([]any, 0, len(issue.Comments))
				for _, comment := range issue.Comments {
					out = append(out, s.store.CommentJSON(comment))
				}
				s.sendJson(c.w, 200, out, nil)
				return true
			case "POST":
				commentBody, ok := asStringValue(c.body["body"])
				if !ok {
					s.sendError(c.w, 422, "body is required")
					return true
				}
				comment := s.store.AddComment(repo, issue, commentBody)
				s.sendJson(c.w, 201, s.store.CommentJSON(comment), nil)
				return true
			}
		}
	}
	return false
}

// validatedLabels extracts the label names from a create-issue body and
// checks each one against the repo. Label entries may be plain strings or
// {"name": ...} objects; empty names are skipped. It returns the list and
// the first unknown label name, or "" when every label exists.
func (c *restCtx) validatedLabels(repo *Repo) (labels []string, badLabel string) {
	var rawLabels []any
	if arr, isArr := c.body["labels"].([]any); isArr {
		rawLabels = arr
	}
	for _, raw := range rawLabels {
		var name string
		if str, isStr := raw.(string); isStr {
			name = str
		} else if rec, isRec := raw.(map[string]any); isRec {
			name, _ = asStringValue(rec["name"])
		}
		if name == "" {
			continue
		}
		if _, exists := repo.Labels[name]; !exists {
			return nil, name
		}
		labels = append(labels, name)
	}
	return labels, ""
}

// routePulls answers /pulls: list and create, one pull by number, its
// changed files, and the squash merge.
func (c *restCtx) routePulls(method string, repo *Repo, rest []string) bool {
	if len(rest) < 1 || rest[0] != "pulls" {
		return false
	}
	s := c.srv
	if len(rest) == 1 {
		switch method {
		case "GET":
			state := c.listState()
			items := make([]any, 0, len(repo.Pulls))
			for _, pr := range repo.Pulls {
				if state == "all" || pr.State == state {
					items = append(items, s.store.PullJSON(repo, pr))
				}
			}
			page, link := paginate(items, c.search, c.r, c.pathname)
			s.sendJson(c.w, 200, page, linkHeaders(link))
			return true
		case "POST":
			title, okTitle := asStringValue(c.body["title"])
			head, okHead := asStringValue(c.body["head"])
			base, okBase := asStringValue(c.body["base"])
			if !okBase {
				base = repo.DefaultBranch
			}
			if !okTitle || !okHead || title == "" || head == "" {
				s.sendError(c.w, 422, "title and head are required")
				return true
			}
			if _, ok := s.store.ResolveBranch(repo, head); !ok {
				s.sendError(c.w, 422, "head or base branch does not exist")
				return true
			}
			if _, ok := s.store.ResolveBranch(repo, base); !ok {
				s.sendError(c.w, 422, "head or base branch does not exist")
				return true
			}
			for _, pr := range repo.Pulls {
				if pr.State == "open" && pr.HeadRef == head && pr.BaseRef == base {
					s.sendError(c.w, 422, "A pull request already exists for this head and base")
					return true
				}
			}
			bodyStr, _ := asStringValue(c.body["body"])
			pr := s.store.CreatePull(repo, title, bodyStr, head, base)
			if pr == nil {
				s.sendError(c.w, 422, "head or base branch does not exist")
				return true
			}
			s.sendJson(c.w, 201, s.store.PullJSON(repo, pr), nil)
			return true
		}
		return false
	}
	if len(rest) >= 2 {
		numberF, okNum := jsNumber(rest[1])
		if !okNum || !jsIsInteger(numberF) || numberF <= 0 {
			return false
		}
		pr := s.store.GetPull(repo, int(numberF))
		if pr == nil {
			s.sendError(c.w, 404, "Not Found")
			return true
		}
		if len(rest) == 2 && method == "GET" {
			s.sendJson(c.w, 200, s.store.PullJSON(repo, pr), nil)
			return true
		}
		if len(rest) == 3 && rest[2] == "files" && method == "GET" {
			files := s.store.PullFiles(repo, pr)
			out := make([]any, 0, len(files))
			for _, f := range files {
				out = append(out, map[string]any{
					"filename": f.Path, "status": f.Status,
					"additions": f.Additions, "deletions": f.Deletions,
					"changes": f.Additions + f.Deletions,
				})
			}
			s.sendJson(c.w, 200, out, nil)
			return true
		}
		if len(rest) == 3 && rest[2] == "merge" && method == "PUT" {
			if pr.State != "open" {
				s.sendError(c.w, 405, "Pull request is already closed")
				return true
			}
			merged := s.store.MergePull(repo, pr)
			if merged == nil {
				s.sendError(c.w, 405, "This pull request cannot be merged: conflicts")
				return true
			}
			s.sendJson(c.w, 200, map[string]any{"sha": merged.Sha, "merged": true, "message": merged.Message}, nil)
			return true
		}
	}
	return false
}

// b64encode renders bytes as standard base64, as the contents API does.
func b64encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
