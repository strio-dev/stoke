package source

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/strio-dev/stoke/internal/artifact"
)

func TestComponentInputsIsolateDependencyClosures(t *testing.T) {
	ref := pin("parameters")
	library := `version: 1
inputs:
  version: {required: true}
  mode: {default: release}
tasks:
  base:
    image: scratch
    env: {VERSION: {input: version}, MODE: {input: mode}}
  native:
    coopr:
      files: [image.coopr]
      args: {BASE: {from: base}, VERSION: {input: version}}
  verify:
    from: native
    env: {VERSION: {input: version}}
    steps: [{copy: {from: base, sources: [/out], destination: /result}}]
`
	root := "version: 1\nimports: {tools: " + ref + `}
tasks:
  local: {image: scratch}
  first:
    uses: tools/verify
    with: {version: "1.2", mode: debug}
    needs: [local]
    env: {VERSION: caller-override}
    strategy: {matrix: {label: [one, two]}}
  second:
    uses: tools/verify
    with: {version: "2.0"}
`
	result, err := loader(map[string]*artifact.Document{ref: {Data: []byte(library)}}).Load(context.Background(), local(t, root))
	if err != nil {
		t.Fatal(err)
	}
	closeLoaded(t, result)
	if len(result.Workflow.Tasks) != 7 {
		t.Fatalf("unexpected tasks: %v", sortedKeys(result.Workflow.Tasks))
	}
	for _, tc := range []struct{ call, version, mode string }{{"first", "1.2", "debug"}, {"second", "2.0", "release"}} {
		prefix := tc.call + "--tools--"
		base := result.Workflow.Tasks[prefix+"base"]
		native := result.Workflow.Tasks[prefix+"native"]
		verify := result.Workflow.Tasks[tc.call]
		if *base.Env["VERSION"].Value != tc.version || *base.Env["MODE"].Value != tc.mode || *native.Coopr.Args["VERSION"].Value != tc.version {
			t.Fatalf("%s leaked values: base=%+v native=%+v", tc.call, base.Env, native.Coopr.Args)
		}
		if native.Coopr.Args["BASE"].From != prefix+"base" || verify.From != prefix+"native" || verify.Steps[0].Copy.From != prefix+"base" {
			t.Fatalf("%s leaked dependency scope: native=%+v verify=%+v", tc.call, native.Coopr.Args, verify)
		}
		if verify.Uses != "" || len(verify.With) != 0 {
			t.Fatal("unresolved invocation reached execution")
		}
	}
	if !reflect.DeepEqual(result.Workflow.Tasks["first"].Needs, []string{"local"}) || *result.Workflow.Tasks["first"].Env["VERSION"].Value != "caller-override" || *result.Workflow.Tasks["second"].Env["VERSION"].Value != "2.0" {
		t.Fatal("caller overrides escaped their scope")
	}
	plan, err := result.Workflow.Expand(result.Targets)
	if err != nil {
		t.Fatal(err)
	}
	jobs := 0
	for _, group := range plan.Groups {
		jobs += len(group.Jobs)
	}
	if jobs != 8 {
		t.Fatalf("independent caller matrix lost: %d jobs", jobs)
	}
	*result.Workflow.Tasks["first--tools--base"].Env["VERSION"].Value = "mutated"
	if *result.Workflow.Tasks["second--tools--base"].Env["VERSION"].Value != "2.0" {
		t.Fatal("invocations alias bound values")
	}
}

func TestInputsForwardThroughNestedComponents(t *testing.T) {
	leaf, wrapper := pin("leaf"), pin("wrapper")
	documents := map[string]*artifact.Document{
		leaf: {Data: []byte(`version: 1
inputs: {version: {required: true}}
tasks:
  build: {image: scratch, env: {VERSION: {input: version}}}
  verify: {from: build}
`)},
		wrapper: {Data: []byte("version: 1\nimports: {leaf: " + leaf + `}
inputs: {release: {required: true}}
tasks:
  gate: {image: scratch, env: {RELEASE: {input: release}}}
  run:
    uses: leaf/verify
    with: {version: {input: release}}
    needs: [gate]
`)},
	}
	l := loader(documents)
	l.Inputs = map[string]string{"version": "literal=$VALUE=with=equals"}
	root := "version: 1\ninputs: {version: {required: true}}\nimports: {tools: " + wrapper + "}\ntasks:\n  run:\n    uses: tools/run\n    with: {release: {input: version}}\n"
	result, err := l.Load(context.Background(), local(t, root))
	if err != nil {
		t.Fatal(err)
	}
	closeLoaded(t, result)
	run := result.Workflow.Tasks["run"]
	if run.From != "run--tools--run--leaf--build" || !reflect.DeepEqual(run.Needs, []string{"run--tools--gate"}) {
		t.Fatalf("nested edges: %+v", run)
	}
	for _, name := range []string{"run--tools--run--leaf--build", "run--tools--gate"} {
		for _, value := range result.Workflow.Tasks[name].Env {
			if *value.Value != l.Inputs["version"] {
				t.Fatal("nested forwarding changed literal bytes")
			}
		}
	}
}

func TestInputInvocationFailuresAndUnusedDeclarations(t *testing.T) {
	ref := pin("required")
	l := loader(map[string]*artifact.Document{ref: {Data: []byte("version: 1\ninputs: {version: {required: true}}\ntasks: {run: {image: scratch}}\n")}})
	for _, tc := range []struct{ task, want string }{
		{"{uses: lib/run}", "required input"},
		{"{uses: lib/run, with: {version: present, typo: bad}}", "unknown input"},
		{"{uses: lib/missing, with: {version: present}}", "does not exist"},
		{"{uses: lib/run, with: {version: {matrix: axis}}}", "input binding"},
	} {
		t.Run(tc.want+tc.task, func(t *testing.T) {
			_, err := l.Load(context.Background(), local(t, "version: 1\nimports: {lib: "+ref+"}\ntasks: {run: "+tc.task+"}\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want %s", err, tc.want)
			}
		})
	}
	// Importing a reusable definition does not invoke its required inputs.
	result, err := l.Load(context.Background(), local(t, "version: 1\nimports: {lib: "+ref+"}\ntasks: {run: {image: scratch}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	closeLoaded(t, result)
	if len(result.Workflow.Tasks) != 1 {
		t.Fatal("unused definition was instantiated")
	}
}

func TestRepeatedInvocationsRespectExpandedTaskLimit(t *testing.T) {
	ref := pin("large")
	var library strings.Builder
	library.WriteString("version: 1\ntasks:\n  run:\n    image: scratch\n    needs: [")
	for n := range 100 {
		fmt.Fprintf(&library, "dep%d,", n)
	}
	library.WriteString("]\n")
	for n := range 100 {
		fmt.Fprintf(&library, "  dep%d: {image: scratch}\n", n)
	}
	root := "version: 1\nimports: {lib: " + ref + "}\ntasks:\n"
	for n := range 11 {
		root += fmt.Sprintf("  call%d: {uses: lib/run}\n", n)
	}
	_, err := loader(map[string]*artifact.Document{ref: {Data: []byte(library.String())}}).Load(context.Background(), local(t, root))
	if err == nil || !strings.Contains(err.Error(), "1024") {
		t.Fatalf("expanded invocation limit: %v", err)
	}
}

func TestUnusedImportedTemplatesStillValidateResolvedKinds(t *testing.T) {
	leaf, wrapper := pin("leaf"), pin("wrapper")
	for _, tc := range []struct{ task, want string }{
		{"bad: {uses: leaf/native, env: {BAD: text}}", "coopr cannot"},
		{"value: {uses: leaf/component}\n  bad: {from: value}", "required image"},
		{"value: {uses: leaf/publish}\n  bad: {coopr: {files: [x.coopr], args: {BASE: {from: value}}}}", "required image or component"},
		{"bad: {uses: leaf/verify, env: {STOKE_MATRIX_LABEL: text}}", "reserved"},
		{"bad: {uses: leaf/native, strategy: {matrix: {other: [value]}}}", "unknown matrix key"},
	} {
		t.Run(tc.want+tc.task, func(t *testing.T) {
			documents := map[string]*artifact.Document{
				leaf: {Data: []byte(`version: 1
tasks:
  native:
    coopr: {files: [x.coopr], args: {MODE: {matrix: mode}}}
    strategy: {matrix: {mode: [one]}}
  component: {coopr: {kind: component, files: [x.coopr]}}
  publish: {publish: {from: component, destinations: [registry.test/component:latest]}}
  verify:
    image: scratch
    strategy: {matrix: {label: [one, two]}}
`)},
				wrapper: {Data: []byte("version: 1\nimports: {leaf: " + leaf + "}\ntasks:\n  " + tc.task + "\n")},
			}
			_, err := loader(documents).Load(context.Background(), local(t, "version: 1\nimports: {unused: "+wrapper+"}\ntasks: {run: {image: scratch}}\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unused template error=%v want %s", err, tc.want)
			}
		})
	}
}

func TestUnusedImportedInputDeclarationsStillValidate(t *testing.T) {
	ref := pin("unused-inputs")
	for name, declaration := range map[string]string{
		"invalid type":     "{type: integer}",
		"required default": "{required: true, default: debug}",
	} {
		t.Run(name, func(t *testing.T) {
			l := loader(map[string]*artifact.Document{
				ref: {Data: []byte("version: 1\ninputs: {mode: " + declaration + "}\ntasks: {run: {image: scratch}}\n")},
			})
			_, err := l.Load(context.Background(), local(t, "version: 1\nimports: {unused: "+ref+"}\ntasks: {run: {image: scratch}}\n"))
			if err == nil {
				t.Fatal("unused imported input declaration was not validated")
			}
		})
	}
}

func TestLoadCancellationDuringImportValidation(t *testing.T) {
	ref := pin("cancel")
	ctx, cancel := context.WithCancel(context.Background())
	l := Loader{Fetch: func(context.Context, string) (*artifact.Document, error) {
		cancel()
		return &artifact.Document{Reference: ref, Data: []byte("version: 1\ntasks: {run: {image: scratch}}\n")}, nil
	}}
	_, err := l.Load(ctx, local(t, "version: 1\nimports: {lib: "+ref+"}\ntasks: {run: {uses: lib/run}}\n"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled load: %v", err)
	}
}
