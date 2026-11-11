# Component files and caller context

The component owns `native/image.coopr` and `scripts/check.sh`, listed in its
`assets` allowlist. The application owns `project/message.txt`.

With an API-capable local Coopr selected through `STOKE_COOPR_BINARY`:

```sh
stoke check -f examples/packaged-workflow/component/stoke.yaml \
  --context examples/packaged-workflow/project
stoke plan -f examples/packaged-workflow/component/stoke.yaml verify
stoke run -f examples/packaged-workflow/component/stoke.yaml \
  --context examples/packaged-workflow/project verify
```

The build reads its definition from the component, its application input from
`--context`, and its native helper through the reserved `stoke-assets` context.
The verification tasks demonstrate native and literal component-file copying.
Running needs Docker Hub access for the BusyBox image and a suitable native
image policy. This example is separate from the local-only acceptance fixtures.

Publish the component to a registry you control:

```sh
stoke publish -f examples/packaged-workflow/component/stoke.yaml \
  registry.example.com/team/checks:v1
```

That command publishes the YAML and its two declared files, not the application
project. Import the actual returned digest from a consumer workflow, then select
that consumer's project using `--context`. Use `stoke pull --directory DIR REF`
when retrieving the complete package into a fresh local directory.
