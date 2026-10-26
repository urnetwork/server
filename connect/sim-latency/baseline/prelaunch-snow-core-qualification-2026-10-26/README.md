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
