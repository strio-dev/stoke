package assets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestValidation(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../secret", "/absolute", "./file", "a/../file", "a//b", "a/", "a\\b", "a\x00b", "stoke.yaml", "stoke.yaml/file", string([]byte{'f', 0xff})} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(map[string]File{name: {}}); err == nil {
				t.Fatal("accepted invalid path")
			}
		})
	}
	if err := Validate(map[string]File{"a": {}, "a/b": {}}); err == nil {
		t.Fatal("accepted file/directory conflict")
	}
	if err := Validate(map[string]File{"white space ☃/script.sh": {Executable: true}}); err != nil {
		t.Fatal(err)
	}
	tooMany := map[string]File{}
	for i := 0; i <= MaxFiles; i++ {
		tooMany[strings.Repeat("a", i+1)] = File{}
	}
	if err := Validate(tooMany); err == nil {
		t.Fatal("accepted too many files")
	}
	if err := Validate(map[string]File{"large": {Data: make([]byte, MaxFileBytes+1)}}); err == nil {
		t.Fatal("accepted oversized file")
	}
	total := map[string]File{}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		total[name] = File{Data: make([]byte, MaxFileBytes)}
	}
	if err := Validate(total); err == nil {
		t.Fatal("accepted oversized component")
	}
}

func TestCollectConfinesReadsAndRejectsSpecialFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "scripts"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts/check.sh"), []byte("echo ok\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("scripts/check.sh", filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("not packaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	files, err := Collect(root, []string{"scripts/check.sh", "linked"})
	if err != nil || string(files["linked"].Data) != "echo ok\n" || !files["scripts/check.sh"].Executable {
		t.Fatalf("collected=%v err=%v", files, err)
	}
	for _, names := range [][]string{{"outside"}, {"fifo"}, {"scripts"}, {"missing"}, {"linked", "linked"}, {"../secret"}} {
		if _, err := Collect(root, names); err == nil {
			t.Fatalf("accepted files %v", names)
		}
	}
	large := filepath.Join(root, "large")
	file, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Collect(root, []string{"large"}); err == nil {
		t.Fatal("collected oversized file")
	}
}

func TestMaterializeStableContentModesAndTimes(t *testing.T) {
	files := map[string]File{"scripts/check.sh": {Data: []byte("#!/bin/sh\necho ok\n"), Executable: true}, "templates/data.txt": {Data: []byte("hello\n")}, "empty": {}}
	for range 2 {
		root := t.TempDir()
		if err := Materialize(root, files); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{".", "scripts", "templates", "scripts/check.sh", "templates/data.txt", "empty"} {
			info, err := os.Stat(filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			if !info.ModTime().Equal(time.Unix(0, 0)) {
				t.Errorf("%s timestamp=%s", name, info.ModTime())
			}
			if name == "scripts/check.sh" && info.Mode().Perm() != 0755 || name == "templates/data.txt" && info.Mode().Perm() != 0644 {
				t.Errorf("%s mode=%s", name, info.Mode())
			}
		}
		collected, err := Collect(root, []string{"scripts/check.sh", "templates/data.txt", "empty"})
		if err != nil {
			t.Fatal(err)
		}
		for name, want := range files {
			got := collected[name]
			if string(got.Data) != string(want.Data) || got.Executable != want.Executable {
				t.Fatalf("round trip %s: %#v", name, got)
			}
		}
	}
}

func TestMaterializeValidatesBeforeWritesAndNeverOverwrites(t *testing.T) {
	root := t.TempDir()
	if err := Materialize(root, map[string]File{"good": {Data: []byte("new")}, "../bad": {}}); err == nil {
		t.Fatal("accepted invalid file")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid package wrote files: %v %v", entries, err)
	}
	keep := filepath.Join(root, "keep")
	if err := os.WriteFile(keep, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(keep)
	if err != nil {
		t.Fatal(err)
	}
	if err := Materialize(root, map[string]File{"keep": {Data: []byte("replacement")}}); err == nil {
		t.Fatal("replaced caller file")
	}
	after, err := os.Stat(keep)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("caller inode replaced")
	}
	data, err := os.ReadFile(keep)
	if err != nil || string(data) != "original" {
		t.Fatal("caller bytes replaced")
	}
}
