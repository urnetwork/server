**WGONDEMAND review — findings addressed**

**Fix follow-up, 2026-10-30.** All five original findings below have been addressed following `connect/CODESTYLE.md`.

- Device miss results now carry resolution identity. Removal, configuration resets and refusal clearing invalidate previous answers, and completion checks ownership atomically with installation. Update-only IPC removal also invalidates an unpublished peer.
- Complete peer initialization and removal share the configuration lock. On-demand peers are published only after configuration/start, so another creator or packet lookup cannot obtain a partial peer.
- Proxy reconciliation compares preshared keys, removes obsolete generations before publishing or endpoint-seeding replacements, and maintains the eager public-key index. Unchanged peers retain their sessions.
- The parallel review identified two adjacent WireGuard races, also fixed: `InitiateHandshake` now holds the control lock while resolving and creating its peer; eviction preserves attachments used since its activity snapshot. Delayed attachment calls reconcile the current address/channel, including a hosted tun moved to another address. External tun calls hold no proxy state lock.

Permanent regressions are in [device/peer_miss_lifecycle_test.go](/Users/brien/urnetwork/userwireguard/device/peer_miss_lifecycle_test.go) and [wg_reconcile_regression_test.go](/Users/brien/urnetwork/proxy/wg_reconcile_regression_test.go). They use handler and logger barriers, explicit operation boundaries, and gated tun setters; timeouts only bound hangs. The lifecycle tests observe a competing operation actually blocked on its mutex before releasing construction.

Each relevant fix was selectively reverted with a temporary Go overlay while retaining unrelated code. Those runs failed on the intended assertions: revoked authorization, overwritten pending resolution, restarted/unconfigured peers, stale PSKs, lost seeded peers, stale eager status, uncoordinated handshake creation, and stale attachment writes. Overlays are retained in `/private/tmp/wgondemand-fix-regressions-236417808`.

**Final fix validation:**

| Check | Result |
| --- | --- |
| userwireguard: `go test -race -count=1 -timeout=180s ./device` | Passed, 26.993 s |
| proxy: `go test -race -count=1 -timeout=180s -run '^TestWg' .` | Passed, 124.048 s; opt-in soak skipped |
| proxy removal/reconciliation checks after the final IPC-removal fix | Passed under `-race`, 13.234 s |
| server on-demand configuration and peer-metrics checks, with the local test environment | Passed under `-race`, 2.887 s |
| Selective regression overlays, including update-only IPC removal and moved-address attachment | Failed on the intended assertions |
| Formatting and `git diff --check` | Passed |

The original WireGuard validation did not rerun the long soak, mixed eager/on-demand deployment handoff or production memory/CPU measurements. The four separate server findings from the parallel commit review are addressed in the server follow-up below, including database-backed regression coverage.

The updated [design](/Users/brien/urnetwork/server/proxy/WGONDEMAND.md) describes the synchronization and removes the incorrect orphan-lifetime claim. The original review below is preserved as the pre-fix record; its line references and validation results refer to the revisions listed there.

**Original review — request changes**

Five findings: two P1 issues involving revocation and peer lifetime, and three P2 configuration/status regressions. The existing tests pass, but deterministic review tests reproduce the failures below. The P1 issues should be fixed before enabling this version in production.

The changes were merged during this review. These are the revisions checked:

| Repository | Current revision | On-demand implementation |
| --- | --- | --- |
| proxy | `main` at `9db4197cb0d84c5242fccedd44d74d0adcfcfa3e` | `05b4ade` |
| userwireguard | `master` at `157e84138ebc126d61e4089feeea894854d2ccdb` | `4524829` |
| server | `main` at `1bcdf6350` | `d97ceb783`; design added by `bbe8d977c` |

Reviewed [server/proxy/WGONDEMAND.md](/Users/brien/urnetwork/server/proxy/WGONDEMAND.md), the device miss handler and peer lifecycle, the proxy client index/eviction/handoff paths, and server enablement and metrics. The merged WireGuard files in proxy and userwireguard are identical to the tested feature revisions. The additional server changes concern HTTP CONNECT and are outside this review.

1. **[P1] A stale miss result can restore a revoked key and route its traffic into the replacement client's tunnel.**

   Locations: [userwireguard/device/peer_miss.go:311–314](/Users/brien/urnetwork/userwireguard/device/peer_miss.go:311), [proxy/wg.go:745–752](/Users/brien/urnetwork/proxy/wg.go:745), and [proxy/wg.go:1040–1046](/Users/brien/urnetwork/proxy/wg.go:1040).

   `peerMiss` snapshots the key's configuration and releases `stateLock`. The asynchronous completion subsequently installs that configuration without checking that it is still authorized. Removing a client or replacing its key only deletes the current index entry and current device peer; it does not invalidate an already accepted result.

   Deterministic reproduction: pause the handler immediately after the real directory lookup accepts key A at address X; register key B at X with a different tun and an endpoint; then release A's saved result. A's original, valid WireGuard initiation completes, its stale allowed-IP insertion takes ownership of X, and an encrypted packet from A is delivered to B's tun. The review test observed that packet, not just an extra peer in the map. Proxy ingress selects the tun by source address in `Write`/`activateClient`, so removing A from the public-key index does not prevent this traffic.

   This contradicts the design's [claim that an orphan carries no traffic and lasts at most about 11 minutes](/Users/brien/urnetwork/server/proxy/WGONDEMAND.md:118). Even without address reuse, subsequent valid handshakes on the resurrected peer bypass the miss handler and refresh its activity, so eviction does not provide the claimed maximum lifetime.

   **Required change:** make authorization and installation agree on a client/configuration generation, and invalidate pending results when that generation is removed or replaced. The final generation check and installation must be atomic with invalidation; another lookup before installation leaves the same race. Add a channel-controlled regression covering key replacement/address reuse and late completion after removal. Review reproduction: `TestReviewLateMissMustNotActivateRevokedClient`.

2. **[P1] Removal during creation can restart a peer outside the peer map and permanently leak its workers.**

   Locations: [userwireguard/device/peer_miss.go:371–396](/Users/brien/urnetwork/userwireguard/device/peer_miss.go:371), [userwireguard/device/peer.go:114–116](/Users/brien/urnetwork/userwireguard/device/peer.go:114), and [proxy/wg.go:1043–1045](/Users/brien/urnetwork/proxy/wg.go:1043).

   `NewPeer` publishes the peer before `AddPeerOnDemand` finishes configuration and calls `handlePeerPostConfig`. The latter holds `ipcMutex.RLock`, but `RemovePeer` does not participate in that lock. Consequently, a client removal can delete the published peer while creation is still in progress. `Stop` sees a peer that has not started and returns; creation then calls `Start` on the deleted object.

   A temporary synchronization hook immediately before the existing post-config call reproduced this exact ordering. After both operations returned, `PeerCount()` was zero while the removed peer's `isRunning` was true. Its two goroutines retain the peer and device, and its encryption-queue reference remains outstanding. Neither subsequent eviction nor device close can find the peer in the map to stop it; the outstanding reference also prevents the encryption queue from closing.

   Early publication has another observable consequence: with the first creator blocked on allowed-IP insertion, a second `AddPeerOnDemand` returns success with the still-unconfigured, stopped peer. The existing concurrency test waits for every creator before checking readiness, so it misses this.

   **Required change:** serialize the complete install/start operation with removal and make concurrent callers wait for initialization to finish. Removal must either stop a fully installed peer or prevent its later start. Preserve a deterministic creation-versus-removal regression and a per-call readiness assertion. Review reproductions: `TestReviewRemovedCreatingPeerMustNotRestart` and `TestReviewConcurrentCreateMustReturnConfiguredPeer`.

3. **[P2] Updating a preshared key leaves an active on-demand peer using the old key.**

   Locations: [proxy/wg.go:650–665](/Users/brien/urnetwork/proxy/wg.go:650), [proxy/wg.go:425–427](/Users/brien/urnetwork/proxy/wg.go:425), and [userwireguard/device/peer_miss.go:361–362](/Users/brien/urnetwork/userwireguard/device/peer_miss.go:361).

   `indexClients` invalidates peers when the public key or address changes, but does not compare preshared keys. For an unchanged public key/address, both `AddClients` and `SetClients` update the index and report success while retaining the live peer's old PSK. Endpoint seeding does not repair it: `AddPeerOnDemand` returns an existing peer unchanged.

   Reproduction: install an endpoint-seeded client with PSK A, update only its PSK to B, and inspect `IpcGet`. Both update APIs still report A on the live peer in on-demand mode. The same test passes in eager mode. A client switching to B cannot complete its next handshake until the stale peer is removed; a holder of A can continue using it and keep refreshing the peer.

   **Required change:** include PSK changes in configuration reconciliation and apply or replace the live peer before reporting success. Test both update APIs with the same address and public key, including setting and clearing a PSK. Review reproduction: `TestReviewPresharedKeyUpdateMustReachLivePeer`.

4. **[P2] `SetClients` deletes an endpoint-seeded peer after moving its key to a new address.**

   Location: [proxy/wg.go:420–432](/Users/brien/urnetwork/proxy/wg.go:420).

   The on-demand branch clears the old indexes, calls `addClients` (which eagerly seeds clients with endpoints), and only then removes keys whose address changed relative to `previousKeys`. For the same public key moving from X to Y, seeding returns the old peer or creates a new one, and the subsequent removal unconditionally deletes it. Since the old index was cleared before `indexClients`, that helper cannot perform its normal moved-key reconciliation.

   Reproduction: add a client at X with an endpoint, then call `SetClients` with that key and endpoint at Y. The call succeeds with one configured client and zero device peers. This breaks the promised server-first path. A later `InitiateHandshake` can create a peer from `wgClientKey`, but that structure does not retain the endpoint it just lost.

   **Required change:** reconcile/remove obsolete peer configurations before seeding their replacements. Add a test that moves an endpoint-bearing key through `SetClients` and verifies the new route, retained endpoint, and ability to initiate. Review reproduction: `TestReviewSetClientsMustKeepMovedSeededPeer`.

5. **[P2] Eager-mode status reporting retains replaced keys and exports removed clients.**

   Locations: [proxy/wg.go:557–562](/Users/brien/urnetwork/proxy/wg.go:557) and [proxy/wg.go:834–847](/Users/brien/urnetwork/proxy/wg.go:834).

   The eager registration path adds entries to `clientKeys` without removing the previous key for an address. The rewritten `PeerStatuses` trusts this new index, whereas the previous implementation derived membership from the current `clients` map. If A is replaced by B at the same address, both keys remain eligible for export; their snapshots can overwrite each other in map iteration order.

   A deterministic public-API reproduction adds A at X, replaces it with B using `AddClients`, then removes X using `RemoveClients`. `ClientCount()` is zero, but `PeerStatuses()` still reports A at X. Retaining the old device peer predates this change; treating its status as belonging to a currently configured client is the new regression. Session sampling and endpoint handoff can therefore consume stale identity/session information even when the on-demand kill switch is off.

   **Required change:** maintain the public-key index consistently in eager mode, including replacement and removal, or verify each snapshot's key against the current client at that address. Add the replacement/removal regression and a check that two surviving peer snapshots cannot select a stale key. Review reproduction: `TestReviewEagerStatusesMustForgetReplacedKey`.

**Validation performed.** These checks ran on the identified feature revisions before the merge; their relevant WireGuard source was compared with the merged revisions afterward.

| Check | Result |
| --- | --- |
| userwireguard: `go test -race -count=1 -timeout=180s ./device` | Passed, 26.771 s |
| proxy: `go test -race -count=1 -timeout=180s -run '^TestWg' .` | Passed, 77.791 s; opt-in soak skipped |
| server: `go test -race -count=1 -timeout=90s -run '^(TestWgPeersOnDemand.*\|TestUpdateWgPeerMetricsPublishesPeersMissesAndEvictions)$' ./proxy`, with local test environment variables | Passed, 2.520 s |
| Temporary device review tests, `-race -run '^TestReview'` | Both failed on the reported lifetime/readiness assertions |
| Temporary proxy review tests, `-race -run '^TestReview'` | All four top-level cases reproduced the findings; eager PSK controls passed |

The review tests are retained in [the proxy reproduction file](/private/tmp/wgondemand-review-c_n__uic/proxy/wg_review_regression_test.go) and [the device reproduction file](/private/tmp/wgondemand-review-c_n__uic/userwireguard/device/peer_review_regression_test.go). Run them from their respective directories with `go test -race -count=1 -timeout=60s -run '^TestReview' -v .` (use `./device` from the userwireguard module root). The temporary device source has one scheduling hook before `handlePeerPostConfig`; the removal test uses that hook to force the interleaving. The readiness test uses the existing allowed-IP mutex. The stale-authorization test wraps the real proxy handler with channels after its lookup. None depends on sleeps or probabilistic scheduling to trigger the failure.

No implementation changes or commits were made. Database-backed server integration tests, the long soak, and mixed eager-to-on-demand deployment handoff were not rerun. The design's production memory/CPU estimates were not independently remeasured. Each fix should include a permanent deterministic regression that fails without the fix and passes with it.


**Server fix follow-up, 2026-10-30.** The four separate findings below have been addressed following `connect/CODESTYLE.md`, with deterministic regressions.

- **Presence:** count distinct reliability minutes, so multiple addresses in one minute cannot inflate connection coverage.
- **Evidence:** network estimates retain their established cadence. Exact fleet scores use signed v2 cut references, complete record/proof replay and the historical eligible binding census. The producer sends each operator only its own private census. Bootstrap requires an explicitly signed empty prior; migrated or approximate prior state cannot seed an exact replica.
- **Recovery:** fold the oldest pending network interval with its own evidence and retained prefix pins. Fleet EMA advances separately through authenticated measurement predecessors in bounded batches. Missing or expired evidence never becomes an empty fold. Late summaries and recovered object reads can finish the replica without changing network EMA; retained v1 history cannot obstruct a valid fresh bootstrap.
- **CONNECT:** apply the request deadline to caller-lock and plan admission, real Redis socket reads, shared device-construction waits, route readiness and dialing. Construction remains owned and joined by the manager, so one canceled caller does not stop healthy siblings. Preserve the synchronous legacy open path and failure cooldown. Ordinary HTTP and SOCKS also receive bounded setup contexts.

`replica_score` is now nullable in the API and TypeScript bindings; the Go SDK exposes `HasReplicaScore`. A verified zero remains distinguishable from unavailable evidence. Published validator scores remain available when exact replay is unavailable.

**Regression evidence.** The permanent presence test failed before the fix with 1 instead of 0.5. Delayed delivery moved the original interval by 20 minutes. The missed-tempo database test skipped directly to index 984 instead of processing 982, 983 and 984 with contributions 2, 0 and 4. Both composed CONNECT creator/waiter tests fail against server `a5f7685fa`; disabling only deadline-aware Redis also fails the real held-socket test. Those overlays are in `/private/tmp/connect-budget-regression.JWwfl6`. Removing only the producer's prior-state check fails `TestHeadSummaryEvidenceDoesNotInventEmptyMigratedPrior` using `/tmp/sn-head-empty-prior.QYD608/overlay.json`.

The real-cut regression replays two complete signed trails. Its sequence cursor excludes the first trail, a historical generation change excludes one hop from the second, and the result is six credited prefixes plus a live zero identity. Moving receipt by five days preserves that result. Missing/corrupt proofs, unknown signing keys, incomplete global evidence and missing predecessors produce no exact score.

**Validation:**

| Check | Result |
| --- | --- |
| CONNECT/admission, shared construction, cancellation, cooldown and legacy ownership regressions | Passed under `-race` |
| Existing HTTP refusal, SNI, route-readiness and plan-cache integration tests | Passed under `-race` with local fixtures |
| SN Top presence, chronology, historical pins, signed replay, late recovery and nullable storage regressions | Passed under `-race` with isolated PostgreSQL/Redis fixtures |
| SN protocol and producer tests, including unchanged v1 golden signatures and migrated-prior rejection | Passed under `-race` |
| API leaderboard and signed-summary HTTP routes | Passed under `-race` with local fixtures |
| SDK SN Top decoding/round trips, TypeScript contract tests, OpenAPI spec tests and generated-client check | Passed |
| Selective pre-fix regressions | Failed on the intended assertions |
| Formatting and whitespace checks | Passed |

**Rollout and limits.** Apply the additive server migration before deploying the server, then deploy the v2-capable server before the validator producer. V1 remains readable. Exact replay requires complete global evidence and authenticated prior history; missing history, migrated prior state or a partial multi-operator census leaves the replica unavailable. The full repository suites, production rollout, long soak and production resource measurements were not run.

**Parallel 24-hour review — separate server findings**

The parallel review covered October 29, 2026 at 05:11:17 UTC through October 30 at 05:11:17 UTC: 134 server, 24 connect, 4 proxy and 19 SDK commit entries, including merge changes. The four remaining findings were in server; no additional connect or SDK defect was identified in that review.

1. **[P2] Count distinct reliability minutes when computing presence.** Server `ce5def2bf`, `GetSnTopPresence` in [model/sn_top_model.go](/Users/brien/urnetwork/server/model/sn_top_model.go). The table key includes an address hash, so `COUNT(*)` counts multiple rows for one client's minute. Two addresses during 36 of a tempo's 72 minutes produced presence 1 instead of 0.5, inflating projected credit and shared-prefix eligibility. The permanent database reproduction is [sn_top_presence_db_test.go](/Users/brien/urnetwork/server/controller/sn_top_presence_db_test.go).

2. **[P2] Keep evidence selection independent of summary delivery.** Server `ce5def2bf`, `snTopFoldTempo` in [sn_top_controller.go](/Users/brien/urnetwork/server/controller/sn_top_controller.go). It derived the scoring interval from `ReceivedAt`. The same signed native epoch delivered 20 minutes later changed the interval from [10:48,12:00) to [11:08,12:20), selecting different trail and reliability records. Receipt time does not authenticate the validator's evidence cut. Exact fleet replay also requires the cut's historical eligible binding census; today's membership cannot supply it.

3. **[P2] Replay missed scoring periods before advancing EMA state.** Server `ce5def2bf`, `ComputeSnTopSnapshot` and [sn_top_projection.go](/Users/brien/urnetwork/server/model/sn_top_projection.go). A task outage skipped to the newest tempo and applied one fold, then marked the state current. Starting at 1 in tempo 10, three actual zero-contribution folds through tempo 13 require 0.421875; one fold produced 0.75. Recovery must also retain nonzero contributions in missed periods. Validator epochs in which no measurement was committed are distinct from measurements the server missed.

4. **[P2] Apply the CONNECT deadline to admission and device setup.** Server `df27527859` with proxy `592f644e`, `httpServer.newHttpProxy` in [server/proxy/server.go](/Users/brien/urnetwork/server/proxy/server.go) and `ProxyDeviceManager.OpenProxyDevice` in [proxy_device.go](/Users/brien/urnetwork/server/proxy/proxy_device.go).

   Bounded CONNECT shipped: the library creates a deadline, with a 30-second default, before calling the dial callback synchronously. The server callback used manager/listener contexts for the caller-lock read and feature lookup, then opened a device without the request context. The constructor and callers waiting for an existing construction could therefore outlive the CONNECT budget. Only the later route wait and network dial honored that deadline; the handler could not send 504 while stuck in setup.

   The library's `TestConnectBudgetBoundsBlockedDial` passes because its callback explicitly waits on `ctx.Done()`. The composed server reproduction holds the real callback's device builder on a channel, uses a one-second budget and advances two seconds under `testing/synctest`: before the fix, no response arrives until the builder is released. This was reconfirmed against server `a5f7685fa` and proxy `584eac3`. Permanent creator and coalesced-waiter cases are in [http_connect_budget_test.go](/Users/brien/urnetwork/server/proxy/http_connect_budget_test.go); admission and shared-owner isolation have adjacent regression files.
