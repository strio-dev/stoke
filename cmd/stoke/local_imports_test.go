package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/strio-dev/stoke/internal/ci"
)

func TestPublishRejectsLocalImportsBeforeRegistryAccess(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	t.Chdir(t.TempDir())
	// Even an unused, missing local dependency must fail before fetching it or
	// accessing the registry; publishing never packages a local import tree.
	writeCIFile(t, "stoke.yaml", "version: 1\nimports: {local: ./missing.yaml}\ntasks: {run: {image: scratch}}")
	var out, diagnostics bytes.Buffer
	code := run([]string{"publish", "--plain-http", "-f", "stoke.yaml", strings.TrimPrefix(server.URL, "http://") + "/workflow:unused"}, &out, &diagnostics)
	if code != 1 || requests.Load() != 0 || out.Len() != 0 || !strings.Contains(diagnostics.String(), "publish the dependency to OCI and use its digest-pinned reference") {
		t.Fatalf("local import publication: exit=%d requests=%d out=%s err=%s", code, requests.Load(), &out, &diagnostics)
	}
}

func TestLocalImportCIIdentityHasNoHostPathsAndTracksSource(t *testing.T) {
	readPlan := func() *ci.Plan {
		t.Helper()
		code, out, diagnostics := invokeCI([]string{"ci", "plan", "run"})
		if code != 0 {
			t.Fatal(diagnostics)
		}
		var plan ci.Plan
		if err := json.Unmarshal([]byte(out), &plan); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "stoke-component-") {
			t.Fatal("loader paths leaked into the portable plan")
		}
		return &plan
	}
	var previous *ci.Plan
	for range 2 {
		root := t.TempDir()
		t.Chdir(root)
		writeCIFile(t, "stoke.yaml", "version: 1\nimports: {local: ./lib/check.yaml}\ntasks: {run: {uses: local/check}}")
		writeCIFile(t, "lib/check.yaml", "version: 1\nassets: [helper.txt]\ntasks: {check: {image: scratch, steps: [{copy: {scope: component, sources: [helper.txt], destination: /helper}}]}}")
		writeCIFile(t, "lib/helper.txt", "first")
		plan := readPlan()
		data, err := json.Marshal(plan)
		if err != nil || strings.Contains(string(data), root) {
			t.Fatalf("checkout path in plan: %v", err)
		}
		if previous != nil && previous.ID != plan.ID {
			t.Fatal("same local imports produced checkout-dependent plan identity")
		}
		previous = plan
	}
	writeCIFile(t, "lib/helper.txt", "changed")
	assetsChanged := readPlan()
	if assetsChanged.ID == previous.ID {
		t.Fatal("local asset change retained a stale plan")
	}
	writeCIFile(t, "lib/check.yaml", "version: 1\nassets: [helper.txt]\ntasks: {check: {image: scratch, steps: [{copy: {scope: component, sources: [helper.txt], destination: /different}}]}}")
	if readPlan().ID == assetsChanged.ID {
		t.Fatal("local definition change retained a stale plan")
	}
}
