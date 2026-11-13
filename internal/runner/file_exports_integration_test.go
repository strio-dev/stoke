package runner

import (
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
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
	busybox := os.Getenv("STOKE_SMOKE_BUSYBOX")
	if busybox == "" {
		t.Fatal("STOKE_SMOKE_BUSYBOX must select the static BusyBox fixture")
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
				return Options{ContextDir: caller, StateDir: filepath.Join(root, phase, "state"), CacheDir: filepath.Join(root, "cache"), CooprBinary: engine, Runtime: runtime, Network: "none", StorageDriver: "vfs", SignaturePolicy: policy, Stdout: stdout, Stderr: os.Stderr}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			var digest string
			var exportedImage ciImage
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
					exportedImage, err = readCIImage(ctx, result.OCI, result.Digest, result.Platform)
					if err != nil {
						t.Fatal(err)
					}
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
			if err := os.Rename(filepath.Join(caller, "exported"), filepath.Join(root, "warm-files")); err != nil {
				t.Fatal(err)
			}
			withoutExports := *w
			withoutExports.Tasks = maps.Clone(w.Tasks)
			task := w.Tasks["build"].Clone()
			steps := task.Steps[:0]
			for _, step := range task.Steps {
				if step.Export == nil {
					steps = append(steps, step)
				}
			}
			task.Steps = steps
			withoutExports.Tasks["build"] = task
			var plainLog bytes.Buffer
			plainOptions := options("without-exports", &plainLog)
			plainOptions.CacheDir = filepath.Join(root, "plain-cache")
			plain, err := Run(ctx, &withoutExports, []string{"build"}, plainOptions)
			if err != nil || plain == nil || plain.Status != "passed" {
				t.Fatalf("build without export checkpoints: %+v %v", plain, err)
			}
			plainResult := plain.Tasks[len(plain.Tasks)-1]
			if plainResult.Filesystem == "" || len(plainResult.Exports) != 0 || !strings.Contains(plainLog.String(), "STOKE_PREFIX_EXECUTED") || !strings.Contains(plainLog.String(), "STOKE_LATER_EXECUTED") {
				t.Fatalf("ordinary output default or fresh execution changed: %+v %s", plainResult, &plainLog)
			}
			plainImage, err := readCIImage(ctx, plainResult.OCI, plainResult.Digest, plainResult.Platform)
			if err != nil {
				t.Fatal(err)
			}
			// Fresh RUNs have different creation/history and filesystem timestamps.
			// Compare runtime config and authored history; warm identity above is exact.
			exportedImage.config.Created, plainImage.config.Created = nil, nil
			exportedImage.config.RootFS.DiffIDs, plainImage.config.RootFS.DiffIDs = nil, nil
			for i := range exportedImage.config.History {
				exportedImage.config.History[i].Created = nil
			}
			for i := range plainImage.config.History {
				plainImage.config.History[i].Created = nil
			}
			if !reflect.DeepEqual(exportedImage.config, plainImage.config) || len(exportedImage.manifest.Layers) != len(plainImage.manifest.Layers) {
				t.Fatal("export checkpoints changed final config, authored history or layer count")
			}
			if got, err := os.ReadFile(filepath.Join(plainResult.Filesystem, "report")); err != nil || string(got) != "v2" {
				t.Fatalf("ordinary final filesystem differs from selected output: %q %v", got, err)
			}
			failed := fixture(t, "version: 1\ntasks:\n  check:\n    cache: false\n"+base+`      - run: [/bin/busybox, sh, -c, 'printf diagnostic > /report; exit 23']
      - export: {source: /report, destination: failed-report}
  gate:
    image: scratch
    needs: [check]
`)
			var stdout bytes.Buffer
			var report *Report
			if native {
				report, err = Run(ctx, failed, []string{"gate"}, options("failed", &stdout))
			} else {
				report, err = RunCI(ctx, failed, CIRequest{Targets: []string{"gate"}, Execute: []string{"check", "gate"}}, options("failed", &stdout))
			}
			if err == nil || !strings.Contains(err.Error(), "23") || report == nil || report.Status != "failed" || report.Tasks[len(report.Tasks)-1].Status != "skipped" {
				t.Fatalf("failed export opened a success gate: %+v %v", report, err)
			}
			if !native && (report.Tasks[0].OCI != "" || report.Tasks[0].Digest != "") {
				t.Fatal("failed pure CI gate exported an unused image")
			}
			got, err := os.ReadFile(filepath.Join(caller, "failed-report"))
			if err != nil || string(got) != "diagnostic" {
				t.Fatalf("failed RUN report lost before teardown: %q %v", got, err)
			}
		})
	}
}
