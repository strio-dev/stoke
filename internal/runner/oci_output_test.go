package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/oras-project/oras-go/v3/content"
	"github.com/oras-project/oras-go/v3/content/oci"
	"github.com/strio-dev/stoke/internal/workflow"
)

func readTestImageConfig(ctx context.Context, path, identity, platform string) (v1.Image, error) {
	var image v1.Image
	root, err := os.OpenRoot(path)
	if err != nil {
		return image, err
	}
	defer func() { _ = root.Close() }()
	store, err := oci.NewFromFS(ctx, root.FS())
	if err != nil {
		return image, err
	}
	selected, err := store.Resolve(ctx, identity)
	if err != nil {
		return image, err
	}
	raw, err := content.FetchAll(ctx, store, selected)
	if err != nil {
		return image, err
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return image, err
	}
	raw, err = content.FetchAll(ctx, store, manifest.Config)
	if err != nil {
		return image, err
	}
	if err := json.Unmarshal(raw, &image); err != nil {
		return image, err
	}
	actual := image.OS + "/" + image.Architecture
	if image.Variant != "" {
		actual += "/" + image.Variant
	}
	actual, err = workflow.NormalizePlatform(actual)
	if err != nil || actual != platform || image.RootFS.Type != "layers" || len(image.RootFS.DiffIDs) != len(manifest.Layers) {
		return image, fmt.Errorf("OCI image config disagrees with reported platform or manifest: %s", actual)
	}
	return image, nil
}
