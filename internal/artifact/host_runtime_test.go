package artifact

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// This acceptance uses the actual Stoke CLI and a loopback registry, with no
// Coopr runtime or host/Nix security changes. It is explicitly provisioned.
func TestHostVisibleCIRegistryLocalRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("requires explicitly selected development Stoke executable")
	}
	binary := os.Getenv("STOKE_BINARY")
	if binary == "" {
		t.Fatal("STOKE_BINARY must select the development executable")
	}
	builder := os.Getenv("STOKE_HOST_BUILDER")
	if !filepath.IsAbs(builder) {
		t.Fatal("STOKE_HOST_BUILDER must select the reviewed static fixture executable")
	}
	parent := os.Getenv("STOKE_HOST_EVIDENCE_DIR")
	if parent == "" {
		t.Fatal("STOKE_HOST_EVIDENCE_DIR must select an owned evidence parent")
	}
	work, err := os.MkdirTemp(parent, "host-registry-")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	server := registryFixture(t)
	f := &ciRegistryFixture{t: t, ctx: ctx, binary: binary, engine: "/coopr-must-not-be-started", work: work, registry: strings.TrimPrefix(server.URL, "http://"), reports: map[string]string{}}
	f.policy = filepath.Join(work, "unused-policy.json")
	f.write(f.policy, []byte(`{"default":[{"type":"reject"}]}`), 0600)
	t.Cleanup(func() {
		f.write(filepath.Join(work, "summary.json"), f.encode(map[string]any{"passed": !t.Failed(), "reports": f.reports, "nativeSystem": runtime.GOOS + "/" + runtime.GOARCH, "sameUID": true, "hostedGitHubActions": false, "cooprStarted": false}), 0600)
	})
	t.Logf("Host registry acceptance evidence: %s", work)
	w := map[string]any{"version": 1, "tasks": map[string]any{
		"produce": map[string]any{"platform": runtime.GOOS + "/" + runtime.GOARCH, "cache": true, "host": map[string]any{"tools": []string{builder}, "inputs": []any{map[string]string{"path": "source"}}, "outputs": []string{"out"}, "effects": "captured", "toolchain": []string{builder}}, "steps": []any{map[string]any{"run": []string{builder, "source", "out/value"}}}},
		"tests":   map[string]any{"platform": runtime.GOOS + "/" + runtime.GOARCH, "host": map[string]any{"tools": []string{"/bin/sh"}, "inputs": []any{map[string]string{"from": "produce", "path": "out"}}, "outputs": []string{"checked"}}, "strategy": map[string]any{"matrix": map[string]any{"mode": []string{"a", "b"}}, "max-parallel": 1}, "steps": []any{map[string]any{"run": []string{"/bin/sh", "-c", "test \"$(cat out/value)\" = exact-input; printf '%s' \"$STOKE_MATRIX_MODE\" > checked; echo HOST_FRESH_TEST_EXECUTED"}}}},
		"package": map[string]any{"platform": runtime.GOOS + "/" + runtime.GOARCH, "needs": []string{"tests"}, "host": map[string]any{"tools": []string{"/bin/sh"}, "inputs": []any{map[string]string{"from": "produce", "path": "out"}}, "outputs": []string{"bundle"}}, "steps": []any{map[string]any{"run": []string{"/bin/sh", "-c", "cp out/value bundle; echo HOST_PACKAGE_EXECUTED"}}}},
	}}
	caller := filepath.Join(work, "local-checkout")
	f.write(filepath.Join(caller, "source"), []byte("exact-input"), 0644)
	f.write(filepath.Join(caller, "stoke.json"), f.encode(w), 0600)
	cache := f.registry + "/cache/host"
	local, log := f.run("local-first", caller, filepath.Join(caller, "stoke.json"), []string{"--allow-host", "--plain-http"}, true)
	f.requireMarkerCount(log, "HOST_PRODUCER_EXECUTED", 1)
	f.requireMarkerCount(log, "HOST_FRESH_TEST_EXECUTED", 2)
	producer := f.task(local, "produce")
	warm, log := f.run("local-warm-seed", caller, filepath.Join(caller, "stoke.json"), []string{"--allow-host", "--plain-http", "--cache-dir", filepath.Join(f.work, "local-first", "cache"), "--cache-to", cache}, true)
	if value := f.task(warm, "produce"); value.Cache == nil || value.Cache.Hits != 1 {
		t.Fatal("warm seed executed producer", value)
	}
	f.requireMarkerCount(log, "HOST_PRODUCER_EXECUTED", 0)
	ciCheckout := filepath.Join(work, "ci-checkout")
	f.write(filepath.Join(ciCheckout, "source"), f.read(filepath.Join(caller, "source")), 0644)
	f.write(filepath.Join(ciCheckout, "stoke.json"), f.read(filepath.Join(caller, "stoke.json")), 0600)
	plan := f.generateRegistryPlan("host", ciCheckout, cache)
	if len(plan.Groups) != 3 || !plan.RequiresResults {
		t.Fatal("visible native stages or exact file transport lost")
	}
	scope := f.exactScope("host", ciCheckout, plan)
	records := map[string]*exactNativeMetadata{}
	for _, group := range plan.Groups {
		for _, job := range group.Jobs {
			needs := plan.needs(t, job, records)
			report, log := f.runExactJob("ci-"+job.ID, ciCheckout, "host.plan.json", plan, job, scope, 1, needs, []string{"--allow-host"}, true)
			records[job.ID] = report.CI
			switch job.Task {
			case "produce":
				value := f.task(report.ciRegistryReport, "produce")
				if value.Cache == nil || value.Cache.Hits != 1 || value.Digest != producer.Digest {
					t.Fatalf("local->CI cache identity differs: %+v", value)
				}
				f.requireMarkerCount(log, "HOST_PRODUCER_EXECUTED", 0)
			case "tests":
				f.requireMarkerCount(log, "HOST_FRESH_TEST_EXECUTED", 1)
			}
		}
	}
	back, log := f.run("local-return", caller, filepath.Join(caller, "stoke.json"), []string{"--allow-host", "--plain-http", "--cache-from", cache}, true)
	if value := f.task(back, "produce"); value.Cache == nil || value.Cache.Hits != 1 || value.Digest != producer.Digest {
		t.Fatal("CI->local cached output changed", value)
	}
	f.requireMarkerCount(log, "HOST_FRESH_TEST_EXECUTED", 2)
	// A native failed prerequisite cannot admit the package job.
	packageJob := plan.job(t, "package")
	needs := plan.needs(t, packageJob, records)
	for key, entry := range needs {
		entry.Result = "failure"
		needs[key] = entry
		break
	}
	_, log = f.runExactJob("failed-native-gate", ciCheckout, "host.plan.json", plan, packageJob, scope, 1, needs, []string{"--allow-host"}, false)
	f.requireMarkerCount(log, "HOST_PACKAGE_EXECUTED", 0)
	// Missing exact bindings never cause an external producer replay.
	testJob := plan.group(t, "tests").Jobs[0]
	_, log = f.runExactJob("missing-native-input", ciCheckout, "host.plan.json", plan, testJob, scope, 1, exactNeeds{}, []string{"--allow-host"}, false)
	f.requireMarkerCount(log, "HOST_PRODUCER_EXECUTED", 0)
	// Prior-attempt immutable producer results can feed an explicit test retry.
	_, log = f.runExactJob("fresh-test-retry", ciCheckout, "host.plan.json", plan, testJob, scope, 2, plan.needs(t, testJob, records), []string{"--allow-host"}, true)
	f.requireMarkerCount(log, "HOST_FRESH_TEST_EXECUTED", 1)
	// Fresh nondeterministic producers keep one exact output across native gates.
	tasks := w["tasks"].(map[string]any)
	freshProducer := tasks["produce"].(map[string]any)
	freshProducer["cache"] = false
	freshHost := freshProducer["host"].(map[string]any)
	freshHost["tools"] = []string{"python3"}
	freshHost["toolchain"] = []string{}
	freshHost["effects"] = "external"
	freshProducer["steps"] = []any{map[string]any{"run": []string{"python3", "-c", "from pathlib import Path; import secrets; Path('out').mkdir(); Path('out/value').write_text(secrets.token_hex(16)); print('HOST_PRODUCER_EXECUTED')"}}}
	tasks["tests"].(map[string]any)["steps"] = []any{map[string]any{"run": []string{"/bin/sh", "-c", "test -s out/value; cp out/value checked; echo HOST_FRESH_TEST_EXECUTED"}}}
	f.write(filepath.Join(ciCheckout, "stoke.json"), f.encode(w), 0600)
	freshPlan := f.generateExactPlan("fresh-host", ciCheckout, "stoke.json", "", f.registry+"/results/fresh", nil, nil)
	freshScope := f.exactScope("fresh-host", ciCheckout, freshPlan)
	freshRecords := map[string]*exactNativeMetadata{}
	expected := ""
	for _, group := range freshPlan.Groups {
		for _, job := range group.Jobs {
			report, log := f.runExactJob("fresh-ci-"+job.ID, ciCheckout, "fresh-host.plan.json", freshPlan, job, freshScope, 1, freshPlan.needs(t, job, freshRecords), []string{"--allow-host"}, true)
			freshRecords[job.ID] = report.CI
			switch job.Task {
			case "produce":
				expected = string(f.read(filepath.Join(f.task(report.ciRegistryReport, "produce").Filesystem, "out/value")))
				f.requireMarkerCount(log, "HOST_PRODUCER_EXECUTED", 1)
			case "tests":
				if string(f.read(filepath.Join(f.task(report.ciRegistryReport, job.Execute[0]).Filesystem, "checked"))) != expected {
					t.Fatal("test consumed another fresh producer output")
				}
			case "package":
				if string(f.read(filepath.Join(f.task(report.ciRegistryReport, "package").Filesystem, "bundle"))) != expected {
					t.Fatal("package differs from tested fresh output")
				}
			}
		}
	}
	if len(expected) != 32 {
		t.Fatal("fresh producer nonce missing")
	}
	noRegistry := f.generateExactPlan("host-no-registry", ciCheckout, "stoke.json", "", "", nil, nil)
	noRegistryScope := f.exactScope("host-no-registry", ciCheckout, noRegistry)
	_, log = f.runExactJob("no-registry", ciCheckout, "host-no-registry.plan.json", noRegistry, noRegistry.job(t, "produce"), noRegistryScope, 1, nil, []string{"--allow-host"}, false)
	f.requireMarkerCount(log, "HOST_PRODUCER_EXECUTED", 0)
}

func TestHostCapturedFilesFeedCoopr(t *testing.T) {
	if testing.Short() {
		t.Skip("requires explicit development Stoke and Coopr executables")
	}
	for _, name := range []string{"STOKE_BINARY", "STOKE_COOPR_BINARY", "STOKE_HOST_EVIDENCE_DIR"} {
		if os.Getenv(name) == "" {
			t.Fatal(name + " must select the qualified local fixture")
		}
	}
	work, err := os.MkdirTemp(os.Getenv("STOKE_HOST_EVIDENCE_DIR"), "host-coopr-")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	f := &ciRegistryFixture{t: t, ctx: ctx, binary: os.Getenv("STOKE_BINARY"), engine: os.Getenv("STOKE_COOPR_BINARY"), work: work, reports: map[string]string{}}
	f.policy = filepath.Join(work, "policy.json")
	f.write(f.policy, []byte(`{"default":[{"type":"reject"}],"transports":{"oci":{"":[{"type":"insecureAcceptAnything"}]},"containers-storage":{"":[{"type":"insecureAcceptAnything"}]}}}`), 0600)
	caller := filepath.Join(work, "context")
	w := map[string]any{"version": 1, "tasks": map[string]any{
		"prepare": map[string]any{"platform": runtime.GOOS + "/" + runtime.GOARCH, "host": map[string]any{"tools": []string{"python3"}, "outputs": []string{"out"}}, "steps": []any{map[string]any{"run": []string{"python3", "-c", "from pathlib import Path; Path('out').mkdir(); Path('out/value').write_text('exact host capture')"}}}},
		"image":   map[string]any{"image": "scratch", "steps": []any{map[string]any{"copy": map[string]any{"from": "prepare", "sources": []string{"out/value"}, "destination": "/input"}}}},
	}}
	f.write(filepath.Join(caller, "stoke.json"), f.encode(w), 0600)
	report, _ := f.run("mixed", caller, filepath.Join(caller, "stoke.json"), []string{"--allow-host"}, true)
	result := f.task(report, "image")
	if string(f.read(filepath.Join(result.Filesystem, "input"))) != "exact host capture" || result.Kind != "image" {
		t.Fatal("host files did not reach ordinary Coopr COPY", result)
	}
	f.write(filepath.Join(work, "summary.json"), f.encode(map[string]any{"status": "PASS", "hostArtifact": f.task(report, "prepare").Digest, "nativeImage": result.Digest, "report": report.Directory}), 0600)
	t.Logf("Mixed host/Coopr evidence: %s", work)
}
