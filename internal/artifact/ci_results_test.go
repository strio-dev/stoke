package artifact

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
)

func ciTestScope() CIScope {
	return CIScope{Provider: "github", Server: "https://github.com", Repository: "strio/stoke", Run: "42", Source: "revision", Invocation: "invocation-one", Plan: strings.Repeat("a", 64)}
}

func ciTestResult(t *testing.T, producer, platform string) CIResult {
	t.Helper()
	nonce, err := NewCIExecutionID()
	if err != nil {
		t.Fatal(err)
	}
	return CIResult{Version: CIVersion, Scope: ciTestScope(), Producer: producer, Attempt: 1, Execution: nonce, Kind: "image", Platform: platform}
}

//nolint:staticcheck // Existing nondistributable OCI layers must retain their local payloads.
func ciTestLayout(t *testing.T, kind, platform, value string) (string, string) {
	t.Helper()
	ctx := t.Context()
	directory := t.TempDir()
	store, err := oci.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	data := []byte(value)
	if err := writer.WriteHeader(&tar.Header{Name: "value", Size: int64(len(data)), Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	layer := content.NewDescriptorFromBytes(v1.MediaTypeImageLayer, archive.Bytes())
	if kind == "foreign-image" {
		layer.MediaType = v1.MediaTypeImageLayerNonDistributable
	}
	if err := store.Push(ctx, layer, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(platform, "/")
	image := v1.Image{Platform: v1.Platform{OS: parts[0], Architecture: parts[1]}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{layer.Digest}}}
	if len(parts) == 3 {
		image.Variant = parts[2]
	}
	configData, err := json.Marshal(image)
	if err != nil {
		t.Fatal(err)
	}
	configType, artifactType := v1.MediaTypeImageConfig, ""
	if kind == "component" {
		configType, artifactType = ciComponentConfigType, ciComponentType
		configData = []byte(`{"version":"coopr.strio.dev/component/v1","platform":{"os":"linux","architecture":"amd64"},"component":{"name":"example"}}`)
	}
	config := content.NewDescriptorFromBytes(configType, configData)
	if err := store.Push(ctx, config, bytes.NewReader(configData)); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: artifactType, Config: config, Layers: []v1.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	root := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, body)
	if err := store.Push(ctx, root, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(ctx, root, "result"); err != nil {
		t.Fatal(err)
	}
	return directory, root.Digest.String()
}

func ciTestMetadata(t *testing.T, client Client, repository, reference string) (v1.Index, v1.Manifest) {
	t.Helper()
	repo, err := client.ciRepository(repository)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := repo.Resolve(t.Context(), strings.Split(reference, "@")[1])
	if err != nil {
		t.Fatal(err)
	}
	data, err := fetch(t.Context(), repo, descriptor, MaxCIMetadataBytes)
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil || descriptor.MediaType != v1.MediaTypeImageIndex || len(index.Manifests) == 0 {
		t.Fatal("CI root is not a metadata-first OCI index", err)
	}
	data, err = fetch(t.Context(), repo, index.Manifests[0], MaxCIMetadataBytes)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return index, manifest
}

// Model metadata-index readback unavailability independently of publication.
// Direct tagged PUTs work; payload validation reads remain available.
func TestCIMetadataRetentionWithoutNewRootReadback(t *testing.T) {
	for _, kind := range []struct{ name, artifactType string }{{"result", CIResultType}, {"inputs", CIInputSetType}} {
		t.Run(kind.name, func(t *testing.T) {
			base := registryFixture(t)
			var mu sync.Mutex
			roots := map[digest.Digest]bool{}
			readsBeforeTag := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, path := registryFixturePath(r.URL.Path)
				if r.Method == http.MethodGet && strings.HasPrefix(path, "manifests/sha256:") {
					mu.Lock()
					tagged, known := roots[digest.Digest(strings.TrimPrefix(path, "manifests/"))]
					if known && !tagged {
						readsBeforeTag++
					}
					mu.Unlock()
					if known && !tagged {
						http.NotFound(w, r)
						return
					}
				}
				var root digest.Digest
				if r.Method == http.MethodPut && strings.HasPrefix(path, "manifests/") && r.Header.Get("Content-Type") == v1.MediaTypeImageIndex {
					data, err := io.ReadAll(r.Body)
					_ = r.Body.Close()
					if err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(data))
					var index v1.Index
					if json.Unmarshal(data, &index) == nil && index.ArtifactType == kind.artifactType {
						root = digest.FromBytes(data)
					}
				}
				response := httptest.NewRecorder()
				base.Config.Handler.ServeHTTP(response, r)
				if response.Code == http.StatusCreated && root != "" {
					mu.Lock()
					roots[root] = roots[root] || strings.TrimPrefix(path, "manifests/") != root.String()
					mu.Unlock()
				}
				for name, values := range response.Header() {
					w.Header()[name] = values
				}
				w.WriteHeader(response.Code)
				_, _ = w.Write(response.Body.Bytes())
			}))
			t.Cleanup(server.Close)
			repository := strings.TrimPrefix(server.URL, "http://") + "/results"
			client := Client{PlainHTTP: true}
			layout, output := ciTestLayout(t, "image", "linux/amd64", "retained producer")
			ref, err := client.PublishCIResult(t.Context(), repository, ciTestResult(t, "build", "linux/amd64"), layout, output)
			if err == nil && kind.name == "inputs" {
				ref, err = client.PublishCIInputSet(t.Context(), repository, CIInputSet{Version: CIVersion, Scope: ciTestScope(), Bindings: map[string]string{"build": ref}})
			}
			if err != nil {
				t.Fatalf("%s metadata retention required reading its new root before tagged publication: %v", kind.name, err)
			}
			mu.Lock()
			reads := readsBeforeTag
			mu.Unlock()
			if reads != 0 {
				t.Fatalf("metadata publication attempted %d pre-tag root reads", reads)
			}
			if kind.name == "inputs" {
				_, err = client.ReadCIInputSet(t.Context(), repository, ref, ciTestScope())
			} else {
				_, err = client.ReadCIResult(t.Context(), repository, ref, CIResultExpectation{Scope: ciTestScope(), Producer: "build", Kind: "image", Platform: "linux/amd64", MaxAttempt: 1})
			}
			if err != nil {
				t.Fatal("tagged metadata root was not independently readable", err)
			}
		})
	}
}

func TestCIResultAndInputSetRoundTrip(t *testing.T) {
	base := registryFixture(t)
	var mu sync.Mutex
	blobUploads, manifestUploads := map[string]bool{}, map[string]bool{}
	blobReads := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, password, ok := r.BasicAuth(); !ok || user != "writer" || password != "fixture-password" {
			w.Header().Set("WWW-Authenticate", `Basic realm="ci-results"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if _, path := registryFixturePath(r.URL.Path); r.Method == http.MethodGet && strings.HasPrefix(path, "blobs/") {
			mu.Lock()
			blobReads[path]++
			mu.Unlock()
		}
		if r.Method == http.MethodPut {
			var manifestIdentity string
			if _, path := registryFixturePath(r.URL.Path); strings.HasPrefix(path, "manifests/") {
				data, err := io.ReadAll(r.Body)
				_ = r.Body.Close()
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(data))
				manifestIdentity = digest.FromBytes(data).String()
			}
			mu.Lock()
			if manifestIdentity != "" {
				manifestUploads[manifestIdentity] = true
			}
			if identity := r.URL.Query().Get("digest"); identity != "" && strings.Contains(r.URL.Path, "/blobs/uploads/") {
				blobUploads[identity] = true
			}
			mu.Unlock()
		}
		base.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	repository := strings.TrimPrefix(server.URL, "http://") + "/results"
	authFile := filepath.Join(t.TempDir(), "auth.json")
	authData, err := json.Marshal(map[string]any{"auths": map[string]any{strings.TrimPrefix(server.URL, "http://"): map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte("writer:fixture-password"))}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authFile, authData, 0600); err != nil {
		t.Fatal(err)
	}
	client := Client{PlainHTTP: true, AuthFile: authFile}
	scope := ciTestScope()
	path, root := ciTestLayout(t, "image", "linux/amd64", "first")
	identity := ciTestResult(t, "build", "linux/amd64")
	ref, err := client.PublishCIResult(t.Context(), repository, identity, path, root)
	if err != nil {
		t.Fatal(err)
	}
	expected := CIResultExpectation{Scope: scope, Producer: "build", MaxAttempt: 1, Kind: "image", Platform: "linux/amd64"}
	result, err := client.ReadCIResult(t.Context(), repository, ref, expected)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output.Digest.String() != root || result.Execution != identity.Execution {
		t.Fatal("canonical result changed")
	}
	set := CIInputSet{Version: CIVersion, Scope: scope, Bindings: map[string]string{"build": ref}}
	inputRef, err := client.PublishCIInputSet(t.Context(), repository, set)
	if err != nil {
		t.Fatal(err)
	}
	read, err := client.ReadCIInputSet(t.Context(), repository, inputRef, scope)
	if err != nil || read.Bindings["build"] != ref {
		t.Fatalf("input bindings %v: %v", read, err)
	}
	reused, merged, err := client.MergeCIInputSets(t.Context(), repository, scope, []string{inputRef, inputRef})
	if err != nil || reused != inputRef || merged.Bindings["build"] != ref {
		t.Fatalf("merge %s %v %v", reused, merged, err)
	}
	value := strings.Repeat("x", 12<<20)
	dependentPath, dependentRoot := ciTestLayout(t, "foreign-image", "linux/amd64", value)
	dependent := ciTestResult(t, "package", "linux/amd64")
	dependent.Inputs = inputRef
	dependentRef, err := client.PublishCIResult(t.Context(), repository, dependent, dependentPath, dependentRoot)
	if err != nil {
		t.Fatal("publish result depending on the exact producer InputSet", err)
	}
	if err := os.RemoveAll(dependentPath); err != nil {
		t.Fatal(err)
	}
	index, metadata := ciTestMetadata(t, client, repository, dependentRef)
	if index.ArtifactType != CIResultType || len(index.Manifests) != 3 || index.Manifests[1].Digest.String() != dependentRoot || index.Manifests[2].MediaType != v1.MediaTypeImageIndex || index.Manifests[2].Digest.String() != strings.Split(inputRef, "@")[1] || metadata.Config.MediaType != ciResultConfigType || metadata.Layers == nil || len(metadata.Layers) != 0 {
		t.Fatal("dependent result put metadata, output or lineage in the wrong OCI role")
	}
	index, metadata = ciTestMetadata(t, client, repository, inputRef)
	if index.ArtifactType != CIInputSetType || len(index.Manifests) != 2 || index.Manifests[1].MediaType != v1.MediaTypeImageIndex || index.Manifests[1].Digest.String() != strings.Split(ref, "@")[1] || metadata.Config.MediaType != ciInputSetConfigType || metadata.Layers == nil || len(metadata.Layers) != 0 {
		t.Fatal("InputSet put its result edge or record in the wrong OCI role")
	}
	expected.Producer = "package"
	consumer := Client{PlainHTTP: true, AuthFile: authFile}
	readResult, err := consumer.ReadCIResult(t.Context(), repository, dependentRef, expected)
	if err != nil || readResult.Inputs != inputRef || readResult.Execution != dependent.Execution {
		t.Fatalf("dependent result lost exact lineage or execution: %+v, %v", readResult, err)
	}
	dir := filepath.Join(t.TempDir(), "payload")
	payload, err := consumer.FetchCIPayload(t.Context(), repository, dependentRef, expected, dir)
	if err != nil {
		t.Fatal(err)
	}
	if payload.CanonicalReference != repository+"@"+dependentRoot || payload.Path != filepath.Join(dir, "image") {
		t.Fatalf("wrong payload %#v", payload)
	}
	local, err := oci.NewFromFS(t.Context(), os.DirFS(payload.Path))
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := local.Resolve(t.Context(), "result")
	if err != nil || descriptor.Digest.String() != dependentRoot {
		t.Fatalf("payload root %v %v", descriptor, err)
	}
	data, err := content.FetchAll(t.Context(), local, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil || len(manifest.Layers) != 1 {
		t.Fatal("restored result lost its image layer", err)
	}
	layer, err := local.Fetch(t.Context(), manifest.Layers[0])
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(layer)
	header, err := archive.Next()
	if err != nil || header.Name != "value" {
		_ = layer.Close()
		t.Fatal("restored layer lost producer file", err)
	}
	data, err = io.ReadAll(archive)
	if err := errors.Join(err, layer.Close()); err != nil || string(data) != value {
		t.Fatal("large result bytes changed during authenticated handoff", err)
	}
	mu.Lock()
	before := blobReads["blobs/"+manifest.Layers[0].Digest.String()]
	mu.Unlock()
	deferredPath := filepath.Join(t.TempDir(), "deferred")
	resolved, err := consumer.ResolveCIInput(t.Context(), repository, dependentRef, expected, deferredPath)
	if err != nil || resolved.Path != "" || resolved.CanonicalReference != repository+"@"+dependentRoot || resolved.Result.Execution != dependent.Execution {
		t.Fatalf("resolved input lost its exact image identity: %+v, %v", resolved, err)
	}
	if _, err := os.Lstat(deferredPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("image resolution created an unrequested local payload", err)
	}
	mu.Lock()
	after := blobReads["blobs/"+manifest.Layers[0].Digest.String()]
	mu.Unlock()
	if before == 0 || after != before {
		t.Fatalf("metadata-only resolution fetched producer layer: before=%d after=%d", before, after)
	}
	if _, err := consumer.FetchCIPayload(t.Context(), repository, dependentRef, expected, dir); err == nil {
		t.Fatal("overwrote existing result")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, identity := range []string{root, dependentRoot, strings.Split(ref, "@")[1], strings.Split(inputRef, "@")[1], strings.Split(dependentRef, "@")[1]} {
		if !manifestUploads[identity] {
			t.Errorf("OCI root was not published through the manifest endpoint: %s", identity)
		}
	}
	for identity := range manifestUploads {
		if blobUploads[identity] {
			t.Errorf("OCI manifest/index was also uploaded as a blob: %s", identity)
		}
	}
}

func TestCIResultsSeparateExecutionsAndRejectRetryLineageConflict(t *testing.T) {
	server := registryFixture(t)
	repository := strings.TrimPrefix(server.URL, "http://") + "/results"
	client := Client{PlainHTTP: true}
	scope := ciTestScope()
	path, root := ciTestLayout(t, "image", "linux/amd64", "same")
	refs := make([]string, 2)
	errs := make([]error, 2)
	ids := []CIResult{ciTestResult(t, "build", "linux/amd64"), ciTestResult(t, "build", "linux/amd64")}
	ids[1].Attempt = 2
	var group sync.WaitGroup
	for i := range ids {
		group.Add(1)
		go func() {
			defer group.Done()
			refs[i], errs[i] = client.PublishCIResult(t.Context(), repository, ids[i], path, root)
		}()
	}
	group.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if refs[0] == refs[1] {
		t.Fatal("different executions collapsed to payload identity")
	}
	sets := make([]string, 2)
	for i, ref := range refs {
		var err error
		sets[i], err = client.PublishCIInputSet(t.Context(), repository, CIInputSet{Version: CIVersion, Scope: scope, Bindings: map[string]string{"build": ref}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := client.MergeCIInputSets(t.Context(), repository, scope, sets); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("mixed new build with old tested result: %v", err)
	}
	impossible := ciTestResult(t, "package", "linux/amd64")
	impossible.Inputs = sets[1]
	if _, err := client.PublishCIResult(t.Context(), repository, impossible, path, root); err == nil {
		t.Fatal("old execution consumed future producer")
	}
	other := ids[0]
	other.Scope.Invocation = "different-reusable-call"
	otherRef, err := client.PublishCIResult(t.Context(), repository, other, path, root)
	if err != nil || otherRef == refs[0] {
		t.Fatal("reusable invocations collided", err)
	}
	previous := CIResultExpectation{Scope: scope, Producer: "build", MaxAttempt: 2, Platform: "linux/amd64"}
	if _, err := client.ReadCIResult(t.Context(), repository, refs[0], previous); err != nil {
		t.Fatal("explicit prior attempt rejected", err)
	}
	previous.MaxAttempt = 1
	if _, err := client.ReadCIResult(t.Context(), repository, refs[1], previous); err == nil {
		t.Fatal("accepted future attempt")
	}
}

func TestCIResultScopePlatformKindAndReferenceValidation(t *testing.T) {
	server := registryFixture(t)
	repository := strings.TrimPrefix(server.URL, "http://") + "/results"
	client := Client{PlainHTTP: true}
	scope := ciTestScope()
	path, root := ciTestLayout(t, "image", "linux/amd64", "value")
	ref, err := client.PublishCIResult(t.Context(), repository, ciTestResult(t, "build", "linux/amd64"), path, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"scope", "producer", "kind", "platform", "mutable", "repository", "plan"} {
		t.Run(name, func(t *testing.T) {
			expect := CIResultExpectation{Scope: scope, Producer: "build", MaxAttempt: 1, Kind: "image", Platform: "linux/amd64"}
			selected := ref
			switch name {
			case "scope":
				expect.Scope.Invocation = "another"
			case "producer":
				expect.Producer = "other"
			case "kind":
				expect.Kind = "component"
			case "platform":
				expect.Platform = "linux/arm64"
			case "mutable":
				selected = repository + ":latest"
			case "repository":
				selected = strings.Replace(ref, "/results@", "/elsewhere@", 1)
			case "plan":
				expect.Scope.Plan = strings.Repeat("b", 64)
			}
			if _, err := client.ReadCIResult(t.Context(), repository, selected, expect); err == nil {
				t.Fatal("accepted incompatible selected result")
			}
		})
	}
	mismatch := ciTestResult(t, "build", "linux/arm64")
	if _, err := client.PublishCIResult(t.Context(), repository, mismatch, path, root); err == nil {
		t.Fatal("published false platform")
	}
	implicit := ciTestResult(t, "implicit", "linux/amd64")
	implicit.Platform = ""
	implicitRef, err := client.PublishCIResult(t.Context(), repository, implicit, path, root)
	if err != nil {
		t.Fatal(err)
	}
	derived, err := client.ReadCIResult(t.Context(), repository, implicitRef, CIResultExpectation{Scope: scope, Producer: "implicit", MaxAttempt: 1, Platform: "linux/amd64"})
	if err != nil || derived.Platform != "linux/amd64" {
		t.Fatal("platform not derived from payload", err)
	}
}

func TestCIInputSetLineageIsClosedAndPayloadFetchIsSelective(t *testing.T) {
	base := registryFixture(t)
	var mu sync.Mutex
	reads := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			mu.Lock()
			reads[r.URL.Path]++
			mu.Unlock()
		}
		base.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	repository := strings.TrimPrefix(server.URL, "http://") + "/results"
	client := Client{PlainHTTP: true}
	scope := ciTestScope()
	parentPath, parentRoot := ciTestLayout(t, "image", "linux/amd64", "ancestor")
	parent, err := client.PublishCIResult(t.Context(), repository, ciTestResult(t, "build", "linux/amd64"), parentPath, parentRoot)
	if err != nil {
		t.Fatal(err)
	}
	parentSet, err := client.PublishCIInputSet(t.Context(), repository, CIInputSet{Version: CIVersion, Scope: scope, Bindings: map[string]string{"build": parent}})
	if err != nil {
		t.Fatal(err)
	}
	childPath, childRoot := ciTestLayout(t, "image", "linux/amd64", "child")
	child := ciTestResult(t, "package", "linux/amd64")
	child.Inputs = parentSet
	childRef, err := client.PublishCIResult(t.Context(), repository, child, childPath, childRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.PublishCIInputSet(t.Context(), repository, CIInputSet{Version: CIVersion, Scope: scope, Bindings: map[string]string{"package": childRef}}); err == nil {
		t.Fatal("accepted missing ancestor binding")
	}
	childSet, err := client.PublishCIInputSet(t.Context(), repository, CIInputSet{Version: CIVersion, Scope: scope, Bindings: map[string]string{"build": parent, "package": childRef}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadCIInputSet(t.Context(), repository, childSet, scope); err != nil {
		t.Fatal(err)
	}
	// The child's ancestor image layer is reachable remotely for GC but is not
	// downloaded as execution input when only the child result is requested.
	parentStore, err := oci.NewFromFS(t.Context(), os.DirFS(parentPath))
	if err != nil {
		t.Fatal(err)
	}
	parentDesc, err := parentStore.Resolve(t.Context(), parentRoot)
	if err != nil {
		t.Fatal(err)
	}
	data, err := content.FetchAll(t.Context(), parentStore, parentDesc)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	clear(reads)
	mu.Unlock()
	if _, err := client.FetchCIPayload(t.Context(), repository, childRef, CIResultExpectation{Scope: scope, Producer: "package", MaxAttempt: 1}, filepath.Join(t.TempDir(), "selected")); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	count := reads["/v2/results/blobs/"+manifest.Layers[0].Digest.String()]
	mu.Unlock()
	if count != 0 {
		t.Fatal("downloaded ancestor payload layer")
	}
}

func TestCIComponentPayloadArchiveAndCanceledFetchCleanup(t *testing.T) {
	base := registryFixture(t)
	var mu sync.Mutex
	blockedArchive, blockedRequests := "", 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		blocked := r.Method == http.MethodGet && r.URL.Path == blockedArchive
		if blocked {
			blockedRequests++
		}
		mu.Unlock()
		if blocked {
			http.NotFound(w, r)
			return
		}
		base.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	repository := strings.TrimPrefix(server.URL, "http://") + "/results"
	client := Client{PlainHTTP: true}
	scope := ciTestScope()
	path, root := ciTestLayout(t, "component", "linux/amd64", "package")
	store, err := oci.New(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(path, "blobs", "sha256", strings.TrimPrefix(root, "sha256:")))
	if err != nil {
		t.Fatal(err)
	}
	var auxiliary v1.Manifest
	if err := json.Unmarshal(raw, &auxiliary); err != nil {
		t.Fatal(err)
	}
	auxiliary.Annotations = map[string]string{"io.strio.bale.fixture": "auxiliary"}
	auxData, err := json.Marshal(auxiliary)
	if err != nil {
		t.Fatal(err)
	}
	auxDesc := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, auxData)
	if err := store.Push(t.Context(), auxDesc, bytes.NewReader(auxData)); err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(t.Context(), auxDesc, "bale-attachment"); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(path, "index.json")
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		t.Fatal(err)
	}
	index.Annotations = map[string]string{"io.strio.fixture.archive-repository": "example.test/original"}
	indexData, err = json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, indexData, 0600); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(t.TempDir(), "original.oci.tar")
	if err := archiveCILayout(t.Context(), path, original); err != nil {
		t.Fatal(err)
	}
	originalBytes, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	identity := ciTestResult(t, "component", "linux/amd64")
	identity.Kind = "component"
	ref, err := client.PublishCIResult(t.Context(), repository, identity, original, root)
	if err != nil {
		t.Fatal(err)
	}
	resultIndex, metadata := ciTestMetadata(t, client, repository, ref)
	result, err := client.ReadCIResult(t.Context(), repository, ref, CIResultExpectation{Scope: scope, Producer: "component", Kind: "component", MaxAttempt: 1})
	if err != nil || result.Archive == nil {
		t.Fatal("component result lost its original archive descriptor", err)
	}
	if len(resultIndex.Manifests) != 2 || !content.Equal(resultIndex.Manifests[1], result.Output) || len(metadata.Layers) != 1 || !content.Equal(metadata.Layers[0], *result.Archive) {
		t.Fatal("component output and archive use the wrong OCI child roles")
	}
	payload, err := client.FetchCIPayload(t.Context(), repository, ref, CIResultExpectation{Scope: scope, Producer: "component", Kind: "component", MaxAttempt: 1}, filepath.Join(t.TempDir(), "component"))
	if err != nil {
		t.Fatal(err)
	}
	fetchedBytes, err := os.ReadFile(payload.Path)
	if err != nil || !bytes.Equal(fetchedBytes, originalBytes) {
		t.Fatal("component archive bytes changed", err)
	}
	archive, err := oci.NewFromTar(t.Context(), payload.Path)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := archive.Resolve(t.Context(), "result")
	if err != nil || descriptor.Digest.String() != root {
		t.Fatalf("component archive %v %v", descriptor, err)
	}
	restoredIndex, err := archive.Resolve(t.Context(), "bale-attachment")
	if err != nil || restoredIndex.Digest != auxDesc.Digest {
		t.Fatal("component auxiliary root lost", err)
	}
	mu.Lock()
	blockedArchive = "/v2/results/blobs/" + result.Archive.Digest.String()
	mu.Unlock()
	missingParent := t.TempDir()
	if _, err := client.FetchCIPayload(t.Context(), repository, ref, CIResultExpectation{Scope: scope, Producer: "component", Kind: "component", MaxAttempt: 1}, filepath.Join(missingParent, "missing-archive")); err == nil {
		t.Fatal("missing original component archive was substituted")
	}
	mu.Lock()
	requests := blockedRequests
	blockedArchive = ""
	mu.Unlock()
	if requests == 0 {
		t.Fatal("missing archive test did not reach the actual archive blob")
	}
	if entries, err := os.ReadDir(missingParent); err != nil || len(entries) != 0 {
		t.Fatal("failed archive fetch left payload staging", entries, err)
	}
	parent := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.FetchCIPayload(ctx, repository, ref, CIResultExpectation{Scope: scope, Producer: "component"}, filepath.Join(parent, "cancelled")); err == nil {
		t.Fatal("cancelled fetch succeeded")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatal("cancelled fetch left files", entries, err)
	}
}

func TestCIResultMissingPayloadAndCorruptMetadataFailClosed(t *testing.T) {
	base := registryFixture(t)
	var mu sync.Mutex
	blocked := ""
	corrupt := ""
	corruptReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		missing, bad := blocked, corrupt
		if bad != "" && r.URL.Path == bad && r.Method == http.MethodGet {
			corruptReads++
		}
		mu.Unlock()
		if missing != "" && r.URL.Path == missing {
			http.NotFound(w, r)
			return
		}
		if bad != "" && r.URL.Path == bad && r.Method == "GET" {
			_, _ = io.WriteString(w, "corrupted")
			return
		}
		base.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	repository := strings.TrimPrefix(server.URL, "http://") + "/results"
	client := Client{PlainHTTP: true}
	scope := ciTestScope()
	path, root := ciTestLayout(t, "image", "linux/amd64", "value")
	ref, err := client.PublishCIResult(t.Context(), repository, ciTestResult(t, "build", "linux/amd64"), path, root)
	if err != nil {
		t.Fatal(err)
	}
	expect := CIResultExpectation{Scope: scope, Producer: "build", MaxAttempt: 1}
	_, metadata := ciTestMetadata(t, client, repository, ref)
	mu.Lock()
	corrupt = "/v2/results/blobs/" + metadata.Config.Digest.String()
	mu.Unlock()
	if _, err := client.ReadCIResult(t.Context(), repository, ref, expect); err == nil {
		t.Fatal("corrupt exact metadata accepted")
	}
	mu.Lock()
	reads := corruptReads
	corrupt = ""
	blocked = "/v2/results/manifests/" + root
	mu.Unlock()
	if reads == 0 {
		t.Fatal("corrupt metadata test did not reach the actual record blob")
	}
	target := filepath.Join(t.TempDir(), "missing")
	if _, err := client.FetchCIPayload(t.Context(), repository, ref, expect, target); err == nil {
		t.Fatal("missing payload substituted")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed fetch left output directory")
	}
	// Cleanup only needs valid scoped metadata, so an orphaned output cannot
	// prevent deletion being attempted (the fixture does not support DELETE).
	if err := client.DeleteCIObject(t.Context(), repository, ref, scope); err == nil || strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("orphan cleanup did not reach delete: %v", err)
	}
}

func archiveCILayout(ctx context.Context, directory, destination string) (retErr error) {
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	writer := tar.NewWriter(out)
	defer func() { retErr = errors.Join(retErr, writer.Close(), out.Close()) }()
	return filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		parts := strings.Split(relative, "/")
		blob := len(parts) == 3 && parts[0] == "blobs" && parts[1] == "sha256" && len(parts[2]) == 64
		if relative != "oci-layout" && relative != "index.json" && !blob {
			return fmt.Errorf("unexpected OCI layout file %q", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("CI OCI layout must contain only regular files")
		}
		if err := writer.WriteHeader(&tar.Header{Name: relative, Mode: 0644, Size: info.Size()}); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(writer, file, info.Size())
		return errors.Join(copyErr, file.Close())
	})
}

func TestCICancelInFlightUploadAndDownload(t *testing.T) {
	for _, phase := range []string{"upload", "download"} {
		t.Run(phase, func(t *testing.T) {
			base := registryFixture(t)
			started := make(chan struct{})
			var once sync.Once
			var mu sync.Mutex
			blocking := phase == "upload"
			layerPath := ""
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				block, path := blocking, layerPath
				mu.Unlock()
				match := phase == "upload" && r.Method == "POST" && strings.Contains(r.URL.Path, "/blobs/uploads/") || phase == "download" && r.Method == "GET" && r.URL.Path == path
				if block && match {
					once.Do(func() { close(started) })
					<-r.Context().Done()
					return
				}
				base.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			repository := strings.TrimPrefix(server.URL, "http://") + "/results"
			client := Client{PlainHTTP: true}
			path, root := ciTestLayout(t, "image", "linux/amd64", "cancel")
			identity := ciTestResult(t, "build", "linux/amd64")
			ref := ""
			if phase == "download" {
				var err error
				ref, err = client.PublishCIResult(t.Context(), repository, identity, path, root)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(filepath.Join(path, "blobs", "sha256", strings.TrimPrefix(root, "sha256:")))
				if err != nil {
					t.Fatal(err)
				}
				var manifest v1.Manifest
				if err := json.Unmarshal(raw, &manifest); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				layerPath = "/v2/results/blobs/" + manifest.Layers[0].Digest.String()
				blocking = true
				mu.Unlock()
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			parent := t.TempDir()
			go func() {
				if phase == "upload" {
					_, err := client.PublishCIResult(ctx, repository, identity, path, root)
					done <- err
				} else {
					_, err := client.FetchCIPayload(ctx, repository, ref, CIResultExpectation{Scope: ciTestScope(), Producer: "build", MaxAttempt: 1}, filepath.Join(parent, "payload"))
					done <- err
				}
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not enter cancellation barrier")
			}
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("inflight cancellation succeeded")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation did not drain")
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatal("cancelled operation left payload staging", entries, err)
			}
		})
	}
}

func TestCIInputSetRejectsStatusFieldsAndBindingLimits(t *testing.T) {
	server := registryFixture(t)
	repository := strings.TrimPrefix(server.URL, "http://") + "/results"
	client := Client{PlainHTTP: true}
	scope := ciTestScope()
	raw := map[string]any{"version": CIVersion, "scope": scope, "bindings": map[string]string{}, "status": "success"}
	ref, err := client.pushCIObject(t.Context(), repository, CIInputSetType, ciInputSetConfigType, scope, "inputs", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadCIInputSet(t.Context(), repository, ref, scope); err == nil {
		t.Fatal("input bindings accepted a success receipt")
	}
	bindings := map[string]string{}
	for i := 0; i <= MaxCIBindings; i++ {
		bindings[fmt.Sprintf("job-%d", i)] = repository + "@sha256:" + strings.Repeat("a", 64)
	}
	if _, err := client.PublishCIInputSet(t.Context(), repository, CIInputSet{Version: CIVersion, Scope: scope, Bindings: bindings}); err == nil {
		t.Fatal("excessive bindings accepted")
	}
	if _, err := MergeCIBindings(CIInputSet{Bindings: bindings}); err == nil {
		t.Fatal("merge bypassed binding bound")
	}
}
