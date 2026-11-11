package workflow

import (
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestPlatformLiteralNormalization(t *testing.T) {
	for input, want := range map[string]string{
		"linux/amd64": "linux/amd64", "linux/x86_64": "linux/amd64",
		"linux/aarch64": "linux/arm64", "linux/arm64/v8": "linux/arm64",
		"linux/arm": "linux/arm/v7", "linux/arm/v6": "linux/arm/v6",
		"linux/armhf": "linux/arm/v7", "linux/armel": "linux/arm/v6",
		"linux/i386": "linux/386", "linux/amd64/v1": "linux/amd64",
		"linux/amd64/v3": "linux/amd64/v3", "linux/riscv64": "linux/riscv64",
		"linux/arm/6": "linux/arm/v6", "linux/arm64/8": "linux/arm64",
		"linux/arm64/v8.0": "linux/arm64", "linux/arm64/9.0": "linux/arm64/v9",
		"linux/arm64/v9.0": "linux/arm64/v9", "linux/i386/custom": "linux/386",
		"linux/armhf/v6": "linux/arm/v7", "linux/armel/v7": "linux/arm/v6",
		"linux/futurearch/custom": "linux/futurearch/custom",
	} {
		t.Run(input, func(t *testing.T) {
			w := nativeWorkflow(t, "version: 1\ntasks: {test: {image: scratch, platform: "+input+"}}\n")
			got, err := w.Tasks["test"].ResolvedPlatform()
			if err != nil || got != want {
				t.Fatalf("platform = %q, %v; want %q", got, err, want)
			}
		})
	}
	if got, err := (Task{}).ResolvedPlatform(); err != nil || got != "" {
		t.Fatalf("omitted platform = %q, %v", got, err)
	}
}

func TestPlatformBindingsRoundTripAndIsolation(t *testing.T) {
	w := nativeWorkflow(t, `version: 1
inputs: {target: {required: true}}
tasks:
  native: {coopr: {files: [ci.coopr]}, platform: {input: target}}
  inline: {environment: {files: [ci.coopr]}, platform: linux/x86_64}
  matrix:
    image: scratch
    platform: {matrix: target}
    strategy:
      matrix: {target: [linux/amd64, linux/arm64], include: [{target: linux/arm/v7}]}
`)
	encoded, err := yaml.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	decoded := nativeWorkflow(t, string(encoded))
	if !reflect.DeepEqual(w, decoded) {
		t.Fatalf("platform roundtrip changed workflow:\n%s", encoded)
	}
	if _, err := w.Tasks["native"].ResolvedPlatform(); err == nil {
		t.Fatal("unbound platform input silently used engine default")
	}
	if err := w.BindInputs(map[string]string{"target": "linux/aarch64"}); err != nil {
		t.Fatal(err)
	}
	plan, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	var matrix []string
	for _, group := range plan.Groups {
		for _, job := range group.Jobs {
			got, err := job.Definition.ResolvedPlatform()
			if err != nil {
				t.Fatal(err)
			}
			if group.Name == "matrix" {
				matrix = append(matrix, got)
			}
			if group.Name == "native" && got != "linux/arm64" {
				t.Fatalf("bound native platform = %q", got)
			}
			*job.Definition.Platform.Value = "linux/s390x"
		}
	}
	if !reflect.DeepEqual(matrix, []string{"linux/amd64", "linux/arm64", "linux/arm/v7"}) {
		t.Fatalf("matrix platforms = %v", matrix)
	}
	if got := *w.Tasks["inline"].Platform.Value; got != "linux/x86_64" {
		t.Fatalf("expanded platform aliases source: %q", got)
	}
	if w.Tasks["matrix"].Platform.Matrix != "target" {
		t.Fatal("matrix binding mutated during expansion")
	}
}

func TestPlatformSchemaRejectsInvalidSelectors(t *testing.T) {
	for _, selector := range []string{
		"null", "''", "false", "[]", "{}", "{matrix: ''}", "{input: absent}",
		"{matrix: absent}", "{from: task}", "{matrix: target, input: target}",
		"linux", "windows/amd64", "linux/AMD64", "linux/amd64/", "linux/amd64/v*",
		"linux/arm64/$target", "linux/arm/v:7", "linux//v7", "linux/amd64/extra/path",
		"' linux/amd64'", "'linux/amd64 '",
	} {
		t.Run(selector, func(t *testing.T) {
			_, err := Decode(strings.NewReader("version: 1\ntasks: {test: {image: scratch, platform: " + selector + "}}\n"))
			if err == nil {
				t.Fatal("invalid platform accepted")
			}
		})
	}
	for _, task := range []string{
		"publish: {from: build, destinations: [registry.test/result:latest]}, platform: linux/amd64",
		"uses: lib/run, platform: linux/amd64",
	} {
		_, err := Decode(strings.NewReader("version: 1\nimports: {lib: registry.test/lib@sha256:" + strings.Repeat("a", 64) + "}\ntasks:\n  build: {image: scratch}\n  test: {" + task + "}\n"))
		if err == nil || !strings.Contains(err.Error(), "platform cannot") {
			t.Fatalf("invalid task platform = %v", err)
		}
	}
}

func TestPlatformBindingValuesFailBeforeExecution(t *testing.T) {
	for _, matrix := range []string{
		"{target: [linux/amd64, invalid]}",
		"{other: [base], include: [{other: standalone, target: linux/amd64}]}",
	} {
		w := nativeWorkflow(t, "version: 1\ntasks: {test: {image: scratch, platform: {matrix: target}, strategy: {matrix: "+matrix+"}}}\n")
		if _, err := w.Expand(nil); err == nil || !strings.Contains(err.Error(), "platform") {
			t.Fatalf("bad matrix platform expansion = %v", err)
		}
	}
	w := nativeWorkflow(t, "version: 1\ninputs: {target: {required: true}}\ntasks: {test: {image: scratch, platform: {input: target}}}\n")
	if err := w.BindInputs(map[string]string{"target": ""}); err == nil {
		t.Fatal("empty bound platform accepted")
	}
}

func TestPlatformBindingAcceptsValidUnderscoreMatrixAxis(t *testing.T) {
	w := nativeWorkflow(t, "version: 1\ntasks: {test: {image: scratch, platform: {matrix: _target}, strategy: {matrix: {_target: [linux/arm64]}}}}\n")
	plan, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := plan.Groups[0].Jobs[0].Definition.ResolvedPlatform()
	if err != nil || got != "linux/arm64" {
		t.Fatalf("valid matrix axis rejected: %q %v", got, err)
	}
}
