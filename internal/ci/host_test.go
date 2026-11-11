package ci

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

const hostGraphFixture = `version: 1
tasks:
  produce:
    platform: linux/amd64
    cache: true
    host:
      tools: [sh]
      capabilities: [provisioned-toolchain]
      inputs: [{path: src}]
      outputs: [out]
      toolchain: [/usr/bin/sh]
      effects: captured
    steps: [{run: [sh, -c, 'mkdir out']}]
  tests:
    platform: linux/amd64
    host:
      tools: [sh]
      inputs: [{from: produce, path: out}]
    strategy: {matrix: {mode: [debug, release]}, max-parallel: 1}
    steps: [{run: [sh, -c, 'test -d out']}]
  package:
    platform: linux/amd64
    needs: [tests]
    host:
      tools: [sh]
      inputs: [{from: produce, path: out}]
    steps: [{run: [sh, -c, 'test -d out']}]
`

func TestHostCIPlanKeepsVisibleStagesAndExactFileEdges(t *testing.T) {
	w := decodeWorkflow(t, hostGraphFixture)
	p := buildPlanFixture(t, w, Config{CacheFrom: []string{"registry.test/cache"}, CacheTo: []string{"registry.test/cache"}})
	if len(p.Groups) != 3 || len(p.Host) != 4 || !p.RequiresResults {
		t.Fatalf("host stages hidden or file edges lost: %#v", p)
	}
	if len(p.Engine.Capabilities) != 0 || p.Engine.ProductionPin != nil || !strings.Contains(p.Engine.PinStatus, "not required") {
		t.Fatalf("host-only graph acquired Coopr requirements: %#v", p.Engine)
	}
	producer, tests, bundle := groupNamed(t, p, "produce"), groupNamed(t, p, "tests"), groupNamed(t, p, "package")
	if !slices.Equal(producer.Jobs[0].Exports, []string{"produce"}) || !tests.Matrix || len(tests.Jobs) != 2 || tests.MaxParallel != 1 {
		t.Fatal("host producer export or native matrix lost")
	}
	for _, selection := range tests.Jobs {
		if !slices.Equal(selection.Imports, []string{"produce"}) || p.Host[selection.ID].Cache {
			t.Fatal("fresh host tests lost exact input or acquired cache reuse")
		}
	}
	if !slices.Contains(bundle.Needs, tests.ID) || !slices.Contains(bundle.Needs, producer.ID) || !slices.Equal(bundle.Jobs[0].Imports, []string{"produce"}) {
		t.Fatal("package no longer gates on all tests and exact tested producer output")
	}
	requirement := p.Host["produce"]
	if requirement.Platform != "linux/amd64" || !requirement.Cache || requirement.Effects != "captured" || !slices.Equal(requirement.Tools, []string{"sh"}) || !slices.Equal(requirement.Capabilities, []string{"provisioned-toolchain"}) {
		t.Fatalf("host provisioning/cache metadata missing: %#v", requirement)
	}
	data, err := Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(bytes.NewReader(data))
	if err != nil || decoded.ID != p.ID || len(decoded.Host) != 4 {
		t.Fatalf("host plan roundtrip: %v", err)
	}
	if bytes.Contains(data, []byte(`"allowHost"`)) {
		t.Fatal("plan contains an execution authority grant")
	}
}

func TestMixedHostCIPlanRetainsOnlyRequiredCooprCapabilities(t *testing.T) {
	w := decodeWorkflow(t, `version: 1
tasks:
  produce:
    platform: linux/amd64
    host: {tools: [sh], outputs: [out]}
    steps: [{run: [sh, -c, 'mkdir out']}]
  image:
    image: scratch
    steps: [{copy: {from: produce, sources: [out], destination: /out}}]
`)
	p := buildPlanFixture(t, w, Config{})
	if !slices.Equal(p.Engine.Capabilities, []string{"execute"}) || len(p.Host) != 1 || !p.RequiresResults {
		t.Fatalf("mixed plan capabilities: %#v", p.Engine)
	}
}

func TestGitHubHostOptInIsCallerControlledAndKeepsNativeGates(t *testing.T) {
	p := buildPlanFixture(t, decodeWorkflow(t, hostGraphFixture), Config{})
	data, rendered := renderFixture(t, p, ".stoke/ci-plan.json")
	call := rendered.On["workflow_call"].(map[string]any)
	inputs := call["inputs"].(map[string]any)
	if inputs["allow_host"].(map[string]any)["default"] != false || inputs["allow_host"].(map[string]any)["type"] != "boolean" || inputs["host_capabilities"].(map[string]any)["default"] != "[]" {
		t.Fatal("generated host authority is not default-denied caller input")
	}
	jobs := renderedJobs(t, rendered)
	if len(jobs) != len(p.Groups)+2 {
		t.Fatal("host tasks changed visible stage count")
	}
	for _, group := range p.Groups {
		job := jobs[group.ID]
		if !strings.Contains(job.Name, "(host)") || !slices.Equal(job.Needs, append([]string{"stoke_context"}, group.Needs...)) {
			t.Fatal("host execution boundary or native gates hidden")
		}
		if job.Env["STOKE_ALLOW_HOST"] != "${{ inputs.allow_host }}" || job.Env["STOKE_HOST_CAPABILITIES"] != "${{ inputs.host_capabilities }}" {
			t.Fatal("host authority/capabilities not supplied by caller")
		}
		run := job.Steps[1].Run
		for _, expected := range []string{"host_args=()", `if [[ "$STOKE_ALLOW_HOST" == true ]]`, "host_args+=(--allow-host)", `"${host_args[@]}"`, `--host-capabilities-json "$STOKE_HOST_CAPABILITIES"`} {
			if !strings.Contains(run, expected) {
				t.Fatalf("safe host argument construction missing %q", expected)
			}
		}
		if strings.Contains(run, "provisioned-toolchain") || strings.Contains(run, "eval ") || strings.Contains(run, "${{ inputs.") {
			t.Fatal("requirement self-attestation or expression/shell injection in generated host command")
		}
		if group.Matrix && (job.Strategy == nil || len(job.Strategy.Matrix["include"]) != 2) {
			t.Fatal("host variants collapsed out of native matrix")
		}
	}
	if bytes.Contains(data, []byte("upload-artifact")) || bytes.Contains(data, []byte("download-artifact")) {
		t.Fatal("host result exchange acquired mandatory CI artifacts")
	}
}
