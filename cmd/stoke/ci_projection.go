package main

import (
	"context"
	"errors"
	"os"

	"github.com/strio-dev/stoke/internal/artifact"
	"github.com/strio-dev/stoke/internal/ci"
	"github.com/strio-dev/stoke/internal/source"
)

func buildCIPlan(loaded *source.Result, config ci.Config) (*ci.Plan, error) {
	return ci.BuildWithSource(loaded.Workflow, loaded.RootWorkflow, config)
}

func loadCIExecution(ctx context.Context, p *ci.Plan, selected ci.Job, client artifact.Client) (*source.Result, bool, error) {
	_, sourceErr := os.Lstat(p.Config.Workflow)
	if errors.Is(sourceErr, os.ErrNotExist) && p.CanRunWithoutCheckout(selected) {
		// A checkout with a removed workflow is stale source, not an invitation
		// to bypass ordinary regeneration checks using the frozen projection.
		if _, err := os.Lstat(".git"); !errors.Is(err, os.ErrNotExist) {
			return nil, false, errors.New("checkout workflow is missing; regenerate and review the CI plan")
		}
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		workspace, err := os.Getwd()
		if err != nil {
			return nil, false, err
		}
		return &source.Result{Workflow: p.Execution, ContextDir: workspace}, true, nil
	}
	if err := validateCICheckout(p.Config, nil); err != nil {
		return nil, false, err
	}
	loaded, err := (source.Loader{Client: client, Inputs: p.Config.Inputs}).Load(ctx, p.Config.Workflow)
	return loaded, false, err
}
