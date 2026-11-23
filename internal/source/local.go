package source

import (
	"errors"
	"os"
	"path/filepath"
)

// Canonical names are used for cycles and ownership; actual reads also use the
// open root, so changing a symlink cannot turn containment into a host read.
func localWorkflowPath(root *os.Root, directory, reference string) (string, error) {
	within := filepath.Join(directory, filepath.FromSlash(reference))
	if !filepath.IsLocal(within) {
		return "", errors.New("local import must stay inside the workflow tree")
	}
	canonical, err := filepath.EvalSymlinks(filepath.Join(root.Name(), within))
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root.Name(), canonical)
	if err != nil || !filepath.IsLocal(relative) {
		return "", errors.New("local import resolves outside the workflow tree")
	}
	return relative, nil
}
