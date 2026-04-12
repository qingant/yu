package agent

import (
	"bytes"
	"testing"
)

func TestBracketedPaste_Buffered(t *testing.T) {
	data := append(append(pasteStart, []byte("line1\nline2")...), pasteEnd...)
	p := &pasteStdin{real: bytes.NewReader(data)}

	buf := make([]byte, 1024)
	n, _ := p.Read(buf)
	if n != 1 || buf[0] != '\r' {
		t.Fatalf("expected \\r, got %d bytes: %q", n, buf[:n])
	}
	paste := p.TakePaste()
	if paste != "line1\nline2" {
		t.Errorf("TakePaste = %q, want %q", paste, "line1\nline2")
	}
}

func TestBracketedPaste_Chunked(t *testing.T) {
	chunk1 := []byte("\033[200")
	chunk2 := append([]byte("~hello\nworld"), pasteEnd...)
	p := &pasteStdin{real: &chunkedReader{chunks: [][]byte{chunk1, chunk2}}}

	buf := make([]byte, 1024)
	n, _ := p.Read(buf)
	if n != 1 || buf[0] != '\r' {
		t.Fatalf("expected \\r, got %d bytes: %q", n, buf[:n])
	}
	if p.TakePaste() != "hello\nworld" {
		t.Error("wrong paste content")
	}
}

func TestNonBracketedFallback(t *testing.T) {
	// Without paste markers, \n in raw mode is still detected as paste.
	// Auto-submits via \r, content available via TakePaste.
	p := &pasteStdin{real: bytes.NewReader([]byte("line1\nline2"))}
	buf := make([]byte, 1024)
	n, _ := p.Read(buf)
	if n != 1 || buf[0] != '\r' {
		t.Fatalf("expected \\r, got %d bytes: %q", n, buf[:n])
	}
	paste := p.TakePaste()
	if paste != "line1\nline2" {
		t.Errorf("TakePaste = %q, want %q", paste, "line1\nline2")
	}
}

func TestReplacePasteNewlines(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"no newlines", "hello", "hello"},
		{"LF", "a\nb", "a\u2028b"},
		{"CRLF", "a\r\nb", "a\u2028b"},
		{"CR preserved", "a\r", "a\r"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(replacePasteNewlines([]byte(tt.input)))
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMatchSeq(t *testing.T) {
	if matchSeq(pasteStart, pasteStart) != matchFull {
		t.Error("full match failed")
	}
	if matchSeq([]byte("\033[20"), pasteStart) != matchPartial {
		t.Error("partial match failed")
	}
	if matchSeq([]byte("abc"), pasteStart) != matchNone {
		t.Error("no match failed")
	}
}

func TestRestorePasteNewlines(t *testing.T) {
	if restorePasteNewlines("a\u2028b") != "a\nb" {
		t.Error("restore failed")
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
