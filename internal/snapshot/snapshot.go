package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Snapshot represents a point-in-time snapshot of the project directory.
type Snapshot struct {
	ID        int
	Path      string
	CreatedAt time.Time
	Trigger   string // "init", "quiet", "threshold", "pre-command"
	Summary   string // e.g. "3 files changed: src/main.go, README.md, +1"
}

// Snapshotter manages project directory snapshots.
type Snapshotter struct {
	ProjectDir  string
	SnapshotDir string // .yu/snapshots/
	Keep        int    // max snapshots to retain
	cloner      Cloner
}

// Cloner is the platform-specific copy implementation.
type Cloner interface {
	Clone(src, dst string) error
	Restore(src, dst string) error
}

// New creates a Snapshotter.
func New(projectDir string, keep int, excludes map[string]bool) *Snapshotter {
	return &Snapshotter{
		ProjectDir:  projectDir,
		SnapshotDir: filepath.Join(projectDir, ".yu", "snapshots"),
		Keep:        keep,
		cloner:      &platformCloner{excludes: excludes},
	}
}

// Create takes a new snapshot, returns its ID.
func (s *Snapshotter) Create(trigger string) (*Snapshot, error) {
	if err := os.MkdirAll(s.SnapshotDir, 0700); err != nil {
		return nil, fmt.Errorf("creating snapshot dir: %w", err)
	}

	id := s.nextID()
	snapPath := filepath.Join(s.SnapshotDir, fmt.Sprintf("%d", id))

	if err := s.cloner.Clone(s.ProjectDir, snapPath); err != nil {
		return nil, fmt.Errorf("cloning: %w", err)
	}

	// Diff against previous snapshot for summary
	summary := "baseline"
	if id > 0 {
		prevPath := filepath.Join(s.SnapshotDir, fmt.Sprintf("%d", id-1))
		summary = diffSummary(prevPath, snapPath)
	}

	// Write metadata
	meta := filepath.Join(snapPath, ".yu-snapshot-meta")
	metaContent := fmt.Sprintf("trigger=%s\ntime=%s\nsummary=%s\n", trigger, time.Now().Format(time.RFC3339), summary)
	os.WriteFile(meta, []byte(metaContent), 0600)

	snap := &Snapshot{
		ID:        id,
		Path:      snapPath,
		CreatedAt: time.Now(),
		Trigger:   trigger,
		Summary:   summary,
	}

	// Prune old snapshots
	s.prune()

	return snap, nil
}

// List returns all snapshots sorted by ID.
func (s *Snapshotter) List() []Snapshot {
	entries, err := os.ReadDir(s.SnapshotDir)
	if err != nil {
		return nil
	}

	var snaps []Snapshot
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		info, _ := e.Info()
		trigger := "unknown"
		summary := ""
		created := time.Time{}
		if meta, err := os.ReadFile(filepath.Join(s.SnapshotDir, e.Name(), ".yu-snapshot-meta")); err == nil {
			for _, line := range splitLines(string(meta)) {
				if strings.HasPrefix(line, "trigger=") {
					trigger = line[8:]
				} else if strings.HasPrefix(line, "summary=") {
					summary = line[8:]
				} else if strings.HasPrefix(line, "time=") {
					if t, err := time.Parse(time.RFC3339, line[5:]); err == nil {
						created = t
					}
				}
			}
		}
		if created.IsZero() {
			if info != nil {
				created = info.ModTime()
			}
		}
		snaps = append(snaps, Snapshot{
			ID:      id,
			Summary: summary,
			Path:      filepath.Join(s.SnapshotDir, e.Name()),
			CreatedAt: created,
			Trigger:   trigger,
		})
	}

	sort.Slice(snaps, func(i, j int) bool { return snaps[i].ID < snaps[j].ID })
	return snaps
}

// Rollback restores the project directory from a snapshot.
func (s *Snapshotter) Rollback(id int) error {
	snapPath := filepath.Join(s.SnapshotDir, fmt.Sprintf("%d", id))
	if _, err := os.Stat(snapPath); err != nil {
		return fmt.Errorf("snapshot %d not found", id)
	}
	return s.cloner.Restore(snapPath, s.ProjectDir)
}

func (s *Snapshotter) nextID() int {
	snaps := s.List()
	if len(snaps) == 0 {
		return 0
	}
	return snaps[len(snaps)-1].ID + 1
}

// prune applies a time-bucketed retention policy.
//
// Strategy (max 5 snapshots kept):
//   - Daily bucket  (1 slot): the most recent snapshot aged >= 24h
//   - Hourly bucket (1 slot): the most recent snapshot aged >= 1h but < 24h
//   - Recent bucket (up to 3 slots): the N most recent snapshots
//
// Any snapshot not selected is deleted. If fewer than 5 exist, nothing is pruned.
func (s *Snapshotter) prune() {
	snaps := s.List() // sorted by ID ascending
	if len(snaps) <= s.Keep {
		return
	}

	now := time.Now()
	keep := make(map[int]bool)

	// --- Bucket 1: daily (>= 24h old) — pick the most recent one ---
	for i := len(snaps) - 1; i >= 0; i-- {
		age := now.Sub(snaps[i].CreatedAt)
		if age >= 24*time.Hour {
			keep[snaps[i].ID] = true
			break
		}
	}

	// --- Bucket 2: hourly (>= 1h old, < 24h) — pick the most recent one ---
	for i := len(snaps) - 1; i >= 0; i-- {
		age := now.Sub(snaps[i].CreatedAt)
		if age >= time.Hour && age < 24*time.Hour && !keep[snaps[i].ID] {
			keep[snaps[i].ID] = true
			break
		}
	}

	// --- Bucket 3: fill remaining slots with most recent snapshots ---
	remaining := s.Keep - len(keep)
	for i := len(snaps) - 1; i >= 0 && remaining > 0; i-- {
		if !keep[snaps[i].ID] {
			keep[snaps[i].ID] = true
			remaining--
		}
	}

	// --- Delete everything not kept ---
	for _, snap := range snaps {
		if !keep[snap.ID] {
			os.RemoveAll(snap.Path)
		}
	}
}

// diffSummary compares two snapshot directories and returns a short summary.
// e.g. "3 files changed: src/main.go, README.md, +1"
func diffSummary(prevDir, curDir string) string {
	prevFiles := listFiles(prevDir)
	curFiles := listFiles(curDir)

	var changed []string

	// Files added or modified
	for path, curInfo := range curFiles {
		prevInfo, exists := prevFiles[path]
		if !exists {
			changed = append(changed, "+"+path)
		} else if curInfo.Size() != prevInfo.Size() || curInfo.ModTime() != prevInfo.ModTime() {
			changed = append(changed, path)
		}
	}

	// Files deleted
	for path := range prevFiles {
		if _, exists := curFiles[path]; !exists {
			changed = append(changed, "-"+path)
		}
	}

	if len(changed) == 0 {
		return "no changes"
	}

	sort.Strings(changed)

	// Show up to 3 file names, then "+N"
	display := changed
	extra := 0
	if len(display) > 3 {
		extra = len(display) - 2
		display = display[:2]
	}

	summary := fmt.Sprintf("%d files: %s", len(changed), strings.Join(display, ", "))
	if extra > 0 {
		summary += fmt.Sprintf(", +%d more", extra)
	}
	return summary
}

// listFiles returns relative paths → FileInfo for all files in a directory.
func listFiles(dir string) map[string]os.FileInfo {
	files := make(map[string]os.FileInfo)
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		// Skip metadata file
		if rel == ".yu-snapshot-meta" {
			return nil
		}
		files[rel] = info
		return nil
	})
	return files
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
