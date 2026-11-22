package main

import (
	"flag"
	"fmt"
	"github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/runner"
	"os"
	"strings"
)

func cacheFlags(fs *flag.FlagSet, options *runner.Options) {
	for _, direction := range []struct {
		name string
		list *[]string
	}{
		{"cache-from", &options.CacheFrom}, {"cache-to", &options.CacheTo},
	} {
		fs.Func(direction.name, "registry cache repository (repeatable; empty clears this direction)", func(value string) error {
			if value == "" {
				*direction.list = []string{}
				return nil
			}
			if err := engine.ValidateCacheRepository(value); err != nil {
				return err
			}
			*direction.list = append(*direction.list, value)
			return nil
		})
	}
}

// Only execution reads environment overrides; generation remains deterministic.
func cacheEnvironment(options *runner.Options) error {
	for _, direction := range []struct {
		name string
		list *[]string
	}{
		{"STOKE_CACHE_FROM", &options.CacheFrom}, {"STOKE_CACHE_TO", &options.CacheTo},
	} {
		if *direction.list != nil {
			continue
		}
		value, set := os.LookupEnv(direction.name)
		if !set {
			continue
		}
		*direction.list = []string{}
		if value == "" {
			continue
		}
		for _, repository := range strings.Split(value, ",") {
			if err := engine.ValidateCacheRepository(repository); err != nil {
				return fmt.Errorf("%s: %w", direction.name, err)
			}
			*direction.list = append(*direction.list, repository)
		}
	}
	return nil
}
