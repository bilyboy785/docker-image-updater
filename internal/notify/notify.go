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
	"strings"
	"time"
)

type Notifier interface {
	// Notify sends a message formatted with Telegram's HTML subset.
	Notify(ctx context.Context, text string) error
}

// Nop is used when Telegram is not configured.
type Nop struct{}

func (Nop) Notify(context.Context, string) error { return nil }

type Telegram struct {
	token, chat, base string
	hc                *http.Client
}

func NewTelegram(token, chat string) *Telegram {
	return &Telegram{token: token, chat: chat, base: "https://api.telegram.org", hc: &http.Client{Timeout: 15 * time.Second}}
}

func (t *Telegram) WithBaseURL(u string) *Telegram {
	t.base = strings.TrimRight(u, "/")
	return t
}

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
		return fmt.Errorf("telegram: status %d: %s", resp.StatusCode, t.redact(strings.TrimSpace(string(msg))))
	}
	return nil
}

func (t *Telegram) redact(s string) string {
	if t.token == "" {
		return s
	}
	return strings.ReplaceAll(s, t.token, "[redacted]")
}
