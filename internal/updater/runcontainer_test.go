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

// twoContainers returns a daemon running "a" (nginx) and "b" (redis), both with a newer image
// available, plus a bare-id container "c" whose resolution would need an inspect call.
func twoContainers() *fakeDocker {
	return &fakeDocker{
		containers: []docker.Container{
			web("a", "nginx:latest", "sha256:1", nil),
			web("b", "redis:latest", "sha256:1", nil),
			web("c", "0123456789ab", "sha256:1", nil),
		},
		info: map[string]*docker.ContainerInfo{"a": basicInfo("a"), "b": basicInfo("b"),
			"c": {Config: map[string]any{"Image": "busybox:latest"}}},
		images: map[string]string{"nginx:latest": "sha256:2", "redis:latest": "sha256:2", "busybox:latest": "sha256:2"},
	}
}

func TestRunContainerOnlyTouchesTheNamedContainer(t *testing.T) {
	f := twoContainers()
	u, n, s := newUpdater(f, false)
	if err := u.RunContainer(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	want := "pull nginx:latest|rename a a_old|stop a|create a|start newid|remove a"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if f.localChecks != 1 {
		t.Fatalf("only the target image origin may be checked, got %d checks", f.localChecks)
	}
	snap := s.Snapshot()
	if snap.Updates != 1 || len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "<b>a</b>") {
		t.Fatalf("%+v %v", snap, n.msgs)
	}
	wantEntry(t, entry(t, s, "a"), stats.ContainerUpdated, "")
}

func TestRunContainerMergesIntoThePublishedListAndKeepsTheOthers(t *testing.T) {
	f := twoContainers()
	u, _, s := newUpdater(f, false)
	old := time.Unix(1000, 0)
	s.SetContainers([]stats.ContainerStatus{
		{Name: "c", Status: stats.ContainerSkipped, Detail: "keep me", CheckedAt: old},
		{Name: "a", Status: stats.ContainerUpToDate, CheckedAt: old},
		{Name: "b", Status: stats.ContainerUpToDate, Detail: "keep me too", CheckedAt: old},
		{Name: "gone", Status: stats.ContainerUpToDate, CheckedAt: old},
	})
	now := time.Unix(5000, 0)
	u.now = func() time.Time { return now }
	if err := u.RunContainer(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	got := s.Snapshot().Containers
	names := []string{}
	for _, c := range got {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "a,b,c,gone" {
		t.Fatalf("list must stay sorted and complete: %v", names)
	}
	for _, c := range got {
		switch c.Name {
		case "b":
			if c.Status != stats.ContainerUpdated || !c.CheckedAt.Equal(now) {
				t.Fatalf("b: %+v", c)
			}
		default:
			if !c.CheckedAt.Equal(old) || c.Status == stats.ContainerUpdated {
				t.Fatalf("%s must be untouched: %+v", c.Name, c)
			}
		}
	}
	if got[2].Detail != "keep me" {
		t.Fatalf("%+v", got[2])
	}
}

func TestRunContainerNotFound(t *testing.T) {
	f := twoContainers()
	u, n, s := newUpdater(f, false)
	s.SetContainers([]stats.ContainerStatus{{Name: "a", Status: stats.ContainerUpToDate}})
	before := s.Snapshot()
	err := u.RunContainer(context.Background(), "nope")
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("got %v", err)
	}
	if len(f.calls) != 0 || len(n.msgs) != 0 {
		t.Fatalf("%v %v", f.calls, n.msgs)
	}
	after := s.Snapshot()
	if len(after.Containers) != 1 || after.Containers[0].Name != "a" || after.Scans != before.Scans {
		t.Fatalf("nothing must be published: %+v", after)
	}
}

func TestRunContainerListFailure(t *testing.T) {
	f := twoContainers()
	f.failOn = map[string]error{"List": errors.New("boom")}
	u, _, s := newUpdater(f, false)
	err := u.RunContainer(context.Background(), "a")
	if err == nil || errors.Is(err, ErrContainerNotFound) || s.Snapshot().Scans != 0 {
		t.Fatalf("%v %+v", err, s.Snapshot())
	}
}

func TestRunContainerSelfOnlyAnnounces(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{
		web("selfself0000aaaa", "ghcr.io/x/updater:latest", "sha256:1", nil),
		web("b", "redis:latest", "sha256:1", nil)},
		images: map[string]string{"ghcr.io/x/updater:latest": "sha256:2", "redis:latest": "sha256:2"}}
	u, n, s := newUpdater(f, false)
	if err := u.RunContainer(context.Background(), "selfself0000aaaa"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls, "|"); got != "pull ghcr.io/x/updater:latest" {
		t.Fatal(got)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "does not update itself") {
		t.Fatalf("%v", n.msgs)
	}
	wantEntry(t, entry(t, s, "selfself0000aaaa"), stats.ContainerUpdateAvailable, "manual update required")
}

func TestRunContainerNotifyPolicy(t *testing.T) {
	f := updatable(map[string]string{PolicyLabel: "notify"})
	u, n, s := newUpdater(f, false)
	if err := u.RunContainer(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if hasMutation(f.calls) || len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "(policy: notify)") {
		t.Fatalf("%v %v", f.calls, n.msgs)
	}
	wantEntry(t, entry(t, s, "a"), stats.ContainerUpdateAvailable, "policy: notify")
	// the announcement is shared with the full scan: not repeated
	u.RunOnce(context.Background())
	if len(n.msgs) != 1 {
		t.Fatalf("%v", n.msgs)
	}
}

func TestRunContainerOutsideItsWindowIsNotUpdated(t *testing.T) {
	f := updatable(map[string]string{WindowLabel: "02:00-03:00"})
	u, n, s := newUpdater(f, false)
	clockAt(u, 12, 0)
	if err := u.RunContainer(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if hasMutation(f.calls) || len(n.msgs) != 0 {
		t.Fatalf("%v %v", f.calls, n.msgs)
	}
	wantEntry(t, entry(t, s, "a"), stats.ContainerWaitingWindow, "outside the update window")
}

func TestRunContainerHealthGateRollback(t *testing.T) {
	u, f, n, s, _ := gateSetup(60*time.Second, running("unhealthy"))
	if err := u.RunContainer(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	want := beforeGate + "|inspect newid|remove newid|rename a a|start a"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if snap := s.Snapshot(); snap.Rollbacks != 1 || len(n.msgs) != 1 {
		t.Fatalf("%+v %v", snap, n.msgs)
	}
	wantEntry(t, entry(t, s, "a"), stats.ContainerRolledBack, "health gate failed")
}

func TestBadImageMemoryIsSharedBetweenTargetedAndFullScans(t *testing.T) {
	u, f, _, s, _ := gateSetup(60*time.Second, running("unhealthy"))
	u.RunContainer(context.Background(), "a")
	f.calls = nil
	u.RunOnce(context.Background())
	if hasMutation(f.calls) {
		t.Fatalf("full scan must not retry the bad image: %v", f.calls)
	}
	wantEntry(t, entry(t, s, "a"), stats.ContainerRolledBack, "will not retry")

	u2, f2, _, _, _ := gateSetup(60*time.Second, running("unhealthy"))
	u2.RunOnce(context.Background())
	f2.calls = nil
	u2.RunContainer(context.Background(), "a")
	if hasMutation(f2.calls) {
		t.Fatalf("targeted scan must not retry the bad image: %v", f2.calls)
	}
}

func TestRunContainerKeepsTheGaugesAndCountsTheScan(t *testing.T) {
	f := twoContainers()
	u, _, s := newUpdater(f, true)
	u.RunOnce(context.Background())
	before := s.Snapshot()
	if err := u.RunContainer(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	after := s.Snapshot()
	if after.Scans != before.Scans+1 || after.Watched != before.Watched || after.Skipped != before.Skipped {
		t.Fatalf("before %+v after %+v", before, after)
	}
}

func TestRunContainerSkippedKinds(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{
		web("optout", "nginx:latest", "sha256:1", map[string]string{OptOutLabel: "false"}),
		web("pinned", "nginx@sha256:abc", "sha256:1", nil),
		web("bare", "sha256:0123456789abcdef", "sha256:1", nil),
		web("local", "mine:1", "sha256:7", nil)},
		info:        map[string]*docker.ContainerInfo{"bare": {Config: map[string]any{"Image": "sha256:0123456789abcdef"}}},
		localImages: map[string]bool{"sha256:7": true}}
	u, _, s := newUpdater(f, false)
	for _, name := range []string{"optout", "pinned", "bare", "local"} {
		if err := u.RunContainer(context.Background(), name); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("nothing should be pulled: %v", f.calls)
	}
	wantEntry(t, entry(t, s, "optout"), stats.ContainerSkipped, "opted out")
	wantEntry(t, entry(t, s, "pinned"), stats.ContainerSkipped, "pinned by digest")
	wantEntry(t, entry(t, s, "bare"), stats.ContainerSkipped, "image reference cannot be resolved")
	wantEntry(t, entry(t, s, "local"), stats.ContainerSkipped, "locally built image")
	if s.Snapshot().Watched != 0 || s.Snapshot().Skipped != 0 {
		t.Fatalf("gauges untouched: %+v", s.Snapshot())
	}
}

func TestRunContainerSharedImageStillOnlyUpdatesTheTarget(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{
		web("a", "nginx:latest", "sha256:1", nil), web("b", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a"), "b": basicInfo("b")},
		images: map[string]string{"nginx:latest": "sha256:2"}}
	u, _, s := newUpdater(f, false)
	u.RunContainer(context.Background(), "a")
	if indexOf(f.calls, "rename b b_old") != -1 || indexOf(f.calls, "rename a a_old") == -1 {
		t.Fatalf("%v", f.calls)
	}
	for _, c := range s.Snapshot().Containers {
		if c.Name == "b" {
			t.Fatalf("b must not be listed: %+v", c)
		}
	}
}
