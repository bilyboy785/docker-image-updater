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

// Config is the validated updater configuration.
type Config struct {
	Interval      time.Duration // time between two scans
	TelegramToken string        // Telegram bot token; empty disables notifications
	TelegramChat  string        // Telegram chat ID
	DryRun        bool          // only notify, never recreate containers
	WebEnabled    bool          // serve the statistics page
	WebAddr       string        // listen address of the web server
	StateFile     string        // statistics persistence file; empty disables persistence
	Socket        string        // Docker socket path
	DockerConfig  string        // directory containing the Docker config.json with registry credentials; empty disables it
	CleanupImages bool          // remove the previous image after a successful update
	HealthWait    time.Duration // how long a new container is observed before the update is accepted; 0 disables the gate
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// maxIntervalMinutes caps the check interval at one year.
const maxIntervalMinutes = 525600

// maxHealthWaitSeconds caps the post-start health gate at ten minutes.
const maxHealthWaitSeconds = 600

// flags holds the parsed values of every setting, before validation.
type flags struct {
	interval, token, chat, dry, web, addr, state, sock, dockerCfg, healthWait, cleanup *string
}

// newFlagSet declares every flag. Environment variables provide the defaults.
func newFlagSet(getenv func(string) string) (*flag.FlagSet, flags) {
	fs := flag.NewFlagSet("docker-image-updater", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs, flags{
		interval:   fs.String("interval", envOr(getenv, "CHECK_INTERVAL_MINUTES", "60"), "check interval in minutes, 1 to 525600 (env CHECK_INTERVAL_MINUTES)"),
		token:      fs.String("telegram-token", getenv("TELEGRAM_BOT_TOKEN"), "Telegram bot token (env TELEGRAM_BOT_TOKEN)"),
		chat:       fs.String("telegram-chat", getenv("TELEGRAM_CHAT_ID"), "Telegram chat ID (env TELEGRAM_CHAT_ID)"),
		dry:        fs.String("dry-run", envOr(getenv, "DRY_RUN", "false"), "only notify, never recreate containers (env DRY_RUN)"),
		web:        fs.String("web", envOr(getenv, "WEB_ENABLED", "false"), "enable the stats web server (env WEB_ENABLED)"),
		addr:       fs.String("web-addr", envOr(getenv, "WEB_ADDR", ":8080"), "web server listen address (env WEB_ADDR)"),
		state:      fs.String("state-file", getenv("STATE_FILE"), "file used to persist statistics (env STATE_FILE)"),
		sock:       fs.String("socket", envOr(getenv, "DOCKER_SOCKET", "/var/run/docker.sock"), "Docker socket path (env DOCKER_SOCKET)"),
		dockerCfg:  fs.String("docker-config", getenv("DOCKER_CONFIG"), "directory containing config.json with registry credentials (env DOCKER_CONFIG)"),
		healthWait: fs.String("health-wait", envOr(getenv, "HEALTH_WAIT_SECONDS", "60"), "seconds a new container is watched before the update is accepted, 0 disables the check, 0 to 600 (env HEALTH_WAIT_SECONDS)"),
		cleanup:    fs.String("cleanup-images", envOr(getenv, "CLEANUP_IMAGES", "false"), "remove the previous image after a successful update (env CLEANUP_IMAGES)"),
	}
}

// Usage returns the help text: every flag with its environment variable and default value.
func Usage() string {
	fs, _ := newFlagSet(func(string) string { return "" })
	var b strings.Builder
	b.WriteString("Usage: docker-image-updater [flags]\n\nFlags override environment variables.\n\n")
	fs.SetOutput(&b)
	fs.PrintDefaults()
	return b.String()
}

// Load builds the configuration. Environment variables provide defaults, flags override them.
// It returns flag.ErrHelp (possibly wrapped) when help was requested.
func Load(args []string, getenv func(string) string) (Config, error) {
	fs, f := newFlagSet(getenv)
	interval, token, chat, dry, web, addr, state, sock, dockerCfg, healthWait, cleanup := f.interval, f.token, f.chat, f.dry, f.web, f.addr, f.state, f.sock, f.dockerCfg, f.healthWait, f.cleanup
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	minutes, err := strconv.Atoi(strings.TrimSpace(*interval))
	if err != nil || minutes < 1 || minutes > maxIntervalMinutes {
		return Config{}, fmt.Errorf("invalid check interval %q: must be an integer between 1 and %d (minutes)", *interval, maxIntervalMinutes)
	}
	wait, err := strconv.Atoi(strings.TrimSpace(*healthWait))
	if err != nil || wait < 0 || wait > maxHealthWaitSeconds {
		return Config{}, fmt.Errorf("invalid HEALTH_WAIT_SECONDS %q: must be an integer between 0 and %d (0 disables the health check)", *healthWait, maxHealthWaitSeconds)
	}
	dryRun, err := strconv.ParseBool(*dry)
	if err != nil {
		return Config{}, fmt.Errorf("invalid DRY_RUN %q", *dry)
	}
	webEnabled, err := strconv.ParseBool(*web)
	if err != nil {
		return Config{}, fmt.Errorf("invalid WEB_ENABLED %q", *web)
	}
	cleanupImages, err := strconv.ParseBool(*cleanup)
	if err != nil {
		return Config{}, fmt.Errorf("invalid CLEANUP_IMAGES %q", *cleanup)
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
		DockerConfig:  strings.TrimSpace(*dockerCfg),
		HealthWait:    time.Duration(wait) * time.Second,
		CleanupImages: cleanupImages,
	}, nil
}
