package workflow

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/containerd/platforms"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.yaml.in/yaml/v3"
)

// Platform selects a Linux container target or a host task's required native
// platform, directly or through an explicit matrix/input binding. It does not
// schedule or provision a host runner.
type Platform struct {
	Value  *string
	Matrix string
	Input  string
}

func (p *Platform) UnmarshalYAML(n *yaml.Node) error {
	*p = Platform{}
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		value := n.Value
		p.Value = &value
	} else {
		if n.Kind != yaml.MappingNode || len(n.Content) != 2 || n.Content[0].Tag != "!!str" || n.Content[1].Kind != yaml.ScalarNode || n.Content[1].Tag != "!!str" {
			return errors.New("platform must be a Linux os/arch[/variant] string or exactly one matrix/input binding")
		}
		switch n.Content[0].Value {
		case "matrix":
			p.Matrix = n.Content[1].Value
		case "input":
			p.Input = n.Content[1].Value
		default:
			return fmt.Errorf("unknown platform binding %q", n.Content[0].Value)
		}
	}
	return p.validate()
}

func (p Platform) MarshalYAML() (any, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if p.Value != nil {
		return *p.Value, nil
	}
	if p.Matrix != "" {
		return map[string]string{"matrix": p.Matrix}, nil
	}
	return map[string]string{"input": p.Input}, nil
}

func (p Platform) validate() error {
	count := 0
	for _, selected := range []bool{p.Value != nil, p.Matrix != "", p.Input != ""} {
		if selected {
			count++
		}
	}
	if count != 1 {
		return errors.New("platform requires exactly one literal, matrix or input value")
	}
	if p.Value != nil {
		_, err := NormalizePlatform(*p.Value)
		return err
	}
	if p.Matrix != "" && !envPattern.MatchString(p.Matrix) {
		return fmt.Errorf("invalid platform matrix key %q", p.Matrix)
	}
	if p.Input != "" && !namePattern.MatchString(p.Input) {
		return fmt.Errorf("invalid platform input name %q", p.Input)
	}
	return nil
}

// Clone returns a selector whose literal storage is independent of the original.
func (p *Platform) Clone() *Platform {
	if p == nil {
		return nil
	}
	copy := *p
	if p.Value != nil {
		value := *p.Value
		copy.Value = &value
	}
	return &copy
}

// ResolvedPlatform returns the canonical target, or empty for the engine default.
// Unresolved bindings are errors and must never silently use the host platform.
func (t Task) ResolvedPlatform() (string, error) {
	if t.Platform == nil {
		return "", nil
	}
	if err := t.Platform.validate(); err != nil {
		return "", err
	}
	if t.Platform.Value == nil {
		return "", errors.New("platform binding must be resolved before execution")
	}
	return NormalizePlatform(*t.Platform.Value)
}

func (t Task) validatePlatform() error {
	if t.Platform == nil {
		return nil
	}
	if t.Publish != nil || t.Uses != "" {
		return errors.New("platform cannot be combined with publish or overridden at a uses callsite")
	}
	if err := t.Platform.validate(); err != nil {
		return err
	}
	if t.Platform.Matrix != "" && !t.hasMatrixKey(t.Platform.Matrix) {
		return fmt.Errorf("platform: unknown matrix key %q", t.Platform.Matrix)
	}
	return nil
}

var platformPartPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// NormalizePlatform canonicalizes an explicit Linux target. Availability and
// foreign-architecture execution support remain the engine's responsibility.
func NormalizePlatform(value string) (string, error) {
	parts := strings.Split(value, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "linux" {
		return "", fmt.Errorf("platform %q must be Linux os/arch[/variant]", value)
	}
	arch := parts[1]
	variant := ""
	if len(parts) == 3 {
		variant = parts[2]
	}
	if !platformPartPattern.MatchString(arch) || (len(parts) == 3 && !platformPartPattern.MatchString(variant)) {
		return "", fmt.Errorf("platform %q requires lowercase architecture and variant names", value)
	}
	return platforms.Format(platforms.Normalize(v1.Platform{OS: "linux", Architecture: arch, Variant: variant})), nil
}
