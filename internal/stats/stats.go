// Package stats keeps scan/update counters and a bounded event history.
package stats

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Event statuses.
const (
	StatusUpdated    = "updated"
	StatusFailed     = "failed"
	StatusRolledBack = "rolled_back"
	StatusAvailable  = "available"

	maxEvents = 50
)

// Container statuses shown in the container list.
const (
	ContainerUpToDate        = "up-to-date"
	ContainerUpdated         = "updated"
	ContainerUpdateAvailable = "update-available"
	ContainerWaitingWindow   = "waiting-window"
	ContainerFailed          = "failed"
	ContainerRolledBack      = "rolled-back"
	ContainerSkipped         = "skipped"
	ContainerSelf            = "self"
)

// ContainerStatuses lists every container status, in display order.
var ContainerStatuses = []string{
	ContainerUpToDate, ContainerUpdated, ContainerUpdateAvailable, ContainerWaitingWindow,
	ContainerFailed, ContainerRolledBack, ContainerSkipped, ContainerSelf,
}

// ContainerStatus is what the last scan found about one container. It is never persisted.
type ContainerStatus struct {
	Name      string    `json:"name"`
	Image     string    `json:"image"`
	ImageID   string    `json:"image_id"` // short (12 hex)
	Status    string    `json:"status"`
	Detail    string    `json:"detail"`
	Policy    string    `json:"policy"` // "auto", "notify" or "invalid"
	Window    string    `json:"window"` // raw window label, or ""
	CheckedAt time.Time `json:"checked_at"`
}

// Event is one entry of the activity history.
type Event struct {
	Time      time.Time `json:"time"`
	Container string    `json:"container"`
	Image     string    `json:"image"`
	OldID     string    `json:"old_id"`
	NewID     string    `json:"new_id"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
}

// Snapshot is a copy of the counters and recent events at a point in time.
type Snapshot struct {
	Version    string            `json:"version"`
	Mode       string            `json:"mode"`
	StartedAt  time.Time         `json:"started_at"`
	Scans      int               `json:"scans"`
	LastScan   time.Time         `json:"last_scan"`
	NextScan   time.Time         `json:"next_scan"`
	LastScanMs int64             `json:"last_scan_ms"`
	Watched    int               `json:"watched"`
	Skipped    int               `json:"skipped"`
	Updates    int               `json:"updates"`
	Failures   int               `json:"failures"`
	Rollbacks  int               `json:"rollbacks"`
	Events     []Event           `json:"events"`
	Scanning   bool              `json:"scanning"`
	Containers []ContainerStatus `json:"containers"`
	// LastProgress is the last time the scan made progress (see Touch). New fields are appended
	// after it: consumers rely on the JSON key order. It is never persisted.
	LastProgress time.Time `json:"last_progress"`
}

// Stats is a concurrency-safe store of counters and recent events, optionally persisted to a file.
type Stats struct {
	mu   sync.Mutex
	snap Snapshot
	path string
}

// New returns a usable store even when the state file is corrupt; the error only reports that.
func New(version, mode, path string) (*Stats, error) {
	s := &Stats{path: path, snap: Snapshot{Version: version, Mode: mode, StartedAt: time.Now(), Events: []Event{}, Containers: []ContainerStatus{}}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	var saved Snapshot
	if err := json.Unmarshal(b, &saved); err != nil {
		return s, fmt.Errorf("ignoring corrupt state file %s: %w", path, err)
	}
	s.snap.Scans, s.snap.LastScan, s.snap.LastScanMs = saved.Scans, saved.LastScan, saved.LastScanMs
	s.snap.Updates, s.snap.Failures, s.snap.Rollbacks = saved.Updates, saved.Failures, saved.Rollbacks
	if saved.Events != nil {
		s.snap.Events = saved.Events
	}
	return s, nil
}

// RecordScan records a finished scan: its start, duration and the number of watched and skipped containers.
func (s *Stats) RecordScan(start time.Time, d time.Duration, watched, skipped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Scans++
	s.snap.LastScan = start
	s.snap.LastScanMs = d.Milliseconds()
	s.snap.Watched, s.snap.Skipped = watched, skipped
	s.saveLocked()
}

// RecordPartialScan records a finished scan of a single container: like RecordScan it counts the scan
// and records its start and duration, but the watched and skipped gauges describe the whole host and a
// partial view must not overwrite them, so they keep their previous values.
func (s *Stats) RecordPartialScan(start time.Time, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Scans++
	s.snap.LastScan = start
	s.snap.LastScanMs = d.Milliseconds()
	s.saveLocked()
}

// Touch records that the scan just made progress (a pull, an inspection, a container step), so
// that a long scan that keeps working is not mistaken for a stuck one. It is never persisted.
func (s *Stats) Touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.LastProgress = time.Now()
}

// SetNextScan sets the time of the next planned scan.
func (s *Stats) SetNextScan(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.NextScan = t
}

// SetScanning records whether a scan is currently running. It is never persisted.
func (s *Stats) SetScanning(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Scanning = v
}

// SetContainers replaces the container list (sorted by name). It is never persisted.
func (s *Stats) SetContainers(list []ContainerStatus) {
	cp := append([]ContainerStatus{}, list...)
	sort.SliceStable(cp, func(i, j int) bool { return cp[i].Name < cp[j].Name })
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Containers = cp
}

// MergeContainers replaces (or adds), by name, the given entries of the container list and keeps every
// other entry untouched, checked_at included. The list stays sorted by name. It is never persisted.
func (s *Stats) MergeContainers(entries []ContainerStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	merged := append([]ContainerStatus{}, s.snap.Containers...)
	for _, e := range entries {
		replaced := false
		for i := range merged {
			if merged[i].Name == e.Name {
				merged[i], replaced = e, true
				break
			}
		}
		if !replaced {
			merged = append(merged, e)
		}
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Name < merged[j].Name })
	s.snap.Containers = merged
}

// RemoveContainer drops the entry called name from the container list, for a container that no longer
// exists. Every other entry is kept as it was. It is never persisted.
func (s *Stats) RemoveContainer(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]ContainerStatus, 0, len(s.snap.Containers))
	for _, c := range s.snap.Containers {
		if c.Name != name {
			kept = append(kept, c)
		}
	}
	s.snap.Containers = kept
}

// RecordEvent adds an event to the history (newest first, bounded) and updates the counters.
func (s *Stats) RecordEvent(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	switch e.Status {
	case StatusUpdated:
		s.snap.Updates++
	case StatusFailed:
		s.snap.Failures++
	case StatusRolledBack:
		s.snap.Rollbacks++
	}
	s.snap.Events = append([]Event{e}, s.snap.Events...)
	if len(s.snap.Events) > maxEvents {
		s.snap.Events = s.snap.Events[:maxEvents]
	}
	s.saveLocked()
}

// Snapshot returns a copy of the current state.
func (s *Stats) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.snap
	out.Events = append([]Event(nil), s.snap.Events...)
	out.Containers = append([]ContainerStatus{}, s.snap.Containers...)
	return out
}

// saveLocked writes the state atomically (temp file + rename). Failures are only logged.
func (s *Stats) saveLocked() {
	if s.path == "" {
		return
	}
	persisted := s.snap
	persisted.Scanning = false
	persisted.Containers = nil
	persisted.LastProgress = time.Time{}
	b, err := json.Marshal(persisted)
	if err == nil {
		tmp := filepath.Join(filepath.Dir(s.path), "."+filepath.Base(s.path)+".tmp")
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			if err = os.Rename(tmp, s.path); err != nil {
				_ = os.Remove(tmp)
			}
		}
	}
	if err != nil {
		slog.Warn("could not persist stats", "error", err)
	}
}
