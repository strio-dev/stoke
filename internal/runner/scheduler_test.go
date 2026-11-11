package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cooprwire "github.com/strio-dev/coopr/api"
	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
)

func TestJobsDefaultAndBounds(t *testing.T) {
	for _, tc := range []struct{ supplied, want int }{{0, 4}, {1, 1}, {256, 256}} {
		got, err := NormalizeJobs(tc.supplied)
		if err != nil || got != tc.want {
			t.Fatalf("NormalizeJobs(%d)=%d,%v want %d", tc.supplied, got, err, tc.want)
		}
	}
	w := fixture(t, "version: 1\ntasks: {work: {image: scratch}}\n")
	for _, jobs := range []int{-1, 257} {
		opts := testOptions(t)
		opts.Jobs = jobs
		opts.CooprBinary = "/does/not/exist"
		if report, err := Run(context.Background(), w, nil, opts); err == nil || report != nil {
			t.Fatalf("invalid jobs started service: report=%v err=%v", report, err)
		}
		if report, err := runLiteral(context.Background(), w, nil, opts, func(context.Context, coopr.Program, coopr.Options) (coopr.Result, error) {
			t.Fatal("invalid limit executed a job")
			return coopr.Result{}, nil
		}); err == nil || report != nil {
			t.Fatalf("invalid jobs started run: report=%v err=%v", report, err)
		}
		if _, err := os.Stat(opts.StateDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid limit created run state: %v", err)
		}
	}
}

func assertPlanReportOrder(t *testing.T, w *workflow.Workflow, report *Report) {
	t.Helper()
	plan, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{}
	for _, group := range plan.Groups {
		for _, job := range group.Jobs {
			want = append(want, job.Name)
		}
	}
	got := []string{}
	for _, item := range report.Tasks {
		got = append(got, item.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("report order=%v want plan=%v", got, want)
	}
}

func TestSchedulerGlobalAndMatrixBoundsShareSlots(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w := fixture(t, `version: 1
tasks:
  a:
    image: scratch
    strategy: {max-parallel: 2, matrix: {mode: [one, two, three]}}
  b:
    image: scratch
    strategy: {max-parallel: 2, matrix: {mode: [one, two, three]}}
  c: {image: scratch}
`)
	opts := testOptions(t)
	opts.Jobs = 3
	plan, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	gates := map[string]chan struct{}{}
	for _, group := range plan.Groups {
		for _, job := range group.Jobs {
			gates[job.Name] = make(chan struct{})
		}
	}
	entered := make(chan string, 7)
	done := make(chan matrixOutcome, 1)
	var active, maximum, activeA, activeB, maximumA, maximumB atomic.Int64
	updateMax := func(counter, maximum *atomic.Int64) {
		value := counter.Add(1)
		for old := maximum.Load(); value > old && !maximum.CompareAndSwap(old, value); old = maximum.Load() {
		}
	}
	go func() {
		report, err := runLiteral(ctx, w, nil, opts, func(ctx context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
			name := string(p.Output)
			updateMax(&active, &maximum)
			defer active.Add(-1)
			if p.Output != "c" {
				counter, maximum := &activeA, &maximumA
				if name[0] == 'b' {
					counter, maximum = &activeB, &maximumB
				}
				updateMax(counter, maximum)
				defer counter.Add(-1)
			}
			entered <- name
			select {
			case <-gates[name]:
			case <-ctx.Done():
				return coopr.Result{}, ctx.Err()
			}
			return coopr.Result{Platform: "linux/amd64", ImageID: name}, nil
		})
		done <- matrixOutcome{report, err}
	}()
	initial := map[byte]bool{}
	for range 3 {
		name := matrixReceive(t, ctx, entered)
		initial[name[0]] = true
	}
	if len(initial) != 3 || active.Load() != 3 || activeA.Load() != 1 || activeB.Load() != 1 {
		t.Fatalf("initial admissions did not share slots: tasks=%v active=%d a=%d b=%d", initial, active.Load(), activeA.Load(), activeB.Load())
	}
	close(gates["c"])
	replacement := matrixReceive(t, ctx, entered)
	if replacement[0] != 'a' || activeA.Load() != 2 {
		t.Fatalf("spare slot did not admit next ready matrix variant: %s a=%d", replacement, activeA.Load())
	}
	for name, gate := range gates {
		if name != "c" {
			close(gate)
		}
	}
	outcome := matrixReceive(t, ctx, done)
	if outcome.err != nil || active.Load() != 0 || maximum.Load() != 3 || maximumA.Load() > 2 || maximumB.Load() > 2 {
		t.Fatalf("bounds/drain err=%v active=%d max=%d a=%d b=%d", outcome.err, active.Load(), maximum.Load(), maximumA.Load(), maximumB.Load())
	}
	assertPlanReportOrder(t, w, outcome.report)
}

func TestSchedulerReleasesReadyBranchWithoutWaveBarrier(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w := fixture(t, `version: 1
tasks:
  fast: {image: scratch}
  child:
    from: fast
    steps: [{copy: {from: fast, sources: [/result], destination: /result}}]
  slow: {image: scratch}
`)
	opts := testOptions(t)
	opts.Jobs = 2
	entered := make(chan string, 3)
	fast, slow := make(chan struct{}), make(chan struct{})
	done := make(chan matrixOutcome, 1)
	go func() {
		report, err := runLiteral(ctx, w, nil, opts, func(ctx context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
			name := string(p.Output)
			entered <- name
			var gate <-chan struct{}
			switch name {
			case "fast":
				gate = fast
			case "slow":
				gate = slow
			case "child":
				if p.States[0].Operations[0].Kind != "copy" || p.States[0].From.Image != "containers-storage:fast-image" || p.States[0].Operations[0].From.Image != "containers-storage:fast-image" {
					return coopr.Result{}, errors.New("child did not receive successful producer snapshot")
				}
				return coopr.Result{Platform: "linux/amd64", ImageID: "child-image"}, nil
			}
			select {
			case <-gate:
				return coopr.Result{Platform: "linux/amd64", ImageID: name + "-image"}, nil
			case <-ctx.Done():
				return coopr.Result{}, ctx.Err()
			}
		})
		done <- matrixOutcome{report, err}
	}()
	initial := map[string]bool{matrixReceive(t, ctx, entered): true, matrixReceive(t, ctx, entered): true}
	if !initial["fast"] || !initial["slow"] {
		t.Fatalf("independent branch did not overlap: %v", initial)
	}
	close(fast)
	if name := matrixReceive(t, ctx, entered); name != "child" {
		t.Fatalf("ready child did not start while slow branch remained active: %s", name)
	}
	close(slow)
	outcome := matrixReceive(t, ctx, done)
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	assertPlanReportOrder(t, w, outcome.report)
}

func TestSchedulerGlobalLimitIncludesNativeAndPublication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w := fixture(t, `version: 1
tasks:
  base: {image: scratch}
  literal: {image: scratch}
  native: {coopr: {files: [image.coopr]}}
  publish: {publish: {from: base, destinations: [registry.invalid/image:test]}}
`)
	opts := nativeOptions(t)
	opts.Jobs = 2
	entered := make(chan string, 4)
	gates := map[string]chan struct{}{"literal": make(chan struct{}), "native": make(chan struct{}), "publish": make(chan struct{})}
	var active, maximum atomic.Int64
	execute := func(ctx context.Context, name string) (coopr.Result, error) {
		value := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); value > old && !maximum.CompareAndSwap(old, value); old = maximum.Load() {
		}
		if name == "base" {
			return coopr.Result{Platform: "linux/amd64", ImageID: "base-image"}, nil
		}
		entered <- name
		select {
		case <-gates[name]:
			return coopr.Result{Platform: "linux/amd64", ImageID: name}, nil
		case <-ctx.Done():
			return coopr.Result{}, ctx.Err()
		}
	}
	done := make(chan matrixOutcome, 1)
	go func() {
		report, err := run(ctx, w, nil, opts, fakeEngine{
			execute: func(ctx context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
				return execute(ctx, string(p.Output))
			},
			build: func(ctx context.Context, _ cooprwire.Build, _ bool, _ coopr.Options) (coopr.Result, error) {
				return execute(ctx, "native")
			},
			publish: func(ctx context.Context, p cooprwire.Publish, _ coopr.Options) (coopr.Result, error) {
				if p.Source != "base-image" {
					return coopr.Result{}, errors.New("publication missed successful producer snapshot")
				}
				return execute(ctx, "publish")
			},
		})
		done <- matrixOutcome{report, err}
	}()
	initial := map[string]bool{matrixReceive(t, ctx, entered): true, matrixReceive(t, ctx, entered): true}
	if !initial["literal"] || !initial["native"] || active.Load() != 2 {
		t.Fatalf("mixed requests did not share slots: %v active=%d", initial, active.Load())
	}
	close(gates["literal"])
	if name := matrixReceive(t, ctx, entered); name != "publish" || active.Load() != 2 {
		t.Fatalf("publication bypassed shared bound: %s active=%d", name, active.Load())
	}
	close(gates["native"])
	close(gates["publish"])
	outcome := matrixReceive(t, ctx, done)
	if outcome.err != nil || maximum.Load() != 2 || active.Load() != 0 {
		t.Fatalf("mixed-kind limit/drain err=%v max=%d active=%d", outcome.err, maximum.Load(), active.Load())
	}
	assertPlanReportOrder(t, w, outcome.report)
}

func TestSchedulerMatrixGateWaitsForEveryVariant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w := fixture(t, `version: 1
tasks:
  matrix:
    image: scratch
    strategy: {max-parallel: 2, matrix: {mode: [one, two]}}
  downstream: {image: scratch, needs: [matrix]}
  independent: {image: scratch}
`)
	opts := testOptions(t)
	opts.Jobs = 3
	entered := make(chan string, 4)
	first, second := make(chan struct{}), make(chan struct{})
	done := make(chan matrixOutcome, 1)
	go func() {
		report, err := runLiteral(ctx, w, nil, opts, func(ctx context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
			name := string(p.Output)
			entered <- name
			if mode := matrixMode(p); mode != "" {
				gate := first
				if mode == "two" {
					gate = second
				}
				select {
				case <-gate:
				case <-ctx.Done():
					return coopr.Result{}, ctx.Err()
				}
			}
			return coopr.Result{Platform: "linux/amd64", ImageID: name}, nil
		})
		done <- matrixOutcome{report, err}
	}()
	for range 3 {
		if name := matrixReceive(t, ctx, entered); name == "downstream" {
			t.Fatal("downstream bypassed active matrix variants")
		}
	}
	close(first)
	// The second variant remains blocked. A later independent completion must
	// not turn the incomplete matrix into a satisfied gate.
	select {
	case name := <-entered:
		t.Fatalf("incomplete matrix released %s", name)
	default:
	}
	close(second)
	if name := matrixReceive(t, ctx, entered); name != "downstream" {
		t.Fatalf("completed matrix released unexpected task %s", name)
	}
	outcome := matrixReceive(t, ctx, done)
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
}

func TestSchedulerFailFastDrainsLateSuccessAndCleanupFailure(t *testing.T) {
	for _, cleanupFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanupFailure=%t", cleanupFailure), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			w := fixture(t, `version: 1
tasks:
  fail: {image: scratch}
  slow: {image: scratch}
  downstream: {image: scratch, needs: [slow]}
`)
			opts := testOptions(t)
			opts.Jobs = 2
			entered := make(chan string, 2)
			failureGate, cleanupGate := make(chan struct{}), make(chan struct{})
			canceled := make(chan struct{})
			done := make(chan matrixOutcome, 1)
			failure := errors.New("assertion failed")
			go func() {
				report, err := runLiteral(ctx, w, nil, opts, func(ctx context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
					name := string(p.Output)
					entered <- name
					if name == "fail" {
						select {
						case <-failureGate:
							return coopr.Result{}, failure
						case <-ctx.Done():
							return coopr.Result{}, ctx.Err()
						}
					}
					if name != "slow" {
						return coopr.Result{}, errors.New("downstream admitted after failure")
					}
					<-ctx.Done()
					close(canceled)
					<-cleanupGate
					if cleanupFailure {
						return coopr.Result{}, errors.Join(ctx.Err(), coopr.ErrCleanupFailed)
					}
					return coopr.Result{Platform: "linux/amd64", ImageID: "late-success"}, nil
				})
				done <- matrixOutcome{report, err}
			}()
			matrixReceive(t, ctx, entered)
			matrixReceive(t, ctx, entered)
			close(failureGate)
			matrixReceive(t, ctx, canceled)
			select {
			case <-done:
				t.Fatal("scheduler returned before active cleanup completed")
			default:
			}
			close(cleanupGate)
			outcome := matrixReceive(t, ctx, done)
			if !errors.Is(outcome.err, failure) || errors.Is(outcome.err, coopr.ErrCleanupFailed) != cleanupFailure {
				t.Fatalf("lost error during drain: %v", outcome.err)
			}
			saved := savedReport(t, outcome.report)
			if saved.Status != "failed" || len(saved.Tasks) != 3 || !strings.Contains(saved.Error, "assertion failed") {
				t.Fatalf("persisted failure report: %+v", saved)
			}
			statuses := map[string]string{}
			for _, item := range saved.Tasks {
				statuses[item.Name] = item.Status
			}
			wantSlow := "passed"
			if cleanupFailure {
				wantSlow = "failed"
			}
			if statuses["fail"] != "failed" || statuses["slow"] != wantSlow || statuses["downstream"] != "skipped" {
				t.Fatalf("failure/late result status=%v", statuses)
			}
			assertPlanReportOrder(t, w, &saved)
		})
	}
}

func TestSchedulerCompleteAllDefersWorkflowAbortUntilGroupFinishes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w := fixture(t, `version: 1
tasks:
  matrix:
    image: scratch
    strategy: {max-parallel: 1, fail-fast: false, matrix: {mode: [a, b, c]}}
  other: {image: scratch}
  downstream: {image: scratch, needs: [matrix]}
`)
	opts := testOptions(t)
	opts.Jobs = 2
	opts.NoCache = true
	entered := make(chan string, 5)
	gateA, gateB, gateC := make(chan struct{}), make(chan struct{}), make(chan struct{})
	otherCanceled := make(chan struct{})
	done := make(chan matrixOutcome, 1)
	failure := errors.New("failed a")
	go func() {
		report, err := runLiteral(ctx, w, nil, opts, func(ctx context.Context, p coopr.Program, o coopr.Options) (coopr.Result, error) {
			if !o.Cache.NoCache {
				t.Error("global no-cache was lost")
			}
			mode := matrixMode(p)
			if mode == "" {
				entered <- string(p.Output)
				<-ctx.Done()
				close(otherCanceled)
				return coopr.Result{}, ctx.Err()
			}
			entered <- mode
			gate := map[string]chan struct{}{"a": gateA, "b": gateB, "c": gateC}[mode]
			select {
			case <-gate:
			case <-ctx.Done():
				return coopr.Result{}, ctx.Err()
			}
			if mode == "a" {
				return coopr.Result{}, failure
			}
			return coopr.Result{Platform: "linux/amd64", ImageID: mode}, nil
		})
		done <- matrixOutcome{report, err}
	}()
	initial := map[string]bool{matrixReceive(t, ctx, entered): true, matrixReceive(t, ctx, entered): true}
	if !initial["a"] || !initial["other"] {
		t.Fatalf("initial jobs=%v", initial)
	}
	close(gateA)
	if next := matrixReceive(t, ctx, entered); next != "b" {
		t.Fatalf("complete-all lost queued sibling: %s", next)
	}
	select {
	case <-otherCanceled:
		t.Fatal("matrix failure aborted workflow before complete-all siblings finished")
	default:
	}
	close(gateB)
	if next := matrixReceive(t, ctx, entered); next != "c" {
		t.Fatalf("complete-all lost final sibling: %s", next)
	}
	close(gateC)
	matrixReceive(t, ctx, otherCanceled)
	outcome := matrixReceive(t, ctx, done)
	if !errors.Is(outcome.err, failure) {
		t.Fatalf("complete-all lost failure: %v", outcome.err)
	}
	if len(outcome.report.Tasks) != 5 {
		t.Fatalf("complete-all report length=%d", len(outcome.report.Tasks))
	}
	for _, item := range outcome.report.Tasks {
		switch {
		case item.Task == "downstream":
			if item.Status != "skipped" {
				t.Fatal("failed matrix released downstream")
			}
		case item.Task == "matrix" && item.Matrix["mode"] != "a":
			if item.Status != "passed" {
				t.Fatalf("complete-all sibling not passed: %+v", item)
			}
		case item.Task == "matrix" && item.Status != "failed":
			t.Fatalf("complete-all lost failed variant: %+v", item)
		}
	}
}

func TestSchedulerOtherFailureCancelsCompleteAllMatrix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w := fixture(t, `version: 1
tasks:
  fail: {image: scratch}
  matrix:
    image: scratch
    strategy: {max-parallel: 2, fail-fast: false, matrix: {mode: [a, b, c]}}
  downstream: {image: scratch, needs: [matrix]}
`)
	opts := testOptions(t)
	opts.Jobs = 3
	entered := make(chan string, 5)
	failureGate := make(chan struct{})
	done := make(chan matrixOutcome, 1)
	failure := errors.New("independent failure")
	var active atomic.Int64
	go func() {
		report, err := runLiteral(ctx, w, nil, opts, func(ctx context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
			active.Add(1)
			defer active.Add(-1)
			mode := matrixMode(p)
			if mode == "" {
				entered <- string(p.Output)
				select {
				case <-failureGate:
					return coopr.Result{}, failure
				case <-ctx.Done():
					return coopr.Result{}, ctx.Err()
				}
			}
			entered <- mode
			<-ctx.Done()
			return coopr.Result{}, ctx.Err()
		})
		done <- matrixOutcome{report, err}
	}()
	initial := map[string]bool{}
	for range 3 {
		initial[matrixReceive(t, ctx, entered)] = true
	}
	if !initial["fail"] || !initial["a"] || !initial["b"] {
		t.Fatalf("initial active work=%v", initial)
	}
	close(failureGate)
	outcome := matrixReceive(t, ctx, done)
	if !errors.Is(outcome.err, failure) || active.Load() != 0 || len(entered) != 0 {
		t.Fatalf("unrelated failure did not stop/drain matrix: err=%v active=%d extra=%d", outcome.err, active.Load(), len(entered))
	}
	for _, item := range outcome.report.Tasks {
		if item.Task == "matrix" {
			want := "canceled"
			if item.Matrix["mode"] == "c" {
				want = "skipped"
			}
			if item.Status != want {
				t.Fatalf("complete-all ignored global failure: %+v want %s", item, want)
			}
		}
		if item.Task == "downstream" && item.Status != "skipped" {
			t.Fatal("globally canceled matrix released downstream")
		}
	}
}
