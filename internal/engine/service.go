package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	cooprwire "github.com/strio-dev/coopr/api"
)

type Service struct {
	Client                  *Client
	Info                    cooprwire.Info
	Binary, Version, Socket string
	cmd                     *exec.Cmd
	parent                  *os.File
	directory               string
	done                    chan struct{}
	waitErr                 error
	once                    sync.Once
	closeErr                error
}

// Start ties the service lifetime to ctx. Call Close to wait for cleanup and
// release its client and private socket directory.
func Start(ctx context.Context, binary string, diagnostics io.Writer) (*Service, error) {
	return start(ctx, ctx, binary, diagnostics)
}

// StartOwned observes ctx during startup, then leaves the ready service alive
// until Close. A workflow uses this lifetime to acknowledge and drain canceled
// execution requests before closing the service's parent pipe.
func StartOwned(ctx context.Context, binary string, diagnostics io.Writer) (*Service, error) {
	return start(ctx, context.WithoutCancel(ctx), binary, diagnostics)
}

func start(ctx, lifetime context.Context, binary string, diagnostics io.Writer) (service *Service, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if binary == "" {
		binary = "coopr"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("coopr API executable: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, err
	}
	parent, err := filepath.Abs(os.TempDir())
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp(parent, "stoke-engine-")
	if err != nil {
		return nil, err
	}
	// Unix socket names have a smaller limit than filesystem paths. Keep only
	// this private socket directory short; the engine's TMPDIR is unchanged.
	if len(filepath.Join(directory, "api.sock")) >= len(syscall.RawSockaddrUnix{}.Path) {
		if err := os.Remove(directory); err != nil {
			return nil, err
		}
		directory, err = os.MkdirTemp("/tmp", "stoke-engine-")
		if err != nil {
			return nil, err
		}
	}
	spawned := false
	defer func() {
		if retErr != nil && !spawned {
			_ = os.RemoveAll(directory)
		}
	}()
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(directory, "api.sock")
	cmd := exec.Command(resolved, "system", "service", "--socket", socket, "--parent-fd", "3")
	cmd.ExtraFiles = []*os.File{reader}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if diagnostics != nil {
		// A shared comparable writer lets os/exec serialize both streams, even
		// when the underlying diagnostic writer itself is not comparable.
		shared := &struct{ io.Writer }{diagnostics}
		cmd.Stdout = shared
		cmd.Stderr = shared
	}
	if err = cmd.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, err
	}
	spawned = true
	_ = reader.Close()
	client, err := NewClient(socket)
	if err != nil {
		_ = writer.Close()
		_ = cmd.Wait()
		return nil, err
	}
	service = &Service{Client: client, Binary: resolved, Socket: socket, cmd: cmd, parent: writer, directory: directory, done: make(chan struct{})}
	owned := service
	go func() { owned.waitErr = cmd.Wait(); close(owned.done) }()
	go func() {
		select {
		case <-lifetime.Done():
			_ = writer.Close()
		case <-owned.done:
		}
	}()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), service.Close())
		case <-service.done:
			return nil, errors.Join(errors.New("coopr service exited before API readiness; API v1 is required"), service.waitErr, service.Close())
		case <-deadline.C:
			return nil, errors.Join(errors.New("coopr API readiness timed out"), service.Close())
		case <-ticker.C:
			infoCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
			info, infoErr := client.Info(infoCtx)
			cancel()
			if errors.Is(infoErr, ErrUnsupportedVersion) {
				return nil, errors.Join(infoErr, service.Close())
			}
			if infoErr != nil {
				continue
			}
			if !slices.Contains(info.Capabilities, "execute") {
				return nil, errors.Join(errors.New("coopr API does not support literal execution"), service.Close())
			}
			if err := ctx.Err(); err != nil {
				return nil, errors.Join(err, service.Close())
			}
			service.Info = info
			service.Version = info.Version
			return service, nil
		}
	}
}
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		_ = s.parent.Close()
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-s.done:
			if s.waitErr != nil {
				s.closeErr = errors.Join(ErrCleanupFailed, s.waitErr)
			}
		case <-timer.C:
			_ = s.cmd.Process.Signal(syscall.SIGTERM)
			grace := time.NewTimer(10 * time.Second)
			select {
			case <-s.done:
				s.closeErr = errors.Join(ErrCleanupFailed, errors.New("coopr service exceeded graceful shutdown deadline"), s.waitErr)
			case <-grace.C:
				_ = s.cmd.Process.Kill()
				select {
				case <-s.done:
					s.closeErr = errors.Join(ErrCleanupFailed, errors.New("coopr service forced termination; workload cleanup is unconfirmed"), s.waitErr)
				case <-time.After(5 * time.Second):
					s.closeErr = errors.Join(ErrCleanupFailed, errors.New("coopr service did not exit; workload cleanup is unconfirmed"))
				}
			}
			grace.Stop()
		}
		select {
		case <-s.done:
			s.Client.CloseIdleConnections()
			s.closeErr = errors.Join(s.closeErr, os.RemoveAll(s.directory))
		default:
		}
	})
	return s.closeErr
}
