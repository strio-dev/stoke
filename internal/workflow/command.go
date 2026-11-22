package workflow

import (
	"errors"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Command is execution argv. Scalar scripts without a shebang normalize here
// before planning or execution; shebang bodies are retained separately on Step.
type Command []string

func (c *Command) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.SequenceNode {
		var argv []string
		if err := n.Decode(&argv); err != nil {
			return err
		}
		*c = argv
		return nil
	}
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" || strings.TrimSpace(n.Value) == "" {
		return errors.New("run must be a nonempty argv list or nonblank shell script")
	}
	// End option parsing even when the script begins with '-'.
	*c = Command{"sh", "-eu", "-c", "--", n.Value}
	return nil
}
