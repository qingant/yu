package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WorkspaceDir returns the config directory for a given project path.
// Inside a sandbox, HOME is fake — use YU_WORKSPACE_DIR env var if set.
// Otherwise: ~/.yu/workspaces/<slug>/
func WorkspaceDir(projectDir string) string {
	if wsDir := os.Getenv("YU_WORKSPACE_DIR"); wsDir != "" {
		return wsDir
	}
	home, _ := os.UserHomeDir()
	slug := slugify(projectDir)
	return filepath.Join(home, ".yu", "workspaces", slug)
}

// MigrateIfNeeded silently moves .yu/ from project dir to ~/.yu/workspaces/<slug>/
// if the old location exists and the new one doesn't.
func MigrateIfNeeded(projectDir string) {
	oldDir := filepath.Join(projectDir, ".yu")
	newDir := WorkspaceDir(projectDir)

	// Old exists, new doesn't → migrate
	if info, err := os.Stat(oldDir); err == nil && info.IsDir() {
		if _, err := os.Stat(newDir); os.IsNotExist(err) {
			os.MkdirAll(filepath.Dir(newDir), 0700)
			if err := os.Rename(oldDir, newDir); err != nil {
				// Rename might fail across filesystems, try copy
				fmt.Fprintf(os.Stderr, "[yu] Warning: could not migrate .yu/ to %s: %v\n", newDir, err)
				return
			}
			fmt.Fprintf(os.Stderr, "[yu] Migrated config from .yu/ to %s\n", newDir)
		}
	}
}

// slugify turns an absolute path into a filesystem-safe directory name.
// Full path, lowercase, non-alphanumeric replaced with -.
// e.g. /Users/tao/projects/foo → "users-tao-projects-foo"
func slugify(path string) string {
	s := strings.ToLower(path)
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, s)
	// Trim leading/trailing dashes
	s = strings.Trim(s, "-")
	// Collapse multiple dashes
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	if s == "" {
		s = "root"
	}
	return s
}
