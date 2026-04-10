package snapshot

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type platformCloner struct {
	projectDir string          // root project dir, for computing relative paths
	excludes   map[string]bool // relative paths to skip (e.g. "zero/repos")
}

// Directories to always skip by name (any level).
var skipClone = map[string]bool{
	".yu": true, ".git": true,
	"node_modules": true, ".next": true, ".nuxt": true,
	"__pycache__": true, ".venv": true, "venv": true, ".tox": true,
	"target": true, "build": true, "dist": true,
	".gradle": true, ".cache": true, ".turbo": true,
	".pytest_cache": true, ".mypy_cache": true,
}

func (c *platformCloner) Clone(src, dst string) error {
	return c.cloneDir(src, dst)
}

// cloneDir recursively clones a directory, skipping excluded paths.
func (c *platformCloner) cloneDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("reading dir: %w", err)
	}

	for _, entry := range entries {
		name := entry.Name()

		// Skip dot-dirs and hardcoded skip list
		if strings.HasPrefix(name, ".") || skipClone[name] {
			continue
		}

		srcPath := filepath.Join(src, name)
		dstPath := filepath.Join(dst, name)

		// Check relative path against excludes
		if c.projectDir != "" {
			rel, _ := filepath.Rel(c.projectDir, srcPath)
			if c.excludes[rel] {
				continue
			}
		}

		if entry.IsDir() {
			// Check if any exclude is a child of this dir
			// If so, we need to recurse instead of bulk clone
			if c.hasExcludedChild(srcPath) {
				if err := c.cloneDir(srcPath, dstPath); err != nil {
					return err
				}
				continue
			}
		}

		// Bulk clone this entry
		cmd := exec.Command("cp", "-c", "-r", srcPath, dstPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "[yu] Warning: APFS clone failed for %s, using regular copy\n", name)
			cmd = exec.Command("cp", "-r", srcPath, dstPath)
			if out2, err2 := cmd.CombinedOutput(); err2 != nil {
				return fmt.Errorf("copying %s: %s %s", name, string(out), string(out2))
			}
			_ = out
		}
	}

	return nil
}

// hasExcludedChild returns true if any exclude path is under this directory.
func (c *platformCloner) hasExcludedChild(dir string) bool {
	if c.projectDir == "" {
		return false
	}
	rel, _ := filepath.Rel(c.projectDir, dir)
	prefix := rel + string(os.PathSeparator)
	for exc := range c.excludes {
		if strings.HasPrefix(exc, prefix) {
			return true
		}
	}
	return false
}

func (c *platformCloner) Restore(snapPath, projectDir string) error {
	entries, err := os.ReadDir(snapPath)
	if err != nil {
		return fmt.Errorf("reading snapshot: %w", err)
	}

	snapFiles := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if name == ".yu-snapshot-meta" {
			continue
		}
		snapFiles[name] = true
	}

	// Remove project files not in snapshot (except .dirs and .git/.yu)
	projectEntries, _ := os.ReadDir(projectDir)
	for _, entry := range projectEntries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if !snapFiles[name] {
			os.RemoveAll(filepath.Join(projectDir, name))
		}
	}

	// Restore from snapshot
	for _, entry := range entries {
		name := entry.Name()
		if name == ".yu-snapshot-meta" || strings.HasPrefix(name, ".") {
			continue
		}

		dstPath := filepath.Join(projectDir, name)
		srcPath := filepath.Join(snapPath, name)
		os.RemoveAll(dstPath)

		cmd := exec.Command("cp", "-c", "-r", srcPath, dstPath)
		if _, err := cmd.CombinedOutput(); err != nil {
			cmd = exec.Command("cp", "-r", srcPath, dstPath)
			if out, err2 := cmd.CombinedOutput(); err2 != nil {
				return fmt.Errorf("restoring %s: %s", name, string(out))
			}
		}
	}

	return nil
}
