package workflow

import (
	"errors"
	"fmt"
	"path"

	"go.yaml.in/yaml/v3"
)

// Build changes only the authored YAML view of an inline Coopr image build.
// Its JSON/native representation keeps the established Files/Environment fields.
type Build CooprBuild

func (b *Build) Native() *CooprBuild { return (*CooprBuild)(b) }

type buildYAML struct {
	Coopr     []string            `yaml:"coopr"`
	Context   string              `yaml:"context,omitempty"`
	FilesFrom string              `yaml:"files-from,omitempty"`
	Kind      string              `yaml:"kind,omitempty"`
	Target    string              `yaml:"target,omitempty"`
	Args      map[string]Argument `yaml:"args,omitempty"`
}

func (b *Build) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("build must be a mapping with an ordered coopr list")
	}
	seen := map[string]bool{}
	for i := 0; i < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		if key.Tag != "!!str" || seen[key.Value] {
			return errors.New("invalid or duplicate build field")
		}
		seen[key.Value] = true
		switch key.Value {
		case "coopr":
			if value.Kind != yaml.SequenceNode {
				return errors.New("build.coopr must be a list of definition file paths")
			}
			for _, file := range value.Content {
				if file.Kind != yaml.ScalarNode || file.Tag != "!!str" {
					return errors.New("build.coopr entries must be strings")
				}
			}
		case "files-from", "kind", "target", "args":
		case "context":
			if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
				return errors.New("build context must be a string")
			}
		default:
			return fmt.Errorf("unknown build field %q", key.Value)
		}
	}
	var authored buildYAML
	if err := n.Decode(&authored); err != nil {
		return err
	}
	context := authored.Context
	if context != "" {
		context = path.Clean(context)
		if context == "." {
			context = ""
		}
	}
	*b = Build{Files: authored.Coopr, FilesFrom: authored.FilesFrom, Kind: authored.Kind, Target: authored.Target, Args: authored.Args, Context: context}
	return nil
}

func (b Build) MarshalYAML() (any, error) {
	return buildYAML{Coopr: b.Files, Context: b.Context, FilesFrom: b.FilesFrom, Kind: b.Kind, Target: b.Target, Args: b.Args}, nil
}
