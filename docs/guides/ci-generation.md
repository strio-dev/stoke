# Generate CI stages

Set the template in the root `stoke.yaml`:

```yaml
ci-template: github
```

```sh
stoke ci generate             # all stages with the repository template
stoke ci generate github test # just test
stoke ci generate --check     # verify generated files without writing
```

The built-in GitHub template writes one composite action per stage or matrix
variant under `.stoke/ci/github/`. Commit the generated directory, including
its file checksums. Keep your handwritten workflow in `.github/workflows/`.
The caller owns checkout, runners, credentials, permissions and success gates.

The publish example under `examples/ci/flat/`
uses authored jobs named `build`, `test`, `release_policy` and `publish`.
In its native workflow, the policy job uses `needs: test` and `publish` waits
for the policy job. The test and publish jobs call their stages using local
`uses:` paths. Configure the example's registry and credentials on the
caller-provisioned runners.

A stage runs `stoke run --ci TASK`. Coopr restores image inputs from its
ordinary local or OCI cache. A missing or unusable producer entry fails
the task without rebuilding the producer. Set a shared `cache-from` and
`cache-to` for separate stores and use `--co-locate TASK` for producers
that cannot be restored. Use deterministic builds for reliable reuse across
jobs. The cache matches build inputs and verifies restored content.

## Choose default task groups

```yaml
groups:
  local: [test]
  delivery: [publish]
default-run: local
default-ci: delivery
```

Groups list authored tasks. Stoke also includes their dependencies. Bare
`stoke run` uses `default-run`. Bare `stoke ci generate` uses `default-ci`.
With no default, each command selects all root tasks. Use
`stoke run delivery` or `stoke ci generate github delivery` to select a group
explicitly. `stoke ci generate github` generates all stages. Use
`stoke ci generate github test` to emit only the test export.
`stoke run --ci` requires an explicit selection.

## Use another template

Use `stoke ci generate --template-file ./stage.tmpl` for a local template or
`stoke ci generate oci://registry.example.com/team/stage-template:v1` for an
OCI template. You can also put that OCI reference in `ci-template`. Pin its
digest for repeatable generation. Explicit selection overrides the root setting.
For a single stage, pass its name after the built-in or OCI selector, or as
the only positional argument with `--template-file`.

Each template contains a YAML header and a Go text template body. The
`output` path is relative to `.stoke/ci/`. The body can be native text
for any CI tool. Review templates and their output before using them in CI.

```text
---
output: jenkins/{{.Job.ID}}.groovy
---
sh '''
{{.Command}}
'''
```

`.Job`, `.Group` and `.Plan` expose the resolved selections.
`.Command` provides a shell invocation of `stoke run --ci`. An OCI
template is an OCI image manifest with an empty JSON config, artifact type
`application/vnd.strio.stoke.ci-template.v1` and one template layer of type
`application/vnd.strio.stoke.ci-template.v1+gotemplate`.
