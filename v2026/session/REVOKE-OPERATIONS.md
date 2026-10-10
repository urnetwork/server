# Session revocation implementation and operations

Implementation accompanies [REVOKE-FINAL.md](REVOKE-FINAL.md). App UI remains the separate [UI plan](REVOKE-UI-FINAL.md); the shared SDK controller and language bindings are implemented. Production enablement follows the deployment gates below.

## Authority and storage

`server/session` owns JWT parsing, common validation, mint registration, Redis session state, lifecycle locks and durable operation primitives. `model` owns client/auth-code business writes and cleanup. The former `server/jwt` package is merged into `session`. Production signing is private: registered mint helpers, and named internal probe exemptions, are the signing boundary. A source guard rejects production use of `Testing_Sign`.

All validation retains network/user/admin, own-client active/device ownership and credential epoch checks. The additional authority is session marker, otherwise database root, otherwise recursive database ancestors (maximum 1,024). For A → B → C, removing B rejects a legacy recursive C credential; a session credential or root credential naming active A may still authorize active C. Session revoke is a distinct action and retires every JWT carrying that SID.

Session/root-tagged credentials require `exp`. Normal expiry is 30 days; the fleet terminal acceptance horizon is `exp + 60 days + 30 seconds`. Retention adds five minutes and rounds up to milliseconds. Strict local expiry never shortens stored fleet retention. Mint/refresh scripts only extend existing horizons, including shared key deadlines. The live inventory is capped at 1,000; retained identities at 10,000. Full capacity refuses admission without evicting accepted credentials or shortening markers. Existing session refresh remains possible.

Network keys use `{ns_<network_id>}`; metadata and typed last use are separate keys. Revoke writes markers and removes live ZSET members in the same script, before asynchronous SQL cleanup. GET excludes expired/revoked sessions independently of maintenance. Last-use metadata means authenticated use, not packet activity or current device presence. Process and Redis 60-second throttles can combine to 120 seconds of healthy-storage lag, plus the 100ms optional-write budget. Raw IP is never stored in that object.

Postgres migrations add client SID/lineage, auth-code origin, operation journal, redemption receipts and index outbox. Client and code lookup indexes use the repository's restartable concurrent-index migration mechanism. Require all indexes to be valid before enabling management traffic. Mint/association/code transactions use the shared network lifecycle advisory lock; revocation/reset/deletion use its exclusive counterpart, before row locks, at READ COMMITTED.

Operations move from prepared to enforced to complete. Receipt recovery precedes actor revalidation, allowing an applied self-revoke to finish after its actor becomes invalid. Known invalid unapplied actors cancel and release quota. HTTP 200 acknowledges Redis enforcement and reports cleanup pending; 202 is still pending; 503 includes the stable operation ID and an unknown outcome. Reuse that ID. Quotas reserve at most 100 single and 20 bulk operations per rolling 24 hours; replay and no-op completion do not consume another reservation.

Auth-code redemption has a durable, globally unique request ID and immutable prepared claims before consuming a use. A different code with the same ID gets generic `409 request_id_conflict`. A completed redemption is an independent session and can replay after its creator is revoked. An unredeemed code still depends on its origin. Hosted NULL migration chooses one durable SID/lineage under a row lock; restarts reuse it. A hosted producer also checks durable operation targets before renewing a stored binding: unfinished cleanup cannot revive a revoked SID after Redis retention expires, and an applicable prepared operation gives unavailable until recovery resolves it. This is a producer check, not a SQL session lookup on every request.

## Bounded maintenance and connections

`MaintainNetworkSessions` is registered in startup, targets and the auth maintenance workload. It runs 32 fixed shards every 30 seconds, reviewing at most 32 networks per task with a five-second outer budget. Shard zero sweeps first, then expires up to 128 terminal receipts, repairs up to 64 outbox entries, and resumes up to 32 operations. Each stage has its own one-second budget; failures do not starve later stages. Optional backlog metrics run last for at most 100ms. Client cleanup is bounded to 128 rows per operation and rechecks SID under row locks, preserving explicit rebinds to a new session. Unfinished operations are not expired.

Index publication only lowers the next review deadline and updates a revision. Review can raise/remove only when its observed revision still matches. Repair recalculates the network's earliest acceptance/retention deadline. Natural TTLs and list filtering remain correct if an index update is delayed. Normal maintenance does not scan Redis keyspace.

The authentication Redis client has a normal pool, no preflight PING and no hidden command retries. Its total budget is two seconds, shortened by the caller; dial, handshake, pool wait, reads, writes and bounded cluster redirects participate. Required storage failure is 503, not a credential rejection. Optional metadata, hints and metrics cannot turn successful authentication into 401.

Platform transports register before their final auth check and before successful admission. Each has an independent 90-second lease, checked every 60 seconds and anchored at check start. Dependency outage cannot delay retirement indefinitely. Close causes distinguish unavailable (4002), revoked (4003), expired (4004) and invalid (4005). H1's sole writer sends the typed cause before socket teardown, with a one-second blocked-writer fallback; relay work is cancelled immediately.

Stream authorization binds immutable endpoint connection generations. Grants last at most 90 seconds and use authenticated clock exchange plus absolute deadlines, conservatively converted to monotonic receiver deadlines with a 30-second fleet-clock margin. A tagged stream cannot open P2P through an endpoint that lacks lease support; relay remains available. A revoked platform endpoint expires within 90 seconds, and the honest P2P peer expires its final grant within another 90 seconds under the documented timer bounds. Retired generations cannot reopen on delayed frames, proofs or migration. Pre-existing unleased legacy P2P and colluding endpoints are outside that bound.

Protocol allocations are frame types 33 (session revision hint) and 34 (stream authorization), Auth fields 8 (ClientInfo) and 9 (lease support), and appended stream generation/deadline fields. Old Auth readers ignore the additions. One subscriber groups listeners per process/network; revision hints coalesce to five seconds and corrective work runs every 60 seconds. Notifications are an optimization; disabled-notification relay/P2P integration tests exercise timer enforcement.

## Enablement and incident handling

`session_creation_enabled` in vault `auth.yml` defaults false when absent. Keep it false while deploying schema, every API/connect/local/hosted/proxy/taskworker authority, and compatible connect/SDK consumers. Even with creation disabled, tagged credentials continue mandatory registration/check-and-extend and revocation enforcement. Enable new mints only after all producer and admission paths, including the oldest supported rollback binary, implement the full invariant. Enable management through the deployment/feature rollout after the correctness and data-plane gates. A pre-session binary is not a valid rollback once tagged credentials exist.

Before enablement, verify deployed Redis persistence, replication and **no eviction of authority keys**, keyspace event configuration for optional hints, index validity, scheduled shard execution and outbox recovery. Redis remains a denylist: losing acknowledged markers can restore acceptance. SQL recovers application crashes and unfinished cleanup; it is not a durable per-request allowlist. A status read detecting a missing retained receipt and missing expected marker reports unavailable and increments `urnetwork_session_authority_loss_total`. Treat any increase as an authority incident, stop new minting/management writes, investigate and restore the known authority state. Do not disable required marker reads as an availability workaround or claim success from a historical SQL result alone.

Every registered mint samples Redis TIME. More than 15 seconds of disagreement per server, or backwards local time, refuses minting; the per-server bound permits at most 30 seconds fleet spread. Alert before 10 seconds and immediately on any refusal. Observe all API/connect/worker hosts and Redis with the deployment's clock monitoring. The five-minute retention margin is not permission to ignore a broken clock; correct the clocks before resuming mints.

Monitor bounded-label metrics:

| Metric | Operational response |
|---|---|
| `urnetwork_session_storage_seconds{operation,outcome}` | Track p95/p99 and unavailable rate; investigate checks approaching the two-second budget. |
| `urnetwork_session_capacity_refusals_total{capacity}` | Investigate live/retained exhaustion; do not evict old accepted entries. |
| `urnetwork_session_clock_disagreement_seconds`, `urnetwork_session_clock_refusals_total` | Alert at the clock thresholds above. |
| `urnetwork_session_journal_pending{state}`, `urnetwork_session_journal_oldest_seconds{state}` | Alert on prepared/enforced/index work older than two maintenance intervals and sustained growth. Keep unfinished rows for recovery. |
| `urnetwork_session_lease_retirements_total{cause}` | Separate dependency-driven retirement from confirmed revocation/expiry. |
| `urnetwork_session_observation_drops_total` | Presentation freshness degradation; do not weaken auth to repair it. |
| `urnetwork_session_authority_loss_total` | Any increase requires authority-state incident investigation. |

Existing auth rejection, compatibility and state-query metrics continue to report common validation. Do not label metrics with network, client, session or operation IDs.

Legacy network JWTs remain a deliberate coverage limit. Upgrade derives a documented lineage ID, not a historical sign-in identity; old auth-code lineages may group. Original untagged tokens remain valid under their original compatibility rules and may derive the lineage again after its finite marker lifetime. Responses therefore say `legacy_coverage: partial`. Account credential rotation remains the account-wide fallback.

## Executed verification

Local fixture environment: Go 1.27.2, PostgreSQL and Redis 8 containers, 2026-10-09. Server tests use `source ./test-env.sh` with `WARP_TEST_ENV_FAIL_FAST=1`, `-mod=readonly`, `-count=1` and `/tmp/codex-go-cache`. These are local measurements, not production sizing claims.

| Gate | Result |
|---|---|
| All 79 non-root server packages compile | Passed. |
| Session/common-auth, deadlines, inventory/capacity, metadata, recovery, maintenance, clock selection | Passed, 205.084s. Additional authority-loss/signing/expired-target selection passed, 35.001s. |
| Normal model auth-code, hosted, cleanup tests | Passed, 51.582s. Confined model race selection passed, 60.965s. |
| API handlers/router operation status and safe 503 envelopes | Passed, 16.048s / 0.748s. |
| Actual task scheduling, per-network expiry, repair and fair stage budgets | Passed, 24.866s. |
| Real auth Redis stalled handshake/read/write, pool exhaustion, cluster routing and reset isolation | Passed. |
| Actual H1 header/frame/H1+/H3 metadata, admission cutoff and distinct close causes, with race detector | Passed, 82.159s. |
| Established relay and rogue P2P, notifications disabled, independent remote revoke | H1 passed, 66.020s; H3 passed, 7.940s. Honest receiver loses the real P2P transport after injected deadline and cannot obtain another grant. |
| Deterministic sole-writer close order and blocked writer timeout, with race detector | Passed, 2.179s. |
| Proxy mint refusal releases parent cancellation ownership, with race detector | Passed, 9.146s. |
| Hosted single/bulk revocation after marker retention, including unknown prepared outcome, with race detector | Passed, 10.364s. |
| Shared mint commit before exclusive revoke cutoff, and failed optional observation preserving accepted auth, with race detector | Passed, 20.164s. |
| Conservative clock sample including response latency, plus final lifecycle/optional-write rerun, with race detector | Passed, 14.700s. |
| Connect typed metadata/stream authorization/clock ownership and protocol compatibility, with race detector | Passed; final frame/field/legacy selection 3.073s / 1.364s. |
| SDK session controller, exact-generation rejection, persistence failures, API-only sign-out, renewal and RPC metadata/hints, with race detector | Passed; final combined selection 2.431s. |
| Native generator, typed handles/listeners and append-only ABI baselines | Full `sdk/cgo/gen` suite passed, 168.024s; packaging generation updated C#, Java, Python, Ruby and Rust. |
| WASM typed snapshot execution | Passed, 1.045s. |
| TypeScript declarations, OpenAPI/type freshness and npm suite | Passed; 138 tests, no skips. Nullable auth-code request identity has a type-checking regression. |

The maximum-state profile includes 1,000 live sessions and 10,000 retained IDs, including 9,000 real markers. Redis keys used 2,717,332 bytes; complete list was 26.884ms and atomic bulk revoke 20.790ms. A sequential 1,000-client common SQL/Redis validation pass took 1.082s (p50 1.017ms, p99 2.064ms). A 1,000-stream two-endpoint grant renewal pass took 1.577s (p50 1.292ms, p99 3.941ms). Repeat these gates against deployment Redis topology, database pool sizes, fleet fan-out and scheduling load before enablement; local figures do not establish fleet capacity.

The unrestricted full session suite encounters the pre-existing live Apple JWK fetch wait in `TestAppleJwk` and timed out; affected offline/local tests pass. Root server-package tests on this macOS host have pre-existing Linux-only ARIN helper references. No production rollout, external SSO verification or whole-repository integration-suite success is claimed.
