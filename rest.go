// REST v3 API (/api/v3): repositories, branches, commits, contents, labels, issues,
// comments, pull requests, files and merges. Every reply is JSON with a non-empty
// X-GitHub-Request-Id (added by app.go); errors carry {"message": ...}.
// Auth is enforced by app.go before this runs: a missing token never reaches here.
//
// Go port of rest.ts (v2, Node). Behavior-identical.

package main

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type restResult struct {
	status  int
	body    any
	headers map[string]string
}

func restOK(body any, status int, headers map[string]string) restResult {
	if status == 0 {
		status = 200
	}
	return restResult{status: status, body: body, headers: headers}
}

func restNotFound(message string) restResult {
	if message == "" {
		message = "Not Found"
	}
	return restResult{status: 404, body: map[string]any{"message": message}}
}

func restUnprocessable(message string) restResult {
	return restResult{status: 422, body: map[string]any{"message": message}}
}

// ---------------------------------------------------------------- shapes

func repoJSON(r *repo) map[string]any {
	openIssues := 0
	for _, i := range r.issues {
		if i.state == "open" {
			openIssues++
		}
	}
	return map[string]any{
		"id":               r.id,
		"node_id":          globalID("Repository", r.id),
		"name":             r.name,
		"full_name":        r.owner + "/" + r.name,
		"owner":            viewerUser(),
		"private":          r.isPrivate,
		"description":      r.description,
		"default_branch":   r.defaultBranch,
		"created_at":       r.createdAt,
		"updated_at":       r.updatedAt,
		"pushed_at":        r.updatedAt,
		"html_url":         "https://example.invalid/" + r.owner + "/" + r.name,
		"url":              "/api/v3/repos/" + r.owner + "/" + r.name,
		"has_issues":       true,
		"has_projects":     true,
		"has_wiki":         true,
		"has_pages":        false,
		"has_downloads":    true,
		"archived":         false,
		"disabled":         false,
		"visibility":       map[bool]string{true: "private", false: "public"}[r.isPrivate],
		"fork":             false,
		"forks_count":      0,
		"stargazers_count": 0,
		"watchers_count":   0,
		"open_issues_count": openIssues,
		"license":          nil,
		"topics":           []any{},
	}
}

func labelJSON(r *repo, name string) map[string]any {
	l := r.labels[name]
	id := 0
	color := "ededed"
	if l != nil {
		id = l.id
		color = l.color
	}
	return map[string]any{
		"id":          id,
		"node_id":     globalID("Label", id),
		"name":        name,
		"color":       color,
		"description": "",
		"default":     false,
	}
}

func issueJSON(r *repo, is *issue) map[string]any {
	labels := make([]any, 0, len(is.labels))
	for _, name := range is.labels {
		labels = append(labels, labelJSON(r, name))
	}
	var closedAt any
	if is.state == "closed" {
		closedAt = is.updatedAt
	}
	return map[string]any{
		"id":         is.id,
		"node_id":    globalID("Issue", is.id),
		"number":     is.number,
		"title":      is.title,
		"body":       is.body,
		"state":      is.state,
		"labels":     labels,
		"user":       viewerUser(),
		"assignee":   nil,
		"assignees":  []any{},
		"locked":     false,
		"comments":   len(r.comments[is.number]),
		"created_at": is.createdAt,
		"updated_at": is.updatedAt,
		"closed_at":  closedAt,
		"html_url":   "https://example.invalid/" + r.owner + "/" + r.name + "/issues/" + strconv.Itoa(is.number),
		"url":        "/api/v3/repos/" + r.owner + "/" + r.name + "/issues/" + strconv.Itoa(is.number),
	}
}

func refJSON(r *repo, ref, sha string) map[string]any {
	return map[string]any{
		"label": r.owner + ":" + ref,
		"ref":   ref,
		"sha":   sha,
		"user":  viewerUser(),
		"repo":  repoJSON(r),
	}
}

func pullJSON(r *repo, p *pull) map[string]any {
	var closedAt any
	if p.state == "closed" {
		closedAt = p.updatedAt
	}
	var mergedAt any
	if p.mergedAt != "" {
		mergedAt = p.mergedAt
	}
	var mergeSha any
	if p.mergeSha != "" {
		mergeSha = p.mergeSha
	}
	return map[string]any{
		"id":               p.id,
		"node_id":          globalID("PullRequest", p.id),
		"number":           p.number,
		"title":            p.title,
		"body":             p.body,
		"state":            p.state,
		"merged":           p.merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSha,
		"mergeable":        true,
		"head":             refJSON(r, p.headRef, p.headSha),
		"base":             refJSON(r, p.baseRef, p.baseSha),
		"user":             viewerUser(),
		"assignee":         nil,
		"assignees":        []any{},
		"locked":           false,
		"created_at":       p.createdAt,
		"updated_at":       p.updatedAt,
		"closed_at":        closedAt,
		"html_url":         "https://example.invalid/" + r.owner + "/" + r.name + "/pull/" + strconv.Itoa(p.number),
		"url":              "/api/v3/repos/" + r.owner + "/" + r.name + "/pulls/" + strconv.Itoa(p.number),
	}
}

func formatISO(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// JS String(v) for JSON-decoded values (used for label lists).
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

var authorLineRe = regexp.MustCompile(`^(.*) <([^>]*)> (\d+) ([+-]\d{4})$`)

func commitJSON(r *repo, sha string) map[string]any {
	obj := getObject(r, sha)
	if obj == nil || obj.typ != objCommit {
		return nil
	}
	c := parseCommit(obj.data)
	authorOf := func(line string) map[string]any {
		m := authorLineRe.FindStringSubmatch(line)
		if m == nil {
			return map[string]any{"name": viewerName, "email": viewerEmail, "date": r.createdAt}
		}
		epoch, _ := strconv.ParseInt(m[3], 10, 64)
		return map[string]any{
			"name":  m[1],
			"email": m[2],
			"date":  formatISO(epoch),
		}
	}
	parents := make([]any, 0, len(c.parents))
	for _, p := range c.parents {
		parents = append(parents, map[string]any{"sha": p})
	}
	message := c.message
	// GitHub presents the message without the trailing newline that the
	// stored commit object carries.
	if strings.HasSuffix(message, "\r\n") {
		message = message[:len(message)-2]
	} else if strings.HasSuffix(message, "\n") {
		message = message[:len(message)-1]
	}
	return map[string]any{
		"sha":     sha,
		"node_id": globalID("Commit", 0) + sha[:8],
		"commit": map[string]any{
			"message":   message,
			"author":    authorOf(c.author),
			"committer": authorOf(c.committer),
			"tree":      map[string]any{"sha": c.tree},
		},
		"author":    map[string]any{"login": viewerLogin, "id": viewerID},
		"committer": map[string]any{"login": viewerLogin, "id": viewerID},
		"parents":   parents,
	}
}

// ---------------------------------------------------------------- helpers

func parsePage(query url.Values) (page, perPage float64) {
	page = 1
	if v := query.Get("page"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f != 0 {
			page = f
		}
	}
	if page < 1 {
		page = 1
	}
	perPage = 30
	if v := query.Get("per_page"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f != 0 {
			perPage = f
		}
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	return page, perPage
}

func pageOf[T any](items []T, page, perPage float64) []T {
	start := int((page - 1) * perPage)
	if start < 0 {
		start = 0
	}
	if start >= len(items) {
		return []T{}
	}
	end := int(page * perPage)
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

// linkHeader rebuilds the query preserving the caller's parameter order
// (URLSearchParams.set keeps a key's position), like the TS original.
func linkHeader(origin, path, rawQuery string, page, perPage float64, hasNext bool) map[string]string {
	if !hasNext {
		return nil
	}
	type kv struct{ k, v string }
	var pairs []kv
	if rawQuery != "" {
		for _, part := range strings.Split(rawQuery, "&") {
			if part == "" {
				continue
			}
			k, v, _ := strings.Cut(part, "=")
			dk, _ := url.QueryUnescape(strings.ReplaceAll(k, "+", " "))
			dv, _ := url.QueryUnescape(strings.ReplaceAll(v, "+", " "))
			pairs = append(pairs, kv{dk, dv})
		}
	}
	set := func(key, val string) {
		for i, p := range pairs {
			if p.k == key {
				pairs[i].v = val
				return
			}
		}
		pairs = append(pairs, kv{key, val})
	}
	set("page", strconv.FormatFloat(page+1, 'f', -1, 64))
	set("per_page", strconv.FormatFloat(perPage, 'f', -1, 64))
	var q strings.Builder
	for i, p := range pairs {
		if i > 0 {
			q.WriteByte('&')
		}
		q.WriteString(url.QueryEscape(p.k))
		q.WriteByte('=')
		q.WriteString(url.QueryEscape(p.v))
	}
	return map[string]string{"link": "<" + origin + path + "?" + q.String() + `>; rel="next"`}
}

func getRepoOr404(s *store, owner, name string) (*repo, *restResult) {
	r := s.viewerRepo(owner, name)
	if r == nil {
		res := restNotFound("")
		return nil, &res
	}
	return r, nil
}

func getIssueOr404(r *repo, n int) (*issue, *restResult) {
	for _, is := range r.issues {
		if is.number == n {
			return is, nil
		}
	}
	res := restNotFound("Issue not found")
	return nil, &res
}

func getPullOr404(r *repo, n int) (*pull, *restResult) {
	for _, p := range r.pulls {
		if p.number == n {
			return p, nil
		}
	}
	res := restNotFound("Pull request not found")
	return nil, &res
}

// Line counts for a changed file: multiset difference of lines.
func lineCounts(oldContent, newContent []byte) (additions, deletions int) {
	linesOf := func(b []byte) []string {
		if b == nil {
			return []string{}
		}
		return strings.Split(string(b), "\n")
	}
	count := func(lines []string) map[string]int {
		m := make(map[string]int)
		for _, l := range lines {
			m[l]++
		}
		return m
	}
	oldLines := count(linesOf(oldContent))
	newLines := count(linesOf(newContent))
	for line, n := range newLines {
		if d := n - oldLines[line]; d > 0 {
			additions += d
		}
	}
	for line, n := range oldLines {
		if d := n - newLines[line]; d > 0 {
			deletions += d
		}
	}
	return additions, deletions
}

func blobContent(r *repo, oid string) []byte {
	if oid == "" {
		return nil
	}
	obj := getObject(r, oid)
	if obj == nil || obj.typ != objBlob {
		return nil
	}
	return obj.data
}

// ---------------------------------------------------------------- the router

func handleRest(s *store, method string, segments []string, query url.Values, rawQuery string, body map[string]any, origin string) restResult {
	path := "/api/v3/" + strings.Join(segments, "/")
	b := body
	if b == nil {
		b = map[string]any{}
	}

	// GET /api/v3/ — the API root.
	if len(segments) == 0 {
		if method != "GET" {
			return restNotFound("")
		}
		return restOK(
			map[string]any{"current_user_url": "/api/v3/user", "rate_limit_url": "/api/v3/rate_limit"},
			200,
			map[string]string{"x-oauth-scopes": "repo, read:org"},
		)
	}

	// GET /api/v3/meta
	if len(segments) == 1 && segments[0] == "meta" {
		if method != "GET" {
			return restNotFound("")
		}
		return restOK(map[string]any{"installed_version": "1.0.0"}, 200, nil)
	}

	// GET /api/v3/user
	if len(segments) == 1 && segments[0] == "user" {
		if method != "GET" {
			return restNotFound("")
		}
		return restOK(viewerUser(), 200, nil)
	}

	// /api/v3/user/repos
	if len(segments) == 2 && segments[0] == "user" && segments[1] == "repos" {
		if method == "GET" {
			page, perPage := parsePage(query)
			all := s.viewerRepos()
			items := pageOf(all, page, perPage)
			hasNext := page*perPage < float64(len(all))
			out := make([]any, 0, len(items))
			for _, r := range items {
				out = append(out, repoJSON(r))
			}
			return restOK(out, 200, linkHeader(origin, path, rawQuery, page, perPage, hasNext))
		}
		if method == "POST" {
			name, _ := b["name"].(string)
			name = strings.TrimSpace(name)
			if name == "" {
				return restUnprocessable("name is required")
			}
			if s.getRepo(viewerLogin, name) != nil {
				return restUnprocessable("Repository already exists")
			}
			priv, _ := b["private"].(bool)
			desc, _ := b["description"].(string)
			r := s.createRepo(name, priv, desc)
			return restOK(repoJSON(r), 201, nil)
		}
		return restNotFound("")
	}

	// /api/v3/repos/{owner}/{repo}...
	if len(segments) >= 3 && segments[0] == "repos" {
		owner, _ := url.PathUnescape(segments[1])
		name, _ := url.PathUnescape(segments[2])
		rest := segments[3:]
		r, notFound := getRepoOr404(s, owner, name)
		if notFound != nil {
			return *notFound
		}

		if len(rest) == 0 {
			if method == "GET" {
				return restOK(repoJSON(r), 200, nil)
			}
			if method == "PATCH" {
				if desc, ok := b["description"].(string); ok {
					r.description = desc
				}
				if newName, ok := b["name"].(string); ok && strings.TrimSpace(newName) != "" && newName != r.name {
					s.deleteRepo(owner, r.name)
					r.name = strings.TrimSpace(newName)
					s.repos[s.key(owner, r.name)] = r
				}
				r.updatedAt = s.nowIso()
				return restOK(repoJSON(r), 200, nil)
			}
			if method == "DELETE" {
				s.deleteRepo(owner, name)
				return restResult{status: 204, body: nil}
			}
			return restNotFound("")
		}

		// POST /git/refs
		if len(rest) == 2 && rest[0] == "git" && rest[1] == "refs" {
			if method != "POST" {
				return restNotFound("")
			}
			ref, _ := b["ref"].(string)
			sha, _ := b["sha"].(string)
			sha = strings.ToLower(sha)
			if !strings.HasPrefix(ref, "refs/") || !isOID(sha) {
				return restUnprocessable("Invalid ref or sha")
			}
			r.refs[ref] = sha
			nodeID := globalID("Ref", 0) + nonWordRe.ReplaceAllString(ref, "")
			return restOK(map[string]any{"ref": ref, "node_id": nodeID, "object": map[string]any{"sha": sha, "type": "commit"}, "url": ""}, 201, nil)
		}

		// GET /branches
		if len(rest) == 1 && rest[0] == "branches" {
			if method != "GET" {
				return restNotFound("")
			}
			keys := make([]string, 0, len(r.refs))
			for ref := range r.refs {
				keys = append(keys, ref)
			}
			sort.Strings(keys)
			var branches []any
			for _, ref := range keys {
				if !strings.HasPrefix(ref, "refs/heads/") {
					continue
				}
				branches = append(branches, map[string]any{
					"name":      ref[len("refs/heads/"):],
					"commit":    map[string]any{"sha": r.refs[ref]},
					"protected": false,
				})
			}
			if branches == nil {
				branches = []any{}
			}
			return restOK(branches, 200, nil)
		}

		// GET /commits
		if len(rest) == 1 && rest[0] == "commits" {
			if method != "GET" {
				return restNotFound("")
			}
			shaParam := query.Get("sha")
			var tip string
			var hasTip bool
			if shaParam != "" {
				if v, ok := r.refs["refs/heads/"+shaParam]; ok {
					tip, hasTip = v, true
				} else if isOID(shaParam) {
					tip, hasTip = shaParam, true
				}
			}
			var start string
			var hasStart bool
			if hasTip {
				start, hasStart = tip, true
			} else if v, ok := r.refs["refs/heads/"+r.defaultBranch]; ok {
				start, hasStart = v, true
			}
			seen := make(map[string]bool)
			var stack []string
			if hasStart {
				stack = append(stack, start)
			}
			var commits []string
			for len(stack) > 0 {
				cur := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if seen[cur] {
					continue
				}
				seen[cur] = true
				obj := getObject(r, cur)
				if obj == nil || obj.typ != objCommit {
					continue
				}
				commits = append(commits, cur)
				for _, p := range parseCommit(obj.data).parents {
					stack = append(stack, p)
				}
			}
			sort.Slice(commits, func(i, j int) bool {
				return r.commitSeq[commits[j]] < r.commitSeq[commits[i]]
			})
			page, perPage := parsePage(query)
			var out []any
			for _, sh := range pageOf(commits, page, perPage) {
				if cj := commitJSON(r, sh); cj != nil {
					out = append(out, cj)
				}
			}
			if out == nil {
				out = []any{}
			}
			return restOK(out, 200, nil)
		}

		// /contents/{path...}
		if len(rest) >= 1 && rest[0] == "contents" {
			parts := make([]string, 0, len(rest)-1)
			for _, seg := range rest[1:] {
				d, _ := url.PathUnescape(seg)
				parts = append(parts, d)
			}
			return handleContents(s, r, method, strings.Join(parts, "/"), query, b)
		}

		// POST /labels
		if len(rest) == 1 && rest[0] == "labels" {
			if method != "POST" {
				return restNotFound("")
			}
			labelName, _ := b["name"].(string)
			labelName = strings.TrimSpace(labelName)
			if labelName == "" {
				return restUnprocessable("name is required")
			}
			color, _ := b["color"].(string)
			if color == "" {
				color = "ededed"
			}
			if _, ok := r.labels[labelName]; !ok {
				r.labels[labelName] = &label{id: r.nextLabelID, name: labelName, color: color}
				r.nextLabelID++
			}
			return restOK(labelJSON(r, labelName), 201, nil)
		}

		// /issues...
		if len(rest) >= 1 && rest[0] == "issues" {
			return handleIssues(s, r, method, rest[1:], query, b)
		}

		// /pulls...
		if len(rest) >= 1 && rest[0] == "pulls" {
			return handlePulls(s, r, method, rest[1:], query, b)
		}

		return restNotFound("")
	}

	return restNotFound("")
}

var nonWordRe = regexp.MustCompile(`\W`)

// ---------------------------------------------------------------- contents

func handleContents(s *store, r *repo, method, path string, query url.Values, b map[string]any) restResult {
	refName := query.Get("ref")
	if refName == "" {
		refName = r.defaultBranch
	}
	tip := r.refs["refs/heads/"+refName]
	treeOfTip := func(oid string, has bool) string {
		if !has {
			return ""
		}
		obj := getObject(r, oid)
		if obj == nil || obj.typ != objCommit {
			return ""
		}
		return parseCommit(obj.data).tree
	}

	if method == "GET" {
		_, hasTip := r.refs["refs/heads/"+refName]
		tree := treeOfTip(tip, hasTip)
		if tree == "" {
			return restNotFound("Not Found")
		}
		entry := treeLookup(r, tree, path)
		if entry == nil {
			// Maybe a directory: list its entries.
			dirEntries := listDir(r, tree, path)
			if dirEntries == nil {
				return restNotFound("Not Found")
			}
			return restOK(dirEntries, 200, nil)
		}
		if entry.mode == "40000" {
			dirEntries := listDir(r, tree, path)
			if dirEntries == nil {
				return restNotFound("Not Found")
			}
			return restOK(dirEntries, 200, nil)
		}
		blob := getObject(r, entry.oid)
		if blob == nil {
			return restNotFound("Not Found")
		}
		name := path
		if idx := strings.LastIndex(path, "/"); idx >= 0 {
			name = path[idx+1:]
		}
		return restOK(map[string]any{
			"type":     "file",
			"encoding": "base64",
			"size":     len(blob.data),
			"name":     name,
			"path":     path,
			"content":  base64.StdEncoding.EncodeToString(blob.data),
			"sha":      entry.oid,
		}, 200, nil)
	}

	if method == "PUT" {
		message, _ := b["message"].(string)
		if message == "" {
			message = "Update file"
		}
		contentB64, _ := b["content"].(string)
		// Node's base64 decoder silently skips non-alphabet bytes and tolerates
		// missing padding; mirror that.
		filtered := strings.Map(func(ch rune) rune {
			if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '+' || ch == '/' || ch == '=' {
				return ch
			}
			return -1
		}, contentB64)
		filtered = strings.TrimRight(filtered, "=")
		content, err := base64.RawStdEncoding.DecodeString(filtered)
		if err != nil {
			return restUnprocessable("Invalid content encoding")
		}
		branch, _ := b["branch"].(string)
		if branch == "" {
			branch = r.defaultBranch
		}
		branchRef := "refs/heads/" + branch
		branchTip, hasBranchTip := r.refs[branchRef]
		if !hasBranchTip {
			// A new branch starts at the default branch tip.
			branchTip, hasBranchTip = r.refs["refs/heads/"+r.defaultBranch]
			if hasBranchTip {
				r.refs[branchRef] = branchTip
			}
		}
		curTree := treeOfTip(branchTip, hasBranchTip)
		var curEntry *treeEntry
		if curTree != "" {
			curEntry = treeLookup(r, curTree, path)
		}
		sha, _ := b["sha"].(string)
		sha = strings.ToLower(sha)
		hasSha := false
		if _, ok := b["sha"].(string); ok {
			hasSha = true
		}
		name := path
		if idx := strings.LastIndex(path, "/"); idx >= 0 {
			name = path[idx+1:]
		}
		if curEntry != nil {
			// Update: a sha is required and must match the current blob.
			if hasSha && sha != curEntry.oid {
				return restResult{status: 409, body: map[string]any{"message": "sha does not match the current blob"}}
			}
			commitOID, blobOID := commitOnBranch(s, r, branch, path, content, message)
			return restOK(map[string]any{
				"content": map[string]any{"sha": blobOID, "encoding": "base64", "size": len(content), "name": name, "path": path},
				"commit":  map[string]any{"sha": commitOID},
			}, 200, nil)
		}
		commitOID, blobOID := commitOnBranch(s, r, branch, path, content, message)
		return restOK(map[string]any{
			"content": map[string]any{"sha": blobOID, "encoding": "base64", "size": len(content), "name": name, "path": path},
			"commit":  map[string]any{"sha": commitOID},
		}, 201, nil)
	}

	if method == "DELETE" {
		branch, _ := b["branch"].(string)
		if branch == "" {
			branch = r.defaultBranch
		}
		branchTip, hasBranchTip := r.refs["refs/heads/"+branch]
		curTree := treeOfTip(branchTip, hasBranchTip)
		var curEntry *treeEntry
		if curTree != "" {
			curEntry = treeLookup(r, curTree, path)
		}
		if curEntry == nil {
			return restNotFound("Not Found")
		}
		sha, _ := b["sha"].(string)
		sha = strings.ToLower(sha)
		if _, ok := b["sha"].(string); ok && sha != curEntry.oid {
			return restResult{status: 409, body: map[string]any{"message": "sha does not match the current blob"}}
		}
		message, _ := b["message"].(string)
		if message == "" {
			message = "Delete file"
		}
		commitOID, _ := commitOnBranch(s, r, branch, path, nil, message)
		return restOK(map[string]any{"commit": map[string]any{"sha": commitOID}}, 200, nil)
	}

	return restNotFound("")
}

// List a directory's entries at path ('' = root), or nil when it is not a directory.
func listDir(r *repo, treeOID, path string) []any {
	parts := splitPath(path)
	cur := treeOID
	for _, part := range parts {
		obj := getObject(r, cur)
		if obj == nil || obj.typ != objTree {
			return nil
		}
		var entry *treeEntry
		for _, e := range parseTree(obj.data) {
			if e.name == part {
				ec := e
				entry = &ec
				break
			}
		}
		if entry == nil || entry.mode != "40000" {
			return nil
		}
		cur = entry.oid
	}
	obj := getObject(r, cur)
	if obj == nil || obj.typ != objTree {
		return nil
	}
	out := make([]any, 0)
	for _, e := range parseTree(obj.data) {
		typ := "file"
		if e.mode == "40000" {
			typ = "dir"
		}
		size := 0
		if e.mode != "40000" {
			if o := getObject(r, e.oid); o != nil {
				size = len(o.data)
			}
		}
		p := e.name
		if path != "" {
			p = path + "/" + e.name
		}
		out = append(out, map[string]any{
			"type": typ,
			"name": e.name,
			"path": p,
			"sha":  e.oid,
			"size": size,
		})
	}
	return out
}

// ---------------------------------------------------------------- issues

func handleIssues(s *store, r *repo, method string, rest []string, query url.Values, b map[string]any) restResult {
	if len(rest) == 0 {
		if method == "GET" {
			state := query.Get("state")
			if state == "" {
				state = "open"
			}
			sortBy := query.Get("sort")
			if sortBy == "" {
				sortBy = "created"
			}
			direction := query.Get("direction")
			if direction == "" {
				direction = "desc"
			}
			items := make([]*issue, 0, len(r.issues))
			for _, is := range r.issues {
				if state != "all" && is.state != state {
					continue
				}
				items = append(items, is)
			}
			if sortBy == "created" {
				if direction == "asc" {
					sort.Slice(items, func(i, j int) bool { return items[i].seq < items[j].seq })
				} else {
					sort.Slice(items, func(i, j int) bool { return items[i].seq > items[j].seq })
				}
			}
			page, perPage := parsePage(query)
			out := make([]any, 0)
			for _, is := range pageOf(items, page, perPage) {
				out = append(out, issueJSON(r, is))
			}
			return restOK(out, 200, nil)
		}
		if method == "POST" {
			title, _ := b["title"].(string)
			bodyText, _ := b["body"].(string)
			var labels []string
			if arr, ok := b["labels"].([]any); ok {
				for _, v := range arr {
					labels = append(labels, jsString(v))
				}
			}
			for _, lbl := range labels {
				if _, ok := r.labels[lbl]; !ok {
					return restUnprocessable(fmt.Sprintf("Label %q does not exist", lbl))
				}
			}
			now := s.nowIso()
			is := &issue{
				id:        s.nextIssueID,
				number:    r.nextIssue,
				title:     title,
				body:      bodyText,
				state:     "open",
				labels:    labels,
				seq:       len(r.issues) + 1,
				createdAt: now,
				updatedAt: now,
			}
			s.nextIssueID++
			r.nextIssue++
			r.issues = append(r.issues, is)
			return restOK(issueJSON(r, is), 201, nil)
		}
		return restNotFound("")
	}

	n, err := strconv.Atoi(rest[0])
	if err != nil || n < 1 {
		return restNotFound("")
	}
	if len(rest) == 1 {
		found, notFound := getIssueOr404(r, n)
		if notFound != nil {
			return *notFound
		}
		if method == "GET" {
			return restOK(issueJSON(r, found), 200, nil)
		}
		if method == "PATCH" {
			if st, ok := b["state"].(string); ok && (st == "open" || st == "closed") {
				found.state = st
			}
			if t, ok := b["title"].(string); ok {
				found.title = t
			}
			if bo, ok := b["body"].(string); ok {
				found.body = bo
			}
			found.updatedAt = s.nowIso()
			return restOK(issueJSON(r, found), 200, nil)
		}
		return restNotFound("")
	}

	if len(rest) == 2 && rest[1] == "comments" {
		_, notFound := getIssueOr404(r, n)
		if notFound != nil {
			return *notFound
		}
		if method == "GET" {
			list := r.comments[n]
			out := make([]any, 0, len(list))
			for _, c := range list {
				out = append(out, map[string]any{
					"id":         c.id,
					"node_id":    globalID("IssueComment", c.id),
					"body":       c.body,
					"user":       viewerUser(),
					"created_at": c.createdAt,
					"updated_at": c.updatedAt,
				})
			}
			return restOK(out, 200, nil)
		}
		if method == "POST" {
			bodyText, _ := b["body"].(string)
			now := s.nowIso()
			c := &comment{id: s.nextCommentID, body: bodyText, seq: len(r.comments[n]) + 1, createdAt: now, updatedAt: now}
			s.nextCommentID++
			r.comments[n] = append(r.comments[n], c)
			return restOK(map[string]any{
				"id":         c.id,
				"node_id":    globalID("IssueComment", c.id),
				"body":       c.body,
				"user":       viewerUser(),
				"created_at": c.createdAt,
				"updated_at": c.updatedAt,
			}, 201, nil)
		}
		return restNotFound("")
	}

	return restNotFound("")
}

// ---------------------------------------------------------------- pulls

func handlePulls(s *store, r *repo, method string, rest []string, query url.Values, b map[string]any) restResult {
	if len(rest) == 0 {
		if method == "GET" {
			state := query.Get("state")
			if state == "" {
				state = "open"
			}
			items := make([]*pull, 0, len(r.pulls))
			for _, p := range r.pulls {
				if state != "all" && p.state != state {
					continue
				}
				items = append(items, p)
			}
			sort.Slice(items, func(i, j int) bool { return items[i].number < items[j].number })
			page, perPage := parsePage(query)
			out := make([]any, 0)
			for _, p := range pageOf(items, page, perPage) {
				out = append(out, pullJSON(r, p))
			}
			return restOK(out, 200, nil)
		}
		if method == "POST" {
			title, _ := b["title"].(string)
			bodyText, _ := b["body"].(string)
			head, _ := b["head"].(string)
			base, _ := b["base"].(string)
			if base == "" {
				base = r.defaultBranch
			}
			headSha, hasHead := r.refs["refs/heads/"+head]
			baseSha, hasBase := r.refs["refs/heads/"+base]
			if !hasHead {
				return restUnprocessable(fmt.Sprintf("Head branch %q does not exist", head))
			}
			if !hasBase {
				return restUnprocessable(fmt.Sprintf("Base branch %q does not exist", base))
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
			return restOK(pullJSON(r, p), 201, nil)
		}
		return restNotFound("")
	}

	n, err := strconv.Atoi(rest[0])
	if err != nil || n < 1 {
		return restNotFound("")
	}
	found, notFound := getPullOr404(r, n)
	if notFound != nil {
		return *notFound
	}
	p := found

	if len(rest) == 1 {
		if method == "GET" {
			return restOK(pullJSON(r, p), 200, nil)
		}
		return restNotFound("")
	}

	if len(rest) == 2 && rest[1] == "files" {
		if method != "GET" {
			return restNotFound("")
		}
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
		var files []any
		for _, d := range diffs {
			if d.status == "removed" {
				continue
			}
			additions, deletions := lineCounts(blobContent(r, d.oldOID), blobContent(r, d.newOID))
			status := "modified"
			if d.status == "added" {
				status = "added"
			}
			files = append(files, map[string]any{
				"filename":  d.path,
				"status":    status,
				"additions": additions,
				"deletions": deletions,
				"changes":   additions + deletions,
				"sha":       d.newOID,
			})
		}
		if files == nil {
			files = []any{}
		}
		return restOK(files, 200, nil)
	}

	if len(rest) == 2 && rest[1] == "merge" {
		if method != "PUT" {
			return restNotFound("")
		}
		if p.merged || p.state == "closed" {
			return restResult{status: 405, body: map[string]any{"message": "Pull request is already merged or closed"}}
		}
		merged := doMergePull(s, r, p)
		if !merged {
			return restResult{status: 405, body: map[string]any{"message": "Merge conflict: the pull request cannot be merged"}}
		}
		return restOK(map[string]any{"sha": p.mergeSha, "merged": true, "message": "Pull request successfully merged"}, 200, nil)
	}

	return restNotFound("")
}

// Squash-merge a pull request: one commit on the base branch whose tree is the head tip's
// tree. Returns false when the pull cannot merge (missing head/base, or a file changed on
// both sides of the merge-base to different content). Shared by REST and GraphQL.
func doMergePull(s *store, r *repo, p *pull) bool {
	if p.merged || p.state == "closed" {
		return false
	}
	// The base is the CURRENT tip of the base branch, not the sha recorded when the
	// pull was created: the branch may have advanced since (e.g. another PR merged
	// first), and a conflict is judged against that current tip.
	currentBaseSha, ok := r.refs["refs/heads/"+p.baseRef]
	if !ok {
		currentBaseSha = p.baseSha
	}
	headObj := getObject(r, p.headSha)
	baseObj := getObject(r, currentBaseSha)
	if headObj == nil || baseObj == nil {
		return false
	}
	headTree := parseCommit(headObj.data).tree
	baseTree := parseCommit(baseObj.data).tree
	base := mergeBase(r, p.headSha, currentBaseSha)
	baseOfMerge := ""
	if base != "" {
		if o := getObject(r, base); o != nil {
			baseOfMerge = parseCommit(o.data).tree
		}
	}
	// Conflict: a file changed on both sides of the merge-base to different content.
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
		var baseOID, headOID string
		if e := treeLookup(r, baseTree, bc.path); e != nil {
			baseOID = e.oid
		}
		if e := treeLookup(r, headTree, bc.path); e != nil {
			headOID = e.oid
		}
		if baseOID != headOID {
			return false
		}
	}
	line, _ := identLine(s)
	message := fmt.Sprintf("Merge pull request #%d from %s/%s\n\n%s\n", p.number, r.owner, p.headRef, p.title)
	commitData := buildCommit(headTree, []string{currentBaseSha}, line, message)
	mergeSha := storeObject(r, objCommit, commitData)
	if _, ok := r.commitSeq[mergeSha]; !ok {
		r.commitSeq[mergeSha] = len(r.commitSeq) + 1
	}
	r.refs["refs/heads/"+p.baseRef] = mergeSha
	p.state = "closed"
	p.merged = true
	p.mergedAt = s.nowIso()
	p.mergeSha = mergeSha
	p.updatedAt = p.mergedAt
	return true
}
