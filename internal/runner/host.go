package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/strio-dev/stoke/internal/artifact"
	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/hostfiles"
	"github.com/strio-dev/stoke/internal/workflow"
)

const hostInputLimit = int64(512 << 20)

func hostContextName(name string) string {
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("stoke-host-%x", sum[:8])
}

func needsEngine(plan *workflow.ExecutionPlan, imports map[string]CIImport) bool {
	for _, group := range plan.Groups {
		for _, job := range group.Jobs {
			if job.Definition.Host == nil {
				return true
			}
		}
	}
	for _, input := range imports {
		if input.Kind != "host" {
			return true
		}
	}
	return false
}

func hostEnvironment(task workflow.Task) map[string]string {
	env := map[string]string{"PATH": os.Getenv("PATH"), "LANG": "C.UTF-8", "TZ": "UTC"}
	for _, name := range task.Host.InheritEnv {
		env[name] = os.Getenv(name)
	}
	for name, value := range task.Env {
		if value.Value != nil {
			env[name] = *value.Value
		}
	}
	return env
}

func hostTool(name, path string) (string, error) {
	check := func(candidate string) (string, error) {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return "", fmt.Errorf("host tool %q is not a regular executable", name)
		}
		return resolved, nil
	}
	if filepath.IsAbs(name) {
		return check(name)
	}
	for _, directory := range filepath.SplitList(path) {
		// Never select an implicit executable from the launch/work directory.
		if !filepath.IsAbs(directory) {
			continue
		}
		if resolved, err := check(filepath.Join(directory, name)); err == nil {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("required host tool %q is unavailable on the provisioned PATH", name)
}

func validateHostExecution(task workflow.Task, options Options) error {
	if !options.AllowHost {
		return errors.New("host execution requires caller opt-in with --allow-host; workflows and imports cannot grant it")
	}
	if task.CacheEnabled() {
		parent, err := filepath.EvalSymlinks(os.TempDir())
		if err != nil {
			return err
		}
		base := filepath.Join(parent, fmt.Sprintf("stoke-host-%d", os.Getuid()))
		for _, path := range []string{options.ContextDir, options.StateDir, options.CacheDir, options.OutputDir} {
			if path != "" && overlap(base, path) {
				return errors.New("reusable host workspace namespace overlaps context or runtime storage; choose a caller TMPDIR outside them")
			}
		}
	}
	platform, err := task.ResolvedPlatform()
	if err != nil {
		return err
	}
	actual := runtime.GOOS + "/" + runtime.GOARCH
	if runtime.GOOS != "linux" || platform != actual {
		return fmt.Errorf("host task requires native %s; runner is %s (host steps do not emulate or provision runners)", platform, actual)
	}
	for _, capability := range task.Host.Capabilities {
		if !slices.Contains(options.HostCapabilities, capability) {
			return fmt.Errorf("required host capability %q is not attested by the caller; provision it and pass --host-capability", capability)
		}
	}
	env := hostEnvironment(task)
	for _, tool := range task.Host.Tools {
		if _, err := hostTool(tool, env["PATH"]); err != nil {
			return err
		}
	}
	return nil
}

type hostFingerprint struct {
	Version      int               `json:"version"`
	Task         workflow.Task     `json:"task"`
	Platform     string            `json:"platform"`
	Env          map[string]string `json:"environment"`
	Files        map[string]string `json:"files"`
	Tools        map[string]string `json:"tools"`
	Toolchain    map[string]string `json:"toolchain"`
	Runtime      map[string]string `json:"runtime"`
	ExecutionDir string            `json:"executionDir"`
}

// Keys describe the explicit cache contract, not proof of host hermeticity.
// The author opts into captured effects and declares complete external inputs;
// fresh tests or undeclared host/network effects must leave caching disabled.
func hostKey(ctx context.Context, task workflow.Task, workspace string, env, tools map[string]string, executionDir string) (string, error) {
	files := map[string]string{}
	remaining := hostInputLimit
	hashPath := func(root, prefix string) error {
		return filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(files) >= 65536 {
				return errors.New("host cache inputs exceed the 65536 entry bound")
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, name)
			if err != nil {
				return err
			}
			key := prefix + "/" + filepath.ToSlash(relative)
			if info.IsDir() {
				files[key] = fmt.Sprintf("dir:%o", info.Mode().Perm())
				return nil
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("host cache input %s must be a regular file or directory (declare resolved toolchain paths)", name)
			}
			if info.Size() > remaining {
				return errors.New("host cache inputs exceed the 512 MiB/65536 entry bound")
			}
			remaining -= info.Size()
			f, err := os.Open(name)
			if err != nil {
				return err
			}
			hash := sha256.New()
			n, copyErr := io.Copy(hash, &hostContextReader{ctx: ctx, source: io.LimitReader(f, info.Size()+1)})
			if err := errors.Join(copyErr, f.Close()); err != nil {
				return err
			}
			if n != info.Size() {
				return errors.New("host cache input changed while hashing")
			}
			files[key] = fmt.Sprintf("file:%o:%x", info.Mode(), hash.Sum(nil))
			return nil
		})
	}
	for _, input := range task.Host.Inputs {
		destination := input.Destination
		if destination == "" {
			destination = input.Path
		}
		if err := hashPath(filepath.Join(workspace, destination), "input:"+destination); err != nil {
			return "", err
		}
	}
	for name, path := range tools {
		if err := hashPath(path, "executable:"+name+":"+path); err != nil {
			return "", err
		}
	}
	for _, path := range task.Host.Toolchain {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", fmt.Errorf("host toolchain input: %w", err)
		}
		if err := hashPath(resolved, "toolchain:"+path+":"+resolved); err != nil {
			return "", err
		}
	}
	definition := task
	definition.ComponentDir = ""
	definition.Needs = nil // Success-only gates do not alter declared execution inputs.
	identity, err := hostRuntimeIdentity()
	if err != nil {
		return "", err
	}
	// Revision 2 binds canonical materialization times and authored argv[0].
	data, err := json.Marshal(hostFingerprint{Version: 2, Task: definition, Platform: runtime.GOOS + "/" + runtime.GOARCH, Env: env, Files: files, Tools: tools, Runtime: identity, ExecutionDir: executionDir})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func hostRuntimeIdentity() (map[string]string, error) {
	groups, err := os.Getgroups()
	if err != nil {
		return nil, err
	}
	sort.Ints(groups)
	identity := map[string]string{"uid": fmt.Sprint(os.Getuid()), "gid": fmt.Sprint(os.Getgid()), "euid": fmt.Sprint(os.Geteuid()), "egid": fmt.Sprint(os.Getegid()), "groups": fmt.Sprint(groups)}
	for _, name := range []string{"osrelease", "version"} {
		data, err := os.ReadFile("/proc/sys/kernel/" + name)
		if err != nil {
			return nil, err
		}
		identity["kernel-"+name] = strings.TrimSpace(string(data))
	}
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Umask:") {
			identity["umask"] = strings.TrimSpace(strings.TrimPrefix(line, "Umask:"))
		}
	}
	cpu, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(cpu, 4<<20))
	if err := errors.Join(readErr, cpu.Close()); err != nil {
		return nil, err
	}
	features := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if slices.Contains([]string{"vendor_id", "cpu family", "model", "model name", "stepping", "flags", "Features", "CPU architecture", "CPU implementer", "CPU part", "CPU revision"}, key) {
			features[key+":"+strings.TrimSpace(value)] = true
		}
	}
	var values []string
	for value := range features {
		values = append(values, value)
	}
	sort.Strings(values)
	identity["cpu"] = strings.Join(values, "\n")
	return identity, nil
}

type hostContextReader struct {
	ctx    context.Context
	source io.Reader
}

type hostLogWriter struct {
	remaining int64
	writer    io.Writer
	cancel    context.CancelFunc
	err       error
}

func (w *hostLogWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		w.err = errors.New("host step log exceeds the 16 MiB stream bound; write large outputs into declared files")
		w.cancel()
		return 0, w.err
	}
	n, err := w.writer.Write(data)
	w.remaining -= int64(n)
	if err != nil {
		w.err = err
		w.cancel()
	}
	return n, err
}

func (r *hostContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(buffer)
}

func executeHost(ctx context.Context, job workflow.Job, options Options, taskDir string, previous map[string]coopr.Result) (coopr.Result, error) {
	return executeHostProcess(ctx, job, options, taskDir, previous, runHostProcess)
}

func executeHostProcess(ctx context.Context, job workflow.Job, options Options, taskDir string, previous map[string]coopr.Result, runProcess func(context.Context, *exec.Cmd) error) (result coopr.Result, retErr error) {
	task := job.Definition
	if err := validateHostExecution(task, options); err != nil {
		return result, err
	}
	workspace, err := os.MkdirTemp(taskDir, ".host-work-")
	if err != nil {
		return result, err
	}
	cleanupRoots := []string{workspace}
	var workspaceLock *os.File
	defer func() {
		for i := len(cleanupRoots) - 1; !errors.Is(retErr, coopr.ErrCleanupFailed) && i >= 0; i-- {
			if err := removePrivateHostTree(cleanupRoots[i]); err != nil {
				retErr = errors.Join(retErr, coopr.ErrCleanupFailed, err)
			}
		}
		if workspaceLock != nil {
			retErr = errors.Join(retErr, workspaceLock.Close())
		}
	}()
	for _, input := range task.Host.Inputs {
		root := options.hostContextDir
		if root == "" {
			root = options.ContextDir
		}
		if input.From != "" {
			root = previous[input.From].Filesystem
			if root == "" {
				return result, fmt.Errorf("host input %q has no captured files", input.From)
			}
		}
		destination := input.Destination
		if destination == "" {
			destination = input.Path
		}
		if err := hostfiles.Copy(ctx, root, input.Path, workspace, destination); err != nil {
			return result, fmt.Errorf("snapshot host input %q: %w", input.Path, err)
		}
		if err := boundHostWorkspace(ctx, workspace); err != nil {
			return result, err
		}
	}
	env := hostEnvironment(task)
	tools := map[string]string{}
	for _, tool := range task.Host.Tools {
		tools[tool], err = hostTool(tool, env["PATH"])
		if err != nil {
			return result, err
		}
	}
	platform := runtime.GOOS + "/" + runtime.GOARCH
	result.Platform = platform
	result.Metadata = map[string]json.RawMessage{"stoke.host.execution": json.RawMessage(`"executed"`)}
	key := ""
	useCache := task.CacheEnabled()
	runtimeRoot := filepath.Join(taskDir, "host-runtime")
	if useCache {
		// This preliminary digest selects a stable private execution location;
		// the actual reusable key below also binds that location and full env.
		seed, err := hostKey(ctx, task, workspace, env, tools, "declared-input-seed")
		if err != nil {
			return result, err
		}
		var root string
		root, workspaceLock, err = leaseHostWorkspace(ctx, seed, job.Name)
		if err != nil {
			return result, err
		}
		cleanupRoots = append(cleanupRoots, root)
		if err := hostfiles.Copy(ctx, workspace, ".", root, "work"); err != nil {
			return result, err
		}
		workspace = filepath.Join(root, "work")
		runtimeRoot = filepath.Join(root, "runtime")
	}
	if err := os.Mkdir(runtimeRoot, 0700); err != nil {
		return result, err
	}
	if !useCache {
		cleanupRoots = append(cleanupRoots, runtimeRoot)
	}
	for name, leaf := range map[string]string{"HOME": "home", "TMPDIR": "tmp"} {
		if _, explicit := env[name]; !explicit {
			directory := filepath.Join(runtimeRoot, leaf)
			if err := os.Mkdir(directory, 0700); err != nil {
				return result, err
			}
			env[name] = directory
		}
	}
	executionDir := filepath.Join(workspace, task.Workdir)
	if useCache {
		key, err = hostKey(ctx, task, workspace, env, tools, executionDir)
		if err != nil {
			return result, err
		}
		result.CacheAvailable = true
		result.Cache.Misses = 1
		if options.NoCache {
			result.Cache.Misses = 0
			result.Cache.Skipped = 1
		} else {
			if hit, err := restoreHostCache(ctx, options, key, platform, task.Host.Outputs, taskDir, &result); err != nil {
				return result, err
			} else if hit {
				result.Cache.Hits, result.Cache.Misses = 1, 0
				result.Metadata["stoke.host.execution"] = json.RawMessage(`"cached-output-restored"`)
				if err := storeHostCache(ctx, options, key, result); err != nil {
					return result, err
				}
				if options.Stderr != nil {
					_, _ = fmt.Fprintf(options.Stderr, "==> %s host outputs restored from cache; commands did not execute\n", job.Name)
				}
				return result, nil
			}
		}
	}
	environment := make([]string, 0, len(env))
	for name, value := range env {
		environment = append(environment, name+"="+value)
	}
	sort.Strings(environment)
	stdout, err := os.OpenFile(filepath.Join(taskDir, "stdout.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	stderr, err := os.OpenFile(filepath.Join(taskDir, "stderr.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, errors.Join(err, stdout.Close())
	}
	defer func() { retErr = errors.Join(retErr, stdout.Close(), stderr.Close()) }()
	stdoutRemaining, stderrRemaining := int64(16<<20), int64(16<<20)
	for index, step := range task.Steps {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if options.Stderr != nil {
			_, _ = fmt.Fprintf(options.Stderr, "==> %s host step %d (fresh execution; runner authority)\n", job.Name, index+1)
		}
		if err := hostWorkdir(workspace, task.Workdir); err != nil {
			return result, err
		}
		command := exec.Command(tools[step.Run[0]], step.Run[1:]...)
		// Resolve and hash the executable without changing authored argv[0]:
		// shells and multicall tools use their invocation name as semantics.
		command.Args[0] = step.Run[0]
		command.Env, command.Dir = environment, filepath.Join(workspace, task.Workdir)
		outs, errs := []io.Writer{stdout}, []io.Writer{stderr}
		if options.Stdout != nil {
			outs = append(outs, options.Stdout)
		}
		if options.Stderr != nil {
			errs = append(errs, options.Stderr)
		}
		var captured *os.File
		if step.Stdout != "" {
			captured, err = openHostStdout(workspace, step.Stdout)
			if err != nil {
				return result, err
			}
			outs = append(outs, captured)
		}
		stepCtx, cancel := context.WithCancel(ctx)
		outWriter := &hostLogWriter{remaining: stdoutRemaining, writer: io.MultiWriter(outs...), cancel: cancel}
		errWriter := &hostLogWriter{remaining: stderrRemaining, writer: io.MultiWriter(errs...), cancel: cancel}
		command.Stdout, command.Stderr = outWriter, errWriter
		err = runProcess(stepCtx, command)
		cancel()
		stdoutRemaining, stderrRemaining = outWriter.remaining, errWriter.remaining
		err = errors.Join(err, outWriter.err, errWriter.err)
		if captured != nil {
			err = errors.Join(err, captured.Close())
		}
		if err != nil {
			return result, fmt.Errorf("host step %d: %w", index+1, err)
		}
	}
	if useCache {
		// Hash the same declared input set and provisioned executable/toolchain
		// again before publishing reusable results. Mutated inputs are not cached.
		afterEnv := hostEnvironment(task)
		for _, name := range []string{"HOME", "TMPDIR"} {
			if _, explicit := afterEnv[name]; !explicit {
				afterEnv[name] = env[name]
			}
		}
		after, err := hostKey(ctx, task, workspace, afterEnv, tools, executionDir)
		if err != nil {
			return result, err
		}
		if after != key {
			return result, errors.New("host inputs/toolchain changed during execution; refusing result reuse")
		}
	}
	if len(task.Host.Outputs) != 0 {
		result.OCI = filepath.Join(taskDir, "files.oci")
		result.ManifestDigest, err = hostfiles.CaptureWithKey(ctx, workspace, task.Host.Outputs, result.OCI, platform, key)
		if err != nil {
			return result, err
		}
		result.Filesystem = filepath.Join(taskDir, "files")
		if err := hostfiles.Restore(ctx, result.OCI, result.ManifestDigest, result.Filesystem); err != nil {
			return result, err
		}
	}
	if useCache {
		if err := storeHostCache(ctx, options, key, result); err != nil {
			return result, err
		}
		result.Cache.Stored = 1
	}
	return result, nil
}

// Freeze selected host context inputs once before admitting commands. Producer
// inputs use their immutable captures instead; host-side writes are not data
// flow unless declared as outputs and consumed through an explicit edge.
func snapshotHostContext(ctx context.Context, plan *workflow.ExecutionPlan, source, runDir string) (string, error) {
	set := map[string]bool{}
	for _, group := range plan.Groups {
		for _, job := range group.Jobs {
			if job.Definition.Host != nil {
				for _, input := range job.Definition.Host.Inputs {
					if input.From == "" {
						set[input.Path] = true
					}
				}
			}
		}
	}
	if len(set) == 0 {
		return "", nil
	}
	var paths []string
	for name := range set {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	root, err := os.MkdirTemp(runDir, "host-inputs-")
	if err != nil {
		return "", err
	}
	var selected []string
	for _, name := range paths {
		covered := false
		for _, prior := range selected {
			if prior == "." || strings.HasPrefix(name, prior+"/") {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		selected = append(selected, name)
		if err := hostfiles.Copy(ctx, source, name, root, name); err != nil {
			return root, fmt.Errorf("host context input %q: %w", name, err)
		}
		if err := boundHostWorkspace(ctx, root); err != nil {
			return root, err
		}
	}
	return root, nil
}

func boundHostWorkspace(ctx context.Context, root string) error {
	var bytes int64
	entries := 0
	return filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		entries++
		if entries > 65536 {
			return errors.New("host input snapshot exceeds 65536 entries")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		bytes += info.Size()
		if bytes > hostInputLimit {
			return errors.New("host input snapshot exceeds 512 MiB")
		}
		return nil
	})
}

func hostWorkdir(workspace, name string) error {
	if name == "" || name == "." {
		return nil
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for current := name; current != "."; current = filepath.Dir(current) {
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("host workdir must resolve to a workspace directory without symlinks")
		}
	}
	return nil
}

func openHostStdout(workspace, name string) (*os.File, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	parent := filepath.Dir(name)
	if parent != "." {
		if err := root.MkdirAll(parent, 0700); err != nil {
			return nil, err
		}
		for cursor := parent; cursor != "."; cursor = filepath.Dir(cursor) {
			info, err := root.Lstat(cursor)
			if err != nil {
				return nil, err
			}
			if !info.IsDir() {
				return nil, errors.New("host stdout parent must be a directory, not a symlink")
			}
		}
	}
	return root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
}

func restoreHostCache(ctx context.Context, options Options, key, platform string, outputs []string, taskDir string, result *coopr.Result) (bool, error) {
	local := filepath.Join(options.CacheDir, "host", key)
	var identity string
	if _, err := os.Stat(local); err == nil {
		data, err := os.ReadFile(filepath.Join(local, "identity"))
		if err != nil {
			return false, err
		}
		identity = strings.TrimSpace(string(data))
		local = filepath.Join(local, "oci")
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if identity == "" {
		client := artifact.Client{AuthFile: options.AuthFile, PlainHTTP: options.HostCachePlainHTTP}
		for _, repository := range options.CacheFrom {
			remote := filepath.Join(taskDir, "registry-cache")
			digest, hit, err := client.FetchHostCache(ctx, repository, key, platform, remote)
			if err != nil {
				return false, err
			}
			if hit {
				local, identity = remote, digest
				break
			}
		}
	}
	if identity == "" {
		return false, nil
	}
	config, err := hostfiles.Validate(ctx, local, identity)
	if err != nil {
		return false, err
	}
	if config.InputKey != key || config.Platform.OS+"/"+config.Platform.Architecture != platform {
		return false, errors.New("host cache identity/platform mismatch")
	}
	expected := slices.Clone(outputs)
	slices.Sort(expected)
	if !slices.Equal(config.Paths, expected) {
		return false, errors.New("host cache captured paths differ from declared outputs")
	}
	result.OCI = filepath.Join(taskDir, "files.oci")
	if err := hostfiles.Copy(ctx, local, ".", taskDir, "files.oci"); err != nil {
		return false, err
	}
	result.ManifestDigest, result.Filesystem = identity, filepath.Join(taskDir, "files")
	if err := hostfiles.Restore(ctx, result.OCI, identity, result.Filesystem); err != nil {
		return false, err
	}
	return true, nil
}

func storeHostCache(ctx context.Context, options Options, key string, result coopr.Result) error {
	parent := filepath.Join(options.CacheDir, "host")
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, ".capture-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := hostfiles.Copy(ctx, result.OCI, ".", staging, "oci"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, "identity"), []byte(result.ManifestDigest+"\n"), 0600); err != nil {
		return err
	}
	if err := hostfiles.Commit(staging, filepath.Join(parent, key)); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	client := artifact.Client{AuthFile: options.AuthFile, PlainHTTP: options.HostCachePlainHTTP}
	for _, repository := range options.CacheTo {
		if err := client.PublishHostCache(ctx, repository, key, result.OCI, result.ManifestDigest); err != nil {
			return err
		}
	}
	return nil
}
