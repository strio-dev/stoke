package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	cooprwire "github.com/strio-dev/coopr/api"
	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
	"golang.org/x/sys/unix"
)

// Admit every selected job before starting an engine or creating run state.
// Matrix variants must declare distinct destinations just as separate jobs do.
func admitFileExports(plan *workflow.ExecutionPlan, workspace string) error {
	var destinations []struct{ job, path string }
	for _, group := range plan.Groups {
		for _, job := range group.Jobs {
			for _, export := range job.Definition.FileExports() {
				if !filepath.IsLocal(export.Destination) || export.Destination == "." || path.Clean(export.Destination) != export.Destination {
					return fmt.Errorf("task %q: selected file destination must be a relative workspace path", job.Name)
				}
				for _, previous := range destinations {
					if overlap(previous.path, export.Destination) {
						return fmt.Errorf("tasks %q and %q have overlapping file export destinations", previous.job, job.Name)
					}
				}
				destinations = append(destinations, struct{ job, path string }{job.Name, export.Destination})
			}
		}
	}
	if len(destinations) == 0 {
		return nil
	}
	for _, group := range plan.Groups {
		for _, job := range group.Jobs {
			inputs := []string{job.Definition.ComponentDir}
			for _, directory := range job.Definition.Contexts {
				inputs = append(inputs, directory)
			}
			for _, mount := range job.Definition.AllMounts() {
				if mount.Type == "host" {
					inputs = append(inputs, mount.Source)
				}
			}
			for _, input := range inputs {
				if input == "" {
					continue
				}
				relative, err := filepath.Rel(input, filepath.Dir(workspace))
				if err != nil || filepath.IsLocal(relative) {
					return fmt.Errorf("task %q: private file export staging would overlap an input or live host mount", job.Name)
				}
			}
		}
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	// Sibling staging stays outside Coopr's input context and permits a single
	// no-replace rename. A nested mount requires an explicit different layout.
	var stagingDevice unix.Stat_t
	if err := unix.Stat(filepath.Dir(workspace), &stagingDevice); err != nil {
		return err
	}
	if err := unix.Access(filepath.Dir(workspace), unix.W_OK|unix.X_OK); err != nil {
		return fmt.Errorf("workspace parent cannot hold private file export staging: %w", err)
	}
	for _, destination := range destinations {
		if _, err := root.Lstat(destination.path); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				err = errors.New("destination already exists")
			}
			return fmt.Errorf("task %q file export %q: %w", destination.job, destination.path, err)
		}
		parent, err := fileExportParent(root, destination.path, false)
		if err != nil {
			return err
		}
		var device unix.Stat_t
		statErr := unix.Fstat(int(parent.Fd()), &device)
		accessErr := unix.Faccessat(int(parent.Fd()), ".", unix.W_OK|unix.X_OK, unix.AT_EACCESS)
		if err := errors.Join(statErr, accessErr, parent.Close()); err != nil {
			return fmt.Errorf("task %q file export %q parent: %w", destination.job, destination.path, err)
		}
		if device.Dev != stagingDevice.Dev {
			return fmt.Errorf("task %q file export %q crosses the workspace staging filesystem", destination.job, destination.path)
		}
	}
	return nil
}

// Return a pinned ordinary parent, or the nearest existing parent at admission.
// Root operations confine resolution even if a caller changes a parent later.
func fileExportParent(root *os.Root, destination string, create bool) (*os.File, error) {
	if !filepath.IsLocal(destination) || path.Clean(destination) != destination || destination == "." {
		return nil, errors.New("file export destination must be a canonical relative path")
	}
	current, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	for _, component := range strings.Split(path.Dir(destination), "/") {
		if component == "." {
			continue
		}
		fd, err := unix.Openat(int(current.Fd()), component, flags, 0)
		if errors.Is(err, unix.ENOENT) {
			if !create {
				return current, nil
			}
			if err = unix.Mkdirat(int(current.Fd()), component, 0755); err == nil || errors.Is(err, unix.EEXIST) {
				fd, err = unix.Openat(int(current.Fd()), component, flags, 0)
			}
		}
		if err != nil {
			return nil, errors.Join(err, current.Close())
		}
		next := os.NewFile(uintptr(fd), component)
		if err := current.Close(); err != nil {
			return nil, errors.Join(err, next.Close())
		}
		current = next
	}
	return current, nil
}

func skippedJobFileExports(job workflow.Job, workspace, reason string) []cooprwire.ExportResult {
	var results []cooprwire.ExportResult
	states := fileExportStates(job.Name, job.Definition.Steps)
	for i, export := range job.Definition.FileExports() {
		results = append(results, cooprwire.ExportResult{State: states[i], Source: export.Source, Destination: filepath.Join(workspace, filepath.FromSlash(export.Destination)), Status: "skipped", Error: reason})
	}
	return results
}

// Adjacent exports share one filesystem checkpoint; later operations begin a
// new state. Destinations never participate in the image mutation graph.
func fileExportStates(name string, steps []workflow.Step) []string {
	var states []string
	current := ""
	for i, step := range steps {
		if step.Export == nil {
			current = ""
			continue
		}
		if current == "" {
			current = fmt.Sprintf("%s-export-%d", name, i)
		}
		states = append(states, current)
	}
	return states
}

func stageJobFileExports(job workflow.Job, workspace string) (string, []cooprwire.FileExport, error) {
	if len(job.Definition.FileExports()) == 0 {
		return "", nil, nil
	}
	staging, err := os.MkdirTemp(filepath.Dir(workspace), ".stoke-file-exports-*")
	if err != nil {
		return "", nil, err
	}
	exports := make([]cooprwire.FileExport, len(job.Definition.FileExports()))
	states := fileExportStates(job.Name, job.Definition.Steps)
	for i, export := range job.Definition.FileExports() {
		exports[i] = cooprwire.FileExport{State: states[i], Source: export.Source, Destination: filepath.Join(staging, fmt.Sprint(i))}
	}
	return staging, exports, nil
}

func publishJobFileExports(ctx context.Context, job workflow.Job, workspace, staging string, exports []cooprwire.FileExport, receipts []cooprwire.ExportResult, runErr error) ([]cooprwire.ExportResult, error) {
	if len(exports) == 0 {
		return nil, nil
	}
	skipped := skippedJobFileExports(job, workspace, "task did not produce confirmed selected files")
	if ctx.Err() != nil {
		return skipped, ctx.Err()
	}
	if errors.Is(runErr, coopr.ErrCleanupFailed) {
		return skipped, nil
	}
	if len(receipts) == 0 && runErr != nil {
		return skipped, nil
	}
	if len(receipts) != len(exports) {
		return skipped, errors.New("engine omitted selected file export receipts")
	}
	for i, receipt := range receipts {
		if receipt.State != exports[i].State || receipt.Source != exports[i].Source || receipt.Destination != exports[i].Destination ||
			(receipt.Status != "passed" && receipt.Status != "failed" && receipt.Status != "skipped") || receipt.Status == "passed" && receipt.Error != "" {
			return skipped, errors.New("engine file export receipt differs from the selected request")
		}
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return skipped, err
	}
	defer func() { _ = root.Close() }()
	source, err := os.Open(staging)
	if err != nil {
		return skipped, err
	}
	defer func() { _ = source.Close() }()
	results := append([]cooprwire.ExportResult(nil), receipts...)
	var failures error
	for i := range results {
		destination := job.Definition.FileExports()[i].Destination
		results[i].Destination = filepath.Join(workspace, filepath.FromSlash(destination))
		if ctx.Err() != nil {
			results[i].Status, results[i].Error = "skipped", "workflow canceled"
			failures = errors.Join(failures, ctx.Err())
			continue
		}
		if results[i].Status != "passed" {
			if runErr == nil {
				failures = errors.Join(failures, fmt.Errorf("file export %q was %s: %s", destination, results[i].Status, results[i].Error))
			}
			continue
		}
		var info unix.Stat_t
		err := unix.Fstatat(int(source.Fd()), fmt.Sprint(i), &info, unix.AT_SYMLINK_NOFOLLOW)
		if err == nil && info.Mode&unix.S_IFMT != unix.S_IFDIR && info.Mode&unix.S_IFMT != unix.S_IFREG {
			err = errors.New("selected export root is not a regular file or directory")
		}
		var parent *os.File
		if err == nil {
			parent, err = fileExportParent(root, destination, true)
			if err == nil {
				err = ctx.Err()
				if err == nil {
					err = unix.Renameat2(int(source.Fd()), fmt.Sprint(i), int(parent.Fd()), path.Base(destination), unix.RENAME_NOREPLACE)
				}
				err = errors.Join(err, parent.Close())
			}
		}
		if err != nil {
			results[i].Status, results[i].Error = "failed", err.Error()
			failures = errors.Join(failures, fmt.Errorf("publish file export %q: %w", destination, err))
		}
	}
	return results, failures
}
