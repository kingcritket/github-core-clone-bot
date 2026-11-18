// MCP endpoint (POST /mcp): JSON-RPC 2.0 over Streamable HTTP, revision 2026-07-28.
// server/discover negotiates the revision; initialize is also answered for the legacy
// handshake era (2025-11-25 and earlier). Tools act on the same store as REST and
// GraphQL, so a repository or issue created here reads back over /api/v3 unchanged.
//
// Go port of mcp.ts (v2, Node). Behavior-identical.

package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const mcpVersion = "2026-07-28"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const mcpServerName = "github-core-clone"
const mcpServerVersion = "1.0.0"

type toolDef struct {
	name        string
	description string
	properties  map[string]toolProp
	required    []string
}

type toolProp struct {
	typ         string
	description string
}

var mcpTools = []toolDef{
	{
		name:        "create_repo",
		description: "Create a new repository owned by the authenticated user.",
		properties: map[string]toolProp{
			"name":        {typ: "string", description: "The repository name."},
			"owner":       {typ: "string", description: "The owner login (the authenticated user)."},
			"description": {typ: "string", description: "A short description of the repository."},
			"private":     {typ: "boolean", description: "Whether the repository is private."},
		},
		required: []string{"name"},
	},
	{
		name:        "get_repo",
		description: "Get a repository by owner and name.",
		properties: map[string]toolProp{
			"owner": {typ: "string", description: "The repository owner login."},
			"repo":  {typ: "string", description: "The repository name."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name:        "create_issue",
		description: "Create an issue in a repository.",
		properties: map[string]toolProp{
			"owner": {typ: "string", description: "The repository owner login."},
			"repo":  {typ: "string", description: "The repository name."},
			"title": {typ: "string", description: "The issue title."},
			"body":  {typ: "string", description: "The issue body."},
		},
		required: []string{"owner", "repo", "title"},
	},
	{
		name:        "list_issues",
		description: "List issues of a repository, optionally filtered by state.",
		properties: map[string]toolProp{
			"owner": {typ: "string", description: "The repository owner login."},
			"repo":  {typ: "string", description: "The repository name."},
			"state": {typ: "string", description: "Issue state: open, closed or all."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name:        "create_pull_request",
		description: "Create a pull request from a head branch onto a base branch.",
		properties: map[string]toolProp{
			"owner": {typ: "string", description: "The repository owner login."},
			"repo":  {typ: "string", description: "The repository name."},
			"title": {typ: "string", description: "The pull request title."},
			"body":  {typ: "string", description: "The pull request body."},
			"head":  {typ: "string", description: "The head branch name."},
			"base":  {typ: "string", description: "The base branch name."},
		},
		required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		name:        "merge_pull_request",
		description: "Merge a pull request with a squash commit onto its base branch.",
		properties: map[string]toolProp{
			"owner":       {typ: "string", description: "The repository owner login."},
			"repo":        {typ: "string", description: "The repository name."},
			"pull_number": {typ: "integer", description: "The pull request number."},
		},
		required: []string{"owner", "repo", "pull_number"},
	},
}

func toolsList() []any {
	out := make([]any, 0, len(mcpTools))
	for _, t := range mcpTools {
		props := map[string]any{}
		for name, p := range t.properties {
			props[name] = map[string]any{"type": p.typ, "description": p.description}
		}
		required := make([]any, 0, len(t.required))
		for _, r := range t.required {
			required = append(required, r)
		}
		out = append(out, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": map[string]any{"type": "object", "properties": props, "required": required},
		})
	}
	return out
}

var nonAlnumRe = regexp.MustCompile(`[^a-z0-9]`)

func normToolName(s string) string {
	return nonAlnumRe.ReplaceAllString(strings.ToLower(s), "")
}

func findTool(name string) *toolDef {
	n := normToolName(name)
	for i := range mcpTools {
		if normToolName(mcpTools[i].name) == n {
			return &mcpTools[i]
		}
	}
	for i := range mcpTools {
		tn := normToolName(mcpTools[i].name)
		if len(n) > len(tn) && strings.HasSuffix(n, tn) {
			return &mcpTools[i]
		}
	}
	return nil
}

func rpcOK(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcErr(id any, code int, message string, data ...any) map[string]any {
	err := map[string]any{"code": code, "message": message}
	if len(data) > 0 && data[0] != nil {
		err["data"] = data[0]
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": err}
}

func toolResult(content any, isError ...bool) map[string]any {
	var text string
	if s, ok := content.(string); ok {
		text = s
	} else {
		text = string(marshalJSON(content))
	}
	out := map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": text}},
		"structuredContent": content,
	}
	if len(isError) > 0 && isError[0] {
		out["isError"] = true
	}
	return out
}

func getMcpRepo(s *store, owner, repoName string) *repo {
	if owner != viewerLogin {
		return nil
	}
	return s.getRepo(owner, repoName)
}

type rpcError struct {
	code    int
	message string
}

func (e *rpcError) Error() string { return e.message }

func callTool(s *store, name string, args map[string]any) (map[string]any, error) {
	tool := findTool(name)
	if tool == nil {
		return nil, &rpcError{code: -32602, message: fmt.Sprintf("Unknown tool: %s", name)}
	}
	// Missing required arguments.
	for _, req := range tool.required {
		v := args[req]
		if v == nil || v == "" {
			return toolResult(fmt.Sprintf("Missing required argument: %s", req), true), nil
		}
	}
	owner, _ := args["owner"].(string)
	if owner == "" {
		owner = viewerLogin
	}
	repoName := ""
	if v, ok := args["repo"].(string); ok {
		repoName = v
	} else if v, ok := args["repository"].(string); ok {
		repoName = v
	} else if v, ok := args["name"].(string); ok {
		repoName = v
	}

	if tool.name == "create_repo" {
		repoNameArg, _ := args["name"].(string)
		if repoNameArg == "" {
			return toolResult("Missing required argument: name", true), nil
		}
		existing := s.getRepo(viewerLogin, repoNameArg)
		var r *repo
		if existing != nil {
			r = existing
		} else {
			priv, _ := args["private"].(bool)
			desc, _ := args["description"].(string)
			r = s.createRepo(repoNameArg, priv, desc)
		}
		return toolResult(repoJSON(r)), nil
	}

	r := getMcpRepo(s, owner, repoName)
	if r == nil {
		return toolResult(fmt.Sprintf("Repository not found: %s/%s", owner, repoName), true), nil
	}

	switch tool.name {
	case "get_repo":
		return toolResult(repoJSON(r)), nil
	case "create_issue":
		title, _ := args["title"].(string)
		bodyText, _ := args["body"].(string)
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
		return toolResult(issueJSON(r, is)), nil
	case "list_issues":
		state, _ := args["state"].(string)
		if state == "" {
			state = "open"
		}
		items := make([]*issue, 0, len(r.issues))
		for _, is := range r.issues {
			if state != "all" && is.state != state {
				continue
			}
			items = append(items, is)
		}
		// sort by number ascending (insertion sort; numbers grow monotonically)
		for i := 1; i < len(items); i++ {
			for j := i; j > 0 && items[j-1].number > items[j].number; j-- {
				items[j-1], items[j] = items[j], items[j-1]
			}
		}
		out := make([]any, 0, len(items))
		for _, is := range items {
			out = append(out, issueJSON(r, is))
		}
		return toolResult(out), nil
	case "create_pull_request":
		title, _ := args["title"].(string)
		bodyText, _ := args["body"].(string)
		head, _ := args["head"].(string)
		base, _ := args["base"].(string)
		if base == "" {
			base = r.defaultBranch
		}
		headSha, hasHead := r.refs["refs/heads/"+head]
		baseSha, hasBase := r.refs["refs/heads/"+base]
		if !hasHead {
			return toolResult(fmt.Sprintf("Head branch %q does not exist", head), true), nil
		}
		if !hasBase {
			return toolResult(fmt.Sprintf("Base branch %q does not exist", base), true), nil
		}
		now := s.nowIso()
		p := &pull{
			id:        s.nextPullID,
			number:    r.nextPull,
			title:     title,
			body:      bodyText,
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
		return toolResult(pullJSON(r, p)), nil
	case "merge_pull_request":
		var n int
		switch v := args["pull_number"].(type) {
		case float64:
			n = int(v)
		case int:
			n = v
		default:
			if v2, ok := args["number"].(float64); ok {
				n = int(v2)
			}
		}
		var p *pull
		for _, cand := range r.pulls {
			if cand.number == n {
				p = cand
				break
			}
		}
		if p == nil {
			return toolResult(fmt.Sprintf("Pull request not found: %d", n), true), nil
		}
		if p.merged || p.state == "closed" {
			return toolResult(fmt.Sprintf("Pull request #%d is already merged or closed", n), true), nil
		}
		if !doMergePull(s, r, p) {
			return toolResult(fmt.Sprintf("Merge conflict: pull request #%d cannot be merged", n), true), nil
		}
		return toolResult(map[string]any{"sha": p.mergeSha, "merged": true, "message": "Pull request successfully merged", "number": p.number}), nil
	}

	return toolResult(fmt.Sprintf("Unknown tool: %s", name), true), nil
}

type mcpOutcome struct {
	status int
	body   any
}

// Handle one POST /mcp request. rawBody is the exact request bytes (a non-JSON body
// must answer HTTP 400 with JSON-RPC -32700).
func handleMcp(s *store, rawBody []byte) mcpOutcome {
	var parsed any
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		return mcpOutcome{status: 400, body: rpcErr(nil, -32700, "Parse error")}
	}
	req, ok := parsed.(map[string]any)
	if !ok {
		return mcpOutcome{status: 400, body: rpcErr(nil, -32600, "Invalid Request")}
	}
	idVal, hasID := req["id"]
	var id any
	if hasID {
		id = idVal
	} else {
		id = nil
	}
	method, _ := req["method"].(string)
	params, _ := req["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	// A notification (no id) needs no reply; answer 202 with no body.
	if !hasID {
		return mcpOutcome{status: 202, body: nil}
	}

	switch method {
	case "server/discover":
		return mcpOutcome{status: 200, body: rpcOK(id, map[string]any{
			"supportedVersions": mcpSupportedVersions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": mcpServerName, "version": mcpServerVersion},
		})}
	case "initialize":
		requested, _ := params["protocolVersion"].(string)
		negotiated := mcpVersion
		for _, v := range mcpSupportedVersions {
			if v == requested {
				negotiated = v
				break
			}
		}
		return mcpOutcome{status: 200, body: rpcOK(id, map[string]any{
			"protocolVersion": negotiated,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpServerName, "version": mcpServerVersion},
		})}
	case "tools/list":
		return mcpOutcome{status: 200, body: rpcOK(id, map[string]any{"tools": toolsList()})}
	case "tools/call":
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		// Accept a full_name ("owner/repo") when owner/repo are absent.
		if (args["owner"] == nil || args["repo"] == nil) {
			if fullName, ok := args["full_name"].(string); ok {
				parts := strings.Split(fullName, "/")
				if len(parts) == 2 {
					if args["owner"] == nil {
						args["owner"] = parts[0]
					}
					if args["repo"] == nil {
						args["repo"] = parts[1]
					}
				}
			}
		}
		result, err := callTool(s, name, args)
		if err != nil {
			if re, ok := err.(*rpcError); ok {
				return mcpOutcome{status: 200, body: rpcErr(id, re.code, re.message)}
			}
			return mcpOutcome{status: 200, body: rpcErr(id, -32603, err.Error())}
		}
		return mcpOutcome{status: 200, body: rpcOK(id, result)}
	case "ping":
		return mcpOutcome{status: 200, body: rpcOK(id, map[string]any{})}
	}

	return mcpOutcome{status: 200, body: rpcErr(id, -32601, fmt.Sprintf("Method not found: %s", method))}
}
