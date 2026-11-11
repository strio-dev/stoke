package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strio-dev/stoke/internal/hostfiles"
)

func ciHostLayout(t *testing.T, value string) (string, string) {
	t.Helper()
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "out"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "out/value"), []byte(value), 0750); err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(t.TempDir(), "layout")
	identity, err := hostfiles.Capture(t.Context(), workspace, []string{"out"}, layout, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	return layout, identity
}

func TestCIHostFilesRoundTripKeepsExactExecutionAndCapturedOutputs(t *testing.T) {
	server := registryFixture(t)
	repository := strings.TrimPrefix(server.URL, "http://") + "/host-results"
	client := Client{PlainHTTP: true}
	layout, identity := ciHostLayout(t, "this exact producer output")
	result := ciTestResult(t, "host-build", "linux/amd64")
	result.Kind = "host"
	first, err := client.PublishCIResult(t.Context(), repository, result, layout, identity)
	if err != nil {
		t.Fatal(err)
	}
	expect := CIResultExpectation{Scope: ciTestScope(), Producer: "host-build", MaxAttempt: 1, Kind: "host", Platform: "linux/amd64"}
	read, err := client.ReadCIResult(t.Context(), repository, first, expect)
	if err != nil {
		t.Fatal(err)
	}
	if read.Output.Digest.String() != identity || read.Execution != result.Execution {
		t.Fatal("changed host output or execution identity")
	}
	destination := filepath.Join(t.TempDir(), "fetched")
	payload, err := client.FetchCIPayload(t.Context(), repository, first, expect, destination)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Path != filepath.Join(destination, "files") || payload.CanonicalReference != repository+"@"+identity {
		t.Fatalf("wrong host payload: %#v", payload)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := hostfiles.Restore(t.Context(), payload.Path, identity, restored); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(restored, "out/value"))
	if err != nil || string(data) != "this exact producer output" {
		t.Fatalf("restored %q: %v", data, err)
	}
	set := CIInputSet{Version: CIVersion, Scope: ciTestScope(), Bindings: map[string]string{"host-build": first}}
	inputs, err := client.PublishCIInputSet(t.Context(), repository, set)
	if err != nil {
		t.Fatal(err)
	}
	test := ciTestResult(t, "host-test", "linux/amd64")
	test.Kind = "host"
	test.Inputs = inputs
	tested, err := client.PublishCIResult(t.Context(), repository, test, layout, identity)
	if err != nil {
		t.Fatal(err)
	}
	expectTest := expect
	expectTest.Producer = "host-test"
	testedResult, err := client.ReadCIResult(t.Context(), repository, tested, expectTest)
	if err != nil || testedResult.Inputs != inputs {
		t.Fatalf("host lineage lost: %#v %v", testedResult, err)
	}
	secondResult := ciTestResult(t, "host-build", "linux/amd64")
	secondResult.Kind = "host"
	second, err := client.PublishCIResult(t.Context(), repository, secondResult, layout, identity)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("collapsed independent executions with equal captured bytes")
	}
	wrongKind := expect
	wrongKind.Kind = "image"
	if _, err := client.ReadCIResult(t.Context(), repository, first, wrongKind); err == nil {
		t.Fatal("host output accepted as runnable image")
	}
	wrongPlatform := expect
	wrongPlatform.Platform = "linux/arm64"
	if _, err := client.ReadCIResult(t.Context(), repository, first, wrongPlatform); err == nil {
		t.Fatal("host platform mismatch accepted")
	}
}

func TestCIHostFilesRejectImagePayloadAndChangedCapturedBytes(t *testing.T) {
	server := registryFixture(t)
	repository := strings.TrimPrefix(server.URL, "http://") + "/host-results"
	client := Client{PlainHTTP: true}
	result := ciTestResult(t, "build", "linux/amd64")
	result.Kind = "host"
	image, identity := ciTestLayout(t, "image", "linux/amd64", "wrong payload kind")
	if _, err := client.PublishCIResult(t.Context(), repository, result, image, identity); err == nil {
		t.Fatal("image published as captured host files")
	}
	layout, identity := ciHostLayout(t, "original")
	manifest := filepath.Join(layout, "blobs/sha256", strings.TrimPrefix(identity, "sha256:"))
	if err := os.WriteFile(manifest, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PublishCIResult(t.Context(), repository, result, layout, identity); err == nil {
		t.Fatal("changed captured payload published")
	}
}
