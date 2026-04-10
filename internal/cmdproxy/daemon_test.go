package cmdproxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateExecProfile(t *testing.T) {
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "project")
	os.MkdirAll(projectDir, 0755)
	keyFile := filepath.Join(tmpDir, "test-key")
	os.WriteFile(keyFile, []byte("fake"), 0600)

	d := &Daemon{
		ProjectDir: projectDir,
		TmpDir:     tmpDir,
		Env: map[string]string{
			"GIT_SSH_COMMAND": "ssh -i " + keyFile + " -o IdentitiesOnly=yes",
			"GH_TOKEN":       "ghp_xxxxx",
		},
	}

	profile := d.generateExecProfile()

	// Should have allow default
	if !strings.Contains(profile, "(allow default)") {
		t.Error("profile should have allow default")
	}

	// Config is now in ~/.yu/workspaces/, not in project dir
	// So no .yu deny rule needed in profile

	t.Logf("Generated profile:\n%s", profile)
}

func TestExtractPaths(t *testing.T) {
	home, _ := os.UserHomeDir()

	paths := extractPaths("ssh -i ~/.ssh/id_ed25519 -o IdentitiesOnly=yes", home)

	// Should find the SSH key path (if it exists on this machine)
	sshKey := home + "/.ssh/id_ed25519"
	if _, err := os.Stat(sshKey); err == nil {
		found := false
		for _, p := range paths {
			if p == sshKey {
				found = true
			}
		}
		if !found {
			t.Errorf("expected to find %s in extracted paths: %v", sshKey, paths)
		}
	}

	// Should not extract non-path arguments
	paths2 := extractPaths("ghp_xxxxx", home)
	if len(paths2) != 0 {
		t.Errorf("expected no paths from token string, got: %v", paths2)
	}
}
