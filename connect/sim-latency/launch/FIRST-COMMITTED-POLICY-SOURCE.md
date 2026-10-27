# Qualified first-committed policy source

These three files preserve the exact source overlays used by the qualified
host-only policy generator and pure phase-two acceptance checker. They are
operational source evidence, not submission code or standalone commands:

| File | SHA-256 |
| --- | --- |
| `first-committed-production-policy-phases.go.txt` | `1316f45d265649b5b279c9bcb350a90c0687efe5b8adbfc23e450ea153ff1b54` |
| `first-committed-production-policy-phases-tests.go.txt` | `f4460b37fad8b111a541388464335243834f8bc3e6f3dfb218b69c4da769fbc6` |
| `first-committed-native-policy-generator_test.go.txt` | `4a3a21ff515e9b6d595f03247d1f96eaf5ab8c6c177913ef5bb8aec94b31ab3d` |

The files were compiled as controller-package test overlays on the exact
previously qualified protected source closure, not on advancing main. Fourteen
race-enabled guards passed three times; a compiled old-count control failed at
the expected stale-two-seed assertion. The evidence manifest is
`0404b010697e2f36a4e3f3299b4af9ccfd1c6566000f92dc83b96a0726198471`,
retained under
`/home/by/urnetwork/temp/first-seed-policy-qa-20261027.H5ezTIru/protected`.
Those retained base sources, dependency graph, native input and receipts are
required to reproduce the composed test binary; these three overlays alone are
not a self-contained build.

The one authorized generation produced [approval ff32](PRODUCTION-FIRST-COMMITTED-PREPARATION-APPROVAL-2026-10-27.md).
Do not invoke the real-vault/file-generation opt-in as an ordinary test, rerun a
sealed generation, activate a source, or change the selected seed from these
files. Their preservation does not amend frozen evaluator/source identities,
qualify the separate first-seed executor, or prove any empirical launch gate.
