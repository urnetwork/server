# FIXPLAN1 — decisions + implementation plan for REVIEW2-UPDATE1 open items

Date: 2026-07-20. Decisions by Brien; this is the plan-of-record for the second fix pass.
Source findings: REVIEW2-UPDATE1.md §4 (still open) + §5 (new issues from the first fix wave).

STATUS 2026-07-20 (end of second fix pass): ALL items below IMPLEMENTED with tests —
decisions 1-14 plus the mechanical batch. See FIXPASS2.md for the per-item summary,
file inventory, and test results.

## Decisions (approved)

1. **MinIO retention — code-owned, merge-preserving.** Retention stays a policy set from the
   taskworker retention path (`ApplyStreamRetention`), not a delete-task. Fix the clobber
   (REVIEW2-UPDATE1 §5.2): `minioBlobStore.SetLifecycle` must MERGE — read the bucket's current
   lifecycle, replace only rules whose ID carries the code-owned prefix, preserve all others.
   Retire the ops-side 7-day ILM rule once the code default is confirmed to match.

2. **Reliability sharded writes — assumed, setting removed** (REVIEW2-UPDATE1 §5.1). Delete the
   `client_reliability_sharded_writes` setting and its `sync.OnceValue` gate; writers always shard.
   Keep the legacy READ path in the rollup for old data and cross-generation reads. No config to
   provision. (Rollback of a taskworker past the shard-aware build stays forbidden — the rollup's
   dual-form read is what makes new-reader/old-writer safe, not a flag.)

3. **Prewarm/window-identity dual-use — gate on the drain-complete beacon** (REVIEW2-UPDATE1 §4.4,
   option b). The new container must not prewarm-force window establishment or restore persisted
   identities until the old instance's drain has begun/finished. Reuse the §4.3 generation-tagged
   handoff export (incl. its empty-set completion marker) as that signal; poll-budget expiry is the
   crashed-old fallback. Build the two-container overlap test that currently doesn't exist.

4. **sdk ContractDetails 6 fields — delete outright, no deprecation** (REVIEW2-UPDATE1 §4.6). The
   `CompanionContractId/UsedByteCount/ByteCount/BitRate/TransferPath` + `ReplacesContractId` fields
   were only ever used in test builds — no field users in the field. Remove from `device.go`, the
   cgo/js projections, and coverage. (The VC/rows/C-export deprecated shims from the first wave stay.)

5. **p2p direct-peer routing — INTENTIONAL dual-path, not a regression** (REVIEW2-UPDATE1 §5,
   confirmed by Brien). The dual path (platform + p2p) is wanted; the delta extends the pre-existing
   stream-exclusive-while-connected/gateway-fallback policy to direct-addressed peer traffic
   (`MatchesSend` now also matches `destination.DestinationId == peerId`). The intended safety valve
   is that **backpressure on a path removes it from matching** (the wedged path drops out of route
   selection so the gateway takes over) — not the 15s `WriteTimeout` teardown alone; reword the
   finding to remove the "regression" framing. APPROVED improvement: fix `Downgrade(source)` to also
   match `self.peerId` (mirror `MatchesSend`) so an audit/degrade signal sheds the direct-class
   transport too — currently `Downgrade` matches only `streamId`. Verify the backpressure→drop-from-
   matching path actually engages for the direct class as part of this.

6. **Window-identity store — async + generation** (REVIEW2-UPDATE1 §5.4). Replace the
   mutex-across-redis-store with: assign a monotonic generation under the lock, hand the snapshot to
   an async writer; the writer applies a snapshot only if its generation ≥ the last written
   (superseded snapshots dropped). Removes the lock-across-blocking-redis rule violation while
   keeping the ordering guarantee.

7. **RemoveClientArgs teardown — restore best-effort delete when NOT proxy** (REVIEW2-UPDATE1 §5.3).
   The ctx-done no-op (skip `identityState.Remove` + `api.RemoveNetworkClient`) applies ONLY when an
   identity store is configured (the proxy case, where identities must survive for the replacement).
   With the default nil store (plain sdk apps), restore best-effort delete on teardown so window
   platform-client rows don't leak until idle reap.

8. **Key-event merge overflow — keep drop→terminate-epoch** (REVIEW2-UPDATE1 §5.8). Accept the
   current policy (drop the message, end the epoch → resubscribe + 5-min corrective resync). No code
   change; document `urnetwork_redis_key_event_merge_drops_total` and the storm mode in SIGNALS.md.

9. **Contract-stats emit ordering — per-contract sequence numbers** (REVIEW2-UPDATE1 §5, contract-
   stats reorder). Do NOT hold locks across callbacks (repo rule). Each `ContractStatsEvent` carries
   a monotonically increasing per-contract sequence number; the consumer discards an event whose seq
   is < the last seen for that contract, so a stale `Open=true` arriving after the final close is
   ignored. (A callback-serialization channel+goroutine was considered and rejected in favor of seq
   numbers, which also protect out-of-order delivery across the sdk rpc boundary.)

10. **Handoff/drain timeouts — into a settings object** (REVIEW2-UPDATE1 §5.6). Move the hardcoded
    wg-handoff constants (`WgHandoffInitiateTimeout`, `WgHandoffPollInterval`, request-key TTL) and
    the multi-block budget behavior into the proxy settings object; also fix the multi-block budget
    gap (start the apply budget when the export appears, or scale budget/TTL by blocks×grace) as a
    configurable rather than a constant.

## Also fixing (mechanical, no decision needed)

- Ghost-client token latch: reset `adopted=false` at the top of the DeviceConfirmAdopt tx callback
  (REVIEW2-UPDATE1 §5.9).
- Peer failed-delta wake: call `Resync()` instead of `forceResync.Store(true)` (REVIEW2-UPDATE1 §4/§5).
- Sample worker: wrap in `server.HandleError`; count drops (REVIEW2-UPDATE1 §5.5).
- st ckey pipeline: distinguish `redis.Nil` from real errors per-command (REVIEW2-UPDATE1 §5.7 — money path).
- `gofmt -w taskworker/taskworker_compat_test.go` (REVIEW2-UPDATE1 §5.10).
- fetch_retry trio: Request-object method, AbortSignal during backoff, cancel discarded bodies.
- Additive types.ts decls: `matchedIps`/`matchedHosts`, max-block-actions accessors.
- Doc/gauge-name fixes: `urnetwork_connect_drain_residents_remaining`, excuse residentId claim,
  PEERSSTREAMS2 "exactly v2" wording, drain-gauge `max_over_time` guidance, ip_sni ECH comment.

## Approved 2026-07-20 (second batch)

11. **UDP idle for unbudgeted providers — longer, more performant default** in
    `DefaultProviderLocalUserNatSettings` (unbudgeted providers get a provider-tuned idle rather
    than the general 60s; budgeted devices keep the scaled defaults).
12. **type-65 with remote DoH off / oversized — fail fast**: answer SERVFAIL (and TC-bit where
    truncation is the accurate signal) instead of silently dropping, so resolvers waiting on the
    HTTPS RR don't time out.
13. **ECH outer-SNI — record the trust domain, not the exact host**: detect the ECH extension in
    the ClientHello; when present, record the outer server_name reduced to its registrable/trust
    domain (the outer name must still be under a domain the destination controls — ECH hides the
    exact hostname, not the trust domain). Never record the outer name as if it were the precise
    destination host. Fix the wrong ip_sni.go header comment.
14. **CreateTime lineage on client tokens — restore** via `NewByJwtWithCreateTime` in both
    AuthNetworkClient branches (AuthCodeLogin already shows the pattern), **with tests** pinning
    root-lineage preservation.

## Deferred / needs product call (not in this pass)

- api SIGTERM-during-startup latch inversion — implementing the narrow call-site guard shape
  (quitEvent check before the ready latch + not-ready precedence in taskworker/connect mains);
  flag if sticky-latch semantics are preferred instead.
- Monitor prod rollout vs its P2 backlog (decision 15) and CI invocation of the cgo/wasm guards
  (decision 16) — unanswered.

## Sequence

Mechanical/low-risk first (2, 4, gofmt, Resync, ghost-token, st-ckey, sample-worker), then the
settings/structural ones (10, 7, 6, 9), then the beacon gate + overlap test (3), then docs/rewording
(5, 8, gauge names). Build + targeted tests after each; `go vet` all repos at the end.
