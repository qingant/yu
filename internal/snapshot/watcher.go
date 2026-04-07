package snapshot

import (
	"os/exec"
	"runtime"
	"sync"
	"time"
)

// LogFunc is a callback for logging.
type LogFunc func(format string, args ...any)

// Watcher monitors file changes and triggers snapshots based on behavior.
type Watcher struct {
	snapshotter   *Snapshotter
	quietSeconds  int
	fileThreshold int
	logFn         LogFunc

	mu           sync.Mutex
	changeCount  int
	lastChange   time.Time
	lastSnapshot time.Time
	stopCh       chan struct{}
	wg           sync.WaitGroup
	watchCmd     *exec.Cmd
}

// NewWatcher creates a behavior-driven snapshot watcher.
func NewWatcher(s *Snapshotter, quietSeconds, fileThreshold int, logFn LogFunc) *Watcher {
	if logFn == nil {
		logFn = func(format string, args ...any) {} // noop
	}
	return &Watcher{
		snapshotter:   s,
		quietSeconds:  quietSeconds,
		fileThreshold: fileThreshold,
		logFn:         logFn,
		stopCh:        make(chan struct{}),
	}
}

// Start begins watching for file changes.
func (w *Watcher) Start() error {
	// Take initial snapshot
	if snap, err := w.snapshotter.Create("init"); err != nil {
		w.logFn("Warning: initial snapshot failed: %v", err)
	} else {
		w.logFn("Snapshot #%d (init)", snap.ID)
		w.lastSnapshot = time.Now()
	}

	// Start fswatch/inotify process
	w.wg.Add(1)
	go w.watchLoop()

	// Start quiet-period checker
	w.wg.Add(1)
	go w.quietChecker()

	return nil
}

// Stop terminates the watcher.
func (w *Watcher) Stop() {
	close(w.stopCh)
	// Kill the file watcher process so its stdout.Read unblocks
	w.mu.Lock()
	if w.watchCmd != nil && w.watchCmd.Process != nil {
		w.watchCmd.Process.Kill()
	}
	w.mu.Unlock()
	w.wg.Wait()
}

// PreCommand is called before a proxied command executes.
// It triggers a snapshot if there have been changes since the last one.
func (w *Watcher) PreCommand(cmdName string) {
	w.mu.Lock()
	changes := w.changeCount
	w.mu.Unlock()

	if changes > 0 {
		w.takeSnapshot("pre-command:" + cmdName)
	}
}

func (w *Watcher) watchLoop() {
	defer w.wg.Done()

	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("fswatch",
			"--recursive",
			"--exclude", `\.yu/`,
			"--exclude", `\.git/`,
			w.snapshotter.ProjectDir,
		)
	} else {
		cmd = exec.Command("inotifywait",
			"-m", "-r",
			"--exclude", `(\.yu|\.git)`,
			"-e", "modify,create,delete,move",
			"--format", "%w%f",
			w.snapshotter.ProjectDir,
		)
	}

	w.mu.Lock()
	w.watchCmd = cmd
	w.mu.Unlock()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		w.logFn("Warning: file watcher pipe failed: %v", err)
		return
	}

	if err := cmd.Start(); err != nil {
		w.logFn("Warning: file watcher not available: %v", err)
		return
	}

	// Read events — blocks until data or process killed
	buf := make([]byte, 4096)
	for {
		n, err := stdout.Read(buf)
		if err != nil {
			return // process killed or pipe closed
		}
		if n > 0 {
			w.mu.Lock()
			w.changeCount++
			w.lastChange = time.Now()
			shouldSnap := w.changeCount >= w.fileThreshold
			w.mu.Unlock()

			if shouldSnap {
				w.takeSnapshot("threshold")
			}
		}
	}
}

func (w *Watcher) quietChecker() {
	defer w.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.mu.Lock()
			changes := w.changeCount
			lastChange := w.lastChange
			w.mu.Unlock()

			if changes > 0 && time.Since(lastChange) >= time.Duration(w.quietSeconds)*time.Second {
				w.takeSnapshot("quiet")
			}
		}
	}
}

func (w *Watcher) takeSnapshot(trigger string) {
	w.mu.Lock()
	w.changeCount = 0
	w.mu.Unlock()

	snap, err := w.snapshotter.Create(trigger)
	if err != nil {
		w.logFn("Warning: snapshot failed (%s): %v", trigger, err)
		return
	}
	w.mu.Lock()
	w.lastSnapshot = time.Now()
	w.mu.Unlock()
	w.logFn("Snapshot #%d (%s)", snap.ID, trigger)
}
