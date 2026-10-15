// Package web serves the optional statistics page.
package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bilyboy785/docker-image-updater/internal/stats"
)

//go:embed index.html
var indexHTML []byte

const csp = "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'"

// ScanStatus is the outcome of a manual scan request.
type ScanStatus int

const (
	// ScanStarted means the scan request was accepted.
	ScanStarted ScanStatus = iota
	// ScanAlreadyRunning means a scan is already in progress.
	ScanAlreadyRunning
	// ScanTooSoon means the last scan started too recently; see ScanResult.RetryAfter.
	ScanTooSoon
)

// ScanRequest is what a manual scan request asks for: every container (zero value), or only the one
// called Container, which is always the name of a container of the last published list.
type ScanRequest struct {
	Container string
}

// maxContainerName is the longest container name accepted by POST /api/scan.
const maxContainerName = 255

// validContainerName reports whether v can be a container name: not blank, at most maxContainerName
// bytes, valid UTF-8, no control character and no slash. It is only a sanity check: the value is then
// compared as is with the known names, never used otherwise.
func validContainerName(v string) bool {
	if strings.TrimSpace(v) == "" || len(v) > maxContainerName || !utf8.ValidString(v) {
		return false
	}
	for _, r := range v {
		if r == '/' || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ScanResult is returned by the trigger passed to Handler. RetryAfter is only used for ScanTooSoon.
type ScanResult struct {
	Status     ScanStatus
	RetryAfter time.Duration
}

// requestedWith is the value required in the X-Requested-With header of a scan request. A cross-site
// form cannot set a custom header, and a cross-origin fetch with one needs a CORS preflight that this server never grants.
const requestedWith = "docker-image-updater"

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// sameOrigin reports whether the Origin header, when present, designates the host the request was sent to.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && u.Host == r.Host
}

// Option customises Handler.
type Option func(*options)

type options struct {
	now      func() time.Time
	interval time.Duration // scan interval used by the /healthz staleness rule; 0 disables the rule
}

// WithHealth makes /healthz report 503 when the scan loop looks dead: no scan completed within
// two scan intervals plus staleSlack (the time reserved for a slow step) of the last sign of life:
// the end of the last scan, the last progress of the scan in progress, or the process start. Without it /healthz always answers 200.
func WithHealth(interval time.Duration) Option {
	return func(o *options) { o.interval = interval }
}

// staleSlack covers the 30 minute image pull timeout of the scan in progress, and then some.
const staleSlack = 35 * time.Minute

// WithClock sets the clock used for time-dependent output (default time.Now).
func WithClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// Handler returns the HTTP handler serving the page (/), the JSON statistics (/api/stats), the
// Prometheus metrics (/metrics), /healthz and POST /api/scan, which asks for an immediate scan
// through trigger (answers 501 when trigger is nil). With ?container=<name> the scan only covers that
// container, which must be in the container list of snap (404 otherwise, 400 for a malformed name).
func Handler(snap func() stats.Snapshot, trigger func(ScanRequest) ScanResult, opts ...Option) http.Handler {
	o := options{now: time.Now}
	for _, opt := range opts {
		opt(&o)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(snap())
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", metricsContentType)
		w.Header().Set("Cache-Control", "no-store")
		writeMetrics(w, snap(), o.now())
	})
	mux.HandleFunc("POST /api/scan", func(w http.ResponseWriter, r *http.Request) {
		// The request body is never read.
		if r.Header.Get("X-Requested-With") != requestedWith || !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		if trigger == nil {
			writeError(w, http.StatusNotImplemented, "manual scans are not available")
			return
		}
		var req ScanRequest
		started := struct {
			Status    string `json:"status"`
			Container string `json:"container,omitempty"`
		}{Status: "started"}
		query, err := url.ParseQuery(r.URL.RawQuery)
		values, present := query["container"]
		if err != nil || (present && (len(values) != 1 || !validContainerName(values[0]))) {
			writeError(w, http.StatusBadRequest, "invalid container name")
			return
		}
		if present {
			known := false
			for _, c := range snap().Containers {
				if c.Name == values[0] {
					known = true
					break
				}
			}
			if !known {
				writeError(w, http.StatusNotFound, "unknown container")
				return
			}
			req.Container = values[0]
			started.Container = req.Container
		}
		res := trigger(req)
		switch res.Status {
		case ScanStarted:
			writeJSON(w, http.StatusAccepted, started)
		case ScanAlreadyRunning:
			writeError(w, http.StatusConflict, "scan already running")
		default:
			secs := int(math.Ceil(res.RetryAfter.Seconds()))
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeError(w, http.StatusTooManyRequests, "last scan started too recently, retry later")
		}
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if msg := o.staleness(snap(), o.now()); msg != "" {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(msg))
			return
		}
		w.Write([]byte("ok"))
	})
	return mux
}

// Serve blocks until ctx is cancelled, then shuts the server down gracefully.
func Serve(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
