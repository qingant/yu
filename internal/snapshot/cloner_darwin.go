package snapshot

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type platformCloner struct{}

// Directories to skip in snapshots.
var skipClone = map[string]bool{
	".yu": true, ".git": true,
	"node_modules": true, ".next": true, ".nuxt": true,
	"__pycache__": true, ".venv": true, "venv": true, ".tox": true,
	"target": true, "build": true, "dist": true,
	".gradle": true, ".cache": true, ".turbo": true,
	".pytest_cache": true, ".mypy_cache": true,
}

func (c *platformCloner) Clone(src, dst string) error {
	// If it's a git repo, use git to snapshot (fast, tiny, reliable)
	if isGitRepo(src) {
		return c.gitClone(src, dst)
	}
	return c.cpClone(src, dst)
}

func (c *platformCloner) Restore(snapPath, projectDir string) error {
	// Check if this is a git-based snapshot
	metaPath := filepath.Join(snapPath, ".yu-snapshot-meta")
	if meta, err := os.ReadFile(metaPath); err == nil {
		if strings.Contains(string(meta), "method=git") {
			return c.gitRestore(snapPath, projectDir)
		}
	}
	return c.cpRestore(snapPath, projectDir)
}

// --- git-based snapshot ---

func (c *platformCloner) gitClone(src, dst string) error {
	os.MkdirAll(dst, 0755)

	// Create a stash-like commit of ALL current changes (staged + unstaged + untracked)
	// git stash create includes staged and unstaged but NOT untracked
	// So we use: git add -A && git stash create && git reset
	// But we don't want to modify the index. Instead, save the diff.

	// 1. Save the current HEAD
	head, err := gitOutput(src, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("git rev-parse HEAD: %w", err)
	}

	// 2. Create a patch of all changes (tracked + untracked)
	// For tracked changes:
	diffTracked, _ := gitOutput(src, "diff", "HEAD")
	// For untracked files, list them
	untrackedList, _ := gitOutput(src, "ls-files", "--others", "--exclude-standard")

	// Save metadata
	os.WriteFile(filepath.Join(dst, "head"), []byte(head), 0600)
	os.WriteFile(filepath.Join(dst, "diff"), []byte(diffTracked), 0600)

	// Save untracked files
	if untrackedList != "" {
		untrackedDir := filepath.Join(dst, "untracked")
		os.MkdirAll(untrackedDir, 0755)
		for _, f := range strings.Split(strings.TrimSpace(untrackedList), "\n") {
			if f == "" {
				continue
			}
			srcFile := filepath.Join(src, f)
			dstFile := filepath.Join(untrackedDir, f)
			os.MkdirAll(filepath.Dir(dstFile), 0755)
			// Copy the file (these are small untracked files, real copy is fine)
			data, err := os.ReadFile(srcFile)
			if err != nil {
				continue
			}
			os.WriteFile(dstFile, data, 0644)
		}
	}

	// Mark as git-based snapshot
	appendMeta(dst, "method=git")

	return nil
}

func (c *platformCloner) gitRestore(snapPath, projectDir string) error {
	// Read saved HEAD
	headBytes, err := os.ReadFile(filepath.Join(snapPath, "head"))
	if err != nil {
		return fmt.Errorf("reading snapshot head: %w", err)
	}
	head := strings.TrimSpace(string(headBytes))

	// Reset to that HEAD
	if _, err := gitOutput(projectDir, "reset", "--hard", head); err != nil {
		return fmt.Errorf("git reset: %w", err)
	}

	// Apply the diff
	diffBytes, _ := os.ReadFile(filepath.Join(snapPath, "diff"))
	if len(diffBytes) > 0 {
		cmd := exec.Command("git", "apply", "--allow-empty")
		cmd.Dir = projectDir
		cmd.Stdin = strings.NewReader(string(diffBytes))
		cmd.CombinedOutput() // best effort
	}

	// Restore untracked files
	untrackedDir := filepath.Join(snapPath, "untracked")
	if info, err := os.Stat(untrackedDir); err == nil && info.IsDir() {
		filepath.Walk(untrackedDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(untrackedDir, path)
			dst := filepath.Join(projectDir, rel)
			os.MkdirAll(filepath.Dir(dst), 0755)
			data, _ := os.ReadFile(path)
			os.WriteFile(dst, data, 0644)
			return nil
		})
	}

	return nil
}

// --- cp-based snapshot (fallback for non-git) ---

func (c *platformCloner) cpClone(src, dst string) error {
	os.MkdirAll(dst, 0755)

	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("reading source dir: %w", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if skipClone[name] {
			continue
		}

		srcPath := filepath.Join(src, name)
		dstPath := filepath.Join(dst, name)

		cmd := exec.Command("cp", "-c", "-r", srcPath, dstPath)
		if _, err := cmd.CombinedOutput(); err != nil {
			// Fallback — but log it
			fmt.Fprintf(os.Stderr, "[yu] Warning: APFS clone failed for %s, using regular copy\n", name)
			cmd = exec.Command("cp", "-r", srcPath, dstPath)
			if out, err2 := cmd.CombinedOutput(); err2 != nil {
				return fmt.Errorf("copying %s: %s", name, string(out))
			}
		}
	}

	appendMeta(dst, "method=cp")
	return nil
}

func (c *platformCloner) cpRestore(snapPath, projectDir string) error {
	entries, err := os.ReadDir(snapPath)
	if err != nil {
		return fmt.Errorf("reading snapshot: %w", err)
	}

	snapFiles := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if name == ".yu-snapshot-meta" || name == "head" || name == "diff" || name == "untracked" {
			continue
		}
		snapFiles[name] = true
	}

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

	for _, entry := range entries {
		name := entry.Name()
		if name == ".yu-snapshot-meta" || name == "head" || name == "diff" || name == "untracked" {
			continue
		}
		if strings.HasPrefix(name, ".yu") {
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

// --- helpers ---

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func appendMeta(snapDir, line string) {
	metaPath := filepath.Join(snapDir, ".yu-snapshot-meta")
	f, err := os.OpenFile(metaPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(line + "\n")
}
