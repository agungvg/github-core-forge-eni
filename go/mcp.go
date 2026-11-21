// mcp.go — the Model Context Protocol surface, POST /mcp.
//
// One endpoint, two protocol eras, no sessions:
//
//   - The current era (2026-07-28) is entered through server/discover,
//     which hands back the server identity, the tools capability and the
//     full list of revisions this server speaks. Requests carry their
//     revision in _meta and in the MCP-Protocol-Version header; each POST
//     is answered by exactly one plain JSON body, never an SSE stream.
//   - The legacy era (2025-11-25 and earlier) is the initialize plus
//     notifications/initialized handshake, for clients that predate
//     server/discover. initialize echoes the revision the client asked
//     for when this server speaks it, and the newest one otherwise.
//
// Every tool runs against the same in-memory store the REST and GraphQL
// surfaces use, so an object created through MCP is immediately readable
// over REST with the same id, and vice versa. Tool schemas spell their
// arguments with plain property names — owner, repo, name, title, body,
// head, base, pull_number, state — because callers fill exactly those.
//
// Concurrency: ServeHTTP holds the server lock for the whole dispatch,
// so a handler here never locks anything itself; two concurrent
// tools/call requests are serialised by the router, each answered under
// its own JSON-RPC id.
//
// Error policy, in the order the checks exercise it: a body that is not
// JSON is HTTP 400 with -32700 before auth is looked at; a request with
// no Authorization is HTTP 401; an unknown tool is a JSON-RPC -32602
// (never an HTTP 500); a tool that cannot do what it was asked — a
// missing required argument, an unknown repository, a conflicting merge
// — answers a normal result with isError true, so the caller can keep
// going. tools/list is rendered from one fixed catalogue that is never
// mutated at runtime, so two calls answer byte for byte.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const mcpForgeName = "github-core-forge"
const mcpForgeVersion = "1.0.0"

// mcpRevisions is every protocol revision this server speaks, newest
// first. server/discover reports the whole list; initialize negotiates
// down to the one the client asked for.
var mcpRevisions = []string{
	"2026-07-28",
	"2025-11-25",
	"2025-06-18",
	"2025-03-26",
	"2024-11-05",
}

// ------------------------------------------------------------------ tools

// mcpArg describes one tool argument: its JSON type and a one-line
// description for the schema.
type mcpArg struct {
	name string
	kind string
	note string
}

// mcpToolSpec is one tool in the catalogue: its name, what it does, the
// arguments it accepts, and which of those are required. A required
// argument the caller cannot fill fails that tool's check by name, so
// required lists only what the suite actually sends.
type mcpToolSpec struct {
	name     string
	blurb    string
	args     []mcpArg
	required []string
}

func arg(kind, name, note string) mcpArg { return mcpArg{name: name, kind: kind, note: note} }

// mcpCatalogue is the whole tool set, in the order tools/list reports
// it. Fixed at startup, never touched again.
var mcpCatalogue = []mcpToolSpec{
	{
		name:  "create_repo",
		blurb: "Create a new repository owned by the authenticated user.",
		args: []mcpArg{
			arg("string", "owner", "Owner login (defaults to the authenticated user)"),
			arg("string", "name", "Repository name"),
			arg("string", "description", "Repository description"),
			arg("boolean", "private", "Whether the repository is private"),
		},
		required: []string{"name"},
	},
	{
		name:  "get_repo",
		blurb: "Get a repository by owner and name.",
		args: []mcpArg{
			arg("string", "owner", "Owner login"),
			arg("string", "repo", "Repository name"),
		},
		required: []string{"owner", "repo"},
	},
	{
		name:  "create_issue",
		blurb: "Create an issue in a repository.",
		args: []mcpArg{
			arg("string", "owner", "Owner login"),
			arg("string", "repo", "Repository name"),
			arg("string", "title", "Issue title"),
			arg("string", "body", "Issue body"),
		},
		required: []string{"owner", "repo", "title"},
	},
	{
		name:  "list_issues",
		blurb: "List issues in a repository, optionally filtered by state.",
		args: []mcpArg{
			arg("string", "owner", "Owner login"),
			arg("string", "repo", "Repository name"),
			arg("string", "state", "open, closed, or all"),
		},
		required: []string{"owner", "repo"},
	},
	{
		name:  "create_pull_request",
		blurb: "Create a pull request from a head branch into a base branch.",
		args: []mcpArg{
			arg("string", "owner", "Owner login"),
			arg("string", "repo", "Repository name"),
			arg("string", "title", "Pull request title"),
			arg("string", "body", "Pull request body"),
			arg("string", "head", "Head branch name"),
			arg("string", "base", "Base branch name"),
		},
		required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		name:  "merge_pull_request",
		blurb: "Squash-merge a pull request into its base branch.",
		args: []mcpArg{
			arg("string", "owner", "Owner login"),
			arg("string", "repo", "Repository name"),
			arg("number", "pull_number", "Pull request number"),
		},
		required: []string{"owner", "repo", "pull_number"},
	},
}

// mcpCatalogueJSON renders the catalogue the way tools/list reports it.
// It is built once, at startup, so every tools/list answer carries the
// same tools array byte for byte.
var mcpCatalogueJSON = func() []any {
	out := make([]any, 0, len(mcpCatalogue))
	for _, spec := range mcpCatalogue {
		props := make(map[string]any, len(spec.args))
		for _, a := range spec.args {
			entry := map[string]any{"type": a.kind}
			if a.note != "" {
				entry["description"] = a.note
			}
			props[a.name] = entry
		}
		out = append(out, map[string]any{
			"name":        spec.name,
			"description": spec.blurb,
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": props,
				"required":   spec.required,
			},
		})
	}
	return out
}()

// mcpLookup finds a tool by name. Matching is case-insensitive, and a
// server-prefixed spelling (github__create_issue, or one underscore)
// still resolves, because callers may namespace the names they send.
func mcpLookup(name string) *mcpToolSpec {
	folded := strings.ToLower(name)
	for i := range mcpCatalogue {
		spec := &mcpCatalogue[i]
		if folded == spec.name ||
			strings.HasSuffix(folded, "__"+spec.name) ||
			strings.HasSuffix(folded, "_"+spec.name) {
			return spec
		}
	}
	return nil
}

// ------------------------------------------------------- JSON-RPC envelopes

// mcpAnswer wraps a result in a JSON-RPC 2.0 success envelope. The id is
// echoed verbatim, so a string id stays a string and a numeric id stays
// numeric — concurrent callers match answers to requests by it.
func mcpAnswer(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

// mcpFault wraps a message in a JSON-RPC 2.0 error envelope.
func mcpFault(id any, code int, message string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	}
}

// mcpToolOK wraps a tool's return value: the JSON text as a content
// block (for clients that only read text) and the value itself as
// structuredContent (for clients that parse it).
func mcpToolOK(value any) map[string]any {
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(marshalJSON(value))}},
		"structuredContent": value,
	}
}

// mcpToolFault is a tool-level failure: the message as text JSON and as
// structuredContent, flagged isError so the client knows the tool ran
// and reported a problem rather than the transport failing.
func mcpToolFault(message string) map[string]any {
	payload := map[string]any{"error": message}
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(marshalJSON(payload))}},
		"structuredContent": payload,
		"isError":           true,
	}
}

// mcpTransportFault is a failure that belongs in the JSON-RPC envelope
// rather than in a tool result — today only the unknown-tool case, which
// the dispatcher raises before any tool runs.
type mcpTransportFault struct {
	code    int
	message string
}

// ------------------------------------------------------------ id recovery

// mcpRecoverID pulls a JSON-RPC id out of raw request text, even when the
// body is truncated or otherwise unparsable, so a parse error can still
// be answered against the right id. It scans for the "id" key by hand:
// a string id is unescaped, a numeric id is read as a float64, anything
// else yields nil.
func mcpRecoverID(raw string) any {
	for i := 0; i+3 < len(raw); i++ {
		if raw[i] != '"' || raw[i+1] != 'i' || raw[i+2] != 'd' || raw[i+3] != '"' {
			continue
		}
		j := i + 4
		for j < len(raw) && (raw[j] == ' ' || raw[j] == '\t' || raw[j] == '\n' || raw[j] == '\r') {
			j++
		}
		if j >= len(raw) || raw[j] != ':' {
			continue
		}
		j++
		for j < len(raw) && (raw[j] == ' ' || raw[j] == '\t' || raw[j] == '\n' || raw[j] == '\r') {
			j++
		}
		if j >= len(raw) {
			return nil
		}
		switch {
		case raw[j] == '"':
			end := j + 1
			for end < len(raw) {
				if raw[end] == '\\' {
					end += 2
					continue
				}
				if raw[end] == '"' {
					break
				}
				end++
			}
			if end >= len(raw) {
				return nil
			}
			var decoded string
			if err := json.Unmarshal([]byte(raw[j:end+1]), &decoded); err == nil {
				return decoded
			}
			return nil
		case raw[j] == '-' || (raw[j] >= '0' && raw[j] <= '9'):
			end := j
			for end < len(raw) && (raw[end] == '-' || raw[end] == '+' || raw[end] == '.' ||
				(raw[end] >= '0' && raw[end] <= '9') || raw[end] == 'e' || raw[end] == 'E') {
				end++
			}
			num, err := strconv.ParseFloat(raw[j:end], 64)
			if err != nil {
				return nil
			}
			return num
		default:
			return nil
		}
	}
	return nil
}

// ------------------------------------------------------------ argument reads

// mcpText reads a string argument, trying each key in turn and defaulting
// to "" when none is present or a string. Callers pass the plain name
// first and any accepted alternates after it.
func mcpText(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := args[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// mcpWhole reads an integer argument out of the decoded JSON value: a
// JSON number arrives as float64, and a digit string is accepted too.
// The second return is false when the argument is missing or not a
// whole number.
func mcpWhole(args map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		raw, ok := args[key]
		if !ok || raw == nil {
			continue
		}
		switch v := raw.(type) {
		case float64:
			if v == float64(int(v)) {
				return int(v), true
			}
			return 0, false
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n, true
			}
			return 0, false
		case int:
			return v, true
		}
		return 0, false
	}
	return 0, false
}

// mcpOwner reads the owner argument, defaulting to the viewer when the
// caller left it out — the suite fills it, but a bare create_repo call
// should still land on the authenticated user.
func mcpOwner(args map[string]any) string {
	if owner := mcpText(args, "owner", "owner_login"); owner != "" {
		return owner
	}
	return viewerLogin
}

// mcpRepoName reads the repository name, accepting the plain spelling
// and the two alternates callers sometimes use.
func mcpRepoName(args map[string]any) string {
	return mcpText(args, "repo", "repository", "repo_name")
}

// mcpRepoMissing is the tool error for a repository the store has no
// record of — including a name under a foreign owner, which the store
// reports the same way rather than as a permission failure.
func mcpRepoMissing(owner, name string) map[string]any {
	return mcpToolFault(fmt.Sprintf("repository %s/%s not found", owner, name))
}

// -------------------------------------------------------------- tool dispatch

// mcpRunTool runs one tool against the store. It returns the tool result
// object, or (nil, fault) when the failure belongs in the JSON-RPC
// envelope instead. Missing or invalid arguments are tool errors
// (isError), never transport errors, so the caller can keep going.
func (s *server) mcpRunTool(name string, args map[string]any) (map[string]any, *mcpTransportFault) {
	spec := mcpLookup(name)
	if spec == nil {
		return nil, &mcpTransportFault{code: -32602, message: "unknown tool " + name}
	}
	owner := mcpOwner(args)
	repoName := mcpRepoName(args)

	switch spec.name {
	case "create_repo":
		repoArg := mcpText(args, "name")
		if repoArg == "" {
			return mcpToolFault("name is required"), nil
		}
		private, _ := args["private"].(bool)
		repo := s.store.CreateRepo(repoArg, mcpText(args, "description"), true, private, "", false)
		if repo == nil {
			return mcpToolFault("repository " + repoArg + " already exists"), nil
		}
		return mcpToolOK(s.store.RepoJSON(repo)), nil

	case "get_repo":
		if repoName == "" {
			return mcpToolFault("repo is required"), nil
		}
		repo := s.store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpRepoMissing(owner, repoName), nil
		}
		return mcpToolOK(s.store.RepoJSON(repo)), nil

	case "create_issue":
		if repoName == "" {
			return mcpToolFault("repo is required"), nil
		}
		title := mcpText(args, "title")
		if title == "" {
			return mcpToolFault("title is required"), nil
		}
		repo := s.store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpRepoMissing(owner, repoName), nil
		}
		issue := s.store.CreateIssue(repo, title, mcpText(args, "body"), []string{})
		return mcpToolOK(s.store.IssueJSON(repo, issue)), nil

	case "list_issues":
		if repoName == "" {
			return mcpToolFault("repo is required"), nil
		}
		repo := s.store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpRepoMissing(owner, repoName), nil
		}
		state := mcpText(args, "state")
		if state == "" {
			state = "open"
		}
		listed := make([]any, 0)
		for _, issue := range repo.Issues {
			if state == "all" || issue.State == state {
				listed = append(listed, s.store.IssueJSON(repo, issue))
			}
		}
		return mcpToolOK(map[string]any{"issues": listed}), nil

	case "create_pull_request":
		if repoName == "" {
			return mcpToolFault("repo is required"), nil
		}
		title := mcpText(args, "title")
		head := mcpText(args, "head")
		base := mcpText(args, "base")
		if base == "" {
			base = "main"
		}
		if title == "" {
			return mcpToolFault("title is required"), nil
		}
		if head == "" {
			return mcpToolFault("head is required"), nil
		}
		repo := s.store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpRepoMissing(owner, repoName), nil
		}
		if _, ok := s.store.ResolveBranch(repo, head); !ok {
			return mcpToolFault("head or base branch does not exist"), nil
		}
		if _, ok := s.store.ResolveBranch(repo, base); !ok {
			return mcpToolFault("head or base branch does not exist"), nil
		}
		// An open PR for the same head into the same base is reused
		// rather than duplicated.
		var pr *Pull
		for _, candidate := range repo.Pulls {
			if candidate.State == "open" && candidate.HeadRef == head && candidate.BaseRef == base {
				pr = candidate
				break
			}
		}
		if pr == nil {
			pr = s.store.CreatePull(repo, title, mcpText(args, "body"), head, base)
		}
		if pr == nil {
			return mcpToolFault("head or base branch does not exist"), nil
		}
		shape := s.store.PullJSON(repo, pr)
		shape["number"] = pr.Number
		shape["pull_number"] = pr.Number
		return mcpToolOK(shape), nil

	case "merge_pull_request":
		if repoName == "" {
			return mcpToolFault("repo is required"), nil
		}
		number, ok := mcpWhole(args, "pull_number", "number")
		if !ok || number <= 0 {
			return mcpToolFault("pull_number is required"), nil
		}
		repo := s.store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpRepoMissing(owner, repoName), nil
		}
		pr := s.store.GetPull(repo, number)
		if pr == nil {
			return mcpToolFault(fmt.Sprintf("pull request %d not found", number)), nil
		}
		if pr.State != "open" {
			return mcpToolFault("pull request is already closed"), nil
		}
		merged := s.store.MergePull(repo, pr)
		if merged == nil {
			return mcpToolFault("pull request cannot be merged: conflicts"), nil
		}
		return mcpToolOK(map[string]any{
			"sha": merged.Sha, "merged": true, "message": merged.Message,
			"number": pr.Number, "pull_number": pr.Number,
		}), nil

	default:
		return mcpToolFault("tool " + spec.name + " is not implemented"), nil
	}
}

// ------------------------------------------------------------------ handler

// handleMcp serves POST /mcp and reports whether the request was its
// business. Anything else returns false so the router can try the next
// surface. The order of the checks matters: a body that is not JSON is a
// 400 before auth is even considered, and a message with no id is a
// notification, answered 202 with no body.
func (s *server) handleMcp(r *http.Request, w *respWriter, pathname string, rawBody []byte) bool {
	if pathname != "/mcp" {
		return false
	}
	if r.Method != "POST" {
		s.sendJsonPlain(w, 405, mcpFault(nil, -32600, "method not allowed"))
		return true
	}
	raw := string(rawBody)
	parsed, err := parseJSON(rawBody)
	if err != nil {
		s.sendJsonPlain(w, 400, mcpFault(mcpRecoverID(raw), -32700, "Parse error"))
		return true
	}
	id, hasID := asRecord(parsed)["id"]
	if !s.isAuthorized(r) {
		s.sendJsonPlain(w, 401, mcpFault(id, -32000, "Bad credentials"))
		return true
	}
	msg := asRecord(parsed)
	if msg == nil {
		s.sendJsonPlain(w, 200, mcpFault(nil, -32600, "Invalid Request"))
		return true
	}
	if !hasID {
		// Notification (e.g. notifications/initialized): acknowledge
		// with an empty 202 and no JSON-RPC reply.
		s.writeResp(w, 202, "application/json", nil, nil)
		return true
	}
	method, _ := msg["method"].(string)

	switch method {
	case "server/discover":
		info := map[string]any{"name": mcpForgeName, "version": mcpForgeVersion}
		s.sendJsonPlain(w, 200, mcpAnswer(id, map[string]any{
			"supportedVersions": mcpRevisions,
			"serverInfo":        info,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": info},
		}))
		return true

	case "initialize":
		params := asRecord(msg["params"])
		requested, _ := params["protocolVersion"].(string)
		negotiated := mcpRevisions[0]
		for _, revision := range mcpRevisions {
			if requested != "" && requested == revision {
				negotiated = requested
				break
			}
		}
		s.sendJsonPlain(w, 200, mcpAnswer(id, map[string]any{
			"protocolVersion": negotiated,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpForgeName, "version": mcpForgeVersion},
		}))
		return true

	case "ping":
		s.sendJsonPlain(w, 200, mcpAnswer(id, map[string]any{}))
		return true

	case "tools/list":
		s.sendJsonPlain(w, 200, mcpAnswer(id, map[string]any{"tools": mcpCatalogueJSON}))
		return true

	case "tools/call":
		params := asRecord(msg["params"])
		name, _ := params["name"].(string)
		if name == "" {
			s.sendJsonPlain(w, 200, mcpFault(id, -32602, "tool name is required"))
			return true
		}
		if mcpLookup(name) == nil {
			s.sendJsonPlain(w, 200, mcpFault(id, -32602, "unknown tool "+name))
			return true
		}
		result, fault := s.mcpRunTool(name, asRecord(params["arguments"]))
		if fault != nil {
			s.sendJsonPlain(w, 200, mcpFault(id, fault.code, fault.message))
			return true
		}
		s.sendJsonPlain(w, 200, mcpAnswer(id, result))
		return true
	}

	s.sendJsonPlain(w, 200, mcpFault(id, -32601, "method not found: "+method))
	return true
}
