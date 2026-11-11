package source

import (
	"context"
	"strings"
	"testing"

	"github.com/strio-dev/stoke/internal/artifact"
)

func TestReusablePlatformBindingsRemainOwnedAndIsolated(t *testing.T) {
	ref := pin("platforms")
	library := `version: 1
inputs: {target: {required: true}}
tasks:
  input: {image: scratch, platform: {input: target}}
  matrix:
    image: scratch
    platform: {matrix: target}
    strategy: {matrix: {target: [linux/amd64, linux/arm64]}}
`
	root := "version: 1\nimports: {tools: " + ref + "}\ntasks:\n  first: {uses: tools/input, with: {target: linux/amd64}}\n  second: {uses: tools/input, with: {target: linux/arm64}}\n  matrix: {uses: tools/matrix, with: {target: linux/amd64}}\n"
	loaded, err := loader(map[string]*artifact.Document{ref: {Data: []byte(library)}}).Load(context.Background(), local(t, root))
	if err != nil {
		t.Fatal(err)
	}
	closeLoaded(t, loaded)
	for name, want := range map[string]string{"first": "linux/amd64", "second": "linux/arm64"} {
		got, err := loaded.Workflow.Tasks[name].ResolvedPlatform()
		if err != nil || got != want {
			t.Fatalf("%s platform = %q, %v", name, got, err)
		}
	}
	*loaded.Workflow.Tasks["first"].Platform.Value = "linux/s390x"
	if got, _ := loaded.Workflow.Tasks["second"].ResolvedPlatform(); got != "linux/arm64" {
		t.Fatal("reusable invocation platform literals alias")
	}
	plan, err := loaded.Workflow.Expand([]string{"matrix"})
	if err != nil || len(plan.Groups) != 1 || len(plan.Groups[0].Jobs) != 2 {
		t.Fatalf("reusable matrix platform expansion = %+v, %v", plan, err)
	}
	for _, job := range plan.Groups[0].Jobs {
		if got, err := job.Definition.ResolvedPlatform(); err != nil || got != job.Matrix["target"] {
			t.Fatalf("reusable matrix platform = %q, %v", got, err)
		}
	}
}

func TestReusablePlatformCallsiteOverrideRejected(t *testing.T) {
	ref := pin("platform-override")
	_, err := loader(map[string]*artifact.Document{ref: {Data: []byte("version: 1\ntasks: {run: {image: scratch, platform: linux/arm64}}\n")}}).Load(context.Background(), local(t, "version: 1\nimports: {tools: "+ref+"}\ntasks: {test: {uses: tools/run, platform: linux/amd64}}\n"))
	if err == nil || !strings.Contains(err.Error(), "platform cannot") {
		t.Fatalf("callsite platform override = %v", err)
	}
}
