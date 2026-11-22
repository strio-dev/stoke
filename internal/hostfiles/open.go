package hostfiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"
)

func openDirectoryRoot(ctx context.Context, name string) (*os.Root, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// On Unix, a terminal separator requires the resolved path to be a
	// directory at open time, even if the path changes before os.OpenRoot.
	if name != "" && name[len(name)-1] != filepath.Separator {
		name += string(filepath.Separator)
	}
	root, err := os.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return root, nil
}

// Admit an inspected regular file without waiting for a special file that
// might have replaced it. os.Root confines lookup; admission is checked again
// on the open descriptor. Nonblocking is not a deadline for filesystem I/O.
func openRegular(ctx context.Context, root *os.Root, name string, expected fs.FileInfo, limit int64) (*os.File, fs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if expected == nil {
		var err error
		expected, err = root.Lstat(name)
		if err != nil {
			return nil, nil, err
		}
	}
	if !expected.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s: host file must be regular", name)
	}
	if expected.Size() < 0 || expected.Size() > limit {
		return nil, nil, fmt.Errorf("%s: host files exceed byte limit", name)
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	actual, err := file.Stat()
	if err == nil && (!actual.Mode().IsRegular() || actual.Mode() != expected.Mode() ||
		actual.Size() != expected.Size() || !os.SameFile(expected, actual)) {
		err = fmt.Errorf("%s: host file changed while opening", name)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, nil, errors.Join(err, file.Close())
	}
	return file, actual, nil
}

func openCapturedFile(ctx context.Context, root *os.Root, entry Entry) (*os.File, error) {
	file, info, err := openRegular(ctx, root, entry.Path, nil, entry.Size)
	if err != nil {
		return nil, err
	}
	if err := regular(info); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if info.Size() != entry.Size || uint32(info.Mode().Perm()) != entry.Mode {
		return nil, errors.Join(fmt.Errorf("%s: host file changed since inventory", entry.Path), file.Close())
	}
	return file, nil
}

// Read directory entries in batches rather than materializing an unbounded
// listing before the inventory's entry and cancellation checks can run.
func walkInventory(ctx context.Context, root *os.Root, name string, entryCount func() int, add func(string, fs.FileInfo) error) error {
	pending := []string{name}
	for len(pending) != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		name = pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if err := add(name, info); err != nil {
			return err
		}
		if info.IsDir() {
			err := readDirectory(ctx, root, name, info, func(child string) error {
				if entryCount()+len(pending) >= MaxEntries {
					return errors.New("host files exceed entry limit")
				}
				pending = append(pending, path.Join(name, child))
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func readDirectory(ctx context.Context, root *os.Root, name string, expected fs.FileInfo, add func(string) error) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := root.OpenFile(name, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, dir.Close()) }()
	actual, err := dir.Stat()
	if err != nil {
		return err
	}
	if !actual.IsDir() || !os.SameFile(expected, actual) || actual.Mode() != expected.Mode() {
		return fmt.Errorf("%s: host directory changed while opening", name)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		names, readErr := dir.Readdirnames(128)
		for _, child := range names {
			if err := add(child); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return ctx.Err()
		}
		if readErr != nil {
			return readErr
		}
	}
}
