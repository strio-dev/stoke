// Package workflow defines and validates Stoke's version-one YAML workflow.
package workflow

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/strio-dev/stoke/internal/assets"
	"go.yaml.in/yaml/v3"
)

const MaxSize = 1 << 20
const ComponentContext = "stoke-assets"

// Workflow owns reusable imports and locally named tasks.
type Workflow struct {
	Version int               `yaml:"version"`
	Imports map[string]string `yaml:"imports,omitempty"`
	Inputs  map[string]Input  `yaml:"inputs,omitempty"`
	Assets  []string          `yaml:"assets,omitempty"`
	Tasks   map[string]Task   `yaml:"tasks"`
}

// Task selects a literal image program, native Coopr build, host program,
// publication or imported template. Prerequisites remain workflow edges.
type Task struct {
	Strategy    *Strategy         `yaml:"strategy,omitempty"`
	Image       string            `yaml:"image,omitempty"`
	From        string            `yaml:"from,omitempty"`
	Uses        string            `yaml:"uses,omitempty"`
	Needs       []string          `yaml:"needs,omitempty"`
	Env         map[string]Value  `yaml:"env,omitempty"`
	With        map[string]Value  `yaml:"with,omitempty"`
	Workdir     string            `yaml:"workdir,omitempty"`
	Cache       *bool             `yaml:"cache,omitempty"`
	Steps       []Step            `yaml:"steps,omitempty"`
	Coopr       *CooprBuild       `yaml:"coopr,omitempty"`
	Publish     *Publish          `yaml:"publish,omitempty"`
	Outputs     *Outputs          `yaml:"outputs,omitempty"`
	Environment *CooprBuild       `yaml:"environment,omitempty"`
	Contexts    map[string]string `yaml:"contexts,omitempty"`
	Mounts      []Mount           `yaml:"mounts,omitempty"`
	Platform    *Platform         `yaml:"platform,omitempty"`
	Host        *Host             `yaml:"host,omitempty" json:",omitempty"`
	// ComponentDir is the resolved, loader-owned tree of declared package files.
	ComponentDir string `yaml:"-"`
}

// Step contains exactly one operation. Run executes literal argv without a shell.
type Step struct {
	Run    []string `yaml:"run,omitempty"`
	Copy   *Copy    `yaml:"copy,omitempty"`
	Mounts []Mount  `yaml:"mounts,omitempty"`
	Stdout string   `yaml:"stdout,omitempty" json:",omitempty"`
}

// Copy reads from the host context or a named task's output.
type Copy struct {
	From        string   `yaml:"from,omitempty"`
	Scope       string   `yaml:"scope,omitempty"`
	Sources     []string `yaml:"sources"`
	Destination string   `yaml:"destination"`
}

var namePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var importPattern = regexp.MustCompile(`^[^\s@]+@sha256:[a-f0-9]{64}$`)

// Decode rejects unknown fields, aliases, merges, duplicate keys, multiple
// documents and inputs larger than MaxSize before semantic validation.
func Decode(r io.Reader) (*Workflow, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSize {
		return nil, errors.New("workflow exceeds 1 MiB")
	}
	var node yaml.Node
	d := yaml.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(&node); err != nil {
		return nil, fmt.Errorf("workflow YAML: %w", err)
	}
	var extra yaml.Node
	if err := d.Decode(&extra); err != io.EOF {
		return nil, errors.New("workflow must contain exactly one YAML document")
	}
	var inspect func(*yaml.Node) error
	inspect = func(n *yaml.Node) error {
		if n.Kind == yaml.AliasNode || n.Anchor != "" || n.Tag == "!!merge" {
			return errors.New("YAML anchors, aliases and merge keys are not supported")
		}
		for _, child := range n.Content {
			if err := inspect(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := validateInputNodes(&node); err != nil {
		return nil, err
	}
	if err := inspect(&node); err != nil {
		return nil, err
	}
	var w Workflow
	d = yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&w); err != nil {
		return nil, fmt.Errorf("workflow YAML: %w", err)
	}
	if err := w.Validate(); err != nil {
		return nil, err
	}
	return &w, nil
}

// Dependencies returns sorted explicit and implicit prerequisite task names.
func (t Task) Dependencies() []string {
	deps := slices.Clone(t.Needs)
	if t.Host != nil {
		for _, input := range t.Host.Inputs {
			if input.From != "" {
				deps = append(deps, input.From)
			}
		}
	}
	if t.From != "" {
		deps = append(deps, t.From)
	}
	for _, s := range t.Steps {
		if s.Copy != nil && s.Copy.From != "" {
			deps = append(deps, s.Copy.From)
		}
	}
	if t.Coopr != nil {
		for _, argument := range t.Coopr.Args {
			if argument.From != "" {
				deps = append(deps, argument.From)
			}
		}
	}
	for _, mount := range t.AllMounts() {
		if mount.From != "" {
			deps = append(deps, mount.From)
		}
	}
	if t.Environment != nil {
		for _, argument := range t.Environment.Args {
			if argument.From != "" {
				deps = append(deps, argument.From)
			}
		}
	}
	if t.Publish != nil && t.Publish.From != "" {
		deps = append(deps, t.Publish.From)
	}
	slices.Sort(deps)
	return slices.Compact(deps)
}

// CacheEnabled defaults to true for container builds. Host tasks require an
// explicit opt-in; publication always executes. Fresh tests disable caching.
func (t Task) CacheEnabled() bool {
	if t.Host != nil {
		return t.Cache != nil && *t.Cache
	}
	return t.Publish == nil && (t.Cache == nil || *t.Cache)
}

// Validate validates every task, including tasks outside a selected target closure.
func (w *Workflow) Validate() error {
	if w == nil {
		return errors.New("workflow is nil")
	}
	if w.Version != 1 {
		return errors.New("workflow version must be 1")
	}
	if err := w.validateInputs(); err != nil {
		return err
	}
	if len(w.Tasks) == 0 {
		return errors.New("workflow requires tasks")
	}
	declared := map[string]assets.File{}
	for _, name := range w.Assets {
		if _, exists := declared[name]; exists {
			return fmt.Errorf("duplicate asset %q", name)
		}
		declared[name] = assets.File{}
	}
	if err := assets.Validate(declared); err != nil {
		return fmt.Errorf("workflow assets: %w", err)
	}
	for alias, ref := range w.Imports {
		if !namePattern.MatchString(alias) {
			return fmt.Errorf("invalid import name %q", alias)
		}
		if !importPattern.MatchString(ref) || strings.Contains(ref, "://") {
			return fmt.Errorf("import %q requires an OCI reference pinned with @sha256", alias)
		}
	}
	for _, name := range taskNames(w.Tasks) {
		t := w.Tasks[name]
		if !namePattern.MatchString(name) {
			return fmt.Errorf("invalid task name %q", name)
		}
		fail := func(message string) error { return fmt.Errorf("task %q: %s", name, message) }
		if err := t.validateStrategy(); err != nil {
			return fail(err.Error())
		}
		if err := t.validatePlatform(); err != nil {
			return fail(err.Error())
		}
		if t.Platform != nil && t.Platform.Input != "" {
			if _, ok := w.Inputs[t.Platform.Input]; !ok {
				return fail(fmt.Sprintf("platform: unknown input %q", t.Platform.Input))
			}
		}
		selectors := 0
		for _, selected := range []bool{t.Image != "", t.From != "", t.Coopr != nil, t.Publish != nil, t.Uses != "", t.Host != nil} {
			if selected {
				selectors++
			}
		}
		if t.Environment != nil {
			selectors++
		}
		if selectors != 1 {
			return fail("select exactly one of image, from, coopr, environment, host, publish or uses")
		}
		if t.Uses == "" && t.With != nil {
			return fail("with requires uses")
		}
		for key, value := range t.With {
			if !namePattern.MatchString(key) {
				return fail(fmt.Sprintf("invalid input name %q", key))
			}
			if err := w.validateValue(value); err != nil {
				return fail(fmt.Sprintf("with %q: %s", key, err))
			}
		}
		if t.Uses != "" {
			parts := strings.Split(t.Uses, "/")
			if len(parts) != 2 || !namePattern.MatchString(parts[0]) || !namePattern.MatchString(parts[1]) || w.Imports[parts[0]] == "" {
				return fail("uses must name a defined import/task")
			}
			if len(t.Steps) != 0 {
				return fail("uses cannot be combined with steps")
			}
		}
		for _, build := range []*CooprBuild{t.Coopr, t.Environment} {
			if build == nil {
				continue
			}
			for key, argument := range build.Args {
				if argument.Input != "" {
					if _, ok := w.Inputs[argument.Input]; !ok {
						return fail(fmt.Sprintf("coopr argument %q: unknown input %q", key, argument.Input))
					}
				}
			}
		}
		if err := t.validateNative(); err != nil {
			return fail(err.Error())
		}
		if err := t.validateHost(); err != nil {
			return fail(err.Error())
		}
		for _, build := range []*CooprBuild{t.Coopr, t.Environment} {
			if build != nil && build.FilesFrom == "component" && len(w.Assets) == 0 && t.ComponentDir == "" {
				return fail("component definition files require declared assets")
			}
		}
		if err := t.validateMounts(w); err != nil {
			return fail(err.Error())
		}
		if strings.ContainsAny(t.Image, "\x00\n\r") {
			return fail("invalid image reference")
		}
		if err := t.validateWorkdir(); err != nil {
			return fail(err.Error())
		}
		for key, value := range t.Env {
			if !envPattern.MatchString(key) {
				return fail("invalid environment entry")
			}
			if err := w.validateValue(value); err != nil {
				return fail(fmt.Sprintf("env %q: %s", key, err))
			}
		}
		for i, s := range t.Steps {
			if (s.Run == nil) == (s.Copy == nil) {
				return fail(fmt.Sprintf("step %d requires exactly one of run or copy", i+1))
			}
			if s.Run != nil {
				if len(s.Run) == 0 || s.Run[0] == "" {
					return fail("run requires nonempty argv")
				}
				for _, arg := range s.Run {
					if strings.ContainsRune(arg, 0) {
						return fail("run argv contains NUL")
					}
				}
			}
			if c := s.Copy; c != nil {
				if c.Scope != "" && c.Scope != "context" && c.Scope != "component" {
					return fail("copy scope must be context or component")
				}
				if c.From != "" && c.Scope != "" {
					return fail("copy scope cannot be combined with from")
				}
				if c.Scope == "component" && len(w.Assets) == 0 && t.ComponentDir == "" {
					return fail("component copy requires declared assets")
				}
				if len(c.Sources) == 0 || !path.IsAbs(c.Destination) || strings.ContainsRune(c.Destination, 0) {
					return fail("copy requires sources and an absolute destination")
				}
				for _, src := range c.Sources {
					if src == "" || strings.ContainsRune(src, 0) {
						return fail("copy source must not be empty or contain NUL")
					}
					clean := path.Clean(src)
					if clean == ".." || strings.HasPrefix(clean, "../") {
						return fail("copy source must not escape its context")
					}
				}
			}
		}
		for _, dep := range t.Dependencies() {
			if _, ok := w.Tasks[dep]; !ok {
				return fail(fmt.Sprintf("unknown dependency %q", dep))
			}
		}
		if err := t.validateSources(w.Tasks); err != nil {
			return fail(err.Error())
		}
	}
	_, err := w.order(nil, false)
	return err
}

func (t Task) validateStrategy() error {
	if t.Strategy == nil {
		return nil
	}
	if err := t.Strategy.Validate(); err != nil {
		return err
	}
	for key := range t.Env {
		if strings.HasPrefix(strings.ToUpper(key), "STOKE_MATRIX_") {
			return errors.New("STOKE_MATRIX_ environment names are reserved")
		}
	}
	return nil
}

// ValidateResolvedTemplates checks task kinds after resolving uses, including
// unused imported templates. Shapes retain their original source scope: source
// edges are checked in the declaring document, before execution instances exist.
func (w *Workflow) ValidateResolvedTemplates(resolved map[string]Task) error {
	for _, name := range taskNames(w.Tasks) {
		template, ok := resolved[name]
		if !ok || template.Uses != "" {
			return fmt.Errorf("task %q: unresolved template", name)
		}
		for _, err := range []error{template.validateStrategy(), template.validatePlatform(), template.validateNative(), template.validateHost(), template.validateWorkdir(), w.Tasks[name].validateSources(resolved)} {
			if err != nil {
				return fmt.Errorf("task %q: %w", name, err)
			}
		}
	}
	return nil
}

// Plan selects the requested targets and their prerequisites in deterministic
// topological order. Empty targets selects every task. Imports must be expanded
// first; unresolved uses cannot silently execute.
func (w *Workflow) Plan(targets []string) ([]string, error) {
	if err := w.Validate(); err != nil {
		return nil, err
	}
	if len(w.Inputs) != 0 {
		return nil, errors.New("workflow inputs must be bound before planning")
	}
	return w.order(targets, true)
}

func (w *Workflow) order(targets []string, requireExpanded bool) ([]string, error) {
	if len(targets) == 0 {
		targets = taskNames(w.Tasks)
	} else {
		targets = slices.Clone(targets)
		slices.Sort(targets)
	}
	status := map[string]uint8{}
	var ordered []string
	var visit func(string) error
	visit = func(name string) error {
		task, ok := w.Tasks[name]
		if !ok {
			return fmt.Errorf("unknown task %q", name)
		}
		if status[name] == 1 {
			return fmt.Errorf("dependency cycle at task %q", name)
		}
		if status[name] == 2 {
			return nil
		}
		if requireExpanded && task.Uses != "" {
			return fmt.Errorf("task %q: imported template must be resolved before planning", name)
		}
		status[name] = 1
		for _, dep := range task.Dependencies() {
			if err := visit(dep); err != nil {
				return err
			}
		}
		status[name] = 2
		ordered = append(ordered, name)
		return nil
	}
	for _, name := range targets {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}
func taskNames(tasks map[string]Task) []string {
	names := make([]string, 0, len(tasks))
	for name := range tasks {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
