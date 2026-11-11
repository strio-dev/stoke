package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/stoke/internal/assets"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

type registryObject struct {
	Data      []byte
	MediaType string
}

type registryStorage struct {
	mu          sync.Mutex
	objects     map[string]registryObject
	uploads     map[string][]byte
	next        int
	allowDelete bool
}

// registryFixture implements the OCI Distribution subset used by these tests,
// including native referrer discovery. It never accesses external registries.
func registryFixture(t *testing.T) *httptest.Server {
	t.Helper()
	return registryFixtureManifestDeletion(t, false)
}

// Manifest deletion removes digest/tag aliases, never shared blobs.
func registryFixtureManifestDeletion(t *testing.T, allowDelete bool) *httptest.Server {
	t.Helper()
	server, _ := registryFixtureStorage(t, allowDelete)
	return server
}

func registryFixtureStorage(t *testing.T, allowDelete bool) (*httptest.Server, *registryStorage) {
	t.Helper()
	storage := &registryStorage{objects: map[string]registryObject{}, uploads: map[string][]byte{}, allowDelete: allowDelete}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repository, path := registryFixturePath(r.URL.Path)
		storage.serve(w, r, repository, path)
	}))
	t.Cleanup(server.Close)
	return server, storage
}

func registryFixturePath(path string) (string, string) {
	raw := strings.TrimPrefix(path, "/v2/")
	for _, marker := range []string{"/blobs/", "/manifests/", "/referrers/", "/tags/"} {
		if i := strings.Index(raw, marker); i >= 0 {
			return raw[:i], raw[i+1:]
		}
	}
	return "", ""
}
func (r *registryStorage) serve(w http.ResponseWriter, q *http.Request, repo, path string) {
	if q.URL.Path == "/v2/" {
		w.WriteHeader(200)
		return
	}
	if repo == "" {
		http.NotFound(w, q)
		return
	}
	// Read network bodies before taking the map lock; response bodies are written
	// after releasing it. Immutable published blobs permit concurrent transfers.
	var data []byte
	if q.Method == "POST" || q.Method == "PATCH" || q.Method == "PUT" {
		var err error
		data, err = io.ReadAll(q.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	}
	key := repo + "/" + path
	if r.allowDelete && q.Method == http.MethodDelete && strings.HasPrefix(path, "manifests/") {
		selected := strings.TrimPrefix(path, "manifests/")
		r.mu.Lock()
		object, exists := r.objects[key]
		if !exists {
			r.mu.Unlock()
			http.NotFound(w, q)
			return
		}
		if selected != digest.FromBytes(object.Data).String() {
			r.mu.Unlock()
			http.Error(w, "manifest deletion requires its digest", http.StatusBadRequest)
			return
		}
		for key, object := range r.objects {
			if strings.HasPrefix(key, repo+"/manifests/") && digest.FromBytes(object.Data).String() == selected {
				delete(r.objects, key)
			}
		}
		r.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if strings.HasPrefix(path, "blobs/uploads/") {
		r.mu.Lock()
		if q.Method == "POST" {
			r.next++
			path = "blobs/uploads/" + strconv.Itoa(r.next)
			key = repo + "/" + path
			r.uploads[key] = data
			r.mu.Unlock()
			w.Header().Set("Location", "/v2/"+repo+"/"+path)
			w.WriteHeader(202)
			return
		}
		if q.Method == "DELETE" {
			delete(r.uploads, key)
			r.mu.Unlock()
			w.WriteHeader(204)
			return
		}
		// Each upload has one sequential writer in the distribution protocol. Only
		// in-memory append/publication is protected, never network I/O.
		r.uploads[key] = append(r.uploads[key], data...)
		size := len(r.uploads[key])
		if q.Method == "PATCH" {
			r.mu.Unlock()
			w.Header().Set("Location", q.URL.Path)
			w.Header().Set("Range", "0-"+strconv.Itoa(max(0, size-1)))
			w.WriteHeader(202)
			return
		}
		identity := q.URL.Query().Get("digest")
		if q.Method != "PUT" || digest.FromBytes(r.uploads[key]).String() != identity {
			r.mu.Unlock()
			http.Error(w, "invalid blob upload", 400)
			return
		}
		r.objects[repo+"/blobs/"+identity] = registryObject{Data: r.uploads[key]}
		delete(r.uploads, key)
		r.mu.Unlock()
		w.Header().Set("Docker-Content-Digest", identity)
		w.Header().Set("Location", "/v2/"+repo+"/blobs/"+identity)
		w.WriteHeader(201)
		return
	}
	if path == "tags/list" {
		tags := []string{}
		r.mu.Lock()
		for k := range r.objects {
			if strings.HasPrefix(k, repo+"/manifests/") {
				tag := strings.TrimPrefix(k, repo+"/manifests/")
				if !strings.Contains(tag, ":") {
					tags = append(tags, tag)
				}
			}
		}
		r.mu.Unlock()
		sort.Strings(tags)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": tags})
		return
	}
	if strings.HasPrefix(path, "referrers/") {
		subject := strings.TrimPrefix(path, "referrers/")
		index := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{}}
		seen := map[string]bool{}
		r.mu.Lock()
		objects := make(map[string]registryObject, len(r.objects))
		for k, v := range r.objects {
			objects[k] = v
		}
		r.mu.Unlock()
		for k, v := range objects {
			if !strings.HasPrefix(k, repo+"/manifests/") {
				continue
			}
			var manifest v1.Manifest
			_ = json.Unmarshal(v.Data, &manifest)
			if manifest.Subject != nil && manifest.Subject.Digest.String() == subject {
				d := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, v.Data)
				d.ArtifactType = manifest.ArtifactType
				if !seen[d.Digest.String()] {
					index.Manifests = append(index.Manifests, d)
					seen[d.Digest.String()] = true
				}
			}
		}
		sort.Slice(index.Manifests, func(i, j int) bool { return index.Manifests[i].Digest < index.Manifests[j].Digest })
		w.Header().Set("Content-Type", v1.MediaTypeImageIndex)
		_ = json.NewEncoder(w).Encode(index)
		return
	}
	if q.Method == "PUT" && strings.HasPrefix(path, "manifests/") {
		identity := digest.FromBytes(data).String()
		object := registryObject{data, q.Header.Get("Content-Type")}
		var blobs, manifests []v1.Descriptor
		switch object.MediaType {
		case v1.MediaTypeImageManifest, "application/vnd.docker.distribution.manifest.v2+json":
			var manifest v1.Manifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				http.Error(w, "invalid manifest", http.StatusBadRequest)
				return
			}
			blobs = append([]v1.Descriptor{manifest.Config}, manifest.Layers...)
		case v1.MediaTypeImageIndex, "application/vnd.docker.distribution.manifest.list.v2+json":
			var index v1.Index
			if err := json.Unmarshal(data, &index); err != nil {
				http.Error(w, "invalid index", http.StatusBadRequest)
				return
			}
			manifests = index.Manifests
		default:
			http.Error(w, "unsupported manifest media type", http.StatusUnsupportedMediaType)
			return
		}
		r.mu.Lock()
		for role, descriptors := range map[string][]v1.Descriptor{"blobs": blobs, "manifests": manifests} {
			for _, descriptor := range descriptors {
				child, exists := r.objects[repo+"/"+role+"/"+descriptor.Digest.String()]
				if !exists || int64(len(child.Data)) != descriptor.Size || digest.FromBytes(child.Data) != descriptor.Digest {
					r.mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": "MANIFEST_BLOB_UNKNOWN", "message": "missing " + role + " child", "detail": descriptor.Digest.String()}}})
					return
				}
			}
		}
		r.objects[key] = object
		r.objects[repo+"/manifests/"+identity] = object
		r.mu.Unlock()
		w.Header().Set("Docker-Content-Digest", identity)
		w.WriteHeader(201)
		return
	}
	r.mu.Lock()
	object, ok := r.objects[key]
	r.mu.Unlock()
	if !ok {
		http.NotFound(w, q)
		return
	}
	w.Header().Set("Content-Type", object.MediaType)
	w.Header().Set("Docker-Content-Digest", digest.FromBytes(object.Data).String())
	w.Header().Set("Content-Length", strconv.Itoa(len(object.Data)))
	if q.Method != "HEAD" {
		_, _ = w.Write(object.Data)
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	server := registryFixture(t)
	reference := strings.TrimPrefix(server.URL, "http://") + "/workflows:v1"
	client := Client{PlainHTTP: true}
	data := []byte("version: 1\ntasks: {}\n")
	pinned, err := client.Push(context.Background(), reference, data, []string{"docker.io/library/alpine:3.22", "scratch"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := client.Pull(context.Background(), pinned)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, document.Data) || document.Reference != pinned {
		t.Fatalf("unexpected document: %#v", document)
	}
}

func TestFetchRejectsInvalidContent(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	data := []byte("workflow")
	descriptor := content.NewDescriptorFromBytes(LayerType, data)
	if err := store.Push(ctx, descriptor, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := fetch(ctx, store, descriptor, 2); err == nil {
		t.Fatal("accepted oversized payload")
	}
	descriptor.Size++
	if _, err := fetch(ctx, store, descriptor, MaxBytes); err == nil {
		t.Fatal("accepted descriptor with incorrect size")
	}
}

func TestDeliveryMappingsRejectAmbiguity(t *testing.T) {
	subject := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, []byte("manifest"))
	pinned := "registry.test/workflows@" + subject.Digest.String()
	records := []bale.InspectedDestination{{Local: true, Subject: subject, Record: bale.DestinationRecord{Destination: pinned, Resolutions: []bale.DestinationResolution{{Dependencies: []bale.DestinationDependency{{Ref: "source.test/image:v1", Source: "source.test/image@sha256:abc", Destination: "dest.test/image@sha256:abc"}}}}}}}
	mappings, err := deliveryMappings(records, pinned, subject)
	if err != nil || mappings["source.test/image:v1"] != "dest.test/image@sha256:abc" {
		t.Fatalf("mappings=%v err=%v", mappings, err)
	}
	records[0].Record.Resolutions = append(records[0].Record.Resolutions, bale.DestinationResolution{Dependencies: []bale.DestinationDependency{{Ref: "source.test/image:v1", Destination: "another.test/image@sha256:abc"}}})
	if _, err = deliveryMappings(records, pinned, subject); err == nil {
		t.Fatal("accepted conflicting destinations")
	}
}

func TestBaleOfflineNestedRelocation(t *testing.T) {
	ctx := context.Background()
	source := registryFixture(t)
	destination := registryFixture(t)
	sourceHost := strings.TrimPrefix(source.URL, "http://")
	destinationHost := strings.TrimPrefix(destination.URL, "http://")
	client := Client{PlainHTTP: true}
	leaf, err := client.Push(ctx, sourceHost+"/leaf:v1", []byte("version: 1\nassets: [scripts/check.sh]\ntasks: {run: {image: scratch}}\n"), nil, map[string]assets.File{"scripts/check.sh": {Data: []byte("echo portable\n"), Executable: true}})
	if err != nil {
		t.Fatal(err)
	}
	middleData := []byte("version: 1\nimports: {leaf: " + leaf + "}\ntasks: {run: {uses: leaf/run}}\n")
	middle, err := client.Push(ctx, sourceHost+"/middle:v1", middleData, []string{leaf}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rootData := []byte("version: 1\nimports: {middle: " + middle + "}\ntasks: {run: {uses: middle/run}}\n")
	root, err := client.Push(ctx, sourceHost+"/root:v1", rootData, []string{middle}, nil)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{sourceHost, destinationHost}})
	if err != nil {
		t.Fatal(err)
	}
	recipe, err := bale.ParseRecipe([]byte("include:\n  - " + root + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(t.TempDir(), "bundle")
	if _, err = transport.CreateLayout(ctx, recipe, layout, bale.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// All subsequent operations must work using captured bytes and the receiving
	// registry. Source coordinates deliberately become unavailable.
	source.Close()
	plan, err := transport.PlanPush(ctx, layout, destinationHost+"/delivery:complete", bale.PushOptions{RepositoryMap: map[string]string{sourceHost + "/root": "root", sourceHost + "/middle": "middle", sourceHost + "/leaf": "leaf"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = transport.Push(ctx, plan); err != nil {
		t.Fatal(err)
	}
	_, rootDigest, _ := strings.Cut(root, "@")
	deliveredRoot := destinationHost + "/delivery/root@" + rootDigest
	rootDocument, err := client.Pull(ctx, deliveredRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rootDocument.Data, rootData) {
		t.Fatal("root payload changed during delivery")
	}
	deliveredMiddle := rootDocument.Dependencies[middle]
	if deliveredMiddle == "" || strings.HasPrefix(deliveredMiddle, sourceHost) {
		t.Fatalf("missing relocated middle: %v", rootDocument.Dependencies)
	}
	middleDocument, err := client.Pull(ctx, deliveredMiddle)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(middleDocument.Data, middleData) {
		t.Fatal("middle payload changed during delivery")
	}
	deliveredLeaf := middleDocument.Dependencies[leaf]
	if deliveredLeaf == "" || strings.HasPrefix(deliveredLeaf, sourceHost) {
		t.Fatalf("missing relocated leaf: %v", middleDocument.Dependencies)
	}
	leafDocument, err := client.Pull(ctx, deliveredLeaf)
	if err != nil {
		t.Fatal(err)
	}
	if !leafDocument.Files["scripts/check.sh"].Executable || string(leafDocument.Files["scripts/check.sh"].Data) != "echo portable\n" {
		t.Fatal("packaged file lost during offline relocation")
	}
}

func TestInvalidDependencyHasNoRegistrySideEffects(t *testing.T) {
	for _, dependency := range []string{"busybox:1.37", "docker.io/library/busybox", "team/busybox:1.37"} {
		t.Run(dependency, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				http.Error(w, "unexpected network access", 500)
			}))
			defer server.Close()
			_, err := (Client{PlainHTTP: true}).Push(context.Background(), strings.TrimPrefix(server.URL, "http://")+"/workflows:existing", []byte("version: 1\n"), []string{dependency}, nil)
			if err == nil || !strings.Contains(err.Error(), "before publication") {
				t.Fatalf("expected explicit preflight failure, got %v", err)
			}
			if requests != 0 {
				t.Fatalf("invalid dependency contacted registry %d times", requests)
			}
		})
	}
}

func TestPullRejectsMalformedManifestEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		schema                       int
		manifestType, descriptorType string
	}{
		{"missing schema version", 0, v1.MediaTypeImageManifest, v1.MediaTypeImageManifest},
		{"unsupported schema version", 1, v1.MediaTypeImageManifest, v1.MediaTypeImageManifest},
		{"wrong manifest type", 2, v1.MediaTypeImageIndex, v1.MediaTypeImageManifest},
		{"wrong descriptor type", 2, v1.MediaTypeImageManifest, v1.MediaTypeImageIndex},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := content.NewDescriptorFromBytes(v1.MediaTypeEmptyJSON, []byte("{}"))
			layer := content.NewDescriptorFromBytes(LayerType, []byte("version: 1\n"))
			data, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: tc.schema}, MediaType: tc.manifestType, ArtifactType: Type, Config: config, Layers: []v1.Descriptor{layer}})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.descriptorType)
				w.Header().Set("Docker-Content-Digest", digest.FromBytes(data).String())
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				if r.Method != "HEAD" {
					_, _ = w.Write(data)
				}
			}))
			defer server.Close()
			_, err = (Client{PlainHTTP: true}).Pull(context.Background(), strings.TrimPrefix(server.URL, "http://")+"/workflows:v1")
			if err == nil {
				t.Fatal("accepted malformed manifest envelope")
			}
		})
	}
}

type fetchCloseReader struct {
	*bytes.Reader
	closeErr error
	closed   bool
}

func (r *fetchCloseReader) Close() error { r.closed = true; return r.closeErr }

type fetchReaderStore struct {
	content.ReadOnlyStorage
	reader io.ReadCloser
}

func (s fetchReaderStore) Fetch(context.Context, v1.Descriptor) (io.ReadCloser, error) {
	return s.reader, nil
}

func TestFetchPreservesCloseErrorsAndDescriptorReadBounds(t *testing.T) {
	payload := []byte("workflow")
	descriptor := content.NewDescriptorFromBytes(LayerType, payload)
	closeErr := errors.New("close failed")
	for _, trailing := range []bool{false, true} {
		data := bytes.Clone(payload)
		if trailing {
			data = append(data, bytes.Repeat([]byte("x"), 64)...)
		}
		reader := &fetchCloseReader{Reader: bytes.NewReader(data), closeErr: closeErr}
		result, err := fetch(t.Context(), fetchReaderStore{reader: reader}, descriptor, MaxBytes)
		if result != nil || !errors.Is(err, closeErr) || !reader.closed {
			t.Fatalf("close failure lost: %q %v closed=%v", result, err, reader.closed)
		}
		if trailing && len(data)-reader.Len() > len(payload)+1 {
			t.Fatal("fetch read past descriptor overflow probe")
		}
	}
}
