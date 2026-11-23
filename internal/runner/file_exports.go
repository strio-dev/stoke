package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	cooprwire "github.com/strio-dev/coopr/api"
	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
	"golang.org/x/sys/unix"
)

// Admit every selected job before starting an engine or creating run state.
// Matrix variants must declare distinct destinations just as separate jobs do.
func admitFileExports(plan *workflow.ExecutionPlan, workspace string, prior, restore map[string]bool) error {
	type destination struct {
		job, path string
		step      int
	}
	var destinations []destination
	for _, group := range plan.Groups {
		for _, job := range group.Jobs {
			if restore[job.Name] {
				continue
			}
			for i, step := range job.Definition.Steps {
				export := step.Export
				if export == nil {
					continue
				}
				if !filepath.IsLocal(export.Destination) || export.Destination == "." || path.Clean(export.Destination) != export.Destination {
					return fmt.Errorf("task %q: selected file destination must be a relative workspace path", job.Name)
				}
				for _, previous := range destinations {
					if overlap(previous.path, export.Destination) {
						return fmt.Errorf("tasks %q and %q have overlapping file export destinations", previous.job, job.Label())
					}
				}
				destinations = append(destinations, destination{job: job.Label(), path: export.Destination, step: i + 1})
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
	// Sibling staging stays outside Coopr's input context and on the same
	// filesystem as the export destinations.
	var stagingDevice unix.Stat_t
	if err := unix.Stat(filepath.Dir(workspace), &stagingDevice); err != nil {
		return err
	}
	if err := unix.Access(filepath.Dir(workspace), unix.W_OK|unix.X_OK); err != nil {
		return fmt.Errorf("workspace parent cannot hold private file export staging: %w", err)
	}
	for _, destination := range destinations {
		if info, err := root.Lstat(destination.path); !errors.Is(err, os.ErrNotExist) {
			switch {
			case err != nil:
			case prior != nil && (info.Mode().IsRegular() || info.IsDir()):
				prior[destination.path] = true
			case prior != nil:
				err = fmt.Errorf("destination is neither a regular file nor a directory: %w", os.ErrExist)
			default:
				err = os.ErrExist
			}
			if err != nil {
				return fmt.Errorf("task %q step %d file export %q: %w", destination.job, destination.step, filepath.Join(workspace, destination.path), fileExportPathError(err))
			}
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

func fileExportPathError(err error) error {
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%w. Move the existing path aside, choose a fresh workspace, or set an unused export destination. --output does not relocate this export", err)
	}
	return err
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

func publishJobFileExports(ctx context.Context, job workflow.Job, workspace, staging string, exports []cooprwire.FileExport, receipts []cooprwire.ExportResult, prior map[string]bool, runErr error) ([]cooprwire.ExportResult, error) {
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
				err = placeFileExport(ctx, source, fmt.Sprint(i), parent, path.Base(destination), prior[destination])
				err = errors.Join(err, parent.Close())
			}
		}
		if err != nil {
			results[i].Status, results[i].Error = "failed", err.Error()
			failures = errors.Join(failures, fmt.Errorf("publish file export %q: %w", destination, fileExportPathError(err)))
		}
	}
	return results, failures
}

// Publish new entries by moving them from the private staging tree. An existing
// directory is traversed through pinned descriptors, visiting incoming entries
// only. The admission decision still protects a newly occupied export path.
func placeFileExport(ctx context.Context, source *os.File, name string, parent *os.File, target string, merge bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	from, to := int(source.Fd()), int(parent.Fd())
	err := unix.Renameat2(from, name, to, target, unix.RENAME_NOREPLACE)
	if !merge || !errors.Is(err, os.ErrExist) {
		return err
	}

	var incoming, existing unix.Stat_t
	if err := unix.Fstatat(from, name, &incoming, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if err := unix.Fstatat(to, target, &existing, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	kind := incoming.Mode & unix.S_IFMT
	if existing.Mode&unix.S_IFMT != kind || (kind != unix.S_IFREG && kind != unix.S_IFDIR) {
		return fmt.Errorf("destination must match a regular file or directory: %w", os.ErrExist)
	}
	if kind == unix.S_IFREG {
		return updateExportFile(from, name, to, target, os.FileMode(incoming.Mode&0777))
	}
	const flags = unix.O_RDONLY | unix.O_DIRECTORY
	children, err := openExportAt(from, name, flags)
	if err != nil {
		return err
	}
	defer func() { _ = children.Close() }()
	directory, err := openExportAt(to, target, flags)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	names, err := children.Readdirnames(-1)
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, child := range names {
		if err := placeFileExport(ctx, children, child, directory, child, true); err != nil {
			return fmt.Errorf("entry %q: %w", child, err)
		}
	}
	return nil
}

func openExportAt(parent int, name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(parent, name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// Update the checked file itself. A replacement rename could remove a
// different type if another process changes the directory entry after inspection.
func updateExportFile(source int, name string, parent int, target string, mode os.FileMode) (retErr error) {
	in, err := openExportAt(source, name, unix.O_RDONLY)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := openExportAt(parent, target, unix.O_WRONLY)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, out.Close()) }()
	fd := int(out.Fd())
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("destination must be a regular file: %w", os.ErrExist)
	}
	if info.Nlink != 1 {
		return fmt.Errorf("destination has multiple hard links: %w", os.ErrExist)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("lock export destination: %w", err)
	}
	size, err := io.Copy(out, in)
	if err != nil {
		return err
	}
	return errors.Join(out.Truncate(size), out.Chmod(mode))
}
