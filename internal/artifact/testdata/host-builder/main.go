// host-builder is a static acceptance fixture, not a shipped Stoke worker.
// Its application logic reads only its named input and writes its named output;
// it reads no environment variables, clocks or subprocess results.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxInputBytes = 1 << 20

func build(input, output string) error {
	file, err := os.Open(input)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return errors.Join(err, file.Close())
	}
	if !info.Mode().IsRegular() || info.Size() > maxInputBytes {
		return errors.Join(errors.New("input must be a regular file of at most 1 MiB"), file.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxInputBytes+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return err
	}
	if len(data) > maxInputBytes || int64(len(data)) != info.Size() {
		return errors.New("input changed or exceeded the 1 MiB limit")
	}
	if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return err
	}
	result, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := result.Write(data)
	if err := errors.Join(writeErr, result.Chmod(0644), result.Close()); err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, "HOST_PRODUCER_EXECUTED")
	return err
}

func main() {
	if len(os.Args) != 3 {
		_, _ = fmt.Fprintln(os.Stderr, "usage: host-builder INPUT OUTPUT")
		os.Exit(2)
	}
	if err := build(os.Args[1], os.Args[2]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "host-builder:", err)
		os.Exit(1)
	}
}
