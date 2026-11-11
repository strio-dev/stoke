package workflow

import (
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestInputBindingResolvesDefaultsAndExplicitValues(t *testing.T) {
	const data = `version: 1
inputs:
  release: {type: string, required: true}
  mode: {default: debug}
  optional: {}
imports:
  child: registry.test/child@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
tasks:
  literal:
    image: scratch
    env:
      RELEASE: {input: release}
      MODE: {input: mode}
      EMPTY: {input: optional}
      LITERAL: '$(inputs.release)'
  native:
    coopr:
      files: [image.coopr]
      args: {VERSION: {input: release}, MODE: {input: mode}}
  forwarded:
    uses: child/build
    with: {version: {input: release}, mode: literal}
`
	w, err := Decode(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Plan(nil); err == nil || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("unbound plan: %v", err)
	}
	original := w.Tasks["literal"]
	if err := w.BindInputs(map[string]string{"release": "1.2.3", "mode": ""}); err != nil {
		t.Fatal(err)
	}
	if len(w.Inputs) != 0 {
		t.Fatal("retained declarations")
	}
	if original.Env["RELEASE"].Input != "release" {
		t.Fatal("mutated previous task map")
	}
	for key, want := range map[string]string{"RELEASE": "1.2.3", "MODE": "", "EMPTY": "", "LITERAL": "$(inputs.release)"} {
		value := w.Tasks["literal"].Env[key]
		if value.Input != "" || value.Value == nil || *value.Value != want {
			t.Fatalf("env %s = %#v", key, value)
		}
	}
	if got := w.Tasks["native"].Coopr.Args["VERSION"]; got.Input != "" || got.Value == nil || *got.Value != "1.2.3" {
		t.Fatalf("native: %#v", got)
	}
	if got := w.Tasks["forwarded"].With["version"]; got.Input != "" || got.Value == nil || *got.Value != "1.2.3" {
		t.Fatalf("forwarded: %#v", got)
	}
	defaulted, err := Decode(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := defaulted.BindInputs(map[string]string{"release": ""}); err != nil {
		t.Fatal(err)
	}
	if got := *defaulted.Tasks["literal"].Env["MODE"].Value; got != "debug" {
		t.Fatalf("default=%q", got)
	}
}

func TestInputBindingFailureIsAtomic(t *testing.T) {
	const data = "version: 1\ninputs: {version: {required: true}}\ntasks: {build: {image: scratch, env: {VERSION: {input: version}}}}\n"
	for name, values := range map[string]map[string]string{
		"missing":      nil,
		"unknown":      {"version": "ok", "typo": "x"},
		"nul":          {"version": "a\x00b"},
		"invalid UTF8": {"version": string([]byte{0xff})},
	} {
		t.Run(name, func(t *testing.T) {
			w, err := Decode(strings.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			before, err := yaml.Marshal(w)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.BindInputs(values); err == nil {
				t.Fatal("accepted invalid input")
			}
			after, err := yaml.Marshal(w)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("failed binding changed workflow")
			}
		})
	}
}

func TestResolveInputsLeavesDeclarationsAndTasksUnchanged(t *testing.T) {
	w, err := Decode(strings.NewReader("version: 1\ninputs: {required: {required: true}, mode: {default: debug}, optional: {}}\ntasks: {run: {image: scratch, env: {MODE: {input: mode}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := yaml.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	supplied := map[string]string{"required": "present"}
	values, err := w.ResolveInputs(supplied)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(values, map[string]string{"required": "present", "mode": "debug", "optional": ""}) {
		t.Fatalf("resolved inputs: %v", values)
	}
	values["required"], values["mode"] = "changed", "changed"
	if supplied["required"] != "present" {
		t.Fatal("resolved inputs alias invocation values")
	}
	after, err := yaml.Marshal(w)
	if err != nil || string(before) != string(after) {
		t.Fatalf("resolving values changed workflow: %v", err)
	}
	invalid := &Workflow{Inputs: map[string]Input{"mode": {Type: "integer"}}}
	if _, err := invalid.ResolveInputs(nil); err == nil {
		t.Fatal("accepted invalid input declaration")
	}
}

func TestTaskInputBindingCopiesAndRejectsMissingValues(t *testing.T) {
	w, err := Decode(strings.NewReader(`version: 1
inputs: {mode: {required: true}}
imports: {lib: registry.test/lib@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}
tasks:
  literal: {image: scratch, env: {MODE: {input: mode}}}
  forwarded: {uses: lib/run, with: {mode: {input: mode}}}
  native: {coopr: {files: [image.coopr], args: {MODE: {input: mode}}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]func(Task) *string{
		"literal":   func(task Task) *string { return task.Env["MODE"].Value },
		"forwarded": func(task Task) *string { return task.With["mode"].Value },
		"native":    func(task Task) *string { return task.Coopr.Args["MODE"].Value },
	} {
		t.Run(name, func(t *testing.T) {
			original := w.Tasks[name]
			before, err := yaml.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			first, err := original.BindInputs(map[string]string{"mode": "first"})
			if err != nil {
				t.Fatal(err)
			}
			second, err := original.BindInputs(map[string]string{"mode": "second"})
			if err != nil || value(first) == nil || value(second) == nil {
				t.Fatalf("binding: %v", err)
			}
			*value(first) = "mutated"
			if *value(second) != "second" {
				t.Fatal("task bindings alias another invocation")
			}
			for _, inputs := range []map[string]string{nil, {"mode": "a\x00b"}, {"mode": string([]byte{0xff})}} {
				if _, err := original.BindInputs(inputs); err == nil {
					t.Fatal("accepted absent or invalid bound input")
				}
			}
			after, err := yaml.Marshal(original)
			if err != nil || string(before) != string(after) {
				t.Fatalf("binding changed source task: %v", err)
			}
		})
	}
}

func TestInputBindingRevalidatesUnselectedProgrammaticTasks(t *testing.T) {
	w, err := Decode(strings.NewReader("version: 1\ninputs: {mode: {default: debug}}\ntasks: {run: {image: scratch}, unused: {image: scratch}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	w.Tasks["unused"] = Task{Image: "scratch", Env: map[string]Value{"MODE": {Input: "undeclared"}}}
	before, err := yaml.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.BindInputs(nil); err == nil {
		t.Fatal("binding skipped full workflow validation")
	}
	after, err := yaml.Marshal(w)
	if err != nil || string(before) != string(after) {
		t.Fatalf("failed validation changed workflow: %v", err)
	}
}

func TestInputSchemaRejectsInvalidDeclarationsAndBindings(t *testing.T) {
	for name, fragment := range map[string]string{
		"null env map":                "tasks: {build: {image: scratch, env: null}}",
		"null with map":               "imports: {lib: registry.test/lib@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}\ntasks: {build: {uses: lib/build, with: null}}",
		"boolean input name":          "inputs: {true: {}}\ntasks: {build: {image: scratch}}",
		"null inputs":                 "inputs: null\ntasks: {build: {image: scratch}}",
		"null declaration":            "inputs: {mode: null}\ntasks: {build: {image: scratch}}",
		"unsupported type":            "inputs: {mode: {type: integer}}\ntasks: {build: {image: scratch}}",
		"null type":                   "inputs: {mode: {type: null}}\ntasks: {build: {image: scratch}}",
		"numeric default":             "inputs: {mode: {default: 42}}\ntasks: {build: {image: scratch}}",
		"null default":                "inputs: {mode: {default: null}}\ntasks: {build: {image: scratch}}",
		"binary default":              "inputs: {mode: {default: !!binary /w==}}\ntasks: {build: {image: scratch}}",
		"required default":            "inputs: {mode: {required: true, default: debug}}\ntasks: {build: {image: scratch}}",
		"wrong required type":         "inputs: {mode: {required: 'true'}}\ntasks: {build: {image: scratch}}",
		"unknown declaration field":   "inputs: {mode: {description: text}}\ntasks: {build: {image: scratch}}",
		"duplicate declaration field": "inputs: {mode: {default: debug, default: release}}\ntasks: {build: {image: scratch}}",
		"duplicate input":             "inputs: {mode: {}, mode: {}}\ntasks: {build: {image: scratch}}",
		"invalid name":                "inputs: {bad.name: {}}\ntasks: {build: {image: scratch}}",
		"undeclared env input":        "tasks: {build: {image: scratch, env: {MODE: {input: mode}}}}",
		"undeclared native input":     "tasks: {build: {coopr: {files: [image.coopr], args: {MODE: {input: mode}}}}}",
		"numeric env":                 "tasks: {build: {image: scratch, env: {MODE: 42}}}",
		"null env":                    "tasks: {build: {image: scratch, env: {MODE: null}}}",
		"empty binding":               "tasks: {build: {image: scratch, env: {MODE: {input: ''}}}}",
		"extra binding":               "inputs: {mode: {}}\ntasks: {build: {image: scratch, env: {MODE: {input: mode, extra: ignored}}}}",
		"with without uses":           "tasks: {build: {image: scratch, with: {mode: debug}}}",
		"empty with without uses":     "tasks: {build: {image: scratch, with: {}}}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader("version: 1\n" + fragment + "\n")); err == nil {
				t.Fatal("accepted invalid input schema")
			}
		})
	}
}

func TestInputYAMLRoundTrip(t *testing.T) {
	const data = "version: 1\ninputs: {mode: {type: string, default: ''}}\nimports: {lib: registry.test/lib@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}\ntasks:\n  run: {image: scratch, env: {MODE: {input: mode}, STATIC: text}}\n  native: {coopr: {files: [image.coopr], args: {MODE: {input: mode}}}}\n  reuse: {uses: lib/run, with: {mode: {input: mode}}}\n"
	w, err := Decode(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Decode(strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w, again) {
		t.Fatalf("round trip changed workflow:\n%s", encoded)
	}
}

func TestMatrixInputEnvironmentValuesDoNotAlias(t *testing.T) {
	w, err := Decode(strings.NewReader("version: 1\ninputs: {mode: {default: release}}\ntasks: {build: {image: scratch, env: {MODE: {input: mode}}, strategy: {matrix: {compiler: [one, two]}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.BindInputs(nil); err != nil {
		t.Fatal(err)
	}
	plan, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	*plan.Groups[0].Jobs[0].Definition.Env["MODE"].Value = "mutated"
	if *plan.Groups[0].Jobs[1].Definition.Env["MODE"].Value != "release" || *w.Tasks["build"].Env["MODE"].Value != "release" {
		t.Fatal("matrix variants alias input values")
	}
}
