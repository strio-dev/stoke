package artifact

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// This models an operator retaining an explicit prior attempt during its retry
// window, then selecting only known closed objects. DeleteCIObject itself has
// no clock, retry-window guard, inventory discovery or writer lease.
func TestCICleanupExplicitInventoryPreservesRetryAndSharedContent(t *testing.T) {
	base := registryFixtureManifestDeletion(t, true)
	var mu sync.Mutex
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleted = append(deleted, r.URL.Path)
			mu.Unlock()
		}
		base.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	repository := host + "/results"
	client := Client{PlainHTTP: true}
	scope := ciTestScope()
	other := scope
	other.Invocation = "another-retained-invocation"
	layout, root := ciTestLayout(t, "image", "linux/amd64", "shared-payload")
	type selection struct{ result, inputs string }
	publish := func(scope CIScope, attempt int) selection {
		t.Helper()
		identity := ciTestResult(t, "build", "linux/amd64")
		identity.Scope, identity.Attempt = scope, attempt
		ref, err := client.PublishCIResult(t.Context(), repository, identity, layout, root)
		if err != nil {
			t.Fatal(err)
		}
		inputs, err := client.PublishCIInputSet(t.Context(), repository, CIInputSet{Version: CIVersion, Scope: scope, Bindings: map[string]string{"build": ref}})
		if err != nil {
			t.Fatal(err)
		}
		return selection{ref, inputs}
	}
	held, closed, unrelated := publish(scope, 1), publish(scope, 2), publish(other, 1)
	cacheData := []byte("version: 1\ntasks: {}\n")
	cacheRef, err := client.Push(t.Context(), host+"/cache:retained", cacheData, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// While the operator's retry window is open, explicit old-attempt metadata
	// and payload remain usable. No automatic expiry is assumed or invoked.
	if _, err := client.ReadCIInputSet(t.Context(), repository, held.inputs, scope); err != nil {
		t.Fatal("retained prior-attempt bindings unavailable", err)
	}
	ciRequireCleanupPayload(t, client, repository, held.result, scope, root)
	mu.Lock()
	deleteCount := len(deleted)
	mu.Unlock()
	if deleteCount != 0 {
		t.Fatal("retention performed an unrequested deletion")
	}
	anchors := ciCleanupPayloadTags(t, server.URL)
	if len(anchors) != 2 {
		t.Fatal("expected one shared payload anchor per invocation", anchors)
	}
	if err := client.DeleteCIObject(t.Context(), repository, closed.inputs, other); err == nil {
		t.Fatal("deleted InputSet from another invocation scope")
	}
	// The operator has stopped writers and selected this exact inventory after
	// its retry window. Deleting an InputSet must not recursively delete results.
	if err := client.DeleteCIObject(t.Context(), repository, closed.inputs, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadCIResult(t.Context(), repository, closed.result, CIResultExpectation{Scope: scope}); err != nil {
		t.Fatal("InputSet cleanup recursively deleted its result", err)
	}
	if err := client.DeleteCIObject(t.Context(), repository, closed.result, scope); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{closed.inputs, closed.result} {
		if err := client.DeleteCIObject(t.Context(), repository, ref, scope); err == nil {
			t.Fatal("already missing exact metadata reported successful deletion")
		}
	}
	if _, err := client.ReadCIInputSet(t.Context(), repository, closed.inputs, scope); err == nil {
		t.Fatal("deleted exact bindings were substituted")
	}
	if _, err := client.ReadCIResult(t.Context(), repository, closed.result, CIResultExpectation{Scope: scope}); err == nil {
		t.Fatal("deleted result was substituted")
	}
	ciRequireCleanupPayload(t, client, repository, held.result, scope, root)
	ciRequireCleanupPayload(t, client, repository, unrelated.result, other, root)
	if _, err := client.ReadCIInputSet(t.Context(), repository, unrelated.inputs, other); err != nil {
		t.Fatal("cleanup damaged another invocation", err)
	}
	if tags := ciCleanupPayloadTags(t, server.URL); !slices.Equal(tags, anchors) {
		t.Fatal("cleanup removed shared payload retention anchors", tags, anchors)
	}
	cache, err := client.Pull(t.Context(), cacheRef)
	if err != nil || string(cache.Data) != string(cacheData) {
		t.Fatal("cleanup damaged a separate OCI namespace", err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"/v2/results/manifests/" + strings.Split(closed.inputs, "@")[1], "/v2/results/manifests/" + strings.Split(closed.result, "@")[1]}
	if !slices.Equal(deleted, want) {
		t.Fatal("cleanup exceeded explicit metadata inventory", deleted, want)
	}
}

// Exact deletion is bounded, but it is not a concurrent garbage collector. A
// writer can still refer to an object being deleted; consumers then fail closed.
func TestCICleanupConcurrentWriterRequiresQuiescence(t *testing.T) {
	base := registryFixtureManifestDeletion(t, true)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		base.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	repository := strings.TrimPrefix(server.URL, "http://") + "/results"
	client := Client{PlainHTTP: true}
	scope := ciTestScope()
	layout, root := ciTestLayout(t, "image", "linux/amd64", "shared-payload")
	old, err := client.PublishCIResult(t.Context(), repository, ciTestResult(t, "build", "linux/amd64"), layout, root)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := client.PublishCIInputSet(t.Context(), repository, CIInputSet{Version: CIVersion, Scope: scope, Bindings: map[string]string{"build": old}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	deleted := make(chan error, 1)
	go func() { deleted <- client.DeleteCIObject(ctx, repository, old, scope) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not enter the controlled delete barrier")
	}
	dependent := ciTestResult(t, "package", "linux/amd64")
	dependent.Attempt, dependent.Inputs = 2, inputs
	dependentRef, err := client.PublishCIResult(t.Context(), repository, dependent, layout, root)
	if err != nil {
		t.Fatal("writer could not select a still-live predecessor", err)
	}
	independent := ciTestResult(t, "build", "linux/amd64")
	independent.Attempt = 2
	independentRef, err := client.PublishCIResult(t.Context(), repository, independent, layout, root)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	ciRequireCleanupPayload(t, client, repository, independentRef, scope, root)
	if _, err := client.ReadCIResult(t.Context(), repository, dependentRef, CIResultExpectation{Scope: scope, Producer: "package"}); err == nil {
		t.Fatal("consumer guessed around deleted predecessor lineage")
	}
	parent := t.TempDir()
	if _, err := client.FetchCIPayload(t.Context(), repository, dependentRef, CIResultExpectation{Scope: scope}, filepath.Join(parent, "payload")); err == nil {
		t.Fatal("consumer rebuilt or substituted missing exact lineage")
	}
	if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
		t.Fatal("failed exact consumption left payload staging", entries, err)
	}
}

func TestCICleanupAccessAndRegistryFailurePreserveObjects(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusMethodNotAllowed, http.StatusNotImplemented, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			base := registryFixtureManifestDeletion(t, true)
			var mu sync.Mutex
			denied := false
			deleteCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				block := denied && r.Method == http.MethodDelete
				if block {
					deleteCalls++
				}
				mu.Unlock()
				if block {
					w.WriteHeader(code)
					return
				}
				base.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			repository := strings.TrimPrefix(server.URL, "http://") + "/results"
			client := Client{PlainHTTP: true}
			layout, root := ciTestLayout(t, "image", "linux/amd64", "failure")
			ref, err := client.PublishCIResult(t.Context(), repository, ciTestResult(t, "build", "linux/amd64"), layout, root)
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			denied = true
			mu.Unlock()
			// Bound a retrying transport without requiring a specific retry count.
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := client.DeleteCIObject(ctx, repository, ref, ciTestScope()); err == nil {
				t.Fatal("registry failure silently reported cleanup success")
			}
			mu.Lock()
			calls := deleteCalls
			mu.Unlock()
			if calls == 0 {
				t.Fatal("failure case did not exercise DELETE")
			}
			ciRequireCleanupPayload(t, client, repository, ref, ciTestScope(), root)
		})
	}
}

func TestCICleanupReadOnlyCredentialsCannotDelete(t *testing.T) {
	base := registryFixtureManifestDeletion(t, true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || password != "fixture-password" || user != "writer" && user != "reader" {
			w.Header().Set("WWW-Authenticate", `Basic realm="cleanup"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if user == "reader" && r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		base.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	repository := host + "/results"
	authFile := func(user string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "auth.json")
		data, err := json.Marshal(map[string]any{"auths": map[string]any{host: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(user + ":fixture-password"))}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	writer := Client{PlainHTTP: true, AuthFile: authFile("writer")}
	reader := Client{PlainHTTP: true, AuthFile: authFile("reader")}
	layout, root := ciTestLayout(t, "image", "linux/amd64", "auth")
	var refs, sets []string
	for _, name := range []string{"build", "other"} {
		ref, err := writer.PublishCIResult(t.Context(), repository, ciTestResult(t, name, "linux/amd64"), layout, root)
		if err != nil {
			t.Fatal(err)
		}
		set, err := writer.PublishCIInputSet(t.Context(), repository, CIInputSet{Version: CIVersion, Scope: ciTestScope(), Bindings: map[string]string{name: ref}})
		if err != nil {
			t.Fatal(err)
		}
		refs, sets = append(refs, ref), append(sets, set)
	}
	if got, _, err := reader.MergeCIInputSets(t.Context(), repository, ciTestScope(), sets[:1]); err != nil || got != sets[0] {
		t.Fatal("read-only credentials cannot reuse identical bindings", err)
	}
	if _, _, err := reader.MergeCIInputSets(t.Context(), repository, ciTestScope(), sets); err == nil {
		t.Fatal("read-only credentials published a new binding union")
	}
	unionRef, union, err := writer.MergeCIInputSets(t.Context(), repository, ciTestScope(), sets)
	if err != nil || len(union.Bindings) != 2 || slices.Contains(sets, unionRef) {
		t.Fatal("new producer union failed", err)
	}
	if _, err := (Client{PlainHTTP: true}).ReadCIInputSet(t.Context(), repository, sets[0], ciTestScope()); err == nil {
		t.Fatal("anonymous read unexpectedly authorized")
	}
	if _, err := reader.PublishCIResult(t.Context(), repository, ciTestResult(t, "forbidden", "linux/amd64"), layout, root); err == nil {
		t.Fatal("read-only credentials published execution")
	}
	if err := reader.DeleteCIObject(t.Context(), repository, refs[0], ciTestScope()); err == nil {
		t.Fatal("read-only credential deleted metadata")
	}
	ciRequireCleanupPayload(t, reader, repository, refs[0], ciTestScope(), root)
	if err := writer.DeleteCIObject(t.Context(), repository, refs[0], ciTestScope()); err != nil {
		t.Fatal("authorized writer cannot delete selected metadata", err)
	}
}

func TestCICleanupCancellationDoesNotPromiseRollback(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "before-commit"
		if committed {
			name = "response-lost-after-commit"
		}
		t.Run(name, func(t *testing.T) {
			base := registryFixtureManifestDeletion(t, true)
			entered := make(chan struct{})
			var once sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete {
					base.Config.Handler.ServeHTTP(w, r)
					return
				}
				if committed {
					recorder := httptest.NewRecorder()
					base.Config.Handler.ServeHTTP(recorder, r)
					if recorder.Code != http.StatusAccepted {
						t.Errorf("fixture did not commit DELETE: %d", recorder.Code)
					}
				}
				once.Do(func() { close(entered) })
				<-r.Context().Done()
			}))
			t.Cleanup(server.Close)
			repository := strings.TrimPrefix(server.URL, "http://") + "/results"
			client := Client{PlainHTTP: true}
			layout, root := ciTestLayout(t, "image", "linux/amd64", "cancel")
			ref, err := client.PublishCIResult(t.Context(), repository, ciTestResult(t, "build", "linux/amd64"), layout, root)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- client.DeleteCIObject(ctx, repository, ref, ciTestScope()) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("cleanup did not reach cancellation barrier")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cleanup cancellation did not propagate", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cleanup did not drain after cancellation")
			}
			_, err = client.ReadCIResult(t.Context(), repository, ref, CIResultExpectation{Scope: ciTestScope()})
			if committed == (err == nil) {
				t.Fatal("cleanup cancellation incorrectly implied rollback", committed, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := (Client{PlainHTTP: true}).DeleteCIObject(ctx, "127.0.0.1:1/results", "127.0.0.1:1/results@sha256:"+strings.Repeat("a", 64), ciTestScope()); !errors.Is(err, context.Canceled) {
		t.Fatal("pre-cancelled cleanup ignored cancellation", err)
	}
}

func ciRequireCleanupPayload(t *testing.T, client Client, repository, reference string, scope CIScope, root string) {
	t.Helper()
	payload, err := client.FetchCIPayload(t.Context(), repository, reference, CIResultExpectation{Scope: scope, MaxAttempt: 3}, filepath.Join(t.TempDir(), "payload"))
	if err != nil || payload.Result.Output.Digest.String() != root || payload.CanonicalReference != repository+"@"+root {
		t.Fatal("retained exact payload unavailable or changed", payload, err)
	}
}

func ciCleanupPayloadTags(t *testing.T, baseURL string) []string {
	t.Helper()
	response, err := http.Get(baseURL + "/v2/results/tags/list")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	var listing struct{ Tags []string }
	if err := json.NewDecoder(response.Body).Decode(&listing); err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("retention-tag inventory failed", err, response.Status)
	}
	var tags []string
	for _, tag := range listing.Tags {
		if strings.HasPrefix(tag, "scip-") {
			tags = append(tags, tag)
		}
	}
	slices.Sort(tags)
	return tags
}
