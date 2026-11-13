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
	"golang.org/x/sys/unix"
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
	runMounts                                         bool
	ciImports                                         map[string]CIImport
	AllowHost                                         bool
	HostCapabilities                                  []string
	HostCachePlainHTTP                                bool
	hostSelections                                    map[string]bool
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
	return runOwned(ctx, w, plan, options)
}

// The ordinary and CI-projected paths share service ownership, capability
// validation, cancellation/draining and report persistence.
func runOwned(ctx context.Context, w *workflow.Workflow, plan *workflow.ExecutionPlan, options Options) (*Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !needsEngine(plan, options.ciImports) {
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
		err = persistReport(report, err)
	}
	return report, err
}

func validateCapabilities(ctx context.Context, plan *workflow.ExecutionPlan, client *coopr.Client, options Options) error {
	// Preflight the entire plan before starting jobs. Execute also negotiates,
	// but discovering an unsupported later job after side effects is too late.
	require := func(label string, capabilities ...string) error {
		_, err := client.Negotiate(ctx, capabilities...)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		return nil
	}
	var invocation []string
	if options.Network != "" {
		invocation = append(invocation, cooprwire.CapabilityRunNetwork)
	}
	if len(options.CacheFrom) != 0 || len(options.CacheTo) != 0 {
		invocation = append(invocation, cooprwire.CapabilityRegistryCache)
	}
	for _, input := range options.ciImports {
		switch input.Kind {
		case "component":
			invocation = append(invocation, cooprwire.CapabilityComponentLoad, cooprwire.CapabilityComponentLoadName)
		case "image":
			invocation = append(invocation, cooprwire.CapabilityExecute)
		}
	}
	if err := require("workflow options and exact CI imports", invocation...); err != nil {
		return err
	}
	for _, group := range plan.Groups {
		for _, job := range group.Jobs {
			if job.Definition.Host != nil {
				continue
			}
			capability := cooprwire.CapabilityExecute
			var required []string
			if len(job.Definition.AllMounts()) != 0 {
				required = append(required, cooprwire.CapabilityRunMounts)
			}
			if len(job.Definition.FileExports()) != 0 {
				required = append(required, cooprwire.CapabilitySelectedFileExports)
			}
			if job.Definition.Coopr != nil {
				platform, err := job.Definition.ResolvedPlatform()
				if err != nil {
					return fmt.Errorf("task %q platform: %w", job.Task, err)
				}
				if platform != "" {
					required = append(required, cooprwire.CapabilityBuildPlatform)
				}
				capability = cooprwire.CapabilityBuild
				if job.Definition.OutputKind() == "component" {
					capability = cooprwire.CapabilityComponent
				}
			} else if job.Definition.Publish != nil {
				if input, imported := options.ciImports[job.Definition.Publish.From]; imported && input.Kind == "image" {
					required = append(required, cooprwire.CapabilityPublishOCILayout)
				}
				capability = cooprwire.CapabilityPublish
			}
			if err := require(fmt.Sprintf("task %q", job.Task), append(required, capability)...); err != nil {
				return err
			}
		}
	}
	return nil
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
	plan, err := w.Expand(targets)
	if err != nil {
		return nil, options, err
	}
	return preparePlan(ctx, plan, options)
}

// Prepare already-expanded execution copies without adding another dependency
// planner. CI projection happens before paths and inline environments resolve.
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
				if options.hostSelections == nil || options.hostSelections[job.Name] {
					if err := validateHostExecution(job.Definition, options); err != nil {
						return nil, options, fmt.Errorf("task %q: %w", job.Task, err)
					}
				}
				continue
			}
			if _, err := job.Definition.ResolvedPlatform(); err != nil {
				return nil, options, fmt.Errorf("task %q platform: %w", job.Task, err)
			}
			if err := resolveTaskMounts(&job.Definition, options.ContextDir); err != nil {
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
				build.Files, err = source.DefinitionFilesForTask(job.Definition, options.ContextDir)
				if err != nil {
					return nil, options, fmt.Errorf("task %q definition files: %w", job.Task, err)
				}
			}
			if environment := job.Definition.Environment; environment != nil {
				environment.Files, err = source.DefinitionFilesForTask(job.Definition, options.ContextDir)
				if err != nil {
					return nil, options, fmt.Errorf("task %q environment files: %w", job.Task, err)
				}
			}
		}
	}
	plan, err = lowerEnvironments(plan)
	if err != nil {
		return nil, options, err
	}
	if err := admitFileExports(plan, options.ContextDir); err != nil {
		return nil, options, err
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
	// Serialize callers sharing this portable OCI cache, including other Stoke
	// processes. An interrupted process automatically releases the OS lock.
	lock, err := os.OpenFile(filepath.Join(options.CacheDir, ".stoke.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer func() {
		retErr = errors.Join(retErr, lock.Close())
		if report != nil {
			retErr = persistReport(report, retErr)
		}
	}()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("cache is in use by another Stoke run: %w", err)
	}
	dir, err := os.MkdirTemp(options.StateDir, "run-")
	if err != nil {
		return nil, err
	}
	output := options.OutputDir
	if output == "" {
		output = filepath.Join(dir, "outputs")
	}
	if err := os.Mkdir(output, 0700); err != nil {
		return nil, err
	}
	report = &Report{Status: "running", Directory: dir, Output: output, Tasks: []TaskResult{}}
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
	var imported map[string]coopr.Result
	if len(options.ciImports) != 0 {
		imported, err = loadCIImports(ctx, options.ciImports, options, dir, engine)
		if err != nil {
			return report, err
		}
	}
	report.Tasks, err = scheduleImported(ctx, w, plan, options, dir, output, engine, imported)
	return report, err
}

func persistReport(report *Report, runErr error) error {
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
	defer func() {
		if len(job.Definition.FileExports()) != 0 && len(result.Exports) == 0 {
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
	if job.Definition.Publish == nil {
		opts.Cache.RegistryFrom = slices.Clone(options.CacheFrom)
		opts.Cache.RegistryTo = slices.Clone(options.CacheTo)
	}
	if !job.Definition.FilesystemEnabled() {
		opts.Output.Filesystem = ""
	}
	staging, exports, err := stageJobFileExports(job, options.ContextDir)
	if err != nil {
		return coopr.Result{}, err
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
		_, _ = fmt.Fprintf(options.Stderr, "==> %s (cache=%t)\n", job.Name, !opts.Cache.NoCache)
	}
	switch {
	case job.Definition.Coopr != nil:
		build := job.Definition.Coopr
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
		result, err = engine.Build(ctx, cooprwire.Build{Files: build.Files, Platform: platform, Target: build.Target, Args: args, ComponentStoreDir: filepath.Join(dir, "components")}, component, opts)
	case job.Definition.Publish != nil:
		publication := job.Definition.Publish
		value, sourceErr := artifactSource(previous, publication.From)
		if sourceErr != nil {
			return coopr.Result{}, sourceErr
		}
		kind := "component"
		if previous[publication.From].ImageID != "" {
			kind = "image"
			value = previous[publication.From].ImageID
		}
		sourceDigest := ""
		if imported := previous[publication.From]; kind == "image" && imported.CanonicalReference != "" {
			_, sourceDigest, _ = strings.Cut(imported.CanonicalReference, "@")
			if imported.CanonicalOCI == "" || validateCIDigest(sourceDigest) != nil {
				return coopr.Result{}, errors.New("exact imported image publication requires its verified original OCI layout and digest")
			}
			value = "oci:" + imported.CanonicalOCI
		}
		opts.Output = coopr.Output{}
		result, err = engine.Publish(ctx, cooprwire.Publish{Artifact: kind, Source: value, SourceDigest: sourceDigest, Destinations: publication.Destinations, ComponentStoreDir: filepath.Join(dir, "components")}, opts)
	default:
		program, compileErr := compile(job.Name, job.Definition, previous)
		if compileErr != nil {
			return coopr.Result{}, compileErr
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
	if options.Stderr != nil && err == nil && result.CacheAvailable {
		_, _ = fmt.Fprintf(options.Stderr, "    %s: cache hits=%d misses=%d stored=%d skipped=%d errors=%d\n", job.Name, result.Cache.Hits, result.Cache.Misses, result.Cache.Stored, result.Cache.Skipped, result.Cache.Errors)
	}
	var exportErr error
	result.Exports, exportErr = publishJobFileExports(ctx, job, options.ContextDir, staging, exports, result.Exports, err)
	return result, errors.Join(err, exportErr)
}

func artifactSource(results map[string]coopr.Result, name string) (string, error) {
	result, ok := results[name]
	if ok && result.ImageID != "" {
		return "containers-storage:" + result.ImageID, nil
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
		from.Image = "containers-storage:" + results[task.From].ImageID
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
					copyOp.From.Image = "containers-storage:" + results[step.Copy.From].ImageID
				}
			} else if step.Copy.Scope == "component" {
				copyOp.From.Context = workflow.ComponentContext
			}
			state.Operations = append(state.Operations, copyOp)
		} else {
			run := coopr.Operation{Kind: "run", Args: step.Run}
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
	for _, p := range []*string{&options.StateDir, &options.CacheDir, &options.OutputDir} {
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
