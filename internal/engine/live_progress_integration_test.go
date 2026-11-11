package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/strio-dev/coopr/api"
)

// This proves delivery through the actual Unix HTTP service/public SDK while
// both isolated stage workers and their enclosing plan worker remain blocked.
func TestRealServiceStreamsWorkerProgressBeforeCompletion(t *testing.T) {
	if testing.Short() {
		t.Skip("requires an explicitly selected native engine")
	}
	binary, evidence := os.Getenv("STOKE_COOPR_BINARY"), os.Getenv("STOKE_HOST_EVIDENCE_DIR")
	if binary == "" || evidence == "" {
		t.Fatal("set STOKE_COOPR_BINARY and STOKE_HOST_EVIDENCE_DIR")
	}
	for _, cancelBuild := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelBuild), func(t *testing.T) {
			root, err := os.MkdirTemp(evidence, "live-http-progress-")
			if err != nil {
				t.Fatal(err)
			}
			t.Log("Retained real service progress evidence:", root)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			requested := make(chan string, 2)
			var mu sync.Mutex
			seen := make(map[string]bool)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				if !seen[r.URL.Path] {
					seen[r.URL.Path] = true
					requested <- r.URL.Path
				}
				mu.Unlock()
				select {
				case <-release:
					_, _ = io.WriteString(w, "live worker progress proof\n")
				case <-r.Context().Done():
				}
			}))
			defer server.Close()
			defer unblock()
			caller := filepath.Join(root, "caller")
			if err := os.Mkdir(caller, 0700); err != nil {
				t.Fatal(err)
			}
			definition := filepath.Join(caller, "image.coopr")
			if err := os.WriteFile(definition, []byte(fmt.Sprintf(`from "scratch" as="left" { add %q "/left"; }
from "scratch" as="right" { add %q "/right"; }
from "scratch" { copy "/left" "/left" from="left"; copy "/right" "/right" from="right"; }
`, server.URL+"/left", server.URL+"/right")), 0600); err != nil {
				t.Fatal(err)
			}
			log, err := os.OpenFile(filepath.Join(root, "engine.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = log.Close() }()
			service, err := StartOwned(ctx, binary, log)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := service.Close(); err != nil {
					t.Error("service cleanup", err)
				}
			}()
			var events []OperationProgress
			notices := make(chan OperationProgress, 2048)
			type outcome struct {
				result Result
				err    error
			}
			done := make(chan outcome, 1)
			layout := filepath.Join(root, "image")
			go func() {
				result, err := service.Client.Build(ctx, api.Build{Files: []string{definition}}, false, Options{
					Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), Driver: "vfs"},
					ContextDir: caller, Isolation: "rootless", Network: "none", Jobs: 2,
					Output: Output{OCI: layout}, Stdout: log, Stderr: log,
					Progress: func(event OperationProgress) {
						mu.Lock()
						events = append(events, event)
						mu.Unlock()
						notices <- event
					},
				})
				done <- outcome{result: result, err: err}
			}()
			joined := false
			defer func() {
				cancel()
				unblock()
				server.CloseClientConnections()
				if !joined {
					select {
					case <-done:
					case <-time.After(30 * time.Second):
						t.Error("execution did not join after cancellation")
					}
				}
			}()
			for range 2 {
				select {
				case <-requested:
				case result := <-done:
					joined = true
					t.Fatal("execution ended before both ADD requests blocked", result.err)
				case <-ctx.Done():
					t.Fatal("both ADD requests did not reach the fixture", ctx.Err())
				}
			}
			pending := make(map[string]bool)
			for len(pending) < 2 {
				select {
				case event := <-notices:
					if event.Instruction == "add" && event.Phase == "prepare" && event.State == "started" {
						pending[event.ID] = true
					}
				case result := <-done:
					joined = true
					t.Fatal("execution ended before live stage events", result.err)
				case <-ctx.Done():
					t.Fatal("worker events were not delivered before completion", ctx.Err())
				}
			}
			if cancelBuild {
				cancel()
			} else {
				unblock()
			}
			var result outcome
			select {
			case result = <-done:
				joined = true
			case <-time.After(30 * time.Second):
				t.Fatal("execution failed to finish after release/cancellation")
			}
			if cancelBuild {
				if !errors.Is(result.err, context.Canceled) || errors.Is(result.err, ErrCleanupFailed) {
					t.Fatal("execution cancellation was not acknowledged", result.err)
				}
				if _, err := os.Lstat(layout); !os.IsNotExist(err) {
					t.Fatal("canceled execution published an image", err)
				}
			} else if result.err != nil || result.result.OCI == "" {
				t.Fatal("released execution failed", result.result, result.err)
			}
			mu.Lock()
			captured := append([]OperationProgress(nil), events...)
			mu.Unlock()
			completions := make(map[string]int)
			for _, event := range captured {
				if pending[event.ID] && event.State != "started" {
					completions[event.ID]++
					if completions[event.ID] != 1 || cancelBuild && event.State == "completed" {
						t.Fatal("duplicate or fabricated completion", event)
					}
				}
			}
			if !cancelBuild {
				for id := range pending {
					if completions[id] != 1 {
						t.Fatal("missing live phase completion", id)
					}
				}
			}
			record := map[string]any{"status": "PASS", "engine": binary, "canceled": cancelBuild, "observedBeforeRelease": pending, "events": captured, "result": result.result}
			data, err := json.MarshalIndent(record, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "result.json"), append(data, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
		})
	}
}
