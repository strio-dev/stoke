// Package hostfiles captures declared host outputs as bounded, immutable OCI data.
// It is a regular-file format, not a runnable image or a filesystem snapshot.
package hostfiles

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	MediaType             = "application/vnd.strio.stoke.host.files.v1"
	ConfigMediaType       = MediaType + "+json"
	LayerMediaType        = MediaType + "+tar"
	MaxBytes        int64 = 512 << 20
	MaxEntries            = 65536
	MaxMetadata           = 8 << 20
)

type Entry struct {
	Path      string `json:"path"`
	Mode      uint32 `json:"mode"`
	Size      int64  `json:"size"`
	Digest    string `json:"digest,omitempty"`
	Directory bool   `json:"directory,omitempty"`
}

type Config struct {
	Version  int         `json:"version"`
	Platform v1.Platform `json:"platform"`
	Paths    []string    `json:"paths"`
	Entries  []Entry     `json:"entries"`
	InputKey string      `json:"inputKey,omitempty"`
}

func cleanName(name string, root bool) bool {
	return name != "" && !strings.ContainsAny(name, "\\\x00") && !path.IsAbs(name) && path.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../") && (root || name != ".")
}

func selectedPaths(paths []string) ([]string, error) {
	out := slices.Clone(paths)
	slices.Sort(out)
	for i, name := range out {
		if !cleanName(name, true) {
			return nil, fmt.Errorf("invalid captured path %q", name)
		}
		for _, prior := range out[:i] {
			if prior == "." || name == prior || strings.HasPrefix(name, prior+"/") {
				return nil, errors.New("captured paths overlap or repeat")
			}
		}
	}
	return out, nil
}

func platform(value string) (v1.Platform, error) {
	if value != "linux/amd64" && value != "linux/arm64" {
		return v1.Platform{}, errors.New("host files require linux/amd64 or linux/arm64 platform")
	}
	parts := strings.Split(value, "/")
	return v1.Platform{OS: parts[0], Architecture: parts[1]}, nil
}

func regular(info fs.FileInfo) error {
	if !info.IsDir() && !info.Mode().IsRegular() {
		return errors.New("host files support regular files and directories only; symlinks and special files are not captured")
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return errors.New("host files do not capture special permission bits")
	}
	if !info.IsDir() {
		if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
			return errors.New("host files do not capture hard-linked files")
		}
	}
	return nil
}

func checkParents(root *os.Root, name string) error {
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		info, err := root.Lstat(parent)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("host file parent is not an ordinary directory")
		}
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func inventory(ctx context.Context, root *os.Root, selected []string) ([]Entry, error) {
	entries := map[string]Entry{}
	var total int64
	add := func(name string, info fs.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if _, ok := entries[name]; ok {
			return nil
		}
		if len(entries) >= MaxEntries {
			return errors.New("host files exceed entry limit")
		}
		if err := regular(info); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		entry := Entry{Path: name, Mode: uint32(info.Mode().Perm()), Directory: info.IsDir()}
		if !info.IsDir() {
			if info.Size() < 0 || info.Size() > MaxBytes-total {
				return errors.New("host files exceed byte limit")
			}
			file, err := root.Open(name)
			if err != nil {
				return err
			}
			actual, err := file.Stat()
			if err != nil {
				_ = file.Close()
				return err
			}
			if err := regular(actual); err != nil {
				_ = file.Close()
				return err
			}
			hash := sha256.New()
			n, copyErr := io.Copy(hash, contextReader{ctx, io.LimitReader(file, info.Size()+1)})
			err = errors.Join(copyErr, file.Close())
			if err != nil {
				return err
			}
			if n != info.Size() {
				return errors.New("host input changed while capturing")
			}
			entry.Size = n
			entry.Digest = "sha256:" + hex.EncodeToString(hash.Sum(nil))
			total += n
		}
		entries[name] = entry
		return nil
	}
	for _, name := range selected {
		if err := checkParents(root, name); err != nil {
			return nil, err
		}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			info, err := root.Lstat(parent)
			if err != nil {
				return nil, err
			}
			if err := add(parent, info); err != nil {
				return nil, err
			}
		}
		if err := fs.WalkDir(root.FS(), name, func(name string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			return add(name, info)
		}); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]Entry, 0, len(names))
	for _, name := range names {
		out = append(out, entries[name])
	}
	return out, nil
}

// Copy snapshots a declared input into a fresh destination path. Both roots
// must exist; sourcePath and destinationPath are relative to their roots.
func Copy(ctx context.Context, sourceRoot, sourcePath, destinationRoot, destinationPath string) error {
	if !cleanName(sourcePath, true) || !cleanName(destinationPath, true) {
		return errors.New("host snapshot paths must be relative and nonescaping")
	}
	source, err := os.OpenRoot(sourceRoot)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	entries, err := inventory(ctx, source, []string{sourcePath})
	if err != nil {
		return err
	}
	dest, err := os.OpenRoot(destinationRoot)
	if err != nil {
		return err
	}
	defer func() { _ = dest.Close() }()
	if destinationPath != "." {
		if _, err := dest.Lstat(destinationPath); !errors.Is(err, os.ErrNotExist) {
			return errors.New("host snapshot destination already exists")
		}
	}
	sourceInfo, err := source.Lstat(sourcePath)
	if err != nil {
		return err
	}
	if !sourceInfo.IsDir() && destinationPath == "." {
		return errors.New("host file cannot replace destination root")
	}
	if sourcePath == "." && destinationPath != "." {
		if err := dest.MkdirAll(destinationPath, 0700); err != nil {
			return err
		}
	}
	materialized := []string{destinationPath}
	for _, entry := range entries {
		relative := entry.Path
		if sourcePath != "." {
			if relative == sourcePath {
				relative = "."
			} else if strings.HasPrefix(relative, sourcePath+"/") {
				relative = strings.TrimPrefix(relative, sourcePath+"/")
			} else {
				continue
			}
		}
		target := path.Join(destinationPath, relative)
		if target == "." {
			continue
		}
		if err := copyEntry(ctx, source, entry, dest, target); err != nil {
			return err
		}
		materialized = append(materialized, target)
	}
	if err := normalizeTimes(ctx, dest, materialized); err != nil {
		return err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if !entry.Directory {
			continue
		}
		relative := entry.Path
		if sourcePath != "." {
			if relative == sourcePath {
				relative = "."
			} else if strings.HasPrefix(relative, sourcePath+"/") {
				relative = strings.TrimPrefix(relative, sourcePath+"/")
			} else {
				continue
			}
		}
		target := path.Join(destinationPath, relative)
		if target != "." {
			if err := dest.Chmod(target, os.FileMode(entry.Mode)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Materialized views expose the archive's canonical time, not the time of a
// restore or source checkout. Include implicit parents and the private root;
// finish children first while directory permissions still permit access.
func normalizeTimes(ctx context.Context, root *os.Root, names []string) error {
	paths := map[string]bool{".": true}
	for _, name := range names {
		for current := name; current != "."; current = path.Dir(current) {
			if paths[current] {
				break
			}
			if len(paths) > MaxEntries {
				return errors.New("host materialization exceeds the entry bound")
			}
			paths[current] = true
		}
	}
	ordered := make([]string, 0, len(paths))
	for name := range paths {
		if name != "." {
			ordered = append(ordered, name)
		}
	}
	slices.Sort(ordered)
	slices.Reverse(ordered)
	ordered = append(ordered, ".")
	for _, name := range ordered {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.Chtimes(name, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
			return err
		}
	}
	return nil
}

func copyEntry(ctx context.Context, source *os.Root, entry Entry, dest *os.Root, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if entry.Directory {
		if _, err := dest.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			return errors.New("host snapshot directory already exists")
		}
		return dest.MkdirAll(target, os.FileMode(entry.Mode)|0700)
	}
	if err := dest.MkdirAll(path.Dir(target), 0700); err != nil {
		return err
	}
	in, err := source.Open(entry.Path)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := dest.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(entry.Mode))
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, hash), contextReader{ctx, io.LimitReader(in, entry.Size+1)})
	err = errors.Join(copyErr, out.Chmod(os.FileMode(entry.Mode)), out.Close())
	if err != nil {
		return err
	}
	if n != entry.Size || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != entry.Digest {
		return errors.New("host snapshot input changed while copying")
	}
	return nil
}

func Capture(ctx context.Context, workspace string, paths []string, layout, platformValue string) (string, error) {
	return CaptureWithKey(ctx, workspace, paths, layout, platformValue, "")
}

// CaptureWithKey binds optional reusable results to the caller's declared-input key.
func CaptureWithKey(ctx context.Context, workspace string, paths []string, layout, platformValue, inputKey string) (identity string, retErr error) {
	selected, err := selectedPaths(paths)
	if err != nil {
		return "", err
	}
	actualPlatform, err := platform(platformValue)
	if err != nil {
		return "", err
	}
	if !validKey(inputKey) {
		return "", errors.New("host result input key must be SHA256 hex")
	}
	if _, err := os.Lstat(layout); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("host capture layout must be fresh")
	}
	source, err := os.OpenRoot(workspace)
	if err != nil {
		return "", err
	}
	defer func() { _ = source.Close() }()
	entries, err := inventory(ctx, source, selected)
	if err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(filepath.Dir(layout), ".host-capture-")
	if err != nil {
		return "", err
	}
	defer func() {
		if retErr != nil {
			_ = os.RemoveAll(staging)
		}
	}()
	blobs := filepath.Join(staging, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(staging, "layer-")
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	limited := &boundedWriter{writer: io.MultiWriter(file, hash), remaining: MaxBytes}
	writer := tar.NewWriter(limited)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			_ = writer.Close()
			_ = file.Close()
			return "", err
		}
		kind := byte(tar.TypeReg)
		if entry.Directory {
			kind = tar.TypeDir
		}
		header := &tar.Header{Name: entry.Path, Mode: int64(entry.Mode), Size: entry.Size, Typeflag: kind, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if err := writer.WriteHeader(header); err != nil {
			_ = file.Close()
			return "", err
		}
		if !entry.Directory {
			in, err := source.Open(entry.Path)
			if err != nil {
				_ = file.Close()
				return "", err
			}
			contentHash := sha256.New()
			n, copyErr := io.Copy(io.MultiWriter(writer, contentHash), contextReader{ctx, io.LimitReader(in, entry.Size+1)})
			err = errors.Join(copyErr, in.Close())
			if err != nil {
				_ = file.Close()
				return "", err
			}
			if n != entry.Size || "sha256:"+hex.EncodeToString(contentHash.Sum(nil)) != entry.Digest {
				_ = file.Close()
				return "", errors.New("host output changed while capturing")
			}
		}
	}
	if err := errors.Join(writer.Close(), file.Close()); err != nil {
		return "", err
	}
	layer := v1.Descriptor{MediaType: LayerMediaType, Digest: digest.NewDigestFromEncoded(digest.SHA256, hex.EncodeToString(hash.Sum(nil))), Size: MaxBytes - limited.remaining}
	if err := os.Rename(file.Name(), filepath.Join(blobs, layer.Digest.Encoded())); err != nil {
		return "", err
	}
	config := Config{Version: 1, Platform: actualPlatform, Paths: selected, Entries: entries, InputKey: inputKey}
	raw, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	if len(raw) > MaxMetadata {
		return "", errors.New("host output inventory exceeds metadata limit")
	}
	configDesc, err := putBlob(blobs, ConfigMediaType, raw)
	if err != nil {
		return "", err
	}
	raw, err = json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: MediaType, Config: configDesc, Layers: []v1.Descriptor{layer}})
	if err != nil {
		return "", err
	}
	manifest, err := putBlob(blobs, v1.MediaTypeImageManifest, raw)
	if err != nil {
		return "", err
	}
	manifest.Annotations = map[string]string{v1.AnnotationRefName: "result"}
	raw, err = json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{manifest}})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(staging, "index.json"), raw, 0600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(staging, "oci-layout"), []byte("{\"imageLayoutVersion\":\"1.0.0\"}"), 0600); err != nil {
		return "", err
	}
	if err := Commit(staging, layout); err != nil {
		return "", err
	}
	return manifest.Digest.String(), nil
}

type boundedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("host capture exceeds byte limit")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}
func putBlob(directory, mediaType string, data []byte) (v1.Descriptor, error) {
	descriptor := v1.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
	err := os.WriteFile(filepath.Join(directory, descriptor.Digest.Encoded()), data, 0600)
	return descriptor, err
}
func validKey(key string) bool {
	if key == "" {
		return true
	}
	raw, err := hex.DecodeString(key)
	return err == nil && len(raw) == sha256.Size
}
