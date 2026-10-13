package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bilyboy785/docker-image-updater/internal/config"
)

func TestRunHealthcheck(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	cfg := config.Config{WebEnabled: true, WebAddr: strings.TrimPrefix(srv.URL, "http://")}
	if err := runHealthcheck(cfg, time.Second); err != nil {
		t.Fatalf("success: %v", err)
	}
	status = http.StatusServiceUnavailable
	if err := runHealthcheck(cfg, time.Second); err == nil {
		t.Fatal("503 must fail")
	}
	srv.Close()
	if err := runHealthcheck(cfg, time.Second); err == nil {
		t.Fatal("connection refused must fail")
	}
}

func TestRunHealthcheckDisabledWebServerIsHealthy(t *testing.T) {
	// Nothing listens there: the check must not even try.
	if err := runHealthcheck(config.Config{WebEnabled: false, WebAddr: "127.0.0.1:1"}, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestConfigErrorExitCode(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{
		{nil, 2},
		{[]string{"-interval", "0"}, 2},
		{[]string{"-healthcheck", "-interval", "0"}, 1},
		{[]string{"--healthcheck"}, 1},
		{[]string{"-interval", "0", "-healthcheck=true"}, 1},
		{[]string{"-healthcheck=false", "-interval", "0"}, 2},
	} {
		if got := configErrorExitCode(tc.args); got != tc.want {
			t.Errorf("%v: got %d want %d", tc.args, got, tc.want)
		}
	}
}
