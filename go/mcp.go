// mcp.go — MCP at POST /mcp: JSON-RPC 2.0 over Streamable HTTP (plain JSON
// answers; no SSE needed). Ported from mcp.ts. Revision 2026-07-28 via
// server/discover, legacy handshake via initialize +
// notifications/initialized. Tools act on the same store as REST, with the
// same ids.

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

var mcpRevisions = []string{"2026-07-28", "2025-11-25"}

type mcpToolDef struct {
	Name        string
	Description string
	InputSchema mcpInputSchema
}

type mcpInputSchema struct {
	Type       string
	Properties map[string]mcpProp
	Required   []string
}

type mcpProp struct {
	Type        string
	Description string
}

var mcpTools = []mcpToolDef{
	{
		Name: "create_repo", Description: "Create a new repository owned by the authenticated user.",
		InputSchema: mcpInputSchema{Type: "object", Properties: map[string]mcpProp{
			"owner":       {Type: "string", Description: "Owner login (defaults to the authenticated user)"},
			"name":        {Type: "string", Description: "Repository name"},
			"description": {Type: "string", Description: "Repository description"},
			"private":     {Type: "boolean", Description: "Whether the repository is private"},
		}, Required: []string{"name"}},
	},
	{
		Name: "get_repo", Description: "Get a repository by owner and name.",
		InputSchema: mcpInputSchema{Type: "object", Properties: map[string]mcpProp{
			"owner": {Type: "string", Description: "Owner login"},
			"repo":  {Type: "string", Description: "Repository name"},
		}, Required: []string{"owner", "repo"}},
	},
	{
		Name: "create_issue", Description: "Create an issue in a repository.",
		InputSchema: mcpInputSchema{Type: "object", Properties: map[string]mcpProp{
			"owner": {Type: "string", Description: "Owner login"},
			"repo":  {Type: "string", Description: "Repository name"},
			"title": {Type: "string", Description: "Issue title"},
			"body":  {Type: "string", Description: "Issue body"},
		}, Required: []string{"owner", "repo", "title"}},
	},
	{
		Name: "list_issues", Description: "List issues in a repository, optionally filtered by state.",
		InputSchema: mcpInputSchema{Type: "object", Properties: map[string]mcpProp{
			"owner": {Type: "string", Description: "Owner login"},
			"repo":  {Type: "string", Description: "Repository name"},
			"state": {Type: "string", Description: "open, closed, or all"},
		}, Required: []string{"owner", "repo"}},
	},
	{
		Name: "create_pull_request", Description: "Create a pull request from a head branch into a base branch.",
		InputSchema: mcpInputSchema{Type: "object", Properties: map[string]mcpProp{
			"owner": {Type: "string", Description: "Owner login"},
			"repo":  {Type: "string", Description: "Repository name"},
			"title": {Type: "string", Description: "Pull request title"},
			"body":  {Type: "string", Description: "Pull request body"},
			"head":  {Type: "string", Description: "Head branch name"},
			"base":  {Type: "string", Description: "Base branch name"},
		}, Required: []string{"owner", "repo", "title", "head", "base"}},
	},
	{
		Name: "merge_pull_request", Description: "Squash-merge a pull request into its base branch.",
		InputSchema: mcpInputSchema{Type: "object", Properties: map[string]mcpProp{
			"owner":       {Type: "string", Description: "Owner login"},
			"repo":        {Type: "string", Description: "Repository name"},
			"pull_number": {Type: "number", Description: "Pull request number"},
		}, Required: []string{"owner", "repo", "pull_number"}},
	},
}

func mcpToolJSON(t mcpToolDef) map[string]any {
	props := make(map[string]any, len(t.InputSchema.Properties))
	for name, p := range t.InputSchema.Properties {
		prop := map[string]any{"type": p.Type}
		if p.Description != "" {
			prop["description"] = p.Description
		}
		props[name] = prop
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

func (s *server) mcpSendJson(w *respWriter, status int, body any) {
	s.sendJsonPlain(w, status, body)
}

func mcpRpcResult(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func mcpRpcError(id any, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

func mcpToolResult(obj any) map[string]any {
	text := string(marshalJSON(obj))
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": text}},
		"structuredContent": obj,
	}
}

func mcpToolError(message string) map[string]any {
	text := string(marshalJSON(map[string]any{"error": message}))
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": text}},
		"structuredContent": map[string]any{"error": message},
		"isError":           true,
	}
}

var sniffIdRe = regexp.MustCompile(`"id"\s*:\s*("(?:[^"\\]|\\.)*"|-?\d+)`)

// Extract a JSON-RPC id from raw text even when the body is truncated.
func sniffId(raw string) any {
	m := sniffIdRe.FindStringSubmatch(raw)
	if m == nil {
		return nil
	}
	token := m[1]
	if strings.HasPrefix(token, "\"") {
		var s string
		if err := json.Unmarshal([]byte(token), &s); err == nil {
			return s
		}
		return nil
	}
	n, _ := strconv.ParseFloat(token, 64)
	return n
}

func findMcpTool(name string) *mcpToolDef {
	lower := strings.ToLower(name)
	for i := range mcpTools {
		t := &mcpTools[i]
		if lower == t.Name || strings.HasSuffix(lower, "__"+t.Name) || strings.HasSuffix(lower, "_"+t.Name) {
			return t
		}
	}
	return nil
}

type mcpRpcErr struct {
	code    int
	message string
}

// callTool returns either a tool result object or (nil, rpcErr) for the
// unknown-tool case (mirroring the __rpcError sentinel in mcp.ts).
func (s *server) mcpCallTool(name string, args map[string]any) (map[string]any, *mcpRpcErr) {
	tool := findMcpTool(name)
	if tool == nil {
		return nil, &mcpRpcErr{code: -32602, message: "unknown tool " + name}
	}
	owner := viewerLogin
	if v, ok := args["owner"].(string); ok {
		owner = v
	}
	repoName := ""
	if v, ok := args["repo"].(string); ok {
		repoName = v
	} else if v, ok := args["repository"].(string); ok {
		repoName = v
	} else if v, ok := args["repo_name"].(string); ok {
		repoName = v
	}

	switch tool.Name {
	case "create_repo":
		repoNameArg, _ := args["name"].(string)
		if repoNameArg == "" {
			return mcpToolError("name is required"), nil
		}
		desc, _ := args["description"].(string)
		priv, _ := args["private"].(bool)
		repo := s.store.CreateRepo(repoNameArg, desc, true, priv, "", false)
		if repo == nil {
			return mcpToolError("repository " + repoNameArg + " already exists"), nil
		}
		return mcpToolResult(s.store.RepoJSON(repo)), nil
	case "get_repo":
		if repoName == "" {
			return mcpToolError("repo is required"), nil
		}
		repo := s.store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError(fmt.Sprintf("repository %s/%s not found", owner, repoName)), nil
		}
		return mcpToolResult(s.store.RepoJSON(repo)), nil
	case "create_issue":
		if repoName == "" {
			return mcpToolError("repo is required"), nil
		}
		title, _ := args["title"].(string)
		if title == "" {
			return mcpToolError("title is required"), nil
		}
		repo := s.store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError(fmt.Sprintf("repository %s/%s not found", owner, repoName)), nil
		}
		bodyStr, _ := args["body"].(string)
		issue := s.store.CreateIssue(repo, title, bodyStr, []string{})
		return mcpToolResult(s.store.IssueJSON(repo, issue)), nil
	case "list_issues":
		if repoName == "" {
			return mcpToolError("repo is required"), nil
		}
		repo := s.store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError(fmt.Sprintf("repository %s/%s not found", owner, repoName)), nil
		}
		state, _ := args["state"].(string)
		if state == "" {
			state = "open"
		}
		issues := make([]any, 0)
		for _, i := range repo.Issues {
			if state == "all" || i.State == state {
				issues = append(issues, s.store.IssueJSON(repo, i))
			}
		}
		return mcpToolResult(map[string]any{"issues": issues}), nil
	case "create_pull_request":
		if repoName == "" {
			return mcpToolError("repo is required"), nil
		}
		title, _ := args["title"].(string)
		head, _ := args["head"].(string)
		base := "main"
		if v, ok := args["base"].(string); ok {
			base = v
		}
		if title == "" {
			return mcpToolError("title is required"), nil
		}
		if head == "" {
			return mcpToolError("head is required"), nil
		}
		repo := s.store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError(fmt.Sprintf("repository %s/%s not found", owner, repoName)), nil
		}
		if _, ok := s.store.ResolveBranch(repo, head); !ok {
			return mcpToolError("head or base branch does not exist"), nil
		}
		if _, ok := s.store.ResolveBranch(repo, base); !ok {
			return mcpToolError("head or base branch does not exist"), nil
		}
		var pr *Pull
		for _, p := range repo.Pulls {
			if p.State == "open" && p.HeadRef == head && p.BaseRef == base {
				pr = p
				break
			}
		}
		if pr == nil {
			bodyStr, _ := args["body"].(string)
			pr = s.store.CreatePull(repo, title, bodyStr, head, base)
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
			v := args["pull_number"]
			if v == nil {
				v = args["number"]
			}
			num = jsNumberValue(v)
		}
		if !jsIsInteger(num) || num <= 0 {
			return mcpToolError("pull_number is required"), nil
		}
		repo := s.store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError(fmt.Sprintf("repository %s/%s not found", owner, repoName)), nil
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
		return mcpToolResult(map[string]any{"sha": merged.Sha, "merged": true, "message": merged.Message, "number": pr.Number, "pull_number": pr.Number}), nil
	default:
		return mcpToolError("tool " + tool.Name + " is not implemented"), nil
	}
}

// Handle POST /mcp. Returns true when handled.
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
		// Non-JSON body: HTTP 400 with JSON-RPC -32700 (modern era).
		s.mcpSendJson(w, 400, mcpRpcError(sniffId(raw), -32700, "Parse error"))
		return true
	}
	if !s.isAuthorized(r) {
		s.mcpSendJson(w, 401, mcpRpcError(nil, -32000, "Bad credentials"))
		return true
	}
	msg := asRecord(parsed)
	var id any
	hasID := false
	if v, ok := msg["id"]; ok {
		id = v
		hasID = true
	}
	method, _ := msg["method"].(string)

	// Notifications carry no id: answer 202 with an empty body.
	if !hasID {
		w.header("content-length", "0")
		w.writeHeader(202)
		w.wrote = true
		return true
	}

	if method == "server/discover" {
		serverInfo := map[string]any{"name": mcpServerName, "version": mcpServerVersion}
		s.mcpSendJson(w, 200, mcpRpcResult(id, map[string]any{
			"supportedVersions": mcpRevisions,
			"serverInfo":        serverInfo,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": serverInfo},
		}))
		return true
	}
	if method == "initialize" {
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
	}
	if method == "ping" {
		s.mcpSendJson(w, 200, mcpRpcResult(id, map[string]any{}))
		return true
	}
	if method == "tools/list" {
		tools := make([]any, 0, len(mcpTools))
		for _, t := range mcpTools {
			tools = append(tools, mcpToolJSON(t))
		}
		s.mcpSendJson(w, 200, mcpRpcResult(id, map[string]any{"tools": tools}))
		return true
	}
	if method == "tools/call" {
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
