package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

type Server struct {
	auth  *Auth
	graph *Graph
	port  string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /api/auth", s.handleAuth)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/libraries", s.handleLibraries)
	mux.HandleFunc("GET /api/children", s.handleChildren)
	mux.HandleFunc("GET /api/ancestors", s.handleAncestors)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	return s.guard(mux)
}

// guard keeps the API local: it rejects requests whose Host isn't loopback (DNS-rebinding defence)
// and POSTs without the custom header (cross-site form posts can't set it).
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && host != "localhost" && host != "::1" && host != "[::1]" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method == "POST" && r.Header.Get("X-SPTree") != "1" {
			http.Error(w, "missing X-SPTree header", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; script-src 'self' 'unsafe-inline'; img-src 'self' data:")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	type apiErr struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	var ge *GraphError
	switch {
	case isNeedLogin(err):
		writeJSON(w, 401, map[string]apiErr{"error": {"needs_login", "Sign in to Microsoft 365 to continue."}})
	case errors.As(err, &ge):
		code := "graph_error"
		switch ge.Status {
		case 403:
			code = "forbidden"
		case 404:
			code = "not_found"
		case 429:
			code = "throttled"
		}
		writeJSON(w, 502, map[string]apiErr{"error": {code, ge.Message}})
	case errors.Is(err, context.DeadlineExceeded):
		writeJSON(w, 504, map[string]apiErr{"error": {"timeout", "Microsoft Graph didn't answer in time."}})
	default:
		writeJSON(w, 502, map[string]apiErr{"error": {"upstream_error", err.Error()}})
	}
}

func ctxFor(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	pending, lastErr := s.auth.Status()
	resp := map[string]any{"signedIn": s.auth.SignedIn(), "pending": pending, "error": lastErr}
	if s.auth.SignedIn() {
		ctx, cancel := ctxFor(r, 15*time.Second)
		defer cancel()
		if name, err := s.graph.Me(ctx); err == nil {
			resp["user"] = name
		} else if isNeedLogin(err) {
			resp["signedIn"] = false
		}
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	s.auth.StartDeviceLogin()
	// give the device-code request a moment so the UI can show the code immediately
	for i := 0; i < 30; i++ {
		if p, e := s.auth.Status(); p != nil || e != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.handleAuth(w, r)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout()
	writeJSON(w, 200, map[string]any{"signedIn": false})
}

func (s *Server) handleLibraries(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxFor(r, 3*time.Minute)
	defer cancel()
	libs, err := s.graph.Libraries(ctx, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"libraries": libs})
}

func (s *Server) handleChildren(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	drive := q.Get("drive")
	if drive == "" {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "bad_request", "message": "drive is required"}})
		return
	}
	ctx, cancel := ctxFor(r, 2*time.Minute)
	defer cancel()
	items, err := s.graph.Children(ctx, drive, q.Get("item"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if items == nil {
		items = []Item{}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) handleAncestors(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ctx, cancel := ctxFor(r, time.Minute)
	defer cancel()
	chain, err := s.graph.Ancestors(ctx, q.Get("drive"), q.Get("item"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"chain": chain})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	size, _ := strconv.Atoi(q.Get("size"))
	if size <= 0 || size > 100 {
		size = 40
	}
	ctx, cancel := ctxFor(r, 30*time.Second)
	defer cancel()
	items, err := s.graph.Search(ctx, strings.TrimSpace(q.Get("q")), q.Get("files") == "1", size)
	if err != nil {
		writeErr(w, err)
		return
	}
	if items == nil {
		items = []Item{}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
