package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
)

const (
	MaxCITransferBytes        int64 = 32 << 30
	MaxCITransferNodes              = 8192
	MaxCITransferDepth              = 128
	maxCIPayloadDocumentBytes       = 8 << 20
)

type ciMetadata struct {
	descriptor v1.Descriptor
	data       []byte
}

// ciPayloadSource preflights exactly the graph ORAS will copy. No layer bodies
// are read until descriptor size, node/depth and all metadata budgets pass.
type ciPayloadSource struct {
	base          oras.ReadOnlyTarget
	root          v1.Descriptor
	allowed       map[digest.Digest]v1.Descriptor
	metadata      map[digest.Digest]ciMetadata
	encodedBytes  int64
	metadataBytes int
	budget        *ciStreamBudget
}

func preflightCIPayload(ctx context.Context, base oras.ReadOnlyTarget, root v1.Descriptor, metadata map[digest.Digest]ciMetadata, initialBytes int) (*ciPayloadSource, error) {
	source := &ciPayloadSource{base: base, root: root, allowed: map[digest.Digest]v1.Descriptor{}, metadata: map[digest.Digest]ciMetadata{}, encodedBytes: int64(initialBytes), metadataBytes: initialBytes}
	for key, value := range metadata {
		source.metadata[key] = value
	}
	active := map[digest.Digest]bool{}
	heights := map[digest.Digest]int{}
	var walk func(v1.Descriptor, int) error
	walk = func(node v1.Descriptor, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > MaxCITransferDepth {
			return errors.New("CI payload graph exceeds depth limit")
		}
		if err := ciDescriptor(node); err != nil {
			return err
		}
		if previous, exists := source.allowed[node.Digest]; exists {
			if !content.Equal(previous, node) {
				return errors.New("CI payload has conflicting descriptor size or media type")
			}
			if active[node.Digest] {
				return errors.New("CI payload graph is cyclic")
			}
			if depth+heights[node.Digest] > MaxCITransferDepth {
				return errors.New("CI payload graph exceeds depth limit")
			}
			return nil
		}
		if len(source.allowed) >= MaxCITransferNodes {
			return errors.New("CI payload graph exceeds node limit")
		}
		source.allowed[node.Digest] = node
		if cached, exists := source.metadata[node.Digest]; exists {
			if !content.Equal(cached.descriptor, node) {
				return errors.New("CI payload conflicts with verified metadata descriptor")
			}
		} else {
			if node.Size > MaxCITransferBytes-source.encodedBytes {
				return errors.New("CI payload graph exceeds encoded byte budget")
			}
			source.encodedBytes += node.Size
		}
		active[node.Digest] = true
		defer delete(active, node.Digest)
		if !ciPayloadMetadataType(node.MediaType) {
			heights[node.Digest] = 0
			return nil
		}
		children, err := content.Successors(ctx, ciGraphMetadataFetcher{source}, node)
		if err != nil {
			return err
		}
		height := 0
		for _, child := range children {
			if err := walk(child, depth+1); err != nil {
				return err
			}
			height = max(height, heights[child.Digest]+1)
		}
		heights[node.Digest] = height
		if depth+height > MaxCITransferDepth {
			return errors.New("CI payload graph exceeds depth limit")
		}
		return nil
	}
	if err := walk(root, 0); err != nil {
		return nil, err
	}
	if source.encodedBytes > MaxCITransferBytes {
		return nil, errors.New("CI payload exceeds transfer budget")
	}
	source.budget = &ciStreamBudget{remaining: MaxCITransferBytes - int64(source.metadataBytes)}
	return source, nil
}

type ciGraphMetadataFetcher struct{ source *ciPayloadSource }

func (f ciGraphMetadataFetcher) Fetch(ctx context.Context, node v1.Descriptor) (io.ReadCloser, error) {
	source := f.source
	if cached, exists := source.metadata[node.Digest]; exists {
		if !content.Equal(cached.descriptor, node) {
			return nil, errors.New("CI payload metadata descriptor conflict")
		}
		return io.NopCloser(bytes.NewReader(cached.data)), nil
	}
	if node.Size > maxCIPayloadDocumentBytes || node.Size > int64(maxCIMetadataTotal-source.metadataBytes) {
		return nil, errors.New("CI payload metadata exceeds document or aggregate budget")
	}
	data, err := fetch(ctx, source.base, node, maxCIPayloadDocumentBytes)
	if err != nil {
		return nil, err
	}
	source.metadataBytes += len(data)
	source.metadata[node.Digest] = ciMetadata{descriptor: node, data: data}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *ciPayloadSource) Resolve(ctx context.Context, reference string) (v1.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return v1.Descriptor{}, err
	}
	if reference != s.root.Digest.String() {
		return v1.Descriptor{}, errors.New("CI payload resolver only permits its selected root")
	}
	return s.root, nil
}
func (s *ciPayloadSource) Exists(ctx context.Context, node v1.Descriptor) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	known, exists := s.allowed[node.Digest]
	return exists && content.Equal(known, node), nil
}
func (s *ciPayloadSource) Fetch(ctx context.Context, node v1.Descriptor) (io.ReadCloser, error) {
	known, exists := s.allowed[node.Digest]
	if !exists || !content.Equal(known, node) {
		return nil, errors.New("CI copy requested a node outside its preflight graph")
	}
	if cached, exists := s.metadata[node.Digest]; exists {
		return io.NopCloser(bytes.NewReader(cached.data)), nil
	}
	reader, err := s.base.Fetch(ctx, node)
	if err != nil {
		return nil, err
	}
	return &ciBoundedStream{ctx: ctx, reader: reader, remaining: node.Size, budget: s.budget}, nil
}

type ciStreamBudget struct {
	mu        sync.Mutex
	remaining int64
}
type ciBoundedStream struct {
	ctx       context.Context
	reader    io.ReadCloser
	remaining int64
	budget    *ciStreamBudget
}

func (r *ciBoundedStream) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	r.budget.mu.Lock()
	allowed := min(int64(len(p)), r.remaining, r.budget.remaining)
	if allowed <= 0 {
		r.budget.mu.Unlock()
		return 0, errors.New("CI streaming transfer byte budget exhausted")
	}
	r.budget.remaining -= allowed
	r.budget.mu.Unlock()
	n, err := r.reader.Read(p[:int(allowed)])
	r.budget.mu.Lock()
	r.budget.remaining += allowed - int64(n)
	r.budget.mu.Unlock()
	r.remaining -= int64(n)
	return n, err
}
func (r *ciBoundedStream) Close() error { return r.reader.Close() }

type ciContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r ciContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func validateCIArchiveTransfer(descriptor v1.Descriptor, metadataBytes int) error {
	if err := ciDescriptor(descriptor); err != nil {
		return err
	}
	if descriptor.Size > MaxCITransferBytes-int64(metadataBytes) {
		return fmt.Errorf("CI component archive exceeds %d-byte transfer budget", MaxCITransferBytes)
	}
	return nil
}

// Ensure known OCI manifest/index descendants have the same treatment as
// content.Successors; arbitrary leaf media types remain opaque bounded blobs.
func ciPayloadMetadataType(mediaType string) bool {
	return slices.Contains([]string{v1.MediaTypeImageManifest, v1.MediaTypeImageIndex,
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.artifact.manifest.v1+json"}, mediaType)
}
