package workflow

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Input declares a string accepted when instantiating a workflow component.
// Optional inputs without a default resolve to the empty string.
type Input struct {
	Type     string  `yaml:"type,omitempty"`
	Required bool    `yaml:"required,omitempty"`
	Default  *string `yaml:"default,omitempty"`
}

func (in *Input) UnmarshalYAML(n *yaml.Node) error {
	*in = Input{}
	if n.Kind != yaml.MappingNode {
		return errors.New("input declaration must be a mapping")
	}
	seen := map[string]bool{}
	for i := 0; i < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		if key.Tag != "!!str" || seen[key.Value] {
			return errors.New("invalid or duplicate input declaration field")
		}
		seen[key.Value] = true
		switch key.Value {
		case "type":
			if value.Tag != "!!str" || value.Value != "string" {
				return errors.New("input type must be string")
			}
			in.Type = value.Value
		case "required":
			if value.Tag != "!!bool" {
				return errors.New("input required must be a boolean")
			}
			if err := value.Decode(&in.Required); err != nil {
				return err
			}
		case "default":
			if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
				return errors.New("input default must be a string")
			}
			v := value.Value
			in.Default = &v
		default:
			return fmt.Errorf("unknown input declaration field %q", key.Value)
		}
	}
	return in.validate()
}

func (in Input) validate() error {
	if in.Type != "" && in.Type != "string" {
		return errors.New("input type must be string")
	}
	if in.Required && in.Default != nil {
		return errors.New("required input cannot have a default")
	}
	if in.Default != nil && !validInputString(*in.Default) {
		return errors.New("input default must be UTF-8 without NUL")
	}
	return nil
}

// Value is a literal string or an explicit reference to a workflow input.
type Value struct {
	Value *string
	Input string
}

// Literal constructs an explicit string value.
func Literal(value string) Value { return Value{Value: &value} }

func (v *Value) UnmarshalYAML(n *yaml.Node) error {
	*v = Value{}
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		*v = Literal(n.Value)
		return v.validate()
	}
	if n.Kind != yaml.MappingNode || len(n.Content) != 2 || n.Content[0].Tag != "!!str" || n.Content[0].Value != "input" || n.Content[1].Tag != "!!str" || n.Content[1].Kind != yaml.ScalarNode {
		return errors.New("value must be a string or exactly one input binding")
	}
	v.Input = n.Content[1].Value
	return v.validate()
}

func (v Value) MarshalYAML() (any, error) {
	if err := v.validate(); err != nil {
		return nil, err
	}
	if v.Value != nil {
		return *v.Value, nil
	}
	return map[string]string{"input": v.Input}, nil
}

func (v Value) validate() error {
	if (v.Value != nil) == (v.Input != "") {
		return errors.New("value requires exactly one literal or input binding")
	}
	if v.Value != nil && !validInputString(*v.Value) {
		return errors.New("value must be UTF-8 without NUL")
	}
	if v.Input != "" && !namePattern.MatchString(v.Input) {
		return fmt.Errorf("invalid input name %q", v.Input)
	}
	return nil
}

func validInputString(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func (w *Workflow) validateInputs() error {
	for name, input := range w.Inputs {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("invalid input name %q", name)
		}
		if err := input.validate(); err != nil {
			return fmt.Errorf("input %q: %w", name, err)
		}
	}
	return nil
}

func (w *Workflow) validateValue(value Value) error {
	if err := value.validate(); err != nil {
		return err
	}
	if value.Input != "" {
		if _, ok := w.Inputs[value.Input]; !ok {
			return fmt.Errorf("unknown input %q", value.Input)
		}
	}
	return nil
}

// BindInputs resolves declarations before expansion. Decode a fresh workflow for
// each invocation; this method replaces task maps only after validation succeeds.
func (w *Workflow) BindInputs(values map[string]string) error {
	if err := w.Validate(); err != nil {
		return err
	}
	resolved, err := w.ResolveInputs(values)
	if err != nil {
		return err
	}
	bound := maps.Clone(w.Tasks)
	for name, task := range bound {
		bound[name], err = task.BindInputs(resolved)
		if err != nil {
			return fmt.Errorf("task %q: %w", name, err)
		}
	}
	w.Tasks = bound
	w.Inputs = nil
	return nil
}

// ResolveInputs validates invocation values and applies declared defaults. A
// loader can validate the complete definition once, then resolve each call's
// values without revalidating or copying unselected tasks.
func (w *Workflow) ResolveInputs(values map[string]string) (map[string]string, error) {
	if err := w.validateInputs(); err != nil {
		return nil, err
	}
	resolved := map[string]string{}
	for name, value := range values {
		if _, ok := w.Inputs[name]; !ok {
			return nil, fmt.Errorf("unknown input %q", name)
		}
		if !validInputString(value) {
			return nil, fmt.Errorf("input %q must be UTF-8 without NUL", name)
		}
	}
	for name, input := range w.Inputs {
		value, supplied := values[name]
		if !supplied && input.Required {
			return nil, fmt.Errorf("required input %q is missing", name)
		}
		if !supplied && input.Default != nil {
			value = *input.Default
		}
		resolved[name] = value
	}
	return resolved, nil
}

// BindInputs resolves binding fields in a copy of a previously validated task.
// Other task semantics remain the responsibility of workflow validation.
func (task Task) BindInputs(resolved map[string]string) (Task, error) {
	task = task.Clone()
	if task.Env == nil {
		task.Env = map[string]Value{}
	}
	resolve := func(name string) (string, error) {
		value, ok := resolved[name]
		if !ok {
			return "", fmt.Errorf("unknown input %q", name)
		}
		if !validInputString(value) {
			return "", fmt.Errorf("input %q must be UTF-8 without NUL", name)
		}
		return value, nil
	}
	if task.Platform != nil && task.Platform.Input != "" {
		value, err := resolve(task.Platform.Input)
		if err != nil {
			return Task{}, fmt.Errorf("platform: %w", err)
		}
		task.Platform = &Platform{Value: &value}
		if _, err := task.ResolvedPlatform(); err != nil {
			return Task{}, fmt.Errorf("platform: %w", err)
		}
	}
	for _, bindings := range []map[string]Value{task.Env, task.With} {
		for key, binding := range bindings {
			if binding.Input == "" {
				continue
			}
			value, err := resolve(binding.Input)
			if err != nil {
				return Task{}, err
			}
			bindings[key] = Literal(value)
		}
	}
	for _, build := range []*CooprBuild{task.Coopr, task.Environment} {
		if build == nil {
			continue
		}
		for key, argument := range build.Args {
			if argument.Input == "" {
				continue
			}
			value, err := resolve(argument.Input)
			if err != nil {
				return Task{}, err
			}
			build.Args[key] = Argument{Value: &value}
		}
	}
	return task, nil
}

func cloneValues(values map[string]Value) map[string]Value {
	cloned := maps.Clone(values)
	for key, value := range cloned {
		if value.Value != nil {
			literal := *value.Value
			value.Value = &literal
			cloned[key] = value
		}
	}
	return cloned
}

// Null values skip custom unmarshaling in yaml.v3. Check map shapes and string
// keys before decoding so null bindings cannot silently become empty maps.
func validateInputNodes(document *yaml.Node) error {
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	root := document.Content[0]
	for i := 0; i < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		switch key.Value {
		case "inputs":
			if value.Kind != yaml.MappingNode {
				return errors.New("inputs must be a mapping")
			}
			for j := 0; j < len(value.Content); j += 2 {
				if value.Content[j].Tag != "!!str" {
					return errors.New("input names must be strings")
				}
				if value.Content[j+1].Kind != yaml.MappingNode {
					return errors.New("input declaration must be a mapping")
				}
			}
		case "tasks":
			if value.Kind != yaml.MappingNode {
				continue
			}
			for j := 1; j < len(value.Content); j += 2 {
				task := value.Content[j]
				if task.Kind != yaml.MappingNode {
					continue
				}
				for k := 0; k < len(task.Content); k += 2 {
					field, bindings := task.Content[k], task.Content[k+1]
					if field.Value == "platform" && bindings.Tag == "!!null" {
						return errors.New("platform cannot be null; omit it to use the engine default")
					}
					if field.Value == "contexts" {
						if bindings.Kind != yaml.MappingNode {
							return errors.New("contexts must be a mapping")
						}
						for b := 0; b < len(bindings.Content); b += 2 {
							if bindings.Content[b].Tag != "!!str" || bindings.Content[b+1].Tag != "!!str" {
								return errors.New("context names and directory paths must be strings")
							}
						}
					}
					if field.Value != "env" && field.Value != "with" {
						continue
					}
					if bindings.Kind != yaml.MappingNode {
						return fmt.Errorf("%s must be a mapping", field.Value)
					}
					for b := 0; b < len(bindings.Content); b += 2 {
						if bindings.Content[b].Tag != "!!str" {
							return fmt.Errorf("%s names must be strings", field.Value)
						}
					}
				}
			}
		}
	}
	return nil
}
