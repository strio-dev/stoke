package artifact

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
		deny := blocked[r.URL.Path]
		mu.Unlock()
		if deny {
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
		"test":    map[string]any{"from": "build", "cache": false, "steps": []any{ciRegistryRun("/bin/busybox cp /value /result; echo RETRY_TEST_RAN")}},
		"package": map[string]any{"from": "build", "needs": []string{"test"}, "steps": []any{ciRegistryRun("/bin/busybox cp /value /result; echo RETRY_PACKAGE_RAN")}},
	}}), 0600)
	plan := f.generateRegistryPlan("retry", caller, f.registry+"/cache/retry")
	build, test, pack := plan.job(t, "build"), plan.job(t, "test"), plan.job(t, "package")
	scope := f.exactScope("retry", caller, plan)
	records := map[string]*exactNativeMetadata{}
	first, _ := f.runExactJob("retry-build-a1", caller, "retry.plan.json", plan, build, scope, 1, nil, nil, true)
	records[build.ID] = first.CI
	checked, _ := f.runExactJob("retry-test-a1", caller, "retry.plan.json", plan, test, scope, 1, plan.needs(t, test, records), nil, true)
	records[test.ID] = checked.CI
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
	f.checks = append(f.checks, "equal-payload-producer-attempt2-conflicts-with-old-successful-test-attempt1", "explicit-prior-inputs-and-rerun-dependent-gates-restore-consistent-lineage", "concurrent-invocation-scopes-remain-isolated", "selected-result-404-fails-before-runner-without-producer-replay", "missing-repository-errors-and-explicit-co-location-runs-with-no-registry-access")
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
