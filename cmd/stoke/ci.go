package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/strio-dev/stoke/internal/artifact"
	"github.com/strio-dev/stoke/internal/ci"
	"github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/runner"
	"github.com/strio-dev/stoke/internal/source"
)

const ciHelp = `Usage: stoke ci <command> [options] [targets...]

Commands:
  plan            Print a resolved, versioned CI plan as JSON
  generate        Generate an owned reusable GitHub Actions workflow and plan
  scope           Create a unique GitHub invocation scope
  run             Run a visible job using exact immutable OCI inputs
  cleanup         Delete one scoped result-metadata object after its retry window
  verify-results  Require all provider job results to be successful

Generate a separate workflow; handwritten workflows remain under your control.
Stages and matrix variants are visible jobs by default; co-location is explicit.
Use --check to verify regeneration without writing. Execution needs a provisioned
Linux runner and Stoke. Container tasks also need a compatible Coopr executable.
Host tasks require explicit --allow-host authority and provisioned tools.
No engine version or host capability is installed or granted by generation.`

func runCI(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 || (len(args) == 1 && slices.Contains([]string{"help", "--help", "-h"}, args[0])) {
		_, err := fmt.Fprintln(out, ciHelp)
		if err != nil {
			return 1
		}
		return 0
	}
	command := args[0]
	if !slices.Contains([]string{"plan", "generate", "scope", "run", "cleanup", "verify-results"}, command) {
		_, _ = fmt.Fprintf(errOut, "stoke ci: unknown command %q\n", command)
		return 2
	}
	fs := flag.NewFlagSet("ci "+command, flag.ContinueOnError)
	fs.SetOutput(errOut)
	var config ci.Config
	var options runner.Options
	tlsVerify := true
	var client artifact.Client
	var exchange ciExchangeOptions
	var cleanupReference string
	var job, expectedPlan, needs string
	planPath, output := ".stoke/ci-plan.json", ".github/workflows/stoke.generated.yml"
	check := false
	if command == "plan" || command == "generate" {
		fs.StringVar(&config.Workflow, "f", "stoke.yaml", "local workflow path, relative to the repository root")
		fs.StringVar(&config.Context, "context", "", "checkout-relative input directory (default: workflow directory)")
		fs.StringVar(&config.JobContainerImage, "job-container-image", "", "digest-pinned tools image for every generated GitHub job")
		fs.StringVar(&config.JobContainerOptions, "job-container-options", "", "explicit Docker job-container options; no security options are added automatically")
		fs.IntVar(&config.TimeoutMinutes, "timeout", 45, "generated job timeout in minutes")
		fs.Func("runner", "provisioned GitHub runner label (repeatable; default: self-hosted, linux)", func(label string) error {
			config.Runners = append(config.Runners, label)
			return nil
		})
		fs.Func("co-locate", "explicitly keep this data-connected component in one invocation (repeatable)", func(task string) error {
			config.CoLocate = append(config.CoLocate, task)
			return nil
		})
		config.Inputs = map[string]string{}
		fs.Func("input", "workflow input NAME=VALUE (recorded in the generated plan)", func(value string) error {
			name, literal, ok := strings.Cut(value, "=")
			if !ok || name == "" {
				return errors.New("input requires NAME=VALUE")
			}
			if _, exists := config.Inputs[name]; exists {
				return fmt.Errorf("duplicate input %q", name)
			}
			config.Inputs[name] = literal
			return nil
		})
	}
	if command == "generate" {
		fs.StringVar(&output, "output", output, "owned generated reusable workflow path")
		fs.StringVar(&planPath, "plan-output", planPath, "owned generated plan path")
		fs.BoolVar(&check, "check", false, "fail on drift without creating or writing files")
	}
	if command == "plan" || command == "generate" || command == "run" {
		fs.StringVar(&options.Network, "network", "", "container RUN network default (recorded in generated plans)")
		for _, entry := range []struct {
			name string
			list *[]string
		}{{"cache-from", &options.CacheFrom}, {"cache-to", &options.CacheTo}} {
			fs.Func(entry.name, "explicit registry cache repository (repeatable)", func(value string) error {
				if err := engine.ValidateCacheRepository(value); err != nil {
					return err
				}
				*entry.list = append(*entry.list, value)
				return nil
			})
		}
	}
	if slices.Contains([]string{"plan", "generate", "run", "cleanup"}, command) {
		fs.Func("results-repository", "explicit OCI repository for exact results, separate from cache configuration", func(value string) error {
			if len(value) > 512 {
				return errors.New("result repository exceeds 512 bytes")
			}
			if err := artifact.ValidateCIRepository(value); err != nil {
				return err
			}
			exchange.Repository = value
			return nil
		})
	}
	if command == "scope" {
		fs.StringVar(&expectedPlan, "plan-id", "", "plan fingerprint for this invocation")
		fs.StringVar(&exchange.GitHubOutput, "github-output", "", "existing native output file")
	}
	if command == "run" || command == "cleanup" {
		fs.StringVar(&exchange.Scope, "scope", "", "invocation scope JSON from native setup")
	}
	if command == "cleanup" {
		fs.StringVar(&cleanupReference, "result", "", "immutable scoped metadata reference to delete; never deletes payloads")
	}
	if command == "run" {
		hostExecutionFlags(fs, &options)
		fs.StringVar(&planPath, "plan", planPath, "generated plan path")
		fs.StringVar(&expectedPlan, "plan-id", "", "expected fingerprint embedded in the generated CI job")
		fs.StringVar(&job, "job", "", "planned execution unit or variant ID")
		fs.IntVar(&exchange.Attempt, "attempt", 0, "current native run attempt (positive integer)")
		fs.StringVar(&exchange.Needs, "needs", "", "explicit native needs JSON for local/provider integration")
		fs.BoolVar(&exchange.NeedsEnv, "needs-env", false, "read expected per-variant native input environment slots")
		fs.StringVar(&exchange.GitHubOutput, "github-output", "", "existing native output file")
		fs.StringVar(&options.CooprBinary, "coopr-binary", os.Getenv("STOKE_COOPR_BINARY"), "compatible Coopr executable (default: coopr on PATH)")
		fs.StringVar(&options.OutputDir, "output", "", "fresh output directory outside the input context")
		fs.StringVar(&options.CacheDir, "cache-dir", "", "local disposable cache directory outside the input context")
		fs.StringVar(&options.Runtime, "runtime", "crun", "OCI runtime name or path")
		fs.StringVar(&options.StorageDriver, "storage-driver", "vfs", "containers/storage driver")
		fs.StringVar(&options.SignaturePolicy, "signature-policy", "", "containers/image signature policy")
		fs.BoolVar(&tlsVerify, "tls-verify", true, "verify native registry/cache TLS (false for isolated test registries)")
		fs.BoolVar(&options.NoCache, "no-cache", false, "disable result reuse; disposable cache volumes remain available")
	}
	if command == "verify-results" {
		fs.StringVar(&needs, "results", "", "JSON object from GitHub toJSON(needs)")
	} else {
		fs.StringVar(&client.AuthFile, "authfile", "", "registry authentication file (never included in generated files)")
		fs.BoolVar(&client.PlainHTTP, "plain-http", false, "HTTP workflow/result registries, for local qualification only")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	options.HostCachePlainHTTP = client.PlainHTTP
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "tls-verify" {
			options.TLSVerify = &tlsVerify
		}
	})
	var err error
	switch command {
	case "scope":
		if fs.NArg() != 0 {
			err = errors.New("scope does not accept targets")
		} else {
			err = createCIScope(expectedPlan, exchange.GitHubOutput, out)
		}
	case "cleanup":
		var scope artifact.CIScope
		if fs.NArg() != 0 || exchange.Repository == "" || cleanupReference == "" {
			err = errors.New("cleanup requires --results-repository, --scope JSON and --result immutable-reference; stop writers and wait for the retry window first")
		} else if err = decodeCIJSON(exchange.Scope, &scope, 8192); err == nil {
			err = client.DeleteCIObject(ctx, exchange.Repository, cleanupReference, scope)
			if err == nil {
				_, err = fmt.Fprintln(out, "Deleted scoped metadata; shared payloads remain under registry retention policy")
			}
		}
	case "verify-results":
		if fs.NArg() != 0 {
			err = errors.New("verify-results does not accept targets")
		} else {
			err = verifyCIResults(needs)
		}
		if err == nil {
			_, err = fmt.Fprintln(out, "All generated CI jobs succeeded")
		}
	case "plan", "generate":
		config.Targets = fs.Args()
		config.CacheFrom, config.CacheTo = options.CacheFrom, options.CacheTo
		config.Network = options.Network
		config.ResultsRepository = exchange.Repository
		if config.Context == "" {
			config.Context = filepath.ToSlash(filepath.Dir(config.Workflow))
		}
		var plan *ci.Plan
		plan, err = loadCIPlan(ctx, config, client)
		if err != nil {
			break
		}
		var data []byte
		data, err = ci.Encode(plan)
		if err != nil {
			break
		}
		if command == "plan" {
			_, err = out.Write(data)
			break
		}
		var generated []byte
		generated, err = ci.RenderGitHub(plan, filepath.ToSlash(planPath))
		if err == nil {
			err = writeGeneratedCI(planPath, output, data, generated, check)
		}
		if err == nil {
			verb := "Generated"
			if check {
				verb = "Verified"
			}
			_, err = fmt.Fprintf(out, "%s %s and %s (plan %s)\n", verb, planPath, output, plan.ID)
		}
	case "run":
		if fs.NArg() != 0 || job == "" {
			err = errors.New("ci run requires --job and accepts no targets")
			break
		}
		var plan *ci.Plan
		var file *os.File
		file, err = os.Open(planPath)
		if err == nil {
			plan, err = ci.Decode(file)
			err = errors.Join(err, file.Close())
		}
		if err != nil {
			break
		}
		if expectedPlan != "" && plan.ID != expectedPlan {
			err = errors.New("generated CI workflow and plan fingerprints differ; regenerate both files and review")
			break
		}
		selected, exists := plan.FindJob(job)
		if !exists {
			err = fmt.Errorf("job %q is absent from the plan", job)
			break
		}
		if err = validateCIHostAuthority(plan, selected, options); err != nil {
			break
		}
		var loaded *source.Result
		var executionErr error
		if err = validateCICheckout(plan.Config, nil); err != nil {
			break
		}
		loaded, err = (source.Loader{Client: client, Inputs: plan.Config.Inputs}).Load(ctx, plan.Config.Workflow)
		if err != nil {
			break
		}
		defer func() { _ = cleanupExecutionInputs(executionErr, loaded.Close) }()
		if err = validateCICheckout(plan.Config, loaded); err != nil {
			break
		}
		var current *ci.Plan
		current, err = ci.Build(loaded.Workflow, plan.Config)
		if err == nil && current.ID != plan.ID {
			err = errors.New("workflow differs from the generated CI plan; regenerate and review it")
		}
		if err != nil {
			break
		}
		options.ContextDir, options.AuthFile = plan.Config.Context, client.AuthFile
		if options.Network != "" && options.Network != plan.Config.Network {
			err = errors.New("network setting differs from the generated plan; regenerate and review it")
			break
		}
		options.Network = plan.Config.Network
		if len(options.CacheFrom) == 0 {
			options.CacheFrom = slices.Clone(plan.Config.CacheFrom)
		}
		if len(options.CacheTo) == 0 {
			options.CacheTo = slices.Clone(plan.Config.CacheTo)
		}
		options.Stdout, options.Stderr = errOut, errOut
		if err = loaded.ValidateContext(options.ContextDir); err != nil {
			break
		}
		var report *ciRunReport
		report, err = runCIExchange(ctx, loaded.Workflow, plan, selected, options, client, exchange)
		executionErr = err
		err = persistCIRun(report, err)
		if report != nil && report.Report != nil {
			err = errors.Join(err, json.NewEncoder(out).Encode(report))
		}
	}
	if err != nil {
		_, _ = fmt.Fprintln(errOut, "stoke ci:", err)
		return 1
	}
	return 0
}

func loadCIPlan(ctx context.Context, config ci.Config, client artifact.Client) (*ci.Plan, error) {
	if err := validateCICheckout(config, nil); err != nil {
		return nil, err
	}
	loaded, err := (source.Loader{Client: client, Inputs: config.Inputs}).Load(ctx, config.Workflow)
	if err != nil {
		return nil, err
	}
	defer func() { _ = loaded.Close() }()
	if len(config.Targets) == 0 {
		config.Targets = loaded.Targets
	}
	if err := validateCICheckout(config, loaded); err != nil {
		return nil, err
	}
	return ci.Build(loaded.Workflow, config)
}

// Check the physical locations as well as the portable selectors. Symlinked
// inputs must not silently depend on files unavailable in a fresh checkout.
func validateCICheckout(config ci.Config, loaded *source.Result) error {
	root, err := filepath.Abs(".")
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	within := func(name string, directory bool) error {
		if filepath.IsAbs(name) {
			return fmt.Errorf("CI input %q must be checkout-relative", name)
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(root, name))
		if err != nil {
			return fmt.Errorf("CI input %q unavailable: %w", name, err)
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("CI input %q escapes the checkout", name)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return err
		}
		if directory && !info.IsDir() || !directory && !info.Mode().IsRegular() {
			return fmt.Errorf("CI input %q has the wrong file type", name)
		}
		return nil
	}
	if err := within(config.Workflow, false); err != nil {
		return err
	}
	if err := within(config.Context, true); err != nil {
		return err
	}
	if loaded != nil {
		plan, err := loaded.Workflow.Expand(config.Targets)
		if err != nil {
			return err
		}
		for _, group := range plan.Groups {
			for _, job := range group.Jobs {
				for _, directory := range job.Definition.Contexts {
					if err := within(filepath.Join(config.Context, directory), true); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func verifyCIResults(raw string) error {
	if strings.HasPrefix(strings.TrimSpace(raw), "[") {
		var values []string
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			return fmt.Errorf("invalid provider statuses: %w", err)
		}
		if len(values) == 0 {
			return errors.New("provider statuses are empty")
		}
		for _, value := range values {
			if value != "success" {
				return fmt.Errorf("provider job is %q, expected success", value)
			}
		}
		return nil
	}
	var results map[string]struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &results); err != nil {
		return fmt.Errorf("invalid provider job results: %w", err)
	}
	if len(results) == 0 {
		return errors.New("provider job results are empty")
	}
	var failures []string
	for name, job := range results {
		if job.Result != "success" {
			failures = append(failures, fmt.Sprintf("%s=%s", name, job.Result))
		}
	}
	slices.Sort(failures)
	if len(failures) != 0 {
		return fmt.Errorf("CI dependency gate failed: %s", strings.Join(failures, ", "))
	}
	return nil
}

// Validate both destinations before writing either. A crash between atomic
// replacements is detected by plan fingerprint checks at execution/regeneration.
func writeGeneratedCI(planPath, workflowPath string, plan, generated []byte, check bool) error {
	if filepath.Clean(planPath) == filepath.Clean(workflowPath) {
		return errors.New("plan and generated workflow need different paths")
	}
	files := []struct {
		path string
		data []byte
		kind string
	}{{planPath, plan, "plan"}, {workflowPath, generated, "workflow"}}
	for _, file := range files {
		if err := safeGeneratedPath(file.path); err != nil {
			return err
		}
		current, err := os.ReadFile(file.path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if check {
			if err != nil || !bytes.Equal(current, file.data) {
				return fmt.Errorf("generated %s is missing or stale: %s; regenerate and review", file.kind, file.path)
			}
			continue
		}
		if err == nil {
			if file.kind == "workflow" {
				err = ci.VerifyGenerated(current)
			} else {
				_, err = ci.Decode(bytes.NewReader(current))
			}
			if err != nil {
				return fmt.Errorf("refusing to overwrite handwritten or edited %s %s: %w", file.kind, file.path, err)
			}
		}
	}
	if check {
		return nil
	}
	for _, file := range files {
		current, err := os.ReadFile(file.path)
		if err == nil && bytes.Equal(current, file.data) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(file.path), 0755); err != nil {
			return err
		}
		temporary, err := os.CreateTemp(filepath.Dir(file.path), ".stoke-ci-*")
		if err != nil {
			return err
		}
		_, writeErr := temporary.Write(file.data)
		if err = errors.Join(writeErr, temporary.Chmod(0644), temporary.Close()); err == nil {
			err = os.Rename(temporary.Name(), file.path)
		}
		_ = os.Remove(temporary.Name())
		if err != nil {
			return err
		}
	}
	return nil
}

func safeGeneratedPath(name string) error {
	clean := filepath.Clean(name)
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("generated path %q must be inside the repository", name)
	}
	for p := clean; p != "."; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && (!info.IsDir() && p != clean || info.Mode()&os.ModeSymlink != 0) {
			return fmt.Errorf("generated path %q traverses a symlink or nondirectory", name)
		}
		if err == nil && p == clean && !info.Mode().IsRegular() {
			return fmt.Errorf("generated destination %q is not a regular file", name)
		}
	}
	return nil
}
