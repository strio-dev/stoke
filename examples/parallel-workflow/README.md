# Parallel workflow

`prepare` copies a small input file. `lint` and the two `tests` variants then
become ready together. `package` waits for `prepare`, `lint` and every `tests`
variant to succeed, then writes `/dist/message.txt` in its image/filesystem output.

From the repository root:

```sh
stoke check -f examples/parallel-workflow/stoke.yaml
stoke plan -f examples/parallel-workflow/stoke.yaml package
stoke run -f examples/parallel-workflow/stoke.yaml package
```

The scheduler's internal four-job bound can accommodate `lint` and both test
variants after `prepare` completes. `strategy.max-parallel: 2` caps the test
matrix separately. Cache hits and short commands can make log timing unsuitable
as proof of overlap; the runtime acceptance fixture uses explicit
synchronization instead.

Execution requires an API-capable Coopr selected with `STOKE_COOPR_BINARY`,
working rootless execution, Docker Hub access for BusyBox and a suitable image
policy. This walkthrough illustrates the scheduling graph; it is distinct from
the local-only acceptance suite. See `docs/guides/parallel-tasks.md` for failure,
cancellation and report semantics.
