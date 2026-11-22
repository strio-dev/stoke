package workflow

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

func (w *Workflow) validateGroups() error {
	for _, name := range slices.Sorted(maps.Keys(w.Groups)) {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("invalid task group name %q", name)
		}
		if _, exists := w.Tasks[name]; exists {
			return fmt.Errorf("task group %q conflicts with a task of the same name", name)
		}
		if strings.Contains(name, "--matrix-") {
			return fmt.Errorf("task group %q uses the reserved matrix-variant separator", name)
		}
		members := w.Groups[name]
		if len(members) == 0 {
			return fmt.Errorf("task group %q requires at least one task", name)
		}
		seen := map[string]bool{}
		for _, member := range members {
			if _, exists := w.Tasks[member]; !exists {
				return fmt.Errorf("task group %q selects unknown task %q", name, member)
			}
			if seen[member] {
				return fmt.Errorf("task group %q repeats task %q", name, member)
			}
			seen[member] = true
		}
	}
	for _, selection := range []struct{ field, name string }{{"default-run", w.DefaultRun}, {"default-ci", w.DefaultCI}} {
		if _, exists := w.Groups[selection.name]; selection.name != "" && !exists {
			return fmt.Errorf("%s selects unknown task group %q", selection.field, selection.name)
		}
	}
	return nil
}

// ExpandGroups keeps task selectors in authored order. The normal dependency
// planner still owns closure, gates and matrix expansion.
func (w *Workflow) ExpandGroups(selectors []string) []string {
	if len(w.Groups) == 0 || len(selectors) == 0 {
		return selectors
	}
	var expanded []string
	seen := map[string]bool{}
	for _, selector := range selectors {
		members, isGroup := w.Groups[selector]
		if !isGroup {
			members = []string{selector}
		}
		for _, member := range members {
			if !seen[member] {
				seen[member] = true
				expanded = append(expanded, member)
			}
		}
	}
	return expanded
}
