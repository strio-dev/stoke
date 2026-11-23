package runner

import (
	"context"
	"strings"
	"testing"

	coopr "github.com/strio-dev/stoke/internal/engine"
)

func TestStandaloneCIGateUsesWorkflowOutputDemand(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  test:
    image: scratch
    cache: false
    steps: [{run: [test]}]
  finish: {image: scratch, needs: [test]}
  unrelated:
    image: scratch
    platform: {matrix: arch}
    strategy: {matrix: {arch: [not-a-platform]}}
`)
	request, err := SelectCI(w, []string{"test"})
	if err != nil {
		t.Fatal(err)
	}
	whole, err := w.Expand(request.Targets)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Expand(nil); err == nil {
		t.Fatal("unrelated matrix fixture must fail runtime platform resolution")
	}
	opts := testOptions(t)
	opts.CacheTo = []string{"registry.invalid/cache"}
	caps, err := RequiredCapabilities(w, whole, &request, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(caps, ","), "optional-oci-output") {
		t.Fatalf("standalone planned capability lost: %v", caps)
	}
	_, err = runCI(t.Context(), w, request, opts, fakeEngine{execute: func(_ context.Context, _ coopr.Program, native coopr.Options) (coopr.Result, error) {
		if native.Output.OCI != "" || native.Cache.LocalDir != "" || len(native.Cache.RegistryTo) != 0 || !native.Cache.NoCache {
			t.Fatalf("standalone gate retained unused publication: %#v %#v", native.Output, native.Cache)
		}
		return coopr.Result{ImageID: "native", Platform: "linux/amd64"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStagingFailureRecordsEveryUnstartedTask(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  first:
    platform: linux/amd64
    host: {tools: [/bin/sh], inputs: [{path: missing}]}
    steps: [{run: [/bin/sh, -c, true]}]
  second:
    image: scratch
    needs: [first]
`)
	plan, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	options := testOptions(t)
	report, err := runPrepared(t.Context(), w, plan, options, fakeEngine{execute: func(context.Context, coopr.Program, coopr.Options) (coopr.Result, error) {
		t.Fatal("staging failure executed a task")
		return coopr.Result{}, nil
	}})
	if err == nil || report == nil || report.Status != "failed" || len(report.Tasks) != 2 {
		t.Fatalf("missing staging outcome: %#v %v", report, err)
	}
	saved := savedReport(t, report)
	if saved.Status != report.Status || saved.Error != report.Error {
		t.Fatalf("persisted staging report differs: %#v %#v", report, saved)
	}
	for _, task := range saved.Tasks {
		if task.Status != "skipped" {
			t.Fatalf("unstarted task lost skipped outcome: %#v", task)
		}
	}
}
