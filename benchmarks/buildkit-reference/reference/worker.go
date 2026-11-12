package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/plugins/snapshots/native"
	"github.com/moby/buildkit/cache/remotecache"
	registrycache "github.com/moby/buildkit/cache/remotecache/registry"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/executor/oci"
	"github.com/moby/buildkit/exporter"
	"github.com/moby/buildkit/frontend"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/filesync"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/bboltcachestorage"
	"github.com/moby/buildkit/solver/llbsolver"
	"github.com/moby/buildkit/util/network/cniprovider"
	"github.com/moby/buildkit/util/network/netproviders"
	"github.com/moby/buildkit/worker"
	"github.com/moby/buildkit/worker/base"
	ociworker "github.com/moby/buildkit/worker/runc"
	"github.com/moby/sys/userns"
	"github.com/tonistiigi/fsutil"
)

type engine struct {
	worker                    *base.Worker
	opt                       *base.WorkerOpt
	keys                      *bboltcachestorage.Store
	sessions                  *session.Manager
	solver                    *llbsolver.Solver
	observer                  *observedExecutor
	cacheExport, cacheImport  string
	imageConfig               []byte
	contextDir, platform      string
	imageDigest, configDigest string
	registryAuth              session.Attachable
}

func newEngine(ctx context.Context, root, runtimeBinary string, access ...registryOptions) (_ *engine, retErr error) {
	if os.Geteuid() != 0 || !userns.RunningInUserNS() {
		return nil, errors.New("worker requires mapped root in a caller-prepared user namespace")
	}
	if root == "" || runtimeBinary == "" {
		return nil, errors.New("worker root and explicit OCI runtime binary are required")
	}
	if len(access) > 1 {
		return nil, errors.New("one registry configuration is supported")
	}
	e := &engine{}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, e.Close())
		}
	}()
	opt, err := ociworker.NewWorkerOpt(root, ociworker.SnapshotterFactory{
		Name: "native", New: native.NewSnapshotter,
	}, true, oci.ProcessSandbox, nil, nil,
		netproviders.Opt{Mode: "host", CNI: cniprovider.Opt{Root: root}}, nil, runtimeBinary, "", false, nil, "", "", nil)
	if err != nil {
		return nil, fmt.Errorf("initialize OCI worker: %w", err)
	}
	e.opt = &opt
	if len(access) == 1 {
		opt.RegistryHosts = access[0].Hosts
		e.registryAuth = access[0].Auth
	}
	// Host means only the verified private RootlessKit network namespace.
	e.observer = &observedExecutor{Executor: opt.Executor}
	opt.Executor = e.observer
	e.worker, err = base.NewWorker(ctx, opt)
	if err != nil {
		return nil, err
	}
	controller := &worker.Controller{}
	if err := controller.Add(e.worker); err != nil {
		return nil, err
	}
	e.keys, err = bboltcachestorage.NewStore(filepath.Join(root, "cache.db"))
	if err != nil {
		return nil, err
	}
	e.sessions, err = session.NewManager()
	if err != nil {
		return nil, err
	}
	e.solver, err = llbsolver.New(llbsolver.Opt{
		WorkerController: controller,
		SessionManager:   e.sessions,
		CacheManager:     solver.NewCacheManager(ctx, "stoke-buildkit-reference", e.keys, worker.NewCacheResultStorage(controller)),
		CacheResolvers:   map[string]remotecache.ResolveCacheImporterFunc{"registry": registrycache.ResolveCacheImporterFunc(e.sessions, opt.ContentStore, opt.RegistryHosts)},
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

func (e *engine) Solve(ctx context.Context, outputDir string) (retErr error) {
	if outputDir == "" {
		return errors.New("local output directory is required")
	}
	if err := os.MkdirAll(outputDir, 0700); err != nil {
		return err
	}
	s, err := session.NewSession(ctx, "stoke-buildkit-reference")
	if err != nil {
		return err
	}
	if e.registryAuth != nil {
		s.Allow(e.registryAuth)
	}
	source, err := fsutil.NewFS(e.contextDir)
	if err != nil {
		return err
	}
	s.Allow(filesync.NewFSSyncProvider(filesync.StaticDirSource{"context": source}))
	targets := []filesync.FSSyncTarget{filesync.WithFSSyncDir(0, outputDir)}
	{
		targets = append(targets, filesync.WithFSSync(1, func(map[string]string) (io.WriteCloser, error) {
			return os.OpenFile(outputDir+".oci.tar", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		}))
	}
	s.Allow(filesync.NewFSSyncTarget(targets...))
	sessionCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	var manager sync.WaitGroup
	var managerErr error
	go func() {
		done <- s.Run(sessionCtx, func(ctx context.Context, _ string, meta map[string][]string) (net.Conn, error) {
			client, server := net.Pipe()
			manager.Add(1)
			go func() {
				defer manager.Done()
				managerErr = e.sessions.HandleConn(ctx, server, meta)
				_ = server.Close()
			}()
			return client, nil
		})
	}()
	defer func() {
		cancel()
		closeErr := s.Close()
		runErr := <-done
		manager.Wait()
		retErr = errors.Join(retErr, closeErr, runErr, managerErr)
	}()
	// Wait for the in-memory session before resolving an exporter that uses it.
	if _, err := e.sessions.Get(sessionCtx, s.ID(), false); err != nil {
		return err
	}
	definition, err := e.definition(ctx, s.ID())
	if err != nil {
		return err
	}
	local, err := e.worker.Exporter(client.ExporterLocal, e.sessions)
	if err != nil {
		return err
	}
	output, err := local.Resolve(ctx, 0, nil)
	if err != nil {
		return err
	}
	request := frontend.SolveRequest{Definition: definition.ToPB()}
	if e.cacheImport != "" {
		request.CacheImports = []frontend.CacheOptionsEntry{{Type: "registry", Attrs: map[string]string{"ref": e.cacheImport}}}
	}
	exports := llbsolver.ExporterRequest{Exporters: []exporter.ExporterInstance{output}}
	if e.cacheExport != "" {
		ce, err := registrycache.ResolveCacheExporterFunc(e.sessions, e.opt.RegistryHosts)(ctx, session.NewGroup(s.ID()), map[string]string{"ref": e.cacheExport, "oci-mediatypes": "true", "image-manifest": "true"})
		if err != nil {
			return err
		}
		exports.CacheExporters = []llbsolver.RemoteCacheExporter{{Exporter: ce, CacheExportMode: solver.CacheExportModeMax}}
	}
	if e.imageConfig != nil {
		image, err := e.worker.Exporter(client.ExporterOCI, e.sessions)
		if err != nil {
			return err
		}
		instance, err := image.Resolve(ctx, 1, map[string]string{"containerimage.config": string(e.imageConfig), "source-date-epoch": "0", "rewrite-timestamp": "true"})
		if err != nil {
			return err
		}
		exports.Exporters = append(exports.Exporters, instance)
	}
	// Internal direct-definition solves bypass the build-history/control API.
	started := time.Now()
	response, err := e.solver.Solve(ctx, s.ID(), s.ID(), request,
		0, exports,
		nil, nil, true, nil, "", false)
	if err == nil {
		e.imageDigest, e.configDigest = response.ExporterResponse["containerimage.digest"], response.ExporterResponse["containerimage.config.digest"]
		emit(map[string]any{"phase": "oci-output", "imageDigest": e.imageDigest, "configDigest": e.configDigest})
	}
	emit(map[string]any{"phase": "solve-cache-export-oci", "seconds": time.Since(started).Seconds(), "runCalls": e.observer.runs.Load(), "error": errorText(err)})
	return err
}

func (e *engine) Close() error {
	if e == nil {
		return nil
	}
	var errs []error
	if e.solver != nil {
		errs = append(errs, e.solver.Close())
	}
	if e.opt != nil {
		errs = append(errs, e.opt.Snapshotter.Close())
		if e.worker != nil {
			errs = append(errs, e.worker.Close())
		} else {
			errs = append(errs, e.opt.MetadataStore.Close(), e.opt.ContentMetadata.Close(), e.opt.ResourceMonitor.Close())
			if e.opt.ProxyProvider != nil {
				errs = append(errs, e.opt.ProxyProvider.Close())
			}
			for _, provider := range e.opt.NetworkProviders {
				errs = append(errs, provider.Close())
			}
		}
	}
	if e.keys != nil {
		errs = append(errs, e.keys.Close())
	}
	return errors.Join(errs...)
}
