package main

import (
	"bytes"
	"flag"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/strio-dev/stoke/internal/runner"
	"github.com/strio-dev/stoke/internal/workflow"
)

func TestCacheDirectionPrecedenceAndClearing(t *testing.T) {
	for _, tc := range []struct {
		name             string
		args             []string
		from, to         *string
		wantFrom, wantTo []string
	}{
		{name: "YAML", wantFrom: []string{"registry.test/yaml-read"}, wantTo: []string{"registry.test/yaml-write"}},
		{name: "environment", from: cacheString("registry.test/a,registry.test/b"), to: cacheString(""), wantFrom: []string{"registry.test/a", "registry.test/b"}},
		{name: "write only", from: cacheString(""), wantTo: []string{"registry.test/yaml-write"}},
		{name: "CLI outranks invalid environment", args: []string{"--cache-from=registry.test/cli", "--cache-to="}, from: cacheString("bad,,list"), to: cacheString("bad"), wantFrom: []string{"registry.test/cli"}},
		{name: "independent CLI clear", args: []string{"--cache-from="}, to: cacheString("registry.test/env-write"), wantTo: []string{"registry.test/env-write"}},
		{name: "repeat and reset", args: []string{"--cache-to=registry.test/old", "--cache-to=", "--cache-to=registry.test/new"}, wantFrom: []string{"registry.test/yaml-read"}, wantTo: []string{"registry.test/new"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range map[string]*string{"STOKE_CACHE_FROM": tc.from, "STOKE_CACHE_TO": tc.to} {
				t.Setenv(key, "")
				if value == nil { // Unset, distinct from a set-empty override.
					if err := os.Unsetenv(key); err != nil {
						t.Fatal(err)
					}
				} else {
					t.Setenv(key, *value)
				}
			}
			var opts runner.Options
			fs := flag.NewFlagSet("cache", flag.ContinueOnError)
			fs.SetOutput(&bytes.Buffer{})
			cacheFlags(fs, &opts)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			if err := cacheEnvironment(&opts); err != nil {
				t.Fatal(err)
			}
			root := workflow.Workflow{CacheFrom: []string{"registry.test/yaml-read"}, CacheTo: []string{"registry.test/yaml-write"}}
			from, to := root.CacheRepositories(opts.CacheFrom, opts.CacheTo)
			if !slices.Equal(from, tc.wantFrom) || !slices.Equal(to, tc.wantTo) {
				t.Fatalf("from=%v to=%v", from, to)
			}
		})
	}
	for _, value := range []string{"registry.test/a,", "registry.test/a, registry.test/b", "[registry.test/a]"} {
		t.Setenv("STOKE_CACHE_FROM", value)
		if err := cacheEnvironment(&runner.Options{}); err == nil || !strings.Contains(err.Error(), "STOKE_CACHE_FROM") {
			t.Fatalf("ambiguous environment accepted: %q %v", value, err)
		}
	}
}

func cacheString(s string) *string { return &s }

func TestCICachePolicyGenerationAndRuntime(t *testing.T) {
	t.Chdir(t.TempDir())
	const yaml = "version: 1\ncache-from: [registry.test/yaml-read]\ncache-to: [registry.test/yaml-write]\ntasks: {build: {image: scratch}}\n"
	writeCIFile(t, "stoke.yaml", yaml)
	t.Setenv("STOKE_CACHE_FROM", "invalid generator ambient value")
	t.Setenv("STOKE_CACHE_TO", "")
	args := []string{"ci", "generate", "github", "build", "--cache-to="}
	if code, _, stderr := invokeCI(args); code != 0 {
		t.Fatal(stderr)
	}
	before := readCIFile(t, stageAction("build"))
	if !bytes.Contains(before, []byte("stoke run --ci --cache-to= build")) || bytes.Contains(before, []byte("--cache-from")) {
		t.Fatalf("explicit clear or ambient isolation lost: %s", before)
	}
	t.Setenv("STOKE_CACHE_FROM", "registry.test/different-generator-host")
	if code, _, stderr := invokeCI(append(args, "--check")); code != 0 || !bytes.Equal(before, readCIFile(t, stageAction("build"))) {
		t.Fatal("ambient environment changed generation", stderr)
	}
	runArgs := []string{"run", "--ci", "--cache-to=", "--coopr-binary", "/missing/coopr"}
	t.Setenv("STOKE_CACHE_FROM", "bad runtime environment")
	if code, _, stderr := invokeCI(append(runArgs, "build")); code != 1 || !strings.Contains(stderr, "STOKE_CACHE_FROM") {
		t.Fatal("runtime did not read its own override", code, stderr)
	}
	t.Setenv("STOKE_CACHE_TO", "bad runtime write environment")
	if code, _, stderr := invokeCI(append(runArgs, "--cache-from=registry.test/direct-read", "build")); code != 1 || !strings.Contains(stderr, "/missing/coopr") {
		t.Fatal("direct CLI read and write clear did not override the runtime environment", code, stderr)
	}
	if err := os.Remove("stoke.yaml"); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := invokeCI(append(runArgs, "build")); code != 1 || !strings.Contains(stderr, "stoke.yaml") {
		t.Fatal("CI runner accepted a missing checkout", code, stderr)
	}
}

func TestRegistryCacheFlagsValidateBeforeLoadingWorkflow(t *testing.T) {
	for _, flag := range []string{"--cache-from", "--cache-to"} {
		for _, value := range []string{"team/cache", "registry.example/cache:tag", "registry.example/cache:", "https://registry.example/cache", "oci:/tmp/cache", "type=local,src=/cache", "type=registry,ref=registry.example/cache"} {
			t.Run(flag+value, func(t *testing.T) {
				var out, errOut bytes.Buffer
				code := run([]string{"run", flag, value, "-f", "/missing/workflow.yaml", "--coopr-binary", "/missing/coopr"}, &out, &errOut)
				if code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), strings.TrimPrefix(flag, "--")) || strings.Contains(errOut.String(), "no such file") {
					t.Fatalf("code=%d stdout=%s stderr=%s", code, &out, &errOut)
				}
			})
		}
	}
}

func TestRegistryCacheFlagsAreRepeatableRunOptions(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"run", "--cache-from", "registry.example/one", "--cache-from", "registry.example/two", "--cache-to", "registry.example/write-one", "--cache-to", "registry.example/write-two", "-f", "/missing/workflow.yaml"}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "no such file") {
		t.Fatalf("repeat flags were not accepted: code=%d stdout=%s stderr=%s", code, &out, &errOut)
	}
	for _, command := range []string{"check", "plan", "publish", "pull"} {
		out.Reset()
		errOut.Reset()
		if code := run([]string{command, "--cache-from", "registry.example/read"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "flag provided but not defined") {
			t.Fatalf("%s accepted a run cache option: code=%d stderr=%s", command, code, &errOut)
		}
	}
}
