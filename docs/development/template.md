# Template provenance

Stoke follows the [Coopr](https://github.com/strio-dev/coopr) and
[Bale](https://github.com/strio-dev/bale) repository conventions inspected on
2026-10-29. Both use Just, Nix flakes, Go CLIs under `cmd`, direnv, the MIT
license, Zensical and the shared `strio-zensical-theme` input.

Bale's lock supplies Nixpkgs, flake-parts and shared-theme pins. Its `nix/go.nix` source override is preserved. Docs use the same `mkZensicalSite` and scoped `mkPreview` APIs. The original scaffold reduced package/check recipes to its dependency-free CLI. It did not copy sibling containers, VM integration suites, release/tag conventions, Pages publication or product runtime assumptions.

CI uses the siblings' pinned checkout/Nix installer and runs flake checks on
x86_64 and ARM Linux. Hosted qualification is recorded separately from local
checks.

Stoke uses Strio palette red `#B34753` as its docs accent, with hover
`#B14652` in light and dark modes. The theme derives these from its shared
palette. The approved artwork uses the Direction 03 Continuous Rail symbol
and an outlined Nimbus Sans wordmark. `stoke-mark.svg` and
`stoke-lockup-original.svg` retain their original bytes with SHA-256 values
`1af3c39070ef8bdba9fdb2da40b820de4e8be555d0e688de53c30a862d059c8d` and
`3392bf56ad59cdbb3a040a349c8aaf4df26175a3fe005023e00cdf72e28e7d6e`.
The wordmark's outer canvas follows the theme's 70% lowercase-body convention
with the outlined paths and spacing preserved. The favicon uses the filled
symbol on the siblings' 64px white rounded canvas.

## First implementation

Historically, the initial runtime implementation replaced the dependency-free CLI scaffold with a YAML workflow frontend over Coopr's published `0.5.0` root Go package and Bale `v0.1.3` metadata integration. The original prep evidence remains historical; current acceptance belongs to the implementation checks and runtime evidence, not the prep report. That embedded build needed Coopr's CGO/native dependencies and tags. That historical implementation used no local Coopr module replacement, separate Coopr executable, shared storage service or new documentation framework.

Documentation now follows the sibling task-oriented navigation: getting started, tutorial, guides, concepts and reference. Approved assets, palette and homepage action destinations remain unchanged.

The authorized Unix API migration supersedes that embedding choice. Stoke is a standalone wire client and owns a workflow-scoped Coopr service; Coopr retains native build/runtime dependencies in its executable. Coopr `0.6.0` provides the API and the SDK revision used by Stoke. See [the boundary decision](coopr-boundary.md).
