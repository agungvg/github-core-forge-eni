// mcp.go — the MCP surface at POST /mcp.
//
// JSON-RPC 2.0 over Streamable HTTP, answered as plain JSON (no SSE
// stream is needed: every request here gets exactly one response). Two
// protocol eras are served from the same handler:
//
//   - the current era, entered through server/discover, which reports the
//     supported revisions (newest first, "2026-07-28" at the head);
//   - the legacy handshake, initialize plus notifications/initialized,
//     for clients that still speak 2025-11-25 or earlier.
//
// Tools run against the same in-memory store as REST, so an object made
// through MCP is immediately visible to REST (and vice versa) with the
// same ids. Tool schemas use plain property names — owner, repo, name,
// title, body, head, base, pull_number, state — because the calling
// clients fill exactly those names.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const mcpServerName = "github-core-forge"
const mcpServerVersion = "1.0.0"

// Revisions this server speaks, newest first. initialize answers the
// client's requested revision when it is one of these, else the newest.
var mcpRevisions = []string{"2026-07-28", "2025-11-25"}

// mcpToolDef is one tool: its name, a human description, and the JSON
// Schema of the arguments it accepts.
type mcpToolDef struct {
	Name        string
	Description string
	InputSchema mcpInputSchema
}

// mcpInputSchema is the inputSchema object: always type "object", with
// named properties and the subset of them that are required.
type mcpInputSchema struct {
	Type       string
	Properties map[string]mcpProp
	Required   []string
}

// mcpProp is one argument property: its JSON type and a short description.
type mcpProp struct {
	Type        string
	Description string
}

// prop is a shorthand for building a property inline.
func prop(typ, desc string) mcpProp { return mcpProp{Type: typ, Description: desc} }

// mcpTools is the whole catalogue, in the order tools/list reports it.
// That order is fixed: two tools/list calls must answer byte for byte,
// and the catalogue is never mutated at runtime.
var mcpTools = []mcpToolDef{
	{
		Name:        "create_repo",
		Description: "Create a new repository owned by the authenticated user.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpProp{
				"owner":       prop("string", "Owner login (defaults to the authenticated user)"),
				"name":        prop("string", "Repository name"),
				"description": prop("string", "Repository description"),
				"private":     prop("boolean", "Whether the repository is private"),
			},
			Required: []string{"name"},
		},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpProp{
				"owner": prop("string", "Owner login"),
				"repo":  prop("string", "Repository name"),
			},
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpProp{
				"owner": prop("string", "Owner login"),
				"repo":  prop("string", "Repository name"),
				"title": prop("string", "Issue title"),
				"body":  prop("string", "Issue body"),
			},
			Required: []string{"owner", "repo", "title"},
		},
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository, optionally filtered by state.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpProp{
				"owner": prop("string", "Owner login"),
				"repo":  prop("string", "Repository name"),
				"state": prop("string", "open, closed, or all"),
			},
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request from a head branch into a base branch.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpProp{
				"owner": prop("string", "Owner login"),
				"repo":  prop("string", "Repository name"),
				"title": prop("string", "Pull request title"),
				"body":  prop("string", "Pull request body"),
				"head":  prop("string", "Head branch name"),
				"base":  prop("string", "Base branch name"),
			},
			Required: []string{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		Name:        "merge_pull_request",
		Description: "Squash-merge a pull request into its base branch.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: map[string]mcpProp{
				"owner":       prop("string", "Owner login"),
				"repo":        prop("string", "Repository name"),
				"pull_number": prop("number", "Pull request number"),
			},
			Required: []string{"owner", "repo", "pull_number"},
		},
	},
}

// mcpToolJSON renders one tool the way tools/list reports it.
func mcpToolJSON(t mcpToolDef) map[string]any {
	props := make(map[string]any, len(t.InputSchema.Properties))
	for name, p := range t.InputSchema.Properties {
		rendered := map[string]any{"type": p.Type}
		if p.Description != "" {
			rendered["description"] = p.Description
		}
		props[name] = rendered
	}
	return map[string]any{
		"name":        t.Name,
		"description": t.Description,
		"inputSchema": map[string]any{
			"type":       t.InputSchema.Type,
			"properties": props,
			"required":   t.InputSchema.Required,
		},
	}
}

// mcpSendJson answers an MCP request with a plain JSON body.
func (s *server) mcpSendJson(w *respWriter, status int, body any) {
	s.sendJsonPlain(w, status, body)
}

// mcpRpcResult wraps a result in a JSON-RPC 2.0 success envelope.
func mcpRpcResult(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

// mcpRpcError wraps a message in a JSON-RPC 2.0 error envelope.
func mcpRpcError(id any, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

// mcpToolResult wraps a tool's return value: the JSON text as a content
// block (for clients that only read text) and the value itself as
// structuredContent (for clients that parse it).
func mcpToolResult(obj any) map[string]any {
	text := string(marshalJSON(obj))
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": text}},
		"structuredContent": obj,
	}
}

// mcpToolError is a tool-level failure: the message as text JSON and as
// structuredContent, flagged isError so the client knows the tool ran
// and reported a problem rather than the transport failing.
func mcpToolError(message string) map[string]any {
	text := string(marshalJSON(map[string]any{"error": message}))
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": text}},
		"structuredContent": map[string]any{"error": message},
		"isError":           true,
	}
}

// sniffIdRe finds a JSON-RPC id inside raw request text, even when the
// body is truncated or otherwise unparsable, so a parse error can still
// be answered against the right id.
var sniffIdRe = regexp.MustCompile(`"id"\s*:\s*("(?:[^"\\]|\\.)*"|-?[0-9]+)`)

// sniffId pulls the id out of raw text, or nil when there is none.
func sniffId(raw string) any {
	match := sniffIdRe.FindStringSubmatch(raw)
	if match == nil {
		return nil
	}
	token := match[1]
	if strings.HasPrefix(token, "\"") {
		var decoded string
		if err := json.Unmarshal([]byte(token), &decoded); err == nil {
			return decoded
		}
		return nil
	}
	num, _ := strconv.ParseFloat(token, 64)
	return num
}

// findMcpTool looks a tool up by name, case-insensitively. Clients may
// send a server-prefixed name (github__create_issue) or a single
// underscore prefix; both match on the suffix.
func findMcpTool(name string) *mcpToolDef {
	lowered := strings.ToLower(name)
	for i := range mcpTools {
		tool := &mcpTools[i]
		if lowered == tool.Name || strings.HasSuffix(lowered, "__"+tool.Name) || strings.HasSuffix(lowered, "_"+tool.Name) {
			return tool
		}
	}
	return nil
}

// mcpRpcErr is a failure that must surface as a JSON-RPC error envelope
// rather than a tool result — today only the unknown-tool case.
type mcpRpcErr struct {
	code    int
	message string
}

// argString reads a string argument, defaulting to "" when absent or of
// the wrong type.
func argString(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// resolveRepoName reads the repository name out of the arguments,
// accepting the plain "repo" spelling and two common alternates.
func resolveRepoName(args map[string]any) string {
	for _, key := range []string{"repo", "repository", "repo_name"} {
		if name := argString(args, key); name != "" {
			return name
		}
	}
	return ""
}

// resolveOwner reads the owner argument, defaulting to the viewer.
func resolveOwner(args map[string]any) string {
	if owner := argString(args, "owner"); owner != "" {
		return owner
	}
	return viewerLogin
}

// notFoundError builds the "repository … not found" tool error.
func notFoundError(owner, repoName string) map[string]any {
	return mcpToolError(fmt.Sprintf("repository %s/%s not found", owner, repoName))
}

// mcpCallTool runs one tool against the store. It returns a tool result
// object, or (nil, rpcErr) when the failure belongs in the JSON-RPC
// envelope instead. Missing or invalid arguments are tool errors
// (isError), never transport errors, so the caller can keep going.
func (s *server) mcpCallTool(name string, args map[string]any) (map[string]any, *mcpRpcErr) {
	tool := findMcpTool(name)
	if tool == nil {
		return nil, &mcpRpcErr{code: -32602, message: "unknown tool " + name}
	}
	owner := resolveOwner(args)
	repoName := resolveRepoName(args)

	switch tool.Name {
	case "create_repo":
		repoArg := argString(args, "name")
		if repoArg == "" {
			return mcpToolError("name is required"), nil
		}
		desc := argString(args, "description")
		private, _ := args["private"].(bool)
		repo := s.store.CreateRepo(repoArg, desc, true, private, "", false)
		if repo == nil {
			return mcpToolError("repository " + repoArg + " already exists"), nil
		}
		return mcpToolResult(s.store.RepoJSON(repo)), nil

	case "get_repo":
		if repoName == "" {
			return mcpToolError("repo is required"), nil
		}
		repo := s.store.GetRepo(owner, repoName)
		if repo == nil {
			return notFoundError(owner, repoName), nil
		}
		return mcpToolResult(s.store.RepoJSON(repo)), nil

	case "create_issue":
		if repoName == "" {
			return mcpToolError("repo is required"), nil
		}
		title := argString(args, "title")
		if title == "" {
			return mcpToolError("title is required"), nil
		}
		repo := s.store.GetRepo(owner, repoName)
		if repo == nil {
			return notFoundError(owner, repoName), nil
		}
		issue := s.store.CreateIssue(repo, title, argString(args, "body"), []string{})
		return mcpToolResult(s.store.IssueJSON(repo, issue)), nil

	case "list_issues":
		if repoName == "" {
			return mcpToolError("repo is required"), nil
		}
		repo := s.store.GetRepo(owner, repoName)
		if repo == nil {
			return notFoundError(owner, repoName), nil
		}
		state := argString(args, "state")
		if state == "" {
			state = "open"
		}
		issues := make([]any, 0)
		for _, issue := range repo.Issues {
			if state == "all" || issue.State == state {
				issues = append(issues, s.store.IssueJSON(repo, issue))
			}
		}
		return mcpToolResult(map[string]any{"issues": issues}), nil

	case "create_pull_request":
		if repoName == "" {
			return mcpToolError("repo is required"), nil
		}
		title := argString(args, "title")
		head := argString(args, "head")
		base := argString(args, "base")
		if base == "" {
			base = "main"
		}
		if title == "" {
			return mcpToolError("title is required"), nil
		}
		if head == "" {
			return mcpToolError("head is required"), nil
		}
		repo := s.store.GetRepo(owner, repoName)
		if repo == nil {
			return notFoundError(owner, repoName), nil
		}
		if _, ok := s.store.ResolveBranch(repo, head); !ok {
			return mcpToolError("head or base branch does not exist"), nil
		}
		if _, ok := s.store.ResolveBranch(repo, base); !ok {
			return mcpToolError("head or base branch does not exist"), nil
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
			pr = s.store.CreatePull(repo, title, argString(args, "body"), head, base)
		}
		if pr == nil {
			return mcpToolError("head or base branch does not exist"), nil
		}
		obj := s.store.PullJSON(repo, pr)
		obj["number"] = pr.Number
		obj["pull_number"] = pr.Number
		return mcpToolResult(obj), nil

	case "merge_pull_request":
		if repoName == "" {
			return mcpToolError("repo is required"), nil
		}
		var num float64
		if f, ok := args["pull_number"].(float64); ok {
			num = f
		} else {
			raw := args["pull_number"]
			if raw == nil {
				raw = args["number"]
			}
			num = jsNumberValue(raw)
		}
		if !jsIsInteger(num) || num <= 0 {
			return mcpToolError("pull_number is required"), nil
		}
		repo := s.store.GetRepo(owner, repoName)
		if repo == nil {
			return notFoundError(owner, repoName), nil
		}
		pr := s.store.GetPull(repo, int(num))
		if pr == nil {
			return mcpToolError(fmt.Sprintf("pull request %s not found", jsNumberString(num))), nil
		}
		if pr.State != "open" {
			return mcpToolError("pull request is already closed"), nil
		}
		merged := s.store.MergePull(repo, pr)
		if merged == nil {
			return mcpToolError("pull request cannot be merged: conflicts"), nil
		}
		return mcpToolResult(map[string]any{
			"sha": merged.Sha, "merged": true, "message": merged.Message,
			"number": pr.Number, "pull_number": pr.Number,
		}), nil

	default:
		return mcpToolError("tool " + tool.Name + " is not implemented"), nil
	}
}

// handleMcp serves POST /mcp and reports whether the request was its
// business. Anything else returns false so the router can try the next
// surface. The order of the checks matters: a body that is not JSON is
// a 400 before auth is even considered, and a missing id means the
// message is a notification, answered 202 with no body.
func (s *server) handleMcp(r *http.Request, w *respWriter, pathname string, rawBody []byte) bool {
	if pathname != "/mcp" {
		return false
	}
	if r.Method != "POST" {
		s.mcpSendJson(w, 405, mcpRpcError(nil, -32600, "method not allowed"))
		return true
	}
	raw := string(rawBody)
	parsed, err := parseJSON(rawBody)
	if err != nil {
		s.mcpSendJson(w, 400, mcpRpcError(sniffId(raw), -32700, "Parse error"))
		return true
	}
	if !s.isAuthorized(r) {
		s.mcpSendJson(w, 401, mcpRpcError(nil, -32000, "Bad credentials"))
		return true
	}
	msg := asRecord(parsed)
	id, hasID := msg["id"]
	if !hasID {
		// Notification (e.g. notifications/initialized): acknowledge
		// with an empty 202 and no JSON-RPC reply.
		w.header("content-length", "0")
		w.writeHeader(202)
		w.wrote = true
		return true
	}
	method, _ := msg["method"].(string)

	switch method {
	case "server/discover":
		serverInfo := map[string]any{"name": mcpServerName, "version": mcpServerVersion}
		s.mcpSendJson(w, 200, mcpRpcResult(id, map[string]any{
			"supportedVersions": mcpRevisions,
			"serverInfo":        serverInfo,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": serverInfo},
		}))
		return true

	case "initialize":
		params := asRecord(msg["params"])
		requested, _ := params["protocolVersion"].(string)
		protocolVersion := mcpRevisions[0]
		if requested != "" && containsStr(mcpRevisions, requested) {
			protocolVersion = requested
		}
		s.mcpSendJson(w, 200, mcpRpcResult(id, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpServerName, "version": mcpServerVersion},
		}))
		return true

	case "ping":
		s.mcpSendJson(w, 200, mcpRpcResult(id, map[string]any{}))
		return true

	case "tools/list":
		tools := make([]any, 0, len(mcpTools))
		for _, tool := range mcpTools {
			tools = append(tools, mcpToolJSON(tool))
		}
		s.mcpSendJson(w, 200, mcpRpcResult(id, map[string]any{"tools": tools}))
		return true

	case "tools/call":
		params := asRecord(msg["params"])
		name, _ := params["name"].(string)
		if name == "" {
			s.mcpSendJson(w, 200, mcpRpcError(id, -32602, "tool name is required"))
			return true
		}
		if findMcpTool(name) == nil {
			s.mcpSendJson(w, 200, mcpRpcError(id, -32602, "unknown tool "+name))
			return true
		}
		args := asRecord(params["arguments"])
		result, rpcErr := s.mcpCallTool(name, args)
		if rpcErr != nil {
			s.mcpSendJson(w, 200, mcpRpcError(id, rpcErr.code, rpcErr.message))
			return true
		}
		s.mcpSendJson(w, 200, mcpRpcResult(id, result))
		return true
	}

	s.mcpSendJson(w, 200, mcpRpcError(id, -32601, "method not found: "+method))
	return true
}
