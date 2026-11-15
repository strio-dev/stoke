package ci

import (
	"errors"
	"slices"
	"strings"

	"github.com/opencontainers/go-digest"
	"github.com/strio-dev/stoke/internal/workflow"
	"oras.land/oras-go/v2/registry"
)

// BuildWithSource permits a frozen execution only for tasks authored directly in
// the local root document. Resolved imported bodies have no retained provenance
// in Workflow and must never be copied into generated workflow files.
func BuildWithSource(w, root *workflow.Workflow, config Config) (*Plan, error) {
	return build(w, root, config)
}

func pinnedImage(reference string) bool {
	if reference == "scratch" {
		return true
	}
	r, err := registry.ParseReference(strings.TrimPrefix(reference, "docker://"))
	if err != nil || r.Registry == "" || r.Repository == "" {
		return false
	}
	identity, err := r.Digest()
	return err == nil && identity.Algorithm() == digest.SHA256
}

func artifactTask(task workflow.Task, definitions map[string]workflow.Task) bool {
	if task.Host != nil || task.Coopr != nil || task.Environment != nil || task.Uses != "" || task.ComponentDir != "" || len(task.Contexts) != 0 {
		return false
	}
	image := func(name string) bool {
		value, exists := definitions[name]
		return exists && value.OutputKind() == "image"
	}
	if task.Image != "" && !pinnedImage(task.Image) || task.From != "" && !image(task.From) {
		return false
	}
	if task.Publish != nil && !image(task.Publish.From) {
		return false
	}
	for _, step := range task.Steps {
		if step.Copy != nil && (step.Copy.From == "" || !image(step.Copy.From)) {
			return false
		}
	}
	for _, mount := range task.AllMounts() {
		switch mount.Type {
		case "cache":
		case "stage":
			if !image(mount.From) {
				return false
			}
		case "image":
			if !pinnedImage(mount.Image) {
				return false
			}
		case "artifact":
			if !pinnedImage(mount.Ref) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func freezeExecution(w, root *workflow.Workflow, p *Plan) {
	if root == nil || len(p.Host) != 0 || len(p.Environments) != 0 {
		return
	}
	expanded, err := w.Expand(p.Config.Targets)
	if err != nil {
		return // Build has already validated this expansion.
	}
	definitions, logical := map[string]workflow.Task{}, map[string]string{}
	for _, group := range expanded.Groups {
		for _, job := range group.Jobs {
			definitions[job.Name], logical[job.Name] = job.Definition, job.Task
			if job.Definition.Environment != nil {
				return // Preserve canonical native environment preparation.
			}
		}
	}
	bodies := map[string]bool{}
	for _, group := range p.Groups {
		for _, job := range group.Jobs {
			eligible := true
			for _, name := range job.Execute {
				authored, exists := root.Tasks[logical[name]]
				eligible = eligible && exists && authored.Uses == "" && artifactTask(definitions[name], definitions)
			}
			for _, name := range job.Imports {
				eligible = eligible && definitions[name].OutputKind() == "image"
			}
			if eligible {
				p.ArtifactOnly = append(p.ArtifactOnly, job.ID)
				for _, name := range job.Execute {
					bodies[logical[name]] = true
				}
			}
		}
	}
	if len(p.ArtifactOnly) == 0 {
		return
	}
	slices.Sort(p.ArtifactOnly)
	p.Execution = &workflow.Workflow{Version: 1, Tasks: map[string]workflow.Task{}}
	for name, task := range w.Tasks {
		copy := task.Clone()
		if bodies[name] {
			copy.ComponentDir = ""
		} else {
			// These ordinary Task values supply expected producer kind/platform
			// and graph identity. Their source instructions are not delivered.
			copy = workflow.Task{Image: "scratch", Needs: task.Dependencies(), Strategy: copy.Strategy, Platform: copy.Platform}
			if task.Coopr != nil {
				copy.Image = ""
				copy.Coopr = &workflow.CooprBuild{Files: []string{"retained.coopr"}, Kind: task.Coopr.Kind}
			}
			if task.Publish != nil {
				copy.Image = ""
				copy.Publish = &workflow.Publish{From: task.Publish.From, Destinations: []string{"registry.invalid/retained:metadata"}}
			}
		}
		p.Execution.Tasks[name] = copy
	}
}

func (p *Plan) CanRunWithoutCheckout(job Job) bool {
	return p.Execution != nil && slices.Contains(p.ArtifactOnly, job.ID)
}

func (p *Plan) validateExecution() error {
	if p.Execution == nil {
		if len(p.ArtifactOnly) != 0 {
			return errors.New("artifact-only jobs require their frozen workflow")
		}
		return nil
	}
	if len(p.ArtifactOnly) == 0 || !slices.Equal(p.ArtifactOnly, sortedUnique(p.ArtifactOnly)) || len(p.Host) != 0 || len(p.Environments) != 0 || len(p.Execution.Imports)+len(p.Execution.Inputs)+len(p.Execution.Assets) != 0 {
		return errors.New("invalid frozen CI execution")
	}
	expanded, err := p.Execution.Expand(p.Config.Targets)
	if err != nil {
		return err
	}
	definitions := map[string]workflow.Task{}
	for _, task := range p.Execution.Tasks {
		if task.ComponentDir != "" || task.Uses != "" || task.Environment != nil || task.Host != nil {
			return errors.New("frozen CI execution contains source-owned data")
		}
	}
	for _, group := range expanded.Groups {
		for _, job := range group.Jobs {
			definitions[job.Name] = job.Definition
		}
	}
	for _, id := range p.ArtifactOnly {
		job, exists := p.FindJob(id)
		if !exists {
			return errors.New("unknown artifact-only CI job")
		}
		for _, name := range job.Execute {
			task, exists := definitions[name]
			if !exists || !artifactTask(task, definitions) {
				return errors.New("artifact-only CI job requires caller source")
			}
		}
		for _, name := range job.Imports {
			if task, exists := definitions[name]; !exists || task.OutputKind() != "image" {
				return errors.New("artifact-only CI input requires an image producer")
			}
		}
	}
	return nil
}
