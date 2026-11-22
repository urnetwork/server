# CPUOPT8 — typed score pages, and contract stats only for listeners (2026-11-22)

On 2026-11-22 at about 07:40Z the owner decided on two of CPUOPT7's larger levers
(CPUOPT7 §6, items 3 and 4): "Build now, ship after CPUOPT7". This round implements and tests them.
Nothing is merged or deployed. Shipping waits for CPUOPT7's canary results and the
owner's go (§5, §6).

**Owner rule (binding, as in CPUOPT4–7):** "We can't sacrifice correctness of performance
timings for the cpu savings." Both changes keep every answer:
- a typed score page decodes to exactly the value a gob encode and decode of the same
  scores gives;
- a contract stats listener gets exactly the reports it did, and the getters answer as
  before.

## Summary

- **Typed score pages** (server, §2): a binary page format beside gob, read without
  reflection into a few page-sized slabs, with equal maps of a page decoded to one map.
  - **Real pages** (the largest sampled from Main, 168–190 providers): a decode takes
    0.15 ms, 268 KB and 666 allocations, where gob takes 1.29 ms, 1,275 KB and 18,823.
  - **Smaller:** pages are 41% smaller, and a cached page retains 266 KB of heap where
    gob's retains 741 KB, so the 64 MiB page cache holds about 2.6 times the pages.
  - **Compatible:** readers accept both formats. The export writes gob until
    `typed_score_pages` is set in provider.yml, which waits until every reader runs this
    build (§5).
- **Contract stats only for listeners** (sdk, §3): a device's contract tracker built its
  stats and per-contract details on every emit, though a hosted proxy device has no
  listener. It now builds only the reports that have one. An emit without listeners
  allocated 416 objects for 40 contracts and now allocates none.
- **Expected saving** (§4): about 0.7–1.1 cores on crisp and 1.0–1.6 on fireside, 1.7–2.7
  of the fleet's 44 proxy cores, on top of CPUOPT7's. Most of it is collector work at the
  memory limit.
  - **Not counted:** fewer page cache misses from the larger effective cache, about one
    taskworker core from faster page encodes, the api's decodes, and 41% fewer page bytes
    in Redis.
- **Rollout** (§5): readers first. Every reader (api, alt, proxy, mcp, taskworker) runs
  this build with the flag off; then the flag goes on with a config release and a
  taskworker restart. Turning it off is the rollback, effective at the next export pass.
- **Branches**, not merged:
  - server `perf/cpuopt8-score-codec-1122` on main `45a8de9c6`;
  - sdk `perf/cpuopt8-contract-stats-1122` (`0d4dbff6`) on CPUOPT7's sdk merge
    `bced97ce`.

  Both apply cleanly to CPUOPT7's hotfix pins (§5).

## 1. Who reads and writes score pages

From the symbols linked into each service binary built from this branch (`go tool nm`; the
linker drops functions a binary cannot reach):

| Binary | Reads score pages | Writes score pages |
|---|---|---|
| api, alt, proxy, mcp | yes | no |
| taskworker | yes | yes: the score export, about every five minutes |
| all (an operator's one-process build) | yes | yes |
| bringyourctl | no: filter documents only | no |
| connect, gossip, monitor, egress-prober, competition workers | no | no |

- **Decodes now** (Mimir, the hour to 07:30Z):
  - the proxy: 84/s on crisp, 70/s on fireside;
  - api: 79/s across its processes;
  - taskworker: 0.9/s;
  - mcp: none recorded, and no alt series.
- **What does not change:** the count, filter and partition documents and the native
  manifests stay gob in both formats. The monitor reads only count documents.

## 2. Typed score pages (server)

### 2.1 The format (`model/network_client_score_page_codec.go`)

```
page   = magic version uvarint(records) uvarint(scores) uvarint(ids)
         uvarint(times) uvarint(strings) string* score{records}
string = uvarint(length) bytes
score  = uint32le(fields) value* , one value per set field, in bit order
```

- **Detection:** a typed page starts with `0xc5`, then the version (1). No gob stream
  starts with `0xc5`: gob opens with the byte length of its first message, which is a byte
  below `0x80` or a negated byte count, `0xf8`–`0xff`. Every other page is read as gob.
- **Fields:** each of ClientScore's 28 exported fields has a bit, set exactly when gob
  sends the field.
- **Values:**
  - times go through `AppendBinary` and `UnmarshalBinary`, as gob's do;
  - floats keep their bits;
  - ints are varints and ids are 16 bytes;
  - strings index the page's string table;
  - maps are written in key order, and lookback scores nest.
- **Header counts:** they size the slabs, and each is bounded by the bytes left, so a
  malformed page cannot allocate past its length.

### 2.2 Exactly gob's value

A typed page decodes to the value a gob encode and decode of the same scores gives, in
every case gob distinguishes:
- nil and empty maps stay apart;
- a set time pointer survives even when it points to the zero time;
- a zero time field stays zero, while a zoned zero instant is kept;
- a set location id pointer survives even to the zero id;
- negative zero becomes zero, NaN payloads are kept, and a float32 map value goes through
  float64 as gob's does;
- unexported fields are not written;
- an empty page decodes to a nil slice.

**The time pointer case was found by the tests.** Gob reaches `*time.Time`'s `GobEncode`
without dereferencing the pointer, so it sends every set pointer. The first draft dropped a
pointer to the zero time, and `TestClientScorePageCodecEdgeValues` failed on it.

### 2.3 Where the allocations go

- **Slabs:** four allocations per page hold all its records:
  - the scores with their lookback scores;
  - the location ids;
  - the validity instants;
  - the record pointers.

  One more string holds the page's map keys and names.
- **Shared maps:** equal maps of one page decode to one map. Within a page, equal maps
  have equal encodings, because the writer orders the keys. On the real pages:
  - Scores and Tiers maps hold 1–8 distinct values among about 680 each;
  - PassesMinimums holds one;
  - ScaledWeights holds 39–144 among about 170.
- **Why sharing changes no answer:** a decoded page is cached and shared with every reader,
  and no reader writes a cached map. Each reader replaces a map with a changed clone
  (`filterPool`) and copies the scores it keeps (`mergeClientScorePage`). The cache
  comments now say so.
  - `TestClientScorePageCacheReadersCannotChangeCachedTypedPages` runs discovery in every
    mode that changes scores over typed pages whose maps are shared. Every cached page
    still equals a fresh decode of its bytes.
  - Two decodes never share a map.
- **Decoder scratch:** decoders are pooled. A decode allocates what it returns plus a short
  key per distinct map.
- **Cache size estimate:** a typed decode estimates the heap it retains, from the slab and
  map sizes, for the page cache's 64 MiB bound. On the real pages it is within 3% of the
  measured heap (260 vs 266 KB), while gob's estimate of six times the page bytes
  undercounts by 10% (671 vs 741 KB).

### 2.4 The writer and the flag

- **The flag:** `EgressIndexSettings.TypedScorePages`, `typed_score_pages` in the
  `egress_index` block of provider.yml, default false. UpdateClientScores reads it once at
  the start of each pass and logs `client score pages are gob|typed`.
- **What it governs:** only the score pages. A page the typed format cannot hold is
  written as gob, the bytes the export writes today:
  - a nil score or lookback, which gob refuses too;
  - lookbacks nested past 8;
  - a time `AppendBinary` refuses.
- **Old binaries:** a binary that predates the key ignores it. An unknown key in the block
  leaves the rest of the block decoded, so the config line is harmless to old services.
- **Metrics:**
  - `urnetwork_client_score_page_decodes_total{format,result}` on every reader;
  - `urnetwork_client_score_page_encodes_total{format}` on the writer, with `format` one
    of gob, typed or gob_fallback.
- **Dashboards:** panels on the api (page cache events), proxy and taskworker dashboards.
  They go live with a grafana deploy, which the roll does not need: §5 gives the queries.

### 2.5 Benchmarks

On a laptop (Apple M1 Max), warm caches, medians of three runs.

**Real pages.** 20 baseline native pages were read from Main's Redis read-only (GET and
HGET through the data tunnel): quality mode, the five countries with the most providers.
They are kept outside the repository, because they hold provider ids. All 20 decode
identically through both formats. The 7 largest (168–190 providers, three lookbacks each;
112 KB gob, 67 KB typed on average):

| Decode of one page | ns/op | B/op | allocs/op |
|---|---|---|---|
| gob | 1,290,000 | 1,275,100 | 18,823 |
| typed | 149,000 | 267,600 | 666 |
| change | 8.7× faster | −79% | −96.5% |

**Committed benchmarks** (`BenchmarkClientScorePage*`): a generated 200-provider page shaped
like the real ones, 131 KB gob and 76 KB typed.

| One page | gob | typed | change |
|---|---|---|---|
| decode | 1.42 ms, 1,447 KB, 21,328 allocs | 0.19 ms, 334 KB, 1,141 allocs | 7.6× faster, −77% bytes |
| encode | 1.06 ms, 937 KB, 9,150 allocs | 0.24 ms, 82 KB, 1 alloc | 4.5× faster, −91% bytes |

The generated page has more distinct ScaledWeights than the real ones, so it shares fewer
maps.

**Retained heap**, 64 decodes of the real pages kept alive, after a collection:

| Format | page bytes | retained per page | the cache's estimate |
|---|---|---|---|
| gob | 111,909 | 740,977 | 671,454 |
| typed | 66,716 | 266,387 | 260,403 |

### 2.6 Tests (`model/network_client_score_page_codec_test.go`, `..._db_test.go`)

- **Against gob:**
  - `TestClientScorePageCodecMatchesGob`: 3,000 seeded pages. Every exported field is
    drawn by reflection, with edge values and nested lookbacks, and checked against gob's
    round trip with floats compared by their bits.
  - `TestClientScorePageCodecEdgeValues`: each case gob distinguishes, one by one.
- **Field guard:** `TestClientScorePageCodecCoversEveryField` fails when ClientScore gains
  an exported field the codec does not write. The generator also fails on a field type it
  does not know.
- **A new reader reads old pages:** `TestClientScorePageReaderReadsGobPages`. Gob pages
  from the export's gob path decode exactly as the old reader's plain gob decode, directly
  and through the page cache.
- **An old reader is never handed new pages while the flag is off:**
  - `TestClientScorePageExportWritesGobUnlessTypedScorePages` covers the default, an absent
    provider.yml, `false`, and an unreadable value. Every union, facet, native and
    capability page the export writes is gob and reads with a plain gob decode. Set,
    every page is typed and reads as the same scores, and the counters count each page.
  - `TestUpdateClientScoresWritesTypedPagesOnlyWithTypedScorePages` (DB) runs one real
    export over the database: gob pages, union and native, all readable by the old
    reader. With the flag, the next export writes every page typed, the old reader refuses
    each, and the readers load the same providers.
- **An old reader handed a typed page fails it** with an error, never as other scores:
  `TestClientScorePageOldReaderFailsTypedPages`.
- **Fallback:** `TestClientScorePageTypedFallsBackToGob` (nil scores, nil lookbacks, an
  unencodable time, depth).
- **Malformed pages:** `TestClientScorePageCodecRejectsMalformedPages` covers every
  truncation, every single changed byte, another version, unknown field bits, huge counts
  and trailing bytes. `FuzzClientScorePageDecode` checks no panic and a stable re-encode:
  two local runs of 60 s and 30 s (35,057 executions in the second), no failure.
- **Allocations and heap:**
  - `TestClientScorePageTypedDecodeAllocations` fails on the gob decoder.
  - `TestClientScorePageTypedSizeEstimate` checks the estimate against the measured heap,
    within a quarter.
- **Concurrent readers:** `TestClientScorePageCacheConcurrentReadersOfTypedPages`, run
  with the race detector too. Eight readers decode through the pooled decoders with
  evictions and change their copies, and every page still decodes to its scores.
- **Page cache (DB), over typed pages:**
  - `TestClientScorePageCacheReadersCannotChangeCachedTypedPages`;
  - `TestClientScorePageCacheTypedPagesKeepLoadedScoresAndAnswers`: the same scores
    published gob and then typed load equal and give the same answer and bands, and one
    request reads a gob target and a typed target together.

## 3. Contract stats only for listeners (sdk)

### 3.1 Where they are built

- **connect** builds a contract's stats events only when a callback is registered
  (`ContractManager.emitContractStats`).
- **The multi-client** registers its window's forwarder on every window client.
- **The sdk's DeviceLocal** registers on the multi-client, and on the provider, for every
  device. Its getters build from the tracker's state, which the events feed:
  `GetEgressContractStats` and the details getters, which the device RPC serves to a
  remote too.
  - The tracker's bit rates come from the event arrival times. So the events must keep
    flowing for a later getter or listener to get the same answer, and connect's part
    stays.
- **The tracker's emit** built all four reports every time, whether or not anything
  listened: egress and ingress stats, and a details entry per contract with its ids and
  transfer path. The device RPC subscribes on the device only while a remote holds a
  listener, so a hosted proxy device normally has none.
- **Its cost** (CPUOPT7's captures, mean of three processes):

  | Share of a process | crisp | fireside |
  |---|---|---|
  | the tracker's emit | 1.71% | 3.33% |
  | of which `encodeUuid` | 1.02% | 1.98% |
  | of which GC assists | 0.93% | 2.52% |

  CPUOPT7's `encodeUuid` change removes about three quarters of the `encodeUuid` share.

### 3.2 The change (`sdk/device_local.go`)

- **The listened reports:** `update` and `flushPending` take the reports that have a
  listener (`deviceContractReports`), read from the listener lists before stateLock is
  taken. `emit` builds only those, and the device dispatches each one it built.
- **The lifecycle:** the tombstone, the eviction, the gate and the settle loop advance
  exactly as before, whichever reports are built.
- **Deterministic tests:** `updateAt` and `flushPendingAt` take the time.

### 3.3 Why the answers are the same

- **The state:** every update folds its events into the contract state and every emit
  advances the lifecycle, so the state is the same with and without listeners. The
  getters, and a listener added later, read that state.
- **A listener added during an emit:** a listener added after the read gets the next emit,
  as one added just after a dispatch always has.
- **The emit's own work:** the reports only read the state, so skipping them changes
  nothing else.

### 3.4 Tests (`sdk/device_local_contract_reports_test.go`)

- **`TestDeviceContractTrackerBuildsOnlyListenedReports`:** three trackers take 4,000
  seeded steps in virtual time: opens, progress, closes, replays, gated batches, flushes
  and clears.
  - One tracker builds every report, as the tracker did before; one builds none; one
    builds a random subset each step.
  - After every step, their state, lifecycle and getters agree. The subset tracker's
    reports are exactly the full tracker's, and the tracker without listeners builds
    nothing.
- **Allocations,** both failing on the old behavior:
  - `TestDeviceContractTrackerWithoutListenersBuildsNoReports`: 0 allocations, against 416
    for 40 contracts before;
  - `TestDeviceLocalContractStatsBuildsOnlyForListeners`: the device callback, 0 against
    210 for 20 contracts. A details listener gets the getter's details, and nothing is
    built again once it leaves.
- **The existing sequence and block tests** pass every report, as before.
- **Applies cleanly to the deployed pin** `efbbafcc` (sdk `35fd1127` with CPUOPT7's
  `encodeUuid`), and its tests pass there.

## 4. Expected savings per host

The proxy hosts run about 22 proxy cores each (22.1 and 22.0, the hour to 07:30Z).

| Change | crisp | fireside | How |
|---|---|---|---|
| typed pages: decode work | 0.1–0.17 | 0.12–0.2 | the non-assist decode share (0.89% / 1.05%), 87% removed, between today's decode rate and CPUOPT7's window B |
| typed pages: collector | 0.37–0.57 | 0.46–0.73 | 0.38 MB less allocation per decode, at 7–14 decodes a second a process: 4–6% of the allocation, times the GC share (41% / 53%) |
| contract stats for listeners | 0.2–0.35 | 0.4–0.7 | the emit's remaining non-assist work (0.4% / 0.5%), plus its allocation share after CPUOPT7 (about 3% / 5%) times the GC share |
| **Total** | **0.7–1.1** | **1.0–1.6** | **1.7–2.7 of the fleet's 44 proxy cores** |

- **The collector estimates** assume, as CPUOPT7 measured, that at the memory limit
  collections scale with allocation bytes.
- **The ranges:** the typed-page rows run from today's decode rate (7–8 a second a
  process) to CPUOPT7's window B (12–14). The contract stats row's low end takes half of
  its collector estimate.
- **The page allocation:** real pages allocate 11.4 bytes per gob byte with gob and 21% of
  that typed. At today's 42 KB average cached page, that saves 0.38 MB per decode.
- **The contract stats share:** its allocation share is estimated from its share of GC
  assists, which likely undercounts.
- **Not counted:**
  - **The page cache.** Today it holds about 265 pages a process, and every miss evicts
    (evictions 83/s and 70/s, equal to the misses). Typed pages fit about 2.6 times as many
    in the same 64 MiB, so misses (14% of lookups) should fall. Each avoided miss also
    saves its Redis page read.
  - **The taskworker.** The export's encode phase averages 1.3 s of wall time a second
    today, and page encodes run 4.5 times faster with 91% less allocation: up to about one
    core there.
  - **The api's** 79 decodes a second, and 41% fewer score page bytes written to and read
    from Redis.
- **When each saving starts:** the contract stats saving arrives with the proxy roll. The
  typed pages saving arrives when the flag goes on (§5).

## 5. Rollout: readers first, then the flag

1. **Land after CPUOPT7.**
   - **Main:** merge the server and sdk branches with `--no-ff` once CPUOPT7's land, and
     run the merged-tree tests.
   - **Hotfix pins:** cherry-pick onto CPUOPT7's hotfix pins. All three apply cleanly:
     server `a4225d5aa` + `50a93973e`, sdk `efbbafcc` + `0d4dbff6`, connect unchanged
     (`91f1d695`). On that pin set, the codec's unit, DB and page cache tests pass (29),
     and so do the sdk's contract stats tests.
2. **Every reader takes the build with the flag off.** Pages stay gob.
   - **Which:** the proxy (§6), api, alt (it mounts the api router), mcp and the
     taskworker, in any order. All must finish.
   - **The contract stats saving** starts with the proxy blocks.
3. **Check every reader.**
   - **Series:** `count by (service) (urnetwork_client_score_page_decodes_total)` has
     series from every api, alt, proxy, mcp and taskworker process. A process from before
     has none.
   - **Versions:** the version guard shows every block on the new build.
   - **Optional:** sample today's pages read-only again, as in §2.5, and check they decode
     identically through both formats.
4. **Turn the flag on.**
   - **The config change:** add `typed_score_pages: true` to the `egress_index` line of
     `config/main/provider.yml`, ship a config release, and recreate the taskworker
     containers. A pass reads the flag at its start. Other services ignore it.
   - **What changes:** every page is republished about every five minutes, so within a pass
     or two every reader reads typed pages.
5. **Check:**
   - `sum by (format) (rate(urnetwork_client_score_page_encodes_total[5m]))` shows typed and
     no gob_fallback;
   - `sum by (service, format, result) (rate(urnetwork_client_score_page_decodes_total[5m]))`
     shows typed ok decodes on every reader, no typed errors, and gob decodes falling to
     zero;
   - on the proxy hosts, allocation per process falls by about 3–5 MB/s and the page
     decode share by about 85% (§6). The page cache now decodes through
     `decodeClientScorePageBytes`, which replaces `decodeClientScoreSample` in the
     stacks CPUOPT7 measured.
6. **Rollback.**
   - **The flag:** `typed_score_pages: false` with a config release and a taskworker
     restart. The next pass writes gob, and readers keep reading both.
   - **A reader rollback below this build** waits until the flag is off and a whole pass
     has finished, and its typed decodes read zero. Any typed page left unreferenced
     expires with the export's 5 h ttl.
7. **Later** (no hidden settings): after a week on, make typed the code default and drop
   the key. A later release can drop the gob page writer. Readers keep reading gob for at
   least the ttl after the last gob writer.

## 6. Proxy roll plan

Nothing is deployed. A proxy roll needs the owner's go, after CPUOPT7's canary results, and
then the roll script's canary and validator gates (memory notes proxy-roll-canary-checks
and validator-deploy-gate).

- **Build:** a proxy image only, as in CPUOPT7 §7. Two choices:
  - **(A) Hotfix pins:** CPUOPT7's pins plus these commits (§5 step 1).
  - **(B) Main:** main after both rounds merge.
  - **Either way:** the same build gates (a private version home, the registry guard,
    govulncheck, the stamps, the `-trimpath` audit rebuild, the dependency audit).
  - **Rollback target:** the CPUOPT7 proxy build.
- **Order:**
  1. g3 as the canary with a 30-minute hold.
  2. The WireGuard canary: check WireGuard packets by block first, as WireGuard moves
     between blocks (g6 on 11-21).
  3. The other eight blocks, with a half-fleet check after the fifth.
- **Gates per block, as CPUOPT7:**
  - **Validator idle:** finalized or applied, at least 900 s to the boundary and 600 s past
    it, and no `attempt-artifact` POSTs for 180 s. A canary needs its hold + 600 s.
  - **Health:**
    - 503/s and api latency;
    - PgBouncer waiting under 1,000;
    - edge-2 busy without iowait;
    - descendant walks at most 20, failing closed;
    - at least 12 GB available on each host.
  - **Timing:** `SETTLE=360` before after-readings; outside 17:30–19:30Z.
  - **Version guard:** every block on the CPUOPT7 build before its deploy.
- **Judge each block at 60 minutes, against the other blocks on the same host:**
  - **The direct check:** a perf capture of one rolled and one old process on each host,
    30 s at 99 Hz, under the validator gate. `deviceContractTracker).emit` should fall from
    1.7% / 3.3% (less after CPUOPT7's `encodeUuid`) to near zero, since hosted devices
    have no contract listener.
  - **Mimir:** allocation MB/s and GC a minute per process; proxy cores per admission and
    per window flow.
  - **Service:** FindProviders2 load p95, proxy-test setup p50 and p90, and mint refusals.
- **The flag is not a per-block step.** It comes after every reader runs the build (§5),
  and it is judged fleet-wide with a five-minute rollback:
  - typed decode errors, zero on every reader;
  - the same Mimir signals;
  - a perf capture where the page decode (`decodeClientScorePageBytes`) should fall to
    about an eighth of CPUOPT7's `decodeClientScoreSample` share.
- **Revert:**
  - a canary failure reverts the rolled blocks to the CPUOPT7 build;
  - one regressed block reverts that block and stops the roll;
  - while typed pages are being written, the flag goes off before any reader reverts
    (§5 step 6).

## 7. Tests run

- **The layout:** each repository a worktree under `temp/cpuopt8`:
  - server on this branch;
  - sdk on its branch;
  - connect at CPUOPT7's merge `0709a55a`;
  - sn pinned at `ed921902`.

  sn's 09:18Z `golang.org/x` bump needs a server go.mod bump of its own, which is not part
  of this round.
- **DB tests** ran through test-env.sh, after confirming that local-pg and local-redis
  resolve to 10.213.0.1 and both containers are healthy. No `./monitor/` tests were run
  (planetoid runs the monitor).
- **sdk:**
  - the full suite (`go test ./...`): 7 packages ok, the root package in 861 s;
  - the new tests under the race detector;
  - on the deployed pin `efbbafcc` with this change: the contract stats, block and
    `encodeUuid` tests pass.
  - The new allocation tests fail with the old build-every-report emit (416 and 210
    allocations).
- **server:**
  - **Build and vet:** `go build ./...`; `go vet` of `.`, session, model, localclient,
    proxy, api, connect, controller, bringyourctl, taskworker, alt and mcp.
  - **`./grafana`** with `URNETWORK_PROMTOOL` (promtool 3.5.0) in a pure env: ok. Its
    application metric coverage test failed on the two new counters until the dashboard
    commit.
  - **`TestBuildsForDarwin`:** pass.
  - **The codec's unit tests:** pass, and the concurrency ones under the race detector.
    The fuzz target ran 60 s and 30 s.
  - **The codec and page cache DB tests:** pass.
  - **The proxy package (DB)**, with this sdk as its sibling, so its own tests ran the
    listener-aware tracker: ok in 914 s, 330 passed, 0 failed.
  - **localclient (DB):** ok, 52 passed.
  - **taskworker/work (DB):** 1,051 passed, 7 skipped (environment-gated), 1 failed:
    `TestContractCreationSameNetworkLargeNContentionFree`, a throughput bound, at a load
    average near 50. All 512 creations completed. Rerun alone, as test.sh's admission
    gate runs it (`-p=1 -race`), it passes in 15 s.
  - **The full model package (DB),** sharded four ways by name on commit `50a93973e`:
    4,744 passed, 11 skipped, 11 failed. Every failure is listed below; none touches score
    pages.
  - **On CPUOPT7's hotfix pins** (server `a4225d5aa`, connect `91f1d695`, sdk `efbbafcc`,
    each with this round's commit): the codec's unit, DB and page cache tests, 29 passed.
- **The model package's 11 failures are not this round's:**
  - **Seven fail the same way on main `45a8de9c6`,** run in a worktree layout like this
    one:
    - `TestModelSourcesDoNotReferenceRetiredLegacySettlementQueue`: a source scan finds
      `legacy_settlement_intent` in `network_deletion_cleanup.go`;
    - `TestEveryApiKeyWriteOwesTheSecurityNotice`: account_api_key writes in
      `auth_verify_claim.go` and `network_deletion_cleanup_testing.go`;
    - `TestProductUpdatesHonorMarketingSuppression` and
      `TestSuppressNetworksMarketingInTx`;
    - `TestLoadSubscriptionMetricsSnapshotForecastsNewRevenue`;
    - `TestParticipantSweepTimestampUsesUTC`: mismatched param and argument count;
    - `TestUnresolvedPredicateStatisticsReturnThePayerReadsToTheirPartialIndex`.
  - **Four are the native-fee public tests,** whose verifier builds only on Linux. test.sh
    skips them by name on this Mac.

## 8. Not done

1. **connect's event build** (0.7–1.2% of a process, besides the tracker): it stays.
   - **Why:** the sdk's getters, which the device RPC also serves, need the tracker's state,
     and its bit rates come from the event arrival times. A lazy pull would change a later
     reader's answer.
   - **An option for later:** allocating a pass's events together would cut allocations
     without changing an answer.
2. **Baking in the flag:** a week after it goes on (§5 step 7).
3. **The page cache bound:** unchanged at 64 MiB. Typed pages already fit about 2.6 times
   as many. A larger bound waits for the memory headroom of the two new hosts (CPUOPT7 §6
   item 2).
4. **A typed format for the counts, filter and partition documents and the manifests:**
   they are small, and a typed format would not pay for a second migration.
