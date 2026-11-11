// Package ociarchive supplies bounded, cancellation-aware archive access to ORAS.
// ORAS's tar constructor lacks these admission limits; graph and digest handling
// remain in ORAS, rather than a second archive-backed content store.
package ociarchive

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"
)

const (
	MaxBytes         int64 = 32 << 30
	MaxMetadataBytes int64 = 64 << 20
	MaxDocumentBytes int64 = 8 << 20
	MaxEntries             = 8194
)

// OpenRegular does not follow a final symlink or block opening a FIFO.
func OpenRegular(name string) (*os.File, error) {
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.Join(errors.New("OCI content must be a regular file"), err)
	}
	return file, nil
}

type archiveEntry struct {
	offset int64
	header *tar.Header
}
type archiveFS struct {
	ctx      context.Context
	filename string
	entries  map[string]archiveEntry
	info     fs.FileInfo
}
type archiveFile struct {
	ctx    context.Context
	reader *io.SectionReader
	file   *os.File
	info   fs.FileInfo
}

func (f *archiveFile) Read(p []byte) (int, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	return f.reader.Read(p)
}
func (f *archiveFile) Close() error               { return f.file.Close() }
func (f *archiveFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *archiveFS) Open(name string) (fs.File, error) {
	if err := f.ctx.Err(); err != nil {
		return nil, err
	}
	if !fs.ValidPath(name) {
		return nil, fs.ErrInvalid
	}
	entry, exists := f.entries[name]
	if !exists {
		return nil, fs.ErrNotExist
	}
	file, err := OpenRegular(f.filename)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !os.SameFile(info, f.info) || info.Size() != f.info.Size() {
		_ = file.Close()
		return nil, errors.Join(errors.New("OCI archive changed after indexing"), err)
	}
	return &archiveFile{ctx: f.ctx, reader: io.NewSectionReader(file, entry.offset, entry.header.Size), file: file, info: entry.header.FileInfo()}, nil
}

type contextSeekReader struct {
	ctx               context.Context
	file              *os.File
	metadataRemaining *int64
}

func (r contextSeekReader) Read(p []byte) (int, error) {
	if *r.metadataRemaining <= 0 {
		return 0, errors.New("OCI archive indexing exceeds metadata byte budget")
	}
	limit := min(int64(len(p)), *r.metadataRemaining)
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.file.Read(p[:int(limit)])
	*r.metadataRemaining -= int64(n)
	return n, err
}
func (r contextSeekReader) Seek(offset int64, whence int) (int64, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.file.Seek(offset, whence)
}

// New indexes regular OCI members without reading blob bodies or extracting files.
// maxHeaders retains the caller's admission limit, including ignored directories.
func New(ctx context.Context, filename string, maxHeaders int) (resultFS fs.FS, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxHeaders <= 0 {
		return nil, errors.New("invalid OCI archive header limit")
	}
	file, err := OpenRegular(filename)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxBytes {
		return nil, errors.New("CI archive exceeds encoded size budget or is not regular")
	}
	result := &archiveFS{ctx: ctx, filename: filename, entries: map[string]archiveEntry{}, info: info}
	metadataRemaining := int64(MaxMetadataBytes)
	reader := tar.NewReader(contextSeekReader{ctx: ctx, file: file, metadataRemaining: &metadataRemaining})
	headerCount := 0
	var total int64
	seen := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		headerCount++
		if headerCount > maxHeaders {
			return nil, errors.New("OCI archive exceeds header count limit")
		}
		name := strings.TrimPrefix(header.Name, "./")
		if header.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
			if name == "" {
				name = "."
			}
		}
		if name != path.Clean(name) || strings.Contains(name, "\\") {
			return nil, errors.New("unsafe OCI archive path")
		}
		for key := range header.PAXRecords {
			if strings.HasPrefix(key, "GNU.sparse") || key == "SCHILY.realsize" {
				return nil, errors.New("sparse OCI archive entries are unsupported")
			}
		}
		if header.Typeflag == tar.TypeDir && header.Size != 0 {
			return nil, errors.New("OCI archive directory must be empty")
		}
		if header.Typeflag == tar.TypeDir && name == "." {
			continue
		}
		if !fs.ValidPath(name) || name == "." {
			return nil, errors.New("unsafe OCI archive path")
		}
		if seen[name] {
			return nil, errors.New("OCI archive contains duplicate paths")
		}
		seen[name] = true
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return nil, errors.New("OCI archive contains a nonregular entry")
		}
		if len(result.entries) >= MaxEntries {
			return nil, errors.New("OCI archive exceeds entry limit")
		}

		offset, err := file.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		if offset > info.Size() || header.Size > info.Size()-offset {
			return nil, io.ErrUnexpectedEOF
		}
		if header.Size < 0 || header.Size > MaxBytes-total {
			return nil, errors.New("OCI archive entry exceeds encoded byte budget")
		}
		if (name == "oci-layout" || name == "index.json") && header.Size > MaxDocumentBytes {
			return nil, errors.New("OCI archive index metadata exceeds document limit")
		}
		if name != "oci-layout" && name != "index.json" {
			parts := strings.Split(name, "/")
			if len(parts) != 3 || parts[0] != "blobs" || parts[1] != "sha256" || digest.Digest("sha256:"+parts[2]).Validate() != nil {
				return nil, fmt.Errorf("unexpected OCI archive entry %q", name)
			}
		}
		total += header.Size
		result.entries[name] = archiveEntry{offset: offset, header: header}
	}
	return result, nil
}
