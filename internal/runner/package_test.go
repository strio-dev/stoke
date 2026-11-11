package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cooprwire "github.com/strio-dev/coopr/api"
	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/source"
	"github.com/strio-dev/stoke/internal/workflow"
)

func TestComponentInputsPreserveCallerContext(t *testing.T) {
	opts := testOptions(t)
	component := t.TempDir()
	if err := os.WriteFile(filepath.Join(component, "build.coopr"), []byte("from \"scratch\" {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ref := "registry.test/tools@sha256:" + strings.Repeat("a", 64)
	w := fixture(t, `version: 1
assets: [build.coopr, check.sh]
tasks:
  native:
    coopr:
      files: [build.coopr]
      files-from: component
      args: {TOOLS: {ref: "`+ref+`"}}
  test:
    from: native
    steps:
      - copy: {scope: component, sources: [check.sh], destination: /check.sh}
`)
	for name, task := range w.Tasks {
		task.ComponentDir = component
		w.Tasks[name] = task
	}
	builds, copies := 0, 0
	_, err := run(context.Background(), w, []string{"test"}, opts, fakeEngine{
		build: func(_ context.Context, b cooprwire.Build, _ bool, o coopr.Options) (coopr.Result, error) {
			builds++
			if b.Files[0] != filepath.Join(component, "build.coopr") || b.Args["TOOLS"] != ref || o.ContextDir != opts.ContextDir || o.Contexts[workflow.ComponentContext] != component {
				t.Errorf("mixed package/caller inputs b=%#v o=%#v", b, o)
			}
			return coopr.Result{Platform: "linux/amd64", ImageID: "native"}, nil
		},
		execute: func(_ context.Context, p coopr.Program, o coopr.Options) (coopr.Result, error) {
			copies++
			copy := p.States[0].Operations[0]
			if copy.Kind != "copy" || copy.From.Context != workflow.ComponentContext || copy.From.Image != "" || o.ContextDir != opts.ContextDir || o.Contexts[workflow.ComponentContext] != component {
				t.Errorf("wrong explicit input: %#v %#v", copy, o)
			}
			return coopr.Result{Platform: "linux/amd64", ImageID: "tested"}, nil
		},
	})
	if err != nil || builds != 1 || copies != 1 {
		t.Fatalf("builds=%d copies=%d err=%v", builds, copies, err)
	}
}

func TestComponentMaterializationCannotEnterCallerContext(t *testing.T) {
	opts := testOptions(t)
	t.Setenv("TMPDIR", opts.ContextDir)
	helper := filepath.Join(opts.ContextDir, "helper.sh")
	if err := os.WriteFile(helper, []byte("caller-owned helper\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(opts.ContextDir, "stoke.yaml")
	if err := os.WriteFile(file, []byte("version: 1\nassets: [helper.sh]\ntasks: {copy: {image: scratch, steps: [{copy: {sources: [.], destination: /project}}]}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := (source.Loader{}).Load(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = loaded.Close() }()
	owned := loaded.Workflow.Tasks["copy"].ComponentDir
	calls := 0
	report, err := runLiteral(context.Background(), loaded.Workflow, nil, opts, func(context.Context, coopr.Program, coopr.Options) (coopr.Result, error) {
		calls++
		return coopr.Result{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "TMPDIR") || calls != 0 || report != nil {
		t.Fatalf("overlapping package entered caller COPY: err=%v calls=%d report=%v", err, calls, report)
	}
	if err := loaded.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Fatalf("owned package survived failure: %v", err)
	}
	data, err := os.ReadFile(helper)
	if err != nil || string(data) != "caller-owned helper\n" {
		t.Fatalf("caller file affected: %q %v", data, err)
	}
}

func TestUnresolvedComponentInputCannotExecute(t *testing.T) {
	w := fixture(t, `version: 1
assets: [check.sh]
tasks:
  test:
    image: scratch
    steps: [{copy: {scope: component, sources: [check.sh], destination: /check.sh}}]
`)
	calls := 0
	report, err := runLiteral(context.Background(), w, nil, testOptions(t), func(context.Context, coopr.Program, coopr.Options) (coopr.Result, error) {
		calls++
		return coopr.Result{}, nil
	})
	if err == nil || calls != 0 || report != nil {
		t.Fatalf("unresolved input ran: report=%v calls=%d err=%v", report, calls, err)
	}
}
