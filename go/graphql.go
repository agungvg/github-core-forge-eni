// graphql.go — GraphQL v4 at POST /api/graphql. Ported from graphql.ts.
// A small executor for the documents gh and github-mcp-server send: viewer,
// repository, issues, pull requests, node, __type introspection, the five
// mutations, and batched operations. Unknown fields answer HTTP 200 with an
// errors array naming the field; unknown repositories answer data.repository
// null with no errors.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ------------------------------------------------------------------ parsing

// gqlVar is a variable reference ($name) inside a parsed value.
type gqlVar struct{ name string }

type gqlField struct {
	alias      *string
	name       string
	args       map[string]any
	selections []*gqlField // nil = no sub-selections
	inlineType *string
}

type gqlOperation struct {
	kind       string // "query" | "mutation"
	selections []*gqlField
}

func tokenize(input string) []string {
	var tokens []string
	i := 0
	for i < len(input) {
		c := input[i]
		if isJSSpace(rune(c)) {
			i++
			continue
		}
		if c == '#' {
			for i < len(input) && input[i] != '\n' {
				i++
			}
			continue
		}
		if c == '.' && strings.HasPrefix(input[i:], "...") {
			tokens = append(tokens, "...")
			i += 3
			continue
		}
		if strings.IndexByte("{}():,=!@$[]", c) >= 0 {
			tokens = append(tokens, string(c))
			i++
			continue
		}
		if c == '"' {
			j := i + 1
			var out strings.Builder
			for j < len(input) && input[j] != '"' {
				if input[j] == '\\' && j+1 < len(input) {
					out.WriteByte(input[j+1])
					j += 2
				} else {
					out.WriteByte(input[j])
					j++
				}
			}
			tokens = append(tokens, string(marshalJSON(out.String())))
			i = j + 1
			continue
		}
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' {
			j := i
			for j < len(input) {
				d := input[j]
				if (d >= 'A' && d <= 'Z') || (d >= 'a' && d <= 'z') || (d >= '0' && d <= '9') || d == '_' {
					j++
					continue
				}
				break
			}
			tokens = append(tokens, input[i:j])
			i = j
			continue
		}
		if (c >= '0' && c <= '9') || c == '-' {
			j := i
			for j < len(input) {
				d := input[j]
				if (d >= '0' && d <= '9') || d == '.' || d == 'e' || d == 'E' || d == '+' || d == '-' {
					j++
					continue
				}
				break
			}
			tokens = append(tokens, input[i:j])
			i = j
			continue
		}
		i++ // skip anything else
	}
	return tokens
}

type gqlParser struct {
	pos    int
	tokens []string
}

func (p *gqlParser) peek() (string, bool) {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos], true
	}
	return "", false
}

func (p *gqlParser) next() (string, bool) {
	if p.pos < len(p.tokens) {
		t := p.tokens[p.pos]
		p.pos++
		return t, true
	}
	return "", false
}

func (p *gqlParser) expect(tok string) error {
	got, ok := p.next()
	if !ok || got != tok {
		gotStr := "end"
		if ok {
			gotStr = got
		}
		return fmt.Errorf("expected %s, got %s", tok, gotStr)
	}
	return nil
}

func (p *gqlParser) parseOperation() (gqlOperation, error) {
	kind := "query"
	if peek, ok := p.peek(); ok && (peek == "query" || peek == "mutation") {
		kind, _ = p.next()
	}
	// Optional operation name.
	if peek, ok := p.peek(); ok && peek != "{" && peek != "(" {
		p.next()
	}
	// Optional variable definitions: skip balanced parens.
	if peek, ok := p.peek(); ok && peek == "(" {
		depth := 0
		for p.pos < len(p.tokens) {
			t, _ := p.next()
			if t == "(" {
				depth++
			} else if t == ")" {
				depth--
				if depth == 0 {
					break
				}
			}
		}
	}
	selections, err := p.parseSelectionSet()
	if err != nil {
		return gqlOperation{}, err
	}
	return gqlOperation{kind: kind, selections: selections}, nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlField, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	fields := []*gqlField{}
	for {
		peek, ok := p.peek()
		if !ok || peek == "}" {
			break
		}
		parsed, err := p.parseField()
		if err != nil {
			return nil, err
		}
		fields = append(fields, parsed...)
	}
	if err := p.expect("}"); err != nil {
		return nil, err
	}
	return fields, nil
}

func (p *gqlParser) parseField() ([]*gqlField, error) {
	if peek, ok := p.peek(); ok && peek == "..." {
		p.next()
		var inlineType *string
		if peek, ok := p.peek(); ok && peek == "on" {
			p.next()
			t, _ := p.next()
			inlineType = &t
		} else {
			// Named fragment spread (not used by the suite's documents).
			p.next()
			return []*gqlField{}, nil
		}
		selections, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		for _, f := range selections {
			f.inlineType = inlineType
		}
		return selections, nil
	}
	name, _ := p.next()
	var alias *string
	if peek, ok := p.peek(); ok && peek == ":" {
		p.next()
		alias = &name
		name, _ = p.next()
	}
	args := make(map[string]any)
	if peek, ok := p.peek(); ok && peek == "(" {
		p.next()
		for {
			peek, ok := p.peek()
			if !ok || peek == ")" {
				break
			}
			argName, _ := p.next()
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			args[argName] = v
			if peek, ok := p.peek(); ok && peek == "," {
				p.next()
			}
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
	}
	var selections []*gqlField
	if peek, ok := p.peek(); ok && peek == "{" {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		selections = sel
	}
	return []*gqlField{{alias: alias, name: name, args: args, selections: selections}}, nil
}

var gqlNumberRe = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

func (p *gqlParser) parseValue() (any, error) {
	tok, ok := p.next()
	if !ok {
		// TS throws a TypeError reading .startsWith of undefined here.
		return nil, errors.New("Cannot read properties of undefined (reading 'startsWith')")
	}
	if tok == "$" {
		name, _ := p.next()
		return gqlVar{name: name}, nil
	}
	if tok == "[" {
		var items []any
		for {
			peek, ok := p.peek()
			if !ok || peek == "]" {
				break
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			items = append(items, v)
			if peek, ok := p.peek(); ok && peek == "," {
				p.next()
			}
		}
		if err := p.expect("]"); err != nil {
			return nil, err
		}
		if items == nil {
			items = []any{}
		}
		return items, nil
	}
	if tok == "{" {
		obj := make(map[string]any)
		for {
			peek, ok := p.peek()
			if !ok || peek == "}" {
				break
			}
			key, _ := p.next()
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			obj[key] = v
			if peek, ok := p.peek(); ok && peek == "," {
				p.next()
			}
		}
		if err := p.expect("}"); err != nil {
			return nil, err
		}
		return obj, nil
	}
	if strings.HasPrefix(tok, "\"") {
		var s string
		if err := json.Unmarshal([]byte(tok), &s); err != nil {
			return nil, err
		}
		return s, nil
	}
	if tok == "true" {
		return true, nil
	}
	if tok == "false" {
		return false, nil
	}
	if tok == "null" {
		return nil, nil
	}
	if gqlNumberRe.MatchString(tok) {
		f, _ := strconv.ParseFloat(tok, 64)
		return f, nil
	}
	return tok, nil // enum value, kept as a bare string
}

func resolveVariables(value any, variables map[string]any) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = resolveVariables(e, variables)
		}
		return out
	case gqlVar:
		if val, ok := variables[v.name]; ok {
			return val
		}
		return nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[k] = resolveVariables(e, variables)
		}
		return out
	default:
		return value
	}
}

// ------------------------------------------------------------------ schema

var typeFields = map[string][]string{
	"Query":                 {"viewer", "repository", "node", "__type", "__schema"},
	"User":                  {"login", "id", "name", "email"},
	"Repository":            {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequest", "pullRequests"},
	"Issue":                 {"id", "number", "title", "state", "body"},
	"PullRequest":           {"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"},
	"IssueConnection":       {"nodes", "pageInfo", "totalCount"},
	"PullRequestConnection": {"nodes", "pageInfo", "totalCount"},
	"FileConnection":        {"nodes"},
	"PageInfo":              {"hasNextPage", "endCursor"},
	"Ref":                   {"name"},
	"CommentEdge":           {"node"},
	"Comment":               {"id", "body"},
	"File":                  {"path"},
	"__Type":                {"name", "kind", "fields", "enumValues"},
	"__Field":               {"name"},
	"__EnumValue":           {"name"},
}

type introspectionInfo struct {
	fields     []string
	enumValues []string
}

var introspection = map[string]introspectionInfo{
	"Issue":       {fields: []string{"id", "number", "title", "state", "body"}},
	"PullRequest": {fields: []string{"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"}},
	"Repository":  {fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"}},
	"SearchType":  {enumValues: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION"}},
}

// ------------------------------------------------------------------ execution

type gqlCtx struct {
	store     *Store
	variables map[string]any
	errors    []map[string]any
}

func unknownField(ctx *gqlCtx, typeName, fieldName string) any {
	ctx.errors = append(ctx.errors, map[string]any{"message": fmt.Sprintf("Cannot query field \"%s\" on type \"%s\".", fieldName, typeName)})
	return nil
}

func fieldKey(f *gqlField) string {
	if f.alias != nil {
		return *f.alias
	}
	return f.name
}

func selectFields(ctx *gqlCtx, typeName string, value map[string]any, selections []*gqlField) map[string]any {
	if value == nil {
		return nil
	}
	if selections == nil {
		return value
	}
	out := make(map[string]any)
	allowed := typeFields[typeName]
	for _, field := range selections {
		key := fieldKey(field)
		if !containsStr(allowed, field.name) {
			out[key] = unknownField(ctx, typeName, field.name)
			continue
		}
		out[key] = resolveField(ctx, typeName, value, field)
	}
	return out
}

func issueValue(ctx *gqlCtx, repo *Repo, issueNumber int, selections []*gqlField) map[string]any {
	issue := ctx.store.GetIssue(repo, issueNumber)
	if issue == nil {
		return nil
	}
	state := "CLOSED"
	if issue.State == "open" {
		state = "OPEN"
	}
	return selectFields(ctx, "Issue", map[string]any{
		"id":     globalId("I", repo.ID, issue.Number),
		"number": issue.Number,
		"title":  issue.Title,
		"state":  state,
		"body":   issue.Body,
	}, selections)
}

func pullValue(ctx *gqlCtx, repo *Repo, pr *Pull, selections []*gqlField) map[string]any {
	mergeable := "MERGEABLE"
	if ctx.store.MergeConflicts(repo, pr) {
		mergeable = "CONFLICTING"
	}
	state := "CLOSED"
	if pr.Merged {
		state = "MERGED"
	} else if pr.State == "open" {
		state = "OPEN"
	}
	return selectFields(ctx, "PullRequest", map[string]any{
		"id":          globalId("P", repo.ID, pr.Number),
		"number":      pr.Number,
		"headRefName": pr.HeadRef,
		"baseRefName": pr.BaseRef,
		"mergeable":   mergeable,
		"state":       state,
		"merged":      pr.Merged,
		"__repo":      repo,
		"__pr":        pr,
	}, selections)
}

func cursorFor(number int) string {
	return base64urlEncode([]byte("issue:" + strconv.Itoa(number)))
}

var cursorRe = regexp.MustCompile(`^issue:(\d+)$`)

func cursorNumber(cursor string) *int {
	text := string(lenientBase64urlDecode(cursor))
	m := cursorRe.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	n, _ := strconv.Atoi(m[1])
	return &n
}

// jsNumberValue coerces a JSON value the way Number(v) does.
func jsNumberValue(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case float64:
		return t
	case string:
		n, _ := jsNumber(t)
		return n
	case []any:
		if len(t) == 0 {
			return 0
		}
		parts := make([]string, len(t))
		for i, e := range t {
			if e == nil {
				parts[i] = ""
			} else {
				parts[i] = jsString(e)
			}
		}
		n, _ := jsNumber(strings.Join(parts, ","))
		return n
	default:
		return math.NaN()
	}
}

// numFromValue converts a GraphQL arg the way Number(v) does, then to an
// int when it is an integer (TS compares with ===, so 5.7 never matches).
func numFromValue(v any) (int, bool) {
	f := jsNumberValue(v)
	if !jsIsInteger(f) {
		return 0, false
	}
	if f > 9e15 || f < -9e15 {
		return 0, false
	}
	return int(f), true
}

// numArg converts a decoded global-id part the way Number(part) does, then
// to an int when it is an integer id.
func numArg(s string) (int, bool) {
	f, ok := jsNumber(s)
	if !ok || !jsIsInteger(f) {
		return 0, false
	}
	if f > 9e15 || f < -9e15 {
		return 0, false
	}
	return int(f), true
}

// part returns the i-th decoded part, if present (TS yields undefined for a
// missing part, and Number(undefined) is NaN, which matches nothing).
func (d *decodedGlobalId) part(i int) (string, bool) {
	if i < len(d.Parts) {
		return d.Parts[i], true
	}
	return "", false
}

func resolveField(ctx *gqlCtx, typeName string, value map[string]any, field *gqlField) any {
	args := make(map[string]any, len(field.args))
	for k, v := range field.args {
		args[k] = resolveVariables(v, ctx.variables)
	}

	switch typeName {
	case "Query":
		if field.name == "viewer" {
			return selectFields(ctx, "User", map[string]any{"login": viewerLogin, "id": viewerID, "name": viewerName, "email": viewerEmail}, field.selections)
		}
		if field.name == "repository" {
			owner := jsString(firstNonNil(args["owner"], ""))
			name := jsString(firstNonNil(args["name"], ""))
			repo := ctx.store.GetRepo(owner, name)
			if repo == nil {
				return nil
			}
			return selectFields(ctx, "Repository", repositoryValue(repo), field.selections)
		}
		if field.name == "node" {
			id := jsString(firstNonNil(args["id"], ""))
			decoded := decodeGlobalId(id)
			if decoded == nil {
				return nil
			}
			if decoded.Tag == "R" {
				part0, ok := decoded.part(0)
				if !ok {
					return nil
				}
				repoID, ok := numArg(part0)
				if !ok {
					return nil
				}
				repo := ctx.store.GetRepoById(repoID)
				if repo == nil {
					return nil
				}
				return selectFields(ctx, "Repository", repositoryValue(repo), field.selections)
			}
			if decoded.Tag == "I" {
				part0, ok0 := decoded.part(0)
				part1, ok1 := decoded.part(1)
				if !ok0 || !ok1 {
					return nil
				}
				repoID, ok := numArg(part0)
				if !ok {
					return nil
				}
				repo := ctx.store.GetRepoById(repoID)
				if repo == nil {
					return nil
				}
				num, ok := numArg(part1)
				if !ok {
					return nil
				}
				return issueValue(ctx, repo, num, field.selections)
			}
			if decoded.Tag == "P" {
				part0, ok0 := decoded.part(0)
				part1, ok1 := decoded.part(1)
				if !ok0 || !ok1 {
					return nil
				}
				repoID, ok := numArg(part0)
				if !ok {
					return nil
				}
				repo := ctx.store.GetRepoById(repoID)
				if repo == nil {
					return nil
				}
				num, ok := numArg(part1)
				if !ok {
					return nil
				}
				pr := ctx.store.GetPull(repo, num)
				if pr == nil {
					return nil
				}
				return pullValue(ctx, repo, pr, field.selections)
			}
			return nil
		}
		if field.name == "__type" {
			name := jsString(firstNonNil(args["name"], ""))
			info, ok := introspection[name]
			if !ok {
				return nil
			}
			kind := "OBJECT"
			if info.enumValues != nil {
				kind = "ENUM"
			}
			fields := info.fields
			if fields == nil {
				fields = []string{}
			}
			enumValues := info.enumValues
			if enumValues == nil {
				enumValues = []string{}
			}
			return selectFields(ctx, "__Type", map[string]any{"name": name, "kind": kind, "__fields": fields, "__enumValues": enumValues}, field.selections)
		}
		if field.name == "__schema" {
			return map[string]any{"queryType": map[string]any{"name": "Query"}}
		}
		return unknownField(ctx, typeName, field.name)
	case "Repository":
		repo := value["__repo"].(*Repo)
		if field.name == "owner" {
			return selectFields(ctx, "User", map[string]any{"login": repo.Owner, "id": viewerID, "name": viewerName, "email": viewerEmail}, field.selections)
		}
		if field.name == "defaultBranchRef" {
			tip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
			if !ok {
				return nil
			}
			_ = tip
			return selectFields(ctx, "Ref", map[string]any{"name": repo.DefaultBranch}, field.selections)
		}
		if field.name == "issue" {
			num, ok := numFromValue(args["number"])
			if !ok {
				return nil
			}
			return issueValue(ctx, repo, num, field.selections)
		}
		if field.name == "issues" {
			var states []any
			if arr, ok := args["states"].([]any); ok {
				states = arr
			}
			orderBy := asRecord(args["orderBy"])
			first := 10.0
			if f, ok := args["first"].(float64); ok {
				first = f
			}
			var after *int
			if s, ok := args["after"].(string); ok {
				after = cursorNumber(s)
			}
			issues := append([]*Issue{}, repo.Issues...)
			if states != nil {
				var filtered []*Issue
				for _, i := range issues {
					upper := strings.ToUpper(i.State)
					for _, st := range states {
						if st == upper {
							filtered = append(filtered, i)
							break
						}
					}
				}
				issues = filtered
			}
			orderField, _ := orderBy["field"].(string)
			orderDir, _ := orderBy["direction"].(string)
			if orderField == "CREATED_AT" && orderDir == "DESC" {
				sort.SliceStable(issues, func(a, b int) bool {
					if issues[a].CreatedSeq != issues[b].CreatedSeq {
						return issues[a].CreatedSeq > issues[b].CreatedSeq
					}
					return issues[a].Number > issues[b].Number
				})
			} else if orderField == "CREATED_AT" && orderDir == "ASC" {
				sort.SliceStable(issues, func(a, b int) bool {
					if issues[a].CreatedSeq != issues[b].CreatedSeq {
						return issues[a].CreatedSeq < issues[b].CreatedSeq
					}
					return issues[a].Number < issues[b].Number
				})
			} else {
				sort.SliceStable(issues, func(a, b int) bool { return issues[a].Number < issues[b].Number })
			}
			if after != nil {
				idx := -1
				for i, iss := range issues {
					if iss.Number == *after {
						idx = i
						break
					}
				}
				if idx >= 0 {
					issues = issues[idx+1:]
				}
			}
			lo, hi := jsSlice(len(issues), 0, first)
			page := issues[lo:hi]
			hasNext := len(issues) > int(first)
			var endCursor any
			if len(page) > 0 {
				endCursor = cursorFor(page[len(page)-1].Number)
			}
			var nodeSelections []*gqlField
			var pageInfoSel []*gqlField
			for _, s := range field.selections {
				if s.name == "nodes" {
					nodeSelections = s.selections
				}
				if s.name == "pageInfo" {
					pageInfoSel = s.selections
				}
			}
			nodes := make([]any, 0, len(page))
			for _, issue := range page {
				nodes = append(nodes, issueValue(ctx, repo, issue.Number, nodeSelections))
			}
			var pageInfo any
			if pageInfoSel != nil {
				pageInfo = selectFields(ctx, "PageInfo", map[string]any{"hasNextPage": hasNext, "endCursor": endCursor}, pageInfoSel)
			} else {
				pageInfo = map[string]any{"hasNextPage": hasNext, "endCursor": endCursor}
			}
			return selectFields(ctx, "IssueConnection", map[string]any{
				"__nodes":    nodes,
				"__pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": endCursor},
				"totalCount": len(issues),
				"nodes":      nodes,
				"pageInfo":   pageInfo,
			}, field.selections)
		}
		if field.name == "pullRequest" {
			num, ok := numFromValue(args["number"])
			if !ok {
				return nil
			}
			pr := ctx.store.GetPull(repo, num)
			if pr == nil {
				return nil
			}
			return pullValue(ctx, repo, pr, field.selections)
		}
		if field.name == "pullRequests" {
			first := 10.0
			if f, ok := args["first"].(float64); ok {
				first = f
			}
			pulls := append([]*Pull{}, repo.Pulls...)
			sort.SliceStable(pulls, func(a, b int) bool { return pulls[a].Number < pulls[b].Number })
			lo, hi := jsSlice(len(pulls), 0, first)
			page := pulls[lo:hi]
			var nodeSelections []*gqlField
			for _, s := range field.selections {
				if s.name == "nodes" {
					nodeSelections = s.selections
				}
			}
			nodes := make([]any, 0, len(page))
			for _, pr := range page {
				nodes = append(nodes, pullValue(ctx, repo, pr, nodeSelections))
			}
			return selectFields(ctx, "PullRequestConnection", map[string]any{"__nodes": nodes, "nodes": nodes, "totalCount": len(repo.Pulls)}, field.selections)
		}
		// Scalar repository fields live on the value object.
		if v, ok := value[field.name]; ok {
			return v
		}
		return nil
	case "PullRequest":
		if field.name == "files" {
			repo := value["__repo"].(*Repo)
			pr := value["__pr"].(*Pull)
			first := 10.0
			if f, ok := args["first"].(float64); ok {
				first = f
			}
			files := ctx.store.PullFiles(repo, pr)
			lo, hi := jsSlice(len(files), 0, first)
			page := files[lo:hi]
			var nodeSelections []*gqlField
			for _, s := range field.selections {
				if s.name == "nodes" {
					nodeSelections = s.selections
				}
			}
			nodes := make([]any, 0, len(page))
			for _, f := range page {
				nodes = append(nodes, selectFields(ctx, "File", map[string]any{"path": f.Path}, nodeSelections))
			}
			return selectFields(ctx, "FileConnection", map[string]any{"nodes": nodes}, field.selections)
		}
		if v, ok := value[field.name]; ok {
			return v
		}
		return nil
	case "IssueConnection":
		if field.name == "nodes" {
			if v, ok := value["__nodes"]; ok {
				return v
			}
			return []any{}
		}
		if field.name == "pageInfo" {
			info := value["__pageInfo"].(map[string]any)
			return selectFields(ctx, "PageInfo", info, field.selections)
		}
		if v, ok := value[field.name]; ok {
			return v
		}
		return nil
	case "PullRequestConnection":
		if field.name == "nodes" {
			if v, ok := value["__nodes"]; ok {
				return v
			}
			return []any{}
		}
		if v, ok := value[field.name]; ok {
			return v
		}
		return nil
	case "FileConnection":
		if field.name == "nodes" {
			if v, ok := value["nodes"]; ok {
				return v
			}
			return []any{}
		}
		if v, ok := value[field.name]; ok {
			return v
		}
		return nil
	case "__Type":
		if field.name == "fields" {
			names := value["__fields"].([]string)
			out := make([]any, 0, len(names))
			for _, n := range names {
				out = append(out, selectFields(ctx, "__Field", map[string]any{"name": n}, field.selections))
			}
			return out
		}
		if field.name == "enumValues" {
			names := value["__enumValues"].([]string)
			out := make([]any, 0, len(names))
			for _, n := range names {
				out = append(out, selectFields(ctx, "__EnumValue", map[string]any{"name": n}, field.selections))
			}
			return out
		}
		if v, ok := value[field.name]; ok {
			return v
		}
		return nil
	default:
		if v, ok := value[field.name]; ok {
			return v
		}
		return nil
	}
}

// firstNonNil mirrors `v ?? fallback` for a possibly-missing arg.
func firstNonNil(v any, fallback any) any {
	if v == nil {
		return fallback
	}
	return v
}

func repositoryValue(repo *Repo) map[string]any {
	return map[string]any{
		"__repo":           repo,
		"id":               globalId("R", repo.ID),
		"databaseId":       repo.ID,
		"name":             repo.Name,
		"nameWithOwner":    repo.Owner + "/" + repo.Name,
		"viewerPermission": "ADMIN",
		"hasIssuesEnabled": true,
	}
}

// ------------------------------------------------------------------ mutations

func executeMutation(ctx *gqlCtx, field *gqlField) map[string]any {
	args := make(map[string]any, len(field.args))
	for k, v := range field.args {
		args[k] = resolveVariables(v, ctx.variables)
	}
	input := asRecord(args["input"])
	key := fieldKey(field)

	if field.name == "createIssue" {
		decoded := decodeGlobalId(jsString(firstNonNil(input["repositoryId"], "")))
		var repo *Repo
		if decoded != nil && decoded.Tag == "R" {
			if part0, ok := decoded.part(0); ok {
				if id, ok := numArg(part0); ok {
					repo = ctx.store.GetRepoById(id)
				}
			}
		}
		if repo == nil {
			ctx.errors = append(ctx.errors, map[string]any{"message": "repository not found"})
			return map[string]any{key: nil}
		}
		issue := ctx.store.CreateIssue(repo, jsString(firstNonNil(input["title"], "")), jsString(firstNonNil(input["body"], "")), []string{})
		var sel []*gqlField
		for _, s := range field.selections {
			if s.name == "issue" {
				sel = s.selections
			}
		}
		return map[string]any{key: map[string]any{"issue": issueValue(ctx, repo, issue.Number, sel)}}
	}
	if field.name == "closeIssue" {
		decoded := decodeGlobalId(jsString(firstNonNil(input["issueId"], "")))
		var repo *Repo
		var issue *Issue
		if decoded != nil && decoded.Tag == "I" {
			if part0, ok := decoded.part(0); ok {
				if id, ok := numArg(part0); ok {
					repo = ctx.store.GetRepoById(id)
				}
			}
			if repo != nil {
				if part1, ok := decoded.part(1); ok {
					if num, ok := numArg(part1); ok {
						issue = ctx.store.GetIssue(repo, num)
					}
				}
			}
		}
		if repo == nil || issue == nil {
			ctx.errors = append(ctx.errors, map[string]any{"message": "issue not found"})
			return map[string]any{key: nil}
		}
		issue.State = "closed"
		issue.UpdatedSeq = ctx.store.nextSeq()
		issue.jsonCache = nil
		var sel []*gqlField
		for _, s := range field.selections {
			if s.name == "issue" {
				sel = s.selections
			}
		}
		return map[string]any{key: map[string]any{"issue": issueValue(ctx, repo, issue.Number, sel)}}
	}
	if field.name == "createPullRequest" {
		decoded := decodeGlobalId(jsString(firstNonNil(input["repositoryId"], "")))
		var repo *Repo
		if decoded != nil && decoded.Tag == "R" {
			if part0, ok := decoded.part(0); ok {
				if id, ok := numArg(part0); ok {
					repo = ctx.store.GetRepoById(id)
				}
			}
		}
		if repo == nil {
			ctx.errors = append(ctx.errors, map[string]any{"message": "repository not found"})
			return map[string]any{key: nil}
		}
		head := jsString(firstNonNil(input["headRefName"], ""))
		base := jsString(firstNonNil(input["baseRefName"], repo.DefaultBranch))
		var pr *Pull
		for _, p := range repo.Pulls {
			if p.State == "open" && p.HeadRef == head && p.BaseRef == base {
				pr = p
				break
			}
		}
		if pr == nil {
			pr = ctx.store.CreatePull(repo, jsString(firstNonNil(input["title"], "")), jsString(firstNonNil(input["body"], "")), head, base)
		}
		if pr == nil {
			ctx.errors = append(ctx.errors, map[string]any{"message": "head or base branch does not exist"})
			return map[string]any{key: nil}
		}
		var sel []*gqlField
		for _, s := range field.selections {
			if s.name == "pullRequest" {
				sel = s.selections
			}
		}
		return map[string]any{key: map[string]any{"pullRequest": pullValue(ctx, repo, pr, sel)}}
	}
	if field.name == "mergePullRequest" {
		decoded := decodeGlobalId(jsString(firstNonNil(input["pullRequestId"], "")))
		var repo *Repo
		var pr *Pull
		if decoded != nil && decoded.Tag == "P" {
			if part0, ok := decoded.part(0); ok {
				if id, ok := numArg(part0); ok {
					repo = ctx.store.GetRepoById(id)
				}
			}
			if repo != nil {
				if part1, ok := decoded.part(1); ok {
					if num, ok := numArg(part1); ok {
						pr = ctx.store.GetPull(repo, num)
					}
				}
			}
		}
		if repo == nil || pr == nil {
			ctx.errors = append(ctx.errors, map[string]any{"message": "pull request not found"})
			return map[string]any{key: nil}
		}
		if pr.State == "open" {
			ctx.store.MergePull(repo, pr)
		}
		var sel []*gqlField
		for _, s := range field.selections {
			if s.name == "pullRequest" {
				sel = s.selections
			}
		}
		return map[string]any{key: map[string]any{"pullRequest": pullValue(ctx, repo, pr, sel)}}
	}
	if field.name == "addComment" {
		decoded := decodeGlobalId(jsString(firstNonNil(input["subjectId"], "")))
		var repo *Repo
		var issue *Issue
		if decoded != nil && decoded.Tag == "I" {
			if part0, ok := decoded.part(0); ok {
				if id, ok := numArg(part0); ok {
					repo = ctx.store.GetRepoById(id)
				}
			}
			if repo != nil {
				if part1, ok := decoded.part(1); ok {
					if num, ok := numArg(part1); ok {
						issue = ctx.store.GetIssue(repo, num)
					}
				}
			}
		}
		if repo == nil || issue == nil {
			ctx.errors = append(ctx.errors, map[string]any{"message": "subject not found"})
			return map[string]any{key: nil}
		}
		comment := ctx.store.AddComment(repo, issue, jsString(firstNonNil(input["body"], "")))
		var nodeSel []*gqlField
		for _, s := range field.selections {
			if s.name == "commentEdge" {
				for _, s2 := range s.selections {
					if s2.name == "node" {
						nodeSel = s2.selections
					}
				}
			}
		}
		return map[string]any{key: map[string]any{
			"commentEdge": map[string]any{
				"node": selectFields(ctx, "Comment", map[string]any{"id": globalId("C", repo.ID, issue.Number, comment.ID), "body": comment.Body}, nodeSel),
			},
		}}
	}
	ctx.errors = append(ctx.errors, map[string]any{"message": fmt.Sprintf("Cannot query field \"%s\" on type \"Mutation\".", field.name)})
	return map[string]any{key: nil}
}

func executeOperation(store *Store, query string, variables map[string]any) map[string]any {
	ctx := &gqlCtx{store: store, variables: variables}
	parser := &gqlParser{tokens: tokenize(query)}
	operation, err := parser.parseOperation()
	if err != nil {
		return map[string]any{"data": nil, "errors": []map[string]any{{"message": err.Error()}}}
	}
	data := make(map[string]any)
	if operation.kind == "mutation" {
		for _, field := range operation.selections {
			result := executeMutation(ctx, field)
			for k, v := range result {
				data[k] = v
			}
		}
	} else {
		for _, field := range operation.selections {
			key := fieldKey(field)
			if field.name == "viewer" || field.name == "repository" || field.name == "node" || field.name == "__type" || field.name == "__schema" {
				data[key] = resolveField(ctx, "Query", map[string]any{}, field)
			} else {
				data[key] = unknownField(ctx, "Query", field.name)
			}
		}
	}
	out := map[string]any{"data": data}
	if len(ctx.errors) > 0 {
		out["errors"] = ctx.errors
	}
	return out
}

// ------------------------------------------------------------------ HTTP

// Handle POST /api/graphql. Returns true when handled.
func (s *server) handleGraphQL(r *http.Request, w *respWriter, pathname string, rawBody []byte) bool {
	if pathname != "/api/graphql" {
		return false
	}
	if r.Method != "POST" {
		s.sendJsonPlain(w, 405, map[string]any{"data": nil, "errors": []map[string]any{{"message": "method not allowed"}}})
		return true
	}
	if !s.isAuthorized(r) {
		s.sendJsonPlain(w, 401, map[string]any{"message": "Bad credentials"})
		return true
	}
	parsed, err := parseJSON(rawBody)
	if err != nil {
		s.sendJsonPlain(w, 400, map[string]any{"data": nil, "errors": []map[string]any{{"message": "request body is not valid JSON"}}})
		return true
	}
	if arr, ok := parsed.([]any); ok {
		results := make([]any, 0, len(arr))
		for _, entry := range arr {
			op := asRecord(entry)
			query, _ := op["query"].(string)
			variables := asRecord(op["variables"])
			results = append(results, executeOperation(s.store, query, variables))
		}
		s.sendJsonPlain(w, 200, results)
		return true
	}
	op := asRecord(parsed)
	query, _ := op["query"].(string)
	variables := asRecord(op["variables"])
	s.sendJsonPlain(w, 200, executeOperation(s.store, query, variables))
	return true
}
