package web

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/bilyboy785/docker-image-updater/internal/stats"
)

// metricsContentType is the Prometheus text exposition format, version 0.0.4.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// labelEscaper escapes a label value as the exposition format requires.
var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func fnum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// unixSeconds returns t as a Unix time in seconds, or 0 for an unset time.
func unixSeconds(t time.Time) float64 {
	if t.IsZero() || t.Unix() <= 0 {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}

// writeMetrics renders snap, observed at now, in the Prometheus text format. Only counters and
// aggregate values are exposed: no container name, image, detail or environment.
func writeMetrics(w io.Writer, snap stats.Snapshot, now time.Time) {
	metric := func(name, typ, help string, samples ...string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		for _, s := range samples {
			fmt.Fprintln(w, s)
		}
	}
	plain := func(name string, v string) string { return name + " " + v }
	b2f := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	uptime := 0.0
	if !snap.StartedAt.IsZero() && now.After(snap.StartedAt) {
		uptime = now.Sub(snap.StartedAt).Seconds()
	}

	metric("diu_scans_total", "counter", "Number of completed scans.", plain("diu_scans_total", strconv.Itoa(snap.Scans)))
	metric("diu_updates_total", "counter", "Number of containers updated.", plain("diu_updates_total", strconv.Itoa(snap.Updates)))
	metric("diu_failures_total", "counter", "Number of failed updates and image checks.", plain("diu_failures_total", strconv.Itoa(snap.Failures)))
	metric("diu_rollbacks_total", "counter", "Number of updates rolled back.", plain("diu_rollbacks_total", strconv.Itoa(snap.Rollbacks)))
	metric("diu_scanning", "gauge", "Whether a scan is running (1) or not (0).", plain("diu_scanning", b2f(snap.Scanning)))
	metric("diu_containers_watched", "gauge", "Containers checked by the last scan.", plain("diu_containers_watched", strconv.Itoa(snap.Watched)))
	metric("diu_containers_skipped", "gauge", "Containers ignored by the last scan.", plain("diu_containers_skipped", strconv.Itoa(snap.Skipped)))
	metric("diu_last_scan_timestamp_seconds", "gauge", "Start of the last scan, as a Unix time (0 when none yet).",
		plain("diu_last_scan_timestamp_seconds", fnum(unixSeconds(snap.LastScan))))
	metric("diu_last_scan_duration_seconds", "gauge", "Duration of the last scan.",
		plain("diu_last_scan_duration_seconds", fnum(float64(snap.LastScanMs)/1000)))
	metric("diu_next_scan_timestamp_seconds", "gauge", "Planned time of the next scan, as a Unix time (0 when unknown).",
		plain("diu_next_scan_timestamp_seconds", fnum(unixSeconds(snap.NextScan))))
	metric("diu_uptime_seconds", "gauge", "Time since the updater started.", plain("diu_uptime_seconds", fnum(uptime)))
	metric("diu_build_info", "gauge", "Version and mode of the updater; always 1.",
		fmt.Sprintf(`diu_build_info{version="%s",mode="%s"} 1`, labelEscaper.Replace(snap.Version), labelEscaper.Replace(snap.Mode)))

	counts := map[string]int{}
	for _, c := range snap.Containers {
		counts[c.Status]++
	}
	samples := make([]string, 0, len(stats.ContainerStatuses))
	for _, st := range stats.ContainerStatuses {
		samples = append(samples, fmt.Sprintf(`diu_containers{status="%s"} %d`, labelEscaper.Replace(st), counts[st]))
	}
	metric("diu_containers", "gauge", "Containers by status after the last scan.", samples...)
}
