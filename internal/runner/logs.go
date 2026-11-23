package runner

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"unicode/utf8"
)

// Buffer incomplete lines per task/stream, so another task cannot inherit a
// fragment's label. Bound long lines without buffering unlimited process output.
type taskLogWriter struct {
	out     io.Writer
	prefix  string
	mu      sync.Mutex
	pending []byte
}

func taskLogs(out io.Writer, name string) *taskLogWriter {
	return &taskLogWriter{out: out, prefix: "[" + name + "] "}
}

func (w *taskLogWriter) Write(data []byte) (int, error) {
	if w.out == nil {
		return len(data), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	total := len(data)
	for len(data) != 0 {
		n := min(len(data), 32*1024-len(w.pending))
		if end := bytes.IndexByte(data[:n], '\n'); end >= 0 {
			n = end + 1
		}
		w.pending = append(w.pending, data[:n]...)
		data = data[n:]
		if w.pending[len(w.pending)-1] == '\n' || len(w.pending) == 32*1024 {
			end := len(w.pending)
			if w.pending[end-1] != '\n' {
				start := end - 1
				for start > end-utf8.UTFMax && !utf8.RuneStart(w.pending[start]) {
					start--
				}
				if !utf8.FullRune(w.pending[start:]) {
					end = start
				}
			}
			tail := w.pending[end:]
			w.pending = w.pending[:end]
			err := w.flush()
			w.pending = append(w.pending, tail...)
			if err != nil {
				return total - len(data), err
			}
		}
	}
	return total, nil
}

func (w *taskLogWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flush()
}

func (w *taskLogWriter) flush() error {
	if len(w.pending) == 0 {
		return nil
	}
	line := w.prefix + string(w.pending)
	if line[len(line)-1] != '\n' {
		line += "\n"
	}
	w.pending = w.pending[:0]
	if w.out == nil {
		return nil
	}
	n, err := io.WriteString(w.out, line)
	if n != len(line) && err == nil {
		err = io.ErrShortWrite
	}
	return err
}

func labelTaskLogs(options *Options, name string) func() error {
	stdout, stderr := taskLogs(options.Stdout, name), taskLogs(options.Stderr, name)
	options.Stdout, options.Stderr = stdout, stderr
	return func() error { return errors.Join(stdout.Flush(), stderr.Flush()) }
}
