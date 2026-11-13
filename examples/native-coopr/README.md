# Native Coopr tasks

Install Coopr `0.6.0` on PATH or select it with `STOKE_COOPR_BINARY`. This example pulls BusyBox from Docker Hub and requires the usual rootless runtime, network access and image signature policy.

From the repository root, validate and run the verified artifact target:

```sh
stoke check -f examples/native-coopr/stoke.yaml
stoke run -f examples/native-coopr/stoke.yaml artifact
```

The component task packages `message.txt`, and the native image definition consumes that component through a typed task-output argument. The test and artifact tasks consume the native image through `from` and `copy.from`. An export step in the artifact task snapshots `/output.txt` to `artifacts/native/output.txt` in the caller workspace. That destination must be fresh; move or remove an earlier export before repeating the example. Cached execution still restores the selected file. Tasks with export steps omit the full rootfs by default; other image tasks retain it. `outputs.filesystem: true` explicitly includes the full filesystem without changing image outputs.

To explicitly export the tested image to a local OCI layout, select `publish`. The destination must be fresh; edit it before running if `/tmp/stoke-native-example-image` already exists.

```sh
stoke run -f examples/native-coopr/stoke.yaml publish
```

Running without targets selects every task, including the declared publication task. This example publishes locally, not to an external registry. Workflow OCI publication carries YAML only; callers still supply these native definition and source files in their context.
