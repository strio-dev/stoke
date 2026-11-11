// Package assets validates, snapshots and materializes component-owned files.
package assets

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const MaxFiles = 256
const MaxFileBytes = 8 << 20
const MaxTotalBytes = 32 << 20

// File carries bytes and the only permission distinction preserved by packaging.
type File struct {
	Data       []byte
	Executable bool
}

// ValidatePath requires one canonical slash-separated path within a component.
func ValidatePath(name string) error {
	if !utf8.ValidString(name) || name == "" || name == "." || strings.HasPrefix(name, "/") || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00") {
		return fmt.Errorf("asset path %q must be a canonical relative path", name)
	}
	if name == "stoke.yaml" || strings.HasPrefix(name, "stoke.yaml/") {
		return errors.New("asset path stoke.yaml is reserved for the workflow")
	}
	return nil
}

func validateNames(names []string) error {
	if len(names) > MaxFiles {
		return fmt.Errorf("component exceeds %d asset files", MaxFiles)
	}
	seen := map[string]bool{}
	for _, name := range names {
		if err := ValidatePath(name); err != nil {
			return err
		}
		if seen[name] {
			return fmt.Errorf("duplicate asset path %q", name)
		}
		seen[name] = true
	}
	for _, name := range names {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if seen[parent] {
				return fmt.Errorf("asset paths %q and %q conflict", parent, name)
			}
		}
	}
	return nil
}

// Validate rejects invalid paths, conflicting files and oversized payloads.
func Validate(files map[string]File) error {
	names := make([]string, 0, len(files))
	total := 0
	for name, file := range files {
		names = append(names, name)
		if len(file.Data) > MaxFileBytes {
			return fmt.Errorf("asset %q exceeds %d bytes", name, MaxFileBytes)
		}
		total += len(file.Data)
		if total > MaxTotalBytes {
			return fmt.Errorf("component assets exceed %d bytes", MaxTotalBytes)
		}
	}
	return validateNames(names)
}

// Collect snapshots explicitly named regular files. Root-confined opens prevent
// symlinks from escaping the component directory, including during path races.
func Collect(directory string, names []string) (_ map[string]File, retErr error) {
	if err := validateNames(names); err != nil {
		return nil, err
	}
	files := make(map[string]File, len(names))
	if len(names) == 0 {
		return files, nil
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	total := 0
	for _, name := range names {
		file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, fmt.Errorf("asset %q: %w", name, err)
		}
		info, err := file.Stat()
		if err != nil {
			return nil, errors.Join(err, file.Close())
		}
		if !info.Mode().IsRegular() {
			return nil, errors.Join(fmt.Errorf("asset %q is not a regular file", name), file.Close())
		}
		if info.Size() > MaxFileBytes {
			return nil, errors.Join(fmt.Errorf("asset %q exceeds %d bytes", name, MaxFileBytes), file.Close())
		}
		data, readErr := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
		if err = errors.Join(readErr, file.Close()); err != nil {
			return nil, err
		}
		if len(data) > MaxFileBytes {
			return nil, fmt.Errorf("asset %q exceeds %d bytes", name, MaxFileBytes)
		}
		total += len(data)
		if total > MaxTotalBytes {
			return nil, fmt.Errorf("component assets exceed %d bytes", MaxTotalBytes)
		}
		files[name] = File{Data: data, Executable: info.Mode().Perm()&0111 != 0}
	}
	return files, nil
}

// Materialize writes validated files into an existing empty directory. It never
// replaces an existing entry; the caller owns and cleans up its fresh directory.
func Materialize(directory string, files map[string]File) (retErr error) {
	if err := Validate(files); err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("asset directory must be empty")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	directories := map[string]bool{".": true}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if parent := path.Dir(name); parent != "." {
			if err := root.MkdirAll(parent, 0755); err != nil {
				return err
			}
			for current := parent; current != "."; current = path.Dir(current) {
				directories[current] = true
			}
		}
		mode := os.FileMode(0644)
		if files[name].Executable {
			mode = 0755
		}
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(files[name].Data)
		chmodErr := file.Chmod(mode)
		if err := errors.Join(writeErr, chmodErr, file.Close()); err != nil {
			return err
		}
		if err := root.Chtimes(name, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
			return err
		}
	}
	for directory := range directories {
		if directory != "." {
			if err := root.Chmod(directory, 0755); err != nil {
				return err
			}
		}
		if err := root.Chtimes(directory, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
			return err
		}
	}
	return nil
}
