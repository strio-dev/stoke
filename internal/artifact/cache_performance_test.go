package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const cachePerfStamp int64 = 1600000000
const cachePerfBuildMarker = "STOKE_CACHE_PERF_BUILD_RAN"
const cachePerfVerifyMarker = "STOKE_CACHE_PERF_VERIFY_RAN"

type cachePerfCase struct {
	Workload             string             `json:"workload"`
	Scenario             string             `json:"scenario"`
	Iteration            int                `json:"iteration"`
	WallSeconds          float64            `json:"wallSeconds"`
	UserCPUSeconds       float64            `json:"userCPUSeconds"`
	SystemCPUSeconds     float64            `json:"systemCPUSeconds"`
	MaxRSSKiB            int64              `json:"maxRSSKiB"`
	MaxRSSSemantics      string             `json:"maxRSSSemantics"`
	RegistryBlobGetBytes int64              `json:"registryBlobGetBytes"`
	RegistryRequests     []cachePerfRequest `json:"registryRequests"`
	RunDirectory         string             `json:"runDirectory"`
	Status               string             `json:"status"`
	Proofs               map[string]bool    `json:"proofs"`
}
type cachePerfReport struct {
	Schema      int              `json:"schema"`
	Label       string           `json:"label"`
	Samples     int              `json:"samples"`
	FixtureHash string           `json:"fixtureHash"`
	SeedHash    string           `json:"seedHash"`
	Resources   map[string]any   `json:"resources"`
	EngineSHA   string           `json:"engineSHA"`
	StokeSHA    string           `json:"stokeSHA"`
	Cases       []*cachePerfCase `json:"cases"`
	Proofs      map[string]bool  `json:"proofs"`
}
type cachePerfFile struct {
	Path  string `json:"path"`
	SHA   string `json:"sha256"`
	Mode  uint32 `json:"mode"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
}
type cachePerfWork struct {
	BaselineEngine string `json:"baseline_engine"`
	BaselineStoke  string `json:"baseline_stoke"`
}
type cachePerfCollector struct {
	t                             *testing.T
	ctx                           context.Context
	root, binary, engine, busybox string
	work                          cachePerfWork
	report                        cachePerfReport
}

// TestCachePerformanceCollector is opt-in and subprocess-only. Its fixed offline
// fixtures measure source packaging/cache I/O, not compilation performance.
func TestCachePerformanceCollector(t *testing.T) {
	output := os.Getenv("STOKE_CACHE_PERF_REPORT")
	if testing.Short() || output == "" {
		t.Skip("opt-in native cache performance collector")
	}
	if !filepath.IsAbs(output) {
		t.Fatal("STOKE_CACHE_PERF_REPORT must be absolute")
	}
	env := func(name string) string {
		t.Helper()
		v := os.Getenv(name)
		if v == "" {
			t.Fatalf("%s is required", name)
		}
		return v
	}
	c := &cachePerfCollector{t: t, root: env("STOKE_CACHE_PERF_ROOT"), binary: env("STOKE_BINARY"), engine: env("STOKE_COOPR_BINARY"), busybox: env("STOKE_SMOKE_BUSYBOX")}
	if !filepath.IsAbs(c.root) {
		t.Fatal("STOKE_CACHE_PERF_ROOT must be absolute")
	}
	c.must(json.Unmarshal(c.read(filepath.Join(c.root, "work.json")), &c.work))
	if c.work.BaselineEngine == "" || c.work.BaselineStoke == "" {
		t.Fatal("work.json must freeze baseline executable paths")
	}
	samples := 5
	if v := os.Getenv("STOKE_CACHE_PERF_SAMPLES"); v != "" {
		var err error
		samples, err = strconv.Atoi(v)
		c.must(err)
		if samples < 1 {
			t.Fatal("positive samples required")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 44*time.Minute)
	defer cancel()
	c.ctx = ctx
	// Resource values describe the external launcher contract, not measured limits.
	c.report = cachePerfReport{Schema: 1, Label: env("STOKE_CACHE_PERF_LABEL"), Samples: samples, EngineSHA: c.fileSHA(c.engine), StokeSHA: c.fileSHA(c.binary), Cases: []*cachePerfCase{}, Proofs: map[string]bool{}, Resources: map[string]any{
		"memoryMaxBytes": int64(8 << 30), "memorySwapMaxBytes": 0, "tasksMax": 256, "cpuQuotaPercent": 400, "gomaxprocs": 4, "go": "1.27.3", "driverCGO": false, "nativeEngineCGO": true, "memoryPeak": "unknown; parent fence limits aggregate memory", "workload": "offline source and binary packaging/cache I/O; no compilation claim",
	}}
	defer func() {
		c.report.Proofs["complete"] = !t.Failed() && len(c.report.Cases) == samples*10
		c.write(output, c.encode(c.report), 0600)
	}()
	fixtures := filepath.Join(c.root, "fixture")
	if _, err := os.Stat(filepath.Join(fixtures, "manifest.json")); os.IsNotExist(err) {
		if c.report.Label != "baseline" && !strings.Contains(c.report.Label, "preflight") {
			t.Fatal("only baseline/preflight may initialize fixture")
		}
		c.initializeFixture(fixtures, env("STOKE_CACHE_PERF_SOURCE_STOKE"), env("STOKE_CACHE_PERF_SOURCE_COOPR"))
	} else {
		c.must(err)
	}
	manifest := c.fixtureManifest(fixtures)
	if !bytes.Equal(c.read(filepath.Join(fixtures, "manifest.json")), c.encode(manifest)) {
		t.Fatal("frozen fixture bytes/modes/mtimes changed")
	}
	c.report.FixtureHash = cachePerfSHA(c.encode(manifest))
	c.report.Proofs["fixtureFrozen"] = true
	for _, family := range []string{"source-image", "component-image"} {
		c.ensureSeed(family)
	}
	seedManifest := c.seedManifest()
	c.report.SeedHash = cachePerfSHA(c.encode(seedManifest))
	c.report.Proofs["seedFrozen"] = true
	for _, family := range []string{"source-image", "component-image"} {
		for _, scenario := range []string{"cold", "local-warm", "registry-warm", "registry-rw", "changed-source"} {
			for iteration := 1; iteration <= samples; iteration++ {
				result := &cachePerfCase{Workload: family, Scenario: scenario, Iteration: iteration, Status: "failed", Proofs: map[string]bool{}, RegistryRequests: []cachePerfRequest{}}
				c.report.Cases = append(c.report.Cases, result)
				home := filepath.Join(c.root, "runs", c.report.Label, family, scenario, strconv.Itoa(iteration))
				if _, err := os.Stat(home); !os.IsNotExist(err) {
					t.Fatalf("run directory already exists: %s", home)
				}
				c.must(os.MkdirAll(home, 0700))
				caller := filepath.Join(fixtures, family, "context")
				if scenario == "changed-source" {
					caller = filepath.Join(home, "context")
					c.copyTree(filepath.Join(fixtures, family, "context"), caller)
					c.write(filepath.Join(caller, "payload", "build-input.txt"), []byte("cache-performance-edit\n"), 0644)
					c.stamp(filepath.Join(caller, "payload", "build-input.txt"))
				}
				if scenario == "local-warm" || scenario == "changed-source" {
					c.copyTree(filepath.Join(c.root, "seeds", family, "cache"), filepath.Join(home, "cache"))
				}
				var registry *cachePerfRegistry
				flags := []string{}
				if strings.HasPrefix(scenario, "registry-") {
					objects, err := loadCachePerfSnapshot(filepath.Join(c.root, "seeds", family, "registry"))
					c.must(err)
					registry = newCachePerfRegistry(objects)
					host := strings.TrimPrefix(registry.server.URL, "http://")
					flags = append(flags, "--cache-from", host+"/cache/source")
					if scenario == "registry-rw" {
						flags = append(flags, "--cache-to", host+"/cache/mirror")
					}
				}
				c.run(result, home, caller, filepath.Join(fixtures, family, "workflow.json"), c.binary, c.engine, flags, registry)
				result.Proofs["warmBuildSkippedOrColdBuildRan"] = true // checked by run
				if !bytes.Equal(c.read(filepath.Join(fixtures, "manifest.json")), c.encode(c.fixtureManifest(fixtures))) {
					t.Fatal("run mutated frozen fixture")
				}
				result.Proofs["fixtureUnchanged"] = true
				result.Status = "passed"
				c.write(output, c.encode(c.report), 0600)
			}
		}
	}
	if got := cachePerfSHA(c.encode(c.seedManifest())); got != c.report.SeedHash {
		t.Fatal("run mutated frozen cache seed")
	}
	c.report.Proofs["seedUnchanged"] = true
}
func cachePerfSHA(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func (c *cachePerfCollector) must(err error) {
	c.t.Helper()
	if err != nil {
		c.t.Fatal(err)
	}
}
func (c *cachePerfCollector) read(path string) []byte {
	c.t.Helper()
	b, e := os.ReadFile(path)
	c.must(e)
	return b
}
func (c *cachePerfCollector) write(path string, data []byte, mode os.FileMode) {
	c.t.Helper()
	c.must(os.MkdirAll(filepath.Dir(path), 0700))
	c.must(os.WriteFile(path, data, mode))
	c.must(os.Chmod(path, mode))
}
func (c *cachePerfCollector) encode(value any) []byte {
	c.t.Helper()
	b, e := json.MarshalIndent(value, "", "  ")
	c.must(e)
	return append(b, '\n')
}
func (c *cachePerfCollector) stamp(path string) {
	stamp := time.Unix(cachePerfStamp, 0)
	c.must(os.Chtimes(path, stamp, stamp))
}
func (c *cachePerfCollector) fileSHA(path string) string {
	c.t.Helper()
	f, e := os.Open(path)
	c.must(e)
	defer func() { c.must(f.Close()) }()
	h := sha256.New()
	_, e = io.Copy(h, f)
	c.must(e)
	return hex.EncodeToString(h.Sum(nil))
}
func (c *cachePerfCollector) manifest(root string, exclude map[string]bool) []cachePerfFile {
	c.t.Helper()
	files := []cachePerfFile{}
	c.must(filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		if exclude[rel] {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("fixture requires regular files: %s", path)
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		files = append(files, cachePerfFile{filepath.ToSlash(rel), c.fileSHA(path), uint32(info.Mode().Perm()), info.Size(), info.ModTime().UnixNano()})
		return nil
	}))
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}
func (c *cachePerfCollector) copyTree(source, dest string) {
	c.t.Helper()
	directories := []struct {
		path string
		info fs.FileInfo
	}{}
	c.must(filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, e := filepath.Rel(source, path)
		if e != nil {
			return e
		}
		target := filepath.Join(dest, rel)
		info, e := d.Info()
		if e != nil {
			return e
		}
		if d.IsDir() {
			directories = append(directories, struct {
				path string
				info fs.FileInfo
			}{target, info})
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("nonregular copy source %s", path)
		}
		c.copyFile(path, target, info.Mode().Perm())
		return os.Chtimes(target, info.ModTime(), info.ModTime())
	}))
	for i := len(directories) - 1; i >= 0; i-- {
		d := directories[i]
		c.must(os.Chmod(d.path, d.info.Mode().Perm()))
		c.must(os.Chtimes(d.path, d.info.ModTime(), d.info.ModTime()))
	}
}
func (c *cachePerfCollector) copyFile(source, target string, mode os.FileMode) {
	c.t.Helper()
	c.must(os.MkdirAll(filepath.Dir(target), 0700))
	in, e := os.Open(source)
	c.must(e)
	out, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	c.must(e)
	_, e = io.Copy(out, in)
	c.must(e)
	c.must(out.Close())
	c.must(in.Close())
}
func (c *cachePerfCollector) initializeFixture(root, stokeSource, cooprSource string) {
	c.t.Helper()
	checksum := `LC_ALL=C; export LC_ALL; cd /payload; /bin/busybox find . -type f -exec /bin/busybox sha256sum '{}' ';' | /bin/busybox sort > /checksums.txt; echo ` + cachePerfBuildMarker
	command := strconv.Quote(checksum)
	for _, family := range []string{"source-image", "component-image"} {
		caller := filepath.Join(root, family, "context")
		payload := filepath.Join(caller, "payload")
		for name, source := range map[string]string{"stoke": stokeSource, "coopr": cooprSource} {
			for _, dir := range []string{"cmd", "internal"} {
				c.copyTree(filepath.Join(source, dir), filepath.Join(payload, "source", name, dir))
			}
			for _, file := range []string{"go.mod", "go.sum", "LICENSE", "README.md"} {
				c.copyFile(filepath.Join(source, file), filepath.Join(payload, "source", name, file), 0644)
			}
		}
		stokeELF := filepath.Join(filepath.Dir(c.work.BaselineStoke), ".stoke-wrapped")
		if _, err := os.Stat(stokeELF); os.IsNotExist(err) {
			stokeELF = c.work.BaselineStoke
		} else {
			c.must(err)
		}
		c.copyFile(stokeELF, filepath.Join(payload, "bin", "stoke"), 0755)
		c.copyFile(c.work.BaselineEngine, filepath.Join(payload, "bin", "coopr"), 0755)
		c.copyFile(c.busybox, filepath.Join(caller, "busybox"), 0755)
		c.write(filepath.Join(payload, "build-input.txt"), []byte("cache-performance-base\n"), 0644)
		native := "from \"scratch\" {\n    copy \"busybox\" \"/bin/busybox\"\n    copy \"payload\" \"/payload\"\n    run { exec \"/bin/busybox\" \"sh\" \"-eu\" \"-c\" " + command + " }\n}\n"
		build := map[string]any{"coopr": map[string]any{"files": []string{"native.coopr"}}}
		tasks := map[string]any{"build": build,
			"verify":   map[string]any{"from": "build", "cache": false, "outputs": map[string]any{"filesystem": false}, "steps": []any{map[string]any{"run": []string{"/bin/busybox", "sh", "-eu", "-c", "cd /payload; /bin/busybox sha256sum -c /checksums.txt >/dev/null; echo " + cachePerfVerifyMarker}}}},
			"artifact": map[string]any{"image": "scratch", "needs": []string{"verify"}, "steps": []any{map[string]any{"copy": map[string]any{"from": "build", "sources": []string{"/checksums.txt"}, "destination": "/checksums.txt"}}}},
		}
		if family == "component-image" {
			c.write(filepath.Join(caller, "component.coopr"), []byte("package as=\"payload\" { copy \"payload\" \"/payload\" }\nextend as=\"installed\" { copy \"/payload\" \"/payload\" from=\"payload\" }\n"), 0644)
			tasks["component"] = map[string]any{"coopr": map[string]any{"kind": "component", "files": []string{"component.coopr"}, "target": "installed"}}
			build["coopr"] = map[string]any{"files": []string{"native.coopr"}, "args": map[string]any{"COMPONENT": map[string]any{"from": "component"}}}
			native = "from \"scratch\" {\n    arg \"COMPONENT\"\n    component \"${COMPONENT}\"\n    copy \"busybox\" \"/bin/busybox\"\n    run { exec \"/bin/busybox\" \"sh\" \"-eu\" \"-c\" " + command + " }\n}\n"
		}
		c.write(filepath.Join(caller, "native.coopr"), []byte(native), 0644)
		c.write(filepath.Join(root, family, "workflow.json"), c.encode(map[string]any{"version": 1, "tasks": tasks}), 0644)
	}
	c.write(filepath.Join(root, "policy.json"), []byte(`{"default":[{"type":"reject"}],"transports":{"oci":{"":[{"type":"insecureAcceptAnything"}]},"oci-archive":{"":[{"type":"insecureAcceptAnything"}]},"containers-storage":{"":[{"type":"insecureAcceptAnything"}]},"docker":{"":[{"type":"insecureAcceptAnything"}]}}}`), 0600)
	c.must(filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		mode := os.FileMode(0644)
		if strings.Contains(filepath.ToSlash(path), "/bin/") || d.Name() == "busybox" {
			mode = 0755
		}
		c.must(os.Chmod(path, mode))
		c.stamp(path)
		return nil
	}))
	// Freeze directories after creating all children; caller context metadata
	// cannot depend on when the preflight happened.
	directories := []string{}
	c.must(filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			directories = append(directories, path)
		}
		return nil
	}))
	for i := len(directories) - 1; i >= 0; i-- {
		c.must(os.Chmod(directories[i], 0755))
		c.stamp(directories[i])
	}
	c.write(filepath.Join(root, "manifest.json"), c.encode(c.fixtureManifest(root)), 0600)
}
func (c *cachePerfCollector) ensureSeed(family string) {
	seed := filepath.Join(c.root, "seeds", family)
	marker := filepath.Join(seed, "complete.json")
	if data, err := os.ReadFile(marker); err == nil {
		var proof map[string]string
		c.must(json.Unmarshal(data, &proof))
		if proof["fixtureHash"] != c.report.FixtureHash {
			c.t.Fatal("seed fixture identity mismatch")
		}
		return
	} else if !os.IsNotExist(err) {
		c.must(err)
	}
	if c.report.Label != "baseline" && !strings.Contains(c.report.Label, "preflight") {
		c.t.Fatal("candidate cannot initialize cache seed")
	}
	if _, err := os.Stat(seed); !os.IsNotExist(err) {
		c.t.Fatalf("incomplete seed exists, retain and inspect before reuse: %s", seed)
	}
	c.must(os.MkdirAll(seed, 0700))
	registry := newCachePerfRegistry(nil)
	host := strings.TrimPrefix(registry.server.URL, "http://")
	result := &cachePerfCase{Workload: family, Scenario: "seed", Proofs: map[string]bool{}}
	c.run(result, seed, filepath.Join(c.root, "fixture", family, "context"), filepath.Join(c.root, "fixture", family, "workflow.json"), c.work.BaselineStoke, c.work.BaselineEngine, []string{"--cache-to", host + "/cache/source"}, registry)
	c.must(registry.saveSnapshot(filepath.Join(seed, "registry")))
	c.write(marker, c.encode(map[string]string{"fixtureHash": c.report.FixtureHash, "engineSHA": c.fileSHA(c.work.BaselineEngine), "stokeSHA": c.fileSHA(c.work.BaselineStoke)}), 0600)
}
func (c *cachePerfCollector) run(result *cachePerfCase, home, caller, workflow, binary, engine string, flags []string, registry *cachePerfRegistry) {
	c.t.Helper()
	result.RunDirectory = home
	result.MaxRSSSemantics = "Linux RUSAGE_CHILDREN for waited descendants; maxrss is largest waited process, not aggregate worker memory peak"
	args := []string{"run", "-f", workflow, "--context", caller, "--signature-policy", filepath.Join(c.root, "fixture", "policy.json"), "--tls-verify=false", "--cache-dir", filepath.Join(home, "cache"), "--output", filepath.Join(home, "outputs")}
	args = append(args, flags...)
	args = append(args, "artifact")
	cmd := exec.CommandContext(c.ctx, binary, args...)
	cmd.Env = append(os.Environ(), "STOKE_COOPR_BINARY="+engine, "GOMAXPROCS=4", "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_DATA_HOME="+filepath.Join(home, "data"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_CACHE_HOME="+filepath.Join(home, "xdg-cache"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	started := time.Now()
	err := cmd.Run()
	result.WallSeconds = time.Since(started).Seconds()
	if cmd.ProcessState != nil {
		result.UserCPUSeconds = cmd.ProcessState.UserTime().Seconds()
		result.SystemCPUSeconds = cmd.ProcessState.SystemTime().Seconds()
		if usage, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			result.MaxRSSKiB = usage.Maxrss
		}
	}
	c.write(filepath.Join(home, "stdout.json"), stdout.Bytes(), 0600)
	c.write(filepath.Join(home, "stderr.log"), stderr.Bytes(), 0600)
	if registry != nil {
		result.RegistryRequests = registry.closeEvents()
		for _, event := range result.RegistryRequests {
			if event.Method == "GET" && event.Status == 200 && strings.Contains(event.Path, "/blobs/sha256:") {
				result.RegistryBlobGetBytes += event.WrittenBytes
			}
		}
		c.write(filepath.Join(home, "registry-requests.json"), c.encode(result.RegistryRequests), 0600)
	}
	if err != nil {
		c.t.Fatalf("%s/%s: %v; retained logs: %s\n%s", result.Workload, result.Scenario, err, home, stderr.String())
	}
	var report registryCacheReport
	c.must(json.Unmarshal(stdout.Bytes(), &report))
	if report.Status != "passed" {
		c.t.Fatalf("workflow did not pass: %+v", report)
	}
	var emitted, persisted any
	c.must(json.Unmarshal(stdout.Bytes(), &emitted))
	c.must(json.Unmarshal(c.read(filepath.Join(report.Directory, "run.json")), &persisted))
	if !reflect.DeepEqual(emitted, persisted) {
		c.t.Fatal("emitted and persisted reports differ")
	}
	result.Proofs["persistedReportMatches"] = true
	warm := result.Scenario == "local-warm" || strings.HasPrefix(result.Scenario, "registry-")
	if bytes.Contains(stderr.Bytes(), []byte("\n"+cachePerfBuildMarker+"\n")) == warm {
		c.t.Fatalf("%s/%s build marker mismatch (warm=%t); see %s", result.Workload, result.Scenario, warm, home)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("\n"+cachePerfVerifyMarker+"\n")) {
		c.t.Fatal("uncached verifier did not execute")
	}
	result.Proofs["uncachedVerifierExecuted"] = true
	expected := []string{}
	manifest := c.manifest(filepath.Join(caller, "payload"), nil)
	for _, file := range manifest {
		expected = append(expected, file.SHA+"  ./"+file.Path+"\n")
	}
	sort.Strings(expected)
	expectedBytes := []byte(strings.Join(expected, ""))
	foundBuild, foundArtifact := false, false
	for _, task := range report.Tasks {
		if task.Status != "passed" {
			c.t.Fatalf("task failed: %+v", task)
		}
		if task.Name == "build" {
			foundBuild = true
			exported := c.manifest(filepath.Join(task.Filesystem, "payload"), nil)
			if len(exported) != len(manifest) {
				c.t.Fatal("payload file count changed")
			}
			for i := range manifest {
				if manifest[i].Path != exported[i].Path || manifest[i].SHA != exported[i].SHA || manifest[i].Mode != exported[i].Mode {
					c.t.Fatalf("payload bytes/mode mismatch: %+v != %+v", manifest[i], exported[i])
				}
			}
		}
		if task.Name == "artifact" {
			foundArtifact = true
			if !bytes.Equal(c.read(filepath.Join(task.Filesystem, "checksums.txt")), expectedBytes) {
				c.t.Fatal("artifact checksum content mismatch")
			}
			if info, err := os.Stat(task.OCI); err != nil || info.Size() == 0 {
				c.t.Fatal("missing artifact OCI output")
			}
		}
	}
	if !foundBuild || !foundArtifact {
		c.t.Fatal("missing build or artifact task")
	}
	result.Proofs["exactPayloadBytesAndModes"] = true
	result.Proofs["artifactMatchesPayload"] = true
	if strings.HasPrefix(result.Scenario, "registry-") {
		sourceWrites, mirrorWrites := 0, 0
		for _, event := range result.RegistryRequests {
			if event.Method != "GET" && event.Method != "HEAD" {
				if event.Repository == "cache/source" {
					sourceWrites++
				}
				if event.Repository == "cache/mirror" {
					mirrorWrites++
				}
			}
		}
		if sourceWrites != 0 {
			c.t.Fatal("read-only source registry was modified")
		}
		result.Proofs["sourceRegistryReadOnly"] = true
		if result.Scenario == "registry-rw" {
			if mirrorWrites == 0 {
				c.t.Fatal("registry mirror did not receive writes")
			}
			result.Proofs["mirrorRegistryWritten"] = true
		}
	}
	result.Proofs["warmBuildSkippedOrColdBuildRan"] = true
	result.Status = "passed"
}

// Only portable local cache and registry content belong to seed identity. Native
// storage, worker state and seed-run logs remain evidence, never warm inputs.
func (c *cachePerfCollector) seedManifest() []cachePerfFile {
	files := []cachePerfFile{}
	for _, family := range []string{"source-image", "component-image"} {
		root := filepath.Join(c.root, "seeds", family)
		for _, kind := range []string{"cache", "registry"} {
			for _, entry := range c.manifest(filepath.Join(root, kind), nil) {
				entry.Path = family + "/" + kind + "/" + entry.Path
				files = append(files, entry)
			}
		}
		info, err := os.Stat(filepath.Join(root, "complete.json"))
		c.must(err)
		files = append(files, cachePerfFile{family + "/complete.json", c.fileSHA(filepath.Join(root, "complete.json")), uint32(info.Mode().Perm()), info.Size(), info.ModTime().UnixNano()})
	}
	return files
}

func (c *cachePerfCollector) fixtureManifest(root string) []cachePerfFile {
	entries := c.manifest(root, map[string]bool{"manifest.json": true})
	for _, family := range []string{"source-image", "component-image"} {
		c.must(filepath.WalkDir(filepath.Join(root, family, "context"), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			entries = append(entries, cachePerfFile{filepath.ToSlash(rel) + "/", "directory", uint32(info.Mode().Perm()), 0, info.ModTime().UnixNano()})
			return nil
		}))
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries
}
