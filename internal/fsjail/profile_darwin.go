package fsjail

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DarwinGenerator generates macOS sandbox-exec profiles.
type DarwinGenerator struct{}

// DefaultDenyPaths returns paths that should always be denied.
func DefaultDenyPaths() []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".aws"),
		filepath.Join(home, ".gnupg"),
		filepath.Join(home, ".config", "gh"),
		filepath.Join(home, ".config", "git", "credentials"),
		filepath.Join(home, ".netrc"),
		filepath.Join(home, ".npmrc"),
		filepath.Join(home, ".pypirc"),
		filepath.Join(home, ".docker", "config.json"),
	}
}

// Generate creates a sandbox-exec profile file.
func (d *DarwinGenerator) Generate(p Profile) (string, error) {
	var sb strings.Builder

	sb.WriteString("(version 1)\n")
	sb.WriteString("(allow default)\n\n")

	// Deny credential paths
	sb.WriteString("; Credential directories — agent cannot access these\n")
	for _, path := range p.DenyPaths {
		// Check if it's a file or directory to use the right filter
		info, err := os.Stat(path)
		if err != nil {
			// Path doesn't exist, deny as subpath anyway (defensive)
			sb.WriteString(fmt.Sprintf("(deny file-read* (subpath %q))\n", path))
			sb.WriteString(fmt.Sprintf("(deny file-write* (subpath %q))\n", path))
			continue
		}
		if info.IsDir() {
			sb.WriteString(fmt.Sprintf("(deny file-read* (subpath %q))\n", path))
			sb.WriteString(fmt.Sprintf("(deny file-write* (subpath %q))\n", path))
		} else {
			sb.WriteString(fmt.Sprintf("(deny file-read* (literal %q))\n", path))
			sb.WriteString(fmt.Sprintf("(deny file-write* (literal %q))\n", path))
		}
	}

	// Write profile to temp file
	profilePath := filepath.Join(p.TmpDir, "sandbox.sb")
	if err := os.WriteFile(profilePath, []byte(sb.String()), 0600); err != nil {
		return "", fmt.Errorf("writing sandbox profile: %w", err)
	}
	return profilePath, nil
}

// WrapCommand wraps a command to run under sandbox-exec.
func (d *DarwinGenerator) WrapCommand(profilePath string, command []string) (string, []string) {
	args := []string{"-f", profilePath}
	args = append(args, command...)
	return "/usr/bin/sandbox-exec", args
}
