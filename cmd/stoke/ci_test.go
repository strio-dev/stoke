package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/strio-dev/stoke/internal/ci"
)

func TestCIGenerationPreservesHandwrittenWorkflows(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCIFile(t, "stoke.yaml", "version: 1\ntasks:\n  build: {image: scratch}\n  test:\n    from: build\n    cache: false\n    strategy: {matrix: {mode: [debug, release]}}\n  done: {image: scratch, needs: [test]}\n")
	handwritten := "# owned by the project\nname: Manual\non: push\njobs: {}\n"
	writeCIFile(t, ".github/workflows/ci.yml", handwritten)
	args := []string{"ci", "generate", "done"}
	if code, _, stderr := invokeCI(args); code != 0 {
		t.Fatalf("generate exit=%d: %s", code, stderr)
	}
	planBefore := readCIFile(t, ".stoke/ci-plan.json")
	generatedBefore := readCIFile(t, ".github/workflows/stoke.generated.yml")
	plan, err := ci.Decode(bytes.NewReader(planBefore))
	if err != nil || len(plan.Groups) != 3 || !plan.RequiresResults {
		t.Fatalf("data-connected stages were hidden: %#v %v", plan, err)
	}
	var buildID, testID string
	for _, group := range plan.Groups {
		if slices.Contains(group.Members, "build") {
			buildID = group.ID
			if !slices.Equal(group.Members, []string{"build"}) || len(group.Jobs) != 1 || !slices.Equal(group.Jobs[0].Exports, []string{"build"}) {
				t.Fatalf("build producer is not independently visible: %#v", group)
			}
		}
		if slices.Contains(group.Members, "test") {
			testID = group.ID
			if !group.Matrix || len(group.Jobs) != 2 {
				t.Fatalf("test variants are not native matrix jobs: %#v", group)
			}
			for _, job := range group.Jobs {
				if len(job.Execute) != 1 || !slices.Equal(job.Imports, []string{"build"}) {
					t.Fatal("test variant would replay a producer")
				}
			}
		}
	}
	for _, group := range plan.Groups {
		if slices.Contains(group.Members, "test") && (buildID == "" || !slices.Equal(group.Needs, []string{buildID})) {
			t.Fatal("test lost native build success gate")
		}
		if slices.Contains(group.Members, "done") && (testID == "" || !slices.Equal(group.Needs, []string{testID})) {
			t.Fatal("independent downstream job lost its provider success gate")
		}
	}
	for _, again := range [][]string{args, {"ci", "generate", "--check", "done"}} {
		if code, _, stderr := invokeCI(again); code != 0 {
			t.Fatalf("repeat/check exit=%d: %s", code, stderr)
		}
	}
	if !bytes.Equal(planBefore, readCIFile(t, ".stoke/ci-plan.json")) || !bytes.Equal(generatedBefore, readCIFile(t, ".github/workflows/stoke.generated.yml")) {
		t.Fatal("regeneration changed identical output")
	}
	if string(readCIFile(t, ".github/workflows/ci.yml")) != handwritten {
		t.Fatal("handwritten workflow changed")
	}
	writeCIFile(t, "stoke.yaml", "version: 1\ntasks: {done: {image: scratch}}\n")
	if code, _, stderr := invokeCI([]string{"ci", "generate", "--check", "done"}); code != 1 || !strings.Contains(stderr, "stale") {
		t.Fatalf("drift accepted exit=%d: %s", code, stderr)
	}
	if !bytes.Equal(planBefore, readCIFile(t, ".stoke/ci-plan.json")) {
		t.Fatal("--check wrote files")
	}
	if code, _, stderr := invokeCI(args); code != 0 {
		t.Fatalf("reviewable update rejected exit=%d: %s", code, stderr)
	}
}

func TestCIGenerationProtectsExistingAndEditedFiles(t *testing.T) {
	for _, edited := range []bool{false, true} {
		t.Run(map[bool]string{false: "handwritten", true: "edited generated"}[edited], func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeCIFile(t, "stoke.yaml", "version: 1\ntasks: {test: {image: scratch}}\n")
			if edited {
				if code, _, stderr := invokeCI([]string{"ci", "generate"}); code != 0 {
					t.Fatal(stderr)
				}
			}
			path := ".github/workflows/stoke.generated.yml"
			before := []byte("# handwritten content\n")
			if edited {
				before = append(readCIFile(t, path), []byte("# manual edit\n")...)
			}
			writeCIFile(t, path, string(before))
			if code, _, stderr := invokeCI([]string{"ci", "generate"}); code != 1 || !strings.Contains(stderr, "refusing to overwrite") {
				t.Fatalf("unowned changes accepted exit=%d: %s", code, stderr)
			}
			if !bytes.Equal(before, readCIFile(t, path)) {
				t.Fatal("existing content overwritten")
			}
			if !edited {
				if _, err := os.Stat(".stoke"); !os.IsNotExist(err) {
					t.Fatalf("failed preflight wrote plan directory: %v", err)
				}
			}
		})
	}
}

func TestCICheckHasNoWritesAndRejectsSymlinkDestinations(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCIFile(t, "stoke.yaml", "version: 1\ntasks: {test: {image: scratch}}\n")
	if code, _, _ := invokeCI([]string{"ci", "generate", "--check"}); code != 1 {
		t.Fatal("missing generated files accepted")
	}
	if _, err := os.Stat(".stoke"); !os.IsNotExist(err) {
		t.Fatal("check created directory")
	}
	external := t.TempDir()
	if err := os.Symlink(external, ".github"); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := invokeCI([]string{"ci", "generate"}); code != 1 || !strings.Contains(stderr, "symlink") {
		t.Fatalf("symlink accepted exit=%d: %s", code, stderr)
	}
	entries, err := os.ReadDir(external)
	if err != nil || len(entries) != 0 {
		t.Fatalf("external tree changed: %v %v", entries, err)
	}
}

func TestCIProviderResultGate(t *testing.T) {
	for _, result := range []string{"failure", "cancelled", "skipped", ""} {
		if err := verifyCIResults(`{"build":{"result":"success"},"test":{"result":"` + result + `"}}`); err == nil {
			t.Fatalf("accepted result %q", result)
		}
	}
	for _, raw := range []string{"", "null", "{}", "[]", `{"test":{}}`} {
		if err := verifyCIResults(raw); err == nil {
			t.Fatalf("accepted malformed/empty results %q", raw)
		}
	}
	if err := verifyCIResults(`{"build":{"result":"success","outputs":{}},"test":{"result":"success"}}`); err != nil {
		t.Fatal(err)
	}
}

func TestCIRunRejectsChangedPlanBeforeStartingEngine(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCIFile(t, "stoke.yaml", "version: 1\ntasks: {test: {image: scratch}}\n")
	if code, _, stderr := invokeCI([]string{"ci", "generate"}); code != 0 {
		t.Fatal(stderr)
	}
	plan, err := ci.Decode(bytes.NewReader(readCIFile(t, ".stoke/ci-plan.json")))
	if err != nil {
		t.Fatal(err)
	}
	writeCIFile(t, "stoke.yaml", "version: 1\ntasks: {test: {image: scratch, env: {CHANGED: yes}}}\n")
	args := []string{"ci", "run", "--job", plan.Groups[0].Jobs[0].ID, "--coopr-binary", "/missing/coopr"}
	if code, _, stderr := invokeCI(args); code != 1 || !strings.Contains(stderr, "differs from the generated CI plan") {
		t.Fatalf("changed workflow accepted exit=%d: %s", code, stderr)
	}
}

func TestCIRunRejectsInterruptedGenerationPair(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCIFile(t, "stoke.yaml", "version: 1\ntasks: {first: {image: scratch}}\n")
	if code, _, stderr := invokeCI([]string{"ci", "generate"}); code != 0 {
		t.Fatal(stderr)
	}
	old, err := ci.Decode(bytes.NewReader(readCIFile(t, ".stoke/ci-plan.json")))
	if err != nil {
		t.Fatal(err)
	}
	// Both source and plan are valid, but an old YAML file would omit this new
	// independent task. Its embedded old plan ID must prevent a partial pass.
	writeCIFile(t, "stoke.yaml", "version: 1\ntasks: {first: {image: scratch}, second: {image: scratch}}\n")
	if code, _, stderr := invokeCI([]string{"ci", "generate"}); code != 0 {
		t.Fatal(stderr)
	}
	args := []string{"ci", "run", "--plan-id", old.ID, "--job", "first", "--coopr-binary", "/missing/coopr"}
	if code, _, stderr := invokeCI(args); code != 1 || !strings.Contains(stderr, "fingerprints differ") {
		t.Fatalf("mixed pair accepted: exit=%d %s", code, stderr)
	}
}

func TestCIRejectsEscapingCheckoutInputs(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCIFile(t, "stoke.yaml", "version: 1\ntasks: {test: {image: scratch}}\n")
	external := t.TempDir()
	if err := os.Symlink(external, "external"); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := invokeCI([]string{"ci", "plan", "--context", "external"}); code != 1 || !strings.Contains(stderr, "escapes the checkout") {
		t.Fatalf("escape accepted: exit=%d %s", code, stderr)
	}
}

func TestCIContextValidationUsesSelectedClosure(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCIFile(t, "stoke.yaml", "version: 1\ntasks:\n  test: {image: scratch}\n  dev: {image: scratch, contexts: {local: missing-on-CI}}\n")
	if code, _, stderr := invokeCI([]string{"ci", "plan", "test"}); code != 0 {
		t.Fatalf("unselected context blocked selection: exit=%d %s", code, stderr)
	}
	if code, _, stderr := invokeCI([]string{"ci", "plan", "dev"}); code != 1 || !strings.Contains(stderr, "unavailable") {
		t.Fatalf("selected missing context accepted: exit=%d %s", code, stderr)
	}
}

func TestCICacheDirectionsAreGeneratedWithoutArtifactTransport(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCIFile(t, "stoke.yaml", "version: 1\ntasks: {build: {image: scratch}, test: {image: scratch, needs: [build], cache: false}}\n")
	args := []string{"ci", "generate", "--cache-from", "registry.example.com/team/read", "--cache-from", "registry.example.com/team/fallback", "--cache-to", "registry.example.com/team/write", "test"}
	if code, _, stderr := invokeCI(args); code != 0 {
		t.Fatal(stderr)
	}
	p, err := ci.Decode(bytes.NewReader(readCIFile(t, ".stoke/ci-plan.json")))
	if err != nil || len(p.Config.CacheFrom) != 2 || len(p.Config.CacheTo) != 1 {
		t.Fatalf("cache configuration lost: %#v %v", p, err)
	}
	generated := string(readCIFile(t, ".github/workflows/stoke.generated.yml"))
	for _, want := range []string{"--cache-from 'registry.example.com/team/read'", "--cache-from 'registry.example.com/team/fallback'", "--cache-to 'registry.example.com/team/write'"} {
		if !strings.Contains(generated, want) {
			t.Fatalf("missing direction %q", want)
		}
	}
	for _, forbidden := range []string{"upload-artifact", "download-artifact", "--bundle", "--handoffs", "STOKE_CI_ARTIFACT"} {
		if strings.Contains(generated, forbidden) {
			t.Fatalf("mandatory archive protocol retained: %q", forbidden)
		}
	}
	if code, _, stderr := invokeCI([]string{"ci", "plan", "--cache-from", "https://registry.example.com/team/cache"}); code != 2 || !strings.Contains(stderr, "cache") {
		t.Fatalf("bad cache repository accepted: %d %s", code, stderr)
	}
}

func writeCIFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func readCIFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func invokeCI(args []string) (int, string, string) {
	var out, stderr bytes.Buffer
	code := run(args, &out, &stderr)
	return code, out.String(), stderr.String()
}

func TestCIContainerGenerationChecksExactImageAndOptions(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCIFile(t, "stoke.yaml", "version: 1\ntasks: {test: {image: scratch}}\n")
	image := "registry.example.test/team/stoke@sha256:" + strings.Repeat("a", 64)
	args := []string{"ci", "generate", "--job-container-image", image, "--job-container-options", "--user=1001:1001"}
	if code, _, stderr := invokeCI(args); code != 0 {
		t.Fatal(stderr)
	}
	plan, err := ci.Decode(bytes.NewReader(readCIFile(t, ".stoke/ci-plan.json")))
	if err != nil || plan.Config.JobContainerImage != image || plan.Config.JobContainerOptions != "--user=1001:1001" {
		t.Fatal("container flags not recorded", plan, err)
	}
	if code, _, stderr := invokeCI(append(slices.Clone(args), "--check")); code != 0 {
		t.Fatal(stderr)
	}
	before := readCIFile(t, ".github/workflows/stoke.generated.yml")
	if code, _, stderr := invokeCI([]string{"ci", "generate", "--check"}); code != 1 || !strings.Contains(stderr, "stale") {
		t.Fatalf("omitted container selection accepted: exit=%d %s", code, stderr)
	}
	if !bytes.Equal(before, readCIFile(t, ".github/workflows/stoke.generated.yml")) {
		t.Fatal("failed container check changed workflow")
	}
}
