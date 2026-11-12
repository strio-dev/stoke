package main

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/platforms"
	"github.com/moby/buildkit/util/system"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func verifyOutput(output, platform, imageDigest, configDigest string) error {
	image, err := digest.Parse(imageDigest)
	if err != nil || image.Algorithm() != digest.SHA256 {
		return errors.New("exporter returned no sha256 image digest")
	}
	config, err := digest.Parse(configDigest)
	if err != nil || config.Algorithm() != digest.SHA256 {
		return errors.New("exporter returned no sha256 config digest")
	}
	info, err := os.Lstat(filepath.Join(output, "stoke"))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0755 {
		return errors.New("exported stoke must be a regular executable with mode0755")
	}
	file, err := os.Open(filepath.Join(output, "stoke"))
	if err != nil {
		return err
	}
	hasher := digest.SHA256.Digester()
	_, readErr := io.Copy(hasher.Hash(), file)
	if err := errors.Join(readErr, file.Close()); err != nil {
		return err
	}
	archivePath := output + ".oci.tar"
	wanted := map[string][]byte{"index.json": nil, "blobs/sha256/" + image.Encoded(): nil, "blobs/sha256/" + config.Encoded(): nil}
	if err := readArchiveMetadata(archivePath, wanted); err != nil {
		return err
	}
	var index v1.Index
	if err := json.Unmarshal(wanted["index.json"], &index); err != nil {
		return err
	}
	if index.SchemaVersion != 2 || len(index.Manifests) != 1 || index.Manifests[0].Digest != image || index.Manifests[0].MediaType != v1.MediaTypeImageManifest {
		return errors.New("OCI archive root differs from exporter image manifest")
	}
	manifestJSON := wanted["blobs/sha256/"+image.Encoded()]
	if index.Manifests[0].Size != int64(len(manifestJSON)) || digest.FromBytes(manifestJSON) != image {
		return errors.New("OCI root manifest differs from its descriptor size or digest")
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return err
	}
	if manifest.SchemaVersion != 2 || manifest.Config.Digest != config || manifest.Config.MediaType != v1.MediaTypeImageConfig || len(manifest.Layers) != 1 || manifest.Subject != nil {
		return errors.New("OCI root must bind the exported config and one scratch layer")
	}
	configJSON := wanted["blobs/sha256/"+config.Encoded()]
	if manifest.Config.Size != int64(len(configJSON)) || digest.FromBytes(configJSON) != config {
		return errors.New("OCI config differs from its manifest descriptor size or digest")
	}
	var cfg v1.Image
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return err
	}
	expected, err := platforms.Parse(platform)
	if err != nil {
		return err
	}
	if !platforms.OnlyStrict(expected).Match(cfg.Platform) || cfg.Config.WorkingDir != "/" || cfg.Config.User != "" || len(cfg.Config.Cmd) != 0 || len(cfg.Config.Entrypoint) != 0 || len(cfg.Config.Env) != 1 || cfg.Config.Env[0] != "PATH="+system.DefaultPathEnvUnix {
		return errors.New("OCI functional scratch configuration differs from frozen recipe")
	}
	if cfg.RootFS.Type != "layers" || len(cfg.RootFS.DiffIDs) != 1 {
		return errors.New("OCI config must bind exactly one scratch DiffID")
	}
	layer := manifest.Layers[0]
	switch layer.MediaType {
	case v1.MediaTypeImageLayer, v1.MediaTypeImageLayerGzip, v1.MediaTypeImageLayerZstd:
	default:
		return errors.New("unsupported scratch layer media type")
	}
	if err := verifyArchiveLayer(archivePath, layer, cfg.RootFS.DiffIDs[0], hasher.Digest(), info.Size(), info.Mode().Perm()); err != nil {
		return err
	}
	emit(map[string]any{"phase": "output-proof", "sourceCommit": sourceCommit, "platform": platforms.Format(cfg.Platform), "binarySHA256": hasher.Digest(), "binaryMode": fmt.Sprintf("%04o", info.Mode().Perm()), "binarySize": info.Size(), "imageDigest": image, "configDigest": config, "layerDigest": layer.Digest, "layerDiffID": cfg.RootFS.DiffIDs[0], "layerBinaryMatchesFilesystem": true, "functionalConfig": cfg.Config, "metadataPolicy": "BuildKit metadata; fixed epoch0; compare own cold/warm OCI identity only"})
	return nil
}

func readArchiveMetadata(name string, wanted map[string][]byte) (retErr error) {
	file, err := os.Open(name)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := path.Clean(header.Name)
		previous, needed := wanted[name]
		if !needed {
			continue
		}
		if previous != nil || header.Size < 0 || header.Size > 1<<20 || header.Typeflag != tar.TypeReg {
			return errors.New("duplicate or invalid OCI proof metadata")
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		wanted[name] = data
	}
	for _, data := range wanted {
		if data == nil {
			return errors.New("OCI archive lacks required proof metadata")
		}
	}
	return nil
}

func verifyArchiveLayer(name string, layer v1.Descriptor, diffID, binary digest.Digest, size int64, mode os.FileMode) (retErr error) {
	if layer.Digest.Validate() != nil || layer.Size < 0 || diffID.Validate() != nil {
		return errors.New("invalid OCI layer descriptor or DiffID")
	}
	file, err := os.Open(name)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	reader := tar.NewReader(file)
	found := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if path.Clean(header.Name) != "blobs/"+layer.Digest.Algorithm().String()+"/"+layer.Digest.Encoded() {
			continue
		}
		if found || header.Typeflag != tar.TypeReg || header.Size != layer.Size {
			return errors.New("duplicate or mismatched OCI layer entry")
		}
		found = true
		if err := verifyScratchLayer(reader, layer, diffID, binary, size, mode); err != nil {
			return err
		}
	}
	if !found {
		return errors.New("OCI archive lacks its final scratch layer")
	}
	return nil
}

func verifyScratchLayer(source io.Reader, layer v1.Descriptor, diffID, binary digest.Digest, size int64, mode os.FileMode) (retErr error) {
	limited := &io.LimitedReader{R: source, N: layer.Size}
	compressedDigest := layer.Digest.Algorithm().Digester()
	compressed := io.TeeReader(limited, compressedDigest.Hash())
	plain, err := compression.DecompressStream(compressed)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := plain.Close()
		_, drainErr := io.Copy(io.Discard, compressed)
		var verifyErr error
		if limited.N != 0 {
			verifyErr = io.ErrUnexpectedEOF
		} else if compressedDigest.Digest() != layer.Digest {
			verifyErr = errors.New("compressed OCI layer differs from its digest")
		}
		retErr = errors.Join(retErr, closeErr, drainErr, verifyErr)
	}()
	uncompressed := diffID.Algorithm().Digester()
	stream := io.TeeReader(plain, uncompressed.Hash())
	reader := tar.NewReader(stream)
	found := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := path.Clean(header.Name)
		if path.IsAbs(header.Name) || name == ".." || strings.HasPrefix(name, "../") {
			return errors.New("scratch layer contains an escaping path")
		}
		if header.Typeflag == tar.TypeDir && name == "." {
			continue
		}
		if name != "stoke" || header.Typeflag != tar.TypeReg || found || header.Mode&07777 != int64(mode) || header.Size != size {
			return errors.New("scratch layer contains unexpected content or binary metadata")
		}
		found = true
		hasher := binary.Algorithm().Digester()
		if _, err := io.Copy(hasher.Hash(), reader); err != nil {
			return err
		}
		if hasher.Digest() != binary {
			return errors.New("OCI layer stoke differs from filesystem output")
		}
	}
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return err
	}
	if !found || uncompressed.Digest() != diffID {
		return errors.New("scratch layer lacks stoke or differs from config DiffID")
	}
	return nil
}
