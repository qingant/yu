package agent

import (
	"bytes"
	"testing"
)

func TestPasteProcess_NoPaste(t *testing.T) {
	p := &pasteStdin{}
	out := p.process([]byte("hello world"))
	if string(out) != "hello world" {
		t.Errorf("got %q, want %q", out, "hello world")
	}
}

func TestPasteProcess_BracketedPasteBuffered(t *testing.T) {
	// Bracketed paste content should be buffered, not in output
	data := append(append(pasteStart, []byte("line1\nline2")...), pasteEnd...)
	p := &pasteStdin{}
	out := p.process(data)

	// Output should be empty (paste content is buffered)
	if len(out) != 0 {
		t.Errorf("process should return empty output, got %q", out)
	}
	// completedPaste should have the content with newlines preserved
	if p.completedPaste != "line1\nline2" {
		t.Errorf("completedPaste = %q, want %q", p.completedPaste, "line1\nline2")
	}
	if !p.sendSubmit {
		t.Error("sendSubmit should be true after paste end")
	}
}

func TestPasteStdinRead_BracketedPaste(t *testing.T) {
	// Full bracketed paste in one read
	data := append(append(pasteStart, []byte("hello\nworld")...), pasteEnd...)
	p := &pasteStdin{real: bytes.NewReader(data)}

	buf := make([]byte, 1024)

	// First read: paste is absorbed, returns \r (submit trigger)
	n, _ := p.Read(buf)
	if n != 1 || buf[0] != '\r' {
		t.Errorf("expected \\r submit trigger, got %d bytes: %q", n, buf[:n])
	}

	// TakePaste should have the content
	paste := p.TakePaste()
	if paste != "hello\nworld" {
		t.Errorf("TakePaste = %q, want %q", paste, "hello\nworld")
	}

	// Second TakePaste returns empty
	if p.TakePaste() != "" {
		t.Error("second TakePaste should return empty")
	}
}

func TestPasteStdinRead_TextBeforePaste(t *testing.T) {
	// Text typed before paste
	data := append([]byte("typed: "), append(append(pasteStart, []byte("pasted\ntext")...), pasteEnd...)...)
	p := &pasteStdin{real: bytes.NewReader(data)}

	buf := make([]byte, 1024)

	// First read: should return "typed: " then \r (from paste completion)
	// Actually, process returns "typed: " as out, then sees paste markers.
	// When paste ends, sendSubmit is set. Read saves "typed: " to buf and returns \r.
	n, _ := p.Read(buf)
	if n != 1 || buf[0] != '\r' {
		// If the typed text comes first, it might be returned before the \r
		// depending on implementation
		got := string(buf[:n])
		if got == "typed: " {
			// Next read should be \r
			n2, _ := p.Read(buf)
			if n2 != 1 || buf[0] != '\r' {
				t.Errorf("expected \\r after typed text, got %d bytes: %q", n2, buf[:n2])
			}
		} else if got != "\r" {
			t.Errorf("expected \\r or typed text, got %q", got)
		}
	}

	paste := p.TakePaste()
	if paste != "pasted\ntext" {
		t.Errorf("TakePaste = %q, want %q", paste, "pasted\ntext")
	}
}

func TestPastePartialSequence(t *testing.T) {
	// Paste arriving in two chunks
	chunk1 := []byte("\033[200")
	chunk2 := append([]byte("~hello\nworld"), pasteEnd...)

	r := &chunkedReader{chunks: [][]byte{chunk1, chunk2}}
	p := &pasteStdin{real: r}

	buf := make([]byte, 1024)

	// First Read: partial sequence buffered, returns 0
	n1, _ := p.Read(buf)
	if n1 != 0 {
		t.Errorf("first Read should return 0, got %d: %q", n1, buf[:n1])
	}

	// Second Read: paste completed, returns \r
	n2, _ := p.Read(buf)
	if n2 != 1 || buf[0] != '\r' {
		t.Errorf("expected \\r, got %d bytes: %q", n2, buf[:n2])
	}

	paste := p.TakePaste()
	if paste != "hello\nworld" {
		t.Errorf("TakePaste = %q, want %q", paste, "hello\nworld")
	}
}

func TestReplacePasteNewlines(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no newlines", "hello", "hello"},
		{"LF replaced", "hello\nworld", "hello\u2028world"},
		{"trailing LF replaced", "hello\n", "hello\u2028"},
		{"CR preserved", "hello\r", "hello\r"},
		{"CRLF as one", "a\r\nb", "a\u2028b"},
		{"multi-line", "line1\nline2\nline3", "line1\u2028line2\u2028line3"},
		{"single LF", "\n", "\u2028"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replacePasteNewlines([]byte(tt.input))
			if string(got) != tt.want {
				t.Errorf("replacePasteNewlines(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNonBracketedFallback(t *testing.T) {
	// No paste markers — \n replaced with U+2028 by fallback
	data := []byte("line1\nline2\nline3")
	p := &pasteStdin{real: bytes.NewReader(data)}

	buf := make([]byte, 1024)
	n, _ := p.Read(buf)
	got := string(buf[:n])
	want := "line1\u2028line2\u2028line3"
	if got != want {
		t.Errorf("non-bracketed fallback: Read = %q, want %q", got, want)
	}
}

func TestRestorePasteNewlines(t *testing.T) {
	input := "line1\u2028line2\u2028line3"
	got := restorePasteNewlines(input)
	want := "line1\nline2\nline3"
	if got != want {
		t.Errorf("restorePasteNewlines = %q, want %q", got, want)
	}
}

func TestMatchSeq(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		seq  []byte
		want matchType
	}{
		{"full match", pasteStart, pasteStart, matchFull},
		{"partial match", []byte("\033[20"), pasteStart, matchPartial},
		{"no match", []byte("abc"), pasteStart, matchNone},
		{"single ESC partial", []byte{0x1B}, pasteStart, matchPartial},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchSeq(tt.data, tt.seq)
			if got != tt.want {
				t.Errorf("matchSeq() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInject(t *testing.T) {
	p := &pasteStdin{real: bytes.NewReader([]byte("world"))}
	p.Inject([]byte("hello "))

	buf := make([]byte, 1024)
	n, _ := p.Read(buf)
	got := string(buf[:n])
	if got != "hello " {
		t.Errorf("Read after Inject = %q, want %q", got, "hello ")
	}
}

// chunkedReader returns pre-set chunks on each Read call.
type chunkedReader struct {
	chunks [][]byte
	idx    int
}

func (r *chunkedReader) Read(b []byte) (int, error) {
	if r.idx >= len(r.chunks) {
		return 0, nil
	}
	n := copy(b, r.chunks[r.idx])
	r.idx++
	return n, nil
}
