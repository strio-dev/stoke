package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/strio-dev/stoke/internal/hostfiles"
)

func TestHostRegistryCacheRestoresBoundInputKeyAndRejectsWrongLookup(t *testing.T) {
	server := registryFixture(t)
	repository := strings.TrimPrefix(server.URL, "http://") + "/cache"
	client := Client{PlainHTTP: true}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "value"), []byte("captured exact bytes"), 0750); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("a", 64)
	layout := filepath.Join(t.TempDir(), "layout")
	identity, err := hostfiles.CaptureWithKey(t.Context(), workspace, []string{"value"}, layout, "linux/amd64", key)
	if err != nil {
		t.Fatal(err)
	}
	// Concurrent independent writers of the same declared result are safe.
	var writers sync.WaitGroup
	for range 2 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			if err := client.PublishHostCache(t.Context(), repository, key, layout, identity); err != nil {
				t.Error(err)
			}
		}()
	}
	writers.Wait()
	fetched := filepath.Join(t.TempDir(), "fresh")
	got, hit, err := client.FetchHostCache(t.Context(), repository, key, "linux/amd64", fetched)
	if err != nil || !hit || got != identity {
		t.Fatalf("cache: %s %t %v", got, hit, err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := hostfiles.Restore(t.Context(), fetched, got, restored); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(restored, "value"))
	if err != nil || string(data) != "captured exact bytes" {
		t.Fatal(string(data), err)
	}
	missing := strings.Repeat("b", 64)
	if _, hit, err := client.FetchHostCache(t.Context(), repository, missing, "linux/amd64", filepath.Join(t.TempDir(), "missing")); err != nil || hit {
		t.Fatal("miss", hit, err)
	}
	repo, err := client.ciRepository(repository)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := repo.Resolve(t.Context(), identity)
	if err != nil {
		t.Fatal(err)
	}
	tag, _ := hostCacheTag(missing)
	if err := repo.Tag(t.Context(), descriptor, tag); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.FetchHostCache(t.Context(), repository, missing, "linux/amd64", filepath.Join(t.TempDir(), "wrong")); err == nil {
		t.Fatal("trusted lookup tag instead of input binding")
	}
	if _, _, err := client.FetchHostCache(t.Context(), repository, key, "linux/arm64", filepath.Join(t.TempDir(), "platform")); err == nil {
		t.Fatal("accepted foreign platform")
	}
}
