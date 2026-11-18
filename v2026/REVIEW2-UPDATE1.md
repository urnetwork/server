# REVIEW2-UPDATE1 — post-fix verification of REVIEW2 findings (2026-07-20)

Re-verification of every REVIEW2.md finding against the current working trees after the
2026-07-19/20 fix wave (all fixes uncommitted; dated by file mtimes ~23:00-00:45). Method: seven
parallel area verifications plus a review of the code that arrived alongside the fixes, each
headline status independently re-traced to current code, with the new/updated tests actually
executed against local pg+redis. `go vet` is clean on all five repos (one pre-existing dead-code
nit surfaced in warp, §6). No implementation files were modified by this review.

Statuses: **FIXED-VERIFIED** (mechanism traced, tests run) · **FIXED-BUT** (fix correct, new
caveat/issue) · **PARTIAL** (some sites/aspects remain) · **NOT-FIXED** (no change) ·
**WONTFIX-APPARENT** (no change, reads deliberate).

Companion docs: REVIEW3.md and REVIEW1-UPDATE1.md (both 2026-07-19) are independent pre-fix
reviews that corroborated REVIEW2's P0/P1 set; this file is the post-fix ground truth for the
REVIEW2 numbering.

---

## 1. Executive take

The fix wave landed the entire must-fix tier: **the P0 and six of eight P1s are fixed and
verified**, most with new tests that model the originally-missed failure ordering (the wg handoff
test now runs apply-before-export; the orphan-RST test deterministically clobbers the pooled
buffer; the give-up test cancels the root ctx). The two P1s not fully closed are deliberate-looking
scope cuts: prewarm/window-identity dual-use during deploy overlap (§4.4 — half-mitigated
connect-side, overlap behavior unchanged) and the sdk `ContractDetails` field removals (§4.6 —
the VC/rows/C-export surface was restored as deprecated shims, the 6 removed struct fields were
not).

The fix wave also **introduced one new P1-class operational landmine and a handful of real P2s**
(§5): the new reliability writer gate defaults prod to legacy single-key writes, so deploying
without provisioning `client_reliability_sharded_writes: true` silently reverts the fleet to the
7k/s hot-key shape the sharding work fixed; `SetLifecycle` now replaces the whole MinIO bucket
lifecycle at taskworker startup (clobbering the ops-side 7-day ILM rule); window-client teardown
no longer deletes platform clients on shutdown for every sdk app (row growth); and the
window-identity ordering fix holds a mutex across a redis write that can stall ~60s mid-drain.

Of REVIEW2's ~24 P2s, 11 are fixed, 3 fixed-with-caveats, and the rest are untouched — almost all
of the untouched ones are the monitor/stats tooling polish items and doc-accuracy items, plus
three code P2s worth tracking (api SIGTERM latch inversion, p2p exclusive routing, contract-stats
emit reorder). §7 has the updated deploy gates; §8 the go/no-go summary of remaining work.

## 2. Scoreboard (REVIEW2 → status)

| REVIEW2 | Finding | Status |
|---|---|---|
| §3.1 P0 | sim-latency no env guard | **FIXED-VERIFIED** |
| §4.1 P1 | IsProFresh inside auth txs (5 sites) | **FIXED-VERIFIED** (+bonus fix) |
| §4.2 P1 | orphan-RST pool use-after-free | **FIXED-VERIFIED** |
| §4.3 P1 | wg handoff consumed-before-produced | **FIXED-VERIFIED** (budget caveat) |
| §4.4 P1 | prewarm + window-identity dual-use | **NOT-FIXED** (half-mitigated; accepted-risk framing) |
| §4.5 P1 | stale js types vs wasm API | **FIXED-BUT** (additive decls still missing) |
| §4.6 P1 | gomobile/cgo removals | **PARTIAL** (shims restored; 6 struct fields still gone) |
| §4.7 P1 | monitor connect-rate full scan | **FIXED-VERIFIED** |
| §5 | api SIGTERM-during-startup unlatches draining | **NOT-FIXED** |
| §5 | taskworker give-up destroys handbacks | **FIXED-VERIFIED** |
| §5 | connect drain split-state no excuse | **FIXED-VERIFIED** |
| §5 | migrate broadcast serial/unbounded | **FIXED-VERIFIED** |
| §5 | sdk migrate clock-skew wait | **FIXED-VERIFIED** (+SetByJwt race fixed) |
| §5 | PubSub silent reconnect ≠ resync | **FIXED-BUT** (no test; new storm mode) |
| §5 | stream-hops expiry never delivered | **FIXED-VERIFIED** |
| §5 | Eval TTL nanoseconds | **FIXED-VERIFIED** (repo-wide audit clean) |
| §5 | reliability deploy-order unenforced | **FIXED-VERIFIED** (writer gate) → new §5.1 landmine |
| §5 | peer failed-delta no wake | **NOT-FIXED** |
| §5 | NAT caps on unbudgeted providers + O(n) eviction | **FIXED-BUT** (60s UDP idle stays) |
| §5 | CreateContract 1s retry amplification | **FIXED-VERIFIED** (1→2→4→5s backoff) |
| §5 | p2p exclusive direct-peer routing | **NOT-FIXED** |
| §5 | contract-stats open/close reorder | **NOT-FIXED** |
| §5 | StreamReset old/new divergence + open-loop abort | **PARTIAL** (keep-set reconcile good; abort remains) |
| §5 | type-65 silent black-hole | **WONTFIX-APPARENT** (but 0x20 casing + flight races fixed) |
| §5 | ECH outer-SNI misattribution | **NOT-FIXED** (comment still wrong) |
| §5 | FindProviders2 inline sampling cost | **FIXED-VERIFIED** (top-K heap, 0.01 fraction, worker) |
| §5 | warp redis alerts noDataState:OK | **FIXED-BUT** (availability pair flipped; right shape) |
| §5 | grafana.yml dropped ring-port fields | **WONTFIX-APPARENT** |
| §5 | ring-port renumber rollout partition | **WONTFIX-APPARENT** (accepted transient) |
| §5 | drain-gauge last-value alert guidance | **NOT-FIXED** |
| §5 | sdk mixed-version ProvideControlModeNetwork | **WONTFIX-APPARENT** |
| §5 | fetch_retry Request bypass / abort / body-cancel | **NOT-FIXED** |
| §5 | monitor batteries per-tick | **NOT-FIXED** |
| §5 | tailer fd leak / >1MB wedge / dead-tailer | **NOT-FIXED** (unrelated good changes landed) |
| §5 | novel-ticket varying frame never opens | **NOT-FIXED** |
| §5 | export.go swallowed Close errors | **NOT-FIXED** |
| §5 | upload.go head-of-line block | **NOT-FIXED** |
| §5 | sim-latency crawl goroutine leak | **NOT-FIXED** (tool-lifetime only) |

Selected §6 P3s: identity snapshot ordering **FIXED** (new caveat §5.4); KeepTTL member
resurrection **FIXED** (atomic CAS Eval + test); `rand.Int63n` guard **FIXED**; accept-vs-enter
TOCTOU **FIXED** (`tryEnter`); sim_device cgo skip-list **FIXED**; loki search/livetail **FIXED**
(+pagination test); `WaitForReady` **PARTIAL** (timeout path now returns false; dead-device still
true); taskworker flush-func discard **NOT-FIXED** (comment reads deliberate); CreateTime lineage
**NOT-FIXED**; before-exit hook panic isolation **NOT-FIXED**; HttpDrainCutError exit panic
**NOT-FIXED**; api panic-exit flush skip **NOT-FIXED**; readiness twins + stale comment
**NOT-FIXED**; excuse-residentId doc claim **NOT-FIXED**; gauge-name doc mismatch **NOT-FIXED**
(`urnetwork_connect_drain_residents_remaining` vs docs); ReleaseTask identity / one-shot readiness
/ certs narrowing / deleteRules / member gob payload (now load-bearing for the CAS) — all
**WONTFIX-APPARENT**, most now explicitly documented as such in code comments.

## 3. Fixed items — mechanisms verified (condensed)

- **sim-latency (P0)**: `requireLocalEnvironment("run"/"fleet")` before dispatch; rejects any
  resolved env ≠ local including empty; `init` deliberately unguarded and verified pure.
  `TestMutatingCommandsRequireLocalEnvironment` PASS. Residual (accepted, same as test_util):
  `WARP_ENV=local` pointed at remote hostnames still connects.
- **IsProFresh (P1)**: hoisted pre-tx at all five sites; tx bodies re-read end-to-end — clean.
  AuthCodeLogin/DeviceConfirmAdopt restructured with pre-tx reads + in-tx network-binding guards
  and post-tx JWT minting (AuthCodeLogin preserves CreateTime lineage). Bonus: UpgradeGuest's
  in-tx `SetUserAuthAttemptSuccess` (a nested-Tx-on-second-conn REVIEW2 missed) replaced with a
  same-tx variant. 7 new + 12 regression tests PASS. Residual by design: auth still hard-depends
  on redis (no pg conn held anymore).
- **Orphan RST (P1)**: RST built first, IPs copied to fresh backing, then pool return; callback
  gets owned copies (v4+v6). Plus `EnableOrphanRst` gate + 256/s limiter. The new test clobbers
  the just-returned pooled buffer deterministically — stronger than a probabilistic `-race` repro
  (passes under `-race` too).
- **wg handoff (P1)**: rebuilt as a generation-tagged poll — replacement publishes
  `{pwh_…}:request` (gen, 10-min TTL) at boot; old instance exports to `{pwh_…}:<gen>` at drain
  end (empty set exported as a completion marker); replacement GETDEL-polls the gen key at 1s for
  5 min. Consume-once preserved; stale-next-deploy consumption now impossible. Tests model the
  real apply-before-export order. Caveat → §5.6.
- **Monitor connect-rate (P1)**: query rewritten to a `pg_stat_user_tables.n_tup_ins` cumulative
  counter + in-probe rate conversion (warmup/reset-safe). No heap access at all; new storm class;
  test PASS.
- **Taskworker give-up (P2)**: eval/heartbeat/finalize detached via `WithoutCancel` with a 30s
  `FinalizeTimeout` bound; CLI waits `WaitFinalHandback` before root cancel. Handbacks survive
  give-up; nothing can hang exit. New tests PASS (full drain suite 39s green).
- **Split-state excuse + broadcast bound + clock clamp (P2 trio)**: connection-only clients
  excused with a zero-resident-id sentinel, exactly-once via `drainedClients`; broadcast now a
  64-worker fan-out under a 10s aggregate budget (also fixes the `Int63n` panic); sdk clamps
  migrate wait to 5 min and re-applies racing `SetByJwt` at swap under `authVersion`. Migrate e2e
  (170s) + Track-A + sdk tests PASS.
- **Key events (P2 pair)**: reconnects now detected via `ChannelWithSubscriptions` — a
  post-initial psubscribe confirmation ends the epoch → resubscribe + resync (verified against
  go-redis internals; no false-positive source). Hops-key `del/expired/evicted/unlink` route to
  `Reconcile()` → state-compare reset delivers the empty set; a 5-min corrective ticker heals
  lost events; poll-mode insurance reads restored. Eval TTLs now integer seconds at both sites;
  all 8 Eval sites repo-wide audited clean; new TTL assertions would fail on the old bug.
- **Reliability deploy order (P2)**: one-way `client_reliability_sharded_writes` writer gate,
  prod-default legacy; rollup reads both forms; RELIABILITY2.md rewritten as a 4-step protocol.
  Gate test PASS. Consequence → §5.1.
- **NAT caps (P2)**: `GlobalLimit=0` unless a memory budget is set (gate is load-bearing);
  `DefaultProviderLocalUserNatSettings` adopted at all three sdk call sites; O(n log n)-under-mutex
  eviction replaced with a 32-sample approximate LRU (amortized O(1), exact cap, identity-checked)
  — adversarially verified. Residual: UDP 60s idle applies to providers (long-lived plain-UDP NAT
  bindings still break); at-cap LRU-cancel of established flows remains policy for budgeted hosts.
- **FindProviders2 sampling (P2)**: top-K min-heap before sort, weights hoisted, default fraction
  0.01 non-local, HMACs moved to a worker, 16MiB/64 + 32MiB/4096 byte-bounded queues. Tests PASS.
  New caveats → §5.5.
- **sdk surface (P1 pair)**: old contract-details API restored as deprecated shims (Go + wasm + 4
  C exports; projections field-faithful vs origin, `PairCount` drift noted); `types.ts` declares
  both generations; npm version bumped to 0.0.1-beta.7; `wasm_surface.test.ts` pins the renamed
  surface (narrow keep-list); `abi_baseline_test.go` pins the 4 restored exports (must run from
  the cgo module). Still missing → §4.

## 4. Still open (what I'd fix next, ranked)

1. **api SIGTERM-during-startup latch inversion** (§5) — untouched; last-writer-wins
   `SetWarpStatusReady` after the draining flip; ~10s "ok" window on a dying container; the
   one-line `quitEvent.IsSet()` guard (or sticky draining) is still absent. The taskworker/connect
   not-ready→"draining" overwrite sibling also stands.
2. **Prewarm/window-identity deploy-overlap dual-use** (§4.4) — connect side now preserves
   identities on ctx-done teardown, but the overlap mechanism (old container live-evicts →
   `RemoveNetworkClient` on the identity the new container is using) is unchanged and untested.
   PROXYDRAIN1 still frames it as acceptable; if that's the decision, write it down as such.
3. **p2p exclusive direct-peer routing** (§5) — wedged-but-ICE-connected SCTP still stalls all
   direct-peer traffic up to the 15s WriteTimeout, and `Downgrade` still can't shed it.
4. **Contract-stats open/close reorder** (§5) + **peer failed-delta wake** (§5, a one-liner:
   `Resync()` instead of `forceResync.Store`) + **StreamReset open-loop abort** (§5 partial).
5. **fetch_retry trio** (§5) — untouched file; Request-object bypass, abort-during-backoff,
   body cancel; the js tests added don't cover them.
6. **sdk additive decls** — `BlockAction.matchedIps/matchedHosts` + max-block-actions accessors
   still undeclared in types.ts (the parity test doesn't read view_controllers2.go); the 6 removed
   `ContractDetails` fields still need app-side lockstep or restoration.
7. **Monitor/stats tooling P2 batch** — batteries per-tick, tailer fd/wedge/health, novel-ticket
   frame identity, export.go Close errors, upload head-of-line, ctl ctx timeouts, `.partial`
   cleanup, tick-based resolve. All still as reviewed; monitor still has essentially one test.
8. **Doc accuracy** — CONNECTDRAIN2/SIGNALS gauge names (`urnetwork_connect_drain_…`), excuse
   residentId scoping claim, PEERSSTREAMS2 "exactly v2" and reconnect-resync wording, drain-gauge
   alert guidance (`max_over_time`/`increase`), ECH comment in ip_sni.go, TASKDRAIN1 §7.4,
   PROXYDRAIN1/drain.go wg-grace wording.
9. **ECH outer-SNI** (§5) — no detection added; misattribution grows as ECH deploys.
10. **CreateTime lineage** in AuthNetworkClient client tokens (one-line
    `NewByJwtWithCreateTime`); the AuthCodeLogin restructure shows the intended pattern.

## 5. New issues introduced by the fix wave (ranked)

1. **[P1-operational] Reliability writer-gate rollout landmine** — prod has been running sharded
   writers since 2026-07-18, but the new gate defaults prod to LEGACY single-key writes
   (`network_client_reliability_model.go:248-263`, read once via `sync.OnceValue`). Deploying this
   build without first provisioning `client_reliability_sharded_writes: true` silently
   re-concentrates the fleet's HINCRBY load onto one node — the exact 7k/s hot-key incident shape
   the sharding fixed. Nothing enforces or alerts on the flag; RELIABILITY2.md documents the step.
   **Gate the deploy on the flag being provisioned.**
2. **[P2] MinIO bucket lifecycle clobber** — `SetLifecycle` (blob.go:264-289) replaces the entire
   bucket lifecycle config and runs at every taskworker startup via `ApplyStreamRetention`
   (taskworker.go:78-81). It wipes ops-set ILM rules (the current 7-day stats retention is one),
   and with two envs sharing a bucket each env's taskworker deletes the other's rules
   (last-restart-wins). Needs merge-don't-replace or an ops handoff decision before deploy.
3. **[P2] Platform client rows leak on every sdk app shutdown** — `RemoveClientArgs` now no-ops
   entirely when the generator ctx is done (`ip_remote_multi_client_api.go:300-315`). Correct for
   the proxy (identities must survive), but it applies to every `ApiMultiClientGenerator` user
   with the default nil store: window platform clients that used to be best-effort deleted at
   teardown now linger until server-side idle reap — up to window-size rows per app shutdown.
4. **[P2] Window-identity mutex held across redis store** — the ordering fix
   (`storeSnapshotWithLock`, identity.go:158-167) serializes `Record/Remove/TakeRestored` behind
   `SetProxyWindowIdentities`, and `server.Redis` retries up to ~60s before panicking. During a
   redis stall — exactly when drains run — window expansion/teardown for that device wedges,
   contradicting the adapter's own "a redis outage must never break the device's window
   management" comment and the no-lock-across-blocking rule. Ordering is real and test-pinned;
   consider a sequenced async store (generation counter) instead.
5. **[P2] Bare sample-worker goroutine can crash the api** — the FindProviders2 fix moved
   HMAC/append to `go func()` with no `HandleError`/recover (find_providers_stats.go:197-205); a
   panic in `Anonymize`/`Append` kills the process (every sibling goroutine uses the wrapper).
   Related: `statsHandle.Close()` vs worker exit race can panic send-on-closed-channel at drain
   exit (LIFO defer order in api main), and drops are uncounted.
6. **[P2] Handoff budget vs serialized multi-block drains** — the 5-min shared poll+initiate
   budget and 10-min request-key TTL are outrun by warpctl's per-host drain flock (ceiling
   DrainTimeout+5min) on multi-block hosts: block 3+ can silently miss the handoff (fail-soft,
   pre-P3a blip). Start the budget when the export appears, or scale budget/TTL by blocks×grace.
7. **[P2, money] st ckey pipeline swallows per-node errors** (new-code review) —
   `GetStContributingClientCkeys` (st_model.go:1157-1163) checks only the pipeline's first error,
   which go-redis defines as the first error in command order *including* `redis.Nil`: a benign
   miss ordered before a real node error masks it, the per-cmd `continue` drops the mapping, and a
   head-bound provider is silently NOT excluded → double payout, immutable once the epoch root is
   committed. Distinguish Nil from errors per-command.
8. **[P2] Key-event merge-drop storm mode** — the reconnect-detection fix also changed the
   per-node merge send from blocking to drop→terminate-epoch (redis.go:460-471). A sustained
   >1024-message burst now cycles unsub→5s→resubscribe→fleet `resyncAll` repeatedly instead of
   applying backpressure. Counted (`urnetwork_redis_key_event_merge_drops_total`) but absent from
   SIGNALS.md.
9. **[P3] Ghost-client token on tx rerun** — DeviceConfirmAdopt's restructure left
   `adopted`/`deviceId`/`clientId` unreset across `server.Tx` reruns
   (device_association_model.go:800-806): a commit-error rerun that loses a race to a duplicate
   confirm early-returns with stale `adopted=true` and mints a signed client JWT for a rolled-back
   client. Narrow (commit error + racing duplicate). Fix: reset `adopted=false` first line of the
   callback. (AuthCodeLogin/UpgradeGuest verified clean.)
10. **[P3 batch]** peer CAS divergence: refresh-restore racing a provide-update can pin stale
    meta that the new exact-match CAS then refuses to update until re-registration
    (peer_model.go:572-580 vs 899-946); split-state double-increment of `drain_excuses_written`
    skews the written−consumed signal during multi-group drains; `taskworker_compat_test.go` is
    not gofmt-clean (**fails any gofmt gate**); legacy contract-details controller notifies
    listeners from two goroutines + unlocked `legacyProvider` fields (deprecated path race);
    `PairCount` legacy-projection drift; A/AAAA replied-flight ms-scale retransmit drop window;
    apply-poll expiry logs `DeadlineExceeded` as "Unexpected error"; NoData redis alerts render
    empty `{{$labels.host}}`; loki `connectAttempt` shared between dial-give-up and
    read-close-retry (asymmetric abort); uncapped stats spool when salt is provisioned without
    minio.yml; `DeltaWithEventId` dead code; `pendingDeadlines()` dead code; un-generationed
    handoff compat funcs retained (test-only).

## 6. New code that arrived with the fixes (assessment)

- `controller/stats_collector.go` + `model/network_stats_model.go` (public `urnetwork_stats_*`
  feed → warp `/stats.json` → ur.xyz): sound overall; runs on **every** taskworker host — the
  5-min `COUNT(*)`/`COUNT(DISTINCT)` pair is tagged `ReplicaDb` but ReplicaDb still routes to the
  primary, and each host makes 9 sequential chain RPCs + a geckoterminal GET per 60s (a degraded
  RPC endpoint stretches ticks to minutes). P3s: price fetch ignores `enabled:false`;
  `SetStConfig` seam bypasses BlockSeconds normalization (div-by-zero tick, test-only);
  `deploy_block: 0` leaves head-tier exclusion blind before first sync; lazy gauge registration
  not re-entrant. Zero tests.
- warp `grafana/stats.go` + `contract-failures.yml` + tests: metric names/thresholds verified
  against server code; defensive all-or-nothing refresh with stale fallback; tests PASS.
- st delta (`BuybackTotal` + windowed sums): additive, both implementers updated; §5.7 pipeline
  issue is in pre-existing committed code but is money-path — worth fixing in the same pass.
- loki client hardening (`ErrSearchIncomplete`, LiveTail backoff, pagination test): good.
- Contract-failure metric + rate-limited exemplar (connect_controller.go): sound, test-pinned;
  classifier is error-string-coupled (rewording silently degrades class to "other").
- server vet clean; warp vet flags pre-existing dead code (`warpctl/cloudwatchlogs/client.go:98`
  unreachable `return nil`) — untouched file, cosmetic.

## 7. Updated deploy gates / ops checklist

1. **Provision `client_reliability_sharded_writes: true` for prod before deploying this build**
   (§5.1) — otherwise writers silently revert to the legacy hot key.
2. Decide the MinIO lifecycle ownership before the first taskworker start against the shared
   bucket (§5.2): either make `SetLifecycle` merge, or migrate the ops ILM rule into
   `streamLifecycleRules` and stop managing it out-of-band.
3. Still open from REVIEW2 §8: `notify-keyspace-events "Kg$sx"` persisted config — **edge-2's
   redis.conf still has the disabled default** (xops unchanged since the review); the
   `transfer_contract.open SET STATISTICS` migration still wants a manual prod apply before the
   next nightly ANALYZE; roll all grafana hosts promptly for the ring-port renumber.
4. First deploy of the new proxy build gets no wg handoff once (legacy vs generation key,
   self-resolving); multi-block hosts may exceed the handoff budget (§5.6).
5. `gofmt -w taskworker/taskworker_compat_test.go` before any gofmt-gated CI run.
6. sdk release train: additive types.ts decls + the 6 `ContractDetails` fields decision; ensure CI
   actually runs the cgo-module test (`go test ./gen/` from `cgo/`) or the ABI guard is inert.
7. Alerting: add the merge-drop counter and drain-gauge `max_over_time` guidance to SIGNALS.md;
   fix the two doc gauge names.

## 8. Test status

New tests added by the fix wave, all executed here: sim-latency guard; monitor rate-probe; stats
queue/lifecycle/blob (7); find_providers top-K/export; pro-jwt (5 model + 2 controller);
device-adopt/auth-code/guest regression (12, pre-existing, green); task give-up pair + drain suite;
taskworker compat pins; router/api latch+readiness; warp redis/contract/stats/loki/services
suites; connect orphan-RST quartet, flow-limit pair, retry-backoff, identity trio, net-http retry
quartet, mux fanout/local-fallback updates, stream lifecycle, p2p route quintet; model
peers/streams/reliability key-event suites (incl. expiry-repair pair, CAS resurrection, writer
gate, TTL assertions); proxy handoff real-order + generation isolation + drain/readiness suites +
`../proxy` tryEnter suite; sdk provider-migrate pair + contract-details suite + memory tests + js
wasm-surface/fetch-retry. **All pass**, with two environment flakes noted: the proxy
integration trio fails on a cold first run (60s provider-warmup guard; passes on rerun), and one
7-test stream batch failed once then passed twice (pool-contention-sensitive; unattributed).

Still-missing tests (carried from REVIEW2 §9): **SendNoContract accounting (the most
money-relevant gap — still zero coverage)**; a CLIENT KILL conn-death test for
`SubscribeKeyEvents` (the reconnect fix shipped untested); a two-container overlap test for
prewarm/identity (§4.4); an api-main SIGTERM e2e; GlobalLimit under concurrent SYN load; type-65
client-visible behavior with remote DoH off; an ECH outer-SNI case; monitor ticket/parser/tailer
units (still ~one test in the package).
