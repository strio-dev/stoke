package artifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

func TestCICleanupDigestDeletionRemovesAliasesWithoutOtherWrites(t *testing.T) {
	base := registryFixtureManifestDeletion(t, true)
	var inspect atomic.Bool
	var mu sync.Mutex
	var writes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if inspect.Load() && r.Method != http.MethodGet && r.Method != http.MethodHead {
			mu.Lock()
			writes = append(writes, r.Method+" "+r.URL.Path)
			mu.Unlock()
		}
		base.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	repository := strings.TrimPrefix(server.URL, "http://") + "/results"
	client := Client{PlainHTTP: true}
	scope := ciTestScope()
	layout, payload := ciTestLayout(t, "image", "linux/amd64", "shared")
	ref, err := client.PublishCIResult(t.Context(), repository, ciTestResult(t, "build", "linux/amd64"), layout, payload)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := client.PublishCIResult(t.Context(), repository, ciTestResult(t, "retained", "linux/amd64"), layout, payload)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := client.ciRepository(repository)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := ciReference(repository, ref)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := repo.Resolve(t.Context(), selected)
	if err != nil {
		t.Fatal(err)
	}
	aliases := []string{"extra-alias", "another-alias"}
	for _, alias := range aliases {
		if err := repo.Tag(t.Context(), descriptor, alias); err != nil {
			t.Fatal(err)
		}
		if resolved, err := repo.Resolve(t.Context(), alias); err != nil || resolved.Digest != descriptor.Digest {
			t.Fatalf("fixture alias %q does not select the cleanup target: %v, %v", alias, resolved, err)
		}
	}
	anchors := ciCleanupPayloadTags(t, server.URL)
	inspect.Store(true)
	if err := client.DeleteCIObject(t.Context(), repository, ref, scope); err != nil {
		t.Fatal(err)
	}
	for _, name := range append(aliases, selected) {
		if _, err := repo.Resolve(t.Context(), name); !errors.Is(err, errdef.ErrNotFound) {
			t.Fatalf("deleted metadata alias %q remains or failed unexpectedly: %v", name, err)
		}
	}
	ciRequireCleanupPayload(t, client, repository, retained, scope, payload)
	if after := ciCleanupPayloadTags(t, server.URL); !slices.Equal(after, anchors) {
		t.Fatalf("metadata cleanup changed shared payload anchors: before=%v after=%v", anchors, after)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"DELETE /v2/results/manifests/" + selected}; !slices.Equal(writes, want) {
		t.Fatalf("cleanup performed additional writes, blob deletion or index mutation: got=%v want=%v", writes, want)
	}
}

func TestCICleanupRejectsUnverifiedMetadataBeforeAnyWrite(t *testing.T) {
	for _, mode := range []string{"contradictory HEAD digest", "corrupt manifest", "manifest subject", "wrong config type", "wrong scope", "payload root", "mutable tag", "other repository"} {
		t.Run(mode, func(t *testing.T) {
			base := registryFixtureManifestDeletion(t, true)
			var inspect atomic.Bool
			var writes atomic.Int32
			var injected atomic.Bool
			var target string
			var descriptor v1.Descriptor
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if inspect.Load() {
					if r.Method != http.MethodGet && r.Method != http.MethodHead {
						writes.Add(1)
					}
					if r.URL.Path == target {
						if mode == "contradictory HEAD digest" && r.Method == http.MethodHead {
							injected.Store(true)
							w.Header().Set("Content-Type", descriptor.MediaType)
							w.Header().Set("Content-Length", strconv.FormatInt(descriptor.Size, 10))
							w.Header().Set("Docker-Content-Digest", digest.FromString("different manifest").String())
							w.WriteHeader(http.StatusOK)
							return
						}
						if mode == "corrupt manifest" && r.Method == http.MethodGet {
							injected.Store(true)
							w.Header().Set("Content-Type", descriptor.MediaType)
							w.Header().Set("Docker-Content-Digest", descriptor.Digest.String())
							_, _ = w.Write([]byte("{}"))
							return
						}
					}
				}
				base.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			repository := strings.TrimPrefix(server.URL, "http://") + "/results"
			client := Client{PlainHTTP: true}
			scope := ciTestScope()
			layout, payload := ciTestLayout(t, "image", "linux/amd64", "preserved")
			ref, err := client.PublishCIResult(t.Context(), repository, ciTestResult(t, "build", "linux/amd64"), layout, payload)
			if err != nil {
				t.Fatal(err)
			}
			original := ref
			repo, err := client.ciRepository(repository)
			if err != nil {
				t.Fatal(err)
			}
			selected, err := ciReference(repository, ref)
			if err != nil {
				t.Fatal(err)
			}
			descriptor, err = repo.Resolve(t.Context(), selected)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "manifest subject" || mode == "wrong config type" {
				data, err := fetch(t.Context(), repo, descriptor, MaxCIMetadataBytes)
				if err != nil {
					t.Fatal(err)
				}
				var manifest v1.Manifest
				if err := json.Unmarshal(data, &manifest); err != nil {
					t.Fatal(err)
				}
				if mode == "manifest subject" {
					subject, err := repo.Resolve(t.Context(), payload)
					if err != nil {
						t.Fatal(err)
					}
					manifest.Subject = &subject
				} else {
					manifest.Config.MediaType = "application/vnd.example.invalid.config.v1+json"
				}
				data, err = json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				descriptor = content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, data)
				if err := repo.Push(t.Context(), descriptor, bytes.NewReader(data)); err != nil {
					t.Fatal(err)
				}
				ref = repository + "@" + descriptor.Digest.String()
			}
			target = "/v2/results/manifests/" + descriptor.Digest.String()
			switch mode {
			case "wrong scope":
				scope.Invocation = "another-invocation"
			case "payload root":
				ref = repository + "@" + payload
			case "mutable tag":
				if err := repo.Tag(t.Context(), descriptor, "mutable"); err != nil {
					t.Fatal(err)
				}
				if resolved, err := repo.Resolve(t.Context(), "mutable"); err != nil || resolved.Digest != descriptor.Digest {
					t.Fatalf("mutable fixture tag is not a live alias: %v, %v", resolved, err)
				}
				ref = repository + ":mutable"
			case "other repository":
				otherRepository := repository + "-other"
				other, err := client.ciRepository(otherRepository)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := oras.Copy(t.Context(), repo, descriptor.Digest.String(), other, descriptor.Digest.String(), oras.DefaultCopyOptions); err != nil {
					t.Fatal(err)
				}
				ref = otherRepository + "@" + descriptor.Digest.String()
				if _, err := client.ReadCIResult(t.Context(), otherRepository, ref, CIResultExpectation{Scope: scope}); err != nil {
					t.Fatal("other-repository fixture is not valid scoped metadata", err)
				}
			}
			inspect.Store(true)
			if err := client.DeleteCIObject(t.Context(), repository, ref, scope); err == nil {
				t.Fatal("cleanup accepted unverified or out-of-scope metadata")
			}
			inspect.Store(false)
			if writes.Load() != 0 {
				t.Fatalf("rejected cleanup performed %d writes", writes.Load())
			}
			if (mode == "contradictory HEAD digest" || mode == "corrupt manifest") && !injected.Load() {
				t.Fatal("cleanup did not exercise the controlled registry response")
			}
			ciRequireCleanupPayload(t, client, repository, original, ciTestScope(), payload)
		})
	}
}
