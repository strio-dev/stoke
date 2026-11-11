package artifact

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
)

type mountsRuntimeTask struct {
	Name, Task, Kind, Status, Filesystem, Digest string
	Cache                                        *struct{ Hits, Misses, Stored, Skipped, Errors int }
}

type mountsRuntimeReport struct {
	Status, Directory string
	Tasks             []mountsRuntimeTask
}

type mountsRuntimeFixture struct {
	t                          *testing.T
	ctx                        context.Context
	binary, work, caller       string
	workflow, policy, registry string
	env                        []string
	reports                    map[string]string
	checks                     []string
}

// TestMountsAndEnvironmentsWithCoopr runs the authored Stoke surface against
// actual packaged binaries. All registry content and writable directories belong
// to this fixture; its evidence is retained for independent inspection.
func TestMountsAndEnvironmentsWithCoopr(t *testing.T) {
	if testing.Short() {
		t.Skip("requires configured rootless host and packaged Stoke/Coopr executables")
	}
	binary, coopr, busybox := os.Getenv("STOKE_BINARY"), os.Getenv("STOKE_COOPR_BINARY"), os.Getenv("STOKE_SMOKE_BUSYBOX")
	for name, value := range map[string]string{"STOKE_BINARY": binary, "STOKE_COOPR_BINARY": coopr, "STOKE_SMOKE_BUSYBOX": busybox} {
		if value == "" {
			t.Fatalf("%s must select the configured acceptance executable", name)
		}
		if _, err := os.Stat(value); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	work, err := os.MkdirTemp("", "stoke-mounts-")
	if err != nil {
		t.Fatal(err)
	}
	registry := registryFixture(t)
	f := &mountsRuntimeFixture{
		t: t, ctx: ctx, binary: binary, work: work, caller: filepath.Join(work, "caller"),
		workflow: filepath.Join(work, "workflow.json"), policy: filepath.Join(work, "policy.json"),
		registry: strings.TrimPrefix(registry.URL, "http://"), reports: map[string]string{},
	}
	t.Logf("Retained mounts/environment acceptance evidence: %s", work)
	temp := filepath.Join(work, "tmp")
	if err := os.Mkdir(temp, 0700); err != nil {
		t.Fatal(err)
	}
	f.env = append(os.Environ(), "STOKE_COOPR_BINARY="+coopr,
		"TMPDIR="+temp, "XDG_STATE_HOME="+filepath.Join(work, "state"),
		"XDG_CACHE_HOME="+filepath.Join(work, "cache"), "XDG_DATA_HOME="+filepath.Join(work, "data"),
		"XDG_CONFIG_HOME="+filepath.Join(work, "config"))
	defer func() {
		data, marshalErr := json.MarshalIndent(map[string]any{"passed": !t.Failed(), "evidence": work, "checks": f.checks, "reports": f.reports}, "", "  ")
		if marshalErr != nil {
			t.Error(marshalErr)
			return
		}
		if err := os.WriteFile(filepath.Join(work, "summary.json"), append(data, '\n'), 0600); err != nil {
			t.Error(err)
		}
	}()
	f.write(filepath.Join(f.caller, "busybox"), f.read(busybox), 0755)
	// Make equivalent cold producers reproducible before testing warm mount
	// reuse. Otherwise the runtime creates these mountpoint directories with
	// wall-clock mtimes, so concurrent results can have different rootfs IDs.
	stamp := time.Unix(1_600_000_000, 0)
	for _, directory := range []string{"dev", "proc", "sys", "."} {
		path := filepath.Join(f.caller, "runtime-dirs", directory)
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	f.write(filepath.Join(f.caller, "payload.txt"), []byte("primary-one\n"), 0644)
	f.write(filepath.Join(work, "inputs", "payload.txt"), []byte("named-one\n"), 0644)
	f.write(filepath.Join(work, "live", "input.txt"), []byte("live-one\n"), 0644)
	f.write(f.policy, []byte(`{"default":[{"type":"reject"}],"transports":{"oci":{"":[{"type":"insecureAcceptAnything"}]},"oci-archive":{"":[{"type":"insecureAcceptAnything"}]},"containers-storage":{"":[{"type":"insecureAcceptAnything"}]},"docker":{"":[{"type":"insecureAcceptAnything"}]}}}`), 0600)
	f.write(filepath.Join(f.caller, "environment.coopr"), []byte(`arg "SOURCE_DATE_EPOCH" "1600000000"
from "scratch" as="ci" {
    copy "busybox" "/bin/busybox"
    copy "runtime-dirs/" "/"
    env NATIVE="native" OVERRIDE="native"
    workdir "/bin"
    run {
        mount "bind" from="fixtures" target="/native-input" readonly="true"
        exec "/bin/busybox" "sh" "-eu" "-c" "/bin/busybox cat /native-input/payload.txt > /environment.txt; /bin/busybox touch -t 202009131226.40 /environment.txt; echo STOKE_ENVIRONMENT_BUILDER_RAN"
    }
}
`), 0644)
	archive := mountsRuntimeTar(t, map[string][]byte{"inside/payload.txt": []byte("archive stays packed\n")}, 0644)
	artifact := f.pushArtifact("files", map[string][]byte{"payload.txt": []byte("artifact-one\n"), "payload.tar": archive})
	image := f.pushImage("image", map[string][]byte{"bin/busybox": f.read(busybox), "image.txt": []byte("image-one\n"), "left.txt": []byte("selected\n"), "right.txt": []byte("selected\n")})
	definition := map[string]any{"files": []string{"environment.coopr"}, "target": "ci"}
	contexts := map[string]string{"fixtures": "../inputs"}
	equivalence := []any{mountsRuntimeRun(`printf '%s|%s|%s\n' "$NATIVE" "$OVERRIDE" "$PWD" > /equivalence.txt`, nil)}
	inherited := []any{mountsRuntimeRun(`printf '%s|%s|%s\n' "$NATIVE" "$OVERRIDE" "$PWD" > /inherited.txt`, nil)}
	inline := func() map[string]any {
		return map[string]any{"environment": definition, "contexts": contexts, "env": map[string]string{"OVERRIDE": "task"}, "workdir": "/task-work", "steps": equivalence}
	}
	readerMounts := []any{
		map[string]any{"type": "context", "source": "payload.txt", "target": "/primary.txt"},
		map[string]any{"type": "context", "context": "fixtures", "target": "/named"},
		map[string]any{"type": "image", "image": image, "target": "/image"},
		map[string]any{"type": "image", "image": image, "source": "/image.txt", "target": "/selected-image.txt"},
		map[string]any{"type": "stage", "from": "base", "source": "/environment.txt", "target": "/stage.txt"},
		map[string]any{"type": "artifact", "ref": artifact, "target": "/artifact"},
		map[string]any{"type": "artifact", "ref": artifact, "source": "payload.txt", "target": "/selected-artifact.txt"},
		map[string]any{"type": "host", "source": "../live", "target": "/live-readonly", "relabel": "shared"},
	}
	// Live bind mounts intentionally execute even when read-only. Keep their
	// assertion separate so the immutable reader can prove warm result reuse.
	readonlyHost := readerMounts[len(readerMounts)-1]
	readerMounts = readerMounts[:len(readerMounts)-1]
	readerCommand := `/bin/busybox cat /primary.txt /named/payload.txt /image/image.txt /selected-image.txt /stage.txt /artifact/payload.txt /selected-artifact.txt > /proof.txt; /bin/busybox cp /artifact/payload.tar /saved.tar; test ! -e /artifact/inside/payload.txt; for file in /primary.txt /named/payload.txt /image/image.txt /stage.txt /artifact/payload.txt; do if (printf bad >> "$file") 2>/dev/null; then echo "writable-immutable-input:$file" >&2; exit 19; fi; done; echo STOKE_MOUNT_READER_RAN`
	selectorMount := map[string]any{"type": "image", "image": image, "source": "/left.txt", "target": "/selected.txt"}
	tasks := map[string]any{
		"base":     map[string]any{"coopr": definition, "contexts": contexts},
		"inline-a": inline(), "inline-b": inline(),
		"inline-inherited":   map[string]any{"environment": definition, "contexts": contexts, "steps": inherited},
		"explicit-inherited": map[string]any{"from": "base", "steps": inherited},
		"explicit":           map[string]any{"from": "base", "env": map[string]string{"OVERRIDE": "task"}, "workdir": "/task-work", "steps": equivalence},
		"reader":             map[string]any{"image": "scratch", "contexts": contexts, "mounts": readerMounts, "steps": mountsRuntimeSteps(readerCommand, nil)},
		"selector":           map[string]any{"image": "scratch", "steps": mountsRuntimeSteps(`/bin/busybox cp /selected.txt /selected-proof.txt; echo STOKE_MOUNT_SELECTOR_RAN`, []any{selectorMount})},
		"snapshot":           map[string]any{"image": "scratch", "steps": mountsRuntimeSteps(`printf generated > /snapshot/generated; /bin/busybox cp /snapshot/generated /snapshot-proof.txt`, []any{map[string]any{"type": "context", "target": "/snapshot", "read-only": false}})},
		"named-snapshot":     map[string]any{"image": "scratch", "contexts": contexts, "steps": mountsRuntimeSteps(`printf generated > /snapshot/generated; /bin/busybox cp /snapshot/generated /snapshot-proof.txt`, []any{map[string]any{"type": "context", "context": "fixtures", "target": "/snapshot", "read-only": false}})},
		"image-overlay":      map[string]any{"image": "scratch", "steps": append(mountsRuntimeSteps(`printf disposable >/input/temporary; /bin/busybox cp /input/temporary /overlay-proof.txt`, []any{map[string]any{"type": "image", "image": image, "target": "/input", "read-only": false}}), mountsRuntimeRun(`test ! -e /input/temporary`, []any{map[string]any{"type": "image", "image": image, "target": "/input"}}))},
		"stage-overlay":      map[string]any{"image": "scratch", "steps": append(mountsRuntimeSteps(`printf disposable >/input/temporary; /bin/busybox cp /input/temporary /overlay-proof.txt`, []any{map[string]any{"type": "stage", "from": "base", "target": "/input", "read-only": false}}), mountsRuntimeRun(`test ! -e /input/temporary`, []any{map[string]any{"type": "stage", "from": "base", "target": "/input"}}))},
		"live-write":         map[string]any{"image": "scratch", "steps": mountsRuntimeSteps(`printf 'mutation\n' >> /live/effects.txt; /bin/busybox cp /live/input.txt /live-proof.txt; echo STOKE_LIVE_MOUNT_RAN`, []any{map[string]any{"type": "host", "source": "../live", "target": "/live", "read-only": false, "relabel": "shared"}})},
		"live-readonly":      map[string]any{"image": "scratch", "steps": mountsRuntimeSteps(`if (printf bad >>/live-readonly/input.txt) 2>/dev/null; then exit 21; fi; /bin/busybox cp /live-readonly/input.txt /live-proof.txt`, []any{readonlyHost})},
		"cache":              map[string]any{"image": "scratch", "cache": false, "steps": mountsRuntimeSteps(`if test -e /cache/seed; then printf warm >/cache-state.txt; else printf cold >/cache-state.txt; printf seed >/cache/seed; fi`, []any{map[string]any{"type": "cache", "id": "stoke-mount-runtime-cache", "sharing": "locked", "target": "/cache"}})},
	}
	f.setTasks(tasks)
	cold, coldLog := f.run("cold", true)
	f.verifyEquivalence(cold)
	f.requireFile(cold, "reader", "proof.txt", "primary-one\nnamed-one\nimage-one\nimage-one\nnamed-one\nartifact-one\nartifact-one\n")
	f.requireFile(cold, "cache", "cache-state.txt", "cold")
	if !bytes.Equal(f.read(filepath.Join(f.task(cold, "reader").Filesystem, "saved.tar")), archive) {
		t.Fatal("artifact RUN mount unpacked or altered an archive blob")
	}
	if !mountsRuntimeExecuted(coldLog, "STOKE_MOUNT_READER_RAN") {
		t.Fatal("cold immutable RUN did not execute")
	}
	for _, task := range []string{"snapshot", "named-snapshot"} {
		f.requireFile(cold, task, "snapshot-proof.txt", "generated")
	}
	for _, directory := range []string{f.caller, filepath.Join(work, "inputs")} {
		if _, err := os.Stat(filepath.Join(directory, "generated")); !os.IsNotExist(err) {
			t.Fatalf("snapshot RUN wrote back into caller input %s: %v", directory, err)
		}
	}
	for _, task := range []string{"image-overlay", "stage-overlay"} {
		f.requireFile(cold, task, "overlay-proof.txt", "disposable")
	}
	f.checks = append(f.checks, "all-sources-whole-selected-raw-artifact-ro-and-disposable-writes", "inline-environment-config-precedence-two-task-equivalence-and-dedup")
	warm, warmLog := f.run("warm", true)
	f.verifyEquivalence(warm)
	f.requireFile(warm, "cache", "cache-state.txt", "warm")
	reader := f.task(warm, "reader")
	if reader.Cache == nil || reader.Cache.Hits == 0 || reader.Cache.Misses != 0 || mountsRuntimeExecuted(warmLog, "STOKE_MOUNT_READER_RAN") {
		t.Fatalf("immutable reader failed warm reuse: cache=%+v", reader.Cache)
	}
	if string(f.read(filepath.Join(work, "live", "effects.txt"))) != "mutation\nmutation\n" {
		t.Fatalf("live writable effects were skipped on warm result: %q", f.read(filepath.Join(work, "live", "effects.txt")))
	}
	if !mountsRuntimeExecuted(warmLog, "STOKE_LIVE_MOUNT_RAN") {
		t.Fatal("warm writable host mutation RUN was cached")
	}
	f.checks = append(f.checks, "cold-warm-immutable-reuse-live-side-effects-and-persistent-disposable-cache")

	// Equal-size edits retain mode and mtime. Inputs are identified by content,
	// rather than merely their pathname or stat tuple.
	f.write(filepath.Join(work, "inputs", "payload.txt"), []byte("named-two\n"), 0644)
	changed, changedLog := f.run("named-content-change", true, "reader")
	f.requireFile(changed, "reader", "proof.txt", "primary-one\nnamed-two\nimage-one\nimage-one\nnamed-two\nartifact-one\nartifact-one\n")
	if !mountsRuntimeExecuted(changedLog, "STOKE_MOUNT_READER_RAN") {
		t.Fatal("named context content edit did not invalidate RUN")
	}
	f.write(filepath.Join(f.caller, "payload.txt"), []byte("primary-two\n"), 0644)
	changed, changedLog = f.run("primary-content-change", true, "reader")
	f.requireFile(changed, "reader", "proof.txt", "primary-two\nnamed-two\nimage-one\nimage-one\nnamed-two\nartifact-one\nartifact-one\n")
	if !mountsRuntimeExecuted(changedLog, "STOKE_MOUNT_READER_RAN") {
		t.Fatal("primary context content edit did not invalidate RUN")
	}
	newArtifact := f.pushArtifact("files", map[string][]byte{"payload.txt": []byte("artifact-two\n"), "payload.tar": archive})
	for _, raw := range readerMounts {
		mount := raw.(map[string]any)
		if mount["type"] == "artifact" {
			mount["ref"] = newArtifact
		}
	}
	f.setTasks(tasks)
	changed, changedLog = f.run("artifact-digest-change", true, "reader")
	f.requireFile(changed, "reader", "proof.txt", "primary-two\nnamed-two\nimage-one\nimage-one\nnamed-two\nartifact-two\nartifact-two\n")
	if !mountsRuntimeExecuted(changedLog, "STOKE_MOUNT_READER_RAN") {
		t.Fatal("artifact digest edit did not invalidate RUN")
	}
	newImage := f.pushImage("image", map[string][]byte{"bin/busybox": f.read(busybox), "image.txt": []byte("image-two\n"), "left.txt": []byte("selected\n"), "right.txt": []byte("selected\n")})
	for _, raw := range readerMounts {
		mount := raw.(map[string]any)
		if mount["type"] == "image" {
			mount["image"] = newImage
		}
	}
	f.setTasks(tasks)
	changed, changedLog = f.run("image-digest-change", true, "reader")
	f.requireFile(changed, "reader", "proof.txt", "primary-two\nnamed-two\nimage-two\nimage-two\nnamed-two\nartifact-two\nartifact-two\n")
	if !mountsRuntimeExecuted(changedLog, "STOKE_MOUNT_READER_RAN") {
		t.Fatal("image digest edit did not invalidate RUN")
	}
	selectorMount["source"] = "/right.txt"
	f.setTasks(tasks)
	_, changedLog = f.run("selected-path-change", true, "selector")
	if !mountsRuntimeExecuted(changedLog, "STOKE_MOUNT_SELECTOR_RAN") {
		t.Fatal("selected image path edit reused an unrelated RUN result")
	}
	f.checks = append(f.checks, "equal-size-preserved-mtime-content-digest-and-selected-path-invalidation")

	f.qualifyLockedCache()
	f.qualifyCancellation(newArtifact)
	f.requirePrivateCleanup()
	f.checks = append(f.checks, "private-materialization-and-owned-service-cleanup")
}

// Native progress prints authored argv even for cache hits. Only a standalone
// output line attests that the fixture marker command actually ran.
func mountsRuntimeExecuted(log []byte, marker string) bool {
	for line := range bytes.SplitSeq(log, []byte("\n")) {
		if bytes.Equal(line, []byte(marker)) {
			return true
		}
	}
	return false
}

func TestMountExecutionMarkersExcludePrintedInstructions(t *testing.T) {
	for _, marker := range []string{"STOKE_MOUNT_READER_RAN", "STOKE_PLATFORM_EXECUTED:aarch64"} {
		for _, rendered := range []string{
			"STEP 3: RUN echo " + marker + "\nUsing cache",
			`RUN ["/bin/sh","-c","echo ` + marker + `"]`,
			"==> RUN echo " + marker,
		} {
			if mountsRuntimeExecuted([]byte(rendered+"\n"), marker) {
				t.Fatalf("printed instruction mistaken for executed output: %s", rendered)
			}
		}
		if !mountsRuntimeExecuted([]byte("STEP 3: RUN echo "+marker+"\n"+marker+"\n"), marker) {
			t.Fatal("actual standalone execution marker not recognized")
		}
	}
}

func mountsRuntimeRun(command string, mounts []any) map[string]any {
	step := map[string]any{"run": []string{"/bin/busybox", "sh", "-eu", "-c", command}}
	if len(mounts) != 0 {
		step["mounts"] = mounts
	}
	return step
}

func mountsRuntimeSteps(command string, mounts []any) []any {
	return []any{
		map[string]any{"copy": map[string]any{"sources": []string{"busybox"}, "destination": "/bin/busybox"}},
		mountsRuntimeRun(command, mounts),
	}
}

func (f *mountsRuntimeFixture) write(path string, data []byte, mode os.FileMode) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		f.t.Fatal(err)
	}
	stamp := time.Unix(1_600_000_000, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		f.t.Fatal(err)
	}
}

func (f *mountsRuntimeFixture) read(path string) []byte {
	f.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	return data
}

func (f *mountsRuntimeFixture) encode(value any) []byte {
	f.t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	return append(data, '\n')
}

func (f *mountsRuntimeFixture) setTasks(tasks map[string]any) {
	f.write(f.workflow, f.encode(map[string]any{"version": 1, "tasks": tasks}), 0600)
}

func (f *mountsRuntimeFixture) command(targets []string) *exec.Cmd {
	// The fixture uses local filesystem/FIFO gates, not workload networking.
	args := []string{"run", "--network", "none", "--context", f.caller, "--signature-policy", f.policy, "--tls-verify=false", "-f", f.workflow}
	args = append(args, targets...)
	cmd := exec.CommandContext(f.ctx, f.binary, args...)
	cmd.Env = f.env
	return cmd
}

func (f *mountsRuntimeFixture) run(label string, success bool, targets ...string) (mountsRuntimeReport, []byte) {
	f.t.Helper()
	cmd := f.command(targets)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	f.write(filepath.Join(f.work, label+".stdout"), stdout.Bytes(), 0600)
	f.write(filepath.Join(f.work, label+".stderr"), stderr.Bytes(), 0600)
	if (err == nil) != success {
		f.t.Fatalf("%s success=%t: %v\n%s", label, success, err, &stderr)
	}
	report := f.report(label, stdout.Bytes())
	if success {
		if report.Status != "passed" {
			f.t.Fatalf("%s report status=%q", label, report.Status)
		}
		for _, task := range report.Tasks {
			if task.Status != "passed" {
				f.t.Fatalf("%s task %s status=%s", label, task.Name, task.Status)
			}
		}
	}
	return report, stderr.Bytes()
}

func (f *mountsRuntimeFixture) report(label string, stdout []byte) mountsRuntimeReport {
	f.t.Helper()
	var report mountsRuntimeReport
	if err := json.Unmarshal(stdout, &report); err != nil {
		f.t.Fatalf("%s malformed report: %v; stdout=%s", label, err, stdout)
	}
	var emitted, persisted any
	if err := json.Unmarshal(stdout, &emitted); err != nil {
		f.t.Fatal(err)
	}
	if err := json.Unmarshal(f.read(filepath.Join(report.Directory, "run.json")), &persisted); err != nil || !reflect.DeepEqual(emitted, persisted) {
		f.t.Fatalf("%s emitted and persisted reports differ: %v", label, err)
	}
	f.reports[label] = report.Directory
	return report
}

func (f *mountsRuntimeFixture) task(report mountsRuntimeReport, name string) mountsRuntimeTask {
	f.t.Helper()
	for _, task := range report.Tasks {
		if task.Name == name {
			if task.Status != "passed" {
				f.t.Fatalf("task %s status=%s", name, task.Status)
			}
			return task
		}
	}
	f.t.Fatalf("task %s absent from report", name)
	return mountsRuntimeTask{}
}

func (f *mountsRuntimeFixture) requireFile(report mountsRuntimeReport, task, path, want string) {
	f.t.Helper()
	if got := string(f.read(filepath.Join(f.task(report, task).Filesystem, path))); got != want {
		f.t.Fatalf("%s/%s = %q, want %q", task, path, got, want)
	}
}

func (f *mountsRuntimeFixture) verifyEquivalence(report mountsRuntimeReport) {
	f.t.Helper()
	producers := 0
	for _, task := range report.Tasks {
		if strings.HasPrefix(task.Name, "stoke-environment-") {
			producers++
		}
	}
	if producers != 1 {
		f.t.Fatalf("equivalent inline environments built %d generated producers, want one", producers)
	}
	for _, name := range []string{"inline-a", "inline-b", "explicit"} {
		f.requireFile(report, name, "equivalence.txt", "native|task|/task-work\n")
	}
	for _, name := range []string{"inline-inherited", "explicit-inherited"} {
		f.requireFile(report, name, "inherited.txt", "native|native|/bin\n")
	}
}

func mountsRuntimeTar(t *testing.T, files map[string][]byte, mode int64) []byte {
	t.Helper()
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		fileMode := mode
		if name == "bin/busybox" {
			fileMode = 0755
		}
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: fileMode, Size: int64(len(files[name])), ModTime: time.Unix(0, 0)}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func (f *mountsRuntimeFixture) pushArtifact(repository string, files map[string][]byte) string {
	f.t.Helper()
	store := memory.New()
	layers := []v1.Descriptor{}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		descriptor := content.NewDescriptorFromBytes("application/octet-stream", files[name])
		descriptor.Annotations = map[string]string{v1.AnnotationTitle: name}
		if err := store.Push(f.ctx, descriptor, bytes.NewReader(files[name])); err != nil {
			f.t.Fatal(err)
		}
		layers = append(layers, descriptor)
	}
	manifest, err := oras.PackManifest(f.ctx, store, oras.PackManifestVersion1_1, "application/vnd.stoke.mount-fixture.v1", oras.PackManifestOptions{Layers: layers, ManifestAnnotations: map[string]string{v1.AnnotationCreated: "1970-01-01T00:00:00Z"}})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.push(store, manifest, repository)
}

func (f *mountsRuntimeFixture) pushImage(repository string, files map[string][]byte) string {
	f.t.Helper()
	layer := mountsRuntimeTar(f.t, files, 0644)
	store := memory.New()
	layerDescriptor := content.NewDescriptorFromBytes(v1.MediaTypeImageLayer, layer)
	config := f.encode(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{layerDescriptor.Digest}}})
	configDescriptor := content.NewDescriptorFromBytes(v1.MediaTypeImageConfig, config)
	manifest := f.encode(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: configDescriptor, Layers: []v1.Descriptor{layerDescriptor}})
	manifestDescriptor := content.NewDescriptorFromBytes(v1.MediaTypeImageManifest, manifest)
	for _, blob := range []struct {
		descriptor v1.Descriptor
		data       []byte
	}{{layerDescriptor, layer}, {configDescriptor, config}, {manifestDescriptor, manifest}} {
		if err := store.Push(f.ctx, blob.descriptor, bytes.NewReader(blob.data)); err != nil {
			f.t.Fatal(err)
		}
	}
	return f.push(store, manifestDescriptor, repository)
}

func (f *mountsRuntimeFixture) push(store *memory.Store, descriptor v1.Descriptor, repository string) string {
	f.t.Helper()
	if err := store.Tag(f.ctx, descriptor, "fixture"); err != nil {
		f.t.Fatal(err)
	}
	repo, err := remote.NewRepository(f.registry + "/" + repository)
	if err != nil {
		f.t.Fatal(err)
	}
	repo.PlainHTTP = true
	published, err := oras.Copy(f.ctx, store, "fixture", repo, "fixture", oras.DefaultCopyOptions)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.registry + "/" + repository + "@" + published.Digest.String()
}

// FIFO gates keep rendezvous local to the fixture instead of assuming a RUN
// network namespace can connect to a listener on the host loopback interface.
func (f *mountsRuntimeFixture) fifoGate(name string, names ...string) (map[string]*os.File, map[string]any, func()) {
	f.t.Helper()
	directory := filepath.Join(f.work, "gates", name)
	if err := os.MkdirAll(directory, 0700); err != nil {
		f.t.Fatal(err)
	}
	files := map[string]*os.File{}
	closeFiles := func() {
		for _, file := range files {
			_ = file.Close()
		}
	}
	f.t.Cleanup(closeFiles)
	for _, name := range names {
		path := filepath.Join(directory, name)
		if err := syscall.Mkfifo(path, 0600); err != nil {
			f.t.Fatal(err)
		}
		// O_RDWR avoids an open-time writer dependency; O_NONBLOCK lets Go's
		// poller interrupt a pending Read when cleanup closes the descriptor.
		file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0600)
		if err != nil {
			f.t.Fatal(err)
		}
		files[name] = file
	}
	mount := map[string]any{"type": "host", "source": directory, "target": "/gate", "read-only": false, "relabel": "shared"}
	return files, mount, closeFiles
}

func (f *mountsRuntimeFixture) qualifyLockedCache() {
	f.t.Helper()
	files, gate, closeGate := f.fifoGate("locked-cache", "ready", "release-a", "release-b")
	defer closeGate()
	barrier := make(chan error, 1)
	var arrivals [2]byte
	go func() {
		_, err := io.ReadFull(files["ready"], arrivals[:])
		if err == nil && string(arrivals[:]) != "ab" && string(arrivals[:]) != "ba" {
			err = fmt.Errorf("unexpected cache barrier arrivals %q", arrivals[:])
		}
		if err == nil {
			for _, name := range []string{"a", "b"} {
				if _, err = files["release-"+name].Write([]byte("r")); err != nil {
					break
				}
			}
		}
		barrier <- err
	}()
	tasks := map[string]any{}
	for _, name := range []string{"a", "b"} {
		preflight := fmt.Sprintf(`printf %s >/gate/ready; /bin/busybox head -c 1 /gate/release-%s >/ready`, name, name)
		critical := `if ! /bin/busybox mkdir /cache/active; then echo concurrent-cache-writer >&2; exit 29; fi; count=0; if test -e /cache/count; then count=$(/bin/busybox cat /cache/count); fi; count=$((count+1)); printf '%s' "$count" >/cache/count; printf '%s' "$count" >/counter.txt; /bin/busybox rmdir /cache/active`
		tasks[name] = map[string]any{"image": "scratch", "cache": false, "steps": append(mountsRuntimeSteps(preflight, []any{gate}), mountsRuntimeRun(critical, []any{map[string]any{"type": "cache", "id": "stoke-mount-runtime-critical", "sharing": "locked", "target": "/cache"}}))}
	}
	f.setTasks(tasks)
	report, _ := f.run("concurrent-locked-cache", true)
	select {
	case err := <-barrier:
		if err != nil {
			f.t.Fatal(err)
		}
	case <-f.ctx.Done():
		f.t.Fatal(f.ctx.Err())
	}
	counts := []string{}
	for _, name := range []string{"a", "b"} {
		counts = append(counts, string(f.read(filepath.Join(f.task(report, name).Filesystem, "counter.txt"))))
	}
	slices.Sort(counts)
	if !reflect.DeepEqual(counts, []string{"1", "2"}) {
		f.t.Fatalf("locked cache did not serialize writers: %v", counts)
	}
	f.write(filepath.Join(f.work, "cache-sharing.json"), f.encode(map[string]any{"protocol": "fixture-owned-fifo", "arrivals": string(arrivals[:]), "sharing": "locked", "counterValues": counts, "concurrentGuardFailures": 0}), 0600)
	f.checks = append(f.checks, "concurrently-admitted-locked-cache-writers-share-one-persistent-volume")
}

func (f *mountsRuntimeFixture) qualifyCancellation(artifact string) {
	f.t.Helper()
	files, gate, closeGate := f.fifoGate("cancel", "ready")
	defer closeGate()
	command := `/bin/busybox cat /artifact/payload.txt >/started.txt; /bin/busybox sleep 300 & child=$!; printf r >/gate/ready; wait "$child"`
	f.setTasks(map[string]any{
		"cancel": map[string]any{"image": "scratch", "cache": false, "steps": mountsRuntimeSteps(command, []any{
			map[string]any{"type": "artifact", "ref": artifact, "target": "/artifact"},
			map[string]any{"type": "cache", "id": "stoke-mount-runtime-cancel", "sharing": "locked", "target": "/cache"},
			gate,
		})},
		"blocked": map[string]any{"image": "scratch", "needs": []string{"cancel"}, "steps": mountsRuntimeSteps(`echo should-not-run >/blocked.txt`, nil)},
	})
	stdout, descendants := f.cancelAtGate(f.command([]string{"blocked"}), files["ready"])
	report := f.report("cancel", stdout)
	if report.Status != "failed" {
		f.t.Fatalf("canceled workflow status=%q", report.Status)
	}
	statuses := map[string]string{}
	for _, task := range report.Tasks {
		statuses[task.Name] = task.Status
	}
	if statuses["cancel"] != "canceled" || statuses["blocked"] != "skipped" {
		f.t.Fatalf("cancellation/downstream gates: %v", statuses)
	}
	f.write(filepath.Join(f.work, "cancellation-descendants.json"), f.encode(map[string]any{"observedDescendants": descendants, "liveSurvivors": []int{}, "terminalStatuses": statuses}), 0600)
	f.checks = append(f.checks, "artifact-cache-mounted-cancellation-drains-observed-workers-and-blocks-dependent")
}

// cancelAtGate shares subprocess ownership while callers keep their own task,
// readiness command, report type and platform assertions.
func (f *mountsRuntimeFixture) cancelAtGate(cmd *exec.Cmd, gate *os.File) ([]byte, []int) {
	f.t.Helper()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// Do not hang joining Wait if a failed child leaves inherited pipes open.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		f.t.Fatal(err)
	}
	var waitErr error
	done := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	defer func() {
		select {
		case <-done:
		default:
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	var readyErr error
	ready := make(chan struct{})
	go func() {
		var marker [1]byte
		_, readyErr = io.ReadFull(gate, marker[:])
		if readyErr == nil && marker[0] != 'r' {
			readyErr = fmt.Errorf("unexpected cancellation readiness %q", marker[:])
		}
		close(ready)
	}()
	defer func() {
		_ = gate.Close()
		<-ready
	}()
	select {
	case <-ready:
		if readyErr != nil {
			f.t.Fatal(readyErr)
		}
	case <-done:
		f.t.Fatalf("cancellation fixture exited before RUN readiness: %v\n%s", waitErr, &stderr)
	case <-f.ctx.Done():
		f.t.Fatal(f.ctx.Err())
	}
	descendants := mountsRuntimeDescendants(cmd.Process.Pid)
	if len(descendants) == 0 {
		f.t.Fatal("no workers observed at cancellation gate")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		f.t.Fatal(err)
	}
	select {
	case <-done:
		if waitErr == nil {
			f.t.Fatal("canceled workflow succeeded")
		}
	case <-time.After(40 * time.Second):
		f.t.Fatal("canceled workflow did not drain workers")
	}
	f.write(filepath.Join(f.work, "cancel.stdout"), stdout.Bytes(), 0600)
	f.write(filepath.Join(f.work, "cancel.stderr"), stderr.Bytes(), 0600)
	for _, pid := range descendants {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			f.t.Fatal(err)
		}
		end := strings.LastIndexByte(string(data), ')')
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) == 0 || fields[0] != "Z" {
			f.t.Fatalf("observed child %d survived cancellation: %s", pid, data)
		}
	}
	return stdout.Bytes(), descendants
}

func mountsRuntimeDescendants(pid int) []int {
	seen := map[int]bool{}
	queue := []int{pid}
	for len(queue) != 0 {
		parent := queue[0]
		queue = queue[1:]
		tasks, _ := filepath.Glob(filepath.Join("/proc", strconv.Itoa(parent), "task", "*", "children"))
		for _, task := range tasks {
			data, err := os.ReadFile(task)
			if err != nil {
				continue
			}
			for _, field := range strings.Fields(string(data)) {
				child, err := strconv.Atoi(field)
				if err != nil || seen[child] {
					continue
				}
				seen[child] = true
				queue = append(queue, child)
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func (f *mountsRuntimeFixture) requirePrivateCleanup() {
	f.t.Helper()
	leaks, err := mountsRuntimePrivateLeaks(f.work)
	if err != nil {
		f.t.Fatal(err)
	}
	if len(leaks) != 0 {
		f.t.Fatalf("private materializations survived engine drain: %v", leaks)
	}
	f.write(filepath.Join(f.work, "private-cleanup.json"), f.encode(map[string]any{"ownedRoots": []string{filepath.Join(f.work, "tmp"), filepath.Join(f.work, "cache"), filepath.Join(f.work, "state")}, "privateLeaks": []string{}, "persistentNativeCacheVolumesRetained": true}), 0600)
}

// Inspect the actual owned temp parents, including Cache.MountDir and the
// graph-root temp directories selected by worker supervision. Buildah cache
// volumes and retained native image filesystems are deliberately persistent.
func mountsRuntimePrivateLeaks(work string) ([]string, error) {
	var leaks []string
	for _, name := range []string{"tmp", "cache", "state"} {
		root := filepath.Join(work, name)
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				return nil // Never follow a runtime symlink outside these owned roots.
			}
			if strings.HasPrefix(entry.Name(), "buildah-cache-") {
				return filepath.SkipDir
			}
			if filepath.Base(filepath.Dir(path)) == "graphroot" && (entry.Name() == "vfs" || entry.Name() == "overlay") {
				return filepath.SkipDir
			}
			for _, prefix := range []string{
				"stoke-engine-", "coopr-artifact-files-", "coopr-context-", "coopr-worker-",
				"coopr-run-bind-file-", "coopr-buildah-input-", "coopr-named-context-",
				"coopr-instruction-cache-", "coopr-component-cache-",
				".coopr-build-worker-", ".coopr-stage-worker-", ".coopr-api-", ".coopr-run-",
			} {
				if strings.HasPrefix(entry.Name(), prefix) {
					relative, err := filepath.Rel(work, path)
					if err != nil {
						return err
					}
					leaks = append(leaks, relative)
					return filepath.SkipDir
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.Sort(leaks)
	return leaks, nil
}

func TestMountPrivateCleanupChecksWorkerTempAndCacheParents(t *testing.T) {
	work := t.TempDir()
	want := []string{
		"cache/stoke/mounts/coopr-artifact-files-leak",
		"state/stoke/runs/run-fixture/graphroot-tmp/coopr-context-leak",
		"state/stoke/runs/run-fixture/.coopr-build-worker-leak",
		"tmp/stoke-engine-leak",
	}
	for _, relative := range append(slices.Clone(want),
		"cache/stoke/mounts/buildah-cache-1000/volume/coopr-artifact-files-user-content",
		"state/stoke/runs/run-fixture/graphroot/vfs/dir/image/coopr-context-image-content") {
		if err := os.MkdirAll(filepath.Join(work, relative), 0700); err != nil {
			t.Fatal(err)
		}
	}
	leaks, err := mountsRuntimePrivateLeaks(work)
	slices.Sort(want)
	if err != nil || !reflect.DeepEqual(leaks, want) {
		t.Fatalf("owned private leaks=%v, want %v; err=%v", leaks, want, err)
	}
}
