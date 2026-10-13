package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bilyboy785/docker-image-updater/internal/stats"
)

func handler() http.Handler {
	return Handler(func() stats.Snapshot {
		return stats.Snapshot{Version: "1.2.3", Mode: "active", Scans: 7, Updates: 3, Events: []stats.Event{{Container: "<img src=x>", Status: "updated"}}}
	})
}

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func TestIndex(t *testing.T) {
	rec := get(t, "/")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("%d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "docker-image-updater") {
		t.Fatal("page title missing")
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("CSP %q", csp)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("expected no-store")
	}
}

func TestAPIStats(t *testing.T) {
	rec := get(t, "/api/stats")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	var snap stats.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil || snap.Scans != 7 || snap.Updates != 3 {
		t.Fatalf("%v %+v", err, snap)
	}
	if snap.Events[0].Container != "<img src=x>" {
		t.Error("data must be served raw as JSON; escaping happens in the page")
	}
}

func TestHealthz(t *testing.T) {
	rec := get(t, "/healthz")
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
}

func TestUnknownPathAndMethod(t *testing.T) {
	if get(t, "/nope").Code != 404 {
		t.Error("unknown path must be 404")
	}
	rec := httptest.NewRecorder()
	handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/stats", nil))
	if rec.Code != 405 {
		t.Errorf("POST must be 405, got %d", rec.Code)
	}
}

func TestPageNeverUsesInnerHTMLForData(t *testing.T) {
	if strings.Contains(string(indexHTML), "innerHTML") {
		t.Fatal("index.html must render data with textContent, never innerHTML")
	}
}
