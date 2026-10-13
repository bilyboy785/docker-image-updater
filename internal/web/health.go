package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bilyboy785/docker-image-updater/internal/stats"
)

// staleness returns "" when the scan loop looks alive and a short "stale: ..." text when it does
// not. The reference time is the latest of the end of the last completed scan, the last scan
// progress heartbeat and the process start: a scan restored from the state file says nothing
// about this process, and a long scan that keeps working is alive.
func (o options) staleness(snap stats.Snapshot, now time.Time) string {
	if o.interval <= 0 {
		return ""
	}
	limit := 2*o.interval + staleSlack
	ref, what := snap.StartedAt, "no scan completed since the start"
	if snap.Scans > 0 && !snap.LastScan.IsZero() {
		if end := snap.LastScan.Add(time.Duration(snap.LastScanMs) * time.Millisecond); end.After(ref) {
			ref, what = end, "last scan completed"
		}
	}
	if snap.LastProgress.After(ref) {
		ref, what = snap.LastProgress, "last scan progress"
	}
	if ref.IsZero() {
		return ""
	}
	age := now.Sub(ref)
	if age <= limit {
		return ""
	}
	return fmt.Sprintf("stale: %s %s ago (limit %s)", what, age.Round(time.Second), limit)
}

// LocalAddr turns a listen address (":8080", "0.0.0.0:8080", "host:8080") into one a local client
// can dial: an unspecified host becomes 127.0.0.1, any other host is kept.
func LocalAddr(listen string) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		return "", fmt.Errorf("invalid web address %q: %w", listen, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

// CheckHealth performs GET /healthz against the web server listening on listen and returns an
// error unless it answers 200 within timeout.
func CheckHealth(listen string, timeout time.Duration) error {
	addr, err := LocalAddr(listen)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz answered HTTP %d", resp.StatusCode)
	}
	return nil
}
