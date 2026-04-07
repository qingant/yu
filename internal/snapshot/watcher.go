package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
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
	fsWatcher    *fsnotify.Watcher
}

// NewWatcher creates a behavior-driven snapshot watcher.
func NewWatcher(s *Snapshotter, quietSeconds, fileThreshold int, logFn LogFunc) *Watcher {
	if logFn == nil {
		logFn = func(format string, args ...any) {}
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

	// Start native file watcher
	var err error
	w.fsWatcher, err = fsnotify.NewWatcher()
	if err != nil {
		w.logFn("Warning: file watcher failed: %v", err)
		return nil // non-fatal, snapshots still work via pre-command hooks
	}

	// Add project directory and all subdirectories (excluding .yu and .git)
	w.addDirRecursive(w.snapshotter.ProjectDir)

	w.wg.Add(1)
	go w.watchLoop()

	w.wg.Add(1)
	go w.quietChecker()

	return nil
}

// Stop terminates the watcher.
func (w *Watcher) Stop() {
	close(w.stopCh)
	if w.fsWatcher != nil {
		w.fsWatcher.Close()
	}
	w.wg.Wait()
}

// PreCommand is called before a proxied command executes.
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

	for {
		select {
		case <-w.stopCh:
			return
		case event, ok := <-w.fsWatcher.Events:
			if !ok {
				return
			}
			if w.shouldIgnore(event.Name) {
				continue
			}

			// If a new directory is created, watch it too
			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					w.addDirRecursive(event.Name)
				}
			}

			w.mu.Lock()
			w.changeCount++
			w.lastChange = time.Now()
			shouldSnap := w.changeCount >= w.fileThreshold
			w.mu.Unlock()

			if shouldSnap {
				w.takeSnapshot("threshold")
			}

		case err, ok := <-w.fsWatcher.Errors:
			if !ok {
				return
			}
			w.logFn("Warning: file watcher error: %v", err)
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

// shouldIgnore returns true for paths we don't want to trigger snapshots.
func (w *Watcher) shouldIgnore(path string) bool {
	rel, err := filepath.Rel(w.snapshotter.ProjectDir, path)
	if err != nil {
		return true
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	for _, p := range parts {
		if p == ".yu" || p == ".git" {
			return true
		}
	}
	return false
}

// addDirRecursive adds a directory and all subdirectories to the watcher.
func (w *Watcher) addDirRecursive(root string) {
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if name == ".yu" || name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			w.fsWatcher.Add(path)
		}
		return nil
	})
}
