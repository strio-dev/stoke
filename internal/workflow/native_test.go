package workflow

import (
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func nativeWorkflow(t *testing.T, document string) *Workflow {
	t.Helper()
	w, err := Decode(strings.NewReader(document))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestNativeBuildTestPublishGraphAndRoundTrip(t *testing.T) {
	w := nativeWorkflow(t, `version: 1
tasks:
  toolkit:
    coopr:
      kind: component
      files: [components/toolkit.coopr]
  image:
    coopr:
      files: [image.coopr]
      target: release
      args:
        TOOLKIT: {from: toolkit}
        MESSAGE: '${literal} with spaces ☃'
        EMPTY: ''
    outputs: {filesystem: false}
  test:
    from: image
    cache: false
    steps:
      - export: {source: /workspace/dist/tool, destination: dist/tool}
      - run: [check]
  deliver:
    needs: [test]
    publish:
      from: image
      destinations: ['oci:/tmp/delivered']
`)
	order, err := w.Plan([]string{"deliver"})
	if err != nil || !reflect.DeepEqual(order, []string{"toolkit", "image", "test", "deliver"}) {
		t.Fatalf("order=%v err=%v", order, err)
	}
	if w.Tasks["toolkit"].OutputKind() != "component" || w.Tasks["toolkit"].FilesystemEnabled() || w.Tasks["image"].OutputKind() != "image" || w.Tasks["image"].FilesystemEnabled() || w.Tasks["test"].FilesystemEnabled() {
		t.Fatal("artifact kinds/filesystem declarations lost")
	}
	if w.Tasks["deliver"].OutputKind() != "" || w.Tasks["deliver"].CacheEnabled() || w.Tasks["deliver"].FilesystemEnabled() {
		t.Fatal("publication treated as a cached build")
	}
	encoded, err := yaml.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip := nativeWorkflow(t, string(encoded))
	if !reflect.DeepEqual(roundTrip, w) {
		t.Fatalf("YAML roundtrip differs:\n%s", encoded)
	}
}

func TestExportStepsSelectFilesystemOutputs(t *testing.T) {
	for _, tc := range []struct {
		task string
		full bool
	}{
		{"image: scratch", true},
		{"image: scratch, outputs: {}", true},
		{"image: scratch, outputs: {filesystem: false}", false},
		{"image: scratch, steps: [{export: {source: /report, destination: reports/report}}]", false},
		{"image: scratch, steps: [{export: {source: /report, destination: reports/report}}], outputs: {filesystem: true}", true},
		{"image: scratch, steps: [{export: {source: /report, destination: reports/report}}], outputs: {filesystem: false}", false},
	} {
		w := nativeWorkflow(t, "version: 1\ntasks:\n  test: {"+tc.task+"}\n")
		if got := w.Tasks["test"].FilesystemEnabled(); got != tc.full {
			t.Fatalf("full filesystem export=%v want=%v for %s", got, tc.full, tc.task)
		}
	}
}

func TestExportStepsPreserveOrderAndMatrixOwnership(t *testing.T) {
	w := nativeWorkflow(t, `version: 1
tasks:
  test:
    image: scratch
    strategy: {matrix: {mode: [debug, release]}}
    steps:
      - run: [produce]
      - export: {source: /first, destination: first}
      - run: [change]
      - export: {source: /later, destination: later}
`)
	plan, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	jobs := plan.Groups[0].Jobs
	want := []FileExport{{Source: "/first", Destination: "first"}, {Source: "/later", Destination: "later"}}
	if len(jobs) != 2 || !reflect.DeepEqual(jobs[0].Definition.FileExports(), want) || jobs[0].Definition.Steps[1].Export == nil || jobs[0].Definition.Steps[2].Run[0] != "change" {
		t.Fatalf("export placement or order lost: %+v", jobs)
	}
	jobs[0].Definition.Steps[1].Export.Destination = "changed"
	if !reflect.DeepEqual(jobs[1].Definition.FileExports(), want) || !reflect.DeepEqual(w.Tasks["test"].FileExports(), want) {
		t.Fatal("export steps alias their matrix template or sibling")
	}
}

func TestNativeSchemaRejectsInvalidDefinitions(t *testing.T) {
	for _, tc := range []struct{ name, tasks string }{
		{"unknown native field", "a: {coopr: {files: [x.coopr], typo: true}}"},
		{"unknown outputs field", "a: {image: scratch, outputs: {typo: false}}"},
		{"task-level file exports", "a: {image: scratch, outputs: {files: [{source: /out, destination: out}]}}"},
		{"run and export", "a: {image: scratch, steps: [{run: [true], export: {source: /out, destination: out}}]}"},
		{"copy and export", "a: {image: scratch, steps: [{copy: {sources: [in], destination: /out}, export: {source: /out, destination: out}}]}"},
		{"native export step", "a: {coopr: {files: [x.coopr]}, steps: [{export: {source: /out, destination: out}}]}"},
		{"host export step", "a: {host: {tools: [sh]}, platform: linux/amd64, steps: [{export: {source: /out, destination: out}}]}"},
		{"publication export step", "a: {image: scratch}\nb: {publish: {from: a, destinations: [example.test/a:v1]}, steps: [{export: {source: /out, destination: out}}]}"},
		{"export mounts", "a: {image: scratch, steps: [{export: {source: /out, destination: out}, mounts: [{type: tmpfs, target: /tmp}]}]}"},
		{"unknown file export field", "a: {image: scratch, steps: [{export: {source: /out, destination: out, typo: true}}] }"},
		{"missing export source", "a: {image: scratch, steps: [{export: {destination: out}}] }"},
		{"relative export source", "a: {image: scratch, steps: [{export: {source: out, destination: out}}] }"},
		{"root export source", "a: {image: scratch, steps: [{export: {source: /, destination: out}}] }"},
		{"noncanonical export source", "a: {image: scratch, steps: [{export: {source: /work/../out, destination: out}}] }"},
		{"glob export source", "a: {image: scratch, steps: [{export: {source: '/out/*', destination: out}}] }"},
		{"missing export destination", "a: {image: scratch, steps: [{export: {source: /out}}] }"},
		{"absolute export destination", "a: {image: scratch, steps: [{export: {source: /out, destination: /out}}] }"},
		{"escaping export destination", "a: {image: scratch, steps: [{export: {source: /out, destination: ../out}}] }"},
		{"noncanonical export destination", "a: {image: scratch, steps: [{export: {source: /out, destination: ./out}}] }"},
		{"root export destination", "a: {image: scratch, steps: [{export: {source: /out, destination: .}}] }"},
		{"glob export destination", "a: {image: scratch, steps: [{export: {source: /out, destination: 'out?'}}] }"},
		{"git export destination", "a: {image: scratch, steps: [{export: {source: /out, destination: .git/config}}] }"},
		{"stoke export destination", "a: {image: scratch, steps: [{export: {source: /out, destination: .stoke/run}}] }"},
		{"workflow export destination", "a: {image: scratch, steps: [{export: {source: /out, destination: stoke.yaml}}] }"},
		{"duplicate export destination", "a: {image: scratch, steps: [{export: {source: /a, destination: out}}, {export: {source: /b, destination: out}}]}"},
		{"overlapping export destinations", "a: {image: scratch, steps: [{export: {source: /a, destination: out/nested}}, {export: {source: /b, destination: out}}]}"},
		{"component selected exports", "a: {coopr: {files: [x.coopr], kind: component}, steps: [{export: {source: /out, destination: out}}] }"},
		{"unknown publication field", "a: {image: scratch}\nb: {publish: {from: a, destinations: [example.test/a:v1], typo: false}}"},
		{"selector collision", "a: {image: scratch, coopr: {files: [x.coopr]}}"},
		{"missing files", "a: {coopr: {}}"},
		{"empty files", "a: {coopr: {files: []}}"},
		{"empty file", "a: {coopr: {files: ['']}}"},
		{"absolute file", "a: {coopr: {files: [/tmp/x.coopr]}}"},
		{"escaping file", "a: {coopr: {files: [a/../../x.coopr]}}"},
		{"context directory file", "a: {coopr: {files: [a/..]}}"},
		{"duplicate normalized file", "a: {coopr: {files: [x.coopr, ./x.coopr]}}"},
		{"unknown kind", "a: {coopr: {files: [x.coopr], kind: archive}}"},
		{"native steps", "a: {coopr: {files: [x.coopr]}, steps: [{run: [true]}]}"},
		{"native env", "a: {coopr: {files: [x.coopr]}, env: {MODE: debug}}"},
		{"native workdir", "a: {coopr: {files: [x.coopr]}, workdir: /work}"},
		{"component filesystem", "a: {coopr: {files: [x.coopr], kind: component}, outputs: {filesystem: true}}"},
		{"argument number", "a: {coopr: {files: [x.coopr], args: {MODE: 1}}}"},
		{"argument boolean", "a: {coopr: {files: [x.coopr], args: {MODE: true}}}"},
		{"argument null", "a: {coopr: {files: [x.coopr], args: {MODE: null}}}"},
		{"argument list", "a: {coopr: {files: [x.coopr], args: {MODE: [x]}}}"},
		{"argument unknown binding", "a: {coopr: {files: [x.coopr], args: {MODE: {env: MODE}}}}"},
		{"argument empty binding", "a: {coopr: {files: [x.coopr], args: {MODE: {from: ''}}}}"},
		{"argument numeric binding", "a: {coopr: {files: [x.coopr], args: {MODE: {from: 1}}}}"},
		{"argument two bindings", "a: {coopr: {files: [x.coopr], args: {MODE: {from: b, matrix: mode}}}}\nb: {image: scratch}"},
		{"argument duplicate field", "a: {coopr: {files: [x.coopr], args: {MODE: {from: b, from: b}}}}\nb: {image: scratch}"},
		{"argument missing task", "a: {coopr: {files: [x.coopr], args: {BASE: {from: missing}}}}"},
		{"argument cycle", "a: {coopr: {files: [a.coopr], args: {BASE: {from: b}}}}\nb: {coopr: {files: [b.coopr], args: {BASE: {from: a}}}}"},
		{"argument invalid name", "a: {coopr: {files: [x.coopr], args: {'A=B': x}}}"},
		{"matrix binding without strategy", "a: {coopr: {files: [x.coopr], args: {MODE: {matrix: mode}}}}"},
		{"matrix unknown binding", "a: {coopr: {files: [x.coopr], args: {MODE: {matrix: missing}}}, strategy: {matrix: {mode: [debug]}}}"},
		{"publication missing source", "a: {publish: {destinations: [example.test/a:v1]}}"},
		{"publication missing destinations", "a: {image: scratch}\nb: {publish: {from: a}}"},
		{"publication duplicate destinations", "a: {image: scratch}\nb: {publish: {from: a, destinations: [example.test/a:v1, example.test/a:v1]}}"},
		{"publication empty destination", "a: {image: scratch}\nb: {publish: {from: a, destinations: ['']}}"},
		{"publication padded destination", "a: {image: scratch}\nb: {publish: {from: a, destinations: [' example.test/a:v1']}}"},
		{"publication cache", "a: {image: scratch}\nb: {publish: {from: a, destinations: [example.test/a:v1]}, cache: false}"},
		{"publication strategy", "a: {image: scratch}\nb: {publish: {from: a, destinations: [example.test/a:v1]}, strategy: {matrix: {mode: [debug]}}}"},
		{"publication outputs", "a: {image: scratch}\nb: {publish: {from: a, destinations: [example.test/a:v1]}, outputs: {filesystem: false}}"},
		{"publication env", "a: {image: scratch}\nb: {publish: {from: a, destinations: [example.test/a:v1]}, env: {MODE: debug}}"},
		{"component as image source", "a: {coopr: {files: [x.coopr], kind: component}}\nb: {from: a}"},
		{"component as copy source", "a: {coopr: {files: [x.coopr], kind: component}}\nb: {image: scratch, steps: [{copy: {from: a, sources: [/x], destination: /x}}]}"},
		{"publication as source", "a: {image: scratch}\nb: {publish: {from: a, destinations: [example.test/a:v1]}}\nc: {from: b}"},
		{"publication as argument", "a: {image: scratch}\nb: {publish: {from: a, destinations: [example.test/a:v1]}}\nc: {coopr: {files: [x.coopr], args: {BASE: {from: b}}}}"},
		{"publication as publication source", "a: {image: scratch}\nb: {publish: {from: a, destinations: [example.test/a:v1]}}\nc: {publish: {from: b, destinations: [example.test/b:v1]}}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document := "version: 1\ntasks:\n  " + strings.ReplaceAll(tc.tasks, "\n", "\n  ")
			if _, err := Decode(strings.NewReader(document)); err == nil {
				t.Fatal("accepted invalid native workflow")
			}
		})
	}
}

func TestNativeMatrixBindsArgumentsWithoutContainerEnvironment(t *testing.T) {
	w := nativeWorkflow(t, `version: 1
tasks:
  toolkit:
    coopr: {kind: component, files: [toolkit.coopr]}
  image:
    coopr:
      files: [image.coopr]
      args:
        TOOLKIT: {from: toolkit}
        MODE: {matrix: mode}
        FLAVOR: {matrix: flavor}
        LITERAL: '${literal}'
    outputs:
      filesystem: false
    strategy:
      matrix:
        mode: [debug, release]
        include:
          - {mode: debug, flavor: next}
          - {mode: custom}
`)
	plan, err := w.Expand([]string{"image"})
	if err != nil {
		t.Fatal(err)
	}
	jobs := plan.Groups[1].Jobs
	if len(jobs) != 3 {
		t.Fatalf("jobs=%d", len(jobs))
	}
	for i, expected := range []struct{ mode, flavor string }{{"debug", "next"}, {"release", ""}, {"custom", ""}} {
		def := jobs[i].Definition
		args := def.Coopr.Args
		if *args["MODE"].Value != expected.mode || *args["FLAVOR"].Value != expected.flavor || args["MODE"].Matrix != "" || args["TOOLKIT"].From != "toolkit" || *args["LITERAL"].Value != "${literal}" || len(def.Env) != 0 {
			t.Fatalf("incorrect concrete native job %#v", jobs[i])
		}
	}
	// A job's files, literal pointers and output declaration must not alias its
	// template or siblings, because expansion materializes concrete task values.
	jobs[0].Definition.Coopr.Files[0] = "changed.coopr"
	*jobs[0].Definition.Coopr.Args["LITERAL"].Value = "changed"
	*jobs[0].Definition.Outputs.Filesystem = true
	if jobs[1].Definition.Coopr.Files[0] != "image.coopr" || *jobs[1].Definition.Coopr.Args["LITERAL"].Value != "${literal}" || jobs[1].Definition.FilesystemEnabled() || w.Tasks["image"].Coopr.Files[0] != "image.coopr" || *w.Tasks["image"].Coopr.Args["LITERAL"].Value != "${literal}" || w.Tasks["image"].FilesystemEnabled() {
		t.Fatal("native task expansion mutated template or sibling")
	}
	if w.Tasks["image"].Coopr.Args["MODE"].Matrix != "mode" {
		t.Fatal("expansion consumed original matrix binding")
	}
}

func TestNativeDataSourcesRequireSingleProducerVariant(t *testing.T) {
	for _, consumer := range []string{
		"coopr: {files: [image.coopr], args: {BASE: {from: producer}}}",
		"publish: {from: producer, destinations: [example.test/output:v1]}",
	} {
		for _, matrix := range []string{"mode: [debug, release]", "mode: [debug]"} {
			w := nativeWorkflow(t, "version: 1\ntasks:\n  producer:\n    coopr: {files: [image.coopr]}\n    strategy: {matrix: {"+matrix+"}}\n  consumer:\n    "+consumer+"\n")
			plan, err := w.Expand([]string{"consumer"})
			if strings.Contains(matrix, "release") {
				if err == nil || !strings.Contains(err.Error(), "multiple matrix variants") {
					t.Fatalf("ambiguous native source accepted: %v", err)
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			def := plan.Groups[1].Jobs[0].Definition
			source := ""
			if def.Coopr != nil {
				source = def.Coopr.Args["BASE"].From
			} else {
				source = def.Publish.From
			}
			if source != plan.Groups[0].Jobs[0].Name || source == "producer" {
				t.Fatalf("unresolved singleton variant %q", source)
			}
		}
	}
}

func TestNativeProducerTypeValidationWaitsForTemplateExpansion(t *testing.T) {
	document := "version: 1\nimports: {tools: registry.test/tools@sha256:" + strings.Repeat("a", 64) + "}\ntasks:\n  source: {uses: tools/build}\n  image: {from: source}\n  native: {coopr: {files: [image.coopr], args: {BASE: {from: source}}}}\n  deliver: {publish: {from: source, destinations: [example.test/output:v1]}}\n"
	w := nativeWorkflow(t, document)
	if _, err := w.Plan([]string{"native"}); err == nil || !strings.Contains(err.Error(), "must be resolved") {
		t.Fatalf("unresolved producer planned: %v", err)
	}
}

func TestPublicationExpansionDoesNotAliasDestinationDeclaration(t *testing.T) {
	w := nativeWorkflow(t, "version: 1\ntasks:\n  build: {image: scratch}\n  deliver: {publish: {from: build, destinations: ['oci:/tmp/image']}}\n")
	plan, err := w.Expand([]string{"deliver"})
	if err != nil {
		t.Fatal(err)
	}
	plan.Groups[1].Jobs[0].Definition.Publish.Destinations[0] = "oci:/tmp/changed"
	if w.Tasks["deliver"].Publish.Destinations[0] != "oci:/tmp/image" {
		t.Fatal("publication expansion changed authored destination")
	}
}

func TestNativeProgrammaticArgumentsValidateUnion(t *testing.T) {
	value := "literal"
	for _, argument := range []Argument{{}, {Value: &value, From: "base"}, {From: "base", Matrix: "mode"}} {
		w := &Workflow{Version: 1, Tasks: map[string]Task{"base": {Image: "scratch"}, "build": {Coopr: &CooprBuild{Files: []string{"image.coopr"}, Args: map[string]Argument{"BASE": argument}}}}}
		if w.Validate() == nil {
			t.Fatal("accepted ambiguous programmatic argument")
		}
		if _, err := argument.MarshalYAML(); err == nil {
			t.Fatal("marshaled ambiguous programmatic argument")
		}
	}
}
