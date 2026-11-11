package artifact

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
)

// Preflight cases model malformed content already retained by a registry.
// Inject it directly rather than relaxing the fixture's publication checks.
func ciStoreTestObject(t *testing.T, storage *registryStorage, repository, media string, data []byte) v1.Descriptor {
	t.Helper()
	descriptor := content.NewDescriptorFromBytes(media, data)
	role := "blobs"
	if media == v1.MediaTypeImageManifest || media == v1.MediaTypeImageIndex {
		role = "manifests"
	}
	storage.mu.Lock()
	storage.objects[strings.SplitN(repository, "/", 2)[1]+"/"+role+"/"+descriptor.Digest.String()] = registryObject{Data: data, MediaType: media}
	storage.mu.Unlock()
	return descriptor
}
func ciRawResult(t *testing.T, storage *registryStorage, repository string, layers []v1.Descriptor, subject *v1.Descriptor, component bool, archive *v1.Descriptor) string {
	t.Helper()
	diffs := make([]digest.Digest, len(layers))
	for i, layer := range layers {
		diffs[i] = layer.Digest
	}
	configData, err := json.Marshal(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, RootFS: v1.RootFS{Type: "layers", DiffIDs: diffs}})
	if err != nil {
		t.Fatal(err)
	}
	configType, artifactType := v1.MediaTypeImageConfig, ""
	result := ciTestResult(t, "build", "linux/amd64")
	if component {
		result.Kind = "component"
		configType, artifactType = ciComponentConfigType, ciComponentType
		configData = []byte(`{"platform":{"os":"linux","architecture":"amd64"}}`)
	}
	config := ciStoreTestObject(t, storage, repository, configType, configData)
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: artifactType, Config: config, Layers: layers, Subject: subject})
	if err != nil {
		t.Fatal(err)
	}
	output := ciStoreTestObject(t, storage, repository, v1.MediaTypeImageManifest, raw)
	result.Output = output
	result.Archive = archive
	raw, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	config = ciStoreTestObject(t, storage, repository, ciResultConfigType, raw)
	metadataLayers := []v1.Descriptor{}
	if archive != nil {
		metadataLayers = append(metadataLayers, *archive)
	}
	raw, err = json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: CIResultType, Config: config, Layers: metadataLayers})
	if err != nil {
		t.Fatal(err)
	}
	metadata := ciStoreTestObject(t, storage, repository, v1.MediaTypeImageManifest, raw)
	raw, err = json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, ArtifactType: CIResultType, Manifests: []v1.Descriptor{metadata, output}})
	if err != nil {
		t.Fatal(err)
	}
	root := ciStoreTestObject(t, storage, repository, v1.MediaTypeImageIndex, raw)
	return repository + "@" + root.Digest.String()
}

func TestCIPayloadPreflightRejectsOversizeBeforeBlobDownloads(t *testing.T) {
	for _, scenario := range []string{"aggregate", "archive", "depth", "nodes", "conflicting-size", "conflicting-type", "metadata"} {
		t.Run(scenario, func(t *testing.T) {
			base, storage := registryFixtureStorage(t, false)
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
			leaf := v1.Descriptor{MediaType: v1.MediaTypeImageLayer, Digest: digest.FromString("not-downloaded"), Size: 1}
			layers := []v1.Descriptor{}
			var subject, archive *v1.Descriptor
			component := false
			var neverRead []digest.Digest
			switch scenario {
			case "aggregate":
				for i := 0; i < 3; i++ {
					d := leaf
					d.Digest = digest.FromString(fmt.Sprint(i))
					d.Size = MaxCITransferBytes / 2
					layers = append(layers, d)
					neverRead = append(neverRead, d.Digest)
				}
			case "archive":
				component = true
				value := v1.Descriptor{MediaType: CIComponentArchiveType, Digest: leaf.Digest, Size: MaxCITransferBytes + 1}
				archive = &value
				neverRead = append(neverRead, value.Digest)
			case "depth":
				next := leaf
				for i := 0; i < MaxCITransferDepth+1; i++ {
					raw, _ := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{next}})
					next = ciStoreTestObject(t, storage, repository, v1.MediaTypeImageIndex, raw)
				}
				subject = &next
				neverRead = append(neverRead, leaf.Digest)
			case "nodes":
				nodes := make([]v1.Descriptor, MaxCITransferNodes)
				for i := range nodes {
					nodes[i] = leaf
					nodes[i].Digest = digest.FromString(fmt.Sprintf("leaf-%d", i))
					neverRead = append(neverRead, nodes[i].Digest)
				}
				raw, _ := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: nodes})
				next := ciStoreTestObject(t, storage, repository, v1.MediaTypeImageIndex, raw)
				subject = &next
			case "conflicting-size":
				a, b := leaf, leaf
				b.Size = 2
				layers = []v1.Descriptor{a, b}
				neverRead = append(neverRead, leaf.Digest)
			case "conflicting-type":
				a, b := leaf, leaf
				b.MediaType = v1.MediaTypeImageLayerGzip
				layers = []v1.Descriptor{a, b}
				neverRead = append(neverRead, leaf.Digest)
			case "metadata":
				next := v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: leaf.Digest, Size: maxCIPayloadDocumentBytes + 1}
				subject = &next
				neverRead = append(neverRead, leaf.Digest)
			}
			ref := ciRawResult(t, storage, repository, layers, subject, component, archive)
			mu.Lock()
			clear(reads)
			mu.Unlock()
			parent := t.TempDir()
			if _, err := client.FetchCIPayload(t.Context(), repository, ref, CIResultExpectation{Scope: ciTestScope(), Producer: "build", MaxAttempt: 1}, filepath.Join(parent, "payload")); err == nil {
				t.Fatal("oversized/conflicting/deep graph accepted")
			}
			mu.Lock()
			for _, value := range neverRead {
				if reads["/v2/results/blobs/"+value.String()] != 0 || reads["/v2/results/manifests/"+value.String()] != 0 {
					t.Errorf("payload/oversize metadata body was downloaded: %s", value)
				}
			}
			mu.Unlock()
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatal("preflight left payload staging", entries, err)
			}
		})
	}
}

func TestCITransferBudgetUniqueNodesAndBoundedRuntimeReads(t *testing.T) {
	ctx := t.Context()
	value := []byte("payload")
	leaf := content.NewDescriptorFromBytes(v1.MediaTypeImageLayer, value)
	store := &ciObservedSource{blobs: map[digest.Digest][]byte{leaf.Digest: value}}
	raw, _ := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{leaf, leaf}})
	root := content.NewDescriptorFromBytes(v1.MediaTypeImageIndex, raw)
	store.blobs[root.Digest] = raw
	store.root = root
	source, err := preflightCIPayload(ctx, store, root, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if source.encodedBytes != root.Size+leaf.Size || len(source.allowed) != 2 {
		t.Fatal("duplicate digest counted twice")
	}
	if store.reads[leaf.Digest] != 0 {
		t.Fatal("preflight read layer body")
	}
	budget := &ciStreamBudget{remaining: 5}
	first := &ciBoundedStream{ctx: ctx, reader: io.NopCloser(strings.NewReader("abcdef")), remaining: 3, budget: budget}
	data, err := io.ReadAll(first)
	if err != nil || string(data) != "abc" {
		t.Fatal("descriptor read overflow", string(data), err)
	}
	second := &ciBoundedStream{ctx: ctx, reader: io.NopCloser(strings.NewReader("xyz")), remaining: 3, budget: budget}
	data, err = io.ReadAll(second)
	if err == nil || string(data) != "xy" {
		t.Fatal("aggregate read overflow", string(data), err)
	}
}

type ciObservedSource struct {
	blobs map[digest.Digest][]byte
	root  v1.Descriptor
	reads map[digest.Digest]int
}

func (s *ciObservedSource) Resolve(context.Context, string) (v1.Descriptor, error) {
	return s.root, nil
}
func (s *ciObservedSource) Exists(_ context.Context, d v1.Descriptor) (bool, error) {
	_, ok := s.blobs[d.Digest]
	return ok, nil
}
func (s *ciObservedSource) Fetch(_ context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	if s.reads == nil {
		s.reads = map[digest.Digest]int{}
	}
	s.reads[d.Digest]++
	data, ok := s.blobs[d.Digest]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func TestCIComponentOversizeRejectedBeforeIndexHashOrRegistry(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "oversized.tar")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxCITransferBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	identity := ciTestResult(t, "component", "linux/amd64")
	identity.Kind = "component"
	if _, err := (Client{PlainHTTP: true}).PublishCIResult(t.Context(), strings.TrimPrefix(server.URL, "http://")+"/results", identity, path, digest.FromString("root").String()); err == nil {
		t.Fatal("oversized sparse archive accepted")
	}
	if requests != 0 {
		t.Fatal("oversized local archive reached registry")
	}
	ctx, cancel := context.WithCancel(t.Context())
	reader := &ciCancellingReader{cancel: cancel}
	_, err = io.Copy(io.Discard, ciContextReader{ctx: ctx, reader: reader})
	if !errors.Is(err, context.Canceled) || reader.reads != 1 {
		t.Fatal("local hashing ignored cancellation", err, reader.reads)
	}
}

type ciCancellingReader struct {
	cancel context.CancelFunc
	reads  int
}

func (r *ciCancellingReader) Read(p []byte) (int, error) {
	r.reads++
	r.cancel()
	return copy(p, "first-block"), nil
}

func TestCIArchiveRejectsUnsafeAndUnboundedEntries(t *testing.T) {
	for _, name := range []string{"../index.json", "nested/../index.json", "/index.json", "./../index.json", "blobs/sha256/not-a-digest"} {
		t.Run(name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "bad.tar")
			file, err := os.Create(filename)
			if err != nil {
				t.Fatal(err)
			}
			archive := tar.NewWriter(file)
			if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := archive.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			if err := errors.Join(archive.Close(), file.Close()); err != nil {
				t.Fatal(err)
			}
			if _, err := newCIArchiveFS(t.Context(), filename); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

func TestCIArchiveAcceptsConventionalDirectories(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "directories.tar")
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewWriter(file)
	for _, name := range []string{"./", ".", "blobs/", "blobs/sha256/"} {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0700, Typeflag: tar.TypeDir}); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(archive.Close(), file.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err := newCIArchiveFS(t.Context(), filename); err != nil {
		t.Fatal(err)
	}
}
