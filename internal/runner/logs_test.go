package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	coopr "github.com/strio-dev/stoke/internal/engine"
)

func TestOverlappingVariantLogsKeepFragmentIdentity(t *testing.T) {
	w := fixture(t, "version: 1\ntasks:\n  check:\n    image: scratch\n    strategy: {matrix: {mode: [one, two]}}\n")
	plan, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions(t)
	var log bytes.Buffer
	opts.Stdout, opts.Stderr = &log, &log
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}, 2), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := runLiteral(ctx, w, nil, opts, func(ctx context.Context, p coopr.Program, o coopr.Options) (coopr.Result, error) {
			_, _ = io.WriteString(o.Stdout, "par")
			_, _ = io.WriteString(o.Stderr, "err-")
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return coopr.Result{}, ctx.Err()
			}
			_, _ = io.WriteString(o.Stdout, "t\nnext\nlast")
			_, _ = io.WriteString(o.Stderr, "done\ntail")
			return coopr.Result{ImageID: string(p.Output)}, nil
		})
		done <- err
	}()
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("variants did not overlap")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, job := range plan.Groups[0].Jobs {
		for _, line := range []string{"part", "next", "last", "err-done", "tail"} {
			if strings.Count(log.String(), "[check[mode="+job.Matrix["mode"]+"]] "+line+"\n") != 1 {
				t.Fatalf("lost or misattributed %s/%s: %s", job.Name, line, &log)
			}
		}
	}
}

func TestTaskLogBoundsAndWriterErrors(t *testing.T) {
	var out bytes.Buffer
	w := taskLogs(&out, "check")
	payload := strings.Repeat("x", 32767) + "€" + strings.Repeat("x", 70000)
	if _, err := io.WriteString(w, payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(out.Bytes()) {
		t.Fatal("bounded output split a UTF-8 character")
	}
	var restored strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if !strings.HasPrefix(line, "[check] ") || len(line) > 32*1024+len("[check] ") {
			t.Fatal("unbounded or unattributed line")
		}
		restored.WriteString(strings.TrimPrefix(line, "[check] "))
	}
	if restored.String() != payload {
		t.Fatal("long output lost bytes")
	}
	w = taskLogs(shortLogWriter{}, "check")
	if _, err := io.WriteString(w, "line\n"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short writer error lost: %v", err)
	}
	w = taskLogs(shortLogWriter{}, "check")
	_, _ = io.WriteString(w, "tail")
	if err := w.Flush(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("partial-tail flush error lost: %v", err)
	}
}

type shortLogWriter struct{}

func (shortLogWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
