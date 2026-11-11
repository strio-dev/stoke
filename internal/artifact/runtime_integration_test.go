package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/strio-dev/bale"
	cooprwire "github.com/strio-dev/coopr/api"
	"github.com/strio-dev/stoke/internal/assets"
	"github.com/strio-dev/stoke/internal/engine"
	"oras.land/oras-go/v2/registry/remote"
)

type ociRuntimeTask struct {
	Name       string                                               `json:"name"`
	Task       string                                               `json:"task"`
	Status     string                                               `json:"status"`
	Filesystem string                                               `json:"filesystem"`
	Matrix     map[string]string                                    `json:"matrix"`
	Cache      *struct{ Hits, Misses, Stored, Skipped, Errors int } `json:"cache"`
}

type ociRuntimeReport struct {
	Status    string           `json:"status"`
	Directory string           `json:"directory"`
	Tasks     []ociRuntimeTask `json:"tasks"`
}

// TestPackagedWorkflowWithCooprBale exercises actual binaries and rootless
// execution. Short/hermetic checks deliberately leave this host path separate.
func TestPackagedWorkflowWithCooprBale(t *testing.T) {
	if testing.Short() {
		t.Skip("requires configured rootless host and packaged Stoke/Coopr executables")
	}
	binary := os.Getenv("STOKE_BINARY")
	cooprBinary := os.Getenv("STOKE_COOPR_BINARY")
	busybox := os.Getenv("STOKE_SMOKE_BUSYBOX")
	for label, value := range map[string]string{"STOKE_BINARY": binary, "STOKE_COOPR_BINARY": cooprBinary, "STOKE_SMOKE_BUSYBOX": busybox} {
		if value == "" {
			t.Fatalf("%s must select the configured acceptance executable", label)
		}
		if _, err := os.Stat(value); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 7*time.Minute)
	defer cancel()
	work, err := os.MkdirTemp("", "stoke-oci-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Retained OCI acceptance evidence: %s", work)
	checks := []string{}
	reports := map[string]string{}
	defer func() {
		data, _ := json.MarshalIndent(map[string]any{"passed": !t.Failed(), "evidence": work, "checks": checks, "reports": reports}, "", "  ")
		if err := os.WriteFile(filepath.Join(work, "summary.json"), append(data, '\n'), 0600); err != nil {
			t.Error(err)
		}
	}()
	write := func(path string, data []byte, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) []byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	encode := func(value any) []byte {
		t.Helper()
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(data, '\n')
	}
	policy := filepath.Join(work, "policy.json")
	write(policy, []byte(`{"default":[{"type":"reject"}],"transports":{"oci":{"":[{"type":"insecureAcceptAnything"}]},"oci-archive":{"":[{"type":"insecureAcceptAnything"}]},"containers-storage":{"":[{"type":"insecureAcceptAnything"}]},"docker":{"":[{"type":"insecureAcceptAnything"}]}}}`), 0600)
	publisher := filepath.Join(work, "publisher")
	write(filepath.Join(publisher, "payload.txt"), []byte("nested OCI component\n"), 0600)
	outputs := filepath.Join(work, "publisher-outputs")
	if err := os.Mkdir(outputs, 0700); err != nil {
		t.Fatal(err)
	}
	serviceLog, err := os.OpenFile(filepath.Join(work, "publisher-engine.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := serviceLog.Close(); err != nil {
			t.Error(err)
		}
	}()
	service, err := engine.Start(ctx, cooprBinary, serviceLog)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	}()
	source := registryFixture(t)
	destination := registryFixture(t)
	sourceHost := strings.TrimPrefix(source.URL, "http://")
	destinationHost := strings.TrimPrefix(destination.URL, "http://")
	components := filepath.Join(work, "publisher-components")
	baseOptions := engine.Options{
		Store:      engine.StoreOptions{RunRoot: filepath.Join(work, "publisher-store", "runroot"), GraphRoot: filepath.Join(work, "publisher-store", "graphroot"), Driver: "vfs"},
		ContextDir: publisher, Runtime: "crun", Cache: engine.CacheOptions{LocalDir: filepath.Join(work, "publisher-cache")},
		Registry: engine.RegistryOptions{SignaturePolicyPath: policy, TLSVerify: new(false)}, Stdout: serviceLog, Stderr: serviceLog,
	}
	buildComponent := func(name, definition string) string {
		t.Helper()
		file := filepath.Join(publisher, name+".coopr")
		write(file, []byte(definition), 0600)
		options := baseOptions
		options.Output = engine.Output{OCI: filepath.Join(outputs, name+".tar")}
		built, err := service.Client.Build(ctx, cooprwire.Build{Files: []string{file}, ComponentStoreDir: components}, true, options)
		if err != nil {
			t.Fatalf("build %s component: %v; see %s", name, err, serviceLog.Name())
		}
		options.Output = engine.Output{}
		repository := sourceHost + "/" + name
		published, err := service.Client.Publish(ctx, cooprwire.Publish{Artifact: "component", Source: built.Reference, Destinations: []string{repository + ":stable"}, ComponentStoreDir: components}, options)
		if err != nil {
			t.Fatalf("publish %s component: %v (%+v)", name, err, published.Destinations)
		}
		repo, err := remote.NewRepository(repository)
		if err != nil {
			t.Fatal(err)
		}
		repo.PlainHTTP = true
		descriptor, err := repo.Resolve(ctx, "stable")
		if err != nil {
			t.Fatal(err)
		}
		return repository + "@" + descriptor.Digest.String()
	}
	child := buildComponent("child", `package as="payload" {
    copy "payload.txt" "/payload"
}
extend {
    copy "/payload" "/from-child" from="payload"
}
`)
	parent := buildComponent("parent", fmt.Sprintf("extend {\n    component %q\n    env PARENT=\"delivered\"\n}\n", child))
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	checks = append(checks, "published-nested-coopr-components")

	nativeDefinition := []byte(`arg "PARENT"
from "scratch" {
    arg "PARENT"
    component "${PARENT}"
    copy "busybox" "/bin/busybox"
    copy "project.txt" "/project.txt"
    copy "scripts/check.sh" "/module-check.sh" from="stoke-assets"
    env RUN_KIND="NATIVE"
    run { exec "/bin/busybox" "sh" "-eu" "/module-check.sh" }
}
`)
	helper := func(version string) []byte {
		return []byte("test \"$(/bin/busybox cat /project.txt)\" = caller-project\nprintf '%s\\n' " + version + " > /asset.txt\necho STOKE_MODULE_${RUN_KIND}_EXECUTED\n")
	}
	libraryDocument := func(reference string) []byte {
		return encode(map[string]any{"version": 1, "assets": []string{"native.coopr", "scripts/check.sh"}, "tasks": map[string]any{
			"native": map[string]any{"coopr": map[string]any{"files": []string{"native.coopr"}, "files-from": "component", "args": map[string]any{"PARENT": map[string]string{"ref": reference}}}},
			"cached": map[string]any{"image": "scratch", "env": map[string]string{"RUN_KIND": "CACHED"}, "steps": []any{
				map[string]any{"copy": map[string]any{"sources": []string{"busybox"}, "destination": "/bin/busybox"}},
				map[string]any{"copy": map[string]any{"sources": []string{"project.txt"}, "destination": "/project.txt"}},
				map[string]any{"copy": map[string]any{"scope": "component", "sources": []string{"scripts/check.sh"}, "destination": "/module-check.sh"}},
				map[string]any{"run": []string{"/bin/busybox", "sh", "-eu", "/module-check.sh"}},
			}},
			"check": map[string]any{"from": "native", "needs": []string{"cached"}, "cache": false, "env": map[string]string{"RUN_KIND": "CHECK"}, "steps": []any{
				map[string]any{"copy": map[string]any{"scope": "component", "sources": []string{"scripts/check.sh"}, "destination": "/module-check.sh"}},
				map[string]any{"run": []string{"/bin/busybox", "sh", "-eu", "/module-check.sh"}},
				map[string]any{"run": []string{"/bin/busybox", "sh", "-eu", "-c", "test \"$(/bin/busybox cat /from-child)\" = 'nested OCI component'; test \"$PARENT\" = delivered"}},
			}},
		}})
	}
	command := func(label string, args []string, env []string) ([]byte, []byte) {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		write(filepath.Join(work, label+".stdout"), stdout.Bytes(), 0600)
		write(filepath.Join(work, label+".stderr"), stderr.Bytes(), 0600)
		if err != nil {
			t.Fatalf("%s failed: %v\n%s", label, err, &stderr)
		}
		return stdout.Bytes(), stderr.Bytes()
	}
	module := filepath.Join(publisher, "module")
	write(filepath.Join(module, "native.coopr"), nativeDefinition, 0644)
	write(filepath.Join(module, "scripts/check.sh"), helper("module-v1"), 0755)
	write(filepath.Join(module, "not-declared.txt"), []byte("not a package file"), 0600)
	moduleYAML := filepath.Join(module, "stoke.yaml")
	write(moduleYAML, libraryDocument(parent), 0600)
	stdout, _ := command("publish-library", []string{"publish", "--plain-http", "-f", moduleYAML, sourceHost + "/library:stable"}, os.Environ())
	library := strings.TrimSpace(string(stdout))
	rootYAML := filepath.Join(publisher, "stoke.yaml")
	write(rootYAML, encode(map[string]any{"version": 1, "imports": map[string]string{"library": library}, "tasks": map[string]any{"run": map[string]string{"uses": "library/check"}}}), 0600)
	stdout, _ = command("publish-root", []string{"publish", "--plain-http", "-f", rootYAML, sourceHost + "/workflow:stable"}, os.Environ())
	rootReference := strings.TrimSpace(string(stdout))
	client := Client{PlainHTTP: true}
	publishedLibrary, err := client.Pull(ctx, library)
	if err != nil {
		t.Fatal(err)
	}
	if len(publishedLibrary.Files) != 2 || !publishedLibrary.Files["scripts/check.sh"].Executable || !bytes.Equal(publishedLibrary.Files["scripts/check.sh"].Data, helper("module-v1")) {
		t.Fatalf("published package files incorrect: %#v", publishedLibrary.Files)
	}
	checks = append(checks, "explicit-workflow-assets-and-reference-declaration")
	transport, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{sourceHost, destinationHost}})
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(work, "bundle")
	if _, err := transport.CreateLayout(ctx, bale.Recipe{Include: []bale.Selection{{Ref: rootReference}}}, layout, bale.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	source.Close()
	if err := os.RemoveAll(publisher); err != nil {
		t.Fatal(err)
	}
	if response, err := (&http.Client{Timeout: time.Second}).Get(source.URL + "/v2/"); err == nil {
		_ = response.Body.Close()
		t.Fatal("source registry remained available")
	}
	plan, err := transport.PlanPush(ctx, layout, destinationHost+"/delivery:complete", bale.PushOptions{RepositoryMap: map[string]string{
		sourceHost + "/workflow": "workflow", sourceHost + "/library": "library", sourceHost + "/parent": "parent", sourceHost + "/child": "child",
	}})
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := transport.Push(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	locations := map[string]string{}
	for _, reference := range []string{rootReference, library, parent, child} {
		repository, value, ok := strings.Cut(reference, "@")
		if !ok {
			t.Fatalf("unversioned fixture reference %q", reference)
		}
		identity, err := digest.Parse(value)
		if err != nil {
			t.Fatal(err)
		}
		location, ok := delivered.Relocations.Lookup(bale.ArtifactID{Repository: repository, Digest: identity})
		if !ok {
			t.Fatalf("Bale did not capture/relocate nested input %s", reference)
		}
		locations[reference] = location.Destination.Repository + "@" + location.Destination.Digest.String()
	}
	write(filepath.Join(work, "relocations.json"), encode(locations), 0600)
	checks = append(checks, "bale-recursive-relocation-source-off-publisher-removed")
	caller := filepath.Join(work, "caller")
	write(filepath.Join(caller, "busybox"), read(busybox), 0755)
	write(filepath.Join(caller, "project.txt"), []byte("caller-project\n"), 0600)
	write(filepath.Join(caller, "scripts/check.sh"), []byte("echo WRONG_CALLER_SCRIPT; exit 91\n"), 0755)
	env := append(os.Environ(), "STOKE_COOPR_BINARY="+cooprBinary,
		"XDG_STATE_HOME="+filepath.Join(work, "consumer-state"),
		"XDG_CACHE_HOME="+filepath.Join(work, "consumer-cache"),
		"XDG_DATA_HOME="+filepath.Join(work, "consumer-data"),
		"XDG_CONFIG_HOME="+filepath.Join(work, "consumer-config"))
	run := func(label, location, expected string) (ociRuntimeReport, []byte) {
		t.Helper()
		// These filesystem/component checks need no RUN networking. Registry
		// resolution stays outside the workload network namespace.
		stdout, stderr := command(label, []string{"run", "--network", "none", "--plain-http", "--tls-verify=false", "--context", caller, "--signature-policy", policy, "-f", location}, env)
		var report ociRuntimeReport
		if err := json.Unmarshal(stdout, &report); err != nil {
			t.Fatal(err)
		}
		if report.Status != "passed" {
			t.Fatalf("%s report status %s", label, report.Status)
		}
		var saved, emitted any
		if err := json.Unmarshal(read(filepath.Join(report.Directory, "run.json")), &saved); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(stdout, &emitted); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(saved, emitted) {
			t.Fatal("persisted report differs")
		}
		reports[label] = report.Directory
		write(filepath.Join(work, label+".run.json"), stdout, 0600)
		for _, task := range report.Tasks {
			if task.Status != "passed" {
				t.Fatalf("%s task %+v", label, task)
			}
			if task.Filesystem != "" {
				if got := string(read(filepath.Join(task.Filesystem, "asset.txt"))); got != expected+"\n" {
					t.Fatalf("%s/%s component asset=%q", label, task.Name, got)
				}
				if got := string(read(filepath.Join(task.Filesystem, "project.txt"))); got != "caller-project\n" {
					t.Fatalf("caller context changed: %q", got)
				}
			}
		}
		return report, stderr
	}
	remoteRoot := "oci://" + locations[rootReference]
	cold, coldLog := run("cold", remoteRoot, "module-v1")
	if !bytes.Contains(coldLog, []byte("STOKE_MODULE_CACHED_EXECUTED")) || !bytes.Contains(coldLog, []byte("STOKE_MODULE_NATIVE_EXECUTED")) {
		t.Fatal("cold fixture did not execute literal and native helper")
	}
	checks = append(checks, "native-and-literal-packaged-helpers-with-separate-caller-input")
	warm, warmLog := run("warm", remoteRoot, "module-v1")
	if cold.Directory == warm.Directory {
		t.Fatal("warm run reused native run directory")
	}
	if bytes.Contains(warmLog, []byte("STOKE_MODULE_CACHED_EXECUTED")) {
		t.Fatal("rematerialized component files missed controlled literal cache")
	}
	if bytes.Contains(warmLog, []byte("STOKE_MODULE_NATIVE_EXECUTED")) {
		t.Fatal("rematerialized component files missed controlled native RUN cache")
	}
	var cachedHits int
	for _, task := range warm.Tasks {
		if task.Task == "run--library--cached" && task.Cache != nil {
			cachedHits = task.Cache.Hits
		}
	}
	if cachedHits == 0 {
		t.Fatal("warm literal cache counters did not record reuse")
	}
	checks = append(checks, "fresh-materialization-portable-cache-reuse")
	bundleDir := filepath.Join(work, "pulled")
	command("pull-directory", []string{"pull", "--plain-http", "--directory", bundleDir, locations[library]}, os.Environ())
	if !bytes.Equal(read(filepath.Join(bundleDir, "scripts/check.sh")), helper("module-v1")) {
		t.Fatal("pull lost packaged helper")
	}
	_, _ = run("pulled", filepath.Join(bundleDir, "stoke.yaml"), "module-v1")
	checks = append(checks, "pulled-package-runs-with-caller-context")
	// New receiving-registry packages have the same asset path but changed
	// content/mode. The original publisher and registry remain unavailable.
	changed := filepath.Join(work, "changed-package")
	if err := os.Mkdir(changed, 0700); err != nil {
		t.Fatal(err)
	}
	changedFiles := map[string]assets.File{"native.coopr": {Data: nativeDefinition}, "scripts/check.sh": {Data: helper("module-v2"), Executable: true}}
	if err := assets.Materialize(changed, changedFiles); err != nil {
		t.Fatal(err)
	}
	changedYAML := filepath.Join(changed, "stoke.yaml")
	write(changedYAML, libraryDocument(locations[parent]), 0600)
	stdout, _ = command("publish-changed", []string{"publish", "--plain-http", "-f", changedYAML, destinationHost + "/changed:stable"}, os.Environ())
	changedReference := strings.TrimSpace(string(stdout))
	_, changedLog := run("changed-bytes", "oci://"+changedReference, "module-v2")
	if !bytes.Contains(changedLog, []byte("STOKE_MODULE_CACHED_EXECUTED")) {
		t.Fatal("changed packaged bytes did not invalidate controlled cache")
	}
	checks = append(checks, "changed-asset-bytes-invalidate-cache")
	if err := os.Chmod(filepath.Join(changed, "scripts/check.sh"), 0644); err != nil {
		t.Fatal(err)
	}
	stdout, _ = command("publish-mode", []string{"publish", "--plain-http", "-f", changedYAML, destinationHost + "/changed:mode"}, os.Environ())
	modeReference := strings.TrimSpace(string(stdout))
	modeReport, modeLog := run("changed-mode", "oci://"+modeReference, "module-v2")
	if !bytes.Contains(modeLog, []byte("STOKE_MODULE_CACHED_EXECUTED")) {
		t.Fatal("changed packaged mode did not invalidate controlled cache")
	}
	for _, task := range modeReport.Tasks {
		info, err := os.Stat(filepath.Join(task.Filesystem, "module-check.sh"))
		if err != nil || info.Mode().Perm() != 0644 {
			t.Fatalf("changed packaged mode not preserved: %v %v", info, err)
		}
	}
	checks = append(checks, "changed-asset-mode-invalidate-cache")
}
