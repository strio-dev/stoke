package artifact

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/stoke/internal/hostfiles"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

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
// binds the key; exact CI consumers still select the separate immutable result.
func (c Client) FetchHostCache(ctx context.Context, repository, key, platform, destination string) (identity string, hit bool, retErr error) {
	tag, err := hostCacheTag(key)
	if err != nil {
		return "", false, err
	}
	repo, err := c.ciRepository(repository)
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
	session := &ciReadSession{repo: repo}
	result := CIResult{Kind: "host", Output: root, Platform: platform}
	if err := session.validateOutput(ctx, result); err != nil {
		return "", false, err
	}
	data, err := session.fetch(ctx, root)
	if err != nil {
		return "", false, err
	}
	var manifest v1.Manifest
	if err := ciDecode(data, &manifest); err != nil {
		return "", false, err
	}
	configData, err := session.fetch(ctx, manifest.Config)
	if err != nil {
		return "", false, err
	}
	var config hostfiles.Config
	if err := json.Unmarshal(configData, &config); err != nil {
		return "", false, err
	}
	if config.InputKey != key {
		return "", false, errors.New("registry host cache payload has a different input key")
	}
	bounded, err := preflightCIPayload(ctx, repo, root, session.metadata, session.bytes)
	if err != nil {
		return "", false, err
	}
	if bounded.encodedBytes > hostfiles.MaxBytes+2*hostfiles.MaxMetadata {
		return "", false, errors.New("host cache transfer exceeds bounded capture size")
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
	options := oras.DefaultCopyOptions
	options.Concurrency = 1
	copied, err := oras.Copy(ctx, bounded, root.Digest.String(), store, "host", options)
	if err != nil {
		return "", false, err
	}
	if copied.Digest != root.Digest {
		return "", false, errors.New("host cache copy changed identity")
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
	store, err := newCILocalSource(ctx, root.FS(), identity)
	if err != nil {
		return err
	}
	descriptor, err := store.Resolve(ctx, identity)
	if err != nil {
		return err
	}
	bounded, err := preflightCIPayload(ctx, store, descriptor, nil, 0)
	if err != nil {
		return err
	}
	if bounded.encodedBytes > hostfiles.MaxBytes+2*hostfiles.MaxMetadata {
		return errors.New("host cache transfer exceeds bounded capture size")
	}
	repo, err := c.ciRepository(repository)
	if err != nil {
		return err
	}
	options := oras.DefaultCopyOptions
	options.Concurrency = 1
	_, err = oras.Copy(ctx, bounded, identity, repo, tag, options)
	return err
}
