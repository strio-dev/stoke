package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cooprwire "github.com/strio-dev/coopr/api"
)

func socketClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	return rawSocketClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/info" {
			_ = json.NewEncoder(w).Encode(cooprwire.Info{APIVersion: cooprwire.Version, Capabilities: []string{
				cooprwire.CapabilityExecute, cooprwire.CapabilityBuild, cooprwire.CapabilityComponent,
				cooprwire.CapabilityComponentLoad, cooprwire.CapabilityComponentLoadName,
				cooprwire.CapabilityPublish, cooprwire.CapabilityPublishOCILayout,
				cooprwire.CapabilityBuildPlatform, cooprwire.CapabilityRegistryCache,
				cooprwire.CapabilityOperationProgress, cooprwire.CapabilityNamedContexts,
				cooprwire.CapabilityRunMounts, cooprwire.CapabilityRunNetwork,
				cooprwire.CapabilityOptionalOCIOutput,
			}})
			return
		}
		handler.ServeHTTP(w, r)
	}))
}

// rawSocketClient lets negotiation tests supply the Info endpoint themselves.
func rawSocketClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "stoke-client-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	client, err := NewClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}
func TestLiteralProgramAndCacheResult(t *testing.T) {
	var got cooprwire.Request
	client := socketClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(cooprwire.Event{Type: "result", Result: &cooprwire.Result{ImageID: "image", Cache: &cooprwire.CacheStats{Hits: 2}}})
	}))
	literal := "${literal} quoted \" value\nnext"
	program := Program{Platform: "linux/amd64", States: []State{{ID: "one", From: Source{Image: "scratch"}, Operations: []Operation{
		{Kind: "env", Name: "VALUE", Value: literal},
		{Kind: "workdir", Path: "/work"},
		{Kind: "copy", Sources: []string{"entrée.txt"}, Destination: "/work/input", From: Source{Context: "fixtures"}},
		{Kind: "run", Args: []string{"echo", literal}},
	}}}, Output: "one"}
	result, err := client.Execute(context.Background(), program, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := cooprwire.Program{Platform: "linux/amd64", States: []cooprwire.State{{ID: "one", From: cooprwire.Source{Image: "scratch"}, Operations: []cooprwire.Operation{
		{Kind: "env", Name: "VALUE", Value: literal},
		{Kind: "workdir", Path: "/work"},
		{Kind: "copy", Sources: []string{"entrée.txt"}, Destination: "/work/input", From: cooprwire.Source{Context: "fixtures"}},
		{Kind: "run", Args: []string{"echo", literal}},
	}}}, Output: "one"}
	if got.Kind != "execute" || !reflect.DeepEqual(got.Program, &want) {
		t.Fatalf("literal program altered: %#v", got)
	}
	if !result.CacheAvailable || result.Cache.Hits != 2 {
		t.Fatalf("cache result %#v", result)
	}
}

// Coopr's public client owns malformed-wire parsing. Stoke verifies the
// observable error and cleanup classifications that it exposes to workflows.
func TestExecutionCleanupClassification(t *testing.T) {
	for _, test := range []struct {
		name    string
		ack     cooprwire.Status
		failure bool
	}{
		{name: "confirmed cancellation", ack: cooprwire.Status{State: "canceled"}},
		{name: "confirmed execution failure", ack: cooprwire.Status{State: "failed", Error: &cooprwire.Error{Code: "execution_failed", Message: "command failed"}}},
		{name: "unconfirmed cleanup", ack: cooprwire.Status{State: "running"}, failure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var deletes atomic.Int32
			client := socketClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deletes.Add(1)
					ack := test.ack
					ack.ID = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
					_ = json.NewEncoder(w).Encode(ack)
				}
			}))
			_, err := client.Execute(t.Context(), Program{}, Options{})
			if err == nil || errors.Is(err, ErrCleanupFailed) != test.failure || deletes.Load() != 1 {
				t.Fatalf("error=%v, DELETEs=%d; want cleanup failure=%v, one DELETE", err, deletes.Load(), test.failure)
			}
		})
	}
}

func TestCancellationWaitsForAcknowledgement(t *testing.T) {
	entered := make(chan struct{})
	deleteEntered := make(chan struct{})
	release := make(chan struct{})
	client := socketClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			close(deleteEntered)
			<-release
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			_ = json.NewEncoder(w).Encode(cooprwire.Status{ID: id, State: "canceled"})
			return
		}
		close(entered)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.Execute(ctx, Program{}, Options{}); done <- err }()
	<-entered
	cancel()
	<-deleteEntered
	select {
	case err := <-done:
		t.Fatalf("returned before cleanup acknowledgement: %v", err)
	default:
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("wrong cancellation: %v", err)
	}
}
func TestInfoVersion(t *testing.T) {
	client := rawSocketClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(cooprwire.Info{APIVersion: 2})
	}))
	if _, err := client.Info(context.Background()); err == nil {
		t.Fatal("accepted incompatible API")
	}
}

func TestTerminalCleanupFailure(t *testing.T) {
	var deletes atomic.Int32
	client := socketClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
		}
		_ = json.NewEncoder(w).Encode(cooprwire.Event{Type: "result", Result: &cooprwire.Result{ImageID: "partial-image"}, Error: &cooprwire.Error{Code: "cleanup_failed", Message: "worker reaping incomplete"}})
	}))
	result, err := client.Execute(t.Context(), Program{}, Options{})
	if !errors.Is(err, ErrCleanupFailed) || !strings.Contains(err.Error(), "worker reaping incomplete") || result.ImageID != "partial-image" || deletes.Load() != 0 {
		t.Fatalf("partial result/cleanup failure lost: %+v, %v; DELETEs=%d", result, err, deletes.Load())
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("cleanup failure classified as ordinary cancellation: %v", err)
	}
}

func TestCanceledExecutionWithFailedCleanupAcknowledgement(t *testing.T) {
	entered := make(chan struct{})
	client := socketClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			_ = json.NewEncoder(w).Encode(cooprwire.Status{ID: id, State: "failed", Error: &cooprwire.Error{Code: "cleanup_failed", Message: "worker still running"}})
			return
		}
		close(entered)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.Execute(ctx, Program{}, Options{}); done <- err }()
	<-entered
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrCleanupFailed) {
		t.Fatalf("expected canceled execution and cleanup failure: %v", err)
	}
}

func TestNamedContextCopyWireAndBuildOptions(t *testing.T) {
	var requests []cooprwire.Request
	client := socketClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request cooprwire.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		requests = append(requests, request)
		_ = json.NewEncoder(w).Encode(cooprwire.Event{Type: "result", Result: &cooprwire.Result{ImageID: "image"}})
	}))
	opts := Options{ContextDir: "/project", Contexts: map[string]string{"stoke-assets": "/component"}}
	program := Program{Output: "out", States: []State{{ID: "out", From: Source{Image: "scratch"}, Operations: []Operation{{Kind: "copy", Sources: []string{"scripts/check.sh"}, Destination: "/check.sh", From: Source{Context: "stoke-assets"}}}}}}
	if _, err := client.Execute(context.Background(), program, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Build(context.Background(), cooprwire.Build{Files: []string{"/component/build.coopr"}}, false, opts); err != nil {
		t.Fatal(err)
	}
	for _, request := range requests {
		if request.Options.ContextDir != "/project" || request.Options.Contexts["stoke-assets"] != "/component" {
			t.Fatalf("input roots combined/lost: %#v", request.Options)
		}
	}
	if requests[0].Program.States[0].Operations[0].From.Context != "stoke-assets" {
		t.Fatal("named COPY source lost")
	}
}

func replyCleanup(w http.ResponseWriter, r *http.Request, state string) {
	id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	_, _ = io.WriteString(w, `{"id":"`+id+`","state":"`+state+`"}`)
}

// A generic HTTP failure may follow admission. Keep its structured error while
// reconciling this invocation; explicit admission refusals are tested by the SDK.
func TestHTTPFailureRetainsIdentityAndReconciles(t *testing.T) {
	var deletes atomic.Int32
	client := socketClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
			replyCleanup(w, r, "canceled")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(cooprwire.Error{Code: "admission", Message: "preserve structured error"})
	}))
	_, err := client.Execute(t.Context(), Program{}, Options{})
	var remote *cooprwire.Error
	if !errors.As(err, &remote) || remote.Code != "admission" || deletes.Load() != 1 {
		t.Fatalf("uncertain HTTP failure lost identity or cleanup: %v; DELETEs=%d", err, deletes.Load())
	}
}

type shortLogWriter struct{}

func (shortLogWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

func TestShortLogWritesAndRedirectsAreUncertain(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		var deletes, followed atomic.Int32
		c := socketClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				deletes.Add(1)
				replyCleanup(w, r, "canceled")
			} else if r.URL.Path == "/replayed" {
				followed.Add(1)
				_, _ = io.WriteString(w, `{"type":"result","result":{}}`)
			} else if redirect {
				w.Header().Set("Location", "http://coopr/replayed")
				w.WriteHeader(http.StatusTemporaryRedirect)
			} else {
				_, _ = io.WriteString(w, "{\"type\":\"stdout\",\"data\":\"YWJj\"}\n{\"type\":\"result\",\"result\":{}}\n")
			}
		}))
		_, err := c.Execute(t.Context(), Program{}, Options{Stdout: shortLogWriter{}})
		if err == nil || deletes.Load() != 1 || followed.Load() != 0 || (!redirect && !errors.Is(err, io.ErrShortWrite)) {
			t.Fatal("uncertain delivery was accepted or replayed", redirect, err, deletes.Load(), followed.Load())
		}
	}
}
