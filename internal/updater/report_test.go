package updater

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bilyboy785/docker-image-updater/internal/docker"
	"github.com/bilyboy785/docker-image-updater/internal/stats"
)

// entry returns the container list entry named name after one scan.
func entry(t *testing.T, s *stats.Stats, name string) stats.ContainerStatus {
	t.Helper()
	for _, c := range s.Snapshot().Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no entry for %q in %+v", name, s.Snapshot().Containers)
	return stats.ContainerStatus{}
}

func wantEntry(t *testing.T, got stats.ContainerStatus, status, detail string) {
	t.Helper()
	if got.Status != status || !strings.Contains(got.Detail, detail) {
		t.Fatalf("want status %q detail containing %q, got %+v", status, detail, got)
	}
	if got.CheckedAt.IsZero() {
		t.Fatalf("CheckedAt not set: %+v", got)
	}
}

func scan(f *fakeDocker, dry bool) (*Updater, *stats.Stats) {
	u, _, s := newUpdater(f, dry)
	u.RunOnce(context.Background())
	return u, s
}

func TestReportUpToDate(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1234567890abcdef", map[string]string{WindowLabel: "02:00-03:00"})},
		images: map[string]string{"nginx:latest": "sha256:1234567890abcdef"}}
	_, s := scan(f, false)
	e := entry(t, s, "a")
	wantEntry(t, e, stats.ContainerUpToDate, "")
	if e.Image != "nginx:latest" || e.ImageID != "1234567890ab" || e.Policy != "auto" || e.Window != "02:00-03:00" {
		t.Fatalf("%+v", e)
	}
}

func TestReportSkippedKinds(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{
		web("optout", "nginx:latest", "sha256:1", map[string]string{OptOutLabel: "false"}),
		web("pinned", "nginx@sha256:abc", "sha256:1", nil),
		web("bare", "sha256:0123456789abcdef", "sha256:1", nil),
		web("local", "mine:1", "sha256:7", nil)},
		info:        map[string]*docker.ContainerInfo{"bare": {Config: map[string]any{"Image": "sha256:0123456789abcdef"}}},
		localImages: map[string]bool{"sha256:7": true}}
	_, s := scan(f, false)
	wantEntry(t, entry(t, s, "optout"), stats.ContainerSkipped, "opted out (enable=false)")
	wantEntry(t, entry(t, s, "pinned"), stats.ContainerSkipped, "pinned by digest")
	wantEntry(t, entry(t, s, "bare"), stats.ContainerSkipped, "image reference cannot be resolved")
	wantEntry(t, entry(t, s, "local"), stats.ContainerSkipped, "locally built image")
}

func TestReportSelf(t *testing.T) {
	self := map[string]string{SelfLabel: "true"}
	f := &fakeDocker{containers: []docker.Container{web("me", "diu:1", "sha256:1", self)},
		images: map[string]string{"diu:1": "sha256:1"}}
	_, s := scan(f, false)
	wantEntry(t, entry(t, s, "me"), stats.ContainerSelf, "never updates itself")

	f = &fakeDocker{containers: []docker.Container{web("me", "diu:1", "sha256:1", self)},
		images: map[string]string{"diu:1": "sha256:2"}}
	_, s = scan(f, false)
	wantEntry(t, entry(t, s, "me"), stats.ContainerUpdateAvailable, "manual update required")
}

func TestReportPullAndImageIDFailures(t *testing.T) {
	long := strings.Repeat("x", 500)
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		pullErr: map[string]error{"nginx:latest": errors.New(long)}}
	_, s := scan(f, false)
	e := entry(t, s, "a")
	wantEntry(t, e, stats.ContainerFailed, "xxx")
	if n := len([]rune(e.Detail)); n > 200 {
		t.Fatalf("detail not truncated: %d", n)
	}

	f = &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		images: map[string]string{"nginx:latest": "sha256:1"}}
	u, n, s2 := newUpdater(f, false)
	_ = n
	u.docker = &imageIDFails{f}
	u.RunOnce(context.Background())
	wantEntry(t, entry(t, s2, "a"), stats.ContainerFailed, "no such image")
}

func TestReportUpdateAvailableReasons(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		dry    bool
		hostCf map[string]any
		detail string
		policy string
	}{
		{"notify", map[string]string{PolicyLabel: "notify"}, false, nil, "policy: notify", "notify"},
		{"dry", nil, true, nil, "dry-run", "auto"},
		{"rm", nil, false, map[string]any{"AutoRemove": true}, "AutoRemove (--rm): manual update required", "auto"},
	}
	for _, tc := range cases {
		f := updatable(tc.labels)
		if tc.hostCf != nil {
			f.info["a"].HostConfig = tc.hostCf
		}
		_, s := scan(f, tc.dry)
		e := entry(t, s, "a")
		wantEntry(t, e, stats.ContainerUpdateAvailable, tc.detail)
		if e.Policy != tc.policy || e.ImageID != "1" {
			t.Fatalf("%s: %+v", tc.name, e)
		}
	}
}

func TestReportWaitingWindow(t *testing.T) {
	f := updatable(map[string]string{WindowLabel: "02:00-03:00"})
	u, _, s := newUpdater(f, false)
	clockAt(u, 12, 0)
	u.RunOnce(context.Background())
	e := entry(t, s, "a")
	wantEntry(t, e, stats.ContainerWaitingWindow, "02:00-03:00")
	if e.Window != "02:00-03:00" {
		t.Fatalf("%+v", e)
	}
}

func TestReportInvalidLabel(t *testing.T) {
	f := updatable(map[string]string{PolicyLabel: "sometimes"})
	_, s := scan(f, false)
	wantEntry(t, entry(t, s, "a"), stats.ContainerSkipped, "invalid label: ")
}

func TestReportUpdated(t *testing.T) {
	f := updatable(nil)
	f.containers[0].ImageID = "sha256:1111111111111111"
	f.images["nginx:latest"] = "sha256:2222222222222222"
	_, s := scan(f, false)
	e := entry(t, s, "a")
	wantEntry(t, e, stats.ContainerUpdated, "111111111111 -> 222222222222")
	if e.ImageID != "222222222222" {
		t.Fatalf("%+v", e)
	}
}

func TestReportUpdatedWithHealthNote(t *testing.T) {
	u, _, s, _ := func() (*Updater, *fakeDocker, *stats.Stats, *gateClock) {
		u, f, _, s, c := gateSetup(5*time.Second, running("starting"))
		return u, f, s, c
	}()
	u.RunOnce(context.Background())
	wantEntry(t, entry(t, s, "a"), stats.ContainerUpdated, "healthcheck still starting")
}

func TestReportGateRollbackAndNoRetry(t *testing.T) {
	u, _, _, s, _ := gateSetup(60*time.Second, running("unhealthy"))
	u.RunOnce(context.Background())
	wantEntry(t, entry(t, s, "a"), stats.ContainerRolledBack, "health gate failed: the healthcheck reports the container unhealthy")
	u.RunOnce(context.Background())
	wantEntry(t, entry(t, s, "a"), stats.ContainerRolledBack, "previous update failed the health gate, will not retry this image")
}

func TestReportOtherFailureIsFailedWithTheError(t *testing.T) {
	f := updatable(nil)
	f.failOn = map[string]error{"Inspect": errors.New("daemon exploded")}
	_, s := scan(f, false)
	wantEntry(t, entry(t, s, "a"), stats.ContainerFailed, "daemon exploded")
}

func TestReportIsReplacedAtEachScanAndPublishedWhenInterrupted(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{
		web("a", "aaa:1", "sha256:1", nil), web("b", "bbb:1", "sha256:1", nil)},
		images: map[string]string{"aaa:1": "sha256:1", "bbb:1": "sha256:1"}}
	u, _, s := newUpdater(f, false)
	ctx, cancel := context.WithCancel(context.Background())
	f.onPull = cancel // interrupted while pulling the first image
	u.RunOnce(ctx)
	got := s.Snapshot().Containers
	if len(got) != 0 {
		t.Fatalf("nothing is known about the unchecked containers: %+v", got)
	}

	f.onPull = nil
	f.containers = f.containers[:1]
	u.RunOnce(context.Background())
	if got := s.Snapshot().Containers; len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("list must be replaced by each scan: %+v", got)
	}
}

func TestReportKeepsKnownEntriesWhenInterrupted(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{
		web("a", "aaa:1", "sha256:1", map[string]string{OptOutLabel: "false"}), web("b", "bbb:1", "sha256:1", nil)},
		images: map[string]string{"bbb:1": "sha256:1"}}
	u, _, s := newUpdater(f, false)
	ctx, cancel := context.WithCancel(context.Background())
	f.onPull = cancel
	u.RunOnce(ctx)
	got := s.Snapshot().Containers
	if len(got) != 1 || got[0].Name != "a" || got[0].Status != stats.ContainerSkipped {
		t.Fatalf("%+v", got)
	}
}

func TestReportListFailureKeepsThePreviousList(t *testing.T) {
	f := updatable(nil)
	f.images["nginx:latest"] = "sha256:1"
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	f.failOn = map[string]error{"List": errors.New("down")}
	if err := u.RunOnce(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	if len(s.Snapshot().Containers) != 1 {
		t.Fatalf("%+v", s.Snapshot().Containers)
	}
}

func TestTruncKeepsAtMostNCharactersIncludingTheEllipsis(t *testing.T) {
	for _, s := range []string{strings.Repeat("x", 500), strings.Repeat("é", 500), strings.Repeat("x", 201), strings.Repeat("x", 200), "short"} {
		got := trunc(s, 200)
		if n := len([]rune(got)); n > 200 {
			t.Fatalf("%d runes for an input of %d", n, len([]rune(s)))
		}
		if len([]rune(s)) > 200 && !strings.HasSuffix(got, "…") {
			t.Fatalf("truncated text must end with an ellipsis: %q", got)
		}
		if len([]rune(s)) <= 200 && got != s {
			t.Fatalf("short text must be kept as is: %q", got)
		}
	}
}

func TestWindowIsTruncatedInTheContainerStatus(t *testing.T) {
	f := updatable(map[string]string{WindowLabel: strings.Repeat("9", 300)})
	_, s := scan(f, false)
	if n := len([]rune(entry(t, s, "a").Window)); n > 64 {
		t.Fatalf("window not truncated: %d", n)
	}
}

func TestFailedRollbackDetailKeepsTheEssentialInformationFirst(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"},
		failOn: map[string]error{"StartNew": errors.New(strings.Repeat("c", 500))}}
	u, _, s := newUpdater(f, false)
	u.docker = &renameBackFails{f}
	u.RunOnce(context.Background())
	e := entry(t, s, "a")
	if e.Status != stats.ContainerFailed || len([]rune(e.Detail)) > 200 {
		t.Fatalf("%d %+v", len([]rune(e.Detail)), e)
	}
	if !strings.HasPrefix(e.Detail, "rollback failed, the service may be DOWN; old container is named a_old") {
		t.Fatalf("the essential information must come first: %q", e.Detail)
	}
	// The event keeps the whole text.
	if ev := s.Snapshot().Events[0]; !strings.Contains(ev.Error, strings.Repeat("c", 500)) {
		t.Fatalf("the event must keep the full cause: %q", ev.Error)
	}
}
