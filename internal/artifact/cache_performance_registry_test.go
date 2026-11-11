package artifact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

type cachePerfRequest struct {
	Method       string  `json:"method"`
	Path         string  `json:"path"`
	Repository   string  `json:"repository"`
	Digest       string  `json:"digest,omitempty"`
	Status       int     `json:"status"`
	ReadBytes    int64   `json:"readBytes"`
	WrittenBytes int64   `json:"writtenBytes"`
	Seconds      float64 `json:"seconds"`
}
type cachePerfRegistry struct {
	*registryStorage
	events []cachePerfRequest
	server *httptest.Server
}
type cachePerfReader struct {
	io.ReadCloser
	n int64
}

func (r *cachePerfReader) Read(p []byte) (int, error) {
	n, e := r.ReadCloser.Read(p)
	r.n += int64(n)
	return n, e
}

type cachePerfWriter struct {
	http.ResponseWriter
	status int
	n      int64
}

func (w *cachePerfWriter) WriteHeader(s int) {
	if w.status == 0 {
		w.status = s
		w.ResponseWriter.WriteHeader(s)
	}
}
func (w *cachePerfWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	n, e := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, e
}
func newCachePerfRegistry(objects map[string]registryObject) *cachePerfRegistry {
	if objects == nil {
		objects = map[string]registryObject{}
	}
	reg := &cachePerfRegistry{registryStorage: &registryStorage{objects: objects, uploads: map[string][]byte{}}}
	reg.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		body := &cachePerfReader{ReadCloser: r.Body}
		r.Body = body
		out := &cachePerfWriter{ResponseWriter: w}
		repo, path := registryFixturePath(r.URL.Path)
		reg.serve(out, r, repo, path)
		status := out.status
		if status == 0 {
			status = 200
		}
		identity := ""
		if strings.HasPrefix(path, "blobs/sha256:") {
			identity = strings.TrimPrefix(path, "blobs/")
		}
		if strings.HasPrefix(path, "manifests/sha256:") {
			identity = strings.TrimPrefix(path, "manifests/")
		}
		if r.URL.Query().Get("digest") != "" {
			identity = r.URL.Query().Get("digest")
		}
		event := cachePerfRequest{r.Method, r.URL.Path, repo, identity, status, body.n, out.n, time.Since(started).Seconds()}
		reg.mu.Lock()
		reg.events = append(reg.events, event)
		reg.mu.Unlock()
	}))
	return reg
}
func (r *cachePerfRegistry) closeEvents() []cachePerfRequest {
	r.server.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]cachePerfRequest(nil), r.events...)
}

type cachePerfSnapshotEntry struct {
	Key       string `json:"key"`
	File      string `json:"file"`
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType"`
}

func (r *cachePerfRegistry) saveSnapshot(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	r.mu.Lock()
	objects := make(map[string]registryObject, len(r.objects))
	for k, v := range r.objects {
		objects[k] = v
	}
	r.mu.Unlock()
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := []cachePerfSnapshotEntry{}
	for _, key := range keys {
		o := objects[key]
		file := digest.FromString(key).Encoded()
		if err := os.WriteFile(filepath.Join(dir, file), o.Data, 0600); err != nil {
			return err
		}
		entries = append(entries, cachePerfSnapshotEntry{key, file, digest.FromBytes(o.Data).String(), o.MediaType})
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "index.json"), data, 0600)
}
func loadCachePerfSnapshot(dir string) (map[string]registryObject, error) {
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil, err
	}
	var entries []cachePerfSnapshotEntry
	if err = json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	objects := map[string]registryObject{}
	for _, entry := range entries {
		if filepath.Base(entry.File) != entry.File {
			return nil, fmt.Errorf("invalid snapshot filename %q", entry.File)
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.File))
		if err != nil {
			return nil, err
		}
		if digest.FromBytes(data).String() != entry.Digest {
			return nil, fmt.Errorf("snapshot digest mismatch %q", entry.Key)
		}
		objects[entry.Key] = registryObject{data, entry.MediaType}
	}
	return objects, nil
}

func TestCachePerformanceRegistryCountersAndSnapshot(t *testing.T) {
	registry := newCachePerfRegistry(nil)
	payload := []byte("actual-response-body")
	identity := digest.FromBytes(payload).String()
	send := func(method, path string, body []byte) {
		t.Helper()
		request, err := http.NewRequest(method, registry.server.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if method == "GET" && !bytes.Equal(data, payload) {
			t.Fatalf("body = %q", data)
		}
	}
	send("POST", "/v2/cache/source/blobs/uploads/", nil)
	send("PUT", "/v2/cache/source/blobs/uploads/1?digest="+identity, payload)
	send("HEAD", "/v2/cache/source/blobs/"+identity, nil)
	send("GET", "/v2/cache/source/blobs/"+identity, nil)
	events := registry.closeEvents()
	if len(events) != 4 || events[1].ReadBytes != int64(len(payload)) || events[2].WrittenBytes != 0 || events[3].WrittenBytes != int64(len(payload)) {
		t.Fatalf("actual byte counters: %+v", events)
	}
	directory := t.TempDir()
	if err := registry.saveSnapshot(directory); err != nil {
		t.Fatal(err)
	}
	objects, err := loadCachePerfSnapshot(directory)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(objects["cache/source/blobs/"+identity].Data, payload) {
		t.Fatal("snapshot changed blob bytes")
	}
	if err := os.WriteFile(filepath.Join(directory, digest.FromString("cache/source/blobs/"+identity).Encoded()), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCachePerfSnapshot(directory); err == nil {
		t.Fatal("accepted corrupt frozen registry seed")
	}
}
