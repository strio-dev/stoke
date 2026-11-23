package workflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// ReadFile reads a bounded regular workflow file. A nonblocking open prevents a
// FIFO or device selected as a workflow from trapping the caller before parsing.
// Symlinks to regular files retain the ordinary local-workflow path semantics.
func ReadFile(ctx context.Context, path string) ([]byte, error) {
	return readFile(ctx, func() (*os.File, error) { return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) })
}

// ReadFileAt applies the same bounded ordinary-file rules inside an open root.
func ReadFileAt(ctx context.Context, root *os.Root, path string) ([]byte, error) {
	return readFile(ctx, func() (*os.File, error) { return root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) })
}

func readFile(ctx context.Context, open func() (*os.File, error)) (_ []byte, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := open()
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("workflow %q is not a regular file", file.Name())
	}
	if info.Size() > MaxSize {
		return nil, errors.New("workflow exceeds 1 MiB")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxSize+1))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) > MaxSize {
		return nil, errors.New("workflow exceeds 1 MiB")
	}
	return data, nil
}
