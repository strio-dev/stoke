package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
)

func TestHostShebangScripts(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("requires provisioned Bash")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("requires provisioned Python for the non-shell interpreter proof")
	}
	w := hostFixture(t, fmt.Sprintf(`version: 1
tasks:
  scripts:
    platform: NATIVE
    env: {MESSAGE: kept}
    host: {tools: [%q, %q, sh, printf]}
    steps:
      - run: |
          #!%s
          [[ "$MESSAGE" == kept ]]
          printf 'bash\n'
      - run: |
          #!%s -u
          import os, pathlib, sys
          assert os.environ["MESSAGE"] == "kept"
          assert pathlib.Path(__file__).read_text().startswith("#!")
          assert sys.argv == [__file__]
          print("python")
      - run: |
          case "$-" in *u*) printf 'default\n';; *) exit 9;; esac
      - run: [printf, '%%s\n', '$MESSAGE']
`, bash, python, bash, python))
	for _, ci := range []bool{false, true} {
		t.Run(fmt.Sprintf("ci=%t", ci), func(t *testing.T) {
			options := hostRunOptions(t)
			var diagnostics bytes.Buffer
			options.Stderr = &diagnostics
			var report *Report
			var err error
			if ci {
				report, err = RunCI(t.Context(), w, CIRequest{Execute: []string{"scripts"}}, options)
			} else {
				report, err = Run(t.Context(), w, []string{"scripts"}, options)
			}
			if err != nil {
				t.Fatalf("%v\n%s", err, &diagnostics)
			}
			out, err := os.ReadFile(filepath.Join(report.Output, "scripts", "stdout.log"))
			if err != nil || string(out) != "bash\npython\ndefault\n$MESSAGE\n" {
				t.Fatalf("script/argv behavior: %q %v", out, err)
			}
		})
	}
}

func TestContainerShebangStagingAndIdentity(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  scripts:
    image: scratch
    steps:
      - run: |
          #!/bin/bash -eu
          printf script
      - run: 'printf default'
`)
	plan, err := w.Expand([]string{"scripts"})
	if err != nil {
		t.Fatal(err)
	}
	caps, err := RequiredCapabilities(nil, plan, nil, Options{})
	if err != nil || !slices.Contains(caps, "named-contexts") || !slices.Contains(caps, "run-mounts") {
		t.Fatalf("script capabilities: %v %v", caps, err)
	}
	var previous coopr.Program
	for range 2 {
		var staged string
		_, err := runLiteral(t.Context(), w, []string{"scripts"}, testOptions(t), func(_ context.Context, p coopr.Program, options coopr.Options) (coopr.Result, error) {
			staged = options.Contexts[workflow.ScriptContext]
			op := p.States[0].Operations[0]
			if len(op.Args) != 1 || !strings.HasPrefix(op.Args[0], workflow.ScriptMountRoot+"/") || len(op.Mounts) != 1 || !op.Mounts[0].ReadOnly {
				t.Fatalf("script was not a read-only executable file: %+v", op)
			}
			file := filepath.Join(staged, op.Mounts[0].Source)
			data, err := os.ReadFile(file)
			if err != nil || string(data) != w.Tasks["scripts"].Steps[0].Script {
				t.Fatal("script bytes changed", err)
			}
			info, err := os.Stat(file)
			if err != nil || info.Mode().Perm() != 0555 || info.ModTime().Unix() != 0 || p.States[0].Operations[1].Args[0] != "sh" {
				t.Fatal("permissions or following interpreter changed", err)
			}
			if len(previous.States) != 0 && !reflect.DeepEqual(previous, p) {
				t.Fatal("private staging path changed program identity")
			}
			previous = p
			return coopr.Result{ImageID: "result"}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(staged); !os.IsNotExist(err) {
			t.Fatal("script staging not cleaned", err)
		}
	}
}
