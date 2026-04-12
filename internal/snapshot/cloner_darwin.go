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
	".git": true,
	"node_modules": true, ".next": true, ".nuxt": true,
	"__pycache__": true, ".venv": true, "venv": true, ".tox": true,
	"target": true, "build": true, "dist": true,
	".gradle": true, ".cache": true, ".turbo": true,
	".pytest_cache": true, ".mypy_cache": true,
}

func (c *platformCloner) Clone(src, dst string) error {
	// If the project is a git repo, snapshot only git-known files.
	// This is precise and fast — no need to traverse giant directories.
	if isGitRepo(src) {
		return c.cloneGitFiles(src, dst)
	}

	// For non-git dirs, bail if too large (e.g. home directory).
	entries, err := os.ReadDir(src)
	if err == nil && len(entries) > 200 {
		return fmt.Errorf("too many entries (%d) for non-git snapshot, skipping", len(entries))
	}

	return c.cloneDir(src, dst)
}

// cloneGitFiles snapshots only files known to git (tracked + untracked non-ignored).
func (c *platformCloner) cloneGitFiles(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}

	// git ls-files -co --exclude-standard: tracked + untracked, respects .gitignore
	cmd := exec.Command("git", "ls-files", "-co", "--exclude-standard")
	cmd.Dir = src
	out, err := cmd.Output()
	if err != nil {
		// Fallback to directory walk
		return c.cloneDir(src, dst)
	}

	files := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(files) == 0 || (len(files) == 1 && files[0] == "") {
		return nil
	}

	// Create all needed directories and clone files
	dirsMade := make(map[string]bool)
	for _, rel := range files {
		if rel == "" {
			continue
		}

		// Check excludes
		if c.isExcludedPath(rel) {
			continue
		}

		srcPath := filepath.Join(src, rel)
		dstPath := filepath.Join(dst, rel)

		// Ensure parent directory exists
		dir := filepath.Dir(dstPath)
		if !dirsMade[dir] {
			os.MkdirAll(dir, 0755)
			dirsMade[dir] = true
		}

		// APFS clone single file
		cp := exec.Command("cp", "-c", srcPath, dstPath)
		if _, err := cp.CombinedOutput(); err != nil {
			// Fallback to regular copy
			cp = exec.Command("cp", srcPath, dstPath)
			if out2, err2 := cp.CombinedOutput(); err2 != nil {
				return fmt.Errorf("copying %s: %s", rel, string(out2))
			}
		}
	}

	return nil
}

// isExcludedPath checks if a relative path should be excluded.
func (c *platformCloner) isExcludedPath(rel string) bool {
	// Check exact match
	if c.excludes[rel] {
		return true
	}
	// Check each path component against skipClone
	parts := strings.Split(rel, string(os.PathSeparator))
	for _, p := range parts {
		if skipClone[p] {
			return true
		}
	}
	// Check parent paths against excludes
	for p := filepath.Dir(rel); p != "." && p != ""; p = filepath.Dir(p) {
		if c.excludes[p] {
			return true
		}
	}
	return false
}

// isGitRepo checks if a directory contains a .git directory.
func isGitRepo(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && info.IsDir()
}

// cloneDir recursively clones a directory, skipping excluded paths.
// Used as fallback for non-git projects.
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
