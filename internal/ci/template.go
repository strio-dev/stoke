package ci

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	"text/template"

	"go.yaml.in/yaml/v3"
)

const MaxTemplateBytes = 1 << 20
const StageDirectory = ".stoke/ci"

//go:embed templates/*.tmpl
var templates embed.FS

func BuiltinTemplate(name string) ([]byte, error) {
	if strings.Contains(name, "/") || name == "" {
		return nil, fmt.Errorf("unknown built-in CI template %q", name)
	}
	data, err := templates.ReadFile("templates/" + name + ".tmpl")
	if err != nil {
		return nil, fmt.Errorf("unknown built-in CI template %q", name)
	}
	return data, nil
}

func BuiltinTemplates() []string {
	entries, _ := fs.ReadDir(templates, "templates")
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, strings.TrimSuffix(entry.Name(), ".tmpl"))
	}
	return names
}

type StageFile struct {
	Path string
	Data []byte
}

type stageTemplateData struct {
	Plan    *Plan
	Group   Group
	Job     Job
	Command string
}

// RenderStages gives both path and body the same provider-neutral view.
func RenderStages(plan *Plan, selection string, source []byte) ([]StageFile, error) {
	if len(source) == 0 || len(source) > MaxTemplateBytes {
		return nil, errors.New("CI template must contain 1 to 1048576 bytes")
	}
	header, body, ok := strings.Cut(strings.TrimPrefix(string(source), "---\n"), "\n---\n")
	if !bytes.HasPrefix(source, []byte("---\n")) || !ok {
		return nil, errors.New("CI template needs a YAML header delimited by ---")
	}
	var config struct {
		Output string `yaml:"output"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(header))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("CI template header: %w", err)
	}
	if config.Output == "" {
		return nil, errors.New("CI template header requires output")
	}
	pathTemplate, err := template.New("output").Option("missingkey=error").Parse(config.Output)
	if err != nil {
		return nil, fmt.Errorf("CI template output: %w", err)
	}
	funcs := template.FuncMap{"indent": func(prefix, value string) string {
		return prefix + strings.ReplaceAll(value, "\n", "\n"+prefix)
	}}
	stage, err := template.New("stage").Option("missingkey=error").Funcs(funcs).Parse(body)
	if err != nil {
		return nil, fmt.Errorf("CI template body: %w", err)
	}
	var files []StageFile
	paths := map[string]bool{}
	for _, group := range plan.Groups {
		for _, job := range group.Jobs {
			if selection != "" && selection != job.ID && !slices.Contains(group.Members, selection) {
				continue
			}
			data := stageTemplateData{Plan: plan, Group: group, Job: job, Command: stageCommand(plan, job)}
			var renderedPath bytes.Buffer
			if err := pathTemplate.Execute(&renderedPath, data); err != nil {
				return nil, fmt.Errorf("CI template output for %s: %w", job.ID, err)
			}
			name := renderedPath.String()
			if err := relativePath(name, false); err != nil {
				return nil, fmt.Errorf("CI template output %q: %w", name, err)
			}
			if name == ".checksums" || strings.HasPrefix(name, ".checksums/") {
				return nil, errors.New("CI template output uses the reserved .checksums path")
			}
			if paths[name] {
				return nil, fmt.Errorf("CI template produces duplicate output %q", name)
			}
			paths[name] = true
			var rendered bytes.Buffer
			if err := stage.Execute(&rendered, data); err != nil {
				return nil, fmt.Errorf("CI template body for %s: %w", job.ID, err)
			}
			if len(bytes.TrimSpace(rendered.Bytes())) == 0 {
				return nil, fmt.Errorf("CI template produces empty stage %q", job.ID)
			}
			files = append(files, StageFile{Path: StageDirectory + "/" + name, Data: rendered.Bytes()})
		}
	}
	for name := range paths {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if paths[parent] {
				return nil, fmt.Errorf("CI template outputs conflict: %q is a parent of %q", parent, name)
			}
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("stage %q is absent from the CI plan", selection)
	}
	return files, nil
}

func stageCommand(plan *Plan, job Job) string {
	args := []string{"stoke", "run", "--ci"}
	config := plan.Config
	if config.Workflow != "stoke.yaml" {
		args = append(args, "-f", config.Workflow)
	}
	if config.Context != path.Dir(config.Workflow) {
		args = append(args, "--context", config.Context)
	}
	if config.Network != "" {
		args = append(args, "--network", config.Network)
	}
	for _, direction := range []struct {
		flag  string
		repos []string
	}{{"--cache-from", config.CacheFrom}, {"--cache-to", config.CacheTo}} {
		if direction.repos != nil && len(direction.repos) == 0 {
			args = append(args, direction.flag+"=")
		}
		for _, repo := range direction.repos {
			args = append(args, direction.flag, repo)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(config.Inputs)) {
		args = append(args, "--input", name+"="+config.Inputs[name])
	}
	for i := range args {
		args[i] = quoteStageArgument(args[i])
	}
	for _, name := range job.Execute {
		if _, host := plan.Host[name]; host {
			args = append(args, `"$@"`)
			break
		}
	}
	for _, name := range job.Execute {
		args = append(args, quoteStageArgument(name))
	}
	return strings.Join(args, " ")
}

func quoteStageArgument(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-.=/:", r)
	}) < 0 {
		return value
	}
	// Quote dollar signs as their own literal shell segments as well.
	replacer := strings.NewReplacer("'", "'\"'\"'", "$", "$''")
	return "'" + replacer.Replace(value) + "'"
}

// StageChecksum keeps file ownership separate from the backend's syntax.
func StageChecksum(body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(hex.EncodeToString(sum[:]) + "\n")
}
