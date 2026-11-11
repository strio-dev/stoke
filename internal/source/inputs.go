package source

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/strio-dev/stoke/internal/workflow"
)

// ValidateComponentContext keeps loader-owned package files out of the caller's
// primary context, including when TMPDIR points into the project tree.
func ValidateComponentContext(componentDir, callerDir string) error {
	if componentDir == "" {
		return nil
	}
	resolve := func(directory string) (string, error) {
		absolute, err := filepath.Abs(directory)
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(absolute)
	}
	component, err := resolve(componentDir)
	if err != nil {
		return err
	}
	caller, err := resolve(callerDir)
	if err != nil {
		return err
	}
	within := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if within(component, caller) || within(caller, component) {
		return errors.New("component assets overlap the caller context; select TMPDIR outside the input context")
	}
	return nil
}

// DefinitionFilesForTask resolves definitions from the caller or the owning
// component, independently from the native build's primary caller context.
func DefinitionFilesForTask(task workflow.Task, caller string) ([]string, error) {
	build := task.Coopr
	if build == nil {
		build = task.Environment
	}
	if build == nil {
		return nil, errors.New("task has no native definition files")
	}
	directory := caller
	if build.FilesFrom == "component" {
		if task.ComponentDir == "" {
			return nil, errors.New("component definition files must be resolved before execution")
		}
		directory = task.ComponentDir
	}
	return DefinitionFiles(directory, build.Files)
}

// DefinitionFiles resolves caller-provided native definition files inside the
// context. It checks host paths only; Coopr owns definition parsing and semantics.
func DefinitionFiles(contextDir string, files []string) ([]string, error) {
	if len(files) == 0 {
		return nil, errors.New("native build requires definition files")
	}
	contextDir, err := filepath.Abs(contextDir)
	if err != nil {
		return nil, err
	}
	contextDir, err = filepath.EvalSymlinks(contextDir)
	if err != nil {
		return nil, fmt.Errorf("definition context: %w", err)
	}
	info, err := os.Stat(contextDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("definition context must be a directory")
	}
	resolved := make([]string, 0, len(files))
	seen := map[string]bool{}
	for _, file := range files {
		clean := filepath.Clean(file)
		if file == "" || strings.ContainsRune(file, 0) || filepath.IsAbs(file) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("definition file %q must be a context-relative path", file)
		}
		path, err := filepath.EvalSymlinks(filepath.Join(contextDir, clean))
		if err != nil {
			return nil, fmt.Errorf("definition file %q: %w", file, err)
		}
		relative, err := filepath.Rel(contextDir, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("definition file %q resolves outside the context", file)
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("definition file %q: %w", file, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("definition file %q must be a regular file", file)
		}
		if seen[path] {
			return nil, fmt.Errorf("duplicate definition file %q", file)
		}
		seen[path] = true
		resolved = append(resolved, path)
	}
	return resolved, nil
}
