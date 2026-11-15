package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Separate CLI/service processes share only portable results and disposable
// volumes. Each invocation keeps its own native store and exported outputs.
func TestConcurrentCacheWithCoopr(t *testing.T) {
	if testing.Short() {
		t.Skip("requires explicit Stoke/Coopr executables and a rootless runtime")
	}
	binary, engine := os.Getenv("STOKE_BINARY"), os.Getenv("STOKE_COOPR_BINARY")
	evidence, busybox := os.Getenv("STOKE_HOST_EVIDENCE_DIR"), os.Getenv("STOKE_SMOKE_BUSYBOX")
	if binary == "" || engine == "" || evidence == "" || busybox == "" {
		t.Fatal("select STOKE_BINARY, STOKE_COOPR_BINARY, STOKE_HOST_EVIDENCE_DIR and STOKE_SMOKE_BUSYBOX")
	}
	runtime, err := nativeTestRuntime("STOKE_EXPORT_RUNTIME")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(evidence, "concurrent-cache-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("Retained two-process evidence:", root)
	write := func(path string, data []byte, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	caller := filepath.Join(root, "caller")
	data, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(caller, "busybox"), data, 0755)
	policy := filepath.Join(root, "policy.json")
	write(policy, []byte(`{"default":[{"type":"reject"}],"transports":{"oci":{"":[{"type":"insecureAcceptAnything"}]},"containers-storage":{"":[{"type":"insecureAcceptAnything"}]}}}`), 0600)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	type process struct {
		cmd    *exec.Cmd
		cancel context.CancelFunc
		out    string
		log    string
	}
	start := func(label, task string) process {
		t.Helper()
		phase := filepath.Join(root, label)
		write(filepath.Join(phase, "stdout.json"), nil, 0600)
		stdout, err := os.OpenFile(filepath.Join(phase, "stdout.json"), os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stdout.Close() })
		stderr, err := os.OpenFile(filepath.Join(phase, "stderr.log"), os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stderr.Close() })
		child, stop := context.WithCancel(ctx)
		cmd := exec.CommandContext(child, binary, "run", "-f", filepath.Join(caller, "stoke.yaml"), "--context", caller, "--cache-dir", filepath.Join(root, "cache"), "--coopr-binary", engine, "--runtime", runtime, "--network", "none", "--storage-driver", "vfs", "--signature-policy", policy, task)
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		cmd.WaitDelay = 10 * time.Second
		cmd.Env = append(os.Environ(), "XDG_STATE_HOME="+filepath.Join(phase, "state"), "GOMAXPROCS=1")
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Start(); err != nil {
			stop()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			stop()
			if cmd.ProcessState == nil {
				_ = cmd.Wait()
			}
		})
		return process{cmd, stop, stdout.Name(), stderr.Name()}
	}
	read := func(path string) []byte { data, _ := os.ReadFile(path); return data }
	waitReady := func(p process) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !strings.Contains("\n"+string(read(p.log)), "\nCACHE_READY\n") {
			if time.Now().After(deadline) || ctx.Err() != nil {
				p.cancel()
				_ = p.cmd.Wait()
				t.Fatalf("first process never entered RUN: %s", read(p.log))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	wait := func(p process) Report {
		t.Helper()
		err := p.cmd.Wait()
		p.cancel()
		if err != nil {
			t.Fatalf("process failed: %v\n%s", err, read(p.log))
		}
		var report Report
		if err := json.Unmarshal(read(p.out), &report); err != nil {
			t.Fatalf("invalid process report: %v\n%s", err, read(p.out))
		}
		return report
	}
	for _, sharing := range []string{"shared", "locked", "cancel"} {
		var definition strings.Builder
		definition.WriteString("version: 1\ntasks:\n")
		for _, side := range []string{"left", "right"} {
			other := map[string]string{"left": "right", "right": "left"}[side]
			command := fmt.Sprintf("/bin/busybox touch /cache/%s; echo CACHE_READY; i=0; until test -e /cache/%s; do i=$((i+1)); test $i -lt 1000; /bin/busybox sleep .02; done; printf %s > /result", side, other, side)
			if sharing != "shared" {
				command = fmt.Sprintf("/bin/busybox mkdir /cache/active; echo CACHE_READY; /bin/busybox sleep 1; /bin/busybox rmdir /cache/active; printf %s > /result", side)
			}
			mountSharing, cache := sharing, "true"
			if sharing == "cancel" {
				mountSharing, cache = "locked", "false"
				if side == "left" {
					command = "echo CACHE_READY; /bin/busybox sleep 60"
				}
			}
			fmt.Fprintf(&definition, "  %s:\n    image: scratch\n    cache: %s\n    outputs: {filesystem: true}\n    steps:\n      - copy: {sources: [busybox], destination: /bin/busybox}\n      - run: [/bin/busybox, sh, -eu, -c, %q]\n        mounts: [{type: cache, id: concurrency-%s, sharing: %s, target: /cache}]\n", side, cache, command, sharing, mountSharing)
		}
		write(filepath.Join(caller, "stoke.yaml"), []byte(definition.String()), 0600)
		left := start(sharing+"-left", "left")
		waitReady(left)
		right := start(sharing+"-right", "right")
		if sharing == "cancel" {
			left.cancel()
			leftErr := left.cmd.Wait()
			survivor := wait(right)
			if len(survivor.Tasks) != 1 || survivor.Tasks[0].Status != "passed" || string(read(filepath.Join(survivor.Tasks[0].Filesystem, "result"))) != "right" {
				t.Fatalf("cancellation affected the other process: %+v", survivor)
			}
			var canceled Report
			if err := json.Unmarshal(read(left.out), &canceled); err != nil || canceled.Status != "failed" || !strings.Contains(canceled.Error, "context canceled") || leftErr == nil || strings.Contains(canceled.Error, "cleanup") {
				t.Fatalf("canceled process did not drain cleanly: %v\n%s", leftErr, read(left.log))
			}
			continue
		}
		// Always drain the first process, including the baseline's root-lock error.
		rightErr := right.cmd.Wait()
		right.cancel()
		if rightErr != nil {
			left.cancel()
			_ = left.cmd.Wait()
			t.Fatalf("second process could not share cache: %v\n%s", rightErr, read(right.log))
		}
		leftReport := wait(left)
		var rightReport Report
		if err := json.Unmarshal(read(right.out), &rightReport); err != nil {
			t.Fatal(err)
		}
		for side, report := range map[string]Report{"left": leftReport, "right": rightReport} {
			if len(report.Tasks) != 1 || report.Tasks[0].Status != "passed" || string(read(filepath.Join(report.Tasks[0].Filesystem, "result"))) != side {
				t.Fatalf("concurrent %s output mismatch: %+v", sharing, report)
			}
			warm := wait(start(sharing+"-warm-"+side, side))
			if len(warm.Tasks) != 1 || warm.Tasks[0].Cache == nil || warm.Tasks[0].Cache.Hits == 0 || report.Tasks[0].Digest == "" || warm.Tasks[0].Digest != report.Tasks[0].Digest || strings.Contains("\n"+string(read(filepath.Join(root, sharing+"-warm-"+side, "stderr.log"))), "\nCACHE_READY\n") || string(read(filepath.Join(warm.Tasks[0].Filesystem, "result"))) != side {
				t.Fatalf("shared result was not restored exactly: %+v", warm)
			}
		}
	}
}
