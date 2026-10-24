package main

import (
	"io"
	"os"
	"sync"
)

// logRing keeps the most recent core log lines in memory for the status
// panel and appends everything to a size-rotated file on disk.
const (
	ringLines     = 500
	logRotateSize = 1 << 20 // 1 MB
	logKeepFiles  = 2
)

type logRing struct {
	mu      sync.Mutex
	lines   []string
	file    *os.File
	path    string
	written int64
}

func newLogRing(path string) *logRing {
	return &logRing{path: path}
}

// openFile opens (or reopens after rotation) the backing log file.
func (r *logRing) openFile() {
	if r.file != nil {
		return
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return // in-memory ring still works without the file
	}
	if st, err := f.Stat(); err == nil {
		r.written = st.Size()
	}
	r.file = f
}

func (r *logRing) rotateLocked() {
	if r.file == nil {
		return
	}
	_ = r.file.Close()
	r.file = nil
	_ = os.Remove(r.path + "." + itoa(logKeepFiles))
	for i := logKeepFiles - 1; i >= 1; i-- {
		_ = os.Rename(r.path+"."+itoa(i), r.path+"."+itoa(i+1))
	}
	_ = os.Rename(r.path, r.path+".1")
	r.written = 0
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// write appends one finished line (no trailing newline required).
func (r *logRing) write(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.openFile()
	if r.file != nil {
		if r.written > logRotateSize {
			r.rotateLocked()
			r.openFile()
		}
		if n, err := io.WriteString(r.file, line+"\n"); err == nil {
			r.written += int64(n)
		}
	}
	r.lines = append(r.lines, line)
	if len(r.lines) > ringLines {
		r.lines = r.lines[len(r.lines)-ringLines:]
	}
}

// tail returns up to n recent lines, oldest first.
func (r *logRing) tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.lines) {
		n = len(r.lines)
	}
	out := make([]string, n)
	copy(out, r.lines[len(r.lines)-n:])
	return out
}

// clear drops every buffered line and truncates the file — used when the
// core restarts or the node is deleted, so a fresh view starts fresh.
func (r *logRing) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = nil
	if r.file != nil {
		_ = r.file.Truncate(0)
		r.written = 0
	}
}

func (r *logRing) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}
}
