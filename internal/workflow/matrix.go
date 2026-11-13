package workflow

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

const MaxVariants = 256
const MaxJobs = 1024
const DefaultMaxParallel = 4

type Strategy struct {
	Matrix         *Matrix `yaml:"matrix"`
	MaxParallel    int     `yaml:"max-parallel,omitempty"`
	FailFast       *bool   `yaml:"fail-fast,omitempty"`
	maxParallelSet bool
}
type Matrix struct {
	Axes    map[string][]string
	Include []map[string]string
	Exclude []map[string]string
}
type Job struct {
	Name, Task string
	Matrix     map[string]string
	Definition Task
}
type Group struct {
	Name        string
	Jobs        []Job
	MaxParallel int
	FailFast    bool
}
type ExecutionPlan struct{ Groups []Group }

// UnmarshalYAML preserves whether max-parallel was explicitly supplied while
// retaining strict field and duplicate-key validation for strategy mappings.
func (s *Strategy) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("strategy must be a mapping")
	}
	seen := map[string]bool{}
	for i := 0; i < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		if key.Tag != "!!str" || seen[key.Value] {
			return errors.New("invalid or duplicate strategy field")
		}
		seen[key.Value] = true
		switch key.Value {
		case "matrix":
			if value.Kind != yaml.MappingNode {
				return errors.New("strategy matrix must be a mapping")
			}
			s.Matrix = &Matrix{}
			if err := value.Decode(s.Matrix); err != nil {
				return err
			}
		case "max-parallel":
			s.maxParallelSet = true
			if value.Tag != "!!int" {
				return errors.New("max-parallel must be an integer")
			}
			if err := value.Decode(&s.MaxParallel); err != nil {
				return err
			}
		case "fail-fast":
			if value.Tag != "!!bool" {
				return errors.New("fail-fast must be a boolean")
			}
			var flag bool
			if err := value.Decode(&flag); err != nil {
				return err
			}
			s.FailFast = &flag
		default:
			return fmt.Errorf("unknown strategy field %q", key.Value)
		}
	}
	return nil
}

func scalar(n *yaml.Node) (string, error) {
	if n.Kind != yaml.ScalarNode || (n.Tag != "!!str" && n.Tag != "!!int" && n.Tag != "!!bool") || strings.ContainsRune(n.Value, 0) {
		return "", errors.New("matrix values must be strings, integers or booleans")
	}
	if n.Tag == "!!int" {
		var value int64
		if err := n.Decode(&value); err != nil {
			return "", err
		}
		return fmt.Sprint(value), nil
	}
	if n.Tag == "!!bool" {
		var value bool
		if err := n.Decode(&value); err != nil {
			return "", err
		}
		return fmt.Sprint(value), nil
	}
	return n.Value, nil
}
func entries(n *yaml.Node) ([]map[string]string, error) {
	if n.Kind != yaml.SequenceNode {
		return nil, errors.New("matrix include/exclude must be lists of mappings")
	}
	var out []map[string]string
	for _, node := range n.Content {
		if node.Kind != yaml.MappingNode || len(node.Content) == 0 {
			return nil, errors.New("matrix include/exclude entries must be nonempty mappings")
		}
		entry := map[string]string{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Tag != "!!str" {
				return nil, errors.New("matrix keys must be strings")
			}
			if _, ok := entry[key.Value]; ok {
				return nil, fmt.Errorf("duplicate matrix key %q", key.Value)
			}
			value, err := scalar(node.Content[i+1])
			if err != nil {
				return nil, err
			}
			entry[key.Value] = value
		}
		out = append(out, entry)
	}
	return out, nil
}
func (m *Matrix) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("matrix must be a mapping")
	}
	m.Axes = map[string][]string{}
	seen := map[string]bool{}
	for i := 0; i < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		if key.Tag != "!!str" || seen[key.Value] {
			return errors.New("invalid or duplicate matrix key")
		}
		seen[key.Value] = true
		switch key.Value {
		case "include", "exclude":
			list, err := entries(value)
			if err != nil {
				return err
			}
			if key.Value == "include" {
				m.Include = list
			} else {
				m.Exclude = list
			}
		default:
			if value.Kind != yaml.SequenceNode || len(value.Content) == 0 {
				return errors.New("matrix axes require nonempty lists")
			}
			for _, node := range value.Content {
				v, err := scalar(node)
				if err != nil {
					return err
				}
				m.Axes[key.Value] = append(m.Axes[key.Value], v)
			}
		}
	}
	return nil
}
func (m Matrix) MarshalYAML() (any, error) {
	out := map[string]any{}
	for k, v := range m.Axes {
		out[k] = v
	}
	if len(m.Include) > 0 {
		out["include"] = m.Include
	}
	if len(m.Exclude) > 0 {
		out["exclude"] = m.Exclude
	}
	return out, nil
}
func (s *Strategy) Validate() error {
	if s.Matrix == nil {
		return errors.New("strategy requires matrix")
	}
	if s.MaxParallel < 0 || s.MaxParallel > MaxVariants || (s.maxParallelSet && s.MaxParallel == 0) {
		return errors.New("max-parallel must be between 1 and 256 when supplied")
	}
	_, err := s.Matrix.Combinations()
	return err
}
func matches(combo, subset map[string]string) bool {
	for k, v := range subset {
		if actual, ok := combo[k]; !ok || actual != v {
			return false
		}
	}
	return true
}
func (m *Matrix) Combinations() ([]map[string]string, error) {
	if m == nil {
		return nil, errors.New("matrix is required")
	}
	names := make([]string, 0, len(m.Axes))
	envNames := map[string]string{}
	checkKey := func(key string) error {
		if !envPattern.MatchString(key) {
			return fmt.Errorf("invalid matrix key %q", key)
		}
		upper := strings.ToUpper(key)
		if old, ok := envNames[upper]; ok && old != key {
			return fmt.Errorf("matrix keys %q and %q share an environment name", old, key)
		}
		envNames[upper] = key
		return nil
	}
	product := 1
	for key, values := range m.Axes {
		if err := checkKey(key); err != nil {
			return nil, err
		}
		if len(values) == 0 {
			return nil, errors.New("matrix axis is empty")
		}
		seen := map[string]bool{}
		for _, v := range values {
			if seen[v] || strings.ContainsRune(v, 0) {
				return nil, errors.New("matrix axis contains duplicate or invalid values")
			}
			seen[v] = true
		}
		if product > MaxVariants/len(values) {
			return nil, errors.New("matrix exceeds 256 combinations")
		}
		product *= len(values)
		names = append(names, key)
	}
	for _, list := range [][]map[string]string{m.Include, m.Exclude} {
		for _, entry := range list {
			if len(entry) == 0 {
				return nil, errors.New("matrix entries must not be empty")
			}
			for key, v := range entry {
				if err := checkKey(key); err != nil {
					return nil, err
				}
				if strings.ContainsRune(v, 0) {
					return nil, errors.New("matrix value contains NUL")
				}
			}
		}
	}
	for _, entry := range m.Exclude {
		for key := range entry {
			if _, ok := m.Axes[key]; !ok {
				return nil, fmt.Errorf("exclude references unknown axis %q", key)
			}
		}
	}
	slices.Sort(names)
	original := []map[string]string{{}}
	for _, key := range names {
		next := []map[string]string{}
		for _, combo := range original {
			for _, v := range m.Axes[key] {
				c := maps.Clone(combo)
				c[key] = v
				next = append(next, c)
			}
		}
		original = next
	}
	if len(names) == 0 {
		original = nil
	}
	surviving := original[:0]
	for _, combo := range original {
		excluded := false
		for _, entry := range m.Exclude {
			if matches(combo, entry) {
				excluded = true
				break
			}
		}
		if !excluded {
			surviving = append(surviving, combo)
		}
	}
	original = surviving
	result := make([]map[string]string, len(original))
	for i, c := range original {
		result[i] = maps.Clone(c)
	}
	standalone := map[string]bool{}
	for _, entry := range m.Include {
		augmented := false
		for i, combo := range original {
			compatible := true
			for key, v := range entry {
				if old, ok := combo[key]; ok && old != v {
					compatible = false
					break
				}
			}
			if compatible {
				maps.Copy(result[i], entry)
				augmented = true
			}
		}
		if !augmented {
			encoded, _ := json.Marshal(entry)
			key := string(encoded)
			if !standalone[key] {
				standalone[key] = true
				result = append(result, maps.Clone(entry))
			}
		}
		if len(result) > MaxVariants {
			return nil, errors.New("matrix exceeds 256 combinations")
		}
	}
	unique := []map[string]string{}
	seen := map[string]bool{}
	for _, combo := range result {
		data, _ := json.Marshal(combo)
		if !seen[string(data)] {
			seen[string(data)] = true
			unique = append(unique, combo)
		}
	}
	if len(unique) == 0 {
		return nil, errors.New("matrix expands to no variants")
	}
	return unique, nil
}

// Clone copies task declarations without sharing mutable fields between invocations.
func (t Task) Clone() Task {
	t.Platform = t.Platform.Clone()
	t.Host = t.Host.Clone()
	t.Needs = slices.Clone(t.Needs)
	t.Env = cloneValues(t.Env)
	t.With = cloneValues(t.With)
	t.Contexts = maps.Clone(t.Contexts)
	t.Mounts = cloneMounts(t.Mounts)
	t.Steps = slices.Clone(t.Steps)
	for i := range t.Steps {
		t.Steps[i].Run = slices.Clone(t.Steps[i].Run)
		t.Steps[i].Mounts = cloneMounts(t.Steps[i].Mounts)
		if c := t.Steps[i].Copy; c != nil {
			copy := *c
			copy.Sources = slices.Clone(c.Sources)
			t.Steps[i].Copy = &copy
		}
		if e := t.Steps[i].Export; e != nil {
			export := *e
			t.Steps[i].Export = &export
		}
	}
	if t.Cache != nil {
		value := *t.Cache
		t.Cache = &value
	}
	for _, slot := range []**CooprBuild{&t.Coopr, &t.Environment} {
		if *slot == nil {
			continue
		}
		build := **slot
		build.Files = slices.Clone(build.Files)
		build.Args = maps.Clone(build.Args)
		for name, argument := range build.Args {
			if argument.Value != nil {
				value := *argument.Value
				argument.Value = &value
				build.Args[name] = argument
			}
		}
		*slot = &build
	}
	if t.Outputs != nil {
		outputs := *t.Outputs
		if outputs.Filesystem != nil {
			value := *outputs.Filesystem
			outputs.Filesystem = &value
		}
		t.Outputs = &outputs
	}
	if t.Publish != nil {
		publication := *t.Publish
		publication.Destinations = slices.Clone(publication.Destinations)
		t.Publish = &publication
	}
	if t.Strategy != nil {
		strategy := *t.Strategy
		if strategy.FailFast != nil {
			value := *strategy.FailFast
			strategy.FailFast = &value
		}
		if strategy.Matrix != nil {
			matrix := *strategy.Matrix
			matrix.Axes = make(map[string][]string, len(strategy.Matrix.Axes))
			for key, values := range strategy.Matrix.Axes {
				matrix.Axes[key] = slices.Clone(values)
			}
			matrix.Include = cloneEntries(strategy.Matrix.Include)
			matrix.Exclude = cloneEntries(strategy.Matrix.Exclude)
			strategy.Matrix = &matrix
		}
		t.Strategy = &strategy
	}
	return t
}

func cloneEntries(entries []map[string]string) []map[string]string {
	out := make([]map[string]string, len(entries))
	for i, entry := range entries {
		out[i] = maps.Clone(entry)
	}
	return out
}

func (w *Workflow) Expand(targets []string) (*ExecutionPlan, error) {
	order, err := w.Plan(targets)
	if err != nil {
		return nil, err
	}
	plan := &ExecutionPlan{}
	groups := map[string]Group{}
	ids := map[string]bool{}
	count := 0
	for _, name := range order {
		task := w.Tasks[name]
		group := Group{Name: name, MaxParallel: 1, FailFast: true}
		combos := []map[string]string{nil}
		if task.Strategy != nil {
			combos, err = task.Strategy.Matrix.Combinations()
			if err != nil {
				return nil, err
			}
			group.MaxParallel = task.Strategy.MaxParallel
			if group.MaxParallel == 0 {
				group.MaxParallel = DefaultMaxParallel
			}
			if task.Strategy.FailFast != nil {
				group.FailFast = *task.Strategy.FailFast
			}
		}
		for _, combo := range combos {
			def := task
			def.Strategy = nil
			def = def.Clone()
			if def.Env == nil {
				def.Env = map[string]Value{}
			}
			if def.Platform != nil && def.Platform.Matrix != "" {
				key := def.Platform.Matrix
				value, ok := combo[key]
				if !ok {
					return nil, fmt.Errorf("task %q: platform matrix key %q is missing from a variant", name, key)
				}
				def.Platform = &Platform{Value: &value}
			}
			if _, err := def.ResolvedPlatform(); err != nil {
				return nil, fmt.Errorf("task %q: platform: %w", name, err)
			}
			if err := def.validateHost(); err != nil {
				return nil, fmt.Errorf("task %q: %w", name, err)
			}
			id := name
			if combo != nil {
				data, _ := json.Marshal(combo)
				sum := sha256.Sum256(data)
				id = fmt.Sprintf("%s--matrix-%x", name, sum[:8])
				if _, exists := w.Tasks[id]; exists {
					return nil, fmt.Errorf("matrix job name collides with task %q", id)
				}
				if def.Coopr == nil {
					// Clear names absent from standalone include variants so inherited
					// image values cannot masquerade as matrix inputs.
					for key := range task.Strategy.Matrix.Axes {
						def.Env["STOKE_MATRIX_"+strings.ToUpper(key)] = Literal("")
					}
					for _, entry := range task.Strategy.Matrix.Include {
						for key := range entry {
							def.Env["STOKE_MATRIX_"+strings.ToUpper(key)] = Literal("")
						}
					}
					for key, v := range combo {
						def.Env["STOKE_MATRIX_"+strings.ToUpper(key)] = Literal(v)
					}
				}
			}
			resolve := func(source string) (string, error) {
				if source == "" {
					return "", nil
				}
				g := groups[source]
				if len(g.Jobs) != 1 {
					return "", fmt.Errorf("task %q: data source %q has multiple matrix variants; use needs for an all-variant gate", name, source)
				}
				return g.Jobs[0].Name, nil
			}
			def.From, err = resolve(def.From)
			if err != nil {
				return nil, err
			}
			for i := range def.Steps {
				if c := def.Steps[i].Copy; c != nil {
					c.From, err = resolve(c.From)
					if err != nil {
						return nil, err
					}
				}
			}
			if def.Host != nil {
				for i := range def.Host.Inputs {
					def.Host.Inputs[i].From, err = resolve(def.Host.Inputs[i].From)
					if err != nil {
						return nil, err
					}
				}
			}
			for _, build := range []*CooprBuild{def.Coopr, def.Environment} {
				if build == nil {
					continue
				}
				for name, argument := range build.Args {
					argument.From, err = resolve(argument.From)
					if err != nil {
						return nil, err
					}
					if argument.Matrix != "" {
						value := combo[argument.Matrix]
						argument = Argument{Value: &value}
					}
					build.Args[name] = argument
				}
			}
			for _, mounts := range append([][]Mount{def.Mounts}, stepMounts(def.Steps)...) {
				for i := range mounts {
					mounts[i].From, err = resolve(mounts[i].From)
					if err != nil {
						return nil, err
					}
				}
			}
			if def.Publish != nil {
				def.Publish.From, err = resolve(def.Publish.From)
				if err != nil {
					return nil, err
				}
			}
			if ids[id] {
				return nil, fmt.Errorf("duplicate expanded job %q", id)
			}
			ids[id] = true
			count++
			if count > MaxJobs {
				return nil, errors.New("workflow exceeds 1024 expanded jobs")
			}
			group.Jobs = append(group.Jobs, Job{Name: id, Task: name, Matrix: maps.Clone(combo), Definition: def})
		}
		groups[name] = group
		plan.Groups = append(plan.Groups, group)
	}
	return plan, nil
}
