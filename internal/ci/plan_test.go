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
	w := decodeWorkflow(t, "version: 1\ntasks:\n  "+strings.Repeat("a", 513)+": {image: scratch}\n")
	if _, err := Build(w, Config{}); err == nil || !strings.Contains(err.Error(), "identity limit") {
		t.Fatalf("oversized producer could reach runtime publication: %v", err)
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
		{Workflow: "${{ secrets.FILE }}"}, {Runners: []string{"${{ secrets.RUNNER }}"}},
		{TimeoutMinutes: -1}, {TimeoutMinutes: 361},
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
	for i := range MaxGroups + 1 {
		w.Tasks["t"+strings.Repeat("a", i+1)] = workflow.Task{Image: "scratch"}
	}
	if _, err := Build(w, Config{}); err == nil || !strings.Contains(err.Error(), "provider execution units") {
		t.Fatalf("provider group bound not enforced: %v", err)
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
		bytes.Replace(data, []byte(`"version": 2`), []byte(`"version": 3`), 1),
		bytes.Replace(data, []byte(`"generatedBy": "stoke-ci"`), []byte(`"generatedBy": "handwritten"`), 1),
		bytes.Replace(data, []byte(`"timeoutMinutes": 45`), []byte(`"timeoutMinutes": 46`), 1),
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
		"files":    "environment: {files: [other.coopr]}, cache: false",
		"cache":    "environment: {files: [image.coopr]}, cache: true",
		"platform": "environment: {files: [image.coopr]}, cache: false, platform: linux/arm64",
		"target":   "environment: {files: [image.coopr], target: different}, cache: false",
		"argument": "environment: {files: [image.coopr], args: {MODE: different}}, cache: false",
		"gates":    "environment: {files: [image.coopr]}, cache: false, needs: [gate]",
	} {
		w := decodeWorkflow(t, "version: 1\ntasks:\n  gate: {image: scratch}\n  first: {environment: {files: [image.coopr]}, cache: false}\n  second: {"+second+"}\n")
		p, err := Build(w, Config{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if groupContaining(t, p, "first").ID == groupContaining(t, p, "second").ID {
			t.Fatalf("different %s identity was merged", name)
		}
	}
}

func TestVisibleDataStagesKeepNativeMatrixAndExactImports(t *testing.T) {
	w := decodeWorkflow(t, `version: 1
tasks:
  build: {image: scratch, cache: false}
  tests:
    from: build
    cache: false
    strategy: {max-parallel: 2, fail-fast: false, matrix: {mode: [debug, release]}}
  package:
    image: scratch
    needs: [tests]
    steps: [{copy: {from: build, sources: [/out], destination: /dist}}]
`)
	p := buildPlanFixture(t, w, Config{})
	build, tests, pack := groupNamed(t, p, "build"), groupNamed(t, p, "tests"), groupNamed(t, p, "package")
	if len(p.Groups) != 3 || !p.RequiresResults || !tests.Matrix || len(tests.Jobs) != 2 || tests.FailFast || tests.MaxParallel != 2 {
		t.Fatalf("visible stages/matrix lost: %#v", p.Groups)
	}
	if !slices.Equal(build.Jobs[0].Exports, []string{"build"}) || len(build.Jobs[0].Imports) != 0 {
		t.Fatal("producer export missing")
	}
	for _, j := range tests.Jobs {
		if !slices.Equal(j.Execute, []string{j.ID}) || !slices.Equal(j.Imports, []string{"build"}) || j.OutputKey != OutputKey(j.ID) {
			t.Fatalf("matrix exact input missing: %#v", j)
		}
	}
	if !slices.Equal(pack.Needs, sortedUnique([]string{build.ID, tests.ID})) || !slices.Equal(pack.Jobs[0].Imports, []string{"build"}) {
		t.Fatal("package lost data or success gate")
	}
}

func TestPureGatesNeedNoResultRegistryOrDataReplay(t *testing.T) {
	p := planFixture(t)
	if len(p.Groups) != 4 || p.RequiresResults {
		t.Fatal("pure gates require result transport")
	}
	build, lint, tests, pack := groupNamed(t, p, "build"), groupNamed(t, p, "lint"), groupNamed(t, p, "tests"), groupNamed(t, p, "package")
	if !slices.Equal(lint.Needs, []string{build.ID}) || !slices.Equal(tests.Needs, []string{build.ID}) || !slices.Equal(pack.Needs, sortedUnique([]string{lint.ID, tests.ID})) {
		t.Fatal("native gates changed")
	}
	keys := map[string]bool{}
	for _, g := range p.Groups {
		for _, j := range g.Jobs {
			if len(j.Imports)+len(j.Exports)+len(j.Data) != 0 || !slices.Equal(j.Execute, []string{j.ID}) || keys[j.OutputKey] {
				t.Fatal("pure gates became data or output slots collided")
			}
			keys[j.OutputKey] = true
		}
	}
}

func TestNativeComponentAndSingleEnvironmentImports(t *testing.T) {
	inlineEnvironmentContext(t)
	w := decodeWorkflow(t, `version: 1
tasks:
  component: {coopr: {files: [component.coopr], kind: component}}
  native: {coopr: {files: [image.coopr], args: {TOOLS: {from: component}}}}
  check:
    environment: {files: [image.coopr], args: {TOOLS: {from: component}}}
    platform: linux/arm64
    needs: [native]
    mounts: [{type: stage, from: native, target: /native}]
    steps: [{run: [true]}]
`)
	p := buildPlanFixture(t, w, Config{})
	if len(p.Groups) != 3 || len(p.Environments) != 0 || !slices.Equal(groupNamed(t, p, "check").Jobs[0].Imports, []string{"component", "native"}) {
		t.Fatal("single env leaked or exact inputs lost")
	}
	for _, capability := range []string{"component", "build", "execute", "run-mounts", "build-platform", "component-load"} {
		if !slices.Contains(p.Engine.Capabilities, capability) {
			t.Fatalf("missing %s", capability)
		}
	}
}

func TestCopyAndPublishSelectSameProducerWithoutCoLocation(t *testing.T) {
	w := decodeWorkflow(t, `version: 1
tasks:
  build: {image: scratch, cache: false}
  test: {from: build, cache: false}
  publish: {publish: {from: build, destinations: [registry.example.test/release:reviewed]}, needs: [test]}
`)
	p := buildPlanFixture(t, w, Config{})
	if len(p.Groups) != 3 || !p.RequiresResults {
		t.Fatal("fresh producer hid visible test/publish stages")
	}
	for _, name := range []string{"test", "publish"} {
		if !slices.Equal(groupNamed(t, p, name).Jobs[0].Imports, []string{"build"}) {
			t.Fatal("consumers do not select producer result")
		}
	}
	if !slices.Contains(groupNamed(t, p, "publish").Needs, groupNamed(t, p, "test").ID) {
		t.Fatal("publish bypasses test native gate")
	}
}

func TestImportedImagePublicationDeclaresExactLayoutCapability(t *testing.T) {
	for _, scenario := range []struct {
		name, producer string
		coLocate       []string
		want           bool
	}{
		{name: "split literal image", producer: "image: scratch", want: true},
		{name: "split native image", producer: "coopr: {files: [image.coopr]}", want: true},
		{name: "co-located image", producer: "image: scratch", coLocate: []string{"build"}},
		{name: "split component", producer: "coopr: {files: [component.coopr], kind: component}"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w := decodeWorkflow(t, "version: 1\ntasks:\n  build: {"+scenario.producer+"}\n  publish: {publish: {from: build, destinations: [registry.example.test/release:reviewed]}}\n")
			p := buildPlanFixture(t, w, Config{CoLocate: scenario.coLocate})
			if slices.Contains(p.Engine.Capabilities, "publish-oci-layout") != scenario.want {
				t.Fatalf("exact-layout publication capability=%v, want %t", p.Engine.Capabilities, scenario.want)
			}
		})
	}
	// An explicit group can absorb a pure gate which imports an unrelated
	// image. That import does not make an internally produced publication need
	// exact-layout import support.
	w := decodeWorkflow(t, `version: 1
tasks:
  external: {image: scratch}
  a: {image: scratch}
  gate: {from: external, needs: [a]}
  consumer: {from: a, needs: [gate]}
  publish: {publish: {from: a, destinations: [registry.example.test/release:reviewed]}, needs: [consumer]}
`)
	p := buildPlanFixture(t, w, Config{CoLocate: []string{"a"}})
	unit := groupContaining(t, p, "publish")
	if !slices.Equal(unit.Jobs[0].Imports, []string{"external"}) || slices.Contains(p.Engine.Capabilities, "publish-oci-layout") {
		t.Fatal("unrelated image import changed an internal publication's requirement")
	}
}

func TestExplicitCoLocationIsRequiredToContractData(t *testing.T) {
	w := decodeWorkflow(t, `version: 1
tasks:
  gate: {image: scratch, cache: false}
  base: {image: scratch}
  middle: {from: base, needs: [gate]}
  selected: {from: middle}
`)
	ordinary := buildPlanFixture(t, w, Config{})
	if len(ordinary.Groups) != 4 || !ordinary.RequiresResults {
		t.Fatal("default hid stages")
	}
	p := buildPlanFixture(t, w, Config{CoLocate: []string{"middle", "middle"}})
	unit := groupContaining(t, p, "middle")
	if len(p.Groups) != 2 || p.RequiresResults || len(unit.Jobs[0].Execute) != 3 || len(unit.Jobs[0].Imports) != 0 || !slices.Equal(unit.Needs, []string{groupNamed(t, p, "gate").ID}) {
		t.Fatalf("explicit grouping or pure gate lost: %#v", p.Groups)
	}
	if p.ID == ordinary.ID || p.WorkflowDigest != ordinary.WorkflowDigest || !slices.Equal(p.Config.CoLocate, []string{"middle"}) {
		t.Fatal("configuration/semantic identity wrong")
	}
	if _, err := Build(w, Config{CoLocate: []string{"not-selected"}}); err == nil {
		t.Fatal("unknown grouping accepted")
	}
}

func TestCachePolicyDoesNotSilentlyHideStages(t *testing.T) {
	for _, policy := range []string{"cache: true", "cache: false", "mounts: [{type: context, target: /src, read-only: false}], steps: [{run: ['true']} ]"} {
		w := decodeWorkflow(t, "version: 1\ntasks:\n  producer: {image: scratch, "+policy+"}\n  consumer: {from: producer}\n")
		p := buildPlanFixture(t, w, Config{})
		if len(p.Groups) != 2 || !p.RequiresResults {
			t.Fatal("cache/effect policy silently grouped stages")
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
	if len(p.Groups) != 1 || p.RequiresResults || len(p.Groups[0].Jobs[0].Execute) != 6 {
		t.Fatal("explicit gate quotient remains cyclic")
	}
}

func TestPlanRejectsUnboundOrWrongOwnerImports(t *testing.T) {
	w := decodeWorkflow(t, "version: 1\ntasks: {build: {image: scratch}, test: {from: build}}\n")
	for _, mutation := range []func(*Plan){
		func(p *Plan) { p.Groups[1].Jobs[0].Imports = nil },
		func(p *Plan) { p.Groups[0].Jobs[0].Exports = nil },
		func(p *Plan) { p.RequiresResults = false },
		func(p *Plan) {
			p.Groups[1].Jobs[0].Imports = []string{"unknown"}
			p.Groups[1].Jobs[0].Data = []string{"unknown"}
		},
		func(p *Plan) { p.Groups[1].Jobs[0].OutputKey = p.Groups[0].Jobs[0].OutputKey },
	} {
		p, err := Build(w, Config{})
		if err != nil {
			t.Fatal(err)
		}
		mutation(p)
		p.ID, err = planDigest(p)
		if err != nil {
			t.Fatal(err)
		}
		if p.Validate() == nil {
			t.Fatal("self-consistent invalid exact-result plan accepted")
		}
	}
}

func TestSharedInlineEnvironmentsPromoteVisibleProducer(t *testing.T) {
	inlineEnvironmentContext(t)
	for _, alias := range []string{"image.coopr", "./image.coopr", "alias.coopr"} {
		w := decodeWorkflow(t, "version: 1\ntasks:\n  first: {environment: {files: [image.coopr]}, cache: false, contexts: {data: inputs}}\n  second: {environment: {files: ['"+alias+"']}, cache: false, contexts: {data: alias-inputs}}\n")
		before, _ := json.Marshal(w)
		p := buildPlanFixture(t, w, Config{})
		after, _ := json.Marshal(w)
		if !bytes.Equal(before, after) {
			t.Fatal("caller definitions mutated")
		}
		if len(p.Groups) != 3 || len(p.Environments) != 1 || !p.RequiresResults {
			t.Fatalf("shared fresh env hid authored jobs: %#v", p.Groups)
		}
		for id, representative := range p.Environments {
			if representative != "first" {
				t.Fatal("unstable representative")
			}
			env := groupNamed(t, p, id)
			if !slices.Equal(env.Jobs[0].Exports, []string{id}) {
				t.Fatal("env not exported")
			}
			for _, name := range []string{"first", "second"} {
				g := groupNamed(t, p, name)
				if !slices.Equal(g.Jobs[0].Imports, []string{id}) || !slices.Equal(g.Needs, []string{env.ID}) {
					t.Fatal("shared env result not required")
				}
			}
		}
		joined := buildPlanFixture(t, w, Config{CoLocate: []string{"first"}})
		if len(joined.Groups) != 1 || len(joined.Environments) != 0 || joined.RequiresResults {
			t.Fatal("explicit shared env grouping still needs registry")
		}
	}
}

func TestSharedEnvironmentMatrixKeepsVisibleRows(t *testing.T) {
	inlineEnvironmentContext(t)
	for _, bound := range []bool{false, true} {
		args := ""
		if bound {
			args = ", args: {MODE: {matrix: mode}}"
		}
		w := decodeWorkflow(t, "version: 1\ntasks:\n  checks:\n    environment: {files: [image.coopr]"+args+"}\n    cache: false\n    strategy: {max-parallel: 2, fail-fast: false, matrix: {mode: [one, two]}}\n")
		p := buildPlanFixture(t, w, Config{})
		g := groupNamed(t, p, "checks")
		if !g.Matrix || len(g.Jobs) != 2 || g.MaxParallel != 2 || g.FailFast {
			t.Fatal("matrix rows hidden")
		}
		if !bound && (len(p.Environments) != 1 || len(p.Groups) != 2) {
			t.Fatal("shared env not promoted")
		}
		if bound && (len(p.Environments) != 0 || len(p.Groups) != 1) {
			t.Fatal("different env args got merged")
		}
	}
}

func TestResultsRepositoryConfigurationIsExplicitAndBounded(t *testing.T) {
	w := decodeWorkflow(t, "version: 1\ntasks: {build: {image: scratch}, test: {from: build}}\n")
	empty, err := Build(w, Config{})
	if err != nil || !empty.RequiresResults || empty.Config.ResultsRepository != "" {
		t.Fatal("call-time registry configuration unavailable")
	}
	p, err := Build(w, Config{ResultsRepository: "results.example.test/team/run-results"})
	if err != nil || p.ID == empty.ID {
		t.Fatal("result destination not bound")
	}
	for _, bad := range []string{"https://registry.test/results", "registry.test/results:latest", "registry.test/${{secrets.RESULTS}}", "registry.test/" + strings.Repeat("a", 513)} {
		if _, err := Build(w, Config{ResultsRepository: bad}); err == nil {
			t.Fatalf("invalid results repo accepted %q", bad)
		}
	}
}

func TestNativeMetadataBudgetsAllowMatrixAndRejectHugeFanIn(t *testing.T) {
	values := []string{}
	for i := range 256 {
		values = append(values, fmt.Sprint(i))
	}
	matrix := strings.Join(values, ",")
	for _, count := range []int{1, 3} {
		text := "version: 1\ntasks:\n"
		needs := []string{}
		for i := range count {
			name := fmt.Sprintf("matrix%d", i)
			needs = append(needs, name)
			text += "  " + name + ": {image: scratch, strategy: {matrix: {axis: [" + matrix + "]}}}\n"
		}
		text += "  done: {image: scratch, needs: [" + strings.Join(needs, ",") + "]}\n"
		p, err := Build(decodeWorkflow(t, text), Config{})
		if count == 1 && (err != nil || len(groupNamed(t, p, "matrix0").Jobs) != 256) {
			t.Fatalf("valid max matrix rejected: %v", err)
		}
		if count == 3 && (err == nil || !strings.Contains(err.Error(), "environment budget")) {
			t.Fatalf("oversized native env accepted: %v", err)
		}
	}
}
