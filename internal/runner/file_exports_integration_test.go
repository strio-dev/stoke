package runner

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStageExportsWithCoopr(t *testing.T) {
	if testing.Short() {
		t.Skip("requires an explicit development engine and rootless runtime")
	}
	engine, evidence := os.Getenv("STOKE_COOPR_BINARY"), os.Getenv("STOKE_HOST_EVIDENCE_DIR")
	if engine == "" || evidence == "" {
		t.Fatal("select STOKE_COOPR_BINARY and STOKE_HOST_EVIDENCE_DIR")
	}
	runtime, err := nativeTestRuntime("STOKE_EXPORT_RUNTIME")
	if err != nil {
		t.Fatal(err)
	}
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatal("required static BusyBox fixture:", err)
	}
	data, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatal(err)
	}
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "literal", true: "native-environment"}[native], func(t *testing.T) {
			root, err := os.MkdirTemp(evidence, "stage-exports-")
			if err != nil {
				t.Fatal(err)
			}
			t.Log("Retained stage export evidence:", root)
			caller := filepath.Join(root, "caller")
			if err := os.Mkdir(caller, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(caller, "busybox"), data, 0755); err != nil {
				t.Fatal(err)
			}
			policy := filepath.Join(root, "policy.json")
			if err := os.WriteFile(policy, []byte(`{"default":[{"type":"reject"}],"transports":{"oci":{"":[{"type":"insecureAcceptAnything"}]},"containers-storage":{"":[{"type":"insecureAcceptAnything"}]}}}`), 0600); err != nil {
				t.Fatal(err)
			}
			base := "    image: scratch\n    steps:\n      - copy: {sources: [busybox], destination: /bin/busybox}\n"
			if native {
				if err := os.WriteFile(filepath.Join(caller, "environment.coopr"), []byte("from \"scratch\" { copy \"busybox\" \"/bin/busybox\" }\n"), 0600); err != nil {
					t.Fatal(err)
				}
				base = "    environment: {files: [environment.coopr]}\n    steps:\n"
			}
			w := fixture(t, "version: 1\ntasks:\n  build:\n"+base+`      - run: [/bin/busybox, sh, -c, 'printf v1 > /report; echo STOKE_PREFIX_EXECUTED']
      - export: {source: /report, destination: exported/prefix}
      - run: [/bin/busybox, sh, -c, 'printf v2 > /report; echo STOKE_LATER_EXECUTED']
      - export: {source: /report, destination: exported/final}
`)
			options := func(phase string, stdout *bytes.Buffer) Options {
				return Options{ContextDir: caller, StateDir: filepath.Join(root, phase, "state"), CacheDir: filepath.Join(root, "cache"), CooprBinary: engine, Runtime: runtime, Network: "none", StorageDriver: "vfs", SignaturePolicy: policy, Stdout: stdout}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			var digest string
			for _, phase := range []string{"cold", "warm"} {
				var stdout bytes.Buffer
				report, err := Run(ctx, w, []string{"build"}, options(phase, &stdout))
				if err != nil || report == nil || report.Status != "passed" {
					t.Fatalf("%s stage exports: %+v %v", phase, report, err)
				}
				result := report.Tasks[len(report.Tasks)-1]
				if result.Name != "build" || result.Filesystem != "" || len(result.Exports) != 2 || result.Exports[0].Status != "passed" || result.Exports[1].Status != "passed" {
					t.Fatalf("%s selected output report: %+v", phase, result)
				}
				for name, want := range map[string]string{"prefix": "v1", "final": "v2"} {
					got, err := os.ReadFile(filepath.Join(caller, "exported", name))
					if err != nil || string(got) != want {
						t.Fatalf("%s %s did not snapshot its own position: %q %v", phase, name, got, err)
					}
				}
				if phase == "cold" {
					digest = result.Digest
					if !strings.Contains(stdout.String(), "STOKE_PREFIX_EXECUTED") || !strings.Contains(stdout.String(), "STOKE_LATER_EXECUTED") {
						t.Fatal("cold run did not execute both image mutations")
					}
					if err := os.Rename(filepath.Join(caller, "exported"), filepath.Join(root, "cold-files")); err != nil {
						t.Fatal(err)
					}
				} else if result.Digest != digest || strings.Contains(stdout.String(), "_EXECUTED") {
					t.Fatalf("warm export changed image identity or reran commands: %+v %s", result, &stdout)
				}
			}
			failed := fixture(t, "version: 1\ntasks:\n  check:\n    cache: false\n"+base+`      - run: [/bin/busybox, sh, -c, 'printf diagnostic > /report; exit 23']
      - export: {source: /report, destination: failed-report}
  gate:
    image: scratch
    needs: [check]
`)
			var stdout bytes.Buffer
			report, err := Run(ctx, failed, []string{"gate"}, options("failed", &stdout))
			if err == nil || !strings.Contains(err.Error(), "23") || report == nil || report.Status != "failed" || report.Tasks[len(report.Tasks)-1].Status != "skipped" {
				t.Fatalf("failed export opened a success gate: %+v %v", report, err)
			}
			got, err := os.ReadFile(filepath.Join(caller, "failed-report"))
			if err != nil || string(got) != "diagnostic" {
				t.Fatalf("failed RUN report lost before teardown: %q %v", got, err)
			}
		})
	}
}
