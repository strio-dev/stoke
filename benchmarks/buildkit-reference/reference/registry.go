package main

import (
	"context"
	"errors"
	"os"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
	"github.com/docker/cli/cli/config/types"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/moby/buildkit/util/resolver"
)

type registryOptions struct {
	Hosts docker.RegistryHosts
	Auth  session.Attachable
}

// Use only the explicitly supplied job authfile. The upstream provider's
// token-authority seeds are isolated in an owned temporary Docker config dir.
func registryAccess(authfile string) (registryOptions, func(), error) {
	info, err := os.Lstat(authfile)
	if err != nil {
		return registryOptions{}, nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return registryOptions{}, nil, errors.New("authfile must be a private regular file")
	}
	f, err := os.Open(authfile)
	if err != nil {
		return registryOptions{}, nil, err
	}
	cf := configfile.New(authfile)
	if err := errors.Join(cf.LoadFromReader(f), f.Close()); err != nil {
		return registryOptions{}, nil, err
	}
	if cf.CredentialsStore != "" || len(cf.CredentialHelpers) != 0 {
		return registryOptions{}, nil, errors.New("benchmark requires explicit inline auth, without credential helpers")
	}
	tokenDir, err := os.MkdirTemp("", "buildkit-reference-auth-")
	if err != nil {
		return registryOptions{}, nil, err
	}
	config.SetDir(tokenDir)
	cleanup := func() { _ = os.RemoveAll(tokenDir) }
	auth := authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{AuthConfigProvider: func(_ context.Context, host string, _ []string, _ authprovider.ExpireCachedAuthCheck) (types.AuthConfig, error) {
		if host == authprovider.DockerHubRegistryHost || host == "docker.io" {
			host = authprovider.DockerHubConfigfileKey
		}
		return cf.GetAuthConfig(host)
	}})
	return registryOptions{Hosts: resolver.NewRegistryConfig(nil), Auth: auth}, cleanup, nil
}
