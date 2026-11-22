package workflow

import (
	"errors"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

const ScriptContext = "stoke-scripts"
const ScriptMountRoot = "/.stoke-scripts"

func (t Task) HasScripts() bool {
	return slices.ContainsFunc(t.Steps, func(s Step) bool { return s.Script != "" })
}

// IsRun includes scalar shebang scripts, which require a real script file.
func (s Step) IsRun() bool { return s.Run != nil || s.Script != "" }

// Executable identifies the tool required by a step, not a shell to wrap it.
func (s Step) Executable() string {
	if s.Script != "" {
		interpreter, _ := scriptInterpreter(s.Script)
		return interpreter
	}
	if len(s.Run) != 0 {
		return s.Run[0]
	}
	return ""
}

func scriptInterpreter(script string) (string, error) {
	line, _, _ := strings.Cut(script, "\n")
	if !strings.HasPrefix(line, "#!") || !validInputString(script) || strings.ContainsRune(line, '\r') {
		return "", errors.New("run shebang requires a valid first line without CR or NUL")
	}
	interpreter := strings.TrimLeft(line[2:], " \t")
	if end := strings.IndexAny(interpreter, " \t"); end >= 0 {
		interpreter = interpreter[:end]
	}
	if !path.IsAbs(interpreter) {
		return "", errors.New("run shebang requires an absolute interpreter path; use /usr/bin/env for PATH lookup")
	}
	return interpreter, nil // Linux, not Stoke, parses the optional shebang argument.
}

func (s Step) MarshalYAML() (any, error) {
	type plain Step
	if s.Script == "" {
		return plain(s), nil
	}
	var n yaml.Node
	if err := n.Encode(plain(s)); err != nil {
		return nil, err
	}
	n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "run"}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s.Script})
	return &n, nil
}
