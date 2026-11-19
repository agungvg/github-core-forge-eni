// graphql.go — the GraphQL v4 endpoint at POST /api/graphql.
//
// A deliberately small executor, not a general GraphQL engine: it understands
// exactly the documents the trial clients send — viewer and repository reads,
// issue and pull-request connections, node() lookups, __type introspection,
// the five mutations, and batched request arrays. Anything outside that shape
// still gets a spec-shaped answer: an unknown field comes back as HTTP 200
// with an errors entry naming the field, an unknown repository as
// data.repository null with no errors, and a missing bearer token as exactly
// 401.
//
// Two wire rules matter throughout (see docs/DECISIONS.md D5 and D13):
// global ids are opaque base64url blobs over a one-letter tag plus ids, and
// this layer spells states in capitals — OPEN, CLOSED, MERGED, MERGEABLE,
// CONFLICTING — where the REST layer spells them lowercase. Both layers read
// the same stored state, so they can never drift apart.
//
// The whole module is in-memory and allocation-light on the hot paths: the
// lexer works on the raw document bytes, the parser builds one small tree,
// and the resolvers walk the store's slices directly.

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

// ---------------------------------------------------------------------------
// Lexer: a GraphQL document becomes a flat token list.
//
// String literals are kept in their JSON-quoted form (marshalJSON output),
// so the parser can hand them straight to json.Unmarshal. Everything else
// is a bare word: punctuation, names, numbers, "...", "$".

func tokenize(input string) []string {
	tokens := []string{}
	i := 0
	for i < len(input) {
		ch := input[i]
		switch {
		case isJSSpace(rune(ch)):
			i++
		case ch == '#':
			// Comment: run to the end of the line.
			for i < len(input) && input[i] != '\n' {
				i++
			}
		case ch == '.' && strings.HasPrefix(input[i:], "..."):
			tokens = append(tokens, "...")
			i += 3
		case strings.IndexByte("{}():,=!@$[]", ch) >= 0:
			tokens = append(tokens, string(ch))
			i++
		case ch == '"':
			tokens = append(tokens, tokenizeQuoted(input, &i))
		case ch == '_' || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z'):
			j := i + 1
			for j < len(input) && isNameByte(input[j]) {
				j++
			}
			tokens = append(tokens, input[i:j])
			i = j
		case (ch >= '0' && ch <= '9') || ch == '-':
			j := i + 1
			for j < len(input) && isNumberByte(input[j]) {
				j++
			}
			tokens = append(tokens, input[i:j])
			i = j
		default:
			// Any other byte is not part of the grammar; drop it.
			i++
		}
	}
	return tokens
}

// isNameByte reports whether b can continue a GraphQL name after its first
// character (letters, digits, underscore).
func isNameByte(b byte) bool {
	return b == '_' || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// isNumberByte reports whether b can continue a numeric literal, including
// the exponent markers a float may carry.
func isNumberByte(b byte) bool {
	return (b >= '0' && b <= '9') || b == '.' || b == 'e' || b == 'E' || b == '+' || b == '-'
}

// tokenizeQuoted scans one double-quoted literal starting at input[*pos]
// and advances *pos just past the closing quote. A backslash escapes the
// next byte verbatim (the backslash itself is dropped), matching the
// behaviour the documents in the suite rely on. The result is the literal
// re-encoded as JSON, ready for json.Unmarshal in parseValue.
func tokenizeQuoted(input string, pos *int) string {
	var raw strings.Builder
	j := *pos + 1
	for j < len(input) && input[j] != '"' {
		if input[j] == '\\' && j+1 < len(input) {
			raw.WriteByte(input[j+1])
			j += 2
			continue
		}
		raw.WriteByte(input[j])
		j++
	}
	*pos = j + 1
	return string(marshalJSON(raw.String()))
}

// ---------------------------------------------------------------------------
// Parser: the token list becomes one operation with a selection tree.
//
// gqlVar marks a variable reference ($name) inside a parsed value; it is
// resolved against the request's variables map only when the field runs, so
// the same parsed document could be reused across requests.

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
		seen := "end"
		if ok {
			seen = got
		}
		return fmt.Errorf("expected %s, got %s", tok, seen)
	}
	return nil
}

// skipParens consumes a balanced, parenthesised group — the variable
// definitions after an operation name, which this executor never needs to
// interpret because variables arrive in the request body.
func (p *gqlParser) skipParens() {
	depth := 0
	for p.pos < len(p.tokens) {
		t, _ := p.next()
		if t == "(" {
			depth++
		} else if t == ")" {
			depth--
			if depth == 0 {
				return
			}
		}
	}
}

func (p *gqlParser) parseOperation() (gqlOperation, error) {
	kind := "query"
	if head, ok := p.peek(); ok && (head == "query" || head == "mutation") {
		kind, _ = p.next()
	}
	// An operation name, when present, is just a label.
	if head, ok := p.peek(); ok && head != "{" && head != "(" {
		p.next()
	}
	if head, ok := p.peek(); ok && head == "(" {
		p.skipParens()
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
		head, ok := p.peek()
		if !ok || head == "}" {
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
	if head, ok := p.peek(); ok && head == "..." {
		return p.parseFragment()
	}
	name, _ := p.next()
	var alias *string
	if head, ok := p.peek(); ok && head == ":" {
		p.next()
		// Snapshot the alias: taking &name then reassigning name would
		// make the alias point at the field name instead.
		a := name
		alias = &a
		name, _ = p.next()
	}
	args := make(map[string]any)
	if head, ok := p.peek(); ok && head == "(" {
		p.next()
		for {
			head, ok := p.peek()
			if !ok || head == ")" {
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
			if head, ok := p.peek(); ok && head == "," {
				p.next()
			}
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
	}
	var selections []*gqlField
	if head, ok := p.peek(); ok && head == "{" {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		selections = sel
	}
	return []*gqlField{{alias: alias, name: name, args: args, selections: selections}}, nil
}

// parseFragment handles a spread after the leading "..." has been seen. An
// inline fragment ("... on Type { ... }") splices its fields into the
// enclosing set, tagged with the type condition; a named spread is not used
// by any document in the suite, so its name is consumed and it contributes
// nothing.
func (p *gqlParser) parseFragment() ([]*gqlField, error) {
	p.next() // consume "..."
	if head, ok := p.peek(); ok && head == "on" {
		p.next()
		typeName, _ := p.next()
		inner, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		for _, f := range inner {
			f.inlineType = &typeName
		}
		return inner, nil
	}
	p.next() // named fragment spread: consume the name, contribute nothing
	return []*gqlField{}, nil
}

var gqlNumberRe = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

func (p *gqlParser) parseValue() (any, error) {
	tok, ok := p.next()
	if !ok {
		// The reference implementation surfaces this as a TypeError reading
		// .startsWith of undefined; keep the message so clients see the
		// same failure for a truncated document.
		return nil, errors.New("Cannot read properties of undefined (reading 'startsWith')")
	}
	switch {
	case tok == "$":
		name, _ := p.next()
		return gqlVar{name: name}, nil
	case tok == "[":
		return p.parseList()
	case tok == "{":
		return p.parseObject()
	case strings.HasPrefix(tok, "\""):
		var s string
		if err := json.Unmarshal([]byte(tok), &s); err != nil {
			return nil, err
		}
		return s, nil
	case tok == "true":
		return true, nil
	case tok == "false":
		return false, nil
	case tok == "null":
		return nil, nil
	case gqlNumberRe.MatchString(tok):
		f, _ := strconv.ParseFloat(tok, 64)
		return f, nil
	default:
		return tok, nil // enum value, kept as a bare string
	}
}

// parseList reads the elements of a "[" value until the closing bracket.
func (p *gqlParser) parseList() (any, error) {
	items := []any{}
	for {
		head, ok := p.peek()
		if !ok || head == "]" {
			break
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		items = append(items, v)
		if head, ok := p.peek(); ok && head == "," {
			p.next()
		}
	}
	if err := p.expect("]"); err != nil {
		return nil, err
	}
	return items, nil
}

// parseObject reads the key/value pairs of a "{" value until the closing brace.
func (p *gqlParser) parseObject() (any, error) {
	obj := make(map[string]any)
	for {
		head, ok := p.peek()
		if !ok || head == "}" {
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
		if head, ok := p.peek(); ok && head == "," {
			p.next()
		}
	}
	if err := p.expect("}"); err != nil {
		return nil, err
	}
	return obj, nil
}

// resolveVariables walks a parsed value and substitutes every $name with
// the matching request variable (a missing variable becomes nil). Lists and
// objects are rebuilt so the parsed document is never mutated.
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

// ---------------------------------------------------------------------------
// Schema: the field names each type may answer, plus the small slice of
// introspection the clients actually ask for.
//
// The tables are the allow-list: a field that is not listed for its type is
// an error, never a silent null. Keeping them as plain data (instead of a
// generated schema) is what keeps this executor small.

// typeFields lists, per GraphQL type, the fields selectFields may resolve.
// "__"-prefixed keys never appear here; they are internal carriers on the
// value maps (see repositoryValue / pullValue) and are never selected.
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

// introspection answers __type(name:) for the types the suite probes. Only
// SearchType carries enum values; the object types carry their field lists.
var introspection = map[string]introspectionInfo{
	"Issue":       {fields: []string{"id", "number", "title", "state", "body"}},
	"PullRequest": {fields: []string{"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"}},
	"Repository":  {fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"}},
	"SearchType":  {enumValues: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION"}},
}

// ---------------------------------------------------------------------------
// Execution context and field selection.

// gqlCtx carries one operation's store, variables, and the errors it has
// accumulated so far. Errors never abort the operation: a field that fails
// answers null in place and the rest of the document still runs.
type gqlCtx struct {
	store     *Store
	variables map[string]any
	errors    []map[string]any
}

// unknownField records the standard "cannot query field" error and answers
// nil, which is what the field's slot holds in the response.
func unknownField(ctx *gqlCtx, typeName, fieldName string) any {
	ctx.errors = append(ctx.errors, map[string]any{"message": fmt.Sprintf("Cannot query field \"%s\" on type \"%s\".", fieldName, typeName)})
	return nil
}

// fieldKey is the response key for a field: its alias when one was written,
// otherwise its name.
func fieldKey(f *gqlField) string {
	if f.alias != nil {
		return *f.alias
	}
	return f.name
}

// selectFields projects one value map through a selection set. A nil value
// stays nil (a missing object has no fields to pick), and a nil selection
// set returns the whole map — the caller asked for the object as a whole.
// Every selected field is checked against the type's allow-list first.
func selectFields(ctx *gqlCtx, typeName string, value map[string]any, selections []*gqlField) map[string]any {
	if value == nil {
		return nil
	}
	if selections == nil {
		return value
	}
	out := make(map[string]any, len(selections))
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

// gqlChild picks the sub-selection named name out of a field's selections,
// or nil when the field does not select it. Mutation payloads and
// connections both read their nested objects this way.
func gqlChild(field *gqlField, name string) []*gqlField {
	for _, s := range field.selections {
		if s.name == name {
			return s.selections
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Object values: the maps resolvers project through selectFields.
//
// Internal carriers use "__"-prefixed keys so they can travel with the
// value without ever colliding with a real GraphQL field name: "__repo"
// holds the *Repo a repository field was resolved from, "__pr" the *Pull.

// gqlIssueState spells a stored lowercase issue state as a GraphQL enum.
func gqlIssueState(issue *Issue) string {
	if issue.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func issueValue(ctx *gqlCtx, repo *Repo, issueNumber int, selections []*gqlField) map[string]any {
	issue := ctx.store.GetIssue(repo, issueNumber)
	if issue == nil {
		return nil
	}
	return selectFields(ctx, "Issue", map[string]any{
		"id":     globalId("I", repo.ID, issue.Number),
		"number": issue.Number,
		"title":  issue.Title,
		"state":  gqlIssueState(issue),
		"body":   issue.Body,
	}, selections)
}

// gqlPullState spells a stored PR state as a GraphQL enum: a merged PR is
// MERGED even though its stored state is "closed".
func gqlPullState(pr *Pull) string {
	if pr.Merged {
		return "MERGED"
	}
	if pr.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

// gqlMergeableWord answers the mergeable enum for a PR from a live conflict
// check, never from a stored flag.
func gqlMergeableWord(ctx *gqlCtx, repo *Repo, pr *Pull) string {
	if ctx.store.MergeConflicts(repo, pr) {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

func pullValue(ctx *gqlCtx, repo *Repo, pr *Pull, selections []*gqlField) map[string]any {
	return selectFields(ctx, "PullRequest", map[string]any{
		"id":          globalId("P", repo.ID, pr.Number),
		"number":      pr.Number,
		"headRefName": pr.HeadRef,
		"baseRefName": pr.BaseRef,
		"mergeable":   gqlMergeableWord(ctx, repo, pr),
		"state":       gqlPullState(pr),
		"merged":      pr.Merged,
		"__repo":      repo,
		"__pr":        pr,
	}, selections)
}

// ---------------------------------------------------------------------------
// Cursors and number coercion.

// cursorFor encodes an issue number as the opaque page cursor for the
// issues connection ("issue:<number>", base64url).
func cursorFor(number int) string {
	return base64urlEncode([]byte("issue:" + strconv.Itoa(number)))
}

var cursorRe = regexp.MustCompile(`^issue:(\d+)$`)

// cursorNumber decodes a page cursor back to its issue number, or nil when
// the cursor is not one this connection minted.
func cursorNumber(cursor string) *int {
	text := string(lenientBase64urlDecode(cursor))
	m := cursorRe.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	n, _ := strconv.Atoi(m[1])
	return &n
}

// jsNumberValue coerces a JSON value the way JavaScript's Number(v) does:
// null is 0, booleans are 0/1, a string parses as a number (or NaN), a
// single-element array coerces through its element, and anything else is
// NaN. The GraphQL layer needs it because argument values arrive as
// untyped JSON — a "number" argument may be a string in disguise.
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

// gqlSafeInt accepts a float as an id/number only when it is an integer
// inside the range where a float still represents it exactly.
func gqlSafeInt(f float64) (int, bool) {
	if !jsIsInteger(f) {
		return 0, false
	}
	if f > 9e15 || f < -9e15 {
		return 0, false
	}
	return int(f), true
}

// numFromValue converts a GraphQL argument the way Number(v) does, then to
// an int when it is an integer (a fractional value never matches a number).
func numFromValue(v any) (int, bool) {
	return gqlSafeInt(jsNumberValue(v))
}

// numArg converts one decoded global-id part the way Number(part) does,
// then to an int when it is an integer id.
func numArg(s string) (int, bool) {
	f, ok := jsNumber(s)
	if !ok {
		return 0, false
	}
	return gqlSafeInt(f)
}

// part returns the i-th decoded part, if present. A missing part behaves
// like JavaScript's undefined: Number(undefined) is NaN, which matches
// nothing, so callers treat a missing part as "no such object".
func (d *decodedGlobalId) part(i int) (string, bool) {
	if i < len(d.Parts) {
		return d.Parts[i], true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Field resolution.
//
// resolveField is the single dispatch point: it resolves a field's
// arguments against the request variables, then hands the field to the
// handler for its parent type. Handlers fall back to a plain map lookup for
// scalar fields that already sit on the value, and to nil for anything the
// value does not carry — a missing scalar is null, never an error.

func resolveField(ctx *gqlCtx, typeName string, value map[string]any, field *gqlField) any {
	args := make(map[string]any, len(field.args))
	for k, v := range field.args {
		args[k] = resolveVariables(v, ctx.variables)
	}

	switch typeName {
	case "Query":
		return gqlQueryField(ctx, field, args)
	case "Repository":
		return gqlRepositoryField(ctx, value, field, args)
	case "PullRequest":
		return gqlPullRequestField(ctx, value, field, args)
	case "IssueConnection":
		return gqlIssueConnectionField(ctx, value, field)
	case "PullRequestConnection":
		return gqlPullConnectionField(ctx, value, field)
	case "FileConnection":
		return gqlFileConnectionField(ctx, value, field)
	case "__Type":
		return gqlTypeField(ctx, value, field)
	default:
		if v, ok := value[field.name]; ok {
			return v
		}
		return nil
	}
}

// gqlQueryField answers the five top-level query roots. Anything else is
// an unknown field on Query.
func gqlQueryField(ctx *gqlCtx, field *gqlField, args map[string]any) any {
	switch field.name {
	case "viewer":
		return selectFields(ctx, "User", map[string]any{
			"login": viewerLogin, "id": viewerID, "name": viewerName, "email": viewerEmail,
		}, field.selections)
	case "repository":
		repo := ctx.store.GetRepo(
			jsString(firstNonNil(args["owner"], "")),
			jsString(firstNonNil(args["name"], "")),
		)
		if repo == nil {
			// Unknown repository: null, deliberately with no error.
			return nil
		}
		return selectFields(ctx, "Repository", repositoryValue(repo), field.selections)
	case "node":
		return gqlNodeField(ctx, jsString(firstNonNil(args["id"], "")), field.selections)
	case "__type":
		return gqlIntrospectedType(ctx, jsString(firstNonNil(args["name"], "")), field.selections)
	case "__schema":
		return map[string]any{"queryType": map[string]any{"name": "Query"}}
	default:
		return unknownField(ctx, "Query", field.name)
	}
}

// gqlNodeField resolves a global id to the object it names. The tag picks
// the kind — R repository, I issue, P pull request — and each leg re-checks
// that the decoded repo and number actually exist before answering.
func gqlNodeField(ctx *gqlCtx, id string, selections []*gqlField) any {
	decoded := decodeGlobalId(id)
	if decoded == nil {
		return nil
	}
	switch decoded.Tag {
	case "R":
		repo := gqlRepoFromParts(ctx, decoded, 0)
		if repo == nil {
			return nil
		}
		return selectFields(ctx, "Repository", repositoryValue(repo), selections)
	case "I":
		repo, issue := gqlIssueFromParts(ctx, decoded)
		if repo == nil || issue == nil {
			return nil
		}
		return issueValue(ctx, repo, issue.Number, selections)
	case "P":
		repo, pr := gqlPullFromParts(ctx, decoded)
		if repo == nil || pr == nil {
			return nil
		}
		return pullValue(ctx, repo, pr, selections)
	default:
		return nil
	}
}

// gqlRepoFromParts decodes the repo id at parts[i] and loads that repo.
func gqlRepoFromParts(ctx *gqlCtx, decoded *decodedGlobalId, i int) *Repo {
	part, ok := decoded.part(i)
	if !ok {
		return nil
	}
	repoID, ok := numArg(part)
	if !ok {
		return nil
	}
	return ctx.store.GetRepoById(repoID)
}

// gqlIssueFromParts decodes an "I" id (repo id, issue number) and loads both.
func gqlIssueFromParts(ctx *gqlCtx, decoded *decodedGlobalId) (*Repo, *Issue) {
	repo := gqlRepoFromParts(ctx, decoded, 0)
	if repo == nil {
		return nil, nil
	}
	part, ok := decoded.part(1)
	if !ok {
		return repo, nil
	}
	num, ok := numArg(part)
	if !ok {
		return repo, nil
	}
	return repo, ctx.store.GetIssue(repo, num)
}

// gqlPullFromParts decodes a "P" id (repo id, PR number) and loads both.
func gqlPullFromParts(ctx *gqlCtx, decoded *decodedGlobalId) (*Repo, *Pull) {
	repo := gqlRepoFromParts(ctx, decoded, 0)
	if repo == nil {
		return nil, nil
	}
	part, ok := decoded.part(1)
	if !ok {
		return repo, nil
	}
	num, ok := numArg(part)
	if !ok {
		return repo, nil
	}
	return repo, ctx.store.GetPull(repo, num)
}

// gqlIntrospectedType answers __type(name:) from the introspection table.
// An enum type reports kind ENUM and its values; an object type reports
// kind OBJECT and its fields. Unknown names answer null.
func gqlIntrospectedType(ctx *gqlCtx, name string, selections []*gqlField) any {
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
	return selectFields(ctx, "__Type", map[string]any{
		"name": name, "kind": kind,
		"__fields": fields, "__enumValues": enumValues,
	}, selections)
}

// gqlRepositoryField answers one field of a repository. The *Repo travels
// on the value map under "__repo"; scalar fields (id, name, ...) are plain
// map lookups at the bottom.
func gqlRepositoryField(ctx *gqlCtx, value map[string]any, field *gqlField, args map[string]any) any {
	repo := value["__repo"].(*Repo)
	switch field.name {
	case "owner":
		return selectFields(ctx, "User", map[string]any{
			"login": repo.Owner, "id": viewerID, "name": viewerName, "email": viewerEmail,
		}, field.selections)
	case "defaultBranchRef":
		if _, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; !ok {
			return nil
		}
		return selectFields(ctx, "Ref", map[string]any{"name": repo.DefaultBranch}, field.selections)
	case "issue":
		num, ok := numFromValue(args["number"])
		if !ok {
			return nil
		}
		return issueValue(ctx, repo, num, field.selections)
	case "issues":
		return gqlIssuesConnection(ctx, repo, field, args)
	case "pullRequest":
		num, ok := numFromValue(args["number"])
		if !ok {
			return nil
		}
		pr := ctx.store.GetPull(repo, num)
		if pr == nil {
			return nil
		}
		return pullValue(ctx, repo, pr, field.selections)
	case "pullRequests":
		return gqlPullsConnection(ctx, repo, field, args)
	default:
		if v, ok := value[field.name]; ok {
			return v
		}
		return nil
	}
}

// gqlIssuesConnection answers repository.issues as a connection: optional
// states filter (uppercase enum values compared against the stored state),
// orderBy (CREATED_AT DESC sorts newest first with ties broken by number
// DESC, ASC the mirror; anything else falls back to number ASC), cursor
// pagination via after, and a first page size that defaults to 10.
func gqlIssuesConnection(ctx *gqlCtx, repo *Repo, field *gqlField, args map[string]any) any {
	issues := append([]*Issue{}, repo.Issues...)
	if states, ok := args["states"].([]any); ok {
		issues = gqlFilterByStates(issues, states)
	}
	issues = gqlOrderIssues(issues, asRecord(args["orderBy"]))
	if after, ok := args["after"].(string); ok {
		if at := cursorNumber(after); at != nil {
			issues = gqlAfterNumber(issues, *at)
		}
	}
	first := 10.0
	if f, ok := args["first"].(float64); ok {
		first = f
	}
	lo, hi := jsSlice(len(issues), 0, first)
	page := issues[lo:hi]
	hasNext := len(issues) > int(first)
	var endCursor any
	if len(page) > 0 {
		endCursor = cursorFor(page[len(page)-1].Number)
	}
	nodes := make([]any, 0, len(page))
	for _, issue := range page {
		nodes = append(nodes, issueValue(ctx, repo, issue.Number, gqlChild(field, "nodes")))
	}
	pageInfoSel := gqlChild(field, "pageInfo")
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

// gqlFilterByStates keeps only the issues whose stored state, uppercased,
// appears in the requested states list.
func gqlFilterByStates(issues []*Issue, states []any) []*Issue {
	var kept []*Issue
	for _, issue := range issues {
		upper := strings.ToUpper(issue.State)
		for _, st := range states {
			if st == upper {
				kept = append(kept, issue)
				break
			}
		}
	}
	return kept
}

// gqlOrderIssues sorts a copy of the issue list per orderBy. CREATED_AT
// orders by creation sequence (ties by number, same direction); any other
// field orders by number ascending.
func gqlOrderIssues(issues []*Issue, orderBy map[string]any) []*Issue {
	field, _ := orderBy["field"].(string)
	direction, _ := orderBy["direction"].(string)
	switch {
	case field == "CREATED_AT" && direction == "DESC":
		sort.SliceStable(issues, func(a, b int) bool {
			if issues[a].CreatedSeq != issues[b].CreatedSeq {
				return issues[a].CreatedSeq > issues[b].CreatedSeq
			}
			return issues[a].Number > issues[b].Number
		})
	case field == "CREATED_AT" && direction == "ASC":
		sort.SliceStable(issues, func(a, b int) bool {
			if issues[a].CreatedSeq != issues[b].CreatedSeq {
				return issues[a].CreatedSeq < issues[b].CreatedSeq
			}
			return issues[a].Number < issues[b].Number
		})
	default:
		sort.SliceStable(issues, func(a, b int) bool { return issues[a].Number < issues[b].Number })
	}
	return issues
}

// gqlAfterNumber drops every element up to and including the one with the
// given number; a number that is not in the list drops nothing.
func gqlAfterNumber(issues []*Issue, number int) []*Issue {
	for i, issue := range issues {
		if issue.Number == number {
			return issues[i+1:]
		}
	}
	return issues
}

// gqlPullsConnection answers repository.pullRequests: number ascending, a
// first page size defaulting to 10, totalCount over all of the repo's PRs.
func gqlPullsConnection(ctx *gqlCtx, repo *Repo, field *gqlField, args map[string]any) any {
	first := 10.0
	if f, ok := args["first"].(float64); ok {
		first = f
	}
	pulls := append([]*Pull{}, repo.Pulls...)
	sort.SliceStable(pulls, func(a, b int) bool { return pulls[a].Number < pulls[b].Number })
	lo, hi := jsSlice(len(pulls), 0, first)
	page := pulls[lo:hi]
	nodes := make([]any, 0, len(page))
	for _, pr := range page {
		nodes = append(nodes, pullValue(ctx, repo, pr, gqlChild(field, "nodes")))
	}
	return selectFields(ctx, "PullRequestConnection", map[string]any{
		"__nodes": nodes, "nodes": nodes, "totalCount": len(repo.Pulls),
	}, field.selections)
}

// gqlPullRequestField answers the files connection of a PR (its merge-base
// diff, first 10 by default) and plain map lookups for its scalars.
func gqlPullRequestField(ctx *gqlCtx, value map[string]any, field *gqlField, args map[string]any) any {
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
		nodes := make([]any, 0, len(page))
		for _, f := range page {
			nodes = append(nodes, selectFields(ctx, "File", map[string]any{"path": f.Path}, gqlChild(field, "nodes")))
		}
		return selectFields(ctx, "FileConnection", map[string]any{"nodes": nodes}, field.selections)
	}
	if v, ok := value[field.name]; ok {
		return v
	}
	return nil
}

// gqlIssueConnectionField answers the nodes/pageInfo/totalCount children of
// an already-built issues connection.
func gqlIssueConnectionField(ctx *gqlCtx, value map[string]any, field *gqlField) any {
	switch field.name {
	case "nodes":
		if v, ok := value["__nodes"]; ok {
			return v
		}
		return []any{}
	case "pageInfo":
		info := value["__pageInfo"].(map[string]any)
		return selectFields(ctx, "PageInfo", info, field.selections)
	default:
		if v, ok := value[field.name]; ok {
			return v
		}
		return nil
	}
}

// gqlPullConnectionField answers the children of an already-built PR
// connection; pageInfo is not served here because the suite never asks
// a pull-requests connection for one.
func gqlPullConnectionField(ctx *gqlCtx, value map[string]any, field *gqlField) any {
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
}

// gqlFileConnectionField answers the nodes of a files connection.
func gqlFileConnectionField(ctx *gqlCtx, value map[string]any, field *gqlField) any {
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
}

// gqlTypeField expands the __Type children: fields and enumValues are
// stored as name lists and projected through their one-field types.
func gqlTypeField(ctx *gqlCtx, value map[string]any, field *gqlField) any {
	switch field.name {
	case "fields":
		names := value["__fields"].([]string)
		out := make([]any, 0, len(names))
		for _, n := range names {
			out = append(out, selectFields(ctx, "__Field", map[string]any{"name": n}, field.selections))
		}
		return out
	case "enumValues":
		names := value["__enumValues"].([]string)
		out := make([]any, 0, len(names))
		for _, n := range names {
			out = append(out, selectFields(ctx, "__EnumValue", map[string]any{"name": n}, field.selections))
		}
		return out
	default:
		if v, ok := value[field.name]; ok {
			return v
		}
		return nil
	}
}

// firstNonNil mirrors `v ?? fallback` for a possibly-missing argument: a
// nil (or absent) value takes the fallback, anything else stands.
func firstNonNil(v any, fallback any) any {
	if v == nil {
		return fallback
	}
	return v
}

// repositoryValue builds the value map for a repository: its scalar fields
// plus the *Repo carrier its sub-fields resolve against.
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

// ---------------------------------------------------------------------------
// Mutations.
//
// Each mutation reads its `input` object, decodes the global id(s) in it,
// performs the store operation, and answers with the payload the document
// selected (issue / pullRequest / commentEdge). A mutation that cannot find
// its target records an error and answers null for the whole payload — the
// same shape the reference implementation produces, so clients see
// "repository not found" / "issue not found" / "pull request not found" /
// "subject not found" exactly as before.

func executeMutation(ctx *gqlCtx, field *gqlField) map[string]any {
	args := make(map[string]any, len(field.args))
	for k, v := range field.args {
		args[k] = resolveVariables(v, ctx.variables)
	}
	input := asRecord(args["input"])
	key := fieldKey(field)

	switch field.name {
	case "createIssue":
		return gqlCreateIssue(ctx, input, field, key)
	case "closeIssue":
		return gqlCloseIssue(ctx, input, field, key)
	case "createPullRequest":
		return gqlCreatePullRequest(ctx, input, field, key)
	case "mergePullRequest":
		return gqlMergePullRequest(ctx, input, field, key)
	case "addComment":
		return gqlAddComment(ctx, input, field, key)
	default:
		ctx.errors = append(ctx.errors, map[string]any{"message": fmt.Sprintf("Cannot query field \"%s\" on type \"Mutation\".", field.name)})
		return map[string]any{key: nil}
	}
}

// gqlRepoFromGlobalId decodes a repository global id ("R:<dbId>") and loads
// the repo, or nil when the id is malformed or the repo does not exist.
func gqlRepoFromGlobalId(ctx *gqlCtx, raw string) *Repo {
	decoded := decodeGlobalId(raw)
	if decoded == nil || decoded.Tag != "R" {
		return nil
	}
	return gqlRepoFromParts(ctx, decoded, 0)
}

// gqlCreateIssue handles createIssue(input: {repositoryId, title, body}).
func gqlCreateIssue(ctx *gqlCtx, input map[string]any, field *gqlField, key string) map[string]any {
	repo := gqlRepoFromGlobalId(ctx, jsString(firstNonNil(input["repositoryId"], "")))
	if repo == nil {
		ctx.errors = append(ctx.errors, map[string]any{"message": "repository not found"})
		return map[string]any{key: nil}
	}
	issue := ctx.store.CreateIssue(repo,
		jsString(firstNonNil(input["title"], "")),
		jsString(firstNonNil(input["body"], "")),
		[]string{},
	)
	return map[string]any{key: map[string]any{
		"issue": issueValue(ctx, repo, issue.Number, gqlChild(field, "issue")),
	}}
}

// gqlCloseIssue handles closeIssue(input: {issueId}). Closing bumps the
// issue's update sequence and clears its cached REST rendering, so the
// next REST read sees the closed state.
func gqlCloseIssue(ctx *gqlCtx, input map[string]any, field *gqlField, key string) map[string]any {
	decoded := decodeGlobalId(jsString(firstNonNil(input["issueId"], "")))
	var repo *Repo
	var issue *Issue
	if decoded != nil && decoded.Tag == "I" {
		repo, issue = gqlIssueFromParts(ctx, decoded)
	}
	if repo == nil || issue == nil {
		ctx.errors = append(ctx.errors, map[string]any{"message": "issue not found"})
		return map[string]any{key: nil}
	}
	issue.State = "closed"
	issue.UpdatedSeq = ctx.store.nextSeq()
	issue.jsonCache = nil
	return map[string]any{key: map[string]any{
		"issue": issueValue(ctx, repo, issue.Number, gqlChild(field, "issue")),
	}}
}

// gqlCreatePullRequest handles createPullRequest(input: {repositoryId,
// title, body, headRefName, baseRefName}). An open PR already covering
// the same head/base pair is returned instead of opening a duplicate —
// the head branch was pushed before the mutation ran, so the branch must
// exist; a missing head or base is an error, not an empty PR.
func gqlCreatePullRequest(ctx *gqlCtx, input map[string]any, field *gqlField, key string) map[string]any {
	repo := gqlRepoFromGlobalId(ctx, jsString(firstNonNil(input["repositoryId"], "")))
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
		pr = ctx.store.CreatePull(repo,
			jsString(firstNonNil(input["title"], "")),
			jsString(firstNonNil(input["body"], "")),
			head, base,
		)
	}
	if pr == nil {
		ctx.errors = append(ctx.errors, map[string]any{"message": "head or base branch does not exist"})
		return map[string]any{key: nil}
	}
	return map[string]any{key: map[string]any{
		"pullRequest": pullValue(ctx, repo, pr, gqlChild(field, "pullRequest")),
	}}
}

// gqlMergePullRequest handles mergePullRequest(input: {pullRequestId}).
// An open PR is squash-merged; the payload reports the PR as it stands
// afterwards either way (a conflicted merge leaves it open).
func gqlMergePullRequest(ctx *gqlCtx, input map[string]any, field *gqlField, key string) map[string]any {
	decoded := decodeGlobalId(jsString(firstNonNil(input["pullRequestId"], "")))
	var repo *Repo
	var pr *Pull
	if decoded != nil && decoded.Tag == "P" {
		repo, pr = gqlPullFromParts(ctx, decoded)
	}
	if repo == nil || pr == nil {
		ctx.errors = append(ctx.errors, map[string]any{"message": "pull request not found"})
		return map[string]any{key: nil}
	}
	if pr.State == "open" {
		ctx.store.MergePull(repo, pr)
	}
	return map[string]any{key: map[string]any{
		"pullRequest": pullValue(ctx, repo, pr, gqlChild(field, "pullRequest")),
	}}
}

// gqlAddComment handles addComment(input: {subjectId, body}). The subject
// is an issue global id; the answer nests the new comment under
// commentEdge.node with its own opaque id ("C:<repo>:<issue>:<comment>").
func gqlAddComment(ctx *gqlCtx, input map[string]any, field *gqlField, key string) map[string]any {
	decoded := decodeGlobalId(jsString(firstNonNil(input["subjectId"], "")))
	var repo *Repo
	var issue *Issue
	if decoded != nil && decoded.Tag == "I" {
		repo, issue = gqlIssueFromParts(ctx, decoded)
	}
	if repo == nil || issue == nil {
		ctx.errors = append(ctx.errors, map[string]any{"message": "subject not found"})
		return map[string]any{key: nil}
	}
	comment := ctx.store.AddComment(repo, issue, jsString(firstNonNil(input["body"], "")))
	var nodeSel []*gqlField
	for _, edge := range field.selections {
		if edge.name == "commentEdge" {
			nodeSel = gqlChild(edge, "node")
		}
	}
	return map[string]any{key: map[string]any{
		"commentEdge": map[string]any{
			"node": selectFields(ctx, "Comment", map[string]any{
				"id":   globalId("C", repo.ID, issue.Number, comment.ID),
				"body": comment.Body,
			}, nodeSel),
		},
	}}
}

// ---------------------------------------------------------------------------
// Operation execution: one parsed document becomes one response object.

// executeOperation tokenizes, parses, and runs a single GraphQL document.
// A document that does not parse answers data null with the parse error;
// otherwise data holds one entry per top-level field, and errors appears
// only when at least one field recorded one.
func executeOperation(store *Store, query string, variables map[string]any) map[string]any {
	ctx := &gqlCtx{store: store, variables: variables}
	parser := &gqlParser{tokens: tokenize(query)}
	operation, err := parser.parseOperation()
	if err != nil {
		return map[string]any{"data": nil, "errors": []map[string]any{{"message": err.Error()}}}
	}
	data := make(map[string]any, len(operation.selections))
	if operation.kind == "mutation" {
		for _, field := range operation.selections {
			for k, v := range executeMutation(ctx, field) {
				data[k] = v
			}
		}
	} else {
		for _, field := range operation.selections {
			key := fieldKey(field)
			switch field.name {
			case "viewer", "repository", "node", "__type", "__schema":
				data[key] = resolveField(ctx, "Query", map[string]any{}, field)
			default:
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

// ---------------------------------------------------------------------------
// HTTP: POST /api/graphql.
//
// Auth runs before the body is read: without a valid bearer token the
// answer is exactly 401 {"message":"Bad credentials"}. A JSON array body
// is a batch — one result per entry, in request order. Anything else is a
// single operation object.

// handleGraphQL serves POST /api/graphql. It returns true when the path is
// this endpoint (even when the method or body is wrong); false lets the
// dispatcher try the next handler.
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
	if batch, ok := parsed.([]any); ok {
		results := make([]any, 0, len(batch))
		for _, entry := range batch {
			op := asRecord(entry)
			query, _ := op["query"].(string)
			results = append(results, executeOperation(s.store, query, asRecord(op["variables"])))
		}
		s.sendJsonPlain(w, 200, results)
		return true
	}
	op := asRecord(parsed)
	query, _ := op["query"].(string)
	s.sendJsonPlain(w, 200, executeOperation(s.store, query, asRecord(op["variables"])))
	return true
}
