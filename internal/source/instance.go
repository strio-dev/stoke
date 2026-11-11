package source

import (
	"context"
	"fmt"
	"strings"

	"github.com/strio-dev/stoke/internal/artifact"
	"github.com/strio-dev/stoke/internal/workflow"
)

// A component retains authored bytes and immutable files, independently of its
// invocations. Each invocation binds a task copy and emits its own task graph.
type component struct {
	document  *artifact.Document
	workflow  *workflow.Workflow
	templates map[string]workflow.Task
	directory string
	imports   map[string]*component
}

func (c *component) validateCalls(ctx context.Context) error {
	w := c.workflow
	c.templates = map[string]workflow.Task{}
	for _, name := range sortedKeys(w.Tasks) {
		if err := ctx.Err(); err != nil {
			return err
		}
		task := w.Tasks[name]
		if task.Uses == "" {
			c.templates[name] = task
			continue
		}
		alias, target, _ := strings.Cut(task.Uses, "/")
		child := c.imports[alias].workflow
		if _, ok := child.Tasks[target]; !ok {
			return fmt.Errorf("task %q: imported template %q does not exist", name, task.Uses)
		}
		for _, input := range sortedKeys(task.With) {
			if _, ok := child.Inputs[input]; !ok {
				return fmt.Errorf("task %q: unknown input %q for %s", name, input, task.Uses)
			}
		}
		for _, input := range sortedKeys(child.Inputs) {
			if _, supplied := task.With[input]; !supplied && child.Inputs[input].Required {
				return fmt.Errorf("task %q: required input %q is missing for %s", name, input, task.Uses)
			}
		}
		c.templates[name] = mergeTemplate(c.imports[alias].templates[target].Clone(), task.Clone())
	}
	return w.ValidateResolvedTemplates(c.templates)
}

type instance struct {
	ctx      context.Context
	source   *component
	workflow *workflow.Workflow
	inputs   map[string]string
	prefix   string
	expanded *workflow.Workflow
	emitted  map[string]string
}

func newInstance(ctx context.Context, c *component, values map[string]string, prefix string, expanded *workflow.Workflow) (*instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	inputs, err := c.workflow.ResolveInputs(values)
	if err != nil {
		return nil, fmt.Errorf("component %q: %w", c.document.Reference, err)
	}
	return &instance{ctx: ctx, source: c, workflow: c.workflow, inputs: inputs, prefix: prefix, expanded: expanded, emitted: map[string]string{}}, nil
}

func (i *instance) emit(name string) (string, error) {
	if key, ok := i.emitted[name]; ok {
		return key, nil
	}
	key := i.prefix + name
	task, err := i.resolve(name)
	if err != nil {
		return "", fmt.Errorf("task %q: %w", key, err)
	}
	if _, exists := i.expanded.Tasks[key]; exists {
		return "", fmt.Errorf("expanded task name collision %q", key)
	}
	if len(i.expanded.Tasks) >= 1024 {
		return "", fmt.Errorf("expanded workflow exceeds 1024 tasks")
	}
	i.expanded.Tasks[key] = task
	i.emitted[name] = key
	return key, nil
}

func (i *instance) resolve(name string) (workflow.Task, error) {
	if err := i.ctx.Err(); err != nil {
		return workflow.Task{}, err
	}
	task, err := i.workflow.Tasks[name].BindInputs(i.inputs)
	if err != nil {
		return workflow.Task{}, err
	}
	task.ComponentDir = i.source.directory
	// Resolve edges through this invocation, including caller-added needs on a
	// nested use. Those edges belong to the caller, not the called component.
	for _, dependency := range task.Dependencies() {
		if _, err := i.emit(dependency); err != nil {
			return workflow.Task{}, err
		}
	}
	for n, dependency := range task.Needs {
		task.Needs[n] = i.emitted[dependency]
	}
	if task.Uses != "" {
		alias, target, _ := strings.Cut(task.Uses, "/")
		values := map[string]string{}
		for input, value := range task.With {
			values[input] = *value.Value
		}
		child, err := newInstance(i.ctx, i.source.imports[alias], values, i.prefix+name+"--"+alias+"--", i.expanded)
		if err != nil {
			return workflow.Task{}, err
		}
		template, err := child.resolve(target)
		if err != nil {
			return workflow.Task{}, err
		}
		return mergeTemplate(template, task), nil
	}
	if task.From != "" {
		task.From = i.emitted[task.From]
	}
	if task.Host != nil {
		for n := range task.Host.Inputs {
			if from := task.Host.Inputs[n].From; from != "" {
				task.Host.Inputs[n].From = i.emitted[from]
			}
		}
	}
	for n := range task.Steps {
		if copy := task.Steps[n].Copy; copy != nil && copy.From != "" {
			copy.From = i.emitted[copy.From]
		}
	}
	for _, build := range []*workflow.CooprBuild{task.Coopr, task.Environment} {
		if build == nil {
			continue
		}
		for name, argument := range build.Args {
			if argument.From != "" {
				argument.From = i.emitted[argument.From]
			}
			if argument.Ref != "" {
				argument.Ref = RelocatedReference(argument.Ref, i.source.document.Dependencies)
			}
			build.Args[name] = argument
		}
	}
	for _, mounts := range taskMountLists(task) {
		for n := range mounts {
			if mounts[n].From != "" {
				mounts[n].From = i.emitted[mounts[n].From]
			}
			mounts[n].Image = RelocatedReference(mounts[n].Image, i.source.document.Dependencies)
			mounts[n].Ref = RelocatedReference(mounts[n].Ref, i.source.document.Dependencies)
		}
	}
	if task.Publish != nil {
		task.Publish.From = i.emitted[task.Publish.From]
	}
	task.Image = RelocatedReference(task.Image, i.source.document.Dependencies)
	return task, nil
}

// Shape validation and actual instantiation apply the same allowed overrides
// to independent task copies, retaining the component's file owner/definition.
func mergeTemplate(template, caller workflow.Task) workflow.Task {
	template.Needs = append(template.Needs, caller.Needs...)
	if template.Env == nil {
		template.Env = map[string]workflow.Value{}
	}
	for name, value := range caller.Env {
		template.Env[name] = value
	}
	if caller.Workdir != "" {
		template.Workdir = caller.Workdir
	}
	if caller.Cache != nil {
		template.Cache = caller.Cache
	}
	if caller.Strategy != nil {
		template.Strategy = caller.Strategy
	}
	if caller.Outputs != nil {
		template.Outputs = caller.Outputs
	}
	return template
}
