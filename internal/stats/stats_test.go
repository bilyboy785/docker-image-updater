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
