package workflow

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"unicode/utf8"
)

// IsLocalImport identifies explicit local syntax, never a relocated reference.
func IsLocalImport(reference string) bool { return strings.HasPrefix(reference, "./") }

// ValidateImportReference accepts explicit local syntax or a pinned OCI import.
// The loader checks filesystem availability and containment.
func ValidateImportReference(reference string) error {
	if IsLocalImport(reference) {
		if path.Clean(reference) == "." || !utf8.ValidString(reference) || strings.ContainsAny(reference, "\\\x00\r\n") {
			return errors.New("local import requires a ./relative YAML file path")
		}
		return nil // Declaring-file-relative containment is checked by the loader.
	}
	if !importPattern.MatchString(reference) || strings.Contains(reference, "://") {
		return errors.New("requires a ./relative YAML path or an OCI reference pinned with @sha256")
	}
	return nil
}

// ValidatePortableImports rejects local dependencies before publication starts.
func (w *Workflow) ValidatePortableImports() error {
	for _, alias := range slices.Sorted(maps.Keys(w.Imports)) {
		if IsLocalImport(w.Imports[alias]) {
			return fmt.Errorf("import %q is local: publish the dependency to OCI and use its digest-pinned reference before publishing this workflow", alias)
		}
	}
	return nil
}
