// Package config reads the updater settings from environment variables and flags.
package config

import (
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Interval      time.Duration
	TelegramToken string
	TelegramChat  string
	DryRun        bool
	WebEnabled    bool
	WebAddr       string
	StateFile     string
	Socket        string
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// Load builds the configuration. Environment variables provide defaults, flags override them.
func Load(args []string, getenv func(string) string) (Config, error) {
	fs := flag.NewFlagSet("docker-image-updater", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	interval := fs.String("interval", envOr(getenv, "CHECK_INTERVAL_MINUTES", "60"), "check interval in minutes")
	token := fs.String("telegram-token", getenv("TELEGRAM_BOT_TOKEN"), "Telegram bot token")
	chat := fs.String("telegram-chat", getenv("TELEGRAM_CHAT_ID"), "Telegram chat ID")
	dry := fs.String("dry-run", envOr(getenv, "DRY_RUN", "false"), "only notify, never recreate containers")
	web := fs.String("web", envOr(getenv, "WEB_ENABLED", "false"), "enable the stats web server")
	addr := fs.String("web-addr", envOr(getenv, "WEB_ADDR", ":8080"), "web server listen address")
	state := fs.String("state-file", getenv("STATE_FILE"), "file used to persist statistics")
	sock := fs.String("socket", envOr(getenv, "DOCKER_SOCKET", "/var/run/docker.sock"), "Docker socket path")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	minutes, err := strconv.Atoi(strings.TrimSpace(*interval))
	if err != nil || minutes < 1 {
		return Config{}, fmt.Errorf("invalid check interval %q: must be an integer >= 1 (minutes)", *interval)
	}
	dryRun, err := strconv.ParseBool(*dry)
	if err != nil {
		return Config{}, fmt.Errorf("invalid DRY_RUN %q", *dry)
	}
	webEnabled, err := strconv.ParseBool(*web)
	if err != nil {
		return Config{}, fmt.Errorf("invalid WEB_ENABLED %q", *web)
	}
	if (*token == "") != (*chat == "") {
		return Config{}, fmt.Errorf("TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID must be set together")
	}
	if webEnabled && strings.TrimSpace(*addr) == "" {
		return Config{}, fmt.Errorf("WEB_ADDR must not be empty when the web server is enabled")
	}
	return Config{
		Interval:      time.Duration(minutes) * time.Minute,
		TelegramToken: *token,
		TelegramChat:  *chat,
		DryRun:        dryRun,
		WebEnabled:    webEnabled,
		WebAddr:       *addr,
		StateFile:     *state,
		Socket:        *sock,
	}, nil
}
