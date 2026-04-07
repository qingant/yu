package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// logger writes yu logs to a file instead of stdout, so agent output is clean.
type logger struct {
	mu   sync.Mutex
	file *os.File
	path string
}

var log *logger

func initLog(tmpDir string) {
	path := filepath.Join(tmpDir, "yu.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		// Fallback: log to stderr
		log = &logger{file: os.Stderr, path: "stderr"}
		return
	}
	log = &logger{file: f, path: path}

	// Print the log path once to stderr so user knows where to find it
	fmt.Fprintf(os.Stderr, "[yu] Log: %s\n", path)
}

func closeLog() {
	if log != nil && log.file != os.Stderr {
		log.file.Close()
	}
}

func yuLog(format string, args ...any) {
	if log == nil {
		return
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	ts := time.Now().Format("15:04:05")
	fmt.Fprintf(log.file, "[%s] %s\n", ts, fmt.Sprintf(format, args...))
}
