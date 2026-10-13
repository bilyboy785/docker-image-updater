package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bilyboy785/docker-image-updater/internal/stats"
)

var fixedNow = time.Unix(1700003661, 0)

func metricsBody(t *testing.T, snap stats.Snapshot) (*httptest.ResponseRecorder, string) {
	t.Helper()
	h := Handler(func() stats.Snapshot { return snap }, nil, WithClock(func() time.Time { return fixedNow }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec, rec.Body.String()
}

const goldenMetrics = `# HELP diu_scans_total Number of completed scans.
# TYPE diu_scans_total counter
diu_scans_total 7
# HELP diu_updates_total Number of containers updated.
# TYPE diu_updates_total counter
diu_updates_total 3
# HELP diu_failures_total Number of failed updates and image checks.
# TYPE diu_failures_total counter
diu_failures_total 1
# HELP diu_rollbacks_total Number of updates rolled back.
# TYPE diu_rollbacks_total counter
diu_rollbacks_total 2
# HELP diu_scanning Whether a scan is running (1) or not (0).
# TYPE diu_scanning gauge
diu_scanning 1
# HELP diu_containers_watched Containers checked by the last scan.
# TYPE diu_containers_watched gauge
diu_containers_watched 5
# HELP diu_containers_skipped Containers ignored by the last scan.
# TYPE diu_containers_skipped gauge
diu_containers_skipped 2
# HELP diu_last_scan_timestamp_seconds Start of the last scan, as a Unix time (0 when none yet).
# TYPE diu_last_scan_timestamp_seconds gauge
diu_last_scan_timestamp_seconds 1700000000
# HELP diu_last_scan_duration_seconds Duration of the last scan.
# TYPE diu_last_scan_duration_seconds gauge
diu_last_scan_duration_seconds 1.5
# HELP diu_next_scan_timestamp_seconds Planned time of the next scan, as a Unix time (0 when unknown).
# TYPE diu_next_scan_timestamp_seconds gauge
diu_next_scan_timestamp_seconds 1700000600
# HELP diu_uptime_seconds Time since the updater started.
# TYPE diu_uptime_seconds gauge
diu_uptime_seconds 3661
# HELP diu_build_info Version and mode of the updater; always 1.
# TYPE diu_build_info gauge
diu_build_info{version="1.2.3",mode="active"} 1
# HELP diu_containers Containers by status after the last scan.
# TYPE diu_containers gauge
diu_containers{status="up-to-date"} 2
diu_containers{status="updated"} 1
diu_containers{status="update-available"} 0
diu_containers{status="waiting-window"} 0
diu_containers{status="failed"} 0
diu_containers{status="rolled-back"} 0
diu_containers{status="skipped"} 1
diu_containers{status="self"} 0
`

func fixedSnapshot() stats.Snapshot {
	return stats.Snapshot{Version: "1.2.3", Mode: "active", StartedAt: fixedNow.Add(-3661 * time.Second),
		Scans: 7, LastScan: time.Unix(1700000000, 0), NextScan: time.Unix(1700000600, 0), LastScanMs: 1500,
		Watched: 5, Skipped: 2, Updates: 3, Failures: 1, Rollbacks: 2, Scanning: true,
		Containers: []stats.ContainerStatus{
			{Name: "a", Status: stats.ContainerUpToDate}, {Name: "b", Status: stats.ContainerUpToDate},
			{Name: "c", Status: stats.ContainerUpdated}, {Name: "d", Status: stats.ContainerSkipped},
			{Name: "e", Status: "bogus"}}}
}

func TestMetricsGolden(t *testing.T) {
	rec, body := metricsBody(t, fixedSnapshot())
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if body != goldenMetrics {
		t.Fatalf("got:\n%s\nwant:\n%s", body, goldenMetrics)
	}
}

func TestMetricsContentType(t *testing.T) {
	rec, _ := metricsBody(t, fixedSnapshot())
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("expected no-store")
	}
}

func TestMetricsZeroValueSnapshot(t *testing.T) {
	_, body := metricsBody(t, stats.Snapshot{})
	for _, want := range []string{
		"diu_scans_total 0\n", "diu_scanning 0\n", "diu_last_scan_timestamp_seconds 0\n",
		"diu_next_scan_timestamp_seconds 0\n", "diu_last_scan_duration_seconds 0\n", "diu_uptime_seconds 0\n",
		`diu_build_info{version="",mode=""} 1`, `diu_containers{status="self"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

func TestMetricsEscapesLabelValues(t *testing.T) {
	_, body := metricsBody(t, stats.Snapshot{Version: "a\\b\"c\nd", Mode: "x"})
	if !strings.Contains(body, `diu_build_info{version="a\\b\"c\nd",mode="x"} 1`) {
		t.Fatalf("label not escaped:\n%s", body)
	}
	if strings.Count(body, "\n") != strings.Count(strings.ReplaceAll(body, "\\n", ""), "\n") {
		t.Fatal("raw newline leaked into a label")
	}
}

func TestMetricsAreWellFormed(t *testing.T) {
	_, body := metricsBody(t, fixedSnapshot())
	name := regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	label := regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	sample := regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{(.*)\})? (\S+)$`)
	typed := map[string]bool{}
	if !strings.HasSuffix(body, "\n") {
		t.Fatal("must end with a newline")
	}
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "# HELP "), strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(line)
			if len(f) < 4 || !name.MatchString(f[2]) {
				t.Errorf("bad comment %q", line)
			}
			if f[1] == "TYPE" {
				typed[f[2]] = true
				if f[3] != "counter" && f[3] != "gauge" {
					t.Errorf("bad type %q", line)
				}
			}
		default:
			m := sample.FindStringSubmatch(line)
			if m == nil {
				t.Errorf("bad sample %q", line)
				continue
			}
			if !typed[m[1]] {
				t.Errorf("sample %q without TYPE", line)
			}
			if m[3] != "" {
				for _, kv := range regexp.MustCompile(`([^=,]+)="`).FindAllStringSubmatch(m[3], -1) {
					if !label.MatchString(kv[1]) {
						t.Errorf("bad label name %q in %q", kv[1], line)
					}
				}
			}
		}
	}
}

func TestMetricsMethodNotAllowed(t *testing.T) {
	h := Handler(testSnap, nil)
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(m, "/metrics", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d", m, rec.Code)
		}
	}
}

func TestMetricsDoNotLeakContainerDetails(t *testing.T) {
	snap := fixedSnapshot()
	snap.Containers[0].Detail = "SECRET-DETAIL"
	snap.Containers[0].Name = "SECRET-NAME"
	snap.Containers[0].Image = "SECRET-IMAGE"
	_, body := metricsBody(t, snap)
	if strings.Contains(body, "SECRET") {
		t.Fatal("container names, images and details must not be exposed")
	}
}
