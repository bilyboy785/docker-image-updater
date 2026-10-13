package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCounters(t *testing.T) {
	s, _ := New("v1", "active", "")
	s.RecordScan(time.Unix(100, 0), 1500*time.Millisecond, 4, 2)
	s.RecordEvent(Event{Container: "a", Status: StatusUpdated})
	s.RecordEvent(Event{Container: "b", Status: StatusFailed})
	s.RecordEvent(Event{Container: "c", Status: StatusRolledBack})
	s.RecordEvent(Event{Container: "d", Status: StatusAvailable})
	snap := s.Snapshot()
	if snap.Scans != 1 || snap.Watched != 4 || snap.Skipped != 2 || snap.LastScanMs != 1500 {
		t.Fatalf("%+v", snap)
	}
	if snap.Updates != 1 || snap.Failures != 1 || snap.Rollbacks != 1 {
		t.Fatalf("%+v", snap)
	}
	if len(snap.Events) != 4 || snap.Events[0].Container != "d" || snap.Events[0].Time.IsZero() {
		t.Fatalf("events newest first with time: %+v", snap.Events)
	}
}

func TestEventCap(t *testing.T) {
	s, _ := New("v", "active", "")
	for i := 0; i < 60; i++ {
		s.RecordEvent(Event{Container: "c", Status: StatusUpdated})
	}
	snap := s.Snapshot()
	if len(snap.Events) != 50 || snap.Updates != 60 {
		t.Fatalf("events=%d updates=%d", len(snap.Events), snap.Updates)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	s, _ := New("v", "active", "")
	s.RecordEvent(Event{Container: "a", Status: StatusUpdated})
	snap := s.Snapshot()
	snap.Events[0].Container = "mutated"
	if s.Snapshot().Events[0].Container != "a" {
		t.Fatal("snapshot shares memory with the store")
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := New("v1", "active", path)
	if err != nil {
		t.Fatal(err)
	}
	s.RecordScan(time.Unix(100, 0), time.Second, 3, 1)
	s.RecordEvent(Event{Container: "a", Status: StatusUpdated})

	s2, err := New("v2", "dry-run", path)
	if err != nil {
		t.Fatal(err)
	}
	snap := s2.Snapshot()
	if snap.Scans != 1 || snap.Updates != 1 || len(snap.Events) != 1 {
		t.Fatalf("not reloaded: %+v", snap)
	}
	if snap.Version != "v2" || snap.Mode != "dry-run" {
		t.Fatalf("version/mode must come from the new process: %+v", snap)
	}
}

func TestCorruptStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	s, err := New("v", "active", path)
	if err == nil || s == nil {
		t.Fatalf("expected an error and a usable store, got %v %v", s, err)
	}
	s.RecordEvent(Event{Status: StatusUpdated}) // must not panic
}

func TestJSONKeys(t *testing.T) {
	s, _ := New("v", "active", "")
	b, _ := json.Marshal(s.Snapshot())
	if !strings.Contains(string(b), `"updates":0,"failures":0`) {
		t.Fatalf("unexpected JSON layout: %s", b)
	}
}

func TestFailedRenameRemovesTempFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state.json")
	if err := os.Mkdir(target, 0o700); err != nil { // renaming a file over a directory fails
		t.Fatal(err)
	}
	s, _ := New("v", "active", target)
	s.RecordScan(time.Now(), time.Millisecond, 1, 0)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

func TestScanning(t *testing.T) {
	s, _ := New("v", "active", "")
	if s.Snapshot().Scanning {
		t.Fatal("scanning must start false")
	}
	s.SetScanning(true)
	if !s.Snapshot().Scanning {
		t.Fatal("scanning not set")
	}
	s.SetScanning(false)
	if s.Snapshot().Scanning {
		t.Fatal("scanning not cleared")
	}
}

func TestScanningIsNotPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := New("v", "active", path)
	s.SetScanning(true)
	s.RecordEvent(Event{Status: StatusUpdated}) // triggers a save while scanning
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"scanning":false`) {
		t.Fatalf("scanning leaked into the state file: %s", b)
	}
	s2, _ := New("v", "active", path)
	if s2.Snapshot().Scanning {
		t.Fatal("scanning must be false after a reload")
	}
}

func TestScanningJSONKeyPrecedesContainers(t *testing.T) {
	s, _ := New("v", "active", "")
	s.SetScanning(true)
	b, _ := json.Marshal(s.Snapshot())
	if !strings.Contains(string(b), `,"scanning":true,"containers":[]`) {
		t.Fatalf("scanning must be the last key before containers: %s", b)
	}
}

func TestSetContainersSortsByNameAndReplaces(t *testing.T) {
	s, _ := New("v", "active", "")
	s.SetContainers([]ContainerStatus{{Name: "b", Status: ContainerUpToDate}, {Name: "a", Status: ContainerUpdated}})
	got := s.Snapshot().Containers
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("not sorted: %+v", got)
	}
	s.SetContainers([]ContainerStatus{{Name: "c"}})
	if got := s.Snapshot().Containers; len(got) != 1 || got[0].Name != "c" {
		t.Fatalf("not replaced: %+v", got)
	}
	s.SetContainers(nil)
	if got := s.Snapshot().Containers; got == nil || len(got) != 0 {
		t.Fatalf("want an empty non-nil list: %#v", got)
	}
}

func TestSetContainersCopiesItsInputAndItsOutput(t *testing.T) {
	s, _ := New("v", "active", "")
	in := []ContainerStatus{{Name: "a"}, {Name: "b"}}
	s.SetContainers(in)
	in[0].Name = "mutated-in"
	snap := s.Snapshot()
	if snap.Containers[0].Name != "a" {
		t.Fatal("store shares memory with its input")
	}
	snap.Containers[0].Name = "mutated-out"
	if s.Snapshot().Containers[0].Name != "a" {
		t.Fatal("snapshot shares memory with the store")
	}
}

func TestContainersAreNotPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := New("v", "active", path)
	s.SetContainers([]ContainerStatus{{Name: "secret-name", Status: ContainerUpdated}})
	s.RecordEvent(Event{Status: StatusUpdated}) // triggers a save
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret-name") {
		t.Fatalf("containers leaked into the state file: %s", b)
	}
	s2, _ := New("v", "active", path)
	if got := s2.Snapshot().Containers; got == nil || len(got) != 0 {
		t.Fatalf("containers must be empty after a reload: %#v", got)
	}
	if len(s.Snapshot().Containers) != 1 {
		t.Fatal("saving must not clear the live list")
	}
}

func TestContainersJSONKeyIsLast(t *testing.T) {
	s, _ := New("v", "active", "")
	s.SetContainers([]ContainerStatus{{Name: "a", Image: "i", ImageID: "abc", Status: ContainerSkipped, Detail: "d", Policy: "auto", Window: "w", CheckedAt: time.Unix(0, 0).UTC()}})
	b, _ := json.Marshal(s.Snapshot())
	want := `"containers":[{"name":"a","image":"i","image_id":"abc","status":"skipped","detail":"d","policy":"auto","window":"w","checked_at":"1970-01-01T00:00:00Z"}],"last_progress":`
	if !strings.Contains(string(b), want) {
		t.Fatalf("last_progress must directly follow containers: %s", b)
	}
	if !strings.HasSuffix(string(b), "Z\"}") {
		t.Fatalf("last_progress must be the last key: %s", b)
	}
}

func TestTouchRecordsProgressAndIsNotPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := New("v", "active", path)
	if !s.Snapshot().LastProgress.IsZero() {
		t.Fatal("no progress before the first Touch")
	}
	before := time.Now()
	s.Touch()
	got := s.Snapshot().LastProgress
	if got.Before(before) || got.After(time.Now()) {
		t.Fatalf("LastProgress %v not set to now", got)
	}
	s.RecordEvent(Event{Status: StatusUpdated}) // triggers a save
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"last_progress":"0001-01-01T00:00:00Z"`) {
		t.Fatalf("last_progress leaked into the state file: %s", b)
	}
	s2, _ := New("v", "active", path)
	if !s2.Snapshot().LastProgress.IsZero() {
		t.Fatal("LastProgress must be zero after a reload")
	}
	if s.Snapshot().LastProgress.IsZero() {
		t.Fatal("saving must not clear the live value")
	}
}
