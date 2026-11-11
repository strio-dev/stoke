package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	cooprwire "github.com/strio-dev/coopr/api"
)

func TestRegistryCacheRepositoriesRequireBareExplicitNames(t *testing.T) {
	for _, repository := range []string{"ghcr.io/team/cache", "registry.example:443/cache", "localhost/cache", "127.0.0.1:5000/team/cache", "[::1]:5000/team/cache"} {
		if err := ValidateCacheRepository(repository); err != nil {
			t.Errorf("valid cache %q rejected: %v", repository, err)
		}
	}
	for _, repository := range []string{"", "cache", "team/cache", "./cache", "../cache", "registry.example/cache:tag", "registry.example/cache:", "registry.example/cache@sha256:" + strings.Repeat("a", 64), "registry.example/cache@", "https://registry.example/cache", "docker://registry.example/cache", "oci:/tmp/cache", "oci-layout:/tmp/cache", "dir:/tmp/cache", "registry.example:/cache", "type=registry,ref=registry.example/cache", "registry.example//cache", "registry.example/UPPER", "registry.example/cache ", "registry.example/cache\x00", string([]byte{0xff}) + "/cache", "user:password@registry.example/cache"} {
		if err := ValidateCacheRepository(repository); err == nil {
			t.Errorf("invalid cache %q accepted", repository)
		}
	}
}

func TestRegistryCacheWirePreservesReadWriteDirections(t *testing.T) {
	var requests []cooprwire.Request
	client := socketClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request cooprwire.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		requests = append(requests, request)
		_ = json.NewEncoder(w).Encode(cooprwire.Event{Type: "result", Result: &cooprwire.Result{ImageID: "image"}})
	}))
	cache := CacheOptions{LocalDir: "/cache", NoCache: true, RegistryFrom: []string{"registry.example/read-one", "registry.example/read-two"}, RegistryTo: []string{"registry.example/write"}}
	options := Options{Cache: cache}
	if _, err := client.Execute(context.Background(), Program{}, options); err != nil {
		t.Fatal(err)
	}
	for _, component := range []bool{false, true} {
		if _, err := client.Build(context.Background(), cooprwire.Build{Files: []string{"/project/image.coopr"}}, component, options); err != nil {
			t.Fatal(err)
		}
	}
	for index, kind := range []string{"execute", "build", "component"} {
		if requests[index].Kind != kind || !reflect.DeepEqual(requests[index].Options.Cache, cache) {
			t.Fatalf("%s lost cache directions: %#v", kind, requests[index])
		}
	}
}

func TestInvalidRegistryCacheDoesNotMakeClientRequests(t *testing.T) {
	var requests atomic.Int64
	client := rawSocketClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	for _, cache := range []CacheOptions{{RegistryFrom: []string{"registry.example/cache:tag"}}, {RegistryTo: []string{"type=registry,ref=registry.example/cache"}}} {
		if _, err := client.Execute(context.Background(), Program{}, Options{Cache: cache}); err == nil {
			t.Fatal("invalid registry cache reached execution")
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid cache made %d HTTP requests", requests.Load())
	}
}
