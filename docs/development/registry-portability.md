# Qualify registry transport across independent stores

The optional two-guest fixture has been retired. It depended on the removed
per-job result exchange and cannot qualify current stage execution.

Use the existing registry acceptance tests for ordinary cache behavior. Select
reviewed, matching Stoke and Coopr executables and a disposable evidence
directory on a provisioned rootless host. See
[local qualification](local-ci-qualification.md) for the real-workflow fixture
and [contributing](../contributing.md) for the smaller registry suites.

Qualify split stages separately through the generated commands:

1. Freeze the workflow, template and executable identities. Generate the
   selected stages and verify that the native caller remains byte-identical.
2. Run the producer with a writable cache repository in a disposable registry.
3. Run a generated consumer in a fresh process and native store using that
   cache. Check actual output bytes and require the upstream RUN marker to be
   absent while a fresh selected test runs.
4. Repeat with a missing entry and an unavailable payload. Both must fail
   before upstream or selected RUN work, without producing publishable output.
5. Check the native caller's scheduling and failure gates independently.

Use only cache repositories for cross-store reuse. The generated stage invokes
`stoke run --ci TASK`; it passes no job results, references or artifact metadata.
The native caller owns success gates. A matching declared graph establishes
eligible cache reuse, not an immutable output binding to a previous CI attempt.

A loopback registry on one host does not establish distinct-host/user,
native ARM, hosted GitHub Actions or GHCR behavior. Record the exact environment
and commands exercised. Historical hosted results have their own
[source-bound receipts](hosted-ci-qualification.md).
