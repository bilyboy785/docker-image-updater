package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bilyboy785/docker-image-updater/internal/updater"
	"github.com/bilyboy785/docker-image-updater/internal/web"
)

func TestTriggerImageMapsTheSchedulerStatus(t *testing.T) {
	s := updater.NewScheduler(func(context.Context) error { return nil }, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if r := triggerImage(s, "docker.io/library/app:1"); r.Status != web.UpdateQueued {
		t.Fatalf("got %+v", r)
	}
	for i := 0; i < 200; i++ { // far more distinct images than the queue holds
		if r := triggerImage(s, fmt.Sprintf("docker.io/library/app:%d", i+2)); r.Status == web.UpdateQueueFull {
			return
		}
	}
	t.Fatal("the queue never reported being full")
}
