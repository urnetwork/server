# Snow native-core unit qualification

Independent Sol 6.1 low execution of seven native winner-review tests, five
once-executed-suite/Snow attestation tests, and five model-policy tests.
Every selection passed three times under the exact Go 1.27.3 source/dependency
receipt, with no skips. Provider transports are mocks: no model inference ran.

These tests cover journal no-reroll boundaries, all-three finding attribution,
missing/mutated/duplicate member evidence, token accounting, policy separation,
and deterministic test-execution/attestation behavior. They do not qualify
actual Snow model compatibility, broker containment, the SQL migration, main
API deployment, the evaluator host, calibration, or production activation.

Complete selected logs and the execution script are retained; large binaries
remain in local private artifacts with their digests in `INDEX.json`.

## Real PostgreSQL migration qualification

`sql-failed/` preserves the original first attempt: the static check passed,
but PostgreSQL rejected an unparenthesized CASE inside a PL/pgSQL IF condition.
No main database was involved, and there was no automatic retry.

`sql-passed/` preserves the corrected source
`e82b4ea3cc08615baa67eabdc8da5ef17ffcedbf`: three selected migration tests
passed three times, without skips (6/6/6 seconds). Each run created and removed
an isolated, networkless, 512 MiB PostgreSQL 18 fixture. The new grouping
regression, historical/source guard checks and real provider/finding predicates
passed. This is nine additional passes, distinct from the 51 native-core passes
in `INDEX.json`; it is not a main migration or deployment receipt.
