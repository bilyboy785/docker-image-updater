// Package notify sends update notifications.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Notifier delivers a notification message.
type Notifier interface {
	// Notify sends a message formatted with Telegram's HTML subset.
	Notify(ctx context.Context, text string) error
}

// Nop is used when Telegram is not configured.
type Nop struct{}

// Notify does nothing.
func (Nop) Notify(context.Context, string) error { return nil }

// Telegram sends notifications through the Telegram Bot API.
type Telegram struct {
	token, chat, base string
	hc                *http.Client
}

// NewTelegram returns a notifier sending to the given chat with the given bot token.
func NewTelegram(token, chat string) *Telegram {
	return &Telegram{token: token, chat: chat, base: "https://api.telegram.org", hc: &http.Client{Timeout: 15 * time.Second}}
}

// WithBaseURL overrides the API base URL (used by tests) and returns t.
func (t *Telegram) WithBaseURL(u string) *Telegram {
	t.base = strings.TrimRight(u, "/")
	return t
}

// Notify sends text as an HTML message. Errors never contain the bot token.
func (t *Telegram) Notify(ctx context.Context, text string) error {
	body, _ := json.Marshal(map[string]any{
		"chat_id":                  t.chat,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", t.base+"/bot"+t.token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return errors.New(t.redact(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.hc.Do(req)
	if err != nil {
		// net/http errors embed the request URL, which contains the bot token.
		return errors.New(t.redact(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if resp.StatusCode == http.StatusTooManyRequests {
			return &RateLimitError{RetryAfter: parseRetryAfter(msg, resp.Header.Get("Retry-After"))}
		}
		return &StatusError{Code: resp.StatusCode, Body: t.redact(strings.TrimSpace(string(msg)))}
	}
	return nil
}

// Bounds and default of the delay Telegram asks us to wait after an HTTP 429.
const (
	defaultRetryAfter = 5 * time.Second
	minRetryAfter     = time.Second
	maxRetryAfter     = 10 * time.Minute
)

// RateLimitError is returned when Telegram answers HTTP 429.
type RateLimitError struct {
	// RetryAfter is how long to wait before sending again, clamped to [1s, 10min].
	RetryAfter time.Duration
}

// Error never contains the bot token nor the response body.
func (e *RateLimitError) Error() string {
	return fmt.Sprintf("telegram: rate limited, retry after %s", e.RetryAfter)
}

// StatusError is returned for any other non-200 answer.
type StatusError struct {
	Code int
	Body string // already redacted
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("telegram: status %d: %s", e.Code, e.Body)
}

// IsRetryable reports whether sending the same message again may succeed:
// true for rate limits, server errors, timeouts and transport errors, false for
// other HTTP 4xx answers (bad chat id, invalid HTML, revoked token...).
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code < 400 || se.Code >= 500 || se.Code == http.StatusRequestTimeout
	}
	return true
}

// parseRetryAfter reads parameters.retry_after from the JSON body, then the
// Retry-After header, then falls back to a default; the result is clamped.
func parseRetryAfter(body []byte, header string) time.Duration {
	secs, ok := int64(0), false
	var r struct {
		Parameters struct {
			RetryAfter *int64 `json:"retry_after"`
		} `json:"parameters"`
	}
	if json.Unmarshal(body, &r) == nil && r.Parameters.RetryAfter != nil {
		secs, ok = *r.Parameters.RetryAfter, true
	} else if n, err := strconv.ParseInt(strings.TrimSpace(header), 10, 64); err == nil {
		secs, ok = n, true
	}
	d := defaultRetryAfter
	if ok {
		switch {
		case secs > int64(maxRetryAfter/time.Second):
			d = maxRetryAfter
		case secs < 1:
			d = minRetryAfter
		default:
			d = time.Duration(secs) * time.Second
		}
	}
	return d
}

func (t *Telegram) redact(s string) string {
	if t.token == "" {
		return s
	}
	return strings.ReplaceAll(s, t.token, "[redacted]")
}
