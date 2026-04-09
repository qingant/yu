package cmdproxy

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// maxMessageSize caps how many bytes ReadMessage will allocate for a single
// message, preventing a malicious (or buggy) sender from triggering a ~4 GiB
// allocation via the 4-byte length prefix.
const maxMessageSize = 64 * 1024 * 1024 // 64 MiB

// Request is sent from shim to daemon.
type Request struct {
	Command string   `json:"cmd"`
	Args    []string `json:"args"`
	Cwd     string   `json:"cwd"`
}

// Response is streamed from daemon to shim.
// Multiple response frames may be sent (for stdout/stderr streaming).
type Response struct {
	Type     string `json:"type"` // "stdout", "stderr", "exit"
	Data     []byte `json:"data,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

// WriteMessage sends a length-prefixed JSON message.
func WriteMessage(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// Write 4-byte big-endian length prefix
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))
	if _, err := w.Write(lenBuf); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// ReadMessage reads a length-prefixed JSON message.
func ReadMessage(r io.Reader, v any) error {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(r, lenBuf); err != nil {
		return err
	}
	length := binary.BigEndian.Uint32(lenBuf)
	if length > maxMessageSize {
		return fmt.Errorf("message too large: %d bytes (max %d)", length, maxMessageSize)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
