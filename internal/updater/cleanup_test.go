package updater

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bilyboy785/docker-image-updater/internal/docker"
	"github.com/bilyboy785/docker-image-updater/internal/stats"
)

func cleanupSetup(enabled bool) (*Updater, *fakeDocker, *stats.Stats) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"}}
	u, _, s := newUpdater(f, false)
	u.WithCleanupImages(enabled)
	return u, f, s
}

func TestCleanupRemovesTheOldImageAfterTheOldContainer(t *testing.T) {
	u, f, s := cleanupSetup(true)
	u.RunOnce(context.Background())
	if got := strings.Join(f.calls, "|"); got != fullSequence+"|rmi sha256:1" {
		t.Fatalf("got %s", got)
	}
	if s.Snapshot().Updates != 1 {
		t.Fatalf("%+v", s.Snapshot())
	}
}

func TestCleanupDisabledByDefault(t *testing.T) {
	u, f, _ := cleanupSetup(false)
	u.RunOnce(context.Background())
	if got := strings.Join(f.calls, "|"); got != fullSequence {
		t.Fatalf("got %s", got)
	}
}

func TestCleanupSkippedWhenImageIDsAreEqual(t *testing.T) {
	u, f, _ := cleanupSetup(true)
	// The container reports a stale image id, but the pulled image has the very same id.
	f.containers[0].ImageID = "sha256:2"
	f.images["nginx:latest"] = "sha256:2"
	u.RunOnce(context.Background())
	if indexOf(f.calls, "rmi sha256:2") != -1 {
		t.Fatalf("%v", f.calls)
	}
	u.cleanupImage(context.Background(), "a", "sha256:2", "sha256:2")
	if indexOf(f.calls, "rmi sha256:2") != -1 {
		t.Fatalf("%v", f.calls)
	}
}

func TestCleanupIgnoresConflictAndNotFound(t *testing.T) {
	for _, status := range []int{404, 409} {
		u, f, s := cleanupSetup(true)
		f.rmiErr = &docker.APIError{Status: status, Message: "refused"}
		var logs bytes.Buffer
		u.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		u.RunOnce(context.Background())
		snap := s.Snapshot()
		if snap.Updates != 1 || snap.Failures != 0 || snap.Events[0].Status != stats.StatusUpdated {
			t.Fatalf("%d: %+v", status, snap)
		}
		if strings.Contains(logs.String(), "level=WARN") {
			t.Fatalf("%d must not warn: %s", status, logs.String())
		}
	}
}

func TestCleanupOtherErrorsAreWarnedAndNeverFailTheUpdate(t *testing.T) {
	u, f, s := cleanupSetup(true)
	f.rmiErr = errors.New("disk on fire")
	var logs bytes.Buffer
	u.log = slog.New(slog.NewTextHandler(&logs, nil))
	u.RunOnce(context.Background())
	if snap := s.Snapshot(); snap.Updates != 1 || snap.Failures != 0 {
		t.Fatalf("%+v", snap)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "disk on fire") {
		t.Fatalf("%s", logs.String())
	}
}

func TestCleanupLogsInfoWhenRemoved(t *testing.T) {
	u, _, _ := cleanupSetup(true)
	var logs bytes.Buffer
	u.log = slog.New(slog.NewTextHandler(&logs, nil))
	u.RunOnce(context.Background())
	if !strings.Contains(logs.String(), "level=INFO") || !strings.Contains(logs.String(), "removed old image") {
		t.Fatalf("%s", logs.String())
	}
}

func TestCleanupNotCalledAfterAGateRollback(t *testing.T) {
	u, f, s, _ := func() (*Updater, *fakeDocker, *stats.Stats, *gateClock) {
		u, f, _, s, c := gateSetup(60*time.Second, running("unhealthy"))
		return u, f, s, c
	}()
	u.WithCleanupImages(true)
	u.RunOnce(context.Background())
	for _, c := range f.calls {
		if strings.HasPrefix(c, "rmi") {
			t.Fatalf("no image removal after a rollback: %v", f.calls)
		}
	}
	if s.Snapshot().Rollbacks != 1 {
		t.Fatalf("%+v", s.Snapshot())
	}
}

func TestCleanupNotCalledWhenTheOldContainerCouldNotBeRemoved(t *testing.T) {
	u, f, _ := cleanupSetup(true)
	f.failOn = map[string]error{"RemoveOld": errors.New("busy")}
	u.RunOnce(context.Background())
	if indexOf(f.calls, "rmi sha256:1") != -1 {
		t.Fatalf("the old container still uses the image: %v", f.calls)
	}
}

func TestCleanupRunsAfterTheGateSucceeded(t *testing.T) {
	u, f, _, _, _ := gateSetup(60*time.Second, running("healthy"))
	u.WithCleanupImages(true)
	u.RunOnce(context.Background())
	if got := strings.Join(f.calls, "|"); got != beforeGate+"|inspect newid|remove a|rmi sha256:1" {
		t.Fatalf("got %s", got)
	}
}
