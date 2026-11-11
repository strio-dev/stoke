package source

import (
	"context"
	"strings"
	"testing"

	"github.com/strio-dev/stoke/internal/artifact"
)

func TestImportedHostTasksKeepIndependentCapturedFileScopes(t *testing.T) {
	component := pin("host-tools")
	document := `version: 1
tasks:
  produce:
    platform: linux/amd64
    host: {tools: [sh], outputs: [out]}
    steps: [{run: [sh, -c, 'mkdir out']}]
  check:
    platform: linux/amd64
    host:
      tools: [sh]
      inputs: [{from: produce, path: out}]
      outputs: [report]
    steps: [{run: [sh, -c, 'test -d out'], stdout: report}]
`
	path := local(t, "version: 1\nimports: {tools: "+component+"}\ntasks:\n  first: {uses: tools/check, workdir: out}\n  second: {uses: tools/check}\n")
	result, err := loader(map[string]*artifact.Document{component: {Data: []byte(document)}}).Load(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = result.Close() }()
	first, second := result.Workflow.Tasks["first"], result.Workflow.Tasks["second"]
	if first.Host == nil || second.Host == nil || first.Host.Inputs[0].From != "first--tools--produce" || second.Host.Inputs[0].From != "second--tools--produce" {
		t.Fatalf("host invocation scopes: %#v %#v", first, second)
	}
	if first.CacheEnabled() || second.CacheEnabled() || first.Workdir != "out" {
		t.Fatal("host template defaults or relative workdir lost")
	}
	if _, err := result.Workflow.Expand(result.Targets); err != nil {
		t.Fatal(err)
	}
	first.Host.Tools[0] = "other"
	first.Host.Inputs[0].Path = "changed"
	first.Host.Outputs[0] = "changed"
	if second.Host.Tools[0] != "sh" || second.Host.Inputs[0].Path != "out" || second.Host.Outputs[0] != "report" {
		t.Fatal("import invocations share host declaration storage")
	}
}

func TestHostTemplateOverridesCannotChangeContainerWorkdirSemantics(t *testing.T) {
	component := pin("container")
	documents := map[string]*artifact.Document{component: {Data: []byte("version: 1\ntasks: {run: {image: scratch}}\n")}}
	path := local(t, "version: 1\nimports: {tools: "+component+"}\ntasks: {run: {uses: tools/run, workdir: relative}}\n")
	if _, err := loader(documents).Load(context.Background(), path); err == nil || !strings.Contains(err.Error(), "absolute container path") {
		t.Fatalf("relative workdir accepted for container template: %v", err)
	}
}
