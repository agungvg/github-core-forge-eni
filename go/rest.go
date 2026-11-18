// rest.go — REST v3 under /api/v3. Ported from rest.ts.
// Every reply is application/json with a non-empty X-GitHub-Request-Id.
// Auth is a bearer token (MAKEFUN_SHOP_TOKEN); missing/wrong -> 401.
// A repo belongs to the token's login; another owner -> 404, never 403.

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

// sendCachedJson writes a pre-serialized body (the memoized issue JSON).
func (s *server) sendCachedJson(w *respWriter, status int, body []byte) {
	n := atomic.AddInt64(&s.requestCounter, 1)
	w.header("content-type", jsonType)
	w.header("x-github-request-id", "req-"+strconv.FormatInt(n, 16))
	w.header("content-length", strconv.Itoa(len(body)))
	w.writeHeader(status)
	w.write(body)
}

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

func (s *server) sendError(w *respWriter, status int, message string) {
	s.sendJson(w, status, map[string]any{"message": message}, nil)
}

func (s *server) isAuthorized(r *http.Request) bool {
	if s.token == "" {
		return false
	}
	return r.Header.Get("Authorization") == s.expectedAuth
}

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

// Paginate a list; returns the page plus a `Link: rel="next"` value when needed.
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

func linkHeaders(link string) map[string]string {
	if link == "" {
		return nil
	}
	return map[string]string{"link": link}
}

var issueFastPathRe = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/([1-9][0-9]*)$`)

// Handle a REST request. Returns true when the path is under /api/v3
// (handled, even on 404); false otherwise.
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

	// Fast path: GET /repos/{owner}/{repo}/issues/{n} — the read workload's
	// 2,000 sequential requests. Only when every segment is plain ASCII (no
	// '%' to decode, no trailing slash, no query): the slow path below handles
	// everything else byte-identically.
	if method == "GET" && !strings.Contains(path, "%") {
		if m := issueFastPathRe.FindStringSubmatch(path); m != nil {
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

	// Body parsing for methods that carry one.
	var bodyValue any = map[string]any{}
	if method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE" {
		parsed, ok := parseBody(rawBody)
		if !ok {
			s.sendError(w, 400, "Problems parsing JSON")
			return true
		}
		bodyValue = parsed
	}
	body := asRecord(bodyValue)

	var segs []string
	for _, seg := range strings.Split(path, "/") {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	// seg decodes one segment; a malformed escape throws in TS (caught as 500).
	seg := func(i int) string {
		if i >= len(segs) {
			return ""
		}
		dec, err := jsDecodeURIComponent(segs[i])
		if err != nil {
			panic(err)
		}
		return dec
	}

	// The route body: any panic (e.g. a bad escape) answers 500, as the
	// try/catch in rest.ts does.
	defer func() {
		if rec := recover(); rec != nil {
			if !w.wrote {
				s.sendError(w, 500, "Internal Server Error")
			}
		}
	}()

	// ---- API root + meta + viewer -------------------------------------
	if method == "GET" && path == "/" {
		s.sendJson(w, 200, map[string]any{"current_user_url": "/api/v3/user", "rate_limit_url": "/api/v3/rate_limit"}, map[string]string{"x-oauth-scopes": "repo, read:org"})
		return true
	}
	if method == "GET" && path == "/meta" {
		s.sendJson(w, 200, map[string]any{"installed_version": "1.0.0", "verifiable_password_authentication": false}, nil)
		return true
	}
	if method == "GET" && path == "/user" {
		s.sendJson(w, 200, map[string]any{"login": viewerLogin, "id": viewerID, "name": viewerName, "email": viewerEmail, "type": "User"}, nil)
		return true
	}

	// ---- /user/repos ----------------------------------------------------
	if len(segs) >= 2 && segs[0] == "user" && segs[1] == "repos" && len(segs) == 2 {
		if method == "GET" {
			repos := s.store.ListViewerRepos()
			items := make([]any, 0, len(repos))
			for _, r := range repos {
				items = append(items, s.store.RepoJSON(r))
			}
			page, link := paginate(items, search, r, pathname)
			s.sendJson(w, 200, page, linkHeaders(link))
			return true
		}
		if method == "POST" {
			name, ok := asStringValue(body["name"])
			if !ok || name == "" {
				s.sendError(w, 422, "name is required")
				return true
			}
			desc, _ := asStringValue(body["description"])
			priv, _ := body["private"].(bool)
			repo := s.store.CreateRepo(name, desc, true, priv, "", false)
			if repo == nil {
				s.sendError(w, 422, "Repository already exists")
				return true
			}
			s.sendJson(w, 201, s.store.RepoJSON(repo), nil)
			return true
		}
	}

	// ---- /repos/{owner}/{repo}/... --------------------------------------
	if len(segs) >= 1 && segs[0] == "repos" && len(segs) >= 3 {
		owner := seg(1)
		repoName := seg(2)
		repo := s.store.GetRepo(owner, repoName)
		if repo == nil {
			s.sendError(w, 404, "Not Found")
			return true
		}
		rest := segs[3:]

		// GET/PATCH/DELETE the repo itself.
		if len(rest) == 0 {
			if method == "GET" {
				s.sendJson(w, 200, s.store.RepoJSON(repo), nil)
				return true
			}
			if method == "PATCH" {
				if description, ok := asStringValue(body["description"]); ok {
					repo.Description = description
				}
				if priv, ok := body["private"].(bool); ok {
					repo.Private = priv
				}
				repo.UpdatedSeq = s.store.nextSeq()
				s.sendJson(w, 200, s.store.RepoJSON(repo), nil)
				return true
			}
			if method == "DELETE" {
				s.store.DeleteRepo(owner, repoName)
				s.sendJson(w, 204, nil, nil)
				return true
			}
		}

		// Branches.
		if len(rest) >= 1 && rest[0] == "branches" {
			if method == "GET" && len(rest) == 1 {
				var names []string
				for ref := range repo.Refs {
					if strings.HasPrefix(ref, "refs/heads/") {
						names = append(names, ref[len("refs/heads/"):])
					}
				}
				sort.SliceStable(names, func(i, j int) bool {
					a, b := names[i], names[j]
					if a == repo.DefaultBranch {
						return true
					}
					if b == repo.DefaultBranch {
						return false
					}
					return a < b
				})
				out := make([]any, 0, len(names))
				for _, name := range names {
					out = append(out, map[string]any{"name": name, "commit": map[string]any{"sha": repo.Refs["refs/heads/"+name]}})
				}
				s.sendJson(w, 200, out, nil)
				return true
			}
			if method == "GET" && len(rest) == 2 {
				branch := seg(4) // rest[1] is segs[4]
				sha, ok := repo.Refs["refs/heads/"+branch]
				if !ok {
					s.sendError(w, 404, "Branch not found")
					return true
				}
				s.sendJson(w, 200, map[string]any{"name": branch, "commit": map[string]any{"sha": sha}}, nil)
				return true
			}
		}

		// Git refs.
		if len(rest) >= 2 && rest[0] == "git" && rest[1] == "refs" {
			if method == "GET" && len(rest) >= 4 && rest[2] == "heads" {
				var parts []string
				for _, p := range rest[3:] {
					dec, err := jsDecodeURIComponent(p)
					if err != nil {
						panic(err)
					}
					parts = append(parts, dec)
				}
				branch := strings.Join(parts, "/")
				sha, ok := repo.Refs["refs/heads/"+branch]
				if !ok {
					s.sendError(w, 404, "Not Found")
					return true
				}
				s.sendJson(w, 200, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"sha": sha, "type": "commit"}}, nil)
				return true
			}
			if method == "POST" && len(rest) == 2 {
				ref, okRef := asStringValue(body["ref"])
				sha, okSha := asStringValue(body["sha"])
				if !okRef || !okSha || ref == "" || sha == "" {
					s.sendError(w, 422, "ref and sha are required")
					return true
				}
				obj := repo.Store.Get(sha)
				if obj == nil || obj.Type != ObjCommit {
					s.sendError(w, 422, "sha is not a known commit")
					return true
				}
				repo.Refs[ref] = sha
				s.sendJson(w, 201, map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"}}, nil)
				return true
			}
		}

		// Commits.
		if len(rest) >= 1 && rest[0] == "commits" && method == "GET" && len(rest) == 1 {
			branch, ok := search.Get("sha")
			if !ok {
				branch = repo.DefaultBranch
			}
			commits := s.store.ListCommits(repo, branch)
			out := make([]any, 0, len(commits))
			for _, c := range commits {
				out = append(out, map[string]any{
					"sha": c.Sha,
					"commit": map[string]any{
						"message":   c.Message,
						"author":    map[string]any{"name": viewerName, "email": viewerEmail},
						"committer": map[string]any{"name": viewerName, "email": viewerEmail},
					},
				})
			}
			s.sendJson(w, 200, out, nil)
			return true
		}

		// Contents.
		if len(rest) >= 1 && rest[0] == "contents" {
			var parts []string
			for _, p := range rest[1:] {
				dec, err := jsDecodeURIComponent(p)
				if err != nil {
					panic(err)
				}
				parts = append(parts, dec)
			}
			contentPath := strings.Join(parts, "/")
			ref, ok := search.Get("ref")
			if !ok {
				ref = repo.DefaultBranch
			}
			if method == "GET" {
				if contentPath != "" {
					if oid, data, found := s.store.ReadFileAt(repo, ref, contentPath); found {
						name := contentPath
						if idx := strings.LastIndex(contentPath, "/"); idx >= 0 {
							name = contentPath[idx+1:]
						}
						s.sendJson(w, 200, map[string]any{
							"type": "file", "encoding": "base64",
							"content": b64encode(data), "sha": oid, "size": len(data),
							"name": name, "path": contentPath,
						}, nil)
						return true
					}
				}
				if dir := s.store.ListDirAt(repo, ref, contentPath); dir != nil {
					out := make([]any, 0, len(dir))
					for _, e := range dir {
						out = append(out, map[string]any{"type": e.Type, "name": e.Name, "path": e.Path, "sha": e.OID})
					}
					s.sendJson(w, 200, out, nil)
					return true
				}
				s.sendError(w, 404, "Not Found")
				return true
			}
			if method == "PUT" {
				message, _ := asStringValue(body["message"])
				contentB64, okB64 := asStringValue(body["content"])
				if !okB64 {
					s.sendError(w, 422, "content is required")
					return true
				}
				content := lenientBase64Decode(contentB64)
				branch, okBranch := asStringValue(body["branch"])
				if !okBranch {
					branch = repo.DefaultBranch
				}
				existingOid, _, existing := s.store.ReadFileAt(repo, branch, contentPath)
				sha, hasSha := asStringValue(body["sha"])
				if existing {
					if !hasSha || sha != existingOid {
						s.sendError(w, 409, "sha does not match the current blob")
						return true
					}
				}
				result := s.store.CommitFile(repo, branch, contentPath, content, message)
				if result == nil {
					s.sendError(w, 404, "Branch not found")
					return true
				}
				name := contentPath
				if idx := strings.LastIndex(contentPath, "/"); idx >= 0 {
					name = contentPath[idx+1:]
				}
				status := 201
				if existing {
					status = 200
				}
				s.sendJson(w, status, map[string]any{
					"content": map[string]any{"name": name, "path": contentPath, "sha": result.BlobSha, "encoding": "base64"},
					"commit":  map[string]any{"sha": result.CommitSha},
				}, nil)
				return true
			}
			if method == "DELETE" {
				oid, _, existing := s.store.ReadFileAt(repo, repo.DefaultBranch, contentPath)
				if !existing {
					s.sendError(w, 404, "Not Found")
					return true
				}
				sha, hasSha := asStringValue(body["sha"])
				if hasSha && sha != oid {
					s.sendError(w, 409, "sha does not match the current blob")
					return true
				}
				message, _ := asStringValue(body["message"])
				result := s.store.CommitFile(repo, repo.DefaultBranch, contentPath, nil, message)
				if result == nil {
					s.sendError(w, 404, "Not Found")
					return true
				}
				s.sendJson(w, 200, map[string]any{"commit": map[string]any{"sha": result.CommitSha}}, nil)
				return true
			}
		}

		// Labels.
		if len(rest) >= 1 && rest[0] == "labels" {
			if method == "GET" && len(rest) == 1 {
				out := make([]any, 0, len(repo.LabelOrder))
				for _, name := range repo.LabelOrder {
					out = append(out, s.store.LabelJSON(repo.Labels[name]))
				}
				s.sendJson(w, 200, out, nil)
				return true
			}
			if method == "POST" && len(rest) == 1 {
				name, ok := asStringValue(body["name"])
				if !ok || name == "" {
					s.sendError(w, 422, "name is required")
					return true
				}
				color, okColor := asStringValue(body["color"])
				if !okColor {
					color = "ededed"
				}
				label := s.store.CreateLabel(repo, name, color)
				s.sendJson(w, 201, s.store.LabelJSON(label), nil)
				return true
			}
		}

		// Issues.
		if len(rest) >= 1 && rest[0] == "issues" {
			if len(rest) == 1 {
				if method == "GET" {
					state, _ := search.Get("state")
					if state == "" {
						if _, present := search.Get("state"); !present {
							state = "open"
						}
					}
					direction, _ := search.Get("direction")
					if direction == "" {
						if _, present := search.Get("direction"); !present {
							direction = "desc"
						}
					}
					var issues []*Issue
					for _, i := range repo.Issues {
						if state == "all" || i.State == state {
							issues = append(issues, i)
						}
					}
					sort.SliceStable(issues, func(a, b int) bool {
						if direction == "asc" {
							return issues[a].CreatedSeq < issues[b].CreatedSeq
						}
						return issues[a].CreatedSeq > issues[b].CreatedSeq
					})
					items := make([]any, 0, len(issues))
					for _, i := range issues {
						items = append(items, s.store.IssueJSON(repo, i))
					}
					page, link := paginate(items, search, r, pathname)
					s.sendJson(w, 200, page, linkHeaders(link))
					return true
				}
				if method == "POST" {
					title, ok := asStringValue(body["title"])
					if !ok || title == "" {
						s.sendError(w, 422, "title is required")
						return true
					}
					var rawLabels []any
					if arr, isArr := body["labels"].([]any); isArr {
						rawLabels = arr
					}
					var labels []string
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
							s.sendError(w, 422, "label "+name+" does not exist")
							return true
						}
						labels = append(labels, name)
					}
					bodyStr, _ := asStringValue(body["body"])
					issue := s.store.CreateIssue(repo, title, bodyStr, labels)
					s.sendCachedJson(w, 201, s.store.IssueJSONBuffer(repo, issue))
					return true
				}
			}
			if len(rest) >= 2 {
				numberF, okNum := jsNumber(rest[1])
				if okNum && jsIsInteger(numberF) && numberF > 0 {
					number := int(numberF)
					issue := s.store.GetIssue(repo, number)
					if issue == nil {
						s.sendError(w, 404, "Not Found")
						return true
					}
					if len(rest) == 2 {
						if method == "GET" {
							s.sendCachedJson(w, 200, s.store.IssueJSONBuffer(repo, issue))
							return true
						}
						if method == "PATCH" {
							if state, ok := asStringValue(body["state"]); ok && (state == "open" || state == "closed") {
								issue.State = state
							}
							if title, ok := asStringValue(body["title"]); ok {
								issue.Title = title
							}
							if b, ok := asStringValue(body["body"]); ok {
								issue.Body = b
							}
							issue.UpdatedSeq = s.store.nextSeq()
							issue.jsonCache = nil
							s.sendJson(w, 200, s.store.IssueJSON(repo, issue), nil)
							return true
						}
					}
					if len(rest) >= 3 && rest[2] == "comments" {
						if method == "GET" {
							out := make([]any, 0, len(issue.Comments))
							for _, c := range issue.Comments {
								out = append(out, s.store.CommentJSON(c))
							}
							s.sendJson(w, 200, out, nil)
							return true
						}
						if method == "POST" {
							commentBody, ok := asStringValue(body["body"])
							if !ok {
								s.sendError(w, 422, "body is required")
								return true
							}
							comment := s.store.AddComment(repo, issue, commentBody)
							s.sendJson(w, 201, s.store.CommentJSON(comment), nil)
							return true
						}
					}
				}
			}
		}

		// Pull requests.
		if len(rest) >= 1 && rest[0] == "pulls" {
			if len(rest) == 1 {
				if method == "GET" {
					state, _ := search.Get("state")
					if state == "" {
						if _, present := search.Get("state"); !present {
							state = "open"
						}
					}
					var pulls []*Pull
					for _, p := range repo.Pulls {
						if state == "all" || p.State == state {
							pulls = append(pulls, p)
						}
					}
					items := make([]any, 0, len(pulls))
					for _, p := range pulls {
						items = append(items, s.store.PullJSON(repo, p))
					}
					page, link := paginate(items, search, r, pathname)
					s.sendJson(w, 200, page, linkHeaders(link))
					return true
				}
				if method == "POST" {
					title, okTitle := asStringValue(body["title"])
					head, okHead := asStringValue(body["head"])
					base, okBase := asStringValue(body["base"])
					if !okBase {
						base = repo.DefaultBranch
					}
					if !okTitle || !okHead || title == "" || head == "" {
						s.sendError(w, 422, "title and head are required")
						return true
					}
					if _, ok := s.store.ResolveBranch(repo, head); !ok {
						s.sendError(w, 422, "head or base branch does not exist")
						return true
					}
					if _, ok := s.store.ResolveBranch(repo, base); !ok {
						s.sendError(w, 422, "head or base branch does not exist")
						return true
					}
					for _, p := range repo.Pulls {
						if p.State == "open" && p.HeadRef == head && p.BaseRef == base {
							s.sendError(w, 422, "A pull request already exists for this head and base")
							return true
						}
					}
					bodyStr, _ := asStringValue(body["body"])
					pr := s.store.CreatePull(repo, title, bodyStr, head, base)
					if pr == nil {
						s.sendError(w, 422, "head or base branch does not exist")
						return true
					}
					s.sendJson(w, 201, s.store.PullJSON(repo, pr), nil)
					return true
				}
			}
			if len(rest) >= 2 {
				numberF, okNum := jsNumber(rest[1])
				if okNum && jsIsInteger(numberF) && numberF > 0 {
					number := int(numberF)
					pr := s.store.GetPull(repo, number)
					if pr == nil {
						s.sendError(w, 404, "Not Found")
						return true
					}
					if len(rest) == 2 && method == "GET" {
						s.sendJson(w, 200, s.store.PullJSON(repo, pr), nil)
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
						s.sendJson(w, 200, out, nil)
						return true
					}
					if len(rest) == 3 && rest[2] == "merge" && method == "PUT" {
						if pr.State != "open" {
							s.sendError(w, 405, "Pull request is already closed")
							return true
						}
						merged := s.store.MergePull(repo, pr)
						if merged == nil {
							s.sendError(w, 405, "This pull request cannot be merged: conflicts")
							return true
						}
						s.sendJson(w, 200, map[string]any{"sha": merged.Sha, "merged": true, "message": merged.Message}, nil)
						return true
					}
				}
			}
		}
	}

	// Unknown route under /api/v3.
	s.sendError(w, 404, "Not Found")
	return true
}

func b64encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
