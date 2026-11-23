package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestCIGenerationRefusesUnscopedOwnershipWithoutWriting(t *testing.T) {
	const body = "handwritten stage\n"
	digest := stageDigest([]byte(body))
	scope := stageDigest([]byte("other origin"))
	for _, scenario := range []struct {
		name     string
		metadata string
	}{
		{"bare-checksum", digest + "\n"},
		{"unscoped-current", fmt.Sprintf(`{"version":1,"current":{"sha256":%q}}`, digest)},
		{"unscoped-pending", fmt.Sprintf(`{"version":1,"current":{"sha256":%q,"scope":%q},"pending":{"sha256":%q}}`, digest, scope, digest)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeCIFile(t, "stoke.yaml", "version: 1\ntasks: {test: {image: scratch}}\n")
			path := stageAction("test")
			sidecar := stageChecksumPath(path)
			writeCIFile(t, path, body)
			writeCIFile(t, sidecar, scenario.metadata)

			for _, flags := range [][]string{nil, {"--check"}} {
				runGenerationOwnership(t, 1, "invalid ownership checksum for generated stage "+path, append([]string{"github"}, flags...)...)
				if string(readCIFile(t, path)) != body || string(readCIFile(t, sidecar)) != scenario.metadata {
					t.Fatalf("generate %v modified the stage or sidecar", flags)
				}
			}
		})
	}
}

func TestCIGenerationConflictLeavesOutputUntouched(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCIFile(t, "stoke.yaml", "version: 1\ntasks: {first: {image: scratch}, second: {image: scratch}}\n")
	// The bodies can coexist; the first checksum is a parent of the second.
	writeCIFile(t, "stage.tmpl", `---
output: '{{if eq .Job.ID "first"}}linux.yml{{else}}linux.yml.sha256/stage.yml{{end}}'
---
{{.Command}}
`)
	diagnostic := runGenerationOwnership(t, 1, "", "--template-file", "stage.tmpl")
	if _, err := os.Lstat(".stoke"); !os.IsNotExist(err) {
		t.Fatalf("conflicting batch touched the output tree: %v; diagnostic: %s", err, diagnostic)
	}
	if !strings.Contains(diagnostic, "generated targets conflict") {
		t.Fatalf("expected batch target conflict, got: %s", diagnostic)
	}
}

func TestCIGenerationFullDetectsScopedOrphans(t *testing.T) {
	t.Chdir(t.TempDir())
	const workflow = "version: 1\ntasks: {first: {image: scratch}, test: {image: scratch}}\n"
	writeCIFile(t, "stoke.yaml", workflow)
	template := func(path string) string {
		return "---\noutput: " + path + "/{{.Job.ID}}.sh\n---\n{{.Command}}\n"
	}
	writeCIFile(t, "stage.tmpl", template("before"))
	writeCIFile(t, "other.tmpl", template("other"))
	runGenerationOwnership(t, 0, "", "--template-file", "stage.tmpl")
	runGenerationOwnership(t, 0, "", "--template-file", "other.tmpl")
	const obsolete = ".stoke/ci/before/test.sh"
	const other = ".stoke/ci/other/test.sh"
	const hand = ".stoke/ci/before/handwritten.sh"
	writeCIFile(t, hand, "# handwritten\n")
	otherBody := readCIFile(t, other)
	oldBody := readCIFile(t, obsolete)
	oldChecksumPath := ".stoke/ci/.checksums/before/test.sh.sha256"
	oldChecksum := readCIFile(t, oldChecksumPath)
	changedTemplate := template("after")
	writeCIFile(t, "stage.tmpl", changedTemplate)
	// A single-stage run neither treats other stages as stale nor deletes them.
	runGenerationOwnership(t, 0, "", "first", "--template-file", "./stage.tmpl")
	// Both modes report the stale outputs even while a new output is missing.
	for _, flags := range [][]string{{"--check"}, nil} {
		diagnostic := runGenerationOwnership(t, 1, "obsolete managed stage", append([]string{"--template-file", "./stage.tmpl"}, flags...)...)
		if !strings.Contains(diagnostic, obsolete) || strings.Contains(diagnostic, other) || strings.Contains(diagnostic, hand) {
			t.Fatalf("incorrect ownership report: %s", diagnostic)
		}
		if _, err := os.Lstat(".stoke/ci/after/test.sh"); !os.IsNotExist(err) {
			t.Fatalf("a conflicting full invocation wrote new output: %v", err)
		}
	}
	if !bytes.Equal(readCIFile(t, obsolete), oldBody) ||
		!bytes.Equal(readCIFile(t, oldChecksumPath), oldChecksum) ||
		!bytes.Equal(readCIFile(t, other), otherBody) ||
		string(readCIFile(t, hand)) != "# handwritten\n" ||
		string(readCIFile(t, "stoke.yaml")) != workflow ||
		string(readCIFile(t, "stage.tmpl")) != changedTemplate {
		t.Fatal("generation modified obsolete, unrelated, or authored files")
	}
	runGenerationOwnership(t, 0, "", "--template-file", "other.tmpl", "--check")
}

func TestCIGenerationKeepsExplicitAndDefaultGroupsPartial(t *testing.T) {
	t.Chdir(t.TempDir())
	const workflow = `version: 1
ci-template: github
default-ci: delivery
groups:
  local: [lint]
  delivery: [test]
tasks:
  lint: {image: scratch}
  test: {image: scratch}
  publish: {image: scratch}
`
	writeCIFile(t, "stoke.yaml", workflow)
	runGenerationOwnership(t, 0, "", "github", "local")
	runGenerationOwnership(t, 0, "")
	runGenerationOwnership(t, 0, "", "github", "local", "--check")
	runGenerationOwnership(t, 0, "", "--check")
	runGenerationOwnership(t, 0, "", "github")
	runGenerationOwnership(t, 0, "", "github", "--check")
	// Removing an unrelated stage affects the full run only, even when a
	// configured template and an explicit selector generated the prior outputs.
	writeCIFile(t, "stoke.yaml", strings.ReplaceAll(workflow, "  publish: {image: scratch}\n", ""))
	runGenerationOwnership(t, 0, "", "--check")
	runGenerationOwnership(t, 0, "", "github", "local")
	runGenerationOwnership(t, 1, stageAction("publish"), "github", "--check")
	_ = readCIFile(t, stageAction("publish"))
}

func runGenerationOwnership(t *testing.T, wantCode int, wantDiagnostic string, args ...string) string {
	t.Helper()
	code, out, stderr := invokeCI(append([]string{"ci", "generate"}, args...))
	if code != wantCode || !strings.Contains(stderr, wantDiagnostic) {
		t.Fatalf("generate %v = %d, stdout %q, stderr %q; want code %d and diagnostic %q", args, code, out, stderr, wantCode, wantDiagnostic)
	}
	return stderr
}
