package runner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/strio-dev/stoke/internal/workflow"
)

// CIRequest selects visible CI jobs and their exact externally produced inputs.
// External pure success gates remain the CI provider's responsibility.
// Environments maps portable synthetic producer names to representative consumers
// so the existing environment lowering remains the only producer definition.
type CIRequest struct {
	Targets         []string
	Execute         []string
	RequiredOutputs []string // Producer values required by the validated CI plan.
	Environments    map[string]string
	Imports         map[string]CIImport
}

// CIImport identifies an exact output whose metadata was verified by the caller.
// Images may use a pinned registry reference without a local OCI payload.
// Images use an OCI layout directory; components use an OCI archive file.
// CanonicalReference selects the original immutable payload for publication.
type CIImport struct {
	Kind, OCI, Digest, Platform, CanonicalReference string
	ArchiveDigest                                   string // Optional verified transport descriptor for a component archive.
}

// RunCI executes selected jobs using exact imported data. Missing or invalid
// inputs fail; cache misses never authorize replay of an external producer.
func RunCI(ctx context.Context, w *workflow.Workflow, request CIRequest, options Options) (*Report, error) {
	plan, options, err := prepareCI(ctx, w, request, options)
	if err != nil {
		return nil, err
	}
	return runOwned(ctx, w, plan, options)
}

func runCI(ctx context.Context, w *workflow.Workflow, request CIRequest, options Options, engine backend) (*Report, error) {
	plan, options, err := prepareCI(ctx, w, request, options)
	if err != nil {
		return nil, err
	}
	return runPrepared(ctx, w, plan, options, engine)
}

func prepareCI(ctx context.Context, w *workflow.Workflow, request CIRequest, options Options) (*workflow.ExecutionPlan, Options, error) {
	if err := ctx.Err(); err != nil {
		return nil, options, err
	}
	if len(request.Execute) == 0 {
		return nil, options, errors.New("CI projection requires selected concrete jobs")
	}
	expanded, err := w.Expand(request.Targets)
	if err != nil {
		return nil, options, err
	}
	original := map[string]workflow.Job{}
	for _, group := range expanded.Groups {
		for _, job := range group.Jobs {
			original[job.Name] = job
		}
	}
	// Validate public selections before resolving any unrelated execution input.
	for _, name := range request.Execute {
		job, exists := original[name]
		if !exists {
			consumer, synthetic := request.Environments[name]
			job, exists = original[consumer]
			if !synthetic || !exists || job.Definition.Environment == nil {
				return nil, options, fmt.Errorf("CI job %q is not a selected expanded variant or environment producer", name)
			}
		}
		if err := portableJobPaths(job.Definition); err != nil {
			return nil, options, fmt.Errorf("CI job %q: %w", name, err)
		}
	}
	// Preserve authored gate identities through ordinary path resolution and
	// environment lowering. Prune external scheduling edges only afterwards.
	options.hostSelections = map[string]bool{}
	for _, name := range request.Execute {
		options.hostSelections[name] = true
	}
	prepared, options, err := preparePlan(ctx, expanded, options)
	if err != nil {
		return nil, options, err
	}
	jobs, groups := indexCIJobs(prepared)
	hiddenConsumers := map[string][]string{}
	for name, job := range original {
		if job.Definition.Environment != nil {
			hidden := jobs[name].Definition.From
			hiddenConsumers[hidden] = append(hiddenConsumers[hidden], name)
		}
	}
	renames := map[string]string{}
	for stable, representative := range request.Environments {
		if !safeCIName(stable) {
			return nil, options, fmt.Errorf("invalid CI environment producer %q", stable)
		}
		if _, exists := jobs[stable]; exists {
			return nil, options, fmt.Errorf("CI environment producer %q collides with an existing job", stable)
		}
		consumer, exists := original[representative]
		resolved, resolvedExists := jobs[representative]
		if !exists || !resolvedExists || consumer.Definition.Environment == nil {
			return nil, options, fmt.Errorf("CI environment representative %q has no inline environment", representative)
		}
		actual := resolved.Definition.From
		if _, exists := original[actual]; exists || actual == "" {
			return nil, options, errors.New("CI environment representative did not lower to a private producer")
		}
		if _, duplicate := renames[actual]; duplicate {
			return nil, options, errors.New("the same inline environment has multiple CI producer identities")
		}
		renames[actual] = stable
	}
	if len(renames) != 0 {
		for gi := range prepared.Groups {
			group := &prepared.Groups[gi]
			if stable := renames[group.Name]; stable != "" {
				group.Name = stable
			}
			for ji := range group.Jobs {
				job := &group.Jobs[ji]
				if stable := renames[job.Name]; stable != "" {
					job.Name, job.Task = stable, stable
				}
				if stable := renames[job.Definition.From]; stable != "" {
					job.Definition.From = stable
				}
			}
		}
		jobs, groups = indexCIJobs(prepared)
	}
	primary := map[string]bool{}
	for _, name := range request.Execute {
		if _, exists := jobs[name]; !exists {
			return nil, options, fmt.Errorf("CI job %q is absent after environment lowering", name)
		}
		if primary[name] {
			return nil, options, fmt.Errorf("duplicate selected CI job %q", name)
		}
		primary[name] = true
	}
	imports := maps.Clone(request.Imports)
	for name, input := range imports {
		job, exists := jobs[name]
		_, authored := original[name]
		_, promoted := request.Environments[name]
		if !exists || !authored && !promoted || primary[name] || job.Definition.Publish != nil {
			return nil, options, fmt.Errorf("CI import %q is not an external image or component producer", name)
		}
		if err := validateCIImport(ctx, input, job.Definition); err != nil {
			return nil, options, fmt.Errorf("CI import %q: %w", name, err)
		}
	}
	include := map[string]bool{}
	usedImports := map[string]CIImport{}
	var require func(string) error
	require = func(name string) error {
		if include[name] {
			return nil
		}
		if input, imported := imports[name]; imported {
			usedImports[name] = input
			return nil
		}
		job, exists := jobs[name]
		if !exists {
			return fmt.Errorf("CI data prerequisite %q is absent from the execution plan", name)
		}
		_, authored := original[name]
		_, promoted := request.Environments[name]
		if !primary[name] && (authored || promoted) {
			return fmt.Errorf("CI unit is not data-closed: job requires exact data prerequisite %q; rerun its producer and dependent gates", name)
		}
		for _, consumer := range hiddenConsumers[name] {
			if !primary[consumer] {
				return fmt.Errorf("shared inline environment requires an exact promoted result or all its consumers in one explicit invocation; missing consumer %q", consumer)
			}
		}
		// Only an unpromoted hidden environment is implicitly local to its
		// selected consumer. Authored producers are never silently executed.
		include[name] = true
		data := job.Definition
		data.Needs = nil
		for _, dependency := range data.Dependencies() {
			if err := require(dependency); err != nil {
				return err
			}
		}
		return nil
	}
	for _, name := range request.Execute {
		if err := require(name); err != nil {
			return nil, options, err
		}
	}
	if len(usedImports) != len(imports) {
		return nil, options, errors.New("CI imports contain an unused data producer")
	}
	projection := &workflow.ExecutionPlan{}
	for _, originalGroup := range prepared.Groups {
		group := originalGroup
		group.Jobs = nil
		for _, originalJob := range originalGroup.Jobs {
			if !include[originalJob.Name] {
				continue
			}
			job := originalJob
			var needs []string
			for _, dependency := range originalJob.Definition.Needs {
				count := 0
				for _, prerequisite := range groups[dependency] {
					if include[prerequisite.Name] {
						count++
					}
				}
				if count == 0 {
					continue // Native CI owns this external pure success gate.
				}
				if count != len(groups[dependency]) {
					return nil, options, fmt.Errorf("CI projection includes only part of internal prerequisite group %q", dependency)
				}
				needs = append(needs, dependency)
			}
			job.Definition.Needs = needs
			for name, directory := range job.Definition.Contexts {
				relative, err := filepath.Rel(options.ContextDir, directory)
				if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
					return nil, options, fmt.Errorf("CI named context %q resolves outside the checkout", name)
				}
			}
			group.Jobs = append(group.Jobs, job)
		}
		if len(group.Jobs) != 0 {
			projection.Groups = append(projection.Groups, group)
		}
	}
	if err := admitFileExports(projection, options.ContextDir); err != nil {
		return nil, options, err
	}
	// Without explicit terminal targets, retain the ordinary all-target output
	// contract. Otherwise preserve planned values and every local data edge.
	if len(request.Targets) != 0 {
		options.ciRequiredOutputs = map[string]bool{}
		for _, name := range request.RequiredOutputs {
			if _, exists := jobs[name]; !exists {
				return nil, options, fmt.Errorf("required CI output %q is absent from the selected plan", name)
			}
			options.ciRequiredOutputs[name] = true
		}
		for _, group := range projection.Groups {
			for _, job := range group.Jobs {
				if slices.Contains(request.Targets, job.Name) || slices.Contains(request.Targets, job.Task) {
					options.ciRequiredOutputs[job.Name] = true
				}
				for name := range artifactResults(job.Definition, nil) {
					options.ciRequiredOutputs[name] = true
				}
			}
		}
	}
	options.ciImports = usedImports
	return projection, options, nil
}

func omitCIImageOutput(job workflow.Job, options Options) bool {
	task := job.Definition
	return options.ciRequiredOutputs != nil && !options.ciRequiredOutputs[job.Name] &&
		task.Cache != nil && !*task.Cache && !task.FilesystemEnabled() &&
		task.Coopr == nil && task.Host == nil && task.Publish == nil
}

func indexCIJobs(plan *workflow.ExecutionPlan) (map[string]workflow.Job, map[string][]workflow.Job) {
	jobs, groups := map[string]workflow.Job{}, map[string][]workflow.Job{}
	for _, group := range plan.Groups {
		groups[group.Name] = group.Jobs
		for _, job := range group.Jobs {
			jobs[job.Name] = job
			groups[job.Name] = group.Jobs
		}
	}
	return jobs, groups
}

var ciNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,255}$`)

func safeCIName(name string) bool {
	return ciNamePattern.MatchString(name)
}

func portableJobPaths(task workflow.Task) error {
	for _, mount := range task.AllMounts() {
		if mount.Type == "host" {
			return errors.New("live host mounts are not portable between CI jobs; use declared context or image outputs")
		}
	}
	for name, directory := range task.Contexts {
		if filepath.IsAbs(directory) || directory == ".." || strings.HasPrefix(filepath.Clean(directory), ".."+string(filepath.Separator)) {
			return fmt.Errorf("named context %q must remain inside the checkout", name)
		}
	}
	return nil
}
