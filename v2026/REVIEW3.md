# REVIEW3 — cross-repository delta review

Original review: 2026-07-19

Remediation validated: 2026-07-20

Scope: the effective local delta against each repository's recorded upstream ref,
including committed, modified, and untracked source in `server`, `connect`, `sdk`,
`proxy`, `warp`, and the Apple, Android, and Windows clients.

The original review phase did not modify implementation code. The follow-up
remediation phase did: every P0, P1, and P2 finding was addressed except finding
23, which was explicitly waived because the prior `BlockAction` semantics were
never shipped. The original findings are retained below as the pre-fix record;
this remediation section and the post-remediation disposition supersede their
release status.

## Remediation summary

| Severity | Disposition |
| --- | --- |
| P0 | 1 fixed, 0 open |
| P1 | 10 fixed, 0 open |
| P2 | 14 fixed, 1 explicitly waived (#23) |
| Total requested scope | 25 fixed, 1 waived, 0 open |

No new P0, P1, or P2 defect was confirmed during the post-fix re-audit. P3
hardening items and unproven integration risks were not part of the requested
fix scope.

Finding 23 remains intentionally unchanged. That is a coherent compatibility
decision given the confirmation that the old `BlockAction` contract was never
shipped: there is no deployed consumer behavior to preserve, and adding a
legacy compatibility representation would create complexity without protecting
an actual client.

## Finding-by-finding resolution

| # | Status | Resolution |
| --- | --- | --- |
| 1 | Fixed | `sim-latency` now rejects `run` and `fleet` unless the resolved `WARP_ENV` is exactly `local`; `init` remains available because it only writes the requested local configuration file. Guard tests cover non-local refusal. |
| 2 | Fixed | Fresh entitlement/database/cache work was moved before authentication transactions and its immutable result is carried into token creation. Auth-code login also revalidates the preflight network inside the transaction, closing the race between preflight and consumption. |
| 3 | Fixed | The orphan-RST path copies source and destination IPs before returning the packet buffer to the pool. Real pooled IPv4 and IPv6 tests exercise reuse under the race detector. |
| 4 | Fixed | WG handoff is generation-tagged and follows the real deploy order: the replacement publishes its generation, the old proxy exports to that exact generation during drain, and the replacement polls for only its matching payload. The deployment-order integration test covers replacement-before-old-drain. |
| 5 | Fixed | Sharded reliability writes are behind the startup-cached `client_reliability_sharded_writes` setting and default off in production. The new rollup reads both legacy and shard formats. Rollout documentation and tests enforce new-reader-before-new-writer ordering and prohibit taskworker rollback while sharded writers are enabled. |
| 6 | Fixed | Stream expiry now performs an authoritative reconcile. Peer reconciliation compares prepared state fingerprints rather than only event versions, and one immutable snapshot is shared per network/process. Corrective reads repair missed expiries without making ordinary churn produce per-listener full reads. |
| 7 | Fixed | Deprecated Go/native entry points and C ABI symbols were restored as forwarding aliases; the P2P and taskworker API changes have compatibility wrappers. JavaScript declarations now match runtime methods, the package version was advanced, and generated ABI-baseline tests protect exported symbols. |
| 8 | Fixed | Production sampling now defaults to 1%, samples before preprocessing, caps retained candidates at 256, uses O(N log K) top-K selection, and moves HMAC/protobuf work to a bounded worker with count and byte queue limits. |
| 9 | Fixed | The minute probe now derives connection-rate deltas from `pg_stat_user_tables.n_tup_ins`, avoiding a time-range scan of `network_client_connection`. |
| 10 | Fixed | Primary and fallback DNS workers share a flight cancellation context; the winner cancels losers, and flight accounting remains until workers exit. |
| 11 | Fixed | Redis Lua scripts now pass TTLs as integer seconds. TTL assertions cover the stream add/join paths. |
| 12 | Fixed | Drain excuses connection-only clients, broadcasts migration with bounded concurrency and aggregate timeout, and waits only for clients that were actually sent a migrate frame. |
| 13 | Fixed | Task finalization uses a bounded context detached from root cancellation. `WaitFinalHandback` keeps the CLI alive for a final bounded handback grace after drain gives up, while preserving leases for tasks that still do not return. |
| 14 | Fixed | SDK migration versions and reapplies auth at swap time, so a racing token refresh cannot install stale credentials. Server migration timestamps are clamped to a bounded schedule. |
| 15 | Fixed | HTTPS/SVCB fanout restores each responder's original query casing and atomically detaches the flight/responders before completion, eliminating both the DNS 0x20 mismatch and late-attach loss. |
| 16 | Fixed | Window-identity persistence is serialized with mutation order. Deterministic tests prove an older snapshot cannot complete after and overwrite a newer snapshot. |
| 17 | Fixed | HTTP and SOCKS admission use an atomic drain-locked `tryEnter`, so drain cannot observe idle between acceptance and active accounting. |
| 18 | Fixed | Contract-details controllers arm timers only for real deadlines/resort work, and run totals expire at their activity deadline even if no later contract event arrives. |
| 19 | Fixed | Provide-mode updates use an atomic Lua operation that requires the member key to exist, retain a positive TTL, and still belong to the expected resident. Missing, persistent, or stale members are refused rather than resurrected. |
| 20 | Fixed | Key-event subscriptions are epoch-scoped and cancel prior resync work, use an explicit bounded drop-and-resync policy, and retain a five-minute corrective backstop. Shared snapshot read failure skips fanout and retries on the next corrective epoch instead of causing per-listener read amplification. |
| 21 | Fixed | Provider/server flow limits use a distinct profile, over-cap eviction uses bounded approximate-LRU selection instead of full sorting under the global lock, and contract creation uses fast-first exponential backoff. |
| 22 | Fixed | Missing MinIO configuration disables upload in non-local environments rather than silently using host disk. The local backend now has a byte cap in addition to TTL retention. |
| 23 | Waived | No compatibility shim was added. The prior `BlockAction.Ips/Hosts` semantics were never shipped, so the new matched/unmatched representation is the initial supported contract. |
| 24 | Fixed | Redis availability alerts now treat no data as alerting, with tests that lock the availability-rule behavior. |
| 25 | Fixed | Contract failures emit bounded cause-class counters plus rate-limited visible exemplars. Grafana rules and monitor documentation consume the lossless counters rather than sampled log volume. |
| 26 | Fixed | Loki search restored a 1,000-entry page bound and returns `ErrSearchIncomplete` when an equal-timestamp boundary cannot be exhausted safely. HTTP-backed pagination tests prove partial output cannot be reported as success. |

## Post-remediation validation

| Repository/area | Result |
| --- | --- |
| server compile sweep: `go test -run '^$' ./...` with the local test environment | Passed every package |
| standalone `connect`: `go test -mod=readonly -count=1 ./...` | Passed; root package completed in 572.332s |
| standalone `sdk`: `go test -mod=readonly -count=1 ./...` | Passed; root package completed in 482.206s |
| standalone `proxy`: `go test -mod=readonly -count=1 ./...` | Passed; root package completed in 50.694s |
| standalone `warp`: `go test -mod=readonly -count=1 ./...` | Passed every package, including Grafana alert and Loki pagination tests |
| race: connect orphan RST, DNS, window identity, NAT, and retry tests | Passed |
| race: SDK migration/auth, contract details, and memory tests | Passed |
| race: proxy drain and generation handoff tests | Passed |
| race: server reliability, peer/stream convergence, task finalization, and drain tests | Passed |
| server connect key-event reconnect/drop-repair integration | Passed |
| server connect no-NACK integration | Passed |
| SDK C generator and ABI baseline | Passed; generated surface contains 534 functions, 133 callbacks, and 44 constants |
| SDK JavaScript tests and production build | Passed |
| `go vet ./...` in server, connect, SDK, and proxy | Passed |
| `go vet ./...` in warp | One pre-existing warning remains at `warpctl/cloudwatchlogs/client.go:98` for unreachable code; that file is unchanged by this delta |
| `git diff --check` in server, connect, SDK, proxy, and warp | Passed |

The full server suite was not run as one monolithic command because it includes
large database/integration and fixed-port groups; the complete compile sweep,
focused database/integration suites, and race-enabled boundary suites above
cover the remediated paths. Apple, Android, and Windows application builds and
production-scale load tests were not run.

## Remaining release operations

These are coordinated deployment requirements, not open P0/P1/P2 code
findings:

1. Deploy shard-aware reliability taskworkers first, verify them, then enable
   `client_reliability_sharded_writes` and restart/connect-roll the writers.
   Do not roll back taskworkers to a legacy reader while the setting is active.
2. Apply the PostgreSQL migrations before dependent writers and verify Redis
   keyspace notifications (`notify-keyspace-events "Kg$sx"`) on every master.
3. Build and sign the Apple, Android, and Windows artifacts from the same SDK
   generation.
4. Run production-like load validation for provider-scale NAT churn,
   `FindProviders2` sampling, DNS outage behavior, peer fanout, and monitor
   telemetry before broad rollout.

## Original executive summary (pre-remediation)

The delta contains substantial, worthwhile work: Redis reliability sharding,
event-driven peer/hop updates, bounded client memory, P2P fixes, deploy drains,
proxy continuity, and lower-churn SDK views. Several of those improvements are
real and should materially reduce Redis stalls, reconnect churn, and client
memory use.

The combined tree is not safe for an unrestricted rollout or rollback yet.
The strongest confirmed issues are:

1. The new `sim-latency` tool has no local-environment guard and can migrate and
   provision the production databases when invoked from a production-configured
   shell (for example, one carrying `WARP_ENV=main` and production resources).
2. client authentication now performs a second PostgreSQL checkout and Redis
   access while holding an authentication transaction open, creating an
   outage-class pool-exhaustion path during Redis or PG pressure.
3. the new orphan-TCP-RST path returns a packet to the pool before using IP
   slices that alias that packet, creating a use-after-return data race.
4. WireGuard endpoint handoff is consumed by the replacement before the old
   proxy produces it, so the advertised continuity feature does not operate in
   the real warp deploy order.
5. new sharded reliability writers are not backward-compatible with an old
   taskworker rollup; a mixed generation or rollback can silently lose whole
   reliability blocks.
6. peer/stream key-event recovery does not meet its corrective-poll contract:
   stream-hop expiry is not applied even when its event arrives, and a missed
   peer expiry can remain stale until the 24-hour version key expires.
7. the SDK's JavaScript declarations do not describe the runtime object, while
   native and Go entry points were removed rather than deprecated. This is a
   source/ABI compatibility break despite the platform apps being updated
   locally.
8. two opt-in observability features become expensive immediately when enabled:
   `FindProviders2` samples synchronously sort/HMAC the full candidate pool, and
   the monitor runs an unindexed scan of `network_client_connection` every
   minute.

Recommendation: hold the coordinated release until the P0/P1 findings below
are fixed or explicitly gated. At minimum, prevent production use of
`sim-latency`, move entitlement lookup out of transactions, fix the pooled
packet alias, repair the actual WG handoff order, enforce reliability rollout
direction, and reconcile the SDK surfaces.

Severity used below:

- **P0** — production data/outage safety hazard.
- **P1** — confirmed defect on a realistic path, shipped feature does not work,
  or material performance/compatibility regression.
- **P2** — edge-case defect, conditional operational hazard, or lower-impact
  behavior/contract regression.
- **P3** — hardening or test-quality issue.

## Baseline and repository inventory

The comparison uses the locally recorded upstream refs; no network fetch was
performed. This matters because the baseline is only as current as those refs.

| Repository | HEAD | Recorded upstream | Effective delta |
| --- | --- | --- | --- |
| `server` | `0d614959` | `ddbdc8a5` | 3 commits ahead; 79 tracked files, +11,941/-847, plus many untracked files |
| `connect` | `491578c` | `2ce0a49` | 1 commit ahead; 42 tracked files, +5,988/-341, plus untracked files |
| `sdk` | `54add5d` | `7e50cb0` | 1 commit ahead; 43 tracked files, +4,271/-809, plus untracked files |
| `proxy` | `20a6070` | `20a6070` | no commits ahead; dirty tracked and untracked drain/WG work |
| `warp` | `eaec9e2` | `3b3c70a` | 2 commits ahead; 15 tracked files, +1,485/-1,135, plus untracked files |
| `apple` | `0a748c5` | `0a748c5` | dirty; 18 tracked files, +1,378/-530, plus new UI sources |
| `android` | `9eaca2bb` | `9eaca2bb` | dirty; 18 tracked files, +1,166/-545, plus quick-connect sources/assets |
| `windows` | `5b5afae` | `5b5afae` | dirty; 13 tracked files, +1,122/-441 |

`glog` and `userwireguard` are clean. `sn` has only an unrelated untracked
`price.yml`.

`server/go.mod` uses local replacements for `../connect`, `../proxy`, and
`../sdk`. The effective build is therefore a coordinated workspace, not a set
of independently releasable repository deltas. Untracked Go files are
load-bearing and were included in the review even though `git diff --stat`
cannot count them.

Path notation below is repository-oriented: `connect/...`, `sdk/...`,
`proxy/...`, and `warp/...` name sibling repositories. Unprefixed paths such as
`model/...`, `cli/...`, `monitor/...`, and `redis.go` are in `server`;
`server/connect/...` and `server/proxy/...` are used where a server subdirectory
would otherwise be ambiguous.

## P0

### 1. `sim-latency` can mutate production

Confidence: confirmed.

`server/connect/sim-latency/main.go:64,162-177` supplies `WARP_ENV=local` only when the
caller did not already set an environment. It never rejects a non-local
environment. That differs from the test harness, which hard-fails outside
`local` at `test_util.go:191-195`.

The `run` command then:

- applies the current local migrations (`server/connect/sim-latency/run.go:51-53`);
- bulk-provisions simulated provider/client identities and balances
  (`server/connect/sim-latency/provision.go:124-138`);
- refreshes reliability state and can upsert passing reliability weights for
  every connected/valid provider
  (`server/connect/sim-latency/provision.go:220-257`);
- updates the same PostgreSQL and Redis resources selected by `WARP_ENV`.

Because operator workflows elsewhere intentionally use `WARP_ENV=main`, a
shell carrying that value and the corresponding resource configuration turns a
tool documented as local simulation into a production writer. The default
command also models a very large fleet, so an accidental invocation is not a
small contamination event.

Required action: refuse every mutating `run`/`fleet` entry point unless the
resolved environment is exactly `local`. Do not rely on setting a default.
Ideally require a separate explicit destructive-test acknowledgement as well.

## P1

### 2. auth holds a transaction while acquiring another PG connection and Redis

Confidence: confirmed.

Both `AuthNetworkClient` branches open a transaction at
`model/network_client_model.go:269` and `:546`, then call `IsProFresh` inside it
at `:413` and `:655`.

The call chain is:

`IsProFresh` -> `IsProNetworkFresh` -> `UpdateProNetwork` ->
`loadProNetwork` (`model/pro_model.go:127-130,167-192`) -> `server.Db`, followed
by `setProNetworkCached` -> Redis (`pro_model.go:209-216`).

The inner `server.Db` cannot reuse the transaction connection, so each auth can
hold one pool slot while waiting for another. Redis first performs `PING` and
uses the normal retry loop (`redis.go:304-333`), whose default retry budget is
60 seconds. Under Redis failure or PG pool pressure, concurrent auth requests
can therefore pin the pool in idle/open transactions, double connection
demand, and make auth fail when PostgreSQL itself could have answered.

The same nested pattern appears on lower-volume transaction paths in
`model/auth_model.go:1626`, `model/device_association_model.go:894`, and
`model/network_model.go:1249,1344`.

Required action: derive the fresh entitlement before opening each transaction,
then carry the immutable result into token creation. Redis cache refresh should
not be allowed to extend a correctness transaction.

### 3. orphan-RST callback reads packet-pool memory after return

Confidence: confirmed.

`connect/ip.go:762-795` explicitly documents that parsed IPv4/IPv6 address
slices alias the input packet. On the new orphan-RST path:

1. `tcpRstForOrphan` is built;
2. `MessagePoolReturn(ipPacket)` runs at `connect/ip.go:1883`;
3. the receive callback is invoked at `:1995-2007` with an `IpPath` whose
   `SourceIp` and `DestinationIp` still point into that returned packet.

A concurrent `MessagePoolGet` can overwrite the backing memory while policy,
association, or egress code reads the path. Outcomes include a race-detector
failure, a dropped reset, or garbage IPs entering policy/association state.
The RST byte slice itself is separate; the unsafe values are the path IPs.

The current test constructs `parsedTcp` with standalone `net.IP` values
(`connect/ip_orphan_rst_test.go:37-51`), so it cannot expose the alias.

Required action: copy the two IP slices before returning the packet, as
`ParseIpPathWithPayload` already does at `connect/ip.go:3907-3914`, or defer
pool return until the callback has finished. Add a real pooled-packet race test
for IPv4 and IPv6.

### 4. WG endpoint handoff is consumed before it is produced

Confidence: confirmed.

The replacement calls `ApplyWgHandoff` immediately after initial proxy-client
sync (`cli/proxy/main.go:167-181`). `ApplyWgHandoff` performs one
`GETDEL` and returns if absent (`server/proxy/wg_handoff.go:70-81`;
`model/network_client_proxy_wg_handoff_model.go:73-99`).

The old instance does not export until the drain coordinator's before-exit
phase (`cli/proxy/main.go:120-123`; `server/proxy/drain.go:159-178`), after the
SOCKS/HTTP grace has completed.

Warp's real order is replacement start -> ready poll -> traffic redirect ->
asynchronous old-container drain (`warp/warpctl/run.go:582-630,667-707`).
Consequently the replacement's one-shot read happens before the old instance's
write. The later export remains for ten minutes and can be consumed stale by a
subsequent deploy.

The test uses export-before-apply order
(`server/proxy/wg_handoff_test.go:51-86`), which proves the primitives but not the
deployment protocol.

Required action: either export before redirect/drain, or have the replacement
wait/poll for a generation-tagged export after old-instance drain begins.
Exercise the exact warp order in an integration test.

### 5. reliability sharding is unsafe in the old-reader/new-writer direction

Confidence: confirmed.

New writers only write one of 32 shard hashes
(`model/network_client_reliability_model.go:319-350`). The new rollup correctly
reads the legacy hash plus all shards (`:413-447`), covering old writer/new
reader rollout.

The old rollup, visible in the `origin/main` version of the same file, reads
only the legacy hash. It then records block coverage, removes the shared block
marker, and advances the high-water mark. If an old taskworker claims a block
containing new shard writes, it persists an empty/partial legacy view while the
shards become undiscoverable and expire.

`RELIABILITY2.md:101-114` acknowledges that an old taskworker cannot see the
shards, but describes the result as bounded stats loss. The failure is stronger:
the old worker can declare an entire block drained and orphan every shard for
that block. Existing mixed tests model old writers with a new reader
(`model/network_client_reliability_shard_test.go:111-181`), not the unsafe
direction.

Required action: enforce new-taskworker-before-new-connect and forbid rollback
to an old taskworker while sharded writers are active, or dual-write/version the
pending marker until all generations can read shards. Add an old-reader/new-
writer deployment test. If mixed generations have already run, audit the
affected block range.

### 6. key-event convergence is wrong, and normal peer churn still causes fleet-wide resets

Confidence: confirmed.

There are three related defects.

**Stream-hop expiry is not applied even when its event arrives.**

The hop set has an 8-hour TTL while the event-id key lasts 24 hours
(`model/stream_model.go:209-216,323-345`). Every hop key event, including
`expired`, only calls `StreamHopListener.Kick`
(`server/connect/key_event_subscriber.go:201-213`). The listener full-reads only when
the version differs, and `reset` only delivers when that version differs
(`model/stream_model.go:749-817`). Redis expiry does not increment the version.
The result is a phantom hop for roughly the remaining 16 hours until the
event-id key expires, plus the corrective-poll delay.

**A missed peer expiry is not repaired by the advertised five-minute poll.**

Peer membership is represented by TTL member keys
(`model/peer_model.go:109-117,493-582`). A delivered `expired` event produces a
remove delta, but a notification lost during reconnect/config drift does not
bump the peer event id. The corrective full read sees the same event id and
suppresses its callback (`model/peer_model.go:1139-1155,1213-1239`). In
key-event mode the unconditional read is disabled and polling is moved to five
minutes (`server/connect/resident.go:1319-1340`). The stale peer can remain until a
later visible mutation or the 24-hour version-key TTL.

The dropped-event test in `server/connect/peer_discovery_test.go:1009-1080` drops a
write that does increment the version; it does not cover lost expiration.

**Every ordinary peer mutation schedules a redundant full snapshot.**

Add/remove/provide changes increment the Redis version, but a successfully
delivered delta deliberately leaves each listener's local event id unchanged
(`model/peer_model.go:1158-1181`). At the next corrective poll, every listener
detects a mismatch and full-reads/delivers the complete peer set. For a network
with N peers/listeners, one normal mutation therefore creates delayed O(N²)
serialization and fanout work every poll interval, despite the comment that
corrective deliveries should be near zero.

Required action: make expiry an authoritative state transition, and make a
full-state reconcile deliver based on state change rather than only version
change. Normal deltas also need a safe way to advance/acknowledge the committed
version, preferably once per network/process rather than once per listener.
Add explicit peer-expiry and hop-expiry tests with notification loss.

### 7. SDK JavaScript declarations and native/Go APIs are breaking

Confidence: confirmed.

The JS runtime exposes:

- `openClientContractDetailsViewController` and
  `openProviderContractDetailsViewController`
  (`sdk/js/device_remote.go:217-228`);
- `getContractRows`, `setAtTop`, and `pendingCount`
  (`sdk/js/view_controllers.go:235-283`).

`sdk/js/src/types.ts:154-167,228-268` still declares the removed
`openContractDetailsViewController`,
`getClientContractRows`/`getProviderContractRows`, and the old
`ContractClientRow`. A TypeScript consumer therefore compiles successfully and
then calls undefined runtime properties. `tsc --noEmit` passing only proves the
declaration file is internally consistent; it does not compare it to the Go
WASM object.

The native C header and `.def` remove the old contract-details symbols rather
than retaining aliases (`sdk/cgo/include/urnetwork_sdk.h:516-523,658-705`;
`sdk/cgo/include/urnetwork_sdk.def:128-133,250-291`). Existing dynamically
linked consumers lose ABI symbols on the next DLL/framework update.

The public Go `ViewControllerManager` also replaces one method with two
(`sdk/view_controller.go:39-44`). Other exported Go signatures changed, such as
`connect.NewP2pSendTransport` gaining `peerId`
(`connect/transport_p2p.go:279-286`) and
`taskworker.InitTaskWorker` gaining settings
(`taskworker/taskworker.go:81-90`).

The local Apple/Android/Windows sources were updated for the new SDK, so a
lockstep source build may be fine. External consumers and mixed package
versions are not backward-compatible.

Required action: keep deprecated forwarding aliases for at least one release,
restore old C exports, update the TS declarations/package version, and add
automated runtime-vs-declaration and exported-symbol baseline checks.

### 8. `FindProviders2` telemetry adds synchronous hot-path work at 100% sampling

Confidence: confirmed; impact is conditional on stats being enabled.

The default sample fraction is 1.0 and the cap is 2,000 candidates
(`model/find_providers_stats.go:41-48`). For every sampled request, the code:

- collects and sorts the entire candidate map before applying the cap
  (`:83-102`);
- performs per-candidate protobuf allocation;
- HMAC-anonymizes both client and network IDs for every retained candidate
  (`:104-125`);
- calls this synchronously inside `FindProviders2`
  (`model/network_client_location_model.go:3066-3079`).

The normal selection path loads at least 1,000 candidates but only sorts the
selected output count (`network_client_location_model.go:2972-3006,3035-3052`).
Telemetry can therefore cost more CPU/allocation than selection itself.
`stats.Append` is nonblocking only after all construction is complete. Its
4,096-element queue is count-bounded, not byte-bounded, so large samples can
also retain substantial memory while the writer catches up.

`cli/api/main.go:98-105` enables stats whenever site/salt configuration permits,
making the default 100% rate easy to activate operationally.

Required action: default to a small fraction, sample before any preprocessing,
cap/partially select before a full sort, move HMAC/protobuf work to a bounded
worker, byte-bound the queue, and benchmark API p50/p99 with realistic pools.

### 9. the monitor can full-scan `network_client_connection` every minute

Confidence: confirmed.

`monitor/probe_pg_tier1.go:59-80` runs:

```sql
SELECT count(*)
FROM network_client_connection
WHERE connect_time >= ... AND connect_time < ...;
```

every 60 seconds. The schema has indexes led by `client_id`, `connected`, or
`disconnect_time`; the only `connect_time` occurrence is secondary to
`client_id`. There is no index usable for this time-only range. On the large,
delete-churned production table described by the surrounding monitor docs, this
is a repeated primary-database scan, bounded only by statement timeout.

Required action: do not deploy this standing probe until it uses a suitable
BRIN/btree index, a rollup table, or an existing counter metric. Validate with
`EXPLAIN (ANALYZE, BUFFERS)` against production-like cardinality.

### 10. local DNS fallback releases the flight but not the losing tunnel worker

Confidence: confirmed.

The default client mux uses a 60-second resolve budget, starts local fallback
after five seconds, and caps the in-flight question map at 96
(`connect/ip_mux_upgrade.go:95-160`).

Primary and fallback workers receive independent contexts
(`ip_mux_upgrade.go:658-770`). When fallback wins, `reply` marks the flight
replied and deletes it from the 96-entry map, but it does not cancel the
tunnel worker. That worker can remain blocked in the DoH resolution/semaphore
path for the rest of its 60-second budget. New distinct names immediately
occupy the freed map slots and create more losing workers.

The DoH HTTP and resolution semaphores bound active network work, but not the
number of goroutines/single-flight entries waiting behind them. During a tunnel
outage, outstanding primary work scales with query arrival rate times the
remaining timeout, not `MaxInflightQueries`.

Required action: give a flight a shared cancellation context and cancel losing
workers after the first authoritative answer. Keep accounting until all
workers exit, or separately cap loser/queued workers.

The fallback also intentionally sends A/AAAA names over local egress after five
seconds (`ip_mux_upgrade.go:136-144,751-769`). That is a documented availability
tradeoff, not an accidental bug, but it is a privacy and split-DNS behavior
change that needs product/security approval and release notes.

### 11. Lua stream TTLs use nanoseconds where Redis expects seconds

Confidence: confirmed; the delta expands an inherited defect.

`AddToStream` and the new `joinStream` pass an `8*time.Hour` value directly as
a Lua `EXPIRE` argument (`model/stream_model.go:256-296,410-442`). go-redis
v9.21.0 serializes a `time.Duration` argument as `Nanoseconds()`, while Redis
`EXPIRE` interprets the integer as seconds. The resulting TTL is about 913,000
years, not eight hours.

Typed `Set`/`Expire` calls in the surrounding code convert duration correctly;
only the raw Lua argument is wrong. The original `AddToStream` site predates
part of this delta, but `joinStream` copies it into the new companion-join path
and makes the current stream-memory work depend on it.

Consequences include effectively permanent crashed-stream keys and possible
reattachment to stale stream membership.

Required action: pass `int64(ttl/time.Second)` to both scripts and assert Redis
TTLs in tests.

## P2

### 12. connect drain misses split-state clients and can spend the whole migrate window serially

Confidence: confirmed.

Drain initially snapshots and excuses only local residents
(`server/connect/resident.go:1232-1243`). During the straggler sweep, a client can
exist in `connections` while its resident is hosted elsewhere; the lookup at
`:1275-1277` then returns `resident == nil`, so the transport is canceled
without `markDrained` and without a migrate frame. That client takes an
unexcused reconnect, undermining the reliability protection that motivated the
drain work.

The migrate broadcast is also serial, with up to one second per
`SendWithTimeout` (`resident.go:1161-1186`), and checks the aggregate drain
deadline only after the full loop. Its subsequent wait uses all local
connections, including split-state connections that were never sent a migrate
frame (`:1192-1214`), so even a successful resident migration can wait the
entire two-minute window.

Required action: excuse connection-only clients, restrict the wait to the
population actually asked to migrate, and use bounded parallel fanout with an
aggregate deadline.

### 13. taskworker give-up cancels the context needed to hand tasks back

Confidence: confirmed on the control flow.

`TaskWorker.Drain` waits 60 seconds, cancels task function contexts, waits
another 30 seconds, and can then return with tasks still running
(`task/task.go:1076-1121`). The CLI drain goroutine has `defer cancel()`
(`cli/taskworker/main.go:139-158`), so return from the give-up case cancels the
root context.

Batch finalization and claim release use `self.ctx`
(`task/task.go:1526-1628`). If a slow task unwinds after give-up, its finalize
transaction starts with a canceled context. Successfully completed batch-mates
are finalized in the same pass and can lose their already-earned handback too.
Claims then remain until their max-time lease rather than being released
immediately as the drain comments promise.

Required action: run finalization on a bounded context detached from the root
cancel, or keep the process context alive for a final handback grace.

### 14. SDK migration can install stale auth or remain stuck on a bad timestamp

Confidence: confirmed races; exact production frequency is unknown.

`sdk/device_local_provider.go:146-198` snapshots `self.auth`, constructs and
connects a replacement transport, then swaps it in. `SetByJwt` updates
`self.auth` and only the currently installed transport (`:209-219`). If token
refresh occurs after the migration snapshot but before swap, the old transport
receives the new JWT and the replacement becomes current with the old JWT.

The same function waits until the server-provided absolute `migrate_time`
without an upper bound (`:146-153`). Clock skew or a malformed future timestamp
keeps `migrating=true`; subsequent migrate frames are discarded by the CAS at
`:129-135`.

The only migration test covers timeout/keep-old behavior
(`sdk/device_local_provider_test.go:18-97`), not successful swap, auth rotation,
or skew.

Required action: version/reapply auth while holding the swap lock and clamp
scheduled migration to a small multiple of the server window.

### 15. forwarded HTTPS/SVCB DNS replies can violate query-case matching and lose a responder

Confidence: confirmed code behavior; client impact depends on resolver.

The mux normalizes the domain to lowercase
(`connect/ip_mux_upgrade.go:544-548`) and constructs the DoH wire question from
that lowercase name (`connect/net_http_doh.go:875-889`). For raw HTTPS/SVCB
fanout it changes only the two-byte transaction ID
(`connect/ip_mux_upgrade.go:843-851`); unlike A/AAAA synthesis, it does not
restore each responder's original-cased question. Stubs that validate DNS 0x20
case entropy can reject an otherwise valid answer.

There is also a narrow race: `fanOutHttpsForward` sets `fl.replied` and
snapshots responders without atomically deleting the flight (`:818-828`);
cleanup is deferred in the caller (`:789-797`). A query attaching during that
window is appended after the snapshot and receives no response.

Required action: rebuild/patch a response from each original question and
remove the flight atomically with the responder snapshot (or reject attachment
to a replied flight). Extend the fanout test to use randomized query casing and
an attach-vs-complete race.

### 16. persisted window snapshots can complete out of order

Confidence: confirmed.

`connect/ip_remote_multi_client_identity.go:129-170` mutates state and captures
a full snapshot under a mutex, then performs `StoreWindowClientIdentities`
after releasing it. Two concurrent changes can write newer state first and an
older snapshot second. The server adapter performs a synchronous Redis
replacement (`proxy/window_identity.go:29-47`).

A crash/restart in that state can lose a live identity or resurrect a removed
one, defeating the NAT-continuity purpose for the next restart.

Required action: serialize store completion, or attach a monotonic generation
and use compare-and-set/Lua on Redis. Add a deterministic reordered-store test.

### 17. proxy drain active accounting has an admission race

Confidence: confirmed; the scheduling window is small.

`proxy/drain.go:65-143` checks draining and increments active state in separate
operations; `enter` explicitly never refuses. HTTP checks `Draining()` and then
calls `enter()` (`proxy/http.go:280-291`). SOCKS accepts a connection and starts
a goroutine, with `enter()` only inside that goroutine
(`proxy/socks5_server.go:73-110`).

Drain can close admission and observe `active == 0` between those operations,
return idle, and let the caller cancel the serving context just as the
accepted/requested operation becomes active. Existing tests exercise already-
active relays, not forced scheduling at this boundary.

Required action: use an atomic `tryEnter` under the drain lock, or account for a
SOCKS connection before goroutine handoff.

### 18. contract-details totals can remain stale, and each open controller wakes at 10 Hz

Confidence: confirmed.

The controller always creates a 100 ms ticker
(`sdk/contract_details_view_controller.go:248-291`), despite the adjacent
comment saying idle controllers do not tick. Every open screen therefore wakes
ten times per second even with no changes or deadlines.

Run totals reset only when the aggregator recomputes
(`contract_details_view_controller.go:706-718`). The upstream tracker emits one
trailing bit-rate decay and then stops (`sdk/device_local.go:3723-3755`). If
that zero-rate event arrives before the five-second activity window expires,
the recompute preserves the old total. Later resort ticks only reorder cached
rows and never invoke the reset, so the displayed total can remain stale until
an unrelated contract event.

The idle-reset unit test directly invokes the aggregator at a later timestamp;
it does not cover the no-more-events controller path.

Required action: arm timers only for actual deadlines/resort work and perform a
cheap cached run-total expiry when the activity deadline passes.

### 19. peer provide-mode update can resurrect a TTL-less member key

Confidence: confirmed from Redis command semantics.

`UpdateNetworkPeerProvideModes` reads metadata from the 24-hour hash and then
uses `SET ... KEEPTTL` on the per-member key
(`model/peer_model.go:858-897`). If that shorter-lived member key has already
expired, `KEEPTTL` creates a new key with no TTL. A concurrent remove between
the read and pipeline has the same effect.

The update also bumps the version and produces a `set` event. Delta reads use
the metadata hash (`peer_model.go:152-169`), so listeners can see a stale peer,
and the newly persistent event key will never emit its expected expiry.

Required action: make metadata/member existence and update atomic in Lua,
validate resident ownership, and refuse to create a missing member key.

### 20. key-event subscriber failure handling can amplify a Redis incident

Confidence: confirmed mechanics; production threshold is workload-dependent.

Each per-master PubSub reader blocks on a shared 1,024-message output channel
(`redis.go:400-441`). A sufficiently large event burst can stop draining the
go-redis PubSub channel and increase the chance of output-buffer loss or
reconnect exactly while Redis is stressed.

Every subscription establishment starts a new uncancelled trickle goroutine
over all registered listeners (`server/connect/key_event_subscriber.go:129-174,
236-272`). Repeated topology/subscription flaps can overlap those full-read
waves. In addition, go-redis PubSub normally auto-reconnects internally, so a
connection gap may not close `Channel()` and may never trigger the explicit
resync path; repair then relies on the already-defective corrective polling.

Required action: use generation-scoped/cancelable resync, define an explicit
drop-and-force-resync policy at the merge, and instrument actual PubSub
connection epochs rather than only channel closure.

### 21. new NAT defaults can reset live provider flows and make over-cap insertion expensive

Confidence: confirmed mechanics; appropriate production limits require load
data.

Default TCP/UDP global limits are now 512/2,048 when no memory budget is set
(`connect/ip.go:86-93,99-126`). Those defaults apply to provider-side users of
the generic buffers as well as memory-constrained apps.

At the limit, every new flow collects all sequences, takes each sequence's
activity lock, sorts the whole set, and cancels the least-recently-active flow
while holding the buffer-wide mutex (`connect/ip.go:1019-1074,1899-1936,
4125-4155`). This is O(N log N) work in the dispatch path and resets an
established flow. UDP idle timeout is also now 60 seconds.

Separately, `CreateContractRetryInterval` changed from five seconds to one
second (`connect/transfer.go:124-145,2524-2563`), multiplying control/API work
by about five for destinations that repeatedly cannot create a contract.

Required action: define provider/server profiles rather than sharing phone
defaults, replace sorting with O(1)/O(log N) LRU accounting, and use
fast-first-then-backoff contract retries. Validate under provider-scale
concurrent flows.

### 22. absent MinIO config silently turns the API host into the blob store

Confidence: confirmed; disk-fill rate depends on traffic and retention.

`LoadBlobStore` treats absent `minio.yml` as a successful local backend
(`blob.go:105-123`). The uploader then copies finalized segments into that
local blob tree and deletes the source (`stats/upload.go:125-154`).

The source tree has an 8 GiB failure cap, but successful local copies bypass
that cap. The local backend now has a TTL reaper
(`blob.go:351-405`), so this is not infinite retention; however it has no byte
cap and can accumulate the full retention window. At 100% `FindProviders2`
sampling with up to 2,000 candidates per sample, that can fill an API host well
before seven days.

Required action: in non-local environments, treat missing MinIO configuration
as upload-disabled or fatal rather than silently selecting local storage, and
apply a byte cap to the local object backend.

### 23. `BlockAction.Ips/Hosts` changed meaning despite additive fields

Confidence: confirmed.

Before this delta, `Ips` and `Hosts` represented the entire association
cluster. They now contain only unmatched values; matched values moved into
`MatchedIps`/`MatchedHosts`
(`connect/ip_block_action.go:50-75` and collector changes near `:722`).

The wire/source change looks additive, but an older consumer that only reads
`Ips`/`Hosts` silently omits exactly the values that matched an override. This
is a semantic backward-compatibility break, not merely an optional enhancement.

Required action: preserve the old full-set fields and add explicitly named
matched/unmatched subsets, or version the event schema and all consumers.

### 24. Redis alert rules turn complete telemetry loss into OK

Confidence: confirmed.

All eleven new Redis Grafana rules in
`warp/grafana/alerting/redis-cluster.yml` use `noDataState: OK`, including
`RedisNodeDown`. If Redis, the exporter, scraping, or the metric pipeline
disappears completely, the rules resolve rather than alert. `RedisNodeDown`
only works when the exporter remains up and reports `redis_up == 0`.

Required action: make no-data alerting for availability rules, or add an
independent exporter/scrape heartbeat rule.

### 25. default logging hides the only cause-specific contract-failure signal

Confidence: confirmed.

`controller/connect_controller.go:335-340` says the underlying error must
always be logged because every `nextContract` failure is returned to the client
as `InsufficientBalance`, and calls that log line the only way to distinguish
unrelated causes. The current worktree changes the line from `glog.Infof` to
`glog.V(2).Infof`; the server default is verbosity zero
(`env.go:42-48`), so normal production logs no longer contain it.

This directly contradicts the new operations guide. `monitor/SIGNALS.md:455-457`
uses the rate and error text of `[contract][error]` lines to distinguish real
balance failures from missing companion origins and to detect regressions.
With default settings, those documented signals disappear and all causes
remain collapsed into the same client response.

The former volume (the guide records more than 1,000 expected lines/minute)
justifies reducing log cost, but not removing the only diagnostic dimension.

Required action: emit bounded error-class counters and rate-limited/sampled
examples at normal visibility. If the log is retained at V(2), update the
monitoring and runbook to use a replacement metric before rollout.

### 26. Warp log search can skip results and still report success

Confidence: confirmed when more than one page of entries shares a timestamp.

`warp/warpctl/loki/client.go:189` raises the page size from 1,000 to 5,000.
Pagination deduplicates the inclusive boundary by `{block,line}`. If the next
full response contains only already-seen entries at that timestamp,
`printedCount == 0`; the new behavior advances `start` by one nanosecond and
continues (`:228-241`).

Any as-yet-unreturned entries at that same timestamp are then permanently
skipped. The function logs a warning but returns `nil`, so callers and
automation cannot tell that a search documented to print up to `limit`
matching lines is incomplete. Previously this condition returned an error.
The larger page also raises per-query response and client allocation bounds by
up to five times. Existing Loki tests cover query construction and stream
sorting, not pagination or live-tail reconnect behavior.

Required action: preserve an explicit incomplete-result error/status unless
Loki provides a stable secondary cursor that can exhaust equal timestamps.
Add an HTTP-backed test with more than one page at the same timestamp and
unique entries beyond the first page.

## P3 and unproven integration risks

- Proxy prewarm restores the same `{clientId, jwt, instanceId}` while the old
  container can still be serving it
  (`connect/ip_remote_multi_client_api.go:268-297`). A live-window eviction
  removes the identity and calls `RemoveNetworkClient` (`:300-323`). Sequence
  supersession makes interference plausible during overlap, but the timing was
  not proven. Add a real two-container deploy-order test before relying on
  prewarm/window persistence for continuity.
- API SIGTERM can set `draining`, then an in-progress readiness/warmup path can
  overwrite it with `ready` (`cli/api/main.go:73-83,112-127`). Make draining a
  sticky state and guard the ready latch.
- `reverseIndex.shed` says it drops records at or below the median but deletes
  only values strictly below it (`connect/ip_mux_upgrade.go:1147-1172`). Batch
  inserts share timestamps, so a memory-pressure shed can delete little or
  nothing when many entries tie.
- The full connect suite has a timing-sensitive assertion:
  `connect/ip_block_action_test.go:541-558` accepts the first cumulative stats
  callback after registration, which can describe the preceding event epoch
  (`connect/ip_remote_multi_client.go:599-624`). The isolated test passed 105
  consecutive reruns; the full suite failed once with local egress count 1
  instead of 2. Wait for the required cumulative values rather than assuming
  the first callback.
- `sdk/js/package.json` retains the previous beta version across a breaking
  public API change.
- `git diff --check` is clean except trailing whitespace in
  `apple/app/network/Shared/Views/Stats/ContractDetailsView.swift:325`.

## Original backward-compatibility assessment (pre-remediation)

| Surface | Assessment | Detail |
| --- | --- | --- |
| Transfer wire protocol | Compatible | `TransferResidentMigrate = 28` and its message are additive. Existing message numbers/types are unchanged; old clients can ignore the unknown control frame. |
| `StreamReset` | Wire-compatible, generation-dependent behavior | New clients reconcile and retain listed streams/P2P state; old clients cancel/reopen. Server drain must tolerate both. |
| Redis reliability | One-way compatible only | New reader handles old and new writers. Old reader does not handle new writers; rollback/mixed generations lose data. |
| Other Redis keys | Mostly additive | Proxy activity, WG handoff, window identity, peer member, and stream pair keys are additive/TTL-scoped. Peer TTL/update behavior has the correctness issues above. |
| PostgreSQL | Additive but ordered | Apply `connection_excused_new_count` before code writes it. `open SET STATISTICS 10000` is an instant catalog change but makes future ANALYZE substantially more expensive; it addresses a much worse planner failure. |
| SDK JS | Incompatible | Type declarations name removed runtime methods and omit current methods/types. |
| SDK native ABI | Incompatible on rebuild/update | Old C exports were removed from header/DEF; locally updated apps need matching SDK binaries. |
| Exported Go APIs | Incompatible | Several signatures/method sets changed without compatibility wrappers. |
| SDK RPC/gob | Compatible | Removed/added struct fields are skipped/zero-filled by gob; method names used over RPC were not changed. |
| JWTs | Compatible | No claim-number or validation change was found; the risk is the migration race, not token format. |
| Warp `/status` | Compatible | `ok`, `draining`, and `error not ready...` remain distinguishable by old/new warp status handling. |
| Contract-failure telemetry | Operationally incompatible | Default deployments no longer emit the cause-specific log that the new runbook treats as an incident signal. |
| Warp Loki search | Behavior regression | An overfull equal-timestamp boundary now yields partial successful output instead of an explicit error. |
| `BlockAction` | Semantically incompatible | Existing field meanings changed even though new fields are additive. |
| DNS behavior | Intentional default change | Local-egress fallback after five seconds trades privacy/split-DNS correctness for startup availability. |
| Provide control | Product behavior change | `auto` can keep Network providing while otherwise idle; mixed old RPC hosts may interpret the new control value as no providing. |

The local Apple, Android, and Windows source trees appear updated to the new SDK
model, but platform builds were not run. Compiled frameworks/DLLs and app
sources must be produced from the same SDK generation.

## Original performance assessment (pre-remediation)

### Improvements that hold up

- Reliability stats use compact binary fields, 32 shards, and chunked HSCAN
  instead of one huge HGETALL. This directly addresses long Redis event-loop
  stalls and spreads write load.
- Stream companion lookup uses a pair marker rather than scanning contracts.
- Healthy key-event delivery removes constant five-second full polling. The
  expiry/version and redundant-reset defects need correction before the
  intended fleet-level savings are realized.
- P2P ready-header serialization removes a real concurrent-reader/short-buffer
  failure, and peer-connection/message sizes are bounded.
- Client reverse-DNS, SNI, block-action, NAT, and provider collections have
  explicit caps and memory-pressure hooks. The provider profile and median-tie
  issues above remain.
- SDK contract-detail aggregation removes prior pairing work, throttles data
  recomputation, and reduces feed count. The permanent timer and idle-total
  edge are localized fixes.
- HTTP, task, connect, and proxy drains generally gate new work before waiting
  for old work. The targeted HTTP/task/proxy tests passed; the handoff and
  boundary races are orchestration defects rather than a rejection of the
  overall design.
- The PostgreSQL statistics target is a defensible fix for the documented
  `open=true` selectivity failure. Operationally, expect ANALYZE to sample
  roughly 100x more rows for that column.

### Main regression risks

| Risk | Trigger | Expected effect |
| --- | --- | --- |
| nested auth I/O | Redis/PG pressure during client auth | pool amplification, long-held transactions, broad auth outage |
| peer corrective resets | any normal peer churn | delayed full peer snapshots across every listener; O(N²) network serialization |
| unresolved DNS losers | tunnel slow/down while local fallback succeeds | goroutine/queue growth beyond the configured flight cap |
| `FindProviders2` samples | stats site+salt enabled | request CPU, allocation, HMAC, and queue-memory spike |
| monitor connect-rate query | monitor enabled | primary PG table scan every minute |
| NAT caps | busy provider reaches 512 TCP / 2,048 UDP | established-flow resets plus O(N log N) work under a global lock |
| one-second contract retry | unavailable/unauthorized destinations | about 5x contract-create control load |
| local blob fallback | stats enabled, MinIO missing | API-host disk growth up to the retention window |
| key-event reconnect storms | Redis/topology instability | overlapping resync full reads while the cluster is already unhealthy |
| Loki search page expansion | broad/high-volume log query | up to 5x entries decoded per request; equal-timestamp overflow is silently skipped |

No new benchmark results were produced, so performance conclusions are based on
hot-path placement, algorithmic complexity, lock scope, timeout accounting, and
I/O topology. Production-like load tests are still required for
`FindProviders2`, provider NAT caps, DNS outage behavior, peer fanout, and the
monitor query.

## Original validation performed

| Repository/area | Result |
| --- | --- |
| standalone `proxy`: `go test -mod=readonly -count=1 ./...` | Passed; root package completed in 51.153s |
| `connect`: full `go test -mod=readonly -count=1 ./...` | One failure after 532.620s: `TestMultiClientBlockActionOverrides`, expected cumulative local egress 2, received 1 |
| isolated connect failure | Passed 5 repetitions and then 100 repetitions; classified as a suite timing flake, not evidence that the asserted runtime count is wrong |
| SDK targeted Go tests | Passed: migrate-timeout, contract run totals, provide modes, and grid reconcile tests |
| SDK JavaScript tests | 7/7 passed |
| SDK TypeScript `tsc --noEmit` | Passed, but does not compare declarations to the WASM runtime |
| server model targeted tests | Passed in 19.640s with local PG/Redis environment |
| server task drain targeted tests | Passed in 23.667s |
| server HTTP drain targeted tests | Passed in 5.374s |
| standalone warp `go test -mod=readonly -count=1 ./warpctl/...` | Passed all Warp CLI packages, including Loki, in 5.597s; Loki has no pagination/live-tail tests |
| server `proxy` selected integration suite | Inconclusive: did not finish after more than 35 minutes and was interrupted |
| SDK full Go suite in the initial sandbox | Inconclusive: loopback bind denial caused timeout; targeted rerun with required access passed |
| whitespace validation | Clean except the Apple trailing-whitespace line noted above |

The database-backed tests initially failed when run without the repository's
required local environment variables; those setup failures were superseded by
runs using `WARP_ENV=local` and the configured local PG/Redis endpoints.

No Apple/Android/Windows build, race-enabled full suite, production-config
probe, or external network fetch was performed. Builds were intentionally
avoided because they create derived artifacts, and the original request
prohibited modifications before review.

## Original required release gates

1. Add the hard local-only guard to `sim-latency`.
2. Move all fresh-Pro/database/cache work out of open auth transactions.
3. Fix and race-test the orphan-RST packet ownership.
4. Redesign WG handoff around the real warp ordering and test that ordering.
5. Make reliability generation ordering enforceable; never roll an old
   taskworker under sharded writers.
6. Fix peer/hop expiry convergence and remove normal-churn corrective
   full-snapshot amplification.
7. Restore SDK compatibility aliases and make JS declarations match runtime.
8. Keep `FindProviders2` sampling and the connect-rate monitor probe disabled
   until their hot-path/database cost is bounded.
9. Apply the PostgreSQL migrations before dependent writers and verify
   `notify-keyspace-events "Kg$sx"` on every Redis master.
10. Run coordinated SDK platform builds, a real two-generation proxy deploy
    test, race tests for packet/identity/drain boundaries, and production-scale
    load tests for the five performance hotspots listed above.
11. Replace the suppressed contract-error log with actionable metrics and make
    Loki search surface incomplete results.

## Original disposition (superseded)

The architecture is directionally strong, and several changes address real
production failure modes with thoughtful bounds and observability. The current
effective workspace nonetheless combines multiple generations and contains
confirmed correctness, rollout, ABI, and hot-path regressions. It should be
treated as a coordinated pre-release branch, not independently merged repos,
until the P0/P1 items and deployment gates are resolved.

## Post-remediation disposition

The accepted code-level release blockers from this review are resolved: all 25
requested P0/P1/P2 findings are fixed, finding 23 is intentionally waived, and
the post-fix re-audit found no additional P0/P1/P2 regression in the reviewed
delta.

The repositories still form a coordinated release rather than independently
deployable generations. Release approval should therefore remain conditional
on the ordered reliability rollout, required migrations and Redis
configuration, platform artifact builds, and production-like load validation
listed above. Those gates manage deployment and capacity risk; they do not
represent a known unfixed P0/P1/P2 correctness defect.
