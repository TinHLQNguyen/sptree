package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeMicrosoft serves the handful of Entra ID and Graph endpoints sptree calls.
func fakeMicrosoft(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	j := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer AT-2" {
			w.WriteHeader(401)
			j(w, map[string]any{"error": map[string]string{"code": "InvalidAuthenticationToken", "message": "bad token"}})
			return false
		}
		return true
	}
	mux.HandleFunc("POST /contoso/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "RT-1" {
			w.WriteHeader(400)
			j(w, map[string]string{"error": "invalid_grant", "error_description": "nope"})
			return
		}
		j(w, map[string]any{"access_token": "AT-2", "refresh_token": "RT-2", "expires_in": 3600})
	})
	mux.HandleFunc("GET /v1.0/sites", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if r.URL.Query().Get("page") == "2" {
			j(w, map[string]any{"value": []map[string]string{{"id": "S2", "displayName": "Beta", "webUrl": "https://t/sites/beta"}}})
			return
		}
		j(w, map[string]any{
			"value":           []map[string]string{{"id": "S1", "displayName": "Alpha", "webUrl": "https://t/sites/alpha"}},
			"@odata.nextLink": srv.URL + "/v1.0/sites?page=2",
		})
	})
	mux.HandleFunc("GET /v1.0/sites/{id}/drives", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if r.PathValue("id") == "S2" {
			w.WriteHeader(403)
			j(w, map[string]any{"error": map[string]string{"code": "accessDenied"}})
			return
		}
		j(w, map[string]any{"value": []map[string]string{
			{"id": "D1", "name": "Documents", "webUrl": "https://t/sites/alpha/Shared Documents", "driveType": "documentLibrary"},
		}})
	})
	mux.HandleFunc("GET /v1.0/me/drive", func(w http.ResponseWriter, r *http.Request) {
		j(w, map[string]string{"id": "OD", "name": "OneDrive", "webUrl": "https://t-my/personal/me/Documents", "driveType": "business"})
	})
	mux.HandleFunc("GET /v1.0/me", func(w http.ResponseWriter, r *http.Request) {
		j(w, map[string]string{"displayName": "Test User"})
	})
	mux.HandleFunc("GET /v1.0/drives/D1/root/children", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if r.URL.Query().Get("skip") == "1" {
			j(w, map[string]any{"value": []map[string]any{{"id": "F2", "name": "b.docx", "size": 9, "file": map[string]any{}, "webUrl": "https://t/b.docx"}}})
			return
		}
		j(w, map[string]any{
			"value":           []map[string]any{{"id": "A", "name": "Projects", "size": 5, "folder": map[string]int{"childCount": 1}, "webUrl": "https://t/sites/alpha/Shared Documents/Projects"}},
			"@odata.nextLink": srv.URL + "/v1.0/drives/D1/root/children?skip=1",
		})
	})
	mux.HandleFunc("GET /v1.0/drives/D1/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case "B":
			j(w, map[string]any{"id": "B", "name": "2026", "folder": map[string]int{}, "parentReference": map[string]string{"id": "A", "driveId": "D1"}})
		case "A":
			j(w, map[string]any{"id": "A", "name": "Projects", "folder": map[string]int{}, "parentReference": map[string]string{"id": "ROOT", "driveId": "D1"}})
		case "ROOT":
			j(w, map[string]any{"id": "ROOT", "name": "root", "root": map[string]any{}})
		}
	})
	mux.HandleFunc("POST /v1.0/search/query", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"queryString":"proj* IsContainer:true"`) {
			t.Errorf("unexpected search body %s", b)
		}
		j(w, map[string]any{"value": []any{map[string]any{"hitsContainers": []any{map[string]any{"hits": []any{
			map[string]any{"resource": map[string]any{"id": "B", "name": "2026", "webUrl": "https://t/sites/alpha/Shared Documents/Projects/2026",
				"parentReference": map[string]string{"driveId": "D1"}}},
		}}}}}})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestServer(t *testing.T) (http.Handler, *Auth) {
	ms := fakeMicrosoft(t)
	cache := filepath.Join(t.TempDir(), "token.json")
	// an expired access token with a valid refresh token: the first call must refresh
	b, _ := json.Marshal(token{AccessToken: "AT-1", RefreshToken: "RT-1", ExpiresAt: time.Now().Add(-time.Hour)})
	if err := os.WriteFile(cache, b, 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewAuth("cid", "contoso", ms.URL, cache, io.Discard)
	s := &Server{auth: a, graph: NewGraph(ms.URL+"/v1.0", a)}
	return s.Handler(), a
}

func get(t *testing.T, h http.Handler, method, path string, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1:8765"+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestLibrariesChildrenAncestorsSearch(t *testing.T) {
	h, _ := newTestServer(t)

	code, body := get(t, h, "GET", "/api/libraries", nil)
	if code != 200 {
		t.Fatalf("libraries: %d %v", code, body)
	}
	libs := body["libraries"].([]any)
	if len(libs) != 2 { // Alpha/Documents + OneDrive; Beta is forbidden and skipped
		t.Fatalf("want 2 libraries, got %v", libs)
	}

	code, body = get(t, h, "GET", "/api/children?drive=D1", nil)
	items := body["items"].([]any)
	if code != 200 || len(items) != 2 {
		t.Fatalf("children (paged): %d %v", code, body)
	}
	if first := items[0].(map[string]any); first["folder"] != true || first["name"] != "Projects" {
		t.Fatalf("bad first child %v", first)
	}

	code, body = get(t, h, "GET", "/api/ancestors?drive=D1&item=B", nil)
	chain := body["chain"].([]any)
	if code != 200 || len(chain) != 2 || chain[0].(map[string]any)["id"] != "A" || chain[1].(map[string]any)["id"] != "B" {
		t.Fatalf("ancestors: %d %v", code, body)
	}

	code, body = get(t, h, "GET", "/api/search?q=proj", nil)
	hits := body["items"].([]any)
	if code != 200 || len(hits) != 1 {
		t.Fatalf("search: %d %v", code, body)
	}
	if hit := hits[0].(map[string]any); hit["driveId"] != "D1" || hit["path"] != "alpha › Projects" {
		t.Fatalf("bad hit %v", hit)
	}
}

func TestGuard(t *testing.T) {
	h, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "http://evil.example/api/libraries", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("foreign Host should be rejected, got %d", rec.Code)
	}
	if code, _ := get(t, h, "POST", "/api/auth/logout", nil); code != 403 {
		t.Fatalf("POST without X-SPTree should be rejected, got %d", code)
	}
	if code, _ := get(t, h, "POST", "/api/auth/logout", map[string]string{"X-SPTree": "1"}); code != 200 {
		t.Fatalf("logout failed: %d", code)
	}
	if code, body := get(t, h, "GET", "/api/children?drive=D1", nil); code != 401 || body["error"].(map[string]any)["code"] != "needs_login" {
		t.Fatalf("after logout want needs_login, got %d %v", code, body)
	}
}

func TestKQL(t *testing.T) {
	cases := map[string]string{"proj": "proj*", ` "a b" OR c:d `: "a* b* c* d*", "見積書": "見積書*", "": ""}
	for in, want := range cases {
		if got := kqlTerms(in); got != want {
			t.Errorf("kqlTerms(%q)=%q want %q", in, got, want)
		}
	}
}
