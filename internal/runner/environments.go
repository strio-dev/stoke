package runner

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/strio-dev/stoke/internal/source"
	"github.com/strio-dev/stoke/internal/workflow"
)

// SharedEnvironmentConsumers identifies concrete jobs which local execution
// gives the same inline environment producer. It resolves execution copies using
// the ordinary path rules, without creating runtime directories or starting Coopr.
func SharedEnvironmentConsumers(expanded *workflow.ExecutionPlan, contextDir string) ([][]string, error) {
	contextDir, err := filepath.Abs(contextDir)
	if err != nil {
		return nil, err
	}
	contextDir, err = mountDirectory("", contextDir)
	if err != nil {
		return nil, fmt.Errorf("environment context: %w", err)
	}
	plan := &workflow.ExecutionPlan{Groups: slices.Clone(expanded.Groups)}
	consumers := map[string]bool{}
	for gi := range plan.Groups {
		group := &plan.Groups[gi]
		group.Jobs = slices.Clone(group.Jobs)
		for ji := range group.Jobs {
			job := &group.Jobs[ji]
			definition := &job.Definition
			if definition.Environment == nil {
				continue
			}
			consumers[job.Name] = true
			environment := *definition.Environment
			definition.Environment = &environment
			definition.Mounts = slices.Clone(definition.Mounts)
			definition.Steps = slices.Clone(definition.Steps)
			for si := range definition.Steps {
				definition.Steps[si].Mounts = slices.Clone(definition.Steps[si].Mounts)
			}
			if err := resolveTaskMounts(definition, contextDir); err != nil {
				return nil, fmt.Errorf("task %q: %w", job.Name, err)
			}
			if err := source.ValidateComponentContext(definition.ComponentDir, contextDir); err != nil {
				return nil, fmt.Errorf("task %q: %w", job.Name, err)
			}
			environment.Files, err = source.DefinitionFilesForTask(*definition, contextDir)
			if err != nil {
				return nil, fmt.Errorf("task %q environment files: %w", job.Name, err)
			}
		}
	}
	lowered, err := lowerEnvironments(plan)
	if err != nil {
		return nil, err
	}
	byProducer := map[string][]string{}
	for _, group := range lowered.Groups {
		for _, job := range group.Jobs {
			if consumers[job.Name] {
				byProducer[job.Definition.From] = append(byProducer[job.Definition.From], job.Name)
			}
		}
	}
	var shared [][]string
	for _, names := range byProducer {
		if len(names) > 1 {
			slices.Sort(names)
			shared = append(shared, names)
		}
	}
	slices.SortFunc(shared, func(a, b []string) int { return slices.Compare(a, b) })
	return shared, nil
}

// Lower inline environments after matrix/input binding and file resolution.
// The resulting producer is an ordinary native image build; consumers inherit
// its stored image through the same path as an authored `coopr` + `from` pair.
func lowerEnvironments(plan *workflow.ExecutionPlan) (*workflow.ExecutionPlan, error) {
	output := &workflow.ExecutionPlan{}
	names := map[string]bool{}
	for _, group := range plan.Groups {
		names[group.Name] = true
		for _, job := range group.Jobs {
			names[job.Name] = true
		}
	}
	producers := map[string]string{}
	for _, original := range plan.Groups {
		group := original
		group.Jobs = slices.Clone(group.Jobs)
		for index := range group.Jobs {
			job := &group.Jobs[index]
			definition := job.Definition
			if definition.Environment == nil {
				continue
			}
			// Gates are part of producer identity. Combining gates from different
			// consumers can introduce cycles or run an environment too early.
			producer := workflow.Task{
				Coopr: definition.Environment, ComponentDir: definition.ComponentDir,
				Contexts: definition.Contexts, Cache: definition.Cache,
				Needs: definition.Dependencies(),
			}
			platform, err := definition.ResolvedPlatform()
			if err != nil {
				return nil, fmt.Errorf("environment platform: %w", err)
			}
			if platform != "" {
				producer.Platform = &workflow.Platform{Value: &platform}
			}
			filesystem := false
			producer.Outputs = &workflow.Outputs{Filesystem: &filesystem}
			key, err := json.Marshal(producer)
			if err != nil {
				return nil, fmt.Errorf("environment identity: %w", err)
			}
			name, exists := producers[string(key)]
			if !exists {
				name = fmt.Sprintf("stoke-environment-%x", sha256.Sum256(key))
				if names[name] {
					return nil, fmt.Errorf("environment producer collides with task %q", name)
				}
				names[name] = true
				producers[string(key)] = name
				output.Groups = append(output.Groups, workflow.Group{
					Name: name, MaxParallel: 1, FailFast: true,
					Jobs: []workflow.Job{{Name: name, Task: name, Definition: producer}},
				})
			}
			definition.Environment = nil
			definition.From = name
			job.Definition = definition
		}
		output.Groups = append(output.Groups, group)
	}
	return output, nil
}
