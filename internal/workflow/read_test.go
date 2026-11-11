package workflow

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileRegularAndSizeBoundaries(t *testing.T) {
	for _, size := range []int{0, MaxSize, MaxSize + 1} {
		t.Run(map[int]string{0: "empty", MaxSize: "limit", MaxSize + 1: "oversize"}[size], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stoke.yaml")
			data := bytes.Repeat([]byte("x"), size)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			for _, symlink := range []bool{false, true} {
				selected := path
				if symlink {
					selected = filepath.Join(t.TempDir(), "linked.yaml")
					if err := os.Symlink(path, selected); err != nil {
						t.Fatal(err)
					}
				}
				got, err := ReadFile(context.Background(), selected)
				if size > MaxSize {
					if err == nil || !strings.Contains(err.Error(), "exceeds 1 MiB") {
						t.Fatalf("oversized read: %v", err)
					}
				} else if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("regular read length=%d err=%v", len(got), err)
				}
			}
		})
	}
}

func TestReadFileRejectsDirectoryAndHonorsCancellation(t *testing.T) {
	if _, err := ReadFile(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory read: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadFile(ctx, filepath.Join(t.TempDir(), "missing.yaml")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost to open: %v", err)
	}
}
