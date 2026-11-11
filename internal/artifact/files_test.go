package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/stoke/internal/assets"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

type observedStore struct {
	*memory.Store
	fetches int
}

type corruptStore struct{ *memory.Store }

func (s corruptStore) Fetch(context.Context, v1.Descriptor) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("corrupt")), nil
}

func (s *observedStore) Fetch(ctx context.Context, descriptor v1.Descriptor) (io.ReadCloser, error) {
	s.fetches++
	return s.Store.Fetch(ctx, descriptor)
}

func TestFilesRegistryRoundTripIsDeterministic(t *testing.T) {
	server := registryFixture(t)
	reference := strings.TrimPrefix(server.URL, "http://") + "/files:v1"
	client := Client{PlainHTTP: true}
	data := []byte("version: 1\nassets: [scripts/check.sh,templates/data,empty,'templates/white space ☃']\ntasks: {}\n")
	files := map[string]assets.File{"scripts/check.sh": {Data: []byte("echo same\n"), Executable: true}, "templates/data": {Data: []byte("echo same\n")}, "empty": {}, "templates/white space ☃": {Data: []byte("Unicode ☃\n")}}
	pinned, err := client.Push(context.Background(), reference, data, nil, files)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Push(context.Background(), reference, data, nil, map[string]assets.File{"templates/white space ☃": files["templates/white space ☃"], "empty": {}, "templates/data": files["templates/data"], "scripts/check.sh": files["scripts/check.sh"]})
	if err != nil || second != pinned {
		t.Fatalf("unstable package: %s %s %v", pinned, second, err)
	}
	document, err := client.Pull(context.Background(), pinned)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(document.Data, data) || len(document.Files) != len(files) {
		t.Fatalf("document %#v", document)
	}
	for name, want := range files {
		got := document.Files[name]
		if !bytes.Equal(got.Data, want.Data) || got.Executable != want.Executable {
			t.Fatalf("file %q: %#v", name, got)
		}
	}
}

func fileDescriptor(name string, data []byte, executable bool) v1.Descriptor {
	d := content.NewDescriptorFromBytes(FileType, data)
	mode := "0644"
	if executable {
		mode = "0755"
	}
	d.Annotations = map[string]string{v1.AnnotationTitle: name, FileModeAnnotation: mode}
	return d
}

func TestFileLayersRejectMalformedMetadataBeforeFetch(t *testing.T) {
	descriptor := fileDescriptor("scripts/check.sh", []byte("echo ok\n"), true)
	for _, tc := range []struct {
		name string
		edit func([]v1.Descriptor) []v1.Descriptor
	}{
		{"traversal", func(d []v1.Descriptor) []v1.Descriptor { d[0].Annotations[v1.AnnotationTitle] = "../outside"; return d }},
		{"absolute", func(d []v1.Descriptor) []v1.Descriptor { d[0].Annotations[v1.AnnotationTitle] = "/outside"; return d }},
		{"reserved", func(d []v1.Descriptor) []v1.Descriptor { d[0].Annotations[v1.AnnotationTitle] = "stoke.yaml"; return d }},
		{"unknown media", func(d []v1.Descriptor) []v1.Descriptor { d[0].MediaType = "application/not-stoke"; return d }},
		{"second yaml", func(d []v1.Descriptor) []v1.Descriptor { d[0].MediaType = LayerType; return d }},
		{"missing title", func(d []v1.Descriptor) []v1.Descriptor { delete(d[0].Annotations, v1.AnnotationTitle); return d }},
		{"missing mode", func(d []v1.Descriptor) []v1.Descriptor { delete(d[0].Annotations, FileModeAnnotation); return d }},
		{"setuid mode", func(d []v1.Descriptor) []v1.Descriptor { d[0].Annotations[FileModeAnnotation] = "4755"; return d }},
		{"duplicate path", func(d []v1.Descriptor) []v1.Descriptor { return append(d, d[0]) }},
		{"prefix conflict", func(d []v1.Descriptor) []v1.Descriptor { return append(d, fileDescriptor("scripts", nil, false)) }},
		{"negative size", func(d []v1.Descriptor) []v1.Descriptor { d[0].Size = -1; return d }},
		{"file size", func(d []v1.Descriptor) []v1.Descriptor { d[0].Size = assets.MaxFileBytes + 1; return d }},
		{"total size", func(d []v1.Descriptor) []v1.Descriptor {
			result := []v1.Descriptor{}
			for _, name := range []string{"a", "b", "c", "d", "e"} {
				item := fileDescriptor(name, nil, false)
				item.Size = assets.MaxFileBytes
				result = append(result, item)
			}
			return result
		}},
		{"file count", func(d []v1.Descriptor) []v1.Descriptor { return make([]v1.Descriptor, assets.MaxFiles+1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloned := descriptor
			cloned.Annotations = map[string]string{v1.AnnotationTitle: "scripts/check.sh", FileModeAnnotation: "0755"}
			store := &observedStore{Store: memory.New()}
			if _, err := readFiles(context.Background(), store, tc.edit([]v1.Descriptor{cloned})); err == nil {
				t.Fatal("accepted malformed files")
			}
			if store.fetches != 0 {
				t.Fatal("malformed metadata fetched file content")
			}
		})
	}
}

func TestFileLayersVerifyContentBeforeReturningPackage(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	data := []byte("payload")
	descriptor := fileDescriptor("data", data, false)
	if err := store.Push(ctx, descriptor, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if files, err := readFiles(ctx, corruptStore{store}, []v1.Descriptor{descriptor}); err == nil || files != nil {
		t.Fatalf("same-size corrupted bytes returned: %v %v", files, err)
	}
	for _, edit := range []func(*v1.Descriptor){func(d *v1.Descriptor) { d.Size++ }, func(d *v1.Descriptor) { d.Digest = digest.Digest("sha256:" + strings.Repeat("0", 64)) }} {
		changed := descriptor
		edit(&changed)
		files, err := readFiles(ctx, store, []v1.Descriptor{changed})
		if err == nil || files != nil {
			t.Fatalf("invalid content returned: %v %v", files, err)
		}
	}
}

func TestInvalidFilePackageDoesNotContactRegistry(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; http.Error(w, "unexpected request", 500) }))
	defer server.Close()
	for _, files := range []map[string]assets.File{{"../bad": {}}, {"bad": {Data: make([]byte, assets.MaxFileBytes+1)}}, {"a": {}, "a/b": {}}, {string([]byte{'f', 0xff}): {}}} {
		_, err := (Client{PlainHTTP: true}).Push(context.Background(), strings.TrimPrefix(server.URL, "http://")+"/package:v1", []byte("version: 1\n"), nil, files)
		if err == nil {
			t.Fatal("accepted invalid assets")
		}
	}
	if requests != 0 {
		t.Fatalf("invalid package contacted registry %d times", requests)
	}
}

func TestPullRejectsFileEnvelopeBeforeReturningAnyPayload(t *testing.T) {
	server := registryFixture(t)
	reference := strings.TrimPrefix(server.URL, "http://") + "/files:v1"
	client := Client{PlainHTTP: true}
	data := []byte("version: 1\nassets: [good]\n")
	pinned, err := client.Push(context.Background(), reference, data, nil, map[string]assets.File{"good": {Data: []byte("ok")}})
	if err != nil {
		t.Fatal(err)
	}
	repo, _, _, err := client.clients(pinned)
	if err != nil {
		t.Fatal(err)
	}
	// A second YAML layer violates the single-definition envelope, even when bytes
	// are valid and the first file layer could otherwise have been returned.
	descriptor, err := repo.Resolve(context.Background(), strings.Split(pinned, "@")[1])
	if err != nil {
		t.Fatal(err)
	}
	raw, err := fetch(context.Background(), repo, descriptor, MaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Layers = append(manifest.Layers, manifest.Layers[0])
	changed, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	target := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, changed)
	if err := repo.PushReference(context.Background(), target, bytes.NewReader(changed), "malformed"); err != nil {
		t.Fatal(err)
	}
	doc, err := client.Pull(context.Background(), strings.TrimPrefix(server.URL, "http://")+"/files:malformed")
	if err == nil || doc != nil {
		t.Fatalf("partially returned invalid package: %#v %v", doc, err)
	}
}
