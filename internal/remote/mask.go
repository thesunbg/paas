package remote

import (
	"regexp"
	"strings"
	"sync"
)

// MaskText replaces every secret in s with "********". Secrets shorter than four
// characters are not masked: they would garble ordinary output.
func MaskText(s string, secrets []string) string {
	for _, sec := range secrets {
		if len(sec) >= 4 {
			s = strings.ReplaceAll(s, sec, "********")
		}
	}
	return s
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// maxLine flushes a line without a newline once it is this long.
const maxLine = 8 << 10

// LineWriter turns a command's output into masked lines without terminal colour codes.
// A carriage return (progress output) keeps the last text written on the line. It is
// safe for concurrent writes. Close flushes the last partial line.
type LineWriter struct {
	mu      sync.Mutex
	buf     []byte
	emit    func(string)
	secrets []string
}

// NewLineWriter calls emit with each complete line, secrets masked.
func NewLineWriter(emit func(string), secrets ...string) *LineWriter {
	return &LineWriter{emit: emit, secrets: secrets}
}

func (w *LineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		w.line(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	// Keep the tail of a long line that has no newline: a secret may straddle the cut.
	if len(w.buf) > maxLine {
		w.line(string(w.buf[:maxLine/2]))
		w.buf = append([]byte(nil), w.buf[maxLine/2:]...)
	}
	return len(p), nil
}

func (w *LineWriter) line(s string) {
	// Mask first: a colour code could otherwise split a secret.
	s = MaskText(s, w.secrets)
	s = ansiRE.ReplaceAllString(s, "")
	s = strings.TrimRight(s, "\r")
	if i := strings.LastIndexByte(s, '\r'); i >= 0 {
		s = s[i+1:]
	}
	s = MaskText(s, w.secrets)
	w.emit(s)
}

// Close emits what is left of the last line.
func (w *LineWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.line(string(w.buf))
		w.buf = nil
	}
	return nil
}
