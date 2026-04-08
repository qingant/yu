package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshotWithSummary(t *testing.T) {
	dir := t.TempDir()

	// Create initial files
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Hello"), 0644)

	s := New(dir, 5)

	// Snapshot 0 — baseline
	snap0, err := s.Create("init")
	if err != nil {
		t.Fatal(err)
	}
	if snap0.Summary != "baseline" {
		t.Errorf("snap0 summary: got %q, want %q", snap0.Summary, "baseline")
	}
	t.Logf("#%d [%s] %s", snap0.ID, snap0.Trigger, snap0.Summary)

	// Modify a file
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}"), 0644)

	// Snapshot 1 — should show main.go changed
	snap1, err := s.Create("quiet")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("#%d [%s] %s", snap1.ID, snap1.Trigger, snap1.Summary)
	if snap1.Summary == "baseline" || snap1.Summary == "no changes" {
		t.Errorf("snap1 should show changes, got: %q", snap1.Summary)
	}

	// Add a new file
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("key: value"), 0644)

	// Snapshot 2 — should show new file
	snap2, err := s.Create("threshold")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("#%d [%s] %s", snap2.ID, snap2.Trigger, snap2.Summary)

	// Delete a file
	os.Remove(filepath.Join(dir, "README.md"))

	// Snapshot 3 — should show deleted file
	snap3, err := s.Create("pre-command:git")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("#%d [%s] %s", snap3.ID, snap3.Trigger, snap3.Summary)

	// List all and verify summaries are persisted
	snaps := s.List()
	for _, sn := range snaps {
		t.Logf("List: #%d [%s] %s", sn.ID, sn.Trigger, sn.Summary)
		if sn.Summary == "" {
			t.Errorf("snapshot #%d has empty summary", sn.ID)
		}
	}
}

func TestPruneTimeBucketed(t *testing.T) {
	// Create a snapshotter with Keep=5 and manually craft snapshots
	// with different ages to verify the time-bucketed retention.
	dir := t.TempDir()
	snapDir := filepath.Join(dir, ".yu", "snapshots")
	os.MkdirAll(snapDir, 0700)

	s := &Snapshotter{
		ProjectDir:  dir,
		SnapshotDir: snapDir,
		Keep:        5,
		cloner:      &platformCloner{},
	}

	now := time.Now()
	ages := []time.Duration{
		48 * time.Hour,  // #0: 2 days old  → daily bucket
		36 * time.Hour,  // #1: 1.5 days old
		25 * time.Hour,  // #2: 25h old
		3 * time.Hour,   // #3: 3h old      → hourly bucket
		2 * time.Hour,   // #4: 2h old
		30 * time.Minute, // #5: 30min old  → recent
		10 * time.Minute, // #6: 10min old  → recent
		1 * time.Minute,  // #7: 1min old   → recent
	}

	for i, age := range ages {
		snapPath := filepath.Join(snapDir, fmt.Sprintf("%d", i))
		os.MkdirAll(snapPath, 0700)
		ts := now.Add(-age)
		meta := fmt.Sprintf("trigger=test\ntime=%s\nsummary=test snap %d\n", ts.Format(time.RFC3339), i)
		os.WriteFile(filepath.Join(snapPath, ".yu-snapshot-meta"), []byte(meta), 0600)
	}

	// Verify we have 8 snapshots before prune
	before := s.List()
	if len(before) != 8 {
		t.Fatalf("expected 8 snapshots before prune, got %d", len(before))
	}

	s.prune()

	after := s.List()
	t.Logf("After prune: %d snapshots", len(after))
	for _, sn := range after {
		age := now.Sub(sn.CreatedAt).Round(time.Minute)
		t.Logf("  #%d  age=%v  %s", sn.ID, age, sn.Summary)
	}

	if len(after) != 5 {
		t.Fatalf("expected 5 snapshots after prune, got %d", len(after))
	}

	// Check that the daily bucket kept a snapshot >= 24h old
	kept := make(map[int]bool)
	for _, sn := range after {
		kept[sn.ID] = true
	}

	// #0 (2 days) should be the daily bucket pick (most recent >= 24h → actually #2 at 25h)
	// The algorithm picks the most recent >= 24h, which is #2
	if !kept[2] {
		t.Error("expected snapshot #2 (25h old) to be kept as daily bucket")
	}

	// #3 (3h) should be the hourly bucket pick (most recent >= 1h and < 24h that's not already kept)
	// Actually #4 is 2h old and also >= 1h — the algorithm picks most recent first,
	// so it picks #4 (2h) for hourly bucket
	if !kept[4] {
		t.Error("expected snapshot #4 (2h old) to be kept as hourly bucket")
	}

	// Most recent 3: #7, #6, #5
	if !kept[7] || !kept[6] || !kept[5] {
		t.Errorf("expected most recent 3 (#5,#6,#7) to be kept, got kept=%v", kept)
	}
}
