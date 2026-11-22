package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSharedEnvironmentConsumersUsesResolvedLoweringIdentity(t *testing.T) {
	opts := nativeOptions(t)
	if err := os.Symlink("image.coopr", filepath.Join(opts.ContextDir, "alias.coopr")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(opts.ContextDir, "inputs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("inputs", filepath.Join(opts.ContextDir, "alias-inputs")); err != nil {
		t.Fatal(err)
	}
	w := fixture(t, `version: 1
tasks:
  first:
    cache: false
    build: {coopr: [image.coopr]}
    contexts: {assets: inputs}
    mounts: [{type: host, source: inputs, target: /host}]
    steps: [{run: [check], mounts: [{type: host, source: inputs, target: /step}]}]
  second:
    cache: false
    build: {coopr: [alias.coopr]}
    contexts: {assets: alias-inputs}
    steps: [{run: [other]}]
  gate: {image: scratch}
  gated:
    cache: false
    build: {coopr: [image.coopr]}
    contexts: {assets: inputs}
    needs: [gate]
    steps: [{run: [check]}]
  platform:
    cache: false
    platform: linux/arm64
    build: {coopr: [image.coopr]}
    contexts: {assets: inputs}
    steps: [{run: [check]}]
  cached:
    cache: true
    build: {coopr: [image.coopr]}
    contexts: {assets: inputs}
    steps: [{run: [check]}]
  data:
    cache: false
    build: {coopr: [image.coopr], args: {BASE: {from: gate}}}
    contexts: {assets: inputs}
    steps: [{run: [check]}]
  target:
    cache: false
    build: {coopr: [image.coopr], target: other}
    contexts: {assets: inputs}
    steps: [{run: [check]}]
`)
	plan, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := SharedEnvironmentConsumers(plan, opts.ContextDir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(groups, [][]string{{"first", "second"}}) {
		t.Fatalf("shared consumers = %v", groups)
	}
	after, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("environment planning mutated expanded definitions")
	}
}

func TestSharedEnvironmentConsumersMatrixConcreteNames(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  check:
    cache: false
    build: {coopr: [image.coopr]}
    strategy: {matrix: {mode: [one, two]}}
    steps: [{run: [check]}]
`)
	plan, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := SharedEnvironmentConsumers(plan, nativeOptions(t).ContextDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0]) != 2 {
		t.Fatalf("shared matrix = %v", groups)
	}
	for _, job := range plan.Groups[0].Jobs {
		found := false
		for _, name := range groups[0] {
			found = found || name == job.Name
		}
		if !found {
			t.Fatalf("concrete job %q absent from %v", job.Name, groups)
		}
	}
}

func TestSharedEnvironmentConsumersMissingSelectedFiles(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  selected: {build: {coopr: [missing.coopr]}, steps: [{run: [check]}]}
  available: {build: {coopr: [image.coopr]}, steps: [{run: [check]}]}
  unselected: {build: {coopr: [also-missing.coopr]}, steps: [{run: [check]}]}
`)
	plan, err := w.Expand([]string{"selected"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = SharedEnvironmentConsumers(plan, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), `task "selected" build files`) || !strings.Contains(err.Error(), "missing.coopr") {
		t.Fatalf("missing file error: %v", err)
	}
}

func TestSharedEnvironmentConsumersIgnoresUnselectedDefinitions(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  selected: {build: {coopr: [image.coopr]}, steps: [{run: [check]}]}
  unselected: {build: {coopr: [missing.coopr]}, steps: [{run: [check]}]}
`)
	plan, err := w.Expand([]string{"selected"})
	if err != nil {
		t.Fatal(err)
	}
	groups, err := SharedEnvironmentConsumers(plan, nativeOptions(t).ContextDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Fatalf("single selected consumer = %v", groups)
	}
}
