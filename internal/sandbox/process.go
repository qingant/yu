package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/taoai/yu/internal/cloud"
	"github.com/taoai/yu/internal/config"
	"github.com/taoai/yu/internal/fsjail"
)

// Default env var whitelist — only these pass into the sandbox.
// Everything else is stripped.
var defaultEnvWhitelist = []string{
	// System essentials
	"PATH",
	"HOME",
	"USER",
	"LOGNAME",
	"SHELL",
	"TERM",
	"TERM_PROGRAM",
	"COLORTERM",
	"LANG",
	"LC_",        // prefix: LC_ALL, LC_CTYPE, etc.
	"TMPDIR",
	"XDG_",       // prefix: XDG_RUNTIME_DIR, etc.
	"DISPLAY",
	"EDITOR",
	"VISUAL",
	"PAGER",
	"LESS",
	"TZ",

	// Build tools / runtimes
	"GOPATH",
	"GOROOT",
	"CARGO_HOME",
	"RUSTUP_HOME",
	"NVM_DIR",
	"PYENV_ROOT",
	"VOLTA_HOME",
	"JAVA_HOME",
	"SDKMAN_DIR",
	"ASDF_",      // prefix

	// Agent config (non-secret) — prefixes pass through base URLs, org IDs, etc.
	// Secret vars (containing KEY, TOKEN, SECRET, PASSWORD) are intercepted
	// and replaced with dummies — real values injected by the network proxy.
	"ANTHROPIC_",  // prefix
	"OPENAI_",     // prefix
	"GOOGLE_",     // prefix
	"GEMINI_",     // prefix
	"CLAUDE_",     // prefix
	"CODEX_",      // prefix

	// Proxy (pass through if user has their own proxy setup)
	"HTTP_PROXY",
	"HTTPS_PROXY",
	"http_proxy",
	"https_proxy",
	"NO_PROXY",
	"no_proxy",

	// Yu internal
	"YU_",        // prefix
}

// secretPatterns — env var names containing any of these substrings
// are credentials. They get replaced with a dummy value in the sandbox;
// the real value is used by the network proxy for header injection.
var secretPatterns = []string{
	"KEY",
	"TOKEN",
	"SECRET",
	"PASSWORD",
	"CREDENTIAL",
}

// isSecret returns true if the env var name looks like a credential.
func isSecret(key string) bool {
	upper := strings.ToUpper(key)
	for _, pat := range secretPatterns {
		if strings.Contains(upper, pat) {
			return true
		}
	}
	return false
}

// isWhitelisted checks if an env var key is allowed through.
func isWhitelisted(key string, extraKeep []string) bool {
	for _, pattern := range defaultEnvWhitelist {
		if strings.HasSuffix(pattern, "_") {
			if strings.HasPrefix(key, pattern) {
				return true
			}
		} else if key == pattern {
			return true
		}
	}
	for _, k := range extraKeep {
		if key == k {
			return true
		}
	}
	return false
}

// launch executes the command inside the sandbox with filesystem isolation.
func (s *Sandbox) launch() (int, error) {
	command := s.Command

	// Apply filesystem jail on supported platforms
	if runtime.GOOS == "darwin" {
		// Collect agent config dirs that need to be accessible
		var allowPaths []string
		realHome, _ := os.UserHomeDir()
		if info := s.agentInfo(); info != nil {
			for _, dir := range info.ConfigDirs {
				allowPaths = append(allowPaths, filepath.Join(realHome, dir))
			}
		}

		// Allow ~/.local (user-installed binaries, libraries, support files)
		// Allow ~/.claude (Claude Code config, sessions, history)
		allowPaths = append(allowPaths, filepath.Join(realHome, ".local"))
		allowPaths = append(allowPaths, filepath.Join(realHome, ".claude"))

		// Resolve command to absolute path so sandbox doesn't need PATH lookup
		if binPath, err := exec.LookPath(command[0]); err == nil {
			if resolved, err := filepath.EvalSymlinks(binPath); err == nil {
				command[0] = resolved
			} else {
				command[0], _ = filepath.Abs(binPath)
			}
		}

		profile := fsjail.Profile{
			ProjectDir:   s.ProjectDir,
			TmpDir:       s.TmpDir,
			WorkspaceDir: config.WorkspaceDir(s.ProjectDir),
			AllowPaths:   allowPaths,
			DenyPaths:    fsjail.DefaultDenyPaths(),
		}
		gen := &fsjail.DarwinGenerator{}
		profilePath, err := gen.Generate(profile)
		if err != nil {
			return 1, fmt.Errorf("generating sandbox profile: %w", err)
		}
		bin, args := gen.WrapCommand(profilePath, command)
		command = append([]string{bin}, args...)
	}

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = s.ProjectDir
	cmd.Env = s.buildEnv()

	if s.cloudSession != nil {
		// Relay mode: pipe stdout/stderr through cloud, accept input from cloud
		return s.launchWithRelay(cmd)
	}

	// Local mode: direct stdin/stdout
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return 1, fmt.Errorf("running command: %w", err)
	}
	return 0, nil
}

// launchWithRelay runs the command with stdin/stdout relayed to both
// the local terminal AND the cloud session.
func (s *Sandbox) launchWithRelay(cmd *exec.Cmd) (int, error) {
	// Stdout: tee to local terminal + cloud
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return 1, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return 1, err
	}

	// Stdin: merge from local terminal + cloud
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return 1, err
	}

	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("starting command: %w", err)
	}

	// Relay stdout: read from agent, write to terminal + cloud
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				data := string(buf[:n])
				os.Stdout.WriteString(data)
				s.cloudSession.SendOutput(data)
			}
			if err != nil {
				return
			}
		}
	}()

	// Relay stderr: same treatment
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				data := string(buf[:n])
				os.Stderr.WriteString(data)
				s.cloudSession.SendOutput(data)
			}
			if err != nil {
				return
			}
		}
	}()

	// Read from local stdin → agent
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				stdinPipe.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// Read from cloud → agent stdin
	go func() {
		for {
			msg, err := s.cloudSession.Receive()
			if err != nil {
				return
			}
			if msg.Type == "input" {
				stdinPipe.Write([]byte(msg.Data + "\n"))
			}
		}
	}()

	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return 1, fmt.Errorf("running command: %w", err)
		}
	}

	s.cloudSession.Send(cloud.Message{Type: "session_end"})
	return exitCode, nil
}

// buildEnv creates the environment for the sandboxed process.
// Whitelist only — starts empty, adds only allowed vars.
// Secret vars (API keys, tokens) are replaced with dummies.
func (s *Sandbox) buildEnv() []string {
	var env []string
	seen := make(map[string]bool)

	extraKeep := s.Config.Env.Keep
	for _, e := range os.Environ() {
		eqIdx := strings.IndexByte(e, '=')
		if eqIdx < 0 {
			continue
		}
		key := e[:eqIdx]
		if !isWhitelisted(key, extraKeep) {
			continue
		}
		// If this key has a proxy dummy, use that
		if val, ok := s.dummyKeys[key]; ok {
			env = append(env, key+"="+val)
		} else if isSecret(key) {
			// Secret without a proxy route — redact to prevent leaking
			env = append(env, key+"=yu-redacted")
		} else {
			env = append(env, e)
		}
		seen[key] = true
	}

	// Inject dummyKeys entries that weren't in the original env.
	// This covers BASE_URLs that the user never set — we still need
	// to point the agent at the local API proxy.
	for key, val := range s.dummyKeys {
		if !seen[key] {
			env = append(env, key+"="+val)
		}
	}

	// Override HOME (persistent) and TMPDIR (ephemeral)
	env = setEnv(env, "HOME", filepath.Join(config.WorkspaceDir(s.ProjectDir), "home"))
	env = setEnv(env, "TMPDIR", filepath.Join(s.TmpDir, "tmp"))

	// Override PATH to include shims first
	shimsDir := filepath.Join(s.TmpDir, "shims")
	currentPath := os.Getenv("PATH")
	env = setEnv(env, "PATH", shimsDir+":"+currentPath)



	// Yu markers
	env = setEnv(env, "YU_SANDBOX", "1")
	env = setEnv(env, "YU_SANDBOX_ID", s.ID)
	env = setEnv(env, "YU_PROJECT_DIR", s.ProjectDir)
	env = setEnv(env, "YU_WORKSPACE_DIR", config.WorkspaceDir(s.ProjectDir))
	if s.apiProxy != nil {
		env = setEnv(env, "YU_PROXY_SECRET", s.apiProxy.Secret)
	}
	env = setEnv(env, "YU_LOG_FILE", filepath.Join(s.TmpDir, "yu.log"))

	return env
}

// setEnv sets or replaces an env var in the list.
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
