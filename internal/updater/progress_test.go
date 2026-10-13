package updater

import (
	"context"
	"testing"
	"time"

	"github.com/bilyboy785/docker-image-updater/internal/docker"
)

// A scan that keeps working must keep touching the progress heartbeat of the stats store: at its
// start, when it starts on an image, and after each pull.
func TestScanTouchesTheProgressHeartbeat(t *testing.T) {
	f := &fakeDocker{
		containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil), web("b", "redis:7", "sha256:3", nil)},
		info:       map[string]*docker.ContainerInfo{},
		images:     map[string]string{"nginx:latest": "sha256:1", "redis:7": "sha256:3"},
	}
	u, _, s := newUpdater(f, false)

	var beforeFirstPull time.Time
	var pulledAt time.Time
	pulls := 0
	f.onPull = func() {
		pulls++
		if pulls == 1 {
			beforeFirstPull = s.Snapshot().LastProgress
		}
		time.Sleep(3 * time.Millisecond)
		pulledAt = time.Now()
	}
	if err := u.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if beforeFirstPull.IsZero() {
		t.Fatal("the scan must touch the heartbeat before it pulls anything")
	}
	if got := s.Snapshot().LastProgress; !got.After(pulledAt) {
		t.Fatalf("heartbeat %v must be touched after the last pull (%v)", got, pulledAt)
	}
}

// A scan that cannot even list the containers makes no progress: the heartbeat must stay unset so
// that /healthz still goes stale when the Docker socket is unreachable.
func TestFailedListDoesNotTouchTheHeartbeat(t *testing.T) {
	f := &fakeDocker{failOn: map[string]error{"List": context.DeadlineExceeded}}
	u, _, s := newUpdater(f, false)
	if err := u.RunOnce(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	if !s.Snapshot().LastProgress.IsZero() {
		t.Fatalf("heartbeat touched by a failed list: %v", s.Snapshot().LastProgress)
	}
}

// lookupProbe records the heartbeat at the moment the image lookup that follows a pull runs: only
// the touch right after the pull can explain a value newer than the pull.
type lookupProbe struct {
	*fakeDocker
	probe func()
}

func (l *lookupProbe) ImageID(ctx context.Context, ref string) (string, error) {
	l.probe()
	return l.fakeDocker.ImageID(ctx, ref)
}

func TestHeartbeatIsTouchedAfterThePull(t *testing.T) {
	f := &fakeDocker{
		containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		images:     map[string]string{"nginx:latest": "sha256:1"},
	}
	u, _, s := newUpdater(f, false)
	var pulledAt, seen time.Time
	f.onPull = func() {
		time.Sleep(3 * time.Millisecond) // the touches before the pull are older than this
		pulledAt = time.Now()
	}
	u.docker = &lookupProbe{f, func() { seen = s.Snapshot().LastProgress }}
	u.RunOnce(context.Background())
	if pulledAt.IsZero() || !seen.After(pulledAt) {
		t.Fatalf("heartbeat %v must be touched after the pull (%v)", seen, pulledAt)
	}
}
