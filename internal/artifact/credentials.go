package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/oras-project/oras-go/v3/registry/remote"
	"github.com/oras-project/oras-go/v3/registry/remote/config"
	"github.com/oras-project/oras-go/v3/registry/remote/credentials"
	"github.com/oras-project/oras-go/v3/registry/remote/properties"
	nativeconfig "go.podman.io/image/v5/pkg/docker/config"
	"oras.land/oras-go/v2/registry"
)

func registryCredential(path string, ref registry.Reference) (credentials.CredentialFunc, error) {
	if path == "" {
		path = os.Getenv("REGISTRY_AUTH_FILE")
	}
	required := path != ""
	if dir := os.Getenv("DOCKER_CONFIG"); !required && dir != "" {
		path = filepath.Join(dir, "config.json")
	}
	host := strings.ToLower(ref.Host())
	if host == "registry-1.docker.io" {
		host = "docker.io"
	}
	scope := properties.Resource{Registry: host, Path: ref.Repository}
	lookup := nativeCredential
	if path != "" {
		info, err := os.Stat(path)
		switch {
		case err == nil:
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("registry auth file %q is not a regular file", path)
			}
			lookup, err = selectedCredential(path, host, !required)
			if err != nil {
				return nil, err
			}
		case required || !errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("inspect registry auth file %q: %w", path, err)
		}
	}
	return func(ctx context.Context, res properties.Resource) (credentials.Credential, error) {
		if err := ctx.Err(); err != nil {
			return credentials.EmptyCredential, err
		}
		if res.Registry != scope.Registry || (res.Path != "" && res.Path != scope.Path) {
			return credentials.EmptyCredential, nil
		}
		return lookup(ctx, res)
	}, nil
}

func selectedCredential(path, host string, dockerDefault bool) (credentials.CredentialFunc, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	hostKey := remote.ServerAddressFromRegistry(host)
	if dockerDefault && !cfg.IsAuthConfigured() {
		if store, ok := credentials.NewDefaultNativeStore(); ok {
			return func(ctx context.Context, _ properties.Resource) (credentials.Credential, error) {
				return store.Get(ctx, hostKey)
			}, nil
		}
	}

	// Upstream hierarchical lookup uses canonical keys. Normalize legacy
	// host entries only in the loaded config, after ruling out a helper.
	normalizeHost := sync.OnceValue(func() error {
		value, err := cfg.GetAuthConfig(host)
		if err == nil && value == (credentials.AuthConfig{}) && hostKey != host {
			value, err = cfg.GetAuthConfig(hostKey)
		}
		if err != nil || value == (credentials.AuthConfig{}) {
			return err
		}
		cfg.RemoveAuthConfig(host)
		return cfg.SetAuthConfig(host, value)
	})
	return func(ctx context.Context, res properties.Resource) (credentials.Credential, error) {
		key := hostKey
		// Use the configured helper's own key so a failure remains an error.
		// An empty helper entry explicitly selects the file.
		helper := cfg.CredentialsStore()
		for name := res.String(); name != res.Registry; name = name[:strings.LastIndexByte(name, '/')] {
			if scoped, found := cfg.GetCredentialHelper(name); found {
				helper, key = scoped, name
				break
			}
		}
		if key == hostKey {
			if configured, found := cfg.GetCredentialHelper(key); found {
				helper = configured
			} else if configured, found := cfg.GetCredentialHelper(res.Registry); found {
				helper, key = configured, res.Registry
			}
		}
		if helper != "" {
			return credentials.NewNativeStore(helper).Get(ctx, key)
		}

		if err := normalizeHost(); err != nil {
			return credentials.EmptyCredential, err
		}
		value, err := cfg.GetAuthConfigHierarchical(res.String())
		if err != nil {
			return credentials.EmptyCredential, err
		}
		return credentials.NewCredential(value)
	}, nil
}

func nativeCredential(ctx context.Context, res properties.Resource) (credentials.Credential, error) {
	native, err := nativeconfig.GetCredentials(nil, res.String())
	if err != nil {
		return credentials.EmptyCredential, err
	}
	credential := credentials.Credential{
		Username: native.Username, Password: native.Password, RefreshToken: native.IdentityToken,
	}
	if credential != credentials.EmptyCredential {
		return credential, nil
	}
	// containers/image does not read Docker's global credsStore.
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return credentials.EmptyCredential, err
	}
	return store.Get(ctx, remote.ServerAddressFromRegistry(res.Registry))
}
