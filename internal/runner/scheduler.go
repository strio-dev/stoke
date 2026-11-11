package runner

import (
	"context"
	"errors"
	"fmt"
	"maps"

	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
)

const DefaultJobs = 4
const MaxJobs = 256

// NormalizeJobs applies the API default and validates the workflow-wide limit.
func NormalizeJobs(jobs int) (int, error) {
	if jobs == 0 {
		return DefaultJobs, nil
	}
	if jobs < 1 || jobs > MaxJobs {
		return 0, fmt.Errorf("jobs must be between 1 and %d", MaxJobs)
	}
	return jobs, nil
}

type scheduledGroup struct {
	group                           workflow.Group
	dependencies                    []int
	offset, next, active, completed int
	failed, successful              bool
}

type jobCompletion struct {
	group, job int
	result     coopr.Result
	err        error
}

// schedule owns all mutable scheduling and result state. Workers receive a
// snapshot, and no dependency becomes ready before its entire group succeeds.
func schedule(ctx context.Context, w *workflow.Workflow, plan *workflow.ExecutionPlan, options Options, dir, output string, engine backend) ([]TaskResult, error) {
	return scheduleImported(ctx, w, plan, options, dir, output, engine, nil)
}

// Imported entries are verified data identities, never successful task records.
func scheduleImported(ctx context.Context, w *workflow.Workflow, plan *workflow.ExecutionPlan, options Options, dir, output string, engine backend, imported map[string]coopr.Result) ([]TaskResult, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	groups := make([]scheduledGroup, len(plan.Groups))
	indices := map[string]int{}
	items := []TaskResult{}
	for index, group := range plan.Groups {
		indices[group.Name] = index
		groups[index] = scheduledGroup{group: group, offset: len(items)}
		for _, job := range group.Jobs {
			indices[job.Name] = index
			item := jobResult(job, coopr.Result{}, nil)
			item.Status, item.Error = "skipped", "workflow stopped before job started"
			items = append(items, item)
		}
	}
	for index := range groups {
		seen := map[int]bool{}
		for _, job := range groups[index].group.Jobs {
			for _, name := range job.Definition.Dependencies() {
				dependency, ok := indices[name]
				if !ok {
					if _, exactData := imported[name]; exactData {
						continue
					}
					return items, fmt.Errorf("task %q: dependency %q is absent from the execution plan", groups[index].group.Name, name)
				}
				if !seen[dependency] {
					groups[index].dependencies = append(groups[index].dependencies, dependency)
					seen[dependency] = true
				}
			}
		}
	}
	completed := make(chan jobCompletion, len(items))
	results := maps.Clone(imported)
	if results == nil {
		results = map[string]coopr.Result{}
	}
	jobErrors := make([]error, len(items))
	active, finished := 0, 0
	cursor := 0
	stopped := false
	abort := func() {
		stopped = true
		cancel()
	}
	accept := func(done jobCompletion) {
		state := &groups[done.group]
		job := state.group.Jobs[done.job]
		state.active--
		state.completed++
		active--
		finished++
		items[state.offset+done.job] = jobResult(job, done.result, done.err)
		if done.err != nil {
			state.failed = true
			jobErrors[state.offset+done.job] = fmt.Errorf("task %s: %w", job.Name, done.err)
			if state.group.FailFast {
				abort()
			}
		} else {
			results[job.Name] = done.result
		}
		if state.completed == len(state.group.Jobs) {
			state.successful = !state.failed
			if state.failed {
				abort()
			}
		}
	}
	ready := func(state *scheduledGroup) bool {
		if state.next == len(state.group.Jobs) || state.active >= state.group.MaxParallel {
			return false
		}
		for _, dependency := range state.dependencies {
			if !groups[dependency].successful {
				return false
			}
		}
		return true
	}
	for {
		if runCtx.Err() != nil {
			abort()
		}
		// Observe completed failures before admitting more work whenever their
		// completion is already available. Actual completion order is unrestricted.
	drain:
		for {
			select {
			case done := <-completed:
				accept(done)
			default:
				break drain
			}
		}
		for !stopped && active < options.Jobs {
			if runCtx.Err() != nil {
				abort()
				break
			}
			select {
			case done := <-completed:
				accept(done)
				continue
			default:
			}
			// Observe completions before a fresh scan: accepting one midway
			// through a scan could otherwise strand a newly ready earlier group.
			index := -1
			for n := range len(groups) {
				candidate := (cursor + n) % len(groups)
				if ready(&groups[candidate]) {
					index = candidate
					break
				}
			}
			if index < 0 {
				break
			}
			// Rotate across admissions, including after a slot opens, so a
			// ready later branch does not wait behind an entire early matrix.
			cursor = (index + 1) % len(groups)
			state := &groups[index]
			jobIndex := state.next
			job := state.group.Jobs[jobIndex]
			failFast := state.group.FailFast
			previous := artifactResults(job.Definition, results)
			state.next++
			state.active++
			active++
			go func() {
				result, err := executeJob(runCtx, job, options, dir, output, previous, engine)
				if err != nil && failFast {
					cancel()
				}
				completed <- jobCompletion{group: index, job: jobIndex, result: result, err: err}
			}()
		}
		if active == 0 {
			break
		}
		select {
		case done := <-completed:
			accept(done)
		case <-ctx.Done():
			abort()
			// Every started worker must return, including cleanup failures. Keep
			// draining rather than returning when the parent context is canceled.
			accept(<-completed)
		}
	}
	if !stopped && finished != len(items) {
		return items, errors.New("workflow scheduler has no ready task")
	}
	return items, errors.Join(append(jobErrors, ctx.Err())...)
}

// Workers need artifact identities, not the full history of completed jobs.
// Build this private snapshot while the scheduler exclusively owns results.
func artifactResults(task workflow.Task, results map[string]coopr.Result) map[string]coopr.Result {
	var previous map[string]coopr.Result
	add := func(name string) {
		if name == "" {
			return
		}
		if previous == nil {
			previous = map[string]coopr.Result{}
		}
		previous[name] = results[name]
	}
	add(task.From)
	if task.Host != nil {
		for _, input := range task.Host.Inputs {
			add(input.From)
		}
	}
	for _, mount := range task.AllMounts() {
		add(mount.From)
	}
	for _, step := range task.Steps {
		if step.Copy != nil {
			add(step.Copy.From)
		}
	}
	if task.Coopr != nil {
		for _, argument := range task.Coopr.Args {
			add(argument.From)
		}
	}
	if task.Publish != nil {
		add(task.Publish.From)
	}
	return previous
}

func jobResult(job workflow.Job, result coopr.Result, err error) TaskResult {
	kind := job.Definition.OutputKind()
	if job.Definition.Publish != nil {
		kind = "publish"
	}
	item := TaskResult{Name: job.Name, Task: job.Task, Matrix: job.Matrix, Status: "passed", Kind: kind, ImageID: result.ImageID, Platform: result.Platform, Digest: result.ManifestDigest, OCI: result.OCI, Filesystem: result.Filesystem, Reference: result.Reference, Metadata: result.Metadata, Destinations: result.Destinations, ProgressDropped: result.ProgressDropped}
	if result.CacheAvailable {
		item.Cache = &result.Cache
	}
	if err != nil {
		item.Status = "failed"
		if !errors.Is(err, coopr.ErrCleanupFailed) && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			item.Status = "canceled"
		}
		item.Error = err.Error()
	}
	return item
}
