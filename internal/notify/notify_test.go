package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNotifySendsMessage(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	tg := NewTelegram("TOKEN123", "42").WithBaseURL(srv.URL)
	if err := tg.Notify(context.Background(), "<b>hi</b>"); err != nil {
		t.Fatal(err)
	}
	if path != "/botTOKEN123/sendMessage" {
		t.Fatalf("path %s", path)
	}
	if got["chat_id"] != "42" || got["text"] != "<b>hi</b>" || got["parse_mode"] != "HTML" {
		t.Fatalf("body %v", got)
	}
}

func TestNotifyHTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"ok":false,"description":"chat not found"}`, 400)
	}))
	defer srv.Close()
	err := NewTelegram("TOKEN123", "42").WithBaseURL(srv.URL).Notify(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "TOKEN123") {
		t.Fatal("token leaked")
	}
}

func TestNotifyNeverLeaksTokenOnTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // connection refused: the resulting url.Error embeds the full URL
	err := NewTelegram("TOKEN123", "42").WithBaseURL(url).Notify(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "TOKEN123") {
		t.Fatalf("token leaked: %v", err)
	}
}

func TestNop(t *testing.T) {
	if err := (Nop{}).Notify(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
}

func rateLimitServer(status int, header string, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if header != "" {
			w.Header().Set("Retry-After", header)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
}

func TestRateLimitParsing(t *testing.T) {
	cases := []struct {
		name, header, body string
		want               time.Duration
	}{
		{"body", "", `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 7","parameters":{"retry_after":7}}`, 7 * time.Second},
		{"body wins over header", "30", `{"parameters":{"retry_after":7}}`, 7 * time.Second},
		{"header fallback", "12", `{"ok":false}`, 12 * time.Second},
		{"default", "", `not json`, 5 * time.Second},
		{"unparsable header", "soon", ``, 5 * time.Second},
		{"clamp low", "", `{"parameters":{"retry_after":0}}`, time.Second},
		{"clamp high", "", `{"parameters":{"retry_after":999999}}`, 10 * time.Minute},
		{"negative header", "-4", ``, time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := rateLimitServer(429, c.header, c.body)
			defer srv.Close()
			err := NewTelegram("TOKEN123", "42").WithBaseURL(srv.URL).Notify(context.Background(), "x")
			var rl *RateLimitError
			if !errors.As(err, &rl) {
				t.Fatalf("want *RateLimitError, got %T %v", err, err)
			}
			if rl.RetryAfter != c.want {
				t.Fatalf("retry after %v, want %v", rl.RetryAfter, c.want)
			}
			if strings.Contains(err.Error(), "TOKEN123") {
				t.Fatal("token leaked")
			}
			if !IsRetryable(err) {
				t.Fatal("rate limit must be retryable")
			}
		})
	}
}

func TestRateLimitErrorTextHasNoToken(t *testing.T) {
	srv := rateLimitServer(429, "", `{"description":"bad TOKEN123 here","parameters":{"retry_after":2}}`)
	defer srv.Close()
	err := NewTelegram("TOKEN123", "42").WithBaseURL(srv.URL).Notify(context.Background(), "x")
	if err == nil || strings.Contains(err.Error(), "TOKEN123") {
		t.Fatalf("got %v", err)
	}
}

func TestIsRetryable(t *testing.T) {
	for code, want := range map[int]bool{400: false, 401: false, 403: false, 404: false, 408: true, 429: true, 500: true, 502: true, 503: true} {
		srv := rateLimitServer(code, "", `{"description":"x"}`)
		err := NewTelegram("TOKEN123", "42").WithBaseURL(srv.URL).Notify(context.Background(), "x")
		srv.Close()
		if err == nil {
			t.Fatalf("%d: expected error", code)
		}
		if got := IsRetryable(err); got != want {
			t.Errorf("status %d: IsRetryable=%v, want %v", code, got, want)
		}
	}
	// Transport errors are retryable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	if err := NewTelegram("TOKEN123", "42").WithBaseURL(url).Notify(context.Background(), "x"); !IsRetryable(err) {
		t.Fatalf("transport error should be retryable: %v", err)
	}
	if IsRetryable(nil) {
		t.Fatal("nil is not retryable")
	}
}
