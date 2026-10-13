package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bilyboy785/docker-image-updater/internal/stats"
)

func testSnap() stats.Snapshot {
	return stats.Snapshot{Version: "1.2.3", Mode: "active", Scans: 7, Updates: 3, Events: []stats.Event{{Container: "<img src=x>", Status: "updated"}}}
}

func handler() http.Handler { return Handler(testSnap, nil) }

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

func TestPageHasTheContainerList(t *testing.T) {
	page := string(indexHTML)
	for _, want := range []string{
		`id="containers"`, `id="chips"`, `id="ctable"`, `id="cempty"`, `No containers observed yet`,
		`>Containers<`, `aria-pressed`, `scope="col"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
	if strings.Index(page, `id="containers"`) > strings.Index(page, "Recent activity") {
		t.Error("the container list must come above the recent activity")
	}
	// Every known status must have an allow-listed class and a pill style.
	for _, st := range stats.ContainerStatuses {
		if !strings.Contains(page, "'"+st+"'") || !strings.Contains(page, ".pill.c-"+st) {
			t.Errorf("status %s is not handled by the page", st)
		}
	}
}

func TestPageChipsAndLastCheckedAreAccessible(t *testing.T) {
	page := string(indexHTML)
	for _, want := range []string{
		`setAttribute('aria-label', (key ? CLABELS[key] : 'All') + ': ' + (counts[key] || 0))`,
		`cell(tr, rel(c.checked_at)).title = when(c.checked_at)`,
		`gap:6px`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
}

func scanHandler(res ScanResult) (http.Handler, *int) {
	calls := 0
	return Handler(testSnap, func() ScanResult { calls++; return res }), &calls
}

func post(h http.Handler, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "http://example.test:8080/api/scan", strings.NewReader(`{"ignored":true}`))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var csrfHeader = map[string]string{"X-Requested-With": "docker-image-updater"}

func errBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %q", ct)
	}
	var b struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	return b.Error
}

func TestScanStarted(t *testing.T) {
	h, calls := scanHandler(ScanResult{Status: ScanStarted})
	rec := post(h, csrfHeader)
	if rec.Code != 202 || strings.TrimSpace(rec.Body.String()) != `{"status":"started"}` {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers %v", rec.Header())
	}
	if *calls != 1 {
		t.Fatalf("trigger called %d times", *calls)
	}
}

func TestScanAlreadyRunning(t *testing.T) {
	h, _ := scanHandler(ScanResult{Status: ScanAlreadyRunning})
	rec := post(h, csrfHeader)
	if rec.Code != 409 || errBody(t, rec) != "scan already running" {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestScanTooSoon(t *testing.T) {
	h, _ := scanHandler(ScanResult{Status: ScanTooSoon, RetryAfter: 12*time.Second + 300*time.Millisecond})
	rec := post(h, csrfHeader)
	if rec.Code != 429 || errBody(t, rec) == "" {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra != "13" {
		t.Fatalf("Retry-After %q, want 13 (rounded up)", ra)
	}
}

func TestScanRejectedRequestsNeverTrigger(t *testing.T) {
	cases := map[string]map[string]string{
		"no header":            {},
		"wrong header value":   {"X-Requested-With": "XMLHttpRequest"},
		"foreign origin":       {"X-Requested-With": "docker-image-updater", "Origin": "http://evil.test"},
		"foreign origin, port": {"X-Requested-With": "docker-image-updater", "Origin": "http://example.test:9999"},
		"null origin":          {"X-Requested-With": "docker-image-updater", "Origin": "null"},
		"garbage origin":       {"X-Requested-With": "docker-image-updater", "Origin": "::"},
	}
	for name, hdr := range cases {
		t.Run(name, func(t *testing.T) {
			h, calls := scanHandler(ScanResult{Status: ScanStarted})
			rec := post(h, hdr)
			if rec.Code != 403 {
				t.Fatalf("got %d", rec.Code)
			}
			if errBody(t, rec) == "" {
				t.Error("missing error message")
			}
			if *calls != 0 {
				t.Fatal("trigger must not be called")
			}
		})
	}
}

func TestScanMatchingOriginAccepted(t *testing.T) {
	h, calls := scanHandler(ScanResult{Status: ScanStarted})
	rec := post(h, map[string]string{"X-Requested-With": "docker-image-updater", "Origin": "http://example.test:8080"})
	if rec.Code != 202 || *calls != 1 {
		t.Fatalf("%d, calls=%d", rec.Code, *calls)
	}
}

func TestScanGetIsNotAllowed(t *testing.T) {
	h, calls := scanHandler(ScanResult{Status: ScanStarted})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/scan", nil))
	if rec.Code != 405 || *calls != 0 {
		t.Fatalf("%d, calls=%d", rec.Code, *calls)
	}
}

func TestScanWithoutTrigger(t *testing.T) {
	rec := post(handler(), csrfHeader)
	if rec.Code != 501 || errBody(t, rec) == "" {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestPageHasScanButton(t *testing.T) {
	for _, want := range []string{"/api/scan", "X-Requested-With", "docker-image-updater", "Scan now"} {
		if !strings.Contains(string(indexHTML), want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
}
