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
