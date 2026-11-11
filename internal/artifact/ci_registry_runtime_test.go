package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

type ciRegistryTask struct {
	Name, Status, Kind, ImageID, Platform, Digest, Filesystem, OCI string
	Cache                                                          *struct{ Hits, Misses, Stored, Skipped, Errors int }
}

type ciRegistryReport struct {
	Status, Directory string
	Tasks             []ciRegistryTask
}

type ciRegistryFixture struct {
	t                    *testing.T
	ctx                  context.Context
	binary, engine, work string
	policy, registry     string
	checks               []string
	reports              map[string]string
	trace                []map[string]string
	mu                   sync.Mutex
	prepareCommand       func(string, string, *exec.Cmd)
	closeRegistry        func()
}

// TestCIRegistryCacheRoundTripWithCoopr exercises normal Stoke execution, with
// no task-result transport. Only the test-owned OCI registry is shared between
// independent native/local stores and physically different checkouts.
func TestCIRegistryCacheRoundTripWithCoopr(t *testing.T) {
	f := newCIRegistryFixture(t)
	caller := filepath.Join(f.work, "checkout-local")
	f.write(filepath.Join(caller, "busybox"), f.read(os.Getenv("STOKE_SMOKE_BUSYBOX")), 0755)
	f.write(filepath.Join(caller, "payload.txt"), []byte("tracked-one\n"), 0644)
	f.write(filepath.Join(caller, "producer.coopr"), []byte(`arg "FLAVOR"
from "scratch" {
    copy "busybox" "/bin/busybox"
    copy "payload.txt" "/value"
    arg "FLAVOR"
    env PARENT_FLAVOR="${FLAVOR}"
}


`), 0644)
	f.write(filepath.Join(caller, "native.coopr"), []byte(`arg "BASE"
from "${BASE}" {
    run {
        exec "/bin/busybox" "sh" "-eu" "-c" "/bin/busybox cp /value /native-result; /bin/busybox touch -t 202009131226.40 /native-result; echo CI_NATIVE_IMAGE_RAN"
    }
}
`), 0644)
	f.write(filepath.Join(caller, "component.coopr"), []byte(`package as="payload" {
    copy "payload.txt" "/component-value"
}
extend {
    copy "/component-value" "/component-value" from="payload"
}
`), 0644)
	f.write(filepath.Join(caller, "consume.coopr"), []byte(`arg "BASE"
arg "COMPONENT"
from "${BASE}" {
    arg "COMPONENT"
    component "${COMPONENT}"
    run {
        exec "/bin/busybox" "sh" "-eu" "-c" "/bin/busybox cat /component-value > /component-result; /bin/busybox touch -t 202009131226.40 /component-result; echo CI_COMPONENT_IMAGE_RAN"
    }
}
`), 0644)
	workflow := filepath.Join(f.work, "workflow.json")
	f.setWorkflow(workflow, "one")
	source, mirror := f.registry+"/cache/chain", f.registry+"/cache/return"
	producer, producerLog := f.run("local-producer", caller, workflow, []string{"--cache-to", source}, true)
	f.requireResults(producer, "tracked-one\n")
	f.requireMarkers(producerLog, []string{"CI_FROM_RAN", "CI_COPY_RAN", "CI_STAGE_RAN", "CI_NATIVE_IMAGE_RAN", "CI_COMPONENT_IMAGE_RAN", "CI_FRESH_RAN"}, true)
	f.checks = append(f.checks, "local-input-keyed-producer-exports-to-test-registry")
	ciCaller := filepath.Join(f.work, "unrelated-ci-checkout-root")
	f.copyCheckout(caller, ciCaller)
	consumer, consumerLog := f.run("ci-consumer", ciCaller, workflow, []string{"--cache-from", source, "--cache-to", mirror}, true)
	f.requireResults(consumer, "tracked-one\n")
	f.requireMarkers(consumerLog, []string{"CI_FROM_RAN", "CI_COPY_RAN", "CI_STAGE_RAN", "CI_NATIVE_IMAGE_RAN", "CI_COMPONENT_IMAGE_RAN"}, false)
	f.requireMarkers(consumerLog, []string{"CI_FRESH_RAN"}, true)
	f.requireHits(consumer, "from", "copied", "mounted")
	f.checks = append(f.checks, "fresh-ci-store-and-different-checkout-root-restore-data-edges-from-registry")
	returnCaller := filepath.Join(f.work, "another-local-checkout-root")
	f.copyCheckout(caller, returnCaller)
	returned, returnLog := f.run("local-return", returnCaller, workflow, []string{"--cache-from", mirror}, true)
	f.requireResults(returned, "tracked-one\n")
	f.requireHits(returned, "from", "copied", "mounted")
	f.requireMarkers(returnLog, []string{"CI_FROM_RAN", "CI_COPY_RAN", "CI_STAGE_RAN", "CI_NATIVE_IMAGE_RAN", "CI_COMPONENT_IMAGE_RAN"}, false)
	f.requireMarkers(returnLog, []string{"CI_FRESH_RAN"}, true)
	f.checks = append(f.checks, "ci-registry-writes-reused-by-another-fresh-local-store")
	// Configuration changes intentionally produce different native image IDs.
	// COPY and stage mounts consume the same measured filesystem, whereas FROM
	// and native image inheritance also consume configuration and must miss.
	f.setWorkflow(workflow, "two")
	variant, variantLog := f.run("config-only-source-id-change", ciCaller, workflow, []string{"--cache-from", source}, true)
	f.requireResults(variant, "tracked-one\n")
	before, after := f.task(producer, "producer"), f.task(variant, "producer")
	if before.ImageID == after.ImageID {
		t.Fatal("config change did not produce distinct native source image handles")
	}
	if !reflect.DeepEqual(f.imageConfig(before.OCI).RootFS, f.imageConfig(after.OCI).RootFS) {
		t.Fatal("configuration-only fixture changed filesystem layer identities")
	}
	f.requireMarkers(variantLog, []string{"CI_FROM_RAN", "CI_NATIVE_IMAGE_RAN", "CI_COMPONENT_IMAGE_RAN", "CI_FRESH_RAN"}, true)
	f.requireMarkers(variantLog, []string{"CI_COPY_RAN", "CI_STAGE_RAN"}, false)
	f.requireHits(variant, "copied", "mounted")
	f.write(filepath.Join(f.work, "source-identities.json"), f.encode(map[string]any{
		"original": before, "configurationChanged": after,
		"copyAndMountContentUnchanged": true, "copyAndMountMustReuse": true,
	}), 0600)
	f.checks = append(f.checks, "filesystem-consumers-ignore-store-handles-and-unrelated-config-while-from-invalidates")
	// Source bytes are tracked inputs, unlike the explicitly fresh-only branch.
	f.setWorkflow(workflow, "one")
	f.write(filepath.Join(ciCaller, "payload.txt"), []byte("tracked-two\n"), 0644)
	changed, changedLog := f.run("changed-source", ciCaller, workflow, []string{"--cache-from", source}, true)
	f.requireResults(changed, "tracked-two\n")
	if task := f.task(changed, "copied"); task.Cache == nil || task.Cache.Misses == 0 {
		t.Fatal("changed input did not report a literal cache miss")
	}
	f.requireMarkers(changedLog, []string{"CI_FROM_RAN", "CI_COPY_RAN", "CI_STAGE_RAN", "CI_NATIVE_IMAGE_RAN", "CI_COMPONENT_IMAGE_RAN", "CI_FRESH_RAN"}, true)
	f.checks = append(f.checks, "changed-source-invalidates-dependent-registry-results")
	// The absence of an entry must execute, not turn into a failed dependency.
	miss, missLog := f.run("empty-registry", caller, workflow, []string{"--cache-from", f.registry + "/cache/empty"}, true)
	f.requireResults(miss, "tracked-one\n")
	f.requireMarkers(missLog, []string{"CI_FROM_RAN", "CI_COPY_RAN", "CI_STAGE_RAN", "CI_NATIVE_IMAGE_RAN", "CI_COMPONENT_IMAGE_RAN", "CI_FRESH_RAN"}, true)
	f.checks = append(f.checks, "empty-registry-miss-executes-the-authored-graph")
	// A failed fresh prerequisite remains a real gate, not cached pass evidence.
	f.write(workflow, f.encode(map[string]any{"version": 1, "tasks": map[string]any{
		"failure": map[string]any{"image": "scratch", "cache": false, "steps": []any{
			ciRegistryCopy("", "busybox", "/bin/busybox"),
			ciRegistryRun("echo CI_FAILED_TEST_RAN; exit 7"),
		}},
		"blocked": map[string]any{"image": "scratch", "needs": []string{"failure"}, "steps": []any{
			ciRegistryCopy("", "busybox", "/bin/busybox"), ciRegistryRun("echo CI_BLOCKED_RAN"),
		}},
	}}), 0600)
	failed, failedLog := f.run("failed-prerequisite", caller, workflow, []string{"--cache-from", source}, false)
	if f.task(failed, "failure").Status != "failed" || f.task(failed, "blocked").Status != "skipped" {
		t.Fatalf("fresh failure did not gate downstream: %+v", failed)
	}
	f.requireMarkers(failedLog, []string{"CI_FAILED_TEST_RAN"}, true)
	f.requireMarkers(failedLog, []string{"CI_BLOCKED_RAN"}, false)
	f.checks = append(f.checks, "fresh-failure-blocks-downstream-without-success-receipts")
}

// TestCIRegistryCacheInputInvalidationWithCoopr keeps image publication confined
// to this process's registry. The ARM case only copies bytes; it does not claim
// native ARM or emulated RUN qualification.
func TestCIRegistryCacheInputInvalidationWithCoopr(t *testing.T) {
	f := newCIRegistryFixture(t)
	caller := filepath.Join(f.work, "checkout")
	f.write(filepath.Join(caller, "payload.txt"), []byte("platform-input\n"), 0644)
	baseOne := f.pushBaseImage("base", map[string][]byte{"bin/busybox": f.read(os.Getenv("STOKE_SMOKE_BUSYBOX")), "base-value": []byte("base-one\n")}, true)
	workflow := filepath.Join(f.work, "inputs-workflow.json")
	cache := f.registry + "/cache/resolutions"
	set := func(image, input, workdir string) {
		t.Helper()
		f.write(workflow, f.encode(map[string]any{"version": 1, "tasks": map[string]any{
			"resolved":   map[string]any{"image": image, "steps": []any{ciRegistryRun("/bin/busybox cp /base-value /resolved-result; echo CI_BASE_RAN")}},
			"configured": map[string]any{"image": image, "env": map[string]string{"INPUT": input}, "workdir": workdir, "steps": []any{ciRegistryRun(`printf '%s|%s\n' "$INPUT" "$PWD" >/config-result; echo CI_CONFIG_RAN`)}},
		}}), 0600)
	}
	set(baseOne, "one", "/work-one")
	cold, coldLog := f.run("inputs-cold", caller, workflow, []string{"--cache-to", cache}, true)
	f.requireMarkers(coldLog, []string{"CI_BASE_RAN", "CI_CONFIG_RAN"}, true)
	f.requireFile(cold, "resolved", "resolved-result", "base-one\n")
	f.requireFile(cold, "configured", "config-result", "one|/work-one\n")
	warm, warmLog := f.run("inputs-warm", caller, workflow, []string{"--cache-from", cache}, true)
	f.requireMarkers(warmLog, []string{"CI_BASE_RAN", "CI_CONFIG_RAN"}, false)
	f.requireHits(warm, "resolved", "configured")
	f.checks = append(f.checks, "digest-pinned-base-and-configuration-restore-in-independent-store")
	set(baseOne, "two", "/work-one")
	envChanged, envLog := f.run("changed-environment", caller, workflow, []string{"--cache-from", cache}, true)
	f.requireMarkers(envLog, []string{"CI_BASE_RAN"}, false)
	f.requireMarkers(envLog, []string{"CI_CONFIG_RAN"}, true)
	f.requireFile(envChanged, "configured", "config-result", "two|/work-one\n")
	set(baseOne, "one", "/work-two")
	workdirChanged, workdirLog := f.run("changed-workdir", caller, workflow, []string{"--cache-from", cache}, true)
	f.requireMarkers(workdirLog, []string{"CI_BASE_RAN"}, false)
	f.requireMarkers(workdirLog, []string{"CI_CONFIG_RAN"}, true)
	f.requireFile(workdirChanged, "configured", "config-result", "one|/work-two\n")
	f.checks = append(f.checks, "environment-and-workdir-change-invalidate-only-relevant-execution")
	baseTwo := f.pushBaseImage("base", map[string][]byte{"bin/busybox": f.read(os.Getenv("STOKE_SMOKE_BUSYBOX")), "base-value": []byte("base-two\n")}, true)
	if baseOne == baseTwo {
		t.Fatal("changed base content did not change its resolved digest")
	}
	set(baseTwo, "one", "/work-one")
	baseChanged, baseLog := f.run("changed-base-digest", caller, workflow, []string{"--cache-from", cache}, true)
	f.requireMarkers(baseLog, []string{"CI_BASE_RAN", "CI_CONFIG_RAN"}, true)
	f.requireFile(baseChanged, "resolved", "resolved-result", "base-two\n")
	f.checks = append(f.checks, "resolved-image-digest-and-content-change-invalidate")
	// The mutable name now selects baseTwo. A fresh runner must resolve it and
	// cannot claim baseOne's identity simply because its authored tag is equal.
	set(f.registry+"/base:fixture", "one", "/work-one")
	mutable, mutableLog := f.run("mutable-tag-resolution", caller, workflow, []string{"--cache-from", cache}, true)
	f.requireMarkers(mutableLog, []string{"CI_BASE_RAN", "CI_CONFIG_RAN"}, true)
	f.requireFile(mutable, "resolved", "resolved-result", "base-two\n")
	f.write(filepath.Join(f.work, "resolved-bases.json"), f.encode(map[string]any{"first": baseOne, "second": baseTwo, "mutableName": f.registry + "/base:fixture", "selectedContent": "base-two\n"}), 0600)
	f.checks = append(f.checks, "mutable-tag-is-resolved-again-on-an-independent-invocation")
	// Exercise platform identity without pretending this AMD64 BusyBox is an
	// ARM executable. No RUN exists in either platform's copy-only program.
	platformCache := f.registry + "/cache/platform"
	platformWorkflow := func(platform string) {
		f.write(workflow, f.encode(map[string]any{"version": 1, "tasks": map[string]any{"platform": map[string]any{"image": "scratch", "platform": platform, "steps": []any{ciRegistryCopy("", "payload.txt", "/platform-result")}}}}), 0600)
	}
	platformWorkflow("linux/amd64")
	_, _ = f.run("amd64-copy", caller, workflow, []string{"--cache-to", platformCache}, true)
	platformWorkflow("linux/arm64")
	arm, _ := f.run("arm64-copy-miss", caller, workflow, []string{"--cache-from", platformCache, "--cache-to", platformCache}, true)
	if task := f.task(arm, "platform"); task.Platform != "linux/arm64" || task.Cache == nil || task.Cache.Hits != 0 || task.Cache.Misses == 0 {
		t.Fatalf("target platform did not partition input identity: %+v", task)
	}
	armWarm, _ := f.run("arm64-copy-warm", caller, workflow, []string{"--cache-from", platformCache}, true)
	f.requireHits(armWarm, "platform")
	f.checks = append(f.checks, "platform-partitions-copy-cache-no-ARM-execution-claimed")
	// Fresh-only and live effects must execute even when immutable image inputs
	// and the selected registry cache are warm. Cache volumes remain empty on
	// each independent runner and are never required output transport.
	live := filepath.Join(f.work, "live")
	f.write(filepath.Join(live, "input"), []byte("live\n"), 0644)
	f.write(workflow, f.encode(map[string]any{"version": 1, "tasks": map[string]any{
		"live":     map[string]any{"image": baseOne, "mounts": []any{map[string]any{"type": "host", "source": live, "target": "/live", "read-only": false, "relabel": "shared"}}, "steps": []any{ciRegistryRun(`printf 'effect\n' >> /live/effects; echo CI_LIVE_RAN`)}},
		"snapshot": map[string]any{"image": baseOne, "mounts": []any{map[string]any{"type": "context", "source": ".", "target": "/snapshot", "read-only": false}}, "steps": []any{ciRegistryRun("printf generated >/snapshot/generated; /bin/busybox cp /snapshot/generated /snapshot-result; echo CI_SNAPSHOT_RAN")}},
		"volume":   map[string]any{"image": baseOne, "cache": false, "mounts": []any{map[string]any{"type": "cache", "id": "ci-disposable", "sharing": "locked", "target": "/cache"}}, "steps": []any{ciRegistryRun("test ! -e /cache/seed; printf seed >/cache/seed; printf empty >/volume-result; echo CI_EMPTY_VOLUME_RAN")}},
	}}), 0600)
	for _, label := range []string{"effects-cold", "effects-warm"} {
		report, log := f.run(label, caller, workflow, []string{"--cache-from", cache, "--cache-to", cache}, true)
		f.requireMarkers(log, []string{"CI_LIVE_RAN", "CI_SNAPSHOT_RAN", "CI_EMPTY_VOLUME_RAN"}, true)
		f.requireFile(report, "snapshot", "snapshot-result", "generated")
		f.requireFile(report, "volume", "volume-result", "empty")
		if _, err := os.Stat(filepath.Join(caller, "generated")); !os.IsNotExist(err) {
			t.Fatalf("writable snapshot wrote back to caller: %v", err)
		}
	}
	if string(f.read(filepath.Join(live, "effects"))) != "effect\neffect\n" {
		t.Fatal("warm cache skipped or duplicated required live mutations")
	}
	f.checks = append(f.checks, "live-writes-and-disposable-context-effects-execute-empty-volumes-are-correct")
	// A configured read cache becoming unavailable is an optional miss. The
	// base image itself stays available in the separate test-owned registry.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadRepository := strings.TrimPrefix(dead.URL, "http://") + "/cache/unavailable"
	dead.Close()
	set(baseOne, "one", "/work-one")
	uncached, unavailableLog := f.run("unavailable-read-cache", caller, workflow, []string{"--cache-from", deadRepository}, true)
	f.requireMarkers(unavailableLog, []string{"CI_BASE_RAN", "CI_CONFIG_RAN"}, true)
	f.requireFile(uncached, "resolved", "resolved-result", "base-one\n")
	f.checks = append(f.checks, "unavailable-read-cache-rebuilds-from-declared-inputs")
}

// History is optional in an OCI image configuration. Keep this independently
// named regression so a successful history-bearing fixture cannot hide a
// restoration failure for otherwise valid images without history records.
func TestCIRegistryCacheOptionalImageHistoryWithCoopr(t *testing.T) {
	f := newCIRegistryFixture(t)
	caller := filepath.Join(f.work, "checkout")
	if err := os.Mkdir(caller, 0700); err != nil {
		t.Fatal(err)
	}
	image := f.pushBaseImage("history-absent", map[string][]byte{"bin/busybox": f.read(os.Getenv("STOKE_SMOKE_BUSYBOX")), "base-value": []byte("optional-history\n")}, false)
	workflow := filepath.Join(f.work, "optional-history.json")
	f.write(workflow, f.encode(map[string]any{"version": 1, "tasks": map[string]any{
		"resolved": map[string]any{"image": image, "steps": []any{ciRegistryRun("/bin/busybox cp /base-value /resolved-result; echo CI_OPTIONAL_HISTORY_RAN")}},
	}}), 0600)
	cache := f.registry + "/cache/optional-history"
	cold, coldLog := f.run("history-absent-cold", caller, workflow, []string{"--cache-to", cache}, true)
	f.requireMarkers(coldLog, []string{"CI_OPTIONAL_HISTORY_RAN"}, true)
	f.requireFile(cold, "resolved", "resolved-result", "optional-history\n")
	warm, warmLog := f.run("history-absent-warm", caller, workflow, []string{"--cache-from", cache}, true)
	f.requireMarkers(warmLog, []string{"CI_OPTIONAL_HISTORY_RAN"}, false)
	f.requireHits(warm, "resolved")
	f.requireFile(warm, "resolved", "resolved-result", "optional-history\n")
	f.checks = append(f.checks, "optional-absent-image-history-registry-restoration")
}

// This witness bounds the existing resolution contract: default pulls retain a
// mutable selection within one shared native store, but a later independent
// store selects the retagged content. The pinned control remains stable in both.
func TestCIRegistryMutableResolutionScopeWithCoopr(t *testing.T) {
	f := newCIRegistryFixture(t)
	caller := filepath.Join(f.work, "checkout")
	if err := os.Mkdir(caller, 0700); err != nil {
		t.Fatal(err)
	}
	proofs := map[string]any{}
	for _, pinned := range []bool{false, true} {
		label := "mutable"
		if pinned {
			label = "pinned"
		}
		first := f.pushBaseImage(label, map[string][]byte{"bin/busybox": f.read(os.Getenv("STOKE_SMOKE_BUSYBOX")), "base-value": []byte("before\n")}, true)
		selector := f.registry + "/" + label + ":fixture"
		if pinned {
			selector = first
		}
		gates := &mountsRuntimeFixture{t: t, ctx: f.ctx, work: f.work}
		files, mount, closeGate := gates.fifoGate(label, "ready", "release")
		workflow := filepath.Join(f.work, label+".json")
		f.write(workflow, f.encode(map[string]any{"version": 1, "tasks": map[string]any{
			"first":  map[string]any{"image": selector, "cache": false, "mounts": []any{mount}, "steps": []any{ciRegistryRun("/bin/busybox cp /base-value /selection; printf r >/gate/ready; /bin/busybox head -c 1 /gate/release >/released; echo CI_RESOLUTION_FIRST_RAN")}},
			"second": map[string]any{"image": selector, "cache": false, "needs": []string{"first"}, "steps": []any{ciRegistryRun("/bin/busybox cp /base-value /selection; echo CI_RESOLUTION_SECOND_RAN")}},
		}}), 0600)
		command := f.command(label+"-resolution", caller, workflow, nil)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = command.Process.Kill() }()
		wait := make(chan error, 1)
		go func() { wait <- command.Wait() }()
		ready := make(chan error, 1)
		go func() {
			var value [1]byte
			_, err := io.ReadFull(files["ready"], value[:])
			ready <- err
		}()
		select {
		case err := <-ready:
			if err != nil {
				_ = command.Process.Kill()
				t.Fatal(err)
			}
		case err := <-wait:
			t.Fatalf("%s exited before resolution gate: %v\n%s", label, err, &stderr)
		case <-f.ctx.Done():
			_ = command.Process.Kill()
			t.Fatal(f.ctx.Err())
		}
		second := f.pushBaseImage(label, map[string][]byte{"bin/busybox": f.read(os.Getenv("STOKE_SMOKE_BUSYBOX")), "base-value": []byte("after\n")}, true)
		if _, err := files["release"].Write([]byte("r")); err != nil {
			_ = command.Process.Kill()
			t.Fatal(err)
		}
		select {
		case err := <-wait:
			if err != nil {
				t.Fatalf("%s workflow failed: %v\n%s", label, err, &stderr)
			}
		case <-f.ctx.Done():
			_ = command.Process.Kill()
			t.Fatal(f.ctx.Err())
		}
		closeGate()
		f.write(filepath.Join(f.work, label+"-resolution.stdout"), stdout.Bytes(), 0600)
		f.write(filepath.Join(f.work, label+"-resolution.stderr"), stderr.Bytes(), 0600)
		var report ciRegistryReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		f.reports[label+"-resolution"] = report.Directory
		f.requireFile(report, "first", "selection", "before\n")
		f.requireFile(report, "second", "selection", "before\n")
		f.requireMarkers(stderr.Bytes(), []string{"CI_RESOLUTION_FIRST_RAN", "CI_RESOLUTION_SECOND_RAN"}, true)
		// Keep only the second task's work for the fresh-store control. This is
		// an independent invocation, not a fabricated success for the first gate.
		f.write(workflow, f.encode(map[string]any{"version": 1, "tasks": map[string]any{
			"fresh": map[string]any{"image": selector, "cache": false, "steps": []any{ciRegistryRun("/bin/busybox cp /base-value /selection; echo CI_RESOLUTION_FRESH_RAN")}},
		}}), 0600)
		fresh, freshLog := f.run(label+"-independent-resolution", caller, workflow, nil, true)
		expected := "after\n"
		if pinned {
			expected = "before\n"
		}
		f.requireFile(fresh, "fresh", "selection", expected)
		f.requireMarkers(freshLog, []string{"CI_RESOLUTION_FRESH_RAN"}, true)
		proofs[label] = map[string]any{"authoredSelector": selector, "firstPublished": first, "afterRetag": second, "sharedStoreFirstContent": "before\n", "sharedStoreSecondContent": "before\n", "independentStoreContent": expected, "retagProtocol": "host-owned-fifo-before-second-task-admission"}
	}
	f.write(filepath.Join(f.work, "resolution-scope.json"), f.encode(proofs), 0600)
	f.checks = append(f.checks, "mutable-selection-sticks-within-default-shared-store-but-not-independent-stores-pinned-control-stable")
}

func newCIRegistryFixture(t *testing.T) *ciRegistryFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("requires rootless execution and explicit Stoke/Coopr/BusyBox binaries")
	}
	for _, name := range []string{"STOKE_BINARY", "STOKE_COOPR_BINARY", "STOKE_SMOKE_BUSYBOX"} {
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("%s must select the configured executable", name)
		}
		if info, err := os.Stat(value); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("%s unavailable: %v", name, err)
		}
	}
	parent := os.Getenv("STOKE_CACHE_CI_EVIDENCE_DIR")
	if parent == "" {
		t.Fatal("STOKE_CACHE_CI_EVIDENCE_DIR must select a disk-backed evidence parent")
	}
	work, err := os.MkdirTemp(parent, "registry-roundtrip-")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 9*time.Minute)
	t.Cleanup(cancel)
	f := &ciRegistryFixture{t: t, ctx: ctx, binary: os.Getenv("STOKE_BINARY"), engine: os.Getenv("STOKE_COOPR_BINARY"), work: work, policy: filepath.Join(work, "policy.json"), reports: map[string]string{}}
	f.write(f.policy, []byte(`{"default":[{"type":"reject"}],"transports":{"oci":{"":[{"type":"insecureAcceptAnything"}]},"oci-archive":{"":[{"type":"insecureAcceptAnything"}]},"containers-storage":{"":[{"type":"insecureAcceptAnything"}]},"docker":{"":[{"type":"insecureAcceptAnything"}]}}}`), 0600)
	storage := registryFixture(t)
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.trace = append(f.trace, map[string]string{"method": r.Method, "path": r.URL.Path})
		f.mu.Unlock()
		storage.Config.Handler.ServeHTTP(w, r)
	}))
	f.closeRegistry = registry.Close
	t.Cleanup(f.closeRegistry)
	f.registry = strings.TrimPrefix(registry.URL, "http://")
	t.Cleanup(func() {
		f.write(filepath.Join(work, "summary.json"), f.encode(map[string]any{
			"passed": !t.Failed(), "evidence": work, "checks": f.checks, "reports": f.reports,
			"scope": "independent checkout/native/cache stores on one " + runtime.GOOS + "/" + runtime.GOARCH + " host; provider admission is local to this fixture",
		}), 0600)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.write(filepath.Join(work, "registry-requests.json"), f.encode(f.trace), 0600)
	})
	t.Logf("Retained input-keyed registry acceptance evidence: %s", work)
	return f
}

func (f *ciRegistryFixture) setWorkflow(path, flavor string) {
	f.t.Helper()
	f.write(path, f.encode(map[string]any{"version": 1, "tasks": map[string]any{
		"producer": map[string]any{"coopr": map[string]any{"files": []string{"producer.coopr"}, "args": map[string]string{"FLAVOR": flavor}}},
		"from":     map[string]any{"from": "producer", "steps": []any{ciRegistryRun("/bin/busybox cp /value /from-result; /bin/busybox touch -t 202009131226.40 /from-result; echo CI_FROM_RAN")}},
		"copied": map[string]any{"image": "scratch", "steps": []any{
			ciRegistryCopy("", "busybox", "/bin/busybox"), ciRegistryCopy("producer", "/value", "/value"), ciRegistryRun("/bin/busybox cp /value /copy-result; /bin/busybox touch -t 202009131226.40 /copy-result; echo CI_COPY_RAN"),
		}},
		"mounted": map[string]any{"image": "scratch", "steps": []any{
			ciRegistryCopy("", "busybox", "/bin/busybox"), map[string]any{"run": []string{"/bin/busybox", "sh", "-eu", "-c", "/bin/busybox cp /input/value /mount-result; /bin/busybox touch -t 202009131226.40 /mount-result; echo CI_STAGE_RAN"}, "mounts": []any{map[string]any{"type": "stage", "from": "producer", "target": "/input"}}},
		}},
		"native":    map[string]any{"coopr": map[string]any{"files": []string{"native.coopr"}, "args": map[string]any{"BASE": map[string]string{"from": "producer"}}}},
		"component": map[string]any{"coopr": map[string]any{"kind": "component", "files": []string{"component.coopr"}}},
		"component-image": map[string]any{"coopr": map[string]any{"files": []string{"consume.coopr"}, "args": map[string]any{
			"BASE": map[string]string{"from": "producer"}, "COMPONENT": map[string]string{"from": "component"},
		}}},
		"fresh": map[string]any{"from": "producer", "cache": false, "steps": []any{ciRegistryRun("/bin/busybox cp /value /fresh-result; echo CI_FRESH_RAN")}},
	}}), 0600)
}

func ciRegistryCopy(from, source, destination string) map[string]any {
	value := map[string]any{"sources": []string{source}, "destination": destination}
	if from != "" {
		value["from"] = from
	}
	return map[string]any{"copy": value}
}

func ciRegistryRun(command string) map[string]any {
	return map[string]any{"run": []string{"/bin/busybox", "sh", "-eu", "-c", command}}
}

func (f *ciRegistryFixture) run(label, caller, workflow string, flags []string, success bool) (ciRegistryReport, []byte) {
	f.t.Helper()
	return f.runCommand(label, f.command(label, caller, workflow, flags), success)
}

func (f *ciRegistryFixture) runCommand(label string, cmd *exec.Cmd, success bool) (ciRegistryReport, []byte) {
	f.t.Helper()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	f.write(filepath.Join(f.work, label+".stdout"), stdout.Bytes(), 0600)
	f.write(filepath.Join(f.work, label+".stderr"), stderr.Bytes(), 0600)
	if (err == nil) != success {
		f.t.Fatalf("%s success=%t err=%v\n%s", label, success, err, &stderr)
	}
	var report ciRegistryReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		f.t.Fatalf("%s report: %v\n%s", label, err, &stderr)
	}
	f.reports[label] = report.Directory
	var emitted, persisted any
	if err := json.Unmarshal(stdout.Bytes(), &emitted); err != nil {
		f.t.Fatal(err)
	}
	if err := json.Unmarshal(f.read(filepath.Join(report.Directory, "run.json")), &persisted); err != nil || !reflect.DeepEqual(emitted, persisted) {
		f.t.Fatalf("%s emitted/persisted reports differ: %v", label, err)
	}
	if success && report.Status != "passed" || !success && report.Status != "failed" {
		f.t.Fatalf("%s wrong report status: %+v", label, report)
	}
	return report, stderr.Bytes()
}

func (f *ciRegistryFixture) command(label, caller, workflow string, flags []string) *exec.Cmd {
	f.t.Helper()
	args := []string{"run", "-f", workflow, "--context", caller, "--coopr-binary", f.engine, "--signature-policy", f.policy, "--tls-verify=false", "--cache-dir", filepath.Join(f.work, label, "cache")}
	return f.commandFor(label, caller, append(args, flags...))
}

func (f *ciRegistryFixture) commandFor(label, caller string, args []string) *exec.Cmd {
	f.t.Helper()
	home := filepath.Join(f.work, label, "home")
	if err := os.MkdirAll(home, 0700); err != nil {
		f.t.Fatal(err)
	}
	command := exec.CommandContext(f.ctx, f.binary, args...)
	command.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(f.work, label, "state"), "XDG_DATA_HOME="+filepath.Join(f.work, label, "data"), "XDG_CACHE_HOME="+filepath.Join(f.work, label, "xdg-cache"), "XDG_CONFIG_HOME="+filepath.Join(f.work, label, "config"))
	if f.prepareCommand != nil {
		f.prepareCommand(label, caller, command)
	}
	return command
}

func (f *ciRegistryFixture) requireResults(report ciRegistryReport, expected string) {
	f.t.Helper()
	for name, path := range map[string]string{"producer": "value", "from": "from-result", "copied": "copy-result", "mounted": "mount-result", "native": "native-result", "component-image": "component-result", "fresh": "fresh-result"} {
		task := f.task(report, name)
		if task.Status != "passed" || string(f.read(filepath.Join(task.Filesystem, path))) != expected {
			f.t.Fatalf("%s content/status differs: %+v", name, task)
		}
	}
	if task := f.task(report, "component"); task.Status != "passed" || task.Kind != "component" {
		f.t.Fatalf("component result differs: %+v", task)
	}
}

func (f *ciRegistryFixture) requireFile(report ciRegistryReport, name, path, expected string) {
	f.t.Helper()
	task := f.task(report, name)
	if task.Status != "passed" || string(f.read(filepath.Join(task.Filesystem, path))) != expected {
		f.t.Fatalf("%s content/status differs: %+v", name, task)
	}
}

func (f *ciRegistryFixture) task(report ciRegistryReport, name string) ciRegistryTask {
	f.t.Helper()
	for _, task := range report.Tasks {
		if task.Name == name {
			return task
		}
	}
	f.t.Fatalf("task %q absent", name)
	return ciRegistryTask{}
}

func (f *ciRegistryFixture) requireMarkers(log []byte, markers []string, expected bool) {
	f.t.Helper()
	for _, marker := range markers {
		if got := mountsRuntimeExecuted(log, marker); got != expected {
			f.t.Fatalf("execution marker %s=%t, want %t; evidence %s", marker, got, expected, f.work)
		}
	}
}

func (f *ciRegistryFixture) requireHits(report ciRegistryReport, names ...string) {
	f.t.Helper()
	for _, name := range names {
		task := f.task(report, name)
		if task.Cache == nil || task.Cache.Hits == 0 || task.Cache.Misses != 0 {
			f.t.Fatalf("%s did not fully restore eligible instruction results: %+v", name, task.Cache)
		}
	}
}

func (f *ciRegistryFixture) copyCheckout(source, destination string) {
	f.t.Helper()
	root, err := os.Stat(source)
	if err != nil {
		f.t.Fatal(err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			f.t.Fatalf("fixture checkout unexpectedly contains directory %s", entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			f.t.Fatal(err)
		}
		f.write(filepath.Join(destination, entry.Name()), f.read(filepath.Join(source, entry.Name())), info.Mode().Perm())
	}
	// Captured context identity includes the root mtime. File creation must not
	// turn this equivalent-input copy into the separate fresh-metadata probe.
	if err := os.Chtimes(destination, root.ModTime(), root.ModTime()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *ciRegistryFixture) write(path string, data []byte, mode os.FileMode) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		f.t.Fatal(err)
	}
	stamp := time.Unix(1_600_000_000, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		f.t.Fatal(err)
	}
}

func (f *ciRegistryFixture) read(path string) []byte {
	f.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	return data
}

func (f *ciRegistryFixture) encode(value any) []byte {
	f.t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	return append(data, '\n')
}

func (f *ciRegistryFixture) imageConfig(directory string) v1.Image {
	f.t.Helper()
	var index v1.Index
	if err := json.Unmarshal(f.read(filepath.Join(directory, "index.json")), &index); err != nil || len(index.Manifests) != 1 {
		f.t.Fatalf("image index: %v", err)
	}
	blob := func(digest string) []byte {
		return f.read(filepath.Join(directory, "blobs", filepath.FromSlash(strings.Replace(digest, ":", "/", 1))))
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(blob(index.Manifests[0].Digest.String()), &manifest); err != nil {
		f.t.Fatal(err)
	}
	var config v1.Image
	if err := json.Unmarshal(blob(manifest.Config.Digest.String()), &config); err != nil {
		f.t.Fatal(err)
	}
	return config
}

func (f *ciRegistryFixture) pushBaseImage(repository string, files map[string][]byte, withHistory bool) string {
	f.t.Helper()
	layer := mountsRuntimeTar(f.t, files, 0644)
	store := memory.New()
	layerDescriptor := content.NewDescriptorFromBytes(v1.MediaTypeImageLayer, layer)
	image := v1.Image{Platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{layerDescriptor.Digest}}}
	if withHistory {
		stamp := time.Unix(0, 0).UTC()
		image.History = []v1.History{{Created: &stamp, CreatedBy: "ci registry fixture rootfs"}}
	}
	config := f.encode(image)
	configDescriptor := content.NewDescriptorFromBytes(v1.MediaTypeImageConfig, config)
	manifest := f.encode(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: configDescriptor, Layers: []v1.Descriptor{layerDescriptor}})
	manifestDescriptor := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, manifest)
	for _, blob := range []struct {
		descriptor v1.Descriptor
		data       []byte
	}{{layerDescriptor, layer}, {configDescriptor, config}, {manifestDescriptor, manifest}} {
		if err := store.Push(f.ctx, blob.descriptor, bytes.NewReader(blob.data)); err != nil {
			f.t.Fatal(err)
		}
	}
	images := &mountsRuntimeFixture{t: f.t, ctx: f.ctx, work: f.work, registry: f.registry}
	return images.push(store, manifestDescriptor, repository)
}
