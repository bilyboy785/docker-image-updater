package config

import (
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	c, err := Load(nil, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval != 60*time.Minute || c.Socket != "/var/run/docker.sock" || c.WebAddr != ":8080" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.DryRun || c.WebEnabled || c.TelegramToken != "" || c.StateFile != "" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestFromEnv(t *testing.T) {
	c, err := Load(nil, env(map[string]string{
		"CHECK_INTERVAL_MINUTES": "5",
		"TELEGRAM_BOT_TOKEN":     "tok",
		"TELEGRAM_CHAT_ID":       "42",
		"DRY_RUN":                "true",
		"WEB_ENABLED":            "true",
		"WEB_ADDR":               "127.0.0.1:9000",
		"STATE_FILE":             "/data/state.json",
		"DOCKER_SOCKET":          "/tmp/d.sock",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{5 * time.Minute, "tok", "42", true, true, "127.0.0.1:9000", "/data/state.json", "/tmp/d.sock"}
	if c != want {
		t.Fatalf("got %+v want %+v", c, want)
	}
}

func TestFlagOverridesEnv(t *testing.T) {
	c, err := Load([]string{"-interval", "10", "-dry-run=true"}, env(map[string]string{"CHECK_INTERVAL_MINUTES": "5"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval != 10*time.Minute || !c.DryRun {
		t.Fatalf("got %+v", c)
	}
}

func TestInvalid(t *testing.T) {
	cases := map[string]map[string]string{
		"zero interval":  {"CHECK_INTERVAL_MINUTES": "0"},
		"negative":       {"CHECK_INTERVAL_MINUTES": "-5"},
		"not a number":   {"CHECK_INTERVAL_MINUTES": "abc"},
		"token only":     {"TELEGRAM_BOT_TOKEN": "tok"},
		"chat only":      {"TELEGRAM_CHAT_ID": "42"},
		"bad bool":       {"DRY_RUN": "maybe"},
		"empty web addr": {"WEB_ENABLED": "true", "WEB_ADDR": " "},
	}
	for name, e := range cases {
		if _, err := Load(nil, env(e)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := Load([]string{"-nope"}, env(nil)); err == nil {
		t.Error("unknown flag: expected error")
	}
}
