package runner

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strio-dev/stoke/internal/workflow"
)

// This uses the explicitly selected native network mode. Neither host
// networking nor an automatic fallback qualifies isolated network execution.
func TestNetworkedRootlessWorkflowWithCoopr(t *testing.T) {
	if testing.Short() {
		t.Skip("requires an explicit engine, complete local OCI image and native network mode")
	}
	engine := os.Getenv("STOKE_COOPR_BINARY")
	evidence := os.Getenv("STOKE_HOST_EVIDENCE_DIR")
	layout := os.Getenv("STOKE_NETWORK_BASE_LAYOUT")
	mode := os.Getenv("STOKE_NETWORK_MODE")
	if engine == "" || evidence == "" || layout == "" || mode == "" {
		t.Fatal("select STOKE_COOPR_BINARY, STOKE_HOST_EVIDENCE_DIR, STOKE_NETWORK_BASE_LAYOUT and STOKE_NETWORK_MODE")
	}
	graphMode := os.Getenv("STOKE_NETWORK_GRAPH_MODE")
	if graphMode == "" {
		graphMode = "serial"
	}
	if graphMode != "serial" && graphMode != "parallel" {
		t.Fatal("STOKE_NETWORK_GRAPH_MODE must select serial or parallel", graphMode)
	}
	helper, _, _ := strings.Cut(mode, ":")
	if helper != "slirp4netns" && helper != "pasta" {
		t.Fatal("network readiness requires explicit slirp4netns or pasta mode", mode)
	}
	runtime, err := nativeTestRuntime("STOKE_NETWORK_RUNTIME")
	if err != nil {
		t.Fatal(err)
	}
	dnsAddress := os.Getenv("STOKE_NETWORK_DNS_TCP_ADDRESS")
	if dnsAddress == "" {
		dnsAddress = "cache.nixos.org:443"
	}
	dnsName, dnsPortText, err := net.SplitHostPort(dnsAddress)
	dnsName = strings.ToLower(dnsName)
	dnsPort, portErr := strconv.Atoi(dnsPortText)
	if err != nil || portErr != nil || dnsPort < 1 || dnsPort > 65535 || net.ParseIP(dnsName) != nil || len(dnsName) > 253 {
		t.Fatal("STOKE_NETWORK_DNS_TCP_ADDRESS must select a DNS hostname and port", dnsAddress)
	}
	for _, label := range strings.Split(strings.TrimSuffix(dnsName, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			t.Fatal("invalid DNS hostname label", dnsName)
		}
		for _, character := range label {
			letter := character >= 'a' && character <= 'z'
			digit := character >= '0' && character <= '9'
			if !letter && !digit && character != '-' {
				t.Fatal("invalid DNS hostname character", dnsName)
			}
		}
	}
	// Reject legacy numeric IPv4 spellings (for example 127.1 or 0x7f000001)
	// that libc can accept without querying DNS even though net.ParseIP rejects
	// them. A DNS host's final label must begin with an ASCII letter here.
	labels := strings.Split(strings.TrimSuffix(dnsName, "."), ".")
	if last := labels[len(labels)-1]; last[0] < 'a' || last[0] > 'z' {
		t.Fatal("DNS target must have a nonnumeric final hostname label", dnsName)
	}
	dnsAddress = net.JoinHostPort(dnsName, strconv.Itoa(dnsPort))
	if os.Geteuid() == 0 {
		t.Fatal("rootless acceptance requires a non-root invoking user")
	}
	root, err := os.MkdirTemp(evidence, "network-readiness-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	successMode, cancelMode := mode, mode
	pastaPID := ""
	if helper == "pasta" {
		if strings.ContainsAny(root, ",\r\n") {
			t.Fatal("Pasta PID observation needs an evidence path without commas or line breaks")
		}
		_, options, _ := strings.Cut(mode, ":")
		for _, option := range strings.Split(options, ",") {
			name, _, _ := strings.Cut(option, "=")
			if strings.HasPrefix(name, "--pid") || strings.HasPrefix(name, "-P") || name == "--pi" || name == "--" {
				t.Fatal("Pasta readiness owns its PID-file option; remove the supplied PID option", option)
			}
		}
		separator := ":"
		if options != "" {
			separator = ","
		} else {
			successMode, cancelMode = helper, helper
		}
		successMode += separator + "--pid," + filepath.Join(root, "success-pasta.pid")
		pastaPID = filepath.Join(root, "cancel-pasta.pid")
		cancelMode += separator + "--pid," + pastaPID
	}
	t.Log("Retained rootless network evidence:", root)
	record := map[string]any{"status": "FAILED", "networkMode": mode, "nativeGraphMode": graphMode, "expectedNetworkHelper": helper, "runtime": runtime, "dnsTCPAddress": dnsAddress, "successNetworkMode": successMode, "cancelNetworkMode": cancelMode, "engine": engine, "baseLayout": layout, "invokingUID": os.Geteuid(), "registryUsed": false}
	defer func() {
		data, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(filepath.Join(root, "result.json"), append(data, '\n'), 0600); err != nil {
			t.Error(err)
		}
	}()

	host := networkReadinessHost(t)
	listener, err := net.Listen("tcp4", net.JoinHostPort(host.String(), "0"))
	if err != nil {
		t.Fatal("listen on the selected routable host address", err)
	}
	var nonceBytes [16]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	nonce := hex.EncodeToString(nonceBytes[:])
	var requests atomic.Int32
	blocked := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/"+nonce+"/block" {
			blocked <- struct{}{}
			<-r.Context().Done()
			disconnected <- struct{}{}
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/"+nonce {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, nonce+"\n")
	}))
	// NewUnstartedServer owns an initial loopback listener; replace it without
	// retaining an unused socket alongside the declared routable listener.
	_ = server.Listener.Close()
	server.Listener = listener
	server.Config.ReadHeaderTimeout = 5 * time.Second
	server.Config.WriteTimeout = 5 * time.Second
	server.Start()
	t.Cleanup(server.Close)
	endpoint := listener.Addr().(*net.TCPAddr)
	record["httpAddress"], record["httpNonce"] = endpoint.String(), nonce

	caller := filepath.Join(root, "caller")
	if err := os.Mkdir(caller, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(caller, "base"), os.DirFS(layout)); err != nil {
		t.Fatal("copy the complete declared OCI input", err)
	}
	script := fmt.Sprintf(`set -euo pipefail
/bin/readlink /proc/self/ns/net > /network-namespace
/bin/readlink /proc/self/ns/user > /network-user-namespace
exec 3<>/dev/tcp/%s/%d
printf 'GET /%s HTTP/1.0\r\nHost: coopr-network-test\r\nConnection: close\r\n\r\n' >&3
/bin/cat <&3 > /network-http
exec 3>&-
exec 3<&-
while IFS= read -r entry; do
    case "${entry,,}" in *%s*) printf 'DNS test name is present in /etc/hosts\n' >&2; exit 1;; esac
done < /etc/hosts
/bin/cat /etc/resolv.conf >&2
if [ -f /etc/nsswitch.conf ]; then /bin/cat /etc/nsswitch.conf >&2; fi
exec 4<>/dev/tcp/%s/%d
exec 4>&-
exec 4<&-
printf '%s\n' > /network-dns-tcp
printf 'NETWORK_RUN_EXECUTED\n'
`, host.String(), endpoint.Port, nonce, strings.TrimSuffix(dnsName, "."), dnsName, dnsPort, dnsAddress)
	definitionFor := func(script string) string {
		if graphMode == "serial" {
			return fmt.Sprintf("from \"oci:base:image\" {\n run { exec \"/bin/bash\" \"-euc\" %q; }\n}\n", script)
		}
		// Both producers are initially ready and required by the final target.
		// The peer uses COPY, so only the network stage creates a network helper.
		return fmt.Sprintf(`from "oci:base:image" as="network-input" {
 run { exec "/bin/bash" "-euc" %q; }
}
from "scratch" as="parallel-input" {
 copy "/parallel-proof" { file "parallel-proof" "parallel-native-stage\n"; }
}
from "network-input" as="network-result" {
 copy "/parallel-proof" "/parallel-proof" from="parallel-input"
}
`, script)
	}
	target := ""
	if graphMode == "parallel" {
		target = ", target: network-result"
	}
	definition := definitionFor(script)
	if err := os.WriteFile(filepath.Join(caller, "network.coopr"), []byte(definition), 0600); err != nil {
		t.Fatal(err)
	}
	workflowSource := fmt.Sprintf("version: 1\ntasks:\n  network:\n    cache: false\n    coopr: {files: [network.coopr]%s}\n", target)
	if err := os.WriteFile(filepath.Join(root, "workflow.yaml"), []byte(workflowSource), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := workflow.Decode(strings.NewReader(workflowSource))
	if err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"reject"}],"transports":{"oci":{"":[{"type":"insecureAcceptAnything"}]},"containers-storage":{"":[{"type":"insecureAcceptAnything"}]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	stdout, err := os.OpenFile(filepath.Join(root, "stdout.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdout.Close() }()
	stderr, err := os.OpenFile(filepath.Join(root, "stderr.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stderr.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	report, runErr := Run(ctx, w, []string{"network"}, Options{
		ContextDir: caller, StateDir: filepath.Join(root, "state"), CacheDir: filepath.Join(root, "cache"),
		OutputDir: filepath.Join(root, "output"), CooprBinary: engine, StorageDriver: "vfs", Runtime: runtime,
		Network: successMode, SignaturePolicy: policy, Stdout: stdout, Stderr: stderr,
	})
	record["report"], record["httpRequests"] = report, requests.Load()
	if runErr != nil {
		record["error"] = runErr.Error()
		t.Fatal("networked rootless workflow failed; inspect retained logs", runErr)
	}
	if report == nil || report.Status != "passed" || len(report.Tasks) != 1 || report.Tasks[0].Name != "network" {
		t.Fatal("unexpected network workflow report", report)
	}
	filesystem := report.Tasks[0].Filesystem
	if graphMode == "parallel" {
		data, err := os.ReadFile(filepath.Join(filesystem, "parallel-proof"))
		if err != nil || string(data) != "parallel-native-stage\n" {
			t.Fatal("final target did not consume the independent native stage", string(data), err)
		}
		record["parallelPeerConsumed"] = true
	}
	for _, pair := range []struct{ file, namespace string }{{"network-namespace", "net"}, {"network-user-namespace", "user"}} {
		data, err := os.ReadFile(filepath.Join(filesystem, pair.file))
		if err != nil {
			t.Fatal(err)
		}
		actual := strings.TrimSpace(string(data))
		hostNamespace, err := os.Readlink("/proc/self/ns/" + pair.namespace)
		if err != nil || actual == "" || actual == hostNamespace {
			t.Fatal("RUN did not use a distinct namespace", pair.namespace, actual, hostNamespace, err)
		}
		record[pair.namespace+"Namespace"], record["host"+pair.namespace+"Namespace"] = actual, hostNamespace
	}
	data, err := os.ReadFile(filepath.Join(filesystem, "network-http"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(data)), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal("parse actual HTTP response", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK || string(body) != nonce+"\n" || requests.Load() != 1 {
		t.Fatal("HTTP proof differs from the controlled response", response.StatusCode, string(body), requests.Load(), readErr, closeErr)
	}
	data, err = os.ReadFile(filepath.Join(filesystem, "network-dns-tcp"))
	if err != nil || string(data) != dnsAddress+"\n" {
		t.Fatal("hostname resolution/outbound TCP proof missing", string(data), err)
	}
	data, err = os.ReadFile(filepath.Join(root, "stdout.log"))
	if err != nil || strings.Count(string(data), "NETWORK_RUN_EXECUTED") != 1 {
		t.Fatal("fresh network RUN marker missing or duplicated", string(data), err)
	}
	// Cancel an actual connected RUN and verify that observed processes and the
	// TCP peer drain before downstream work can pass its prerequisite gate.
	blockedScript := fmt.Sprintf(`exec 3<>/dev/tcp/%s/%d
printf 'GET /%s/block HTTP/1.0\r\nHost: coopr-network-test\r\n\r\n' >&3
/bin/cat <&3
`, host.String(), endpoint.Port, nonce)
	definition = definitionFor(blockedScript)
	if err := os.WriteFile(filepath.Join(caller, "cancel.coopr"), []byte(definition), 0600); err != nil {
		t.Fatal(err)
	}
	cancelSource := fmt.Sprintf("version: 1\ntasks:\n  blocked:\n    cache: false\n    coopr: {files: [cancel.coopr]%s}\n  downstream:\n    needs: [blocked]\n    image: scratch\n    steps: [{run: [must-not-execute]}]\n", target)
	cancelWorkflow, err := workflow.Decode(strings.NewReader(cancelSource))
	if err != nil {
		t.Fatal(err)
	}
	cancelContext, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	type outcome struct {
		report *Report
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		report, err := Run(cancelContext, cancelWorkflow, []string{"downstream"}, Options{
			ContextDir: caller, StateDir: filepath.Join(root, "cancel-state"), CacheDir: filepath.Join(root, "cache"),
			OutputDir: filepath.Join(root, "cancel-output"), CooprBinary: engine, StorageDriver: "vfs", Runtime: runtime,
			Network: cancelMode, SignaturePolicy: policy, Stdout: stdout, Stderr: stderr,
		})
		done <- outcome{report: report, err: err}
	}()
	select {
	case <-blocked:
	case result := <-done:
		t.Fatal("network cancellation RUN did not reach controlled HTTP peer", result.err)
	case <-ctx.Done():
		t.Fatal("network cancellation RUN did not start", ctx.Err())
	}
	owned := networkReadinessDescendants(t)
	if len(owned) == 0 {
		t.Fatal("no owned execution processes observed while network RUN was blocked")
	}
	if graphMode == "parallel" {
		observed := false
		for pid, process := range owned {
			data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal("observe isolated native stage worker", pid, err)
			}
			command := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
			if len(command) > 0 && command[0] == "coopr-buildah-stage-worker-v1" {
				process.Command = command
				owned[pid] = process
				observed = true
			}
		}
		if !observed {
			t.Fatal("parallel native graph did not expose an isolated stage worker during the blocked RUN", owned)
		}
		record["isolatedStageWorkerObserved"] = true
	}
	if pastaPID != "" {
		// Pasta forks into the background. Its daemon may no longer descend
		// from this test, so bind it to the unique PID file for this RUN.
		pid, process := networkReadinessPastaProcess(t, pastaPID)
		owned[pid] = process
	}
	found := false
	for _, process := range owned {
		found = found || process.Name == helper || (helper == "pasta" && networkReadinessPastaName(process.Name))
	}
	if !found {
		t.Fatal("selected network helper was not observed before cancellation", helper, owned)
	}
	record["networkHelperObserved"] = true
	record["cancellationProcesses"] = owned
	cancelRun()
	select {
	case result := <-done:
		record["cancellationReport"] = result.report
		if !errors.Is(result.err, context.Canceled) || result.report == nil || result.report.Status != "failed" || len(result.report.Tasks) != 2 {
			t.Fatal("network RUN cancellation did not drain/report failure", result.report, result.err)
		}
		for _, task := range result.report.Tasks {
			want := "canceled"
			if task.Name == "downstream" {
				want = "skipped"
			}
			if task.Status != want {
				t.Fatal("cancellation violated prerequisite gate", task)
			}
		}
	case <-time.After(45 * time.Second):
		t.Fatal("network RUN cancellation failed to return within the cleanup bound")
	}
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled network RUN retained its HTTP connection")
	}
	for pid, process := range owned {
		current, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(string(current)[strings.LastIndexByte(string(current), ')')+1:])
		if len(fields) > 19 && fields[19] == process.Start {
			t.Fatal("owned process remains after cancellation acknowledgement", pid, process, string(current))
		}
	}
	record["status"], record["hostnameResolutionAndOutboundTCP"], record["freshRun"], record["cancellationCleanup"] = "PASS", true, true, true
}

type networkReadinessProcess struct {
	Parent  int      `json:"parent"`
	Start   string   `json:"start"`
	Name    string   `json:"name"`
	UID     int      `json:"uid,omitempty"`
	Command []string `json:"command,omitempty"`
}

func networkReadinessPastaName(name string) bool {
	// Pasta is a symlink to Passt; the CPU dispatcher can exec passt.avx2
	// while retaining the original Pasta argv[0].
	return name == "pasta" || name == "pasta.avx2" || name == "passt" || name == "passt.avx2"
}

func networkReadinessPastaProcess(t *testing.T, pidPath string) (int, networkReadinessProcess) {
	t.Helper()
	file, err := os.Open(pidPath)
	if err != nil {
		t.Fatal("Pasta did not write its controlled PID file", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, 32))
	if err := errors.Join(err, file.Close()); err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 || len(data) == 32 {
		t.Fatal("Pasta PID file did not identify a helper process", string(data), err)
	}
	process, err := readNetworkReadinessProcess(pid)
	if err != nil || !networkReadinessPastaName(process.Name) {
		t.Fatal("controlled Pasta helper was not observed before cancellation", pid, process, err)
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	ownedUID := false
	for _, line := range strings.Split(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "Uid:" {
			ownedUID = fields[1] == strconv.Itoa(os.Geteuid()) && fields[2] == strconv.Itoa(os.Geteuid())
		}
	}
	if !ownedUID {
		t.Fatal("controlled Pasta helper belongs to another user", pid, string(status))
	}
	process.UID = os.Geteuid()
	data, err = os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		t.Fatal(err)
	}
	process.Command = strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	if len(process.Command) == 0 || (filepath.Base(process.Command[0]) != "pasta" && filepath.Base(process.Command[0]) != "pasta.avx2") {
		t.Fatal("controlled helper command is not a Pasta invocation", pid, process)
	}
	pidOptions, controlled := 0, false
	for index, argument := range process.Command {
		name, _, _ := strings.Cut(argument, "=")
		if argument == "--pid" {
			pidOptions++
			controlled = index+1 < len(process.Command) && process.Command[index+1] == pidPath
		} else if name == "--pid" || strings.HasPrefix(name, "-P") || name == "--pi" {
			pidOptions++
		}
	}
	if !controlled || pidOptions != 1 {
		t.Fatal("Pasta command does not belong to the unique controlled PID file", pid, process)
	}
	current, err := readNetworkReadinessProcess(pid)
	if err != nil || current.Start != process.Start || current.Name != process.Name {
		t.Fatal("controlled Pasta identity changed during observation", pid, process, current, err)
	}
	return pid, process
}

func readNetworkReadinessProcess(pid int) (networkReadinessProcess, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return networkReadinessProcess{}, err
	}
	text := string(data)
	end, start := strings.LastIndexByte(text, ')'), strings.IndexByte(text, '(')
	if start < 0 || end <= start {
		return networkReadinessProcess{}, errors.New("malformed process name in /proc stat")
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) < 20 {
		return networkReadinessProcess{}, errors.New("missing process identity in /proc stat")
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return networkReadinessProcess{}, err
	}
	return networkReadinessProcess{Parent: parent, Start: fields[19], Name: text[start+1 : end]}, nil
}

func networkReadinessDescendants(t *testing.T) map[int]networkReadinessProcess {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	all := make(map[int]networkReadinessProcess)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		process, err := readNetworkReadinessProcess(pid)
		if err != nil {
			continue // A process can disappear during observation.
		}
		all[pid] = process
	}
	owned := make(map[int]networkReadinessProcess)
	for changed := true; changed; {
		changed = false
		for pid, process := range all {
			_, known := owned[pid]
			_, parentOwned := owned[process.Parent]
			if !known && (process.Parent == os.Getpid() || parentOwned) {
				owned[pid] = process
				changed = true
			}
		}
	}
	return owned
}

func networkReadinessHost(t *testing.T) net.IP {
	t.Helper()
	value := os.Getenv("STOKE_NETWORK_HOST")
	if value == "" {
		// Connecting a UDP socket selects a route without sending a datagram.
		connection, err := net.DialTimeout("udp4", "1.1.1.1:53", 2*time.Second)
		if err != nil {
			t.Fatal("select a host route or set STOKE_NETWORK_HOST", err)
		}
		value = connection.LocalAddr().(*net.UDPAddr).IP.String()
		if err := connection.Close(); err != nil {
			t.Fatal(err)
		}
	}
	ip := net.ParseIP(value).To4()
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
		t.Fatal("STOKE_NETWORK_HOST must select a routable host IPv4 address", value)
	}
	return ip
}
