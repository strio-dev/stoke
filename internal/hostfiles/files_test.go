package hostfiles

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func fixture(t *testing.T) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "out"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out", "tool"), []byte("captured\n"), 0751); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "private"), []byte("not selected"), 0600); err != nil {
		t.Fatal(err)
	}
	return dir, []string{"out"}
}
func TestCaptureRestoresOnlyDeclaredFilesAndStableIdentity(t *testing.T) {
	dir, paths := fixture(t)
	parent := t.TempDir()
	first := filepath.Join(parent, "first")
	key := strings.Repeat("a", 64)
	identity, err := CaptureWithKey(t.Context(), dir, paths, first, "linux/amd64", key)
	if err != nil {
		t.Fatal(err)
	}
	config, err := Validate(t.Context(), first, identity)
	if err != nil {
		t.Fatal(err)
	}
	if config.InputKey != key || len(config.Entries) != 2 {
		t.Fatalf("wrong metadata: %#v", config)
	}
	second := filepath.Join(parent, "second")
	if err := os.Chtimes(filepath.Join(dir, "out", "tool"), time.Now().Add(-time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	again, err := CaptureWithKey(t.Context(), dir, paths, second, "linux/amd64", key)
	if err != nil || again != identity {
		t.Fatalf("timestamp changed immutable output: %s %v", again, err)
	}
	restored := filepath.Join(parent, "restored")
	if err := Restore(t.Context(), first, identity, restored); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(restored, "out", "tool"))
	if err != nil || string(data) != "captured\n" {
		t.Fatalf("restore: %q %v", data, err)
	}
	info, err := os.Stat(filepath.Join(restored, "out", "tool"))
	if err != nil || info.Mode().Perm() != 0751 {
		t.Fatalf("mode: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(restored, "private")); !os.IsNotExist(err) {
		t.Fatal("undeclared file captured")
	}
	if err := Restore(t.Context(), first, identity, restored); err == nil {
		t.Fatal("overwrote existing destination")
	}
	if err := os.WriteFile(filepath.Join(dir, "out", "tool"), []byte("changed"), 0751); err != nil {
		t.Fatal(err)
	}
	changed, err := Capture(t.Context(), dir, paths, filepath.Join(parent, "changed"), "linux/amd64")
	if err != nil || changed == identity {
		t.Fatalf("content change not identified: %v", err)
	}
}
func TestCaptureNestedOutputIncludesParents(t *testing.T) {
	dir, _ := fixture(t)
	layout := filepath.Join(t.TempDir(), "layout")
	identity, err := Capture(t.Context(), dir, []string{"out/tool"}, layout, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	config, err := Validate(t.Context(), layout, identity)
	if err != nil || len(config.Entries) != 2 || config.Entries[0].Path != "out" {
		t.Fatalf("nested output: %#v %v", config, err)
	}
}
func TestSnapshotMapsInputsAndRejectsOverwrites(t *testing.T) {
	dir, _ := fixture(t)
	dest := t.TempDir()
	if err := Copy(t.Context(), dir, "out/tool", dest, "input/tool"); err != nil {
		t.Fatal(err)
	}
	if err := Copy(t.Context(), dir, "out/tool", dest, "input/tool"); err == nil {
		t.Fatal("snapshot overwrote a file")
	}
	data, err := os.ReadFile(filepath.Join(dest, "input/tool"))
	if err != nil || string(data) != "captured\n" {
		t.Fatalf("copy: %q %v", data, err)
	}
	root := t.TempDir()
	if err := Copy(t.Context(), dir, ".", root, "."); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "private")); err != nil {
		t.Fatal(err)
	}
	if err := Copy(t.Context(), dir, "out/tool", t.TempDir(), "."); err == nil {
		t.Fatal("file silently mapped onto directory root")
	}
}

func TestMaterializedTimesAreCanonicalAcrossCopyAndRestore(t *testing.T) {
	source, _ := fixture(t)
	original, err := os.Stat(filepath.Join(source, "out/tool"))
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(t.TempDir(), "layout")
	identity, err := Capture(t.Context(), source, []string{"out/tool"}, layout, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	check := func(root string, names ...string) {
		t.Helper()
		for _, name := range names {
			info, err := os.Stat(filepath.Join(root, name))
			if err != nil || !info.ModTime().Equal(time.Unix(0, 0)) {
				t.Fatalf("materialized %s does not have the archive epoch: %v %v", name, info, err)
			}
		}
	}
	for range 2 {
		copied := t.TempDir()
		if err := Copy(t.Context(), source, "out/tool", copied, "input/deep/tool"); err != nil {
			t.Fatal(err)
		}
		check(copied, ".", "input", "input/deep", "input/deep/tool")
		tree := t.TempDir()
		if err := Copy(t.Context(), source, "out", tree, "input"); err != nil {
			t.Fatal(err)
		}
		check(tree, ".", "input", "input/tool")
		restored := filepath.Join(t.TempDir(), "restored")
		if err := Restore(t.Context(), layout, identity, restored); err != nil {
			t.Fatal(err)
		}
		check(restored, ".", "out", "out/tool")
	}
	after, err := os.Stat(filepath.Join(source, "out/tool"))
	if err != nil || !after.ModTime().Equal(original.ModTime()) {
		t.Fatal("source timestamp changed", after, err)
	}
}
func TestCaptureRejectsUnsafeInputsAndCancellation(t *testing.T) {
	for _, paths := range [][]string{{"../escape"}, {"/absolute"}, {"out", "out/tool"}, {"out", "out"}, {"out/../private"}} {
		dir, _ := fixture(t)
		if _, err := Capture(t.Context(), dir, paths, filepath.Join(t.TempDir(), "layout"), "linux/amd64"); err == nil {
			t.Fatalf("accepted %#v", paths)
		}
	}
	dir, _ := fixture(t)
	if err := os.Symlink("../private", filepath.Join(dir, "out/link")); err != nil {
		t.Fatal(err)
	}
	if _, err := Capture(t.Context(), dir, []string{"out"}, filepath.Join(t.TempDir(), "layout"), "linux/amd64"); err == nil {
		t.Fatal("captured symlink")
	}
	linked, _ := fixture(t)
	if err := os.Link(filepath.Join(linked, "private"), filepath.Join(linked, "out/link")); err != nil {
		t.Fatal(err)
	}
	if _, err := Capture(t.Context(), linked, []string{"out"}, filepath.Join(t.TempDir(), "layout"), "linux/amd64"); err == nil {
		t.Fatal("captured hard link")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	layout := filepath.Join(t.TempDir(), "layout")
	if _, err := Capture(ctx, dir, []string{"out"}, layout, "linux/amd64"); err == nil {
		t.Fatal("canceled capture succeeded")
	}
	if _, err := os.Stat(layout); !os.IsNotExist(err) {
		t.Fatal("canceled capture left layout")
	}
}

func maliciousLayout(t *testing.T, header tar.Header, extra bool) (string, string) {
	t.Helper()
	layout := t.TempDir()
	blobs := filepath.Join(layout, "blobs/sha256")
	if err := os.MkdirAll(blobs, 0700); err != nil {
		t.Fatal(err)
	}
	entry := Entry{Path: "out", Mode: 0644, Size: 1, Digest: digest.FromString("x").String()}
	config := Config{Version: 1, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Paths: []string{"out"}, Entries: []Entry{entry}}
	raw, _ := json.Marshal(config)
	conf, err := putBlob(blobs, ConfigMediaType, raw)
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	if err := writer.WriteHeader(&header); err != nil {
		t.Fatal(err)
	}
	if header.Typeflag == tar.TypeReg {
		if _, err := writer.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if extra {
		if err := writer.WriteHeader(&tar.Header{Name: "extra", Size: 0, Mode: 0644, ModTime: time.Unix(0, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	layer, err := putBlob(blobs, LayerMediaType, data.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: MediaType, Config: conf, Layers: []v1.Descriptor{layer}})
	manifest, err := putBlob(blobs, v1.MediaTypeImageManifest, raw)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{manifest}})
	if err := os.WriteFile(filepath.Join(layout, "index.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	return layout, manifest.Digest.String()
}
func TestRestoreRejectsHostileArchivesWithoutEscapingStaging(t *testing.T) {
	for name, header := range map[string]tar.Header{
		"traversal": {Name: "../escape", Size: 1, Mode: 0644, Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)},
		"symlink":   {Name: "out", Linkname: "../escape", Mode: 0644, Typeflag: tar.TypeSymlink, ModTime: time.Unix(0, 0)},
		"hardlink":  {Name: "out", Linkname: "../escape", Mode: 0644, Typeflag: tar.TypeLink, ModTime: time.Unix(0, 0)},
		"mode":      {Name: "out", Size: 1, Mode: 0777, Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)},
	} {
		t.Run(name, func(t *testing.T) {
			layout, identity := maliciousLayout(t, header, false)
			parent := t.TempDir()
			dest := filepath.Join(parent, "result")
			if err := Restore(t.Context(), layout, identity, dest); err == nil {
				t.Fatal("restored hostile archive")
			}
			children, err := os.ReadDir(parent)
			if err != nil || len(children) != 0 {
				t.Fatalf("failed restore leaked files: %v %v", children, err)
			}
		})
	}
	header := tar.Header{Name: "out", Size: 1, Mode: 0644, Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}
	layout, identity := maliciousLayout(t, header, true)
	if err := Restore(t.Context(), layout, identity, filepath.Join(t.TempDir(), "result")); err == nil {
		t.Fatal("accepted extra archive entry")
	}
}
func TestRestoreRejectsCorruptBlob(t *testing.T) {
	dir, paths := fixture(t)
	layout := filepath.Join(t.TempDir(), "layout")
	identity, err := Capture(t.Context(), dir, paths, layout, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	config, err := Validate(t.Context(), layout, identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Entries) == 0 {
		t.Fatal("empty fixture")
	}
	manifestBlob := filepath.Join(layout, "blobs/sha256", strings.TrimPrefix(identity, "sha256:"))
	if err := os.WriteFile(manifestBlob, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if err := Restore(t.Context(), layout, identity, dest); err == nil {
		t.Fatal("restored corrupt manifest")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("corrupt restore committed output")
	}
}

func TestValidateManifestChecksConfigIdentity(t *testing.T) {
	dir, paths := fixture(t)
	layout := filepath.Join(t.TempDir(), "layout")
	identity, err := Capture(t.Context(), dir, paths, layout, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(layout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	manifest, _, err := metadata(t.Context(), root, identity)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readBlob(t.Context(), root, manifest.Config, MaxMetadata)
	if err != nil {
		t.Fatal(err)
	}
	changed := append([]byte(nil), raw...)
	changed[len(changed)-1] = ' '
	if _, err := ValidateManifest(manifest, changed); err == nil {
		t.Fatal("accepted changed config with unchanged descriptor")
	}
	manifest.Config.Size++
	if _, err := ValidateManifest(manifest, raw); err == nil {
		t.Fatal("accepted config with wrong descriptor size")
	}
}

func TestCommitNeverReplacesFreshnessRaceDestination(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "staging")
	destination := filepath.Join(parent, "destination")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "value"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destination, 0711); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := Commit(source, destination); err == nil {
		t.Fatal("replaced existing empty directory")
	}
	after, err := os.Stat(destination)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("existing destination changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(source, "value")); err != nil {
		t.Fatal("failed commit removed staging contents")
	}
	if _, err := os.Stat(filepath.Join(destination, "value")); !os.IsNotExist(err) {
		t.Fatal("failed commit exposed staged file")
	}
}
