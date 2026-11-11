package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/strio-dev/stoke/internal/artifact"
	"github.com/strio-dev/stoke/internal/ci"
	"github.com/strio-dev/stoke/internal/runner"
	"github.com/strio-dev/stoke/internal/workflow"
)

func TestHostFlagAuthorityIsExplicitAndCapabilityTransportIsLiteral(t *testing.T) {
	t.Setenv("STOKE_ALLOW_HOST", "true")
	t.Setenv("STOKE_HOST_CAPABILITIES", `["ungranted"]`)
	var options runner.Options
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	hostExecutionFlags(fs, &options)
	if err := fs.Parse([]string{"--host-capability", "nix", "--host-capabilities-json", `["sandbox"]`}); err != nil {
		t.Fatal(err)
	}
	if options.AllowHost || !slices.Equal(options.HostCapabilities, []string{"nix", "sandbox"}) {
		t.Fatal("environment granted authority or JSON transport altered values")
	}
	if err := fs.Parse([]string{"--allow-host"}); err != nil || !options.AllowHost {
		t.Fatalf("explicit opt-in not parsed: %v", err)
	}
}

func TestHostCapabilityJSONRejectsMalformedAndInjectedValues(t *testing.T) {
	for _, arguments := range [][]string{
		{"--host-capabilities-json", "null"},
		{"--host-capabilities-json", `{}`},
		{"--host-capabilities-json", `["nix",1]`},
		{"--host-capabilities-json", `["nix",null]`},
		{"--host-capabilities-json", `["nix","nix"]`},
		{"--host-capability", "nix", "--host-capabilities-json", `["nix"]`},
		{"--host-capabilities-json", `["$(touch /never)"]`},
		{"--host-capabilities-json", `["nix"] []`},
		{"--host-capability", "nix\n--allow-host"},
	} {
		var options runner.Options
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		hostExecutionFlags(fs, &options)
		if err := fs.Parse(arguments); err == nil {
			t.Fatalf("invalid host capability grant accepted: %q", arguments)
		}
		if options.AllowHost {
			t.Fatal("capability transport granted execution authority")
		}
	}
}

const cliHostFixture = `version: 1
tasks:
  check:
    platform: linux/amd64
    host: {tools: [sh], capabilities: [provisioned-nix]}
    steps: [{run: [sh, -c, true]}]
`

func TestLocalHostRunCannotUseEnvironmentAsAuthority(t *testing.T) {
	root := t.TempDir()
	workflowPath := filepath.Join(root, "stoke.yaml")
	if err := os.WriteFile(workflowPath, []byte(cliHostFixture), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STOKE_ALLOW_HOST", "true")
	output := filepath.Join(t.TempDir(), "not-created")
	var out, diagnostics bytes.Buffer
	code := run([]string{"run", "-f", workflowPath, "--output", output, "--coopr-binary", "/missing/coopr"}, &out, &diagnostics)
	if code != 1 || !strings.Contains(diagnostics.String(), "allow-host") {
		t.Fatalf("host authority not denied before engine: code=%d out=%s error=%s", code, &out, &diagnostics)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("denied host execution created output state")
	}
}

func TestCIHostPlanDoesNotGrantExecutionOrCapabilities(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCIFile(t, "stoke.yaml", cliHostFixture)
	if code, _, stderr := invokeCI([]string{"ci", "generate"}); code != 0 {
		t.Fatal(stderr)
	}
	p, err := ci.Decode(bytes.NewReader(readCIFile(t, ".stoke/ci-plan.json")))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Host) != 1 || len(p.Engine.Capabilities) != 0 {
		t.Fatal("host declaration was lost or acquired Coopr requirements")
	}
	t.Setenv("STOKE_ALLOW_HOST", "true")
	if code, _, stderr := invokeCI([]string{"ci", "run", "--job", p.Groups[0].Jobs[0].ID}); code != 1 || !strings.Contains(stderr, "--allow-host") {
		t.Fatalf("generated plan or environment granted authority: exit=%d error=%s", code, stderr)
	}
	w, err := workflow.Decode(strings.NewReader(cliHostFixture))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runCIExchange(context.Background(), w, p, p.Groups[0].Jobs[0], runner.Options{}, artifact.Client{}, ciExchangeOptions{}); err == nil || !strings.Contains(err.Error(), "--allow-host") {
		t.Fatalf("direct CI exchange bypassed host authority preflight: %v", err)
	}
	if err := validateCIHostAuthority(p, p.Groups[0].Jobs[0], runner.Options{AllowHost: true}); err != nil {
		t.Fatal(err)
	}
}
