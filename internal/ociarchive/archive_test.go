package ociarchive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"
)

func archiveFixture(t *testing.T, headers ...*tar.Header) string {
	t.Helper()
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	for _, header := range headers {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(bytes.Repeat([]byte("x"), int(header.Size))); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "input.tar")
	if err := os.WriteFile(name, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestArchiveSharedAdmission(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []*tar.Header
	}{
		{"duplicate-directory", []*tar.Header{{Name: "blobs/", Typeflag: tar.TypeDir}, {Name: "blobs/", Typeflag: tar.TypeDir}}},
		{"file-directory-alias", []*tar.Header{{Name: "index.json", Size: 1}, {Name: "index.json/", Typeflag: tar.TypeDir}}},
		{"sparse-pax", []*tar.Header{{Name: "index.json", Size: 1, Format: tar.FormatPAX, PAXRecords: map[string]string{"SCHILY.realsize": "1"}}}},
		{"backslash", []*tar.Header{{Name: "bad\\name/", Typeflag: tar.TypeDir}}},
		{"hardlink", []*tar.Header{{Name: "index.json", Typeflag: tar.TypeLink, Linkname: "other"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(t.Context(), archiveFixture(t, tc.headers...), 100); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
	name := archiveFixture(t, &tar.Header{Name: "index.json", Size: 1}, &tar.Header{Name: "oci-layout", Size: 1})
	if _, err := New(t.Context(), name, 1); err == nil {
		t.Fatal("header bound ignored")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := New(ctx, name, 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestArchiveRejectsReplacedOrSpecialFile(t *testing.T) {
	name := archiveFixture(t, &tar.Header{Name: "index.json", Size: 1})
	archive, err := New(t.Context(), name, 100)
	if err != nil {
		t.Fatal(err)
	}
	replacement := archiveFixture(t, &tar.Header{Name: "index.json", Size: 1})
	if err := os.Rename(replacement, name); err != nil {
		t.Fatal(err)
	}
	if f, err := archive.Open("index.json"); err == nil {
		_ = f.Close()
		t.Fatal("replaced archive accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(name, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{link, fifo} {
		if _, err := New(t.Context(), name, 100); err == nil {
			t.Fatal("nonregular archive accepted")
		}
	}
}

func TestArchiveConcurrentBoundedReads(t *testing.T) {
	blob := "blobs/sha256/" + digest.FromString("xxx").Encoded()
	name := archiveFixture(t, &tar.Header{Name: "./", Typeflag: tar.TypeDir}, &tar.Header{Name: "ingest/", Typeflag: tar.TypeDir}, &tar.Header{Name: blob, Size: 3})
	archive, err := New(t.Context(), name, 100)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := archive.Open("ingest"); !errors.Is(err, fs.ErrNotExist) {
		if f != nil {
			_ = f.Close()
		}
		t.Fatal("directory exposed as content")
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			f, err := archive.Open(blob)
			if err != nil {
				t.Error(err)
				return
			}
			data, err := io.ReadAll(f)
			err = errors.Join(err, f.Close())
			if err != nil || string(data) != "xxx" {
				t.Errorf("read=%q error=%v", data, err)
			}
		})
	}
	workers.Wait()
}
