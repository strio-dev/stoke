package main

import (
	"context"
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
	"github.com/strio-dev/stoke/internal/runner"
	"github.com/strio-dev/stoke/internal/source"
)

const ciHelp = `Usage: stoke ci <command> [options]

Workflow authoring:
  plan            Print a resolved CI plan as JSON
  generate        Render standalone stage files from a CI template

Use stoke run --ci TASK to run a stage with cache-only image prerequisites.

The native caller owns checkout, runners, credentials and success gates.
Use generate [TEMPLATE [STAGE]] or generate [STAGE] --template-file FILE.
Use generate --check to compare the generated files without writing.
CI jobs require a provisioned runner and Stoke. Container jobs also require
a compatible Coopr executable. Host tasks require --allow-host and provisioned
tools. Use stoke ci <command> --help for options.`

func runCI(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 || (len(args) == 1 && slices.Contains([]string{"help", "--help", "-h"}, args[0])) {
		_, err := fmt.Fprintln(out, ciHelp)
		if err != nil {
			return 1
		}
		return 0
	}
	command := args[0]
	if !slices.Contains([]string{"plan", "generate"}, command) {
		_, _ = fmt.Fprintf(errOut, "stoke ci: unknown command %q\n", command)
		return 2
	}
	fs := flag.NewFlagSet("ci "+command, flag.ContinueOnError)
	fs.SetOutput(errOut)
	var config ci.Config
	var options runner.Options
	var client artifact.Client
	var templateFile string
	check := false
	if command == "plan" || command == "generate" {
		fs.StringVar(&config.Workflow, "f", "stoke.yaml", "local workflow path, relative to the repository root")
		fs.StringVar(&config.Context, "context", "", "checkout-relative input directory (default: workflow directory)")
		fs.Func("co-locate", "explicitly keep this data-connected component in one invocation (repeatable)", func(task string) error {
			config.CoLocate = append(config.CoLocate, task)
			return nil
		})
		config.Inputs = map[string]string{}
		fs.Func("input", "workflow input NAME=VALUE (explicit input supplied to plan or generation)", func(value string) error {
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
		fs.Usage = func() {
			_, _ = fmt.Fprintf(errOut, "Usage: stoke ci generate [TEMPLATE [STAGE]] [options]\n       stoke ci generate [STAGE] --template-file FILE [options]\n\nBuilt-ins: %s. Use oci://REGISTRY/REPOSITORY:TAG for a registry template.\nWith no selector, use ci-template and default-ci from the root workflow.\n", strings.Join(ci.BuiltinTemplates(), ", "))
			fs.PrintDefaults()
		}
		fs.StringVar(&templateFile, "template-file", "", "read the plain stage template from a local file")
		fs.BoolVar(&check, "check", false, "fail on drift without creating or writing files")
	}
	fs.StringVar(&options.Network, "network", "", "container RUN network default")
	cacheFlags(fs, &options)
	httpScope := "OCI workflow imports"
	if command == "generate" {
		httpScope = "OCI templates and workflow imports"
	}
	fs.StringVar(&client.AuthFile, "authfile", "", "registry credential file (overrides REGISTRY_AUTH_FILE and DOCKER_CONFIG; never generated)")
	fs.BoolVar(&client.PlainHTTP, "plain-http", false, "use HTTP for "+httpScope+" (isolated tests only)")
	// Generate supports the natural selector-first form with trailing
	// options. The other commands retain the standard flag ordering.
	parseArgs := args[1:]
	var selectors []string
	if command == "generate" {
		for len(parseArgs) != 0 && !strings.HasPrefix(parseArgs[0], "-") {
			selectors = append(selectors, parseArgs[0])
			parseArgs = parseArgs[1:]
		}
	}
	if err := fs.Parse(parseArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if command == "generate" {
		selectors = append(selectors, fs.Args()...)
	}
	var err error
	switch command {
	case "plan", "generate":
		if command == "plan" {
			config.Targets = fs.Args()
		}
		config.CacheFrom, config.CacheTo = options.CacheFrom, options.CacheTo
		config.Network = options.Network
		if config.Context == "" {
			config.Context = filepath.ToSlash(filepath.Dir(config.Workflow))
		}
		if err = validateCICheckout(config, nil); err != nil {
			break
		}
		loaded, loadErr := (source.Loader{Client: client, Inputs: config.Inputs}).Load(ctx, config.Workflow)
		if loadErr != nil {
			err = loadErr
			break
		}
		defer func() { _ = loaded.Close() }()
		root := loaded.RootWorkflow
		if len(config.Targets) == 0 {
			config.Targets = loaded.Targets
		}
		var selector, stage string
		var full bool
		if command == "generate" {
			selector, stage, err = selectStageTemplate(selectors, templateFile != "", root.CITemplate)
			if err != nil {
				break
			}
			if len(selectors) == 0 && templateFile == "" {
				stage = root.DefaultCI
			}
			full = stage == ""
			if members, isGroup := root.Groups[stage]; isGroup {
				config.Targets, stage = slices.Clone(members), ""
			} else if stage != "" {
				request, selectErr := runner.SelectCI(loaded.Workflow, []string{stage})
				if selectErr != nil {
					err = selectErr
					break
				}
				config.Targets = request.Targets
			}
		}
		config.Targets = root.ExpandGroups(config.Targets)
		if err = validateCICheckout(config, loaded); err != nil {
			break
		}
		var plan *ci.Plan
		plan, err = ci.Build(loaded.Workflow, config)
		if err != nil {
			break
		}
		if command == "generate" {
			err = generateStages(ctx, out, plan, selector, stage, templateFile, client, check, full)
			break
		}
		var data []byte
		data, err = ci.Encode(plan)
		if err == nil {
			_, err = out.Write(data)
		}

	}
	if err != nil {
		_, _ = fmt.Fprintln(errOut, "stoke ci:", err)
		return 1
	}
	return 0
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
