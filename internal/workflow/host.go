package workflow

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Host declares runner requirements and a captured-file contract. It requests
// host execution; authority is granted separately by the invoking runner.
type Host struct {
	Inputs       []HostInput `yaml:"inputs,omitempty"`
	Outputs      []string    `yaml:"outputs,omitempty"`
	Tools        []string    `yaml:"tools"`
	InheritEnv   []string    `yaml:"inherit-env,omitempty"`
	Toolchain    []string    `yaml:"toolchain,omitempty"`
	Capabilities []string    `yaml:"capabilities,omitempty"`
	Effects      string      `yaml:"effects,omitempty"`
}

// HostInput snapshots a caller-context path or a prior host task's captured
// path into the private workspace. An omitted destination retains Path.
type HostInput struct {
	Path        string `yaml:"path"`
	From        string `yaml:"from,omitempty"`
	Destination string `yaml:"destination,omitempty"`
}

// Clone keeps imported templates and expanded variants independent.
func (h *Host) Clone() *Host {
	if h == nil {
		return nil
	}
	copy := *h
	copy.Inputs = slices.Clone(h.Inputs)
	copy.Outputs = slices.Clone(h.Outputs)
	copy.Tools = slices.Clone(h.Tools)
	copy.InheritEnv = slices.Clone(h.InheritEnv)
	copy.Toolchain = slices.Clone(h.Toolchain)
	copy.Capabilities = slices.Clone(h.Capabilities)
	return &copy
}

func hostPath(value string, root bool) bool {
	return value != "" && validInputString(value) && !strings.ContainsAny(value, "\\\r\n") &&
		!path.IsAbs(value) && path.Clean(value) == value && value != ".." &&
		!strings.HasPrefix(value, "../") && (root || value != ".")
}

func hostPathOverlap(a, b string) bool {
	return a == b || a == "." || b == "." || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

var hostCapabilityPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]{0,127}$`)

func (t Task) validateWorkdir() error {
	if t.Workdir == "" || t.Host != nil {
		return nil // Host validation checks the private-workspace path.
	}
	if t.Uses != "" {
		if validInputString(t.Workdir) && (path.IsAbs(t.Workdir) || hostPath(t.Workdir, true)) {
			return nil // The resolved template determines which form is valid.
		}
		return errors.New("template workdir must be an absolute container path or normalized relative host workspace path")
	}
	if !path.IsAbs(t.Workdir) || strings.ContainsRune(t.Workdir, 0) {
		return errors.New("workdir must be an absolute container path")
	}
	return nil
}

func (t Task) validateHost() error {
	if t.Host == nil {
		for _, step := range t.Steps {
			if step.Stdout != "" {
				return errors.New("stdout capture requires host execution")
			}
		}
		return nil
	}
	h := t.Host
	if t.Outputs != nil || len(t.Contexts) != 0 || len(t.AllMounts()) != 0 {
		return errors.New("host tasks use host inputs/outputs and cannot declare outputs, contexts or mounts")
	}
	if t.Platform == nil {
		return errors.New("host tasks require an explicit native platform")
	}
	if t.Platform.Value != nil {
		platform, err := t.ResolvedPlatform()
		if err != nil {
			return err
		}
		if platform != "linux/amd64" && platform != "linux/arm64" {
			return errors.New("host tasks support native linux/amd64 or linux/arm64; host execution does not emulate platforms")
		}
	}
	if t.Workdir != "" && !hostPath(t.Workdir, true) {
		return errors.New("host workdir must be a normalized relative workspace path")
	}
	if h.Effects != "" && h.Effects != "external" && h.Effects != "captured" {
		return errors.New("host effects must be external or captured")
	}
	if len(h.Tools) == 0 {
		return errors.New("host tasks require declared tools")
	}
	tools := map[string]bool{}
	for _, tool := range h.Tools {
		if !validInputString(tool) || strings.TrimSpace(tool) != tool || tool == "" || len(tool) > 4096 ||
			strings.ContainsAny(tool, "\r\n\\") || (strings.Contains(tool, "/") && (!path.IsAbs(tool) || path.Clean(tool) != tool)) ||
			tool == "." || tool == ".." || tool == "/" || tools[tool] {
			return errors.New("host tools must be distinct executable names or normalized absolute paths")
		}
		tools[tool] = true
	}
	if len(t.Steps) == 0 {
		return errors.New("host tasks require run steps")
	}
	for i, step := range t.Steps {
		if step.Copy != nil || len(step.Run) == 0 {
			return fmt.Errorf("host step %d requires literal run argv", i+1)
		}
		if !tools[step.Run[0]] {
			return fmt.Errorf("host step %d executable %q must be a declared tool", i+1, step.Run[0])
		}
		if step.Stdout != "" && !hostPath(step.Stdout, false) {
			return fmt.Errorf("host step %d stdout must be a normalized relative file path", i+1)
		}
	}
	var destinations []string
	for _, input := range h.Inputs {
		if !hostPath(input.Path, true) || input.From != "" && !namePattern.MatchString(input.From) {
			return errors.New("host inputs require normalized relative paths and optional producer names")
		}
		destination := input.Destination
		if destination == "" {
			destination = input.Path
		}
		if !hostPath(destination, true) {
			return errors.New("host input destinations must be normalized relative paths")
		}
		for _, prior := range destinations {
			if hostPathOverlap(prior, destination) {
				return errors.New("host input destinations must not overlap")
			}
		}
		destinations = append(destinations, destination)
	}
	var outputs []string
	for _, output := range h.Outputs {
		if !hostPath(output, false) {
			return errors.New("host outputs must be normalized relative paths below the workspace")
		}
		for _, prior := range outputs {
			if hostPathOverlap(prior, output) {
				return errors.New("host output paths must not overlap")
			}
		}
		outputs = append(outputs, output)
	}
	seen := map[string]bool{}
	for _, key := range h.InheritEnv {
		if !envPattern.MatchString(key) || strings.HasPrefix(strings.ToUpper(key), "STOKE_") || seen[key] {
			return errors.New("host inherit-env requires distinct nonreserved environment names")
		}
		seen[key] = true
	}
	seen = map[string]bool{}
	for _, item := range h.Toolchain {
		if !validInputString(item) || strings.ContainsAny(item, "\\\r\n") || !path.IsAbs(item) || path.Clean(item) != item || item == "/" || seen[item] {
			return errors.New("host toolchain requires distinct normalized absolute paths")
		}
		seen[item] = true
	}
	seen = map[string]bool{}
	for _, capability := range h.Capabilities {
		if !hostCapabilityPattern.MatchString(capability) || seen[capability] {
			return errors.New("host capabilities must be distinct bounded names")
		}
		seen[capability] = true
	}
	if t.CacheEnabled() {
		for _, input := range destinations {
			for _, output := range outputs {
				if hostPathOverlap(input, output) {
					return errors.New("cacheable host inputs and outputs must not overlap; snapshot sources into a separate directory")
				}
			}
		}
	}
	if t.CacheEnabled() && (h.Effects != "captured" || len(h.Inputs) == 0 || len(h.Outputs) == 0 || len(h.Toolchain) == 0) {
		return errors.New("host cache reuse requires captured effects and explicit inputs, outputs and toolchain")
	}
	return nil
}
