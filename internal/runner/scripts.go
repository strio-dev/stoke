package runner

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/strio-dev/stoke/internal/assets"
	"github.com/strio-dev/stoke/internal/workflow"
)

func scriptName(script string) string {
	return fmt.Sprintf("%x.script", sha256.Sum256([]byte(script)))
}

// Stage definition-owned scripts with the same safe paths and deterministic
// timestamps as packaged assets. Linux handles the original shebang directly.
func stageTaskScripts(task workflow.Task, directory string) error {
	files := map[string]assets.File{}
	for _, step := range task.Steps {
		if step.Script != "" {
			files[scriptName(step.Script)] = assets.File{Data: []byte(step.Script), Executable: true}
		}
	}
	if len(files) == 0 {
		return nil
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return err
	}
	if err := assets.Materialize(directory, files); err != nil {
		return err
	}
	for name := range files {
		if err := os.Chmod(filepath.Join(directory, name), 0555); err != nil {
			return err
		}
	}
	return nil
}
