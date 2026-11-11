package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/strio-dev/stoke/internal/workflow"
)

// This exercises foreign image metadata and COPY through the real engine. It
// deliberately runs no foreign executable and does not qualify ARM emulation.
func TestCINativePlatformOverrideWithCoopr(t *testing.T) {
	if testing.Short() {
		t.Skip("requires an explicit development engine and rootless runner")
	}
	engine := os.Getenv("STOKE_COOPR_BINARY")
	evidence := os.Getenv("STOKE_HOST_EVIDENCE_DIR")
	if engine == "" || evidence == "" {
		t.Fatal("select STOKE_COOPR_BINARY and STOKE_HOST_EVIDENCE_DIR")
	}
	runtime, err := nativeTestRuntime("STOKE_PLATFORM_RUNTIME")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(evidence, "native-ci-platform-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("Retained native platform handoff evidence:", root)
	caller := filepath.Join(root, "caller")
	if err := os.Mkdir(caller, 0700); err != nil {
		t.Fatal(err)
	}
	const payload = "exact native ARM image payload\n"
	for name, value := range map[string]string{
		"payload":     payload,
		"image.coopr": "from \"scratch\" platform=\"linux/arm64\" {\n copy \"payload\" \"/payload\"\n}\n",
	} {
		if err := os.WriteFile(filepath.Join(caller, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"reject"}],"transports":{"oci":{"":[{"type":"insecureAcceptAnything"}]},"containers-storage":{"":[{"type":"insecureAcceptAnything"}]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := workflow.Decode(strings.NewReader(`version: 1
tasks:
  producer:
    platform: linux/amd64
    cache: false
    coopr: {files: [image.coopr]}
  consume:
    from: producer
    cache: false
    env: {PLATFORM_HANDOFF: exact}
`))
	if err != nil {
		t.Fatal(err)
	}
	options := func(phase string) Options {
		directory := filepath.Join(root, phase)
		return Options{
			ContextDir: caller, StateDir: filepath.Join(directory, "state"),
			CacheDir: filepath.Join(directory, "cache"), OutputDir: filepath.Join(directory, "output"),
			CooprBinary: engine, StorageDriver: "vfs", Runtime: runtime,
			Network: "none", SignaturePolicy: policy,
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	producer, err := Run(ctx, w, []string{"producer"}, options("local-producer"))
	if err != nil {
		t.Fatal("native platform producer failed", err)
	}
	if producer.Status != "passed" || len(producer.Tasks) != 1 || producer.Tasks[0].Name != "producer" {
		t.Fatal("unexpected native producer report", producer)
	}
	output := producer.Tasks[0]
	if output.Platform != "linux/arm64" || output.Digest == "" || output.OCI == "" {
		t.Fatal("native FROM did not override the authored default", output)
	}
	input := CIImport{
		Kind: "image", OCI: output.OCI, Digest: output.Digest, Platform: output.Platform,
		CanonicalReference: "registry.invalid/platform-proof@" + output.Digest,
	}
	consumer, err := RunCI(ctx, w, CIRequest{Execute: []string{"consume"}, Imports: map[string]CIImport{"producer": input}}, options("ci-consumer"))
	if err != nil {
		t.Fatal("exact native platform handoff failed", err)
	}
	if consumer.Status != "passed" || len(consumer.Tasks) != 1 || consumer.Tasks[0].Name != "consume" || consumer.Tasks[0].Platform != "linux/arm64" {
		t.Fatal("consumer lost actual platform or replayed the producer", consumer)
	}
	for _, result := range []TaskResult{output, consumer.Tasks[0]} {
		data, err := os.ReadFile(filepath.Join(result.Filesystem, "payload"))
		if err != nil || string(data) != payload {
			t.Fatal("exact native output bytes changed", result.Name, string(data), err)
		}
		if _, err := readCIImage(ctx, result.OCI, result.Digest, "linux/arm64"); err != nil {
			t.Fatal("actual OCI platform differs from reported platform", result.Name, err)
		}
	}
	encoded, err := json.MarshalIndent(map[string]any{
		"status": "PASS", "producer": producer, "consumer": consumer,
		"registryUsed": false, "foreignExecutableRun": false,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "result.json"), append(encoded, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}
