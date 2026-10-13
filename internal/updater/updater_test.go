package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

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
	pullAuth   map[string]string // ref -> registry auth passed to PullImage
	onPull     func()            // optional hook, runs inside PullImage
	onStop     func()            // optional hook, runs inside StopContainer

	imageCfg         map[string]any // returned by ImageConfig
	imageCfgErr      error
	oldGoneAfterPull bool // ImageConfig fails once PullImage ran, like the containerd store dropping a replaced image
	pulled           bool
	localChecks      int
	localErr         map[string]error                           // image id -> error from ImageIsLocal
	localImages      map[string]bool                            // image ids that never came from a registry
	created          []map[string]any                           // bodies passed to CreateContainer
	inspectNew       func(n int) (*docker.ContainerInfo, error) // optional: inspect results of the new container (n counts from 1)
	inspectNewCalls  int
	rmiErr           error // returned by RemoveImage
}

func (f *fakeDocker) rec(s string)        { f.calls = append(f.calls, s) }
func (f *fakeDocker) fail(m string) error { return f.failOn[m] }

// Every method honours an already-cancelled context, like the real client does.

func (f *fakeDocker) ListContainers(ctx context.Context) ([]docker.Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.containers, f.fail("List")
}
func (f *fakeDocker) InspectContainer(ctx context.Context, id string) (*docker.ContainerInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id == "newid" && f.inspectNew != nil {
		f.inspectNewCalls++
		f.rec("inspect newid")
		return f.inspectNew(f.inspectNewCalls)
	}
	return f.info[id], f.fail("Inspect")
}
func (f *fakeDocker) PullImage(ctx context.Context, ref, registryAuth string) error {
	f.rec("pull " + ref)
	f.pulled = true
	if f.pullAuth == nil {
		f.pullAuth = map[string]string{}
	}
	f.pullAuth[ref] = registryAuth
	if f.onPull != nil {
		f.onPull()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.pullErr[ref]
}
func (f *fakeDocker) ImageID(ctx context.Context, ref string) (string, error) {
	return f.images[ref], ctx.Err()
}
func (f *fakeDocker) ImageConfig(ctx context.Context, ref string) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.oldGoneAfterPull && f.pulled {
		return nil, &docker.APIError{Status: 404, Message: "no such image"}
	}
	return f.imageCfg, f.imageCfgErr
}
func (f *fakeDocker) ImageIsLocal(ctx context.Context, id string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.localChecks++
	if err := f.localErr[id]; err != nil {
		return false, err
	}
	return f.localImages[id], nil
}
func (f *fakeDocker) CreateContainer(ctx context.Context, name string, body map[string]any) (string, error) {
	f.rec("create " + name)
	f.created = append(f.created, body)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "newid", f.fail("Create")
}
func (f *fakeDocker) StartContainer(ctx context.Context, id string) error {
	f.rec("start " + id)
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "newid" {
		return f.fail("StartNew")
	}
	return nil
}
func (f *fakeDocker) StopContainer(ctx context.Context, id string) error {
	f.rec("stop " + id)
	if f.onStop != nil {
		f.onStop()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.fail("Stop")
}
func (f *fakeDocker) RemoveContainer(ctx context.Context, id string, force bool) error {
	f.rec("remove " + id)
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "a" {
		return f.fail("RemoveOld")
	}
	return nil
}
func (f *fakeDocker) RenameContainer(ctx context.Context, id, name string) error {
	f.rec(fmt.Sprintf("rename %s %s", id, name))
	return ctx.Err()
}
func (f *fakeDocker) ConnectNetwork(ctx context.Context, network, id string, _ map[string]any) error {
	f.rec("connect " + network)
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.fail("Connect")
}

func (f *fakeDocker) RemoveImage(ctx context.Context, id string) error {
	f.rec("rmi " + id)
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.rmiErr
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

func TestStopFailureRestoresNameAndRestarts(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"},
		failOn: map[string]error{"Stop": errors.New("timeout")}}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	want := "pull nginx:latest|rename a a_old|stop a|rename a a|start a"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if snap := s.Snapshot(); snap.Rollbacks != 1 || snap.Events[0].Status != stats.StatusRolledBack {
		t.Fatalf("%+v", snap)
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

const fullSequence = "pull nginx:latest|rename a a_old|stop a|create a|start newid|remove a"

// noCtxCheckPull lets inspect, pull and image-id lookup succeed even with a cancelled context, so
// the scenario "ctx cancelled right as the cycle reaches recreate" can be exercised.
type noCtxCheckPull struct{ *fakeDocker }

func (c *noCtxCheckPull) PullImage(_ context.Context, ref, auth string) error {
	return c.fakeDocker.PullImage(context.Background(), ref, auth)
}
func (c *noCtxCheckPull) ImageID(_ context.Context, ref string) (string, error) {
	return c.fakeDocker.ImageID(context.Background(), ref)
}
func (c *noCtxCheckPull) InspectContainer(_ context.Context, id string) (*docker.ContainerInfo, error) {
	return c.fakeDocker.InspectContainer(context.Background(), id)
}

// cancelOnInspect cancels the caller context when the container is inspected, right before recreate.
type cancelOnInspect struct {
	*noCtxCheckPull
	cancel func()
}

func (c *cancelOnInspect) InspectContainer(ctx context.Context, id string) (*docker.ContainerInfo, error) {
	c.cancel()
	return c.noCtxCheckPull.InspectContainer(ctx, id)
}

func TestCancelledContextBeforeRecreateStillCompletesUpdate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"}}
	u, n, s := newUpdater(f, false)
	u.docker = &cancelOnInspect{&noCtxCheckPull{f}, cancel} // the cycle reaches recreate with a dead caller context
	u.RunOnce(ctx)
	if got := strings.Join(f.calls, "|"); got != fullSequence {
		t.Fatalf("got  %s\nwant %s", got, fullSequence)
	}
	if s.Snapshot().Updates != 1 || len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "Updated") {
		t.Fatalf("%+v %v", s.Snapshot(), n.msgs)
	}
}

func TestCancelDuringStopStillCompletesUpdate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"}}
	f.onStop = cancel
	u, n, s := newUpdater(f, false)
	u.RunOnce(ctx)
	if got := strings.Join(f.calls, "|"); got != fullSequence {
		t.Fatalf("got  %s\nwant %s", got, fullSequence)
	}
	if s.Snapshot().Updates != 1 || len(n.msgs) != 1 {
		t.Fatalf("%+v %v", s.Snapshot(), n.msgs)
	}
}

func TestOldContainerRemovalFailureIsReported(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"},
		failOn: map[string]error{"RemoveOld": errors.New("busy")}}
	u, n, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	if s.Snapshot().Events[0].Status != stats.StatusUpdated || len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "a_old") {
		t.Fatalf("%+v %v", s.Snapshot(), n.msgs)
	}
}

func TestBareImageIDIsResolvedFromContainerConfig(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "sha256:0123456789abcdef", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"}}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	if snap := s.Snapshot(); snap.Updates != 1 || snap.Watched != 1 || snap.Events[0].Image != "nginx:latest" {
		t.Fatalf("%+v %v", s.Snapshot(), f.calls)
	}
}

func TestBareImageIDWithBareConfigImageIsSkipped(t *testing.T) {
	info := basicInfo("a")
	info.Config["Image"] = "sha256:0123456789abcdef"
	f := &fakeDocker{containers: []docker.Container{web("a", "0123456789ab", "sha256:1", nil)},
		info: map[string]*docker.ContainerInfo{"a": info}}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	if snap := s.Snapshot(); snap.Skipped != 1 || snap.Watched != 0 || len(f.calls) != 0 {
		t.Fatalf("%+v %v", snap, f.calls)
	}
}

func TestConnectNetworkFailureRollsBack(t *testing.T) {
	info := basicInfo("a")
	info.NetworkSettings.Networks = map[string]map[string]any{"bridge": {}, "backend": {}}
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": info},
		images: map[string]string{"nginx:latest": "sha256:2"},
		failOn: map[string]error{"Connect": errors.New("no such network")}}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	got := strings.Join(f.calls, "|")
	if !strings.Contains(got, "create a|connect ") || !strings.HasSuffix(got, "|remove newid|rename a a|start a") {
		t.Fatalf("got %s", got)
	}
	if snap := s.Snapshot(); snap.Rollbacks != 1 || snap.Events[0].Status != stats.StatusRolledBack {
		t.Fatalf("%+v", snap)
	}
}

func TestCreateBodyIsStrippedOfOldImageDefaults(t *testing.T) {
	info := basicInfo("a")
	info.Config["Env"] = []any{"PG_MAJOR=16", "CUSTOM=yes"}
	info.Config["Cmd"] = []any{"old"}
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:     map[string]*docker.ContainerInfo{"a": info},
		images:   map[string]string{"nginx:latest": "sha256:2"},
		imageCfg: map[string]any{"Env": []any{"PG_MAJOR=16"}, "Cmd": []any{"old"}}}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	if len(f.created) != 1 || s.Snapshot().Updates != 1 {
		t.Fatalf("%v %+v", f.created, s.Snapshot())
	}
	body := f.created[0]
	if !reflect.DeepEqual(body["Env"], []any{"CUSTOM=yes"}) {
		t.Errorf("env %v", body["Env"])
	}
	if _, ok := body["Cmd"]; ok {
		t.Errorf("cmd must come from the new image: %v", body["Cmd"])
	}
}

func TestOldImageConfigIsReadBeforeThePullReplacesIt(t *testing.T) {
	info := basicInfo("a")
	info.Config["Env"] = []any{"PG_MAJOR=16", "CUSTOM=yes"}
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:             map[string]*docker.ContainerInfo{"a": info},
		images:           map[string]string{"nginx:latest": "sha256:2"},
		imageCfg:         map[string]any{"Env": []any{"PG_MAJOR=16"}},
		oldGoneAfterPull: true}
	u, _, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	if s.Snapshot().Updates != 1 || !reflect.DeepEqual(f.created[0]["Env"], []any{"CUSTOM=yes"}) {
		t.Fatalf("old defaults must still be stripped: %v", f.created)
	}
}

func TestImageConfigErrorStillUpdates(t *testing.T) {
	info := basicInfo("a")
	info.Config["Env"] = []any{"PG_MAJOR=16"}
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:        map[string]*docker.ContainerInfo{"a": info},
		images:      map[string]string{"nginx:latest": "sha256:2"},
		imageCfgErr: &docker.APIError{Status: 404, Message: "no such image"}}
	u, n, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	if snap := s.Snapshot(); snap.Updates != 1 || snap.Failures != 0 || len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "Updated") {
		t.Fatalf("%+v %v", snap, n.msgs)
	}
	if !reflect.DeepEqual(f.created[0]["Env"], []any{"PG_MAJOR=16"}) {
		t.Errorf("body must be left unstripped: %v", f.created[0])
	}
}

func autoRemoveInfo(id string) *docker.ContainerInfo {
	info := basicInfo(id)
	info.HostConfig["AutoRemove"] = true
	return info
}

func TestAutoRemoveContainerIsNeverTouchedAndNotifiedOnce(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": autoRemoveInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"}}
	u, n, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	u.RunOnce(context.Background())
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "pull") {
			t.Fatalf("an AutoRemove container must never be touched: %v", f.calls)
		}
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "--rm") || !strings.Contains(n.msgs[0], "manual") {
		t.Fatalf("%v", n.msgs)
	}
	snap := s.Snapshot()
	if len(snap.Events) != 1 || snap.Events[0].Status != stats.StatusAvailable || snap.Updates != 0 {
		t.Fatalf("%+v", snap)
	}
}

func TestPullFailureIsReportedOnce(t *testing.T) {
	f := &fakeDocker{
		containers: []docker.Container{web("a<", "priv/img:1", "sha256:1", nil), web("b", "priv/img:1", "sha256:1", nil)},
		pullErr:    map[string]error{"priv/img:1": errors.New("pull access denied <x>")},
	}
	u, n, s := newUpdater(f, false)
	u.RunOnce(context.Background())
	u.RunOnce(context.Background())
	snap := s.Snapshot()
	if len(snap.Events) != 1 || snap.Failures != 1 || snap.Events[0].Status != stats.StatusFailed {
		t.Fatalf("%+v", snap)
	}
	if ev := snap.Events[0]; ev.Container != "a<, b" || ev.Image != "priv/img:1" || !strings.Contains(ev.Error, "pull access denied") {
		t.Fatalf("%+v", ev)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "a&lt;, b") || !strings.Contains(n.msgs[0], "&lt;x&gt;") {
		t.Fatalf("%v", n.msgs)
	}
}

func TestImageIDFailureIsReportedOnce(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)}}
	u, n, s := newUpdater(f, false)
	u.docker = &imageIDFails{f}
	u.RunOnce(context.Background())
	u.RunOnce(context.Background())
	if snap := s.Snapshot(); len(snap.Events) != 1 || snap.Failures != 1 || len(n.msgs) != 1 {
		t.Fatalf("%+v %v", snap, n.msgs)
	}
}

type imageIDFails struct{ *fakeDocker }

func (imageIDFails) ImageID(context.Context, string) (string, error) {
	return "", errors.New("no such image")
}

func TestDependantsOfNetworkModeContainerAreNamed(t *testing.T) {
	vpn := web("vpn", "nginx:latest", "sha256:1", nil)
	vpn.ID = "vpnfullid"
	dep := web("dep<", "other:1", "sha256:9", nil)
	dep.NetworkMode = "container:vpnfullid"
	byName := web("dep2", "other:1", "sha256:9", nil)
	byName.NetworkMode = "container:vpn"
	unrelated := web("x", "other:1", "sha256:9", nil)
	unrelated.NetworkMode = "container:else"
	f := &fakeDocker{containers: []docker.Container{vpn, dep, byName, unrelated},
		info:   map[string]*docker.ContainerInfo{"vpnfullid": basicInfo("vpn")},
		images: map[string]string{"nginx:latest": "sha256:2", "other:1": "sha256:9"}}
	u, n, _ := newUpdater(f, false)
	u.RunOnce(context.Background())
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "dep&lt;, dep2") || strings.Contains(n.msgs[0], "<b>x</b>") ||
		!strings.Contains(n.msgs[0], "manually") {
		t.Fatalf("%v", n.msgs)
	}
}

func TestCancelledContextBetweenPullAndInspectDoesNotReportFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"}}
	f.onPull = cancel
	u, n, s := newUpdater(f, false)
	u.docker = &noCtxCheckPull{f}
	u.RunOnce(ctx)
	if len(n.msgs) != 0 || len(s.Snapshot().Events) != 0 || strings.Contains(strings.Join(f.calls, "|"), "rename") {
		t.Fatalf("%v %+v %v", n.msgs, s.Snapshot(), f.calls)
	}
}

func TestRollbackFailureMentionsServiceDown(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
		info:   map[string]*docker.ContainerInfo{"a": basicInfo("a")},
		images: map[string]string{"nginx:latest": "sha256:2"},
		failOn: map[string]error{"StartNew": errors.New("boom")}}
	u, _, s := newUpdater(f, false)
	u.docker = &renameBackFails{f}
	u.RunOnce(context.Background())
	ev := s.Snapshot().Events[0]
	if ev.Status != stats.StatusFailed || !strings.Contains(ev.Error, "DOWN") || !strings.Contains(ev.Error, "a_old") {
		t.Fatalf("%+v", ev)
	}
}

type renameBackFails struct{ *fakeDocker }

func (r *renameBackFails) RenameContainer(ctx context.Context, id, name string) error {
	if name == "a" {
		return errors.New("rename refused")
	}
	return r.fakeDocker.RenameContainer(ctx, id, name)
}

func TestSelfPrefixMatchNeedsAtLeast12Chars(t *testing.T) {
	u, _, _ := newUpdater(&fakeDocker{}, false)
	u.selfID = "abc"
	if u.isSelf(web("abcdef123456", "i", "x", nil)) {
		t.Error("a short hostname must not match by prefix")
	}
	u.selfID = "abcdef123456"
	if !u.isSelf(web("abcdef1234567890", "i", "x", nil)) {
		t.Error("a 12-char id must match by prefix")
	}
}

// blockingDocker blocks selected calls until their context is done, like a hung daemon.
type blockingDocker struct {
	*fakeDocker
	list, pull bool
}

func (b *blockingDocker) ListContainers(ctx context.Context) ([]docker.Container, error) {
	if b.list {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return b.fakeDocker.ListContainers(ctx)
}

func (b *blockingDocker) PullImage(ctx context.Context, ref, auth string) error {
	if b.pull {
		<-ctx.Done()
		return ctx.Err()
	}
	return b.fakeDocker.PullImage(ctx, ref, auth)
}

func runWithDeadline(t *testing.T, u *Updater) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- u.RunOnce(context.Background()) }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnce hung on a blocked Docker call")
		return nil
	}
}

func TestStuckListDoesNotHangTheScan(t *testing.T) {
	f := &fakeDocker{}
	u, _, _ := newUpdater(f, false)
	u.docker = &blockingDocker{fakeDocker: f, list: true}
	u.apiTimeout = 50 * time.Millisecond
	if err := runWithDeadline(t, u); err == nil || !strings.Contains(err.Error(), "list containers") {
		t.Fatalf("%v", err)
	}
}

func TestStuckPullDoesNotHangTheScan(t *testing.T) {
	f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil), web("b", "other:1", "sha256:1", nil)},
		images: map[string]string{"other:1": "sha256:1"}}
	u, _, s := newUpdater(f, false)
	u.docker = &blockingDocker{fakeDocker: f, pull: true}
	u.pullTimeout = 50 * time.Millisecond
	if err := runWithDeadline(t, u); err != nil {
		t.Fatal(err)
	}
	if snap := s.Snapshot(); snap.Scans != 1 || snap.Failures != 2 {
		t.Fatalf("%+v", snap)
	}
}

type fakeAuth map[string]string

func (a fakeAuth) AuthHeader(ref string) string { return a[ref] }

func TestRegistryAuthIsForwardedToPull(t *testing.T) {
	f := &fakeDocker{
		containers: []docker.Container{web("a", "ghcr.io/o/priv:1", "sha256:1", nil), web("b", "nginx:latest", "sha256:1", nil)},
		images:     map[string]string{"ghcr.io/o/priv:1": "sha256:1", "nginx:latest": "sha256:1"},
	}
	u, _, _ := newUpdater(f, false)
	u.WithRegistryAuth(fakeAuth{"ghcr.io/o/priv:1": "HEADER-FOR-GHCR"})
	if err := u.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.pullAuth["ghcr.io/o/priv:1"] != "HEADER-FOR-GHCR" || f.pullAuth["nginx:latest"] != "" {
		t.Fatalf("%v", f.pullAuth)
	}
}

func TestNoAuthProviderPullsAnonymously(t *testing.T) {
	for _, withNil := range []bool{false, true} {
		f := &fakeDocker{containers: []docker.Container{web("a", "nginx:latest", "sha256:1", nil)},
			images: map[string]string{"nginx:latest": "sha256:1"}}
		u, _, _ := newUpdater(f, false)
		if withNil {
			u.WithRegistryAuth(nil)
		}
		u.RunOnce(context.Background())
		if auth, ok := f.pullAuth["nginx:latest"]; !ok || auth != "" {
			t.Fatalf("withNil=%v: %v", withNil, f.pullAuth)
		}
	}
}

func TestUnauthorizedPullIsReportedOnceWithoutLeakingCredentials(t *testing.T) {
	const secret = "SECRET-AUTH-HEADER"
	f := &fakeDocker{
		containers: []docker.Container{web("a", "ghcr.io/o/priv:1", "sha256:1", nil)},
		pullErr:    map[string]error{"ghcr.io/o/priv:1": errors.New("pull ghcr.io/o/priv:1: unauthorized: authentication required")},
	}
	n := &fakeNotifier{}
	s, _ := stats.New("test", "active", "")
	var logs strings.Builder
	u := New(f, n, s, "selfself0000", false, slog.New(slog.NewTextHandler(&logs, nil)))
	u.WithRegistryAuth(fakeAuth{"ghcr.io/o/priv:1": secret})
	u.RunOnce(context.Background())
	u.RunOnce(context.Background())
	snap := s.Snapshot()
	if len(snap.Events) != 1 || snap.Failures != 1 || snap.Events[0].Status != stats.StatusFailed ||
		!strings.Contains(snap.Events[0].Error, "unauthorized") {
		t.Fatalf("%+v", snap)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "unauthorized") {
		t.Fatalf("%v", n.msgs)
	}
	evJSON := fmt.Sprintf("%+v", snap.Events)
	for name, text := range map[string]string{"log": logs.String(), "notification": strings.Join(n.msgs, "\n"), "events": evJSON} {
		if strings.Contains(text, secret) {
			t.Errorf("%s leaks the registry credentials: %s", name, text)
		}
	}
}
