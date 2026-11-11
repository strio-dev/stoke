package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cooprwire "github.com/strio-dev/coopr/api"
	coopr "github.com/strio-dev/stoke/internal/engine"
)

func TestMountSourcesLowerThroughCooprAndResolveCallerPaths(t *testing.T) {
	pinned := "registry.test/artifact@sha256:" + strings.Repeat("a", 64)
	w := fixture(t, `version: 1
tasks:
  base: {image: scratch}
  test:
    from: base
    contexts: {fixtures: ./fixtures}
    mounts: [{type: context, context: fixtures, source: data, target: /fixtures}]
    steps:
      - run: [check]
        mounts:
          - {type: host, source: ./live, target: /live, read-only: false, relabel: shared}
          - {type: cache, id: compiler, sharing: locked, target: /cache}
          - {type: image, image: registry.test/image:tag, source: /tool, target: /tool}
          - {type: stage, from: base, source: /built, target: /built, read-only: false}
          - {type: artifact, ref: '`+pinned+`', source: file.tar, target: /file.tar}
`)
	opts := testOptions(t)
	for _, directory := range []string{"fixtures", "live"} {
		if err := os.Mkdir(filepath.Join(opts.ContextDir, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	_, err := runLiteral(t.Context(), w, nil, opts, func(_ context.Context, p coopr.Program, o coopr.Options) (coopr.Result, error) {
		if p.Output == "test" {
			if o.Cache.MountDir != filepath.Join(opts.CacheDir, "mounts") {
				t.Errorf("cache volumes did not receive a stable native parent: %+v", o.Cache)
			}
			if o.Contexts["fixtures"] != filepath.Join(opts.ContextDir, "fixtures") {
				t.Errorf("named context not rooted in caller inputs: %v", o.Contexts)
			}
			operation := p.States[0].Operations[0]
			if operation.Kind != "run" {
				t.Fatalf("mounts attached to non-run operation: %+v", operation)
			}
			mounts := operation.Mounts
			if len(mounts) != 6 {
				t.Fatalf("mounts=%+v", mounts)
			}
			if mounts[0].From.Context != "fixtures" || !mounts[0].ReadOnly || mounts[1].HostPath != filepath.Join(opts.ContextDir, "live") || mounts[1].Source != "" || mounts[1].ReadOnly || mounts[1].Relabel != "shared" {
				t.Errorf("context/live semantics lost: %+v", mounts[:2])
			}
			if mounts[2].Type != "cache" || mounts[2].ID != "compiler" || mounts[2].Sharing != "locked" || mounts[2].ReadOnly {
				t.Errorf("cache mount changed: %+v", mounts[2])
			}
			if mounts[3].From.Image != "registry.test/image:tag" || mounts[4].From.Image != "containers-storage:base-image" || mounts[4].ReadOnly || mounts[5].From.Image != pinned || !mounts[5].ReadOnly {
				t.Errorf("image/stage/artifact source changed: %+v", mounts[3:])
			}
		}
		return coopr.Result{Platform: "linux/amd64", ImageID: string(p.Output) + "-image"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.Tasks["test"].Contexts["fixtures"] != "./fixtures" || w.Tasks["test"].Steps[0].Mounts[0].Source != "./live" {
		t.Fatal("path resolution changed the authored workflow")
	}
}

func TestMissingMountDirectoriesFailBeforeEngineStartup(t *testing.T) {
	for _, declaration := range []string{
		"contexts: {data: ./absent}",
		"mounts: [{type: host, source: ./absent, target: /data}]",
	} {
		w := fixture(t, "version: 1\ntasks:\n  test:\n    image: scratch\n    "+declaration+"\n    steps: [{run: [check]}]\n")
		opts := testOptions(t)
		opts.CooprBinary = "/not/an/engine"
		if _, err := Run(t.Context(), w, nil, opts); err == nil || !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("missing mount did not fail clearly before execution: %v", err)
		}
	}
}

func TestMountFeatureCapabilityIsNegotiated(t *testing.T) {
	w := fixture(t, "version: 1\ntasks: {test: {image: scratch, steps: [{run: [check], mounts: [{type: cache, target: /cache}]}]}}\n")
	plan, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := testCapabilities(t, plan, cooprwire.Info{Capabilities: []string{"execute"}}, Options{}); err == nil || !strings.Contains(err.Error(), "run-mounts") {
		t.Fatalf("missing mount capability accepted: %v", err)
	}
	if err := testCapabilities(t, plan, cooprwire.Info{Capabilities: []string{"execute", "run-mounts"}}, Options{}); err != nil {
		t.Fatal(err)
	}
}
