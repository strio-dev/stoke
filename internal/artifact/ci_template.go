package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const CITemplateType = "application/vnd.strio.stoke.ci-template.v1"
const CITemplateLayerType = "application/vnd.strio.stoke.ci-template.v1+gotemplate"

// PullCITemplate reads a single size- and digest-verified plain template.
func (c Client) PullCITemplate(ctx context.Context, reference string, limit int64) ([]byte, error) {
	repo, _, tag, err := c.clients(reference)
	if err != nil {
		return nil, err
	}
	desc, err := repo.Resolve(ctx, tag)
	if err != nil {
		return nil, fmt.Errorf("resolve OCI CI template: %w", err)
	}
	if desc.MediaType != v1.MediaTypeImageManifest {
		return nil, errors.New("CI template requires an OCI image manifest")
	}
	raw, err := fetch(ctx, repo, desc, limit)
	if err != nil {
		return nil, err
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != v1.MediaTypeImageManifest || manifest.ArtifactType != CITemplateType || manifest.Config.Digest != v1.DescriptorEmptyJSON.Digest || manifest.Config.Size != v1.DescriptorEmptyJSON.Size || manifest.Config.MediaType != v1.MediaTypeEmptyJSON || len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != CITemplateLayerType || manifest.Layers[0].Size == 0 {
		return nil, errors.New("unsupported CI template artifact: expected one plain template layer and empty OCI config")
	}
	return fetch(ctx, repo, manifest.Layers[0], limit)
}
