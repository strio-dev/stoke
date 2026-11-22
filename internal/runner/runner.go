// Package runner schedules workflow tasks; Coopr owns each task's build graph.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	cooprwire "github.com/strio-dev/coopr/api"
	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/source"
	"github.com/strio-dev/stoke/internal/workflow"
)

type Options struct {
	ContextDir, OutputDir, StateDir, CacheDir         string
	Runtime, StorageDriver, AuthFile, SignaturePolicy string
	CooprBinary                                       string
	Network                                           string
	CacheFrom, CacheTo                                []string
	Jobs                                              int
	NoCache                                           bool
	TLSVerify                                         *bool
	Stdout, Stderr                                    io.Writer
	Events                                            *CDEvents
	EventName                                         string
	EventPath                                         string
	runMounts                                         bool
	priorFileExports                                  map[string]bool
	ciRequiredOutputs                                 map[string]bool
	ciCacheOnly                                       map[string]bool
	AllowHost                                         bool
	HostCapabilities                                  []string
	HostCachePlainHTTP                                bool
	hostContextDir                                    string
}

type TaskResult struct {
	Name            string                     `json:"name"`
	Task            string                     `json:"task,omitempty"`
	Matrix          map[string]string          `json:"matrix,omitempty"`
	Status          string                     `json:"status"`
	Kind            string                     `json:"kind"`
	Error           string                     `json:"error,omitempty"`
	ImageID         string                     `json:"imageID,omitempty"`
	Platform        string                     `json:"platform,omitempty"`
	Digest          string                     `json:"digest,omitempty"`
	OCI             string                     `json:"oci,omitempty"`
	Filesystem      string                     `json:"filesystem,omitempty"`
	Reference       string                     `json:"reference,omitempty"`
	Cache           *coopr.CacheStats          `json:"cache,omitempty"`
	Metadata        map[string]json.RawMessage `json:"metadata,omitempty"`
	ProgressDropped uint64                     `json:"progressDropped,omitempty"`
	Destinations    []cooprwire.Destination    `json:"destinations,omitempty"`
	Exports         []cooprwire.ExportResult   `json:"exports,omitempty"`
}

type Report struct {
	Status    string       `json:"status"`
	Error     string       `json:"error,omitempty"`
	Directory string       `json:"directory"`
	Output    string       `json:"output"`
	Tasks     []TaskResult `json:"tasks"`
}

type executeFunc func(context.Context, coopr.Program, coopr.Options) (coopr.Result, error)

type backend interface {
	Execute(context.Context, coopr.Program, coopr.Options) (coopr.Result, error)
	Build(context.Context, cooprwire.Build, bool, coopr.Options) (coopr.Result, error)
	Publish(context.Context, cooprwire.Publish, coopr.Options) (coopr.Result, error)
}

func Run(ctx context.Context, w *workflow.Workflow, targets []string, options Options) (*Report, error) {
	plan, options, err := prepare(ctx, w, targets, options)
	if err != nil {
		return nil, err
	}
	name := options.EventName
	if name == "" && options.Events != nil {
		name = options.Events.RunID
	}
	options.Events.PipelineStarted(name)
	if err := options.Events.Err(); err != nil {
		return nil, err
	}
	report, runErr := runOwned(ctx, w, plan, options)
	// The invocation owner finishes the pipeline after releasing its inputs.
	return report, cdeventsResult(report, runErr, options.Events)
}

// The ordinary and CI-projected paths share service ownership, capability
// validation, cancellation/draining and report persistence.
func runOwned(ctx context.Context, w *workflow.Workflow, plan *workflow.ExecutionPlan, options Options) (*Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !needsEngine(plan) {
		return runPrepared(ctx, w, plan, options, nil)
	}
	// Readiness observes cancellation. Once ready, the owner keeps the service
	// alive until canceled requests acknowledge cleanup and every job drains.
	service, err := coopr.StartOwned(ctx, options.CooprBinary, options.Stderr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = service.Close() }()
	if err := validateCapabilities(ctx, plan, service.Client, options); err != nil {
		return nil, errors.Join(err, service.Close())
	}
	options.runMounts = slices.Contains(service.Info.Capabilities, "run-mounts")
	report, runErr := runPrepared(ctx, w, plan, options, service.Client)
	closeErr := service.Close()
	err = errors.Join(runErr, closeErr)
	if report != nil && closeErr != nil {
		err = FinalizeReport(report, err)
	}
	return report, err
}

// RequiredCapabilities describes the selected execution using the same policy as
// runtime preflight.
func RequiredCapabilities(w *workflow.Workflow, plan *workflow.ExecutionPlan, request *CIRequest, options Options) ([]string, error) {
	if request != nil {
		if w == nil {
			return nil, errors.New("CI capability planning requires the resolved workflow")
		}
		options.ciRequiredOutputs = ciOutputDemand(w)
		projection, restore, err := projectCI(plan, request.Execute, options.NoCache)
		if err != nil {
			return nil, err
		}
		plan, options.ciCacheOnly = projection, restore
	}
	var required []string
	if len(options.ciCacheOnly) != 0 {
		required = append(required, cooprwire.CapabilityCacheRequireHit)
	}
	hostSources := map[string]bool{}
	for _, group := range plan.Groups {
		for _, job := range group.Jobs {
			if job.Definition.Host != nil {
				hostSources[job.Name] = true
			}

		}
	}
	for _, group := range plan.Groups {
		for _, job := range group.Jobs {
			task := job.Definition
			if task.Host != nil {
				continue
			}
			if task.Publish != nil {
				required = append(required, cooprwire.CapabilityPublish)
				continue
			}
			capability := cooprwire.CapabilityExecute
			if task.Coopr != nil {
				capability = cooprwire.CapabilityBuild
				if task.OutputKind() == "component" {
					capability = cooprwire.CapabilityComponent
				}
			}
			required = append(required, capability)
			if task.Environment != nil {
				required = append(required, cooprwire.CapabilityBuild)
			}
			omitOCI := omitCIImageOutput(job, options)
			if omitOCI {
				required = append(required, cooprwire.CapabilityOptionalOCIOutput)
			}
			if options.Network != "" {
				required = append(required, cooprwire.CapabilityRunNetwork)
			}
			cacheFrom, cacheTo := jobRegistryCaches(job, options)
			// An unlowered environment adds an outputful native build, even
			// when its consumer omits OCI. Runtime sees that producer directly.
			if len(cacheFrom) != 0 || len(cacheTo) != 0 || (task.Environment != nil && len(options.CacheTo) != 0) {
				required = append(required, cooprwire.CapabilityRegistryCache)
			}
			if len(task.AllMounts()) != 0 {
				required = append(required, cooprwire.CapabilityRunMounts)
			}
			if !options.ciCacheOnly[job.Name] && len(task.FileExports()) != 0 {
				required = append(required, cooprwire.CapabilitySelectedFileExports)
			}
			named := len(task.Contexts) != 0 || task.ComponentDir != ""
			if task.HasScripts() {
				required = append(required, cooprwire.CapabilityRunMounts)
				named = true
			}
			for name := range artifactResults(task, nil) {
				named = named || hostSources[name]
			}
			if named {
				required = append(required, cooprwire.CapabilityNamedContexts)
			}
			platform, err := task.ResolvedPlatform()
			if err != nil {
				return nil, fmt.Errorf("task %q platform: %w", job.Task, err)
			}
			if platform != "" && (task.Coopr != nil || task.Environment != nil) {
				required = append(required, cooprwire.CapabilityBuildPlatform)
			}
		}
	}
	slices.Sort(required)
	return slices.Compact(required), nil
}

func jobRegistryCaches(job workflow.Job, options Options) (from, to []string) {
	if job.Definition.Publish != nil {
		return nil, nil
	}
	if options.ciCacheOnly[job.Name] || omitCIImageOutput(job, options) {
		return options.CacheFrom, nil
	}
	return options.CacheFrom, options.CacheTo
}

func validateCapabilities(ctx context.Context, plan *workflow.ExecutionPlan, client *coopr.Client, options Options) error {
	// Negotiate the entire selected plan before any task can have side effects.
	required, err := RequiredCapabilities(nil, plan, nil, options)
	if err != nil {
		return err
	}
	_, err = client.Negotiate(ctx, required...)
	return err
}

func run(ctx context.Context, w *workflow.Workflow, targets []string, options Options, engine backend) (*Report, error) {
	plan, options, err := prepare(ctx, w, targets, options)
	if err != nil {
		return nil, err
	}
	return runPrepared(ctx, w, plan, options, engine)
}

// Prepare the selected plan once, including every path check, before starting
// an engine or creating run state. Its resolved definitions belong to this run.
func prepare(ctx context.Context, w *workflow.Workflow, targets []string, options Options) (*workflow.ExecutionPlan, Options, error) {
	if err := ctx.Err(); err != nil {
		return nil, options, err
	}
	options.CacheFrom, options.CacheTo = w.CacheRepositories(options.CacheFrom, options.CacheTo)
	plan, err := w.Expand(targets)
	if err != nil {
		return nil, options, err
	}
	plan, options, err = preparePlan(ctx, plan, options)
	if err != nil {
		return nil, options, err
	}
	options.priorFileExports = map[string]bool{}
	if err := admitFileExports(plan, options.ContextDir, options.priorFileExports, nil); err != nil {
		return nil, options, err
	}
	return plan, options, nil
}

// Prepare already-expanded execution copies without adding another dependency
// planner. Callers admit exports after selecting the jobs that will execute.
func preparePlan(ctx context.Context, plan *workflow.ExecutionPlan, options Options) (*workflow.ExecutionPlan, Options, error) {
	if err := ctx.Err(); err != nil {
		return nil, options, err
	}
	options, err := defaults(options)
	if err != nil {
		return nil, options, err
	}
	// Resolve selected definitions before any task can execute or publish.
	for gi := range plan.Groups {
		for ji := range plan.Groups[gi].Jobs {
			if err := ctx.Err(); err != nil {
				return nil, options, err
			}
			job := &plan.Groups[gi].Jobs[ji]
			if job.Definition.Host != nil {
				if err := validateHostExecution(job.Definition, options); err != nil {
					return nil, options, fmt.Errorf("task %q: %w", job.Task, err)
				}
				continue
			}
			if _, err := job.Definition.ResolvedPlatform(); err != nil {
				return nil, options, fmt.Errorf("task %q platform: %w", job.Task, err)
			}
			if err := resolveTaskMounts(&job.Definition, options.ContextDir); err != nil {
				return nil, options, fmt.Errorf("task %q: %w", job.Task, err)
			}
			if err := validateMountIsolation(job.Definition, options); err != nil {
				return nil, options, fmt.Errorf("task %q: %w", job.Task, err)
			}
			if err := source.ValidateComponentContext(job.Definition.ComponentDir, options.ContextDir); err != nil {
				return nil, options, fmt.Errorf("task %q: %w", job.Task, err)
			}
			for _, step := range job.Definition.Steps {
				if step.Copy != nil && step.Copy.Scope == "component" && job.Definition.ComponentDir == "" {
					return nil, options, fmt.Errorf("task %q: component files must be resolved before execution", job.Task)
				}
			}
			if build := job.Definition.Coopr; build != nil {
				job.Definition.Coopr, err = source.ResolveDefinitionBuild(job.Definition, options.ContextDir)
				if err != nil {
					return nil, options, fmt.Errorf("task %q definition files: %w", job.Task, err)
				}
			}
			if environment := job.Definition.Environment; environment != nil {
				resolved, err := source.ResolveDefinitionBuild(job.Definition, options.ContextDir)
				if err != nil {
					return nil, options, fmt.Errorf("task %q build files: %w", job.Task, err)
				}
				job.Definition.Environment = (*workflow.Build)(resolved)
			}
		}
	}
	plan, err = lowerEnvironments(plan)
	if err != nil {
		return nil, options, err
	}
	if options.EventPath != "" {
		roots := []string{options.ContextDir, options.StateDir, options.CacheDir, options.OutputDir}
		for _, group := range plan.Groups {
			for _, job := range group.Jobs {
				roots = append(roots, job.Definition.ComponentDir)
				if job.Definition.Host != nil {
					for _, path := range job.Definition.Host.Toolchain {
						resolved, err := filepath.EvalSymlinks(path)
						if err != nil {
							return nil, options, err
						}
						roots = append(roots, resolved)
					}
				}
				for _, directory := range job.Definition.Contexts {
					roots = append(roots, directory)
				}
				for _, mount := range job.Definition.AllMounts() {
					if mount.Type == "host" {
						roots = append(roots, mount.Source)
					}
				}
			}
		}
		for _, root := range roots {
			if root != "" && overlap(root, options.EventPath) {
				return nil, options, errors.New("CDEvents file overlaps an execution input or runtime directory")
			}
		}
	}
	// Native stores and generated exports must never become COPY inputs.
	for _, p := range []string{options.StateDir, options.CacheDir, options.OutputDir} {
		if p != "" && overlap(options.ContextDir, p) {
			return nil, options, fmt.Errorf("context and runtime path overlap: %s; choose a path outside the context", p)
		}
	}
	if options.OutputDir != "" {
		if _, err := os.Lstat(options.OutputDir); !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return nil, options, err
			}
			return nil, options, errors.New("output directory already exists")
		}
	}
	// Service diagnostics and concurrent jobs share one writer lock.
	mu := &sync.Mutex{}
	options.Stdout = synchronize(options.Stdout, mu)
	options.Stderr = synchronize(options.Stderr, mu)
	return plan, options, nil
}

func runPrepared(ctx context.Context, w *workflow.Workflow, plan *workflow.ExecutionPlan, options Options, engine backend) (report *Report, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(options.StateDir, 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(options.CacheDir, 0700); err != nil {
		return nil, err
	}
	defer func() {
		if report != nil {
			retErr = FinalizeReport(report, retErr)
		}
	}()
	// Coopr locks portable-cache operations and persistent volumes. Native
	// stores are invocation-private; host tasks retain their workspace leases.
	dir, err := os.MkdirTemp(options.StateDir, "run-")
	if err != nil {
		return nil, err
	}
	output := options.OutputDir
	if output == "" {
		output = filepath.Join(dir, "outputs")
	}
	report = &Report{Status: "running", Directory: dir, Output: output, Tasks: skippedTasks(plan, options)}
	if err := os.Mkdir(output, 0700); err != nil {
		return report, err
	}
	options.hostContextDir, err = snapshotHostContext(ctx, plan, options.ContextDir, dir)
	if options.hostContextDir != "" {
		defer func() {
			if cleanupErr := removePrivateHostTree(options.hostContextDir); cleanupErr != nil {
				retErr = errors.Join(retErr, coopr.ErrCleanupFailed, cleanupErr)
			}
		}()
	}
	if err != nil {
		return report, err
	}
	report.Tasks, err = schedule(ctx, w, plan, options, dir, output, engine)
	return report, err
}

// FinalizeReport records the invocation outcome after execution and caller-owned cleanup.
// Task outcomes remain the observed execution results.
func FinalizeReport(report *Report, runErr error) error {
	report.Status, report.Error = "passed", ""
	if runErr != nil {
		report.Status, report.Error = "failed", runErr.Error()
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err == nil {
		err = writeReport(report.Directory, append(data, '\n'))
	}
	if err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("persist run report: %w", err))
		report.Status, report.Error = "failed", runErr.Error()
	}
	return runErr
}

// Replace a complete report in one rename, including after a service-close
// failure. A failed write must not truncate the previously persisted report.
func writeReport(dir string, data []byte) error {
	f, err := os.CreateTemp(dir, ".run-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "run.json"))
}

type synchronizedWriter struct {
	out io.Writer
	mu  *sync.Mutex
}

func (w synchronizedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}
func synchronize(out io.Writer, mu *sync.Mutex) io.Writer {
	if out == nil {
		return nil
	}
	return synchronizedWriter{out: out, mu: mu}
}

func executeJob(ctx context.Context, job workflow.Job, options Options, dir, output string, previous map[string]coopr.Result, engine backend) (result coopr.Result, retErr error) {
	flushLogs := labelTaskLogs(&options, job.Label())
	defer func() { retErr = errors.Join(retErr, flushLogs()) }()
	var observedBuild *CDEvents
	restore := options.ciCacheOnly[job.Name]
	defer func() { observedBuild.BuildFinished(job, result, retErr) }()
	defer func() {
		if !restore && len(job.Definition.FileExports()) != 0 && len(result.Exports) == 0 {
			result.Exports = skippedJobFileExports(job, options.ContextDir, "task did not produce selected files")
		}
	}()
	if err := ctx.Err(); err != nil {
		return coopr.Result{}, err
	}
	platform, err := job.Definition.ResolvedPlatform()
	if err != nil {
		return coopr.Result{}, fmt.Errorf("platform: %w", err)
	}
	taskDir := filepath.Join(output, job.Name)
	if err := os.Mkdir(taskDir, 0700); err != nil {
		return coopr.Result{}, err
	}
	if job.Definition.Host != nil {
		return executeHost(ctx, job, options, taskDir, previous)
	}
	opts := coopr.Options{
		Store:      coopr.StoreOptions{RunRoot: filepath.Join(dir, "runroot"), GraphRoot: filepath.Join(dir, "graphroot"), Driver: options.StorageDriver},
		ContextDir: options.ContextDir, Runtime: options.Runtime,
		Output:   coopr.Output{OCI: filepath.Join(taskDir, "image"), Filesystem: filepath.Join(taskDir, "rootfs")},
		Cache:    coopr.CacheOptions{LocalDir: filepath.Join(options.CacheDir, "results"), NoCache: options.NoCache || !job.Definition.CacheEnabled()},
		Registry: coopr.RegistryOptions{AuthFile: options.AuthFile, SignaturePolicyPath: options.SignaturePolicy, TLSVerify: options.TLSVerify},
		Stdout:   options.Stdout, Stderr: options.Stderr,
	}
	if job.Definition.Publish == nil {
		opts.Network = options.Network
		if options.runMounts || len(job.Definition.AllMounts()) != 0 {
			opts.Cache.MountDir = filepath.Join(options.CacheDir, "mounts")
		}
		opts.Contexts = make(map[string]string, len(job.Definition.Contexts)+1)
		for name, directory := range job.Definition.Contexts {
			opts.Contexts[name] = directory
		}
		if job.Definition.ComponentDir != "" {
			opts.Contexts[workflow.ComponentContext] = job.Definition.ComponentDir
		}
		for name, value := range previous {
			if value.ImageID == "" && value.Filesystem != "" {
				contextName := hostContextName(name)
				if _, exists := opts.Contexts[contextName]; exists {
					return coopr.Result{}, errors.New("generated host context collides with a declared context")
				}
				opts.Contexts[contextName] = value.Filesystem
			}
		}
	}
	cacheFrom, cacheTo := jobRegistryCaches(job, options)
	opts.Cache.RegistryFrom = slices.Clone(cacheFrom)
	opts.Cache.RegistryTo = slices.Clone(cacheTo)
	if restore || !job.Definition.FilesystemEnabled() {
		opts.Output.Filesystem = ""
	}
	if restore {
		opts.Cache.RequireHit = true
	}
	omitOCI := omitCIImageOutput(job, options)
	if omitOCI {
		opts.Output.OCI = ""
		opts.Cache.LocalDir = ""
	}
	var staging string
	var exports []cooprwire.FileExport
	if !restore {
		staging, exports, err = stageJobFileExports(job, options.ContextDir)
		if err != nil {
			return coopr.Result{}, err
		}
	}
	opts.Output.Exports = exports
	if staging != "" {
		defer func() {
			if errors.Is(retErr, coopr.ErrCleanupFailed) {
				retErr = errors.Join(retErr, fmt.Errorf("file export staging retained after unconfirmed cleanup: %s", staging))
			} else {
				retErr = errors.Join(retErr, os.RemoveAll(staging))
			}
		}()
	}
	if options.Stderr != nil {
		if restore {
			_, _ = fmt.Fprintf(options.Stderr, "==> %s (cache-only)\n", job.Name)
		} else {
			_, _ = fmt.Fprintf(options.Stderr, "==> %s (cache=%t)\n", job.Name, !opts.Cache.NoCache)
		}
	}
	switch {
	case job.Definition.Coopr != nil:
		build := job.Definition.Coopr
		if build.Context != "" {
			opts.ContextDir = build.Context
		}
		args := make(map[string]string, len(build.Args))
		for name, argument := range build.Args {
			if argument.From != "" {
				value, sourceErr := artifactSource(previous, argument.From)
				if sourceErr != nil {
					return coopr.Result{}, sourceErr
				}
				args[name] = value
			} else if argument.Value != nil {
				args[name] = *argument.Value
			} else if argument.Ref != "" {
				args[name] = argument.Ref
			} else {
				return coopr.Result{}, fmt.Errorf("unresolved native argument %q", name)
			}
		}
		component := job.Definition.OutputKind() == "component"
		if component {
			opts.Output.OCI = filepath.Join(taskDir, "component.oci.tar")
		}
		if !restore && opts.Output.OCI != "" {
			observedBuild = options.Events
			observedBuild.BuildStarted(job)
		}
		result, err = engine.Build(ctx, cooprwire.Build{Files: build.Files, Platform: platform, Target: build.Target, Args: args, ComponentStoreDir: filepath.Join(dir, "components")}, component, opts)
	case job.Definition.Publish != nil:
		publication := job.Definition.Publish
		value, sourceErr := artifactSource(previous, publication.From)
		if sourceErr != nil {
			return coopr.Result{}, sourceErr
		}
		kind := "component"
		if imageSource(previous[publication.From]) != "" {
			kind = "image"
			value = previous[publication.From].ImageID
		}

		opts.Output = coopr.Output{}
		result, err = engine.Publish(ctx, cooprwire.Publish{Artifact: kind, Source: value, Destinations: publication.Destinations, ComponentStoreDir: filepath.Join(dir, "components")}, opts)
	default:
		if job.Definition.HasScripts() {
			scriptDir := filepath.Join(taskDir, "scripts")
			defer func() {
				if !errors.Is(retErr, coopr.ErrCleanupFailed) {
					retErr = errors.Join(retErr, os.RemoveAll(scriptDir))
				}
			}()
			if err := stageTaskScripts(job.Definition, scriptDir); err != nil {
				return coopr.Result{}, err
			}
			opts.Contexts[workflow.ScriptContext] = scriptDir
		}
		program, compileErr := compile(job.Name, job.Definition, previous)
		if compileErr != nil {
			return coopr.Result{}, compileErr
		}
		if !restore && opts.Output.OCI != "" {
			observedBuild = options.Events
			observedBuild.BuildStarted(job)
		}
		result, err = engine.Execute(ctx, program, opts)
		if err == nil && program.Platform != "" {
			actual, platformErr := workflow.NormalizePlatform(result.Platform)
			if platformErr != nil {
				err = fmt.Errorf("coopr did not report a valid result platform: %w", platformErr)
				result.Exports = nil
			} else if actual != program.Platform {
				err = fmt.Errorf("coopr returned platform %q for requested platform %q", actual, program.Platform)
				result.Exports = nil
			}
		}
	}
	logErr := flushLogs()
	if options.Stderr != nil && err == nil && result.CacheAvailable {
		_, _ = fmt.Fprintf(options.Stderr, "    %s: cache hits=%d misses=%d stored=%d skipped=%d errors=%d\n", job.Name, result.Cache.Hits, result.Cache.Misses, result.Cache.Stored, result.Cache.Skipped, result.Cache.Errors)
	}
	var exportErr error
	result.Exports, exportErr = publishJobFileExports(ctx, job, options.ContextDir, staging, exports, result.Exports, options.priorFileExports, err)
	if omitOCI {
		result.OCI, result.ManifestDigest = "", ""
	}
	return result, errors.Join(err, exportErr, logErr)
}

func imageSource(result coopr.Result) string {
	if result.ImageID != "" {
		return "containers-storage:" + result.ImageID
	}

	return ""
}

func artifactSource(results map[string]coopr.Result, name string) (string, error) {
	result, ok := results[name]
	if source := imageSource(result); ok && source != "" {
		return source, nil
	}
	if ok && result.Reference != "" {
		return result.Reference, nil
	}
	return "", fmt.Errorf("task %q did not return a usable image or component identity", name)
}

func compile(name string, task workflow.Task, results map[string]coopr.Result) (coopr.Program, error) {
	platform, err := taskPlatform(task, results)
	if err != nil {
		return coopr.Program{}, err
	}
	from := coopr.Source{Image: task.Image}
	if task.From != "" {
		from.Image = imageSource(results[task.From])
	}
	state := coopr.State{ID: name, From: from}
	keys := make([]string, 0, len(task.Env))
	for key := range task.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		state.Operations = append(state.Operations, coopr.Operation{Kind: "env", Name: key, Value: *task.Env[key].Value})
	}
	if task.Workdir != "" {
		state.Operations = append(state.Operations, coopr.Operation{Kind: "workdir", Path: task.Workdir})
	}
	var states []coopr.State
	exportStates := fileExportStates(name, task.Steps)
	exportIndex := 0
	for _, step := range task.Steps {
		if step.Export != nil {
			id := exportStates[exportIndex]
			exportIndex++
			if len(states) == 0 || states[len(states)-1].ID != id {
				state.ID = id
				states = append(states, state)
				state = coopr.State{ID: name, From: coopr.Source{State: id}}
			}
			continue
		}
		if step.Copy != nil {
			copyOp := coopr.Operation{Kind: "copy", Sources: step.Copy.Sources, Destination: step.Copy.Destination}
			if step.Copy.From != "" {
				if results[step.Copy.From].ImageID == "" && results[step.Copy.From].Filesystem != "" {
					copyOp.From.Context = hostContextName(step.Copy.From)
				} else {
					copyOp.From.Image = imageSource(results[step.Copy.From])
				}
			} else if step.Copy.Scope == "component" {
				copyOp.From.Context = workflow.ComponentContext
			}
			state.Operations = append(state.Operations, copyOp)
		} else {
			run := coopr.Operation{Kind: "run", Args: step.Run}
			if step.Script != "" {
				name := scriptName(step.Script)
				target := workflow.ScriptMountRoot + "/" + name
				run.Args = []string{target}
				run.Mounts = append(run.Mounts, coopr.Mount{Type: "bind", Target: target, Source: name, From: coopr.Source{Context: workflow.ScriptContext}, ReadOnly: true})
			}
			for _, mount := range append(slices.Clone(task.Mounts), step.Mounts...) {
				run.Mounts = append(run.Mounts, compileMount(mount, results))
			}
			state.Operations = append(state.Operations, run)
		}
	}
	if len(states) == 0 || len(state.Operations) != 0 {
		states = append(states, state)
	}
	return coopr.Program{Platform: platform, States: states, Output: states[len(states)-1].ID}, nil
}

func defaults(options Options) (Options, error) {
	var err error
	if err := validateRegistryCaches(options); err != nil {
		return options, err
	}
	options.Jobs, err = NormalizeJobs(options.Jobs)
	if err != nil {
		return options, err
	}
	if options.ContextDir == "" {
		options.ContextDir = "."
	}
	options.ContextDir, err = filepath.Abs(options.ContextDir)
	if err != nil {
		return options, err
	}
	options.ContextDir, err = filepath.EvalSymlinks(options.ContextDir)
	if err != nil {
		return options, err
	}
	info, err := os.Stat(options.ContextDir)
	if err != nil {
		return options, err
	}
	if !info.IsDir() {
		return options, errors.New("context must be a directory")
	}
	if options.StateDir == "" {
		root := os.Getenv("XDG_STATE_HOME")
		if root == "" {
			root, err = os.UserHomeDir()
			if err != nil {
				return options, err
			}
			root = filepath.Join(root, ".local", "state")
		}
		options.StateDir = filepath.Join(root, "stoke", "runs")
	}
	if options.CacheDir == "" {
		root, e := os.UserCacheDir()
		if e != nil {
			return options, e
		}
		options.CacheDir = filepath.Join(root, "stoke")
	}
	for _, p := range []*string{&options.StateDir, &options.CacheDir, &options.OutputDir, &options.EventPath} {
		if *p == "" {
			continue
		}
		*p, err = canonical(*p)
		if err != nil {
			return options, err
		}
	}
	for _, p := range []*string{&options.AuthFile, &options.SignaturePolicy} {
		if *p != "" {
			*p, err = filepath.Abs(*p)
			if err != nil {
				return options, err
			}
		}
	}
	if options.Runtime == "" {
		options.Runtime = "crun"
	}
	if options.StorageDriver == "" {
		options.StorageDriver = "vfs"
	}
	return options, nil
}

func validateRegistryCaches(options Options) error {
	return coopr.ValidateRegistryCaches(coopr.CacheOptions{RegistryFrom: options.CacheFrom, RegistryTo: options.CacheTo})
}

// Resolve existing parents too, so a symlink cannot hide a context overlap.
func canonical(p string) (string, error) {
	absolute, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(absolute)
	if parent == absolute {
		return "", err
	}
	resolved, err = canonical(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(absolute)), nil
}

func overlap(a, b string) bool {
	within := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return within(a, b) || within(b, a)
}
