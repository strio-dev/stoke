# Native Coopr tasks

Install Coopr `0.6.0` on PATH or select it with `STOKE_COOPR_BINARY`. This example pulls BusyBox from Docker Hub and requires the usual rootless runtime, network access and image signature policy.

From the repository root, validate and run the verified artifact target:

```sh
stoke check -f examples/native-coopr/stoke.yaml
stoke run -f examples/native-coopr/stoke.yaml artifact
```

The component task packages `message.txt`, and the native image definition consumes that component through a typed task-output argument. The test and artifact tasks consume the native image through `from` and `copy.from`. Disabling the build's flattened filesystem export does not remove its image output.

To explicitly export the tested image to a local OCI layout, select `publish`. The destination must be fresh; edit it before running if `/tmp/stoke-native-example-image` already exists.

```sh
stoke run -f examples/native-coopr/stoke.yaml publish
```

Running without targets selects every task, including the declared publication task. This example publishes locally, not to an external registry. Workflow OCI publication carries YAML only; callers still supply these native definition and source files in their context.
