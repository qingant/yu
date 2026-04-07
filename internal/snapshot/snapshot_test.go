package snapshot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotWithSummary(t *testing.T) {
	dir := t.TempDir()

	// Create initial files
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Hello"), 0644)

	s := New(dir, 10)

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
