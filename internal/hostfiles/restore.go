package hostfiles

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// ValidateManifest verifies host-file metadata without fetching its layer. The
// layer body must separately pass Validate or Restore before being consumed.
func ValidateManifest(manifest v1.Manifest, raw []byte) (Config, error) {
	var config Config
	if manifest.SchemaVersion != 2 || manifest.MediaType != v1.MediaTypeImageManifest || manifest.ArtifactType != MediaType || manifest.Subject != nil || manifest.Config.MediaType != ConfigMediaType || len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != LayerMediaType {
		return config, errors.New("invalid host-file artifact manifest")
	}
	if err := descriptor(manifest.Config, MaxMetadata); err != nil {
		return config, err
	}
	if err := descriptor(manifest.Layers[0], MaxBytes); err != nil {
		return config, err
	}
	if len(raw) > MaxMetadata {
		return config, errors.New("host-file inventory exceeds metadata limit")
	}
	if int64(len(raw)) != manifest.Config.Size || digest.FromBytes(raw) != manifest.Config.Digest {
		return config, errors.New("host-file config integrity mismatch")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return config, errors.New("host-file config requires one JSON document")
	}
	actual, err := platform(config.Platform.OS + "/" + config.Platform.Architecture)
	if err != nil || config.Platform.Variant != "" || !equalPlatform(config.Platform, actual) {
		return config, errors.New("invalid host-file platform")
	}
	if config.Version != 1 || !validKey(config.InputKey) || len(config.Entries) > MaxEntries {
		return config, errors.New("invalid host-file inventory version, key or size")
	}
	paths, err := selectedPaths(config.Paths)
	if err != nil || !slices.Equal(paths, config.Paths) {
		return config, errors.New("host-file selected paths are not canonical")
	}
	entries := map[string]Entry{}
	var total int64
	previous := ""
	for _, entry := range config.Entries {
		if !cleanName(entry.Path, false) || entry.Path <= previous || entry.Mode > 0777 || entry.Size < 0 {
			return config, errors.New("invalid host-file inventory entry")
		}
		previous = entry.Path
		allowed := false
		for _, selected := range paths {
			if selected == "." || entry.Path == selected || strings.HasPrefix(entry.Path, selected+"/") || strings.HasPrefix(selected, entry.Path+"/") {
				allowed = true
				break
			}
		}
		if !allowed {
			return config, errors.New("host-file inventory includes undeclared output")
		}
		if entry.Directory {
			if entry.Size != 0 || entry.Digest != "" {
				return config, errors.New("invalid host directory metadata")
			}
		} else {
			hash, err := digest.Parse(entry.Digest)
			if err != nil || hash.Algorithm() != digest.SHA256 || entry.Size > MaxBytes-total {
				return config, errors.New("invalid host file content metadata")
			}
			total += entry.Size
		}
		for parent := path.Dir(entry.Path); parent != "."; parent = path.Dir(parent) {
			prior, ok := entries[parent]
			if !ok || !prior.Directory {
				return config, errors.New("host-file inventory has an absent or non-directory parent")
			}
		}
		entries[entry.Path] = entry
	}
	for _, selected := range paths {
		if selected != "." {
			if _, ok := entries[selected]; !ok {
				return config, errors.New("declared host output missing from inventory")
			}
		}
	}
	return config, nil
}
func equalPlatform(a, b v1.Platform) bool {
	return a.OS == b.OS && a.Architecture == b.Architecture && a.OSVersion == "" && len(a.OSFeatures) == 0 && a.Variant == b.Variant
}
func descriptor(d v1.Descriptor, maxSize int64) error {
	if d.Digest.Validate() != nil || d.Digest.Algorithm() != digest.SHA256 || d.Size < 0 || d.Size > maxSize || len(d.URLs) > 0 || len(d.Data) > 0 {
		return errors.New("invalid host-file OCI descriptor")
	}
	return nil
}

func readSmall(ctx context.Context, root *os.Root, name string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("invalid host-file OCI metadata file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(contextReader{ctx, io.LimitReader(file, limit+1)})
	err = errors.Join(readErr, file.Close())
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("host-file metadata exceeds limit")
	}
	return data, nil
}
func readBlob(ctx context.Context, root *os.Root, d v1.Descriptor, limit int64) ([]byte, error) {
	if err := descriptor(d, limit); err != nil {
		return nil, err
	}
	data, err := readSmall(ctx, root, "blobs/sha256/"+d.Digest.Encoded(), limit)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != d.Size || digest.FromBytes(data) != d.Digest {
		return nil, errors.New("host-file OCI blob integrity mismatch")
	}
	return data, nil
}
func metadata(ctx context.Context, root *os.Root, identity string) (v1.Manifest, Config, error) {
	var manifest v1.Manifest
	var config Config
	hash, err := digest.Parse(identity)
	if err != nil || hash.Algorithm() != digest.SHA256 {
		return manifest, config, errors.New("host files require SHA256 manifest identity")
	}
	raw, err := readSmall(ctx, root, "oci-layout", MaxMetadata)
	if err != nil {
		return manifest, config, err
	}
	var layout v1.ImageLayout
	if json.Unmarshal(raw, &layout) != nil || layout.Version != v1.ImageLayoutVersion {
		return manifest, config, errors.New("invalid host-file OCI layout")
	}
	raw, err = readSmall(ctx, root, "index.json", MaxMetadata)
	if err != nil {
		return manifest, config, err
	}
	var index v1.Index
	if json.Unmarshal(raw, &index) != nil || index.SchemaVersion != 2 || len(index.Manifests) != 1 || index.Manifests[0].Digest != hash || index.Manifests[0].MediaType != v1.MediaTypeImageManifest {
		return manifest, config, errors.New("host-file OCI index does not select exact manifest")
	}
	raw, err = readBlob(ctx, root, index.Manifests[0], MaxMetadata)
	if err != nil {
		return manifest, config, err
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return manifest, config, err
	}
	raw, err = readBlob(ctx, root, manifest.Config, MaxMetadata)
	if err != nil {
		return manifest, config, err
	}
	config, err = ValidateManifest(manifest, raw)
	return manifest, config, err
}

// Validate checks the selected immutable layout, including every captured byte.
func Validate(ctx context.Context, layout, identity string) (Config, error) {
	root, err := os.OpenRoot(layout)
	if err != nil {
		return Config{}, err
	}
	defer func() { _ = root.Close() }()
	manifest, config, err := metadata(ctx, root, identity)
	if err != nil {
		return config, err
	}
	return config, readLayer(ctx, root, manifest.Layers[0], config, nil)
}

// Restore materializes a verified capture into a fresh destination. Failure
// removes only this operation's private staging tree, never caller data.
func Restore(ctx context.Context, layout, identity, destination string) (retErr error) {
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return errors.New("host-file restore destination must be fresh")
	}
	root, err := os.OpenRoot(layout)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	manifest, config, err := metadata(ctx, root, identity)
	if err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(destination), ".host-restore-")
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			// A verified archive may contain read-only directories. Restore
			// write access only inside this operation's private staging tree
			// before removing it after a failed atomic commit.
			cleanup, err := os.OpenRoot(staging)
			if err == nil {
				for _, entry := range config.Entries {
					if entry.Directory {
						_ = cleanup.Chmod(entry.Path, 0700)
					}
				}
				err = cleanup.Close()
			}
			retErr = errors.Join(retErr, err, os.RemoveAll(staging))
		}
	}()
	target, err := os.OpenRoot(staging)
	if err != nil {
		return err
	}
	readErr := readLayer(ctx, root, manifest.Layers[0], config, target)
	if err := errors.Join(readErr, target.Close()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return Commit(staging, destination)
}

func readLayer(ctx context.Context, source *os.Root, layer v1.Descriptor, config Config, target *os.Root) error {
	name := "blobs/sha256/" + layer.Digest.Encoded()
	info, err := source.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != layer.Size {
		return errors.New("invalid host-file layer")
	}
	file, err := source.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	limited := &io.LimitedReader{R: contextReader{ctx, file}, N: layer.Size + 1}
	reader := io.TeeReader(limited, hash)
	archive := tar.NewReader(reader)
	for _, entry := range config.Entries {
		header, err := archive.Next()
		if err != nil {
			return fmt.Errorf("host-file archive differs from inventory: %w", err)
		}
		if header.Name != entry.Path || header.Mode != int64(entry.Mode) || header.Size != entry.Size || header.Linkname != "" || header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" || !header.ModTime.Equal(time.Unix(0, 0)) || !header.AccessTime.IsZero() || !header.ChangeTime.IsZero() {
			return errors.New("host-file archive metadata differs from inventory")
		}
		for key := range header.PAXRecords {
			if key != "path" {
				return errors.New("host-file archive has unsupported extended metadata")
			}
		}
		kind := byte(tar.TypeReg)
		if entry.Directory {
			kind = tar.TypeDir
		}
		if header.Typeflag != kind {
			return errors.New("host-file archive contains unsupported file type")
		}
		if entry.Directory {
			if target != nil {
				if err := target.MkdirAll(entry.Path, 0700); err != nil {
					return err
				}
			}
			continue
		}
		output := io.Discard
		var out *os.File
		if target != nil {
			if err := target.MkdirAll(path.Dir(entry.Path), 0700); err != nil {
				return err
			}
			out, err = target.OpenFile(entry.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			output = out
		}
		contentHash := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(output, contentHash), contextReader{ctx, archive})
		if out != nil {
			copyErr = errors.Join(copyErr, out.Chmod(os.FileMode(entry.Mode)), out.Close())
		}
		if copyErr != nil {
			return copyErr
		}
		if n != entry.Size || "sha256:"+hex.EncodeToString(contentHash.Sum(nil)) != entry.Digest {
			return errors.New("host-file content differs from inventory")
		}
	}
	if _, err := archive.Next(); !errors.Is(err, io.EOF) {
		return errors.New("host-file archive has extra entries or invalid trailer")
	}
	buffer := make([]byte, 32768)
	for {
		n, readErr := reader.Read(buffer)
		for _, b := range buffer[:n] {
			if b != 0 {
				return errors.New("host-file archive has nonzero trailing data")
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if layer.Size%512 != 0 || limited.N != 1 || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != layer.Digest.String() {
		return errors.New("host-file layer integrity mismatch")
	}
	if target != nil {
		paths := make([]string, 0, len(config.Entries))
		for _, entry := range config.Entries {
			paths = append(paths, entry.Path)
		}
		if err := normalizeTimes(ctx, target, paths); err != nil {
			return err
		}
		for i := len(config.Entries) - 1; i >= 0; i-- {
			entry := config.Entries[i]
			if entry.Directory {
				if err := target.Chmod(entry.Path, os.FileMode(entry.Mode)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
