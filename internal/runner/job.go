package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/strio-dev/stoke/internal/workflow"
)

// CIRequest selects the concrete tasks in one native job. The CI provider
// owns success gates outside this selection.
type CIRequest struct {
	Targets []string
	Execute []string
}

// SelectCI resolves explicitly selected tasks and concrete matrix variants
// without selecting their predecessors for execution.
func SelectCI(w *workflow.Workflow, selectors []string) (CIRequest, error) {
	if len(selectors) == 0 {
		return CIRequest{}, errors.New("--ci requires at least one explicit task")
	}
	var request CIRequest
	for _, name := range selectors {
		logical := name
		if _, ok := w.Tasks[name]; !ok {
			if split := strings.LastIndex(name, "--matrix-"); split > 0 {
				logical = name[:split]
			}
		}
		if !slices.Contains(request.Targets, logical) {
			request.Targets = append(request.Targets, logical)
		}
	}
	expanded, err := w.Expand(request.Targets)
	if err != nil {
		return CIRequest{}, err
	}
	jobs, groups := indexCIJobs(expanded)
	for _, selector := range selectors {
		if _, authored := w.Tasks[selector]; authored {
			for _, job := range groups[selector] {
				request.Execute = append(request.Execute, job.Name)
			}
		} else if job, exists := jobs[selector]; exists {
			request.Execute = append(request.Execute, job.Name)
		} else {
			return CIRequest{}, fmt.Errorf("unknown CI task or variant %q", selector)
		}
	}
	return request, nil
}

// RunCI restores external image dependencies before running the selected jobs.
func RunCI(ctx context.Context, w *workflow.Workflow, request CIRequest, options Options) (*Report, error) {
	if options.Events != nil && options.Events.ci == nil {
		return nil, errors.New("CDEvents CI observation requires the selected unit and attempt")
	}
	plan, options, err := prepareCI(ctx, w, request, options)
	if err != nil {
		return nil, err
	}
	report, runErr := runOwned(ctx, w, plan, options)
	return report, cdeventsResult(report, runErr, options.Events)
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
	options.CacheFrom, options.CacheTo = w.CacheRepositories(options.CacheFrom, options.CacheTo)
	expanded, err := w.Expand(request.Targets)
	if err != nil {
		return nil, options, err
	}
	projection, restore, err := projectCI(expanded, request.Execute, options.NoCache)
	if err != nil {
		return nil, options, err
	}
	options.ciCacheOnly = restore
	_, groups := indexCIJobs(expanded)
	selected := make(map[string]bool, len(request.Execute))
	for _, name := range request.Execute {
		selected[name] = true
	}
	options.ciRequiredOutputs = ciOutputDemand(w)
	projection, options, err = preparePlan(ctx, projection, options)
	if err != nil {
		return nil, options, err
	}
	// Lower environments with their original gates before removing external
	// gates. The authored gates distinguish otherwise identical environments.
	for gi := range projection.Groups {
		for ji := range projection.Groups[gi].Jobs {
			job := &projection.Groups[gi].Jobs[ji]
			var needs []string
			for _, dependency := range job.Definition.Needs {
				count := 0
				for _, prerequisite := range groups[dependency] {
					if selected[prerequisite.Name] {
						count++
					}
				}
				if count == 0 {
					continue // A native success gate outside this job.
				}
				if count != len(groups[dependency]) {
					return nil, options, fmt.Errorf("CI selection includes only part of internal prerequisite group %q", dependency)
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
		}
	}
	if err := admitFileExports(projection, options.ContextDir, nil, restore); err != nil {
		return nil, options, err
	}
	return projection, options, nil
}

// projectCI follows the ordinary data edges, leaving external success gates
// to the CI provider. Only explicitly selected jobs may execute on a miss.
func projectCI(expanded *workflow.ExecutionPlan, execute []string, noCache bool) (*workflow.ExecutionPlan, map[string]bool, error) {
	jobs, _ := indexCIJobs(expanded)
	included := make(map[string]bool, len(execute))
	restore := map[string]bool{}
	queue := slices.Clone(execute)
	for _, name := range execute {
		if _, exists := jobs[name]; !exists || included[name] {
			return nil, nil, fmt.Errorf("unknown or duplicate CI selection %q", name)
		}
		included[name] = true
	}
	for i := 0; i < len(queue); i++ {
		job := jobs[queue[i]]
		if err := portableJobPaths(job.Definition); err != nil {
			return nil, nil, fmt.Errorf("CI task %q: %w", job.Name, err)
		}
		data := job.Definition
		data.Needs = nil
		for _, name := range data.Dependencies() {
			if included[name] {
				continue
			}
			producer, exists := jobs[name]
			if !exists {
				return nil, nil, fmt.Errorf("CI task %q has unknown data dependency %q", job.Name, name)
			}
			task := producer.Definition
			if noCache || !task.CacheEnabled() || task.OutputKind() != "image" || task.Environment != nil || len(task.AllMounts()) != 0 || task.HasScripts() {
				return nil, nil, fmt.Errorf("CI task %q requires %q in the same job; use --co-locate %s because the producer cannot use cache-only image restoration", job.Name, name, job.Task)
			}
			included[name], restore[name] = true, true
			queue = append(queue, name)
		}
	}
	projection := &workflow.ExecutionPlan{}
	for _, original := range expanded.Groups {
		group := original
		group.Jobs = nil
		for _, job := range original.Jobs {
			if included[job.Name] {
				group.Jobs = append(group.Jobs, job)
			}
		}
		if len(group.Jobs) != 0 {
			projection.Groups = append(projection.Groups, group)
		}
	}
	return projection, restore, nil
}

// Every stage derives logical output demand from the authored graph without
// expanding unrelated tasks. Data producers and terminal tasks keep outputs;
// a fresh stage used only as a success gate needs no image.
func ciOutputDemand(w *workflow.Workflow) map[string]bool {
	required, consumed := map[string]bool{}, map[string]bool{}
	for _, task := range w.Tasks {
		for _, name := range task.Dependencies() {
			consumed[name] = true
		}
		task.Needs = nil
		for _, name := range task.Dependencies() {
			required[name] = true
		}
	}
	for name := range w.Tasks {
		if !consumed[name] {
			required[name] = true
		}
	}
	return required
}

func omitCIImageOutput(job workflow.Job, options Options) bool {
	task := job.Definition
	return options.ciRequiredOutputs != nil && !options.ciRequiredOutputs[job.Task] &&
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
