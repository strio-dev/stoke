package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/strio-dev/stoke/internal/ci"
)

func TestCIGenerationWriterDetectsOrphansOnlyForFullOrigin(t *testing.T) {
	t.Chdir(t.TempDir())
	owner := generationTestScope(t, "stoke.yaml", "stage.tmpl")
	otherOwner := generationTestScope(t, "stoke.yaml", "another.tmpl")
	old := ci.StageFile{Path: ".stoke/ci/before/test.sh", Data: []byte("old stage\n")}
	other := ci.StageFile{Path: ".stoke/ci/other/test.sh", Data: []byte("other stage\n")}
	current := ci.StageFile{Path: ".stoke/ci/after/test.sh", Data: []byte("current stage\n")}
	if err := writeStages([]ci.StageFile{old}, owner, false, false); err != nil {
		t.Fatal(err)
	}
	if err := writeStages([]ci.StageFile{other}, otherOwner, false, false); err != nil {
		t.Fatal(err)
	}
	for _, check := range []bool{true, false} {
		if err := writeStages([]ci.StageFile{current}, owner, check, true); err == nil ||
			!strings.Contains(err.Error(), "obsolete managed stage") ||
			!strings.Contains(err.Error(), old.Path) ||
			strings.Contains(err.Error(), other.Path) {
			t.Fatalf("full invocation (check=%t) returned incorrect error: %v", check, err)
		}
	}
	if _, err := os.Lstat(current.Path); !os.IsNotExist(err) {
		t.Fatalf("an orphaned output allowed changes to other stages: %v", err)
	}
	if err := writeStages([]ci.StageFile{current}, owner, false, false); err != nil {
		t.Fatalf("partial invocation returned an orphan error: %v", err)
	}
	if err := writeStages([]ci.StageFile{other}, otherOwner, true, true); err != nil {
		t.Fatalf("unrelated origin returned an orphan error: %v", err)
	}
	if !bytes.Equal(readCIFile(t, old.Path), old.Data) || !bytes.Equal(readCIFile(t, other.Path), other.Data) {
		t.Fatal("orphan discovery changed an existing stage")
	}
}

func TestCIGenerationWriterRecoversInterruptedBodyAndChecksum(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, stop := range []int{2, 3} {
			t.Run(fmt.Sprintf("existing=%t/stop-before-write-%d", existing, stop), func(t *testing.T) {
				t.Chdir(t.TempDir())
				scope := generationTestScope(t, "stoke.yaml", "stage.tmpl")
				stage := ci.StageFile{Path: ".stoke/ci/target.sh", Data: []byte("generated\n")}
				if existing {
					previous := ci.StageFile{Path: stage.Path, Data: []byte("prior generated\n")}
					if err := writeStages([]ci.StageFile{previous}, scope, false, false); err != nil {
						t.Fatal(err)
					}
				}
				update, err := prepareStageUpdate(stage, scope, false)
				if err != nil {
					t.Fatal(err)
				}
				interrupted := errors.New("writer interrupted")
				calls := 0
				err = update.apply(func(path string, data []byte) error {
					calls++
					if calls == stop {
						return interrupted
					}
					return writeStageFile(path, data)
				})
				if !errors.Is(err, interrupted) {
					t.Fatalf("interruption was not observed: %v", err)
				}
				pending := readCIFile(t, update.checksum)
				if err := writeStages([]ci.StageFile{stage}, scope, true, false); err == nil || !strings.Contains(err.Error(), "stale") {
					t.Fatalf("--check accepted unfinished ownership: %v", err)
				}
				if !bytes.Equal(pending, readCIFile(t, update.checksum)) {
					t.Fatal("--check changed pending metadata")
				}
				if err := writeStages([]ci.StageFile{stage}, scope, false, false); err != nil {
					t.Fatalf("could not recover: %v", err)
				}
				if err := writeStages([]ci.StageFile{stage}, scope, true, false); err != nil {
					t.Fatalf("recovered stage did not pass --check: %v", err)
				}
				if !bytes.Equal(readCIFile(t, stage.Path), stage.Data) {
					t.Fatal("recovery did not retain the intended stage")
				}
			})
		}
	}
}

func TestCIGenerationWriterDoesNotBlessEditDuringRecovery(t *testing.T) {
	t.Chdir(t.TempDir())
	scope := generationTestScope(t, "stoke.yaml", "stage.tmpl")
	stage := ci.StageFile{Path: ".stoke/ci/target.sh", Data: []byte("pending generate\n")}
	update, err := prepareStageUpdate(stage, scope, false)
	if err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("writer interrupted")
	if err := update.apply(func(path string, data []byte) error {
		if path == stage.Path {
			return interrupted
		}
		return writeStageFile(path, data)
	}); !errors.Is(err, interrupted) {
		t.Fatalf("interruption was not observed: %v", err)
	}
	metadata := readCIFile(t, update.checksum)
	// The next template render happens to equal the intervening edit.
	stage.Data = []byte("edited outside the generator\n")
	writeCIFile(t, stage.Path, string(stage.Data))
	if err := writeStages([]ci.StageFile{stage}, scope, false, false); err == nil || !strings.Contains(err.Error(), "edited stage") {
		t.Fatalf("accepted bytes not authorized by the pending write: %v", err)
	}
	if !bytes.Equal(metadata, readCIFile(t, update.checksum)) ||
		!bytes.Equal(stage.Data, readCIFile(t, stage.Path)) {
		t.Fatal("refusing the edit modified the file or sidecar")
	}
}

func TestCIGenerationWriterUsesActualOwnerDuringInterruptedTransfer(t *testing.T) {
	t.Chdir(t.TempDir())
	first := generationTestScope(t, "stoke.yaml", "first.tmpl")
	second := generationTestScope(t, "stoke.yaml", "second.tmpl")
	before := ci.StageFile{Path: ".stoke/ci/shared.sh", Data: []byte("first body\n")}
	after := ci.StageFile{Path: before.Path, Data: []byte("second body\n")}
	keep := ci.StageFile{Path: ".stoke/ci/keep.sh", Data: []byte("unchanged\n")}
	if err := writeStages([]ci.StageFile{before, keep}, first, false, false); err != nil {
		t.Fatal(err)
	}
	update, err := prepareStageUpdate(after, second, false)
	if err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("writer interrupted")
	calls := 0
	if err := update.apply(func(path string, data []byte) error {
		calls++
		if calls == 3 {
			return interrupted
		}
		return writeStageFile(path, data)
	}); !errors.Is(err, interrupted) {
		t.Fatalf("interruption was not observed: %v", err)
	}
	if err := writeStages([]ci.StageFile{keep}, first, true, true); err != nil {
		t.Fatalf("first origin incorrectly claims second origin's bytes: %v", err)
	}
	if err := writeStages([]ci.StageFile{keep}, second, true, true); err == nil || !strings.Contains(err.Error(), after.Path) {
		t.Fatalf("second origin did not find its obsolete output: %v", err)
	}
	if err := writeStages([]ci.StageFile{after}, second, false, false); err != nil {
		t.Fatalf("second origin could not finish the transfer: %v", err)
	}
}

func TestCIGenerationWriterChecksLegacyChecksumWithoutInferringOrigin(t *testing.T) {
	t.Chdir(t.TempDir())
	scope := generationTestScope(t, "stoke.yaml", "stage.tmpl")
	legacy := ci.StageFile{Path: ".stoke/ci/legacy.sh", Data: []byte("legacy output\n")}
	legacyChecksum := ci.StageChecksum(legacy.Data)
	writeCIFile(t, legacy.Path, string(legacy.Data))
	legacySidecar := stageChecksumPath(legacy.Path)
	writeCIFile(t, legacySidecar, string(legacyChecksum))
	if err := writeStages([]ci.StageFile{legacy}, scope, true, true); err != nil {
		t.Fatalf("legacy generated output is not checkable: %v", err)
	}
	if !bytes.Equal(legacyChecksum, readCIFile(t, legacySidecar)) {
		t.Fatal("--check rewrote a legacy checksum")
	}
	other := ci.StageFile{Path: ".stoke/ci/new.sh", Data: []byte("new output\n")}
	if err := writeStages([]ci.StageFile{other}, scope, false, true); err != nil {
		t.Fatalf("legacy sidecar was incorrectly attributed: %v", err)
	}
	if err := writeStages([]ci.StageFile{legacy}, scope, false, false); err != nil {
		t.Fatalf("selected legacy file could not be adopted: %v", err)
	}
	if err := writeStages([]ci.StageFile{other}, scope, true, true); err == nil || !strings.Contains(err.Error(), legacy.Path) {
		t.Fatalf("adopted stage was not reported as obsolete: %v", err)
	}
}

func generationTestScope(t *testing.T, workflow, template string) string {
	t.Helper()
	scope, err := stageGenerationScope(workflow, "", template)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}
