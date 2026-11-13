package workflow

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"go.yaml.in/yaml/v3"
	"oras.land/oras-go/v2/registry"
)

// CooprBuild delegates parsing and execution of caller-context definition files.
type CooprBuild struct {
	Files     []string            `yaml:"files"`
	FilesFrom string              `yaml:"files-from,omitempty"`
	Kind      string              `yaml:"kind,omitempty"`
	Target    string              `yaml:"target,omitempty"`
	Args      map[string]Argument `yaml:"args,omitempty"`
}

// Argument is a literal value or an explicit task-output/matrix binding.
type Argument struct {
	Value  *string
	From   string
	Matrix string
	Ref    string
	Input  string
}

func (a *Argument) UnmarshalYAML(node *yaml.Node) error {
	*a = Argument{}
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		value := node.Value
		a.Value = &value
		return nil
	}
	if node.Kind != yaml.MappingNode || len(node.Content) != 2 {
		return errors.New("argument must be a string or exactly one from/matrix/ref/input binding")
	}
	key, value := node.Content[0], node.Content[1]
	if key.Tag != "!!str" || value.Kind != yaml.ScalarNode || value.Tag != "!!str" || value.Value == "" {
		return errors.New("argument binding requires a nonempty string")
	}
	switch key.Value {
	case "from":
		a.From = value.Value
	case "matrix":
		a.Matrix = value.Value
	case "ref":
		a.Ref = value.Value
	case "input":
		a.Input = value.Value
	default:
		return fmt.Errorf("unknown argument binding %q", key.Value)
	}
	return nil
}

func (a Argument) MarshalYAML() (any, error) {
	if err := a.validate(); err != nil {
		return nil, err
	}
	if a.Value != nil {
		return *a.Value, nil
	}
	if a.Input != "" {
		return map[string]string{"input": a.Input}, nil
	}
	if a.From != "" {
		return map[string]string{"from": a.From}, nil
	}
	if a.Ref != "" {
		return map[string]string{"ref": a.Ref}, nil
	}
	return map[string]string{"matrix": a.Matrix}, nil
}

func (a Argument) validate() error {
	count := 0
	for _, selected := range []bool{a.Value != nil, a.From != "", a.Matrix != "", a.Ref != "", a.Input != ""} {
		if selected {
			count++
		}
	}
	if count != 1 {
		return errors.New("argument requires exactly one literal, from, matrix, ref or input value")
	}
	if a.Value != nil && !validInputString(*a.Value) {
		return errors.New("argument literal must be UTF-8 without NUL")
	}
	if a.Input != "" && !namePattern.MatchString(a.Input) {
		return fmt.Errorf("invalid input name %q", a.Input)
	}
	if a.Ref != "" {
		host, _, qualified := strings.Cut(a.Ref, "/")
		if !qualified || strings.HasPrefix(a.Ref, ".") || (!strings.ContainsAny(host, ".:") && host != "localhost") || strings.Contains(a.Ref, "://") || !importPattern.MatchString(a.Ref) {
			return errors.New("argument ref requires an explicit registry and an @sha256 digest")
		}
		if _, err := registry.ParseReference(a.Ref); err != nil {
			return fmt.Errorf("argument registry ref: %w", err)
		}
	}
	return nil
}

// Outputs controls flattened filesystem export; every image build also exports OCI.
type Outputs struct {
	Filesystem *bool `yaml:"filesystem,omitempty"`
}

// FileExport snapshots one file or directory at its step's position into the
// caller workspace. Imported templates retain the caller destination.
type FileExport struct {
	Source      string `yaml:"source"`
	Destination string `yaml:"destination"`
}

// FileExports collects explicit export operations in their authored order.
func (t Task) FileExports() []FileExport {
	var exports []FileExport
	for _, step := range t.Steps {
		if step.Export != nil {
			exports = append(exports, *step.Export)
		}
	}
	return exports
}

func (t Task) validateFileExports() error {
	exports := t.FileExports()
	for i, file := range exports {
		if !validInputString(file.Source) || !path.IsAbs(file.Source) || path.Clean(file.Source) != file.Source || file.Source == "/" || strings.ContainsAny(file.Source, "\\\r\n*?[") {
			return fmt.Errorf("export source %q must be a canonical absolute file or directory path without globs", file.Source)
		}
		if !hostPath(file.Destination, false) || strings.ContainsAny(file.Destination, "*?[") {
			return fmt.Errorf("export destination %q must be a canonical relative workspace path without globs", file.Destination)
		}
		first, _, _ := strings.Cut(file.Destination, "/")
		if first == ".git" || first == ".stoke" || first == "stoke.yaml" {
			return fmt.Errorf("export destination %q is reserved", file.Destination)
		}
		for _, prior := range exports[:i] {
			if hostPathOverlap(prior.Destination, file.Destination) {
				return errors.New("export destinations must not overlap or repeat")
			}
		}
	}
	return nil
}

// Publish transfers a completed task's artifact after all prerequisite gates.
type Publish struct {
	From         string   `yaml:"from"`
	Destinations []string `yaml:"destinations"`
}

// OutputKind describes the artifact produced by this task, if known. Templates
// are checked after expansion, while publication does not produce a build value.
func (t Task) OutputKind() string {
	if t.Publish != nil || t.Uses != "" {
		return ""
	}
	if t.Coopr != nil && t.Coopr.Kind == "component" {
		return "component"
	}
	if t.Host != nil {
		return "host"
	}
	return "image"
}

func (t Task) FilesystemEnabled() bool {
	if t.Host != nil {
		return true
	}
	if t.OutputKind() != "image" {
		return false
	}
	if t.Outputs != nil && t.Outputs.Filesystem != nil {
		return *t.Outputs.Filesystem
	}
	return len(t.FileExports()) == 0
}

func (t Task) validateNative() error {
	if err := t.validateFileExports(); err != nil {
		return err
	}
	if t.Environment != nil {
		if t.Environment.Kind != "" && t.Environment.Kind != "image" {
			return errors.New("environment must build an image, not a component")
		}
		buildTask := Task{Coopr: t.Environment, Strategy: t.Strategy}
		if err := buildTask.validateNative(); err != nil {
			return fmt.Errorf("environment: %w", err)
		}
	}
	if t.Coopr != nil {
		if len(t.Env) > 0 || t.Workdir != "" || len(t.Steps) > 0 {
			return errors.New("coopr cannot be combined with env, workdir or steps; use a separate from task")
		}
		build := t.Coopr
		if build.FilesFrom != "" && build.FilesFrom != "context" && build.FilesFrom != "component" {
			return errors.New("coopr files-from must be context or component")
		}
		if build.Kind != "" && build.Kind != "image" && build.Kind != "component" {
			return errors.New("coopr kind must be image or component")
		}
		if len(build.Files) == 0 {
			return errors.New("coopr requires definition files")
		}
		seen := map[string]bool{}
		for _, file := range build.Files {
			clean := path.Clean(file)
			if file == "" || path.IsAbs(file) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsRune(file, 0) {
				return errors.New("coopr files must be nonempty relative paths inside the context")
			}
			if seen[clean] {
				return fmt.Errorf("duplicate coopr definition file %q", file)
			}
			seen[clean] = true
		}
		if strings.ContainsRune(build.Target, 0) {
			return errors.New("coopr target contains NUL")
		}
		for name, argument := range build.Args {
			if !envPattern.MatchString(name) {
				return fmt.Errorf("invalid coopr argument name %q", name)
			}
			if err := argument.validate(); err != nil {
				return fmt.Errorf("coopr argument %q: %w", name, err)
			}
			if argument.Matrix != "" && !t.hasMatrixKey(argument.Matrix) {
				return fmt.Errorf("coopr argument %q: unknown matrix key %q", name, argument.Matrix)
			}
		}
	}
	if t.Publish != nil {
		if len(t.Env) > 0 || t.Workdir != "" || len(t.Steps) > 0 || t.Cache != nil || t.Strategy != nil || t.Outputs != nil {
			return errors.New("publish cannot be combined with env, workdir, steps, cache, strategy or outputs")
		}
		if t.Publish.From == "" || len(t.Publish.Destinations) == 0 {
			return errors.New("publish requires from and nonempty destinations")
		}
		seen := map[string]bool{}
		for _, destination := range t.Publish.Destinations {
			if destination == "" || strings.TrimSpace(destination) != destination || strings.ContainsAny(destination, "\x00\n\r") || seen[destination] {
				return errors.New("publish destinations must be nonempty, distinct references")
			}
			seen[destination] = true
		}
	}
	if t.Uses == "" && t.OutputKind() == "component" && t.Outputs != nil && t.Outputs.Filesystem != nil && *t.Outputs.Filesystem {
		return errors.New("component tasks cannot export a filesystem")
	}
	return nil
}

func (t Task) hasMatrixKey(key string) bool {
	if t.Strategy == nil || t.Strategy.Matrix == nil {
		return false
	}
	if _, ok := t.Strategy.Matrix.Axes[key]; ok {
		return true
	}
	for _, entry := range t.Strategy.Matrix.Include {
		if _, ok := entry[key]; ok {
			return true
		}
	}
	return false
}

func (t Task) validateSources(tasks map[string]Task) error {
	check := func(name string, imageOnly bool) error {
		if name == "" || tasks[name].Uses != "" {
			return nil
		}
		kind := tasks[name].OutputKind()
		if kind == "" || kind == "host" || (imageOnly && kind != "image") {
			required := "image or component"
			if imageOnly {
				required = "image"
			}
			return fmt.Errorf("task %q does not produce the required %s", name, required)
		}
		return nil
	}
	if err := check(t.From, true); err != nil {
		return err
	}
	for _, step := range t.Steps {
		if step.Copy != nil {
			if step.Copy.From != "" && tasks[step.Copy.From].Host != nil {
				if len(tasks[step.Copy.From].Host.Outputs) == 0 {
					return fmt.Errorf("task %q does not declare captured host outputs", step.Copy.From)
				}
				continue // Captured host files become an explicit native context.
			}
			if err := check(step.Copy.From, true); err != nil {
				return err
			}
		}
	}
	if t.Host != nil {
		for _, input := range t.Host.Inputs {
			if input.From == "" || tasks[input.From].Uses != "" {
				continue
			}
			producer := tasks[input.From]
			if producer.OutputKind() != "host" || len(producer.Host.Outputs) == 0 {
				return fmt.Errorf("task %q does not declare captured host outputs", input.From)
			}
			captured := input.Path == "."
			for _, output := range producer.Host.Outputs {
				captured = captured || input.Path == output || strings.HasPrefix(input.Path, output+"/")
			}
			if !captured {
				return fmt.Errorf("host input %q is outside task %q's declared outputs", input.Path, input.From)
			}
		}
	}
	if t.Coopr != nil {
		for _, argument := range t.Coopr.Args {
			if err := check(argument.From, false); err != nil {
				return err
			}
		}
	}
	for _, mount := range t.AllMounts() {
		if err := check(mount.From, true); err != nil {
			return err
		}
	}
	if t.Environment != nil {
		for _, argument := range t.Environment.Args {
			if err := check(argument.From, false); err != nil {
				return err
			}
		}
	}
	if t.Publish != nil {
		return check(t.Publish.From, false)
	}
	return nil
}
