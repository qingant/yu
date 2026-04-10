package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WorkspaceDir returns the config directory for a given project path.
// Located at ~/.yu/workspaces/<slug>/ where slug is derived from the absolute path.
func WorkspaceDir(projectDir string) string {
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
// Uses a short hash + readable suffix for uniqueness + readability.
// e.g. /Users/tao/projects/foo → "foo-a1b2c3d4"
func slugify(path string) string {
	// Get the last component for readability
	base := filepath.Base(path)
	// Hash the full path for uniqueness
	h := sha256.Sum256([]byte(path))
	short := hex.EncodeToString(h[:4])
	// Clean up base name
	base = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, base)
	if base == "" {
		base = "root"
	}
	return base + "-" + short
}
