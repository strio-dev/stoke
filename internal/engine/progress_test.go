package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cooprwire "github.com/strio-dev/coopr/api"
)

func testOperationProgress() OperationProgress {
	return OperationProgress{ID: "phase-1", Stage: "build", Operation: 2, Instruction: "copy", Phase: "lookup", State: "started", Started: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
}

func TestProgressEventsInterleaveWithLogsAndResult(t *testing.T) {
	for _, observe := range []bool{false, true} {
		t.Run(map[bool]string{false: "unobserved", true: "observed"}[observe], func(t *testing.T) {
			started := testOperationProgress()
			finished := started
			finished.State, finished.Cache, finished.DurationNanos = "completed", "hit", 50
			// Duration is monotonic elapsed time. Wall-clock adjustments need
			// not make the serialized timestamps agree with that duration.
			completed := started.Started.Add(-time.Second)
			finished.Completed = &completed
			client := socketClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				encoder := json.NewEncoder(w)
				for _, event := range []cooprwire.Event{
					{Type: "stdout", Data: []byte("output\n")},
					{Type: "progress", Progress: &started},
					{Type: "stderr", Data: []byte("building\n")},
					{Type: "progress", Progress: &finished},
					{Type: "result", Result: &cooprwire.Result{ImageID: "image", ProgressDropped: 7}},
				} {
					if err := encoder.Encode(event); err != nil {
						t.Error(err)
						return
					}
				}
			}))
			var stdout, stderr bytes.Buffer
			var events []OperationProgress
			options := Options{Stdout: &stdout, Stderr: &stderr}
			if observe {
				options.Progress = func(event OperationProgress) { events = append(events, event) }
			}
			result, err := client.Execute(context.Background(), Program{}, options)
			if err != nil {
				t.Fatal(err)
			}
			if stdout.String() != "output\n" || stderr.String() != "building\n" {
				t.Fatalf("logs changed: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if result.ImageID != "image" || result.ProgressDropped != 7 {
				t.Fatalf("terminal result changed: %#v", result)
			}
			if observe && (len(events) != 2 || events[0].State != "started" || events[1].State != "completed" || events[1].Cache != "hit" || events[1].DurationNanos != 50) {
				t.Fatalf("progress delivery changed: %#v", events)
			}
			if !observe && len(events) != 0 {
				t.Fatalf("nil callback observed progress: %#v", events)
			}
		})
	}
}

func TestProgressCallbackCancellationWaitsForCleanup(t *testing.T) {
	var acknowledged atomic.Bool
	progress := testOperationProgress()
	client := socketClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			acknowledged.Store(true)
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			_ = json.NewEncoder(w).Encode(cooprwire.Status{ID: id, State: "canceled"})
			return
		}
		_ = json.NewEncoder(w).Encode(cooprwire.Event{Type: "progress", Progress: &progress})
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.Execute(ctx, Program{}, Options{Progress: func(OperationProgress) { cancel() }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("callback cancellation not preserved: %v", err)
	}
	if !acknowledged.Load() {
		t.Fatal("callback cancellation returned before cleanup acknowledgement")
	}
}

func TestFailedAndCanceledOperationProgress(t *testing.T) {
	for _, state := range []string{"failed", "canceled"} {
		t.Run(state, func(t *testing.T) {
			progress := testOperationProgress()
			completed := progress.Started.Add(time.Millisecond)
			progress.State, progress.Completed, progress.DurationNanos, progress.Error = state, &completed, int64(time.Millisecond), "operation stopped"
			client := socketClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				encoder := json.NewEncoder(w)
				_ = encoder.Encode(cooprwire.Event{Type: "progress", Progress: &progress})
				_ = encoder.Encode(cooprwire.Event{Type: "result", Result: &cooprwire.Result{}})
			}))
			var observed []OperationProgress
			_, err := client.Execute(t.Context(), Program{}, Options{Progress: func(event OperationProgress) { observed = append(observed, event) }})
			if err != nil {
				t.Fatalf("valid terminal state rejected: %v", err)
			}
			if len(observed) != 1 || observed[0].State != state || observed[0].Error != progress.Error {
				t.Fatalf("terminal progress changed: %#v", observed)
			}
		})
	}
}
