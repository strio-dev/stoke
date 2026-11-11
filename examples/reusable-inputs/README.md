# Reusable string inputs

The local definition accepts a required `version` string and a `mode` string
that defaults to `release`. Both the build and its verification task bind the
same document inputs explicitly.

From the repository root:

```sh
stoke check -f examples/reusable-inputs/stoke.yaml --input version=1.2.3
stoke plan -f examples/reusable-inputs/stoke.yaml --input version=1.2.3 verify
stoke run -f examples/reusable-inputs/stoke.yaml --input version=1.2.3 verify
```

Supply another mode with `--input mode=debug`. Flags precede task names. Input
values are strings; the CLI preserves text after the first `=`. In YAML, quote
numeric-looking and boolean-looking string values.

The separate native example binds the input to a Coopr argument and selects its
declared definition file from component assets:

```sh
stoke check -f examples/reusable-inputs/native.yaml --input version=1.2.3
stoke plan -f examples/reusable-inputs/native.yaml --input version=1.2.3 build
stoke run -f examples/reusable-inputs/native.yaml --input version=1.2.3 build
```

Running requires an API-capable Coopr selected with `STOKE_COOPR_BINARY`, working
rootless execution, Docker Hub access for BusyBox and a suitable image policy.
These walkthroughs are distinct from local-only runtime acceptance fixtures.

To reuse the literal definition, publish `stoke.yaml` to a registry you control
and copy the returned digest into an import. Invoke `checks/verify` with
`with: {version: "1.2.3", mode: debug}`. Each invocation binds its own build
prerequisite; omitted `mode` selects the component default. See the reusable-input
and shared-workflow guides for publication and nested forwarding examples.
