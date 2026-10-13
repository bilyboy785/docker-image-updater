package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/bilyboy785/docker-image-updater/internal/docker"
	"github.com/bilyboy785/docker-image-updater/internal/stats"
)

type fakeDocker struct {
	containers []docker.Container
	info       map[string]*docker.ContainerInfo
	images     map[string]string // ref -> image id after pull
	pullErr    map[string]error
	failOn     map[string]error // method name -> error
	calls      []string
}

func (f *fakeDocker) rec(s string)        { f.calls = append(f.calls, s) }
func (f *fakeDocker) fail(m string) error { return f.failOn[m] }

func (f *fakeDocker) ListContainers(context.Context) ([]docker.Container, error) {
	return f.containers, f.fail("List")
}
func (f *fakeDocker) InspectContainer(_ context.Context, id string) (*docker.ContainerInfo, error) {
	return f.info[id], f.fail("Inspect")
}
func (f *fakeDocker) PullImage(_ context.Context, ref string) error {
	f.rec("pull " + ref)
	return f.pullErr[ref]
}
func (f *fakeDocker) ImageID(_ context.Context, ref string) (string, error) {
	return f.images[ref], nil
}
func (f *fakeDocker) CreateContainer(_ context.Context, name string, _ map[string]any) (string, error) {
	f.rec("create " + name)
	return "newid", f.fail("Create")
}
func (f *fakeDocker) StartContainer(_ context.Context, id string) error {
	f.rec("start " + id)
	if id == "newid" {
		return f.fail("StartNew")
	}
	return nil
}
func (f *fakeDocker) StopContainer(_ context.Context, id string) error {
	f.rec("stop " + id)
	return f.fail("Stop")
}
func (f *fakeDocker) RemoveContainer(_ context.Context, id string, force bool) error {
	f.rec("remove " + id)
	return nil
}
func (f *fakeDocker) RenameContainer(_ context.Context, id, name string) error {
	f.rec(fmt.Sprintf("rename %s %s", id, name))
	return nil
}
func (f *fakeDocker) ConnectNetwork(_ context.Context, network, id string, _ map[string]any) error {
	f.rec("connect " + network)
	return nil
}

type fakeNotifier struct{ msgs []string }

func (n *fakeNotifier) Notify(_ context.Context, text string) error {
	n.msgs = append(n.msgs, text)
	return nil
}

func newUpdater(f *fakeDocker, dry bool) (*Updater, *fakeNotifier, *stats.Stats) {
	n := &fakeNotifier{}
	s, _ := stats.New("test", "active", "")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(f, n, s, "selfself0000", dry, log), n, s
}

func web(id, image, imageID string, labels map[string]string) docker.Container {
	return docker.Container{ID: id, Names: []string{"/" + id}, Image: image, ImageID: imageID, Labels: labels}
}

func basicInfo(id string) *docker.ContainerInfo {
	info := &docker.ContainerInfo{ID: id + "0000000000", Name: "/" + id,
		Config:     map[string]any{"Image": "nginx:latest"},
		HostConfig: map[string]any{"NetworkMode": "bridge"}}
	info.NetworkSettings.Networks = map[string]map[string]any{"bridge": {}}
	return info
}

func TestUpToDateContainerIsUntouched(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		images: map[string]string{"nginx:latest": "sha256:1"}}
	u, n, s := newUpdater(f, false)
	if err := u.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.msgs) != 0 || s.Snapshot().Updates != 0 || s.Snapshot().Scans != 1 || s.Snapshot().Watched != 1 {
		t.Fatalf("%v %+v", n.msgs, s.Snapshot())
	}
}

func TestSuccessfulUpdateSequence(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"}}
	u, n, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	want := "pull nginx:latest|rename a a_old|stop a|create a|start newid|remove a"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	snap := s.Snapshot()
	if snap.Updates != 1 || snap.Events[0].Status != stats.StatusUpdated || snap.Events[0].NewID != "2" {
		t.Fatalf("%+v", snap)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "nginx:latest") {
		t.Fatalf("%v", n.msgs)
	}
}

func TestOptOutAndDigestAndBareIDAreSkipped(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{
		web("a", "nginx:latest", "sha256:1", map[string]string{OptOutLabel: "false"}),
		web("b", "nginx@sha256:abcdef", "sha256:1", nil),
		web("c", "0123456789ab", "sha256:1", nil),
		web("d", "sha256:0123456789abcdef", "sha256:1", nil),
	}, images: map[string]string{}}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	if len(f.calls) != 0 {
		t.Fatalf("nothing should be pulled: %v", f.calls)
	}
	if snap := s.Snapshot(); snap.Skipped != 4 || snap.Watched != 0 {
		t.Fatalf("%+v", snap)
	}
}

func TestImagePulledOnceForSeveralContainers(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{
		web("a", "nginx:latest", "sha256:1", nil), web("b", "nginx:latest", "sha256:1", nil)},
		images: map[string]string{"nginx:latest": "sha256:1"}}
	u, _, _ := newUpdater(f, false)
	u.RunOnce(context.Background())
	if len(f.calls) != 1 {
		t.Fatalf("%v", f.calls)
	}
}

func TestSelfIsNotRecreatedOnlyNotifiedOnce(t *testing.T) {
	self := web("selfself0000aaaa", "ghcr.io/x/updater:latest", "sha256:1", nil)
	f := &fakeDocker{containers: []docker.Container{self}, images: map[string]string{"ghcr.io/x/updater:latest": "sha256:2"}}
	u, n, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	u.RunOnce(context.Background())
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "pull") {
			t.Fatalf("self must never be touched: %v", f.calls)
		}
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "manual") {
		t.Fatalf("expected exactly one manual-update notification, got %v", n.msgs)
	}
	if s.Snapshot().Events[0].Status != stats.StatusAvailable {
		t.Fatalf("%+v", s.Snapshot().Events)
	}
}

func TestSelfDetectedByLabel(t *testing.T) {
	c := web("other", "ghcr.io/x/updater:latest", "sha256:1", map[string]string{SelfLabel: "true"})
	f := &fakeDocker{containers: []docker.Container{c}, images: map[string]string{"ghcr.io/x/updater:latest": "sha256:2"}}
	u, _, _ := newUpdater(f, false)
	u.RunOnce(context.Background())
	if len(f.calls) != 1 {
		t.Fatalf("%v", f.calls)
	}
}

func TestDryRunNotifiesWithoutRecreating(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		images: map[string]string{"nginx:latest": "sha256:2"}}
	u, n, _ := newUpdater(f, true)
	u.RunOnce(context.Background())
	u.RunOnce(context.Background())
	if len(f.calls) != 2 || len(n.msgs) != 1 {
		t.Fatalf("calls=%v msgs=%v", f.calls, n.msgs)
	}
}

func TestPullFailureDoesNotStopOtherImages(t *testing.T) {
	f := &fakeDocker{
		containers: []docker.Container{web("a", "local/only:1", "sha256:1", nil), web("b", "nginx:latest", "sha256:1", nil)},
		info:       map[string]*docker.ContainerInfo{"b": basicInfo("b")},
		images:     map[string]string{"nginx:latest": "sha256:2"},
		pullErr:    map[string]error{"local/only:1": errors.New("pull access denied")},
	}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	if s.Snapshot().Updates != 1 {
		t.Fatalf("second image must still be updated: %v", f.calls)
	}
}

func TestStartFailureRollsBack(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"},
		failOn: map[string]error{"StartNew": errors.New("port already allocated")}}
	u, n, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	want := "pull nginx:latest|rename a a_old|stop a|create a|start newid|remove newid|rename a a|start a"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	snap := s.Snapshot()
	if snap.Rollbacks != 1 || snap.Events[0].Status != stats.StatusRolledBack || !strings.Contains(snap.Events[0].Error, "port already allocated") {
		t.Fatalf("%+v", snap)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "rolled back") {
		t.Fatalf("%v", n.msgs)
	}
}

func TestCreateFailureRollsBackWithoutRemovingNew(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"},
		failOn: map[string]error{"Create": errors.New("invalid config")}}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	want := "pull nginx:latest|rename a a_old|stop a|create a|rename a a|start a"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if s.Snapshot().Rollbacks != 1 {
		t.Fatalf("%+v", s.Snapshot())
	}
}

func TestStopFailureRestoresName(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"},
		failOn: map[string]error{"Stop": errors.New("timeout")}}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	want := "pull nginx:latest|rename a a_old|stop a|rename a a"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if s.Snapshot().Failures != 1 {
		t.Fatalf("%+v", s.Snapshot())
	}
}

func TestRollbackStillRunsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"},
		failOn: map[string]error{"StartNew": errors.New("boom")}}
	u, _, _ := newUpdater(f, false)
	u.docker = &cancelOnStart{fakeDocker: f, cancel: cancel}
	u.RunOnce(ctx)
	if !strings.HasSuffix(strings.Join(f.calls, "|"), "rename a a|start a") {
		t.Fatalf("rollback must complete despite cancellation: %v", f.calls)
	}
}

type cancelOnStart struct {
	*fakeDocker
	cancel func()
}

func (c *cancelOnStart) StartContainer(ctx context.Context, id string) error {
	if id == "newid" {
		c.cancel()
	}
	return c.fakeDocker.StartContainer(ctx, id)
}

func TestMessagesAreHTMLEscaped(t *testing.T) {
	c := web("a<b>&", "nginx:latest", "sha256:1", nil)
	f := &fakeDocker{containers: []docker.Container{c}, images: map[string]string{"nginx:latest": "sha256:2"}}
	u, n, _ := newUpdater(f, true)
	u.RunOnce(context.Background())
	if strings.Contains(n.msgs[0], "<b>&") || !strings.Contains(n.msgs[0], "a&lt;b&gt;&amp;") {
		t.Fatalf("%s", n.msgs[0])
	}
}

func TestListFailureIsReturned(t *testing.T) {
	f := &fakeDocker{failOn: map[string]error{"List": errors.New("socket gone")}}
	u, _, _ := newUpdater(f, false)
	if err := u.RunOnce(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}
