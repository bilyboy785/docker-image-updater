// Package stats keeps scan/update counters and a bounded event history.
package stats

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	StatusUpdated    = "updated"
	StatusFailed     = "failed"
	StatusRolledBack = "rolled_back"
	StatusAvailable  = "available"

	maxEvents = 50
)

type Event struct {
	Time      time.Time `json:"time"`
	Container string    `json:"container"`
	Image     string    `json:"image"`
	OldID     string    `json:"old_id"`
	NewID     string    `json:"new_id"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
}

type Snapshot struct {
	Version    string    `json:"version"`
	Mode       string    `json:"mode"`
	StartedAt  time.Time `json:"started_at"`
	Scans      int       `json:"scans"`
	LastScan   time.Time `json:"last_scan"`
	NextScan   time.Time `json:"next_scan"`
	LastScanMs int64     `json:"last_scan_ms"`
	Watched    int       `json:"watched"`
	Skipped    int       `json:"skipped"`
	Updates    int       `json:"updates"`
	Failures   int       `json:"failures"`
	Rollbacks  int       `json:"rollbacks"`
	Events     []Event   `json:"events"`
}

type Stats struct {
	mu   sync.Mutex
	snap Snapshot
	path string
}

// New returns a usable store even when the state file is corrupt; the error only reports that.
func New(version, mode, path string) (*Stats, error) {
	s := &Stats{path: path, snap: Snapshot{Version: version, Mode: mode, StartedAt: time.Now(), Events: []Event{}}}
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

func (s *Stats) RecordScan(start time.Time, d time.Duration, watched, skipped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Scans++
	s.snap.LastScan = start
	s.snap.LastScanMs = d.Milliseconds()
	s.snap.Watched, s.snap.Skipped = watched, skipped
	s.saveLocked()
}

func (s *Stats) SetNextScan(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.NextScan = t
}

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

func (s *Stats) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.snap
	out.Events = append([]Event(nil), s.snap.Events...)
	return out
}

// saveLocked writes the state atomically (temp file + rename). Failures are only logged.
func (s *Stats) saveLocked() {
	if s.path == "" {
		return
	}
	b, err := json.Marshal(s.snap)
	if err == nil {
		tmp := filepath.Join(filepath.Dir(s.path), "."+filepath.Base(s.path)+".tmp")
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, s.path)
		}
	}
	if err != nil {
		slog.Warn("could not persist stats", "error", err)
	}
}
