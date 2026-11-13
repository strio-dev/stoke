package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/strio-dev/stoke/internal/artifact"
	"github.com/strio-dev/stoke/internal/ci"
	"github.com/strio-dev/stoke/internal/runner"
	"github.com/strio-dev/stoke/internal/workflow"
	"golang.org/x/sys/unix"
)

type ciExchangeOptions struct {
	Scope, Needs, GitHubOutput, Repository string
	Attempt                                int
	NeedsEnv                               bool
}

// Native metadata selects immutable input bindings. It contains no success
// assertion; gate-only jobs emit it before executing their task.
type ciNativeMetadata struct {
	Version int    `json:"version"`
	Scope   string `json:"scope"`
	Unit    string `json:"unit"`
	Attempt int    `json:"attempt"`
	Inputs  string `json:"inputs,omitempty"`
}

type ciRunReport struct {
	*runner.Report
	CI *ciNativeMetadata `json:"ci,omitempty"`
}

func decodeCIJSON(raw string, value any, limit int) error {
	if len(raw) > limit {
		return errors.New("CI metadata exceeds its size limit")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("CI metadata requires one JSON document")
	}
	return nil
}

func writeGitHubOutput(name, value, filename string) error {
	if filename == "" {
		return nil
	}
	if strings.ContainsAny(name+value, "\r\n") || name == "" {
		return errors.New("CI native output must be one line")
	}
	fd, err := unix.Open(filename, unix.O_WRONLY|unix.O_APPEND|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open native output file: %w", err)
	}
	f := os.NewFile(uintptr(fd), filename)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.Join(errors.New("native output destination must be an existing regular file"), err, f.Close())
	}
	_, err = fmt.Fprintf(f, "%s=%s\n", name, value)
	return errors.Join(err, f.Close())
}

func createCIScope(plan, githubOutput string, out io.Writer) error {
	nonce, err := artifact.NewCIExecutionID()
	if err != nil {
		return err
	}
	scope := artifact.CIScope{Provider: "github", Server: os.Getenv("GITHUB_SERVER_URL"), Repository: os.Getenv("GITHUB_REPOSITORY"), Run: os.Getenv("GITHUB_RUN_ID"), Source: os.Getenv("GITHUB_SHA"), Invocation: nonce, Plan: plan}
	if err := scope.Validate(); err != nil {
		return fmt.Errorf("invocation setup requires a plan fingerprint and provisioned GitHub run context: %w", err)
	}
	data, err := json.Marshal(scope)
	if err != nil {
		return err
	}
	if err := writeGitHubOutput("scope", string(data), githubOutput); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(data))
	return err
}

func ciParentJobs(p *ci.Plan, selected ci.Job) ([]ci.Job, error) {
	var group *ci.Group
	for i := range p.Groups {
		if slices.ContainsFunc(p.Groups[i].Jobs, func(j ci.Job) bool { return j.ID == selected.ID }) {
			group = &p.Groups[i]
			break
		}
	}
	if group == nil {
		return nil, errors.New("selected CI job has no provider group")
	}
	var parents []ci.Job
	for _, id := range group.Needs {
		found := false
		for _, candidate := range p.Groups {
			if candidate.ID == id {
				parents = append(parents, candidate.Jobs...)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown provider prerequisite %q", id)
		}
	}
	return parents, nil
}

func readCINativeInputs(p *ci.Plan, job ci.Job, options ciExchangeOptions, scopeHash string) ([]string, error) {
	parents, err := ciParentJobs(p, job)
	if err != nil {
		return nil, err
	}
	if options.NeedsEnv && options.Needs != "" {
		return nil, errors.New("choose either --needs-env or --needs JSON")
	}
	var needs map[string]struct {
		Result  string            `json:"result"`
		Outputs map[string]string `json:"outputs"`
	}
	if options.Needs != "" {
		if err := decodeCIJSON(options.Needs, &needs, 1<<20); err != nil {
			return nil, fmt.Errorf("native needs metadata: %w", err)
		}
		for _, v := range needs {
			if v.Result != "success" {
				return nil, errors.New("native prerequisite is not successful")
			}
		}
	}
	var refs []string
	for _, parent := range parents {
		value := ""
		if options.NeedsEnv {
			value = os.Getenv(ci.InputEnv(parent.OutputKey))
		} else {
			for _, group := range p.Groups {
				if slices.ContainsFunc(group.Jobs, func(job ci.Job) bool { return job.ID == parent.ID }) {
					value = needs[group.ID].Outputs[parent.OutputKey]
					break
				}
			}
		}
		if value == "" {
			return nil, fmt.Errorf("native metadata for %q is unavailable; rerun its producer and dependent gates (partial-rerun outputs are not guaranteed)", parent.ID)
		}
		var metadata ciNativeMetadata
		if err := decodeCIJSON(value, &metadata, ci.MaxNativeMetadataBytes); err != nil {
			return nil, fmt.Errorf("native metadata for %q: %w", parent.ID, err)
		}
		if metadata.Version != 1 || metadata.Scope != scopeHash || metadata.Unit != parent.ID || metadata.Attempt < 1 || metadata.Attempt > options.Attempt {
			return nil, fmt.Errorf("native metadata scope/unit/attempt mismatch for %q; rerun producer and dependent gates", parent.ID)
		}
		if len(parent.Exports) != 0 && metadata.Inputs == "" {
			return nil, fmt.Errorf("producer %q omitted exact result bindings", parent.ID)
		}
		if metadata.Inputs != "" {
			refs = append(refs, metadata.Inputs)
		}
	}
	slices.Sort(refs)
	return slices.Compact(refs), nil
}

type ciProducerSpec struct{ Kind, Platform, Unit string }

func ciProducerSpecs(w *workflow.Workflow, p *ci.Plan) (map[string]ciProducerSpec, error) {
	expanded, err := w.Expand(p.Config.Targets)
	if err != nil {
		return nil, err
	}
	all := map[string]ciProducerSpec{}
	for _, group := range expanded.Groups {
		for _, job := range group.Jobs {
			if job.Definition.Publish != nil {
				continue
			}
			platform, err := job.Definition.ResolvedPlatform()
			if err != nil {
				return nil, err
			}
			if job.Definition.Coopr != nil {
				// A native FROM may override the authored build default.
				platform = ""
			}
			all[job.Name] = ciProducerSpec{Kind: job.Definition.OutputKind(), Platform: platform}
		}
	}
	for name, consumer := range p.Environments {
		spec, exists := all[consumer]
		if !exists {
			return nil, fmt.Errorf("unknown environment consumer %q", consumer)
		}
		spec.Kind = "image"
		// Promoted inline environments are native builds too. Their consumers
		// retain any explicit platform requirement in their own specs.
		spec.Platform = ""
		all[name] = spec
	}
	result := map[string]ciProducerSpec{}
	for _, group := range p.Groups {
		for _, job := range group.Jobs {
			for _, name := range job.Exports {
				spec, exists := all[name]
				if !exists || spec.Kind == "publication" {
					return nil, fmt.Errorf("invalid exact result producer %q", name)
				}
				spec.Unit = job.ID
				result[name] = spec
			}
		}
	}
	return result, nil
}

func validateCIBindings(ctx context.Context, client artifact.Client, repository string, scope artifact.CIScope, attempt int, set artifact.CIInputSet, specs map[string]ciProducerSpec) (map[string]artifact.CIResult, error) {
	results := map[string]artifact.CIResult{}
	units := map[string]string{}
	for name, ref := range set.Bindings {
		spec, exists := specs[name]
		if !exists {
			return nil, fmt.Errorf("unknown exact result producer %q", name)
		}
		value, err := client.ReadCIResult(ctx, repository, ref, artifact.CIResultExpectation{Scope: scope, Producer: name, MaxAttempt: attempt, Kind: spec.Kind, Platform: spec.Platform})
		if err != nil {
			return nil, err
		}
		unitExecution := strconv.Itoa(value.Attempt) + ":" + value.Execution
		if previous, exists := units[spec.Unit]; exists && previous != unitExecution {
			return nil, fmt.Errorf("inconsistent executions within producer unit %q", spec.Unit)
		}
		units[spec.Unit] = unitExecution
		results[name] = value
	}
	// Ancestor bindings must be present with identical execution references, not
	// merely byte-equal payloads. Read metadata only, never ancestor payload graphs.
	seen := map[string]bool{}
	for _, value := range results {
		if value.Inputs == "" || seen[value.Inputs] {
			continue
		}
		seen[value.Inputs] = true
		ancestor, err := client.ReadCIInputSet(ctx, repository, value.Inputs, scope)
		if err != nil {
			return nil, err
		}
		for name, ref := range ancestor.Bindings {
			if set.Bindings[name] != ref {
				return nil, fmt.Errorf("incomplete or conflicting input lineage for %q", name)
			}
		}
	}
	return results, nil
}

func ciInputDirectory(options runner.Options) (string, error) {
	parent := options.CacheDir
	if parent == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		parent = filepath.Join(base, "stoke", "ci-inputs")
	}
	parent, err := filepath.Abs(parent)
	if err != nil {
		return "", err
	}
	contextDir, err := filepath.Abs(options.ContextDir)
	if err != nil {
		return "", err
	}
	// Resolve existing ancestors before creating anything: a cache symlink into
	// the checkout must not turn downloaded results into new workflow inputs.
	physical := func(path string) (string, error) {
		var missing []string
		for {
			_, err := os.Lstat(path)
			if err == nil {
				resolved, err := filepath.EvalSymlinks(path)
				if err != nil {
					return "", err
				}
				for i := len(missing) - 1; i >= 0; i-- {
					resolved = filepath.Join(resolved, missing[i])
				}
				return resolved, nil
			}
			if !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
			missing = append(missing, filepath.Base(path))
			next := filepath.Dir(path)
			if next == path {
				return "", err
			}
			path = next
		}
	}
	parent, err = physical(parent)
	if err != nil {
		return "", err
	}
	contextDir, err = physical(contextDir)
	if err != nil {
		return "", err
	}
	within := func(a, b string) bool {
		rel, err := filepath.Rel(a, b)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if within(parent, contextDir) || within(contextDir, parent) {
		return "", errors.New("CI input staging and context overlap; choose cache storage outside the checkout")
	}
	if err := os.MkdirAll(parent, 0700); err != nil {
		return "", err
	}
	return os.MkdirTemp(parent, "inputs-")
}

func runCIExchange(ctx context.Context, w *workflow.Workflow, p *ci.Plan, selected ci.Job, options runner.Options, client artifact.Client, exchange ciExchangeOptions) (_ *ciRunReport, retErr error) {
	if err := validateCIHostAuthority(p, selected, options); err != nil {
		return nil, err
	}
	var scope artifact.CIScope
	if err := decodeCIJSON(exchange.Scope, &scope, 8192); err != nil {
		return nil, fmt.Errorf("invocation --scope setup metadata is unavailable or invalid; rerun the whole generated invocation: %w", err)
	}
	scopeHash, err := scope.Hash()
	if err != nil || scope.Plan != p.ID || exchange.Attempt < 1 {
		return nil, errors.Join(errors.New("CI scope/plan/attempt mismatch"), err)
	}
	if exchange.Repository == "" {
		exchange.Repository = p.Config.ResultsRepository
	}
	if len(selected.Imports)+len(selected.Exports) != 0 && exchange.Repository == "" {
		return nil, errors.New("cross-job data requires --results-repository; configure an authorized registry or explicitly generate --co-locate TASK")
	}
	if exchange.Repository != "" {
		if len(exchange.Repository) > 512 {
			return nil, errors.New("result repository exceeds 512 bytes")
		}
		if err := artifact.ValidateCIRepository(exchange.Repository); err != nil {
			return nil, err
		}
	}
	refs, err := readCINativeInputs(p, selected, exchange, scopeHash)
	if err != nil {
		return nil, err
	}
	if len(refs) != 0 && exchange.Repository == "" {
		return nil, errors.New("native result bindings require an explicit result repository")
	}
	inputRef := ""
	inputs := artifact.CIInputSet{Version: artifact.CIVersion, Scope: scope, Bindings: map[string]string{}}
	if len(refs) != 0 {
		sets := make([]artifact.CIInputSet, 0, len(refs))
		for _, ref := range refs {
			set, err := client.ReadCIInputSet(ctx, exchange.Repository, ref, scope)
			if err != nil {
				return nil, fmt.Errorf("exact result bindings: %w", err)
			}
			sets = append(sets, set)
		}
		inputs.Bindings, err = artifact.MergeCIBindings(sets...)
		if err != nil {
			return nil, err
		}
	}
	specs, err := ciProducerSpecs(w, p)
	if err != nil {
		return nil, err
	}
	results, err := validateCIBindings(ctx, client, exchange.Repository, scope, exchange.Attempt, inputs, specs)
	if err != nil {
		return nil, fmt.Errorf("exact result provenance: %w", err)
	}
	for _, name := range selected.Exports {
		if _, exists := inputs.Bindings[name]; exists {
			return nil, fmt.Errorf("input bindings already contain this unit's output %q", name)
		}
	}
	for _, name := range selected.Imports {
		if _, exists := results[name]; !exists {
			return nil, fmt.Errorf("exact result for %q is missing; rerun producer and dependent gates", name)
		}
	}
	// Validate producer identity, attempts, platform and lineage before a join
	// may write metadata. Read-only joins reuse an existing identical InputSet.
	if len(refs) != 0 {
		inputRef, _, err = client.MergeCIInputSets(ctx, exchange.Repository, scope, refs)
		if err != nil {
			return nil, err
		}
	}
	request := runner.CIRequest{Targets: p.Config.Targets, Execute: selected.Execute, RequiredOutputs: append(slices.Clone(selected.Exports), selected.Data...), Environments: p.Environments, Imports: map[string]runner.CIImport{}}
	if len(selected.Imports) != 0 {
		// Publication copies the original canonical layout. Execution inputs
		// instead use Coopr's normal pinned image-source selection.
		fetch := client.ResolveCIInput
		expanded, err := w.Expand(p.Config.Targets)
		if err != nil {
			return nil, err
		}
		for _, group := range expanded.Groups {
			for _, job := range group.Jobs {
				if job.Definition.Publish != nil && slices.Contains(selected.Execute, job.Name) {
					fetch = client.FetchCIPayload
				}
			}
		}
		directory, err := ciInputDirectory(options)
		if err != nil {
			return nil, err
		}
		defer func() {
			_ = cleanupExecutionInputs(retErr, func() error { return os.RemoveAll(directory) })
		}()
		for i, name := range selected.Imports {
			ref := inputs.Bindings[name]
			value, exists := results[name]
			if ref == "" || !exists {
				return nil, fmt.Errorf("exact result for %q is missing; rerun producer and dependent gates", name)
			}
			payload, err := fetch(ctx, exchange.Repository, ref, artifact.CIResultExpectation{Scope: scope, Producer: name, MaxAttempt: exchange.Attempt, Kind: value.Kind, Platform: value.Platform}, filepath.Join(directory, fmt.Sprintf("payload-%d", i)))
			if err != nil {
				return nil, fmt.Errorf("exact result %q unavailable; no producer replay: restore retained content or rerun producer and dependent gates: %w", name, err)
			}
			request.Imports[name] = runner.CIImport{Kind: value.Kind, OCI: payload.Path, Digest: value.Output.Digest.String(), Platform: value.Platform, CanonicalReference: payload.CanonicalReference}
			if value.Archive != nil {
				input := request.Imports[name]
				input.ArchiveDigest = value.Archive.Digest.String()
				request.Imports[name] = input
			}
		}
	}
	metadata := &ciNativeMetadata{Version: 1, Scope: scopeHash, Unit: selected.ID, Attempt: exchange.Attempt, Inputs: inputRef}
	emit := func() error {
		data, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		if len(data) > ci.MaxNativeMetadataBytes {
			return errors.New("native result metadata exceeds its bounded output size")
		}
		return writeGitHubOutput(selected.OutputKey, string(data), exchange.GitHubOutput)
	}
	if len(selected.Exports) == 0 {
		if err := emit(); err != nil {
			return nil, err
		}
	}
	report, runErr := runner.RunCI(ctx, w, request, options)
	result := &ciRunReport{Report: report, CI: metadata}
	if runErr != nil || report == nil {
		return result, runErr
	}
	execution, err := artifact.NewCIExecutionID()
	if err != nil {
		return result, err
	}
	bindings := map[string]string{}
	for name, ref := range inputs.Bindings {
		bindings[name] = ref
	}
	exported := map[string]bool{}
	for _, item := range report.Tasks {
		if !slices.Contains(selected.Exports, item.Name) {
			continue
		}
		if item.Status != "passed" || item.OCI == "" || item.Digest == "" {
			return result, fmt.Errorf("producer %q has no complete exported OCI output", item.Name)
		}
		value := artifact.CIResult{Version: artifact.CIVersion, Scope: scope, Producer: item.Name, Attempt: exchange.Attempt, Execution: execution, Kind: item.Kind, Platform: item.Platform, Inputs: metadata.Inputs}
		ref, err := client.PublishCIResult(ctx, exchange.Repository, value, item.OCI, item.Digest)
		if err != nil {
			return result, fmt.Errorf("publish exact result %q: %w", item.Name, err)
		}
		bindings[item.Name] = ref
		exported[item.Name] = true
		metadata.Inputs, err = client.PublishCIInputSet(ctx, exchange.Repository, artifact.CIInputSet{Version: artifact.CIVersion, Scope: scope, Bindings: bindings})
		if err != nil {
			return result, err
		}
	}
	for _, name := range selected.Exports {
		if !exported[name] {
			return result, fmt.Errorf("producer %q was not present in execution results", name)
		}
	}
	if len(selected.Exports) != 0 {
		if err := emit(); err != nil {
			return result, err
		}
	}
	return result, nil
}

func persistCIRun(report *ciRunReport, runErr error) error {
	if report == nil || report.Report == nil {
		return runErr
	}
	if runErr != nil {
		report.Status, report.Error = "failed", runErr.Error()
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return errors.Join(runErr, err)
	}
	f, err := os.CreateTemp(report.Directory, ".ci-report-")
	if err != nil {
		return errors.Join(runErr, err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = io.Copy(f, bytes.NewReader(append(data, '\n')))
	err = errors.Join(err, f.Close())
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(report.Directory, "run.json"))
	}
	return errors.Join(runErr, err)
}
