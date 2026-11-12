package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/platforms"
	"github.com/moby/sys/userns"
)

var emitMu sync.Mutex

func emit(value any) {
	emitMu.Lock()
	defer emitMu.Unlock()
	_ = json.NewEncoder(os.Stdout).Encode(value)
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
func main() {
	if err := run(); err != nil {
		emit(map[string]any{"phase": "failed", "error": err.Error()})
		os.Exit(1)
	}
}

func run() (retErr error) {
	contextDir := flag.String("context", "", "clean frozen Stoke checkout")
	state := flag.String("state", "", "owned native BuildKit state; retain only for local warm")
	output := flag.String("output", "", "fresh filesystem output directory; OCI archive is PATH.oci.tar")
	platform := flag.String("platform", "linux/amd64", "native workload platform")
	cacheFrom := flag.String("cache-from", "", "standard registry cache reference; omit for cold/local warm")
	cacheTo := flag.String("cache-to", "", "standard registry cache reference; exported mode=max")
	authfile := flag.String("authfile", "", "explicit private short-lived Docker/containers authfile")
	runtimeBinary := flag.String("runtime", "/usr/bin/runc", "explicit OCI runtime executable")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	for _, p := range []string{*contextDir, *state, *output, *authfile, *runtimeBinary} {
		if !filepath.IsAbs(p) {
			return errors.New("context, state, output, authfile and runtime must be absolute paths")
		}
	}
	requested, err := platforms.Parse(*platform)
	if err != nil {
		return err
	}
	processPlatform := platforms.DefaultSpec()
	if !platforms.OnlyStrict(requested).Match(processPlatform) {
		return errors.New("requested platform must match the reference process DefaultSpec")
	}
	if err := requirePrivateRootlessNetwork(); err != nil {
		return err
	}
	git := func(args ...string) (string, error) {
		command := exec.Command("git", append([]string{"-C", *contextDir}, args...)...)
		data, err := command.Output()
		return strings.TrimSpace(string(data)), err
	}
	commit, err := git("rev-parse", "HEAD")
	if err != nil || commit != sourceCommit {
		return errors.New("context is not the frozen public Stoke source commit")
	}
	status, err := git("status", "--porcelain", "--untracked-files=all", "--ignored=matching")
	if err != nil || status != "" {
		return errors.New("frozen source checkout must be clean, including ignored files")
	}
	tree, err := git("rev-parse", "HEAD^{tree}")
	if err != nil {
		return err
	}
	for _, p := range []string{*output, *output + ".oci.tar"} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("output must be fresh: %s", p)
		}
	}
	emit(map[string]any{"phase": "source-and-profile", "sourceCommit": commit, "sourceTree": tree, "platform": *platform, "snapshotter": "native", "processSandbox": true, "privateRootlessKitNetwork": true, "processDefaultPlatform": platforms.Format(processPlatform), "daemon": false})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	total := time.Now()
	access, cleanup, err := registryAccess(*authfile)
	if err != nil {
		return err
	}
	defer cleanup()
	e, err := newEngine(ctx, *state, *runtimeBinary, access)
	if err != nil {
		return err
	}
	e.contextDir, e.platform, e.cacheImport, e.cacheExport = *contextDir, *platform, *cacheFrom, *cacheTo
	emit(map[string]any{"phase": "engine-startup", "seconds": time.Since(total).Seconds()})
	// Proof hashing/archive inspection is deliberately outside engine core time.
	defer func() {
		if retErr == nil {
			retErr = verifyOutput(*output, *platform, e.imageDigest, e.configDigest)
		}
	}()
	defer func() {
		started := time.Now()
		err := e.Close()
		retErr = errors.Join(retErr, err)
		emit(map[string]any{"phase": "engine-shutdown", "seconds": time.Since(started).Seconds(), "error": errorText(err)})
		emit(map[string]any{"phase": "engine-core-total", "seconds": time.Since(total).Seconds(), "runCalls": e.observer.runs.Load(), "error": errorText(retErr)})
	}()
	started := time.Now()
	err = e.Solve(ctx, *output)
	emit(map[string]any{"phase": "engine-solve-output-total", "seconds": time.Since(started).Seconds(), "error": errorText(err)})
	return err
}

func requirePrivateRootlessNetwork() error {
	if os.Geteuid() != 0 || !userns.RunningInUserNS() || os.Getenv("ROOTLESSKIT_STATE_DIR") == "" {
		return errors.New("requires mapped root inside an explicit stock RootlessKit state directory")
	}
	return requireDifferentNetworkNamespace(os.Getenv("BENCHMARK_OUTER_NETNS"))
}

// The trusted wrapper records its own namespace before RootlessKit starts.
// This is benchmark evidence, not an authentication or authority token.
func requireDifferentNetworkNamespace(outer string) error {
	parts := strings.Split(outer, ":")
	if len(parts) != 2 {
		return errors.New("missing or invalid outer network namespace receipt")
	}
	device, deviceErr := strconv.ParseUint(parts[0], 10, 64)
	inode, inodeErr := strconv.ParseUint(parts[1], 10, 64)
	if deviceErr != nil || inodeErr != nil || inode == 0 {
		return errors.New("invalid outer network namespace receipt")
	}
	own, err := os.Stat("/proc/self/ns/net")
	if err != nil {
		return err
	}
	identity, ok := own.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("network namespace has no Linux inode identity")
	}
	if uint64(identity.Dev) == device && identity.Ino == inode {
		return errors.New("RootlessKit network namespace equals its outer namespace")
	}
	return nil
}
