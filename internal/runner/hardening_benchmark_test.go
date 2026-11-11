package runner

import (
	"context"
	"fmt"
	"os"
	"testing"

	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
)

// These fixtures measure Stoke orchestration with no container execution. Keep
// their shape and four-job limit fixed when comparing baseline and changed code.
func benchmarkWorkflow(shape string) *workflow.Workflow {
	w := &workflow.Workflow{Version: 1, Tasks: map[string]workflow.Task{}}
	count := 512
	if shape == "matrix" {
		count = 32
	}
	for i := range count {
		name := fmt.Sprintf("task_%04d", i)
		task := workflow.Task{Image: "scratch"}
		if shape == "chain" && i > 0 {
			task.Image = ""
			task.From = fmt.Sprintf("task_%04d", i-1)
		}
		if shape == "matrix" {
			values := make([]string, 16)
			for n := range values {
				values[n] = fmt.Sprint(n)
			}
			task.Strategy = &workflow.Strategy{Matrix: &workflow.Matrix{Axes: map[string][]string{"variant": values}}}
		}
		w.Tasks[name] = task
	}
	return w
}

func BenchmarkWorkflowExpansion(b *testing.B) {
	for _, shape := range []string{"wide", "chain", "matrix"} {
		b.Run(shape, func(b *testing.B) {
			w := benchmarkWorkflow(shape)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				plan, err := w.Expand(nil)
				if err != nil || len(plan.Groups) != len(w.Tasks) {
					b.Fatalf("expand: %v", err)
				}
			}
		})
	}
}

func BenchmarkWorkflowScheduling(b *testing.B) {
	for _, shape := range []string{"wide", "chain", "matrix"} {
		b.Run(shape, func(b *testing.B) {
			w := benchmarkWorkflow(shape)
			plan, err := w.Expand(nil)
			if err != nil {
				b.Fatal(err)
			}
			root := b.TempDir()
			opts := Options{Jobs: DefaultJobs, ContextDir: root, CacheDir: root}
			engine := fakeEngine{execute: func(_ context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
				return coopr.Result{ImageID: string(p.Output)}, nil
			}}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				b.StopTimer()
				output, err := os.MkdirTemp(root, "outputs-")
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				items, err := schedule(context.Background(), w, plan, opts, root, output, engine)
				b.StopTimer()
				if err != nil || len(items) != 512 {
					b.Fatalf("schedule: jobs=%d err=%v", len(items), err)
				}
				for _, item := range items {
					if item.Status != "passed" {
						b.Fatalf("job %s: %s", item.Name, item.Status)
					}
				}
				if err := os.RemoveAll(output); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}
