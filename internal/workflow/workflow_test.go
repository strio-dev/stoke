package workflow

import (
	"reflect"
	"strings"
	"testing"
)

func TestPlanClosureAndImplicitEdges(t *testing.T) {
	w, err := Decode(strings.NewReader(`version: 1
tasks:
  unrelated: {image: scratch}
  build: {image: scratch}
  test:
    from: build
    cache: false
    steps:
      - run: [test, -f, /artifact]
  artifact:
    image: scratch
    needs: [test]
    steps:
      - copy: {from: build, sources: [/artifact], destination: /artifact}
`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.Plan([]string{"artifact"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"build", "test", "artifact"}) {
		t.Fatalf("plan=%v", got)
	}
	if w.Tasks["test"].CacheEnabled() || !w.Tasks["build"].CacheEnabled() {
		t.Fatal("cache defaults incorrect")
	}
	for range 20 {
		again, err := w.Plan(nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(again, []string{"build", "test", "artifact", "unrelated"}) {
			t.Fatalf("unstable plan: %v", again)
		}
	}
}

func TestDecodeRejectsInvalidDefinitions(t *testing.T) {
	for _, tc := range []struct{ name, document string }{
		{"unknown field", "version: 1\ntasks: {build: {image: scratch, typo: true}}"},
		{"duplicate task", "version: 1\ntasks:\n  a: {image: scratch}\n  a: {image: scratch}"},
		{"multiple documents", "version: 1\ntasks: {a: {image: scratch}}\n---\nversion: 1"},
		{"alias", "version: 1\ntasks: {a: &task {image: scratch}, b: *task}"},
		{"merge", "version: 1\ntasks: {a: {<<: {image: scratch}}}"},
		{"version", "version: 2\ntasks: {a: {image: scratch}}"},
		{"no tasks", "version: 1"},
		{"bad task name", "version: 1\ntasks: {../a: {image: scratch}}"},
		{"missing dependency", "version: 1\ntasks: {a: {image: scratch, needs: [b]}}"},
		{"cycle", "version: 1\ntasks: {a: {from: b}, b: {from: a}}"},
		{"two sources", "version: 1\ntasks: {a: {image: scratch, from: a}}"},
		{"missing source", "version: 1\ntasks: {a: {}}"},
		{"empty run", "version: 1\ntasks: {a: {image: scratch, steps: [{run: []}]}}"},
		{"empty executable", "version: 1\ntasks: {a: {image: scratch, steps: [{run: ['']}]}}"},
		{"two operations", "version: 1\ntasks: {a: {image: scratch, steps: [{run: [true], copy: {sources: [.], destination: /x}}]}}"},
		{"relative workdir", "version: 1\ntasks: {a: {image: scratch, workdir: work}}"},
		{"bad env", "version: 1\ntasks: {a: {image: scratch, env: {'A=B': x}}}"},
		{"escaping copy", "version: 1\ntasks: {a: {image: scratch, steps: [{copy: {sources: [../secret], destination: /x}}]}}"},
		{"copy destination", "version: 1\ntasks: {a: {image: scratch, steps: [{copy: {sources: [.], destination: x}}]}}"},
		{"unpinned import", "version: 1\nimports: {tools: registry.example/tools:latest}\ntasks: {a: {uses: tools/task}}"},
		{"transport-prefixed import", "version: 1\nimports: {tools: 'docker://registry.example/tools@sha256:" + strings.Repeat("a", 64) + "'}\ntasks: {a: {uses: tools/task}}"},
		{"unknown import", "version: 1\ntasks: {a: {uses: tools/task}}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(tc.document)); err == nil {
				t.Fatal("accepted invalid workflow")
			}
		})
	}
}

func TestDecodeLimitsAndEmpty(t *testing.T) {
	for _, input := range []string{"", "# empty\n", strings.Repeat(" ", MaxSize+1)} {
		if _, err := Decode(strings.NewReader(input)); err == nil {
			t.Fatal("accepted empty or oversized definition")
		}
	}
}

func TestUnresolvedImportCannotPlan(t *testing.T) {
	doc := "version: 1\nimports: {tools: registry.example/tools@sha256:" + strings.Repeat("a", 64) + "}\ntasks: {a: {uses: tools/task, needs: [b]}, b: {image: scratch}}"
	w, err := Decode(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Plan([]string{"a"}); err == nil {
		t.Fatal("planned unresolved import")
	}
	if got, err := w.Plan([]string{"b"}); err != nil || !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("unrelated local task: %v %v", got, err)
	}
	if _, err := w.Plan([]string{"missing"}); err == nil {
		t.Fatal("planned missing target")
	}
}
