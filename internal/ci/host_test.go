package ci

import (
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

func TestHostCIPlanKeepsFileEdgesInOneInvocation(t *testing.T) {
	w := decodeWorkflow(t, hostGraphFixture)
	if _, err := Build(w, Config{}); err == nil || !strings.Contains(err.Error(), "--co-locate") {
		t.Fatalf("external host file prerequisite accepted: %v", err)
	}
	p := buildPlanFixture(t, w, Config{CoLocate: []string{"produce"}, CacheFrom: []string{"registry.test/cache"}})
	if len(p.Groups) != 1 || len(p.Groups[0].Jobs[0].Execute) != 4 || len(p.Host) != 4 || len(p.Engine.Capabilities) != 0 {
		t.Fatalf("host-only data unit changed: %#v", p)
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
	p := buildPlanFixture(t, w, Config{CoLocate: []string{"produce"}})
	if !slices.Equal(p.Engine.Capabilities, []string{"execute", "named-contexts"}) || len(p.Host) != 1 {
		t.Fatalf("mixed plan capabilities: %#v", p.Engine)
	}
}
