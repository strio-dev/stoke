// Package engine delegates execution to an owned Coopr Unix API service.
package engine

import (
	"encoding/json"
	"io"

	cooprwire "github.com/strio-dev/coopr/api"
)

// Literal programs use the public SDK contract directly. Workflow syntax and
// admission stay in workflow; the runner only lowers its validated operations.
type Program = cooprwire.Program
type State = cooprwire.State
type Source = cooprwire.Source
type Operation = cooprwire.Operation
type Mount = cooprwire.Mount

type StoreOptions = cooprwire.Store
type Output = cooprwire.Output
type CacheOptions = cooprwire.Cache
type RegistryOptions = cooprwire.Registry
type CacheStats = cooprwire.CacheStats
type OperationProgress = cooprwire.OperationProgress
type Options struct {
	Contexts                       map[string]string
	Store                          StoreOptions
	ContextDir, Runtime, Isolation string
	Network                        string
	Jobs                           int
	Output                         Output
	Cache                          CacheOptions
	Registry                       RegistryOptions
	Stdout, Stderr                 io.Writer
	// Progress receives observed worker phases synchronously and must return.
	// Live delivery depends on the selected engine; it does not control scheduling.
	Progress func(OperationProgress)
}
type Result struct {
	ImageID, ManifestDigest, Platform, OCI, Filesystem, Reference string
	// CanonicalReference retains an exact transported payload for publication;
	// ImageID is only the local execution identity after verified import.
	CanonicalReference string
	// CanonicalOCI retains the verified original layout; publication copies its
	// canonical digest instead of re-exporting the native execution image.
	CanonicalOCI    string
	Cache           CacheStats
	CacheAvailable  bool
	ProgressDropped uint64
	Metadata        map[string]json.RawMessage
	Destinations    []cooprwire.Destination
}

func options(o Options) cooprwire.Options {
	return cooprwire.Options{Contexts: o.Contexts, Store: o.Store, ContextDir: o.ContextDir, Runtime: o.Runtime, Isolation: o.Isolation, Network: o.Network, Jobs: o.Jobs, Output: o.Output, Cache: o.Cache, Registry: o.Registry}
}
func result(r cooprwire.Result) Result {
	out := Result{ImageID: r.ImageID, ManifestDigest: r.ManifestDigest, Platform: r.Platform, OCI: r.OCI, Filesystem: r.Filesystem, Reference: r.Reference, Metadata: r.Metadata, Destinations: r.Destinations, ProgressDropped: r.ProgressDropped}
	if r.Cache != nil {
		out.Cache = *r.Cache
		out.CacheAvailable = true
	}
	return out
}
