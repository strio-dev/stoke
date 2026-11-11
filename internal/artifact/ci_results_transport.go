package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/stoke/internal/hostfiles"
	"github.com/strio-dev/stoke/internal/workflow"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
)

const maxCIObjects = 2 * MaxCIBindings
const maxCIMetadataTotal = 64 << 20

func ciReference(repository, reference string) (string, error) {
	if err := ValidateCIRepository(repository); err != nil {
		return "", err
	}
	ref, err := registry.ParseReference(reference)
	if err != nil {
		return "", err
	}
	if ref.Registry+"/"+ref.Repository != repository ||
		!strings.HasPrefix(reference, repository+"@sha256:") ||
		ref.ValidateReferenceAsDigest() != nil {
		return "", errors.New("CI inputs require a digest-pinned reference in the configured result repository")
	}
	if len(reference) > 1024 {
		return "", errors.New("CI result reference is too long")
	}
	return ref.Reference, nil
}

func (c Client) ciRepository(repository string) (*remote.Repository, error) {
	if err := ValidateCIRepository(repository); err != nil {
		return nil, err
	}
	repo, _, _, err := c.clients(repository)
	return repo, err
}

func ciDecode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("CI metadata must contain exactly one JSON document")
	}
	return nil
}

type ciReadSession struct {
	repo           *remote.Repository
	storage        content.ReadOnlyStorage
	repository     string
	scope          CIScope
	sets           map[string]CIInputSet
	results        map[string]CIResult
	descriptors    map[string]v1.Descriptor
	active         map[string]bool
	bytes, objects int
	metadata       map[digest.Digest]ciMetadata
}

func (c Client) ciSession(repository string, scope CIScope) (*ciReadSession, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	repo, err := c.ciRepository(repository)
	if err != nil {
		return nil, err
	}
	return &ciReadSession{repo: repo, repository: repository, scope: scope,
		sets: map[string]CIInputSet{}, results: map[string]CIResult{}, active: map[string]bool{}, descriptors: map[string]v1.Descriptor{}}, nil
}

func (s *ciReadSession) fetch(ctx context.Context, descriptor v1.Descriptor) ([]byte, error) {
	if err := ciDescriptor(descriptor); err != nil {
		return nil, err
	}
	if cached, exists := s.metadata[descriptor.Digest]; exists {
		if !content.Equal(cached.descriptor, descriptor) {
			return nil, errors.New("CI metadata descriptor size/type conflict")
		}
		return cached.data, nil
	}
	if descriptor.Size > MaxCIMetadataBytes || descriptor.Size > int64(maxCIMetadataTotal-s.bytes) {
		return nil, errors.New("CI metadata exceeds size budget")
	}
	storage := s.storage
	if storage == nil {
		storage = s.repo
	}
	data, err := fetch(ctx, storage, descriptor, MaxCIMetadataBytes)
	if err == nil {
		s.bytes += len(data)
		if s.metadata == nil {
			s.metadata = map[digest.Digest]ciMetadata{}
		}
		s.metadata[descriptor.Digest] = ciMetadata{descriptor: descriptor, data: data}
	}
	return data, err
}

// CI metadata is one index member; other manifest/index members retain graph
// relationships without requiring their envelope bytes in the blob namespace.
type ciObject struct {
	members []v1.Descriptor
	layers  []v1.Descriptor
}

func (object ciObject) validateInputSet(repository string, set CIInputSet, scope CIScope) ([]string, error) {
	if set.Version != CIVersion || set.Scope != scope || len(set.Bindings) > MaxCIBindings || len(object.members) != len(set.Bindings) || len(object.layers) != 0 {
		return nil, errors.New("CI input set scope, version or binding count mismatch")
	}
	names := make([]string, 0, len(set.Bindings))
	for name := range set.Bindings {
		names = append(names, name)
	}
	slices.Sort(names)
	for index, name := range names {
		if !ciText(name, 512) {
			return nil, errors.New("invalid CI binding producer")
		}
		selected, err := ciReference(repository, set.Bindings[name])
		if err != nil {
			return nil, err
		}
		if object.members[index].MediaType != v1.MediaTypeImageIndex || object.members[index].Digest.String() != selected {
			return nil, errors.New("CI input set descriptor does not match binding")
		}
	}
	return names, nil
}

func (object ciObject) validateResult(repository string, result CIResult) error {
	count := 1
	if result.Inputs != "" {
		count++
	}
	if len(object.members) != count || !content.Equal(object.members[0], result.Output) {
		return errors.New("CI result graph does not select its canonical output")
	}
	if result.Archive != nil {
		if len(object.layers) != 1 || !content.Equal(object.layers[0], *result.Archive) {
			return errors.New("CI component archive descriptor mismatch")
		}
	} else if len(object.layers) != 0 {
		return errors.New("CI image result must not contain archive layers")
	}
	if result.Inputs != "" {
		selected, err := ciReference(repository, result.Inputs)
		if err != nil {
			return err
		}
		if object.members[1].Digest.String() != selected || object.members[1].MediaType != v1.MediaTypeImageIndex {
			return errors.New("CI result input-set descriptor mismatch")
		}
	}
	return nil
}

func (s *ciReadSession) object(ctx context.Context, reference, artifactType, configType string, target any) (ciObject, error) {
	selected, err := ciReference(s.repository, reference)
	if err != nil {
		return ciObject{}, err
	}
	if s.objects >= maxCIObjects {
		return ciObject{}, errors.New("too many CI metadata objects")
	}
	s.objects++
	descriptor, err := s.repo.Resolve(ctx, selected)
	if err != nil {
		return ciObject{}, fmt.Errorf("required exact CI result is unavailable; rerun producer and dependent gates: %w", err)
	}
	if descriptor.Digest.String() != selected || descriptor.MediaType != v1.MediaTypeImageIndex {
		return ciObject{}, errors.New("CI metadata root descriptor mismatch")
	}
	if s.descriptors == nil {
		s.descriptors = map[string]v1.Descriptor{}
	}
	s.descriptors[reference] = descriptor
	data, err := s.fetch(ctx, descriptor)
	if err != nil {
		return ciObject{}, err
	}
	var index v1.Index
	if err := ciDecode(data, &index); err != nil {
		return ciObject{}, err
	}
	if index.SchemaVersion != 2 || index.MediaType != v1.MediaTypeImageIndex || index.ArtifactType != artifactType || index.Subject != nil || len(index.Manifests) == 0 || len(index.Manifests) > MaxCIBindings+1 {
		return ciObject{}, errors.New("not a supported CI metadata index")
	}
	for _, member := range index.Manifests {
		if err := ciDescriptor(member); err != nil {
			return ciObject{}, err
		}
		if member.MediaType != v1.MediaTypeImageManifest && member.MediaType != v1.MediaTypeImageIndex {
			return ciObject{}, errors.New("CI index member must be a manifest or index")
		}
	}
	if index.Manifests[0].MediaType != v1.MediaTypeImageManifest {
		return ciObject{}, errors.New("CI index lacks its metadata manifest")
	}
	data, err = s.fetch(ctx, index.Manifests[0])
	if err != nil {
		return ciObject{}, err
	}
	var manifest v1.Manifest
	if err := ciDecode(data, &manifest); err != nil {
		return ciObject{}, err
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != v1.MediaTypeImageManifest ||
		manifest.ArtifactType != artifactType || manifest.Config.MediaType != configType ||
		manifest.Subject != nil || manifest.Layers == nil || len(manifest.Layers) > 1 {
		return ciObject{}, errors.New("not a supported CI result metadata artifact")
	}
	for _, layer := range manifest.Layers {
		if err := ciDescriptor(layer); err != nil {
			return ciObject{}, err
		}
		if layer.MediaType != CIComponentArchiveType {
			return ciObject{}, errors.New("CI metadata layers may contain only the original component archive")
		}
	}
	data, err = s.fetch(ctx, manifest.Config)
	if err != nil {
		return ciObject{}, err
	}
	if err := ciDecode(data, target); err != nil {
		return ciObject{}, fmt.Errorf("CI metadata: %w", err)
	}
	return ciObject{members: index.Manifests[1:], layers: manifest.Layers}, nil
}

func (s *ciReadSession) inputSet(ctx context.Context, reference string) (CIInputSet, error) {
	if set, exists := s.sets[reference]; exists {
		return set, nil
	}
	if len(s.active) >= maxCIObjects || s.active[reference] {
		return CIInputSet{}, errors.New("cyclic or excessively deep CI lineage")
	}
	s.active[reference] = true
	defer delete(s.active, reference)
	var set CIInputSet
	object, err := s.object(ctx, reference, CIInputSetType, ciInputSetConfigType, &set)
	if err != nil {
		return set, err
	}
	names, err := object.validateInputSet(s.repository, set, s.scope)
	if err != nil {
		return set, err
	}
	for index, name := range names {
		ref := set.Bindings[name]
		member := object.members[index]
		result, err := s.result(ctx, ref)
		if err != nil {
			return set, err
		}
		if !content.Equal(member, s.descriptors[ref]) {
			return set, errors.New("CI input set descriptor size mismatch")
		}
		if result.Producer != name {
			return set, errors.New("CI input set producer mismatch")
		}
		if result.Inputs != "" {
			parents, err := s.inputSet(ctx, result.Inputs)
			if err != nil {
				return set, err
			}
			for ancestor, parent := range parents.Bindings {
				if set.Bindings[ancestor] != parent {
					return set, fmt.Errorf("CI input set omits or conflicts with producer %q lineage", ancestor)
				}
			}
		}
	}
	set.Bindings = maps.Clone(set.Bindings)
	s.sets[reference] = set
	return set, nil
}

func (s *ciReadSession) result(ctx context.Context, reference string) (CIResult, error) {
	if result, exists := s.results[reference]; exists {
		return result, nil
	}
	if len(s.active) >= maxCIObjects || s.active[reference] {
		return CIResult{}, errors.New("cyclic or excessively deep CI lineage")
	}
	s.active[reference] = true
	defer delete(s.active, reference)
	var result CIResult
	object, err := s.object(ctx, reference, CIResultType, ciResultConfigType, &result)
	if err != nil {
		return result, err
	}
	if err := result.validate(CIResultExpectation{Scope: s.scope}); err != nil {
		return result, err
	}
	if err := object.validateResult(s.repository, result); err != nil {
		return result, err
	}
	if result.Inputs != "" {
		parents, err := s.inputSet(ctx, result.Inputs)
		if err != nil {
			return result, err
		}
		if !content.Equal(object.members[1], s.descriptors[result.Inputs]) {
			return result, errors.New("CI input-set descriptor size mismatch")
		}
		if _, exists := parents.Bindings[result.Producer]; exists {
			return result, errors.New("CI result depends on an earlier execution of itself")
		}
		for _, ref := range parents.Bindings {
			parent, err := s.result(ctx, ref)
			if err != nil {
				return result, err
			}
			if parent.Attempt > result.Attempt {
				return result, errors.New("CI result cannot consume a future producer attempt")
			}
		}
	}
	if err := s.validateOutput(ctx, result); err != nil {
		return result, err
	}
	s.results[reference] = result
	return result, nil
}

func (s *ciReadSession) outputPlatform(ctx context.Context, result CIResult) (string, error) {
	data, err := s.fetch(ctx, result.Output)
	if err != nil {
		return "", err
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", err
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != v1.MediaTypeImageManifest || len(manifest.Layers) > MaxCIBindings {
		return "", errors.New("CI payload must be a supported single OCI manifest")
	}
	if err := ciDescriptor(manifest.Config); err != nil {
		return "", err
	}
	if manifest.Subject != nil {
		if err := ciDescriptor(*manifest.Subject); err != nil {
			return "", err
		}
	}
	for _, layer := range manifest.Layers {
		if err := ciDescriptor(layer); err != nil {
			return "", err
		}
	}
	if result.Kind == "component" {
		if manifest.ArtifactType != ciComponentType || manifest.Config.MediaType != ciComponentConfigType {
			return "", errors.New("CI component payload kind mismatch")
		}
	} else if result.Kind == "host" {
		if manifest.ArtifactType != hostfiles.MediaType || manifest.Config.MediaType != hostfiles.ConfigMediaType {
			return "", errors.New("CI host-file payload kind mismatch")
		}
	} else if result.Kind != "image" || manifest.ArtifactType != "" || manifest.Config.MediaType != v1.MediaTypeImageConfig {
		return "", errors.New("CI image payload kind mismatch")
	}
	data, err = s.fetch(ctx, manifest.Config)
	if err != nil {
		return "", err
	}
	var platform v1.Platform
	switch result.Kind {
	case "host":
		config, err := hostfiles.ValidateManifest(manifest, data)
		if err != nil {
			return "", err
		}
		platform = config.Platform
	case "component":
		var config struct {
			Platform v1.Platform `json:"platform"`
		}
		if err := json.Unmarshal(data, &config); err != nil {
			return "", err
		}
		platform = config.Platform
	default:
		var image v1.Image
		if err := json.Unmarshal(data, &image); err != nil {
			return "", err
		}
		if image.RootFS.Type != "layers" || len(image.RootFS.DiffIDs) != len(manifest.Layers) {
			return "", errors.New("CI image rootfs metadata mismatch")
		}
		for _, value := range image.RootFS.DiffIDs {
			if value.Validate() != nil || value.Algorithm() != digest.SHA256 {
				return "", errors.New("CI image rootfs digest mismatch")
			}
		}
		platform = image.Platform
	}
	value := platform.OS + "/" + platform.Architecture
	if platform.Variant != "" {
		value += "/" + platform.Variant
	}
	actual, err := workflow.NormalizePlatform(value)
	if err != nil {
		return "", errors.New("CI payload config has invalid platform")
	}
	return actual, nil
}

func (s *ciReadSession) validateOutput(ctx context.Context, result CIResult) error {
	actual, err := s.outputPlatform(ctx, result)
	if err != nil {
		return err
	}
	if actual != result.Platform {
		return errors.New("CI payload config platform mismatch")
	}
	return nil
}

// ReadCIInputSet verifies the selected set and its bounded metadata lineage.
// It fetches no ancestor image layers and never discovers results through tags.
func (c Client) ReadCIInputSet(ctx context.Context, repository, reference string, scope CIScope) (CIInputSet, error) {
	session, err := c.ciSession(repository, scope)
	if err != nil {
		return CIInputSet{}, err
	}
	return session.inputSet(ctx, reference)
}

func (c Client) ReadCIResult(ctx context.Context, repository, reference string, expect CIResultExpectation) (CIResult, error) {
	session, err := c.ciSession(repository, expect.Scope)
	if err != nil {
		return CIResult{}, err
	}
	result, err := session.result(ctx, reference)
	if err == nil {
		err = result.validate(expect)
	}
	return result, err
}

func (c Client) pushCIObject(ctx context.Context, repository, artifactType, configType string, scope CIScope, producer string, value any, children []v1.Descriptor) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	var members []v1.Descriptor
	layers := []v1.Descriptor{}
	for _, child := range children {
		if err := ciDescriptor(child); err != nil {
			return "", err
		}
		switch child.MediaType {
		case v1.MediaTypeImageManifest, v1.MediaTypeImageIndex:
			members = append(members, child)
		case CIComponentArchiveType:
			layers = append(layers, child)
		default:
			return "", errors.New("unsupported CI metadata child media type")
		}
	}
	if len(members) > MaxCIBindings || len(layers) > 1 {
		return "", errors.New("CI metadata child count exceeds limits")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if len(data) > MaxCIMetadataBytes {
		return "", errors.New("CI metadata exceeds size limit")
	}
	repo, err := c.ciRepository(repository)
	if err != nil {
		return "", err
	}
	config := content.NewDescriptorFromBytes(configType, data)
	if err := repo.Push(ctx, config, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return "", err
	}
	manifest := v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: artifactType,
		Config: config, Layers: layers, Annotations: map[string]string{v1.AnnotationCreated: "1970-01-01T00:00:00Z"},
	}
	data, err = json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	if len(data) > MaxCIMetadataBytes {
		return "", errors.New("CI metadata manifest exceeds size limit")
	}
	metadata := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, data)
	if err := repo.Push(ctx, metadata, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return "", err
	}
	index := v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, ArtifactType: artifactType,
		Manifests: append([]v1.Descriptor{metadata}, members...), Annotations: map[string]string{v1.AnnotationCreated: "1970-01-01T00:00:00Z"},
	}
	data, err = json.Marshal(index)
	if err != nil {
		return "", err
	}
	if len(data) > MaxCIMetadataBytes {
		return "", errors.New("CI metadata index exceeds size limit")
	}
	descriptor := content.NewDescriptorFromBytes(v1.MediaTypeImageIndex, data)
	if err := repo.Push(ctx, descriptor, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return "", err
	}
	scopeHash, _ := scope.Hash()
	producerHash := digest.FromString(producer).Encoded()
	tag := "sci-" + scopeHash[:16] + "-" + producerHash[:16] + "-" + descriptor.Digest.Encoded()
	if err := repo.Tag(ctx, descriptor, tag); err != nil {
		return "", fmt.Errorf("retain CI object: %w", err)
	}
	return repository + "@" + descriptor.Digest.String(), nil
}

func (c Client) PublishCIInputSet(ctx context.Context, repository string, set CIInputSet) (string, error) {
	if set.Version != CIVersion {
		return "", errors.New("unsupported CI input set version")
	}
	if err := set.Scope.Validate(); err != nil {
		return "", err
	}
	if len(set.Bindings) > MaxCIBindings {
		return "", errors.New("too many CI input bindings")
	}
	session, err := c.ciSession(repository, set.Scope)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(set.Bindings))
	for name := range set.Bindings {
		names = append(names, name)
	}
	slices.Sort(names)
	layers := make([]v1.Descriptor, 0, len(names))
	for _, name := range names {
		if !ciText(name, 512) {
			return "", errors.New("invalid CI binding producer")
		}
		ref := set.Bindings[name]
		result, err := session.result(ctx, ref)
		if err != nil {
			return "", err
		}
		if result.Producer != name {
			return "", errors.New("CI input set producer mismatch")
		}
		if result.Inputs != "" {
			parents, err := session.inputSet(ctx, result.Inputs)
			if err != nil {
				return "", err
			}
			for ancestor, parent := range parents.Bindings {
				if set.Bindings[ancestor] != parent {
					return "", fmt.Errorf("CI input set omits or conflicts with producer %q lineage", ancestor)
				}
			}
		}
		selected, _ := ciReference(repository, ref)
		descriptor, err := session.repo.Resolve(ctx, selected)
		if err != nil {
			return "", err
		}
		layers = append(layers, descriptor)
	}
	set.Bindings = maps.Clone(set.Bindings)
	return c.pushCIObject(ctx, repository, CIInputSetType, ciInputSetConfigType, set.Scope, "inputs", set, layers)
}

// MergeCIInputSets reuses an incoming set equal to the union, so a gate with one
// producer needs only registry read access. A new union requires metadata write.
func (c Client) MergeCIInputSets(ctx context.Context, repository string, scope CIScope, references []string) (string, CIInputSet, error) {
	session, err := c.ciSession(repository, scope)
	if err != nil {
		return "", CIInputSet{}, err
	}
	sets := make([]CIInputSet, 0, len(references))
	refs := []string{}
	for _, ref := range references {
		if ref == "" || slices.Contains(refs, ref) {
			continue
		}
		set, err := session.inputSet(ctx, ref)
		if err != nil {
			return "", CIInputSet{}, err
		}
		sets = append(sets, set)
		refs = append(refs, ref)
	}
	bindings, err := MergeCIBindings(sets...)
	if err != nil {
		return "", CIInputSet{}, err
	}
	union := CIInputSet{Version: CIVersion, Scope: scope, Bindings: bindings}
	if len(bindings) == 0 {
		return "", union, nil
	}
	for i, set := range sets {
		if maps.Equal(set.Bindings, bindings) {
			return refs[i], union, nil
		}
	}
	ref, err := c.PublishCIInputSet(ctx, repository, union)
	return ref, union, err
}

func (c Client) PublishCIResult(ctx context.Context, repository string, result CIResult, payloadPath, manifestDigest string) (string, error) {
	result.Output = v1.Descriptor{}
	if err := result.Scope.Validate(); err != nil {
		return "", err
	}
	if err := ValidateCIRepository(repository); err != nil {
		return "", err
	}
	identity, err := digest.Parse(manifestDigest)
	if err != nil || identity.Algorithm() != digest.SHA256 {
		return "", errors.New("CI payload requires canonical exported manifest digest")
	}
	if result.Kind == "host" {
		if _, err := hostfiles.Validate(ctx, payloadPath, manifestDigest); err != nil {
			return "", fmt.Errorf("invalid captured host output: %w", err)
		}
	}
	var sourceFS fs.FS
	info, err := os.Stat(payloadPath)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		root, err := os.OpenRoot(payloadPath)
		if err != nil {
			return "", err
		}
		defer func() { _ = root.Close() }()
		sourceFS = root.FS()
	} else {
		if !info.Mode().IsRegular() || info.Size() > MaxCITransferBytes {
			return "", errors.New("CI component/archive output exceeds encoded size budget")
		}
		sourceFS, err = newCIArchiveFS(ctx, payloadPath)
		if err != nil {
			return "", err
		}
	}
	store, err := newCILocalSource(ctx, sourceFS, manifestDigest)
	if err != nil {
		return "", err
	}
	descriptor, err := store.Resolve(ctx, manifestDigest)
	if err != nil {
		return "", err
	}
	result.Output = descriptor
	if result.Kind == "component" {
		if !info.Mode().IsRegular() {
			return "", errors.New("CI component output must be its original OCI archive file")
		}
		file, err := os.Open(payloadPath)
		if err != nil {
			return "", err
		}
		digester := digest.SHA256.Digester()
		size, copyErr := io.Copy(digester.Hash(), ciContextReader{ctx: ctx, reader: io.LimitReader(file, info.Size()+1)})
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return "", err
		}
		if size != info.Size() {
			return "", errors.New("CI component archive changed while hashing")
		}
		archive := v1.Descriptor{MediaType: CIComponentArchiveType, Digest: digester.Digest(), Size: size}
		result.Archive = &archive
	} else {
		result.Archive = nil
	}
	local := &ciReadSession{storage: store}
	actualPlatform, err := local.outputPlatform(ctx, result)
	if err != nil {
		return "", err
	}
	if result.Platform == "" {
		result.Platform = actualPlatform
	}
	if actualPlatform != result.Platform {
		return "", errors.New("CI payload config platform mismatch")
	}
	if err := result.validate(CIResultExpectation{Scope: result.Scope}); err != nil {
		return "", err
	}
	repo, err := c.ciRepository(repository)
	if err != nil {
		return "", err
	}
	session := &ciReadSession{repo: repo, repository: repository, scope: result.Scope, sets: map[string]CIInputSet{}, results: map[string]CIResult{}, active: map[string]bool{}, descriptors: map[string]v1.Descriptor{}}
	layers := []v1.Descriptor{descriptor}
	if result.Archive != nil {
		layers = append(layers, *result.Archive)
	}
	if result.Inputs != "" {
		inputs, err := session.inputSet(ctx, result.Inputs)
		if err != nil {
			return "", err
		}
		if _, exists := inputs.Bindings[result.Producer]; exists {
			return "", errors.New("CI result cannot depend on itself")
		}
		for _, ref := range inputs.Bindings {
			parent, err := session.result(ctx, ref)
			if err != nil {
				return "", err
			}
			if parent.Attempt > result.Attempt {
				return "", errors.New("CI result cannot consume a future producer attempt")
			}
		}
		selected, _ := ciReference(repository, result.Inputs)
		inputDesc, err := repo.Resolve(ctx, selected)
		if err != nil {
			return "", err
		}
		layers = append(layers, inputDesc)
	}
	// Validate the complete encoded graph before uploading payload bodies.
	initialMetadata := session.bytes + local.bytes
	if initialMetadata > maxCIMetadataTotal {
		return "", errors.New("CI payload lineage exceeds metadata budget")
	}
	bounded, err := preflightCIPayload(ctx, store, descriptor, local.metadata, initialMetadata)
	if err != nil {
		return "", err
	}
	if result.Archive != nil {
		if result.Archive.Size > MaxCITransferBytes-bounded.encodedBytes {
			return "", errors.New("CI component representations exceed aggregate transfer budget")
		}
	}
	if result.Archive != nil {
		file, err := os.Open(payloadPath)
		if err != nil {
			return "", err
		}
		pushErr := repo.Push(ctx, *result.Archive, io.LimitReader(file, result.Archive.Size+1))
		closeErr := file.Close()
		if pushErr != nil && !errors.Is(pushErr, errdef.ErrAlreadyExists) {
			return "", errors.Join(pushErr, closeErr)
		}
		if closeErr != nil {
			return "", closeErr
		}
	}

	// Copy immutable bytes, not a storage ID or a newly committed representation.
	if err := bounded.copyForeignLayers(ctx, repo); err != nil {
		return "", err
	}
	copied, err := oras.Copy(ctx, bounded, manifestDigest, repo, manifestDigest, oras.DefaultCopyOptions)
	if err != nil {
		return "", err
	}
	if !content.Equal(copied, descriptor) {
		return "", errors.New("CI payload transport changed its canonical descriptor")
	}
	if err := session.validateOutput(ctx, result); err != nil {
		return "", err
	}
	scopeHash, _ := result.Scope.Hash()
	producerHash := digest.FromString(result.Producer).Encoded()
	payloadTag := "scip-" + scopeHash[:16] + "-" + producerHash[:16] + "-" + result.Output.Digest.Encoded()
	if err := repo.Tag(ctx, result.Output, payloadTag); err != nil {
		return "", fmt.Errorf("retain CI payload: %w", err)
	}
	return c.pushCIObject(ctx, repository, CIResultType, ciResultConfigType, result.Scope, result.Producer, result, layers)
}

func (c Client) FetchCIPayload(ctx context.Context, repository, reference string, expect CIResultExpectation, directory string) (payload CIPayload, retErr error) {
	session, err := c.ciSession(repository, expect.Scope)
	if err != nil {
		return payload, err
	}
	result, err := session.result(ctx, reference)
	if err != nil {
		return payload, err
	}
	if err := result.validate(expect); err != nil {
		return payload, err
	}
	var bounded *ciPayloadSource
	if result.Kind == "component" {
		if err := validateCIArchiveTransfer(*result.Archive, session.bytes); err != nil {
			return payload, err
		}
	} else {
		bounded, err = preflightCIPayload(ctx, session.repo, result.Output, session.metadata, session.bytes)
		if err != nil {
			return payload, err
		}
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		return payload, errors.New("CI payload destination must be fresh")
	}
	staging, err := os.MkdirTemp(filepath.Dir(directory), ".ci-payload-")
	if err != nil {
		return payload, err
	}
	defer func() {
		if retErr != nil {
			_ = os.RemoveAll(staging)
		}
	}()
	relative := "image"
	if result.Kind == "host" {
		relative = "files"
	}
	repo, err := c.ciRepository(repository)
	if err != nil {
		return payload, err
	}
	selected := result.Output.Digest.String()
	if result.Kind == "component" {
		relative = "component.oci.tar"
		out, err := os.OpenFile(filepath.Join(staging, relative), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return payload, err
		}
		rawReader, err := repo.Fetch(ctx, *result.Archive)
		if err != nil {
			_ = out.Close()
			return payload, fmt.Errorf("required exact component archive unavailable; rerun producer and dependent gates: %w", err)
		}
		reader := &ciBoundedStream{ctx: ctx, reader: rawReader, remaining: result.Archive.Size, budget: &ciStreamBudget{remaining: MaxCITransferBytes - int64(session.bytes)}}
		verified := content.NewVerifyReader(reader, *result.Archive)
		_, copyErr := io.Copy(out, verified)
		if err := errors.Join(copyErr, verified.Verify(), reader.Close(), out.Close()); err != nil {
			return payload, fmt.Errorf("CI component archive integrity mismatch: %w", err)
		}
		archiveFS, err := newCIArchiveFS(ctx, filepath.Join(staging, relative))
		if err != nil {
			return payload, err
		}
		store, err := newCILocalSource(ctx, archiveFS, selected)
		if err != nil {
			return payload, err
		}
		actual, err := store.Resolve(ctx, selected)
		if err != nil {
			return payload, err
		}
		if !content.Equal(actual, result.Output) {
			return payload, errors.New("CI component archive root mismatch")
		}
	} else {
		store, err := oci.New(filepath.Join(staging, relative))
		if err != nil {
			return payload, err
		}
		if err := bounded.copyForeignLayers(ctx, store); err != nil {
			return payload, fmt.Errorf("required exact CI payload is unavailable; rerun producer and dependent gates: %w", err)
		}
		copied, err := oras.Copy(ctx, bounded, selected, store, "result", oras.DefaultCopyOptions)
		if err != nil {
			return payload, fmt.Errorf("required exact CI payload is unavailable; rerun producer and dependent gates: %w", err)
		}
		if !content.Equal(copied, result.Output) {
			return payload, errors.New("downloaded CI payload descriptor mismatch")
		}
	}
	if err := ctx.Err(); err != nil {
		return payload, err
	}
	if result.Kind == "host" {
		if _, err := hostfiles.Validate(ctx, filepath.Join(staging, relative), selected); err != nil {
			return payload, fmt.Errorf("invalid captured host output: %w", err)
		}
	}
	if err := os.Rename(staging, directory); err != nil {
		return payload, err
	}
	return CIPayload{Result: result, Path: filepath.Join(directory, relative), CanonicalReference: repository + "@" + selected}, nil
}

// DeleteCIObject removes only a verified result/InputSet metadata root in the
// exact invocation scope. Call only after writers stop and the retry window
// expires. Registries may reject deletion. Payload blobs/manifests are never
// deleted here: they can be shared by other results or caches.
func (c Client) DeleteCIObject(ctx context.Context, repository, reference string, scope CIScope) error {
	session, err := c.ciSession(repository, scope)
	if err != nil {
		return err
	}
	selected, err := ciReference(repository, reference)
	if err != nil {
		return err
	}
	descriptor, err := session.repo.Resolve(ctx, selected)
	if err != nil {
		return err
	}
	if descriptor.Digest.String() != selected || descriptor.MediaType != v1.MediaTypeImageIndex {
		return errors.New("refusing to delete a non-CI metadata index")
	}
	data, err := session.fetch(ctx, descriptor)
	if err != nil {
		return err
	}
	var index v1.Index
	if err := ciDecode(data, &index); err != nil {
		return err
	}
	switch index.ArtifactType {
	case CIResultType:
		var result CIResult
		object, err := session.object(ctx, reference, CIResultType, ciResultConfigType, &result)
		if err != nil {
			return err
		}
		if err := result.validate(CIResultExpectation{Scope: scope}); err != nil {
			return err
		}
		if err := object.validateResult(repository, result); err != nil {
			return err
		}
	case CIInputSetType:
		var set CIInputSet
		object, err := session.object(ctx, reference, CIInputSetType, ciInputSetConfigType, &set)
		if err != nil {
			return err
		}
		if _, err := object.validateInputSet(repository, set, scope); err != nil {
			return err
		}
	default:
		return errors.New("refusing to delete a non-CI metadata object")
	}
	if err := session.repo.Delete(ctx, descriptor); err != nil {
		return fmt.Errorf("registry did not delete scoped CI metadata: %w", err)
	}
	return nil
}
