package source

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/strio-dev/stoke/internal/artifact"
	"github.com/strio-dev/stoke/internal/workflow"
)

func pin(name string) string { return "registry.test/" + name + "@sha256:" + strings.Repeat("a", 64) }
func loader(documents map[string]*artifact.Document) Loader {
	return Loader{Fetch: func(_ context.Context, ref string) (*artifact.Document, error) {
		d, ok := documents[ref]
		if !ok {
			return nil, fmt.Errorf("missing %s", ref)
		}
		copy := *d
		if copy.Reference == "" {
			copy.Reference = ref
		}
		return &copy, nil
	}}
}
func local(t *testing.T, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stoke.yaml")
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNestedTemplatesAndOverrides(t *testing.T) {
	base := pin("base")
	tools := pin("tools")
	documents := map[string]*artifact.Document{
		base:  {Data: []byte("version: 1\ntasks:\n  build:\n    image: scratch\n    env: {MODE: original}\n    workdir: /src\n  test:\n    from: build\n    needs: [build]\n    steps:\n      - copy: {from: build, sources: [/out], destination: /result}\n")},
		tools: {Data: []byte("version: 1\nimports:\n  base: " + base + "\ntasks:\n  check:\n    uses: base/test\n")},
	}
	path := local(t, "version: 1\nimports:\n  tools: "+tools+"\ntasks:\n  release:\n    uses: tools/check\n    env: {MODE: changed}\n    workdir: /work\n    cache: false\n  standalone:\n    image: scratch\n")
	result, err := loader(documents).Load(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Targets, []string{"release", "standalone"}) {
		t.Fatalf("targets %v", result.Targets)
	}
	release := result.Workflow.Tasks["release"]
	if release.From != "release--tools--check--base--build" || release.Steps[0].Copy.From != "release--tools--check--base--build" || *release.Env["MODE"].Value != "changed" || release.Workdir != "/work" || release.CacheEnabled() {
		t.Fatalf("release %#v", release)
	}
	plan, err := result.Workflow.Plan(result.Targets)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan, []string{"release--tools--check--base--build", "release", "standalone"}) {
		t.Fatalf("plan %v", plan)
	}
	if result.ContextDir != filepath.Dir(path) || result.Reference != path {
		t.Fatalf("context %#v", result)
	}
	// Overrides must not mutate the reusable source task.
	if _, exists := result.Workflow.Tasks["tools--base--test"]; exists {
		t.Fatal("uncalled imported task was emitted")
	}
}

func TestRelocationBeforeNestedFetch(t *testing.T) {
	root, child, moved := pin("root"), pin("child"), pin("moved")
	l := loader(map[string]*artifact.Document{
		root:  {Data: []byte("version: 1\nimports:\n  child: " + child + "\ntasks:\n  run:\n    uses: child/build\n"), Dependencies: map[string]string{child: moved}},
		moved: {Data: []byte("version: 1\ntasks:\n  build:\n    image: source.test/base:v1\n"), Dependencies: map[string]string{"source.test/base:v1": "destination.test/base@sha256:" + strings.Repeat("b", 64)}},
	})
	result, err := l.Load(context.Background(), "oci://"+root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Workflow.Tasks["run"].Image != "destination.test/base@sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("unmapped image %q", result.Workflow.Tasks["run"].Image)
	}
}

func TestImportFailures(t *testing.T) {
	a, b := pin("a"), pin("b")
	tests := []struct {
		name, root string
		documents  map[string]*artifact.Document
		want       string
	}{
		{"cycle", a, map[string]*artifact.Document{a: {Data: []byte("version: 1\nimports:\n  b: " + b + "\ntasks: {run: {image: scratch}}\n")}, b: {Data: []byte("version: 1\nimports:\n  a: " + a + "\ntasks: {run: {image: scratch}}\n")}}, "cycle"},
		{"missing template", a, map[string]*artifact.Document{a: {Data: []byte("version: 1\nimports:\n  b: " + b + "\ntasks: {run: {uses: b/missing}}\n")}, b: {Data: []byte("version: 1\ntasks: {run: {image: scratch}}\n")}}, "does not exist"},
		{"collision", a, map[string]*artifact.Document{a: {Data: []byte("version: 1\nimports:\n  b: " + b + "\ntasks: {run: {uses: b/run}, run--b--prepare: {image: scratch}}\n")}, b: {Data: []byte("version: 1\ntasks: {prepare: {image: scratch}, run: {from: prepare}}\n")}}, "collision"},
		{"oversize", a, map[string]*artifact.Document{a: {Data: []byte(strings.Repeat("x", workflow.MaxSize+1))}}, "1 MiB"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loader(tc.documents).Load(context.Background(), "oci://"+tc.root)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %s", err, tc.want)
			}
		})
	}
}

func TestImportLimits(t *testing.T) {
	t.Run("depth", func(t *testing.T) {
		docs := map[string]*artifact.Document{}
		for i := 0; i < 19; i++ {
			data := "version: 1\ntasks: {run: {image: scratch}}\n"
			if i < 18 {
				data += "imports: {next: " + pin(fmt.Sprint(i+1)) + "}\n"
			}
			docs[pin(fmt.Sprint(i))] = &artifact.Document{Data: []byte(data)}
		}
		_, err := loader(docs).Load(context.Background(), "oci://"+pin("0"))
		if err == nil || !strings.Contains(err.Error(), "depth") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("documents", func(t *testing.T) {
		docs := map[string]*artifact.Document{}
		root := "version: 1\ntasks: {run: {image: scratch}}\nimports:\n"
		for i := 0; i < 64; i++ {
			root += fmt.Sprintf("  child%d: %s\n", i, pin(fmt.Sprint(i)))
			docs[pin(fmt.Sprint(i))] = &artifact.Document{Data: []byte("version: 1\ntasks: {run: {image: scratch}}\n")}
		}
		_, err := loader(docs).Load(context.Background(), local(t, root))
		if err == nil || !strings.Contains(err.Error(), "count") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("tasks", func(t *testing.T) {
		data := "version: 1\ntasks:\n"
		for i := 0; i < 1025; i++ {
			data += fmt.Sprintf("  task%d: {image: scratch}\n", i)
		}
		_, err := Loader{}.Load(context.Background(), local(t, data))
		if err == nil || !strings.Contains(err.Error(), "1024") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestDependencies(t *testing.T) {
	w := &workflow.Workflow{Imports: map[string]string{"tool": pin("tool")}, Tasks: map[string]workflow.Task{"a": {Image: "scratch"}, "b": {Image: "oci:/tmp/image"}, "c": {Image: "registry.test/base:v1"}, "d": {Image: "registry.test/base:v1"}}}
	if got := Dependencies(w); !reflect.DeepEqual(got, []string{"registry.test/base:v1", pin("tool")}) {
		t.Fatalf("dependencies %v", got)
	}
}

func TestDockerTransportRelocation(t *testing.T) {
	original := "registry.test/base:v1"
	delivered := "destination.test/base@sha256:" + strings.Repeat("b", 64)
	root := pin("root")
	l := loader(map[string]*artifact.Document{root: {Data: []byte("version: 1\ntasks:\n  build:\n    image: docker://" + original + "\n"), Dependencies: map[string]string{original: delivered}}})
	result, err := l.Load(context.Background(), "oci://"+root)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Workflow.Tasks["build"].Image; got != delivered {
		t.Fatalf("relocated image %q", got)
	}
	for _, reference := range []string{"docker://unrecorded.test/base:v1", "unrecorded.test/base:v1", "busybox:v1", "oci:/tmp/image", "scratch"} {
		if got := RelocatedReference(reference, map[string]string{original: delivered}); got != reference {
			t.Fatalf("unrecorded reference %q changed to %q", reference, got)
		}
	}
	exact := "destination.test/exact:v1"
	if got := RelocatedReference("docker://"+original, map[string]string{original: delivered, "docker://" + original: exact}); got != exact {
		t.Fatalf("exact mapping lost precedence: %q", got)
	}
}

func TestImportedMatrixInheritanceAndReplacement(t *testing.T) {
	ref := pin("matrix")
	library := []byte("version: 1\ntasks:\n  test:\n    image: scratch\n    strategy:\n      max-parallel: 2\n      fail-fast: false\n      matrix:\n        mode: [debug, release]\n        include: [{mode: debug, compiler: next}]\n        exclude: [{mode: release}]\n")
	path := local(t, "version: 1\nimports:\n  checks: "+ref+"\ntasks:\n  inherited:\n    uses: checks/test\n  sibling:\n    uses: checks/test\n  replaced:\n    uses: checks/test\n    strategy:\n      matrix: {mode: [custom]}\n")
	result, err := loader(map[string]*artifact.Document{ref: {Data: library}}).Load(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	inherited := result.Workflow.Tasks["inherited"].Strategy
	replaced := result.Workflow.Tasks["replaced"].Strategy
	if inherited.MaxParallel != 2 || inherited.FailFast == nil || *inherited.FailFast || len(inherited.Matrix.Axes["mode"]) != 2 {
		t.Fatalf("inherited strategy %#v", inherited)
	}
	if replaced.MaxParallel != 0 || replaced.FailFast != nil || !reflect.DeepEqual(replaced.Matrix.Axes["mode"], []string{"custom"}) || len(replaced.Matrix.Exclude) != 0 {
		t.Fatalf("replacement merged instead of replacing: %#v", replaced)
	}
	inherited.Matrix.Axes["mode"][0] = "mutated"
	inherited.Matrix.Include[0]["compiler"] = "mutated"
	inherited.Matrix.Exclude[0]["mode"] = "mutated"
	*inherited.FailFast = true
	for _, name := range []string{"sibling"} {
		strategy := result.Workflow.Tasks[name].Strategy
		if strategy.Matrix.Axes["mode"][0] != "debug" || strategy.Matrix.Include[0]["compiler"] != "next" || strategy.Matrix.Exclude[0]["mode"] != "release" || *strategy.FailFast {
			t.Fatalf("matrix aliases task %q", name)
		}
	}
}
