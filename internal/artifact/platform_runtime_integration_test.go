package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

type platformRuntimeTask struct {
	Name, Task, Status, Error, Platform, ImageID, OCI, Filesystem string
	Matrix                                                        map[string]string
	Cache                                                         *struct{ Hits, Misses, Stored, Skipped, Errors int }
}

type platformRuntimeReport struct {
	Status, Error, Directory string
	Tasks                    []platformRuntimeTask
}

type platformRuntimeFixture struct {
	*mountsRuntimeFixture
	image string
}

// TestPlatformsWithCoopr qualifies the authored platform surface through real
// packaged binaries on an AMD64 rootless host with ARM64 binfmt configured.
// QEMU installation and registration are runner prerequisites, not test actions.
func TestPlatformsWithCoopr(t *testing.T) {
	if testing.Short() {
		t.Skip("requires configured rootless ARM64 emulation and packaged Stoke/Coopr executables")
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("this acceptance fixture qualifies ARM64 userspace execution on an AMD64 Linux host")
	}
	if os.Geteuid() == 0 {
		t.Fatal("platform acceptance must run as an ordinary rootless runner account")
	}
	binary, coopr := os.Getenv("STOKE_BINARY"), os.Getenv("STOKE_COOPR_BINARY")
	for name, value := range map[string]string{"STOKE_BINARY": binary, "STOKE_COOPR_BINARY": coopr} {
		if value == "" {
			t.Fatalf("%s must select the configured acceptance executable", name)
		}
		if _, err := os.Stat(value); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	image := os.Getenv("STOKE_PLATFORM_IMAGE")
	if image == "" {
		image = "docker.io/library/alpine:3.23"
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	evidenceParent := os.Getenv("STOKE_PLATFORM_EVIDENCE_DIR")
	if evidenceParent == "" {
		evidenceParent = "/tmp"
	}
	work, err := os.MkdirTemp(evidenceParent, "stoke-platforms-")
	if err != nil {
		t.Fatal(err)
	}
	f := &platformRuntimeFixture{mountsRuntimeFixture: &mountsRuntimeFixture{
		t: t, ctx: ctx, binary: binary, work: work, caller: filepath.Join(work, "caller"),
		workflow: filepath.Join(work, "workflow.json"), policy: filepath.Join(work, "policy.json"), reports: map[string]string{},
	}, image: image}
	t.Logf("Retained platform acceptance evidence: %s", work)
	temp := filepath.Join(work, "tmp")
	if err := os.Mkdir(temp, 0700); err != nil {
		t.Fatal(err)
	}
	f.env = append(os.Environ(), "STOKE_COOPR_BINARY="+coopr,
		"TMPDIR="+temp, "XDG_STATE_HOME="+filepath.Join(work, "state"),
		"XDG_CACHE_HOME="+filepath.Join(work, "cache"), "XDG_DATA_HOME="+filepath.Join(work, "data"),
		"XDG_CONFIG_HOME="+filepath.Join(work, "config"))
	defer func() {
		f.t = t
		f.write(filepath.Join(work, "summary.json"), f.encode(map[string]any{
			"passed": !t.Failed(), "evidence": work, "checks": f.checks, "reports": f.reports,
			"image": image, "hostPlatform": "linux/amd64", "emulatedPlatform": "linux/arm64",
			"emulatorProvisioning": "existing runner QEMU/binfmt; unchanged by fixture", "tlsVerification": true,
		}), 0600)
	}()

	// Permit the selected public fixture repository and local build transports
	// only. TLS verification remains enabled for the image pull.
	repository := strings.SplitN(image, "@", 2)[0]
	if colon := strings.LastIndexByte(repository, ':'); colon > strings.LastIndexByte(repository, '/') {
		repository = repository[:colon]
	}
	accept := []map[string]string{{"type": "insecureAcceptAnything"}}
	f.write(f.policy, f.encode(map[string]any{
		"default": []map[string]string{{"type": "reject"}},
		"transports": map[string]any{
			"docker": map[string]any{repository: accept}, "oci": map[string]any{"": accept},
			"oci-archive": map[string]any{"": accept}, "containers-storage": map[string]any{"": accept},
		},
	}), 0600)
	if err := os.MkdirAll(f.caller, 0700); err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(f.caller, "environment.coopr"), []byte(platformRuntimeDefinition(image, "")), 0644)
	f.write(filepath.Join(f.caller, "fixed-arm.coopr"), []byte(platformRuntimeDefinition(image, "linux/arm64")), 0644)

	t.Run("mixed-platform-cold-and-warm-image-tasks", func(t *testing.T) {
		f.t = t
		f.qualifyMatrixCache()
	})
	t.Run("native-and-inline-environments-inherit-actual-platform", func(t *testing.T) {
		f.t = t
		f.qualifyEnvironments()
	})
	t.Run("conflicting-consumer-platform-blocks-dependent", func(t *testing.T) {
		f.t = t
		f.qualifyConflict()
	})
	t.Run("emulated-worker-cancellation-and-private-cleanup", func(t *testing.T) {
		f.t = t
		f.qualifyArmCancellation()
		f.requirePrivateCleanup()
	})
}

func platformRuntimeDefinition(image, platform string) string {
	from := fmt.Sprintf("from %q", image)
	if platform != "" {
		from += fmt.Sprintf(" platform=%q", platform)
	}
	command := `test "$(/bin/uname -m)" = "$EXPECTED_ARCH"; test "$(/bin/sh -eu -c 'exec /bin/uname -m')" = "$EXPECTED_ARCH"; printf '%s\n' "$EXPECTED_ARCH" >/native-proof.txt; /bin/touch -t 202009131226.40 /native-proof.txt; printf 'STOKE_PLATFORM_NATIVE_EXECUTED:%s\n' "$EXPECTED_ARCH"`
	return fmt.Sprintf("arg \"EXPECTED_ARCH\"\n%s as=\"ci\" {\n    arg \"EXPECTED_ARCH\"\n    env NATIVE_ENV=\"definition\" OVERRIDE=\"definition\" EXPECTED_ARCH=\"${EXPECTED_ARCH}\"\n    workdir \"/bin\"\n    run { exec \"/bin/sh\" \"-eu\" \"-c\" %q }\n}\n", from, command)
}

func platformRuntimeProof(expected string) []any {
	command := fmt.Sprintf(`expected=%s; test "$(/bin/uname -m)" = "$expected"; test "$(/bin/sh -eu -c 'exec /bin/uname -m')" = "$expected"; printf '%%s|%%s|%%s|%%s\n' "$expected" "$NATIVE_ENV" "$OVERRIDE" "$PWD" >/platform-proof.txt; /bin/touch -t 202009131226.40 /platform-proof.txt; printf 'STOKE_PLATFORM_EXECUTED:%%s\n' "$expected"`, expected)
	return []any{map[string]any{"run": []string{"/bin/sh", "-eu", "-c", command}}}
}

func (f *platformRuntimeFixture) command(targets []string) *exec.Cmd {
	args := []string{"run", "--context", f.caller, "--signature-policy", f.policy, "-f", f.workflow}
	args = append(args, targets...)
	cmd := exec.CommandContext(f.ctx, f.binary, args...)
	cmd.Env = f.env
	return cmd
}

func (f *platformRuntimeFixture) run(label string, success bool, targets ...string) (platformRuntimeReport, []byte) {
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
	report := f.captureReport(label, stdout.Bytes())
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

func (f *platformRuntimeFixture) captureReport(label string, stdout []byte) platformRuntimeReport {
	f.t.Helper()
	// The shared evidence helper also compares the emitted and persisted JSON.
	_ = f.report(label, stdout)
	var report platformRuntimeReport
	if err := json.Unmarshal(stdout, &report); err != nil {
		f.t.Fatalf("%s malformed platform report: %v", label, err)
	}
	return report
}

func (f *platformRuntimeFixture) task(report platformRuntimeReport, logical, platform string) platformRuntimeTask {
	f.t.Helper()
	for _, task := range report.Tasks {
		if task.Task == logical && (platform == "" || task.Matrix["platform"] == platform) {
			return task
		}
	}
	f.t.Fatalf("task %s platform %s absent", logical, platform)
	return platformRuntimeTask{}
}

func (f *platformRuntimeFixture) requirePlatform(task platformRuntimeTask, platform string) {
	f.t.Helper()
	if task.Status != "passed" || task.ImageID == "" || task.OCI == "" {
		f.t.Fatalf("%s missing successful image result: %+v", task.Name, task)
	}
	// ARM64's baseline v8 may be reported explicitly by an image config.
	normalize := func(value string) string {
		if value == "linux/arm64/v8" {
			return "linux/arm64"
		}
		return value
	}
	if normalize(task.Platform) != platform {
		f.t.Fatalf("%s reported platform %q, want %q", task.Name, task.Platform, platform)
	}
	config := platformRuntimeImageConfig(f.t, task.OCI)
	actual := config.OS + "/" + config.Architecture
	if config.Variant != "" {
		actual += "/" + config.Variant
	}
	if normalize(actual) != platform {
		f.t.Fatalf("%s exported image config platform=%q, want %q", task.Name, actual, platform)
	}
}

func platformRuntimeImageConfig(t *testing.T, layout string) v1.Image {
	t.Helper()
	read := func(path string, output any) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, output); err != nil {
			t.Fatal(err)
		}
	}
	var index v1.Index
	read(filepath.Join(layout, "index.json"), &index)
	if len(index.Manifests) != 1 {
		t.Fatalf("expected a single-platform OCI export, got %d descriptors", len(index.Manifests))
	}
	descriptor := index.Manifests[0]
	type declaredPlatform struct {
		digest   string
		platform v1.Platform
	}
	var declared []declaredPlatform
	rememberPlatform := func(descriptor v1.Descriptor) {
		if descriptor.Platform != nil {
			declared = append(declared, declaredPlatform{digest: descriptor.Digest.String(), platform: *descriptor.Platform})
		}
	}
	platformString := func(platform v1.Platform) string {
		value := platform.OS + "/" + platform.Architecture
		if platform.Variant != "" && (platform.Architecture != "arm64" || platform.Variant != "v8") {
			value += "/" + platform.Variant
		}
		return value
	}
	for range 4 {
		rememberPlatform(descriptor)
		path := filepath.Join(layout, "blobs", descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded())
		var envelope struct {
			Manifests []v1.Descriptor `json:"manifests"`
			Config    v1.Descriptor   `json:"config"`
		}
		read(path, &envelope)
		if envelope.Config.Digest != "" {
			var config v1.Image
			read(filepath.Join(layout, "blobs", envelope.Config.Digest.Algorithm().String(), envelope.Config.Digest.Encoded()), &config)
			rememberPlatform(envelope.Config)
			for _, declaration := range declared {
				if got, want := platformString(declaration.platform), platformString(config.Platform); got != want {
					t.Fatalf("OCI descriptor %s platform=%q disagrees with actual image config=%q", declaration.digest, got, want)
				}
			}
			return config
		}
		if len(envelope.Manifests) != 1 {
			t.Fatalf("OCI export descriptor has neither one image nor one index child: %s", path)
		}
		descriptor = envelope.Manifests[0]
	}
	t.Fatal("OCI export index nesting exceeds fixture bound")
	return v1.Image{}
}

func (f *platformRuntimeFixture) requireProof(task platformRuntimeTask, want string) {
	f.t.Helper()
	if got := string(f.read(filepath.Join(task.Filesystem, "platform-proof.txt"))); got != want {
		f.t.Fatalf("%s platform proof=%q, want %q", task.Name, got, want)
	}
}

func (f *platformRuntimeFixture) qualifyMatrixCache() {
	f.setTasks(map[string]any{"direct": map[string]any{
		"image": f.image, "platform": map[string]string{"matrix": "platform"}, "workdir": "/bin",
		"strategy": map[string]any{"matrix": map[string]any{"include": []any{
			map[string]string{"platform": "linux/amd64", "uname": "x86_64"},
			map[string]string{"platform": "linux/arm64", "uname": "aarch64"},
		}}},
		"env":   map[string]string{"NATIVE_ENV": "direct", "OVERRIDE": "direct"},
		"steps": platformRuntimeProof(`"$STOKE_MATRIX_UNAME"`),
	}})
	cold, coldLog := f.run("matrix-cold", true)
	warm, warmLog := f.run("matrix-warm", true)
	images := map[string]string{}
	for platform, arch := range map[string]string{"linux/amd64": "x86_64", "linux/arm64": "aarch64"} {
		first, second := f.task(cold, "direct", platform), f.task(warm, "direct", platform)
		for _, task := range []platformRuntimeTask{first, second} {
			f.requirePlatform(task, platform)
			f.requireProof(task, arch+"|direct|direct|/bin\n")
		}
		marker := "STOKE_PLATFORM_EXECUTED:" + arch
		if first.Cache == nil || first.Cache.Misses == 0 || !mountsRuntimeExecuted(coldLog, marker) {
			f.t.Fatalf("%s cold execution missing: cache=%+v", platform, first.Cache)
		}
		if second.Cache == nil || second.Cache.Hits == 0 || second.Cache.Misses != 0 || mountsRuntimeExecuted(warmLog, marker) {
			f.t.Fatalf("%s warm cache did not reuse its result: cache=%+v", platform, second.Cache)
		}
		images[platform] = first.ImageID
	}
	if images["linux/amd64"] == images["linux/arm64"] {
		f.t.Fatal("different platform outputs share one image identity")
	}
	f.write(filepath.Join(f.work, "matrix-cache.json"), f.encode(map[string]any{
		"coldExecuted": []string{"linux/amd64", "linux/arm64"}, "warmExecuted": []string{},
		"verifiedPlatformImageIDs": images, "resultAndExportedConfigAgree": true,
	}), 0600)
	f.checks = append(f.checks, "mixed-platform-real-shell-child-execution-cold-warm-cache-and-oci-metadata")
}

func (f *platformRuntimeFixture) definition(file, arch string) map[string]any {
	return map[string]any{"files": []string{file}, "target": "ci", "args": map[string]string{"EXPECTED_ARCH": arch}}
}

func (f *platformRuntimeFixture) qualifyEnvironments() {
	arm := f.definition("environment.coopr", "aarch64")
	amd := f.definition("environment.coopr", "x86_64")
	fixed := f.definition("fixed-arm.coopr", "aarch64")
	consumer := func(from string) map[string]any {
		return map[string]any{"from": from, "env": map[string]string{"OVERRIDE": "task"}, "steps": platformRuntimeProof(`"$EXPECTED_ARCH"`)}
	}
	inline := func(platform string, definition map[string]any) map[string]any {
		task := map[string]any{"environment": definition, "env": map[string]string{"OVERRIDE": "task"}, "steps": platformRuntimeProof(`"$EXPECTED_ARCH"`)}
		if platform != "" {
			task["platform"] = platform
		}
		return task
	}
	matching := consumer("fixed-base")
	matching["platform"] = "linux/arm64"
	f.setTasks(map[string]any{
		"native-arm": map[string]any{"platform": "linux/arm64", "coopr": arm},
		"explicit":   consumer("native-arm"),
		"inline-a":   inline("linux/arm64", arm), "inline-b": inline("linux/arm64", arm),
		"inline-amd":     inline("linux/amd64", amd),
		"fixed-base":     map[string]any{"coopr": fixed},
		"fixed-explicit": consumer("fixed-base"), "explicit-match": matching,
		"fixed-inline": inline("", fixed),
	})
	report, log := f.run("environments", true)
	producers := 0
	for _, task := range report.Tasks {
		if strings.HasPrefix(task.Name, "stoke-environment-") {
			producers++
		}
	}
	if producers != 3 {
		f.t.Fatalf("platform/input-equivalent inline environments produced %d builders, want three", producers)
	}
	for _, name := range []string{"native-arm", "fixed-base", "explicit", "inline-a", "inline-b", "fixed-explicit", "explicit-match", "fixed-inline"} {
		task := f.task(report, name, "")
		f.requirePlatform(task, "linux/arm64")
		if name != "native-arm" && name != "fixed-base" {
			f.requireProof(task, "aarch64|definition|task|/bin\n")
		}
		if got := string(f.read(filepath.Join(task.Filesystem, "native-proof.txt"))); got != "aarch64\n" {
			f.t.Fatalf("%s native environment RUN proof=%q", name, got)
		}
	}
	amdTask := f.task(report, "inline-amd", "")
	f.requirePlatform(amdTask, "linux/amd64")
	f.requireProof(amdTask, "x86_64|definition|task|/bin\n")
	if !mountsRuntimeExecuted(log, "STOKE_PLATFORM_NATIVE_EXECUTED:aarch64") {
		f.t.Fatal("native ARM64 builder RUN did not execute")
	}
	proofs := map[string]string{}
	for _, name := range []string{"explicit", "inline-a", "inline-b", "fixed-explicit", "explicit-match", "fixed-inline"} {
		proofs[name] = string(f.read(filepath.Join(f.task(report, name, "").Filesystem, "platform-proof.txt")))
	}
	f.write(filepath.Join(f.work, "environment-equivalence.json"), f.encode(map[string]any{
		"proofs": proofs, "generatedProducerCount": producers,
		"nativeRequestedDefault": "linux/arm64", "explicitFromPlatformWithoutTaskOverride": "linux/arm64",
		"consumerPlatformInheritedFromActualResult": true,
	}), 0600)
	f.checks = append(f.checks, "native-platform-default-explicit-stage-override-actual-result-inheritance-and-inline-equivalence")
}

func (f *platformRuntimeFixture) qualifyConflict() {
	f.setTasks(map[string]any{
		"base":     map[string]any{"coopr": f.definition("fixed-arm.coopr", "aarch64")},
		"conflict": map[string]any{"from": "base", "platform": "linux/amd64", "steps": []any{map[string]any{"run": []string{"/bin/sh", "-eu", "-c", "echo STOKE_PLATFORM_CONFLICT_EXECUTED"}}}},
		"blocked":  map[string]any{"image": f.image, "needs": []string{"conflict"}, "steps": []any{map[string]any{"run": []string{"/bin/true"}}}},
	})
	report, log := f.run("platform-conflict", false, "blocked")
	base, conflict, blocked := f.task(report, "base", ""), f.task(report, "conflict", ""), f.task(report, "blocked", "")
	f.requirePlatform(base, "linux/arm64")
	if report.Status != "failed" || conflict.Status != "failed" || blocked.Status != "skipped" {
		f.t.Fatalf("conflict did not enforce downstream gate: workflow=%s conflict=%s blocked=%s", report.Status, conflict.Status, blocked.Status)
	}
	errorText := report.Error + " " + conflict.Error
	if !strings.Contains(errorText, "platform") || !strings.Contains(errorText, "linux/amd64") || !strings.Contains(errorText, "linux/arm64") {
		f.t.Fatalf("conflict lacks requested/actual platform diagnostic: %s", errorText)
	}
	if mountsRuntimeExecuted(log, "STOKE_PLATFORM_CONFLICT_EXECUTED") {
		f.t.Fatal("conflicting consumer executed")
	}
	f.checks = append(f.checks, "explicit-consumer-platform-conflict-fails-before-run-and-blocks-dependent")
}

func (f *platformRuntimeFixture) qualifyArmCancellation() {
	files, gate, closeGate := f.fifoGate("arm-cancel", "ready")
	defer closeGate()
	f.setTasks(map[string]any{
		"cancel": map[string]any{"image": f.image, "platform": "linux/arm64", "cache": false, "steps": []any{map[string]any{
			"run": []string{"/bin/sh", "-eu", "-c", `test "$(/bin/uname -m)" = aarch64; /bin/sleep 300 & child=$!; printf r >/gate/ready; wait "$child"`}, "mounts": []any{gate},
		}}},
		"blocked": map[string]any{"image": f.image, "platform": "linux/arm64", "needs": []string{"cancel"}, "steps": []any{map[string]any{"run": []string{"/bin/true"}}}},
	})
	stdout, descendants := f.cancelAtGate(f.command([]string{"blocked"}), files["ready"])
	report := f.captureReport("cancel", stdout)
	statuses := map[string]string{}
	for _, task := range report.Tasks {
		statuses[task.Task] = task.Status
	}
	if report.Status != "failed" || statuses["cancel"] != "canceled" || statuses["blocked"] != "skipped" {
		f.t.Fatalf("ARM cancellation status=%s tasks=%v", report.Status, statuses)
	}
	f.write(filepath.Join(f.work, "cancellation-descendants.json"), f.encode(map[string]any{
		"platform": "linux/arm64", "observedDescendants": descendants, "liveSurvivors": []int{}, "terminalStatuses": statuses,
	}), 0600)
	f.checks = append(f.checks, "emulated-arm64-worker-cancellation-drains-descendants-and-blocks-dependent")
}
