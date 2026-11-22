package artifact

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// This opt-in dogfood runs the authored example unchanged, including its real
// Go build and fresh test/vet matrix. Registry/image/module downloads need the
// runner's ordinary network access; only the process-owned cache registry is
// written. Every phase uses ordinary stoke run; generated-stage execution and
// provider success gates require separate qualification.
func TestCIRealWorkflowRegistryWithCoopr(t *testing.T) {
	f := newCIRegistryFixture(t)
	source := os.Getenv("STOKE_REAL_CI_SOURCE_DIR")
	if source == "" {
		t.Fatal("STOKE_REAL_CI_SOURCE_DIR must select the complete actual Stoke checkout")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Minute)
	t.Cleanup(cancel)
	f.ctx = ctx
	execution := f.configureRealCIExecution()
	// Forward to ordinary CLI validation; do not choose or fall back to a backend.
	var networkFlags []string
	if execution.network != "" {
		networkFlags = []string{"--network", execution.network}
	}
	shared := execution.users != nil
	caller := filepath.Join(f.work, "actual-checkout-snapshot")
	f.copyRealCheckout(source, caller, shared, false)
	for _, name := range []string{"go.mod", "go.sum", "examples/ci/stoke.yaml", "examples/ci/build.coopr", "examples/environments/environment.coopr"} {
		if !bytes.Equal(f.read(filepath.Join(source, name)), f.read(filepath.Join(caller, name))) {
			t.Fatalf("snapshot changed actual input %s", name)
		}
	}
	workflow := filepath.Join(caller, "examples/ci/stoke.yaml")
	repository := f.registry + "/cache/real-stoke"
	t.Log("ordinary workflow: initial local producer, fresh tests and package")
	local, localLog := f.run("real-local-producer", caller, workflow, append(slices.Clone(networkFlags), "--cache-from", repository, "--cache-to", repository), true)
	checksum := f.requireRealPackage(local)
	f.requireRealChecks(local, localLog, checksum)
	f.requireRealBuildBinary(local, checksum)
	f.pruneRealNative("real-local-producer", local, execution)
	returnedCaller := filepath.Join(f.work, "checkout-local-return")
	f.copyRealCheckout(caller, returnedCaller, shared, false)
	t.Log("ordinary workflow: local return using registry cache and fresh tests")
	returned, returnLog := f.run("real-local-return", returnedCaller, filepath.Join(returnedCaller, "examples/ci/stoke.yaml"), append(slices.Clone(networkFlags), "--cache-from", repository), true)
	f.requireRealBuildRestored(returnLog)
	f.requireRealChecks(returned, returnLog, checksum)
	f.requireRealBuildBinary(returned, checksum)
	if f.requireRealPackage(returned) != checksum {
		t.Fatal("return package binary differs from the tested producer")
	}
	f.pruneRealNative("real-local-return", returned, execution)
	// A fresh timestamp is a distinct probe: its cache observation is recorded,
	// never substituted for the strict equivalent-input roundtrip assertion.
	freshCaller := filepath.Join(f.work, "checkout-fresh-timestamps")
	f.copyRealCheckout(caller, freshCaller, shared, true)
	t.Log("real workflow: fresh-checkout timestamp cache probe")
	fresh, freshLog := f.run("real-fresh-checkout-build-probe", freshCaller, filepath.Join(freshCaller, "examples/ci/stoke.yaml"), append(slices.Clone(networkFlags), "--cache-from", repository, "build"), true)
	f.requireRealBuildBinary(fresh, checksum)
	f.pruneRealNative("real-fresh-checkout-build-probe", fresh, execution)
	f.write(filepath.Join(f.work, "real-workflow-proof.json"), f.encode(map[string]any{
		"binarySHA256": checksum, "freshChecksPerFullRun": 2, "equivalentInputBuildRUNRestored": true, "freshTimestampBuildRUNRestored": realBuildRestored(freshLog), "freshTimestampProbeUsesCompleteCheckout": true, "sourceOwnershipNormalization": shared, "coordinatorUID": os.Geteuid(), "storageDriver": execution.driver, "network": execution.network, "pruneNative": execution.pruneNative,
	}), 0600)
	f.checks = append(f.checks, "complete-actual-checkout-real-Go-build-fresh-test-vet-matrix-and-executable-package", "real-workflow-producer-mounted-tests-and-package-binary-hashes-match", "real-workflow-independent-invocation-native-build-RUN-cache-restoration", "fresh-complete-checkout-timestamp-probe-records-conservative-misses", "actual-authored-digest-pinned-base-inputs-no-global-mutable-resolution-lock-claim")
}

func (f *ciRegistryFixture) requireRealChecks(report ciRegistryReport, log []byte, checksum string) {
	f.t.Helper()
	count := 0
	for _, task := range report.Tasks {
		if strings.HasPrefix(task.Name, "checks--matrix-") {
			count++
			if task.Status != "passed" || task.Cache != nil && task.Cache.Hits != 0 {
				f.t.Fatalf("fresh check result: %+v", task)
			}
		}
	}
	f.requireRealMountedBinary(log, checksum, 2)
	if count != 2 {
		f.t.Fatalf("real check variants=%d", count)
	}
	if !bytes.Contains(log, []byte("github.com/strio-dev/stoke/")) {
		f.t.Fatal("real go test -count=1 package output missing")
	}
}

func (f *ciRegistryFixture) requireRealBuildRestored(log []byte) {
	f.t.Helper()
	if !realBuildRestored(log) {
		f.t.Fatalf("authored native go-build RUN did not report its own cache restoration; inspect %s", f.work)
	}
}

func realBuildRestored(log []byte) bool {
	lines := strings.Split(string(log), "\n")
	for i, line := range lines {
		if !strings.Contains(line, ": RUN ") || !strings.Contains(line, "go build -mod=readonly -trimpath -o /stoke ./cmd/stoke") {
			continue
		}
		step := strings.Index(line, "STEP ")
		if step < 0 {
			continue
		}
		prefix := line[:step]
		for _, next := range lines[i+1:] {
			if !strings.HasPrefix(next, prefix) {
				continue
			}
			if strings.Contains(next, "Using cache") {
				return true
			}
			if strings.Contains(next, "STEP ") || strings.Contains(next, "COMMIT") {
				break
			}
		}
	}
	return false
}

func (f *ciRegistryFixture) requireRealMountedBinary(log []byte, checksum string, expected int) {
	f.t.Helper()
	count, checks := 0, 0
	for _, line := range strings.Split(string(log), "\n") {
		line = strings.TrimSpace(line)
		if line == "STOKE_BUILT_CLI_CHECK_PASSED" {
			checks++
		}
		if strings.HasPrefix(line, "STOKE_TESTED_BINARY_SHA256=") {
			if strings.TrimPrefix(line, "STOKE_TESTED_BINARY_SHA256=") != checksum {
				f.t.Fatalf("fresh check mounted different binary: %s", line)
			}
			count++
		}
	}
	if count != expected || checks != expected {
		f.t.Fatalf("mounted binary checksum observations=%d CLI checks=%d want=%d", count, checks, expected)
	}
}

func (f *ciRegistryFixture) requireRealBuildBinary(report ciRegistryReport, checksum string) {
	f.t.Helper()
	task := f.task(report, "build")
	actual, err := realImageFileSHA256(task.OCI, "/stoke", task.Digest)
	if err != nil || actual != checksum {
		f.t.Fatalf("canonical producer binary differs: %s %v", actual, err)
	}
}

func (f *ciRegistryFixture) requireRealPackage(report ciRegistryReport) string {
	f.t.Helper()
	task := f.task(report, "package")
	command := exec.CommandContext(f.ctx, filepath.Join(task.Filesystem, "dist/stoke"), "version")
	output, err := command.CombinedOutput()
	if err != nil || !bytes.Contains(bytes.ToLower(output), []byte("stoke")) {
		f.t.Fatalf("actual packaged executable: %v\n%s", err, output)
	}
	checksum, err := realFileSHA256(filepath.Join(task.Filesystem, "dist/stoke"))
	if err != nil {
		f.t.Fatal(err)
	}
	return checksum
}
