// app.go — the HTTP server and router. Ported from app.ts. No business
// logic lives here: the request body is read whole, then the path dispatches
// to one surface (git smart HTTP, REST v3, GraphQL v4, MCP), each in its own
// module, over the one shared in-memory store.
//
// The whole dispatch runs under one mutex, matching the TypeScript server's
// single-threaded event loop: requests are effectively serialized, so the
// store needs no other synchronization.

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

// respWriter tracks whether the response was already written, so error
// paths can avoid writing a second response (like res.writableEnded).
type respWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *respWriter) header(key, value string) {
	w.Header().Set(key, value)
}

func (w *respWriter) writeHeader(status int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *respWriter) write(b []byte) {
	w.wrote = true
	w.ResponseWriter.Write(b)
}

type server struct {
	store          *Store
	mu             sync.Mutex
	token          string
	expectedAuth   string
	requestCounter int64
}

func newServer() *server {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	return &server{
		store:        NewStore(),
		token:        token,
		expectedAuth: "Bearer " + token,
	}
}

// writeResp writes a complete response with an explicit Content-Length.
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

// writeText writes a plain-text response (git errors, 404s).
func (s *server) writeText(w *respWriter, status int, text string) {
	s.writeResp(w, status, "text/plain; charset=utf-8", nil, []byte(text))
}

// writeJSON404 is the app-level 404: {"message":"Not Found"} + '\n'.
func (s *server) writeJSON404(w *respWriter) {
	text := append(marshalJSON(map[string]any{"message": "Not Found"}), '\n')
	s.writeResp(w, 404, jsonType, nil, text)
}

// sendJsonPlain writes a JSON response without the REST request-id header
// (used by GraphQL, MCP and the app itself, matching their own sendJson).
func (s *server) sendJsonPlain(w *respWriter, status int, body any) {
	text := append(marshalJSON(body), '\n')
	s.writeResp(w, status, jsonType, nil, text)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rw := &respWriter{ResponseWriter: w}
	// Read the body whole before dispatch, as the TS server does.
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
		if rec := recover(); rec != nil {
			if !rw.wrote {
				s.writeJSON404(rw)
			}
		}
	}()

	// Fast URL split: `new URL()` per request is one of the hottest costs
	// on this server. The pathname is everything before the first '?', the
	// query after it; handlers decode segments themselves exactly as before.
	// Absolute-form URLs (a proxy spelling) still go through the URL parser.
	rawURL := r.RequestURI
	pathname := "/"
	var search queryParams
	if strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://") {
		if u, err := url.Parse(rawURL); err == nil {
			pathname = u.Path
			search = parseQueryParams(u.RawQuery)
		} else {
			pathname = "/"
		}
	} else {
		if q := strings.IndexByte(rawURL, '?'); q < 0 {
			pathname = rawURL
		} else {
			pathname = rawURL[:q]
			search = parseQueryParams(rawURL[q+1:])
		}
		if pathname == "" {
			pathname = "/"
		}
	}
	method := r.Method

	// The env's boot check calls this twice: 200 {"ok":true}.
	if method == "GET" && pathname == "/healthz" {
		text := append(marshalJSON(map[string]any{"ok": true}), '\n')
		s.writeResp(rw, 200, jsonType, nil, text)
		return
	}
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
