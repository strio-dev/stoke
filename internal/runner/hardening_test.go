package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
)

func TestFastDependencyChainCompletes(t *testing.T) {
	w := benchmarkWorkflow("chain")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for attempt := range 8 {
		report, err := runLiteral(ctx, w, nil, testOptions(t), func(_ context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
			return coopr.Result{Platform: "linux/amd64", ImageID: string(p.Output)}, nil
		})
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if len(report.Tasks) != 512 || report.Status != "passed" {
			t.Fatalf("attempt %d: status=%s tasks=%d", attempt, report.Status, len(report.Tasks))
		}
	}
}

func TestSchedulerGivesWaitingGroupNextAvailableSlot(t *testing.T) {
	w := &workflow.Workflow{Version: 1, Tasks: map[string]workflow.Task{}}
	for _, name := range []string{"a", "b", "c", "d"} {
		w.Tasks[name] = workflow.Task{Image: "scratch", Strategy: &workflow.Strategy{MaxParallel: 1, Matrix: &workflow.Matrix{Axes: map[string][]string{"mode": {"first", "second"}}}}}
	}
	w.Tasks["z"] = workflow.Task{Image: "scratch"}
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	entered := make(chan string, len(gates))
	done := make(chan error, 1)
	opts := testOptions(t)
	go func() {
		_, err := runLiteral(ctx, w, nil, opts, func(ctx context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
			name := string(p.Output)
			entered <- name
			select {
			case <-gates[name]:
				return coopr.Result{Platform: "linux/amd64", ImageID: name}, nil
			case <-ctx.Done():
				return coopr.Result{}, ctx.Err()
			}
		})
		done <- err
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("scheduler cancellation: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("scheduler did not drain canceled jobs")
		}
	}()
	initial := map[string]bool{}
	for range DefaultJobs {
		select {
		case name := <-entered:
			initial[name] = true
		case <-ctx.Done():
			t.Fatal("initial jobs did not start")
		}
	}
	for _, group := range plan.Groups[:DefaultJobs] {
		if !initial[group.Jobs[0].Name] {
			t.Fatalf("initial slots=%v", initial)
		}
	}
	close(gates[plan.Groups[0].Jobs[0].Name])
	select {
	case name := <-entered:
		if name != "z" {
			t.Fatalf("waiting group z lost the next slot to %q", name)
		}
	case <-ctx.Done():
		t.Fatal("waiting group did not start")
	}
}

func TestReportPersistenceFailureMarksRunFailed(t *testing.T) {
	for _, executionFails := range []bool{false, true} {
		t.Run(fmt.Sprint(executionFails), func(t *testing.T) {
			failure := errors.New("workload failed")
			w := fixture(t, "version: 1\ntasks: {work: {image: scratch}}\n")
			report, err := runLiteral(context.Background(), w, nil, testOptions(t), func(_ context.Context, _ coopr.Program, o coopr.Options) (coopr.Result, error) {
				if err := os.Mkdir(filepath.Join(filepath.Dir(o.Store.GraphRoot), "run.json"), 0700); err != nil {
					t.Error(err)
				}
				if executionFails {
					return coopr.Result{}, failure
				}
				return coopr.Result{Platform: "linux/amd64", ImageID: "success"}, nil
			})
			if err == nil || report == nil || report.Status != "failed" || report.Error != err.Error() {
				t.Fatalf("report=%+v err=%v", report, err)
			}
			if executionFails && !errors.Is(err, failure) {
				t.Fatalf("persistence error lost execution failure: %v", err)
			}
			if !strings.Contains(report.Error, "run.json") {
				t.Fatalf("report omitted persistence error: %s", report.Error)
			}
		})
	}
}

func TestRunPreflightPrecedesEngineStartup(t *testing.T) {
	w := fixture(t, "version: 1\ntasks: {work: {image: scratch}}\n")
	for _, scenario := range []string{"missing-context", "overlap", "existing-output"} {
		t.Run(scenario, func(t *testing.T) {
			opts := testOptions(t)
			opts.CooprBinary = "/missing-engine-for-preflight-test"
			switch scenario {
			case "missing-context":
				opts.ContextDir = filepath.Join(opts.ContextDir, "missing")
			case "overlap":
				opts.CacheDir = filepath.Join(opts.ContextDir, "cache")
			case "existing-output":
				opts.OutputDir = t.TempDir()
			}
			report, err := Run(context.Background(), w, nil, opts)
			if err == nil || report != nil || strings.Contains(err.Error(), opts.CooprBinary) {
				t.Fatalf("invalid workflow reached engine startup: report=%+v err=%v", report, err)
			}
			if _, err := os.Stat(opts.StateDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid workflow created state: %v", err)
			}
		})
	}
}

func TestReportReplacementDoesNotFollowExistingSymlink(t *testing.T) {
	w := fixture(t, "version: 1\ntasks: {work: {image: scratch}}\n")
	victim := filepath.Join(t.TempDir(), "keep.txt")
	if err := os.WriteFile(victim, []byte("user data"), 0644); err != nil {
		t.Fatal(err)
	}
	report, err := runLiteral(context.Background(), w, nil, testOptions(t), func(_ context.Context, _ coopr.Program, o coopr.Options) (coopr.Result, error) {
		if err := os.Symlink(victim, filepath.Join(filepath.Dir(o.Store.GraphRoot), "run.json")); err != nil {
			t.Error(err)
		}
		return coopr.Result{Platform: "linux/amd64", ImageID: "success"}, nil
	})
	if err != nil || report.Status != "passed" {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	data, err := os.ReadFile(victim)
	if err != nil || string(data) != "user data" {
		t.Fatalf("report write changed symlink target: %q %v", data, err)
	}
	info, err := os.Lstat(filepath.Join(report.Directory, "run.json"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("report was not atomically replaced with a private regular file: %v %v", info, err)
	}
	if savedReport(t, report).Status != "passed" {
		t.Fatal("replacement report was not persisted")
	}
}
