# Share workflows with OCI and Bale

Stoke stores reusable workflow definitions as OCI artifacts. Import exact digests to reuse task templates without depending on a hosted Strio service. Optional declared files travel with their owning Stoke component. Bale carries declared images, native registry inputs and nested workflow dependencies when bundling or relocating them.

## Publish a definition

Write a workflow such as `shared.yaml`:

```yaml
version: 1
tasks:
  verify:
    image: docker.io/library/busybox:1.37
    cache: false
    steps:
      - run: [sh, -eu, -c, 'test -f /etc/passwd']
```

Publish it to a repository you control:

```sh
stoke publish -f shared.yaml registry.example.com/team/stoke-tasks:v1
```

This is an explicit registry write. Substitute a real repository and configure `--authfile` when needed. The command prints a canonical reference with the actual digest. Copy that reference into your consumer; do not invent a digest from the example below.

Portable publication requires every declared registry image to include an explicit registry and tag or digest, such as `docker.io/library/busybox:1.37`. Short names and missing tags are rejected before registry writes. Local execution can still use native Coopr image references. Local image transports are omitted from dependency declarations and do not become portable merely by publishing YAML.

## Import a task

Save this as `stoke.yaml`, replacing the placeholder digest with the published digest:

```yaml
version: 1
imports:
  checks: registry.example.com/team/stoke-tasks@sha256:REPLACE_WITH_PUBLISHED_DIGEST
tasks:
  verify:
    uses: checks/verify
    cache: false
```

The placeholder is deliberately not a valid executable reference. A real import requires 64 lowercase hexadecimal digest characters. Stoke validates the artifact type and payload, resolves templates and detects dependency cycles before execution.

```sh
stoke check
stoke plan verify
stoke run verify
```

`uses` names one task in an imported workflow. Each invocation brings that task and its prerequisites into an independent generated scope, so arguments and task state do not leak between repeated calls. Imports can themselves contain imports. Resolution is limited to depth 16, 64 documents including the root, 1,024 declared tasks across those documents and 1,024 expanded task instances. Local `env`, `workdir`, `cache` and `outputs` customize a template where its resolved task kind permits those fields; a local `strategy` replaces the template's entire matrix strategy; local `needs` adds workflow ordering. Local selectors and steps cannot replace a template's execution in this schema. Native argument task sources and publication sources follow their invocation scope, just like `from` and `copy.from`. Caller-added `needs` keeps the caller scope. Declare component `inputs` and pass their values with `with`; see [reusable inputs](reusable-inputs.md).

## Retrieve or run a remote root

```sh
stoke pull --output received.yaml registry.example.com/team/stoke-tasks:v1
stoke check -f received.yaml
stoke run -f oci://registry.example.com/team/stoke-tasks:v1 verify
```

For a root with declared inputs, supply values with repeated `--input NAME=VALUE` flags on `check`, `plan` and `run`. Publishing and pulling preserve required-input declarations without needing values.

A root reference may use a tag for convenience. Pin it by digest when you need an exact definition. Remote definitions still receive the host context selected by the caller. Publication collects only explicit `assets` relative to the YAML directory; the application source remains a caller input.

YAML-only definitions can use the `--output` example above. A definition with packaged files requires `stoke pull --directory received REFERENCE`, which writes the YAML and assets into a fresh directory. Native templates select packaged definitions with `coopr.files-from: component`; omitted `files-from` continues to use caller context. Literal tasks select helper files with `copy.scope: component`. See [packaged component files](packaged-components.md).

## Carry the dependencies with Bale

Publishing a definition attaches a Bale dependency declaration for its direct workflow imports, non-scratch image references and typed native argument `ref` bindings. Nested declarations let Bale discover the remaining graph. Ordinary Stoke usage embeds the required integration and does not require a Bale executable. Install Bale separately when operating bundles.

Create a recipe `bale.yaml` pointing to the published root:

```yaml
include:
  - registry.example.com/team/stoke-tasks:v1
```

Then capture and save it using Bale's local bundle store:

```sh
bale create -f bale.yaml -t stoke-release:v1 --output-delivery bale.delivery.yaml
bale inspect stoke-release:v1
bale save stoke-release:v1 -o stoke-release.tar
```

Saving materializes the captured content; copying only the workflow YAML is not a complete offline transfer. On the receiving machine:

```sh
bale load -i stoke-release.tar -t received:v1
bale push received:v1 mirror.example.com/releases/stoke:bundle \
  --delivery bale.delivery.yaml --dry-run
```

Edit delivery paths for the receiving registry, review the preview, then deliberately run the same push without `--dry-run`. Use fresh output/archive paths as required by Bale.

## Relocation

Bale delivery writes destination metadata associating original references with delivered repositories and descriptors. Stoke reads that metadata when resolving a relocated definition and applies the dependency destinations, including nested workflow imports, image references and typed native registry arguments. Ambiguous mappings are errors; Stoke must not guess among conflicting destination records.

The definition's original YAML bytes can remain unchanged. A raw OCI graph copy without dependency capture and destination records does not provide equivalent relocation. Keep the captured declarations and metadata together, inspect the bundle inventory and use the delivered root reference from the receiving registry. A local archive alone is not a `stoke run` input in this implementation; deliver its members to an accessible registry first.

Stoke discovers workflow imports, static task images and native `{ref: REGISTRY_REFERENCE}` arguments. Native `ref` bindings require an explicit registry and digest. Stoke applies their delivered locations before invoking Coopr, which resolves nested Coopr components through its own artifact and Bale delivery metadata. String input values, defaults and `with` arguments are not registry dependency declarations and are not rewritten. An `{input: NAME}` binding cannot be combined with `{ref: REFERENCE}`; keep portable registry dependencies explicitly declared. Stoke does not parse references hidden in KDL or ordinary argument strings and does not rewrite native files. Declare additional dynamically selected dependencies in the Bale recipe. Publication task destinations are outputs, so they are neither captured as inputs nor rewritten to delivered locations.

## Artifact contract

The workflow artifact type is `application/vnd.strio.stoke.workflow.v1`. Its first layer uses `application/vnd.strio.stoke.workflow.v1+yaml`. Optional file layers use `application/vnd.strio.stoke.file.v1`, a canonical relative path in `org.opencontainers.image.title` and mode `0644` or `0755` in `io.strio.stoke.file.mode`. File payloads are bounded and must exactly match the YAML `assets` declaration. See [packaged files](packaged-components.md) for limits and source selection. Bale dependency declarations and destination records use Bale's public metadata formats. These records describe portable dependencies; they are separate from workflow execution reports and from container filesystem layers.

## Matrix templates

A published workflow retains its declarative matrix, including axes, inclusion/exclusion rules and execution strategy. It is expanded after import resolution. Literal tasks receive environment variables; native Coopr tasks use explicit matrix argument bindings. Typed native `ref` arguments are declared registry inputs; matrix and ordinary literal values do not add dynamic dependency discovery. A caller inherits the matrix unless it supplies a complete replacement `strategy`. See [task matrices](matrices.md).
