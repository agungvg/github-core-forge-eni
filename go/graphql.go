// graphql.go — the GraphQL v4 endpoint at POST /api/graphql.
//
// This is a small executor for exactly the documents the trial clients
// send, not a general GraphQL engine: viewer and repository reads, issue
// and pull-request connections, Relay node() lookups, __type probes, the
// five mutations, and batched request arrays. Anything outside that shape
// still gets a spec-shaped answer: an unknown field is HTTP 200 with an
// errors entry naming the field, an unknown repository is data.repository
// null with no errors, and a missing bearer token is exactly 401.
//
// Two wire rules run through the whole file (see docs/DECISIONS.md D5 and
// D13): global ids are opaque base64url blobs over a one-letter tag plus
// ids, decodable again for node() and mutations without a lookup table,
// and this layer spells states in capitals — OPEN, CLOSED, MERGED,
// MERGEABLE, CONFLICTING — where the REST layer spells them lowercase.
// Both layers read the same stored state, so they cannot drift apart.
//
// Everything is in memory: the lexer works on the raw document text, the
// parser builds one small selection tree, and the resolvers walk the
// store's slices directly. No per-request disk I/O anywhere.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Lexer: a GraphQL document becomes a flat list of small tokens.
//
// String literals are decoded at scan time (json.Unmarshal of the raw
// literal, so escapes come out right) and carried as plain Go strings.
// Everything else is a bare word, a number, or one punctuation byte.

type tokKind int

const (
	tkPunct tokKind = iota
	tkName
	tkString
	tkNumber
	tkSpread
	tkDollar
)

type token struct {
	kind tokKind
	text string
}

func lexDoc(src string) []token {
	out := []token{}
	i := 0
	for i < len(src) {
		ch := src[i]
		switch {
		case isJSSpace(rune(ch)):
			i++
		case ch == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case ch == '.' && strings.HasPrefix(src[i:], "..."):
			out = append(out, token{kind: tkSpread, text: "..."})
			i += 3
		case ch == '"':
			text, next := scanQuoted(src, i)
			out = append(out, token{kind: tkString, text: text})
			i = next
		case ch == '$':
			out = append(out, token{kind: tkDollar, text: "$"})
			i++
		case strings.IndexByte("{}():,=![]", ch) >= 0:
			out = append(out, token{kind: tkPunct, text: string(ch)})
			i++
		case ch == '_' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z':
			j := i + 1
			for j < len(src) && isNameByte(src[j]) {
				j++
			}
			out = append(out, token{kind: tkName, text: src[i:j]})
			i = j
		case ch >= '0' && ch <= '9' || ch == '-' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9':
			j := i + 1
			for j < len(src) && isNumberByte(src[j]) {
				j++
			}
			out = append(out, token{kind: tkNumber, text: src[i:j]})
			i = j
		default:
			// Any other byte is not part of the grammar; drop it.
			i++
		}
	}
	return out
}

// isNameByte reports whether b can continue a GraphQL name.
func isNameByte(b byte) bool {
	return b == '_' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}

// isNumberByte reports whether b can continue a numeric literal.
func isNumberByte(b byte) bool {
	return b >= '0' && b <= '9' || b == '.' || b == 'e' || b == 'E' || b == '+' || b == '-'
}

// scanQuoted reads one double-quoted literal starting at src[start] and
// returns its decoded value plus the index just past the closing quote.
// A backslash escapes the next byte, so the raw slice is valid JSON text
// for json.Unmarshal; on a malformed literal the inner bytes are used raw.
func scanQuoted(src string, start int) (string, int) {
	j := start + 1
	for j < len(src) && src[j] != '"' {
		if src[j] == '\\' && j+1 < len(src) {
			j += 2
			continue
		}
		j++
	}
	end := j + 1
	if end > len(src) {
		end = len(src)
	}
	var decoded string
	if err := json.Unmarshal([]byte(src[start:end]), &decoded); err == nil {
		return decoded, end
	}
	inner := src[start+1 : end-1]
	if end-1 <= start+1 {
		inner = ""
	}
	return inner, end
}

// ---------------------------------------------------------------------------
// Parser: the token list becomes one operation with a selection tree.
//
// A $name inside a parsed value is kept as a varRef and resolved against
// the request's variables map only when the field runs, so the same parsed
// document could be reused across requests.

type varRef struct{ name string }

// sel is one field in a selection set. alias holds the response key when
// the document wrote one ("Issue: __type(...)"); cond marks fields that
// came from an inline fragment ("... on Repository"), which only apply
// when the value's type matches.
type sel struct {
	alias   string
	name    string
	args    map[string]any
	kids    []*sel
	cond    string
	hasCond bool
}

type docOp struct {
	mutation bool
	fields   []*sel
}

type docParser struct {
	toks []token
	pos  int
}

func (p *docParser) peek() (token, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return token{}, false
}

func (p *docParser) next() (token, bool) {
	if p.pos < len(p.toks) {
		t := p.toks[p.pos]
		p.pos++
		return t, true
	}
	return token{}, false
}

func (p *docParser) expectPunct(want string) error {
	got, ok := p.next()
	if !ok || got.kind != tkPunct || got.text != want {
		seen := "end of document"
		if ok {
			seen = got.text
		}
		return fmt.Errorf("expected %s, got %s", want, seen)
	}
	return nil
}

// skipBalanced consumes a parenthesised group — the variable definitions
// after an operation name, which this executor never interprets because
// variables arrive in the request body. Braces nest too, so a default
// object value cannot end the skip early.
func (p *docParser) skipBalanced() {
	depth := 0
	for p.pos < len(p.toks) {
		t, _ := p.next()
		if t.kind != tkPunct {
			continue
		}
		switch t.text {
		case "(", "{":
			depth++
		case ")", "}":
			depth--
			if depth == 0 {
				return
			}
		}
	}
}

func parseDoc(toks []token) (*docOp, error) {
	p := &docParser{toks: toks}
	mut := false
	if t, ok := p.peek(); ok && t.kind == tkName && (t.text == "query" || t.text == "mutation") {
		p.next()
		mut = t.text == "mutation"
	}
	// An operation name, when present, is just a label.
	if t, ok := p.peek(); ok && t.kind == tkName {
		p.next()
	}
	if t, ok := p.peek(); ok && t.kind == tkPunct && t.text == "(" {
		p.skipBalanced()
	}
	fields, err := p.parseSelSet()
	if err != nil {
		return nil, err
	}
	return &docOp{mutation: mut, fields: fields}, nil
}

func (p *docParser) parseSelSet() ([]*sel, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	fields := []*sel{}
	for {
		head, ok := p.peek()
		if !ok || head.kind == tkPunct && head.text == "}" {
			break
		}
		parsed, err := p.parseSel()
		if err != nil {
			return nil, err
		}
		fields = append(fields, parsed...)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return fields, nil
}

func (p *docParser) parseSel() ([]*sel, error) {
	head, ok := p.peek()
	if !ok {
		return nil, fmt.Errorf("unexpected end of document")
	}
	if head.kind == tkSpread {
		p.next()
		if nt, ok := p.peek(); ok && nt.kind == tkName && nt.text == "on" {
			p.next()
			tn, _ := p.next()
			inner, err := p.parseSelSet()
			if err != nil {
				return nil, err
			}
			for _, f := range inner {
				f.cond = tn.text
				f.hasCond = true
			}
			return inner, nil
		}
		// A named spread is not used by any document in the suite; consume
		// its name and contribute nothing.
		p.next()
		return nil, nil
	}
	first, _ := p.next()
	f := &sel{name: first.text, args: map[string]any{}}
	if nt, ok := p.peek(); ok && nt.kind == tkPunct && nt.text == ":" {
		p.next()
		real, _ := p.next()
		f.alias = first.text
		f.name = real.text
	}
	if nt, ok := p.peek(); ok && nt.kind == tkPunct && nt.text == "(" {
		p.next()
		args, err := p.parseArgs()
		if err != nil {
			return nil, err
		}
		f.args = args
	}
	if nt, ok := p.peek(); ok && nt.kind == tkPunct && nt.text == "{" {
		kids, err := p.parseSelSet()
		if err != nil {
			return nil, err
		}
		f.kids = kids
	}
	return []*sel{f}, nil
}

func (p *docParser) parseArgs() (map[string]any, error) {
	args := map[string]any{}
	for {
		head, ok := p.peek()
		if !ok || head.kind == tkPunct && head.text == ")" {
			break
		}
		name, _ := p.next()
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[name.text] = v
		if head, ok := p.peek(); ok && head.kind == tkPunct && head.text == "," {
			p.next()
		}
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *docParser) parseValue() (any, error) {
	tok, ok := p.next()
	if !ok {
		return nil, fmt.Errorf("unexpected end of document")
	}
	switch tok.kind {
	case tkDollar:
		name, _ := p.next()
		return varRef{name: name.text}, nil
	case tkString:
		return tok.text, nil
	case tkNumber:
		f, err := strconv.ParseFloat(tok.text, 64)
		if err != nil {
			return nil, err
		}
		return f, nil
	case tkPunct:
		switch tok.text {
		case "[":
			return p.parseList()
		case "{":
			return p.parseObject()
		}
		return nil, fmt.Errorf("unexpected %s in value position", tok.text)
	default: // tkName: true, false, null, or a bare enum word
		switch tok.text {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return tok.text, nil
		}
	}
}

func (p *docParser) parseList() (any, error) {
	items := []any{}
	for {
		head, ok := p.peek()
		if !ok || head.kind == tkPunct && head.text == "]" {
			break
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		items = append(items, v)
		if head, ok := p.peek(); ok && head.kind == tkPunct && head.text == "," {
			p.next()
		}
	}
	if err := p.expectPunct("]"); err != nil {
		return nil, err
	}
	return items, nil
}

func (p *docParser) parseObject() (any, error) {
	obj := map[string]any{}
	for {
		head, ok := p.peek()
		if !ok || head.kind == tkPunct && head.text == "}" {
			break
		}
		key, _ := p.next()
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		obj[key.text] = v
		if head, ok := p.peek(); ok && head.kind == tkPunct && head.text == "," {
			p.next()
		}
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return obj, nil
}

// ---------------------------------------------------------------------------
// Argument values: variable substitution and coercion.

// substVars walks a parsed value and substitutes every $name with the
// matching request variable (a missing variable becomes nil). Lists and
// objects are rebuilt so the parsed document is never mutated.
func substVars(value any, vars map[string]any) any {
	switch v := value.(type) {
	case varRef:
		if val, ok := vars[v.name]; ok {
			return val
		}
		return nil
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = substVars(e, vars)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[k] = substVars(e, vars)
		}
		return out
	default:
		return value
	}
}

// resolvedArgs resolves every argument of a field against the request
// variables and returns the plain map the resolvers read.
func resolvedArgs(f *sel, vars map[string]any) map[string]any {
	out := make(map[string]any, len(f.args))
	for k, v := range f.args {
		out[k] = substVars(v, vars)
	}
	return out
}

// argStr reads one argument the way String(v) does, with a missing or nil
// argument answering "" instead of the word "undefined".
func argStr(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	return jsString(v)
}

// numFromAny converts an argument value to an int the way Number(v) does
// first: a float is accepted only when it is an integer inside the range
// where a float still represents it exactly; a string parses as a number.
// Anything else, including a fractional value, matches no number.
func numFromAny(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		if !jsIsInteger(t) || t > 9e15 || t < -9e15 {
			return 0, false
		}
		return int(t), true
	case string:
		f, ok := jsNumber(t)
		if !ok || !jsIsInteger(f) || f > 9e15 || f < -9e15 {
			return 0, false
		}
		return int(f), true
	default:
		return 0, false
	}
}

// ---------------------------------------------------------------------------
// Schema: the field names each type may answer, plus the small slice of
// introspection the clients actually ask for.
//
// The tables are the allow-list: a field that is not listed for its type
// is an error, never a silent null.

func fieldSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

var allowedFields = map[string]map[string]bool{
	"Query":                 fieldSet("viewer", "repository", "node", "__type", "__schema"),
	"User":                  fieldSet("login", "id", "name", "email"),
	"Repository":            fieldSet("id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequest", "pullRequests"),
	"Issue":                 fieldSet("id", "number", "title", "state", "body"),
	"PullRequest":           fieldSet("id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"),
	"IssueConnection":       fieldSet("nodes", "pageInfo", "totalCount"),
	"PullRequestConnection": fieldSet("nodes", "pageInfo", "totalCount"),
	"FileConnection":        fieldSet("nodes"),
	"PageInfo":              fieldSet("hasNextPage", "endCursor"),
	"Ref":                   fieldSet("name"),
	"CommentEdge":           fieldSet("node"),
	"Comment":               fieldSet("id", "body"),
	"File":                  fieldSet("path"),
	"Type":                  fieldSet("name", "kind", "fields", "enumValues"),
	"Field":                 fieldSet("name"),
	"EnumValue":             fieldSet("name"),
}

type typeInfo struct {
	fields     []string
	enumValues []string
}

// typeCatalog answers __type(name:) for the types the suite probes. Only
// SearchType carries enum values; the object types carry field lists.
var typeCatalog = map[string]typeInfo{
	"Issue":       {fields: []string{"id", "number", "title", "state", "body"}},
	"PullRequest": {fields: []string{"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"}},
	"Repository":  {fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"}},
	"SearchType":  {enumValues: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION"}},
}

// ---------------------------------------------------------------------------
// Execution context and the node model.
//
// A gqlNode is one value the resolvers hand around: its type name, its
// scalar fields, and — for the types that need live store access — the
// store object it was resolved from plus, for connections, its children
// and pageInfo. Projection never sees the store pointers; they travel
// alongside the scalars and are only read by resolveField.

type gqlEnv struct {
	store *Store
	vars  map[string]any
	errs  []any
}

func (e *gqlEnv) failField(typ, field string) {
	e.errs = append(e.errs, map[string]any{"message": fmt.Sprintf("Cannot query field %q on type %q.", field, typ)})
}

type gqlNode struct {
	typ        string
	vals       map[string]any
	repo       *Repo
	issue      *Issue
	pr         *Pull
	kids       []any // connection children, each a *gqlNode
	info       *gqlNode
	fieldNames []string // __Type.fields source list
	enumNames  []string // __Type.enumValues source list
}

func userNode(login string, id int) *gqlNode {
	return &gqlNode{typ: "User", vals: map[string]any{
		"login": login, "id": id, "name": viewerName, "email": viewerEmail,
	}}
}

func repoNode(repo *Repo) *gqlNode {
	return &gqlNode{
		typ:  "Repository",
		repo: repo,
		vals: map[string]any{
			"id":               globalId("R", repo.ID),
			"databaseId":       repo.ID,
			"name":             repo.Name,
			"nameWithOwner":    repo.Owner + "/" + repo.Name,
			"viewerPermission": "ADMIN",
			"hasIssuesEnabled": true,
		},
	}
}

func issueStateWord(issue *Issue) string {
	if issue.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func (e *gqlEnv) issueNode(repo *Repo, issue *Issue) *gqlNode {
	return &gqlNode{
		typ:   "Issue",
		repo:  repo,
		issue: issue,
		vals: map[string]any{
			"id":     globalId("I", repo.ID, issue.Number),
			"number": issue.Number,
			"title":  issue.Title,
			"state":  issueStateWord(issue),
			"body":   issue.Body,
		},
	}
}

func pullStateWord(pr *Pull) string {
	if pr.Merged {
		return "MERGED"
	}
	if pr.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func (e *gqlEnv) prNode(repo *Repo, pr *Pull) *gqlNode {
	mergeable := "MERGEABLE"
	if e.store.MergeConflicts(repo, pr) {
		mergeable = "CONFLICTING"
	}
	return &gqlNode{
		typ:  "PullRequest",
		repo: repo,
		pr:   pr,
		vals: map[string]any{
			"id":          globalId("P", repo.ID, pr.Number),
			"number":      pr.Number,
			"headRefName": pr.HeadRef,
			"baseRefName": pr.BaseRef,
			"mergeable":   mergeable,
			"state":       pullStateWord(pr),
			"merged":      pr.Merged,
		},
	}
}

// ---------------------------------------------------------------------------
// Projection: one node through one selection set.
//
// A nil node stays nil (a missing object has no fields to pick), and a nil
// selection set returns the scalar map whole. Every selected field is
// checked against the type's allow-list first; an inline-fragment field
// whose type condition does not match the node is skipped outright.

func (e *gqlEnv) project(n *gqlNode, sels []*sel) map[string]any {
	if n == nil {
		return nil
	}
	if sels == nil {
		out := make(map[string]any, len(n.vals))
		for k, v := range n.vals {
			out[k] = v
		}
		return out
	}
	out := make(map[string]any, len(sels))
	allowed := allowedFields[n.typ]
	for _, s := range sels {
		if s.hasCond && s.cond != n.typ {
			continue
		}
		key := s.name
		if s.alias != "" {
			key = s.alias
		}
		if !allowed[s.name] {
			e.failField(n.typ, s.name)
			out[key] = nil
			continue
		}
		out[key] = e.render(e.resolveField(n, s), s.kids)
	}
	return out
}

// render turns a resolved value into its response shape: a node is
// projected through the field's sub-selections, a list is rendered
// element by element, and anything else passes through untouched.
func (e *gqlEnv) render(v any, kids []*sel) any {
	switch t := v.(type) {
	case *gqlNode:
		if t == nil {
			return nil
		}
		return e.project(t, kids)
	case []any:
		out := make([]any, len(t))
		for i, el := range t {
			out[i] = e.render(el, kids)
		}
		return out
	default:
		return v
	}
}

// resolveField is the single dispatch point for a field of an already
// resolved node: arguments are resolved first, then the field is answered
// from the node's store object, its children, or its scalar map.
func (e *gqlEnv) resolveField(n *gqlNode, f *sel) any {
	args := resolvedArgs(f, e.vars)
	switch n.typ {
	case "Repository":
		return e.repositoryField(n, f, args)
	case "PullRequest":
		if f.name == "files" {
			return e.filesConn(n.repo, n.pr, args)
		}
	case "IssueConnection", "PullRequestConnection":
		switch f.name {
		case "nodes":
			return n.kids
		case "pageInfo":
			return n.info
		}
	case "FileConnection":
		if f.name == "nodes" {
			return n.kids
		}
	case "Type":
		switch f.name {
		case "fields":
			out := make([]any, 0, len(n.fieldNames))
			for _, name := range n.fieldNames {
				out = append(out, &gqlNode{typ: "Field", vals: map[string]any{"name": name}})
			}
			return out
		case "enumValues":
			out := make([]any, 0, len(n.enumNames))
			for _, name := range n.enumNames {
				out = append(out, &gqlNode{typ: "EnumValue", vals: map[string]any{"name": name}})
			}
			return out
		}
	}
	if v, ok := n.vals[f.name]; ok {
		return v
	}
	return nil
}

func (e *gqlEnv) repositoryField(n *gqlNode, f *sel, args map[string]any) any {
	repo := n.repo
	switch f.name {
	case "owner":
		return userNode(repo.Owner, viewerID)
	case "defaultBranchRef":
		if _, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; !ok {
			return nil
		}
		return &gqlNode{typ: "Ref", vals: map[string]any{"name": repo.DefaultBranch}}
	case "issue":
		num, ok := numFromAny(args["number"])
		if !ok {
			return nil
		}
		issue := e.store.GetIssue(repo, num)
		if issue == nil {
			return nil
		}
		return e.issueNode(repo, issue)
	case "issues":
		return e.issuesConn(repo, args)
	case "pullRequest":
		num, ok := numFromAny(args["number"])
		if !ok {
			return nil
		}
		pr := e.store.GetPull(repo, num)
		if pr == nil {
			return nil
		}
		return e.prNode(repo, pr)
	case "pullRequests":
		return e.prsConn(repo, args)
	}
	if v, ok := n.vals[f.name]; ok {
		return v
	}
	return nil
}

// ---------------------------------------------------------------------------
// Cursors and connections.

// encodeCursor renders an issue number as the opaque page cursor for the
// issues connection ("issue:<number>", base64url).
func encodeCursor(number int) string {
	return base64urlEncode([]byte("issue:" + strconv.Itoa(number)))
}

// decodeCursor decodes a page cursor back to its issue number, or nil
// when the cursor is not one this connection minted.
func decodeCursor(cursor string) *int {
	raw := string(lenientBase64urlDecode(cursor))
	if !strings.HasPrefix(raw, "issue:") {
		return nil
	}
	n, err := strconv.Atoi(raw[len("issue:"):])
	if err != nil {
		return nil
	}
	return &n
}

// issuesConn answers repository.issues as a connection: an optional states
// filter (uppercase enum values against the stored state), orderBy
// (CREATED_AT orders by creation sequence with ties broken by number, any
// other field falls back to number ascending), cursor pagination via
// after, and a first page size that defaults to 10.
func (e *gqlEnv) issuesConn(repo *Repo, args map[string]any) *gqlNode {
	list := append([]*Issue{}, repo.Issues...)
	if raw, ok := args["states"].([]any); ok {
		kept := make([]*Issue, 0, len(list))
		for _, issue := range list {
			for _, st := range raw {
				if s, ok := st.(string); ok && s == strings.ToUpper(issue.State) {
					kept = append(kept, issue)
					break
				}
			}
		}
		list = kept
	}
	orderBy := asRecord(args["orderBy"])
	field, _ := orderBy["field"].(string)
	direction, _ := orderBy["direction"].(string)
	switch {
	case field == "CREATED_AT" && direction == "DESC":
		sort.SliceStable(list, func(a, b int) bool {
			if list[a].CreatedSeq != list[b].CreatedSeq {
				return list[a].CreatedSeq > list[b].CreatedSeq
			}
			return list[a].Number > list[b].Number
		})
	case field == "CREATED_AT" && direction == "ASC":
		sort.SliceStable(list, func(a, b int) bool {
			if list[a].CreatedSeq != list[b].CreatedSeq {
				return list[a].CreatedSeq < list[b].CreatedSeq
			}
			return list[a].Number < list[b].Number
		})
	default:
		sort.SliceStable(list, func(a, b int) bool { return list[a].Number < list[b].Number })
	}
	if cur, ok := args["after"].(string); ok {
		if at := decodeCursor(cur); at != nil {
			for i, issue := range list {
				if issue.Number == *at {
					list = list[i+1:]
					break
				}
			}
		}
	}
	first := 10.0
	if f, ok := args["first"].(float64); ok {
		first = f
	}
	lo, hi := jsSlice(len(list), 0, first)
	page := list[lo:hi]
	kids := make([]any, 0, len(page))
	for _, issue := range page {
		kids = append(kids, e.issueNode(repo, issue))
	}
	var endCursor any
	if len(page) > 0 {
		endCursor = encodeCursor(page[len(page)-1].Number)
	}
	node := &gqlNode{
		typ:  "IssueConnection",
		vals: map[string]any{"totalCount": len(list)},
		kids: kids,
	}
	node.info = &gqlNode{typ: "PageInfo", vals: map[string]any{
		"hasNextPage": hi < len(list),
		"endCursor":   endCursor,
	}}
	return node
}

// prsConn answers repository.pullRequests: number ascending, a first page
// size defaulting to 10, totalCount over all of the repo's PRs.
func (e *gqlEnv) prsConn(repo *Repo, args map[string]any) *gqlNode {
	first := 10.0
	if f, ok := args["first"].(float64); ok {
		first = f
	}
	pulls := append([]*Pull{}, repo.Pulls...)
	sort.SliceStable(pulls, func(a, b int) bool { return pulls[a].Number < pulls[b].Number })
	lo, hi := jsSlice(len(pulls), 0, first)
	page := pulls[lo:hi]
	kids := make([]any, 0, len(page))
	for _, pr := range page {
		kids = append(kids, e.prNode(repo, pr))
	}
	return &gqlNode{
		typ:  "PullRequestConnection",
		vals: map[string]any{"totalCount": len(repo.Pulls)},
		kids: kids,
	}
}

// filesConn answers a pull request's files connection: its merge-base
// diff, first 10 by default, each node carrying just the changed path.
func (e *gqlEnv) filesConn(repo *Repo, pr *Pull, args map[string]any) *gqlNode {
	first := 10.0
	if f, ok := args["first"].(float64); ok {
		first = f
	}
	changes := e.store.PullFiles(repo, pr)
	lo, hi := jsSlice(len(changes), 0, first)
	page := changes[lo:hi]
	kids := make([]any, 0, len(page))
	for _, ch := range page {
		kids = append(kids, &gqlNode{typ: "File", vals: map[string]any{"path": ch.Path}})
	}
	return &gqlNode{typ: "FileConnection", kids: kids}
}

// ---------------------------------------------------------------------------
// Root fields, node() and introspection.

// rootField answers one top-level query field. Anything else is an
// unknown field on Query.
func (e *gqlEnv) rootField(f *sel) any {
	switch f.name {
	case "viewer":
		return userNode(viewerLogin, viewerID)
	case "repository":
		args := resolvedArgs(f, e.vars)
		repo := e.store.GetRepo(argStr(args, "owner"), argStr(args, "name"))
		if repo == nil {
			// Unknown repository: null, deliberately with no error.
			return nil
		}
		return repoNode(repo)
	case "node":
		args := resolvedArgs(f, e.vars)
		return e.nodeByID(argStr(args, "id"))
	case "__type":
		args := resolvedArgs(f, e.vars)
		return typeNodeFor(argStr(args, "name"))
	case "__schema":
		return map[string]any{"queryType": map[string]any{"name": "Query"}}
	default:
		e.failField("Query", f.name)
		return nil
	}
}

// nodeByID resolves a global id to the object it names. The tag picks the
// kind — R repository, I issue, P pull request — and each leg re-checks
// that the decoded repo and number actually exist before answering.
func (e *gqlEnv) nodeByID(id string) *gqlNode {
	dec := decodeGlobalId(id)
	if dec == nil {
		return nil
	}
	switch dec.Tag {
	case "R":
		repo := e.repoByPart(dec, 0)
		if repo == nil {
			return nil
		}
		return repoNode(repo)
	case "I":
		repo, issue := e.issueByParts(dec)
		if repo == nil || issue == nil {
			return nil
		}
		return e.issueNode(repo, issue)
	case "P":
		repo, pr := e.pullByParts(dec)
		if repo == nil || pr == nil {
			return nil
		}
		return e.prNode(repo, pr)
	default:
		return nil
	}
}

// repoByPart decodes the repo id at parts[i] and loads that repo.
func (e *gqlEnv) repoByPart(dec *decodedGlobalId, i int) *Repo {
	if i >= len(dec.Parts) {
		return nil
	}
	id, ok := numFromAny(dec.Parts[i])
	if !ok {
		return nil
	}
	return e.store.GetRepoById(id)
}

// issueByParts decodes an "I" id (repo id, issue number) and loads both.
func (e *gqlEnv) issueByParts(dec *decodedGlobalId) (*Repo, *Issue) {
	repo := e.repoByPart(dec, 0)
	if repo == nil {
		return nil, nil
	}
	if len(dec.Parts) < 2 {
		return repo, nil
	}
	num, ok := numFromAny(dec.Parts[1])
	if !ok {
		return repo, nil
	}
	return repo, e.store.GetIssue(repo, num)
}

// pullByParts decodes a "P" id (repo id, PR number) and loads both.
func (e *gqlEnv) pullByParts(dec *decodedGlobalId) (*Repo, *Pull) {
	repo := e.repoByPart(dec, 0)
	if repo == nil {
		return nil, nil
	}
	if len(dec.Parts) < 2 {
		return repo, nil
	}
	num, ok := numFromAny(dec.Parts[1])
	if !ok {
		return repo, nil
	}
	return repo, e.store.GetPull(repo, num)
}

// typeNodeFor answers __type(name:) from the catalog. An enum type
// reports kind ENUM and its values; an object type reports kind OBJECT
// and its fields. Unknown names answer null.
func typeNodeFor(name string) *gqlNode {
	info, ok := typeCatalog[name]
	if !ok {
		return nil
	}
	kind := "OBJECT"
	if info.enumValues != nil {
		kind = "ENUM"
	}
	return &gqlNode{
		typ:        "Type",
		vals:       map[string]any{"name": name, "kind": kind},
		fieldNames: info.fields,
		enumNames:  info.enumValues,
	}
}

// ---------------------------------------------------------------------------
// Mutations.
//
// Each mutation reads its `input` object, decodes the global id(s) in it,
// performs the store operation, and answers with the payload the document
// selected (issue / pullRequest / commentEdge). A mutation that cannot
// find its target records an error and answers null for the whole payload.

// childSels picks the sub-selection named name out of a field's children,
// or nil when the field does not select it. Mutation payloads and
// connections both read their nested objects this way.
func childSels(f *sel, name string) []*sel {
	for _, k := range f.kids {
		if k.name == name {
			return k.kids
		}
	}
	return nil
}

func (e *gqlEnv) runMutation(f *sel, key string) map[string]any {
	args := resolvedArgs(f, e.vars)
	input := asRecord(args["input"])
	switch f.name {
	case "createIssue":
		return e.mutCreateIssue(f, key, input)
	case "closeIssue":
		return e.mutCloseIssue(f, key, input)
	case "createPullRequest":
		return e.mutCreatePullRequest(f, key, input)
	case "mergePullRequest":
		return e.mutMergePullRequest(f, key, input)
	case "addComment":
		return e.mutAddComment(f, key, input)
	default:
		e.failField("Mutation", f.name)
		return map[string]any{key: nil}
	}
}

// repoFromGlobalID decodes a repository global id ("R:<dbId>") and loads
// the repo, or nil when the id is malformed or the repo does not exist.
func (e *gqlEnv) repoFromGlobalID(raw string) *Repo {
	dec := decodeGlobalId(raw)
	if dec == nil || dec.Tag != "R" {
		return nil
	}
	return e.repoByPart(dec, 0)
}

// mutCreateIssue handles createIssue(input: {repositoryId, title, body}).
func (e *gqlEnv) mutCreateIssue(f *sel, key string, input map[string]any) map[string]any {
	repo := e.repoFromGlobalID(argStr(input, "repositoryId"))
	if repo == nil {
		e.errs = append(e.errs, map[string]any{"message": "repository not found"})
		return map[string]any{key: nil}
	}
	issue := e.store.CreateIssue(repo, argStr(input, "title"), argStr(input, "body"), []string{})
	return map[string]any{key: map[string]any{
		"issue": e.project(e.issueNode(repo, issue), childSels(f, "issue")),
	}}
}

// mutCloseIssue handles closeIssue(input: {issueId}). Closing bumps the
// issue's update sequence and clears its cached REST rendering, so the
// next REST read sees the closed state.
func (e *gqlEnv) mutCloseIssue(f *sel, key string, input map[string]any) map[string]any {
	dec := decodeGlobalId(argStr(input, "issueId"))
	var repo *Repo
	var issue *Issue
	if dec != nil && dec.Tag == "I" {
		repo, issue = e.issueByParts(dec)
	}
	if repo == nil || issue == nil {
		e.errs = append(e.errs, map[string]any{"message": "issue not found"})
		return map[string]any{key: nil}
	}
	issue.State = "closed"
	issue.UpdatedSeq = e.store.nextSeq()
	issue.jsonCache = nil
	return map[string]any{key: map[string]any{
		"issue": e.project(e.issueNode(repo, issue), childSels(f, "issue")),
	}}
}

// mutCreatePullRequest handles createPullRequest(input: {repositoryId,
// title, body, headRefName, baseRefName}). An open PR already covering
// the same head/base pair is returned instead of opening a duplicate; a
// missing head or base branch is an error, not an empty PR.
func (e *gqlEnv) mutCreatePullRequest(f *sel, key string, input map[string]any) map[string]any {
	repo := e.repoFromGlobalID(argStr(input, "repositoryId"))
	if repo == nil {
		e.errs = append(e.errs, map[string]any{"message": "repository not found"})
		return map[string]any{key: nil}
	}
	head := argStr(input, "headRefName")
	base := argStr(input, "baseRefName")
	if base == "" {
		base = repo.DefaultBranch
	}
	var pr *Pull
	for _, p := range repo.Pulls {
		if p.State == "open" && p.HeadRef == head && p.BaseRef == base {
			pr = p
			break
		}
	}
	if pr == nil {
		pr = e.store.CreatePull(repo, argStr(input, "title"), argStr(input, "body"), head, base)
	}
	if pr == nil {
		e.errs = append(e.errs, map[string]any{"message": "head or base branch does not exist"})
		return map[string]any{key: nil}
	}
	return map[string]any{key: map[string]any{
		"pullRequest": e.project(e.prNode(repo, pr), childSels(f, "pullRequest")),
	}}
}

// mutMergePullRequest handles mergePullRequest(input: {pullRequestId}).
// An open PR is squash-merged; the payload reports the PR as it stands
// afterwards either way (a conflicted merge leaves it open).
func (e *gqlEnv) mutMergePullRequest(f *sel, key string, input map[string]any) map[string]any {
	dec := decodeGlobalId(argStr(input, "pullRequestId"))
	var repo *Repo
	var pr *Pull
	if dec != nil && dec.Tag == "P" {
		repo, pr = e.pullByParts(dec)
	}
	if repo == nil || pr == nil {
		e.errs = append(e.errs, map[string]any{"message": "pull request not found"})
		return map[string]any{key: nil}
	}
	if pr.State == "open" {
		e.store.MergePull(repo, pr)
	}
	return map[string]any{key: map[string]any{
		"pullRequest": e.project(e.prNode(repo, pr), childSels(f, "pullRequest")),
	}}
}

// mutAddComment handles addComment(input: {subjectId, body}). The subject
// is an issue global id; the answer nests the new comment under
// commentEdge.node with its own opaque id ("C:<repo>:<issue>:<comment>").
func (e *gqlEnv) mutAddComment(f *sel, key string, input map[string]any) map[string]any {
	dec := decodeGlobalId(argStr(input, "subjectId"))
	var repo *Repo
	var issue *Issue
	if dec != nil && dec.Tag == "I" {
		repo, issue = e.issueByParts(dec)
	}
	if repo == nil || issue == nil {
		e.errs = append(e.errs, map[string]any{"message": "subject not found"})
		return map[string]any{key: nil}
	}
	comment := e.store.AddComment(repo, issue, argStr(input, "body"))
	var nodeKids []*sel
	for _, edge := range f.kids {
		if edge.name == "commentEdge" {
			nodeKids = childSels(edge, "node")
		}
	}
	return map[string]any{key: map[string]any{
		"commentEdge": map[string]any{
			"node": e.project(&gqlNode{typ: "Comment", vals: map[string]any{
				"id":   globalId("C", repo.ID, issue.Number, comment.ID),
				"body": comment.Body,
			}}, nodeKids),
		},
	}}
}

// ---------------------------------------------------------------------------
// Operation execution: one parsed document becomes one response object.

// runDoc tokenizes, parses, and runs a single GraphQL document. A
// document that does not parse answers data null with the parse error;
// otherwise data holds one entry per top-level field, and errors appears
// only when at least one field recorded one.
func runDoc(store *Store, query string, vars map[string]any) map[string]any {
	env := &gqlEnv{store: store, vars: vars}
	op, err := parseDoc(lexDoc(query))
	if err != nil {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": err.Error()}}}
	}
	data := make(map[string]any, len(op.fields))
	if op.mutation {
		for _, f := range op.fields {
			key := f.name
			if f.alias != "" {
				key = f.alias
			}
			for k, v := range env.runMutation(f, key) {
				data[k] = v
			}
		}
	} else {
		for _, f := range op.fields {
			key := f.name
			if f.alias != "" {
				key = f.alias
			}
			data[key] = env.render(env.rootField(f), f.kids)
		}
	}
	out := map[string]any{"data": data}
	if len(env.errs) > 0 {
		out["errors"] = env.errs
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
		s.sendJsonPlain(w, 405, map[string]any{"data": nil, "errors": []any{map[string]any{"message": "method not allowed"}}})
		return true
	}
	if !s.isAuthorized(r) {
		s.sendJsonPlain(w, 401, map[string]any{"message": "Bad credentials"})
		return true
	}
	parsed, err := parseJSON(rawBody)
	if err != nil {
		s.sendJsonPlain(w, 400, map[string]any{"data": nil, "errors": []any{map[string]any{"message": "request body is not valid JSON"}}})
		return true
	}
	if batch, ok := parsed.([]any); ok {
		results := make([]any, 0, len(batch))
		for _, entry := range batch {
			rec := asRecord(entry)
			query, _ := rec["query"].(string)
			results = append(results, runDoc(s.store, query, asRecord(rec["variables"])))
		}
		s.sendJsonPlain(w, 200, results)
		return true
	}
	rec := asRecord(parsed)
	query, _ := rec["query"].(string)
	s.sendJsonPlain(w, 200, runDoc(s.store, query, asRecord(rec["variables"])))
	return true
}
