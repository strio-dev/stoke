//go:build linux

package hostfiles

import (
	"os"

	"golang.org/x/sys/unix"
)

// Commit atomically installs a private staging tree without replacing an
// existing path, including an empty directory created after a freshness check.
// Source and destination must reside on the same filesystem.
func Commit(source, destination string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE); err != nil {
		return &os.LinkError{Op: "rename-noreplace", Old: source, New: destination, Err: err}
	}
	return nil
}
