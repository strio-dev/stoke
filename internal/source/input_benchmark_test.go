package source

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/strio-dev/stoke/internal/artifact"
)

// Measure complete loading with fixed reusable-component shapes. The unused
// library declarations must still validate, but only called tasks are emitted.
func BenchmarkReusableInputsLoad(b *testing.B) {
	for _, fixture := range []struct {
		name          string
		calls, unused int
	}{{"simple", 1, 0}, {"repeated", 32, 64}} {
		b.Run(fixture.name, func(b *testing.B) {
			rootRef, libraryRef := pin("benchmark-root"), pin("benchmark-library")
			libraryTasks := map[string]any{
				"run": map[string]any{"image": "scratch", "env": map[string]any{"MODE": map[string]string{"input": "mode"}}},
			}
			variants := make([]string, 256)
			for n := range variants {
				variants[n] = fmt.Sprint(n)
			}
			for n := range fixture.unused {
				libraryTasks[fmt.Sprintf("unused_%03d", n)] = map[string]any{
					"image": "scratch", "strategy": map[string]any{"matrix": map[string]any{"variant": variants}},
				}
			}
			calls := map[string]any{}
			for n := range fixture.calls {
				calls[fmt.Sprintf("call_%03d", n)] = map[string]any{"uses": "lib/run", "with": map[string]string{"mode": fmt.Sprintf("mode_%03d", n)}}
			}
			marshal := func(value any) []byte {
				data, err := json.Marshal(value)
				if err != nil {
					b.Fatal(err)
				}
				return data
			}
			l := loader(map[string]*artifact.Document{
				rootRef:    {Data: marshal(map[string]any{"version": 1, "imports": map[string]string{"lib": libraryRef}, "tasks": calls})},
				libraryRef: {Data: marshal(map[string]any{"version": 1, "inputs": map[string]any{"mode": map[string]any{"required": true}}, "tasks": libraryTasks})},
			})
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				result, err := l.Load(context.Background(), "oci://"+rootRef)
				if err != nil {
					b.Fatal(err)
				}
				if len(result.Workflow.Tasks) != fixture.calls {
					b.Fatalf("emitted %d tasks; want %d", len(result.Workflow.Tasks), fixture.calls)
				}
				for n := range fixture.calls {
					name, value := fmt.Sprintf("call_%03d", n), fmt.Sprintf("mode_%03d", n)
					if got := result.Workflow.Tasks[name].Env["MODE"].Value; got == nil || *got != value {
						b.Fatalf("invocation %s did not retain its input", name)
					}
				}
				if err := result.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
