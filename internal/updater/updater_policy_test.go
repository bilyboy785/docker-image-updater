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

// updatable returns a fake daemon with one container "a" whose image has a newer version.
func updatable(labels map[string]string) *fakeDocker {
	return &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", labels)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"}}
}

func clockAt(u *Updater, h, m int) {
	u.now = func() time.Time { return time.Date(2026, 1, 2, h, m, 0, 0, time.Local) }
}

func hasMutation(calls []string) bool {
	for _, c := range calls {
		if !strings.HasPrefix(c, "pull") {
			return true
		}
	}
	return false
}

func TestNotifyPolicyAnnouncesOnce(t *testing.T) {
	f := updatable(map[string]string{PolicyLabel: "notify"})
	u, n, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	u.RunOnce(context.Background())
	if hasMutation(f.calls) {
		t.Fatalf("notify must never recreate: %v", f.calls)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "(policy: notify)") {
		t.Fatalf("%v", n.msgs)
	}
	snap := s.Snapshot()
	if len(snap.Events) != 1 || snap.Events[0].Status != stats.StatusAvailable {
		t.Fatalf("%+v", snap.Events)
	}
}

func TestNotifyPolicyIgnoresWindow(t *testing.T) {
	f := updatable(map[string]string{PolicyLabel: "notify", WindowLabel: "02:00-03:00"})
	u, n, _ := newUpdater(f, false)
	clockAt(u, 12, 0)
	u.RunOnce(context.Background())
	if len(n.msgs) != 1 || hasMutation(f.calls) {
		t.Fatalf("%v %v", n.msgs, f.calls)
	}
}

func TestClosedWindowDoesNothing(t *testing.T) {
	f := updatable(map[string]string{WindowLabel: "02:00-03:00"})
	u, n, s := newUpdater(f, false)
	clockAt(u, 12, 0)
	u.RunOnce(context.Background())
	u.RunOnce(context.Background())
	if hasMutation(f.calls) || len(n.msgs) != 0 || len(s.Snapshot().Events) != 0 {
		t.Fatalf("%v %v %+v", f.calls, n.msgs, s.Snapshot().Events)
	}
}

func TestUpdateHappensOnceWindowOpens(t *testing.T) {
	f := updatable(map[string]string{WindowLabel: "02:00-03:00"})
	u, _, s := newUpdater(f, false)
	clockAt(u, 12, 0)
	u.RunOnce(context.Background())
	clockAt(u, 2, 30)
	u.RunOnce(context.Background())
	if s.Snapshot().Updates != 1 || !hasMutation(f.calls) {
		t.Fatalf("%v %+v", f.calls, s.Snapshot())
	}
}

func TestInvalidLabelsFailClosedWithOneWarning(t *testing.T) {
	for _, labels := range []map[string]string{
		{WindowLabel: "25:00-26:00"},
		{PolicyLabel: "sometimes"},
	} {
		f := updatable(labels)
		u, n, s := newUpdater(f, false)
		var buf bytes.Buffer
		u.log = slog.New(slog.NewTextHandler(&buf, nil))
		u.RunOnce(context.Background())
		u.RunOnce(context.Background())
		if hasMutation(f.calls) || len(n.msgs) != 0 || len(s.Snapshot().Events) != 0 {
			t.Fatalf("%v: %v %v %+v", labels, f.calls, n.msgs, s.Snapshot().Events)
		}
		if got := strings.Count(buf.String(), "level=WARN"); got != 1 {
			t.Fatalf("%v: %d warnings: %s", labels, got, buf.String())
		}
	}
}

func TestLocalImageIsSkippedSilently(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{
		web("a", "local-only:1", "sha256:1", nil), web("b", "local-only:1", "sha256:1", nil),
		web("c", "nginx:latest", "sha256:1", nil)},
		info:        map[string]*docker.ContainerInfo{"c": basicInfo("c")},
		images:      map[string]string{"nginx:latest": "sha256:1"},
		localImages: map[string]bool{"sha256:1": true},
		pullErr:     map[string]error{"local-only:1": errors.New("pull access denied")}}
	// Only the first image id is local: give nginx another id.
	f.containers[2].ImageID = "sha256:9"
	f.images["nginx:latest"] = "sha256:9"
	u, n, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	if strings.Contains(strings.Join(f.calls, "|"), "local-only") {
		t.Fatalf("a local image must not be pulled: %v", f.calls)
	}
	snap := s.Snapshot()
	if len(n.msgs) != 0 || len(snap.Events) != 0 || snap.Failures != 0 || snap.Watched != 1 || snap.Skipped != 2 {
		t.Fatalf("%v %+v", n.msgs, snap)
	}
}

func TestRepoDigestsFailureFallsBackToPullWithOneWarning(t *testing.T) {
	f := updatable(nil)
	f.failOn = map[string]error{"RepoDigests": errors.New("boom")}
	u, _, _ := newUpdater(f, false)
	var buf bytes.Buffer
	u.log = slog.New(slog.NewTextHandler(&buf, nil))
	u.RunOnce(context.Background())
	u.RunOnce(context.Background())
	if !strings.Contains(strings.Join(f.calls, "|"), "pull nginx:latest") {
		t.Fatalf("must fall back to pulling: %v", f.calls)
	}
	if got := strings.Count(buf.String(), "level=WARN"); got != 1 {
		t.Fatalf("%d warnings: %s", got, buf.String())
	}
}

func TestImageWithDigestsFollowsNormalPath(t *testing.T) {
	f := updatable(nil)
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	if f.digestCalls != 1 || s.Snapshot().Updates != 1 || s.Snapshot().Watched != 1 || s.Snapshot().Skipped != 0 {
		t.Fatalf("%d %+v", f.digestCalls, s.Snapshot())
	}
}
