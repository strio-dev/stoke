package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	cooprwire "github.com/strio-dev/coopr/api"
	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/ociarchive"
	"golang.org/x/sys/unix"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

func ciImageFixture(t *testing.T, compressed bool, env string) CIImport {
	t.Helper()
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "blobs", "sha256"), 0700); err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	writer := tar.NewWriter(&plain)
	payload := []byte("particular producer output\n")
	if err := writer.WriteHeader(&tar.Header{Name: "value", Mode: 0644, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	encoded, mediaType := plain.Bytes(), v1.MediaTypeImageLayer
	if compressed {
		var b bytes.Buffer
		writer := gzip.NewWriter(&b)
		if _, err := writer.Write(encoded); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		encoded, mediaType = b.Bytes(), v1.MediaTypeImageLayerGzip
	}
	layer := writeCIBlob(t, directory, mediaType, encoded)
	config := v1.Image{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.FromBytes(plain.Bytes())}}, Config: v1.ImageConfig{Env: []string{env}}}
	cfg := writeCIBlob(t, directory, v1.MediaTypeImageConfig, ciJSON(t, config))
	manifest := v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg, Layers: []v1.Descriptor{layer}}
	root := writeCIBlob(t, directory, v1.MediaTypeImageManifest, ciJSON(t, manifest))
	writeCIIndex(t, directory, []v1.Descriptor{root})
	return CIImport{Kind: "image", OCI: directory, Digest: root.Digest.String(), Platform: "linux/amd64", CanonicalReference: "registry.invalid/results@" + root.Digest.String()}
}

func ciJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func writeCIBlob(t *testing.T, directory, mediaType string, data []byte) v1.Descriptor {
	t.Helper()
	d := v1.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
	if err := os.WriteFile(filepath.Join(directory, "blobs", "sha256", d.Digest.Encoded()), data, 0600); err != nil {
		t.Fatal(err)
	}
	return d
}
func writeCIIndex(t *testing.T, directory string, roots []v1.Descriptor) {
	t.Helper()
	for name, data := range map[string][]byte{"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`), "index.json": ciJSON(t, v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: roots})} {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
func importedCIImage(t *testing.T, input CIImport, opts coopr.Options) coopr.Result {
	t.Helper()
	if err := os.CopyFS(opts.Output.OCI, os.DirFS(input.OCI)); err != nil {
		t.Fatal(err)
	}
	return coopr.Result{ImageID: "imported-exact", ManifestDigest: input.Digest, Platform: input.Platform, OCI: opts.Output.OCI}
}

func TestCIExactImportNeverRunsFreshProducerOrExternalGate(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  producer: {image: scratch, cache: false, steps: [{run: [random-output]}]}
  gate: {image: scratch, cache: false, needs: [producer]}
  test: {from: producer, cache: false, needs: [gate], steps: [{run: [test]}]}
`)
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "forced-fresh-consumer"}[forced], func(t *testing.T) {
			input := ciImageFixture(t, true, "EXACT=one")
			opts := testOptions(t)
			opts.NoCache = forced
			imports, calls := 0, 0
			report, err := runCI(t.Context(), w, CIRequest{Execute: []string{"test"}, Imports: map[string]CIImport{"producer": input}}, opts, fakeEngine{execute: func(_ context.Context, p coopr.Program, o coopr.Options) (coopr.Result, error) {
				if strings.HasPrefix(string(p.Output), "stoke-ci-import-") {
					imports++
					if !o.Cache.NoCache || len(p.States[0].Operations) != 0 {
						t.Error("import used cache or executed instructions")
					}
					return importedCIImage(t, input, o), nil
				}
				calls++
				if string(p.Output) != "test" || p.States[0].From.Image != "containers-storage:imported-exact" || !o.Cache.NoCache {
					t.Fatalf("wrong selected execution: %#v %#v", p, o.Cache)
				}
				return coopr.Result{ImageID: "tested", Platform: "linux/amd64"}, nil
			}})
			if err != nil || imports != 1 || calls != 1 || len(report.Tasks) != 1 || report.Tasks[0].Name != "test" || report.Status != "passed" {
				t.Fatalf("imports=%d calls=%d report=%#v error=%v", imports, calls, report, err)
			}
			if !slices.Equal(w.Tasks["test"].Needs, []string{"gate"}) || w.Tasks["test"].From != "producer" {
				t.Fatal("ordinary workflow mutated")
			}
		})
	}
}

func TestCIImportedPublicationSelectsCanonicalProducerManifest(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  producer: {image: scratch, cache: false}
  test: {from: producer, cache: false}
  publish:
    publish: {from: producer, destinations: ['registry.invalid/final:tested']}
    needs: [test]
`)
	input := ciImageFixture(t, true, "EXACT=published")
	tested := false
	published := false
	report, err := runCI(t.Context(), w, CIRequest{Execute: []string{"test", "publish"}, Imports: map[string]CIImport{"producer": input}}, testOptions(t), fakeEngine{
		execute: func(_ context.Context, p coopr.Program, o coopr.Options) (coopr.Result, error) {
			if strings.HasPrefix(string(p.Output), "stoke-ci-import-") {
				return importedCIImage(t, input, o), nil
			}
			if string(p.Output) != "test" {
				t.Fatal("unexpected authored producer replay")
			}
			tested = true
			return coopr.Result{ImageID: "test", Platform: "linux/amd64"}, nil
		},
		publish: func(_ context.Context, p cooprwire.Publish, _ coopr.Options) (coopr.Result, error) {
			if !tested || p.Source != "oci:"+input.OCI || p.SourceDigest != input.Digest || p.Artifact != "image" {
				t.Fatalf("publication changed tested source: %#v", p)
			}
			published = true
			return coopr.Result{Reference: "registry.invalid/final@" + input.Digest}, nil
		},
	})
	if err != nil || !published || report.Status != "passed" || len(report.Tasks) != 2 {
		t.Fatalf("report=%#v err=%v", report, err)
	}
}

func TestCIExactImagePublicationRequiresOCILayoutCapability(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  producer: {image: scratch}
  publish: {publish: {from: producer, destinations: ['registry.invalid/final:tested']}}
`)
	plan, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	info := cooprwire.Info{Capabilities: []string{"execute", "publish"}}
	if err := testCapabilities(t, plan, info, Options{}); err != nil {
		t.Fatal(err)
	}
	opts := Options{ciImports: map[string]CIImport{"producer": {Kind: "image"}}}
	if err := testCapabilities(t, plan, info, opts); err == nil || !strings.Contains(err.Error(), "publish-oci-layout") {
		t.Fatalf("unsupported exact publication error: %v", err)
	}
	info.Capabilities = append(info.Capabilities, "publish-oci-layout")
	if err := testCapabilities(t, plan, info, opts); err != nil {
		t.Fatal(err)
	}
}

func TestCIExactImportsRejectInvalidPayloadBeforeState(t *testing.T) {
	for _, scenario := range []string{"missing", "kind", "platform", "digest", "canonical-tag", "canonical-other", "missing-file", "symlink", "corrupt-config", "unused"} {
		t.Run(scenario, func(t *testing.T) {
			input := ciImageFixture(t, false, "EXACT=one")
			w := fixture(t, "version: 1\ntasks:\n  producer: {image: scratch}\n  unused: {image: scratch}\n  test: {from: producer}\n")
			opts := testOptions(t)
			imports := map[string]CIImport{"producer": input}
			switch scenario {
			case "missing":
				imports = nil
			case "kind":
				input.Kind = "component"
			case "platform":
				input.Platform = "linux/arm64"
			case "digest":
				input.Digest = digest.FromString("wrong").String()
				input.CanonicalReference = "registry.invalid/results@" + input.Digest
			case "canonical-tag":
				input.CanonicalReference = "registry.invalid/results:latest"
			case "canonical-other":
				input.CanonicalReference = "registry.invalid/results@" + digest.FromString("wrong").String()
			case "missing-file":
				input.OCI = filepath.Join(t.TempDir(), "missing")
			case "symlink":
				link := filepath.Join(t.TempDir(), "image")
				if err := os.Symlink(input.OCI, link); err != nil {
					t.Fatal(err)
				}
				input.OCI = link
			case "corrupt-config":
				store, _, err := openCIStore(t.Context(), input)
				if err != nil {
					t.Fatal(err)
				}
				d, err := store.Resolve(t.Context(), input.Digest)
				if err != nil {
					t.Fatal(err)
				}
				b, err := readCIDescriptor(t.Context(), store, d, maxCIImportDocument)
				if err != nil {
					t.Fatal(err)
				}
				var m v1.Manifest
				if err = json.Unmarshal(b, &m); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(input.OCI, "blobs", "sha256", m.Config.Digest.Encoded()), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unused":
				imports["unused"] = input
			}
			if imports != nil {
				imports["producer"] = input
			}
			report, err := runCI(t.Context(), w, CIRequest{Execute: []string{"test"}, Imports: imports}, opts, fakeEngine{})
			if err == nil || report != nil {
				t.Fatalf("invalid payload accepted: report=%#v err=%v", report, err)
			}
			if _, err := os.Stat(opts.StateDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid import created state: %v", err)
			}
		})
	}
}

func TestCIImportedImageRepresentationMustPreserveConfigurationAndFilesystem(t *testing.T) {
	for _, scenario := range []string{"same", "plain-representation", "wrong-config", "wrong-platform", "bad-layer-size", "short-layer", "trailing-layer", "modified-layer"} {
		t.Run(scenario, func(t *testing.T) {
			input := ciImageFixture(t, true, "EXACT=one")
			after := input
			switch scenario {
			case "plain-representation":
				after = ciImageFixture(t, false, "EXACT=one")
			case "wrong-config":
				after = ciImageFixture(t, false, "EXACT=other")
			case "wrong-platform":
				after.Platform = "linux/arm64"
			case "bad-layer-size", "short-layer", "trailing-layer", "modified-layer":
				store, _, err := openCIStore(t.Context(), input)
				if err != nil {
					t.Fatal(err)
				}
				root, err := store.Resolve(t.Context(), input.Digest)
				if err != nil {
					t.Fatal(err)
				}
				data, err := readCIDescriptor(t.Context(), store, root, maxCIImportDocument)
				if err != nil {
					t.Fatal(err)
				}
				var m v1.Manifest
				if err = json.Unmarshal(data, &m); err != nil {
					t.Fatal(err)
				}
				if scenario == "bad-layer-size" {
					m.Layers[0].Size = 0
					root = writeCIBlob(t, input.OCI, v1.MediaTypeImageManifest, ciJSON(t, m))
					writeCIIndex(t, input.OCI, []v1.Descriptor{root})
					after.Digest = root.Digest.String()
				} else {
					name := filepath.Join(input.OCI, "blobs", "sha256", m.Layers[0].Digest.Encoded())
					data, err := os.ReadFile(name)
					if err != nil {
						t.Fatal(err)
					}
					switch scenario {
					case "short-layer":
						data = data[:len(data)-1]
					case "trailing-layer":
						data = append(data, 0)
					case "modified-layer":
						data[len(data)/2] ^= 1
					}
					if err := os.WriteFile(name, data, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			result := coopr.Result{ImageID: "native", OCI: after.OCI, ManifestDigest: after.Digest, Platform: after.Platform}
			err := verifyCIImportedImage(t.Context(), input, result)
			wantPass := scenario == "same" || scenario == "plain-representation"
			if (err == nil) != wantPass {
				t.Fatalf("scenario=%s err=%v", scenario, err)
			}
		})
	}
}

func TestCIImportFailureAndCancellationNeverStartConsumer(t *testing.T) {
	for _, scenario := range []string{"failure", "cancellation", "wrong-reexport"} {
		t.Run(scenario, func(t *testing.T) {
			input := ciImageFixture(t, false, "EXACT=one")
			wrong := ciImageFixture(t, false, "EXACT=wrong")
			w := fixture(t, "version: 1\ntasks:\n  producer: {image: scratch}\n  test: {from: producer}\n")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			report, err := runCI(ctx, w, CIRequest{Execute: []string{"test"}, Imports: map[string]CIImport{"producer": input}}, testOptions(t), fakeEngine{execute: func(ctx context.Context, p coopr.Program, o coopr.Options) (coopr.Result, error) {
				calls++
				if !strings.HasPrefix(string(p.Output), "stoke-ci-import-") {
					t.Fatal("consumer executed after failed import")
				}
				switch scenario {
				case "failure":
					return coopr.Result{}, errors.New("storage unavailable")
				case "cancellation":
					cancel()
					return coopr.Result{}, ctx.Err()
				default:
					return importedCIImage(t, wrong, o), nil
				}
			}})
			if err == nil || report == nil || report.Status != "failed" || len(report.Tasks) != 0 || calls != 1 || savedReport(t, report).Status != "failed" {
				t.Fatalf("calls=%d report=%#v err=%v", calls, report, err)
			}
		})
	}
}

func TestCIPromotedEnvironmentExecutesSeparatelyAndConsumersImportIt(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  gate: {image: scratch, cache: false}
  a: {environment: {files: [image.coopr]}, cache: false, needs: [gate], steps: [{run: [a]}]}
  b: {environment: {files: [image.coopr]}, cache: false, needs: [gate], steps: [{run: [b]}]}
`)
	const producer = "stoke-environment-shared"
	mapping := map[string]string{producer: "a"}
	builds := 0
	report, err := runCI(t.Context(), w, CIRequest{Execute: []string{producer}, Environments: mapping}, nativeOptions(t), fakeEngine{build: func(_ context.Context, _ cooprwire.Build, _ bool, o coopr.Options) (coopr.Result, error) {
		builds++
		if !o.Cache.NoCache {
			t.Error("fresh environment was cached")
		}
		return coopr.Result{ImageID: "built-env", Platform: "linux/amd64"}, nil
	}})
	if err != nil || builds != 1 || len(report.Tasks) != 1 || report.Tasks[0].Name != producer {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	input := ciImageFixture(t, false, "ENV=shared")
	for _, name := range []string{"a", "b"} {
		var calls []string
		report, err := runCI(t.Context(), w, CIRequest{Execute: []string{name}, Environments: mapping, Imports: map[string]CIImport{producer: input}}, nativeOptions(t), fakeEngine{execute: func(_ context.Context, p coopr.Program, o coopr.Options) (coopr.Result, error) {
			if strings.HasPrefix(string(p.Output), "stoke-ci-import-") {
				return importedCIImage(t, input, o), nil
			}
			calls = append(calls, string(p.Output))
			if p.States[0].From.Image != "containers-storage:imported-exact" {
				t.Error("consumer did not use exact shared environment")
			}
			return coopr.Result{Platform: "linux/amd64"}, nil
		}})
		if err != nil || !slices.Equal(calls, []string{name}) || len(report.Tasks) != 1 {
			t.Fatalf("calls=%v report=%#v err=%v", calls, report, err)
		}
	}
}

func ciComponentFixture(t *testing.T) CIImport {
	t.Helper()
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "blobs", "sha256"), 0700); err != nil {
		t.Fatal(err)
	}
	config := writeCIBlob(t, directory, "application/vnd.coopr.strio.dev.component.config.v1+json", []byte(`{"version":"coopr.strio.dev/component/v1"}`))
	root := writeCIBlob(t, directory, v1.MediaTypeImageManifest, ciJSON(t, v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: "application/vnd.coopr.strio.dev.component.v1+json", Config: config}))
	root.Annotations = map[string]string{v1.AnnotationRefName: "component"}
	auxcfg := writeCIBlob(t, directory, v1.MediaTypeEmptyJSON, []byte(`{}`))
	aux := writeCIBlob(t, directory, v1.MediaTypeImageManifest, ciJSON(t, v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: "application/vnd.example.delivery.v1", Config: auxcfg, Subject: &root}))
	writeCIIndex(t, directory, []v1.Descriptor{root, aux})
	archive := ciArchiveFromDirectory(t, directory)
	return CIImport{Kind: "component", OCI: archive, Digest: root.Digest.String()}
}

type ciComponentEngine struct {
	fakeEngine
	load func(context.Context, cooprwire.ComponentLoad, coopr.Options) (coopr.Result, error)
}

func (f ciComponentEngine) LoadComponent(ctx context.Context, r cooprwire.ComponentLoad, o coopr.Options) (coopr.Result, error) {
	return f.load(ctx, r, o)
}

func TestCIComponentImportRetainsArchiveBindingAndImmutableLocalPublication(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  component: {coopr: {kind: component, files: [component.coopr]}}
  consumer: {coopr: {files: [image.coopr], args: {COMPONENT: {from: component}}}}
  publish: {publish: {from: component, destinations: ['registry.invalid/components:tested']}, needs: [consumer]}
`)
	input := ciComponentFixture(t)
	input.CanonicalReference = "registry.invalid/results@" + input.Digest
	loads, builds, publishes := 0, 0, 0
	selectedReference := ""
	engine := ciComponentEngine{fakeEngine: fakeEngine{
		build: func(_ context.Context, b cooprwire.Build, _ bool, _ coopr.Options) (coopr.Result, error) {
			builds++
			if selectedReference == "" || b.Args["COMPONENT"] != selectedReference {
				t.Error("native component argument lost its exact delivery selector")
			}
			return coopr.Result{ImageID: "consumer", Platform: "linux/amd64"}, nil
		},
		publish: func(_ context.Context, p cooprwire.Publish, _ coopr.Options) (coopr.Result, error) {
			publishes++
			if p.Artifact != "component" || selectedReference == "" || p.Source != selectedReference {
				t.Fatalf("component publication lost loaded binding: %#v", p)
			}
			return coopr.Result{}, nil
		},
	}, load: func(_ context.Context, r cooprwire.ComponentLoad, _ coopr.Options) (coopr.Result, error) {
		loads++
		if r.Archive != input.OCI || !filepath.IsAbs(r.ComponentStoreDir) || r.Name == "" {
			t.Error("original bound archive was replaced")
		}
		selectedReference = r.Name + "@" + input.Digest
		return coopr.Result{Reference: selectedReference, ManifestDigest: input.Digest}, nil
	}}
	report, err := runCI(t.Context(), w, CIRequest{Execute: []string{"consumer", "publish"}, Imports: map[string]CIImport{"component": input}}, nativeOptions(t), engine)
	if err != nil || loads != 1 || builds != 1 || publishes != 1 || len(report.Tasks) != 2 {
		t.Fatalf("loads=%d builds=%d publishes=%d report=%#v err=%v", loads, builds, publishes, report, err)
	}
}

func TestCIComponentArchiveSelectorsAreStableAcrossStoresAndOrdering(t *testing.T) {
	input := ciComponentFixture(t)
	alternate := input
	alternate.OCI = filepath.Join(t.TempDir(), "repacked.tar")
	raw, err := os.ReadFile(input.OCI)
	if err != nil {
		t.Fatal(err)
	}
	// A distinct exact archive is allowed to receive a distinct selector, even
	// when a TAR repack preserves its component root and metadata.
	if err := os.WriteFile(alternate.OCI, append(raw, make([]byte, 1024)...), 0600); err != nil {
		t.Fatal(err)
	}
	engine := ciComponentEngine{load: func(_ context.Context, load cooprwire.ComponentLoad, _ coopr.Options) (coopr.Result, error) {
		if load.Name == "" {
			t.Fatal("unnamed exact component load")
		}
		return coopr.Result{Reference: load.Name + "@" + input.Digest, ManifestDigest: input.Digest}, nil
	}}
	first, err := loadCIImports(t.Context(), map[string]CIImport{"left": input, "right": alternate}, Options{}, t.TempDir(), engine)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadCIImports(t.Context(), map[string]CIImport{"before": alternate, "left": input, "right": alternate}, Options{}, t.TempDir(), engine)
	if err != nil {
		t.Fatal(err)
	}
	if first["left"].Reference == first["right"].Reference || first["left"].Reference != second["left"].Reference || first["right"].Reference != second["right"].Reference {
		t.Fatal("selectors depend on roots alone, storage paths or ordering", first, second)
	}
	bad := input
	bad.ArchiveDigest = "sha256:" + strings.Repeat("0", 64)
	if _, err := loadCIImports(t.Context(), map[string]CIImport{"left": bad}, Options{}, t.TempDir(), engine); err == nil {
		t.Fatal("archive differs from expected exact transport descriptor")
	}
	engine.load = func(_ context.Context, _ cooprwire.ComponentLoad, _ coopr.Options) (coopr.Result, error) {
		return coopr.Result{Reference: input.Digest, ManifestDigest: input.Digest}, nil
	}
	if _, err := loadCIImports(t.Context(), map[string]CIImport{"left": input}, Options{}, t.TempDir(), engine); err == nil {
		t.Fatal("service discarded delivery selector")
	}
}

func TestCIArchiveAcceptsBoundedBookkeepingDirectories(t *testing.T) {
	input := ciImageFixture(t, false, "EXACT=bookkeeping")
	for _, name := range []string{"blobs", "blobs/sha256", "ingest", "ingest/empty"} {
		if err := os.MkdirAll(filepath.Join(input.OCI, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	archive := ciArchiveFromDirectory(t, input.OCI)
	_, entries, err := indexCIArchive(t.Context(), archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"blobs", "blobs/sha256", "ingest", "ingest/empty"} {
		if file, err := entries.Open(name); !errors.Is(err, fs.ErrNotExist) {
			if file != nil {
				_ = file.Close()
			}
			t.Fatalf("directory %q exposed as content", name)
		}
	}
}

func TestCIArchiveRejectsTraversalAndSpecialEntries(t *testing.T) {
	for _, name := range []string{"../escape", "blobs/sha256/../../escape", "/escape", "symlink", "truncated-blob"} {
		t.Run(name, func(t *testing.T) {
			file, err := os.Create(filepath.Join(t.TempDir(), "bad.tar"))
			if err != nil {
				t.Fatal(err)
			}
			writer := tar.NewWriter(file)
			header := &tar.Header{Name: name, Mode: 0600}
			if name == "symlink" {
				header.Typeflag = tar.TypeSymlink
				header.Linkname = "/outside"
			}
			if name == "truncated-blob" {
				header.Name, header.Size = "blobs/sha256/"+strings.Repeat("a", 64), 1024
			}
			if err = writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if name == "truncated-blob" {
				err = file.Close() // Leave the declared body absent, without TAR padding.
			} else {
				err = errors.Join(writer.Close(), file.Close())
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = inspectCIArchive(t.Context(), file.Name()); err == nil {
				t.Fatal("unsafe component archive accepted")
			}
			if name == "truncated-blob" && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("skipped truncated body lost its EOF check: %v", err)
			}
		})
	}
}

func ciArchiveFromDirectory(t *testing.T, directory string) string {
	t.Helper()
	archive := filepath.Join(t.TempDir(), "component.oci.tar")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	err = filepath.WalkDir(directory, func(name string, entry os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if name == directory {
			return nil
		}
		relative, e := filepath.Rel(directory, name)
		if e != nil {
			return e
		}
		if entry.IsDir() {
			return writer.WriteHeader(&tar.Header{Name: filepath.ToSlash(relative) + "/", Typeflag: tar.TypeDir, Mode: 0700})
		}
		data, e := os.ReadFile(name)
		if e != nil {
			return e
		}
		if e = writer.WriteHeader(&tar.Header{Name: filepath.ToSlash(relative), Mode: 0600, Size: int64(len(data))}); e != nil {
			return e
		}
		_, e = writer.Write(data)
		return e
	})
	if err = errors.Join(err, writer.Close(), file.Close()); err != nil {
		t.Fatal(err)
	}
	return archive
}

func TestCILocalMetadataIsBoundedBeforeGraphParsing(t *testing.T) {
	for _, scenario := range []string{"large-index", "many-descriptors", "deep-json", "large-image-manifest", "large-archive-manifest", "large-config", "large-archive-config", "deep-graph"} {
		t.Run(scenario, func(t *testing.T) {
			input := ciImageFixture(t, false, "SAFE=one")
			switch scenario {
			case "large-index":
				if err := os.WriteFile(filepath.Join(input.OCI, "index.json"), bytes.Repeat([]byte(" "), int(maxCIImportDocument)+1), 0600); err != nil {
					t.Fatal(err)
				}
				if _, _, err := openCIStore(t.Context(), input); err == nil || !strings.Contains(err.Error(), "document exceeds") {
					t.Fatalf("unbounded index accepted: %v", err)
				}
				return
			case "many-descriptors":
				roots := make([]v1.Descriptor, maxCIImportNodes+1)
				for i := range roots {
					roots[i] = v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString("unavailable"), Size: 1}
				}
				writeCIIndex(t, input.OCI, roots)
				if _, _, err := openCIStore(t.Context(), input); err == nil || !strings.Contains(err.Error(), "item limit") {
					t.Fatalf("descriptor collection allocated before bound: %v", err)
				}
				return
			case "deep-json":
				data := []byte(`{"unknown":` + strings.Repeat("[", 129) + "0" + strings.Repeat("]", 129) + "}")
				if err := os.WriteFile(filepath.Join(input.OCI, "index.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
				if _, _, err := openCIStore(t.Context(), input); err == nil || !strings.Contains(err.Error(), "JSON depth") {
					t.Fatalf("deep metadata parsed: %v", err)
				}
				return
			case "large-image-manifest", "large-archive-manifest":
				root := writeCIBlob(t, input.OCI, v1.MediaTypeImageManifest, bytes.Repeat([]byte(" "), int(maxCIImportDocument)+1))
				input.Digest = root.Digest.String()
				input.CanonicalReference = "registry.invalid/results@" + input.Digest
				writeCIIndex(t, input.OCI, []v1.Descriptor{root})
				if scenario == "large-archive-manifest" {
					input.OCI = ciArchiveFromDirectory(t, input.OCI)
					input.Kind = "component"
				}
				// Opening the local view must not parse manifests or initialize a graph.
				if _, _, err := openCIStore(t.Context(), input); err != nil {
					t.Fatalf("local view eagerly parsed oversized graph: %v", err)
				}
			case "large-config", "large-archive-config":
				config := writeCIBlob(t, input.OCI, v1.MediaTypeImageConfig, bytes.Repeat([]byte(" "), int(maxCIImportDocument)+1))
				manifest := v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config}
				if scenario == "large-archive-config" {
					manifest.ArtifactType = "application/vnd.coopr.strio.dev.component.v1+json"
				}
				root := writeCIBlob(t, input.OCI, v1.MediaTypeImageManifest, ciJSON(t, manifest))
				input.Digest = root.Digest.String()
				input.CanonicalReference = "registry.invalid/results@" + input.Digest
				writeCIIndex(t, input.OCI, []v1.Descriptor{root})
				if scenario == "large-archive-config" {
					input.OCI = ciArchiveFromDirectory(t, input.OCI)
					input.Kind = "component"
				}
			case "deep-graph":
				root := writeCIBlob(t, input.OCI, v1.MediaTypeEmptyJSON, []byte(`{}`))
				for range 130 {
					root = writeCIBlob(t, input.OCI, v1.MediaTypeImageIndex, ciJSON(t, v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{root}}))
				}
				writeCIIndex(t, input.OCI, []v1.Descriptor{root})
				input.Digest = root.Digest.String()
				input.CanonicalReference = "registry.invalid/results@" + input.Digest
			}
			w := fixture(t, "version: 1\ntasks: {producer: {image: scratch}}\n")
			definition := w.Tasks["producer"]
			if input.Kind == "component" {
				definition = fixture(t, "version: 1\ntasks: {producer: {coopr: {kind: component, files: [component.coopr]}}}\n").Tasks["producer"]
			}
			err := validateCIImport(t.Context(), input, definition)
			if err == nil {
				t.Fatal("unsafe graph accepted")
			}
			if scenario == "deep-graph" && !strings.Contains(err.Error(), "input limits") {
				t.Fatalf("graph depth was not first enforced: %v", err)
			}
			if strings.HasPrefix(scenario, "large-") && !strings.Contains(err.Error(), "oversized OCI identity") {
				t.Fatalf("manifest limit not enforced before parse: %v", err)
			}
		})
	}
}

type ciCancelStorage struct {
	content.ReadOnlyStorage
	cancel context.CancelFunc
}
type ciCancelReader struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *ciCancelReader) Read(data []byte) (int, error) {
	n, err := r.ReadCloser.Read(data)
	r.cancel()
	return n, err
}
func (s ciCancelStorage) Fetch(ctx context.Context, d v1.Descriptor) (io.ReadCloser, error) {
	r, err := s.ReadOnlyStorage.Fetch(ctx, d)
	if err != nil {
		return nil, err
	}
	return &ciCancelReader{ReadCloser: r, cancel: s.cancel}, nil
}

type ciCancelContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *ciCancelContext) Err() error {
	c.checks++
	if c.checks == 3 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestCILocalGraphAndArchiveReadsObserveCancellation(t *testing.T) {
	input := ciImageFixture(t, false, "SAFE=one")
	store, roots, err := openCIStore(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	budget := ciGraphBudget{encoded: maxCIImportBytes, metadata: maxCIImportMetadata, seen: map[digest.Digest]v1.Descriptor{}}
	err = verifyCIGraph(ctx, ciCancelStorage{ReadOnlyStorage: store, cancel: cancel}, roots[0], &budget, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("graph read ignored cancellation: %v", err)
	}
	component := ciComponentFixture(t)
	base, stop := context.WithCancel(t.Context())
	defer stop()
	archiveCtx := &ciCancelContext{Context: base, cancel: stop}
	_, _, err = indexCIArchive(archiveCtx, component.OCI)
	if !errors.Is(err, context.Canceled) || archiveCtx.checks < 3 {
		t.Fatalf("archive read ignored mid-scan cancellation: %v checks=%d", err, archiveCtx.checks)
	}
}

func TestCILocalContentRejectsFIFOWithoutWriter(t *testing.T) {
	name := filepath.Join(t.TempDir(), "index.json")
	if err := unix.Mkfifo(name, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		file, err := ociarchive.OpenRegular(name)
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted FIFO content")
		}
	case <-time.After(time.Second):
		// Unblock only this fixture's reader so the pre-fix failure can finish.
		if fd, err := unix.Open(name, unix.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
			_ = unix.Close(fd)
		}
		<-done
		t.Fatal("OCI content open waited for a FIFO writer")
	}
}

func TestCIIndexSubjectMustBeVerified(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	subjectData := []byte("subject content")
	subject := content.NewDescriptorFromBytes("application/octet-stream", subjectData)
	raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Subject: &subject})
	if err != nil {
		t.Fatal(err)
	}
	root := content.NewDescriptorFromBytes(v1.MediaTypeImageIndex, raw)
	if err := store.Push(ctx, root, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	verify := func() error {
		budget := ciGraphBudget{encoded: maxCIImportBytes, metadata: maxCIImportMetadata, seen: map[digest.Digest]v1.Descriptor{}}
		return verifyCIGraph(ctx, store, root, &budget, 0)
	}
	if err := verify(); err == nil {
		t.Fatal("missing index subject accepted")
	}
	if err := store.Push(ctx, subject, bytes.NewReader(subjectData)); err != nil {
		t.Fatal(err)
	}
	if err := verify(); err != nil {
		t.Fatalf("complete index subject rejected: %v", err)
	}
}

func TestCIGraphDepthIncludesPreviouslyVerifiedSubtrees(t *testing.T) {
	for _, wrappers := range []int{125, 126} {
		t.Run(fmt.Sprint(wrappers), func(t *testing.T) {
			store := memory.New()
			push := func(media string, raw []byte) v1.Descriptor {
				descriptor := content.NewDescriptorFromBytes(media, raw)
				if err := store.Push(t.Context(), descriptor, bytes.NewReader(raw)); err != nil {
					t.Fatal(err)
				}
				return descriptor
			}
			index := func(children ...v1.Descriptor) v1.Descriptor {
				raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: children})
				if err != nil {
					t.Fatal(err)
				}
				return push(v1.MediaTypeImageIndex, raw)
			}
			shared := index(index(push("application/octet-stream", []byte("leaf"))))
			long := shared
			for range wrappers {
				long = index(long)
			}
			root := index(shared, long) // Visit the shared subtree through its short path first.
			budget := ciGraphBudget{encoded: maxCIImportBytes, metadata: maxCIImportMetadata, seen: map[digest.Digest]v1.Descriptor{}}
			err := verifyCIGraph(t.Context(), store, root, &budget, 0)
			if (err != nil) != (wrappers == 126) {
				t.Fatalf("wrappers=%d: error=%v", wrappers, err)
			}
		})
	}
}
