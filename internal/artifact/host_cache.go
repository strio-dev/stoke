package artifact

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/oras-project/oras-go/v3"
	"github.com/oras-project/oras-go/v3/content"
	"github.com/oras-project/oras-go/v3/content/oci"
	"github.com/oras-project/oras-go/v3/errdef"
	"github.com/oras-project/oras-go/v3/registry/remote"
	"github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/hostfiles"
)

const maxHostCacheMetadata = 4 << 20

func hostCacheTag(key string) (string, error) {
	if len(key) != 64 {
		return "", errors.New("host cache requires a SHA256 input key")
	}
	if _, err := hex.DecodeString(key); err != nil {
		return "", err
	}
	return "stoke-host-v1-" + key, nil
}

// Host cache tags are disposable input-key lookup hints. The payload config
// binds the key and platform before the archive is fetched.
func (c Client) FetchHostCache(ctx context.Context, repository, key, platform, destination string) (identity string, hit bool, retErr error) {
	tag, err := hostCacheTag(key)
	if err != nil {
		return "", false, err
	}
	repo, err := c.hostCacheRepository(repository)
	if err != nil {
		return "", false, err
	}
	root, err := repo.Resolve(ctx, tag)
	if errors.Is(err, errdef.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if root.MediaType != v1.MediaTypeImageManifest || root.Digest.Validate() != nil ||
		root.Digest.Algorithm() != digest.SHA256 || root.Size < 0 || root.Size > maxHostCacheMetadata ||
		len(root.URLs) != 0 || len(root.Data) != 0 {
		return "", false, errors.New("invalid host cache manifest descriptor")
	}
	data, err := content.FetchAll(ctx, repo, root)
	if err != nil {
		return "", false, err
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", false, err
	}
	configDesc := manifest.Config
	if configDesc.MediaType != hostfiles.ConfigMediaType || configDesc.Digest.Validate() != nil ||
		configDesc.Digest.Algorithm() != digest.SHA256 || configDesc.Size < 0 || configDesc.Size > maxHostCacheMetadata ||
		len(configDesc.URLs) != 0 || len(configDesc.Data) != 0 {
		return "", false, errors.New("invalid host cache config descriptor")
	}
	data, err = content.FetchAll(ctx, repo, configDesc)
	if err != nil {
		return "", false, err
	}
	config, err := hostfiles.ValidateManifest(manifest, data)
	if err != nil {
		return "", false, err
	}
	if config.InputKey != key {
		return "", false, errors.New("registry host cache payload has a different input key")
	}
	if config.Platform.OS+"/"+config.Platform.Architecture != platform {
		return "", false, errors.New("registry host cache payload has a different platform")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return "", false, errors.New("host cache destination must be fresh")
	}
	staging, err := os.MkdirTemp(filepath.Dir(destination), ".host-cache-")
	if err != nil {
		return "", false, err
	}
	defer func() {
		if retErr != nil {
			_ = os.RemoveAll(staging)
		}
	}()
	store, err := oci.New(staging)
	if err != nil {
		return "", false, err
	}
	options := oras.DefaultCopyGraphOptions
	options.Concurrency = 1
	options.MaxMetadataBytes = maxHostCacheMetadata
	if err := oras.CopyGraph(ctx, repo, store, root, options); err != nil {
		return "", false, err
	}
	if err := store.Tag(ctx, root, "host"); err != nil {
		return "", false, err
	}
	if _, err := hostfiles.Validate(ctx, staging, root.Digest.String()); err != nil {
		return "", false, err
	}
	if err := hostfiles.Commit(staging, destination); err != nil {
		return "", false, err
	}
	return root.Digest.String(), true, nil
}

func (c Client) PublishHostCache(ctx context.Context, repository, key, layout, identity string) error {
	tag, err := hostCacheTag(key)
	if err != nil {
		return err
	}
	config, err := hostfiles.Validate(ctx, layout, identity)
	if err != nil {
		return err
	}
	if config.InputKey != key {
		return errors.New("host cache payload input key mismatch")
	}
	root, err := os.OpenRoot(layout)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	store, err := oci.NewFromFS(ctx, root.FS())
	if err != nil {
		return err
	}
	repo, err := c.hostCacheRepository(repository)
	if err != nil {
		return err
	}
	options := oras.DefaultCopyOptions
	options.Concurrency = 1
	options.MaxMetadataBytes = maxHostCacheMetadata
	_, err = oras.Copy(ctx, store, identity, repo, tag, options)
	return err
}

func (c Client) hostCacheRepository(repository string) (*remote.Repository, error) {
	if err := engine.ValidateCacheRepository(repository); err != nil {
		return nil, err
	}
	repo, _, _, err := c.clients(repository)
	return repo, err
}
