package cmdproxy

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// Shim scripts call `yu shim <socket> <cmd> [args...]` — same binary, no separate yu-shim.
const shimScript = `#!/bin/sh
exec "%s" shim "%s" "%s" "$@"
`

// GenerateShims creates shim scripts for all intercepted commands.
// yuBin is the path to the yu binary itself.
func GenerateShims(shimsDir, socketPath, yuBin string, commands []string) error {
	os.MkdirAll(shimsDir, 0755)

	for _, cmd := range commands {
		shimPath := filepath.Join(shimsDir, cmd)
		content := fmt.Sprintf(shimScript, yuBin, socketPath, cmd)
		if err := os.WriteFile(shimPath, []byte(content), 0755); err != nil {
			return fmt.Errorf("writing shim for %s: %w", cmd, err)
		}
	}
	return nil
}

// RunShim is the entry point for `yu shim <socket> <cmd> [args...]`.
func RunShim(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: yu shim <socket-path> <command> [args...]\n")
		os.Exit(1)
	}

	socketPath := args[0]
	command := args[1]
	cmdArgs := args[2:]

	cwd, _ := os.Getwd()

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yu shim: cannot connect to daemon: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	req := Request{
		Command: command,
		Args:    cmdArgs,
		Cwd:     cwd,
	}
	if err := WriteMessage(conn, req); err != nil {
		fmt.Fprintf(os.Stderr, "yu shim: send error: %v\n", err)
		os.Exit(1)
	}

	for {
		var resp Response
		if err := ReadMessage(conn, &resp); err != nil {
			fmt.Fprintf(os.Stderr, "yu shim: read error: %v\n", err)
			os.Exit(1)
		}

		switch resp.Type {
		case "stdout":
			os.Stdout.Write(resp.Data)
		case "stderr":
			os.Stderr.Write(resp.Data)
		case "exit":
			os.Exit(resp.ExitCode)
		}
	}
}
