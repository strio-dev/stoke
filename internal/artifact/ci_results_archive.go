package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/stoke/internal/ociarchive"
	"oras.land/oras-go/v2"
)

// ORAS's OCI constructors eagerly index every root; its tar constructor also
// scans without context checks. This small read-only adapter selects only the
// canonical root, with bounded metadata and cancellation-aware archive seeks.
type ciLocalSource struct {
	fsys fs.FS
	root v1.Descriptor
}

func newCILocalSource(ctx context.Context, fsys fs.FS, selected string) (oras.ReadOnlyTarget, error) {
	read := func(name string) ([]byte, error) {
		file, err := fsys.Open(name)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > maxCIPayloadDocumentBytes {
			_ = file.Close()
			return nil, errors.New("OCI layout metadata exceeds limit or is not regular")
		}
		data, readErr := io.ReadAll(io.LimitReader(ciContextReader{ctx: ctx, reader: file}, maxCIPayloadDocumentBytes+1))
		if err := errors.Join(readErr, file.Close()); err != nil {
			return nil, err
		}
		if len(data) > maxCIPayloadDocumentBytes {
			return nil, errors.New("OCI layout metadata exceeds document budget")
		}
		return data, nil
	}
	raw, err := read("oci-layout")
	if err != nil {
		return nil, err
	}
	var layout v1.ImageLayout
	if err := json.Unmarshal(raw, &layout); err != nil {
		return nil, err
	}
	if layout.Version != v1.ImageLayoutVersion {
		return nil, errors.New("unsupported OCI image layout")
	}
	raw, err = read("index.json")
	if err != nil {
		return nil, err
	}
	var index v1.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		return nil, err
	}
	if index.SchemaVersion != 2 || len(index.Manifests) > MaxCITransferNodes {
		return nil, errors.New("OCI root index exceeds bounds or has invalid schema")
	}
	var root v1.Descriptor
	for _, descriptor := range index.Manifests {
		if descriptor.Digest.String() != selected {
			continue
		}
		if err := ciDescriptor(descriptor); err != nil {
			return nil, err
		}
		if root.Digest != "" && (root.Size != descriptor.Size || root.MediaType != descriptor.MediaType) {
			return nil, errors.New("OCI root index has conflicting selected descriptor")
		}
		root = descriptor
	}
	if root.Digest == "" {
		return nil, errors.New("canonical CI output is absent from OCI root index")
	}
	return &ciLocalSource{fsys: fsys, root: root}, nil
}
func (s *ciLocalSource) Resolve(ctx context.Context, reference string) (v1.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return v1.Descriptor{}, err
	}
	if reference != s.root.Digest.String() {
		return v1.Descriptor{}, errors.New("OCI source only resolves selected root")
	}
	return s.root, nil
}
func (s *ciLocalSource) Fetch(ctx context.Context, descriptor v1.Descriptor) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ciDescriptor(descriptor); err != nil {
		return nil, err
	}
	file, err := s.fsys.Open("blobs/sha256/" + descriptor.Digest.Encoded())
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != descriptor.Size {
		_ = file.Close()
		return nil, errors.New("OCI source blob size/type does not match descriptor")
	}
	return &ciContextReadCloser{ciContextReader: ciContextReader{ctx: ctx, reader: io.LimitReader(file, descriptor.Size)}, closer: file}, nil
}
func (s *ciLocalSource) Exists(ctx context.Context, descriptor v1.Descriptor) (bool, error) {
	file, err := s.Fetch(ctx, descriptor)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, file.Close()
}

type ciContextReadCloser struct {
	ciContextReader
	closer io.Closer
}

func (r *ciContextReadCloser) Close() error { return r.closer.Close() }

func newCIArchiveFS(ctx context.Context, filename string) (fs.FS, error) {
	return ociarchive.New(ctx, filename, MaxCITransferNodes+16)
}
