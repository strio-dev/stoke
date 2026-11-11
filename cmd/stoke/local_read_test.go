package main

import (
	"bytes"
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

func TestLocalWorkflowCommandsRejectFIFO(t *testing.T) {
	if command := os.Getenv("STOKE_FIFO_COMMAND_HELPER"); command != "" {
		args := []string{command, "-f", os.Getenv("STOKE_FIFO_COMMAND_PATH")}
		if command == "publish" {
			args = append(args, "registry.test/unused:fixture")
		}
		var output, diagnostics bytes.Buffer
		code := runContext(context.Background(), args, &output, &diagnostics)
		if code != 1 || !strings.Contains(diagnostics.String(), "not a regular file") {
			_, _ = fmt.Fprintf(os.Stderr, "unexpected result: exit=%d output=%s diagnostics=%s", code, &output, &diagnostics)
			os.Exit(3)
		}
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"check", "plan", "run", "publish"} {
		t.Run(command, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stoke.yaml")
			if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestLocalWorkflowCommandsRejectFIFO$")
			cmd.Env = append(os.Environ(), "STOKE_FIFO_COMMAND_HELPER="+command, "STOKE_FIFO_COMMAND_PATH="+path, "GORACE=atexit_sleep_ms=0")
			cmd.WaitDelay = time.Second
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("%s FIFO read blocked until subprocess deadline: %v\n%s", command, ctx.Err(), output)
			}
			if err != nil {
				t.Fatalf("%s FIFO was not rejected: %v\n%s", command, err, output)
			}
		})
	}
}
