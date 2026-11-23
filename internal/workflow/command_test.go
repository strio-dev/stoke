package workflow

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestRunShorthandNormalizesToArgv(t *testing.T) {
	for _, value := range []string{`'printf "%s\n" "$MESSAGE"'`, "|\n          printf \"%s\\n\" \"$MESSAGE\"\n"} {
		w, err := Decode(strings.NewReader("version: 1\ntasks:\n  task:\n    image: scratch\n    steps:\n      - run: " + value + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		got := w.Tasks["task"].Steps[0].Run
		want := []string{"sh", "-eu", "-c", "--", "printf \"%s\\n\" \"$MESSAGE\""}
		if strings.HasPrefix(value, "|") {
			want[4] += "\n"
		}
		if !reflect.DeepEqual([]string(got), want) {
			t.Fatalf("run=%q want=%q", got, want)
		}
		explicit := *w
		explicit.Tasks = map[string]Task{"task": {Image: "scratch", Steps: []Step{{Run: want}}}}
		left, _ := json.Marshal(w)
		right, _ := json.Marshal(&explicit)
		if string(left) != string(right) {
			t.Fatal("shorthand changed the canonical task representation")
		}
		data, err := yaml.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		again, err := Decode(strings.NewReader(string(data)))
		if err != nil || !reflect.DeepEqual(w, again) {
			t.Fatalf("normalized YAML roundtrip: %v", err)
		}
	}
}

func TestRunRejectsInvalidShapes(t *testing.T) {
	for _, value := range []string{`''`, `'   '`, `"\n\t"`, `true`, `12`, `null`, `{script: echo}`, `[[echo]]`, `[{}]`, `[]`, `null, export: {source: /report, destination: report}`} {
		if _, err := Decode(strings.NewReader("version: 1\ntasks: {task: {image: scratch, steps: [{run: " + value + "}]}}")); err == nil {
			t.Errorf("accepted run: %s", value)
		}
	}
}

func TestStepsRejectInvalidShapesAndUnknownFields(t *testing.T) {
	const prefix = "version: 1\ntasks: {task: {image: scratch, steps: "
	for _, steps := range []string{"null", "{}", "[null]"} {
		if _, err := Decode(strings.NewReader(prefix + steps + "}}")); err == nil {
			t.Errorf("accepted steps: %s", steps)
		}
	}
	_, err := Decode(strings.NewReader(prefix + "[{run: [check], mounts: [{type: cache, target: /cache, typo: true}]}]}}"))
	if err == nil || !strings.Contains(err.Error(), "field typo not found") {
		t.Fatalf("nested unknown-field validation: %v", err)
	}
}

func TestRunShellBehaviorAndLiteralArgv(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "-proof"), []byte("#!"+shell+"\nprintf option-safe\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		value, output string
		failed        bool
	}{
		{`'printf "%s" "$MESSAGE"'`, "expanded", false},
		{`'false; printf unreachable'`, "", true},
		{`'printf "%s" "$STOKE_UNSET_SHELL_PROOF"'`, "", true},
		{`'-proof'`, "option-safe", false},
		{`[printf, '%s', '$MESSAGE']`, "$MESSAGE", false},
		{`[true]`, "", false}, // Preserve existing argv scalar decoding.
	} {
		w, err := Decode(strings.NewReader("version: 1\ntasks: {task: {image: scratch, steps: [{run: " + tc.value + "}]}}"))
		if err != nil {
			t.Fatal(err)
		}
		argv := w.Tasks["task"].Steps[0].Run
		cmd := exec.CommandContext(t.Context(), argv[0], argv[1:]...)
		cmd.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "MESSAGE=expanded"}
		out, err := cmd.Output()
		if (err != nil) != tc.failed || string(out) != tc.output {
			t.Errorf("run=%s output=%q err=%v", tc.value, out, err)
		}
	}
}

func TestRunShebangRoundtripAndValidation(t *testing.T) {
	for _, script := range []string{"#!/bin/bash -eu\nprintf '%s' bash\n", "#!/usr/bin/env -S python3 -u\nprint(__file__)\n"} {
		value, _ := json.Marshal(script)
		w, err := Decode(strings.NewReader("version: 1\ntasks: {task: {image: scratch, steps: [{run: " + string(value) + "}, {run: 'printf default'}]}}"))
		if err != nil {
			t.Fatal(err)
		}
		data, err := yaml.Marshal(w)
		if err != nil || !strings.Contains(string(data), "#!") {
			t.Fatalf("script not retained: %s %v", data, err)
		}
		again, err := Decode(strings.NewReader(string(data)))
		if err != nil || !reflect.DeepEqual(w, again) {
			t.Fatalf("shebang YAML roundtrip: %v", err)
		}
		data, err = json.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		var projected Workflow
		if err := json.Unmarshal(data, &projected); err != nil {
			t.Fatal(err)
		}
		if err := projected.Validate(); err != nil || !reflect.DeepEqual(w, &projected) {
			t.Fatalf("CI projection lost script: %v", err)
		}
		steps := w.Tasks["task"].Steps
		if steps[0].Script != script || steps[0].Run != nil {
			t.Fatal("shebang was not retained on its authored step")
		}
		if got := steps[1]; got.Script != "" || len(got.Run) != 5 || got.Run[0] != "sh" {
			t.Fatalf("following step changed: %+v", got)
		}
	}
	for _, script := range []string{"#!\ntrue", "#!python3\nprint(1)", "#!/bin/sh\r\necho wrong"} {
		value, _ := json.Marshal(script)
		if _, err := Decode(strings.NewReader("version: 1\ntasks: {task: {image: scratch, steps: [{run: " + string(value) + "}]}}")); err == nil {
			t.Errorf("accepted invalid shebang %q", script)
		}
	}
}
