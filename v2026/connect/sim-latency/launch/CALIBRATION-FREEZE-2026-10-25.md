# Stage A freeze before first formal observation

**Prepared and qualified for execution; zero measurements at publication.**
This freezes Stage A of the [statistical protocol](CALIBRATION-PROTOCOL-2026-10-25.md),
not production activation or an accepted epoch baseline. Stage B's exact reference
patches, five fresh hidden-seed commitments and randomized order must be published
before its first result; the failed warm-reader pilot is not a qualified faster
reference and cannot be retried for a favorable draw.

## Exact executable and workload bindings

- Analyzer source: `30f340a2779699225ab40ec7f3c5d978525276d2`; actual Go 1.27.3,
  CGO disabled, trimpath enabled, clean VCS metadata. The Makefile `build` target
  ran in the exact private checkout that passed all three full suites.
- Installed root-owned 0555 analyzer:
  `/usr/local/libexec/urnetwork/competition-calibration-30f340a27-ac60524d/sim-latency`;
  SHA-256 `ac60524d915cf8a0a74e1e61d74d4668b04b722b103a7ed0e2b9b2953eb2d52f`.
- Installed root-owned 0555 cohort runner:
  `/usr/local/libexec/urnetwork/competition-calibration-30f340a27-ac60524d/cohort-runner.sh`;
  SHA-256 `bafdd8307a22d585f76a95565abf568ef75f36c982d1218c729147483f929ef7`.
  Its joined execution/logging pipeline retains the singleton lock until the
  terminal receipt is flushed. An older unexecuted template is retained privately
  as obsolete, never an authorized command.
- Evaluated source, ten-repository lock, image, public seed 20261024, scale,
  R9 counts, resources and all timers are unchanged from the protocol. Policy
  typed canonical SHA-256:
  `8918df4980ea63b937410683fd17d530cebfbff1b38724e5bac44b0ce1e46ead`.
  The canonical bytes are [preserved](../baseline/prelaunch-same-seed-r9-2026-10-25/predeclared/evaluation-policy-canonical.json).

## Ordered predeclared cohort identities

The immutable [five-position plan](../baseline/prelaunch-same-seed-r9-2026-10-25/predeclared/plan.json)
SHA-256 is `94eb6f7b6ef670187ab7c173ddf0351817cda4b58e3205e1d7407bb1e19ec4f8`.
It fixes every job/round ID, request SHA and fixture-provenance SHA before any
measurement. Preserve the order; do not substitute successful later positions
or reuse controls from another cohort. Each request explicitly has attempt 1,
with the template's attempt-2 normalization declared in its fixture provenance.
Only identities, output paths and that attempt counter changed; all other
request fields and original provider/no-op bytes were independently verified.

| Position | Job ID | Round ID |
| --- | --- | --- |
| 1 | `beef4079-93ec-401d-8d72-c3080fee4046` | `8aca1c17-7760-4f82-b603-c30aad175691` |
| 2 | `590c6171-1947-481a-b276-d2c1b0f07d20` | `fca14146-73e1-42b6-b198-c16add370c27` |
| 3 | `c01514e1-5b2a-4e8e-96e4-3310d398dada` | `ebb95dc4-8ef0-4d6c-8f39-04b96bd7d8e9` |
| 4 | `466078eb-281c-481f-a141-12a3a8955600` | `0a6d26b3-291f-4aeb-8335-753fb4074629` |
| 5 | `3389e58c-7483-4f30-bece-cee5ba0eec7d` | `52359912-64e0-46b1-aae3-a9b101151a67` |

## Execution and stop conditions

Run only these five declared positions, serially, under the existing canonical
scoring lock and root principal on management CPUs 20,22. The runner verifies
direct local-leaf hashes, immutable script identity, original-only resources
and exclusive first-start ownership. Each normal trusted evaluator invocation
has its own three-hour budget plus bounded safety cleanup; no automatic external
retry is authorized for Stage A. A failure preserves its position and stops the
campaign for diagnosis, never replacement with a more favorable draw.

Do not reject or rerun a complete, placeable no-op solely for a low p-value,
unexpected direction or takeover flag under the provisional floor. Retain it;
the frozen algorithm chooses the release margin from all five observations and
requires zero no-op takeovers under that selected margin. A gate, integrity,
timeout or cleanup failure cannot count as a successful completed cohort.
No tests, builds, other scoring or resource-pressure work may overlap a running
cohort. Keep every original outcome and raw artifact.

After all five complete, assemble the analyzer manifest in this same order from
their original authenticated bundles. Check IDs and request hashes against the
precommitted plan, pin the manifest hash and run the installed exact analyzer.
Do not equate exit 0 with acceptance: require `accepted: true`, all terms and
replay receipts, then freeze the resulting margin before Stage B.

## Qualification receipts and limits

The [code qualification](CALIBRATION-QUALIFICATION-2026-10-25.md) records 55 targeted
tests, nine behavioral pre-fix/fixed proofs, genuine frozen-image replay and three
complete 501-pass suites. Independent native preflight verified help/CLI routing,
expected missing-manifest exit 1 with strict `accepted: false` JSON, no Docker
activity or lock residue, all five input/provenance hashes and zero started
positions. Its [plan verification log](../baseline/prelaunch-same-seed-r9-2026-10-25/predeclared/plan-verification.log)
SHA-256 is `76627d80b0e6e302cc4917f27e286d310bf4c6e8dbea2342ad0a86c1d58b74bc`.

API/worker/hardware labels in these requests are explicit offline fixtures.
This does not prove live readiness, provider-backed review, MinIO durability,
host rebaseline, final scale acceptance or production opening. The fixed public
opening remains **2026-10-28T00:00:00Z**.
