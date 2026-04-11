package agent

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
	"time"
)

// Bracketed paste escape sequences.
// When enabled, the terminal wraps pasted text in these markers.
var (
	pasteStart = []byte("\033[200~")
	pasteEnd   = []byte("\033[201~")
	// Enable/disable bracketed paste mode
	pasteEnable  = []byte("\033[?2004h")
	pasteDisable = []byte("\033[?2004l")
)

// pasteNewline is a Unicode placeholder (U+2028 LINE SEPARATOR, UTF-8: E2 80 A8)
// used to replace \n inside pasted text so readline doesn't treat it as Enter.
const pasteNewline = '\u2028'

// pasteStdin wraps the real stdin to support bracketed paste.
// When the terminal sends \033[200~ ... \033[201~, newlines within the paste
// are replaced with pasteNewline (U+2028) so readline sees them as regular chars.
// After readline returns, the caller replaces U+2028 back to \n.
type pasteStdin struct {
	real            io.Reader
	mu              sync.Mutex
	buf             []byte // leftover bytes from last read
	inPaste         bool
	matchBuf        []byte // partial match buffer for escape sequences
	seenPasteMarker bool   // true once we've seen \033[200~ (terminal supports bracketed paste)
}

// newPasteStdin creates a paste-aware stdin wrapper and enables bracketed
// paste mode on the terminal.
func newPasteStdin() *pasteStdin {
	// Enable bracketed paste mode
	os.Stdout.Write(pasteEnable)
	return &pasteStdin{real: os.Stdin}
}

func (p *pasteStdin) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Drain leftover buffer first
	if len(p.buf) > 0 {
		n := copy(b, p.buf)
		p.buf = p.buf[n:]
		return n, nil
	}

	// Read from real stdin
	n, err := p.real.Read(b)
	if n == 0 {
		return n, err
	}

	data := b[:n]

	// Prepend any partial match buffer from previous read
	if len(p.matchBuf) > 0 {
		data = append(p.matchBuf, data...)
		p.matchBuf = nil
	}

	// Process the data: find paste start/end markers, replace newlines
	out := p.process(data)

	// Fallback for terminals without bracketed paste: if no paste markers
	// were seen, newlines in the MIDDLE of a chunk are from a paste (all
	// paste bytes arrive in one read). A trailing newline is Enter.
	if !p.inPaste && !p.seenPasteMarker {
		out = replaceMidChunkNewlines(out)
	}

	n = copy(b, out)
	if n < len(out) {
		p.buf = append(p.buf, out[n:]...)
	}
	return n, err
}

func (p *pasteStdin) process(data []byte) []byte {
	var out bytes.Buffer
	i := 0
	for i < len(data) {
		// Check for escape sequence start
		if data[i] == '\033' {
			remaining := data[i:]

			// Check for paste start: \033[200~
			if matchResult := matchSeq(remaining, pasteStart); matchResult == matchFull {
				p.inPaste = true
				p.seenPasteMarker = true
				i += len(pasteStart)
				continue
			} else if matchResult == matchPartial {
				// Save for next Read call
				p.matchBuf = append(p.matchBuf, remaining...)
				return out.Bytes()
			}

			// Check for paste end: \033[201~
			if matchResult := matchSeq(remaining, pasteEnd); matchResult == matchFull {
				p.inPaste = false
				i += len(pasteEnd)
				continue
			} else if matchResult == matchPartial {
				p.matchBuf = append(p.matchBuf, remaining...)
				return out.Bytes()
			}
		}

		// Inside paste: replace \n and \r\n with placeholder
		if p.inPaste && (data[i] == '\n' || data[i] == '\r') {
			if data[i] == '\r' {
				// Skip \r, the \n following it (if any) will be replaced
				if i+1 < len(data) && data[i+1] == '\n' {
					i++ // skip \r, handle \n below
				} else {
					// Bare \r — replace with placeholder
					writeRune(&out, pasteNewline)
					i++
					continue
				}
			}
			// Replace \n with placeholder (UTF-8 encoded U+2028)
			writeRune(&out, pasteNewline)
			i++
			continue
		}

		out.WriteByte(data[i])
		i++
	}
	return out.Bytes()
}

func (p *pasteStdin) Close() error {
	// Disable bracketed paste mode
	os.Stdout.Write(pasteDisable)
	if c, ok := p.real.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

type matchType int

const (
	matchNone    matchType = iota
	matchPartial           // data is a prefix of seq
	matchFull              // data starts with seq
)

func matchSeq(data, seq []byte) matchType {
	n := len(seq)
	if len(data) >= n {
		if bytes.Equal(data[:n], seq) {
			return matchFull
		}
		return matchNone
	}
	// data is shorter than seq — check if it's a prefix
	if bytes.Equal(data, seq[:len(data)]) {
		return matchPartial
	}
	return matchNone
}

func writeRune(buf *bytes.Buffer, r rune) {
	buf.WriteRune(r)
}

// replaceMidChunkNewlines replaces \r and \n that appear before the end of the
// data with U+2028 placeholders. A trailing newline is left as-is (it's the
// user pressing Enter). This handles paste in terminals without bracketed paste:
// all paste bytes arrive in one os.Stdin.Read, so mid-chunk newlines are from
// the pasted text, not from the user pressing Enter.
func replaceMidChunkNewlines(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	// Quick check: any newline before the last byte?
	last := len(data) - 1
	found := false
	for i := 0; i < last; i++ {
		if data[i] == '\n' || data[i] == '\r' {
			found = true
			break
		}
	}
	if !found {
		return data
	}

	var out bytes.Buffer
	out.Grow(len(data) + 8) // slight growth for multi-byte U+2028
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' || data[i] == '\r' {
			// Check if this newline (or \r\n pair) is at the end of the chunk.
			// Trailing newline = Enter key, preserve it.
			end := i
			if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
				end = i + 1
			}
			if end >= last {
				// At or past the last byte — preserve trailing newline(s)
				out.Write(data[i:])
				break
			}
			// Mid-chunk newline — from a paste, replace with placeholder
			if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
				i++ // skip \r, \n is consumed below
			}
			writeRune(&out, pasteNewline)
			continue
		}
		out.WriteByte(data[i])
	}
	return out.Bytes()
}

// DrainStale discards any bytes currently waiting in os.Stdin without blocking.
// Call before WatchForCancel to prevent stale escape sequences from triggering
// false cancellations.
func (p *pasteStdin) DrainStale() {
	stdinFile, ok := p.real.(*os.File)
	if !ok {
		return
	}
	stdinFile.SetReadDeadline(time.Now()) // deadline in the past → immediate return
	drain := make([]byte, 256)
	stdinFile.Read(drain) // discard whatever was waiting
	stdinFile.SetReadDeadline(time.Time{})
}

// Inject prepends data into the pasteStdin buffer so the next Read returns it.
// Used to feed back bytes captured by the turn watcher.
func (p *pasteStdin) Inject(data []byte) {
	if len(data) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(data, p.buf...)
}

// WatchForCancel reads from stdin during an agent turn, looking for ESC (0x1B)
// or Ctrl-C (0x03). If detected, calls cancel. Any other bytes are buffered
// and returned when the context is done. The caller must inject the returned
// bytes back via Inject.
//
// ESC detection uses a 50ms delay to distinguish standalone ESC from escape
// sequences (arrow keys send ESC + '[' + letter). Ctrl-C cancels immediately.
func (p *pasteStdin) WatchForCancel(ctx context.Context, cancel context.CancelFunc) <-chan []byte {
	ch := make(chan []byte, 1)
	go func() {
		var buf []byte
		b := make([]byte, 64)
		stdinFile, ok := p.real.(*os.File)
		if !ok {
			ch <- nil
			return
		}
		defer func() {
			stdinFile.SetReadDeadline(time.Time{})
			ch <- buf
		}()
		for {
			if ctx.Err() != nil {
				return
			}
			stdinFile.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, err := stdinFile.Read(b)
			if n > 0 {
				for i := 0; i < n; i++ {
					c := b[i]
					if c == 0x03 {
						// Ctrl-C: cancel immediately
						cancel()
						return
					}
					if c == 0x1B {
						// ESC: wait briefly to see if it's part of an escape sequence
						// (arrow keys, function keys all start with ESC + '[')
						if i+1 < n && b[i+1] == '[' {
							// Escape sequence in same read — buffer all remaining bytes
							buf = append(buf, b[i:n]...)
							i = n // skip rest
							continue
						}
						// Check if more bytes arrive within 50ms
						stdinFile.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
						peek := make([]byte, 8)
						pn, _ := stdinFile.Read(peek)
						if pn > 0 && peek[0] == '[' {
							// Escape sequence — buffer ESC + the peeked bytes
							buf = append(buf, 0x1B)
							buf = append(buf, peek[:pn]...)
							continue
						}
						// Standalone ESC — cancel the turn
						if pn > 0 {
							buf = append(buf, peek[:pn]...)
						}
						cancel()
						return
					}
					buf = append(buf, c)
				}
			}
			if err != nil && !isTimeoutError(err) {
				return
			}
		}
	}()
	return ch
}

// isTimeoutError checks if an error is a timeout (from SetReadDeadline).
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	type timeout interface {
		Timeout() bool
	}
	if t, ok := err.(timeout); ok {
		return t.Timeout()
	}
	return false
}
