package cmdproxy

import (
	"encoding/binary"
	"encoding/json"
	"io"
)

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
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
