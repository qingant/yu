package snapshot

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// platformCloner uses APFS clone (cp -c) on macOS for instant COW copies.
type platformCloner struct{}

func (c *platformCloner) Clone(src, dst string) error {
	// Use cp -c for APFS clone (instant, COW)
	// Exclude .yu/ directory from the snapshot to avoid recursive snapshots
	// and to avoid copying credentials

	// Create destination
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}

	// List source entries, skip .yu and .git (git dir can be huge and is recoverable)
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("reading source dir: %w", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if name == ".yu" || name == ".git" {
			continue
		}

		srcPath := filepath.Join(src, name)
		dstPath := filepath.Join(dst, name)

		// Use cp -c -r for APFS clone
		cmd := exec.Command("cp", "-c", "-r", srcPath, dstPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			// Fallback to regular copy if APFS clone not supported
			cmd = exec.Command("cp", "-r", srcPath, dstPath)
			if out2, err2 := cmd.CombinedOutput(); err2 != nil {
				return fmt.Errorf("copying %s: %s %s", name, string(out), string(out2))
			}
			_ = out
		}
	}

	return nil
}

func (c *platformCloner) Restore(snapPath, projectDir string) error {
	// List snapshot entries
	entries, err := os.ReadDir(snapPath)
	if err != nil {
		return fmt.Errorf("reading snapshot: %w", err)
	}

	// Collect snapshot file names
	snapFiles := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if name == ".yu-snapshot-meta" {
			continue
		}
		snapFiles[name] = true
	}

	// Remove project files that aren't in the snapshot (except .yu and .git)
	projectEntries, _ := os.ReadDir(projectDir)
	for _, entry := range projectEntries {
		name := entry.Name()
		if name == ".yu" || name == ".git" {
			continue
		}
		if !snapFiles[name] {
			os.RemoveAll(filepath.Join(projectDir, name))
		}
	}

	// Restore each entry from snapshot
	for _, entry := range entries {
		name := entry.Name()
		if name == ".yu-snapshot-meta" {
			continue
		}
		if strings.HasPrefix(name, ".yu") {
			continue
		}

		dstPath := filepath.Join(projectDir, name)
		srcPath := filepath.Join(snapPath, name)

		// Remove existing
		os.RemoveAll(dstPath)

		// Clone from snapshot
		cmd := exec.Command("cp", "-c", "-r", srcPath, dstPath)
		if _, err := cmd.CombinedOutput(); err != nil {
			// Fallback
			cmd = exec.Command("cp", "-r", srcPath, dstPath)
			if out, err2 := cmd.CombinedOutput(); err2 != nil {
				return fmt.Errorf("restoring %s: %s", name, string(out))
			}
		}
	}

	return nil
}
