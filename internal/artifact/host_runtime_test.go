package artifact

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// This acceptance uses the actual Stoke CLI and a loopback registry, with no
// Coopr runtime or host/Nix security changes. It is explicitly provisioned.
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
		"image":   map[string]any{"image": "scratch", "outputs": map[string]any{"filesystem": true}, "steps": []any{map[string]any{"copy": map[string]any{"from": "prepare", "sources": []string{"out/value"}, "destination": "/input"}}}},
	}}
	f.write(filepath.Join(caller, "stoke.json"), f.encode(w), 0600)
	report, _ := f.run("mixed", caller, filepath.Join(caller, "stoke.json"), []string{"--allow-host"}, true)
	result := f.task(report, "image")
	if result.Filesystem == "" || result.Kind != "image" || string(f.read(filepath.Join(result.Filesystem, "input"))) != "exact host capture" {
		t.Fatal("host files did not reach ordinary Coopr COPY", result)
	}
	f.write(filepath.Join(work, "summary.json"), f.encode(map[string]any{"status": "PASS", "hostArtifact": f.task(report, "prepare").Digest, "nativeImage": result.Digest, "report": report.Directory}), 0600)
	t.Logf("Mixed host/Coopr evidence: %s", work)
}
