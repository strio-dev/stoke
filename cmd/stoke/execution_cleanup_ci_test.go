package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/stoke/internal/artifact"
	"github.com/strio-dev/stoke/internal/ci"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
)

// The existing artifact-package registry fixture is test-private. This bounded
// subset uses the same in-memory Distribution boundary, without a new dependency
// or any external registry. It supports only these fixture-owned OCI objects.
func executionCleanupRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	objects := map[string][]byte{}
	media := map[string]string{}
	uploads := map[string][]byte{}
	next := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/v2/" {
			return
		}
		const prefix = "/v2/results/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, prefix)
		if strings.HasPrefix(key, "blobs/uploads/") {
			switch r.Method {
			case http.MethodPost:
				next++
				key = "blobs/uploads/" + strconv.Itoa(next)
				uploads[key] = nil
				w.Header().Set("Location", prefix+key)
				w.WriteHeader(http.StatusAccepted)
			case http.MethodPatch, http.MethodPut:
				data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
				data = append(uploads[key], data...)
				if r.Method == http.MethodPatch {
					uploads[key] = data
					w.Header().Set("Location", prefix+key)
					w.WriteHeader(http.StatusAccepted)
					return
				}
				selected := r.URL.Query().Get("digest")
				if digest.FromBytes(data).String() != selected {
					http.Error(w, "fixture blob digest mismatch", 400)
					return
				}
				objects["blobs/"+selected] = data
				delete(uploads, key)
				w.Header().Set("Docker-Content-Digest", selected)
				w.Header().Set("Location", prefix+"blobs/"+selected)
				w.WriteHeader(http.StatusCreated)
			default:
				http.Error(w, "unexpected upload method", http.StatusMethodNotAllowed)
			}
			return
		}
		if r.Method == http.MethodPut && strings.HasPrefix(key, "manifests/") {
			data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			selected := digest.FromBytes(data).String()
			objects[key], objects["manifests/"+selected] = data, data
			media[key], media["manifests/"+selected] = r.Header.Get("Content-Type"), r.Header.Get("Content-Type")
			w.Header().Set("Docker-Content-Digest", selected)
			w.WriteHeader(http.StatusCreated)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "unexpected fixture request", http.StatusMethodNotAllowed)
			return
		}
		data, exists := objects[key]
		if !exists {
			http.NotFound(w, r)
			return
		}
		contentType := media[key]
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Docker-Content-Digest", digest.FromBytes(data).String())
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func executionCleanupImage(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	store, err := oci.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	configData, err := json.Marshal(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, RootFS: v1.RootFS{Type: "layers"}})
	if err != nil {
		t.Fatal(err)
	}
	config := content.NewDescriptorFromBytes(v1.MediaTypeImageConfig, configData)
	if err := store.Push(t.Context(), config, bytes.NewReader(configData)); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{}})
	if err != nil {
		t.Fatal(err)
	}
	root := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, data)
	if err := store.Push(t.Context(), root, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(t.Context(), root, "result"); err != nil {
		t.Fatal(err)
	}
	return directory, root.Digest.String()
}

func TestCIExecutionCleanupPreservesDownloadedInputsOnlyOnUnconfirmedCleanup(t *testing.T) {
	for _, code := range []string{"cleanup_failed", "execution_failed"} {
		t.Run(code, func(t *testing.T) {
			root, engine, recordPath := executionCleanupFixture(t)
			t.Setenv("STOKE_CLEANUP_CODE", code)
			// Publication still needs the original downloaded layout. The typed
			// import failure below occurs before any publication can execute.
			if err := os.WriteFile("stoke.yaml", []byte("version: 1\ntasks:\n  build: {image: scratch, platform: linux/amd64}\n  test: {publish: {from: build, destinations: ['registry.invalid/never:published']}}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			registry := executionCleanupRegistry(t)
			repository := strings.TrimPrefix(registry.URL, "http://") + "/results"
			var out, diagnostics bytes.Buffer
			if exit := run([]string{"ci", "generate", "--context", "caller", "--results-repository", repository, "test"}, &out, &diagnostics); exit != 0 {
				t.Fatalf("generate exit%d: %s", exit, &diagnostics)
			}
			plan, err := ci.Decode(bytes.NewReader(readCIFile(t, filepath.Join(".stoke", "ci-plan.json"))))
			if err != nil {
				t.Fatal(err)
			}
			producer, consumer := plan.Groups[0].Jobs[0], plan.Groups[1].Jobs[0]
			scope := testCIScope(plan.ID)
			client := artifact.Client{PlainHTTP: true}
			layout, identity := executionCleanupImage(t)
			resultRef, err := client.PublishCIResult(t.Context(), repository, artifact.CIResult{Version: artifact.CIVersion, Scope: scope, Producer: "build", Attempt: 1, Execution: strings.Repeat("b", 32), Kind: "image", Platform: "linux/amd64"}, layout, identity)
			if err != nil {
				t.Fatal(err)
			}
			inputRef, err := client.PublishCIInputSet(t.Context(), repository, artifact.CIInputSet{Version: artifact.CIVersion, Scope: scope, Bindings: map[string]string{"build": resultRef}})
			if err != nil {
				t.Fatal(err)
			}
			scopeHash, err := scope.Hash()
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := json.Marshal(ciNativeMetadata{Version: 1, Scope: scopeHash, Unit: producer.ID, Attempt: 1, Inputs: inputRef})
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(ci.InputEnv(producer.OutputKey), string(metadata))
			scopeData, err := json.Marshal(scope)
			if err != nil {
				t.Fatal(err)
			}
			out.Reset()
			diagnostics.Reset()
			args := []string{"ci", "run", "--plan", filepath.Join(".stoke", "ci-plan.json"), "--job", consumer.ID, "--scope", string(scopeData), "--attempt", "1", "--needs-env", "--plain-http", "--coopr-binary", engine}
			if exit := runContext(t.Context(), args, &out, &diagnostics); exit != 1 || !strings.Contains(diagnostics.String(), "fixture terminal cleanup_failed diagnostic") {
				t.Fatalf("CI import did not reach typed API failure: exit%d stdout=%s stderr=%s", exit, &out, &diagnostics)
			}
			data, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}
			var record executionCleanupRecord
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(record.Input, string(filepath.Separator)+"ci-inputs"+string(filepath.Separator)+"inputs-") {
				t.Fatalf("engine did not receive a registry-downloaded input: %+v", record)
			}
			for _, file := range []string{record.Marker, record.Input} {
				_, err := os.Stat(file)
				if code == "cleanup_failed" && err != nil {
					t.Errorf("downloaded CI input/evidence removed before confirmed cleanup: %s (%v)", file, err)
				}
				if code != "cleanup_failed" && !os.IsNotExist(err) {
					t.Errorf("ordinary failure retained downloaded CI input: %s (%v)", file, err)
				}
			}
			if data, err := os.ReadFile(filepath.Join(root, "caller", "user-input")); err != nil || string(data) != "caller owned" {
				t.Fatalf("caller context changed: %q (%v)", data, err)
			}
		})
	}
}
