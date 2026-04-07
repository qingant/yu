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
		log = &logger{file: os.Stderr, path: "stderr"}
		fmt.Fprintf(os.Stderr, "[yu] Log: stderr (failed to create %s: %v)\n", path, err)
		return
	}
	log = &logger{file: f, path: path}
	fmt.Fprintf(os.Stderr, "[yu] Log: %s\n", path)
}

// yuLogStderr writes to both log file and stderr — for critical messages.
func yuLogStderr(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	yuLog("%s", msg)
	fmt.Fprintf(os.Stderr, "[yu] %s\n", msg)
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
