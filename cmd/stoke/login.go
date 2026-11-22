package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/oras-project/oras-go/v3/registry/remote"
	"github.com/oras-project/oras-go/v3/registry/remote/config"
	"go.podman.io/common/pkg/auth"
	"go.podman.io/image/v5/types"
	"oras.land/oras-go/v2/registry"
)

const registryHelp = `Usage: stoke registry <command>

Commands:
  login  Log in to an OCI registry using shared credentials

Use stoke registry login --help for options.`

func runRegistry(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h")) {
		if _, err := fmt.Fprintln(out, registryHelp); err != nil {
			return 1
		}
		return 0
	}
	if args[0] == "login" {
		return runLogin(ctx, args[1:], out, errOut)
	}
	_, _ = fmt.Fprintf(errOut, "stoke registry: unknown command %q\n", args[0])
	return 2
}

func runLogin(ctx context.Context, args []string, out, errOut io.Writer) int {
	options := auth.LoginOptions{Stdin: os.Stdin, Stdout: out}
	fs := flag.NewFlagSet("registry login", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(errOut, "Usage: stoke registry login [options] REGISTRY\n\nAuthenticate to an OCI registry over verified TLS. Prompts for missing credentials.")
		fs.PrintDefaults()
	}
	fs.StringVar(&options.Username, "username", "", "registry username")
	fs.StringVar(&options.Username, "u", "", "shorthand for --username")
	fs.BoolVar(&options.StdinPassword, "password-stdin", false, "read the password from stdin (requires --username)")
	fs.StringVar(&options.AuthFile, "authfile", "", "shared registry credential file (overrides REGISTRY_AUTH_FILE and DOCKER_CONFIG)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 || (registry.Reference{Registry: fs.Arg(0)}).ValidateRegistry() != nil {
		_, _ = fmt.Fprintln(errOut, "stoke registry login requires one registry host, optionally with a port")
		return 2
	}
	err := checkLoginStore(options.AuthFile, fs.Arg(0))
	if err == nil {
		system := &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolFalse}
		err = auth.Login(ctx, system, &options, fs.Args())
	}
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "stoke registry login: %v\n", err)
		return 1
	}
	return 0
}

func checkLoginStore(path, host string) error {
	helperKey := host
	if path == "" {
		path = auth.GetDefaultAuthFile()
		if host == "docker.io" && os.Getenv("REGISTRY_AUTH_FILE") == "" {
			// The native writer uses Docker's legacy key in DOCKER_CONFIG.
			helperKey = remote.ServerAddressFromRegistry(host)
		}
	}
	if path == "" {
		return nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	// The shared native writer honors per-registry helpers but can write
	// inline credentials into a Docker global-helper file.
	if cfg.CredentialsStore() != "" && cfg.CredentialHelpers()[helperKey] == "" {
		return errors.New("the selected auth file uses Docker's global credsStore; use docker login or choose a separate --authfile")
	}
	return nil
}
