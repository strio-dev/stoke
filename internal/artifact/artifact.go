// Package artifact exchanges Stoke workflow definitions as OCI artifacts and
// preserves Bale dependency declarations and verified delivery mappings.
package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/stoke/internal/assets"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

const (
	Type               = "application/vnd.strio.stoke.workflow.v1"
	LayerType          = "application/vnd.strio.stoke.workflow.v1+yaml"
	FileType           = "application/vnd.strio.stoke.file.v1"
	FileModeAnnotation = "io.strio.stoke.file.mode"
	MaxBytes           = 1 << 20
)

type Client struct {
	PlainHTTP bool
	AuthFile  string
	ciRead    *ciReadSession
}
type Document struct {
	Data         []byte
	Reference    string
	Dependencies map[string]string
	Files        map[string]assets.File
}

func (c Client) clients(reference string) (*remote.Repository, *bale.Client, string, error) {
	if strings.Contains(reference, "://") {
		return nil, nil, "", errors.New("OCI references must not contain a transport prefix")
	}
	ref, err := registry.ParseReference(reference)
	if err != nil {
		return nil, nil, "", err
	}
	var store credentials.Store
	if c.AuthFile == "" {
		store, err = credentials.NewStoreFromDocker(credentials.StoreOptions{})
	} else {
		store, err = credentials.NewStore(c.AuthFile, credentials.StoreOptions{})
	}
	if err != nil {
		return nil, nil, "", err
	}
	credential := credentials.Credential(store)
	repo, err := remote.NewRepository(ref.Registry + "/" + ref.Repository)
	if err != nil {
		return nil, nil, "", err
	}
	repo.PlainHTTP = c.PlainHTTP
	repo.Client = &auth.Client{Client: auth.DefaultClient.Client, Cache: auth.NewCache(), Credential: credential}
	opts := bale.ClientOptions{Credentials: credential}
	if c.PlainHTTP {
		opts.PlainHTTPRegistries = []string{ref.Registry}
	}
	b, err := bale.NewClient(opts)
	if err != nil {
		return nil, nil, "", err
	}
	tag := ref.Reference
	if tag == "" {
		tag = "latest"
	}
	return repo, b, tag, nil
}

func (c Client) Push(ctx context.Context, reference string, data []byte, dependencies []string, files map[string]assets.File) (string, error) {
	if len(data) == 0 || len(data) > MaxBytes {
		return "", errors.New("workflow must contain 1 to 1048576 bytes")
	}
	if err := assets.Validate(files); err != nil {
		return "", fmt.Errorf("workflow assets before publication: %w", err)
	}
	declaration := bale.DependencyDeclaration{SchemaVersion: 1}
	for _, ref := range dependencies {
		if ref == "scratch" || strings.HasPrefix(ref, "oci:") {
			continue
		}
		declaration.Dependencies = append(declaration.Dependencies, bale.DependencyReference{Ref: ref})
	}
	if _, err := bale.EncodeDependencyDeclaration(declaration); err != nil {
		return "", fmt.Errorf("workflow dependencies require explicit registry hosts and tags or digests before publication: %w", err)
	}

	repo, b, tag, err := c.clients(reference)
	if err != nil {
		return "", err
	}
	store := memory.New()
	layer := content.NewDescriptorFromBytes(LayerType, data)
	if err = store.Push(ctx, layer, bytes.NewReader(data)); err != nil {
		return "", err
	}
	layers := []v1.Descriptor{layer}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		file := files[name]
		mode := "0644"
		if file.Executable {
			mode = "0755"
		}
		descriptor := content.NewDescriptorFromBytes(FileType, file.Data)
		descriptor.Annotations = map[string]string{v1.AnnotationTitle: name, FileModeAnnotation: mode}
		if err = store.Push(ctx, descriptor, bytes.NewReader(file.Data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			return "", err
		}
		layers = append(layers, descriptor)
	}
	manifest, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, Type, oras.PackManifestOptions{Layers: layers, ManifestAnnotations: map[string]string{v1.AnnotationCreated: "1970-01-01T00:00:00Z"}})
	if err != nil {
		return "", err
	}
	if manifest.Size > MaxBytes {
		return "", errors.New("workflow manifest metadata exceeds size limit")
	}
	if err = store.Tag(ctx, manifest, "workflow"); err != nil {
		return "", err
	}
	published, err := oras.Copy(ctx, store, "workflow", repo, tag, oras.DefaultCopyOptions)
	if err != nil {
		return "", err
	}
	pinned := repo.Reference.Registry + "/" + repo.Reference.Repository + "@" + published.Digest.String()
	request, err := bale.PrepareDependencyDeclaration(ctx, bale.Root{Repository: repo.Reference.Registry + "/" + repo.Reference.Repository, Descriptor: published}, declaration)
	if err != nil {
		return "", fmt.Errorf("workflow published as %s; prepare dependencies: %w", pinned, err)
	}
	attached, err := b.Attach(ctx, request)
	if err != nil {
		return "", fmt.Errorf("workflow published as %s; attach dependencies: %w", pinned, err)
	}
	if !attached.Published || !attached.Discoverable {
		return "", fmt.Errorf("workflow published as %s but dependency declaration is not discoverable", pinned)
	}
	return pinned, nil
}

func (c Client) Pull(ctx context.Context, reference string) (*Document, error) {
	repo, b, tag, err := c.clients(reference)
	if err != nil {
		return nil, err
	}
	descriptor, err := repo.Resolve(ctx, tag)
	if err != nil {
		return nil, err
	}
	if descriptor.MediaType != v1.MediaTypeImageManifest {
		return nil, errors.New("workflow descriptor must identify an OCI image manifest")
	}
	raw, err := fetch(ctx, repo, descriptor, MaxBytes)
	if err != nil {
		return nil, err
	}
	var manifest v1.Manifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != v1.MediaTypeImageManifest || manifest.ArtifactType != Type || len(manifest.Layers) == 0 || len(manifest.Layers) > assets.MaxFiles+1 || manifest.Layers[0].MediaType != LayerType || manifest.Config.MediaType != v1.MediaTypeEmptyJSON {
		return nil, errors.New("not a supported Stoke workflow artifact")
	}
	config, err := fetch(ctx, repo, manifest.Config, MaxBytes)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(config, []byte("{}")) {
		return nil, errors.New("workflow config must be OCI empty JSON")
	}
	data, err := fetch(ctx, repo, manifest.Layers[0], MaxBytes)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("empty workflow artifact")
	}
	files, err := readFiles(ctx, repo, manifest.Layers[1:])
	if err != nil {
		return nil, err
	}
	pinned := repo.Reference.Registry + "/" + repo.Reference.Repository + "@" + descriptor.Digest.String()
	records, err := b.InspectDestinationsWithOptions(ctx, pinned, bale.DestinationInspectionOptions{MaxMetadataBytes: 8 * MaxBytes, MaxReferrers: 128})
	if err != nil {
		return nil, fmt.Errorf("inspect workflow delivery metadata: %w", err)
	}
	mappings, err := deliveryMappings(records, pinned, descriptor)
	if err != nil {
		return nil, err
	}
	return &Document{Data: data, Reference: pinned, Dependencies: mappings, Files: files}, nil
}

func readFiles(ctx context.Context, store content.ReadOnlyStorage, layers []v1.Descriptor) (map[string]assets.File, error) {
	if len(layers) > assets.MaxFiles {
		return nil, errors.New("too many workflow file layers")
	}
	files := make(map[string]assets.File, len(layers))
	var total int64
	for _, layer := range layers {
		name := layer.Annotations[v1.AnnotationTitle]
		mode := layer.Annotations[FileModeAnnotation]
		if layer.MediaType != FileType || (mode != "0644" && mode != "0755") {
			return nil, errors.New("unsupported workflow file layer or mode")
		}
		if _, exists := files[name]; exists {
			return nil, fmt.Errorf("duplicate workflow file %q", name)
		}
		if layer.Size < 0 || layer.Size > assets.MaxFileBytes {
			return nil, errors.New("workflow file exceeds size limit")
		}
		total += layer.Size
		if total > assets.MaxTotalBytes {
			return nil, errors.New("workflow files exceed total size limit")
		}
		files[name] = assets.File{Executable: mode == "0755"}
	}
	if err := assets.Validate(files); err != nil {
		return nil, err
	}
	for _, layer := range layers {
		data, err := fetch(ctx, store, layer, assets.MaxFileBytes)
		if err != nil {
			return nil, err
		}
		name := layer.Annotations[v1.AnnotationTitle]
		file := files[name]
		file.Data = data
		files[name] = file
	}
	return files, nil
}

func fetch(ctx context.Context, store content.ReadOnlyStorage, descriptor v1.Descriptor, limit int64) ([]byte, error) {
	if descriptor.Size < 0 || descriptor.Size > limit {
		return nil, errors.New("OCI descriptor exceeds workflow size limit")
	}
	reader, err := store.Fetch(ctx, descriptor)
	if err != nil {
		return nil, err
	}
	data, err := content.ReadAll(reader, descriptor)
	err = errors.Join(err, reader.Close())
	if err != nil {
		return nil, err
	}
	return data, nil
}

func deliveryMappings(records []bale.InspectedDestination, pinned string, subject v1.Descriptor) (map[string]string, error) {
	mappings := map[string]string{}
	var selected []byte
	for _, record := range records {
		if !record.Local || record.Record.Destination != pinned || !content.Equal(record.Subject, subject) {
			continue
		}
		for _, resolution := range record.Record.Resolutions {
			encoded, err := json.Marshal(resolution)
			if err != nil {
				return nil, err
			}
			if selected != nil && !bytes.Equal(selected, encoded) {
				return nil, errors.New("ambiguous Bale delivery resolutions for workflow")
			}
			selected = encoded
			for _, dependency := range resolution.Dependencies {
				for _, source := range []string{dependency.Ref, dependency.Source, dependency.Destination} {
					if previous, ok := mappings[source]; ok && previous != dependency.Destination {
						return nil, fmt.Errorf("ambiguous delivered dependency %q", source)
					}
					mappings[source] = dependency.Destination
				}
			}
		}
	}
	return mappings, nil
}
