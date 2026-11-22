package artifact

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/package-url/packageurl-go"
)

func TestCDEventsWithActualCooprPublication(t *testing.T) {
	f := newCIRegistryFixture(t)
	caller := filepath.Join(f.work, "event-source")
	f.write(filepath.Join(caller, "value"), []byte("public synthetic payload\n"), 0600)
	workflow := filepath.Join(caller, "stoke.json")
	f.write(workflow, f.encode(map[string]any{"version": 1, "tasks": map[string]any{
		"arbitrary-producer": map[string]any{"image": "scratch", "steps": []any{ciRegistryCopy("", "value", "/value")}},
		"deliver":            map[string]any{"publish": map[string]any{"from": "arbitrary-producer", "destinations": []string{f.registry + "/events/actual:checked"}}},
	}}), 0600)
	events := filepath.Join(f.work, "events.jsonl")
	report, _ := f.run("events-publish", caller, workflow, []string{"--network", "none", "--events", events}, true)
	if output := f.task(report, "deliver"); output.Status != "passed" {
		t.Fatal("actual publication failed", output)
	}
	response, err := http.Get("http://" + f.registry + "/v2/events/actual/manifests/checked")
	if err != nil {
		t.Fatal(err)
	}
	manifest, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("published manifest unavailable: %v %v %s", readErr, closeErr, response.Status)
	}
	var published int
	for _, line := range bytes.Split(bytes.TrimSpace(f.read(events)), []byte("\n")) {
		var event struct {
			Context struct{ Type string }
			Subject struct{ Id string }
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.Context.Type == "dev.cdevents.artifact.published.0.3.0" {
			published++
			reference, err := packageurl.FromString(event.Subject.Id)
			if err != nil || reference.Type != packageurl.TypeOCI || reference.Name != "actual" || reference.Version != digest.FromBytes(manifest).String() || reference.Qualifiers.Map()["repository_url"] != f.registry+"/events/actual" {
				t.Fatal("actual destination did not yield exact OCI identity", event.Subject.Id)
			}
		}
	}
	if published != 1 {
		t.Fatalf("observed publication events=%d, want1", published)
	}
}
