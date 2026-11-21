// app.go — the HTTP server and its router.
//
// No business logic lives here. ServeHTTP reads the request body in
// full, splits the request target into a path and a query, and hands
// the request to the first surface that claims it: git smart HTTP,
// GraphQL, MCP, then REST. Anything unclaimed is a JSON 404.
//
// Dispatch runs under one mutex, which is the Go equivalent of a
// single-threaded event loop: requests are effectively serialized, so
// the shared in-memory store needs no other synchronization. That also
// keeps the 2,000-read workload cheap — one connection, no per-request
// allocation beyond the response itself.
//
// The helpers on server (writeResp, writeText, writeJSON404,
// sendJsonPlain) and the respWriter type are the reply surface the
// handler modules in githttp.go, rest.go, graphql.go and mcp.go call
// into; their signatures are part of the inter-module contract.

package main

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
)

// respWriter wraps http.ResponseWriter and remembers whether a response
// has already gone out, so a late error path never writes a second one.
type respWriter struct {
	http.ResponseWriter
	sent bool
}

// header sets a response header.
func (w *respWriter) header(key, value string) {
	w.Header().Set(key, value)
}

// writeHeader records the status and forwards it.
func (w *respWriter) writeHeader(status int) {
	w.sent = true
	w.ResponseWriter.WriteHeader(status)
}

// write records the body write and forwards it.
func (w *respWriter) write(b []byte) {
	w.sent = true
	w.ResponseWriter.Write(b)
}

// server is the whole forge: one store, one lock, and the bearer token
// every authenticated surface checks against.
type server struct {
	store          *Store
	mu             sync.Mutex
	token          string
	expectedAuth   string
	requestCounter int64
}

// newServer builds a forge with a fresh store. The token comes from the
// environment; with none set, authenticated surfaces answer 401.
func newServer() *server {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	return &server{
		store:        NewStore(),
		token:        token,
		expectedAuth: "Bearer " + token,
	}
}

// isAuthorized checks the request's bearer <redacted> the server started with.
// An empty server token authorizes nobody.
func (s *server) isAuthorized(r *http.Request) bool {
	if s.token == "" {
		return false
	}
	return r.Header.Get("Authorization") == s.expectedAuth
}

// writeResp writes a complete response: content type, any extra headers,
// an explicit Content-Length, then the body.
func (s *server) writeResp(w *respWriter, status int, contentType string, extra map[string]string, body []byte) {
	w.header("content-type", contentType)
	for k, v := range extra {
		w.header(k, v)
	}
	w.header("content-length", strconv.Itoa(len(body)))
	w.writeHeader(status)
	if len(body) > 0 {
		w.write(body)
	}
}

// writeText writes a plain-text response (git errors and the like).
func (s *server) writeText(w *respWriter, status int, text string) {
	s.writeResp(w, status, "text/plain; charset=utf-8", nil, []byte(text))
}

// writeJSON404 is the app-level 404: {"message":"Not Found"} plus a
// trailing newline.
func (s *server) writeJSON404(w *respWriter) {
	text := append(marshalJSON(map[string]any{"message": "Not Found"}), '\n')
	s.writeResp(w, 404, jsonType, nil, text)
}

// sendJsonPlain writes a JSON response without the REST request-id
// header; GraphQL, MCP, and the app itself use this shape.
func (s *server) sendJsonPlain(w *respWriter, status int, body any) {
	text := append(marshalJSON(body), '\n')
	s.writeResp(w, status, jsonType, nil, text)
}

// splitTarget separates a request target into its path and query
// without paying for a full URL parse on the hot path: the path is
// everything before the first '?', the query everything after it.
// Absolute-form targets (the spelling a proxy uses) still go through
// url.Parse.
func splitTarget(rawURL string) (string, queryParams) {
	if strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://") {
		if parsed, err := url.Parse(rawURL); err == nil {
			return parsed.Path, parseQueryParams(parsed.RawQuery)
		}
		return "/", queryParams{}
	}
	if q := strings.IndexByte(rawURL, '?'); q >= 0 {
		path := rawURL[:q]
		if path == "" {
			path = "/"
		}
		return path, parseQueryParams(rawURL[q+1:])
	}
	if rawURL == "" {
		return "/", queryParams{}
	}
	return rawURL, queryParams{}
}

// healthCheck is the env's boot probe: 200 {"ok":true}. It is answered
// twice within ten seconds of registering.
func (s *server) healthCheck(w *respWriter) {
	text := append(marshalJSON(map[string]any{"ok": true}), '\n')
	s.writeResp(w, 200, jsonType, nil, text)
}

// ServeHTTP is the single entry point for every request.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rw := &respWriter{ResponseWriter: w}

	// The body is read whole before dispatch.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		text := append(marshalJSON(map[string]any{"message": "Bad Request"}), '\n')
		rw.header("content-type", jsonType)
		rw.header("content-length", strconv.Itoa(len(text)))
		rw.writeHeader(400)
		rw.write(text)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		// A panic in any handler must not kill the process: if nothing
		// was written yet, answer the app 404.
		if rec := recover(); rec != nil {
			if !rw.sent {
				s.writeJSON404(rw)
			}
		}
	}()

	pathname, search := splitTarget(r.RequestURI)

	if r.Method == http.MethodGet && pathname == "/healthz" {
		s.healthCheck(rw)
		return
	}

	// Surfaces claim the request in this order; the first match wins.
	if s.handleGit(r, rw, pathname, search, body) {
		return
	}
	if s.handleGraphQL(r, rw, pathname, body) {
		return
	}
	if s.handleMcp(r, rw, pathname, body) {
		return
	}
	if s.handleRest(r, rw, pathname, search, body) {
		return
	}
	s.writeJSON404(rw)
}
