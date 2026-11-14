# Conditional production preparation approval

The operator reviewed and approves the exact [canonical preimage](PRODUCTION-PREPARATION-APPROVAL-2026-10-27.canonical.json),
SHA-256 `f69672873a9d7fee71d60287c0921becbebd463560105229644824d13f375294`,
for **control-plane preparation only**. This does not approve public admission,
source activation, ordinary competition evaluation, or a claim that pending
qualification succeeded. Opening remains fixed at **2026-10-28T00:00:00Z**;
passing every required launch gate remains mandatory.

The exact policy preserves the frozen ten-repository C2 checkpoint, evaluator
`98c9061c…`, scorer/simulator `efd1077d…`, Go 1.27.3, R9, 26.9% margin,
three-hour submission bound, one shared baseline per epoch, six full weekly
windows, $20 submission fee, unbounded FIFO admission, cross-epoch repeat
eligibility, and unanimous three-model Snow review. It exposes only the direct
synthetic local config/vault leaves, never main/all or their parents.

Only the preparation boundary changed from the earlier unapproved candidate:
one explicit preparation-only reconciliation may obtain real readiness evidence;
activation and ordinary competition workers remain forbidden before phase-two
acceptance. The 25 incremental guards passed three consecutive times, zero
skips; the compiled old behavior reproduced seven expected failures. Exact
summary: `659ab7f082674e279709cc77539ed118ccd61423f06bd10bf5a2376b58a23ba9`.
The original missing-`rg` harness failure is retained; only the separately
authorized test-list command was corrected, without rebuilding or changing code.

Generated artifacts remain immutable under
`/var/lib/urnetwork/competition-production-policy-phase1-preparation-only-20261027-v1/generated`.
Their generator receipt is `76f335703ef7579f0ad6198b1754a85425af68dc66078ec332cc42bfd7f30836`.
The published copies clarify stale comments only; all non-comment lines match
the generated, natively validated files exactly:

| Published file | SHA-256 |
| --- | --- |
| `config/main/competition.yml` | `53c2e27128efd8dec2462c9f7a0b9d1b076070dcdc962915ee177e247e31c426` |
| `config/main/sim-latency.yml` | `1d5dc9737b5ee99d580d4253fa214adbb27ea73280e69e2b7d4810e0e6945283` |

The original generated hashes remain `b4fe9c11…` and `92e861a0…`; they are not
relabeled as the comment-clarified copies. Earlier sealed `24789`/`0647`/`2ba81`
artifacts remain historical and unmodified.

Deploy this config through the normal config-updater and restart all serving
main API replicas because settings are cached. Main migrations 891–893 were
still pending in the read-only 12:49 UTC audit; apply them through the normal
deployment pipeline. This approval performs no main DB migration or write.
Existing scoped production credentials must remain installed; this change
does not rotate tokens or seeds.

Config deployment alone creates no source pointer or round. Subsequent
bootstrap/preparation must retain the actual reserved target UUID and immutable
manifest, obtain independent API/worker acknowledgments and current-boot R1,
and stop before activation. Do not use ordinary `supervise --once` as a
preparation-only substitute. The matching new native CLI must first be built
and installed with truthful provenance.

The corrected fresh bridge is independently accepted as an **operational
compatibility witness, not statistical equivalence**, receipt
`2af963f0ca0cecef740744da974bd2d0c65a41a24b5d68cac277d076fe84c930`.
Both approved independent seeds, genuine Snow qualification, positive exact-round
host readiness, durable role acknowledgments, final operator review and native
fenced activation remain mandatory. A one-seed amendment is requested but not
approved. No pending gate is waived by this policy hash.

The inherited January 1 season boundary must be reviewed before any future
backlog would collide with another full weekly window. Never truncate accepted
FIFO drain or shorten a window to fit that boundary; extend prospective bounds
through an explicit reviewed policy procedure first.
