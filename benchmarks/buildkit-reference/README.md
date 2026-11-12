# Benchmark image addition

This recipe adds a standalone BuildKit0.34.2 reference and the same stock
RootlessKit wrapper for both measured engines. It preserves the existing
Coopr/Stoke image recipe, UID/GID1001, verified setuid helpers, native runtimes,
VFS setting and outer runtime profile. No daemon or product backend is added.
The source directory contains only the six accepted Go files and module files.

Import default.nix from the existing tools preparation expression with its
existing stoke/coopr flakes and native system. The fixed-output vendor hash was verified by a native Nix build. The returned
image/copyTo/skopeo attributes fit the existing preparation/publication steps.
No benchmark binary is downloaded or compiled during a measured job.

Invoke benchmark-rootless with either engine and explicit owned store/cache/
output paths. Pass --runtime /bin/crun to the reference; that is the same native
runtime already supplied by the tools image. Its compatibility must be proven
in the unchanged profile. There is no runtime or security fallback. RootlessKit
uses static existing subid files, private slirp4netns networking, no host-loopback
access, and private /etc and /run copy-up. It does not create a PID namespace;
BuildKit retains ProcessSandbox for each RUN. The existing map ranges produce
inside-to-outer UID/GID maps 0:1001:1, 1:1:1000, 1001:1002:64535.

Stock RootlessKit has an ephemeral api.sock inside its private state directory.
This is its namespace supervisor API, not a BuildKit/control daemon. RootlessKit
owns its shutdown/removal; leftover state or a failed command remains failure.

The outer receipt starts before RootlessKit and ends after its shutdown, covering
Coopr bootstrap as well. Inner BuildKit core timing excludes RootlessKit startup;
output proof hashing is separately outside core timing. Use the same image,
native architecture, runtime/profile, CPU/memory allocation and network policy
for each paired engine. Record uname/RUNNER_ARCH plus the printed CPU, memory and
cgroup receipts. Workload compiler limits remain identical in the frozen recipe.
Cold: empty engine state, no cache-from. Local warm: new process, same state.
Registry warm: new process, fresh state, that engine's own cold registry cache.
Compare actual binary SHA/mode/functional configuration across engines; exact
OCI identity is an own-engine cold/warm assertion, not a cross-engine assumption.

Native reference compilation, vet, Nix packaging and wrapper static checks pass.
crun compatibility, stock RootlessKit lifecycle/networking, real Go execution,
registry cold/export/fresh import, output proof and timing remain runtime gates.
A failure is reported as failure; it never enables extra privileges or changes
host settings.

The manual `engine-compare` scenario reuses the native cache job and its fixed
container options. It checks out the frozen application separately from this
reviewed harness. `compare.sh` executes both engines in the same native job:
cold, local warm after process restart, and registry warm with a fresh store.
It checks actual RUN execution, binary bytes/mode, functional image config and
identical cold/warm OCI outputs within each engine. Coopr uses two public CLI
commands for build and archive export; both are included in its core timer.
Use the core timings for comparison. The outer timer includes different output
proof work; it is a complete qualification wall time, not a pure engine measure.
Coopr's shared cache keys do not prove exclusive ancestry from this run's cold
publication. The result must pass all output checks before reporting speed.
