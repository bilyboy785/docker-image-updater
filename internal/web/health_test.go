package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bilyboy785/docker-image-updater/internal/stats"
)

const hInterval = 10 * time.Minute

var hLimit = 2*hInterval + 35*time.Minute

func healthAt(t *testing.T, snap stats.Snapshot, now time.Time) *httptest.ResponseRecorder {
	t.Helper()
	h := Handler(func() stats.Snapshot { return snap }, nil, WithHealth(hInterval), WithClock(func() time.Time { return now }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	return rec
}

func TestHealthzBeforeTheFirstScanUsesTheProcessStart(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	snap := stats.Snapshot{StartedAt: start}
	if rec := healthAt(t, snap, start.Add(hLimit)); rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("just inside: %d %q", rec.Code, rec.Body.String())
	}
	rec := healthAt(t, snap, start.Add(hLimit+time.Second))
	if rec.Code != 503 || !strings.HasPrefix(rec.Body.String(), "stale: ") {
		t.Fatalf("just outside: %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
}

func TestHealthzAfterAScanUsesTheEndOfTheLastScan(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	scan := start.Add(time.Hour)
	snap := stats.Snapshot{StartedAt: start, Scans: 1, LastScan: scan, LastScanMs: 90_000}
	end := scan.Add(90 * time.Second)
	if rec := healthAt(t, snap, end.Add(hLimit)); rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("just inside: %d %q", rec.Code, rec.Body.String())
	}
	rec := healthAt(t, snap, end.Add(hLimit+time.Millisecond))
	if rec.Code != 503 || !strings.HasPrefix(rec.Body.String(), "stale: ") {
		t.Fatalf("just outside: %d %q", rec.Code, rec.Body.String())
	}
}

func TestHealthzAScanRestoredFromTheStateFileDoesNotMakeAFreshProcessStale(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	snap := stats.Snapshot{StartedAt: start, Scans: 40, LastScan: start.Add(-72 * time.Hour), LastScanMs: 1000}
	if rec := healthAt(t, snap, start.Add(time.Minute)); rec.Code != 200 {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if rec := healthAt(t, snap, start.Add(hLimit+time.Second)); rec.Code != 503 {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestHealthzWithoutTheOptionIsAlwaysOK(t *testing.T) {
	h := Handler(func() stats.Snapshot { return stats.Snapshot{StartedAt: time.Unix(1, 0)} }, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestHealthzStaleBodyIsShortAndSayWhy(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	rec := healthAt(t, stats.Snapshot{StartedAt: start}, start.Add(5*time.Hour))
	body := rec.Body.String()
	if len(body) > 200 || strings.Contains(body, "\n") || !strings.Contains(body, "no scan completed") {
		t.Fatalf("%q", body)
	}
}

func TestCheckHealth(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("path %q", r.URL.Path)
		}
		w.WriteHeader(status)
		w.Write([]byte("stale: x"))
	}))
	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := CheckHealth(addr, time.Second); err != nil {
		t.Fatalf("200 must succeed: %v", err)
	}
	status = http.StatusServiceUnavailable
	if err := CheckHealth(addr, time.Second); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("503 must fail: %v", err)
	}
	srv.Close()
	if err := CheckHealth(addr, time.Second); err == nil {
		t.Fatal("connection refused must fail")
	}
}

func TestCheckHealthTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	start := time.Now()
	if err := CheckHealth(strings.TrimPrefix(srv.URL, "http://"), 100*time.Millisecond); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("want a timeout, got %v after %v", err, time.Since(start))
	}
}

func TestLocalAddr(t *testing.T) {
	for in, want := range map[string]string{
		":8080":          "127.0.0.1:8080",
		"0.0.0.0:9000":   "127.0.0.1:9000",
		"[::]:9000":      "127.0.0.1:9000",
		"127.0.0.1:8081": "127.0.0.1:8081",
		"localhost:8082": "localhost:8082",
		"10.1.2.3:8083":  "10.1.2.3:8083",
		"[::1]:8084":     "[::1]:8084",
		" :8080 ":        "127.0.0.1:8080",
	} {
		got, err := LocalAddr(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := LocalAddr("nonsense"); err == nil {
		t.Error("an address without a port must be rejected")
	}
}

func TestHealthzALongScanThatKeepsProgressingStaysHealthy(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	// The last scan ended long ago, but the scan in progress touched the heartbeat a minute ago.
	snap := stats.Snapshot{StartedAt: start, Scans: 3, LastScan: start.Add(time.Hour), LastScanMs: 1000, Scanning: true}
	now := start.Add(10 * time.Hour)
	snap.LastProgress = now.Add(-time.Minute)
	if rec := healthAt(t, snap, now); rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("progressing scan: %d %q", rec.Code, rec.Body.String())
	}
}

func TestHealthzAStuckScanGoesStaleFromItsLastProgress(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	progress := start.Add(2 * time.Hour)
	snap := stats.Snapshot{StartedAt: start, Scans: 3, LastScan: start.Add(time.Hour), LastScanMs: 1000, Scanning: true, LastProgress: progress}
	if rec := healthAt(t, snap, progress.Add(hLimit)); rec.Code != 200 {
		t.Fatalf("just inside: %d %q", rec.Code, rec.Body.String())
	}
	rec := healthAt(t, snap, progress.Add(hLimit+time.Second))
	if rec.Code != 503 || !strings.HasPrefix(rec.Body.String(), "stale: ") || !strings.Contains(rec.Body.String(), "progress") {
		t.Fatalf("just outside: %d %q", rec.Code, rec.Body.String())
	}
}

func TestHealthzAProgressOlderThanTheLastScanIsIgnored(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	scan := start.Add(time.Hour)
	end := scan.Add(time.Second)
	snap := stats.Snapshot{StartedAt: start, Scans: 1, LastScan: scan, LastScanMs: 1000, LastProgress: scan.Add(-time.Minute)}
	if rec := healthAt(t, snap, end.Add(hLimit+time.Millisecond)); rec.Code != 503 || !strings.Contains(rec.Body.String(), "last scan completed") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestCheckHealthDoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()
	err := CheckHealth(strings.TrimPrefix(srv.URL, "http://"), time.Second)
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("a redirect is not a healthy answer: %v", err)
	}
}

func TestCheckHealthIgnoresProxyEnvironment(t *testing.T) {
	// A closed port: a probe that went through this proxy would fail.
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY"} {
		t.Setenv(k, "http://127.0.0.1:1")
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()
	if err := CheckHealth(strings.TrimPrefix(srv.URL, "http://"), time.Second); err != nil {
		t.Fatalf("the probe must ignore proxy variables: %v", err)
	}
	// The standard library never proxies loopback addresses, so also check the configuration.
	tr, ok := healthClient().Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatalf("the probe client must not use a proxy: %#v", healthClient().Transport)
	}
}
