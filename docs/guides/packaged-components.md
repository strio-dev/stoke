# Package reusable component files

A Stoke component is a reusable workflow or task definition distributed through OCI. It can optionally carry files it owns: helper scripts, templates, fixtures or native Coopr definitions. A YAML-only component remains valid. The application source supplied by the caller is a separate input.

Reusable Coopr components remain OCI build artifacts resolved and executed by Coopr. Packaging a Stoke helper does not convert it into a Coopr component or change who owns container execution and caching.

## Declare the component's files

Add an explicit `assets` allowlist beside `tasks`:

```yaml
version: 1
assets:
  - native/image.coopr
  - scripts/check.sh
tasks:
  build:
    coopr:
      files: [native/image.coopr]
      files-from: component
  verify:
    from: build
    cache: false
    steps:
      - copy:
          scope: component
          sources: [scripts/check.sh]
          destination: /check.sh
      - run: [sh, /check.sh]
```

The checked-in `examples/packaged-workflow` keeps these component files under `component/` and application input under `project/`. Check and run it with an explicitly selected compatible Coopr:

```sh
stoke check -f examples/packaged-workflow/component/stoke.yaml \
  --context examples/packaged-workflow/project
stoke plan -f examples/packaged-workflow/component/stoke.yaml verify
stoke run -f examples/packaged-workflow/component/stoke.yaml \
  --context examples/packaged-workflow/project verify
```

This walkthrough uses a registry BusyBox image and therefore needs network access and a suitable image policy. The acceptance suite uses separate local fixtures.

`assets` paths are canonical relative file paths with `/` separators. Absolute paths, `.`/`..`, escaping paths, backslashes, duplicate or ancestor-conflicting paths are rejected. `stoke.yaml` and its subtree are reserved for the workflow. Every declared path must resolve to a regular file inside the directory containing the local YAML. A directory or glob is not an asset entry; list its files explicitly. Source files outside that directory are rejected, including symlink escapes.

Limits are 256 files, 8 MiB per file and 32 MiB in total. Files preserve bytes and their executable/non-executable distinction; permissions normalize to `0755` or `0644`. Materialized files and directories use normalized timestamps, avoiding extraction-time metadata changes. Coopr owns the resulting context identity and reuse.

`stoke publish` collects this allowlist relative to the YAML's directory. `--context` selects caller input for checking/running tasks; it does not change where component files are collected. An import retains its own component files, even when another imported component uses the same filenames or its task expands into matrix variants.

## Select the file source

| Use | Component-owned source | Caller-owned source |
| --- | --- | --- |
| Literal COPY | `scope: component` | Omit `scope`, or use `scope: context`. |
| Native definition files | `files-from: component` | Omit `files-from`, or use `files-from: context`. |
| COPY inside native Coopr | `from="stoke-assets"` | Omit `from`. |

`copy.scope` and task-output `copy.from` are mutually exclusive. Selecting definition files from the component does not replace the native build's primary caller context. A native file can consume both inputs:

```kdl
from "scratch" {
    copy "message.txt" "/project/message.txt"
    copy "scripts/check.sh" "/check.sh" from="stoke-assets"
}
```

The first source belongs to `--context`; the second belongs to the owning component's `assets`. Reserve the native name `stoke-assets` in asset-using definitions. Coopr named contexts can override matching native source/stage names, so do not declare an unrelated stage or input with that reserved name.

Stoke snapshots only the allowlist into private temporary component directories. Loading a component does not merge those files into the caller checkout. Normal completion and load failures remove only loader-owned directories; successful run outputs and caller files remain caller-owned. During execution, component directories remain available until Coopr has finished or acknowledged cancellation cleanup. There is no shared persistent component-content store.

Keep `TMPDIR` outside the caller's input tree. `check` and `run` reject overlaps with every allocated component directory, including unused imports, before task execution. This prevents temporary package files from entering an ordinary caller-context COPY.

## Consume an OCI Coopr component

Declare registry input arguments with `ref` so Bale can discover and relocate them:

```yaml
coopr:
  files: [native/image.coopr]
  files-from: component
  args:
    TOOLS:
      ref: registry.example.com/team/tools@sha256:REPLACE_WITH_PUBLISHED_DIGEST
```

This is a field example, and its placeholder is deliberately not runnable. Use an actual digest of 64 lowercase hexadecimal characters. A `ref` needs an explicit registry and digest; it is separate from a literal string, task-output `{from: TASK}` or matrix `{matrix: KEY}` binding.

The native file consumes the argument through Coopr's normal scope rules:

```kdl
arg "TOOLS"
from "scratch" {
    arg "TOOLS"
    component "${TOOLS}"
}
```

Stoke records the typed reference in the workflow's Bale dependency declaration and substitutes the verified delivered reference before passing the argument to Coopr. Coopr resolves nested component dependencies through its own artifact metadata and Bale delivery scope. Stoke does not parse or rewrite KDL. References hidden in native source, ordinary strings or dynamic argument selection are not automatically declared by Stoke; include additional dynamic dependencies deliberately when creating a Bale bundle.

When producing a Coopr component, a named context supplied during packaging is not automatically saved as an invocation override. Use Coopr's existing `PACKAGE` capture semantics for files that must be embedded in that generated component. The Stoke package's helper files remain the inputs of the Stoke template that owns them.

## Publish, import and retrieve

Publish to a registry you control, then import the canonical digest returned by the command:

```sh
stoke publish -f component/stoke.yaml registry.example.com/team/checks:v1
stoke run -f oci://registry.example.com/team/checks@sha256:ACTUAL_DIGEST \
  --context project verify
```

Replace the example digest and paths with actual values. Publication uploads the YAML, declared asset layers and Bale dependency metadata. It does not upload the caller project. See [shared workflows](shared-workflows.md) for `imports`, `uses` and Bale transfer commands.

Retrieve a complete package into a fresh directory:

```sh
stoke pull --directory received registry.example.com/team/checks:v1
stoke check -f received/stoke.yaml --context project
```

`--directory` writes `stoke.yaml` and the declared files without overwriting an existing directory. A workflow containing assets requires this option when pulling; stdout and `--output FILE` would drop its files and are rejected. YAML-only artifacts can still use the existing stdout or `--output` forms.

This retrieves one package. Imported Stoke packages and OCI Coopr components remain registry references; use Bale to capture and deliver their complete dependency graph to the receiving registry.

The OCI workflow manifest has one YAML layer followed by deterministic file layers. Layer paths, sizes, digests, modes and the YAML allowlist are checked before materialization. Bale copies those layers as part of the root graph and carries declared imports, images and typed native `ref` dependencies with its public metadata. Use the delivered workflow reference after relocation; a raw graph copy without dependency capture and destination records does not establish equivalent reference resolution.

These operations use the selected Coopr executable. Use Coopr `0.6.0` for the service capabilities described in the [engine guide](coopr-api.md).
