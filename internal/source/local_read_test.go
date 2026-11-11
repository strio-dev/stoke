package source

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLocalWorkflowRejectsFIFO(t *testing.T) {
	if path := os.Getenv("STOKE_FIFO_SOURCE_HELPER"); path != "" {
		_, err := (Loader{}).Load(context.Background(), path)
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			_, _ = fmt.Fprintf(os.Stderr, "unexpected workflow read error: %v\n", err)
			os.Exit(3)
		}
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "stoke.yaml")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestLocalWorkflowRejectsFIFO$")
	cmd.Env = append(os.Environ(), "STOKE_FIFO_SOURCE_HELPER="+path, "GORACE=atexit_sleep_ms=0")
	cmd.WaitDelay = time.Second
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("workflow FIFO read blocked until subprocess deadline: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("workflow FIFO was not rejected: %v\n%s", err, output)
	}
}
