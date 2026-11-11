package workflow

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func matrixWorkflow(t *testing.T, strategy string) *Workflow {
	t.Helper()
	w, err := Decode(strings.NewReader("version: 1\ntasks:\n  test:\n    image: scratch\n    strategy:\n" + strategy))
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func TestMatrixIncludeGitHubExample(t *testing.T) {
	w := matrixWorkflow(t, `      matrix:
        fruit: [apple, pear]
        animal: [cat, dog]
        include:
          - {color: green}
          - {color: pink, animal: cat}
          - {fruit: apple, shape: circle}
          - {fruit: banana}
          - {fruit: banana, animal: cat}
`)
	combos, err := w.Tasks["test"].Strategy.Matrix.Combinations()
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]string{
		{"animal": "cat", "fruit": "apple", "color": "pink", "shape": "circle"},
		{"animal": "cat", "fruit": "pear", "color": "pink"},
		{"animal": "dog", "fruit": "apple", "color": "green", "shape": "circle"},
		{"animal": "dog", "fruit": "pear", "color": "green"},
		{"fruit": "banana"}, {"fruit": "banana", "animal": "cat"},
	}
	if !reflect.DeepEqual(combos, want) {
		t.Fatalf("combinations=%v", combos)
	}
}
func TestMatrixExpansionInjectsValuesAndPreservesSources(t *testing.T) {
	w, err := Decode(strings.NewReader(`version: 1
tasks:
  build: {image: scratch}
  test:
    from: build
    cache: false
    env: {CUSTOM: kept}
    strategy:
      max-parallel: 2
      fail-fast: false
      matrix:
        mode: [debug, release]
        exclude: [{mode: release}]
        include: [{mode: extra, boolean: true}]
  gate: {image: scratch, needs: [test]}
`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := w.Expand([]string{"gate"})
	if err != nil {
		t.Fatal(err)
	}
	g := p.Groups[1]
	if len(g.Jobs) != 2 || g.FailFast || g.MaxParallel != 2 {
		t.Fatalf("group=%#v", g)
	}
	for _, job := range g.Jobs {
		if job.Definition.From != "build" || *job.Definition.Env["STOKE_MATRIX_MODE"].Value == "" || *job.Definition.Env["CUSTOM"].Value != "kept" || job.Definition.Strategy != nil || job.Definition.CacheEnabled() {
			t.Fatalf("job=%#v", job)
		}
	}
	if _, ok := w.Tasks["test"].Env["STOKE_MATRIX_MODE"]; ok {
		t.Fatal("mutated source env")
	}
	p2, err := w.Expand([]string{"gate"})
	if err != nil || !reflect.DeepEqual(p, p2) {
		t.Fatal("unstable expansion")
	}
	if !reflect.DeepEqual(p.Groups[2].Jobs[0].Definition.Needs, []string{"test"}) {
		t.Fatal("lost all-variant gate")
	}
}
func TestMatrixDefaultsAndIncludeOnly(t *testing.T) {
	w := matrixWorkflow(t, "      matrix:\n        include: [{value: 1}, {value: true}, {value: 1}]\n")
	p, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Groups[0].Jobs) != 2 || p.Groups[0].MaxParallel != 4 || !p.Groups[0].FailFast {
		t.Fatalf("plan=%#v", p)
	}
	if p.Groups[0].Jobs[0].Matrix["value"] != "1" || p.Groups[0].Jobs[1].Matrix["value"] != "true" {
		t.Fatal("bad scalar normalization")
	}
}
func TestMatrixInvalidDefinitions(t *testing.T) {
	for _, strategy := range []string{
		"      matrix: {}\n",
		"      typo: true\n      matrix: {mode: [x]}\n",
		"      max-parallel: 0\n      matrix: {mode: [x]}\n",
		"      max-parallel: null\n      matrix: {mode: [x]}\n",
		"      max-parallel: -1\n      matrix: {mode: [x]}\n",
		"      max-parallel: 257\n      matrix: {mode: [x]}\n",
		"      matrix: {mode: []}\n",
		"      matrix: {mode: [x, x]}\n",
		"      matrix: {mode: [null]}\n",
		"      matrix: {mode: [1.2]}\n",
		"      matrix: {mode: [[x]]}\n",
		"      matrix: {mode: [x], MODE: [y]}\n",
		"      matrix: {mode: [x], include: [{MODE: x}]}\n",
		"      matrix: {mode: [x], exclude: [{mode: x}]}\n",
		"      matrix: {mode: [x], exclude: [{other: x}]}\n",
		"      matrix: {mode: [x], include: [{}]}\n",
		"      matrix: {mode: [x], mode: [y]}\n",
		"      matrix: {include: [{mode: x, mode: y}]}\n",
	} {
		t.Run(strategy, func(t *testing.T) {
			_, err := Decode(strings.NewReader("version: 1\ntasks:\n  test:\n    image: scratch\n    strategy:\n" + strategy))
			if err == nil {
				t.Fatal("accepted invalid matrix")
			}
		})
	}
}
func TestMatrixBoundsAndReservedEnvironment(t *testing.T) {
	axes := map[string][]string{}
	values := []string{}
	for i := range 17 {
		values = append(values, fmt.Sprint(i))
	}
	axes["x"] = values
	axes["y"] = values
	if _, err := (&Matrix{Axes: axes}).Combinations(); err == nil {
		t.Fatal("unbounded Cartesian product")
	}
	w := &Workflow{Version: 1, Tasks: map[string]Task{}}
	for i := range 5 {
		w.Tasks[fmt.Sprintf("task%d", i)] = Task{Image: "scratch", Strategy: &Strategy{Matrix: &Matrix{Axes: map[string][]string{"x": values[:16], "y": values[:16]}}}}
	}
	if _, err := w.Expand(nil); err == nil {
		t.Fatal("unbounded total jobs")
	}
	w = matrixWorkflow(t, "      matrix: {mode: [x]}\n")
	task := w.Tasks["test"]
	task.Env = map[string]Value{"STOKE_MATRIX_MODE": Literal("override")}
	w.Tasks["test"] = task
	if _, err := w.Expand(nil); err == nil {
		t.Fatal("reserved matrix variable overridden")
	}
}
func TestMatrixDataSourceAmbiguity(t *testing.T) {
	for _, source := range []string{"    from: build\n", "    image: scratch\n    steps:\n      - copy: {from: build, sources: [/out], destination: /out}\n"} {
		doc := "version: 1\ntasks:\n  build:\n    image: scratch\n    strategy:\n      matrix: {mode: [debug, release]}\n  test:\n" + source
		w, err := Decode(strings.NewReader(doc))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Expand(nil); err == nil || !strings.Contains(err.Error(), "multiple matrix variants") {
			t.Fatalf("error=%v", err)
		}
	}
	w := matrixWorkflow(t, "      matrix: {mode: [x]}\n")
	p, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	id := p.Groups[0].Jobs[0].Name
	w.Tasks[id] = Task{Image: "scratch"}
	if _, err = w.Expand(nil); err == nil {
		t.Fatal("authored job-name collision")
	}
}

func TestMatrixAbsentFieldsClearInheritedEnvironment(t *testing.T) {
	w := matrixWorkflow(t, `      matrix:
        include:
          - {mode: debug}
          - {platform: linux}
`)
	plan, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	first, second := plan.Groups[0].Jobs[0], plan.Groups[0].Jobs[1]
	if value, ok := first.Definition.Env["STOKE_MATRIX_PLATFORM"]; !ok || value.Value == nil || *value.Value != "" {
		t.Fatal("missing platform environment was not cleared")
	}
	if value, ok := second.Definition.Env["STOKE_MATRIX_MODE"]; !ok || value.Value == nil || *value.Value != "" {
		t.Fatal("missing mode environment was not cleared")
	}
	if _, ok := first.Matrix["platform"]; ok {
		t.Fatal("metadata was populated with absent matrix field")
	}
}

func TestMatrixBoundsCountUniqueStandaloneIncludes(t *testing.T) {
	repeated := &Matrix{}
	for range MaxVariants + 1 {
		repeated.Include = append(repeated.Include, map[string]string{"mode": "debug"})
	}
	combinations, err := repeated.Combinations()
	if err != nil || len(combinations) != 1 {
		t.Fatalf("repeated includes: count=%d error=%v", len(combinations), err)
	}
	distinct := &Matrix{}
	for i := range MaxVariants + 1 {
		distinct.Include = append(distinct.Include, map[string]string{"mode": fmt.Sprint(i)})
	}
	if _, err := distinct.Combinations(); err == nil {
		t.Fatal("accepted more than 256 unique variants")
	}
}
