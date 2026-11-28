# FIXPASS2 — second fix pass: implementation of FIXPLAN1 (2026-07-20)

All FIXPLAN1 decisions (1-14) plus the mechanical batch are IMPLEMENTED with tests. Every open
P0/P1/P2 from REVIEW2.md / REVIEW2-UPDATE1.md is now closed (fixed, or decided-and-documented
where the decision was "ratify"). All five repos build, vet, and gofmt clean; suites listed in §4
pass. Nothing committed — everything is in the working trees for review.

## 1. Decisions implemented (FIXPLAN1 §1-14)

1. **MinIO retention: code-owned, merge-preserving** — `blob.go` `SetLifecycle` now
   read-merge-writes: replaces only rules matching by exact ID under the
   `urnetwork-ttl-` owned prefix; ops rules and (shared-bucket) other envs' rules survive; a
   failed lifecycle READ aborts instead of blind-clobbering. `TestMergeOwnedLifecycleRules`.
   *Ops follow-up*: retire the ops-side 7-day ILM rule after confirming the code default (7d).
2. **Reliability sharded writes assumed; setting removed** —
   `model/network_client_reliability_model.go` gate deleted; writers always shard; rollup keeps
   the legacy read. `TestClientReliabilityStatsShardedWriter` (inverted); RELIABILITY2.md §status
   + §4 rewritten. **No config to provision — the previously-scheduled settings update is moot.**
3. **Prewarm gated on the drain-complete beacon** — `cli/proxy/main.go` sequences
   `ApplyWgHandoff` (blocks until export-or-budget; the empty export is the completion marker)
   *then* `Prewarm`, same goroutine; readiness/flip not gated; lazy opens untouched; crashed-old
   fallback = budget expiry. Dual-use eliminated for the prewarm set (trade: that set turns warm
   at old-drain-end, not at flip). PROXYDRAIN1 §3.2/§4/§7/§8 updated.
   `TestProxyDeployOverlapPrewarmGate` — a REAL two-instance overlap test (holds at gate ≥3s with
   the old instance live, ungates on export, reuses the persisted identity, fallback covered).
4. **ContractDetails 6 fields deleted outright** — removal (from commit 54add5d) ratified; this
   pass verified zero stragglers repo-wide, regenerated the cgo surface byte-identical (headers
   carry only the remaining fields; the 4 compat exports + ABI baseline intact), and confirmed
   the deprecated ContractClientRow shim computes its aggregates from live rows (no dependency).
5. **p2p dual-path ratified; Downgrade fixed** — `transport_p2p.go` `Downgrade` now also matches
   `peerId` (mirrors MatchesSend incl. the zero-guard). Backpressure valve verified: a full p2p
   route channel makes `MultiRouteSelector.WriteDetailed`'s non-blocking pass fall through to the
   gateway per-write. `TestP2pSendTransportDowngradeMatchesPeer`.
6. **Window-identity store: async + generation** — `ip_remote_multi_client_identity.go` assigns a
   monotonic generation + snapshot under the lock, a single coalescing writer goroutine stores
   OUTSIDE the lock and drops superseded generations; bounded; ctx-exit with best-effort final
   drain. `MultiClientIdentityStore` interface UNCHANGED (server adapter untouched).
   `TestWindowIdentitySlowStoreDoesNotBlock` + adapted ordering test; server proxy identity suite
   re-run green against the final connect.
7. **Teardown scope: best-effort delete when no identity store** —
   `ip_remote_multi_client_api.go` ctx-done branch gated on `hasStore()`; with no store the
   platform client is deleted via a one-shot 30s Background-ctx POST (the generator ctx is
   already done — matches the repo's shutdown-cleanup precedent); proxy (store set) behavior
   unchanged. Three-way test (no-store deletes / store preserves / live eviction deletes).
8. **Key-event merge overflow: drop→terminate-epoch kept** — no code change;
   `urnetwork_redis_key_event_merge_drops_total` + the storm mode documented in SIGNALS.md §9.
9. **Contract-stats per-contract sequence numbers** — producer (connect): `ContractStatsEvent.Sequence
   uint64`, starts at 1, monotonic per ContractId, stamped under `contractStatsLock` at snapshot
   time; callbacks stay outside locks; counter dropped at full close.
   Consumer (sdk): `deviceContractTracker` discards `Sequence <= lastSeen` per contract
   (Sequence==0 bypasses for unsequenced sources); high-water map bounded by tracker lifecycle
   (dropped when the tombstone evicts). Covers every consumer (both device feeds; the VC consumes
   tracker output). Producer + two consumer tests, ×-race.
10. **Handoff timings in settings** — `ProxySettings.WgHandoffPollTimeout` (15min default, sized
    for serialized multi-block host drains; doubles as the prewarm-gate budget) and
    `WgHandoffRequestTtl` (20min; applied via new ttl params on the model funcs);
    `WgHandoffInitiateTimeout` now starts at export-seen (fixes the budget-consumed-by-flock gap);
    poll-expiry DeadlineExceeded handled as clean no-export (`TestProxyWgHandoffPollExpiry`).
11. **Provider UDP idle** — `DefaultProviderLocalUserNatSettings`: unbudgeted providers get the
    historical 300s UDP idle (`providerUdpIdleTimeout`); budgeted devices keep scaled defaults
    incl. 60s. Test extended.
12. **type-65 fail-fast** — remote-DoH-off / forward-failure now answers SERVFAIL; oversized
    (>1232B) answers NOERROR+TC (hints still recorded); malformed → SERVFAIL. Tests incl. the §8
    gap (DoH-off type-65 answered <2s instead of timing out).
13. **ECH outer-SNI → registrable domain** — `ip_sni.go` detects the ECH extension (0xfe0d),
    walks ALL extensions (old code could early-return before seeing ECH), and records the outer
    server_name reduced to eTLD+1 via `x/net/publicsuffix` (real PSL — `co.uk` test); never the
    precise host; non-ECH unchanged; header comment fixed. go.mod unchanged.
14. **CreateTime root lineage restored** — both `AuthNetworkClient` branches mint via
    `NewByJwtWithCreateTime(session.ByJwt.CreateTime)`; `TestClientJwtPreservesRootCreateTime`
    pins both branches.

## 2. Mechanical batch + REVIEW2-UPDATE1 §5 new-issue fixes

- **api SIGTERM latch family**: `SetWarpStatusDrainingIfReady` (not-ready precedence) used by all
  three signal handlers; api main guards the ready latch on `quitEvent.IsSet()` with a
  check-set-recheck to close the race. `TestWarpStatusDrainingIfReadyPrecedence`.
- **Ghost-client token**: `DeviceConfirmAdopt` tx callback resets `adopted=false` at entry
  (tx-rerun safe).
- **Peer failed-delta/snapshot wake**: `Resync()` (flag + wake token) instead of bare flag.
- **st ckey pipeline (money path)**: per-command `redis.Nil`-vs-error classification; real errors
  raise instead of silently dropping a head-bound exclusion.
- **Sample worker**: `server.HandleError` containment (loop survives job panics) +
  `urnetwork_findproviders2_sample_drops_total`; file renamed
  `model/network_client_location_model_stats.go` (+`_test`) per request.
- **Monitor P2 batch** (incubation-quality): batteries once-per-trip via `batteryLatch` (+test);
  tailer fd close / ErrTooLong kill-before-Wait / silence+hot-restart visibility findings
  (+3 tests incl. a real 2MB-line repro); novel-ticket stable identity (+test).
- **stats**: `ExportSamples`(+Flat) propagate Close errors and remove partial output (+3 tests);
  uploader sweeps past failing segments (bounded 16 failures/sweep, cap preserved) (+2 tests).
- **sim-latency crawl leak**: submit-after-cancel closed, queued jobs drained, closer joined
  (+cancel-does-not-leak test).
- **fetch_retry trio**: Request-object method resolution, AbortSignal observed during backoff,
  discarded 502/503 bodies cancelled (+5 tests, 12/12 total).
- **types.ts additive decls**: `BlockAction.matchedIps/matchedHosts`, max-block-actions accessors,
  wasm-shape `ProxyConfigResult`; wasm_surface test widened to view_controllers2.go. Bonus real
  bug found: `js/main.go` passed a `time.Time` to `js.ValueOf` (wasm panic) → `.UnixMilli()`.
- **sdk/test.sh**: runs submodule tests (cgo incl. ABI baseline, js npm test), server-style loop.
- **Docs**: gauge names (`urnetwork_connect_drain_residents_remaining` + counters) fixed in
  SIGNALS.md/CONNECTDRAIN2.md; drain-gauge alerts rewritten to `max_over_time`; excuse
  residentId claim corrected; PEERSSTREAMS2 wording aligned to verified behavior; merge-drop
  counter documented; PROXYDRAIN1 updated (beacon gate + wg-grace wording);
  `gofmt` fix on taskworker_compat_test.go.
- **StreamReset open-loop**: failed `streamOpen` skips (V(1) log) instead of stranding later
  streams (+test).

## 3. Test gaps closed (REVIEW2-UPDATE1 §8 list)

| Gap | Test |
|---|---|
| SendNoContract accounting (money) | `TestSendNoContractMidSequenceDropDrained` + `...WithUnacked` (connect, -race) |
| CLIENT KILL conn-death for SubscribeKeyEvents | `TestSubscribeKeyEventsConnDeathResubscribes` (server root; epoch terminates in 0.27s) |
| Two-container prewarm/identity overlap | `TestProxyDeployOverlapPrewarmGate` (real two-instance) |
| api SIGTERM latch | `TestWarpStatusDrainingIfReadyPrecedence` (router-level; main-wiring guard is check-set-recheck) |
| GlobalLimit under concurrent SYN | `TestUdp/TcpBufferGlobalLimitConcurrent` (-race ×3) |
| type-65 with remote DoH off | `TestUpgradeMuxDnsHttpsFailFastServfail` |
| ECH outer-SNI | `TestSniSnifferEchRecordsRegistrableDomain` + `TestRegistrableDomain` |
| Monitor units | battery latch, tailer trio, novel-ticket, health findings (7 new tests) |
| Contract-stats reorder | producer sequence test (connect) + 2 consumer discard tests (sdk) |

## 4. Verification

- `go build ./...` + `go vet ./...` + `gofmt -l` clean on server, connect, sdk (root+cgo+js/wasm),
  proxy, warp. (warp vet: one pre-existing dead-code note in an untouched cloudwatchlogs file.)
- Server suites: model (196s incl. peers/st/keyevents/reliability), root, monitor, stats, router
  (457s), api, sim-latency, proxy (identity/handoff/overlap/prewarm re-run against final connect)
  — all `ok`.
- connect: full root `-short` suite (186s) + extender + blocker + every touched test `-race` — ok.
- sdk: root `-short` (73s) + 37-test `-race` targeted run + cgo ABI baseline + wasm build +
  `npm test` 14/14 + `tsc --noEmit` — ok.
- task/drain and warp suites unchanged this pass (green in the prior verification).

## 5. Remaining / carried items (not code)

- Ops: retire the ops-side MinIO 7-day ILM rule (code owns it now); deploy the
  `SET STATISTICS` migration before many more nightly ANALYZE passes run (it is in
  db_migrations.go; each intervening default-target ANALYZE risks re-arming the planner
  landmine); grafana ring-port rollout across all hosts promptly.
  RESOLVED 2026-07-20: the edge-2 `notify-keyspace-events` item is moot — redis no longer runs
  on edge-2; the stale `host_files/by-us-fmt-5-edge-2/redis/` config was removed (xops), and the
  only redis-cluster host (edge-6) persists the correct `Kg$sx` block.
- CI: ensure it invokes `sdk/test.sh` (now covers cgo ABI + js) or the guards are inert.
- Deferred by decision: monitor stays in incubation (15); ratified wontfixes per
  REVIEW2-UPDATE1 §2 (one-shot readiness, ReleaseTask identity, certs narrowing, deleteRules,
  vault-root fallback, member gob payload — now load-bearing for the CAS).
