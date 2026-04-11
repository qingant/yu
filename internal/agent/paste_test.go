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

func TestPasteProcess_BracketedPaste(t *testing.T) {
	// Bracketed paste: content is buffered during paste, released on end marker
	data := append(append(pasteStart, []byte("line1\nline2")...), pasteEnd...)
	p := &pasteStdin{}
	out := p.process(data)
	// Output contains the paste content (newlines preserved at this stage)
	if string(out) != "line1\nline2" {
		t.Errorf("process = %q, want %q", out, "line1\nline2")
	}
}

func TestPasteStdinRead_BracketedPaste(t *testing.T) {
	// Full bracketed paste: content flows through with \n replaced
	data := append(append(pasteStart, []byte("hello\nworld")...), pasteEnd...)
	p := &pasteStdin{real: bytes.NewReader(data)}

	buf := make([]byte, 1024)
	n, _ := p.Read(buf)
	got := string(buf[:n])
	want := "hello\u2028world"
	if got != want {
		t.Errorf("Read = %q, want %q", got, want)
	}
}

func TestPastePartialSequence(t *testing.T) {
	// Paste arriving in two chunks
	chunk1 := []byte("\033[200")
	chunk2 := append([]byte("~hello\nworld"), pasteEnd...)
	r := &chunkedReader{chunks: [][]byte{chunk1, chunk2}}
	p := &pasteStdin{real: r}

	buf := make([]byte, 1024)
	n, _ := p.Read(buf)
	got := string(buf[:n])
	want := "hello\u2028world"
	if got != want {
		t.Errorf("Read = %q, want %q", got, want)
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
		{"trailing LF", "hello\n", "hello\u2028"},
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
	// No paste markers — \n replaced by fallback
	data := []byte("line1\nline2\nline3")
	p := &pasteStdin{real: bytes.NewReader(data)}

	buf := make([]byte, 1024)
	n, _ := p.Read(buf)
	got := string(buf[:n])
	want := "line1\u2028line2\u2028line3"
	if got != want {
		t.Errorf("Read = %q, want %q", got, want)
	}
}

func TestRestorePasteNewlines(t *testing.T) {
	got := restorePasteNewlines("line1\u2028line2")
	if got != "line1\nline2" {
		t.Errorf("got %q, want %q", got, "line1\nline2")
	}
}

func TestMatchSeq(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		seq  []byte
		want matchType
	}{
		{"full", pasteStart, pasteStart, matchFull},
		{"partial", []byte("\033[20"), pasteStart, matchPartial},
		{"none", []byte("abc"), pasteStart, matchNone},
		{"single ESC", []byte{0x1B}, pasteStart, matchPartial},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchSeq(tt.data, tt.seq); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInject(t *testing.T) {
	p := &pasteStdin{real: bytes.NewReader([]byte("world"))}
	p.Inject([]byte("hello "))
	buf := make([]byte, 1024)
	n, _ := p.Read(buf)
	if string(buf[:n]) != "hello " {
		t.Errorf("got %q, want %q", buf[:n], "hello ")
	}
}

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
