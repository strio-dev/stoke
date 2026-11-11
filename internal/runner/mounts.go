package runner

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
)

// ValidateMountPaths checks local mount availability without changing the
// authored definition. It is also used by `stoke check`, before any execution.
func ValidateMountPaths(task workflow.Task, contextDir string) error {
	task.Mounts = slices.Clone(task.Mounts)
	task.Steps = slices.Clone(task.Steps)
	for index := range task.Steps {
		task.Steps[index].Mounts = slices.Clone(task.Steps[index].Mounts)
	}
	return resolveTaskMounts(&task, contextDir)
}

// Paths are resolved once against the invocation's caller context, never the
// service working directory or the directory containing a reusable component.
func resolveTaskMounts(task *workflow.Task, contextDir string) error {
	if task.Contexts != nil {
		task.Contexts = maps.Clone(task.Contexts)
		for name, directory := range task.Contexts {
			resolved, err := mountDirectory(contextDir, directory)
			if err != nil {
				return fmt.Errorf("named context %q: %w", name, err)
			}
			task.Contexts[name] = resolved
		}
	}
	resolve := func(mounts []workflow.Mount) error {
		for index := range mounts {
			mount := &mounts[index]
			if mount.Type != "host" {
				continue
			}
			resolved, err := mountDirectory(contextDir, mount.Source)
			if err != nil {
				return fmt.Errorf("live host mount %q: %w", mount.Target, err)
			}
			mount.Source = resolved
		}
		return nil
	}
	if err := resolve(task.Mounts); err != nil {
		return err
	}
	for index := range task.Steps {
		if err := resolve(task.Steps[index].Mounts); err != nil {
			return err
		}
	}
	return nil
}

func mountDirectory(base, directory string) (string, error) {
	if !filepath.IsAbs(directory) {
		directory = filepath.Join(base, directory)
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return "", fmt.Errorf("directory %q is unavailable: %w", directory, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("directory %q is unavailable: %w", directory, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("mount source %q must be a directory", directory)
	}
	return resolved, nil
}

func compileMount(mount workflow.Mount, results map[string]coopr.Result) coopr.Mount {
	value := coopr.Mount{Type: "bind", Target: mount.Target, Source: mount.Source, ReadOnly: mount.IsReadOnly()}
	switch mount.Type {
	case "context":
		value.From.Context = mount.Context
	case "host":
		value.HostPath, value.Source = mount.Source, ""
		value.Relabel = mount.Relabel
	case "cache":
		value.Type, value.ID, value.Sharing = "cache", mount.ID, mount.Sharing
	case "image":
		value.From.Image = mount.Image
	case "stage":
		value.From.Image = "containers-storage:" + results[mount.From].ImageID
	case "artifact":
		value.From.Image = mount.Ref
	}
	return value
}
