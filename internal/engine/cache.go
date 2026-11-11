package engine

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"oras.land/oras-go/v2/registry"
)

// ValidateCacheRepository accepts a bare, explicit registry repository. Cache
// entries select their own references; a caller must not provide a tag/digest.
func ValidateCacheRepository(repository string) error {
	if !utf8.ValidString(repository) || strings.TrimSpace(repository) != repository {
		return errors.New("cache repository must be a UTF-8 registry repository without whitespace")
	}
	if strings.ContainsAny(repository, ",=") {
		return errors.New("cache repository must use bare REGISTRY/REPOSITORY syntax")
	}
	reference, err := registry.ParseReference(repository)
	if err != nil {
		return fmt.Errorf("invalid cache repository: %w", err)
	}
	if strings.HasPrefix(repository, ".") || strings.HasSuffix(reference.Registry, ":") || (!strings.ContainsAny(reference.Registry, ".:") && reference.Registry != "localhost") {
		return errors.New("cache repository requires an explicit registry")
	}
	if repository != reference.Registry+"/"+reference.Repository {
		return errors.New("cache repository must not include a tag or digest")
	}
	return nil
}

// ValidateRegistryCaches keeps malformed cache configuration from reaching an
// engine request, including direct wire-client use outside the workflow runner.
func ValidateRegistryCaches(cache CacheOptions) error {
	for _, direction := range []struct {
		name         string
		repositories []string
	}{{"cache-from", cache.RegistryFrom}, {"cache-to", cache.RegistryTo}} {
		for _, repository := range direction.repositories {
			if err := ValidateCacheRepository(repository); err != nil {
				return fmt.Errorf("%s: %w", direction.name, err)
			}
		}
	}
	return nil
}
