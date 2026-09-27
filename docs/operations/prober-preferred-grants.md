# Opt-in internal-prober grant lookup

This mitigation is disabled without configuration. It changes only the order
in which the persisted internal prober may allocate its **existing unpaid,
data-only grants**. It does not create credit, replace the identity, delete or
consolidate financial records, or change settlement. Ordinary payers and
zero-byte anchors retain the general allocator and incur no extra SQL.

## Why this is separate from the grant index

Main's September 27 read-only same-snapshot sample returned 325 current
prober grants through the existing network/time index, touching 12,294 shared
buffers. A bounded 16-ID lateral primary-key lookup returned 16 grants with
68 shared hits and one read (about 99.4% fewer execution buffers). The old
backup snapshot was still held. These are query-work measurements, not a
production CPU forecast or evidence that all 16 grants are spendable.

The general lookup remains available as fallback. No new index, migration,
planner setting, vacuum, or interruption of the backup is required by this
mitigation. The independent old-snapshot/vacuum investigation still applies.

## Configuration and candidate review

Place `prober_preferred_grants.yml` in the **reviewed runtime configuration**
for processes that serve contract creation (API and Connect). It is loaded
once per process, so configuration replacement requires a normal rollout.
Do not add a live identity or grant IDs to source, tests, or public logs.

```yaml
version: 1
enabled: true
network_id: "<persisted internal-prober network ID>"
balance_ids:
  - "<reviewed existing grant ID>"
```

Use 1–16 distinct existing grants. Before activation, privately verify each
is owned by the singleton prober network, currently active and in-window,
unpaid, non-Pro, zero original/subsidy revenue, and originally at least the
prober top-up size (currently 512 GiB). Prefer ample remaining lifetime and
headroom. Read **both** PostgreSQL durable bytes and the current per-grant
Redis reservation counter; durable bytes alone are not spendable credit.
This review is a routing choice, not permission to mutate accounting rows.

Missing/disabled/malformed configuration leaves the general allocator in
place. Invalid enabled configuration emits one value-redacted startup-use
error. A wrong configured payer cannot authorize a different account: every
preferred lookup independently verifies the persisted singleton identity.

## Runtime behavior and safeguards

For a configured positive-byte payer, one bounded lateral query makes a
primary-key probe for each configured ID. The query revalidates payer,
activity, start/end boundaries, grant size, free/data-only status and revenue
in the current allocation transaction. The `OFFSET 0` boundary prevents stale
network cardinality estimates from choosing the all-grants network index.

One ordinary Redis pipeline reads the eligible candidates' reservation
counters. Missing counters mean zero, negative counters are clamped to zero,
and all other errors fail closed, as in the general allocator. There is no
cached available balance or cross-request admission budget.

The payer-side client ID deterministically chooses a starting position in
the configured list; origins and reverse companions use the same prober-side
client. The first candidate able to fund the **entire** request is selected.
Otherwise the unchanged earliest-expiry allocator runs from scratch. No
partial preferred reservation or mirror update occurs before fallback.
The original endpoint lifecycle locks, financial inserts, priority derivation,
post-commit mirror increment/TTL, and later settlement remain shared code.

Rotation among configured grants is automatic. Replenishment still belongs
to the existing prober preflight, which can create newer grants outside this
list. If configured grants deplete or expire, fallback preserves availability
but the performance benefit disappears; review a replacement list and roll
configuration normally. Never edit old grant balances, Redis counters or
financial records merely to retain the fast path.

## Verification and rollback

Record process/source identity before comparing intervals. Check
`urnetwork_prober_preferred_grant_total{result="selected"}` against
`fallback_ineligible`, `fallback_reserved` and `error`. These count allocation
attempt outcomes, **not completed probes**. Observe actual PostgreSQL CPU,
grant statement call/buffer rates, pool wait, reservation failures and durable
probe coverage. Do not equate a 99.4% per-query buffer improvement with the
same percentage CPU or sweep-time improvement.

For scale only: a hypothetical 120,000-provider cheap pass with two 1-MiB
reservations per provider reserves about 234.4 GiB. One wholly available
512-GiB grant covers about 2.2 such passes; 16 cover about 35.0. Prefetch,
renewal, retries, overlapping generations and the larger full-probe ramp can
raise reservations substantially; settlement can release them. This is not
a coverage/throughput guarantee or a substitute for fresh headroom checks.

Disable `enabled` and roll the configuration to restore original allocation
order. Existing contracts still settle against the same legitimate grants;
there is no financial rollback or index dependency.

Local gates must set `WARP_TEST_ENV_FAIL_FAST=1`, run the preferred-grant
tests plus existing escrow/window/lifecycle/mirror regressions, and repeat
the focused suite under `-race`. Coordinate the exclusive local database
test lane because `DefaultTestEnv` owns disposable PostgreSQL/Redis state.

## Candidate release evidence (September 27)

The behavioral RED test on the original allocator read 34 synthetic grants
instead of one and allocated the earlier grant. The implementation's first
focused GREEN passed in 3.797s. The expanded model suite passed normally in
81.621s and under the race detector in 90.192s; the controller contract-creation
race smoke passed in 145.556s. The expanded fixture includes origin/companion
rotation, fresh Redis and durable-balance observations, strict eligibility and
time boundaries, ordinary/zero-byte compatibility, insufficient/error no-write
behavior, lifecycle rejection, concurrent increments, and a real PK plan.

The read-only 15:45:31Z candidate sample had only four positive-headroom
grants among the latest 16: 1,898.383 GiB available after per-grant reservation
clamping; three mirrors were absent. Twelve were fully reserved. This sample
is **not a configuration recommendation or a durable drift diagnosis**. A
bounded source-of-truth follow-up had a transport failure before producing a
usable result. Fresh candidate validation and a reviewed runtime configuration
remain required before activation; no production financial or Redis writes
were performed for this candidate.
