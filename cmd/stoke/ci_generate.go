package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/strio-dev/stoke/internal/artifact"
	"github.com/strio-dev/stoke/internal/ci"
)

func selectStageTemplate(args []string, local bool, configured string) (template, stage string, err error) {
	if local {
		switch len(args) {
		case 0:
			return "", "", nil
		case 1:
			return "", args[0], nil
		default:
			return "", "", errors.New("--template-file accepts only an optional stage or group name")
		}
	}
	switch len(args) {
	case 2:
		return args[0], args[1], nil
	case 1:
		return args[0], "", nil
	case 0:
		if configured != "" {
			return configured, "", nil
		}
		return "", "", errors.New("set ci-template in the root workflow or supply a built-in name, oci:// reference, or --template-file")
	default:
		return "", "", errors.New("generate accepts a template selector and an optional stage or group")
	}
}

func generateStages(ctx context.Context, out io.Writer, plan *ci.Plan, selector, stage, templateFile string, client artifact.Client, check, full bool) error {
	var data []byte
	var err error
	switch {
	case templateFile != "":
		file, openErr := os.Open(templateFile)
		if openErr != nil {
			return openErr
		}
		data, err = io.ReadAll(io.LimitReader(file, ci.MaxTemplateBytes+1))
		err = errors.Join(err, file.Close())
	case strings.HasPrefix(selector, "oci://"):
		data, err = client.PullCITemplate(ctx, strings.TrimPrefix(selector, "oci://"), ci.MaxTemplateBytes)
	default:
		data, err = ci.BuiltinTemplate(selector)
	}
	if err != nil {
		return err
	}
	files, err := ci.RenderStages(plan, stage, data)
	if err != nil {
		return err
	}
	scope, err := stageGenerationScope(plan.Config.Workflow, selector, templateFile)
	if err != nil {
		return err
	}
	// The caller must distinguish an unfiltered plan from a lowered group.
	if err := writeStages(files, scope, check, full); err != nil {
		return err
	}
	for _, file := range files {
		if _, err := fmt.Fprintln(out, file.Path); err != nil {
			return err
		}
	}
	return nil
}

func writeStages(files []ci.StageFile, scope string, check, full bool) error {
	if err := checkStageTargets(files); err != nil {
		return err
	}
	if full {
		if err := checkObsoleteStages(files, scope); err != nil {
			return err
		}
	}
	var updates []stageUpdate
	// Check ownership of every output before creating any generated files.
	for _, stage := range files {
		update, err := prepareStageUpdate(stage, scope, check)
		if err != nil {
			return err
		}
		if update.next != (stageStamp{}) {
			updates = append(updates, update)
		}
	}
	for _, update := range updates {
		if err := update.apply(writeStageFile); err != nil {
			return err
		}
	}
	return nil
}

// Check the paths the whole batch will create. A checksum suffix can turn
// otherwise independent rendered paths into a file and one of its descendants.
func checkStageTargets(files []ci.StageFile) error {
	targets := make([]string, 0, 2*len(files))
	seen := make(map[string]bool, cap(targets))
	for _, stage := range files {
		for _, target := range []string{stage.Path, stageChecksumPath(stage.Path)} {
			name := filepath.Clean(target)
			if seen[name] {
				return fmt.Errorf("duplicate generated target %q", name)
			}
			seen[name] = true
			targets = append(targets, name)
		}
	}
	for _, name := range targets {
		for directory := name; ; {
			parent := filepath.Dir(directory)
			if parent == "." || parent == directory {
				break
			}
			if seen[parent] {
				return fmt.Errorf("generated targets conflict: %q is a parent of %q", parent, name)
			}
			directory = parent
		}
	}
	return nil
}

func writeStageFile(name string, data []byte) error {
	directory := filepath.Dir(name)
	if err := os.MkdirAll(directory, 0755); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".stoke-ci-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Chmod(0644), file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), name)
}
