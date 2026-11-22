package source

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/strio-dev/stoke/internal/workflow"
)

func TestBuildContextAndDefinitionOwnership(t *testing.T) {
	root, owner, outside := t.TempDir(), t.TempDir(), t.TempDir()
	for _, base := range []string{root, filepath.Join(root, "service"), owner} {
		if err := os.MkdirAll(filepath.Join(base, "ci"), 0700); err != nil {
			t.Fatal(err)
		}
		for _, file := range []string{"first.coopr", "second.coopr"} {
			if err := os.WriteFile(filepath.Join(base, "ci", file), []byte("from \"scratch\" {}"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		context, filesFrom, directory, execution, failure string
	}{
		{directory: root},
		{context: ".", directory: root},
		{context: "./service", directory: filepath.Join(root, "service"), execution: filepath.Join(root, "service")},
		{context: "service", filesFrom: "component", directory: owner, execution: filepath.Join(root, "service")},
		{context: "escape", failure: "outside"},
		{context: "missing", failure: "missing"},
		{context: "ci/first.coopr", failure: "directory"},
	} {
		t.Run(tc.context+"/"+tc.filesFrom, func(t *testing.T) {
			w, err := workflow.Decode(strings.NewReader("version: 1\nassets: [ci/first.coopr, ci/second.coopr]\ntasks: {check: {build: {coopr: [ci/second.coopr, ci/first.coopr], context: '" + tc.context + "', files-from: '" + tc.filesFrom + "'}}}"))
			if err != nil {
				t.Fatal(err)
			}
			task := w.Tasks["check"]
			task.ComponentDir = owner
			resolved, err := ResolveDefinitionBuild(task, root)
			if tc.failure != "" {
				if err == nil || !strings.Contains(err.Error(), tc.failure) {
					t.Fatalf("expected %s: %v", tc.failure, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []string{filepath.Join(tc.directory, "ci/second.coopr"), filepath.Join(tc.directory, "ci/first.coopr")}
			if resolved.Context != tc.execution || !reflect.DeepEqual(resolved.Files, want) || task.Environment.Files[0] != "ci/second.coopr" {
				t.Fatalf("context/ownership/order lost or authored task mutated: %+v", resolved)
			}
		})
	}
}
