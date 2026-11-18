// GraphQL v4 (/api/graphql): a small parser + executor for exactly the documents gh and
// github-mcp-server send. It is not a general GraphQL engine: it parses operations,
// variables, aliases, inline fragments, enums, lists and input objects, resolves them
// against the same store REST uses, and answers unknown fields with HTTP 200 + errors.
//
// Go port of graphql.ts (v2, Node). Behavior-identical.

package main

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- AST + parser

type valueNode struct {
	kind   string // variable|string|int|float|boolean|null|enum|list|object
	name   string
	str    string
	num    float64
	b      bool
	values []*valueNode
	fields []objField
}

type objField struct {
	name  string
	value *valueNode
}

type fieldNode struct {
	alias      string
	hasAlias   bool
	name       string
	args       []argNode
	selections []selectionNode
	hasSel     bool
}

type argNode struct {
	name  string
	value *valueNode
}

type selectionNode struct {
	isFragment bool
	fragType   string
	sels       []selectionNode
	field      *fieldNode
}

type operation struct {
	op         string // query|mutation
	name       string
	vars       []varDef
	selections []selectionNode
}

type varDef struct {
	name         string
	defaultValue *valueNode
}

type token struct {
	kind  string // punct|name|string|int|float
	value string
}

func tokenize(src string) ([]token, error) {
	var tokens []token
	i := 0
	for i < len(src) {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '.' && i+3 <= len(src) && src[i:i+3] == "..." {
			tokens = append(tokens, token{kind: "punct", value: "..."})
			i += 3
			continue
		}
		if strings.ContainsRune("{}():!=$[]", rune(c)) {
			tokens = append(tokens, token{kind: "punct", value: string(c)})
			i++
			continue
		}
		if c == '"' {
			j := i + 1
			var out strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					e := src[j+1]
					switch e {
					case 'n':
						out.WriteByte('\n')
					case 't':
						out.WriteByte('\t')
					case 'r':
						out.WriteByte('\r')
					default:
						out.WriteByte(e)
					}
					j += 2
				} else {
					out.WriteByte(src[j])
					j++
				}
			}
			tokens = append(tokens, token{kind: "string", value: out.String()})
			i = j + 1
			continue
		}
		if (c >= '0' && c <= '9') || c == '-' {
			j := i
			isFloat := false
			for j < len(src) && strings.ContainsRune("0123456789.eE+-", rune(src[j])) {
				if src[j] == '.' || src[j] == 'e' || src[j] == 'E' {
					isFloat = true
				}
				j++
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			tokens = append(tokens, token{kind: kind, value: src[i:j]})
			i = j
			continue
		}
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' {
			j := i
			for j < len(src) && ((src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= 'a' && src[j] <= 'z') || (src[j] >= '0' && src[j] <= '9') || src[j] == '_') {
				j++
			}
			tokens = append(tokens, token{kind: "name", value: src[i:j]})
			i = j
			continue
		}
		return nil, fmt.Errorf("unexpected character %q", string(c))
	}
	return tokens, nil
}

type parser struct {
	pos    int
	tokens []token
}

func (p *parser) peek() *token {
	if p.pos < len(p.tokens) {
		return &p.tokens[p.pos]
	}
	return nil
}

func (p *parser) next() (token, error) {
	if p.pos >= len(p.tokens) {
		return token{}, fmt.Errorf("unexpected end of document")
	}
	t := p.tokens[p.pos]
	p.pos++
	return t, nil
}

func (p *parser) expectPunct(v string) error {
	t, err := p.next()
	if err != nil {
		return err
	}
	if t.kind != "punct" || t.value != v {
		return fmt.Errorf("expected %s, got %s", v, t.value)
	}
	return nil
}

func (p *parser) acceptPunct(v string) bool {
	t := p.peek()
	if t != nil && t.kind == "punct" && t.value == v {
		p.pos++
		return true
	}
	return false
}

func (p *parser) parseDocument() ([]operation, error) {
	var ops []operation
	for p.pos < len(p.tokens) {
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (p *parser) parseOperation() (operation, error) {
	op := operation{op: "query"}
	t := p.peek()
	if t != nil && t.kind == "name" && (t.value == "query" || t.value == "mutation") {
		nt, _ := p.next()
		op.op = nt.value
		if n := p.peek(); n != nil && n.kind == "name" {
			nt2, _ := p.next()
			op.name = nt2.value
		}
	}
	if p.acceptPunct("(") {
		for !p.acceptPunct(")") {
			if err := p.expectPunct("$"); err != nil {
				return op, err
			}
			vname, err := p.next()
			if err != nil {
				return op, err
			}
			if err := p.expectPunct(":"); err != nil {
				return op, err
			}
			if err := p.parseType(); err != nil {
				return op, err
			}
			vd := varDef{name: vname.value}
			if p.acceptPunct("=") {
				dv, err := p.parseValue()
				if err != nil {
					return op, err
				}
				vd.defaultValue = dv
			}
			op.vars = append(op.vars, vd)
		}
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return op, err
	}
	op.selections = sels
	return op, nil
}

func (p *parser) parseType() error {
	if p.acceptPunct("[") {
		if err := p.parseType(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		if _, err := p.next(); err != nil {
			return err
		}
	}
	p.acceptPunct("!")
	return nil
}

func (p *parser) parseSelectionSet() ([]selectionNode, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var out []selectionNode
	for !p.acceptPunct("}") {
		if p.acceptPunct("...") {
			t := p.peek()
			if t != nil && t.kind == "name" && t.value == "on" {
				if _, err := p.next(); err != nil {
					return nil, err
				}
				typ, err := p.next()
				if err != nil {
					return nil, err
				}
				sels, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				out = append(out, selectionNode{isFragment: true, fragType: typ.value, sels: sels})
			} else {
				// A named fragment spread: unsupported; skip its selection set if any.
				if _, err := p.next(); err != nil {
					return nil, err
				}
				if pk := p.peek(); pk != nil && pk.kind == "punct" && pk.value == "{" {
					if _, err := p.parseSelectionSet(); err != nil {
						return nil, err
					}
				}
			}
		} else {
			first, err := p.next()
			if err != nil {
				return nil, err
			}
			if first.kind != "name" {
				return nil, fmt.Errorf("expected field name, got %s", first.value)
			}
			f := &fieldNode{}
			fieldName := first.value
			if p.acceptPunct(":") {
				f.alias = fieldName
				f.hasAlias = true
				fn, err := p.next()
				if err != nil {
					return nil, err
				}
				fieldName = fn.value
			}
			f.name = fieldName
			if p.acceptPunct("(") {
				for !p.acceptPunct(")") {
					aname, err := p.next()
					if err != nil {
						return nil, err
					}
					if err := p.expectPunct(":"); err != nil {
						return nil, err
					}
					v, err := p.parseValue()
					if err != nil {
						return nil, err
					}
					f.args = append(f.args, argNode{name: aname.value, value: v})
				}
			}
			if pk := p.peek(); pk != nil && pk.kind == "punct" && pk.value == "{" {
				sels, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				f.selections = sels
				f.hasSel = true
			}
			out = append(out, selectionNode{field: f})
		}
	}
	return out, nil
}

func (p *parser) parseValue() (*valueNode, error) {
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	if t.kind == "punct" && t.value == "$" {
		n, err := p.next()
		if err != nil {
			return nil, err
		}
		return &valueNode{kind: "variable", name: n.value}, nil
	}
	if t.kind == "string" {
		return &valueNode{kind: "string", str: t.value}, nil
	}
	if t.kind == "int" || t.kind == "float" {
		n, _ := strconv.ParseFloat(t.value, 64)
		return &valueNode{kind: t.kind, num: n}, nil
	}
	if t.kind == "name" {
		switch t.value {
		case "true":
			return &valueNode{kind: "boolean", b: true}, nil
		case "false":
			return &valueNode{kind: "boolean", b: false}, nil
		case "null":
			return &valueNode{kind: "null"}, nil
		default:
			return &valueNode{kind: "enum", str: t.value}, nil
		}
	}
	if t.kind == "punct" && t.value == "[" {
		vn := &valueNode{kind: "list"}
		for !p.acceptPunct("]") {
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			vn.values = append(vn.values, v)
		}
		return vn, nil
	}
	if t.kind == "punct" && t.value == "{" {
		vn := &valueNode{kind: "object"}
		for !p.acceptPunct("}") {
			fname, err := p.next()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			vn.fields = append(vn.fields, objField{name: fname.value, value: v})
		}
		return vn, nil
	}
	return nil, fmt.Errorf("unexpected value token %s", t.value)
}

// ---------------------------------------------------------------- executor

type gqlError struct {
	Message string `json:"message"`
	Path    []any  `json:"path,omitempty"`
}

type execCtx struct {
	store  *store
	vars   map[string]any
	errors []gqlError
}

// Sentinel for a resolver returning "undefined" (unknown field).
type undefT struct{}

var undef = undefT{}

func valueOf(node *valueNode, vars map[string]any) any {
	switch node.kind {
	case "variable":
		return vars[node.name]
	case "string":
		return node.str
	case "int", "float":
		return node.num
	case "boolean":
		return node.b
	case "null":
		return nil
	case "enum":
		return node.str
	case "list":
		out := make([]any, 0, len(node.values))
		for _, v := range node.values {
			out = append(out, valueOf(v, vars))
		}
		return out
	case "object":
		out := map[string]any{}
		for _, f := range node.fields {
			out[f.name] = valueOf(f.value, vars)
		}
		return out
	}
	return nil
}

func argOf(field *fieldNode, name string, ctx *execCtx) any {
	for _, a := range field.args {
		if a.name == name {
			return valueOf(a.value, ctx.vars)
		}
	}
	return nil
}

var issueFields = []string{"id", "number", "title", "state", "body", "createdAt", "updatedAt", "author", "labels", "comments", "url", "repository"}
var pullFields = []string{"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "headRef", "baseRef", "files", "author", "createdAt", "updatedAt"}
var repoFields = []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private", "createdAt", "updatedAt"}
var searchEnum = []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "CODE", "COMMIT"}

func typeFields(typ string) []string {
	switch typ {
	case "Issue":
		return issueFields
	case "PullRequest":
		return pullFields
	case "Repository":
		return repoFields
	case "User":
		return []string{"id", "login", "name"}
	case "Ref":
		return []string{"name", "prefix"}
	case "IssueConnection":
		return []string{"nodes", "edges", "pageInfo", "totalCount"}
	case "PullRequestConnection":
		return []string{"nodes", "edges", "pageInfo", "totalCount"}
	case "PullRequestChangedFileConnection":
		return []string{"nodes", "edges", "pageInfo", "totalCount"}
	case "PullRequestChangedFile":
		return []string{"path", "additions", "deletions", "changeType"}
	case "PageInfo":
		return []string{"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"}
	case "IssueComment":
		return []string{"id", "body", "author", "createdAt", "updatedAt"}
	case "IssueCommentEdge":
		return []string{"node", "cursor"}
	case "__Type":
		return []string{"name", "kind", "fields", "enumValues"}
	case "Query":
		return []string{"viewer", "repository", "node", "__type", "search"}
	case "Mutation":
		return []string{"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"}
	case "CreateIssuePayload", "CloseIssuePayload":
		return []string{"issue"}
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		return []string{"pullRequest"}
	case "AddCommentPayload":
		return []string{"commentEdge"}
	}
	return []string{}
}

func issueState(is *issue) string {
	if is.state == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func pullState(p *pull) string {
	if p.merged {
		return "MERGED"
	}
	if p.state == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func wrapIssue(r *repo, is *issue) map[string]any {
	return map[string]any{"__type": "Issue", "__repo": r, "__issue": is}
}

func wrapPull(r *repo, p *pull) map[string]any {
	return map[string]any{"__type": "PullRequest", "__repo": r, "__pull": p}
}

func wrapRepo(r *repo) map[string]any {
	return map[string]any{"__type": "Repository", "__repo": r}
}

func cursorFor(n int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(n)))
}

var cursorRe = regexp.MustCompile(`^cursor:(\d+)$`)

func cursorNumber(cursor string) (int, bool) {
	if cursor == "" {
		return 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(cursor)
		if err != nil {
			return 0, false
		}
	}
	m := cursorRe.FindSubmatch(raw)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0, false
	}
	return n, true
}

func resolveIssues(r *repo, field *fieldNode, ctx *execCtx) map[string]any {
	first := argOf(field, "first", ctx)
	after := argOf(field, "after", ctx)
	states := argOf(field, "states", ctx)
	orderBy, _ := argOf(field, "orderBy", ctx).(map[string]any)
	items := make([]*issue, 0, len(r.issues))
	items = append(items, r.issues...)
	if arr, ok := states.([]any); ok && len(arr) > 0 {
		want := make(map[string]bool)
		for _, s := range arr {
			want[fmt.Sprint(s)] = true
		}
		filtered := items[:0]
		for _, is := range items {
			if want[issueState(is)] {
				filtered = append(filtered, is)
			}
		}
		items = filtered
	}
	desc := true
	if orderBy != nil {
		if d, _ := orderBy["direction"].(string); d == "ASC" {
			desc = false
		}
	}
	sort.Slice(items, func(i, j int) bool {
		var d int
		if desc {
			d = items[j].seq - items[i].seq
			if d == 0 {
				d = items[j].number - items[i].number
			}
		} else {
			d = items[i].seq - items[j].seq
			if d == 0 {
				d = items[i].number - items[j].number
			}
		}
		return d < 0
	})
	if afterStr, ok := after.(string); ok {
		if afterN, ok := cursorNumber(afterStr); ok {
			filtered := items[:0]
			for _, is := range items {
				if desc {
					if is.number < afterN {
						filtered = append(filtered, is)
					}
				} else {
					if is.number > afterN {
						filtered = append(filtered, is)
					}
				}
			}
			items = filtered
		}
	}
	limit := len(items)
	if f, ok := first.(float64); ok {
		limit = int(f)
	}
	if limit < 0 {
		limit = 0
	}
	if limit > len(items) {
		limit = len(items)
	}
	page := items[:limit]
	hasNext := len(items) > len(page)
	var endCursor, startCursor any
	if len(page) > 0 {
		endCursor = cursorFor(page[len(page)-1].number)
		startCursor = cursorFor(page[0].number)
	}
	nodes := make([]any, 0, len(page))
	for _, is := range page {
		nodes = append(nodes, wrapIssue(r, is))
	}
	return map[string]any{
		"__type": "IssueConnection",
		"__nodes": nodes,
		"__pageInfo": map[string]any{
			"hasNextPage":     hasNext,
			"hasPreviousPage": false,
			"startCursor":     startCursor,
			"endCursor":       endCursor,
		},
	}
}

func resolvePullRequests(r *repo, field *fieldNode) map[string]any {
	items := make([]*pull, 0, len(r.pulls))
	items = append(items, r.pulls...)
	sort.Slice(items, func(i, j int) bool { return items[i].number > items[j].number })
	nodes := make([]any, 0, len(items))
	for _, p := range items {
		nodes = append(nodes, wrapPull(r, p))
	}
	return map[string]any{"__type": "PullRequestConnection", "__nodes": nodes}
}

func resolveFiles(r *repo, p *pull, field *fieldNode, ctx *execCtx) map[string]any {
	first := argOf(field, "first", ctx)
	headObj := getObject(r, p.headSha)
	base := mergeBase(r, p.headSha, p.baseSha)
	baseTree := ""
	if base != "" {
		if o := getObject(r, base); o != nil {
			baseTree = parseCommit(o.data).tree
		}
	}
	headTree := ""
	if headObj != nil && headObj.typ == objCommit {
		headTree = parseCommit(headObj.data).tree
	}
	diffs := diffTrees(r, baseTree, headTree)
	var kept []treeDiff
	for _, d := range diffs {
		if d.status != "removed" {
			kept = append(kept, d)
		}
	}
	limit := len(kept)
	if f, ok := first.(float64); ok {
		limit = int(f)
	}
	if limit < 0 {
		limit = 0
	}
	if limit > len(kept) {
		limit = len(kept)
	}
	nodes := make([]any, 0, limit)
	for _, d := range kept[:limit] {
		nodes = append(nodes, map[string]any{"__type": "PullRequestChangedFile", "path": d.path})
	}
	return map[string]any{"__type": "PullRequestChangedFileConnection", "__nodes": nodes}
}

// Whether merging this pull would conflict (a file changed on both sides differently).
func pullIsConflicted(r *repo, p *pull) bool {
	base := mergeBase(r, p.headSha, p.baseSha)
	headObj := getObject(r, p.headSha)
	baseObj := getObject(r, p.baseSha)
	if headObj == nil || baseObj == nil {
		return false
	}
	headTree := parseCommit(headObj.data).tree
	baseTree := parseCommit(baseObj.data).tree
	baseOfMerge := ""
	if base != "" {
		if o := getObject(r, base); o != nil {
			baseOfMerge = parseCommit(o.data).tree
		}
	}
	baseChanges := diffTrees(r, baseOfMerge, baseTree)
	headChanges := diffTrees(r, baseOfMerge, headTree)
	headMap := make(map[string]string, len(headChanges))
	for _, d := range headChanges {
		headMap[d.path] = d.newOID
	}
	for _, bc := range baseChanges {
		if _, ok := headMap[bc.path]; !ok {
			continue
		}
		baseOID := treeLookupOf(r, baseTree, bc.path)
		headOID := treeLookupOf(r, headTree, bc.path)
		if baseOID != headOID {
			return true
		}
	}
	return false
}

func treeLookupOf(r *repo, tree, path string) string {
	parts := strings.Split(path, "/")
	cur := tree
	for i, part := range parts {
		obj := getObject(r, cur)
		if obj == nil || obj.typ != objTree {
			return ""
		}
		var entry *treeEntry
		for _, e := range parseTree(obj.data) {
			if e.name == part {
				ec := e
				entry = &ec
				break
			}
		}
		if entry == nil {
			return ""
		}
		if i == len(parts)-1 {
			return entry.oid
		}
		cur = entry.oid
	}
	return ""
}

func resolveField(parent map[string]any, field *fieldNode, ctx *execCtx) any {
	typ, _ := parent["__type"].(string)
	name := field.name
	var rp *repo
	if v, ok := parent["__repo"].(*repo); ok {
		rp = v
	}

	if typ == "CreateIssuePayload" || typ == "CloseIssuePayload" {
		if name == "issue" {
			return parent["issue"]
		}
	}
	if typ == "CreatePullRequestPayload" || typ == "MergePullRequestPayload" {
		if name == "pullRequest" {
			return parent["pullRequest"]
		}
	}
	if typ == "AddCommentPayload" {
		if name == "commentEdge" {
			return parent["commentEdge"]
		}
	}

	if typ == "Query" {
		if name == "viewer" {
			return map[string]any{"__type": "User", "login": viewerLogin}
		}
		if name == "repository" {
			owner, _ := argOf(field, "owner", ctx).(string)
			rname, _ := argOf(field, "name", ctx).(string)
			found := ctx.store.viewerRepo(owner, rname)
			if found == nil {
				return nil
			}
			return wrapRepo(found)
		}
		if name == "node" {
			id, _ := argOf(field, "id", ctx).(string)
			kind, n, ok := parseGlobalID(id)
			if !ok {
				return nil
			}
			switch kind {
			case "Repository":
				for _, r := range ctx.store.repos {
					if r.id == n {
						return wrapRepo(r)
					}
				}
				return nil
			case "Issue":
				for _, r := range ctx.store.repos {
					for _, is := range r.issues {
						if is.id == n {
							return wrapIssue(r, is)
						}
					}
				}
				return nil
			case "PullRequest":
				for _, r := range ctx.store.repos {
					for _, p := range r.pulls {
						if p.id == n {
							return wrapPull(r, p)
						}
					}
				}
				return nil
			}
			return nil
		}
		if name == "__type" {
			tname, _ := argOf(field, "name", ctx).(string)
			if tname == "SearchType" {
				enumValues := make([]any, 0, len(searchEnum))
				for _, n := range searchEnum {
					enumValues = append(enumValues, map[string]any{"name": n})
				}
				return map[string]any{"__type": "__Type", "name": tname, "kind": "ENUM", "enumValues": enumValues}
			}
			fields := typeFields(tname)
			if len(fields) == 0 && tname != "Issue" && tname != "PullRequest" && tname != "Repository" {
				return nil
			}
			fieldList := make([]any, 0, len(fields))
			for _, f := range fields {
				fieldList = append(fieldList, map[string]any{"name": f})
			}
			return map[string]any{"__type": "__Type", "name": tname, "kind": "OBJECT", "fields": fieldList}
		}
		if name == "search" {
			return map[string]any{"__type": "SearchResultItemConnection", "__nodes": []any{}}
		}
	}

	if typ == "User" {
		if name == "login" {
			return parent["login"]
		}
		if name == "id" {
			return globalID("User", 1000)
		}
		if name == "name" {
			return "ada iversen"
		}
	}

	if typ == "Repository" && rp != nil {
		switch name {
		case "id":
			return globalID("Repository", rp.id)
		case "databaseId":
			return rp.id
		case "name":
			return rp.name
		case "nameWithOwner":
			return rp.owner + "/" + rp.name
		case "owner":
			return map[string]any{"__type": "User", "login": rp.owner}
		case "defaultBranchRef":
			return map[string]any{"__type": "Ref", "name": rp.defaultBranch}
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return true
		case "description":
			return rp.description
		case "private":
			return rp.isPrivate
		case "issue":
			number := argOf(field, "number", ctx)
			var num int
			switch v := number.(type) {
			case float64:
				num = int(v)
			case int:
				num = v
			}
			for _, is := range rp.issues {
				if is.number == num {
					return wrapIssue(rp, is)
				}
			}
			return nil
		case "issues":
			return resolveIssues(rp, field, ctx)
		case "pullRequests":
			return resolvePullRequests(rp, field)
		case "pullRequest":
			number := argOf(field, "number", ctx)
			var num int
			switch v := number.(type) {
			case float64:
				num = int(v)
			case int:
				num = v
			}
			for _, p := range rp.pulls {
				if p.number == num {
					return wrapPull(rp, p)
				}
			}
			return nil
		}
	}

	if typ == "Issue" && rp != nil {
		is, _ := parent["__issue"].(*issue)
		if is == nil {
			return undef
		}
		switch name {
		case "id":
			return globalID("Issue", is.id)
		case "number":
			return is.number
		case "title":
			return is.title
		case "state":
			return issueState(is)
		case "body":
			return is.body
		case "createdAt":
			return is.createdAt
		case "updatedAt":
			return is.updatedAt
		case "author":
			return map[string]any{"__type": "User", "login": viewerLogin}
		case "repository":
			return wrapRepo(rp)
		}
	}

	if typ == "PullRequest" && rp != nil {
		p, _ := parent["__pull"].(*pull)
		if p == nil {
			return undef
		}
		switch name {
		case "id":
			return globalID("PullRequest", p.id)
		case "number":
			return p.number
		case "title":
			return p.title
		case "body":
			return p.body
		case "state":
			return pullState(p)
		case "merged":
			return p.merged
		case "mergeable":
			if pullIsConflicted(rp, p) {
				return "CONFLICTING"
			}
			return "MERGEABLE"
		case "headRefName":
			return p.headRef
		case "baseRefName":
			return p.baseRef
		case "headRef":
			return map[string]any{"__type": "Ref", "name": p.headRef}
		case "baseRef":
			return map[string]any{"__type": "Ref", "name": p.baseRef}
		case "files":
			return resolveFiles(rp, p, field, ctx)
		case "author":
			return map[string]any{"__type": "User", "login": viewerLogin}
		case "createdAt":
			return p.createdAt
		case "updatedAt":
			return p.updatedAt
		}
	}

	if typ == "Ref" {
		if name == "name" {
			return parent["name"]
		}
		if name == "prefix" {
			return "refs/heads/"
		}
	}

	if typ == "IssueConnection" || typ == "PullRequestConnection" || typ == "PullRequestChangedFileConnection" {
		nodes, _ := parent["__nodes"].([]any)
		if name == "nodes" {
			if nodes == nil {
				return []any{}
			}
			return nodes
		}
		if name == "edges" {
			edges := make([]any, 0, len(nodes))
			for _, n := range nodes {
				edges = append(edges, map[string]any{"__type": "Edge", "node": n})
			}
			return edges
		}
		if name == "pageInfo" {
			if pi, ok := parent["__pageInfo"]; ok {
				return pi
			}
			return map[string]any{"hasNextPage": false, "hasPreviousPage": false, "startCursor": nil, "endCursor": nil}
		}
		if name == "totalCount" {
			return len(nodes)
		}
	}

	if typ == "PullRequestChangedFile" {
		if name == "path" {
			return parent["path"]
		}
		if name == "additions" {
			return 1
		}
		if name == "deletions" {
			return 0
		}
		if name == "changeType" {
			return "ADDED"
		}
	}

	if typ == "PageInfo" {
		switch name {
		case "hasNextPage":
			if v, ok := parent["hasNextPage"]; ok {
				return v
			}
			return false
		case "hasPreviousPage":
			if v, ok := parent["hasPreviousPage"]; ok {
				return v
			}
			return false
		case "startCursor":
			return parent["startCursor"]
		case "endCursor":
			return parent["endCursor"]
		}
	}

	if typ == "__Type" {
		switch name {
		case "name":
			return parent["name"]
		case "kind":
			return parent["kind"]
		case "fields":
			if v, ok := parent["fields"]; ok {
				return v
			}
			return nil
		case "enumValues":
			if v, ok := parent["enumValues"]; ok {
				return v
			}
			return nil
		}
	}

	if typ == "IssueComment" {
		if name == "id" {
			return parent["id"]
		}
		if name == "body" {
			return parent["body"]
		}
		if name == "author" {
			return map[string]any{"__type": "User", "login": viewerLogin}
		}
	}

	if typ == "IssueCommentEdge" || typ == "Edge" {
		if name == "node" {
			return parent["node"]
		}
		if name == "cursor" {
			if v, ok := parent["cursor"]; ok {
				return v
			}
			return nil
		}
	}

	if typ == "SearchResultItemConnection" {
		if name == "nodes" {
			return []any{}
		}
		if name == "pageInfo" {
			return map[string]any{"hasNextPage": false, "hasPreviousPage": false, "startCursor": nil, "endCursor": nil}
		}
	}

	return undef
}

func typeNameOf(value any) string {
	if m, ok := value.(map[string]any); ok {
		if t, ok := m["__type"].(string); ok {
			return t
		}
	}
	return ""
}

func resolveSelections(parent map[string]any, selections []selectionNode, ctx *execCtx, path []any) map[string]any {
	out := map[string]any{}
	typ, _ := parent["__type"].(string)
	for _, sel := range selections {
		if sel.isFragment {
			if sel.fragType == typ {
				sub := resolveSelections(parent, sel.sels, ctx, path)
				for k, v := range sub {
					out[k] = v
				}
			}
			continue
		}
		f := sel.field
		key := f.name
		if f.hasAlias {
			key = f.alias
		}
		known := false
		for _, fn := range typeFields(typ) {
			if fn == f.name {
				known = true
				break
			}
		}
		if !known && !strings.HasPrefix(f.name, "__") {
			ctx.errors = append(ctx.errors, gqlError{
				Message: fmt.Sprintf("Cannot query field %q on type %q.", f.name, typ),
				Path:    append(append([]any{}, path...), key),
			})
			continue
		}
		value := resolveField(parent, f, ctx)
		if value == undef {
			ctx.errors = append(ctx.errors, gqlError{
				Message: fmt.Sprintf("Cannot query field %q on type %q.", f.name, typ),
				Path:    append(append([]any{}, path...), key),
			})
			continue
		}
		var selList []selectionNode
		if f.hasSel {
			selList = f.selections
		}
		out[key] = resolveValue(value, selList, f.hasSel, ctx, append(append([]any{}, path...), key))
	}
	return out
}

func resolveValue(value any, selections []selectionNode, hasSel bool, ctx *execCtx, path []any) any {
	if value == nil {
		return nil
	}
	if arr, ok := value.([]any); ok {
		out := make([]any, len(arr))
		for i, v := range arr {
			out[i] = resolveValue(v, selections, hasSel, ctx, append(append([]any{}, path...), i))
		}
		return out
	}
	if m, ok := value.(map[string]any); ok {
		typ := typeNameOf(value)
		if typ == "" || !hasSel {
			if !hasSel {
				return value
			}
			// A plain object (pageInfo, __Type fields list items, edges): resolve its keys as data.
			out := map[string]any{}
			for _, sel := range selections {
				if sel.isFragment {
					continue
				}
				f := sel.field
				key := f.name
				if f.hasAlias {
					key = f.alias
				}
				raw, present := m[f.name]
				if !present {
					// Nested selection on a plain value (e.g. fields { name } on __Type entries).
					if f.hasSel {
						if _, isStr := m["name"].(string); isStr {
							out[key] = resolveValue(value, f.selections, f.hasSel, ctx, append(append([]any{}, path...), key))
							continue
						}
					}
					out[key] = nil
					continue
				}
				out[key] = resolveValue(raw, f.selections, f.hasSel, ctx, append(append([]any{}, path...), key))
			}
			return out
		}
		_ = m
		return resolveSelections(value.(map[string]any), selections, ctx, path)
	}
	return value
}

// ---------------------------------------------------------------- mutations

func gqlStr(v any) string {
	if v == nil {
		return ""
	}
	return jsString(v)
}

func createIssueGql(s *store, input map[string]any) (map[string]any, error) {
	repoID, _ := input["repositoryId"].(string)
	kind, n, ok := parseGlobalID(repoID)
	var r *repo
	if ok && kind == "Repository" {
		for _, cand := range s.repos {
			if cand.id == n {
				r = cand
				break
			}
		}
	}
	if r == nil {
		return nil, fmt.Errorf("repository not found")
	}
	title := gqlStr(input["title"])
	bodyText, _ := input["body"].(string)
	now := s.nowIso()
	is := &issue{
		id:        s.nextIssueID,
		number:    r.nextIssue,
		title:     title,
		body:      bodyText,
		state:     "open",
		labels:    []string{},
		seq:       len(r.issues) + 1,
		createdAt: now,
		updatedAt: now,
	}
	s.nextIssueID++
	r.nextIssue++
	r.issues = append(r.issues, is)
	return map[string]any{"__type": "Issue", "__repo": r, "__issue": is}, nil
}

func closeIssueGql(s *store, input map[string]any) (map[string]any, error) {
	issueID, _ := input["issueId"].(string)
	kind, n, ok := parseGlobalID(issueID)
	if !ok || kind != "Issue" {
		return nil, fmt.Errorf("issue not found")
	}
	for _, r := range s.repos {
		for _, is := range r.issues {
			if is.id == n {
				is.state = "closed"
				is.updatedAt = s.nowIso()
				return map[string]any{"__type": "Issue", "__repo": r, "__issue": is}, nil
			}
		}
	}
	return nil, fmt.Errorf("issue not found")
}

func createPullGql(s *store, input map[string]any) (map[string]any, error) {
	repoID, _ := input["repositoryId"].(string)
	kind, n, ok := parseGlobalID(repoID)
	var r *repo
	if ok && kind == "Repository" {
		for _, cand := range s.repos {
			if cand.id == n {
				r = cand
				break
			}
		}
	}
	if r == nil {
		return nil, fmt.Errorf("repository not found")
	}
	head, _ := input["headRefName"].(string)
	base, _ := input["baseRefName"].(string)
	if base == "" {
		base = r.defaultBranch
	}
	headSha, hasHead := r.refs["refs/heads/"+head]
	baseSha, hasBase := r.refs["refs/heads/"+base]
	if !hasHead || !hasBase {
		return nil, fmt.Errorf("head or base branch not found")
	}
	now := s.nowIso()
	title := gqlStr(input["title"])
	p := &pull{
		id:        s.nextPullID,
		number:    r.nextPull,
		title:     title,
		body:      bodyStr(input["body"]),
		state:     "open",
		merged:    false,
		headRef:   head,
		baseRef:   base,
		headSha:   headSha,
		baseSha:   baseSha,
		seq:       len(r.pulls) + 1,
		createdAt: now,
		updatedAt: now,
	}
	s.nextPullID++
	r.nextPull++
	r.pulls = append(r.pulls, p)
	return map[string]any{"__type": "PullRequest", "__repo": r, "__pull": p}, nil
}

func bodyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func resolveMutation(parent map[string]any, field *fieldNode, ctx *execCtx) (any, error) {
	input, _ := argOf(field, "input", ctx).(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	switch field.name {
	case "createIssue":
		is, err := createIssueGql(ctx.store, input)
		if err != nil {
			return nil, err
		}
		return map[string]any{"__type": "CreateIssuePayload", "issue": is}, nil
	case "closeIssue":
		is, err := closeIssueGql(ctx.store, input)
		if err != nil {
			return nil, err
		}
		return map[string]any{"__type": "CloseIssuePayload", "issue": is}, nil
	case "createPullRequest":
		p, err := createPullGql(ctx.store, input)
		if err != nil {
			return nil, err
		}
		return map[string]any{"__type": "CreatePullRequestPayload", "pullRequest": p}, nil
	case "mergePullRequest":
		prID, _ := input["pullRequestId"].(string)
		kind, n, ok := parseGlobalID(prID)
		if !ok || kind != "PullRequest" {
			return nil, fmt.Errorf("pull request not found")
		}
		for _, r := range ctx.store.repos {
			for _, p := range r.pulls {
				if p.id == n {
					if !doMergePull(ctx.store, r, p) {
						return nil, fmt.Errorf("merge conflict")
					}
					return map[string]any{"__type": "MergePullRequestPayload", "pullRequest": map[string]any{"__type": "PullRequest", "__repo": r, "__pull": p}}, nil
				}
			}
		}
		return nil, fmt.Errorf("pull request not found")
	case "addComment":
		subjID, _ := input["subjectId"].(string)
		kind, n, ok := parseGlobalID(subjID)
		if !ok || kind != "Issue" {
			return nil, fmt.Errorf("subject not found")
		}
		for _, r := range ctx.store.repos {
			for _, is := range r.issues {
				if is.id == n {
					now := ctx.store.nowIso()
					c := &comment{id: ctx.store.nextCommentID, body: bodyStr(input["body"]), seq: len(r.comments[is.number]) + 1, createdAt: now, updatedAt: now}
					ctx.store.nextCommentID++
					r.comments[is.number] = append(r.comments[is.number], c)
					node := map[string]any{"__type": "IssueComment", "id": globalID("IssueComment", c.id), "body": c.body}
					return map[string]any{"__type": "AddCommentPayload", "commentEdge": map[string]any{"__type": "IssueCommentEdge", "node": node, "cursor": cursorFor(c.id)}}, nil
				}
			}
		}
		return nil, fmt.Errorf("subject not found")
	}
	return undef, nil
}

// The merge itself lives in rest.go (shared with PUT /pulls/{n}/merge).

type gqlResult struct {
	Data   any        `json:"data"`
	Errors []gqlError `json:"errors,omitempty"`
}

func executeOperation(s *store, op operation, variables map[string]any) (result gqlResult) {
	ctx := &execCtx{store: s, vars: variables}
	defer func() {
		if rec := recover(); rec != nil {
			msg := "Internal error"
			if err, ok := rec.(error); ok {
				msg = err.Error()
			} else if str, ok := rec.(string); ok {
				msg = str
			}
			result = gqlResult{Data: nil, Errors: []gqlError{{Message: msg}}}
		}
	}()
	if op.op == "mutation" {
		root := map[string]any{"__type": "Mutation"}
		out := map[string]any{}
		for _, sel := range op.selections {
			if sel.isFragment {
				continue
			}
			f := sel.field
			key := f.name
			if f.hasAlias {
				key = f.alias
			}
			payload, err := resolveMutation(root, f, ctx)
			if err != nil {
				panic(err)
			}
			if payload == undef {
				ctx.errors = append(ctx.errors, gqlError{Message: fmt.Sprintf("Cannot query field %q on type %q.", f.name, "Mutation")})
				continue
			}
			out[key] = resolveValue(payload, f.selections, f.hasSel, ctx, []any{key})
		}
		result = gqlResult{Data: out}
		if len(ctx.errors) > 0 {
			result.Errors = ctx.errors
		}
		return result
	}
	root := map[string]any{"__type": "Query"}
	data := resolveSelections(root, op.selections, ctx, []any{})
	result = gqlResult{Data: data}
	if len(ctx.errors) > 0 {
		result.Errors = ctx.errors
	}
	return result
}

type gqlOutcome struct {
	status int
	body   any
}

// Execute one GraphQL request body (a single operation or a batch array).
func handleGraphql(s *store, body any) gqlOutcome {
	if arr, ok := body.([]any); ok {
		out := make([]gqlResult, 0, len(arr))
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				out = append(out, executeOne(s, m))
			} else {
				out = append(out, gqlResult{Data: nil, Errors: []gqlError{{Message: "Invalid operation"}}})
			}
		}
		return gqlOutcome{status: 200, body: out}
	}
	if m, ok := body.(map[string]any); ok {
		return gqlOutcome{status: 200, body: executeOne(s, m)}
	}
	return gqlOutcome{status: 400, body: map[string]any{"errors": []any{map[string]any{"message": "Invalid GraphQL body"}}}}
}

func executeOne(s *store, item map[string]any) gqlResult {
	query, _ := item["query"].(string)
	variables, _ := item["variables"].(map[string]any)
	if variables == nil {
		variables = map[string]any{}
	}
	toks, err := tokenize(query)
	if err != nil {
		return gqlResult{Data: nil, Errors: []gqlError{{Message: err.Error()}}}
	}
	ops, err := (&parser{tokens: toks}).parseDocument()
	if err != nil {
		return gqlResult{Data: nil, Errors: []gqlError{{Message: err.Error()}}}
	}
	if len(ops) == 0 {
		return gqlResult{Data: nil, Errors: []gqlError{{Message: "Empty document"}}}
	}
	op := ops[0]
	vars := map[string]any{}
	for k, v := range variables {
		vars[k] = v
	}
	for _, v := range op.vars {
		if _, ok := vars[v.name]; !ok && v.defaultValue != nil {
			vars[v.name] = valueOf(v.defaultValue, vars)
		}
	}
	return executeOperation(s, op, vars)
}
