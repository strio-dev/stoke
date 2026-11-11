package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	cooprwire "github.com/strio-dev/coopr/api"
)

func TestMountTransportPreservesLivePathNamedContextAndAccess(t *testing.T) {
	var got cooprwire.Request
	client := socketClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(cooprwire.Event{Type: "result", Result: &cooprwire.Result{ImageID: "image"}})
	}))
	_, err := client.Execute(context.Background(), Program{States: []State{{ID: "one", From: Source{Image: "scratch"}, Operations: []Operation{
		{Kind: "run", Args: []string{"check"}, Mounts: []Mount{
			{Type: "bind", Target: "/live", HostPath: "/caller/data", Relabel: "shared"},
			{Type: "bind", Target: "/input", Source: "selected", From: Source{Context: "fixtures"}, ReadOnly: true},
		}},
	}}}, Output: "one"}, Options{Contexts: map[string]string{"fixtures": "/caller/fixtures"}, Cache: CacheOptions{MountDir: "/cache/mounts"}})
	if err != nil {
		t.Fatal(err)
	}
	mounts := got.Program.States[0].Operations[0].Mounts
	if mounts[0].HostPath != "/caller/data" || mounts[0].Relabel != "shared" || mounts[0].ReadOnly || mounts[0].Source != "" || mounts[1].From.Context != "fixtures" || !mounts[1].ReadOnly || got.Options.Contexts["fixtures"] != "/caller/fixtures" || got.Options.Cache.MountDir != "/cache/mounts" {
		t.Fatalf("transport changed mount semantics: %+v %+v", mounts, got.Options.Contexts)
	}
}
