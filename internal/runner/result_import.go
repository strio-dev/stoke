package runner

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	cooprwire "github.com/strio-dev/coopr/api"
	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/hostfiles"
	"github.com/strio-dev/stoke/internal/ociarchive"
	"github.com/strio-dev/stoke/internal/workflow"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry"
)

const (
	maxCIImportBytes    = int64(32 << 30)
	maxCIImportDocument = int64(8 << 20)
	maxCIImportMetadata = int64(64 << 20)
	maxCIImportNodes    = 8192
)

type componentLoader interface {
	LoadComponent(context.Context, cooprwire.ComponentLoad, coopr.Options) (coopr.Result, error)
}

type ciImage struct {
	root     v1.Descriptor
	manifest v1.Manifest
	config   v1.Image
}

func validateCIImport(ctx context.Context, input CIImport, definition workflow.Task) (retErr error) {
	traceStarted := time.Now()
	defer func() {
		fmt.Fprintf(os.Stderr, "STOKE_IMPORT_TRACE phase=validate_import seconds=%.6f success=%t\n", time.Since(traceStarted).Seconds(), retErr == nil)
	}()
	if input.Kind == "host" {
		if definition.OutputKind() != "host" {
			return errors.New("host files differ from declared producer kind")
		}
		reference, err := registry.ParseReference(input.CanonicalReference)
		if err != nil || reference.Registry == "" || reference.Repository == "" || reference.Reference != input.Digest {
			return errors.New("host result requires its exact canonical OCI digest reference")
		}
		config, err := hostfiles.Validate(ctx, input.OCI, input.Digest)
		if err != nil {
			return err
		}
		expected := slices.Clone(definition.Host.Outputs)
		slices.Sort(expected)
		if !slices.Equal(config.Paths, expected) {
			return errors.New("host result captured paths differ from declared producer outputs")
		}
		platform := config.Platform.OS + "/" + config.Platform.Architecture
		requested, err := definition.ResolvedPlatform()
		if err != nil {
			return err
		}
		if platform != input.Platform || platform != requested {
			return errors.New("host result platform differs from declared producer")
		}
		return nil
	}
	if input.Kind != definition.OutputKind() || input.Kind != "image" && input.Kind != "component" {
		return errors.New("result kind differs from its declared producer")
	}
	if err := validateCIDigest(input.Digest); err != nil {
		return err
	}
	if input.Kind == "image" || input.CanonicalReference != "" {
		reference, err := registry.ParseReference(input.CanonicalReference)
		if err != nil || reference.Registry == "" || reference.Repository == "" || reference.Reference != input.Digest {
			return errors.New("canonical result reference must select its exact OCI output digest")
		}
	}
	if input.Kind == "image" || input.Platform != "" {
		platform, err := workflow.NormalizePlatform(input.Platform)
		if err != nil || platform != input.Platform {
			return errors.New("image result must report a canonical platform")
		}
	}
	requested, err := definition.ResolvedPlatform()
	if err != nil {
		return err
	}
	// Native FROM may override the build default; OCI metadata below remains
	// authoritative for the actual output platform.
	if definition.Coopr == nil && requested != "" && input.Platform != requested {
		return fmt.Errorf("result platform %q differs from producer platform %q", input.Platform, requested)
	}
	if input.Kind == "image" && input.OCI == "" {
		return nil
	}
	store, roots, err := openCIStore(ctx, input)
	if err != nil {
		return err
	}
	found := false
	for _, root := range roots {
		found = found || root.Digest.String() == input.Digest
	}
	if !found {
		return errors.New("OCI layout does not select the declared result digest")
	}
	if input.Kind == "component" {
		if err := validateCIComponentRoots(ctx, store, roots, input.Digest); err != nil {
			return err
		}
	}
	budget := ciGraphBudget{encoded: maxCIImportBytes, metadata: maxCIImportMetadata, seen: map[digest.Digest]v1.Descriptor{}}
	traceGraphStarted := time.Now()
	for _, root := range roots {
		if err := verifyCIGraph(ctx, store, root, &budget, 0); err != nil {
			fmt.Fprintf(os.Stderr, "STOKE_IMPORT_TRACE phase=validate_graph seconds=%.6f success=false\n", time.Since(traceGraphStarted).Seconds())
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "STOKE_IMPORT_TRACE phase=validate_graph seconds=%.6f success=true\n", time.Since(traceGraphStarted).Seconds())
	if input.Kind == "image" {
		traceImageStarted := time.Now()
		_, err = readCIImage(ctx, input.OCI, input.Digest, input.Platform)
		fmt.Fprintf(os.Stderr, "STOKE_IMPORT_TRACE phase=validate_image seconds=%.6f success=%t\n", time.Since(traceImageStarted).Seconds(), err == nil)
	}
	return err
}

func loadCIImports(ctx context.Context, inputs map[string]CIImport, options Options, directory string, engine backend) (_ map[string]coopr.Result, retErr error) {
	traceStarted := time.Now()
	defer func() {
		fmt.Fprintf(os.Stderr, "STOKE_IMPORT_TRACE phase=load_imports seconds=%.6f success=%t\n", time.Since(traceStarted).Seconds(), retErr == nil)
	}()
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	slices.Sort(names)
	results := map[string]coopr.Result{}
	for index, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		input := inputs[name]
		var result coopr.Result
		var err error
		switch input.Kind {
		case "host":
			result = coopr.Result{Platform: input.Platform, ManifestDigest: input.Digest, OCI: input.OCI, Filesystem: filepath.Join(directory, fmt.Sprintf("host-import-%d", index)), CanonicalReference: input.CanonicalReference, CanonicalOCI: input.OCI, Metadata: map[string]json.RawMessage{"stoke.host.execution": json.RawMessage(`"exact-imported-output"`)}}
			err = hostfiles.Restore(ctx, input.OCI, input.Digest, result.Filesystem)
		case "component":
			loader, ok := engine.(componentLoader)
			if !ok {
				return nil, errors.New("coopr API does not support component-load required for exact CI inputs")
			}
			alias, identityErr := ciComponentName(ctx, input)
			if identityErr != nil {
				return nil, fmt.Errorf("component archive identity %q: %w", name, identityErr)
			}
			result, err = loader.LoadComponent(ctx, cooprwire.ComponentLoad{Archive: input.OCI, ComponentStoreDir: filepath.Join(directory, "components"), Name: alias}, coopr.Options{Stdout: options.Stdout, Stderr: options.Stderr})
			if err == nil && (result.ManifestDigest != input.Digest || result.Reference != alias+"@"+input.Digest) {
				err = errors.New("loaded component does not retain its exact immutable root and delivery selector")
			}
		case "image":
			if input.OCI == "" {
				result = coopr.Result{Reference: input.CanonicalReference, CanonicalReference: input.CanonicalReference, ManifestDigest: input.Digest, Platform: input.Platform}
				break
			}
			id := fmt.Sprintf("stoke-ci-import-%d", index)
			program := coopr.Program{Platform: input.Platform, States: []coopr.State{{ID: id, From: coopr.Source{Image: "oci:" + filepath.Base(input.OCI)}}}, Output: id}
			opts := coopr.Options{
				ContextDir: filepath.Dir(input.OCI), Runtime: options.Runtime,
				Store:    coopr.StoreOptions{RunRoot: filepath.Join(directory, "runroot"), GraphRoot: filepath.Join(directory, "graphroot"), Driver: options.StorageDriver},
				Output:   coopr.Output{OCI: filepath.Join(directory, fmt.Sprintf("ci-import-%d", index))},
				Cache:    coopr.CacheOptions{NoCache: true},
				Registry: coopr.RegistryOptions{AuthFile: options.AuthFile, SignaturePolicyPath: options.SignaturePolicy, TLSVerify: options.TLSVerify},
				Stdout:   options.Stdout, Stderr: options.Stderr,
			}
			traceExecuteStarted := time.Now()
			result, err = engine.Execute(ctx, program, opts)
			fmt.Fprintf(os.Stderr, "STOKE_IMPORT_TRACE phase=import_image_execute seconds=%.6f success=%t\n", time.Since(traceExecuteStarted).Seconds(), err == nil)
			if err == nil {
				actual, platformErr := workflow.NormalizePlatform(result.Platform)
				if platformErr != nil || actual != input.Platform || result.ImageID == "" {
					err = errors.New("loaded image has a different platform or no native identity")
				} else {
					err = verifyCIImportedImage(ctx, input, result)
				}
			}
			if err == nil {
				result.CanonicalReference = input.CanonicalReference
				result.CanonicalOCI = input.OCI
			}
		default:
			err = errors.New("unsupported exact CI result kind")
		}
		if err != nil {
			return nil, fmt.Errorf("import exact CI result %q: %w", name, err)
		}
		results[name] = result
	}
	return results, nil
}

// Exact archive bytes retain delivery metadata as well as the component root.
// Do not derive observable native ARG values from paths, stores or import order.
func ciComponentName(ctx context.Context, input CIImport) (string, error) {
	file, err := os.Open(input.OCI)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxCIImportBytes {
		return "", errors.New("component archive is not a bounded regular file")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, &ciContextReader{ctx: ctx, reader: io.LimitReader(file, info.Size()+1)})
	if err != nil || n != info.Size() {
		return "", errors.Join(errors.New("component archive changed while identifying it"), err)
	}
	identity := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if input.ArchiveDigest != "" && input.ArchiveDigest != identity {
		return "", errors.New("component archive differs from its exact transport descriptor")
	}
	return "stoke-ci-" + hex.EncodeToString(hash.Sum(nil)) + ":result", nil
}

func validateCIDigest(value string) error {
	parsed := digest.Digest(value)
	if parsed.Validate() != nil || parsed.Algorithm() != digest.SHA256 {
		return errors.New("exact CI OCI identity requires a SHA256 digest")
	}
	return nil
}

type ciLayoutFS struct{ root string }

func (s ciLayoutFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, errors.New("invalid OCI layout path")
	}
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		info, err := os.Lstat(filepath.Join(s.root, filepath.FromSlash(parent)))
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.Join(errors.New("OCI layout parent must be a real directory"), err)
		}
	}
	return ociarchive.OpenRegular(filepath.Join(s.root, filepath.FromSlash(name)))
}

// ciOCIStore is a read-only local view. It deliberately does not initialize
// ORAS graph indexing: every recursive document is first parsed by our bounded
// descriptor verifier, after digest and size verification.
type ciOCIStore struct {
	content.ReadOnlyStorage
	roots []v1.Descriptor
}

func (s *ciOCIStore) Fetch(ctx context.Context, descriptor v1.Descriptor) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.ReadOnlyStorage.Fetch(ctx, descriptor)
}

func (s *ciOCIStore) Resolve(ctx context.Context, reference string) (v1.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return v1.Descriptor{}, err
	}
	for _, root := range s.roots {
		if root.Digest.String() == reference || root.Annotations[v1.AnnotationRefName] == reference {
			return root, nil
		}
	}
	return v1.Descriptor{}, errors.New("OCI result reference is not an indexed root")
}

func readCILocalDocument(ctx context.Context, layout fs.FS, name string) ([]byte, error) {
	file, err := layout.Open(name)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(&ciContextReader{ctx: ctx, reader: file}, maxCIImportDocument+1))
	err = errors.Join(err, file.Close())
	if err != nil {
		return nil, err
	}
	if len(data) > int(maxCIImportDocument) {
		return nil, errors.New("OCI layout document exceeds its input limit")
	}
	if err := validateCIMetadataJSON(data); err != nil {
		return nil, err
	}
	return data, nil
}

func openCIStore(ctx context.Context, input CIImport) (*ciOCIStore, []v1.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if !filepath.IsAbs(input.OCI) {
		return nil, nil, errors.New("CI payload must use an absolute local path")
	}
	info, err := os.Lstat(input.OCI)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.Join(errors.New("CI payload is unavailable or is a symlink"), err)
	}
	var storage content.ReadOnlyStorage
	var indexData []byte
	if input.Kind == "image" {
		if !info.IsDir() {
			return nil, nil, errors.New("image payload must be an OCI layout directory")
		}
		layout := ciLayoutFS{input.OCI}
		layoutData, err := readCILocalDocument(ctx, layout, "oci-layout")
		if err != nil {
			return nil, nil, err
		}
		var version struct {
			ImageLayoutVersion string `json:"imageLayoutVersion"`
		}
		if json.Unmarshal(layoutData, &version) != nil || version.ImageLayoutVersion != "1.0.0" {
			return nil, nil, errors.New("invalid OCI image layout version")
		}
		indexData, err = readCILocalDocument(ctx, layout, "index.json")
		if err != nil {
			return nil, nil, err
		}
		storage = orasoci.NewStorageFromFS(layout)
	} else {
		if !info.Mode().IsRegular() || info.Size() > maxCIImportBytes {
			return nil, nil, errors.New("component payload must be a bounded regular OCI archive")
		}
		var archive fs.FS
		indexData, archive, err = indexCIArchive(ctx, input.OCI)
		if err != nil {
			return nil, nil, err
		}
		storage = orasoci.NewStorageFromFS(archive)
	}
	var index v1.Index
	if len(indexData) > int(maxCIImportDocument) || json.Unmarshal(indexData, &index) != nil || index.SchemaVersion != 2 || len(index.Manifests) == 0 || len(index.Manifests) > maxCIImportNodes {
		return nil, nil, errors.New("invalid or oversized OCI result index")
	}
	if input.Kind == "image" && (len(index.Manifests) != 1 || index.Manifests[0].Digest.String() != input.Digest) {
		return nil, nil, errors.New("image payload must contain only its declared canonical result root")
	}
	return &ciOCIStore{ReadOnlyStorage: storage, roots: index.Manifests}, index.Manifests, nil
}

func inspectCIArchive(ctx context.Context, name string) ([]byte, error) {
	data, _, err := indexCIArchive(ctx, name)
	return data, err
}

func indexCIArchive(ctx context.Context, name string) ([]byte, fs.FS, error) {
	archive, err := ociarchive.New(ctx, name, maxCIImportNodes)
	if err != nil {
		return nil, nil, err
	}
	layout, err := readCILocalDocument(ctx, archive, "oci-layout")
	if err != nil {
		return nil, nil, err
	}
	var version v1.ImageLayout
	if json.Unmarshal(layout, &version) != nil || version.Version != v1.ImageLayoutVersion {
		return nil, nil, errors.New("invalid component OCI layout version")
	}
	index, err := readCILocalDocument(ctx, archive, "index.json")
	return index, archive, err
}

type ciGraphBudget struct {
	encoded, metadata int64
	seen              map[digest.Digest]v1.Descriptor
	configurations    map[digest.Digest]bool
	heights           map[digest.Digest]int
}

func verifyCIGraph(ctx context.Context, store content.ReadOnlyStorage, descriptor v1.Descriptor, budget *ciGraphBudget, depth int) error {
	if err := validateCIDigest(descriptor.Digest.String()); err != nil {
		return err
	}
	if descriptor.Size < 0 || descriptor.Size > maxCIImportBytes || depth > 128 {
		return errors.New("OCI result descriptor exceeds its input limits")
	}
	if previous, seen := budget.seen[descriptor.Digest]; seen {
		if previous.Size != descriptor.Size || previous.MediaType != descriptor.MediaType {
			return errors.New("OCI graph reuses a digest with conflicting descriptor metadata")
		}
		if budget.heights[descriptor.Digest] < 0 || depth+budget.heights[descriptor.Digest] > 128 {
			return errors.New("OCI result graph is cyclic or exceeds its depth limit")
		}
		return nil
	}
	if budget.heights == nil {
		budget.heights = map[digest.Digest]int{}
	}
	// Memoize subtree height, not only whether its bytes were verified: a
	// shared tail reached later through a longer path still counts in full.
	budget.heights[descriptor.Digest] = -1
	budget.seen[descriptor.Digest] = descriptor
	if len(budget.seen) > maxCIImportNodes || descriptor.Size > budget.encoded {
		return errors.New("OCI result graph exceeds its input limits")
	}
	budget.encoded -= descriptor.Size
	var children []v1.Descriptor
	switch descriptor.MediaType {
	case v1.MediaTypeImageManifest, "application/vnd.docker.distribution.manifest.v2+json", v1.MediaTypeImageIndex, "application/vnd.docker.distribution.manifest.list.v2+json":
		data, err := readCIDescriptor(ctx, store, descriptor, maxCIImportDocument)
		if err != nil {
			return err
		}
		budget.metadata -= int64(len(data))
		// ORAS owns graph edges; retain Stoke's schema and config JSON bounds.
		var document struct {
			SchemaVersion int            `json:"schemaVersion"`
			Config        v1.Descriptor  `json:"config"`
			Subject       *v1.Descriptor `json:"subject"`
		}
		if json.Unmarshal(data, &document) != nil || document.SchemaVersion != 2 {
			return errors.New("invalid OCI result manifest or index")
		}
		if descriptor.MediaType == v1.MediaTypeImageManifest || descriptor.MediaType == "application/vnd.docker.distribution.manifest.v2+json" {
			if budget.configurations == nil {
				budget.configurations = map[digest.Digest]bool{}
			}
			if !budget.configurations[document.Config.Digest] {
				config, err := readCIDescriptor(ctx, store, document.Config, maxCIImportDocument)
				if err != nil {
					return err
				}
				budget.configurations[document.Config.Digest] = true
				budget.metadata -= int64(len(config))
			}
		}
		children, err = content.Successors(ctx, content.FetcherFunc(func(context.Context, v1.Descriptor) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		}), descriptor)
		if err != nil {
			return err
		}
		// Docker manifests do not define subjects, but retain verification of
		// this extension if present, as the previous importer did.
		if descriptor.MediaType == "application/vnd.docker.distribution.manifest.v2+json" && document.Subject != nil {
			children = append(children, *document.Subject)
		}
	default:
		if err := hashCIDescriptor(ctx, store, descriptor); err != nil {
			return err
		}
	}
	if budget.metadata < 0 {
		return errors.New("OCI result graph metadata exceeds its limit")
	}
	height := 0
	for _, child := range children {
		if err := verifyCIGraph(ctx, store, child, budget, depth+1); err != nil {
			return err
		}
		height = max(height, budget.heights[child.Digest]+1)
	}
	budget.heights[descriptor.Digest] = height
	return nil
}

func readCIDescriptor(ctx context.Context, store content.ReadOnlyStorage, descriptor v1.Descriptor, limit int64) ([]byte, error) {
	if validateCIDigest(descriptor.Digest.String()) != nil || descriptor.Size < 0 || descriptor.Size > limit {
		return nil, errors.New("invalid or oversized OCI identity document")
	}
	reader, err := store.Fetch(ctx, descriptor)
	if err != nil {
		return nil, err
	}
	data, err := content.ReadAll(&ciContextReader{ctx: ctx, reader: reader}, descriptor)
	err = errors.Join(err, reader.Close())
	if err != nil {
		return nil, err
	}
	if err := validateCIMetadataJSON(data); err != nil {
		return nil, err
	}
	return data, nil
}

func hashCIDescriptor(ctx context.Context, store content.ReadOnlyStorage, descriptor v1.Descriptor) error {
	reader, err := store.Fetch(ctx, descriptor)
	if err != nil {
		return err
	}
	verified := content.NewVerifyReader(&ciContextReader{ctx: ctx, reader: reader}, descriptor)
	_, err = io.Copy(io.Discard, verified)
	return errors.Join(err, verified.Verify(), reader.Close())
}

func readCIImage(ctx context.Context, directory, identity, platform string) (_ ciImage, retErr error) {
	traceStarted := time.Now()
	var traceInflatedBytes int64
	defer func() {
		fmt.Fprintf(os.Stderr, "STOKE_IMPORT_TRACE phase=read_image seconds=%.6f success=%t inflated_bytes=%d\n", time.Since(traceStarted).Seconds(), retErr == nil, traceInflatedBytes)
	}()
	var image ciImage
	store, roots, err := openCIStore(ctx, CIImport{Kind: "image", OCI: directory, Digest: identity})
	if err != nil {
		return image, err
	}
	image.root = roots[0]
	if image.root.MediaType != v1.MediaTypeImageManifest && image.root.MediaType != "application/vnd.docker.distribution.manifest.v2+json" {
		return image, errors.New("CI image must select a runnable image manifest")
	}
	data, err := readCIDescriptor(ctx, store, image.root, maxCIImportDocument)
	if err != nil {
		return image, err
	}
	if json.Unmarshal(data, &image.manifest) != nil || image.manifest.SchemaVersion != 2 || image.manifest.ArtifactType != "" {
		return image, errors.New("CI image result is not a runnable manifest")
	}
	if image.manifest.Config.MediaType != v1.MediaTypeImageConfig && image.manifest.Config.MediaType != "application/vnd.docker.container.image.v1+json" {
		return image, errors.New("CI image result has no runnable image configuration")
	}
	data, err = readCIDescriptor(ctx, store, image.manifest.Config, maxCIImportDocument)
	if err != nil {
		return image, err
	}
	if json.Unmarshal(data, &image.config) != nil {
		return image, errors.New("invalid OCI image configuration")
	}
	actual := image.config.OS + "/" + image.config.Architecture
	if image.config.Variant != "" {
		actual += "/" + image.config.Variant
	}
	actual, err = workflow.NormalizePlatform(actual)
	if err != nil || actual != platform {
		return image, errors.New("OCI image configuration differs from the selected platform")
	}
	if image.config.RootFS.Type != "layers" || len(image.config.RootFS.DiffIDs) != len(image.manifest.Layers) {
		return image, errors.New("OCI filesystem layer/diffID counts differ")
	}
	remaining := maxCIImportBytes
	for index, layer := range image.manifest.Layers {
		actual, size, err := ciLayerDiffID(ctx, store, layer, remaining)
		traceInflatedBytes += size
		if err != nil {
			return image, err
		}
		remaining -= size
		if actual != image.config.RootFS.DiffIDs[index] {
			return image, errors.New("OCI layer differs from its ordered uncompressed diffID")
		}
	}
	return image, nil
}

func verifyCIImportedImage(ctx context.Context, input CIImport, result coopr.Result) (retErr error) {
	traceStarted := time.Now()
	defer func() {
		fmt.Fprintf(os.Stderr, "STOKE_IMPORT_TRACE phase=verify_imported_image seconds=%.6f success=%t\n", time.Since(traceStarted).Seconds(), retErr == nil)
	}()
	actual, err := workflow.NormalizePlatform(result.Platform)
	if err != nil || actual != input.Platform || result.ImageID == "" {
		return errors.New("imported image has no native identity or a different platform")
	}
	traceReadStarted := time.Now()
	before, err := readCIImage(ctx, input.OCI, input.Digest, input.Platform)
	fmt.Fprintf(os.Stderr, "STOKE_IMPORT_TRACE phase=verify_read_before seconds=%.6f success=%t\n", time.Since(traceReadStarted).Seconds(), err == nil)
	if err != nil {
		return fmt.Errorf("exported image identity: %w", err)
	}
	traceReadStarted = time.Now()
	after, err := readCIImage(ctx, result.OCI, result.ManifestDigest, input.Platform)
	fmt.Fprintf(os.Stderr, "STOKE_IMPORT_TRACE phase=verify_read_after seconds=%.6f success=%t\n", time.Since(traceReadStarted).Seconds(), err == nil)
	if err != nil {
		return fmt.Errorf("imported image identity: %w", err)
	}
	if !reflect.DeepEqual(before.manifest.Config, after.manifest.Config) {
		return errors.New("imported OCI configuration differs from exact producer output")
	}
	a, b := before.manifest, after.manifest
	a.Layers, b.Layers = nil, nil
	if !reflect.DeepEqual(a, b) || len(before.manifest.Layers) != len(after.manifest.Layers) {
		return errors.New("imported OCI manifest differs from exact producer output")
	}
	for i, a := range before.manifest.Layers {
		b := after.manifest.Layers[i]
		var err error
		a.MediaType, _, err = ciLayerMediaType(a.MediaType)
		if err != nil {
			return err
		}
		b.MediaType, _, err = ciLayerMediaType(b.MediaType)
		if err != nil {
			return err
		}
		a.Digest, b.Digest, a.Size, b.Size = "", "", 0, 0
		if !reflect.DeepEqual(a, b) {
			return errors.New("imported OCI layer metadata differs from exact producer output")
		}
	}
	return nil
}

func ciLayerDiffID(ctx context.Context, store content.ReadOnlyStorage, descriptor v1.Descriptor, limit int64) (_ digest.Digest, size int64, retErr error) {
	traceStarted := time.Now()
	defer func() {
		fmt.Fprintf(os.Stderr, "STOKE_IMPORT_TRACE phase=layer_diffid seconds=%.6f success=%t inflated_bytes=%d\n", time.Since(traceStarted).Seconds(), retErr == nil, size)
	}()
	_, compressed, err := ciLayerMediaType(descriptor.MediaType)
	if err != nil {
		return "", 0, err
	}
	if validateCIDigest(descriptor.Digest.String()) != nil || descriptor.Size < 0 || descriptor.Size > maxCIImportBytes || limit < 0 {
		return "", 0, errors.New("invalid OCI layer descriptor")
	}
	reader, err := store.Fetch(ctx, descriptor)
	if err != nil {
		return "", 0, err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	verified := content.NewVerifyReader(&ciContextReader{ctx: ctx, reader: reader}, descriptor)
	var plain io.Reader = verified
	if compressed {
		gzipReader, err := gzip.NewReader(verified)
		if err != nil {
			return "", 0, err
		}
		defer func() { retErr = errors.Join(retErr, gzipReader.Close()) }()
		plain = gzipReader
	}
	hash := sha256.New()
	size, err = io.Copy(hash, io.LimitReader(&ciContextReader{ctx: ctx, reader: plain}, limit+1))
	if err != nil {
		return "", size, err
	}
	if size > limit {
		return "", size, errors.New("uncompressed OCI input exceeds the 32 GiB limit")
	}
	if err := verified.Verify(); err != nil {
		return "", size, fmt.Errorf("OCI layer differs from encoded descriptor size or digest: %w", err)
	}
	return digest.Digest("sha256:" + hex.EncodeToString(hash.Sum(nil))), size, nil
}

func ciLayerMediaType(value string) (string, bool, error) {
	switch value {
	case v1.MediaTypeImageLayer:
		return "oci", false, nil
	case v1.MediaTypeImageLayerGzip:
		return "oci", true, nil
	case "application/vnd.oci.image.layer.nondistributable.v1.tar":
		return "oci-nondistributable", false, nil
	case "application/vnd.oci.image.layer.nondistributable.v1.tar+gzip":
		return "oci-nondistributable", true, nil
	case "application/vnd.docker.image.rootfs.diff.tar":
		return "docker", false, nil
	case "application/vnd.docker.image.rootfs.diff.tar.gzip":
		return "docker", true, nil
	default:
		return "", false, fmt.Errorf("unsupported OCI layer compression %q for exact execution verification", value)
	}
}

type ciContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *ciContextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

// Preserve original attachment roots without treating arbitrary extra roots as
// part of the component. Coopr validates the retained Bale binding on load.
func validateCIComponentRoots(ctx context.Context, store content.ReadOnlyStorage, roots []v1.Descriptor, identity string) error {
	var selected v1.Descriptor
	for _, root := range roots {
		if root.Digest.String() == identity {
			selected = root
		}
	}
	for _, root := range roots {
		if root.MediaType == v1.MediaTypeImageIndex && root.Digest == selected.Digest {
			continue
		}
		if root.MediaType != v1.MediaTypeImageManifest {
			return errors.New("component archive contains an unsupported root type")
		}
		data, err := readCIDescriptor(ctx, store, root, maxCIImportDocument)
		if err != nil {
			return err
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return err
		}
		if root.Digest == selected.Digest {
			if manifest.Subject != nil || manifest.ArtifactType != "application/vnd.coopr.strio.dev.component.v1+json" {
				return errors.New("declared component result is not a Coopr component root")
			}
		} else if manifest.Subject == nil || manifest.Subject.Digest != selected.Digest || manifest.Subject.Size != selected.Size || manifest.Subject.MediaType != selected.MediaType {
			return errors.New("component archive attachment has a different subject")
		}
	}
	return nil
}

// Check container depth and collection sizes before typed JSON decoding can
// allocate a descriptor slice or annotation map. JSON documents themselves are
// already byte-bounded; this closes the small-elements/large-allocation case.
func validateCIMetadataJSON(data []byte) error {
	type frame struct {
		kind   json.Delim
		values int
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var stack []frame
	done := false
	for tokens := 0; ; tokens++ {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			if !done || len(stack) != 0 {
				return errors.New("incomplete OCI metadata object")
			}
			return nil
		}
		if err != nil {
			return err
		}
		if done || tokens >= maxCIImportNodes*64 {
			return errors.New("OCI metadata exceeds its token limit or has multiple documents")
		}
		delimiter, isDelimiter := token.(json.Delim)
		if len(stack) == 0 {
			if !isDelimiter || delimiter != '{' {
				return errors.New("OCI metadata must be a JSON object")
			}
			stack = append(stack, frame{kind: delimiter})
			continue
		}
		if isDelimiter && (delimiter == '}' || delimiter == ']') {
			stack = stack[:len(stack)-1]
			done = len(stack) == 0
			continue
		}
		parent := &stack[len(stack)-1]
		parent.values++
		limit := maxCIImportNodes * 2
		if parent.kind == '[' {
			limit = maxCIImportNodes
		}
		if parent.values > limit {
			return errors.New("OCI metadata collection exceeds its item limit")
		}
		if isDelimiter {
			if len(stack) >= 128 {
				return errors.New("OCI metadata exceeds its JSON depth limit")
			}
			stack = append(stack, frame{kind: delimiter})
		}
	}
}
