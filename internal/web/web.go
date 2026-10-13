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
	"time"

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

// Handler returns the HTTP handler serving the page (/), the JSON statistics (/api/stats), /healthz
// and POST /api/scan, which asks for an immediate scan through trigger (answers 501 when trigger is nil).
func Handler(snap func() stats.Snapshot, trigger func() ScanResult) http.Handler {
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
		res := trigger()
		switch res.Status {
		case ScanStarted:
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
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
