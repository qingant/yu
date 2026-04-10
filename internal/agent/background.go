package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// BgProcess represents a background process.
type BgProcess struct {
	ID        int
	Command   string
	Pid       int
	StartedAt time.Time
	StoppedAt time.Time
	ExitCode  int
	Running   bool
	LogFile   string // temp file for stdout+stderr

	cmd  *exec.Cmd
	done chan struct{}
}

// BgManager manages background processes.
type BgManager struct {
	mu        sync.Mutex
	processes map[int]*BgProcess
	nextID    int
	tmpDir    string
	projectDir string
	onExit    func(p *BgProcess) // called when process exits
}

// NewBgManager creates a new background process manager.
func NewBgManager(projectDir, tmpDir string, onExit func(p *BgProcess)) *BgManager {
	os.MkdirAll(tmpDir, 0700)
	return &BgManager{
		processes:  make(map[int]*BgProcess),
		nextID:     1,
		tmpDir:     tmpDir,
		projectDir: projectDir,
		onExit:     onExit,
	}
}

// Start launches a command in the background.
func (m *BgManager) Start(command string) (*BgProcess, error) {
	m.mu.Lock()
	id := m.nextID
	m.nextID++
	m.mu.Unlock()

	logFile := filepath.Join(m.tmpDir, fmt.Sprintf("bg-%d.log", id))
	f, err := os.Create(logFile)
	if err != nil {
		return nil, fmt.Errorf("creating log file: %w", err)
	}

	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = m.projectDir
	cmd.Stdout = f
	cmd.Stderr = f

	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, fmt.Errorf("starting command: %w", err)
	}

	p := &BgProcess{
		ID:        id,
		Command:   command,
		Pid:       cmd.Process.Pid,
		StartedAt: time.Now(),
		Running:   true,
		LogFile:   logFile,
		cmd:       cmd,
		done:      make(chan struct{}),
	}

	m.mu.Lock()
	m.processes[id] = p
	m.mu.Unlock()

	// Wait for exit in goroutine
	go func() {
		err := cmd.Wait()
		f.Close()

		p.Running = false
		p.StoppedAt = time.Now()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				p.ExitCode = exitErr.ExitCode()
			} else {
				p.ExitCode = -1
			}
		}
		close(p.done)

		if m.onExit != nil {
			m.onExit(p)
		}
	}()

	return p, nil
}

// Logs returns the last N lines of a process's output.
func (m *BgManager) Logs(id, tail int) (string, error) {
	m.mu.Lock()
	p, ok := m.processes[id]
	m.mu.Unlock()

	if !ok {
		return "", fmt.Errorf("no background process #%d", id)
	}

	data, err := os.ReadFile(p.LogFile)
	if err != nil {
		return "", err
	}

	content := string(data)
	if tail <= 0 {
		tail = 50
	}

	lines := strings.Split(content, "\n")
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return strings.Join(lines, "\n"), nil
}

// Stop kills a running process.
func (m *BgManager) Stop(id int) error {
	m.mu.Lock()
	p, ok := m.processes[id]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("no background process #%d", id)
	}
	if !p.Running {
		return fmt.Errorf("process #%d already stopped", id)
	}

	// Try SIGTERM first, then SIGKILL
	p.cmd.Process.Signal(os.Interrupt)

	select {
	case <-p.done:
		return nil
	case <-time.After(3 * time.Second):
		p.cmd.Process.Kill()
		<-p.done
		return nil
	}
}

// List returns all processes (running and recent stopped).
func (m *BgManager) List() []*BgProcess {
	m.mu.Lock()
	defer m.mu.Unlock()

	var result []*BgProcess
	for _, p := range m.processes {
		result = append(result, p)
	}
	return result
}

// RunningCount returns the number of currently running processes.
func (m *BgManager) RunningCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	count := 0
	for _, p := range m.processes {
		if p.Running {
			count++
		}
	}
	return count
}

// StopAll kills all running processes.
func (m *BgManager) StopAll() {
	m.mu.Lock()
	var running []*BgProcess
	for _, p := range m.processes {
		if p.Running {
			running = append(running, p)
		}
	}
	m.mu.Unlock()

	for _, p := range running {
		m.Stop(p.ID)
	}
}

// FormatStatus returns a short status string for a process.
func (p *BgProcess) FormatStatus() string {
	elapsed := time.Since(p.StartedAt)
	if p.Running {
		return fmt.Sprintf("#%-3d  %-30s  \033[32mrunning\033[0m  %s  pid %d",
			p.ID, truncCmd(p.Command, 30), formatDuration(elapsed), p.Pid)
	}
	code := fmt.Sprintf("exit %d", p.ExitCode)
	if p.ExitCode == 0 {
		code = "\033[32mexit 0\033[0m"
	} else {
		code = fmt.Sprintf("\033[31mexit %d\033[0m", p.ExitCode)
	}
	return fmt.Sprintf("#%-3d  %-30s  %s  ran %s",
		p.ID, truncCmd(p.Command, 30), code, formatDuration(p.StoppedAt.Sub(p.StartedAt)))
}

func truncCmd(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
