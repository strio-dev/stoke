package runner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/strio-dev/stoke/internal/hostfiles"
	"golang.org/x/sys/unix"
)

// Reusable commands need a stable actual execution location: absolute cwd,
// HOME and TMPDIR can influence output bytes. Different locations remain
// different cache inputs. The namespace is private, marked and never pruned.
func leaseHostWorkspace(ctx context.Context, seed, job string) (string, *os.File, error) {
	parent, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return "", nil, err
	}
	base := filepath.Join(parent, fmt.Sprintf("stoke-host-%d", os.Getuid()))
	const marker = "stoke-host-workspaces-v1\n"
	if _, err := os.Lstat(base); errors.Is(err, os.ErrNotExist) {
		staging, err := os.MkdirTemp(os.TempDir(), ".stoke-host-init-")
		if err != nil {
			return "", nil, err
		}
		defer func() { _ = os.RemoveAll(staging) }()
		if err := os.WriteFile(filepath.Join(staging, ".owner"), []byte(marker), 0600); err != nil {
			return "", nil, err
		}
		if err := hostfiles.Commit(staging, base); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", nil, err
		}
	} else if err != nil {
		return "", nil, err
	}
	info, err := os.Lstat(base)
	if err != nil {
		return "", nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || stat.Uid != uint32(os.Getuid()) {
		return "", nil, errors.New("host workspace namespace must be a private caller-owned directory")
	}
	data, err := os.ReadFile(filepath.Join(base, ".owner"))
	if err != nil || string(data) != marker {
		return "", nil, errors.New("unrecognized host workspace namespace; choose a different caller TMPDIR")
	}
	token := fmt.Sprintf("%x", sha256.Sum256([]byte(job+"\x00"+seed)))
	fd, err := unix.Open(filepath.Join(base, token+".lock"), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return "", nil, err
	}
	lock := os.NewFile(uintptr(fd), "host-workspace-lock")
	lockInfo, err := lock.Stat()
	if err != nil {
		_ = lock.Close()
		return "", nil, err
	}
	if !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm() != 0600 {
		_ = lock.Close()
		return "", nil, errors.New("host workspace lock must be a private regular file")
	}
	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = lock.Close()
			return "", nil, err
		}
		select {
		case <-ctx.Done():
			_ = lock.Close()
			return "", nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	root := filepath.Join(base, token)
	if err := os.Mkdir(root, 0700); err != nil {
		_ = lock.Close()
		return "", nil, fmt.Errorf("host workspace is not fresh; retained data is not removed: %s: %w", root, err)
	}
	return root, lock, nil
}

// Repair permissions only inside our own workspace, never through symlinks or
// mounted host trees. A mount or an ownership change retains the tree and fails
// cleanup, rather than turning cleanup into a mutation of unrelated user data.
func removePrivateHostTree(root string) error {
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mount := unescape.Replace(fields[4])
		if mount == root || strings.HasPrefix(mount, root+string(filepath.Separator)) {
			return fmt.Errorf("host workspace contains a mount and was retained: %s", root)
		}
	}
	if err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) {
			return errors.New("host workspace directory ownership changed; tree retained")
		}
		return os.Chmod(name, 0700)
	}); err != nil {
		return err
	}
	return os.RemoveAll(root)
}
