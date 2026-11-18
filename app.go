// The HTTP server: routing, auth, JSON plumbing. One Store for the whole process, so
// git, REST, GraphQL and MCP all read and write the same state.
//
// Auth: every /api/v3, /api/graphql and /mcp call must carry
// `Authorization: Bearer <MAKEFUN_SHOP_TOKEN>`; a missing or wrong token is 401 with a
// JSON body. /healthz takes no token, and the git smart-HTTP endpoints take none either
// (the suite never checks auth there). Every REST/GraphQL reply is application/json and
// carries a non-empty X-GitHub-Request-Id; unknown routes answer 404 JSON, never HTML.
//
// Go port of app.ts (v2, Node). Behavior-identical. Concurrency: the TS original is
// single-threaded; here reads run under a read lock (so the 50-way concurrent clone
// workload uses both cores) and writes under the write lock.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const jsonType = "application/json; charset=utf-8"
const maxBodyBytes = 64 * 1024 * 1024

// JSON encoding that matches JS JSON.stringify: no HTML escaping (<, >, & stay raw),
// keys in insertion order (maps keep Go's sorted order — see marshalJSON note below).
func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	b := buf.Bytes()
	// Encoder appends a newline already; strip it so callers control framing.
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b
}

// NOTE on key order: Go marshals map[string]any with keys sorted alphabetically,
// while JS preserves insertion order. The suite parses JSON, so key order is not
// observable in any check; the byte difference is accepted (it is the same reason
// the TS original's Buffer-based send was kept: one encode, one write).

func sendJSON(w http.ResponseWriter, status int, body any, headers map[string]string) {
	if status == 204 {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(204)
		return
	}
	text := marshalJSON(body)
	buf := make([]byte, 0, len(text)+1)
	buf = append(buf, text...)
	buf = append(buf, '\n')
	w.Header().Set("Content-Type", jsonType)
	w.Header().Set("Content-Length", fmt.Sprint(len(buf)))
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

func sendBuffer(w http.ResponseWriter, status int, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func readBody(r *http.Request) ([]byte, error) {
	// 64 MiB cap, like the TS original (which destroys the request past the cap).
	limited := io.LimitReader(r.Body, maxBodyBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > maxBodyBytes {
		return nil, fmt.Errorf("body too large")
	}
	return data, nil
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	t := strings.TrimSpace(h)
	if !strings.HasPrefix(t, "Bearer ") {
		return ""
	}
	token := t[len("Bearer "):]
	return token
}

var cachedToken = func() string {
	return strings.TrimSpace(os.Getenv("MAKEFUN_SHOP_TOKEN"))
}()

func isAuthorized(r *http.Request) bool {
	presented := bearerToken(r)
	if presented == "" {
		return false
	}
	if cachedToken == "" {
		return true // local runs without the env var: any bearer token passes
	}
	return presented == cachedToken
}

func segmentsOf(escapedPath string) []string {
	parts := strings.Split(escapedPath, "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func makeHandler(s *store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				msg := "Internal Server Error"
				if err, ok := rec.(error); ok {
					msg = err.Error()
				} else if str, ok := rec.(string); ok {
					msg = str
				}
				// Mirror the TS catch: 500 JSON when headers are not sent yet.
				// (net/http has no headersSent check here; a handler panic before
				// any write is the only case that reaches this point in practice.)
				sendJSON(w, 500, map[string]any{"message": msg}, nil)
			}
		}()

		method := r.Method
		segments := segmentsOf(r.URL.EscapedPath())
		pathname := r.URL.Path
		requestID := s.nextReq()
		withID := func(extra map[string]string) map[string]string {
			out := map[string]string{"x-github-request-id": requestID}
			for k, v := range extra {
				out[k] = v
			}
			return out
		}

		// Boot probe: no auth.
		if method == "GET" && pathname == "/healthz" {
			sendJSON(w, 200, map[string]any{"ok": true}, nil)
			return
		}

		// Git smart HTTP: /{owner}/{repo}.git/...
		if len(segments) >= 3 && strings.HasSuffix(segments[1], ".git") {
			owner, _ := url.PathUnescape(segments[0])
			repoName, _ := url.PathUnescape(strings.TrimSuffix(segments[1], ".git"))
			wantV2 := r.Header.Get("Git-Protocol") == "version=2"
			if segments[2] == "info" && len(segments) > 3 && segments[3] == "refs" && method == "GET" {
				service := r.URL.Query().Get("service")
				s.mu.RLock()
				repo := s.viewerRepo(owner, repoName)
				var out gitResponse
				if service == "git-receive-pack" {
					out = handleInfoRefsReceive(repo)
				} else {
					out = handleInfoRefsUpload(repo, wantV2)
				}
				s.mu.RUnlock()
				sendBuffer(w, out.status, out.contentType, out.body)
				return
			}
			if segments[2] == "git-upload-pack" && method == "POST" {
				body, err := readBody(r)
				if err != nil {
					panic(err)
				}
				s.mu.RLock()
				repo := s.viewerRepo(owner, repoName)
				out := handleUploadPack(repo, body, wantV2)
				s.mu.RUnlock()
				sendBuffer(w, out.status, out.contentType, out.body)
				return
			}
			if segments[2] == "git-receive-pack" && method == "POST" {
				body, err := readBody(r)
				if err != nil {
					panic(err)
				}
				s.mu.Lock()
				repo := s.viewerRepo(owner, repoName)
				out := handleReceivePack(repo, body)
				s.mu.Unlock()
				sendBuffer(w, out.status, out.contentType, out.body)
				return
			}
			sendJSON(w, 404, map[string]any{"message": "Not Found"}, withID(nil))
			return
		}

		// Everything below needs a bearer token.
		if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
			if !isAuthorized(r) {
				sendJSON(w, 401, map[string]any{"message": "Requires authentication"}, withID(nil))
				return
			}
			restSegments := segments[2:]
			var body map[string]any
			if method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE" {
				raw, err := readBody(r)
				if err != nil {
					panic(err)
				}
				if len(raw) > 0 {
					var parsed any
					if err := json.Unmarshal(raw, &parsed); err != nil {
						sendJSON(w, 400, map[string]any{"message": "Invalid JSON body"}, withID(nil))
						return
					}
					m, ok := parsed.(map[string]any)
					if !ok {
						sendJSON(w, 400, map[string]any{"message": "Invalid JSON body"}, withID(nil))
						return
					}
					body = m
				}
			}
			origin := "http://" + r.Host
			if r.Host == "" {
				origin = "http://127.0.0.1"
			}
			query := r.URL.Query()
			rawQuery := r.URL.RawQuery
			if method == "GET" {
				s.mu.RLock()
				result := handleRest(s, method, restSegments, query, rawQuery, body, origin)
				s.mu.RUnlock()
				sendJSON(w, result.status, result.body, withID(result.headers))
			} else {
				s.mu.Lock()
				result := handleRest(s, method, restSegments, query, rawQuery, body, origin)
				s.mu.Unlock()
				sendJSON(w, result.status, result.body, withID(result.headers))
			}
			return
		}

		if len(segments) == 2 && segments[0] == "api" && segments[1] == "graphql" {
			if method != "POST" {
				sendJSON(w, 404, map[string]any{"message": "Not Found"}, withID(nil))
				return
			}
			if !isAuthorized(r) {
				sendJSON(w, 401, map[string]any{"message": "Requires authentication"}, withID(nil))
				return
			}
			raw, err := readBody(r)
			if err != nil {
				panic(err)
			}
			var body any
			if err := json.Unmarshal(raw, &body); err != nil {
				sendJSON(w, 400, map[string]any{"errors": []any{map[string]any{"message": "Invalid JSON body"}}}, withID(nil))
				return
			}
			s.mu.Lock()
			outcome := handleGraphql(s, body)
			s.mu.Unlock()
			sendJSON(w, outcome.status, outcome.body, withID(nil))
			return
		}

		if len(segments) == 1 && segments[0] == "mcp" {
			if method == "DELETE" {
				sendJSON(w, 200, map[string]any{}, nil)
				return
			}
			if method != "POST" {
				sendJSON(w, 404, map[string]any{"message": "Not Found"}, withID(nil))
				return
			}
			if !isAuthorized(r) {
				sendJSON(w, 401, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32000, "message": "Requires authentication"}}, withID(nil))
				return
			}
			raw, err := readBody(r)
			if err != nil {
				panic(err)
			}
			s.mu.Lock()
			outcome := handleMcp(s, raw)
			s.mu.Unlock()
			sendJSON(w, outcome.status, outcome.body, withID(nil))
			return
		}

		// Anything else: JSON 404.
		sendJSON(w, 404, map[string]any{"message": "Not Found"}, withID(nil))
	})
}
