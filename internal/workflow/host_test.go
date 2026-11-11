package workflow

import (
	"reflect"
	"strings"
	"testing"
)

func hostTaskForTest() Task {
	platform := "linux/amd64"
	return Task{
		Platform: &Platform{Value: &platform},
		Host: &Host{
			Inputs:  []HostInput{{Path: "src"}},
			Outputs: []string{"out"},
			Tools:   []string{"sh"},
		},
		Steps: []Step{{Run: []string{"sh", "-c", "mkdir out"}}},
	}
}

func TestHostTaskDefaultsAndCapturedDependencies(t *testing.T) {
	producer, consumer := hostTaskForTest(), hostTaskForTest()
	consumer.Host.Inputs = []HostInput{{From: "produce", Path: "out", Destination: "input"}}
	consumer.Workdir = "input"
	w := &Workflow{Version: 1, Tasks: map[string]Task{
		"produce": producer,
		"check":   consumer,
		"image": {Image: "scratch", Steps: []Step{{Copy: &Copy{
			From: "produce", Sources: []string{"out"}, Destination: "/out",
		}}}},
	}}
	if err := w.Validate(); err != nil {
		t.Fatal(err)
	}
	if producer.CacheEnabled() || producer.OutputKind() != "host" || !producer.FilesystemEnabled() {
		t.Fatalf("host defaults: cache=%t kind=%q filesystem=%t", producer.CacheEnabled(), producer.OutputKind(), producer.FilesystemEnabled())
	}
	if got, err := w.Plan([]string{"check"}); err != nil || !reflect.DeepEqual(got, []string{"produce", "check"}) {
		t.Fatalf("host data closure: %v %v", got, err)
	}
	producer.Host.Effects = "captured"
	producer.Host.Toolchain = []string{"/usr/bin/sh"}
	yes := true
	producer.Cache = &yes
	w.Tasks["produce"] = producer
	if err := w.Validate(); err != nil || !producer.CacheEnabled() {
		t.Fatalf("explicit captured cache: %v", err)
	}
}

func TestHostValidationRejectsUnsafeOrAmbiguousDeclarations(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Task)
	}{
		{"missing platform", func(task *Task) { task.Platform = nil }},
		{"foreign platform type", func(task *Task) { value := "linux/386"; task.Platform = &Platform{Value: &value} }},
		{"container selector", func(task *Task) { task.Image = "scratch" }},
		{"old outputs", func(task *Task) { task.Outputs = &Outputs{} }},
		{"named contexts", func(task *Task) { task.Contexts = map[string]string{"other": "."} }},
		{"mount", func(task *Task) { task.Mounts = []Mount{{Type: "cache", Target: "/cache"}} }},
		{"step mount", func(task *Task) { task.Steps[0].Mounts = []Mount{{Type: "cache", Target: "/cache"}} }},
		{"absolute workdir", func(task *Task) { task.Workdir = "/project" }},
		{"escaping workdir", func(task *Task) { task.Workdir = "../project" }},
		{"no tools", func(task *Task) { task.Host.Tools = nil }},
		{"relative tool path", func(task *Task) { task.Host.Tools = []string{"./sh"} }},
		{"duplicate tool", func(task *Task) { task.Host.Tools = []string{"sh", "sh"} }},
		{"undeclared direct executable", func(task *Task) { task.Steps[0].Run[0] = "other" }},
		{"no steps", func(task *Task) { task.Steps = nil }},
		{"copy operation", func(task *Task) { task.Steps = []Step{{Copy: &Copy{Sources: []string{"src"}, Destination: "/out"}}} }},
		{"stdout escape", func(task *Task) { task.Steps[0].Stdout = "../outside" }},
		{"input escape", func(task *Task) { task.Host.Inputs[0].Path = "src/../../outside" }},
		{"input destination escape", func(task *Task) { task.Host.Inputs[0].Destination = "../outside" }},
		{"overlapping input destinations", func(task *Task) {
			task.Host.Inputs = []HostInput{{Path: "src"}, {Path: "other", Destination: "src/other"}}
		}},
		{"absolute output", func(task *Task) { task.Host.Outputs = []string{"/outside"} }},
		{"workspace output", func(task *Task) { task.Host.Outputs = []string{"."} }},
		{"overlapping outputs", func(task *Task) { task.Host.Outputs = []string{"out", "out/child"} }},
		{"reserved inherited env", func(task *Task) { task.Host.InheritEnv = []string{"STOKE_MATRIX_ARCH"} }},
		{"invalid inherited env", func(task *Task) { task.Host.InheritEnv = []string{"A=B"} }},
		{"relative toolchain", func(task *Task) { task.Host.Toolchain = []string{"toolchain"} }},
		{"escaping toolchain", func(task *Task) { task.Host.Toolchain = []string{"/usr/../bin/sh"} }},
		{"duplicate capability", func(task *Task) { task.Host.Capabilities = []string{"nix-sandbox", "nix-sandbox"} }},
		{"invalid effect policy", func(task *Task) { task.Host.Effects = "hermetic" }},
		{"cache with undeclared effects", func(task *Task) { yes := true; task.Cache = &yes }},
		{"cache missing toolchain", func(task *Task) { yes := true; task.Cache = &yes; task.Host.Effects = "captured" }},
		{"cache missing outputs", func(task *Task) {
			yes := true
			task.Cache = &yes
			task.Host.Effects = "captured"
			task.Host.Toolchain = []string{"/usr/bin/sh"}
			task.Host.Outputs = nil
		}},
		{"cache missing inputs", func(task *Task) {
			yes := true
			task.Cache = &yes
			task.Host.Effects = "captured"
			task.Host.Toolchain = []string{"/usr/bin/sh"}
			task.Host.Inputs = nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			task := hostTaskForTest()
			test.edit(&task)
			if err := (&Workflow{Version: 1, Tasks: map[string]Task{"run": task}}).Validate(); err == nil {
				t.Fatal("accepted invalid host declaration")
			}
		})
	}
}

func TestHostAuthorityCannotBeGrantedInYAML(t *testing.T) {
	for _, grant := range []string{"allow-host: true", "authority: true", "trusted: true"} {
		document := "version: 1\ntasks:\n  run:\n    platform: linux/amd64\n    host:\n      tools: [sh]\n      " + grant + "\n    steps: [{run: [sh, -c, true]}]\n"
		if _, err := Decode(strings.NewReader(document)); err == nil {
			t.Fatalf("accepted host authority %q", grant)
		}
	}
	if _, err := Decode(strings.NewReader("version: 1\ntasks: {run: {image: scratch, steps: [{run: [true], stdout: result}]}}")); err == nil {
		t.Fatal("accepted host stdout capture on a container task")
	}
}

func TestHostProducerCannotMasqueradeAsAnImageOrNativeArgument(t *testing.T) {
	producer := hostTaskForTest()
	for _, consumer := range []Task{
		{From: "produce"},
		{Coopr: &CooprBuild{Files: []string{"image.coopr"}, Args: map[string]Argument{"BASE": {From: "produce"}}}},
		{Environment: &CooprBuild{Files: []string{"image.coopr"}, Args: map[string]Argument{"BASE": {From: "produce"}}}},
		{Publish: &Publish{From: "produce", Destinations: []string{"registry.test/result:latest"}}},
		{Image: "scratch", Mounts: []Mount{{Type: "stage", From: "produce", Target: "/out"}}, Steps: []Step{{Run: []string{"true"}}}},
	} {
		if err := (&Workflow{Version: 1, Tasks: map[string]Task{"produce": producer, "consume": consumer}}).Validate(); err == nil {
			t.Fatalf("accepted host producer as image/native artifact: %#v", consumer)
		}
	}
	consumer := hostTaskForTest()
	consumer.Host.Inputs = []HostInput{{From: "produce", Path: "secret"}}
	if err := (&Workflow{Version: 1, Tasks: map[string]Task{"produce": producer, "consume": consumer}}).Validate(); err == nil {
		t.Fatal("accepted undeclared producer output")
	}
	consumer.Host.Inputs = []HostInput{{From: "produce", Path: "out"}}
	if err := (&Workflow{Version: 1, Tasks: map[string]Task{"produce": {Image: "scratch"}, "consume": consumer}}).Validate(); err == nil {
		t.Fatal("accepted image filesystem as host captured files")
	}
}

func TestHostMatrixDataIdentityAndIndependentDeclarations(t *testing.T) {
	producer, consumer := hostTaskForTest(), hostTaskForTest()
	producer.Strategy = &Strategy{Matrix: &Matrix{Axes: map[string][]string{"release": {"one"}}}}
	consumer.Host.Inputs = []HostInput{{From: "produce", Path: "out"}}
	consumer.Host.InheritEnv = []string{"PATH"}
	consumer.Host.Toolchain = []string{"/usr/bin/sh"}
	consumer.Host.Capabilities = []string{"local-tools"}
	consumer.Strategy = &Strategy{Matrix: &Matrix{Axes: map[string][]string{"mode": {"one", "two"}}}}
	w := &Workflow{Version: 1, Tasks: map[string]Task{"produce": producer, "consume": consumer}}
	plan, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	jobs := plan.Groups[1].Jobs
	if len(jobs) != 2 || jobs[0].Definition.Host.Inputs[0].From != plan.Groups[0].Jobs[0].Name {
		t.Fatalf("matrix host producer not rewritten: %#v", plan)
	}
	jobs[0].Definition.Host.Inputs[0].Path = "changed"
	jobs[0].Definition.Host.Tools[0] = "other"
	jobs[0].Definition.Host.Outputs[0] = "other"
	jobs[0].Definition.Host.InheritEnv[0] = "OTHER"
	jobs[0].Definition.Host.Toolchain[0] = "/other"
	jobs[0].Definition.Host.Capabilities[0] = "other"
	// The sole intended difference is the concrete producer identity.
	expected := consumer.Host.Clone()
	expected.Inputs[0].From = plan.Groups[0].Jobs[0].Name
	if !reflect.DeepEqual(jobs[1].Definition.Host, expected) {
		t.Fatalf("matrix declarations share storage: %#v", jobs[1].Definition.Host)
	}
	if producer.Host.Tools[0] != "sh" || consumer.Host.Inputs[0].Path != "out" {
		t.Fatal("expansion mutated authored host declarations")
	}
	producer.Strategy.Matrix.Axes["release"] = []string{"one", "two"}
	if _, err := w.Expand(nil); err == nil || !strings.Contains(err.Error(), "multiple matrix variants") {
		t.Fatalf("ambiguous host matrix producer accepted: %v", err)
	}
}

func TestHostPlatformBindingsValidateAfterExpansion(t *testing.T) {
	task := hostTaskForTest()
	task.Platform = &Platform{Matrix: "target"}
	task.Strategy = &Strategy{Matrix: &Matrix{Axes: map[string][]string{"target": {"linux/386"}}}}
	w := &Workflow{Version: 1, Tasks: map[string]Task{"run": task}}
	if _, err := w.Expand(nil); err == nil || !strings.Contains(err.Error(), "does not emulate") {
		t.Fatalf("expanded invalid host platform: %v", err)
	}
	task.Strategy.Matrix.Axes["target"] = []string{"linux/amd64", "linux/arm64"}
	if _, err := w.Expand(nil); err != nil {
		t.Fatal(err)
	}
}
