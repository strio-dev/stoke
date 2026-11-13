package artifact

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// A scoped 404 proxy models an exact result removed/unavailable after success.
// It forwards every other request to the existing test-owned registry fixture.
func exactResultFaultProxy(t *testing.T, f *ciRegistryFixture) func(string, bool) {
	t.Helper()
	upstream, err := url.Parse("http://" + f.registry)
	if err != nil {
		t.Fatal(err)
	}
	forward := httputil.NewSingleHostReverseProxy(upstream)
	var mu sync.Mutex
	blocked := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		// Blob bodies may be unavailable while their presence metadata remains readable.
		deny := blocked[r.URL.Path] && (r.Method == http.MethodGet || !strings.Contains(r.URL.Path, "/blobs/"))
		mu.Unlock()
		if deny {
			f.mu.Lock()
			f.trace = append(f.trace, map[string]string{"method": r.Method, "path": r.URL.Path, "fault": "404"})
			f.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	f.registry = strings.TrimPrefix(server.URL, "http://")
	return func(path string, deny bool) { mu.Lock(); blocked[path] = deny; mu.Unlock() }
}

func TestCIExactRetryLineageWithCoopr(t *testing.T) {
	f := newCIRegistryFixture(t)
	block := exactResultFaultProxy(t, f)
	caller := filepath.Join(f.work, "retry-checkout")
	f.write(filepath.Join(caller, "busybox"), f.read(os.Getenv("STOKE_SMOKE_BUSYBOX")), 0755)
	f.write(filepath.Join(caller, "payload"), []byte("identical-payload\n"), 0644)
	workflow := filepath.Join(caller, "stoke.json")
	f.write(workflow, f.encode(map[string]any{"version": 1, "tasks": map[string]any{
		"build":   map[string]any{"image": "scratch", "steps": []any{ciRegistryCopy("", "busybox", "/bin/busybox"), ciRegistryCopy("", "payload", "/value")}},
		"test":    map[string]any{"from": "build", "cache": false, "outputs": map[string]any{"filesystem": false}, "steps": []any{ciRegistryRun("/bin/busybox cp /value /result; echo RETRY_TEST_RAN")}},
		"package": map[string]any{"image": "scratch", "needs": []string{"test"}, "steps": []any{ciRegistryCopy("", "busybox", "/bin/busybox"), ciRegistryCopy("build", "/value", "/result"), ciRegistryRun("echo RETRY_PACKAGE_RAN")}},
	}}), 0600)
	plan := f.generateExactPlan("retry", caller, "stoke.json", f.registry+"/cache/retry", f.registry+"/results/exact", nil, []string{"package"})
	build, test, pack := plan.job(t, "build"), plan.job(t, "test"), plan.job(t, "package")
	scope := f.exactScope("retry", caller, plan)
	records := map[string]*exactNativeMetadata{}
	first, _ := f.runExactJob("retry-build-a1", caller, "retry.plan.json", plan, build, scope, 1, nil, nil, true)
	records[build.ID] = first.CI
	checked, _ := f.runExactJob("retry-test-a1", caller, "retry.plan.json", plan, test, scope, 1, plan.needs(t, test, records), nil, true)
	records[test.ID] = checked.CI
	if checked.CI.Inputs != first.CI.Inputs {
		t.Fatal("pure gate did not forward the exact producer input binding")
	}
	seed, seedLog := f.runExactJob("retry-package-a1", caller, "retry.plan.json", plan, pack, scope, 1, plan.needs(t, pack, records), nil, true)
	f.requireFile(seed.ciRegistryReport, "package", "result", "identical-payload\n")
	f.requireMarkerCount(seedLog, "RETRY_PACKAGE_RAN", 1)
	imageManifest := func(report ciRegistryReport, name string) v1.Manifest {
		image := f.task(report, name)
		source, err := newCILocalSource(f.ctx, os.DirFS(image.OCI), image.Digest)
		if err != nil {
			t.Fatal(err)
		}
		root, err := source.Resolve(f.ctx, image.Digest)
		if err != nil {
			t.Fatal(err)
		}
		data, err := fetch(f.ctx, source, root, MaxCIMetadataBytes)
		if err != nil {
			t.Fatal(err)
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil || root.MediaType != v1.MediaTypeImageManifest || len(manifest.Layers) == 0 {
			t.Fatalf("fixture image manifest: %+v %v", root, err)
		}
		return manifest
	}
	producer := imageManifest(first.ciRegistryReport, "build")
	packagedLayers := imageManifest(seed.ciRegistryReport, "package").Layers
	var sourceLayer v1.Descriptor
	for _, layer := range producer.Layers {
		if !slices.ContainsFunc(packagedLayers, func(other v1.Descriptor) bool { return other.Digest == layer.Digest }) {
			sourceLayer = layer
			break
		}
	}
	if sourceLayer.Digest == "" {
		t.Fatal("fixture has no producer-only layer to block")
	}
	sourcePath := "/v2/results/exact/blobs/" + sourceLayer.Digest.String()
	sourceReads := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		count := 0
		for _, request := range f.trace {
			if request["method"] == http.MethodGet && request["path"] == sourcePath {
				count++
			}
		}
		return count
	}
	before := sourceReads()
	block(sourcePath, true)
	retained, retainedLog := f.runExactJob("retry-package-only-a2", caller, "retry.plan.json", plan, pack, scope, 2, plan.needs(t, pack, records), nil, true)
	if retained.CI.Inputs != first.CI.Inputs || len(retained.Tasks) != 1 || retained.Tasks[0].Name != "package" {
		t.Fatal("package-only retry replaced prior bindings or replayed an upstream task")
	}
	f.requireFile(retained.ciRegistryReport, "package", "result", "identical-payload\n")
	f.requireMarkerCount(retainedLog, "RETRY_TEST_RAN", 0)
	f.requireMarkerCount(retainedLog, "RETRY_PACKAGE_RAN", 0)
	warm := f.task(retained.ciRegistryReport, "package")
	if warm.Cache == nil || warm.Cache.Hits < 1 || warm.Cache.Misses != 0 || warm.Cache.Errors != 0 || warm.Digest == "" || warm.Digest != f.task(seed.ciRegistryReport, "package").Digest || sourceReads() != before {
		t.Fatalf("warm package fetched an unused producer layer or missed terminal cache: %+v", warm.Cache)
	}
	_, blockedLog := f.runExactJob("retry-fresh-test-source-unavailable", caller, "retry.plan.json", plan, test, scope, 2, plan.needs(t, test, records), nil, false)
	if sourceReads() == before {
		t.Fatal("fresh test did not demand its blocked producer layer")
	}
	f.requireMarkerCount(blockedLog, "RETRY_TEST_RAN", 0)
	block(sourcePath, false)
	// A cached consumer does not bypass the original registry source policy.
	originalPolicy := f.read(f.policy)
	var policy struct {
		Default    []map[string]string                       `json:"default"`
		Transports map[string]map[string][]map[string]string `json:"transports"`
	}
	if err := json.Unmarshal(originalPolicy, &policy); err != nil {
		t.Fatal(err)
	}
	policy.Transports["docker"][f.registry+"/results/exact"] = []map[string]string{{"type": "reject"}}
	f.write(f.policy, f.encode(policy), 0600)
	_, policyLog := f.runExactJob("retry-source-policy-rejected", caller, "retry.plan.json", plan, pack, scope, 2, plan.needs(t, pack, records), nil, false)
	if !bytes.Contains(policyLog, []byte("rejected by policy")) || !bytes.Contains(policyLog, []byte(f.registry+"/results/exact@")) {
		t.Fatalf("registry policy failure was not reported: %s", policyLog)
	}
	f.requireMarkerCount(policyLog, "RETRY_PACKAGE_RAN", 0)
	f.write(f.policy, originalPolicy, 0600)
	_, tlsLog := f.runExactJob("retry-source-tls-required", caller, "retry.plan.json", plan, pack, scope, 2, plan.needs(t, pack, records), []string{"--tls-verify=true"}, false)
	if !bytes.Contains(tlsLog, []byte(f.registry+"/results/exact@")) || !bytes.Contains(tlsLog, []byte("HTTPS")) && !bytes.Contains(tlsLog, []byte("TLS")) {
		t.Fatalf("required registry TLS failure was not reported: %s", tlsLog)
	}
	f.requireMarkerCount(tlsLog, "RETRY_PACKAGE_RAN", 0)
	second, _ := f.runExactJob("retry-build-a2", caller, "retry.plan.json", plan, build, scope, 2, nil, nil, true)
	client := Client{PlainHTTP: true}
	one, err := client.ReadCIInputSet(f.ctx, plan.Config.ResultsRepository, first.CI.Inputs, scope)
	if err != nil {
		t.Fatal(err)
	}
	two, err := client.ReadCIInputSet(f.ctx, plan.Config.ResultsRepository, second.CI.Inputs, scope)
	if err != nil {
		t.Fatal(err)
	}
	result1, err := client.ReadCIResult(f.ctx, plan.Config.ResultsRepository, one.Bindings["build"], CIResultExpectation{Scope: scope, Producer: "build", MaxAttempt: 2})
	if err != nil {
		t.Fatal(err)
	}
	result2, err := client.ReadCIResult(f.ctx, plan.Config.ResultsRepository, two.Bindings["build"], CIResultExpectation{Scope: scope, Producer: "build", MaxAttempt: 2})
	if err != nil {
		t.Fatal(err)
	}
	if one.Bindings["build"] == two.Bindings["build"] || result1.Execution == result2.Execution || result1.Output.Digest != result2.Output.Digest {
		t.Fatal("retry fixture did not retain identical canonical bytes with distinct producer executions")
	}
	records[build.ID] = second.CI
	conflict, log := f.runExactJob("mixed-attempt-lineage", caller, "retry.plan.json", plan, pack, scope, 2, plan.needs(t, pack, records), nil, false)
	if conflict.Directory != "" || (!bytes.Contains(log, []byte("conflict")) && !bytes.Contains(log, []byte("inconsistent"))) {
		t.Fatal("old gate plus new equal-payload producer did not fail before execution")
	}
	f.requireMarkerCount(log, "RETRY_TEST_RAN", 0)
	f.requireMarkerCount(log, "RETRY_PACKAGE_RAN", 0)
	current, _ := f.runExactJob("retry-test-a2", caller, "retry.plan.json", plan, test, scope, 2, plan.needs(t, test, records), nil, true)
	for _, gate := range []exactCIReport{checked, current} {
		value := f.task(gate.ciRegistryReport, "test")
		if value.OCI != "" || value.Digest != "" || value.Filesystem != "" {
			t.Fatal("fresh pure gate exported an unrequested image or filesystem")
		}
	}
	if current.CI.Inputs != second.CI.Inputs {
		t.Fatal("fresh gate did not forward the replacement producer binding")
	}
	records[test.ID] = current.CI
	packaged, _ := f.runExactJob("retry-package-a2", caller, "retry.plan.json", plan, pack, scope, 2, plan.needs(t, pack, records), nil, true)
	f.requireFile(packaged.ciRegistryReport, "package", "result", "identical-payload\n")
	// Two reusable calls in one native run have different invocation nonces and
	// independently selected envelopes, even when their payload bytes are equal.
	other := f.exactScope("parallel-invocation", caller, plan)
	a := f.newExactInvocation("concurrent-scope-a", caller, "retry.plan.json", plan, build, scope, 3, nil, nil)
	b := f.newExactInvocation("concurrent-scope-b", caller, "retry.plan.json", plan, build, other, 1, nil, nil)
	if err := a.command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.command.Start(); err != nil {
		t.Fatal(err)
	}
	ra, _ := a.finish(a.command.Wait(), true)
	rb, _ := b.finish(b.command.Wait(), true)
	if ra.CI.Scope == rb.CI.Scope || ra.CI.Inputs == rb.CI.Inputs {
		t.Fatal("parallel reusable calls collided")
	}
	wrongRecords := map[string]*exactNativeMetadata{build.ID: ra.CI}
	_, wrongLog := f.runExactJob("cross-invocation-input", caller, "retry.plan.json", plan, test, other, 3, plan.needs(t, test, wrongRecords), nil, false)
	if !bytes.Contains(wrongLog, []byte("scope")) {
		t.Fatal("cross-invocation input accepted")
	}
	// Simulated metadata deletion returns404 only for the selected envelope. No
	// mutable-tag scan or ordinary producer/test execution may recover it.
	_, envelopeDigest, ok := strings.Cut(two.Bindings["build"], "@")
	if !ok {
		t.Fatal("unpinned envelope")
	}
	block("/v2/results/exact/manifests/"+envelopeDigest, true)
	unavailable, missingLog := f.runExactJob("selected-result-unavailable", caller, "retry.plan.json", plan, test, scope, 2, plan.needs(t, test, map[string]*exactNativeMetadata{build.ID: second.CI}), nil, false)
	if unavailable.Directory != "" {
		t.Fatal("unavailable exact result started runner state")
	}
	f.requireMarkerCount(missingLog, "RETRY_TEST_RAN", 0)
	f.requireMarkerCount(missingLog, "RETRY_PACKAGE_RAN", 0)
	block("/v2/results/exact/manifests/"+envelopeDigest, false)
	// Cross-job data without an explicit result repository fails clearly.
	noRepo := f.generateExactPlan("no-repository", caller, "stoke.json", "", "", nil, nil)
	if !noRepo.RequiresResults {
		t.Fatal("cross-job graph lost repository requirement")
	}
	noScope := f.exactScope("no-repository", caller, noRepo)
	absent, noRepoLog := f.runExactJob("missing-results-repository", caller, "no-repository.plan.json", noRepo, noRepo.job(t, "build"), noScope, 1, nil, nil, false)
	if absent.Directory != "" || !bytes.Contains(noRepoLog, []byte("results-repository")) {
		t.Fatal("missing explicit result repository did not fail before execution")
	}
	// Explicit co-location needs neither result transport nor registry access.
	colocated := f.generateExactPlan("co-located", caller, "stoke.json", "", "", []string{"--co-locate", "build"}, nil)
	if colocated.RequiresResults || len(colocated.Groups) != 1 || len(colocated.Groups[0].Jobs) != 1 {
		t.Fatal("explicit co-location did not eliminate cross-job data")
	}
	colocatedScope := f.exactScope("co-located", caller, colocated)
	f.mu.Lock()
	requestsBefore := len(f.trace)
	f.mu.Unlock()
	local, _ := f.runExactJob("co-located-no-registry", caller, "co-located.plan.json", colocated, colocated.Groups[0].Jobs[0], colocatedScope, 1, nil, nil, true)
	f.mu.Lock()
	requestsAfter := len(f.trace)
	f.mu.Unlock()
	if requestsBefore != requestsAfter || local.CI.Inputs != "" {
		t.Fatal("co-located execution accessed exact-result registry")
	}
	f.requireFile(local.ciRegistryReport, "package", "result", "identical-payload\n")
	f.checks = append(f.checks, "package-only-retry-retains-exact-prior-build-binding-without-gate-image-exports", "cached-package-does-not-fetch-unused-producer-layer-while-fresh-gate-demands-it", "equal-payload-producer-attempt2-conflicts-with-old-successful-test-attempt1", "explicit-prior-inputs-and-rerun-dependent-gates-restore-consistent-lineage", "concurrent-invocation-scopes-remain-isolated", "selected-result-404-fails-before-runner-without-producer-replay", "missing-repository-errors-and-explicit-co-location-runs-with-no-registry-access")
}

func TestCIExactNativeAndEnvironmentResultsWithCoopr(t *testing.T) {
	f := newCIRegistryFixture(t)
	caller := filepath.Join(f.work, "native-checkout")
	f.write(filepath.Join(caller, "busybox"), f.read(os.Getenv("STOKE_SMOKE_BUSYBOX")), 0755)
	f.write(filepath.Join(caller, "payload"), []byte("component-files\n"), 0644)
	f.write(filepath.Join(caller, "component.coopr"), []byte(`package as="payload" {
    copy "payload" "/component-value"
}
extend {
    copy "/component-value" "/component-value" from="payload"
}
`), 0644)
	f.write(filepath.Join(caller, "image.coopr"), []byte(`from "scratch" {
    copy "busybox" "/bin/busybox"
}
`), 0644)
	f.write(filepath.Join(caller, "consume.coopr"), []byte(`arg "BASE"
arg "COMPONENT"
from "${BASE}" {
    arg "COMPONENT"
    component "${COMPONENT}"
    run {
        exec "/bin/busybox" "sh" "-eu" "-c" "/bin/busybox cp /component-value /result; echo EXACT_COMPONENT_CONSUMER_RAN"
    }
}
`), 0644)
	workflow := filepath.Join(caller, "stoke.json")
	f.write(workflow, f.encode(map[string]any{"version": 1, "tasks": map[string]any{
		"image":     map[string]any{"coopr": map[string]any{"files": []string{"image.coopr"}}},
		"component": map[string]any{"coopr": map[string]any{"kind": "component", "files": []string{"component.coopr"}}},
		"consume":   map[string]any{"coopr": map[string]any{"files": []string{"consume.coopr"}, "args": map[string]any{"BASE": map[string]string{"from": "image"}, "COMPONENT": map[string]string{"from": "component"}}}},
	}}), 0600)
	plan := f.generateRegistryPlan("native", caller, f.registry+"/cache/native")
	scope := f.exactScope("native", caller, plan)
	records := map[string]*exactNativeMetadata{}
	for _, group := range plan.Groups {
		for _, job := range group.Jobs {
			report, log := f.runExactJob("native-"+job.ID, caller, "native.plan.json", plan, job, scope, 1, plan.needs(t, job, records), nil, true)
			records[job.ID] = report.CI
			if job.ID == "consume" {
				f.requireFile(report.ciRegistryReport, "consume", "result", "component-files\n")
				f.requireMarkerCount(log, "EXACT_COMPONENT_CONSUMER_RAN", 1)
			}
		}
	}
	client := Client{PlainHTTP: true}
	component := plan.job(t, "component")
	set, err := client.ReadCIInputSet(f.ctx, plan.Config.ResultsRepository, records[component.ID].Inputs, scope)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ReadCIResult(f.ctx, plan.Config.ResultsRepository, set.Bindings["component"], CIResultExpectation{Scope: scope, Producer: "component", Kind: "component", MaxAttempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Archive == nil {
		t.Fatal("native component archive/auxiliary graph was not retained")
	}
	// Shared environment producers become their own visible job; path aliases
	// lower to one particular producer result, not two independent random runs.
	f.write(filepath.Join(caller, "env.coopr"), []byte(`from "scratch" {
    copy "busybox" "/bin/busybox"
    run {
        exec "/bin/busybox" "sh" "-eu" "-c" "/bin/busybox od -An -N16 -tx1 /dev/urandom >/value; echo EXACT_ENV_RAN"
    }
}
`), 0644)
	f.write(workflow, f.encode(map[string]any{"version": 1, "tasks": map[string]any{
		"one": map[string]any{"cache": false, "environment": map[string]any{"files": []string{"env.coopr"}}, "steps": []any{ciRegistryRun("/bin/busybox cp /value /result; echo EXACT_ENV_ONE_RAN")}},
		"two": map[string]any{"cache": false, "environment": map[string]any{"files": []string{"./env.coopr"}}, "steps": []any{ciRegistryRun("/bin/busybox cp /value /result; echo EXACT_ENV_TWO_RAN")}},
	}}), 0600)
	shared := f.generateRegistryPlan("environment", caller, f.registry+"/cache/environment")
	if len(shared.Groups) != 3 || len(shared.Environments) != 1 || !shared.RequiresResults {
		t.Fatalf("shared environment was not a visible producer: %+v", shared)
	}
	sharedScope := f.exactScope("environment", caller, shared)
	sharedRecords := map[string]*exactNativeMetadata{}
	var producer generatedRegistryJob
	for name := range shared.Environments {
		producer = shared.job(t, name)
	}
	produced, producerLog := f.runExactJob("environment-producer", caller, "environment.plan.json", shared, producer, sharedScope, 1, nil, nil, true)
	sharedRecords[producer.ID] = produced.CI
	f.requireMarkerCount(producerLog, "EXACT_ENV_RAN", 1)
	var values []string
	for _, name := range []string{"one", "two"} {
		job := shared.job(t, name)
		report, log := f.runExactJob("environment-"+name, caller, "environment.plan.json", shared, job, sharedScope, 1, shared.needs(t, job, sharedRecords), nil, true)
		f.requireMarkerCount(log, "EXACT_ENV_RAN", 0)
		values = append(values, string(f.read(filepath.Join(f.task(report.ciRegistryReport, name).Filesystem, "result"))))
	}
	if strings.TrimSpace(values[0]) == "" || values[0] != values[1] {
		t.Fatal("shared promoted environment did not retain one particular random result")
	}
	// Qualify platform envelopes with COPY-only programs. No ARM executable runs.
	f.write(workflow, f.encode(map[string]any{"version": 1, "tasks": map[string]any{
		"amd":      map[string]any{"image": "scratch", "platform": "linux/amd64", "steps": []any{ciRegistryCopy("", "payload", "/value")}},
		"arm":      map[string]any{"image": "scratch", "platform": "linux/arm64", "steps": []any{ciRegistryCopy("", "payload", "/value")}},
		"amd-copy": map[string]any{"image": "scratch", "platform": "linux/amd64", "steps": []any{ciRegistryCopy("amd", "/value", "/result")}},
		"arm-copy": map[string]any{"image": "scratch", "platform": "linux/arm64", "steps": []any{ciRegistryCopy("arm", "/value", "/result")}},
	}}), 0600)
	platforms := f.generateRegistryPlan("platforms", caller, f.registry+"/cache/platforms")
	platformScope := f.exactScope("platforms", caller, platforms)
	platformRecords := map[string]*exactNativeMetadata{}
	for _, group := range platforms.Groups {
		for _, job := range group.Jobs {
			report, _ := f.runExactJob("platform-"+job.ID, caller, "platforms.plan.json", platforms, job, platformScope, 1, platforms.needs(t, job, platformRecords), nil, true)
			platformRecords[job.ID] = report.CI
			if strings.HasSuffix(job.ID, "-copy") {
				f.requireFile(report.ciRegistryReport, job.ID, "result", "component-files\n")
			}
		}
	}
	f.checks = append(f.checks, "native-image-and-component-exact-import-preserves-original-component-archive-graph", "shared-inline-environment-is-one-visible-producer-with-exact-particular-consumers", "amd64-and-arm64-exact-result-platforms-qualified-with-COPY-only-no-ARM-execution")
}
