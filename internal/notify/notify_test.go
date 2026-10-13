package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
