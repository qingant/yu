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
// Strategy: allow default → deny home dir → re-allow project/tmp/workspace.
// In sandbox-exec, later rules take precedence over earlier ones.
func (d *DarwinGenerator) Generate(p Profile) (string, error) {
	home, _ := os.UserHomeDir()
	var sb strings.Builder

	sb.WriteString("(version 1)\n")
	sb.WriteString("(allow default)\n\n")

	// Deny the entire home directory — agent shouldn't read user files
	sb.WriteString("; Deny home directory\n")
	sb.WriteString(fmt.Sprintf("(deny file-read* (subpath %q))\n", home))
	sb.WriteString(fmt.Sprintf("(deny file-write* (subpath %q))\n\n", home))

	// Re-allow project directory (read-write)
	sb.WriteString("; Allow project directory\n")
	sb.WriteString(fmt.Sprintf("(allow file-read* (subpath %q))\n", p.ProjectDir))
	sb.WriteString(fmt.Sprintf("(allow file-write* (subpath %q))\n\n", p.ProjectDir))

	// Re-allow sandbox tmp directory (read-write)
	sb.WriteString("; Allow sandbox tmp\n")
	sb.WriteString(fmt.Sprintf("(allow file-read* (subpath %q))\n", p.TmpDir))
	sb.WriteString(fmt.Sprintf("(allow file-write* (subpath %q))\n\n", p.TmpDir))

	// Re-allow only this project's workspace dir (not all of ~/.yu/)
	if p.WorkspaceDir != "" {
		sb.WriteString("; Allow this project's workspace\n")
		sb.WriteString(fmt.Sprintf("(allow file-read* (subpath %q))\n", p.WorkspaceDir))
		sb.WriteString(fmt.Sprintf("(allow file-write* (subpath %q))\n\n", p.WorkspaceDir))
	}

	// Re-allow agent config directories that were symlinked into sandbox HOME
	// (e.g. ~/.claude, ~/.codex) — these are needed by external agents
	for _, allowPath := range p.AllowPaths {
		sb.WriteString(fmt.Sprintf("(allow file-read* (subpath %q))\n", allowPath))
	}

	// Still explicitly deny credential paths (belt + suspenders)
	sb.WriteString("\n; Credential directories — always denied\n")
	for _, path := range p.DenyPaths {
		info, err := os.Stat(path)
		if err != nil {
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
