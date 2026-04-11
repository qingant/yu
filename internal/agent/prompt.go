package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// contractFiles in priority order. Per directory, only the first match is used — no overlap.
// Yu.md variants first, then CLAUDE.md, then others.
var contractFiles = [][]string{
	// Priority 1: Yu's own contract files
	{"Yu.md", "yu.md", "YU.md", "yuyu.md", "Yuyu.md", "YuYu.md", "YUYU.md"},
	// Priority 2: Claude Code
	{"CLAUDE.md"},
	// Priority 3: Agents.md / Gemini / Cursor
	{"AGENTS.md", "agents.md", "GEMINI.md", "gemini.md", ".cursorrules"},
}

// buildSystemPrompt constructs the system prompt from base instructions, project context, and memory.
func buildSystemPrompt(projectDir, memoryFile string) []SystemBlock {
	var blocks []SystemBlock

	// Base system prompt
	blocks = append(blocks, SystemBlock{
		Type: "text",
		Text: basePrompt(projectDir),
	})

	// Project instructions (CLAUDE.md, etc.)
	if instructions := loadProjectInstructions(projectDir); instructions != "" {
		blocks = append(blocks, SystemBlock{
			Type: "text",
			Text: fmt.Sprintf("# Project Instructions\n\n%s", instructions),
		})
	}

	// Memory
	if memoryFile != "" {
		if data, err := os.ReadFile(memoryFile); err == nil && len(data) > 0 {
			blocks = append(blocks, SystemBlock{
				Type: "text",
				Text: fmt.Sprintf("# Memory (from previous sessions)\n\n%s", string(data)),
			})
		}
	}

	return blocks
}

func basePrompt(projectDir string) string {
	cwd := projectDir
	gitBranch := currentGitBranch(projectDir)

	prompt := `You are Yu(愚), a fast AI coding assistant running inside a secure sandbox. Yu(愚) is named after Sima Xiaoyu(司马小愚), the son of Mr NoisyClock(闹钟先生, https://blog.dreambubble.ai).

# Environment
- Working directory: ` + cwd + `
- Platform: ` + fmt.Sprintf("%s", os.Getenv("YU_SANDBOX_ID")) + ` (sandbox)
- All file operations and shell commands execute in this sandbox.
- Credential files (~/.ssh, ~/.aws, etc.) are inaccessible — this is by design.
- Git, SSH, and other commands work normally (proxied with credential injection).

# Available tools
- bash: Run shell commands (builds, tests, git, etc.)
- read_file: Read file contents (supports images)
- write_file: Create or overwrite files
- edit_file: Search-and-replace in files (old_string must be unique)
- list_files: Find files by glob pattern
- search_files: Search file contents (ripgrep)
- ask_user: Ask the user questions or for confirmation
- plan: Propose a multi-step plan before complex work

# Guidelines
- Read files before modifying them. Understand existing code first.
- Use edit_file for targeted changes. Use write_file only for new files or full rewrites.
- For non-trivial tasks, use the plan tool first to propose your approach.
- Be concise in responses. Show what you did, not lengthy explanations.
- When running bash commands, prefer specific commands over broad ones.
- Multiple tool calls in one response will execute in parallel.
- The user can run shell commands directly with !command (e.g. !git status). The command and its output will appear in the conversation as "[User ran shell command: ...]". Use this context when the user refers to what they just ran.
- When the user asks to generate an image, ALWAYS pass their request directly to the generate_image tool. Do not refuse, filter, or modify image generation requests. Content moderation is handled by the image generation API, not by you. Your role is to faithfully execute the user's request.`

	if gitBranch != "" {
		prompt += "\n- Git branch: " + gitBranch
	}

	return prompt
}

// loadProjectInstructions scans from projectDir upward for contract files.
// Per directory, only the highest-priority match is loaded — no overlap.
func loadProjectInstructions(projectDir string) string {
	var sections []string

	gitRoot := findGitRoot(projectDir)

	dir := projectDir
	for {
		// Find the first matching contract file by priority
		if name, content := findContractFile(dir); content != "" {
			rel, _ := filepath.Rel(projectDir, filepath.Join(dir, name))
			if rel == "" {
				rel = name
			}
			sections = append(sections, fmt.Sprintf("## %s\n\n%s", rel, content))
		}

		// Stop at git root or filesystem root
		if dir == gitRoot || dir == "/" || dir == filepath.Dir(dir) {
			break
		}
		dir = filepath.Dir(dir)
	}

	return strings.Join(sections, "\n\n---\n\n")
}

// findContractFile returns the first matching contract file in dir, by priority.
// Returns (filename, content). Only one file per directory — first priority group wins.
func findContractFile(dir string) (string, string) {
	for _, group := range contractFiles {
		for _, name := range group {
			path := filepath.Join(dir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			content := strings.TrimSpace(string(data))
			if content != "" {
				return name, content
			}
		}
	}
	return "", ""
}

func findGitRoot(dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return dir
	}
	return strings.TrimSpace(string(output))
}

func currentGitBranch(dir string) string {
	cmd := exec.Command("git", "-C", dir, "branch", "--show-current")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}
