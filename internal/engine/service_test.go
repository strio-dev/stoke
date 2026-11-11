package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cooprwire "github.com/strio-dev/coopr/api"
)

func TestServiceHelper(t *testing.T) {
	if os.Getenv("STOKE_ENGINE_HELPER") != "1" {
		return
	}
	socket := ""
	for i, arg := range os.Args {
		if arg == "--socket" && i+1 < len(os.Args) {
			socket = os.Args[i+1]
		}
	}
	if record := os.Getenv("STOKE_ENGINE_SOCKET_RECORD"); record != "" {
		// The parent cancels startup once this record appears. Publish the complete
		// path atomically so its cleanup assertion inspects the socket directory.
		if err := os.WriteFile(record+".tmp", []byte(socket), 0600); err != nil {
			os.Exit(6)
		}
		if err := os.Rename(record+".tmp", record); err != nil {
			os.Exit(6)
		}
	}
	if os.Getenv("STOKE_ENGINE_HELPER_MODE") == "exit" {
		os.Exit(23)
	}
	if os.Getenv("STOKE_ENGINE_HELPER_MODE") == "stall" {
		parent := os.NewFile(3, "parent")
		_, _ = io.Copy(io.Discard, parent)
		_ = parent.Close()
		os.Exit(0)
	}
	if os.Getenv("STOKE_ENGINE_HELPER_MODE") == "diagnostics" {
		var writes sync.WaitGroup
		for _, output := range []*os.File{os.Stdout, os.Stderr} {
			writes.Go(func() {
				for range 128 {
					if _, err := output.WriteString(output.Name() + "\n"); err != nil {
						os.Exit(5)
					}
				}
			})
		}
		writes.Wait()
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		os.Exit(4)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version := 1
		if os.Getenv("STOKE_ENGINE_HELPER_MODE") == "version" {
			version = 2
		}
		_ = json.NewEncoder(w).Encode(cooprwire.Info{APIVersion: version, Version: "helper", Capabilities: []string{"execute"}})
	})}
	go func() {
		parent := os.NewFile(3, "parent")
		_, _ = io.Copy(io.Discard, parent)
		_ = parent.Close()
		_ = server.Close()
	}()
	_ = server.Serve(listener)
	if os.Getenv("STOKE_ENGINE_HELPER_MODE") == "diagnostic-error" {
		if _, err := os.Stdout.WriteString("final diagnostic\n"); err != nil {
			os.Exit(5)
		}
	}
	os.Exit(0)
}
func helperExecutable(t *testing.T) string {
	t.Helper()
	t.Setenv("STOKE_ENGINE_HELPER", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "coopr")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!" + shell + "\nexec " + quote(executable) + " -test.run=TestServiceHelper -- \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return wrapper
}
func TestOwnedServiceLifecycle(t *testing.T) {
	wrapper := helperExecutable(t)
	for _, cancelRun := range []bool{false, true} {
		if cancelRun {
			// Keep lifecycle/cleanup coverage when the configured temporary
			// path is too long for a Unix socket.
			temp := filepath.Join(t.TempDir(), strings.Repeat("nested-", 20))
			if err := os.Mkdir(temp, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMPDIR", temp)
		}
		ctx, cancel := context.WithCancel(context.Background())
		service, err := Start(ctx, wrapper, io.Discard)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if service.Version != "helper" || service.Binary != wrapper {
			t.Fatalf("unexpected service identity: %#v", service)
		}
		if cancelRun {
			cancel()
			select {
			case <-service.done:
			case <-time.After(2 * time.Second):
				t.Fatal("context-owned service did not stop after cancellation")
			}
		}
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		cancel()
		if _, err := os.Lstat(filepath.Dir(service.Socket)); !os.IsNotExist(err) {
			t.Fatalf("socket directory retained after process exit: %v", err)
		}
	}
}

func TestRunOwnedServiceRetainsReadinessAndDrainsAfterCancellation(t *testing.T) {
	wrapper := helperExecutable(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := StartOwned(ctx, wrapper, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	if service.Info.APIVersion != cooprwire.Version || service.Info.Version != "helper" || len(service.Info.Capabilities) != 1 || service.Info.Capabilities[0] != "execute" {
		t.Fatalf("readiness information lost: %+v", service.Info)
	}
	cancel()
	// Cleanup requests need a live service after the run context is canceled.
	info, err := service.Client.Info(context.Background())
	if err != nil || info.Version != "helper" {
		t.Fatalf("ready service was closed before owner cleanup: info=%+v err=%v", info, err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunOwnedServiceStartupHonorsCancellation(t *testing.T) {
	wrapper := helperExecutable(t)
	t.Setenv("STOKE_ENGINE_HELPER_MODE", "stall")
	record := filepath.Join(t.TempDir(), "socket-path")
	t.Setenv("STOKE_ENGINE_SOCKET_RECORD", record)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		service *Service
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		service, err := StartOwned(ctx, wrapper, io.Discard)
		done <- outcome{service, err}
	}()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	var socket []byte
waiting:
	for {
		select {
		case <-deadline.C:
			t.Fatal("service helper did not start")
		case <-ticker.C:
			var err error
			socket, err = os.ReadFile(record)
			if err == nil {
				if !filepath.IsAbs(string(socket)) || filepath.Base(string(socket)) != "api.sock" {
					t.Fatalf("invalid service helper socket record: %q", socket)
				}
				break waiting
			}
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		}
	}
	cancel()
	select {
	case result := <-done:
		if result.service != nil || !errors.Is(result.err, context.Canceled) || errors.Is(result.err, ErrCleanupFailed) {
			t.Fatalf("startup cancellation: service=%v error=%v", result.service, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("startup ignored cancellation until its readiness deadline")
	}
	if _, err := os.Lstat(filepath.Dir(string(socket))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled startup retained socket directory: %v", err)
	}
}

func TestOwnedServiceStartupFailure(t *testing.T) {
	wrapper := helperExecutable(t)
	for _, mode := range []string{"exit", "version"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("STOKE_ENGINE_HELPER_MODE", mode)
			record := filepath.Join(t.TempDir(), "socket-path")
			t.Setenv("STOKE_ENGINE_SOCKET_RECORD", record)
			for range 5 {
				service, err := Start(context.Background(), wrapper, io.Discard)
				if service != nil || err == nil {
					t.Fatalf("startup failure returned service=%v error=%v", service, err)
				}
				if mode == "version" && !errors.Is(err, ErrUnsupportedVersion) {
					t.Fatalf("version failure lost: %v", err)
				}
				socket, readErr := os.ReadFile(record)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if _, statErr := os.Lstat(filepath.Dir(string(socket))); !os.IsNotExist(statErr) {
					t.Fatalf("failed startup retained socket directory: %v", statErr)
				}
			}
		})
	}
}

// Function values are deliberately not comparable, unlike pointer-backed loggers.
type diagnosticWriteFunc func([]byte) (int, error)

func (f diagnosticWriteFunc) Write(p []byte) (int, error) { return f(p) }

func TestOwnedServiceSerializesDiagnosticsAndRetainsWriterErrors(t *testing.T) {
	wrapper := helperExecutable(t)
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "capture", true: "writer-error"}[fail], func(t *testing.T) {
			t.Setenv("STOKE_ENGINE_HELPER_MODE", map[bool]string{false: "diagnostics", true: "diagnostic-error"}[fail])
			var output bytes.Buffer
			var writing, concurrent atomic.Bool
			writeErr := errors.New("diagnostic writer failed")
			writer := diagnosticWriteFunc(func(p []byte) (int, error) {
				if !writing.CompareAndSwap(false, true) {
					concurrent.Store(true)
					return len(p), nil
				}
				defer writing.Store(false)
				runtime.Gosched()
				if fail {
					return 0, writeErr
				}
				return output.Write(p)
			})
			service, err := StartOwned(t.Context(), wrapper, writer)
			if err != nil {
				t.Fatal(err)
			}
			err = service.Close()
			if concurrent.Load() {
				t.Fatal("stdout and stderr called the diagnostic writer concurrently")
			}
			if fail {
				if !errors.Is(err, writeErr) || !errors.Is(err, ErrCleanupFailed) {
					t.Fatalf("diagnostic error was lost or misclassified: %v", err)
				}
			} else if err != nil || strings.Count(output.String(), "/dev/stdout\n") != 128 || strings.Count(output.String(), "/dev/stderr\n") != 128 {
				t.Fatalf("diagnostic capture incomplete: error=%v output=%q", err, output.String())
			}
		})
	}
}
