package artifact

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"go.yaml.in/yaml/v3"
)

type generatedRegistryJob struct {
	ID, Task, OutputKey       string
	Execute, Imports, Exports []string
	Matrix                    map[string]string
}
type generatedRegistryGroup struct {
	ID, Name       string
	Members, Needs []string
	Matrix         bool
	Jobs           []generatedRegistryJob
}
type generatedRegistryPlan struct {
	Version         int
	ID              string
	RequiresResults bool
	Environments    map[string]string
	Config          struct {
		CacheFrom, CacheTo []string
		ResultsRepository  string
	}
	Groups []generatedRegistryGroup
}
type exactNativeMetadata struct {
	Version int    `json:"version"`
	Scope   string `json:"scope"`
	Unit    string `json:"unit"`
	Attempt int    `json:"attempt"`
	Inputs  string `json:"inputs,omitempty"`
}
type exactCIReport struct {
	ciRegistryReport
	CI *exactNativeMetadata `json:"ci"`
}
type exactInvocation struct {
	f              *ciRegistryFixture
	label          string
	command        *exec.Cmd
	stdout, stderr bytes.Buffer
	job            generatedRegistryJob
	scope          CIScope
	attempt        int
	output         string
}

// Native success gates admit jobs. The selected immutable InputSets carry data
// identities only, including when a gate forwards them before execution.
func TestCIGeneratedRegistryExecutionWithCoopr(t *testing.T) {
	f := newCIRegistryFixture(t)
	caller := filepath.Join(f.work, "visible-checkout")
	f.write(filepath.Join(caller, "busybox"), f.read(os.Getenv("STOKE_SMOKE_BUSYBOX")), 0755)
	workflow := filepath.Join(caller, "stoke.json")
	destination := f.registry + "/published/exact:tested"
	f.write(workflow, f.encode(map[string]any{"version": 1, "tasks": map[string]any{
		"build":   map[string]any{"image": "scratch", "cache": false, "steps": []any{ciRegistryCopy("", "busybox", "/bin/busybox"), ciRegistryRun("/bin/busybox od -An -N16 -tx1 /dev/urandom >/value; echo EXACT_BUILD_RAN")}},
		"gate":    map[string]any{"image": "scratch", "needs": []string{"build"}, "cache": false, "steps": []any{ciRegistryCopy("", "busybox", "/bin/busybox"), ciRegistryRun("echo EXACT_GATE_RAN")}},
		"tests":   map[string]any{"from": "build", "needs": []string{"gate"}, "cache": false, "strategy": map[string]any{"matrix": map[string]any{"mode": []string{"a", "b"}}}, "steps": []any{ciRegistryRun("/bin/busybox cp /value /result; echo EXACT_TEST_RAN")}},
		"publish": map[string]any{"needs": []string{"tests", "gate"}, "publish": map[string]any{"from": "build", "destinations": []string{destination}}},
	}}), 0600)
	plan := f.generateRegistryPlan("visible", caller, f.registry+"/cache/visible")
	if plan.Version != 2 || !plan.RequiresResults || len(plan.Groups) != 4 {
		t.Fatalf("authored jobs were not visible: %+v", plan)
	}
	build, gate, publish := plan.job(t, "build"), plan.job(t, "gate"), plan.job(t, "publish")
	tests := plan.group(t, "tests")
	if !tests.Matrix || len(tests.Jobs) != 2 || tests.Jobs[0].OutputKey == tests.Jobs[1].OutputKey {
		t.Fatal("matrix lost unique native output slots")
	}
	scope := f.exactScope("visible", caller, plan)
	records := map[string]*exactNativeMetadata{}
	produced, buildLog := f.runExactJob("build-attempt1", caller, "visible.plan.json", plan, build, scope, 1, nil, nil, true)
	records[build.ID] = produced.CI
	f.requireMarkerCount(buildLog, "EXACT_BUILD_RAN", 1)
	value := string(f.read(filepath.Join(f.task(produced.ciRegistryReport, "build").Filesystem, "value")))
	if strings.TrimSpace(value) == "" {
		t.Fatal("random producer output empty")
	}
	gated, gateLog := f.runExactJob("gate-attempt1", caller, "visible.plan.json", plan, gate, scope, 1, plan.needs(t, gate, records), nil, true)
	records[gate.ID] = gated.CI
	f.requireMarkerCount(gateLog, "EXACT_GATE_RAN", 1)
	f.requireMarkerCount(gateLog, "EXACT_BUILD_RAN", 0)
	// Start matrix variants concurrently with separate stores and native output files.
	pending := make([]*exactInvocation, 0, 2)
	for _, job := range tests.Jobs {
		invocation := f.newExactInvocation("matrix-"+job.ID, caller, "visible.plan.json", plan, job, scope, 1, plan.needs(t, job, records), nil)
		if err := invocation.command.Start(); err != nil {
			t.Fatal(err)
		}
		pending = append(pending, invocation)
	}
	for _, invocation := range pending {
		report, log := invocation.finish(invocation.command.Wait(), true)
		records[invocation.job.ID] = report.CI
		f.requireFile(report.ciRegistryReport, invocation.job.ID, "result", value)
		f.requireMarkerCount(log, "EXACT_TEST_RAN", 1)
		f.requireMarkerCount(log, "EXACT_BUILD_RAN", 0)
		f.requireMarkerCount(log, "EXACT_GATE_RAN", 0)
		if task := f.task(report.ciRegistryReport, invocation.job.ID); task.Cache != nil && task.Cache.Hits != 0 {
			t.Fatal("fresh test reused result")
		}
	}
	published, publishLog := f.runExactJob("publish-attempt1", caller, "visible.plan.json", plan, publish, scope, 1, plan.needs(t, publish, records), nil, true)
	f.requireMarkerCount(publishLog, "EXACT_BUILD_RAN", 0)
	f.requireMarkerCount(publishLog, "EXACT_GATE_RAN", 0)
	f.requireMarkerCount(publishLog, "EXACT_TEST_RAN", 0)
	client := Client{PlainHTTP: true}
	inputs, err := client.ReadCIInputSet(f.ctx, plan.Config.ResultsRepository, produced.CI.Inputs, scope)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ReadCIResult(f.ctx, plan.Config.ResultsRepository, inputs.Bindings["build"], CIResultExpectation{Scope: scope, Producer: "build", MaxAttempt: 1, Kind: "image"})
	if err != nil {
		t.Fatal(err)
	}
	if task := f.task(published.ciRegistryReport, "publish"); task.Status != "passed" {
		t.Fatal("canonical publication failed")
	}
	response, err := http.Get("http://" + f.registry + "/v2/published/exact/manifests/tested")
	if err != nil {
		t.Fatal(err)
	}
	manifest, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK || digest.FromBytes(manifest) != result.Output.Digest {
		t.Fatalf("publish changed canonical tested manifest: %v %v %s", readErr, closeErr, response.Status)
	}
	// A consumer-only retry can use explicitly supplied prior-attempt bindings,
	// even when every local native/cache directory is empty and cache reads miss.
	empty := f.registry + "/cache/evicted"
	retried, retryLog := f.runExactJob("consumer-retry", caller, "visible.plan.json", plan, tests.Jobs[0], scope, 2, plan.needs(t, tests.Jobs[0], records), []string{"--cache-from", empty, "--cache-to", empty}, true)
	f.requireFile(retried.ciRegistryReport, tests.Jobs[0].ID, "result", value)
	f.requireMarkerCount(retryLog, "EXACT_BUILD_RAN", 0)
	f.requireMarkerCount(retryLog, "EXACT_GATE_RAN", 0)
	// Missing matrix metadata is not inferred from registry tags or prior attempts.
	incomplete := plan.needs(t, publish, records)
	delete(incomplete[tests.ID].Outputs, tests.Jobs[1].OutputKey)
	failed, missingLog := f.runExactJob("missing-matrix-output", caller, "visible.plan.json", plan, publish, scope, 2, incomplete, nil, false)
	if failed.Directory != "" || !bytes.Contains(missingLog, []byte("metadata")) {
		t.Fatal("missing matrix metadata did not fail before execution")
	}
	// Another reusable invocation in the same native run cannot consume this set.
	otherScope := f.exactScope("other-invocation", caller, plan)
	_, scopeLog := f.runExactJob("wrong-invocation", caller, "visible.plan.json", plan, publish, otherScope, 1, plan.needs(t, publish, records), nil, false)
	if !bytes.Contains(scopeLog, []byte("scope")) {
		t.Fatal("cross-invocation metadata was not rejected")
	}
	_, futureLog := f.runExactJob("future-attempt", caller, "visible.plan.json", plan, tests.Jobs[0], scope, 1, replaceMetadataAttempt(plan.needs(t, tests.Jobs[0], records), build.OutputKey, 2), nil, false)
	if !bytes.Contains(futureLog, []byte("attempt")) {
		t.Fatal("future producer metadata accepted")
	}
	f.checks = append(f.checks, "visible-build-gate-matrix-publish-consume-one-particular-random-result", "concurrent-matrix-variants-have-unique-native-outputs-and-independent-stores", "consumer-only-prior-attempt-retry-survives-empty-cache-without-producer-replay", "omitted-matrix-output-future-attempt-and-other-invocation-fail-before-execution")
	// A failed gate can already have emitted a binding-only InputSet. Native
	// failure still blocks consumers; presence of that object is not success.
	var failedWorkflow map[string]any
	if err := json.Unmarshal(f.read(workflow), &failedWorkflow); err != nil {
		t.Fatal(err)
	}
	failedWorkflow["tasks"].(map[string]any)["gate"].(map[string]any)["steps"] = []any{ciRegistryCopy("", "busybox", "/bin/busybox"), ciRegistryRun("echo EXACT_GATE_FAILED; exit 7")}
	f.write(workflow, f.encode(failedWorkflow), 0600)
	failedPlan := f.generateRegistryPlan("failed-gate", caller, f.registry+"/cache/failed-gate")
	failedScope := f.exactScope("failed-gate", caller, failedPlan)
	failedRecords := map[string]*exactNativeMetadata{}
	failedBuild := failedPlan.job(t, "build")
	failedGate := failedPlan.job(t, "gate")
	failedTests := failedPlan.group(t, "tests")
	failedProducer, _ := f.runExactJob("failed-gate-producer", caller, "failed-gate.plan.json", failedPlan, failedBuild, failedScope, 1, nil, nil, true)
	failedRecords[failedBuild.ID] = failedProducer.CI
	gateFailure, _ := f.runExactJob("failed-gate-run", caller, "failed-gate.plan.json", failedPlan, failedGate, failedScope, 1, failedPlan.needs(t, failedGate, failedRecords), nil, false)
	if gateFailure.Status != "failed" || gateFailure.CI == nil || gateFailure.CI.Inputs != failedProducer.CI.Inputs {
		t.Fatal("failed gate did not retain its pre-execution binding-only metadata")
	}
	failedRecords[failedGate.ID] = gateFailure.CI
	failedNeeds := failedPlan.needs(t, failedTests.Jobs[0], failedRecords)
	gateGroup := failedPlan.group(t, "gate")
	entry := failedNeeds[gateGroup.ID]
	entry.Result = "failure"
	failedNeeds[gateGroup.ID] = entry
	rejected, rejectLog := f.runExactJob("failed-gate-not-admitted", caller, "failed-gate.plan.json", failedPlan, failedTests.Jobs[0], failedScope, 1, failedNeeds, nil, false)
	if rejected.Directory != "" || !bytes.Contains(rejectLog, []byte("not successful")) {
		t.Fatal("binding metadata bypassed native failure gate")
	}
	f.requireMarkerCount(rejectLog, "EXACT_TEST_RAN", 0)
	f.checks = append(f.checks, "binding-only-metadata-can-exist-before-failed-gate-native-status-still-blocks-consumers")
}

type exactNeedsEntry struct {
	Result  string            `json:"result"`
	Outputs map[string]string `json:"outputs"`
}
type exactNeeds map[string]exactNeedsEntry

func replaceMetadataAttempt(needs exactNeeds, key string, attempt int) exactNeeds {
	for group, entry := range needs {
		if value := entry.Outputs[key]; value != "" {
			var metadata exactNativeMetadata
			_ = json.Unmarshal([]byte(value), &metadata)
			metadata.Attempt = attempt
			encoded, _ := json.Marshal(metadata)
			entry.Outputs[key] = string(encoded)
			needs[group] = entry
		}
	}
	return needs
}
func (p generatedRegistryPlan) group(t *testing.T, name string) generatedRegistryGroup {
	t.Helper()
	for _, g := range p.Groups {
		if g.Name == name || slices.Contains(g.Members, name) {
			return g
		}
	}
	t.Fatalf("missing group %s", name)
	return generatedRegistryGroup{}
}
func (p generatedRegistryPlan) job(t *testing.T, name string) generatedRegistryJob {
	t.Helper()
	for _, g := range p.Groups {
		for _, j := range g.Jobs {
			if j.ID == name || slices.Contains(j.Execute, name) {
				return j
			}
		}
	}
	t.Fatalf("missing job %s", name)
	return generatedRegistryJob{}
}
func (p generatedRegistryPlan) needs(t *testing.T, job generatedRegistryJob, records map[string]*exactNativeMetadata) exactNeeds {
	t.Helper()
	var group generatedRegistryGroup
	found := false
	for _, g := range p.Groups {
		for _, j := range g.Jobs {
			if j.ID == job.ID {
				group = g
				found = true
			}
		}
	}
	if !found {
		t.Fatal("job not in plan")
	}
	needs := exactNeeds{}
	for _, id := range group.Needs {
		for _, parent := range p.Groups {
			if parent.ID != id {
				continue
			}
			entry := exactNeedsEntry{Result: "success", Outputs: map[string]string{}}
			for _, slot := range parent.Jobs {
				metadata := records[slot.ID]
				if metadata == nil {
					t.Fatalf("provider prerequisite %s has no supplied native output", slot.ID)
				}
				data, err := json.Marshal(metadata)
				if err != nil {
					t.Fatal(err)
				}
				entry.Outputs[slot.OutputKey] = string(data)
			}
			needs[id] = entry
		}
	}
	return needs
}
func (f *ciRegistryFixture) exactScope(label, caller string, p generatedRegistryPlan) CIScope {
	f.t.Helper()
	command := f.generatedCommand(label+"-scope", caller, []string{"ci", "scope", "--plan-id", p.ID})
	command.Env = append(command.Env, "GITHUB_SERVER_URL=https://github.example.test", "GITHUB_REPOSITORY=strio/stoke", "GITHUB_RUN_ID=42", "GITHUB_SHA="+strings.Repeat("a", 40))
	output, err := command.Output()
	if err != nil {
		f.t.Fatal(err)
	}
	var scope CIScope
	if err := json.Unmarshal(output, &scope); err != nil {
		f.t.Fatal(err)
	}
	if err := scope.Validate(); err != nil {
		f.t.Fatal(err)
	}
	f.write(filepath.Join(f.work, label+".scope.json"), output, 0600)
	return scope
}
func (f *ciRegistryFixture) newExactInvocation(label, caller, path string, p generatedRegistryPlan, job generatedRegistryJob, scope CIScope, attempt int, needs exactNeeds, flags []string) *exactInvocation {
	f.t.Helper()
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		f.t.Fatal(err)
	}
	if needs == nil {
		needs = exactNeeds{}
	}
	needsJSON, err := json.Marshal(needs)
	if err != nil {
		f.t.Fatal(err)
	}
	output := filepath.Join(f.work, label+".github-output")
	f.write(output, nil, 0600)
	args := []string{"ci", "run", "--plan", path, "--plan-id", p.ID, "--job", job.ID, "--scope", string(scopeJSON), "--attempt", strconv.Itoa(attempt), "--needs", string(needsJSON), "--coopr-binary", f.engine, "--cache-dir", filepath.Join(f.work, label, "cache"), "--tls-verify=false", "--signature-policy", f.policy, "--plain-http", "--github-output", output}
	invocation := &exactInvocation{f: f, label: label, command: f.generatedCommand(label, caller, append(args, flags...)), job: job, scope: scope, attempt: attempt, output: output}
	invocation.command.Stdout = &invocation.stdout
	invocation.command.Stderr = &invocation.stderr
	return invocation
}
func (f *ciRegistryFixture) runExactJob(label, caller, path string, p generatedRegistryPlan, job generatedRegistryJob, scope CIScope, attempt int, needs exactNeeds, flags []string, success bool) (exactCIReport, []byte) {
	f.t.Helper()
	i := f.newExactInvocation(label, caller, path, p, job, scope, attempt, needs, flags)
	return i.finish(i.command.Run(), success)
}
func (i *exactInvocation) finish(err error, success bool) (exactCIReport, []byte) {
	f := i.f
	f.t.Helper()
	f.write(filepath.Join(f.work, i.label+".stdout"), i.stdout.Bytes(), 0600)
	f.write(filepath.Join(f.work, i.label+".stderr"), i.stderr.Bytes(), 0600)
	if (err == nil) != success {
		f.t.Fatalf("%s success=%t: %v\n%s", i.label, success, err, &i.stderr)
	}
	var report exactCIReport
	if len(bytes.TrimSpace(i.stdout.Bytes())) != 0 {
		if decodeErr := json.Unmarshal(i.stdout.Bytes(), &report); decodeErr != nil {
			f.t.Fatalf("%s report: %v\n%s", i.label, decodeErr, &i.stderr)
		}
	}
	if success {
		expectedScope, hashErr := i.scope.Hash()
		if hashErr != nil {
			f.t.Fatal(hashErr)
		}
		if report.Status != "passed" || report.CI == nil || report.CI.Version != 1 || report.CI.Scope != expectedScope || report.CI.Unit != i.job.ID || report.CI.Attempt != i.attempt {
			f.t.Fatalf("%s invalid native metadata: %+v", i.label, report)
		}
		nativeOutput := string(f.read(i.output))
		encoded, marshalErr := json.Marshal(report.CI)
		if marshalErr != nil {
			f.t.Fatal(marshalErr)
		}
		if nativeOutput != i.job.OutputKey+"="+string(encoded)+"\n" {
			f.t.Fatalf("%s native output differs from report", i.label)
		}
	}
	if report.Directory != "" {
		f.mu.Lock()
		f.reports[i.label] = report.Directory
		f.mu.Unlock()
	}
	return report, bytes.Clone(i.stderr.Bytes())
}
func (f *ciRegistryFixture) generateRegistryPlan(label, caller, repository string) generatedRegistryPlan {
	return f.generateExactPlan(label, caller, "stoke.json", repository, f.registry+"/results/exact", nil, nil)
}
func (f *ciRegistryFixture) generateExactPlan(label, caller, workflow, cache, results string, extra, targets []string) generatedRegistryPlan {
	f.t.Helper()
	common := []string{"-f", workflow, "--context", "."}
	if cache != "" {
		common = append(common, "--cache-from", cache, "--cache-to", cache)
	}
	if results != "" {
		common = append(common, "--results-repository", results)
	}
	common = append(common, extra...)
	generate := append([]string{"ci", "generate"}, common...)
	generate = append(generate, "--plan-output", label+".plan.json", "--output", label+".generated.yml")
	if output, err := f.generatedCommand(label+"-generate", caller, append(slices.Clone(generate), targets...)).CombinedOutput(); err != nil {
		f.t.Fatalf("generate: %v\n%s", err, output)
	}
	check := append(slices.Clone(generate), "--check")
	if output, err := f.generatedCommand(label+"-regenerate", caller, append(check, targets...)).CombinedOutput(); err != nil {
		f.t.Fatalf("regeneration: %v\n%s", err, output)
	}
	var plan generatedRegistryPlan
	if err := json.Unmarshal(f.read(filepath.Join(caller, label+".plan.json")), &plan); err != nil {
		f.t.Fatal(err)
	}
	inspect := append([]string{"ci", "plan"}, common...)
	output, err := f.generatedCommand(label+"-plan", caller, append(inspect, targets...)).Output()
	if err != nil {
		f.t.Fatal(err)
	}
	var inspected generatedRegistryPlan
	if err := json.Unmarshal(output, &inspected); err != nil || inspected.ID != plan.ID {
		f.t.Fatal("plan/generate fingerprint mismatch")
	}
	generated := f.read(filepath.Join(caller, label+".generated.yml"))
	for _, forbidden := range []string{"upload-artifact", "download-artifact", "--receipt", "--handoff"} {
		if bytes.Contains(generated, []byte(forbidden)) {
			f.t.Fatalf("generated CI artifact transport: %s", forbidden)
		}
	}
	var provider struct {
		Jobs map[string]struct {
			Needs   []string          `yaml:"needs"`
			Outputs map[string]string `yaml:"outputs"`
		}
	}
	if err := yaml.Unmarshal(generated, &provider); err != nil {
		f.t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, group := range plan.Groups {
		rendered := provider.Jobs[group.ID]
		if !slices.Equal(append([]string{"stoke_context"}, group.Needs...), rendered.Needs) {
			f.t.Fatal("native dependencies differ from plan")
		}
		for _, job := range group.Jobs {
			if job.OutputKey == "" || keys[job.OutputKey] || rendered.Outputs[job.OutputKey] == "" {
				f.t.Fatal("missing/duplicate native output slot")
			}
			keys[job.OutputKey] = true
		}
	}
	return plan
}
func (f *ciRegistryFixture) generatedCommand(label, caller string, args []string) *exec.Cmd {
	command := f.commandFor(label, caller, args)
	command.Dir = caller
	return command
}
func (f *ciRegistryFixture) requireMarkerCount(log []byte, marker string, expected int) {
	f.t.Helper()
	count := 0
	for _, line := range strings.Split(string(log), "\n") {
		if strings.TrimSpace(line) == marker {
			count++
		}
	}
	if count != expected {
		f.t.Fatalf("%s count=%d want%d; evidence%s", marker, count, expected, f.work)
	}
}
