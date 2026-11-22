// Package source loads local or OCI workflows and expands reusable templates.
package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/strio-dev/stoke/internal/artifact"
	"github.com/strio-dev/stoke/internal/assets"
	"github.com/strio-dev/stoke/internal/workflow"
	"oras.land/oras-go/v2/registry"
)

type Loader struct {
	Inputs map[string]string
	Client artifact.Client
	Fetch  func(context.Context, string) (*artifact.Document, error)
}
type Result struct {
	Workflow *workflow.Workflow
	// RootWorkflow is the parsed owning document from the same read that
	// produced Workflow, before template resolution and input binding.
	RootWorkflow *workflow.Workflow
	Targets      []string
	ContextDir   string
	Reference    string
	owned        []string
	closeOnce    sync.Once
	closeErr     error
}

// Close releases only the temporary component trees allocated by this load.
// Call it after execution and engine cancellation/cleanup have completed.
func (r *Result) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		for _, directory := range r.owned {
			r.closeErr = errors.Join(r.closeErr, os.RemoveAll(directory))
		}
	})
	return r.closeErr
}

// ValidateContext checks every allocated package tree, including assets from
// unselected imports and wrappers whose tasks inherit another package's files.
func (r *Result) ValidateContext(callerDir string) error {
	if r == nil {
		return nil
	}
	for _, directory := range r.owned {
		if err := ValidateComponentContext(directory, callerDir); err != nil {
			return err
		}
	}
	return nil
}

// Load expands definitions and snapshots their declared component files. The
// caller context remains separate; loading never executes tasks.
func (l Loader) Load(ctx context.Context, location string) (_ *Result, retErr error) {
	result := &Result{}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, result.Close())
		}
	}()
	rawFetch := l.Fetch
	if rawFetch == nil {
		rawFetch = l.Client.Pull
	}
	fetched := map[string]*artifact.Document{}
	fetch := func(ctx context.Context, reference string) (*artifact.Document, error) {
		if document := fetched[reference]; document != nil {
			return document, nil
		}
		document, err := rawFetch(ctx, reference)
		if err != nil || document == nil {
			return document, err
		}
		if cached := fetched[document.Reference]; document.Reference != "" && cached != nil {
			document = cached
		}
		fetched[reference] = document
		if document.Reference != "" {
			fetched[document.Reference] = document
		}
		return document, nil
	}
	var document *artifact.Document
	var contextDir, localDir string
	var localRoot *os.Root
	remote := strings.HasPrefix(location, "oci://")
	if remote {
		var err error
		document, err = fetch(ctx, strings.TrimPrefix(location, "oci://"))
		if err != nil {
			return nil, err
		}
		if document == nil {
			return nil, errors.New("nil workflow document")
		}
		if document.Reference == "" {
			return nil, errors.New("workflow is missing canonical reference")
		}
		contextDir, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	} else {
		absolute, err := filepath.Abs(location)
		if err != nil {
			return nil, err
		}
		contextDir = filepath.Dir(absolute)
		data, err := workflow.ReadFile(ctx, absolute)
		if err != nil {
			return nil, err
		}
		document = &artifact.Document{Data: data, Reference: absolute}
		directory, err := filepath.EvalSymlinks(contextDir)
		if err != nil {
			return nil, err
		}
		localRoot, err = os.OpenRoot(directory)
		if err != nil {
			return nil, err
		}
		defer func() { retErr = errors.Join(retErr, localRoot.Close()) }()
		localDir = "."
	}
	active := map[string]bool{}
	if remote {
		active[strings.TrimPrefix(location, "oci://")] = true
		active[document.Reference] = true
	} else if rootPath, err := localWorkflowPath(localRoot, ".", "./"+filepath.Base(document.Reference)); err == nil {
		active["local:"+filepath.ToSlash(rootPath)] = true
	}
	localDocuments := map[string]*artifact.Document{}
	materialized := map[string]string{}
	documents := 1
	declaredTasks := 0
	var load func(*artifact.Document, string, int) (*component, error)
	load = func(document *artifact.Document, localDir string, depth int) (*component, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if depth > 16 {
			return nil, errors.New("workflow import depth exceeds 16")
		}
		w, err := workflow.Decode(bytes.NewReader(document.Data))
		if err != nil {
			return nil, err
		}
		declaredTasks += len(w.Tasks)
		if declaredTasks > 1024 {
			return nil, errors.New("loaded workflow declarations exceed 1024 tasks")
		}
		files := document.Files
		if localDir != "" && len(w.Assets) != 0 {
			assetRoot, openErr := localRoot.OpenRoot(localDir)
			if openErr != nil {
				return nil, fmt.Errorf("component assets: %w", openErr)
			}
			files, err = assets.CollectRoot(assetRoot, w.Assets)
			err = errors.Join(err, assetRoot.Close())
			if err != nil {
				return nil, fmt.Errorf("component assets: %w", err)
			}
		}
		if err := ValidateAssets(w, files); err != nil {
			return nil, err
		}
		componentDir := materialized[document.Reference]
		if componentDir == "" && len(files) != 0 {
			componentDir, err = os.MkdirTemp("", "stoke-component-")
			if err != nil {
				return nil, err
			}
			result.owned = append(result.owned, componentDir)
			if err := assets.Materialize(componentDir, files); err != nil {
				return nil, err
			}
			materialized[document.Reference] = componentDir
		}
		c := &component{document: document, workflow: w, directory: componentDir, imports: map[string]*component{}}
		for _, alias := range sortedKeys(w.Imports) {
			authored := w.Imports[alias]
			documents++
			if documents > 64 {
				return nil, errors.New("workflow import count exceeds 64")
			}
			var child *artifact.Document
			var childDir string
			var keys []string
			if workflow.IsLocalImport(authored) {
				if localDir == "" {
					return nil, errors.New("OCI workflows cannot import local YAML; publish the dependency to OCI and use its digest-pinned reference")
				}
				name, pathErr := localWorkflowPath(localRoot, localDir, authored)
				if pathErr != nil {
					return nil, fmt.Errorf("import %q: %w", alias, pathErr)
				}
				child = localDocuments[name]
				if child == nil {
					data, readErr := workflow.ReadFileAt(ctx, localRoot, name)
					if readErr != nil {
						return nil, fmt.Errorf("import %q: %w", alias, readErr)
					}
					child = &artifact.Document{Data: data, Reference: "local:" + filepath.ToSlash(name)}
					localDocuments[name] = child
				}
				childDir, keys = filepath.Dir(name), []string{child.Reference}
			} else {
				ref := RelocatedReference(authored, document.Dependencies)
				if workflow.IsLocalImport(ref) {
					return nil, errors.New("relocated OCI workflow imports cannot use local paths")
				}
				if _, err := registry.ParseReference(ref); err != nil {
					return nil, errors.New("relocated OCI workflow imports require a registry reference")
				}
				if active[ref] || active[authored] {
					return nil, fmt.Errorf("workflow import cycle at %q", ref)
				}
				child, err = fetch(ctx, ref)
				keys = []string{authored, ref}
			}
			if err != nil {
				return nil, fmt.Errorf("import %q: %w", alias, err)
			}
			if child == nil {
				return nil, errors.New("nil imported workflow document")
			}
			canonical := child.Reference
			if canonical == "" {
				return nil, errors.New("import is missing canonical reference")
			}
			if active[canonical] {
				return nil, fmt.Errorf("workflow import cycle at %q", canonical)
			}
			keys = append(keys, canonical)
			for _, key := range keys {
				active[key] = true
			}
			c.imports[alias], err = load(child, childDir, depth+1)
			for _, key := range keys {
				delete(active, key)
			}
			if err != nil {
				return nil, fmt.Errorf("import %q: %w", alias, err)
			}
		}
		if err := c.validateCalls(ctx); err != nil {
			return nil, err
		}
		return c, nil
	}
	root, err := load(document, localDir, 0)
	if err != nil {
		return nil, err
	}
	expanded := &workflow.Workflow{Version: 1, Tasks: map[string]workflow.Task{}}
	expanded.CacheFrom, expanded.CacheTo = root.workflow.CacheRepositories(nil, nil)
	instance, err := newInstance(ctx, root, l.Inputs, "", expanded)
	if err != nil {
		return nil, err
	}
	targets := sortedKeys(instance.workflow.Tasks)
	for _, name := range targets {
		if _, err := instance.emit(name); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err = expanded.Validate(); err != nil {
		return nil, err
	}
	result.Workflow, result.RootWorkflow, result.Targets = expanded, root.workflow, targets
	result.ContextDir, result.Reference = contextDir, document.Reference
	return result, nil
}

// ValidateAssets requires the manifest payload to match the YAML allowlist.
func ValidateAssets(w *workflow.Workflow, files map[string]assets.File) error {
	if err := assets.Validate(files); err != nil {
		return fmt.Errorf("component assets: %w", err)
	}
	if len(w.Assets) != len(files) {
		return errors.New("component assets do not match the workflow declaration")
	}
	for _, name := range w.Assets {
		if _, exists := files[name]; !exists {
			return fmt.Errorf("declared component asset %q is missing", name)
		}
	}
	return nil
}

// RewriteReferences applies verified delivery mappings to declared registry
// inputs. Component file content, literal arguments and destinations stay literal.
func RewriteReferences(w *workflow.Workflow, mappings map[string]string) {
	for name, reference := range w.Imports {
		if !workflow.IsLocalImport(reference) {
			w.Imports[name] = RelocatedReference(reference, mappings)
		}
	}
	for name, task := range w.Tasks {
		task.Image = RelocatedReference(task.Image, mappings)
		for _, build := range []*workflow.CooprBuild{task.Coopr, task.Environment.Native()} {
			if build == nil {
				continue
			}
			for key, argument := range build.Args {
				if argument.Ref != "" {
					argument.Ref = RelocatedReference(argument.Ref, mappings)
				}
				build.Args[key] = argument
			}
		}
		for _, mounts := range taskMountLists(task) {
			for n := range mounts {
				mounts[n].Image = RelocatedReference(mounts[n].Image, mappings)
				mounts[n].Ref = RelocatedReference(mounts[n].Ref, mappings)
			}
		}
		w.Tasks[name] = task
	}
}

// RelocatedReference maps an authored registry input to its verified delivered
// location. Docker transport spelling shares its declaration's registry spelling;
// other transports and unrecorded references retain their authored values.
func RelocatedReference(reference string, mappings map[string]string) string {
	if destination, ok := mappings[reference]; ok {
		return destination
	}
	if strings.HasPrefix(reference, "docker://") {
		if destination, ok := mappings[strings.TrimPrefix(reference, "docker://")]; ok {
			return destination
		}
	}
	return reference
}

// Dependencies returns direct registry inputs for a Bale inclusion declaration.
// Nested workflow references are discovered from each imported artifact's own
// declaration rather than flattening its independently authored dependency graph.
func Dependencies(w *workflow.Workflow) []string {
	refs := []string{}
	if w == nil {
		return refs
	}
	for _, ref := range w.Imports {
		if !workflow.IsLocalImport(ref) {
			refs = append(refs, strings.TrimPrefix(ref, "docker://"))
		}
	}
	for _, task := range w.Tasks {
		for _, build := range []*workflow.CooprBuild{task.Coopr, task.Environment.Native()} {
			if build == nil {
				continue
			}
			for _, argument := range build.Args {
				if argument.Ref != "" {
					refs = append(refs, argument.Ref)
				}
			}
		}
		inputs := []string{task.Image}
		for _, mount := range task.AllMounts() {
			inputs = append(inputs, mount.Image, mount.Ref)
		}
		for _, ref := range inputs {
			if ref == "" || ref == "scratch" || strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, ".") {
				continue
			}
			if prefix, _, ok := strings.Cut(ref, ":"); ok {
				switch prefix {
				case "oci", "oci-archive", "dir", "docker-archive", "containers-storage":
					continue
				}
			}
			refs = append(refs, strings.TrimPrefix(ref, "docker://"))
		}
	}
	slices.Sort(refs)
	return slices.Compact(refs)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
