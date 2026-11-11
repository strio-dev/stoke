package workflow

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// Mount selects one RUN input. Context inputs are snapshotted; host inputs are
// explicit live directory binds. Image/stage writes use disposable RUN overlays,
// while artifact content is immutable and cache contents are only acceleration.
type Mount struct {
	Type     string `yaml:"type"`
	Target   string `yaml:"target"`
	Source   string `yaml:"source,omitempty"`
	Context  string `yaml:"context,omitempty"`
	Image    string `yaml:"image,omitempty"`
	From     string `yaml:"from,omitempty"`
	Ref      string `yaml:"ref,omitempty"`
	ID       string `yaml:"id,omitempty"`
	Sharing  string `yaml:"sharing,omitempty"`
	Relabel  string `yaml:"relabel,omitempty"`
	ReadOnly *bool  `yaml:"read-only,omitempty"`
}

// IsReadOnly gives content inputs a read-only default. An explicit writable
// image/stage mount modifies a per-RUN overlay, never its immutable source.
func (m Mount) IsReadOnly() bool {
	if m.ReadOnly != nil {
		return *m.ReadOnly
	}
	return m.Type != "cache"
}

// AllMounts returns task defaults followed by each step's additional mounts.
func (t Task) AllMounts() []Mount {
	mounts := append([]Mount(nil), t.Mounts...)
	for _, step := range t.Steps {
		mounts = append(mounts, step.Mounts...)
	}
	return mounts
}

func cloneMounts(mounts []Mount) []Mount {
	cloned := append([]Mount(nil), mounts...)
	for i := range cloned {
		if cloned[i].ReadOnly != nil {
			readOnly := *cloned[i].ReadOnly
			cloned[i].ReadOnly = &readOnly
		}
	}
	return cloned
}

func stepMounts(steps []Step) [][]Mount {
	mounts := make([][]Mount, 0, len(steps))
	for _, step := range steps {
		mounts = append(mounts, step.Mounts)
	}
	return mounts
}

func (t Task) validateMounts(w *Workflow) error {
	for name, directory := range t.Contexts {
		if !namePattern.MatchString(name) || name == ComponentContext || name == "build" {
			return fmt.Errorf("invalid or reserved named context %q", name)
		}
		if directory == "" || !validInputString(directory) || strings.ContainsAny(directory, "\r\n") {
			return fmt.Errorf("named context %q requires a nonempty local directory path", name)
		}
	}
	if t.Uses != "" && (len(t.Mounts) != 0 || len(t.Contexts) != 0) {
		return errors.New("uses cannot override template mounts or named contexts")
	}
	if (t.Coopr != nil || t.Publish != nil) && len(t.Mounts) != 0 {
		return errors.New("task mounts require ordinary RUN steps; native definitions retain their own mounts")
	}
	if t.Publish != nil && len(t.Contexts) != 0 {
		return errors.New("publication cannot declare named contexts")
	}
	validate := func(mounts []Mount, targets map[string]bool) error {
		for _, mount := range mounts {
			if err := mount.validate(t, w); err != nil {
				return err
			}
			if targets[mount.Target] {
				return fmt.Errorf("duplicate mount target %q in RUN", mount.Target)
			}
			targets[mount.Target] = true
		}
		return nil
	}
	defaults := map[string]bool{}
	if err := validate(t.Mounts, defaults); err != nil {
		return err
	}
	hasRun := false
	for index, step := range t.Steps {
		if step.Run == nil {
			if len(step.Mounts) != 0 {
				return fmt.Errorf("step %d: mounts require run", index+1)
			}
			continue
		}
		hasRun = true
		targets := map[string]bool{}
		for target := range defaults {
			targets[target] = true
		}
		if err := validate(step.Mounts, targets); err != nil {
			return fmt.Errorf("step %d: %w", index+1, err)
		}
	}
	if len(t.Mounts) != 0 && !hasRun {
		return errors.New("task mounts require at least one RUN step")
	}
	return nil
}

func (m Mount) validate(task Task, w *Workflow) error {
	for _, value := range []string{m.Type, m.Target, m.Source, m.Context, m.Image, m.From, m.Ref, m.ID, m.Sharing, m.Relabel} {
		if !validInputString(value) || strings.ContainsAny(value, ",\r\n") {
			return errors.New("mount fields must be UTF-8 without comma, newline or NUL")
		}
	}
	if !path.IsAbs(m.Target) || path.Clean(m.Target) != m.Target {
		return errors.New("mount target must be a normalized absolute container path")
	}
	if m.Type != "cache" && (m.ID != "" || m.Sharing != "") {
		return errors.New("mount id and sharing apply only to cache volumes")
	}
	if m.Relabel != "" && (m.Type != "host" || (m.Relabel != "shared" && m.Relabel != "private")) {
		return errors.New("relabel requires a live host mount and must be shared or private")
	}
	allowed := func(context, image, from, ref bool) bool {
		return (context || m.Context == "") && (image || m.Image == "") && (from || m.From == "") && (ref || m.Ref == "")
	}
	switch m.Type {
	case "context":
		if !allowed(true, false, false, false) {
			return errors.New("context mount cannot select image, stage or artifact inputs")
		}
		if m.Context == ComponentContext {
			if task.ComponentDir == "" && len(w.Assets) == 0 {
				return errors.New("component context mount requires declared assets")
			}
		} else if m.Context != "" && task.Contexts[m.Context] == "" {
			return fmt.Errorf("unknown named context %q", m.Context)
		}
		if path.IsAbs(m.Source) {
			return errors.New("context mount source must be relative to its snapshot")
		}
	case "host":
		if !allowed(false, false, false, false) || m.Source == "" {
			return errors.New("host mount requires only a local source directory")
		}
		return nil // Explicit live host paths may be outside the caller context.
	case "cache":
		if !allowed(false, false, false, false) || m.Source != "" {
			return errors.New("cache volume cannot select content inputs")
		}
		if m.Sharing == "private" {
			return errors.New("private cache sharing is not supported by Coopr's native Buildah backend; use shared or locked")
		}
		if m.Sharing != "" && m.Sharing != "shared" && m.Sharing != "locked" {
			return errors.New("cache sharing must be shared or locked")
		}
		return nil
	case "image":
		if !allowed(false, true, false, false) || m.Image == "" || m.Image == "scratch" {
			return errors.New("image mount requires only a nonempty image reference")
		}
	case "stage":
		if !allowed(false, false, true, false) || !namePattern.MatchString(m.From) {
			return errors.New("stage mount requires only a prior task name in from")
		}
	case "artifact":
		if !allowed(false, false, false, true) || m.Ref == "" {
			return errors.New("artifact mount requires only a digest-pinned ref")
		}
		if err := (Argument{Ref: m.Ref}).validate(); err != nil {
			return fmt.Errorf("artifact mount: %w", err)
		}
		if !m.IsReadOnly() {
			return errors.New("artifact mounts must be read-only")
		}
	default:
		return fmt.Errorf("unsupported mount type %q", m.Type)
	}
	for _, part := range strings.Split(m.Source, "/") {
		if part == ".." {
			return errors.New("mount source must not traverse outside its input view")
		}
	}
	return nil
}
