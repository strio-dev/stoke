package ci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	cooprwire "github.com/strio-dev/coopr/api"
	"github.com/strio-dev/stoke/internal/assets"
	"github.com/strio-dev/stoke/internal/workflow"
)

const graphFixture = `version: 1
tasks:
  build: {image: scratch}
  lint: {image: scratch, cache: false, needs: [build]}
  tests:
    image: scratch
    needs: [build]
    cache: false
    strategy:
      fail-fast: false
      max-parallel: 2
      matrix: {mode: [debug, release]}
  package: {image: scratch, needs: [lint, tests]}
`

func TestCIRejectsOversizedIdentityBeforeExecution(t *testing.T) {
	w := &workflow.Workflow{Version: 1, Tasks: map[string]workflow.Task{strings.Repeat("a", 256): {Image: "scratch"}}}
	if _, err := Build(w, Config{}); err == nil || !strings.Contains(err.Error(), "255 bytes") {
		t.Fatalf("oversized task could reach runtime: %v", err)
	}
}

func decodeWorkflow(t *testing.T, text string) *workflow.Workflow {
	t.Helper()
	w, err := workflow.Decode(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func buildPlanFixture(t *testing.T, w *workflow.Workflow, config Config) *Plan {
	t.Helper()
	p, err := Build(w, config)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func planFixture(t *testing.T) *Plan {
	t.Helper()
	return buildPlanFixture(t, decodeWorkflow(t, graphFixture), Config{Targets: []string{"package"}})
}

func groupNamed(t *testing.T, p *Plan, name string) Group {
	t.Helper()
	for _, group := range p.Groups {
		if group.Name == name {
			return group
		}
	}
	t.Fatalf("group %q absent", name)
	return Group{}
}

func groupContaining(t *testing.T, p *Plan, member string) Group {
	t.Helper()
	for _, group := range p.Groups {
		if slices.Contains(group.Members, member) {
			return group
		}
	}
	t.Fatalf("no unit contains %q", member)
	return Group{}
}

func TestPlanDeterministicRoundtripAndSemanticInvalidation(t *testing.T) {
	w := decodeWorkflow(t, graphFixture)
	config := Config{Targets: []string{"package", "package"}, Inputs: map[string]string{"z": "last", "a": "first"}}
	first := buildPlanFixture(t, w, config)
	config.Targets = []string{"package"}
	config.Inputs = map[string]string{"a": "first", "z": "last"}
	second := buildPlanFixture(t, w, config)
	a, err := Encode(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encode(second)
	if err != nil || !bytes.Equal(a, b) {
		t.Fatalf("nondeterministic output: %v", err)
	}
	decoded, err := Decode(bytes.NewReader(a))
	if err != nil || decoded.ID != first.ID {
		t.Fatalf("roundtrip: %#v %v", decoded, err)
	}
	task := w.Tasks["build"]
	task.Env = map[string]workflow.Value{"CHANGED": workflow.Literal("literal")}
	w.Tasks["build"] = task
	changed, err := Build(w, config)
	if err != nil || changed.ID == first.ID || changed.WorkflowDigest == first.WorkflowDigest {
		t.Fatalf("semantic change not detected: %v", err)
	}
	if groupNamed(t, first, "build").ID != groupNamed(t, changed, "build").ID {
		t.Fatal("task edit renamed stable provider job")
	}
}

func TestComponentFingerprintIgnoresLoaderPathButIncludesBytesAndModes(t *testing.T) {
	w := decodeWorkflow(t, "version: 1\ntasks: {test: {image: scratch}}\n")
	one, two := t.TempDir(), t.TempDir()
	for _, root := range []string{one, two} {
		if err := os.WriteFile(filepath.Join(root, "helper"), []byte("immutable helper\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	build := func(root string) *Plan {
		t.Helper()
		task := w.Tasks["test"]
		task.ComponentDir = root
		w.Tasks["test"] = task
		p := buildPlanFixture(t, w, Config{})
		return p
	}
	a, b := build(one), build(two)
	if a.ID != b.ID {
		t.Fatal("loader temporary path changed fingerprint")
	}
	if err := os.Chmod(filepath.Join(two, "helper"), 0755); err != nil {
		t.Fatal(err)
	}
	modeChanged := build(two)
	if modeChanged.ID == b.ID {
		t.Fatal("asset executable mode did not invalidate plan")
	}
	if err := os.WriteFile(filepath.Join(two, "helper"), []byte("changed helper\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if c := build(two); c.ID == modeChanged.ID {
		t.Fatal("asset bytes did not invalidate plan")
	}
	if err := os.Symlink(filepath.Join(one, "helper"), filepath.Join(two, "link")); err != nil {
		t.Fatal(err)
	}
	task := w.Tasks["test"]
	task.ComponentDir = two
	w.Tasks["test"] = task
	if _, err := Build(w, Config{}); err == nil {
		t.Fatal("symlink asset accepted")
	}
}

func TestPlanRejectsUnsupportedPathsHostEffectsAndBounds(t *testing.T) {
	w := decodeWorkflow(t, "version: 1\ntasks: {test: {image: scratch}}\n")
	for _, config := range []Config{
		{Workflow: "oci://registry.test/workflow@sha256:" + strings.Repeat("a", 64)},
		{Workflow: "/host/stoke.yaml"}, {Context: "../outside"}, {Context: "a/../b"},
		{Workflow: "${{ secrets.FILE }}"},
	} {
		if _, err := Build(w, config); err == nil {
			t.Fatalf("accepted invalid configuration %#v", config)
		}
	}
	for _, definition := range []string{
		"version: 1\ntasks: {test: {image: scratch, contexts: {other: /outside}}}\n",
		"version: 1\ntasks: {test: {image: scratch, mounts: [{type: host, source: ., target: /host}], steps: [{run: ['true']}]}}\n",
	} {
		if _, err := Build(decodeWorkflow(t, definition), Config{}); err == nil {
			t.Fatal("accepted nonportable task input")
		}
	}

}

func TestPlanRejectsOversizedComponentAsset(t *testing.T) {
	root := t.TempDir()
	file, err := os.Create(filepath.Join(root, "too-large"))
	if err != nil {
		t.Fatal(err)
	}
	err = file.Truncate(assets.MaxFileBytes + 1)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("fixture: %v %v", err, closeErr)
	}
	w := decodeWorkflow(t, "version: 1\ntasks: {test: {image: scratch}}\n")
	task := w.Tasks["test"]
	task.ComponentDir = root
	w.Tasks["test"] = task
	if _, err := Build(w, Config{}); err == nil {
		t.Fatal("oversized asset accepted")
	}
}

func TestPlanDecodeRejectsTamperingUnknownFieldsVersionsAndExtraDocuments(t *testing.T) {
	p := planFixture(t)
	data, err := Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{
		bytes.Replace(data, []byte(fmt.Sprintf(`"version": %d`, Version)), []byte(fmt.Sprintf(`"version": %d`, Version+1)), 1),
		bytes.Replace(data, []byte(`"generatedBy": "stoke-ci"`), []byte(`"generatedBy": "handwritten"`), 1),
		bytes.Replace(data, []byte(`"context": "."`), []byte(`"context": "edited"`), 1),
		append(bytes.TrimSpace(data), []byte("\n{}\n")...),
		append([]byte("{\"unexpected\":true,"), data[1:]...),
		bytes.Repeat([]byte(" "), MaxPlanBytes+1),
	} {
		if _, err := Decode(bytes.NewReader(bad)); err == nil {
			t.Fatal("invalid or modified plan accepted")
		}
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["engine"].(map[string]any)["productionPin"] != nil {
		t.Fatal("serialized plan invents a production pin")
	}
}

func TestRegistryCacheDirectionsAndPriorityAreExplicit(t *testing.T) {
	w := decodeWorkflow(t, "version: 1\ntasks: {build: {image: scratch}}\n")
	config := Config{CacheFrom: []string{"cache.example.test/first", "cache.example.test/second"}, CacheTo: []string{"cache.example.test/write"}}
	p := buildPlanFixture(t, w, config)
	if !slices.Equal(p.Config.CacheFrom, config.CacheFrom) || !slices.Equal(p.Config.CacheTo, config.CacheTo) || !slices.Contains(p.Engine.Capabilities, "registry-cache") || slices.Contains(p.Engine.Capabilities, "component-load") {
		t.Fatal("cache direction/priority or capability changed")
	}
	config.CacheFrom[0], config.CacheFrom[1] = config.CacheFrom[1], config.CacheFrom[0]
	changed, err := Build(w, config)
	if err != nil || changed.ID == p.ID {
		t.Fatal("cache priority not part of plan identity")
	}
	for _, bad := range []string{"cache.example.test/cache:tag", "cache.example.test/cache@sha256:" + strings.Repeat("a", 64), "cache.example.test/${{ secrets.CACHE }}"} {
		if _, err := Build(w, Config{CacheFrom: []string{bad}}); err == nil {
			t.Fatalf("invalid cache read accepted: %q", bad)
		}
		if _, err := Build(w, Config{CacheTo: []string{bad}}); err == nil {
			t.Fatalf("invalid cache write accepted: %q", bad)
		}
	}
	none, err := Build(w, Config{})
	if err != nil || slices.Contains(none.Engine.Capabilities, "registry-cache") {
		t.Fatal("registry cache silently enabled")
	}
}

func inlineEnvironmentContext(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	for _, name := range []string{"image.coopr", "other.coopr"} {
		if err := os.WriteFile(name, []byte("from \"scratch\" {}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("image.coopr", "alias.coopr"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("inputs", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("inputs", "alias-inputs"); err != nil {
		t.Fatal(err)
	}
}

func TestUnlikeInlineEnvironmentProducersStayIndependent(t *testing.T) {
	inlineEnvironmentContext(t)
	for name, second := range map[string]string{
		"files":    "build: {coopr: [other.coopr]}, cache: false",
		"cache":    "build: {coopr: [image.coopr]}, cache: true",
		"platform": "build: {coopr: [image.coopr]}, cache: false, platform: linux/arm64",
		"target":   "build: {coopr: [image.coopr], target: different}, cache: false",
		"argument": "build: {coopr: [image.coopr], args: {MODE: different}}, cache: false",
		"gates":    "build: {coopr: [image.coopr]}, cache: false, needs: [gate]",
	} {
		w := decodeWorkflow(t, "version: 1\ntasks:\n  gate: {image: scratch}\n  first: {build: {coopr: [image.coopr]}, cache: false}\n  second: {"+second+"}\n")
		p, err := Build(w, Config{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if groupContaining(t, p, "first").ID == groupContaining(t, p, "second").ID {
			t.Fatalf("different %s identity was merged", name)
		}
	}
}

func TestPureGatesNeedNoResultRegistryOrDataReplay(t *testing.T) {
	p := planFixture(t)
	if len(p.Groups) != 4 {
		t.Fatal("pure gates require result transport")
	}
	build, lint, tests, pack := groupNamed(t, p, "build"), groupNamed(t, p, "lint"), groupNamed(t, p, "tests"), groupNamed(t, p, "package")
	if !slices.Equal(lint.Needs, []string{build.ID}) || !slices.Equal(tests.Needs, []string{build.ID}) || !slices.Equal(pack.Needs, sortedUnique([]string{lint.ID, tests.ID})) {
		t.Fatal("native gates changed")
	}
	for _, g := range p.Groups {
		for _, j := range g.Jobs {
			if !slices.Equal(j.Execute, []string{j.ID}) {
				t.Fatal("pure gates became data or output slots collided")
			}
		}
	}
}

func TestPlanCapabilitiesFollowSelectedCIOutputs(t *testing.T) {
	for _, tc := range []struct {
		name, gate, consumer, target string
		optional, named, exports     bool
	}{
		{name: "fresh gate", gate: "from: build, cache: false", consumer: "image: scratch, needs: [gate]", target: "done", optional: true},
		{name: "cached gate", gate: "from: build", consumer: "image: scratch, needs: [gate]", target: "done"},
		{name: "requested rootfs", gate: "from: build, cache: false, outputs: {filesystem: true}", consumer: "image: scratch, needs: [gate]", target: "done"},
		{name: "consumed output", gate: "from: build, cache: false", consumer: "from: gate", target: "done"},
		{name: "explicit gate keeps workflow demand", optional: true, gate: "from: build, cache: false", consumer: "image: scratch, needs: [gate]", target: "gate"},
		{name: "named input and report", gate: "from: build, cache: false, contexts: {source: .}, mounts: [{type: context, context: source, target: /source}], steps: [{run: [true]}, {export: {source: /report, destination: report}}]", consumer: "image: scratch, needs: [gate]", target: "done", optional: true, named: true, exports: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := decodeWorkflow(t, "version: 1\ntasks:\n  build: {image: scratch}\n  gate: {"+tc.gate+"}\n  done: {"+tc.consumer+"}\n")
			p := buildPlanFixture(t, w, Config{Targets: []string{tc.target}, CoLocate: []string{"gate"}})
			if p.Engine.APIVersion != cooprwire.Version {
				t.Fatalf("plan API version %d differs from SDK %d", p.Engine.APIVersion, cooprwire.Version)
			}
			for capability, want := range map[string]bool{
				cooprwire.CapabilityOptionalOCIOutput:   tc.optional,
				cooprwire.CapabilityNamedContexts:       tc.named,
				cooprwire.CapabilitySelectedFileExports: tc.exports,
			} {
				if slices.Contains(p.Engine.Capabilities, capability) != want {
					t.Fatalf("%s requirement=%t, want %t: %v", capability, !want, want, p.Engine.Capabilities)
				}
			}
		})
	}
}

func TestSplitDataAndExplicitCoLocation(t *testing.T) {
	w := decodeWorkflow(t, `version: 1
tasks:
  gate: {image: scratch, cache: false}
  base: {image: scratch}
  middle: {from: base, needs: [gate]}
  selected: {from: middle}
`)
	split := buildPlanFixture(t, w, Config{})
	if len(split.Groups) != 4 || !slices.Equal(groupNamed(t, split, "selected").Needs, []string{"middle"}) ||
		!slices.Contains(split.Engine.Capabilities, cooprwire.CapabilityCacheRequireHit) {
		t.Fatalf("split stage lost its native gate or cache-only capability: %#v", split)
	}
	p := buildPlanFixture(t, w, Config{CoLocate: []string{"middle", "middle"}})
	unit := groupContaining(t, p, "middle")
	if len(p.Groups) != 2 || len(unit.Jobs[0].Execute) != 3 || !slices.Equal(unit.Needs, []string{groupNamed(t, p, "gate").ID}) {
		t.Fatalf("explicit grouping or pure gate lost: %#v", p.Groups)
	}
	if !slices.Equal(p.Config.CoLocate, []string{"middle"}) {
		t.Fatal("configuration/semantic identity wrong")
	}
	if _, err := Build(w, Config{CoLocate: []string{"not-selected"}}); err == nil {
		t.Fatal("unknown grouping accepted")
	}
}

func TestSplitDataRejectsIneligibleProducers(t *testing.T) {
	for _, policy := range []string{"cache: false", "mounts: [{type: context, target: /src, read-only: false}], steps: [{run: ['true']} ]"} {
		w := decodeWorkflow(t, "version: 1\ntasks:\n  producer: {image: scratch, "+policy+"}\n  consumer: {from: producer}\n")
		if _, err := Build(w, Config{CacheFrom: []string{"registry.test/cache"}}); err == nil || !strings.Contains(err.Error(), "--co-locate") {
			t.Fatalf("ordinary cache was accepted as guaranteed producer restore: %v", err)
		}
	}
}

func TestExplicitContractionRetainsInterveningGateCycles(t *testing.T) {
	w := decodeWorkflow(t, `version: 1
tasks:
  a: {image: scratch}
  b: {image: scratch, needs: [a]}
  c: {image: scratch, needs: [b]}
  d: {from: a, needs: [c]}
  e: {from: b, needs: [d]}
  f: {from: c, needs: [e]}
`)
	p := buildPlanFixture(t, w, Config{CoLocate: []string{"a", "b", "c"}})
	if len(p.Groups) != 1 || len(p.Groups[0].Jobs[0].Execute) != 6 {
		t.Fatal("explicit gate quotient remains cyclic")
	}
}

func TestSharedInlineEnvironmentRequiresOneInvocation(t *testing.T) {
	w := decodeWorkflow(t, `version: 1
tasks:
  checks:
    build: {coopr: [image.coopr]}
    strategy: {matrix: {mode: [one, two]}}
`)
	// Environment resolution uses the actual context path and recipe.
	t.Chdir(t.TempDir())
	if err := os.WriteFile("image.coopr", []byte("from \"scratch\" {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(w, Config{}); err == nil || !strings.Contains(err.Error(), "shared inline environment") {
		t.Fatalf("shared environment was split: %v", err)
	}
	p := buildPlanFixture(t, w, Config{CoLocate: []string{"checks"}})
	if len(p.Groups) != 1 || len(p.Groups[0].Jobs) != 1 || len(p.Groups[0].Jobs[0].Execute) != 2 {
		t.Fatal("co-location lost variants")
	}
}

func TestRootCachePolicyBindsPlanAndCapabilities(t *testing.T) {
	w := decodeWorkflow(t, "version: 1\ncache-to: [registry.test/write]\ntasks: {gate: {image: scratch, cache: false}}\n")
	p := buildPlanFixture(t, w, Config{})
	if !slices.Contains(p.Engine.Capabilities, "registry-cache") {
		t.Fatal("ordinary terminal output omitted write capability")
	}
	original := p.ID
	w.CacheFrom = []string{"registry.test/read"}
	changed := buildPlanFixture(t, w, Config{})
	if changed.ID == original || changed.WorkflowDigest == p.WorkflowDigest {
		t.Fatal("root policy drift was not bound to plan")
	}
	cleared := buildPlanFixture(t, w, Config{CacheFrom: []string{}, CacheTo: []string{}})
	if slices.Contains(cleared.Engine.Capabilities, "registry-cache") {
		t.Fatal("explicit clears did not suppress registry capability")
	}
	data, err := Encode(cleared)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(bytes.NewReader(data))
	if err != nil || decoded.Config.CacheFrom == nil || decoded.Config.CacheTo == nil {
		t.Fatal("explicit clear lost on wire", err)
	}
}
