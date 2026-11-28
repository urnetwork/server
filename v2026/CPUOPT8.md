# CPUOPT8 — typed score pages, and contract stats only for listeners (2026-11-22)

On 2026-11-22 at about 07:40Z the owner decided on two of CPUOPT7's larger levers
(CPUOPT7 §6, items 3 and 4): "Build now, ship after CPUOPT7". This round implements and tests them.
Both were merged on 2026-11-22 (server `7675df569`, sdk `c82b7831`) and deployed on 2026-11-23 with
the owner's go to accelerate every deployment. §9 records the build, the rolls, the flag and the
measured result.

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
- **Merged** (§9.1):
  - server `perf/cpuopt8-score-codec-1122` as main `7675df569`, on `45a8de9c6`;
  - sdk `perf/cpuopt8-contract-stats-1122` (`0d4dbff6`) as main `c82b7831`, on CPUOPT7's
    sdk merge `bced97ce`.

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
  - sn at `ed921902`, since sn's 09:18Z `golang.org/x` bump needed a server go.mod bump of
    its own.

  That bump has since landed: server `7c12354fd` (go mod tidy, 11-22) raises the
  `golang.org/x` modules to the minimums sn `9a2dbc10` requires. The deployed build compiled
  sn main `71b515b1`, so nothing is pinned at `ed921902` any more (§9.2).
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

## 9. Merge, deploy and the flag (owner, about 01:25Z 11-23: accelerate every deployment)

### 9.1 Merges

- **server main `7675df569`** (11-22, 15:43Z): the `--no-ff` merge of `perf/cpuopt8-score-codec-1122`.
- **sdk main `c82b7831`** (11-22, 15:42Z): the `--no-ff` merge of `perf/cpuopt8-contract-stats-1122` (`0d4dbff6`) on
  `bced97ce`.
- **What shipped with them:** server main `b896cf6d9` (11-23 01:54Z) adds the stuck provider bake-in, and `7c12354fd`
  (go mod tidy) raises the `golang.org/x` modules to sn's minimums (§7). The connect build in these binaries is
  connect main `1ee839f8`, whose extender dialer changes are inert in server processes.

### 9.2 The images

- **Source:** server `b896cf6d9`, with every sibling at its origin head: connect `1ee839f8`, sdk `c82b7831`, sn
  `71b515b1`, proxy `e46247dc`, quic-go `83a1e6f8`, glog, goidenticons, userwireguard and gvisor unchanged. Built
  02:01–02:21Z 11-23.

  | Service | Version | Digest |
  |---|---|---|
  | api | `2026.11.22-outerwerld-1105990880` | `sha256:13d91e74…` |
  | taskworker | `2026.11.22-outerwerld-1105994420` | `sha256:64f1bc3d…` |
  | proxy | `2026.11.22-outerwerld-1105997020` | `sha256:310ede9d…` |
  | mcp | `2026.11.22-outerwerld-1105998450` | `sha256:b16232e5…` |

- **Gates:**
  - a private warpctl and an empty version home; the registry guard (each tag absent before its push);
  - govulncheck on amd64 and arm64: 0 affecting;
  - the pushed digests equal the registry's;
  - the stamps: `b896cf6d9`, `vcs.modified=false`, go1.27.4;
  - the `-trimpath` audit rebuild: MATCH for all four services on both arches;
  - the migration head: 1008 in the code and on Main, `db_migrations.go` unchanged.
- **Replaced:** the builder's `2026.11.22-1105365180`, which every block ran since a fleet-wide deploy on 11-22. It was
  built from server `ff9c9f996`, a release commit on `45a8de9c6` (CPUOPT7). Its digests were the rollback targets, and
  each was unchanged before and after the builds.
- **Readers:** alt was retired on 11-20, so the score page readers are the api, the proxy, mcp and the taskworker.

### 9.3 The rolls

Every roll ran under the validator-idle gate (no bypass), the health gates, the version guard (each block on the
builder's 1105365180, the target newer) and the services lock, with Main reads through a 20 s statement timeout. The
readers rolled first and the flag stayed off.

**api, 02:29–02:50Z, four waves** (beta and g1, then g2, g3 and g4).
- Two waves held at their gate first: 65 s for a PgBouncer waiting spike (115) before the first wave, and 131 s for a
  117 s transaction on Main before the second. Both cleared on their own; nothing had been deployed into either.
- Each wave drained in about 60 s and passed readiness, `/sn/head` per block, the rotate-exit smoke (8 of 8) and the
  OAuth regression probe (8 of 8).
- After the last wave: `/sn/head` through the front 20 of 20, and the 10-minute 5xx watch passed (503s at 0, api mean
  12.7 ms). Over the 40 minutes the api returned 4 server errors in all.
- Score pages: all 20 processes decode, gob only, about 130 a second at 0 errors; the stuck provider signal reports
  from every process with no runtime control.

**taskworker, g1 at 03:03Z, g2 at 03:40Z** (the payout gate: nothing claimed, no account payment pending, the award
not due within the block's 25 minutes).
- g1 drained in about 105 s and its four processes were ready at once. Over its 10-minute watch, execution errors
  stayed at or under 0.04 a second, from `SyncProductUpdatesForUser` on the old g2, as in the two hours before; edge-2
  rose to 0.68 with the new processes' start, under its 0.85 limit.
- The score export on the new build wrote gob pages, as the flag off requires: about 170 page encodes a second over
  the pass, `gob_fallback` 0, and the pass lines read "client score pages are gob".
- A taskworker start seeds the operator block points award to run 15 minutes later
  (`operatorBlockPointsStartupDelay`), so g2 waited at the payout gate until that run ended at 03:18:30Z. It found no
  block due and rescheduled itself to 12:00Z.
- g2 drained in about 135 s, and its watch was clean.

**mcp, 03:53–04:07Z, four waves** (beta and g1, then g2, g3 and g4). Each wave drained in 45–60 s with its four processes
ready and no readiness lines; the 5-minute watch after the last passed, with no mcp server errors.

With those three, every score page reader but the proxy reported the decode counter: the api 20 of 20 processes, mcp
20 of 20 and the taskworker 8 of 8, gob only and no errors.

**proxy: the g3 canary passed (deploy 04:54:57Z, hold to 05:30:28Z, verdict at 05:30:58Z).** It ran under the
validator gate (intent 26510 finalized, 3,696 s to the boundary), the health, payout and memory gates and the proxy
lane lock. The verdict is CPUOPT7's regression check, unchanged:

| Per process, g3 against its 18 peers | 30 min before the deploy | Last 10 min of the hold (36 min after) |
|---|---|---|
| cores | 2.36 (peers 2.24) | 2.05 (peers 2.30) |
| GC a minute | 7.99 (peers 7.78) | 4.27 (peers 8.54) |
| heap+stacks | 7.05 GB (peers 7.04) | 6.81 GB (peers 7.10) |
| session-check unchecked share | 0.045% (peers 0.038%) | 0.012% (peers 0.042%) |

- As in CPUOPT7, a restarted process holds fewer devices for about an hour, so part of this is restart relief.
- The safety rules passed: WireGuard 75 → 147 packets a second with sessions 12 → 13; active devices against the
  other blocks 0.95 (stop below 0.8); user bytes 0.93 (stop below 0.6); edge-2's 15-minute busy CPU without iowait
  0.466 → 0.497 (limit +0.05).
- The per-host comparison, crisp g3 against its peers: 0.23 mint refusals a second against 4.40, 2.03 cores against
  2.28 and 3.0 collections a minute against 6.2.

**The g6 WireGuard canary passed (deploy 06:07:00Z, hold to 06:31:56Z).**
- Active devices against the other blocks 1.02, user bytes 1.20; per process 2.21 cores against 2.32 and 7.9
  collections a minute against 9.4. edge-2's 15-minute average: 0.506 → 0.543.
- WireGuard: g6 kept its 11 sessions, but its packets fell from about 125 to 2 a second after the restart. The rule
  reverts only when the sessions fall too. The fleet's total held at about 1,740 packets a second through 06:15Z, with
  g2, g7 and g8 gaining, so the traffic moved to other blocks. g3, on the same build, gained WireGuard traffic.

**Three stops on edge-2, all judged shared load; each resumed with a fresh baseline and every other guard unchanged.**
1. **Before g1, 06:32:30Z:** the health gate read edge-2's 5-minute busy CPU at 0.551 against a limit of 0.509, set from
   the 0.459 read when the roll started at 04:13Z. Main carried load the roll didn't cause: a zero-byte contract cleanup
   (1,000 rows a second from 01:43Z, 2,000 from 06:43Z), a large export and autovacuum. A connect roll by another
   lane came next (06:50–06:56Z), so the proxy roll waited for it, then resumed at 07:02Z with a fresh baseline (0.588,
   limit 0.638).
2. **After g4, 08:04:26Z (0.494 → 0.635):** g4's state load came at 07:51–07:55 (74–83 acquired connections) and was over
   by 07:57, but edge-2 kept reading 0.62–0.72 through 08:03. The rise matched the end of the connect drain: the old
   connect processes exited at 07:49–07:55Z, api requests rose from 6.9k to 7.7k a second, and edge-2's user CPU from
   0.33–0.37 to 0.46–0.53. g1 and g2 on the same build read 0.557 → 0.471 and 0.453 → 0.469. Resumed at 08:09Z with a
   fresh baseline (0.593, limit 0.643).
3. **After g5, 08:45:55Z (0.507 → 0.641):** g5's state load came at 08:34–08:38 (62–79 acquired connections), with edge-2
   at 0.56–0.60, below the 0.63 it read at 08:20 before g5. The rise to 0.70 came at 08:40–08:44, after the load, with
   bot sweeps rising from 436 to 489 a second; edge-2 read 0.40 at 08:46. Resumed at 08:46Z (baseline 0.636, limit
   0.686).

**The half-fleet check after g4** was read by hand, the stop having come first: proxy-test setup p50 8.11 s against
8.55 s before g6 (revert above 12.8 s), the success share 0.733 against 0.813 (revert below 0.563), and the rolled
blocks' active devices against the old blocks' 1.034, against 0.973 before g6.

**The other blocks.** Each passed the validator, health and payout gates and the version guard, then settled 600 s.
Per process about 15 minutes after its deploy, so the readings include restart relief:

| Block | Deployed | Cores (other blocks) | GC a minute (other blocks) | Heap+stacks GB (other blocks) | edge-2 5m before → after | WireGuard median packets/s, sessions |
|---|---|---|---|---|---|---|
| g1 | 07:19:23Z | 2.06 (2.33) | 6.1 (9.7) | 6.98 (7.14) | 0.557 → 0.471 | 53 → 25, 8 → 8 |
| g2 | 07:34:32Z | 1.99 (2.22) | 7.4 (8.8) | 7.06 (7.16) | 0.453 → 0.469 | 379 → 195, 17 → 16 |
| g4 | 07:49:52Z | 2.33 (2.30) | 9.8 (8.9) | 7.04 (7.08) | 0.494 → 0.635 (stop 2) | 145 → 28, 14 → 19 (read at 08:05) |
| g5 | 08:31:22Z | 2.27 (2.30) | 8.9 (8.8) | 7.16 (7.11) | 0.507 → 0.641 (stop 3) | 194 → 135, 11 → 13 (read at 08:47) |
| g7 | 08:46:51Z | 2.35 (2.30) | 7.4 (9.0) | 7.11 (7.13) | 0.622 → 0.536 | 370 → 81, 13 → 9 |
| g8 | 09:01:57Z | 2.34 (2.30) | 8.2 (9.6) | 6.99 (7.18) | 0.539 → 0.486 | 343 → 124, 18 → 14 |
| g9 | 09:17:03Z | 2.22 (2.32) | 7.9 (8.7) | 6.84 (7.08) | 0.499 → 0.543 | 128 → 87, 15 → 19 |
| g10 | 09:43:04Z | 2.25 (2.30) | 8.6 (9.5) | 7.10 (7.15) | 0.628 → 0.475 | 135 → 33, 17 → 12 |

- Every block's WireGuard resumed above the rule's 10% of its median, with sessions above half. g6's traffic, which had
  moved to other blocks after its restart, was back at 154 packets a second and 14 sessions by 08:05Z.
- The roll ended at 09:58Z with the fleet's unchecked share at 0.0014 (the alert warns above 0.05). Both proxy hosts stayed
  at 99% busy throughout, so the collector runs whatever CPU it is given: per-process cores barely move, and GC a
  minute and allocation are the readings that show the saving.

### 9.4 The flag

The step began once every reader ran its D71 build (warpctl: api 5 of 5 blocks, proxy 10 of 10, mcp 5 of 5, taskworker
2 of 2), every process reported the decode counter (api, proxy and mcp 20 of 20, the taskworker 8 of 8), and no reader
had a decode error.

- **Config:** `typed_score_pages: true` added to the `egress_index` line of `main/provider.yml` and nothing else, merged
  with `--no-ff` and pushed at 09:59Z. The config-updater built from it, `2026.11.23-outerwerld-1106279920`, kept
  the running release's warp, so only that line changed. The server's config and egress-index settings tests passed on
  it (15). It was deployed at 10:13:31Z under the validator gate, and all six hosts published it within 85 s, every file
  hash as built.
- **The writer:** in place of a plain restart, the taskworker rolled to `2026.11.23-outerwerld-1106178960`
  (`sha256:097f9452…`). It is built from server `81287a91a` with every D71 gate and audit, and adds one change, the
  contract horizon reaper's rate cap raised from 2,000 to 5,000 rows a second. The new containers mount the new config.
  g1 rolled at 10:15:55Z.
- **The first typed pass** ran at 10:22:11Z on a new g1 process. Within the next pass every page was written typed: 446
  page encodes a second over the pass, no gob fallback. Readers decoded typed pages at once (the api 115 a second, the
  proxy 70) with no decode error in either format. Gob decodes fell as cached gob pages expired.
- **A stop in g1's watch, judged shared.** At 10:21:22Z the roll's watch read the api's mean latency over 25 ms three
  times in a row (35 ms on the 2-minute rate). Main stalled for a minute at 10:14 and at 10:20, on all four api hosts at
  once and on database-heavy routes (`GET /subscription/balance` 541 and 290 ms), with the hosts' CPU flat. The first
  stall came before the roll, as the config hosts published, and the second before the first typed pass. The latency was
  back to 13.9 ms by 10:25Z, and the roll resumed with its gates unchanged. g2 rolled at 10:52:58Z, after the validator
  window and the award run that g1's start had seeded, and its watch passed.
- **Fully typed from 10:56Z.** Over the ten minutes to 11:06Z every export pass wrote typed pages (360 page encodes a
  second, no gob and no gob fallback), and every reader decoded only typed pages (the api 113 a second, the proxy 63,
  the taskworker 0.7) with no decode error. Page decodes fell against the gob passes (the api 130–260 a second, the
  proxy 150–240), as the larger effective page cache misses less.
- **Rollback:** `typed_score_pages: false`, a config release and a taskworker restart; readers keep reading both formats.

### 9.5 Measured

Both proxy hosts were CPU-bound throughout, 98.5–99.8% busy, so each runs about 23 proxy cores whatever the build: the
collector takes whatever CPU it is given, so a saving shows as less allocation and more work per core, not as fewer
cores. Per host, 30-minute windows:

| | before the roll (03:43–04:13Z) | after the roll (09:51–10:21Z) | after the flag (10:23–10:53Z) |
|---|---|---|---|
| crisp: allocation MB/s per process | 78.7 | 83.6 | 75.6 |
| crisp: active devices | 7,543 | 8,568 | 8,278 |
| crisp: allocation per 1k active devices, MB/s | 104 | 98 | 91 |
| crisp: proxy cores per 1k active devices | 3.00 | 2.68 | 2.79 |
| fireside: allocation MB/s per process | 66.6 | 69.1 | 62.0 |
| fireside: active devices | 6,689 | 7,758 | 7,286 |
| fireside: allocation per 1k active devices, MB/s | 100 | 89 | 85 |
| fireside: proxy cores per 1k active devices | 3.43 | 2.98 | 3.18 |

- **The roll** (the contract stats change, and every process restarted): allocation per active device fell 6% on crisp
  and 11% on fireside, at 14–16% more active devices.
- **The flag** (typed pages): allocation fell 8.0 MB/s per process on crisp and 7.1 on fireside, against §5's
  expectation of 3–5. Per active device the fall is 7% and 5%. The flag's window still held part of one gob pass.
- **Cores:** cores per active device read lower after the roll and the flag than before, but on saturated hosts that
  figure follows the load more than the build. It does not measure §4's 0.7–1.1 and 1.0–1.6 cores. A perf capture
  of `decodeClientScorePageBytes` and the tracker's emit against CPUOPT7's stacks, or a reading once the new hosts end
  the saturation, would.
