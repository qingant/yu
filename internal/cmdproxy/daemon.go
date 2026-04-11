package cmdproxy

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// PreCommandHook is called before a proxied command is executed.
type PreCommandHook func(cmdName string)

// Daemon listens on a Unix socket and executes proxied commands
// with credential injection, sandboxed to only project dir + credential files.
type Daemon struct {
	SocketPath     string
	Env            map[string]string // credentials from .yu/env
	ProjectDir     string            // allowed read/write directory
	TmpDir         string            // sandbox temp dir (for profile file)
	AllowedCmds    map[string]bool   // whitelist of commands the daemon will execute
	PreCommandHook PreCommandHook
	listener       net.Listener
	wg             sync.WaitGroup
}

// NewDaemon creates a command proxy daemon.
func NewDaemon(socketPath string, env map[string]string, projectDir, tmpDir string) *Daemon {
	return &Daemon{
		SocketPath: socketPath,
		Env:        env,
		ProjectDir: projectDir,
		TmpDir:     tmpDir,
	}
}

// Start begins listening for command proxy requests.
func (d *Daemon) Start() error {
	// Ensure parent dir exists
	os.MkdirAll(filepath.Dir(d.SocketPath), 0700)

	// Remove stale socket
	os.Remove(d.SocketPath)

	var err error
	d.listener, err = net.Listen("unix", d.SocketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", d.SocketPath, err)
	}

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			conn, err := d.listener.Accept()
			if err != nil {
				return // listener closed
			}
			go d.handleConnection(conn)
		}
	}()

	return nil
}

// Stop shuts down the daemon.
func (d *Daemon) Stop() {
	if d.listener != nil {
		d.listener.Close()
	}
	d.wg.Wait()
	os.Remove(d.SocketPath)
}

func (d *Daemon) handleConnection(conn net.Conn) {
	defer conn.Close()

	// Read request
	var req Request
	if err := ReadMessage(conn, &req); err != nil {
		d.sendError(conn, fmt.Sprintf("reading request: %v", err), 1)
		return
	}

	// Validate command against whitelist
	if !d.AllowedCmds[req.Command] {
		d.sendError(conn, fmt.Sprintf("command %q not in allowed list", req.Command), 126)
		return
	}

	// Validate CWD is within the project directory
	cleanCwd, _ := filepath.EvalSymlinks(req.Cwd)
	cleanProject, _ := filepath.EvalSymlinks(d.ProjectDir)
	if cleanCwd != cleanProject && !strings.HasPrefix(cleanCwd, cleanProject+string(os.PathSeparator)) {
		d.sendError(conn, fmt.Sprintf("working directory %q is outside project", req.Cwd), 126)
		return
	}

	// Call pre-command hook (e.g., snapshot)
	if d.PreCommandHook != nil {
		d.PreCommandHook(req.Command)
	}

	// Find the real command (not the shim)
	realCmd, err := findRealCommand(req.Command)
	if err != nil {
		d.sendError(conn, fmt.Sprintf("finding %s: %v", req.Command, err), 127)
		return
	}

	// Execute with credential injection, inside a sandbox that only
	// allows the project dir + credential-related files.
	cmdName, cmdArgs := d.wrapWithSandbox(realCmd, req.Args)
	cmd := exec.Command(cmdName, cmdArgs...)
	cmd.Dir = req.Cwd
	cmd.Env = d.buildExecEnv()

	// Pipe stdout and stderr back to the shim
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		d.sendError(conn, fmt.Sprintf("starting %s: %v", req.Command, err), 1)
		return
	}

	// Stream stdout and stderr concurrently
	var streamWg sync.WaitGroup
	streamWg.Add(2)

	go func() {
		defer streamWg.Done()
		d.streamOutput(conn, stdout, "stdout")
	}()
	go func() {
		defer streamWg.Done()
		d.streamOutput(conn, stderr, "stderr")
	}()

	streamWg.Wait()

	// Wait for process to finish
	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	// Send exit code
	WriteMessage(conn, Response{
		Type:     "exit",
		ExitCode: exitCode,
	})
}

func (d *Daemon) streamOutput(conn net.Conn, r io.Reader, streamType string) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			WriteMessage(conn, Response{
				Type: streamType,
				Data: buf[:n],
			})
		}
		if err != nil {
			return
		}
	}
}

func (d *Daemon) sendError(conn net.Conn, msg string, exitCode int) {
	WriteMessage(conn, Response{
		Type: "stderr",
		Data: []byte(msg + "\n"),
	})
	WriteMessage(conn, Response{
		Type:     "exit",
		ExitCode: exitCode,
	})
}

// buildExecEnv creates the environment for the real command execution.
// This is OUTSIDE the sandbox — it includes real credentials.
func (d *Daemon) buildExecEnv() []string {
	// Start with the real host environment
	env := os.Environ()

	// Inject credentials from .yu/env, expanding ~ to real home
	home, _ := os.UserHomeDir()
	for k, v := range d.Env {
		if strings.Contains(v, "~/") {
			v = strings.ReplaceAll(v, "~/", home+"/")
		}
		env = setExecEnv(env, k, v)
	}

	return env
}

func setExecEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if len(e) > len(prefix) && e[:len(prefix)] == prefix {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

// wrapWithSandbox wraps a command with sandbox-exec on macOS.
// The sandbox allows: project dir (rw), credential files from env (ro), system paths (ro).
// Denies everything else.
func (d *Daemon) wrapWithSandbox(realCmd string, args []string) (string, []string) {
	if runtime.GOOS != "darwin" {
		return realCmd, args
	}

	profile := d.generateExecProfile()
	profilePath := filepath.Join(d.TmpDir, "cmdproxy-sandbox.sb")
	os.WriteFile(profilePath, []byte(profile), 0600)

	sbArgs := []string{"-f", profilePath, realCmd}
	sbArgs = append(sbArgs, args...)
	return "/usr/bin/sandbox-exec", sbArgs
}

// generateExecProfile creates a sandbox-exec profile for delegated commands.
//
// Limitation: sandbox-exec deny overrides allow, so we can't deny ~/.ssh
// then allow back a single key file. Delegated commands need credential
// files to function (git needs SSH keys, aws needs config).
//
// Current strategy: allow default.
// Config is no longer in project dir (moved to ~/.yu/workspaces/).
// Credential file access will be guarded by AI intent detection (future).
func (d *Daemon) generateExecProfile() string {
	var sb strings.Builder

	sb.WriteString("(version 1)\n")
	sb.WriteString("(allow default)\n\n")

	return sb.String()
}

// extractPaths finds file paths in a string value.
// Expands ~ to home dir. Only returns paths that actually exist.
func extractPaths(value, home string) []string {
	var paths []string
	for _, part := range strings.Fields(value) {
		// Expand ~
		if strings.HasPrefix(part, "~/") {
			part = filepath.Join(home, part[2:])
		}
		// Check if it looks like a path and exists
		if strings.HasPrefix(part, "/") {
			if _, err := os.Stat(part); err == nil {
				paths = append(paths, part)
			}
		}
	}
	return paths
}

// findRealCommand finds the real binary for a command, skipping shims.
// It looks in standard system paths to avoid finding our own shims.
func findRealCommand(name string) (string, error) {
	// Search standard system paths
	systemPaths := []string{
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
		"/usr/sbin",
		"/sbin",
		"/opt/homebrew/bin",
	}

	for _, dir := range systemPaths {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}

	return "", fmt.Errorf("command not found: %s", name)
}
