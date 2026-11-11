package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
)

func matrixFixture(t *testing.T, failFast bool, cache bool) *workflow.Workflow {
	t.Helper()
	return fixture(t, fmt.Sprintf(`version: 1
tasks:
  matrix:
    image: scratch
    cache: %t
    strategy:
      matrix:
        mode: [a, b, c, d, e]
      max-parallel: 2
      fail-fast: %t
    steps:
      - run: [check]
  downstream:
    image: scratch
    needs: [matrix]
`, cache, failFast))
}

func matrixMode(p coopr.Program) string {
	for _, op := range p.States[0].Operations {
		if op.Kind == "env" && op.Name == "STOKE_MATRIX_MODE" {
			return op.Value
		}
	}
	return ""
}

type matrixOutcome struct {
	report *Report
	err    error
}

func matrixReceive[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatalf("matrix test synchronization timed out: %v", ctx.Err())
		var zero T
		return zero
	}
}

func TestMatrixOverlapBoundAndStableReport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w := matrixFixture(t, true, true)
	opts := testOptions(t)
	var writer bytes.Buffer
	opts.Stdout = &writer
	opts.Stderr = &writer
	entered := make(chan string, 5)
	gates := map[string]chan struct{}{}
	for _, mode := range []string{"a", "b", "c", "d", "e"} {
		gates[mode] = make(chan struct{})
	}
	done := make(chan matrixOutcome, 1)
	var active, maxActive atomic.Int64
	var sharedStore coopr.StoreOptions
	// Store identity is sent out through a channel, avoiding concurrent test writes.
	stores := make(chan coopr.StoreOptions, 5)
	go func() {
		report, err := runLiteral(ctx, w, []string{"matrix"}, opts, func(ctx context.Context, p coopr.Program, o coopr.Options) (coopr.Result, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for old := maxActive.Load(); n > old && !maxActive.CompareAndSwap(old, n); old = maxActive.Load() {
			}
			mode := matrixMode(p)
			stores <- o.Store
			entered <- mode
			if _, err := fmt.Fprintf(o.Stdout, "variant %s stdout\n", mode); err != nil {
				return coopr.Result{}, err
			}
			if _, err := fmt.Fprintf(o.Stderr, "variant %s stderr\n", mode); err != nil {
				return coopr.Result{}, err
			}
			select {
			case <-gates[mode]:
			case <-ctx.Done():
				return coopr.Result{}, ctx.Err()
			}
			return coopr.Result{ImageID: mode, OCI: o.Output.OCI, Filesystem: o.Output.Filesystem}, nil
		})
		done <- matrixOutcome{report, err}
	}()
	seen := map[string]bool{}
	seen[matrixReceive(t, ctx, entered)] = true
	seen[matrixReceive(t, ctx, entered)] = true
	if active.Load() != 2 {
		t.Errorf("expected two overlapping Execute calls, got %d", active.Load())
	}
	// Finish b before a, and require its replacement to start before a is
	// released. This proves report order is independent of completion order.
	for _, transition := range [][2]string{{"b", "c"}, {"a", "d"}, {"c", "e"}} {
		close(gates[transition[0]])
		mode := matrixReceive(t, ctx, entered)
		if mode != transition[1] {
			t.Fatalf("replacement %s want %s", mode, transition[1])
		}
		seen[mode] = true
	}
	close(gates["d"])
	close(gates["e"])
	outcome := matrixReceive(t, ctx, done)
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if len(seen) != 5 || maxActive.Load() != 2 || active.Load() != 0 {
		t.Fatalf("seen=%v max=%d remaining=%d", seen, maxActive.Load(), active.Load())
	}
	for i := 0; i < 5; i++ {
		store := matrixReceive(t, ctx, stores)
		if i == 0 {
			sharedStore = store
		} else if !reflect.DeepEqual(store, sharedStore) {
			t.Error("variant stores differ")
		}
	}
	plan, err := w.Expand([]string{"matrix"})
	if err != nil {
		t.Fatal(err)
	}
	for i, item := range outcome.report.Tasks {
		if item.Name != plan.Groups[0].Jobs[i].Name || item.Task != "matrix" || item.Matrix["mode"] != plan.Groups[0].Jobs[i].Matrix["mode"] || item.Status != "passed" {
			t.Fatalf("unstable or incomplete result %#v", item)
		}
	}
	if writer.Len() == 0 {
		t.Error("shared writers received no output")
	}
}

func TestMatrixFailFastCancelsAndDrains(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w := matrixFixture(t, true, false)
	entered := make(chan string, 5)
	release := make(chan struct{})
	drained := make(chan struct{})
	done := make(chan matrixOutcome, 1)
	failure := errors.New("variant assertion failed")
	go func() {
		report, err := runLiteral(ctx, w, []string{"downstream"}, testOptions(t), func(ctx context.Context, p coopr.Program, o coopr.Options) (coopr.Result, error) {
			mode := matrixMode(p)
			entered <- mode
			if !o.Cache.NoCache {
				t.Error("cache:false variant allowed cache")
			}
			switch mode {
			case "a":
				select {
				case <-release:
					return coopr.Result{}, failure
				case <-ctx.Done():
					return coopr.Result{}, ctx.Err()
				}
			case "b":
				<-ctx.Done()
				close(drained)
				return coopr.Result{}, ctx.Err()
			default:
				return coopr.Result{}, fmt.Errorf("queued or downstream job unexpectedly started: %s", p.Output)
			}
		})
		done <- matrixOutcome{report, err}
	}()
	first, second := matrixReceive(t, ctx, entered), matrixReceive(t, ctx, entered)
	if first == second {
		t.Fatal("same variant started twice")
	}
	close(release)
	outcome := matrixReceive(t, ctx, done)
	matrixReceive(t, ctx, drained)
	if !errors.Is(outcome.err, failure) {
		t.Fatalf("lost root failure: %v", outcome.err)
	}
	if len(outcome.report.Tasks) != 6 {
		t.Fatalf("report length=%d", len(outcome.report.Tasks))
	}
	for _, item := range outcome.report.Tasks {
		if item.Task == "downstream" {
			if item.Status != "skipped" {
				t.Errorf("downstream ran: %+v", item)
			}
			continue
		}
		expected := map[string]string{"a": "failed", "b": "canceled", "c": "skipped", "d": "skipped", "e": "skipped"}[item.Matrix["mode"]]
		if item.Status != expected {
			t.Errorf("variant %s status %s want %s", item.Matrix["mode"], item.Status, expected)
		}
	}
}

func TestMatrixParentCancellationDrainsActiveVariants(t *testing.T) {
	deadline, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	ctx, cancel := context.WithCancel(deadline)
	defer cancel()
	entered := make(chan struct{}, 5)
	done := make(chan matrixOutcome, 1)
	var active atomic.Int64
	go func() {
		report, err := runLiteral(ctx, matrixFixture(t, false, true), nil, testOptions(t), func(ctx context.Context, _ coopr.Program, _ coopr.Options) (coopr.Result, error) {
			active.Add(1)
			defer active.Add(-1)
			entered <- struct{}{}
			<-ctx.Done()
			return coopr.Result{}, ctx.Err()
		})
		done <- matrixOutcome{report, err}
	}()
	matrixReceive(t, deadline, entered)
	matrixReceive(t, deadline, entered)
	cancel()
	outcome := matrixReceive(t, deadline, done)
	if !errors.Is(outcome.err, context.Canceled) || active.Load() != 0 {
		t.Fatalf("err=%v undrained=%d", outcome.err, active.Load())
	}
	for _, item := range outcome.report.Tasks {
		if item.Status != "canceled" && item.Status != "skipped" {
			t.Errorf("canceled variant %#v", item)
		}
	}
}
