package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/strio-dev/stoke/internal/runner"
)

// The stream is a fresh private output, never another mutable build input.
func openCDEvents(name, contextDir string) (*runner.CDEvents, *cdeventsFile, error) {
	if name == "" {
		return nil, nil, nil
	}
	absolute, err := filepath.Abs(name)
	if err != nil {
		return nil, nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return nil, nil, err
	}
	absolute = filepath.Join(parent, filepath.Base(absolute))
	contextDir, err = filepath.Abs(contextDir)
	if err == nil {
		contextDir, err = filepath.EvalSymlinks(contextDir)
	}
	if err != nil {
		return nil, nil, err
	}
	relative, err := filepath.Rel(contextDir, absolute)
	if err != nil || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, nil, errors.New("CDEvents file must be outside the input context")
	}
	file := &cdeventsFile{name: absolute}
	observer, err := runner.NewCDEvents(file)
	if err != nil {
		return nil, nil, errors.Join(err, file.Close())
	}
	return observer, file, nil
}

// Open only after the runner has admitted all resolved input paths. The
// observer serializes writes; the invocation owner closes after workers drain.
type cdeventsFile struct {
	name string
	file *os.File
}

func (f *cdeventsFile) Write(data []byte) (int, error) {
	if f.file == nil {
		file, err := os.OpenFile(f.name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return 0, err
		}
		f.file = file
	}
	return f.file.Write(data)
}

func (f *cdeventsFile) Close() error {
	if f.file == nil {
		return nil
	}
	return f.file.Close()
}
