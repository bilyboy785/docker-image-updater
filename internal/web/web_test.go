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

func TestPageTablesAreWideAndWrapLongText(t *testing.T) {
	page := string(indexHTML)
	for _, want := range []string{
		`main{max-width:min(1680px,96vw)`,
		`td.wrap{white-space:normal;overflow-wrap:anywhere`,
		`td.detail{white-space:normal;overflow-wrap:anywhere`,
		`td.err{white-space:normal;overflow-wrap:anywhere`,
		`table{width:100%;border-collapse:collapse;min-width:720px}`,
		`.scroll{overflow-x:auto}`,
		`cell(tr, c.image, 'mono wrap')`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
	for _, old := range []string{`max-width:1040px`, `max-width:340px`, `max-width:320px`} {
		if strings.Contains(page, old) {
			t.Errorf("index.html still has the old cap %s", old)
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

// scanSnap lists the containers of the last scan: "web", "db" and one with a non-ASCII name.
func scanSnap() stats.Snapshot {
	s := testSnap()
	s.Containers = []stats.ContainerStatus{{Name: "db"}, {Name: "web"}, {Name: "café"}}
	return s
}

var lastScanRequest ScanRequest

func scanHandler(res ScanResult) (http.Handler, *int) {
	calls := 0
	lastScanRequest = ScanRequest{}
	return Handler(scanSnap, func(r ScanRequest) ScanResult { calls++; lastScanRequest = r; return res }), &calls
}

func post(h http.Handler, headers map[string]string) *httptest.ResponseRecorder {
	return postTo(h, "/api/scan", headers)
}

func postTo(h http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "http://example.test:8080"+path, strings.NewReader(`{"ignored":true}`))
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

func TestFullScanRequestHasNoContainer(t *testing.T) {
	h, _ := scanHandler(ScanResult{Status: ScanStarted})
	post(h, csrfHeader)
	if lastScanRequest != (ScanRequest{}) {
		t.Fatalf("%+v", lastScanRequest)
	}
}

func TestScanContainerStarted(t *testing.T) {
	for name, query := range map[string]string{"web": "web", "café": "caf%C3%A9"} {
		h, calls := scanHandler(ScanResult{Status: ScanStarted})
		rec := postTo(h, "/api/scan?container="+query, csrfHeader)
		want := `{"status":"started","container":"` + name + `"}`
		if rec.Code != 202 || strings.TrimSpace(rec.Body.String()) != want {
			t.Fatalf("%d %q, want %s", rec.Code, rec.Body.String(), want)
		}
		if *calls != 1 || lastScanRequest.Container != name {
			t.Fatalf("calls=%d request=%+v", *calls, lastScanRequest)
		}
	}
}

func TestScanContainerUnknownIs404(t *testing.T) {
	for _, q := range []string{"nope", "WEB", "web%20", "%20web", "we", "web.", "caf%C3%A9%20"} {
		h, calls := scanHandler(ScanResult{Status: ScanStarted})
		rec := postTo(h, "/api/scan?container="+q, csrfHeader)
		if rec.Code != 404 || errBody(t, rec) == "" || *calls != 0 {
			t.Fatalf("%q: %d %q calls=%d", q, rec.Code, rec.Body.String(), *calls)
		}
	}
}

func TestScanContainerMalformedIs400(t *testing.T) {
	cases := map[string]string{
		"empty":             "",
		"blank":             "%20%20",
		"slash":             "a%2Fb",
		"nul":               "a%00b",
		"newline":           "a%0Ab",
		"tab":               "a%09b",
		"del":               "a%7Fb",
		"c1 control":        "a%C2%85b",
		"too long":          strings.Repeat("a", 256),
		"invalid utf-8":     "a%FFb",
		"two values":        "web&container=db",
		"bad percent":       "a%zzb",
		"empty with others": "&container=web",
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			h, calls := scanHandler(ScanResult{Status: ScanStarted})
			rec := postTo(h, "/api/scan?container="+q, csrfHeader)
			if rec.Code != 400 || errBody(t, rec) == "" || *calls != 0 {
				t.Fatalf("%d %q calls=%d", rec.Code, rec.Body.String(), *calls)
			}
		})
	}
}

func TestScanContainerMaxLengthNameIsAccepted(t *testing.T) {
	name := strings.Repeat("a", 255)
	snap := func() stats.Snapshot { return stats.Snapshot{Containers: []stats.ContainerStatus{{Name: name}}} }
	calls := 0
	h := Handler(snap, func(ScanRequest) ScanResult { calls++; return ScanResult{Status: ScanStarted} })
	rec := postTo(h, "/api/scan?container="+name, csrfHeader)
	if rec.Code != 202 || calls != 1 {
		t.Fatalf("%d calls=%d", rec.Code, calls)
	}
}

func TestScanContainerGuardsApplyToo(t *testing.T) {
	h, calls := scanHandler(ScanResult{Status: ScanStarted})
	for name, hdr := range map[string]map[string]string{
		"no header":      {},
		"foreign origin": {"X-Requested-With": "docker-image-updater", "Origin": "http://evil.test"},
	} {
		rec := postTo(h, "/api/scan?container=web", hdr)
		if rec.Code != 403 || *calls != 0 {
			t.Fatalf("%s: %d calls=%d", name, rec.Code, *calls)
		}
	}
	// the guards come before the parameter checks
	if rec := postTo(h, "/api/scan?container=a%2Fb", map[string]string{}); rec.Code != 403 {
		t.Fatalf("%d", rec.Code)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/scan?container=web", nil))
	if rec.Code != 405 || *calls != 0 {
		t.Fatalf("%d calls=%d", rec.Code, *calls)
	}
}

func TestScanContainerConflictAndTooSoon(t *testing.T) {
	h, _ := scanHandler(ScanResult{Status: ScanAlreadyRunning})
	if rec := postTo(h, "/api/scan?container=web", csrfHeader); rec.Code != 409 || errBody(t, rec) != "scan already running" {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	h, _ = scanHandler(ScanResult{Status: ScanTooSoon, RetryAfter: 2200 * time.Millisecond})
	rec := postTo(h, "/api/scan?container=web", csrfHeader)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "3" || errBody(t, rec) == "" {
		t.Fatalf("%d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
}

func TestScanContainerWithoutTrigger(t *testing.T) {
	rec := postTo(Handler(scanSnap, nil), "/api/scan?container=web", csrfHeader)
	if rec.Code != 501 {
		t.Fatalf("%d", rec.Code)
	}
}
func TestPageHasPerContainerScanButton(t *testing.T) {
	page := string(indexHTML)
	for _, want := range []string{"/api/scan?container=", "encodeURIComponent", "Action", "Scan ", "aria-label"} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	if strings.Contains(page, "innerHTML") {
		t.Error("index.html must not use innerHTML")
	}
}

func TestUnrelatedMalformedQueryDoesNotBreakAFullScan(t *testing.T) {
	for _, q := range []string{"x=%zz", "a=1;b=2", "%zz=1"} {
		h, calls := scanHandler(ScanResult{Status: ScanStarted})
		rec := postTo(h, "/api/scan?"+q, csrfHeader)
		if rec.Code != 202 || *calls != 1 || lastScanRequest != (ScanRequest{}) {
			t.Fatalf("%q: %d calls=%d", q, rec.Code, *calls)
		}
	}
	for _, q := range []string{"container=%zz", "x=1&container=%zz", "container=%zz&container=web"} {
		h, calls := scanHandler(ScanResult{Status: ScanStarted})
		rec := postTo(h, "/api/scan?"+q, csrfHeader)
		if rec.Code != 400 || *calls != 0 {
			t.Fatalf("%q: %d calls=%d", q, rec.Code, *calls)
		}
	}
}

func TestValidContainerWithUnrelatedMalformedParameterIsAccepted(t *testing.T) {
	h, calls := scanHandler(ScanResult{Status: ScanStarted})
	rec := postTo(h, "/api/scan?container=web&x=%zz", csrfHeader)
	if rec.Code != 202 || *calls != 1 || lastScanRequest.Container != "web" {
		t.Fatalf("%d calls=%d %+v", rec.Code, *calls, lastScanRequest)
	}
}
func TestPageRestoresFocusAfterAScan(t *testing.T) {
	page := string(indexHTML)
	for _, want := range []string{"lastTarget", ".focus()"} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
}
