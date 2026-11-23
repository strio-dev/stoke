package source

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/strio-dev/stoke/internal/artifact"
)

func localImportFile(t *testing.T, root, name, data string) string {
	t.Helper()
	file := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestLocalImportsKeepScopesAssetsAndOCI(t *testing.T) {
	root := t.TempDir()
	remote := pin("remote")
	file := localImportFile(t, root, "stoke.yaml", "version: 1\nimports: {local: ./lib/stoke.yaml, again: ./lib/alias.yaml, remote: "+remote+"}\ntasks: {one: {uses: local/build}, two: {uses: again/build}, remote: {uses: remote/check}}\n")
	localImportFile(t, root, "lib/stoke.yaml", "version: 1\nassets: [helper.txt]\nimports: {leaf: ./../shared/stoke.yaml}\ntasks: {prepare: {uses: leaf/check}, build: {image: scratch, needs: [prepare], steps: [{copy: {scope: component, sources: [helper.txt], destination: /helper}}]}}\n")
	localImportFile(t, root, "shared/stoke.yaml", "version: 1\nassets: [helper.txt]\ntasks: {check: {image: scratch, steps: [{copy: {scope: component, sources: [helper.txt], destination: /helper}}]}}\n")
	localImportFile(t, root, "lib/helper.txt", "library helper")
	localImportFile(t, root, "shared/helper.txt", "leaf helper")
	if err := os.Symlink("stoke.yaml", filepath.Join(root, "lib/alias.yaml")); err != nil {
		t.Fatal(err)
	}
	leafRef, delivered := pin("leaf"), "registry.test/delivered:workflow"
	l := loader(map[string]*artifact.Document{
		remote:    {Data: []byte("version: 1\nimports: {leaf: " + leafRef + "}\ntasks: {check: {uses: leaf/check}}"), Dependencies: map[string]string{leafRef: delivered}},
		delivered: {Data: []byte("version: 1\ntasks: {check: {image: scratch}}"), Reference: leafRef},
	})
	result, err := l.Load(t.Context(), file)
	if err != nil {
		t.Fatal(err)
	}
	closeLoaded(t, result)
	one, two := result.Workflow.Tasks["one"], result.Workflow.Tasks["two"]
	leaf := result.Workflow.Tasks["one--local--prepare"]
	if result.ContextDir != root || result.Reference != file || one.ComponentDir == "" || one.ComponentDir != two.ComponentDir || one.ComponentDir == leaf.ComponentDir || len(result.owned) != 2 {
		t.Fatalf("caller/owner identity changed: %+v", result)
	}
	if !reflect.DeepEqual(one.Needs, []string{"one--local--prepare"}) || !reflect.DeepEqual(two.Needs, []string{"two--again--prepare"}) {
		t.Fatalf("invocations lost independent scopes: %+v %+v", one.Needs, two.Needs)
	}
	localImportFile(t, root, "lib/helper.txt", "changed after load")
	for directory, want := range map[string]string{one.ComponentDir: "library helper", leaf.ComponentDir: "leaf helper"} {
		data, err := os.ReadFile(filepath.Join(directory, "helper.txt"))
		if err != nil || string(data) != want {
			t.Fatalf("owned snapshot: %q %v", data, err)
		}
	}
	if got := Dependencies(result.RootWorkflow); !reflect.DeepEqual(got, []string{remote}) {
		t.Fatalf("local paths became registry dependencies: %v", got)
	}
}

func TestLocalImportBoundaries(t *testing.T) {
	for _, tc := range []struct{ name, ref, failure string }{
		{"parent-escape", "./../outside.yaml", "workflow tree"},
		{"symlink-escape", "./escape.yaml", "workflow tree"},
		{"directory", "./dir", "not a regular file"},
		{"fifo", "./fifo", "not a regular file"},
		{"cycle", "./alias.yaml", "cycle"},
		{"missing", "./missing.yaml", "no such file"},
		{"asset-escape", "./asset.yaml", "component assets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "work")
			file := localImportFile(t, root, "stoke.yaml", "version: 1\nimports: {child: "+tc.ref+"}\ntasks: {run: {image: scratch}}")
			outside := localImportFile(t, parent, "outside.yaml", "version: 1\ntasks: {run: {image: scratch}}")
			if err := os.Mkdir(filepath.Join(root, "dir"), 0700); err != nil {
				t.Fatal(err)
			}
			for name, target := range map[string]string{"escape.yaml": outside, "alias.yaml": "stoke.yaml", "outside.txt": outside} {
				if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
					t.Fatal(err)
				}
			}
			if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
				t.Fatal(err)
			}
			localImportFile(t, root, "asset.yaml", "version: 1\nassets: [outside.txt]\ntasks: {run: {image: scratch}}")
			_, err := (Loader{}).Load(t.Context(), file)
			if err == nil || !strings.Contains(err.Error(), tc.failure) {
				t.Fatalf("expected %s: %v", tc.failure, err)
			}
		})
	}
}

func TestOCIImportsNeverReadLocalPaths(t *testing.T) {
	for _, tc := range []struct{ name, authored, delivered string }{
		{"authored-local", "./host.yaml", ""},
		{"relocated-local", pin("child"), "./host.yaml"},
		{"relocated-absolute", pin("child"), "/host.yaml"},
		{"relocated-absolute-with-digest", pin("child"), "/host.yaml@sha256:" + strings.Repeat("a", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := localImportFile(t, t.TempDir(), "host.yaml", "version: 1\ntasks: {run: {image: scratch}}")
			calls := 0
			l := Loader{Fetch: func(_ context.Context, ref string) (*artifact.Document, error) {
				calls++
				if ref != pin("root") {
					t.Fatalf("unsafe relocated import reached transport: %q", ref)
				}
				// A transport document's reference cannot grant filesystem authority.
				return &artifact.Document{Data: []byte("version: 1\nimports: {child: " + tc.authored + "}\ntasks: {run: {image: scratch}}"), Reference: file, Dependencies: map[string]string{tc.authored: tc.delivered}}, nil
			}}
			for _, entry := range []string{"oci://" + pin("root"), localImportFile(t, t.TempDir(), "stoke.yaml", "version: 1\nimports: {remote: "+pin("root")+"}\ntasks: {run: {uses: remote/run}}")} {
				calls = 0
				_, err := l.Load(t.Context(), entry)
				if err == nil || calls != 1 || !strings.Contains(err.Error(), "OCI") {
					t.Fatalf("OCI provenance boundary lost: calls=%d err=%v", calls, err)
				}
			}
		})
	}
}

func TestLocalImportsRetainGraphLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		count  int
		nested bool
	}{
		{"depth", 18, true}, {"count", 65, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			imports := ""
			for i := range tc.count {
				child := "version: 1\ntasks: {run: {image: scratch}}\n"
				if tc.nested && i+1 < tc.count {
					child += fmt.Sprintf("imports: {next: ./child%d.yaml}\n", i+1)
				}
				localImportFile(t, root, fmt.Sprintf("child%d.yaml", i), child)
				if !tc.nested || i == 0 {
					imports += fmt.Sprintf("  child%d: ./child%d.yaml\n", i, i)
				}
			}
			file := localImportFile(t, root, "stoke.yaml", "version: 1\ntasks: {run: {image: scratch}}\nimports:\n"+imports)
			if _, err := (Loader{}).Load(t.Context(), file); err == nil || !strings.Contains(err.Error(), "import "+tc.name+" exceeds") {
				t.Fatalf("%s limit lost: %v", tc.name, err)
			}
		})
	}
}

func TestImportedCachePolicyCannotOverrideRoot(t *testing.T) {
	for _, policy := range []string{"", "cache-from: [registry.test/root-read]\ncache-to: []\n"} {
		for _, local := range []bool{false, true} {
			root := t.TempDir()
			const child = "version: 1\ncache-from: [registry.test/child-read]\ncache-to: [registry.test/child-write]\ntasks: {build: {image: scratch}}\n"
			reference := pin("cache-child")
			l := loader(map[string]*artifact.Document{reference: {Reference: reference, Data: []byte(child)}})
			if local {
				reference = "./child.yaml"
				localImportFile(t, root, "child.yaml", child)
			}
			file := localImportFile(t, root, "stoke.yaml", "version: 1\n"+policy+"imports: {child: "+reference+"}\ntasks: {build: {uses: child/build}}\n")
			loaded, err := l.Load(t.Context(), file)
			if err != nil {
				t.Fatal(err)
			}
			closeLoaded(t, loaded)
			if !reflect.DeepEqual(loaded.Workflow.CacheFrom, loaded.RootWorkflow.CacheFrom) || !reflect.DeepEqual(loaded.Workflow.CacheTo, loaded.RootWorkflow.CacheTo) {
				t.Fatal("imported cache policy escaped its owning document")
			}
			for _, dependency := range Dependencies(loaded.RootWorkflow) {
				if strings.Contains(dependency, "root-read") {
					t.Fatal("cache repository entered Bale dependency declarations")
				}
			}
		}
	}
}

func TestNestedImportRejectsExpandedFilesystemNameBeforeExecution(t *testing.T) {
	root := t.TempDir()
	caller, dependency := strings.Repeat("a", 200), strings.Repeat("b", 70)
	file := localImportFile(t, root, "stoke.yaml", "version: 1\nimports: {library: ./library.yaml}\ntasks: {"+caller+": {uses: library/build}}\n")
	localImportFile(t, root, "library.yaml", "version: 1\ntasks: {build: {image: scratch, needs: ["+dependency+"]}, "+dependency+": {image: scratch}}\n")
	if _, err := (Loader{}).Load(t.Context(), file); err == nil || !strings.Contains(err.Error(), "expanded task name") {
		t.Fatalf("expanded imported name was not rejected early: %v", err)
	}
}
