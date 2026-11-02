package updater

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bilyboy785/docker-image-updater/internal/docker"
	"github.com/bilyboy785/docker-image-updater/internal/stats"
)

func TestRunImageUpdatesTheContainersOfThatImageOnly(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{
		web("a", "nginx:latest", "sha256:1", nil),
		web("b", "redis:7", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a"), "b": basicInfo("b")},
		images: map[string]string{"nginx:latest": "sha256:2", "redis:7": "sha256:2"}}
	u, _, s := newUpdater(f, false)
	if err := u.RunImage(context.Background(), "nginx"); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(f.calls, "|")
	if !strings.Contains(all, "pull nginx:latest") || strings.Contains(all, "redis") {
		t.Fatalf("only the requested image must be pulled: %v", f.calls)
	}
	if snap := s.Snapshot(); snap.Updates != 1 || len(snap.Events) != 1 || snap.Events[0].Container != "a" {
		t.Fatalf("%+v", snap)
	}
}

func TestRunImageMatchesEquivalentSpellings(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "registry.example.com/app:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"registry.example.com/app:latest": "sha256:2"}}
	u, _, s := newUpdater(f, false)
	u.RunImage(context.Background(), "Registry.Example.com/app")
	if s.Snapshot().Updates != 1 {
		t.Fatalf("%v %+v", f.calls, s.Snapshot())
	}
}

func TestRunImageIgnoresTheUpdateWindow(t *testing.T) {
	f := updatable(map[string]string{WindowLabel: "02:00-03:00"})
	u, _, s := newUpdater(f, false)
	clockAt(u, 12, 0)
	u.RunImage(context.Background(), "nginx:latest")
	if s.Snapshot().Updates != 1 || !hasMutation(f.calls) {
		t.Fatalf("a requested update must not wait for the window: %v %+v", f.calls, s.Snapshot())
	}
}

func TestRunImageIgnoresTheNotifyPolicy(t *testing.T) {
	f := updatable(map[string]string{PolicyLabel: "notify"})
	u, _, s := newUpdater(f, false)
	u.RunImage(context.Background(), "nginx:latest")
	if s.Snapshot().Updates != 1 || !hasMutation(f.calls) {
		t.Fatalf("%v %+v", f.calls, s.Snapshot())
	}
}

func TestScansStillRespectPolicyAndWindowAfterAForcedUpdate(t *testing.T) {
	f := updatable(map[string]string{WindowLabel: "02:00-03:00"})
	f.images["nginx:latest"] = "sha256:1" // nothing to update yet
	u, _, _ := newUpdater(f, false)
	clockAt(u, 12, 0)
	u.RunImage(context.Background(), "nginx:latest")
	f.images["nginx:latest"] = "sha256:2"
	f.calls = nil
	u.RunOnce(context.Background())
	if hasMutation(f.calls) {
		t.Fatalf("force must not leak into the next scan: %v", f.calls)
	}
}

func TestRunImageKeepsTheOptOutLabel(t *testing.T) {
	f := updatable(map[string]string{OptOutLabel: "false"})
	u, _, s := newUpdater(f, false)
	u.RunImage(context.Background(), "nginx:latest")
	if hasMutation(f.calls) || s.Snapshot().Updates != 0 {
		t.Fatalf("%v %+v", f.calls, s.Snapshot())
	}
}

func TestRunImageKeepsDryRun(t *testing.T) {
	f := updatable(nil)
	u, n, s := newUpdater(f, true)
	u.RunImage(context.Background(), "nginx:latest")
	if hasMutation(f.calls) || len(n.msgs) != 1 || s.Snapshot().Updates != 0 {
		t.Fatalf("%v %v %+v", f.calls, n.msgs, s.Snapshot())
	}
}

func TestRunImageKeepsInvalidLabelsFailClosed(t *testing.T) {
	f := updatable(map[string]string{PolicyLabel: "sometimes"})
	u, _, _ := newUpdater(f, false)
	u.RunImage(context.Background(), "nginx:latest")
	if hasMutation(f.calls) {
		t.Fatalf("%v", f.calls)
	}
}

func TestRunImageMergesIntoThePublishedList(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{
		web("a", "nginx:latest", "sha256:1", nil),
		web("b", "redis:7", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2", "redis:7": "sha256:1"}}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	u.RunImage(context.Background(), "nginx:latest")
	got := map[string]string{}
	for _, c := range s.Snapshot().Containers {
		got[c.Name] = c.Status
	}
	if len(got) != 2 || got["a"] != stats.ContainerUpdated || got["b"] != stats.ContainerUpToDate {
		t.Fatalf("%v", got)
	}
}

func TestRunImageWithoutMatchingContainerDoesNothing(t *testing.T) {
	f := updatable(nil)
	u, _, _ := newUpdater(f, false)
	if err := u.RunImage(context.Background(), "unknown:1"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("%v", f.calls)
	}
}

func TestContainersOfImageListsTheRunningContainersUsingIt(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{
		web("a", "nginx:latest", "sha256:1", nil),
		web("b", "docker.io/library/nginx", "sha256:1", map[string]string{OptOutLabel: "false"}),
		web("c", "redis:7", "sha256:1", nil)}}
	u, _, _ := newUpdater(f, false)
	got, err := u.ContainersOfImage(context.Background(), "docker.io/library/nginx:latest")
	if err != nil || strings.Join(got, ",") != "a,b" {
		t.Fatalf("%v %v", got, err)
	}
	if got, _ := u.ContainersOfImage(context.Background(), "docker.io/library/unknown:1"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestContainersOfImageResolvesABareImageID(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "0123456789ab", "sha256:1", nil)},
		info: map[string]*docker.ContainerInfo{"a": basicInfo("a")}}
	u, _, _ := newUpdater(f, false)
	got, err := u.ContainersOfImage(context.Background(), "docker.io/library/nginx:latest")
	if err != nil || strings.Join(got, ",") != "a" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestContainersOfImageReportsADaemonError(t *testing.T) {
	f := &fakeDocker{failOn: map[string]error{"List": errors.New("daemon down")}}
	u, _, _ := newUpdater(f, false)
	if _, err := u.ContainersOfImage(context.Background(), "docker.io/library/nginx:latest"); err == nil {
		t.Fatal("expected an error")
	}
}
