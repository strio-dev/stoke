// Stoke runs portable YAML workflows through the Coopr execution API.
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
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/strio-dev/stoke/internal/artifact"
	"github.com/strio-dev/stoke/internal/assets"
	"github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/runner"
	"github.com/strio-dev/stoke/internal/source"
	"github.com/strio-dev/stoke/internal/workflow"
	"go.yaml.in/yaml/v3"
	"golang.org/x/sys/unix"
)

var version = "dev"

const help = `Stoke — portable workflows

Usage: stoke <command> [options] [targets...]

Commands:
  check     Validate YAML and resolve shared workflow imports
  plan      Show selected tasks in dependency order
  run       Run workflow tasks locally or inside an existing CI job
  ci        Generate workflows for existing CI and integrate with providers
  registry  Manage OCI registry authentication
  publish   Publish a workflow and Bale dependency metadata to OCI
  pull      Download an OCI workflow as YAML
  version   Print the version
  help      Show this help

Use stoke <command> --help for options, or stoke help run. Options precede targets.
Workflow defaults to stoke.yaml. Container execution requires Linux and rootless
runtime setup. Host execution requires --allow-host and provisioned tools.`

const runHelp = `Usage: stoke run [options] [targets...]

Example
  stoke run --output ../stoke-test-results test
  stoke run --ci test

Options precede targets. With no targets, Stoke selects all root tasks,
including publication tasks, unless default-run selects a task group.
Use a fresh directory outside the input context for --output. Local file exports update matching files and merge directories
in the workspace. --output leaves those destinations in place.

Registry credentials use --authfile, then REGISTRY_AUTH_FILE, then an existing
$DOCKER_CONFIG/config.json, then native defaults. Keep credentials outside the
workflow. Registry cache reads and writes each use CLI flags, then
STOKE_CACHE_FROM or STOKE_CACHE_TO, then root YAML, then local-only defaults.
An empty flag or variable clears its direction.

--ci needs explicit targets. Image prerequisites outside the selection must
restore from cache. The caller supplies ordering and success gates.

Options:`

const nativeTLSHelp = "verify TLS for Coopr image, component and cache registries (false for isolated tests only)"

func run(args []string, out, errOut io.Writer) int {
	return runContext(context.Background(), args, out, errOut)
}

func runContext(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h")) {
		if _, err := fmt.Fprintln(out, help); err != nil {
			return 1
		}
		return 0
	}
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		if _, err := fmt.Fprintln(out, "stoke", version); err != nil {
			return 1
		}
		return 0
	}
	if len(args) == 2 && args[0] == "help" && (args[1] == "run" || args[1] == "registry") {
		args = []string{args[1], "--help"}
	}
	command := args[0]
	if command == "ci" {
		return runCI(ctx, args[1:], out, errOut)
	}
	if command == "registry" {
		return runRegistry(ctx, args[1:], out, errOut)
	}
	switch command {
	case "check", "plan", "run", "publish", "pull":
	default:
		_, _ = fmt.Fprintf(errOut, "stoke: unknown command %q; use stoke help\n", command)
		return 2
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(errOut)
	client := artifact.Client{}
	httpScope := "OCI workflow artifacts and imports"
	if command == "run" {
		httpScope = "OCI workflows and host-file caches"
	}
	fs.BoolVar(&client.PlainHTTP, "plain-http", false, "use HTTP for "+httpScope+" (isolated tests only)")
	fs.StringVar(&client.AuthFile, "authfile", "", "registry credential file (overrides REGISTRY_AUTH_FILE and DOCKER_CONFIG)")
	location := "stoke.yaml"
	if command != "pull" {
		fs.StringVar(&location, "f", location, "workflow YAML path or oci://reference")
	}
	var options runner.Options
	var eventsPath string
	var ciMode bool
	inputs := map[string]string{}
	if command == "check" || command == "plan" || command == "run" {
		fs.Func("input", "workflow input NAME=VALUE (repeat for multiple inputs)", func(argument string) error {
			name, value, ok := strings.Cut(argument, "=")
			if !ok || name == "" {
				return errors.New("input requires NAME=VALUE")
			}
			if _, exists := inputs[name]; exists {
				return fmt.Errorf("duplicate input %q", name)
			}
			inputs[name] = value
			return nil
		})
	}
	output := ""
	directory := ""
	tlsVerify := true
	if command == "check" || command == "run" {
		fs.StringVar(&options.ContextDir, "context", "", "input directory (default: workflow's directory; cwd for OCI)")
	}
	if command == "pull" {
		fs.StringVar(&output, "output", "", "write YAML to a fresh file instead of stdout")
		fs.StringVar(&directory, "directory", "", "write YAML and packaged files to a fresh directory")
	}
	if command == "run" {
		fs.Usage = func() {
			_, _ = fmt.Fprintln(fs.Output(), runHelp)
			fs.PrintDefaults()
		}
		fs.StringVar(&eventsPath, "events", "", "write CDEvents JSONL to a fresh private file outside the input context")
		fs.BoolVar(&ciMode, "ci", false, "require cache hits for image prerequisites outside the selected tasks")
		hostExecutionFlags(fs, &options)
		cacheFlags(fs, &options)
		fs.StringVar(&options.CooprBinary, "coopr-binary", os.Getenv("STOKE_COOPR_BINARY"), "Coopr executable supporting API v1 (default: coopr on PATH)")
		fs.StringVar(&options.OutputDir, "output", "", "fresh output directory outside the input context")
		fs.StringVar(&options.CacheDir, "cache-dir", "", "result cache and disposable mount volumes (default: user cache/stoke)")
		fs.BoolVar(&options.NoCache, "no-cache", false, "execute every task without result cache reuse")
		fs.StringVar(&options.Runtime, "runtime", "crun", "OCI runtime name or path")
		fs.StringVar(&options.Network, "network", "", "container RUN network default; explicit native RUN choices still apply")
		fs.StringVar(&options.StorageDriver, "storage-driver", "vfs", "containers/storage driver")
		fs.StringVar(&options.SignaturePolicy, "signature-policy", "", "containers/image signature policy file")
		fs.BoolVar(&tlsVerify, "tls-verify", true, nativeTLSHelp)
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
	var executionErr error
	var loaded *source.Result
	var runReport *runner.Report
	var eventFile *cdeventsFile
	var runTargets []string
	switch command {
	case "pull":
		if fs.NArg() != 1 {
			_, _ = fmt.Fprintln(errOut, "stoke pull requires one OCI reference")
			return 2
		}
		if output != "" && directory != "" {
			_, _ = fmt.Fprintln(errOut, "stoke pull: --output and --directory are mutually exclusive")
			return 2
		}
		err = pull(ctx, client, fs.Arg(0), output, directory, out)
	case "publish":
		if fs.NArg() != 1 {
			_, _ = fmt.Fprintln(errOut, "stoke publish requires one destination OCI reference")
			return 2
		}
		if strings.HasPrefix(location, "oci://") {
			_, _ = fmt.Fprintln(errOut, "stoke publish requires a local YAML file")
			return 2
		}
		var data []byte
		data, err = workflow.ReadFile(ctx, location)
		if err == nil {
			var w *workflow.Workflow
			w, err = workflow.Decode(bytes.NewReader(data))
			if err == nil {
				err = w.ValidatePortableImports()
			}
			if err == nil {
				var files map[string]assets.File
				var absolute string
				absolute, err = filepath.Abs(location)
				if err == nil {
					files, err = assets.Collect(filepath.Dir(absolute), w.Assets)
				}
				var ref string
				if err == nil {
					ref, err = client.Push(ctx, fs.Arg(0), data, source.Dependencies(w), files)
				}
				if err == nil {
					_, err = fmt.Fprintln(out, ref)
				}
			}
		}
	default:
		if command == "check" && fs.NArg() != 0 {
			_, _ = fmt.Fprintln(errOut, "stoke check does not accept targets")
			return 2
		}
		loaded, err = (source.Loader{Client: client, Inputs: inputs}).Load(ctx, location)
		if err != nil {
			break
		}
		defer func() { _ = cleanupExecutionInputs(executionErr, loaded.Close) }()
		if command == "run" {
			if err = cacheEnvironment(&options); err != nil {
				break
			}
		}
		if command == "check" || command == "run" {
			if options.ContextDir == "" {
				options.ContextDir = loaded.ContextDir
			}
			if err = loaded.ValidateContext(options.ContextDir); err != nil {
				break
			}
		}
		targets := fs.Args()
		if ciMode && len(targets) == 0 {
			err = errors.New("--ci requires at least one explicit task")
			break
		}
		if len(targets) == 0 {
			if command == "run" && loaded.RootWorkflow.DefaultRun != "" {
				targets = []string{loaded.RootWorkflow.DefaultRun}
			} else {
				targets = loaded.Targets
			}
		}
		targets = loaded.RootWorkflow.ExpandGroups(targets)
		var plan *workflow.ExecutionPlan
		if command != "run" {
			plan, err = loaded.Workflow.Expand(targets)
			if err != nil {
				break
			}
		}
		switch command {
		case "check":
			for _, group := range plan.Groups {
				for _, job := range group.Jobs {
					if _, err = job.Definition.ResolvedPlatform(); err != nil {
						break
					}
					if err = runner.ValidateMountPaths(job.Definition, options.ContextDir); err != nil {
						break
					}
					if job.Definition.Coopr != nil || job.Definition.Environment != nil {
						_, err = source.DefinitionFilesForTask(job.Definition, options.ContextDir)
						if err != nil {
							break
						}
					}
				}
				if err != nil {
					break
				}
			}
			if err == nil {
				_, err = fmt.Fprintf(out, "Valid workflow: %s (%d selected jobs)\n", loaded.Reference, jobCount(plan))
			}
		case "plan":
			if _, err = fmt.Fprintln(out, "needs: explicit success gates; data-from: consumed outputs (must also succeed)"); err != nil {
				break
			}
			for _, group := range plan.Groups {
				for _, job := range group.Jobs {
					data := job.Definition
					data.Needs = nil
					line := fmt.Sprintf("%s\tcache=%t\tneeds=%s\tdata-from=%s", job.Name, job.Definition.CacheEnabled(), strings.Join(job.Definition.Needs, ","), strings.Join(data.Dependencies(), ","))
					platform, platformErr := job.Definition.ResolvedPlatform()
					if platformErr != nil {
						err = platformErr
						break
					}
					if platform != "" {
						line += "\tplatform=" + platform
					}
					if job.Definition.Host != nil {
						line += fmt.Sprintf("\tkind=host\tauthority=caller-opt-in\ttools=%s\tcapabilities=%s", strings.Join(job.Definition.Host.Tools, ","), strings.Join(job.Definition.Host.Capabilities, ","))
					} else if job.Definition.Coopr != nil {
						line += fmt.Sprintf("\tkind=coopr-%s\tfiles=%s", job.Definition.OutputKind(), strings.Join(job.Definition.Coopr.Files, ","))
					} else if job.Definition.Environment != nil {
						line += fmt.Sprintf("\tkind=build\tcoopr=%s\ttarget=%s", strings.Join(job.Definition.Environment.Files, ","), job.Definition.Environment.Target)
						if job.Definition.Environment.Context != "" {
							line += "\tcontext=" + job.Definition.Environment.Context
						}
					} else if job.Definition.Publish != nil {
						line += fmt.Sprintf("\tkind=publish\tdestinations=%s", strings.Join(job.Definition.Publish.Destinations, ","))
					}
					if mounts := job.Definition.AllMounts(); len(mounts) != 0 {
						line += fmt.Sprintf("\tmounts=%d", len(mounts))
					}
					if job.Matrix != nil {
						values, encodeErr := json.Marshal(job.Matrix)
						if encodeErr != nil {
							err = encodeErr
							break
						}
						line += fmt.Sprintf("\ttask=%s\tmatrix=%s", job.Task, values)
					}
					if _, err = fmt.Fprintln(out, line); err != nil {
						break
					}
				}
				if err != nil {
					break
				}
			}

		case "run":
			runTargets = targets
			options.AuthFile = client.AuthFile
			options.Stdout, options.Stderr = errOut, errOut
			options.EventName = location
			options.Events, eventFile, err = openCDEvents(eventsPath, options.ContextDir)
			if err != nil {
				break
			}
			if eventFile != nil {
				options.EventPath = eventFile.name
			}
			if ciMode {
				var request runner.CIRequest
				request, err = runner.SelectCI(loaded.Workflow, targets)
				if err == nil {
					attempt := 1
					// This is observability metadata only. --ci selects the execution mode.
					if value := os.Getenv("GITHUB_RUN_ATTEMPT"); options.Events != nil && value != "" {
						attempt, _ = strconv.Atoi(value)
					}
					err = options.Events.ConfigureCI(strings.Join(targets, "+"), attempt)
				}
				if err == nil {
					runReport, err = runner.RunCI(ctx, loaded.Workflow, request, options)
				}
			} else {
				runReport, err = runner.Run(ctx, loaded.Workflow, targets, options)
			}
			executionErr = err
		}
	}
	if loaded != nil {
		err = errors.Join(err, cleanupExecutionInputs(executionErr, loaded.Close))
	}
	if command == "run" {
		if eventFile != nil {
			if eventFile.file != nil {
				err = options.Events.Finish(runReport, err)
			}
			err = errors.Join(err, eventFile.Close())
		}
		if runReport != nil {
			if err != nil {
				err = runner.FinalizeReport(runReport, err)
			}
			err = errors.Join(err, json.NewEncoder(out).Encode(runReport))
		}
	}
	if err != nil {
		if command == "run" {
			printFailureSummary(errOut, runReport, runTargets, options, ciMode)
		}
		_, _ = fmt.Fprintln(errOut, "stoke:", err)
		return 1
	}
	return 0
}

func jobCount(plan *workflow.ExecutionPlan) int {
	count := 0
	for _, group := range plan.Groups {
		count += len(group.Jobs)
	}
	return count
}

func pull(ctx context.Context, client artifact.Client, reference, output, directory string, out io.Writer) error {
	doc, err := client.Pull(ctx, reference)
	if err != nil {
		return err
	}
	w, err := workflow.Decode(bytes.NewReader(doc.Data))
	if err != nil {
		return err
	}
	if err := source.ValidateAssets(w, doc.Files); err != nil {
		return err
	}
	// Apply direct destination substitutions before writing a local definition,
	// which otherwise loses its registry referrer context.
	source.RewriteReferences(w, doc.Dependencies)
	data, err := yaml.Marshal(w)
	if err != nil {
		return err
	}
	if directory != "" {
		return pullDirectory(directory, data, doc.Files)
	}
	if len(doc.Files) != 0 {
		return errors.New("workflow contains packaged files; use stoke pull --directory DIR")
	}
	if output == "" {
		_, err = out.Write(data)
		return err
	}
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	return errors.Join(writeErr, f.Close())
}

func pullDirectory(destination string, data []byte, files map[string]assets.File) (retErr error) {
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(absolute); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return errors.New("pull directory already exists")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return err
	}
	absolute = filepath.Join(parent, filepath.Base(absolute))
	staging, err := os.MkdirTemp(parent, ".stoke-pull-")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(staging)) }()
	if err := assets.Materialize(staging, files); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(staging, "stoke.yaml"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	return unix.Renameat2(unix.AT_FDCWD, staging, unix.AT_FDCWD, absolute, unix.RENAME_NOREPLACE)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runContext(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// Execution inputs can still be referenced by an engine whose cleanup was not
// confirmed. Retain their files on that typed execution failure; validation,
// planning and ordinary success/failure retain their existing cleanup behavior.
func cleanupExecutionInputs(executionErr error, cleanup func() error) error {
	if errors.Is(executionErr, engine.ErrCleanupFailed) {
		return nil
	}
	return cleanup()
}
