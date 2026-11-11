package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	coopr "github.com/strio-dev/stoke/internal/engine"
	"golang.org/x/sys/unix"
)

// Keep the leader unreaped until its process group has been stopped. Its PID
// cannot be recycled while we signal the group, including after normal exit.
// Host execution is trusted execution, not a sandbox: deliberately detached
// sessions and work submitted to external daemons are outside this group.
func runHostProcess(ctx context.Context, command *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = 2 * time.Second
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	var watcher sync.WaitGroup
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		select {
		case <-done:
			return
		case <-ctx.Done():
		}
		_ = unix.Kill(-command.Process.Pid, unix.SIGTERM)
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			_ = unix.Kill(-command.Process.Pid, unix.SIGKILL)
		}
	}()
	var info unix.Siginfo
	var waitErr error
	for {
		waitErr = unix.Waitid(unix.P_PID, command.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(waitErr, unix.EINTR) {
			break
		}
	}
	// Only the successful WNOWAIT result proves the leader still pins this ID.
	if waitErr != nil {
		close(done)
		watcher.Wait()
		return errors.Join(coopr.ErrCleanupFailed, waitErr, command.Wait(), ctx.Err())
	}
	// Stop background children before reaping the leader or draining pipe copies.
	killErr := unix.Kill(-command.Process.Pid, unix.SIGKILL)
	if errors.Is(killErr, unix.ESRCH) {
		killErr = nil
	}
	close(done)
	watcher.Wait()
	drainErr := drainHostGroup(command.Process.Pid)
	if drainErr != nil {
		drainErr = errors.Join(coopr.ErrCleanupFailed, drainErr)
	}
	err := command.Wait()
	return errors.Join(killErr, drainErr, err, ctx.Err())
}

func drainHostGroup(group int) error {
	deadline := time.Now().Add(3 * time.Second)
	for {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return err
		}
		live := false
		for _, entry := range entries {
			if _, err := strconv.Atoi(entry.Name()); err != nil {
				continue
			}
			data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
				continue
			}
			if err != nil {
				return err
			}
			end := strings.LastIndex(string(data), ") ")
			if end < 0 {
				return errors.New("invalid process status while draining host group")
			}
			fields := strings.Fields(string(data[end+2:]))
			if len(fields) < 3 {
				return errors.New("incomplete host process status")
			}
			if fields[2] == strconv.Itoa(group) && fields[0] != "Z" && fields[0] != "X" {
				live = true
				break
			}
		}
		if !live {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("host process group %d did not drain after cancellation", group)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
