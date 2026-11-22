package workflow

import (
	"github.com/strio-dev/stoke/internal/engine"
	"slices"
)

func (w *Workflow) validateCaches() error {
	return engine.ValidateRegistryCaches(engine.CacheOptions{RegistryFrom: w.CacheFrom, RegistryTo: w.CacheTo})
}

// CacheRepositories applies owning workflow defaults only to unset directions.
// A nonnil empty list deliberately clears a direction.
func (w *Workflow) CacheRepositories(from, to []string) ([]string, []string) {
	if w != nil {
		if from == nil {
			from = w.CacheFrom
		}
		if to == nil {
			to = w.CacheTo
		}
	}
	return slices.Clone(from), slices.Clone(to)
}
