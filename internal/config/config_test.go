package config

import (
	"errors"
	"flag"
	"strings"
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
	if c.CleanupImages {
		t.Fatal("cleanup must be off by default")
	}
	if c.HealthWait != 60*time.Second {
		t.Fatalf("health wait default: %v", c.HealthWait)
	}
	if c.DryRun || c.WebEnabled || c.TelegramToken != "" || c.StateFile != "" || c.DockerConfig != "" {
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
		"DOCKER_CONFIG":          " /config ",
		"HEALTH_WAIT_SECONDS":    "45",
		"CLEANUP_IMAGES":         "true",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{5 * time.Minute, "tok", "42", true, true, "127.0.0.1:9000", "/data/state.json", "/tmp/d.sock", "/config", true, 45 * time.Second, false}
	if c != want {
		t.Fatalf("got %+v want %+v", c, want)
	}
}

func TestDockerConfigFlagOverridesEnv(t *testing.T) {
	c, err := Load([]string{"-docker-config", "/flag"}, env(map[string]string{"DOCKER_CONFIG": "/env"}))
	if err != nil || c.DockerConfig != "/flag" {
		t.Fatalf("%+v %v", c, err)
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
		"bad cleanup":    {"CLEANUP_IMAGES": "later"},
		"empty web addr": {"WEB_ENABLED": "true", "WEB_ADDR": " "},
		"negative wait":  {"HEALTH_WAIT_SECONDS": "-1"},
		"wait too long":  {"HEALTH_WAIT_SECONDS": "601"},
		"wait not a int": {"HEALTH_WAIT_SECONDS": "soon"},
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

func TestHelpRequested(t *testing.T) {
	for _, arg := range []string{"-h", "-help", "--help"} {
		if _, err := Load([]string{arg}, env(nil)); !errors.Is(err, flag.ErrHelp) {
			t.Errorf("%s: expected flag.ErrHelp, got %v", arg, err)
		}
	}
}

func TestUsageListsFlagsEnvVarsAndDefaults(t *testing.T) {
	u := Usage()
	for _, want := range []string{"-interval", "CHECK_INTERVAL_MINUTES", "-telegram-token", "TELEGRAM_BOT_TOKEN",
		"-dry-run", "DRY_RUN", "-web-addr", "WEB_ADDR", "-state-file", "STATE_FILE", "-socket", "DOCKER_SOCKET", "-docker-config", "DOCKER_CONFIG", "-health-wait", "HEALTH_WAIT_SECONDS", "-cleanup-images", "CLEANUP_IMAGES", "/var/run/docker.sock", "60"} {
		if !strings.Contains(u, want) {
			t.Errorf("usage is missing %q:\n%s", want, u)
		}
	}
}

func TestIntervalIsCapped(t *testing.T) {
	if c, err := Load(nil, env(map[string]string{"CHECK_INTERVAL_MINUTES": "525600"})); err != nil || c.Interval != 525600*time.Minute {
		t.Fatalf("%v %v", c.Interval, err)
	}
	if _, err := Load(nil, env(map[string]string{"CHECK_INTERVAL_MINUTES": "525601"})); err == nil {
		t.Fatal("an interval above one year must be rejected")
	}
	if _, err := Load(nil, env(map[string]string{"CHECK_INTERVAL_MINUTES": "99999999999999999999"})); err == nil {
		t.Fatal("overflowing interval must be rejected")
	}
}

func TestHealthWaitBounds(t *testing.T) {
	for in, want := range map[string]time.Duration{"0": 0, "1": time.Second, "600": 600 * time.Second, " 30 ": 30 * time.Second} {
		c, err := Load(nil, env(map[string]string{"HEALTH_WAIT_SECONDS": in}))
		if err != nil || c.HealthWait != want {
			t.Errorf("%q: %v %v", in, c.HealthWait, err)
		}
	}
	if c, err := Load([]string{"-health-wait", "5"}, env(map[string]string{"HEALTH_WAIT_SECONDS": "30"})); err != nil || c.HealthWait != 5*time.Second {
		t.Errorf("flag must override env: %v %v", c.HealthWait, err)
	}
}

func TestCleanupImagesFlagOverridesEnv(t *testing.T) {
	c, err := Load([]string{"-cleanup-images=false"}, env(map[string]string{"CLEANUP_IMAGES": "true"}))
	if err != nil || c.CleanupImages {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestHealthcheckFlag(t *testing.T) {
	cfg, err := Load([]string{"-healthcheck"}, func(string) string { return "" })
	if err != nil || !cfg.HealthCheck {
		t.Fatalf("%+v %v", cfg, err)
	}
	cfg, err = Load(nil, func(string) string { return "" })
	if err != nil || cfg.HealthCheck {
		t.Fatalf("%+v %v", cfg, err)
	}
	if !strings.Contains(Usage(), "-healthcheck") {
		t.Fatal("the flag must be documented in the usage")
	}
}
