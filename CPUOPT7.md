# CPUOPT7 — the proxy hosts' CPU (2026-11-22)

On 2026-11-22 at about 03:30Z the owner decided: "Profile and cut CPU", with no
customer-visible change. The two proxy hosts, crisp and fireside, ran the proxy service
(10 blocks, 20 processes) at 23.2 and 23.6 of 24 cores busy, against 14.3 and 20.7 on
11-16, the morning after CPUOPT6's proxy changes went live.

This round profiles both hosts, finds what grew in the week, and ships the safe,
behavior-preserving wins:
- one on server main;
- three on connect and sdk branches, ready to merge (§4).

Nothing is deployed. A proxy roll needs the owner's go (§7).

**Owner rule (binding, as in CPUOPT4–6):** "We can't sacrifice correctness of performance
timings for the cpu savings." No limit, quota, timeout, detection bound or placement
changes. Every change below answers exactly what the code answered before, and each one
carries a deterministic test.

## Summary

- **Where the CPU goes** (six 30 s captures, §2):
  - **GC:** 41% of a process on crisp (8000 MiB memory limit) and 53% on fireside (6500
    MiB). Every process sits at its limit.
  - **connect's window machinery:** send and receive sequences 16%, H1 transports 10%,
    TLS 9–10%.
  - **A new cost, per-flow placement scans** in connect's multi-client: 8.6% of a process
    on crisp and 3.7% on fireside for the affinity donor scan, plus 4.1% and 1.5% for the
    flow reaper's pass. Both were 0.2% or less on 11-15.
  - **In-process control:** FindProviders2 3.9%, of which gob-decoding score pages is
    1.5–1.8%. Contract creation is 3.5%.
  - **The rest:** the sdk's contract stats tracker 2.7–4.4%, gVisor 3.6–3.9%, ingress TLS
    handshakes 1.5–2.3%.
- **What grew between 11-16 and 11-22** (matched hours, no restarts, §3):
  - **Load:** HTTP and SOCKS admissions 2.0–2.1×, live ingress connections 2.1–2.4×,
    window flows 1.8–2.0×, FindProviders2 loads 1.2–1.8×.
  - **Score page decodes:** 4.3–8× more (score page cache misses 17 → 140/s on crisp).
  - **Allocation per process:** crisp 50 → 83 MB/s, fireside 54 → 71. GC per process on
    crisp went 3.7 → 6.8 a minute.
  - **Not the growth:** window clients fell 19–25% (CPUOPT6's idle trim), live devices
    rose only 2–10%, and memory per process is flat.
  - **Crisp's proxy cores 12.8 → 22.3.** Fireside was already near its ceiling (19.4 →
    22.8).
- **What changed in code** (§4):
  1. **connect:** the indexed donor scan passes over expired members without reading them
     (8a41fb8f).
  2. **connect:** a flow update's IsDone reads `ctx.Err` (same commit).
  3. **sdk:** encodeUuid writes the id text in place (bc6a4429).
  4. **server:** FindProviders2 page reads copy only the records they keep (02e54f61a, on
     main).
- **Expected saving** (§5): about 3–4 cores of the fleet's 45 proxy cores. That's
  1.8–2.4 on crisp and 1.1–1.5 on fireside, most of it from the connect donor scan.
  - The hosts are saturated, so freed CPU shows first as service: shorter
    find-providers loads and setups, fewer mint refusals. Only after that does it show as
    idle.
- **The larger levers need the owner** (§6):
  - memory headroom (held for the two new hosts, 11-18);
  - a typed codec for score pages: their decode is about 15% of crisp's allocation bytes
    and 20% of its allocations, so it drives GC;
  - CPUOPT6's B #4, contract stats built only for listeners;
  - merging the connect and sdk branches, which every app also builds.

## 1. Method

- **Builds:** the proxy runs `2026.11.21-outerwerld-1104820710` on all 20 processes since
  the 11-21 hotfix roll: server `485cbe0b6`, connect `e9148707`, sdk `35fd1127`. The
  code read is at those pins and at main.
- **Captures:** `perf record -F 99 -g -p <pid>` for 30 s, three processes per host at
  once:
  - crisp g5, g1 and g8, 05:16:30–05:17:02Z;
  - fireside g5, g2 and g7, 05:17:33–05:18:05Z.

  Each is 6.7–7.2k samples, about 2.3–2.4 cores. The hosts were 23.8 and 23.9 of 24 busy
  during the captures.
  - **Validator gate:** both captures ran with the SN25 validator idle: intent 26490
    finalized, 2,388 and 2,328 s to the next boundary, 1,932 and 1,992 s past the last,
    and no `attempt-artifact` POSTs.
  - **Root:** through `xops/bin/by-pass <host> by` piped into `sudo -S`, the owner's 11-15
    allowance for read-only root checks. The password was never captured or printed.
  - **Cleanup:** stacks were folded on the host and copied back; data files deleted; no
    perf process left.
  - CPUOPT6's two 11-15 sweep captures (crisp g5, fireside g5) are the baseline.
- **Mimir:** two windows chosen to have no process restarts:
  - 11-16 00:00–06:00Z (A), after CPUOPT6's trim and crisp's 8000 MiB;
  - 11-22 00:00–05:00Z (B), 90 minutes after the 11-21 roll finished.

  Host sums are rates over the window. Per-process figures are host sums divided by 10.
- **Benchmarks:** on a laptop with warm caches, so they understate the production saving
  of the cache-miss-bound scan.
- **Scope:** nothing in production was changed. Raw outputs and scripts are in
  `<scratch>` (`prof/`, `out/`), outside the repository.

## 2. Where the CPU goes now

Share of a process, averaged over each host's three captures, with the 11-15 captures for
comparison. Rows overlap: TLS runs inside the H1 transports, the Framer inside the
sequences and transports, and GC assists inside the allocating rows.

| Share of a process | crisp | fireside | 11-15 crisp g5 | 11-15 fireside g5 |
|---|---|---|---|---|
| **GC, total** | **40.6%** | **52.5%** | 59.2% | 58.1% |
| background marking | 14.7% | 18.3% | 25.9% | 21.1% |
| mutator assists | 20.6% | 28.3% | 26.3% | 30.7% |
| write-barrier flushes | 3.8% | 4.1% | 5.2% | 4.4% |
| goroutine stack scanning (within the above) | 6.1% | 8.0% | 10.2% | 10.0% |
| **affinity donor scan** (`sendUpdate` → `inheritAffinityClient4WithLock`) | **8.6%** | **3.7%** | 0.0% | 0.0% |
| **flow reaper pass** (`detachIdleFlows`) | **4.1%** | **1.5%** | 0.2% | 0.2% |
| send and receive sequences | 15.8% | 16.2% | 16.5% | 18.1% |
| platform H1 transports | 10.2% | 9.7% | 10.5% | 11.1% |
| TLS, all (utls and crypto/tls) | 8.9% | 10.0% | 7.6% | 8.9% |
| ingress TLS server handshakes | 1.5% | 2.3% | 0.4% | 0.7% |
| Framer | 6.5% | 6.2% | 6.2% | 6.7% |
| FindProviders2, in-process | 3.9% | 3.8% | 4.2% | 2.4% |
| score page gob decode (within FindProviders2) | 1.8% | 1.5% | 1.1% | 0.4% |
| ConnectControl, in-process contracts | 3.6% | 3.5% | 3.5% | 3.2% |
| client-key record decode and verify (within ConnectControl) | 0.6% | 0.6% | 0.5% | 0.6% |
| sdk contract stats tracker | 2.7% | 4.4% | 4.4% | 5.7% |
| sdk `encodeUuid` (within the tracker) | 1.0% | 2.0% | 2.0% | 3.0% |
| gVisor | 3.6% | 3.9% | 3.5% | 3.7% |
| HTTP and SOCKS ingress (`urnetwork/proxy`) | 1.8% | 2.1% | 1.6% | 2.0% |
| go-redis / pgx | 3.0% / 1.9% | 3.6% / 1.8% | 4.0% / 1.5% | 3.5% / 1.5% |
| scheduler | 2.8% | 2.2% | 2.5% | 2.7% |

- **GC fell as a share** because crisp went to 8000 MiB on 11-16 and the trim cut window
  clients. It is still the largest single cost, and it is still paced by the memory limit:
  - crisp runs at 7.7 of 8.4 GB (heap plus stacks), collecting 7–8 times a minute;
  - fireside runs at 6.4 of 6.8 GB, 9.5–12.5 times a minute.
- **The donor scan's self time is cache misses.** A map lookup of each member's tuple
  (`mapaccess2` with the 4-tuple key's hash and equality, 2.9% of crisp g5), then the
  member's update and its selection, for every member of the group. It runs on four of the
  six processes (7.1–9.8%), on fireside g7 at 3.6%, and is absent on fireside g2: it lives
  on the processes that hold a busy device.
- **The reaper pass is `IsDone` per flow** (2.8 of crisp g1's 4.4%): a receive on each
  flow's done channel, every time the reaper wakes, which is on every new flow and every
  finished TCP flow.

## 3. What grew

Both hosts, matched windows A (11-16 00–06Z) and B (11-22 00–05Z), Mimir:

| | A crisp | A fireside | B crisp | B fireside | ratio crisp / fireside |
|---|---|---|---|---|---|
| busy cores | 14.3 | 20.7 | 23.2 | 23.6 | 1.62 / 1.14 |
| proxy cores | 12.8 | 19.4 | 22.3 | 22.8 | 1.75 / 1.17 |
| GC a minute, per process | 3.7 | 9.7 | 6.8 | 10.0 | 1.82 / 1.03 |
| allocation MB/s, per process | 49.8 | 53.9 | 83.4 | 70.8 | 1.67 / 1.31 |
| allocations k/s, per process | 510 | 554 | 873 | 752 | 1.71 / 1.36 |
| heap / stacks GB, per process | 5.46 / 2.07 | 4.66 / 1.76 | 5.83 / 1.90 | 4.86 / 1.55 | ≈ flat |
| goroutines k, per process | 247 | 223 | 231 | 194 | 0.94 / 0.87 |
| live / active devices | 27.5k / 5.8k | 25.2k / 5.9k | 30.2k / 7.7k | 25.5k / 6.9k | 1.10 / 1.02 live |
| window clients | 80.3k | 71.5k | 64.8k | 53.8k | 0.81 / 0.75 |
| live ingress connections | 4.5k | 4.6k | 10.7k | 9.9k | 2.38 / 2.14 |
| ingress admissions/s | 477 | 471 | 1,017 | 939 | 2.13 / 1.99 |
| window flows/s | 555 | 576 | 1,106 | 1,037 | 1.99 / 1.80 |
| FindProviders2 loads/s | 57 | 72 | 102 | 90 | 1.80 / 1.24 |
| score page lookups/s | 363 | 473 | 765 | 655 | 2.11 / 1.38 |
| score page decodes (cache misses)/s | 17 | 27 | 140 | 117 | 8.04 / 4.25 |
| contracts/s | 154 | 163 | 231 | 195 | 1.50 / 1.20 |
| user MB/s | 7.3 | 7.6 | 9.5 | 9.1 | 1.29 / 1.21 |

- **Connections and flows doubled; devices did not.** HTTP admissions alone went from
  about 380/s to 1,200–2,500/s fleet-wide over the week, and each admission is a TLS
  handshake and new flows.
  - The step came on 11-18 at 00–04Z: crisp's admissions went 500 → 1,000–1,600/s and its
    proxy cores 13 → 22 (saturated).
  - The trim kept window clients down, so the new cost is per connection and per flow, not
    per device.
- **The per-flow scans grow faster than the flows.**
  - **The donor scan** reads every member of the new flow's group: each flow in the group
    within its TCP idle timeout, minutes of them, for every new flow. Only members younger
    than `AffinityMaxTime` (one minute) can donate.
  - **The group's size:** without a learned server name, a hosted device's HTTPS flows to
    one destination IP and port share one group. A busy device adds new flows to a group
    that holds thousands, so the work is about flows × group size.
  - **The reaper** reads every live flow on each wake, and every new or finished flow
    wakes it.
- **Score page decodes went up 4–8×.**
  - **The cause:** the export republishes every page every five minutes, 12 runs an
    hour. Each process decodes each page it reads once per run, and the 64 MiB cache
    holds about 209 pages. More loads and more targets enlarged the working set past it:
    the miss share went from 5% to 18% of lookups, and evictions run at the miss rate.
  - **A decode's cost:** a 200-record page of 72 KB of gob takes 1.07 ms to decode and
    allocates 1.22 MB in 17.3k objects (benchmark). Production pages average about 53 KB,
    so about 0.9 MB and 13k objects each.
  - **Its share of the allocation:** on crisp, 14 decodes a second per process is about
    12.6 of 83 MB/s and about 180k of 873k allocations a second. That's about a third of
    crisp's allocation growth since 11-16.
- **GC follows the allocation.** At the memory limit the collection rate is proportional
  to the allocation rate. Memory per process barely moved, but crisp's allocation rose 67%
  and its collections 82%.
- **Not causes:**
  - The 11-21 session-check change: the revocation read is 0.1–0.2% of a process and the
    session package 0.9%. The 2 s budget took the unchecked share from 9% to 0.01% with no
    CPU step.
  - alt: there is no alt container on crisp (05:16Z census), and no alt series from either
    proxy host since 11-20.
  - The proxy build: the hotfix changed only the session budget.

## 4. What changed

All four keep every answer the same, and each has a benchmark.
- 4.3 and 4.4 carry a test that fails on the old code's allocations.
- 4.1's tests compare the new scan with the full scan through the production writers, and
  fail on either broken variant.
- 4.2 has a guard test of its answers.

### 4.1 connect: the indexed donor scan (8a41fb8f, branch `perf/cpuopt7-proxy-1122`)

- **Change:**
  - **The stale index:** the multi-client keeps, per affinity group, the entries that no
    longer name their tuple's current generation (`staleAffinityIp4Paths`, v6 twin).
  - **One writer per change:** every change of a tuple's current generation goes through
    `setIp4PathUpdateWithLock` or `deleteIp4PathUpdateWithLock`: new flows, generation
    reuse, affinity transitions, the reaper, the TCP quarantine and probes. The writer
    marks the replaced generation's entries stale. An entry is unmarked only when the
    current generation writes it.
  - **Why the skip is exact:** an entry that is not stale was written by the current
    generation no earlier than its creation. An affinity origin is never later than its
    update's creation. So once the entry is `AffinityMaxTime` old, `selection.eligible`
    would refuse the member, and `inheritAffinityGroupClient4WithLock` passes over it
    without the lookup or the pointer chases.
  - **What is still read in full:** stale entries and pinned placements.
    `inheritAffinityClient4WithLock` keeps the full scan for callers with an arbitrary
    member map (tests).
- **Tests:**
  - **`TestAffinityGroupScanMatchesFullScan`:** 600 seeded steps per IP version in virtual
    time, through the production writers only:
    - new flows and bound winners;
    - generation reuse, and a learned server name that moves a tuple's next generation to
      another group;
    - the reaper and the rebuild;
    - minutes passing across the boundary.

    After every step it compares both scans for every group, pinned and not, and checks
    the invariant the skip relies on. With the stale marks disabled it fails at the
    invariant (step 179). With the scan ignoring them it fails on a placement (step 110:
    the full scan donates through a stale entry, the shortcut would not).
  - **`TestAffinityGroupScanReadsStaleEntries`:** the stale entry's lifecycle and a
    positive control.
  - **`TestIpPathUpdatesHaveCanonicalWriters`:** keeps every write of the tuple maps in
    the four writers.
  - **`TestEveryPlacementSiteReadsTheFlowLabels`:** now reads the scan's moved body.
- **Benchmark** (`BenchmarkAffinityGroupScan`, one placement, warm caches):

  | Members older than a minute | 100 | 1,000 | 5,000 |
  |---|---|---|---|
  | full scan | 5.6 µs | 59.7 µs | 381 µs |
  | indexed scan | 1.9 µs | 16.1 µs | 75 µs |

  The indexed scan still iterates the group map, at about 15 ns a member. In production
  the full scan's members are cache misses, so the share removed should be higher than
  the bench's 73–80%.
- **Applies cleanly to the deployed pin `e9148707`**, builds there, and its tests and the
  `TestAffinityMaxTime*` boundary tests pass there.
- **Ran:** connect's multi-client tests (`Affinity|Donor|Reaper|Probe|Transition|Rebind|
  Quarantine|Group|Sticky|PathUpdate|MultiClient|Bench|Pin|Flow`) pass, apart from the
  one source test updated above. The full package run is in §8.

### 4.2 connect: `IsDone` reads `ctx.Err` (same commit)

- **Change:** `multiClientChannelUpdate.IsDone` returns `self.ctx.Err() != nil` in place
  of a non-blocking receive from `Done()`.
  - For a live cancel context, `Err` is one atomic load of the context. `Done` makes the
    channel on first use, and the receive reads the channel's state.
  - `Err` is non-nil exactly once the context is canceled, by the flow or by the
    multi-client, and it returns only after the done channel is closed. So the answer is
    the same.
- **Test:** `TestFlowUpdateIsDoneFollowsCancellation` (own cancel, parent cancel, nil and
  bare updates).
- **Benchmark:** `BenchmarkFlowReaperPass` is 0–15% faster on a laptop with warm caches.
  The production gain is the channel read's cache miss, about half of `IsDone`'s 2.8%
  share of crisp g1's pass (estimated).

### 4.3 sdk: `encodeUuid` writes the id text in place (bc6a4429, branch `perf/cpuopt7-proxy-1122`)

- **Change:** the contract stats tracker converts transfer paths for every contract on
  every stats epoch. Each id went through `fmt.Sprintf` with five boxed byte slices. It is
  now hex-encoded into a fixed buffer, giving the same canonical lowercase 8-4-4-4-12
  text.
- **Cost per id:** 183.5 ns, 168 B and 6 allocations before; 35.9 ns, 48 B and 1 allocation
  after.
- **Tests:**
  - `TestEncodeUuidMatchesCanonicalFormat` compares 10,002 ids with the fmt form.
  - `TestEncodeUuidAllocatesOnlyTheString` fails on the fmt version.
- **Applies cleanly to the deployed pin `35fd1127`.**

### 4.4 server: FindProviders2 page reads copy only what they keep (02e54f61a, on main)

- **Change:** `clientScoreCursor.readWithClient` copied every record of every page it read
  (about 57 KiB and a pointer slice), then filtered the copies by the page's facet filter
  and merged them.
  - `mergeClientScorePage` now filters and merges the shared decoded records.
  - It copies only the records it keeps, into one allocation sized by the records the
    filter keeps.
  - The filter and the location rule only read the shared records, and the merged copies
    are the reader's own, as before.
- **Benchmark:** B/op of one page read.

  | Records the filter keeps | 100% | 50% | 10% |
  |---|---|---|---|
  | before | 59,136 B, 2 allocations | 59,136 B, 2 | 59,136 B, 2 |
  | after | 57,344 B, 1 | 32,768 B, 1 | 6,144 B, 1 |
- **Tests:**
  - **`TestClientScorePageMergeMatchesCopyThenFilter`:** seeded, overlapping pages and four
    filters against the old path, plus a check that the shared pages are unchanged.
  - **`TestClientScorePageMergeCopiesOnlyKeptRecords`:** fails before the change, when a
    page the filter drops entirely allocated twice.
  - **The DB tests:** the 12 `TestClientScorePageCache*` tests (including "readers cannot
    change cached pages" and "keeps loaded scores and answers"), the native and selector
    FindProviders2 tests and the score read tests, 91 in all, pass through test-env.sh.

## 5. Expected savings per host

From §2's shares, at about 22.5 proxy cores per host:

| Change | crisp | fireside | How |
|---|---|---|---|
| 4.1 indexed donor scan | 1.4–1.7 | 0.6–0.8 | 8.6% / 3.7% of a process, 75–90% of it removed |
| 4.2 `IsDone` | 0.2–0.3 | 0.1 | 4.1% / 1.5% reaper pass, 20–30% of it |
| 4.3 `encodeUuid` | 0.15–0.2 | 0.3–0.35 | 1.0% / 2.0% of a process, about 75% of it removed |
| 4.4 page reads | 0.05–0.2 | 0.05–0.2 | 5.4% of allocation is page copies; the saved share is the filter's (unmeasured, 10–30%) plus the pointer slice, through GC |
| **Total** | **1.8–2.4** | **1.1–1.5** | about 3–4 of the fleet's 45 proxy cores |

- Only 4.4 lands on main now. 4.1–4.3 reach the proxy when its build pins the connect and
  sdk commits (§7).
- The fewer allocations of 4.3 and 4.4 also slow the collector a little. They are not
  counted above.
- The donor scan's saving scales with the bots' connections: it doubled in the week, and
  so would its saving.

## 6. Not done, and owner decisions

1. **Merge the connect and sdk branches.**
   - **What:** connect `perf/cpuopt7-proxy-1122` (8a41fb8f, on connect main `01ad48d9`)
     and sdk `perf/cpuopt7-proxy-1122` (bc6a4429, on sdk main `a1ae074e`) are local and
     unpushed.
   - **Why it's the owner's call:** both libraries ship in every app and in the provider,
     not only the proxy, so landing them is the owner's call even though the answers are
     unchanged.
   - **Recommendation:** merge both, and build the next proxy with them. (Done 11-22: §9.1.)
2. **Memory headroom** (P90SPEED fix #1, held on 11-18 until the two new hosts and 192 GB
   per host arrive).
   - **The cost:** GC is still 41–53% of a process.
   - **The CPUOPT6 curve:** each 0.35 GB per process of headroom roughly halves the
     collection rate.
   - **Recommendation:** when the hosts come, size the limits up before rebalancing blocks.
3. **A typed codec for score pages** (CPUOPT6 C #3, now with proxy evidence).
   - **The cost now:** a gob decode is about 1 ms and 0.9 MB a page in 13k objects. On
     crisp the decodes are about 15% of allocation bytes and 20% of allocations, and grew
     8× in the week. Through GC, about 6–8% of a proxy process, plus 1.5–1.8% direct.
   - **Options:** a binary page codec written beside gob, readers first, as an additive
     key. Or, with headroom, a larger decoded-page cache, which trades memory against
     decodes; with today's headroom it would make GC worse.
   - **Recommendation:** the codec. It is a writer (taskworker) and reader change.
4. **Contract stats only for listeners** (CPUOPT6 B #4, not chosen on 11-15; sdk).
   - **The cost:** 2.7–4.4% of a process. 4.3 removes part of it.
   - **The change:** hosted devices have no listener, so the tracker's epoch flush can skip
     building the lists.
   - **Recommendation:** approve it as the next sdk change.
5. **The reaper's wakes.**
   - **Today:** every new flow and every finished TCP flow wakes the reaper for a full pass.
   - **The catch:** skipping the wakes that cannot bring an earlier deadline would also
     delay the sticky-flow trim, which today runs on any wake. That is a timing change,
     so it isn't done here.
   - **Options:** keep it; or give the trim its own deadline and wake only on earlier
     deadlines.
6. **The indexed scan's remaining iteration.** A busy group still costs about 15 ns per
   member per new flow. A per-group, time-ordered index would make the scan proportional
   to the members younger than a minute. It's a follow-up if 4.1's measured saving is
   short of §5.
7. **Per-frame allocations** (CPUOPT6 B #5: `Framer.ReadHeader`, utls writes): unchanged
   and still small.
8. **Ingress TLS handshakes** (1.5–2.3%, up 3–4× with admissions): resumption only helps
   clients that resume, so there is no server-side change without behavior change.

## 7. Roll plan

Nothing is deployed. A proxy roll needs the owner's go, then the roll script's canary and
validator gates (memory notes proxy-roll-canary-checks and validator-deploy-gate). It
follows the 11-21 hotfix roll.

- **Build:** a proxy image only. Two choices:
  - **(A) Hotfix pins, recommended:** the running pins plus these commits. That is server
    `485cbe0b6` + `02e54f61a` cherry-picked (its files are unchanged since `485cbe0b6`),
    connect `e9148707` + `8a41fb8f`, and sdk `35fd1127` + `bc6a4429`. All three apply
    cleanly. The other pins stay D64's.
  - **(B) Main:** server main with connect and sdk main after their merges. That brings
    every change since the running build, such as connect's phone h3 budget and the
    quic-go fork.
  - **Either way:** the 11-21 build gates: a private version home, the registry guard,
    govulncheck, the stamps, the `-trimpath` audit rebuild, the dependency audit against
    `1104820710`, and an unchanged rollback target `main-proxy:2026.11.21-outerwerld-1104820710`.
- **Order:**
  1. g3 as the canary with a 30-minute hold.
  2. The WireGuard canary: check WireGuard packets by block first, as WireGuard moves
     between blocks (g6 on 11-21).
  3. The other eight blocks, with a half-fleet check after the fifth.
- **Gates per block:**
  - **Validator idle:** finalized or applied, at least 900 s to the boundary and 600 s
    past it, and no `attempt-artifact` POSTs for 180 s. A canary needs its hold + 600 s.
  - **Health:** 503/s and api latency; PgBouncer waiting under 1,000; edge-2 busy without
    iowait, printed with the sweep rate; descendant walks at most 20, failing closed; at
    least 12 GB available on each host.
  - **Timing:** `SETTLE=360` before after-readings; outside 17:30–19:30Z.
  - **Version guard:** every block on `1104820710` before its deploy.
- **Judge each block at 60 minutes, against the other blocks on the same host.** A restart
  gives about an hour of relief while devices and heaps refill.
  - **The direct check:** a perf capture of one rolled and one old process on each host,
    the same 30 s at 99 Hz, under the validator gate. `inheritAffinityClient4FromWithLock`
    should fall from 7–10% to 1–2% on processes with busy devices.
  - **Mimir:** proxy cores per admission and per window flow; GC a minute; allocation
    MB/s.
  - **Service:** FindProviders2 load p95, proxy-test setup p50 and p90, and mint refusals.
- **Revert:** a canary failure reverts the rolled blocks to `1104820710`. One regressed
  block reverts that block and stops the roll.

## 8. Tests run

- **server, on the exact merge tree** (`perf/cpuopt7-proxy-1122` merged `--no-ff` on main
  `5a3622fca`), 06:15–06:22Z:
  - build;
  - vet of `.`, session, model, localclient, proxy, api, connect, controller,
    bringyourctl and taskworker/work;
  - `./grafana` with `URNETWORK_PROMTOOL` (promtool 3.5.0) in a pure env;
  - `TestBuildsForDarwin`;
  - the new model tests, 2/2;
  - the FindProviders2, score-read and page-cache DB tests, 91/91.
- **server proxy package** (DB, on the branch with the connect branch as its sibling, so
  the proxy's own tests ran the indexed donor scan): ok in 659 s, 330 passed, 0 failed.
- **DB tests** ran through test-env.sh after confirming that local-pg and local-redis
  resolve to 10.213.0.1 and both containers are healthy. No `./monitor/` tests were run
  (planetoid runs the monitor).
- **connect:**
  - the new tests pass on the branch and on the deployed pin `e9148707`;
  - the multi-client subset (§4.1) passes after the source test update;
  - the full root package (9,074 tests) is the branch's merge gate, run on the merged tree
    before it merges.
- **sdk:** the new tests pass.

## 9. Merge, build and roll (owner, about 07:05Z 11-22: "Merge + canary proxy roll")

### 9.1 Merges

- **connect main `0709a55a`:** the `--no-ff` merge of `perf/cpuopt7-proxy-1122` (8a41fb8f) on `33c032e1`, pushed at
  about 07:33Z.
  - The merged tree's full `./...` run passed, apart from failures that main has without the change:
    - the root package fails only the known `TestAuthCancellationIsNotBackendFailure`;
    - `extender` fails 13 phone-memory profile tests (`TestExtenderIosMemoryLive*`,
      `TestExtenderMobileMemoryLiveCompositionMatrix`), which fail the same way on `33c032e1`.
- **sdk main `bced97ce`:** the `--no-ff` merge of bc6a4429 on `a945474c`, pushed at about 07:21Z. Its merged tree passed
  all seven packages, with connect main plus the change as the sibling.

### 9.2 The image (§7, option A)

- **Version:** `main-proxy:2026.11.22-outerwerld-1105311750`.
- **Digest:** `sha256:e8853c96a50c8a62aa20d5caf029c4f17e1ba8c9ad97e06bb32129984ebbcc3a`. Built 07:06–07:16Z by a copy of
  the 11-21 build harness.
- **Sources:** the running pins, plus one commit each, as local `hotfix/cpuopt7-proxy-1122` branches proven by
  patch-id:
  - server `a4225d5aa` = `485cbe0b6` + 02e54f61a;
  - connect `91f1d695` = `e9148707` + 8a41fb8f;
  - sdk `efbbafcc` = `35fd1127` + bc6a4429;
  - sn, proxy, glog, goidenticons, userwireguard and gvisor at 1104820710's pins.
- **Gates:**
  - D64's private warpctl and a private, empty version home;
  - the registry guard: the tag was absent before its push;
  - govulncheck on amd64 and arm64: 0 affecting;
  - the pushed digest equals the registry's;
  - the stamps: `a4225d5aa`, `vcs.modified=false`, go1.27.4;
  - the dependency audit against 1104820710: MATCH, 213 and 212 lines, the VCS lines aside;
  - the `-trimpath` audit rebuild: MATCH on both arches;
  - the rollback target `main-proxy:2026.11.21-outerwerld-1104820710` (`sha256:b4d255d1…`) unchanged before and
    after.
- **The hotfix tree's tests**, with the siblings at the image's pins:
  - build and vet;
  - the new connect, sdk and model tests;
  - the 91 FindProviders2, score-read and page-cache DB tests;
  - the proxy package (DB), 330 passed and 0 failed.

### 9.3 The roll

**The g3 canary passed (deploy 09:43:10Z, hold to 10:18:53Z, verdict at 10:19:22Z).** It ran under the validator
gate (intent 26494 finalized, 3,672 s to the boundary), the health gates, the payout gate and the proxy lane lock. No
api restart landed in its window (every api process started 03:40–03:57Z).

| Per process, g3 against its 18 peers | 30 min before the deploy | Last 10 min of the hold (36 min after) |
|---|---|---|
| cores | 2.31 (peers 2.32) | 2.13 (peers 2.33), −8.7% |
| GC a minute | 8.36 (peers 8.48) | 7.07 (peers 8.99), −21% |
| heap+stacks | 7.07 GB (peers 7.10) | 6.98 GB (peers 7.14) |
| session-check unchecked share | 0.043% (peers 0.033%) | 0.135% (peers 0.037%), under the 1% bound |

- **Restart relief:** a restarted process holds fewer devices for about an hour (crisp g3 2,020 live devices at 7
  minutes against its peers' 3,200–3,500). So part of this is relief. The perf A/B below is the direct check.
- **The perf A/B, 10:03Z:** 30 s at 99 Hz, the new g3 against the old g5 on each host, at the same moment.

  | Share of the process | crisp g3 (new) | crisp g5 (old) | fireside g3 (new) | fireside g5 (old) |
  |---|---|---|---|---|
  | affinity donor scan | 0.04% | 7.42% | 5.36% | 13.91% |
  | flow reaper pass | 0.37% | 2.17% | 4.10% | 5.14% |
  | `IsDone` within it | 0 | 1.50% | 0 (`Err` 1.18%) | 3.17% |
  | sdk `encodeUuid` | 0.20% | 1.16% | 0.13% | 1.09% |
  | page reads (`readWithClient`) | 0.45% | 1.37% | 0.92% | 1.29% |

  fireside g3's remaining donor-scan cost is lookups of members younger than a minute: 1.76% `mapaccess2` and 3.04%
  self. A busy group there adds many flows a minute, which is §6 item 6's follow-up (a time-ordered index).
- **The safety rules passed:**
  - WireGuard: median 104 → 30 packets a second, sessions 21 → 15. The rule reverts below 10% and 50%.
  - Active devices against the other blocks: 0.973 (stop below 0.8).
  - User bytes against the other blocks: 1.171 (stop below 0.6).
  - edge-2's 15-min busy CPU without iowait: 0.461 → 0.471, limit 0.511. It reached 0.538 at 10:03Z with bot sweeps
    at 330–357 a second, then fell. Before the roll it swung 0.446–0.607 with no proxy change.
  - The D57p per-host comparison: crisp g3 had 4.0 mint refusals a second against its peers' 27.3, 0.35 failed CONNECTs
    a second against 0.71, and 1.91 against 2.35 cores. fireside g3 had 12.6 refusals a second against 25.8.
  - proxy-test setup p50: 8.84 → 7.94 s, success share 0.862.

**The g6 WireGuard canary passed (deploy 10:55:24Z, hold to 11:20:24Z).**

- WireGuard: median 52.4 → 43.0 packets a second, sessions 21 → 24.
- Against the other blocks: active devices 0.991, user bytes 1.259. edge-2: 0.507 → 0.459.
- Per process: 2.28 cores against 2.32, and 8.2 collections a minute against 9.1.
- The per-host comparison: fireside g6 had 21.6 mint refusals a second against its peers' 41.1, and 0.31 failed
  CONNECTs a second against 0.65. crisp was level (0.78 against 0.69 failed CONNECTs a second, diff-in-diff +0.03).

**The other blocks.** Each passed the validator, health and payout gates and the version guard, then settled 600 s.
The readings are per process, about 15 minutes after the deploy, so they include restart relief:

| Block | Deployed | Cores (other blocks) | GC a minute (other blocks) | Heap+stacks GB (other blocks) |
|---|---|---|---|---|
| g1 | 11:21:02Z | 2.20 (2.31) | 4.56 (8.51) | 6.70 (7.08) |
| g2 | 11:36:10Z | 2.00 (2.33) | 5.04 (8.46) | 6.73 (7.04) |
| g4 | 12:06:30Z | 2.07 (2.33) | 6.78 (8.67) | 6.81 (7.06) |
| g5 | 12:21:44Z | 2.07 (2.32) | 4.52 (8.73) | 6.67 (7.08) |
| g7 | 12:36:52Z | 2.31 (2.31) | 7.94 (8.59) | 6.94 (7.05) |
| g8 | 13:18:41Z | 1.88 (2.27) | 2.72 (7.66) | 6.64 (7.06) |

- The half-fleet check after g4 passed: proxy-test setup p50 was 8.62 s against 7.90 s before g6, and the success
  share 0.758 against 0.742. The rolled blocks' active devices against the old blocks': 0.977.

**Two stops on the per-block edge-2 rule, both judged false.** The rule compares two single 5-minute readings about
15 minutes apart, with a tolerance of 0.05. Before the roll began, edge-2's 5-minute reading swung by more than 0.05
within 15 of the 20 15-minute windows from 05:00Z, by up to 0.23 (0.48–0.72 at 07:54Z), with no deploy.

1. **After g7, 12:51:27Z (0.494 → 0.571).**
   - g7's state load (12:40–12:44, 68–86 acquired connections) came at edge-2's low point, 0.49–0.51.
   - The rise came at 12:46–12:53, after g7's connections were back to 33–48. It was user CPU (0.36 → 0.44), with
     transaction and tuple rates flat, bot sweeps steady at 390–407 a second and 7–10 autovacuum workers.
   - 12:02–12:11 read 0.57–0.60 before g4 started.
   - Resumed at 12:55Z for g8, g9 and g10, with every guard unchanged. g8 deployed at the next validator window.
2. **After g8, 13:33:16Z (0.476 → 0.594), with a Main stall from 13:30Z.**
   - The stall: api mean latency rose to 600 ms and PgBouncer clients waiting to 132.
   - g8's state load had ended at 13:24, with 8–9 waiting.
   - The stall was the start of a fleet-wide deploy that wasn't part of this roll: taskworker g1 restarted at 13:30:53Z
     and the api's beta block at 13:31:07Z. The api was already slow at 13:30:00Z, before the first restart.
   - The 7 block restarts from 09:43 to 12:37Z caused no stall.

For the next roll: give the per-block rule the canary's 15-minute average, or a tolerance of 0.09.

**WireGuard on restarted blocks.** In the hour after each restart, the restarted block's WireGuard traffic fell
against the blocks not yet restarted. The old build's 11-21 roll did the same, so this is the cost of a restart, not
of this build:

| Roll (pre: 60 min before; post: 10–70 min after) | Restarted blocks, post/pre | Blocks not yet restarted, same windows | Relative |
|---|---|---|---|
| 11-21, the old build 1104820710 (g3–g8) | 0.67 | 1.03 | 0.65 |
| 11-22, this build (g3–g7) | 0.59 | 0.89 | 0.66 |

- Single blocks range widely in both rolls: 0.20–1.53 relative on 11-21 and 0.12–2.20 on 11-22.
- WireGuard sessions fell by about a quarter on most restarted blocks. The handoff exports only active peers, so this
  is a candidate for its own change.

**How the roll ended: 8 blocks, then a fleet-wide deploy.**

- **This roll:** g3, g6, g1, g2, g4, g5, g7 and g8 ran 1105311750, the last from 13:22Z. g9 and g10 were held on
  1104820710 after the second stop. The roll released the proxy lane lock at each stop. Its queued resume held no
  lock and was waiting for 14:06Z.
- **The fleet-wide deploy:** from 13:30Z, a deploy of the builder's `2026.11.22+1105365180` replaced every service:
  - taskworker from 13:30:53Z;
  - api, connect and mcp from 13:31Z to 13:48Z;
  - all 20 proxy processes at 13:48:11–13:51:55Z, about 20 s apart, both hosts of every block.
- **CPUOPT7 is in that build.** Its tags contain all of this round's commits: server `ff9c9f996` (02e54f61a), connect
  `dc447487` (8a41fb8f) and sdk `2c4bb027` (bc6a4429).
- **Main during it:** PgBouncer clients waiting 91–416 at 13:50–13:52Z. At 13:53Z, api requests other than the control
  long-polls averaged about 0.5 s over 2 minutes.
- **The guards would have held the roll.** Had the resumed run been active, its builder guard would have held it at
  the next block on the new api and taskworker versions. Its version guard refuses to deploy over a newer build.
- **For the next deploy-all:** take or check the proxy lane lock (`deploy-proxy.lock.d`), so a deploy-all can't run
  through a stopped proxy roll.

## 10. The next round, after the rollout (11-23)

On 2026-11-23 at about 07:30Z the owner asked: "Now that the latest services are deployed to main that incorporate the
findings of server/CPUOPT7.md, perform another analysis of opportunities for CPU efficiency improvement using evidence
from main and write results to server/CPUOPT7.md. Include all services, e.g. connect (including lb-alt), proxy, api,
etc." This section measures every Main service again, says what the deployed changes saved, and ranks what is next.
§1–§9 are left as written on 11-22.

Nothing in production was changed: no deploy, restart, config, setting or Redis control. The permission system
refused nothing.

**Owner rule (binding, as in CPUOPT4–8):** "We can't sacrifice correctness of performance timings for the cpu
savings." Every proposal below keeps each answer and each timing bound. Where an item comes near a bound, it says
which one and why it does not move.

### Summary

- **The fleet:** busy CPUs on the nine Main service hosts, over the matched hours 00:00–02:20Z, were 278.4 a week ago
  (11-16), 273.5 at §2's baseline (11-22) and 282.1 after the rollout (11-23). At 08:00–08:30Z this morning they were
  319.9, with the day's load and a connect generation an hour old (§10.2).
- **What the deployed changes saved** (§10.2):
  - **The proxy (CPUOPT7, live since 11-22 13:52Z).** Both hosts were saturated in both windows, so the saving shows
    as work done.
    - From B to N, proxy cores rose 4% while window flows rose 31%, ingress admissions 22% and user bytes 26%. That is
      20% less CPU per window flow and 15% less per admission.
    - Service held: proxy-test setup p50 went 7.5 → 6.4 s, and p90 stayed at 40 s.
    - At B's cost per unit, N's work would have needed 54–57 cores on 48 CPUs. §5 predicted 3–4 cores; part of the
      larger gain is the load's mix *(inferred)*.
  - **CPUOPT8's contract stats, on the five rolled proxy blocks:** the tracker fell from 2.0–2.4% of a process to 0.2%.
    The rolled blocks collect 17–25% less often than the old ones on the same host. Typed score pages wait for D71's
    flag step (§10.4).
  - **The services edges (LBALT and LBALTOPT2, 11-20 to 11-22):** busy CPUs per Gbps of NIC traffic went 15.6 → 12.8 →
    12.4 from A to B to N. nginx's 7–9 cores per edge are gone, and connect's allocation fell 29% from B to N.
  - **PostgreSQL:** the direct path removed the nginx stream hop (5.1 cores), and LBALTOPT2's R18 removed 9.3k
    provide-key counts a second. Still, edge-2 costs 0.41–0.43 cores per 1k PgBouncer queries a second, as on 11-15:
    statements got heavier, at 124 µs of execution each against 91–100.
  - **Redis:** 38.0 → 24.6 cores since CPUOPT6. The key-event storm fix took 13.0 of them and the dropped preflight PING
    1.75; MinIO's evidence sharding saved another 3.3 MinIO cores. edge-6's IRQ change fixed latency, not CPU.
- **Where it goes now** (§10.3):
  - **connect**, 113 cores at night and 135 this morning, is two-fifths of the fleet. Its largest removable cost is the
    runtime's timer-heap walks: 7.4–13.8% of every g2 process, against 2.1–2.3% of g1 and g3 on the same hosts.
  - **The proxy** is still at its memory limits, with GC 43–55% of a process.
  - **edge-2** runs 45–56 cores, and **Redis** 24.6–28.4.
- **The next opportunities** (§10.5). The five largest:
  1. Find what sets off connect's timer-heap walks in g2, and take the model's backstop polls off the runtime heap:
     3–7 connect cores *(inferred)*.
  2. Deliver contract-origin edges only to their waiters: about 2.5–3 cores across the api, the proxy and Redis.
  3. The proxy's donor scan visits only the last minute's entries: 1.5–2 proxy cores.
  4. connect's id formatting without `fmt`, and its resident decode without a per-read gob decoder: 1.3–2 connect cores.
  5. The proxy's per-frame allocations: 1.2–1.8 proxy cores.

  The 26 ranked items come to roughly 23–35 cores, about a tenth of the fleet. They overlap in places, as noted.

### 10.1 What runs, and how it was measured

**Builds** (`warpctl ls versions main`, `urnetwork_build_info`, about 07:40Z):

| Service | Build | Since | What it adds since §9 |
|---|---|---|---|
| connect, with its lb_alt front | `2026.11.23-outerwerld+1106163710` | 06:50–06:56Z, old generation drained by about 07:56Z | the stuck-provider bake-in; LBALTOPT2 R13–R23 as live since 11-22 |
| api | `2026.11.22-outerwerld+1105990880` (server b896cf6d9) | 02:22–03:02Z (D71) | CPUOPT8's typed-page readers with the flag off; the bake-in |
| taskworker | `2026.11.22-outerwerld+1105994420` | 03:03–03:40Z | the same server; the score export still writes gob |
| mcp | `2026.11.22-outerwerld+1105998450` | 03:53–04:12Z | the same server |
| proxy | D71's `2026.11.22-outerwerld+1105997020` on g3 (04:55Z), g6 (06:07Z), g1 (07:19Z), g2 (07:35Z) and g4 (07:50Z) by 08:10Z; the rest on the 11-22 deploy-all's `2026.11.22+1105365180` | the deploy-all from 11-22 13:48–13:52Z | the deploy-all carries CPUOPT7; D71's build adds CPUOPT8 (contract stats only for listeners, typed-page readers) |
| gossip, lb units | gossip `2026.11.22+1105365180`; every lb_alt interface's lb unit on warpctl bca1f62 with `--lbaltporthash` since 11-22 03:15Z | | |

**Windows.** Three windows of the same hours, 00:00–02:20Z, with no process restarts in them:
- **A, 11-16:** a week ago, a Monday like 11-23. nginx was still in front of connect, and CPUOPT6's proxy changes were
  live. edge-2 had its PostgreSQL 18.7 restart at 02:01–02:02Z, inside the window.
- **B, 11-22:** §3's window B, before CPUOPT7, with the proxy on the 11-21 build.
- **N, 11-23:** every service on the deploy-all build that carries CPUOPT7, 10–12 hours old, before today's rolls
  (D71's api roll began at 02:22Z).
- **Now:** 08:00–08:30Z on 11-23, after connect's drain, with the proxy roll in progress.

Busy means every CPU mode except idle and iowait (`node_cpu_seconds_total`). A service's cores are its processes'
`process_cpu_seconds_total`.

**Captures.** Every capture ran under the SN25 validator-idle gate: intent finalized or applied, at least 900 s to the
next boundary and 600 s past the last, and no `attempt-artifact` POSTs in 180 s.
- **proxy:** `perf record -F 99 -g`, 30 s, as in §1, of a new-build and an old-build process on each host at once:
  crisp and fireside g3 (D71's build, 2.6 h old) against g10 (the deploy-all build, 17.7 h old), 07:31–07:33Z. The
  hosts were 23.9 of 24 busy.
- **api and taskworker:** the same perf method on the services edges: api g2 and taskworker g2 on edge-4 (07:36Z), and
  taskworker g2 and api g3 on edge-3 (07:38Z). Root came through `by-pass` into `sudo -S` (the owner's 11-15 allowance
  for read-only checks; never printed).
- **connect:** six 30 s CPU profiles from its loopback pprof listener (LBALTOPT1 Appendix C), one process at a time,
  07:58–08:04Z, after a go from connect's lane. Plus a heap snapshot of edge-4 g2 and edge-4 g3, two allocation
  snapshots of edge-4 g2 13 minutes apart, and a 10 s `/proc` census of edge-4.
- **PostgreSQL:** six `pg_stat_statements` snapshots, 07:32–08:03Z, one statement of 2–3 s each, never reset; 21
  activity samples; four 20 s `/proc` censuses of edge-2 without root; PgBouncer's own `stats` log lines.
- **Redis:** the redis exporter for all 48 nodes, the services' Redis client counters, and two 20 s `/proc` censuses of
  edge-5 and edge-6 without root. No Redis command ran, from the analysis machine or on the hosts.
- **Cleanup:** data files were deleted on the hosts after folding or copying, and no perf process was left. Raw
  outputs and scripts are in `<scratch>` outside the repository.

**Caveats.**
- The connect processes were 1.1–1.2 hours old. A 30 s window held no GC cycle on most of them, so GC is read from
  Mimir instead (§10.3.1).
- The new proxy blocks were 1.6–2.8 hours old against the old blocks' 17.9 hours, so restart relief lowers the new
  blocks' GC. The perf A/B's function shares do not depend on it.

### 10.2 The fleet: now, against §2's baseline and a week ago

Busy CPUs by host group, and each Go service's cores on it. Rates are over each window.

| Cores | A 11-16 | B 11-22 (§2's baseline) | N 11-23 | Now 11-23 08:00–08:30Z |
|---|---|---|---|---|
| **Services edges, busy** (edge-0, 1, 3, 4; 312 CPUs) | 170.7 | 145.7 | 153.6 | 178.3 |
| … connect | 98.6 | 107.3 | 113.2 | 134.7 |
| … api | 17.1 | 14.4 | 16.5 | 20.3 |
| … taskworker | 12.3 | 13.0 | 12.9 | 13.1 |
| … mcp | 0.4 | 0.4 | 0.4 | 0.4 |
| … gossip | 0.1 | 0.2 | 0.4 | 0.4 |
| … other | 42.2 | 10.4 | 10.2 | 9.5 |
| **Proxy hosts, busy** (crisp, fireside; 48 CPUs) | 34.2 | 45.7 | 47.5 | 47.8 |
| … proxy | 31.4 | 43.9 | 45.7 | 46.1 |
| … alt | 0.5 | 0.0 | 0.0 | 0.0 |
| … other | 2.2 | 1.8 | 1.8 | 1.7 |
| **edge-2, busy** (PostgreSQL, PgBouncer; 96 CPUs) | 41.9 | 46.0 | 45.2 | 53.5 |
| **edge-5, busy** (16 Redis primaries, MinIO; 96 CPUs) | 12.6 | 11.3 | 11.4 | 12.0 |
| **edge-6, busy** (32 Redis primaries, backup replay; 128 CPUs) | 19.1 | 24.9 | 24.4 | 28.3 |
| **Fleet, the nine hosts above** | 278.4 | 273.5 | 282.1 | 319.9 |

- **Other** on the services edges is busy minus the Go services. In A it was mostly nginx, 7–9 cores per edge. Since
  LBALT (11-20) it's 2–3.4 cores per edge: the Grafana stack, logging, and the interrupt-context softirq where the
  lb_alt split and NIC receive run (§10.3.1). On the proxy hosts it's Loki, Mimir and the kernel.
- **edge-2, edge-5 and edge-6** run no Go service that reports here. §10.3.6 and §10.3.7 split them.

**Per unit of work:**

| Per unit of work | A 11-16 | B 11-22 | N 11-23 | Now |
|---|---|---|---|---|
| services edges: busy CPUs per Gbps of NIC traffic | 15.64 | 12.83 | 12.40 | 11.63 |
| connect: cores per Gbps | 9.04 | 9.45 | 9.14 | 8.79 |
| connect: allocation, GB/s | 1.95 | 2.22 | 1.59 | 1.89 |
| proxy: cores per 1k window flows/s | 28.5 | 18.4 | 14.6 | 20.5 |
| proxy: cores per 1k ingress admissions/s | 34.5 | 19.5 | 16.6 | 24.1 |
| api: cores per 1k requests/s | 1.59 | 1.84 | 2.54 | 2.70 |

- **The proxy rows depend on the load.** At saturation the proxy's cores stay near 46 whatever the demand, so a
  ratio compares only windows of similar load. B and N qualify: in N the hosts carried 31% more window flows with
  proxy-test setup p50 7.5 → 6.4 s and p90 unchanged (40 s); hosted mint admissions held at 211–214 a second while
  refusals rose with demand (178 → 270). The now column carries less demand (2.3k flows a second, with D71
  restarting blocks), so it reads high.
- **The api's cost per request rose mostly with its mix** (§10.3.3). The client-key decode is the one per-request
  cost that grew on its own (A1).
- **PostgreSQL** (§10.3.6): edge-2's cores per 1k PgBouncer queries a second were 0.42 in A, 0.43 in B, 0.41 in N and
  0.42 in the 07–08Z hour.
- **Redis** (§10.3.7): µs of Redis CPU per network request were 128 in A, 131 in B, 133 in N and 132 in the 06:29–07:29Z
  hour, against 111 on 11-15, when the storm's cheap requests diluted it.

**What the deployed changes saved:**

| Change, and when it went live | Prediction | Measured |
|---|---|---|
| CPUOPT7 (the indexed donor scan, `IsDone`, the sdk `encodeUuid`, page reads that copy only kept records), every proxy process from 11-22 13:52Z | 3–4 of 45 proxy cores (§5) | The hosts were saturated in B and N, so it shows as work done. Proxy cores rose 4% (43.9 → 45.7) while window flows rose 31%, ingress admissions 22% and user bytes 26%: 20% less CPU per window flow, 15% less per admission, 17% less per user MB. At B's cost per unit, N's work would have needed 54–57 cores. Directly (§9.3's A/B): the donor scan 7.4% / 13.9% → 0.04% / 5.4%, the reaper pass 2.2% / 5.1% → 0.4% / 4.1%, `encodeUuid` 1.1–1.2% → 0.1–0.2%. More than predicted; part of it is the load's mix *(inferred)*. |
| CPUOPT8's contract stats only for listeners (sdk), D71's proxy blocks from 04:55Z | 1.7–2.7 cores with the typed pages | The tracker 2.0–2.4% → 0.2% of a process; 17–25% fewer collections a minute than the old blocks on the same host (§10.4) |
| LBALT and LBALTOPT2's R13–R23 (connect's front, the listener skip, the timer sweep, announce batching, growable queues, shared tickets, identity limits), 11-20 to 11-22 | – | Services edges' busy CPUs per Gbps 15.6 (A, nginx) → 12.8 (B) → 12.4 (N). connect cores per Gbps 9.0 → 9.5 → 9.1, having taken on the front. connect allocation 2.22 → 1.59 GB/s from B to N (−29%), GC 4.1% → 3.4% of connect, heap in use 683 → 481 GB. nginx's 7–9 cores per edge are gone. |
| PgBouncer's direct path, stages 2–4 (11-15/16), and R18's batched provide-key count (connect, 11-22) | 4.5–5.5 cores (CPUOPT6 D #1) | The nginx stream hop went 5.07 → 0.01 cores, and 9.3k counts a second are gone. Yet edge-2's cost per PgBouncer query is unchanged (0.41–0.43 cores per 1k a second), because statements got heavier: 124 µs of execution each, against 91–100. The SN25 read (0.37 → 1.17 backends), the provider stats reads (0.02 → 0.76), the debit flush (0.015 → 0.38) and the endpoint fence's lock waits (0.035 → 1.34) took it back. |
| The key-event storm fix and the dropped preflight PING (11-15/16); MinIO's evidence sharding and flat move | 15–17 + 1.8–2.2 Redis cores; 3–4.5 MinIO | Redis −13.0 and −1.75 cores, softirq −4.5 and −0.9; MinIO 5.3 → 2.0. Redis is 24.6 cores in N against 38.0 on 11-15, at 11% fewer clients. |
| edge-6's NIC interrupts (11-16) | 1.7–4.1 edge-6 cores | No CPU change (Redis per request against edge-5 unchanged), but the hot spot is gone: the busiest CPU's softirq went 51% → 7%, CPU pressure 34.6% → 1–5% |
| The stuck-provider recycle (11-16) and its bake-in (11-23) | 0.8–1.9 api cores averaged, 2–4.5 at a sawtooth's top | Refusal replays at most 0.82k a second since, against 7.2k at the 11-15 tops; Redis's admission EVALSHA daily maximum 13.2k → at most 8.1k a second |

### 10.3 Where the CPU goes, service by service

#### 10.3.1 connect, with its lb_alt front (services edges)

Six 30 s CPU profiles, 07:58–08:04Z, of processes 1.1–1.2 hours old, at 8.1–9.1 cores each. Since LBALT the front's
request handling, TLS and QUIC run inside connect, so this is the front too. The lb_alt split itself is an iptables
HMARK, so it shows as kernel time outside any process (last bullet).

| Share of a connect process | edge-4 g2 | edge-3 g2 | edge-1 g2 | edge-0 g1 | edge-1 g3 | edge-4 g3 |
|---|---|---|---|---|---|---|
| scheduler `findRunnable` (all idle-P work) | 25.3% | 25.7% | 22.1% | 21.5% | 19.4% | 19.8% |
| … **timer heap walks**, `timers.check` / of which `adjust` | **15.9% / 13.8%** | **13.7% / 11.6%** | **9.1% / 7.4%** | 3.9% / 2.3% | 3.6% / 2.1% | 3.7% / 2.1% |
| … `stealWork` / `netpoll` | 8.9% / 3.9% | 10.0% / 4.3% | 8.4% / 4.2% | 7.3% / 4.6% | 7.1% / 4.0% | 6.5% / 4.1% |
| `selectgo` | 7.6% | 7.8% | 7.6% | 8.2% | 7.8% | 8.9% |
| socket syscalls, all | 21.0% | 20.2% | | 23.0% | 25.4% | |
| client transports' workers (`connectHandlerWorkers`) | 16.9% | 15.8% | | 17.2% | 18.8% | |
| exchange links (`ExchangeConnection.Run`, `handleExchangeConnection`) | 16.2% | 15.0% | | 19.1% | 19.2% | |
| residents and forwards | 17.4% | 17.4% | | 18.4% | 17.9% | |
| front HTTP (`net/http` serve, h2) / of which route handling | 6.8% / 2.5% | 6.3% / 2.2% | – / 2.2% | 5.9% / 2.7% | 5.7% / 2.1% | – / 2.7% |
| TLS records written / read | 5.0% / 4.2% | 4.3% / 3.9% | 4.9% / 4.3% | 4.6% / 4.1% | 5.8% / 4.7% | 4.9% / 4.5% |
| TLS server handshakes | 2.5% | 2.8% | 2.4% | 1.8% | 2.3% | 2.1% |
| QUIC (`quic-go`) | 3.0% | 3.0% | | 3.1% | 4.0% | |
| go-redis / pgx | 6.5% / 2.6% | 7.5% / 3.3% | | 8.0% / 2.9% | 8.2% / 3.0% | |
| connection announce / authorization lease | 2.1% / 0.7% | 2.8% / 1.1% | | 2.9% / 1.0% | 2.9% / 1.2% | |
| exchange message queues (`message_queue.go`) | 2.7% | 2.4% | | 2.8% | 2.7% | |

(Blank cells and dashes weren't tabulated. The g2/g3 pairs on edge-1 and edge-4 were taken to separate the block from
the host.)

- **The timer heap walks are back, and they come from g2.**
  - **Size:** on every host checked, the g2 process walks its timer heaps for 7.4–13.8% of its CPU. g1 and g3 on the
    same hosts spend 2.1–2.3%.
  - **What triggers them:** every walk is the ordinary one at `runtime/time.go:1012`, set off when a modified timer
    comes due (LBALTOPT2 §3.4). 60% of `adjust` is the per-timer state load.
  - **Who modifies timers, as on 11-20:**
    - socket deadlines, 56%: `exchangeDeadline.refresh`, `frontTrackedConn.SetWriteDeadline`,
      `readConnectH1PooledWithDeadline` and go-redis;
    - new timers, 27%;
    - resets, 17%, the h1 writer's ping timer first.
  - **The g2 blocks carry the same load as the others:** 31–34 cores, 3.9–4.0M goroutines and 61–64k h1 sessions a
    block. So the difference is how often a short-horizon modification comes due, not the heap's size alone.
  - **g2 also runs the most Go time per CPU second:** `go_cpu_classes_user` against process CPU is 1.27 on g2, against
    1.21 on g4 and 1.04–1.07 on g1 and g3 (07:53–08:13Z). g4 was not profiled: the validator gate had closed for the
    next boundary.
  - **Fleet-wide:** across the six profiles the walks average 6.6% of connect (`adjust`). That is about 8.5 of
    connect's ~130 morning cores.
- **The heaps still hold about 1.2M timers a process** (heap snapshot of edge-4 g2 at 07:59Z). After §8.1's sweep, the
  largest owners are:
  - **the model's backstop polls:** `StreamHopListener.run` (about 210k on g2 and 64k on g3) and
    `NetworkPeerListener.run` (22k). Each takes a fresh `time.After` per loop. LBALTOPT2 §8.1 named both as the sweep's
    next candidates;
  - **the forward write timer** (`ResidentForward.runWithResidentLookup`): 148k on g2, 37k on g3;
  - **the heartbeats LBALTOPT2 left alone:** the exchange pinger and the h1 writer's ping;
  - **quic-go's reset timers.**
- **Exchange links are the largest single structure:** 15–19% of a process.
  - About 10% of it is socket syscalls: `exchangeIOCountingWriter.Write` 4–6%, `writevFrames` 2%, reads 3.3–3.6%.
  - Fleet-wide, 731k frames a second cross processes each way, at 1.7 frames per syscall.
  - Beside them run 1.67M local frames a second (the D1 local pumps), 70% of exchange frames.
- **Allocation is no longer a lever here.** Each process allocates 80–95 MB/s (1.6–1.9 GB/s fleet-wide, down 29% from
  B), and GC is 3.0–3.4% of connect. What's left:
  - per-session setup (`addTransportWithEdge`, `addForwardWithReceive`, `bufio.NewReaderSize`);
  - TLS handshakes;
  - two formatting costs:
    - connect's own `encodeUuid` still formats through `fmt.Sprintf`, 0.5–0.9% of CPU (the sdk's twin was fixed in
      §4.3);
    - the resident record is gob-decoded with a new decoder per read (`model.loadResident`,
      `ExchangeBuffer.ReadHeader`), which compiles the type every time: 0.65–1.1%.
- **TLS handshakes are cheap now:** 1.8–2.8%, with resumption at 98–99.9% since R22. The identity limit's token checks
  don't register (under 0.05%).
- **Redis from connect:** go-redis is 6.5–8.2% of a process. By caller:
  - `ReadContractHoleLease` 1.4% (§10.3.7's largest Redis consumer);
  - the pub/sub readers 1.1%;
  - `AuthorizeStream` 1.0%;
  - `GetResidentForClient` 0.9%.
- **The edge hosts** (edge-4 census, 10 s at 08:10Z):
  - connect 34.4 of 48.9 busy CPUs, api 5.5, taskworker 4.4;
  - the Grafana stack 1.2 (Mimir 0.5, Loki 0.5), logging 0.1, and no nginx;
  - kernel time 36% of busy, softirq 10.6% of all CPU (804k NET_RX a second);
  - about 3.2 CPUs of softirq run in interrupt context outside any process. That's where the HMARK split and the NIC
    receive land.

#### 10.3.2 proxy (crisp, fireside)

Shares of a process, 30 s at 99 Hz, 07:31–07:33Z, against §2's 11-22 means. Rows overlap as in §2: GC assists sit
inside the allocating rows.

| Share of a process | crisp g3, new build (2.6 h) | crisp g10, old build (17.7 h) | fireside g3, new | fireside g10, old | §2 crisp / fireside |
|---|---|---|---|---|---|
| **GC, total** | 42.8% | 53.8% | 45.8% | 54.8% | 40.6% / 52.5% |
| affinity donor scan (`inheritAffinityClient4FromWithLock`) | **5.3%** | 0.0% | **8.6%** | 4.1% | 8.6% / 3.7% |
| flow reaper pass (`detachIdleFlows`) | 2.8% | 0.4% | 3.2% | 2.1% | 4.1% / 1.5% |
| sdk contract stats tracker | **0.2%** | 2.0% | **0.2%** | 2.4% | 2.7% / 4.4% |
| connect `emitContractStats` (the events) | 1.2% | 1.9% | 0.7% | 2.1% | |
| network-quality listener list (`CallbackList.Add`, `Remove`) | 1.9% | 2.9% | 1.0% | 1.4% | |
| `Framer.ReadHeader` | 2.2% | 3.0% | 2.2% | 2.3% | |
| utls `UConn.Write` | 3.4% | 2.5% | 2.4% | 2.2% | |
| send and receive sequences | 15.7% | 16.9% | 12.3% | 13.6% | 15.8% / 16.2% |
| platform H1 transports | 9.3% | 10.5% | 7.6% | 7.3% | 10.2% / 9.7% |
| TLS, all / ingress server handshakes | 10.2% / 2.4% | 8.6% / 1.2% | 12.2% / 4.5% | 9.8% / 3.0% | 8.9–10.0% / 1.5–2.3% |
| net/http's own TLS dials (`persistConn.addTLS`; x509 within) | 1.4% (1.1%) | 1.6% (1.0%) | 1.6% (1.0%) | 1.4% (0.8%) | |
| FindProviders2, in-process (page decode within) | 4.0% (1.2%) | 4.0% (0.5%) | 2.5% (0.4%) | 3.1% (0.5%) | 3.9% / 3.8% (1.8% / 1.5%) |
| ConnectControl, in-process (client-key decode within) | 3.1% (0.6%) | 4.2% (0.6%) | 2.3% (0.4%) | 3.0% (0.4%) | 3.6% / 3.5% (0.6%) |
| IP association clustering (`IpAssoc.updateClusters`) | 1.8% | 2.0% | 2.7% | 1.7% | |
| `detectBlackhole` | 2.2% | 2.4% | 2.3% | 2.0% | |
| gVisor | 5.9% | 4.2% | 5.2% | 5.6% | 3.6% / 3.9% |
| HTTP and SOCKS ingress | 2.4% | 2.2% | 2.5% | 2.8% | 1.8% / 2.1% |
| go-redis / pgx | 3.0% / 1.6% | 4.0% / 2.0% | 2.2% / 1.3% | 3.2% / 1.7% | 3.0–3.6% / 1.8–1.9% |
| contract origin subscription | 0.2% | 0.5% | 0.0% | 0.6% | |

- **GC is still the largest cost,** paced by the memory limits: crisp 8000 MiB, fireside 6500 MiB, unchanged. Both
  hosts ran 23.9 of 24 busy.
  - crisp's minimum available memory was 30.6 GB over 24 hours (18.3 GB over 7 days); fireside's was 13.8 GB
    (6.7 GB).
  - The owner held the limits until the two new hosts arrive (11-18), so this section sizes no change to them.
- **The donor scan hasn't gone everywhere.** §4.1's index removed the reads of expired members, and on crisp g10 the
  scan is now 0.0%. But a process that holds a busy device still pays 4–9% for the walk over its group: 85–90% of the
  scan's time is per-member iteration (the loop itself 2.3–4.3%, map lookups 1.4–2.7%). Evaluating the members young
  enough to donate is only 0.4–0.5%. This is §6 item 6's case (P1 below).
- **Allocation sites that GC multiplies,** with their assists:
  - **the process-wide network-quality listener list:** every window client and device client registers in it, and
    each add or remove copies the whole list, 8–10k entries a process. That's 128–160 KB a copy at 21–27 copies a
    second, so 3–4 MB/s, 4–5% of the proxy's allocation;
  - **the framer's header read:** a 4-byte array that escapes on every frame;
  - **utls's `UConn.Write`:** a defer inside its compare-and-swap loop allocates a defer record on every write;
  - **`sendingChannelCount`:** a slice per call.
- **The rest is steady.** The sequences, H1 transports, TLS and FindProviders2 hold their §2 shares. Ingress TLS
  handshakes are 1.2–4.5%, with admissions up 22%.

#### 10.3.3 api (services edges)

Two captures, 30 s at 99 Hz: edge-4 g2 at 07:36Z (2.07 cores, 5.0 h old) and edge-3 g3 at 07:38Z (2.07 cores, 4.9 h
old).

| Share of an api process | edge-4 g2 | edge-3 g3 | CPUOPT6 C (11-15, edge-4 g2) |
|---|---|---|---|
| `/connect/control` (`ConnectControlWithOwners`) | 29.0% | 32.5% | 30.4% |
| … `CreateContract` | 17.3% | 21.3% | 15.6% |
| the client-key record decode (`decodeStClientKeyHistoryRecord`, all callers; 2.1–2.8 points of it in `CreateContract`) | 2.5% | 3.4% | 0.5% |
| scheduler (`findRunnable`) | 14.4% | 15.9% | 13.7% |
| GC, all | 8.9% | 9.4% | 9.4% |
| the contract origin subscription (`newContractOriginNotifications` → `runBucket`) | 4.9% | 4.5% | 3.8% |
| fuzzy location search (`SearchLocal.aroundIdsRawN`) | **7.9%** | 0% | 5.3% |
| find-providers2 (score page decode within) | 3.9% (1.2%) | 3.3% (1.1%) | 3.7% (2.9%) |
| auth-client | 2.6% | 2.0% | 2.7% |
| JWT verification / the lag-guest gate's unverified decode | 2.0% / 1.6% | 1.2% / 1.4% | 2.3% / 2.6% |
| go-redis / pgx | 13.3% / 10.5% | 15.1% / 14.5% | 13.3% / 8.7% |
| network softirq on the api's threads | 16.3% | 10.5% | 16.8% |

- **The api costs more per request than on 11-22, and the request mix explains it.**
  - **N against B:** 16.5 against 14.4 cores, at 6.5k against 7.8k requests a second. That's 2.54 against 1.84 cores
    per krps.
  - **The cheap requests fell:**
    - `/connect/control` 5.2k → 4.2k/s: discards 2.7k → 1.7k/s, refusal replays 696 → 475/s;
    - `/hello` 1.9k → 1.4k/s;
    - since 11-22 07:33Z the front's identity limit refuses 0.4–0.9k requests a second, mostly api class, before they
      reach the api (LBALTOPT2 §9.3).
  - **The dear requests rose:**
    - find-providers2 26.7 → 51.7/s, at 15–29 ms each;
    - auth-client 166 → 216/s, remove-client 107 → 167/s;
    - find-provider-locations reached 1.26/s, at 263 ms mean on eight goroutines.
- **The client-key record decode grew fivefold:** 0.5% on 11-15, 2.5–3.4% now. Each contract creation decodes the
  current key record's JSON envelope, verifies its secp256k1 evidence signature and recomputes its canonical bytes,
  for a record that changes only when the key is registered again.
- **The origin subscription receives every committed contract origin in the fleet.** 1,720 are published a second, so
  34.4k deliveries a second reach the 20 api processes (and 16.2k the proxy's), each read by its own socket read. That
  is 4.5–4.9% of every api process.

#### 10.3.4 taskworker (services edges)

edge-3 g2 at 07:38Z, 2.27 cores, 4 h old. The other taskworker capture, edge-4 g2, caught 0.5 cores.
- **The egress probes** (the connect client stack and its frames): about 44%.
- **The unique-exit sweep (`SweepProxyExits`): 15.1%.** Nearly all of it is `FindProviders2` per replacement draw; in it,
  `selectClientScoresByWeight` → `connect.weightedSelectFuncWithEntropy` is 6.7%. CPUOPT6 C #9's cursor and the held
  back-off (14a3b941e) shipped; what's left is the draws themselves.
- **net/http's own TLS dials (`persistConn.addTLS`): 8.9%, with x509 8.2% within.** This is the stock-TLS chain
  verification CPUOPT6 C #4 found, unchanged.
- **Other tasks:** `UpdateClientLocations` 3.6%, `FlushTransferDebits` 3.3%, in-process control 4.6%. GC 9.3%.
- **The score export** wasn't running in this process. Its gob encode goes when CPUOPT8's flag turns on.

#### 10.3.5 `/connect/control`: what it costs, and whether its climb is a candidate

A question raised during this round. After the 07:51–07:56Z drain, api requests rose about 7.2k → 7.5k a second
(`/connect/control` +290, `/hello` +55), and edge-2 went from 0.51–0.61 to 0.64–0.68 busy. The old sawtooth (the
control rate climbing with connect's uptime) suggests the route itself as a candidate.

- **The 11-15 sawtooth is gone.**
  - **What it was:** stuck providers' refusal replays, 0.1 → 7.2k a second over a connect generation.
  - **Since the recycle** (CPUOPT6 C #1, baked in by b896cf6d9), replays run 0.36–0.82k a second. After 11-22's 07:40Z
    roll they climbed only from 0.36k to 0.63k over six hours.
  - **Their cost:** a refusal replay takes about 0.65 ms of api CPU and no database work, so about 0.3 api cores now.
- **What the route carries now** (hourly, 11-22 02Z to 11-23 08Z): 3.9–5.5k requests a second.
  - 1.6–2.8k are audit-only discards answered before authentication;
  - 0.36–0.82k are refusal replays;
  - 2.2–3.2k carry frames, nearly all contract creations (1.9–2.8k completed a second).
  - It follows the day, highest in the evening and lowest at 03–05Z.
  - Today's rise after 07:00Z is the morning plus the new connect generation's providers creating their contracts
    again. It isn't replays, which held flat at 405–418 a second.
- **What it costs the api:** the route's handler is 29–33% of an api process (§10.3.3).
  - That's about 7 cores of the api's 22–23 in the morning, about 10 with its share of HTTP serving, the scheduler and
    GC.
  - Per request it's about 1.5 ms in the handler, and per creation about 3 ms.
  - Inside it: `CreateContract` 17–21% (the Redis admission and the companion escrow transaction about 10%, the
    companion-origin wait, the client-key decode 2.1–2.8%), authentication 6.5%, and admission and preparation about 4%.
- **What it costs PostgreSQL:**
  - The creation path's statements are about 2.3 of 20.8 backends of execution (11%), plus their BEGIN, lock-bound and
    COMMIT round trips: the contract insert 1.01, the escrow insert 0.62, the contracting clients' key-share lock 0.23,
    the provide relationship read 0.21 and the client contract metadata read 0.20.
  - The api makes 77–80% of the creations (2.0–2.2k a second, against the proxy's 0.44–0.48k and the taskworker's
    0.15k), and 35–37% of all pool acquires.
  - At edge-2's 0.42 cores per 1k PgBouncer queries a second, the api's control route is about 5–8 edge-2 cores
    *(inferred from those shares)*.
- **Is it a candidate?** Not as a route.
  - Each creation is a contract the protocol needs, and the replays that made the sawtooth no longer cost anything that
    matters.
  - Its parts are candidates, and those items carry its saving:
    - the client-key decode (A1);
    - the origin delivery behind the companion wait (X1);
    - Lazy BEGIN (D2);
    - the proxy's in-process creations' allocations (P3).

#### 10.3.6 PostgreSQL (edge-2)

| Window | Busy (user / sys / softirq) | Commits/s | PgBouncer queries/s (transactions/s) | **Cores per 1k queries/s** | Pools at cap (of 32) | connect connections created/s |
|---|---|---|---|---|---|---|
| CPUOPT6, 11-15 02–10Z | 41.3 (27.0 / 10.1 / 4.2) | 42.1k | 101.5k (39.2k) | **0.41** | 10.9 | 5.9 |
| A, 11-16 | 41.9 (27.4 / 10.5 / 4.0) | 41.2k | 99.5k (38.0k) | **0.42** | 16.6 | 7.6 |
| B, 11-22 | 46.0 (33.2 / 9.3 / 3.5) | 42.8k | 107.5k (39.8k) | **0.43** | 14.7 | 10.3 |
| N, 11-23 | 45.2 (32.4 / 9.3 / 3.6) | 38.6k | 110.3k (35.5k) | **0.41** | 18.4 | 9.4 |
| 07:03–08:03Z | 56.2 (40.1 / 11.5 / 4.6) | 45.2k | 132.3k (42.5k) | **0.42** | 25.9 | 42.5 |

- **Per PgBouncer query, edge-2 costs what it did on 11-15:** 0.41–0.43 cores per 1k queries a second in every window.
  Per commit is misleading now. R18 (connect from 11-22 06:27Z) batched the announce loop's reads into fewer
  transactions: connect's acquires fell from 16.9k to 10.4k a second and commits by about 4k, while queries held.
- **The nginx stream hop is gone** (the direct path, stages 2–4 on 11-15/16): 5.07 → 0.01 cores. PgBouncer costs 59–61
  µs a query (63.6). But statements got heavier by about as much: 124 µs of execution each, against 91–100 on 11-15.
- **By process** (four 20 s censuses, 07:32–07:57Z):
  - backends through PgBouncer 34.0–41.9 cores (29.7 on 11-15) in 679–700 processes;
  - PgBouncer 7.4–8.4;
  - direct backends 1.8–3.3;
  - autovacuum 0.2–0.6;
  - the nginx hop 0.01.
- **Where execution goes** (pgss 07:32–08:03Z: 168k statements a second, 20.8 backends of execution against CPUOPT6's
  12.3, about 19.5 backends on CPU in the activity samples):

| Statement | Code | Backends | CPUOPT6 |
|---|---|---|---|
| endpoint fence, `pg_advisory_xact_lock(776, …)` | model/provider_work_session.go `providerWorkLockSessionMutationHeadInTx` | 1.34, mostly **lock waits** (0.78 ms a call) | 0.035 |
| close `UPDATE transfer_contract SET outcome` | model/subscription_model.go `claimContractOutcomeWithUsageValueInTx` | 1.30 | 0.73 |
| close-report upsert | model/subscription_model.go `applyLockedContractCloseReportInTx` | 1.19 | 0.63 |
| **SN25 ledger amounts for a network** (GET /account/epochs) | model/st_ledger_v2_reader_model.go `GetStLedgerV2AmountsForNetwork` | **1.17, all CPU** (0.41/s at 2.87 s) | 0.37 |
| contract insert `WITH creation_clock` | model/subscription_redis_admission.go `createRedisTransferEscrowInTx` | 1.01 | 0.63 |
| descendant check | model/network_client_model.go `readActiveChildClientIdsInTx` | 0.86 (2.0 ms, 773 hits) | 0.47 |
| escrow insert | model/subscription_redis_admission.go | 0.62 | 0.28 |
| provider payout attribution (GET /stats/providers) | model/provider_payout_model.go `queryProviderPayoutStats` | 0.46 + 4 parallel workers (37 s a call) | 0.009 |
| **debit flush** | model/subscription_transfer_debit.go `flushTransferDebitBalance` | **0.38** (1.17 ms, 411 hits a call) | 0.015 |
| contract horizon reaper (all its statements) | model/contract_horizon_reaper.go, contract_horizon_zero_byte.go | 0.37 | 0.08 |

- **Families:** the contract lifecycle (transfer_contract 3.26, contract_close 2.37, escrow 2.07, debit 0.84 backends)
  is 41%. network_client, provide keys and connections are 5.34. SN25's ledger and wallets are 1.37, the provider-work
  fences 1.34, provider stats reads 0.76.
- **New since CPUOPT6:**
  - **the debit flush:** it probes all seven daily partitions of transfer_debit_journal (11-16), including drained ones.
    The (contract_id, balance_id) index of an empty day takes 338 scans a second at 383 hits and no tuples each;
  - **the provider stats reads:** each call takes 14–53 s, against 0.16–0.79 s on 11-15;
  - **the endpoint fence's lock waits;**
  - **the SN25 read:** 0.37 → 1.17 backends, because its table grew 701k → 928k rows.
- **The reaper** (the zero-byte pass at about 1,924 contracts scanned a second, 485 deleted):
  - **Cost:** 0.37 backends of execution and 2.6 MB/s of WAL. Per contract deleted that's 0.76 ms, 5.4 KB of WAL, 55
    hits and 3 reads.
  - **At 3,000 a second:** about +0.2–0.3 cores, WAL, checkpoint and vacuum included *(inferred, linear)*.
- **connect's PostgreSQL churn rose after the 06:50Z roll:**
  - **Rates** (15-minute, overnight → 07:09Z → 08:09Z): pool connections created 7–15 → 33 → 54 a second; canceled
    acquires 3–20 → 178 → 106; "empty" acquires 50–1,000 → 3,343 → 857.
  - **No idle or lifetime destroys are counted,** so the extra connections are discarded on errors and cancels.
  - **On edge-2:** PgBouncer logins 25–61 a second, and server parses about 1.0k a second against 247 in N.
  - **Cause:** it may be the new generation's warm-up and the morning, not the build. Its per-backend CPU showed no
    warm-up excess. connect's deployer has the numbers.

#### 10.3.7 Redis (edge-5, edge-6) and MinIO

| Window | Redis cores, both hosts (main threads) | Requests/s | Commands/s | µs of Redis CPU per request / per command | Softirq, both hosts | MinIO |
|---|---|---|---|---|---|---|
| CPUOPT6, 11-15 06–10Z | 38.0 (15.3) | 343k | 1,373k | 111 / 27.7 | 10.6 | 5.3 |
| A, 11-16 | 24.9 (11.4) | 194k | 834k | 128 / 29.8 | 6.6 | 3.7 |
| B, 11-22 | 23.7 (11.8) | 181k | 860k | 131 / 27.5 | 5.6 | 2.2 |
| N, 11-23 | 24.6 (12.3) | 184k | 858k | 133 / 28.7 | 5.9 | 2.0 |
| 06:29–07:29Z (40 connect subscribers during the drain) | 28.4 (14.2) | 216k | 928k | 132 / 30.6 | 7.2 | 2.0 |

- **CPUOPT6 E's items landed close to their predictions.**
  - **The storm fix:** Redis −13.0 cores (predicted 15–17), softirq −4.5. SPOP fell 52.9k → 2.2k a second, and connect's
    dials 783 → 24.
  - **The dropped preflight PING:** −1.75 cores (predicted 1.8–2.2).
  - **MinIO's evidence sharding and flat move:** MinIO 5.3 → 2.0 cores (predicted 3–4.5).
  - **edge-6's IRQ change:** the busiest CPU's softirq went 51% → 7% and CPU pressure 34.6% → 1–5%. The predicted
    1.7–4.1 cores didn't appear: the gain is latency.
- **What Redis CPU pays for.** A fit over 955 five-minute windows, 11-20 to 11-23 (R² 0.93 and 0.97), gives per unit:
  - a request, 78–88 µs;
  - a script's extra, 64–67 µs;
  - a pub/sub delivery, 14–20 µs;
  - softirq, 24 µs per request and 12.5 µs per delivery.

  At N that's requests 15.5 cores, the script premium 6.2 and deliveries 1.7, plus softirq 4.4 + 1.3. Commands' own
  time is only 7.7 of the 24.6 cores.
- **The largest single consumer is connect's contract-hole read:**
  - **Rate and cost:** 24.3k EVAL a second, about 4.2 host cores.
  - **Code:** `model/subscription_contract_hole.go: ReadContractHoleLease`, from
    `connect/resident_contract_manager.go: HasActiveContract`.
  - **What it does:** a 1.3 KB script runs GET, two PTTLs, ZCARD, TIME, a ZREMRANGEBYSCORE (a write) and ZREVRANGE per
    active forwarding pair, about every 2.5 s.
- **A third of Redis's socket writes are pushes,** about 100k a second.
  - **Contract-origin notifications:** 1.57k a second fan out to 46.8k deliveries (api 31.3k, proxy 15.5k).
  - **Key events connect throws away:** 28.3k `expire` a second.
  - **Key events the proxy throws away:** 8.35k `expire` a second on `{*}c_removed`.
- **Still open from CPUOPT6 E:**
  - **EVAL → EVALSHA:** script bodies are about 81 MB/s, 42% of what Redis receives;
  - **GETEX for the resident heartbeat;**
  - **publishing the rewritten sets only on change:** node 6415 still blocks for about 115 ms every ~14.5 s.
- **edge-6's system time is 11–15 cores, against about 6 on 11-16 to 11-18.** That isn't Redis: it's the ZFS write
  threads replaying the 11-19 PostgreSQL base backup (105–170 MB/s since 11-20). Redis slow entries there run 7–24k an
  hour, against 70–160. The ZFS taskq parameters staged on 11-13 take effect at edge-6's next planned reboot.

#### 10.3.8 The rest

- **mcp:** 0.35–0.42 cores across 20 processes. **gossip:** 0.40–0.43 across 4. **competitionworker:** 0.02. Nothing
  to take.
- **The Grafana stack** on each services edge: Mimir 0.5, Loki 0.5, Grafana and warp-grafana 0.2 (edge-4's census).
  Each proxy host carries about 0.8–1.0 cores of non-proxy work, Loki and Mimir included.
- **The lb units:** since LBALT the front is connect's, so there's no nginx CPU left on the edges. The `--lbaltporthash`
  split is an iptables HMARK, which runs in the kernel's receive path: part of the 3.2 CPUs of interrupt-context
  softirq per edge above.

### 10.4 CPUOPT8's verdict so far

- **Contract stats only for listeners** (sdk; live on D71's blocks from 04:55Z, g3 first):
  - **In the perf A/B** (the same 30 s at 07:31–07:33Z): the tracker's emit fell from 2.0–2.4% of a proxy process
    (old-build g10 on each host) to 0.2% (new-build g3).
  - **Per block at 07:55Z,** against the old-build blocks on the same host: crisp collected 6.1–6.3 times a minute
    against 8.1–8.7 (one old block, g8, at 6.2), and fireside 9.0–9.3 against 10.0–11.6. They did that at as many or
    more window flows a second, with the same heap plus stacks (7.8 GB and 6.2–6.4 GB).
  - **Against the prediction:** that is 17–25% fewer collections, about what CPUOPT8 §4 expected from the emit's
    allocation. The new blocks are 1.6–2.8 h old against 17.9 h, though, so part of it is restart relief.
- **Typed score pages: not yet.**
  - Every page is still gob: `urnetwork_client_score_page_encodes_total{format="gob"}` ran 214 a second and typed 0 at
    07:40Z.
  - The readers are in place: the api decodes 179 pages a second and the rolled proxy blocks 52, all gob, none failing.
  - The verdict comes with D71's flag step, by CPUOPT8 §5's steps 4–5. Measure proxy and api GC a minute, allocation
    and page decodes by format before the flag and 30 minutes after it.

### 10.5 Ranked opportunities

Ranked by expected cores at this morning's load. Measured means the share comes from a profile or counter and the
saving from removing part of it. *Inferred* means a model or a share of a share. The items overlap where noted, so
don't add them all.

| # | Item | Service, host, process | Evidence | Saving (cores) | Risk |
|---|---|---|---|---|---|
| 1 | C1. Find what sets off connect's timer-heap walks in g2, and take the model's backstop polls off the runtime heap | connect, edge-0/1/3/4, every g2 process (and the heaps of all) | `adjust` 7.4–13.8% of g2, 2.1–2.3% of g1/g3; about 8.5 cores fleet-wide | **3–7** *(inferred)* | low (measure), low–medium (fix) |
| 2 | X1. Deliver contract-origin edges only to their waiters | api (20), proxy (20), Redis (edge-5/6) | api 4.5–4.9% a process; 46.8k pushes/s for 1.6k publishes | **2.5–3** (api 1.1, proxy 0.15, Redis host 1.2–1.5) | medium–high |
| 3 | P1. The donor scan visits only the last minute's entries | proxy, crisp and fireside, processes with a busy device | 0–8.6% a process, 4.5% mean; 85–90% of it the walk | **1.5–2** | low–medium |
| 4 | C2. connect's id text without `fmt`, and the resident record without a per-read gob decoder | connect, every process | `encodeUuid` 0.5–0.9%, gob 0.65–1.1% | **1.3–2** | low (`encodeUuid`), low–medium (the typed record) |
| 5 | P3. Per-frame allocations: the framer's header, utls's `UConn.Write` defer, `sendingChannelCount` | proxy, every process | 2.7–4.0% a process with assists | **1.2–1.8** | low |
| 6 | A2. An index (or answer cache) for the fuzzy location search | api, every process | 1.26 queries/s at about 2 core-seconds each (0.15/s on 11-15) | **1–2** *(inferred)* | low–medium |
| 7 | A4. GOMAXPROCS 8 → 4 for the api, A/B on one block (approved 11-13, never run) | api | scheduler 14–16% of a process | **1–2** *(inferred)* | low–medium |
| 8 | T1. net/http's own TLS dials verify through the verified-chain cache | taskworker probes; proxy DoH | x509 8.2% of a probe process; 1.0–1.1% of a proxy process | **about 1.3** *(inferred)* | low–medium; owner's probe review |
| 9 | D1. The SN25 attribution read from a per-epoch table | PostgreSQL, edge-2; api's GET /account/epochs | 1.17 backends, all CPU | **1.1–1.2** | low |
| 10 | D2. Lazy BEGIN | PostgreSQL, edge-2, and PgBouncer; every service | 14.2k BEGIN round trips/s | **1.0–1.4** *(inferred)* | medium |
| 11 | P2. The network-quality listener list without copy-on-write | proxy, every process (and the apps) | 1.0–2.9% a process; 4–5% of allocation | **0.8–1.4** | low |
| 12 | A1. Remember decoded client-key records | api, proxy, taskworker | api 2.5–3.4%, proxy 0.35–0.63%, taskworker 0.9% | **about 1** | low |
| 13 | R1. The contract-hole read without its script's prune | Redis; connect's residents | 24.3k EVAL/s, about 4.2 host cores | **0.6–1.1** Redis *(inferred)* | medium |
| 14 | R2. One notice per client removal instead of three key events to every subscriber | Redis; connect and proxy | 33k of 100k pushes/s | **0.6–0.9** host, plus receivers | low–medium |
| 15 | D4. Fewer round trips in a contract close | PostgreSQL, edge-2; connect | 11 sequential statements per close at 1.7k closes/s | **0.6–0.9** *(inferred)* | medium |
| 16 | P4. The flow reaper visits due and cancelled flows only | proxy, every process | 0.4–3.2% a process, 2.1% mean | **0.6–0.9** | medium |
| 17 | D6. Hourly rollups for GET /stats/providers | PostgreSQL, edge-2; api | 0.76 backends + 4 workers, about 100 MB/s of reads | **0.6–1.0** *(inferred)* | medium–high; the author's or owner's |
| 18 | D5. The descendant check from a partial index of active clients | PostgreSQL, edge-2 | 0.86 backends | **0.6–0.75** *(inferred)* | medium |
| 19 | R3. EVAL → EVALSHA | Redis; connect, api, taskworker | about 81 MB/s of script bodies | **0.35–0.7** *(inferred)* | low |
| 20 | D3. The debit flush names its shard, and drained days go sooner | PostgreSQL, edge-2; taskworker | 0.38 backends, from 0.015 | **0.3–0.4** | low |
| 21 | R4. GETEX in the resident heartbeat | Redis; connect | 3.4k requests/s | **0.35** *(inferred)* | low |
| 22 | A5. The lag-guest gate without a claims decode | api | 1.4–1.6% a process | **0.3** | low |
| 23 | D8. The health aggregate once per export | PostgreSQL, edge-2; taskworker | 0.20 backends + workers | **0.2–0.5** | low–medium |
| 24 | D9. Fewer claim probes per task | PostgreSQL, edge-2; taskworker | 1.16k probes/s | **0.2–0.4** *(inferred)* | low–medium |
| 25 | R5. Provider appearances delete only their own stale minutes | Redis; api, proxy | 7.2k HDEL/s of 70 fields | **0.15–0.2** *(inferred)* | low |
| 26 | R6. Publish the rewritten sets only on change | Redis node 6415; taskworker | a 115 ms whole-node block every ~14.5 s | latency (about 0.02) | low |

**Overlaps:** X1 and R2 both remove pushes; R1 and R3 share the contract-hole script; A1, X1 and D2 all sit inside
`/connect/control` (§10.3.5); D2 and D4 both remove round trips. Together the items come to roughly 23–35 cores, about
a tenth of the fleet.

#### C1. connect's timer-heap walks (rank 1)

- **Where:** connect, every g2 process on edge-0/1/3/4 at 7.4–13.8% of its CPU, and 2.1–2.3% elsewhere.
  - **The walk:** Go's `runtime.(*timers).adjust` at `runtime/time.go:1012`, which runs when the earliest modified
    timer on a P comes due.
  - **Modifiers:** server `connect/resident.go` (`exchangeDeadline.refresh`, `sendPooledMessage`'s `timer.Reset`),
    `connect/front_http.go` (`frontTrackedConn.SetWriteDeadline`), `connect/transport.go`
    (`readConnectH1PooledWithDeadline`, and the h1 writer's ping in `ConnectHandler.Connect`), and go-redis and pgx
    deadlines.
  - **Heap owners:** `model/stream_model.go` (`StreamHopListener.run`) and `model/peer_model.go`
    (`NetworkPeerListener.run`), each a `time.After` per loop, and `connect/provider_intent.go`
    (`providerIntentCheck.Run`).
- **Evidence:** §10.3.1. The g2 blocks carry the same load as g1 and g3, so something in g2 modifies a timer with a
  short horizon far more often. A profile can't say what; LBALTOPT2 R14(a) named the measurement and it was never
  built.
- **Change, in two steps:**
  1. **Measure.** Add a bounded, sampled counter of timer modifications by call site and horizon bucket (under
     100 ms, 1 s, 10 s, more), in one canary build on g2, as R14(a) proposed. It changes no behavior.
  2. **Remove the trigger it finds,** if a timer can keep its deadline in place (CPUOPT4's pattern: arm once, check on
     fire), and put the model's two backstop polls and the provider-intent check on `internal/timersweep` (LBALTOPT2
     §8.1). They fire at their deadline or at most one 100 ms tick after, never before, on jittered 5–10-minute
     backstops to key events. That takes about a fifth of a g2 process's timers off the runtime heap.
- **Saving:** the g2 excess alone is about 9 points of four processes' 34 cores, about 3 cores. A fifth smaller heap
  cuts every walk by a fifth or more (§8.1's benchmark: 42% fewer timers, 60–64% less walking). **3–7 cores**
  *(inferred)*.
- **Risk:** low for the counters. For the fix, low to medium depending on what it finds; heartbeats and socket
  deadlines stay native (the 2026-10-30 rule).
- **Tests:**
  - the counters: a deterministic test that each instrumented site counts once per modification, in its horizon
    bucket;
  - the polls on the sweep: §8.1's owner tests (a synctest bubble with the deadline between two ticks: fires at the
    deadline or at most a tick after, never before; the timer leaves the sweep when the owner ends);
  - `TestConnectionTimerCensus` extended to the listeners: no runtime timers per resident for them.

#### X1. Contract-origin edges only to their waiters (rank 2)

- **Where:** server `model/contract_origin_notification.go`.
  - `runPublisher` SPUBLISHes every committed origin to one of 16 bucket channels.
  - `runBucket` and `RunRedisShardSubscription` deliver every message to every subscribing process.
  - `notifyBucket` then finds no local pair for most of them.
  - The waits it serves are `controller.waitForCompanionOrigin` in the api and the proxy's in-process control.
- **Evidence:**
  - **Fan-out:** 1.6–1.7k origins published a second become 46.8k deliveries a second (api 31.3–34.4k, proxy
    15.5–16.2k). That's about 30 per publish, to serve about 850 waits a second (`origin_ready` 816, `missing_origin`
    31).
  - **Cost to the receivers:** 4.5–4.9% of every api process, nearly all of it one socket read per message (go-redis
    `PubSub.ReceiveTimeout`), and up to 0.6% of a proxy process.
  - **Cost to Redis:** by §10.3.7's fit, 14–20 µs a delivery plus 12.5 µs of softirq.
- **Change:**
  - First, count a `matched` outcome on `urnetwork_contract_origin_notifications_total`, to confirm how few deliveries
    find a waiter.
  - Then a waiter registers its pair with its process's channel, under the bucket's hash tag, before its authoritative
    read. So does a process that holds a route refusal for the pair, because the edge reopens that route
    (`notifyBucket` → `routeRejections.invalidate`). The committer, after its commit, publishes in one script only to
    the registered processes' channels, and publishes nothing for a pair nobody waits on or refuses.
  - Register-before-read against commit-before-check means a waiter either sees the commit in its PostgreSQL read or
    is notified. The PostgreSQL read stays authoritative, and every wait bound (100 ms, 500 ms, 3 s) stays as it is.
- **Saving:** api 1.1, proxy 0.15 and Redis host 1.2–1.5 cores: **2.5–3** *(the Redis part inferred)*. CPUOPT4 C #6
  proposed the same at 0.5 api cores.
- **Risk:** medium–high. It needs an ordering proof and a design review; the refusal-ring fences must keep their order.
- **Tests:**
  - a deterministic interleaving test of register, read, commit and publish, in every order, that never strands a
    waiter past its bound;
  - an expired or lost registration falls back to today's bounded PostgreSQL read;
  - a wake reaches only registered processes;
  - the route-refusal fence tests unchanged.

#### P1. The donor scan visits only the last minute's entries (rank 3; §6 item 6)

- **Where:** proxy, every process that holds a busy hosted device. connect `ip_remote_multi_client.go`:
  `inheritAffinityGroupClient4WithLock` → `inheritAffinityClient4FromWithLock`, and the v6 twin.
- **Evidence:** 0.0–8.6% of a process in the four captures, 4.5% on average, about 2 of the 46 proxy cores.
  - 85–90% of what's left is per-member iteration over every flow in the group within its idle timeout: the loop's
    own time 2.3–4.3%, and its map lookups 1.4–2.7% (`mapaccess2` on the 4-tuple key, the stale-set lookup that every
    expired member still pays).
  - Evaluating the young members is 0.4–0.5%.
- **Change:**
  - Per affinity group, keep an append-only ring of (tuple, entry time), written by the same canonical writers
    (`setIp4PathUpdateWithLock`, `deleteIp4PathUpdateWithLock`) whenever they write a group entry.
  - The indexed scan walks the ring from the newest record back to `now − AffinityMaxTime`, skipping a record whose map
    entry has since been rewritten. It then visits the group's stale entries (`staleAffinityIp4Paths[group]`, already
    kept), and trims the records older than the window as it passes them.
  - The candidate set is exactly today's (every member younger than `AffinityMaxTime`, plus the stale ones). So the
    donor is the same, up to the tie order that map iteration already randomizes. Pinned placements keep the full scan.
- **Saving:** **1.5–2 proxy cores** at today's mix.
- **Risk:** low–medium. It is placement code, with §4.1's equivalence harness already in place.
- **Tests:**
  - `TestAffinityGroupScanMatchesFullScan` (600 seeded steps per IP version through the production writers) extended to
    the ring, failing if a young member or a stale entry is missed;
  - a rewritten tuple's superseded ring record is ignored;
  - the ring holds no more than the last minute's writes;
  - `BenchmarkAffinityGroupScan` with 5,000 members, 50 of them young.

#### C2. connect's id text and resident decode (rank 4)

- **Where:** connect, every process.
  - connect `id.go: encodeUuid` formats with `fmt.Sprintf("%x-%x-%x-%x-%x", …)` over five boxed slices, reached from
    `server.Id.String`.
  - server `model/network_client_resident_model.go: loadResident` and `connect/resident.go: (*ExchangeBuffer).ReadHeader`
    build a `gob.NewDecoder` for each record. Gob compiles the type for each new decoder: `compileDec` is 0.86% of a
    process.
- **Evidence:** `encodeUuid` 0.5–0.9% of a process and about 1.9 GB of every 62 GB allocated; gob decoding 0.65–1.1%.
- **Change:**
  - Port §4.3's sdk fix to connect's `encodeUuid`: hex into a fixed buffer, the same canonical lowercase text.
  - Give the resident record and the exchange header a typed encoding beside gob, readers first, as CPUOPT8 did for
    score pages: every reader accepts both, and the writers switch only once every connect process runs the build.
- **Saving:** about 80% of both: **1.3–2 connect cores**.
- **Risk:** low for `encodeUuid`; low–medium for the typed record, which must read both formats through a roll.
- **Tests:**
  - `encodeUuid` over 10,000 ids against the fmt form, and one allocation;
  - the resident record and the exchange header: over seeded values, the typed decode equals a gob round trip field by
    field, and a new reader reads an old writer's gob;
  - a field guard that fails when either struct gains a field the codec doesn't write;
  - malformed input is refused, never read as another value.

#### P3. Per-frame allocations (rank 5; CPUOPT6 B #5, now larger)

- **Where:** the proxy's window clients, every process.
  - connect `message_framer.go: (*Framer).ReadHeader` reads into a `var h [4]byte` that escapes through `io.ReadFull`:
    one heap object per frame read.
  - utls v1.8.2 `u_conn.go: (*UConn).Write` puts `defer c.activeCall.Add(-2)` inside its compare-and-swap loop, so
    every write allocates a defer record. utls's own `(*Conn).Write`, like Go's, defers after the loop.
  - connect `ip_remote_multi_client.go: sendingChannelCount` builds a slice of the clients on every call.
- **Evidence:** with their GC assists, ReadHeader's allocation is 1.1–1.4% of a proxy process, `UConn.Write`'s 1.1–1.9%
  and `sendingChannelCount`'s 0.5–0.75%: 2.7–4.0% together.
- **Change:**
  - a header buffer owned by the framer's read side (its contract already allows one reader);
  - the defer moved after the loop in a carried utls patch (carried as quic-go is, and offered upstream);
  - a scratch slice reused by `sendingChannelCount`.

  No answer, frame or record changes.
- **Saving:** **1.2–1.8 proxy cores**, plus a little in connect (whose framer allocation is 0.17%).
- **Risk:** low.
- **Tests:**
  - `testing.AllocsPerRun` is 0 for `ReadHeader` over a bytes reader, for a small write on a handshaken `UConn` beyond
    the record's own buffers, and for `sendingChannelCount`. Each fails today.
  - The framer, transport and utls handshake tests stay green.

#### A2. The fuzzy location search (rank 6; CPUOPT6 C #8, its rate up eightfold)

- **Where:** api, `POST /network/find-provider-locations`. server `search/search_local.go: (*SearchLocal).aroundIdsRawN`
  scans every alias of every location at the lengths within the distance, with a histogram check each and an edit
  distance for the survivors, on 8 goroutines.
- **Evidence:** 1.26 queries a second fleet-wide (0.15 on 11-15) at 263 ms mean on 8 goroutines. One capture caught
  7.9% of its process in it, the other none.
- **Change:** CPUOPT6 C #8's candidate index (aliases bucketed by length and a histogram signature, so that only the
  buckets within the distance are scanned), with the scan kept as the reference. A cheaper first step is an answer
  cache keyed by query, distance, limit and the index's generation, judged by its hit counter. The scan's answer
  depends on nothing else, and its result limit already makes the parallel scan's pick among equals depend on
  scheduling.
- **Saving:** **1–2 api cores** *(inferred)*, and lower latency for the route.
- **Risk:** low–medium. The results must stay the scan's.
- **Tests:** a property test over a generated corpus at distances 1–3: the indexed search returns the scan's results
  and distances, ties included; a generation change invalidates the cache.

#### A4. GOMAXPROCS 8 → 4 for the api, on one block (rank 7; approved 11-13, never run)

- **Where:** api, any block. The per-block `cores` override exists (warp fc8912f2f).
- **Evidence:** `findRunnable` is 14.4–15.9% of an api process, plus idle parking, on 8 Ps for 0.5–2.1 cores of work.
- **Saving:** **1–2 api cores** *(inferred)*.
- **Risk:** low–medium, with `gmp_apply.sh`'s 72 h auto-revert watcher.
- **Tests:** none in code. The A/B compares the block's cores per krps and route latency with the other blocks.

#### T1. Stock-TLS dials through the verified-chain cache (rank 8; CPUOPT6 C #4)

- **Where:** net/http's own TLS dials (`persistConn.addTLS`), most likely the per-tunnel and per-device DoH clients
  (connect `net_http_doh.go`), which D4(b)'s chain cache does not cover.
- **Evidence:** x509 is 8.2% of a probe-running taskworker process (about 0.9 taskworker cores), and `addTLS` 1.4–1.6%
  of a proxy process, with x509 0.8–1.1% within (about 0.45 proxy cores).
- **Change:** as CPUOPT6 C #4.
  - First, a per-transport handshake counter.
  - Then verification through D4(b)'s process-wide verified-chain cache: keyed on the exact certificate bytes, server
    name and root pool; hostname, validity and key possession checked on every handshake; misses verified in full;
    errors never cached. Every handshake stays a full one.
- **Saving:** **about 1.3 cores** *(inferred)*.
- **Risk:** low–medium: a verification path. The probe part needs the owner's probe review.
- **Tests:** as CPUOPT6 C #4.

#### D1. The SN25 attribution read (rank 9; CPUOPT6 D #7, now edge-2's largest CPU statement)

- **Where:** server `model/st_ledger_v2_reader_model.go: stLedgerV2AttributedAmounts`, `GetStLedgerV2AmountsForNetwork`,
  from `controller/sn_earnings_controller.go: AccountEpochs` (GET /account/epochs).
- **Evidence:** 0.41 calls a second at 2.87 s and 30.8k hits each: 1.17 backends, all on CPU.
  `st_payout_wallet_resolution` holds 3 epochs and about 80 coldkeys, so each call sorts all 928k rows to rebuild
  about 240 (epoch, coldkey) → network rows. The table is insert-only and grows every epoch.
- **Change, exact:** the resolution writer (`model/st_payout_wallet_resolution_batch.go`) also keeps a small (epoch,
  coldkey) → network-of-the-lowest-client-id table, and the reader joins it.
- **Saving:** **1.1–1.2 edge-2 cores**, measured.
- **Risk:** low.
- **Tests:**
  - equality with today's SQL on seeded epochs with several clients per coldkey across networks, ties broken by client
    id;
  - a guard that the per-network read never scans `st_payout_wallet_resolution` (fails today).

#### D2. Lazy BEGIN (rank 10; CPUOPT6 D #3)

- **Where:** server `db.go: beginSqlWithLockTimeout`, `txWithConnection`.
- **Evidence:** 14.2k BEGIN round trips a second, each with its `SET LOCAL lock_timeout`. The largest idle-in-transaction
  bucket is 57 servers sitting after BEGIN, and 18–27 of the 32 pools sit at their cap.
- **Change:** send BEGIN, the lock bound and the first statement in one pipelined write.
- **Saving:** **1.0–1.4 cores** *(inferred: about 59 µs of PgBouncer per round trip, measured, plus 20–40 µs of backend
  protocol)*, and about 57 fewer servers held.
- **Risk:** medium. The error mapping of a pooler-refused BEGIN; `now()` moves to the first statement.
- **Tests:** as CPUOPT6 D #3, with a recorded transport:
  - one write, with the bound in force;
  - a callback that sleeps holds no server;
  - a refused BEGIN surfaces as today's error.

#### P2. The network-quality listener list without copy-on-write (rank 11; LBALTOPT2 R13's second half)

- **Where:** proxy, every process.
  - connect `util.go`: `CallbackList.Add` and `Remove` clone both parallel slices.
  - `transfer_network_quality.go: newClientNetworkQualityState` registers every client the hosted path creates.
  - connect's residents skip the list since R13; the proxy's clients don't.
- **Evidence:** 1.0–2.9% of a proxy process (mean 1.8%), almost all of it GC assists, and 4–5% of the proxy's
  allocation (§10.3.2).
- **Change:** `CallbackList` keeps its entries in a map by id, and rebuilds the read snapshot in id order only when
  `Get` finds it stale. `Get` runs only from `NetworkQualityChanged` and `NetworkChanged`, which a server never calls.
  The listeners, their order and the callbacks are unchanged, and the apps get the change too.
- **Saving:** **0.8–1.4 proxy cores**.
- **Risk:** low.
- **Tests:**
  - a seeded property test against the old list: after any sequence of adds and removes, `Get` returns the same
    callbacks in the same order;
  - `testing.AllocsPerRun` of an add and a remove on a 10,000-entry list is constant (fails on the clone);
  - `NetworkQualityChanged` still reaches every registered client.

#### A1. Remember decoded client-key records (rank 12)

- **Where:** server `model/st_client_key_history.go: decodeStClientKeyHistoryRecord`, from
  `(*stClientKeyCurrentProjection).publicKey`. `GetClientContractMetadata` (`model/client_contract_metadata.go`) calls
  it on every contract creation, and `stClientKeyCurrent` on key reads.
- **Evidence:** 2.5–3.4% of an api process (the evidence envelope's JSON decode, its secp256k1 signature check in
  `startifact.EvidenceBytes`, the canonical re-encode), 0.35–0.63% of a proxy process and 0.9% of a taskworker one.
- **Change:**
  - A bounded process cache of successful decodes, keyed by SHA-256 over the three inputs (registration hash, evidence
    hash, evidence bytes). The decode is a pure function of those bytes.
  - Every per-call check against the head row (client id, network id, generation, domain hash, retired, present) still
    runs on each call.
  - Errors are never cached.
- **Saving:** **about 1 core:** api 0.6–0.8, proxy 0.2–0.3, taskworker 0.1.
- **Risk:** low.
- **Tests:**
  - over generated records, a cached result equals a fresh decode;
  - a changed byte in any input misses;
  - a record that fails to decode is refused on every call;
  - the bound evicts;
  - concurrent readers under the race detector.

#### R1. The contract-hole read without its script's prune (rank 13)

- **Where:** server `model/subscription_contract_hole.go: ReadContractHoleLease`, from
  `connect/resident_contract_manager.go: HasActiveContract` and `connect/resident_contract_allowance.go`.
- **Evidence:** 24.3k EVAL a second.
  - Each one is a 1.3 KB script: GET, two PTTLs, ZCARD, TIME, a ZREMRANGEBYSCORE write and ZREVRANGE.
  - It runs for every active forwarding pair about every 2.5 s, the positive result being cached for min(5 s, the
    lease).
  - By §10.3.7's fit that's about 4.2 host cores, 3.6 of them Redis, plus 1.4% of every connect process.
- **Change:** one MULTI/EXEC of GET, PTTL, PTTL, ZCARD and `ZREVRANGE 0 0 WITHSCORES`, validated in Go. Pruning expired
  members is left to the event script and the TTLs. The answer stays the same: Go already refuses a last deadline at or
  before now, and pruning never removes the maximum. No re-read interval moves.
- **Saving:** 25–45 µs a read, **0.6–1.1 Redis cores** *(inferred; benchmark it in the test environment first)*.
- **Risk:** medium: it's on the authorization path.
- **Tests:**
  - a property test, old against new, over generated states (counts, member deadlines including +inf, expired mixes,
    missing keys, malformed values, TTL edges), comparing status and `validUntil`;
  - the read leaves every key, member and TTL unchanged;
  - a race against the event script.

#### R2. One notice per client removal (rank 14)

- **Where:** server `model/client_close_model.go: SetClientsRemoved` writes `{*}c_removed` with `SET … EX 7200`. That's
  a `set` and an `expire` event now and an `expired` event two hours later, each to all 40 connect and proxy
  subscribers: 120 messages per removal, 40 of them acted on, at about 420 removals a second.
- **Change:** one explicit notice per removal (SPUBLISH on fixed sharded channels after the flag is written). The flag
  stays for `IsClientRemoved`.
- **Saving:** 33k fewer pushes a second, **0.6–0.9 host cores**, plus the receivers' reads.
- **Risk:** low–medium: the delivery path for removals.
- **Tests:** each removal is delivered once to every subscriber; resync and the 5-minute corrective poll are unchanged.

#### D4–D9, P4, R3–R6, A5 (ranks 15–26)

- **D4. Fewer round trips in a contract close** (`model/subscription_model.go: settleEscrowWithOptionsInTx`,
  `model/subscription_settlement_owner.go`).
  - **Evidence:** 11 sequential statements at the close rate (1,710 a second); six of them are point reads of rows
    the close already locked or read.
  - **Change:** fold the lock and the escrow-mode and balance reads into one statement, keeping the lock order (contract
    first), and pipeline the independent reads. That's about 8.5k fewer round trips a second.
  - **Saving and risk:** 0.6–0.9 cores *(inferred)*; medium risk (the financial path).
  - **Tests:** a statement count per close through a pgx tracer (fails today); identical rows for every outcome; the
    concurrent-close lock-order tests unchanged.
- **P4. The flow reaper visits due and cancelled flows only** (connect `ip_remote_multi_client.go: runFlowReaper` →
  `detachIdleFlows`; §6 item 5).
  - **Change:** keep every wake as it is, so the sticky-flow trim runs on the same passes. Keep the flows in a
    deadline-ordered index by the deadline last computed (re-inserted lazily when a due entry has seen activity), plus
    a queue of the flows cancelled since the last pass. A pass then retires exactly what today's pass retires; its next
    delay can only come earlier.
  - **Saving and risk:** 0.6–0.9 proxy cores; medium risk.
  - **Tests:** a virtual-time harness drives both reapers through seeded opens, activity, completions and cap
    crossings, and requires the same retirements on every pass.
- **D6. Hourly rollups for GET /stats/providers** (`model/provider_payout_model.go: queryProviderPayoutStats`,
  `model/provider_model.go`).
  - **Evidence:** per call, 37–53 s of payout attribution (0.79 s on 11-15), 14–20 s of per-destination counts and
    12–15 s of transfer bytes, behind a 30 s cache.
  - **Change:** per-provider rollups of closed hours, built from the same validated rows, plus the open hour read live.
  - **Saving and risk:** 0.6–1.0 cores and about 100 MB/s of reads *(inferred)*; medium–high risk (attribution validity,
    late sweeps), so it's the author's or the owner's call.
  - **Tests:** rollup plus live equals today's query, including invalid allocations, late sweeps and hour boundaries.
- **D5. The descendant check from an additive `(device_id, client_id) WHERE active` index** (`model/network_client_model.go:
  readActiveChildClientIdsInTx`; CPUOPT6 D #5).
  - **Evidence:** 432 calls a second at 2.0 ms and 773 hits.
  - **Saving and risk:** 0.6–0.75 cores *(inferred)*; medium risk (a hot table, 0% HOT updates). The migration applies
    before the code.
  - **Tests:** the same children for a device with many retired windows, under 50 hits a call.
- **R3. EVAL → EVALSHA** (CPUOPT6 E #5).
  - **Where:** the contract-hole read, the reservation (5.8 KB, 4.8k a second), the contract-hole event, net escrow,
    verify-egress, connection authority and the session scripts.
  - **Saving and risk:** 0.35–0.7 cores *(inferred)*; low risk. The pipelined NOSCRIPT retry already exists in
    `model/verify_egress_refresh_model.go`.
  - **Tests:** a NOSCRIPT reply, alone and inside a pipeline, is retried with the body and never returned as a result.
- **D3. The debit flush names its shard** (`model/subscription_transfer_debit.go: flushTransferDebitBalance`).
  - **Change:** add `AND shard = $n` with `transferDebitShard(balanceId)`, the value every insert writes, so each day
    descends its (shard, balance_id, contract_id) index. Drop or detach a day once its drop guard passes, rather than
    2 days after it ends.
  - **Evidence:** 0.38 backends (0.015 on 11-15); the empty day's index takes 338 scans a second at 383 hits each.
  - **Saving and risk:** 0.3–0.4 cores, measured; low risk.
  - **Tests:** with a drained partition, the flush returns the same rows within a buffer bound per partition (fails
    today); a source guard that every journal insert computes the shard the same way.
- **R4. GETEX in the resident heartbeat** (`model/network_client_resident_model.go: GetResidentForClient`; CPUOPT6 E #7).
  - **Change:** one GETEX instead of GET then EXPIRE.
  - **Saving and risk:** 3.4k requests a second, about 0.35 host cores; low risk.
  - **Tests:** the TTL is renewed by the read; missing, mismatched and undecodable records give identical results.
- **A5. The lag-guest gate without a claims decode** (`session/by_jwt.go: byJwtLagGuestUnverified`; CPUOPT6 C #5).
  - **Evidence and saving:** 1.4–1.6% of an api process; 0.3 cores; low risk.
  - **Tests:** as C #5.
- **D8.** The health aggregate once per export (0.2–0.5). **D9.** Claim probes and `pg_backend_pid` folded into the
  claim (0.2–0.4). **R5.** Provider appearances HDEL only the minutes since their own last write
  (`model/provider_appearance_model.go`; 0.15–0.2). **R6.** The rewritten sets published only on change
  (`model/egress_index.go`, `model/network_client_score_capability.go`; latency). Each is exact, with the tests named
  in CPUOPT6 D #9–#10 and E #6, or, for R5, `TestProviderAppearanceHashStaysBounded` extended.

### 10.6 Not proposed, held, or the owner's

- **The proxy's memory headroom.** It's the largest proxy lever, with GC at 43–55%, and it's held for the two new
  hosts (owner, 11-18). crisp's 24-hour minimum available memory is 30.6 GB, so it could take about 1 GB more per
  process. fireside's is 13.8 GB (6.7 GB over 7 days), so it can't.
- **Exchange links between connect processes:** about 10% of connect in socket syscalls, roughly 13 cores, at 1.7
  frames per syscall. The levers are structural: multiplexed links (CPUOPT4 #21) or placing residents beside their
  transports. They're high risk, sized only, so not proposed now.
- **Not re-proposed:**
  - connect's GOMAXPROCS (R17, dropped by the owner on 11-22);
  - the conntrack bypass (declined 11-13);
  - PgBouncer over a Unix socket and the edge-2 reboot (not chosen 11-13);
  - the pool size ("not yet");
  - VACUUM FULL and pg_repack (owner rule);
  - dropping X25519MLKEM768;
  - H1 termination in connect: done by LBALT.
- **A timing change, not proposed:** re-reading the contract-hole lease at 80% of its 5 s window instead of 50% would
  save about 1.3 host cores. It would also lengthen the worst case before a closed hole is refused from about 2.5 s to
  about 4 s.
- **edge-6's backup replay** (host, owner). The ZFS taskq parameters have been staged since 11-13 for the next planned
  reboot. Decide before the 12-17/12-20 full copy, which brings the replay back for days.
- **Noticed, not CPU:**
  - connect's PostgreSQL churn since the 06:50Z roll (§10.3.6; passed to connect's deployer);
  - the provider-work endpoint fence waits on its advisory lock: 1.34 backends, its mean 16 times 11-15's;
  - idle in transaction is 358 s/s against 26.6 s/s active;
  - D71's per-block edge-2 rule stopped its roll twice this morning on swings of the kind §9.3 describes. Use the
    15-minute average or a 0.09 tolerance.

## 11. C1: connect's timer-heap walks, the measurement and the backstop polls (11-23)

The owner approved C1 (§10.5, rank 1) on 2026-11-23. It has two parts:
- the measurement that finds what sets off the g2 blocks' heap walks (LBALTOPT2 R14(a));
- the model's backstop polls, taken off the runtime's timer heaps.

The trigger itself isn't fixed here. The fix depends on what production's counters show (§11.6).

Everything is built, tested and committed on branches. Nothing is merged, built into an image or deployed:
- server `feat/connect-timer-census-1123`, from main f09d3edd6;
- connect `feat/timer-census-1123` 5487fcc6, from 3586f22e;
- quic-go `feat/timer-census-1123` 0fb6192a, from 83a1e6f8.

Nothing in production was touched.

### 11.1 What's counted, and how

**The mechanism** (LBALTOPT2 §3.4; `runtime/time.go` in go1.27.4):
- A timer changed while it's in a P's heap is only marked. That covers a pending timer, and a zombie that a stop or an
  abandoned select left there. The heap keeps the earliest such deadline (`timer.modify`, `minWhenModified`).
- Once that deadline passes, the next timer check on the heap walks all of it (`timers.adjust`): about 60k timers at
  5–7 ns each. The check can be the P's own or another P's while it steals.
- So a heap walks about once per wait for its next change plus that change's horizon. For a source of rate `r` a heap
  and horizon `h`, that's `1/(1/r + h)` walks a second.
- The frequent changes with the shortest horizons set the rate, not the changes' CPU. That's why a profile can't name
  the trigger.

**What that means for the g2 excess:**
- The excess is 5–11.5 points of an 8.5-core process: 0.4–1.0 cores.
- At about 360 µs a walk, that's 1.1k–2.7k walks a second a process, or 55–135 a second on each of its 20 Ps
  *(inferred)*.
- A horizon of 10 ms can't make more than 100 walks a second a heap, whatever the rate. So the trigger must be due
  within about 10–18 ms: in `lt_1ms` or `1ms_10ms`, or at the low edge of `10ms_100ms`.
- At a 1 ms horizon it takes 60–150 changes a second a heap. That's about 1.2k–3k a second a process more on g2 than on
  g1 and g3.

**The counter:** `urnetwork_connect_timer_modifications_total{site, horizon}` (server `timer_census.go`).
- **site** is a fixed set of 27 labels, one per place that can change a runtime timer on a connect hot path (table
  below).
- **horizon** is how far ahead the changed timer is then due: `lt_1ms`, `1ms_10ms`, `10ms_100ms`, `100ms_1s`,
  `1s_10s`, `ge_10s`, and `stop`.
  - The buckets under 100 ms are finer than R14(a)'s, since pacing, ACK compression, loss recovery and short
    operation timeouts live there.
  - A Reset due at once is `lt_1ms`.
- **stop** counts a timer stopped, a deadline cleared, and a socket deadline set at or before now.
  - `poll_runtime_pollSetDeadline` stops the descriptor's timer in all three cases.
  - A stop leaves a zombie and sets off no walk itself. But a socket deadline set again while that zombie is still in
    the heap is a move, so the stops show which sets are likely moves.
- 27 sites times 7 horizons is 189 series a connect process. Every other service exports none: the series are made
  only when the census starts, and only the connect service starts it.

**What a count means.** Each counted call *may* move a timer in a heap. Whether it does depends on the timer's state,
which user code can't see:
- a socket deadline moves the descriptor's timer when that timer is pending or a zombie. It's an insert when the last
  one fired;
- a channel timer's Reset moves it when it's still in a heap. That's while a select waits on it, or while it's a zombie
  the last select left when it woke on another case. Otherwise it's an insert, at the next select that blocks;
- a func timer's Reset moves it when it's pending.

So a count is an upper bound on moves at its horizon. The trigger is the site whose sub-100 ms rate differs between g2
and the other blocks.

**How it's sampled.** One call in `SamplePeriod` is counted, as `SamplePeriod` calls.
- The period is 64 by default (`ExchangeSettings.TimerCensus`). It must be a power of two; non-positive leaves the
  census off.
- Off, a counted call costs one atomic load.
- On, it draws from the calling thread's random source and checks a mask, with no lock and no allocation. The source is
  `math/rand/v2`, the runtime's per-M ChaCha8.
- Only a sampled call reads the clock, and only for an absolute deadline. It then adds to one of 189 counters, an
  uncontended atomic add.
- The connect service starts the census in `Run`, before readiness, as the pprof listener starts, so it covers the
  process's life. If the start fails, `Run` logs it and serves on.

**The libraries.** The connect library and quic-go can't import the server, so each has its own process-wide hook with
the same sampling:
- `connect.SetTimerCensus` (connect `timer_census.go`);
- `quic.SetTimerCensus` (quic-go `urnetwork_timer_census.go`; URNETWORK.md, "A census of timer changes").

Each samples its own calls and hands a sampled one to its `Record`, with the sample period as its count. connect's
`startTimerCensus` maps their sites to the `client_*`, `quic_*` and `http3_*` labels.

**The sites:**

| Site | What it counts | Horizon at the defaults |
|---|---|---|
| `exchange_read_deadline`, `exchange_write_deadline` | `exchangeDeadline.refresh` before each exchange read, and before each write or writev batch | 30 s read, 15 s write |
| `front_read_deadline`, `front_write_deadline` | every deadline set on a public TCP socket of the lb_alt front (`frontTrackedConn`): net/http's, h2's per-write `WriteByteTimeout`, TLS's close_notify, gorilla's and the h1 transport's | the front's `ClientHeaderTimeout` and `SendTimeout`; 15–30 s for the transport; 5 s for close_notify |
| `front_write_bound` | the front's `SendTimeout` bound on a write its owner left without a deadline: set before the write, cleared after | `SendTimeout` |
| `front_response_deadline` | the nginx front's `http.ResponseController` deadlines: each response write's `SendTimeout` and its clear, a streamed request body's read deadline, the lingering read. On h1 they're also the socket's (`front_*`); on h2 they move the stream's timer inside net/http; on h3 they're a stream field | `SendTimeout` 30 s, `LingeringTimeout` 5 s |
| `front_upstream_deadline` | the nginx front's deadlines on its pooled connections to the api: each upstream read and write, and a tunnel's | `ProxyReadTimeout`, `ProxySendTimeout` 30 s, `StreamTimeout` 60 s |
| `h1_read_deadline`, `h1_write_deadline` | the h1 transport's per-read and per-write deadlines (`setConnectH1ReadDeadline`, `setConnectH1WriteDeadline`). A session through the front is also counted at `front_*` | `ReadTimeout`, `WriteTimeout` |
| `h1_ping_timer`, `h3_ping_timer` | the transport writers' heartbeat Reset as their queue goes idle (`resetConnectPingTimer`) | at least `MinPingTimeout`, 1 s |
| `forward_write_timer` | `sendPooledMessage`'s Reset for a send into a full queue, by a forward's or a resident transport's writer | `WriteTimeout`, 15 s |
| `redis_read_deadline`, `redis_write_deadline` | every deadline go-redis sets on a pooled socket. A wrapper on the pools' dial counts them (`redis_timer_census.go`) and keeps `syscall.Conn` for go-redis's socket check | the earlier of the command context's deadline and the pool's timeout: pools of 15 s, 2 s or 1 s; 150 ms–1 s contexts in connect's model calls. go-redis computes it from a clock it caches for up to 50 ms; the census reads the real one |
| `pg_deadline` | every deadline pgx sets, through `pgWriteTrackedConn`, which already wraps every pooled socket under TLS | at once for a canceled query (a stop); 15 s for a close |
| `client_timer` | the connect library's reused wait timer (`resetOrCreateTimer`) at the callers without a site of their own: the encryption idle timer, route reads, the client-side transports | the caller's |
| `client_route_write_timer` | a route write's wait for a writable route | the send's remaining timeout |
| `client_receive_handoff_timer`, `client_receive_delivery_timer` | a receive sequence's Pack handoff and delivery waits | the caller's; the idle and gap timeouts |
| `client_ack_compress_timer` | a receive sequence's ACK compression wait, re-armed for each new deadline | 10 ms, and 1 ms for the quiet head |
| `client_pack_wait_timer` | a send Pack call's admission waits (`sendPackWait.until`) | the send's timeout |
| `client_window_pacing_timer` | a send sequence's pacing wait, re-armed every pass | the pacing wake |
| `client_sequence_deadline_timer` | `sequenceDeadlineTimer`: stops only. It never moves a pending timer (R14) | stop |
| `quic_stream_read_timer`, `quic_stream_write_timer` | quic-go's per-call deadline timer for a stream Read, Peek or Write: a Reset in its wait loop, and its stop while still pending | the stream's deadline |
| `quic_conn_timer` | a connection's run loop timer: stops only. `resetTimer` stops the old timer and arms a new one | stop |
| `http3_idle_timer` | an http3 server connection's idle timer: stopped as a request stream starts, Reset as its last one ends | `KeepaliveTimeout` |

### 11.2 What isn't counted, and why

- **New timers:** an insert never sets off a walk (`maybeAdd` lowers no modified deadline).
- **The exchange's header deadlines** (`ExchangeBuffer.WriteHeader`, `ReadHeader`): set once per exchange connection,
  and C2 is changing those functions.
- **net/http's own h2 timers:** a stream's `ReadTimeout` and `WriteTimeout` timers (unset on the front), and the
  connection's idle timer, Reset when its last stream ends. They belong to the standard library. The deadlines connect
  sets through them are counted at `front_response_deadline`, and h2's socket writes at `front_write_deadline`.
- **connect's private listener** (`/status`): its sockets aren't wrapped, and it carries no transport.
- **The connect library:**
  - a `resetOrCreateTimer` timer's stops, since its callers stop it directly;
  - the zombie a channel timer becomes when a select wakes on another case (`unblockTimerChan`), which no hook sees;
  - the client-side callers in the proxy lane's files (`ip_remote_multi_client*.go`, the framer,
    `framed_message_conn.go`), which weren't edited. Their `resetOrCreateTimer` calls count under `client_timer`;
  - the contract manager's pass timer and the contract stats epoch timer, which run once a pass and aren't hot.
- **quic-go:** none of these is hot, and none is a move:
  - the run loop timer's stop as a connection ends;
  - a path probe's timer;
  - a closed connection's retirement `AfterFunc`;
  - a canceled dial's close wait;
  - `ConnGroup`'s per-read timer;
  - a transport's deadlines at close.
- **Rare paths** count only at the front's socket: the refusal hold's write, the client-limit-exceeded write, the moved
  close and the session notifications' writes.

### 11.3 The cost per call

`BenchmarkTimerCensusCount` (server root), one goroutine. It ran on an M1 Max shared with other sessions, at a load
average of 48–79:

| | A Reset | A deadline |
|---|---|---|
| census off | 2.3 ns | 2.4 ns |
| on, not sampled (period 64) | 7.5 ns | 7.7 ns |
| on, sampled | 10 ns | 28 ns, with the clock read |

- At a period of 64 that's about 8 ns a counted call.
- A busy connect process makes a few hundred thousand counted calls a second; the exchange reads lead (§11.5).
- So the census costs about 2–4 ms of CPU a second, under 0.05% of a process *(inferred)*.

### 11.4 The backstop polls on the sweep

**The problem.** Three loops each took a fresh `time.After` on every pass:
- `StreamHopListener.run` (`model/stream_model.go`);
- `NetworkPeerListener.run` (`model/peer_model.go`);
- `providerIntentCheck.Run` (`connect/provider_intent.go`), once per observation.

A pass woken by a key event (a kick, a delta, a snapshot, a resync) left that runtime timer in a P's heap as a zombie.
It stayed there until a walk or its deadline, 8–12 minutes out in production.

**The change.** Each now waits on `internal/timersweep` (LBALTOPT2 §8.1), with one timer per owner:
- **How they get the sweep:** as a constructor parameter, the way the exchange hands its sweep to its owners.
  - A resident passes `Resident.timerSweep()`, which is the exchange's.
  - The connect handler passes `exchange.timerSweep` to the provider intent check.
  - A nil sweep, as tests build the owners, gives the timer a sweep of its own. There's no new global.
- **The bound:** a poll comes at its deadline or at most one 100 ms tick after it, never before.
- **What's unchanged:**
  - the jittered intervals: ±20% around twice the corrective interval, so 8–12 minutes in production;
  - the error backoff;
  - a kick, a delta or a snapshot still leaves the deadline where it was, since the deadline is absolute;
  - a resync's full read still stands for the poll and moves the deadline;
  - the provider intent check still observes `ObserveInterval` after the end of each pass.
- **Superseded fires:** a fire that a resync or a stop supersedes is discarded, as a Go 1.23 timer's is.
- **Teardown:** the timer leaves the sweep as its owner returns.

**`TestConnectionTimerCensus`** ran 64 simulated connections on the local stack. It now has network peers on, so each
resident runs both listeners:

| | LBALTOPT2 §8.1's branch | This branch |
|---|---|---|
| the converted owners' runtime timers, a connection | 0 (10 owners) | 0 (12 owners) |
| their sweep timers, a connection | 10.00 | 12.00 |
| every runtime timer the connections made, a connection | 3.27 | 2.33 |

**In production:**
- The heap snapshot of §10.3.1 counted about 210k of g2's 1.2M timers in `StreamHopListener.run`, and 22k in
  `NetworkPeerListener.run`. That's about a fifth of the heaps, and every walk visits them.
- They move to the sweep. Its ticks cost about 3–5 ns a timer a second (§8.1's benchmark), about 1 ms a second a
  process for these timers *(inferred)*.

### 11.5 The local evidence

The perf harness reports the census when `CONNECT_PERF_TIMER_CENSUS` is set (`connect_perf_test.go`). It counts every
call and prints `[perfcensus]` lines for each phase. There were two runs on the local stack.

**Through the lb_alt front, one exchange** (`TestConnectPerformanceCarriersLbAlt`). Each span is about 10 s of ping-pong
and blast; rates are per second:

| Site, horizon | websocket | h1+ | quic | whodis |
|---|---|---|---|---|
| messages a second, best run | 69k | 83k | 6.1k | 19 |
| `exchange_read_deadline`, ge_10s | 34.8k | 34.6k | 16.1k | 176 |
| `front_read_deadline` and `h1_read_deadline` (equal), ge_10s | 11.6k | 11.5k | – | – |
| `front_write_deadline` and `h1_write_deadline` (equal), ge_10s | 4.4k | 4.5k | – | – |
| `exchange_write_deadline`, ge_10s | 3.9k | 4.4k | 2.9k | 176 |
| `h1_ping_timer`, 1s_10s | 771 | 831 | – | – |
| `h3_ping_timer`, 1s_10s | – | – | 1.9 | 2.0 |
| `quic_conn_timer`, stop | – | – | 4.2k | 268 |
| `client_ack_compress_timer`, lt_1ms / 1ms_10ms | 299 / 95 | 298 / 96 | 0.3 / 104 | – |
| `client_route_write_timer`, 100ms_1s / ge_10s | 55 / 49 | 48 / 43 | 35 at 1s_10s | 0.6 / 0.8 |
| `client_timer`, lt_1ms / 1ms_10ms | – | – | – | 24 / 215 |
| `redis_*`, any bucket | 2 or fewer each | 2 or fewer | 2 or fewer | 2 or fewer |

**Two exchange hosts, contracts, h1+ without the front** (`TestConnectPerformance`). Each blast is 6 s:

| Site, horizon | tput64 | tput1024 | tput2048 | noack1024 | bidi1024 |
|---|---|---|---|---|---|
| messages a second | 247k | 81k | 31k | 59k | 45k + 48k |
| `exchange_read_deadline`, ge_10s | 96.6k | 43.5k | 36.0k | 158.9k | 56.2k |
| `h1_read_deadline`, ge_10s | 32.2k | 14.5k | 12.0k | 57.6k | 18.7k |
| `exchange_write_deadline`, ge_10s | 20.8k | 6.4k | 3.9k | 3.7k | 6.1k |
| `h1_write_deadline`, ge_10s | 5.1k | 5.8k | 4.5k | 4.8k | 6.7k |
| `h1_ping_timer`, 1s_10s | 4.7k | 1.2k | 724 | 327 | 1.2k |
| `client_ack_compress_timer`, lt_1ms / 1ms_10ms | 823 / 105 | 438 / 79 | 267 / 93 | 0 / 0.2 | 509 / 166 |
| `client_route_write_timer`, 100ms_1s / ge_10s | 32 / 14 | 15 / 43 | 58 / 52 | 10 / 450 | 39 / 93 |
| `redis_*`, largest bucket | 14 (stop) | 16 (stop) | 11 (stop) | 12 (stop) | 31 (stop) |
| `forward_write_timer`, ge_10s | – | – | – | 0.8 | – |

**Caveats:**
- The two clients run in the test process, so the `client_*` counts are theirs as well as their residents'. In
  production `client_*` counts only residents' clients. The whodis `client_timer` counts are the test client's own
  DNS-carried transport.
- The local heaps are small. These runs say which changes are frequent and short, not what they cost.

**What they show:**
- **Every server site is at 1 s or more.**
  - Exchange reads (30 s) lead, at 16k–159k a second.
  - The h1 and front deadlines are 15–30 s.
  - The h1 heartbeat's Reset is 1–10 s.
  - Redis is 100 ms and up, at a few a second.
  - The forward write timer is 15 s and rare.
  - At those horizons, no server site can make more than one walk a second a heap.
- **The front's write bound never ran**, since the h1 transport's writes carry their own deadline. **quic-go's stream
  timers never ran either.** The QUIC carrier ran in hybrid mode, where the server clears the stream's read deadline
  and QUIC's idle timeout owns liveness, and its 1 KiB messages rode DATAGRAMs.
- **The only frequent change due within 10 ms is the connect library's ACK compression timer.** It runs at about
  300–820 a second under `lt_1ms` and 80–170 under `1ms_10ms` while one sequence receives a stream.
  - Each one is a real move when the wait it re-arms woke on the quiet-head signal: the select left the channel timer
    in the heap as a zombie.
  - noack1024, with no ACKs, has none.
- **quic-go's run loop stops its timer about 4k times a second** at 6k messages a second over QUIC. These are only
  stops; the zombies leave from the heap's head within milliseconds.

So locally, nothing on the server's own paths sets off frequent walks. The lead is the library's ACK compression in
residents' clients, whose rate in production follows their control traffic. That fits the lead of §10.3.1
*(inferred)*: the proxies' QUIC still lands on g2 and g4 by address. A hosted proxy's flows make contracts, and their
control frames are what a resident's client receives and acknowledges.

**The walks themselves**, measured two ways.

1. **`BenchmarkTimerWalksByChangeHorizon`** (`internal/timersweep`). On one P, a source re-arms a channel timer after
   each wait that another goroutine's message ended first, as the ACK compression wait does. It runs at 1.2–2.2M
   changes a second, on a heap of 55,000 timers and on an empty one, at a load average of about 13:

   | Horizon | CPU a change, 55k heap | CPU a change, empty heap | Walk CPU a change | Walk CPU a second |
   |---|---|---|---|---|
   | 500 µs | 819 ns | 480 ns | 339 ns | 0.41 s, 41% of the P |
   | 5 ms | 507 ns | 447 ns | 60 ns | 0.10 s |
   | 50 ms | 463 ns | 447 ns | 16 ns | 0.03 s |
   | 500 ms | 453 ns | 465 ns | none (within noise) | – |
   | 5 s | 465 ns | 462 ns | none | – |
   | 50 s | 465 ns | 469 ns | none | – |

   - At the same rate of changes, the walk cost falls about tenfold per tenfold of horizon, as `1/(1/r + h)` predicts.
     From half a second up it's nothing.
   - The 500 µs row implies about 2,000 walks a second at about 200 µs each on this machine; §8.1 measured 389 µs on a
     busier one.

2. **The perf harness with production-sized heaps.** `CONNECT_PERF_TIMER_BALLAST=60000` puts 60k far-off timers on each
   of 10 Ps, each beside a filler allocation and with a random deadline, so a walk misses cache as a server's does. This
   is contract mode, on a quiet machine (load average about 20):

   | Phase | messages a second | ACK compression changes a second, lt_1ms + 1ms_10ms | exchange + h1 read deadlines a second, ge_10s | `timers.check` / `adjust` share of the process |
   |---|---|---|---|---|
   | tput64 | 218k | 775 + 119 | 85k + 29k | 1.0% / 0.5% |
   | tput1024 | 62k | 336 + 114 | 35k + 12k | 2.7% / 1.3% |
   | tput2048 | 29k | 240 + 94 | 33k + 11k | 4.6% / 3.0% |
   | bidi1024 | 35k + 48k | 436 + 186 | 55k + 18k | 3.7% / 2.1% |
   | noack1024 | 59k | 0 + 0.2 | 153k + 57k | none in the profile |

   - The walks follow the ACK compression timer. noack1024 sets more long-horizon socket deadlines than any other phase
     and doesn't walk, and every phase with ACK compression does.
   - The shares also follow each phase's total CPU: tput64 burned 19 s of CPU in 6.6 s, tput2048 6 s.
   - A first run under a load average of about 120 collapsed throughput fifteenfold. It pointed the same way (`adjust`
     4.8–6.1% against 0.9%), but on too few samples to use.

**What this means for production.** Every connect resident runs ACK compression for its client's control sequence.
Locally it's the one source that sets off walks. Its rate in production is what the census will show.

### 11.6 What to read in production, and how to decide the fix

**Rolling it out.** The build carries the census, on at 1 in 64.
1. Roll it to one g2 block first. connect's CPU should stay within noise, and the 189 series should appear.
2. Then roll every block, since the decision compares g2 and g4 with g1 and g3.

**Reading it,** at the morning load, from the connect dashboard's row "runtime timer changes":
1. **"timer changes due within 100 ms / s by block and site":** for each site, compare the rate in `lt_1ms`, `1ms_10ms`
   and `10ms_100ms` on g2 and g4 with g1 and g3. Divide by the processes in the block and by 20 (GOMAXPROCS) to get a
   heap's rate `r`.
2. **The model:** a site makes about `1/(1/r + h)` walks a second a heap, with `h` its bucket's upper edge (1 ms,
   10 ms).
   - The excess of §10.3.1 is 55–135 walks a second a heap.
   - So look for a site whose sub-10 ms rate on g2 exceeds g1's and g3's by about 1.2k–3k a second a process.
   - A site only in `10ms_100ms` can make at most 100 walks a second a heap, and only at its low edge.
3. **Cross-check** with a CPU profile of the same g2 process. `timers.adjust`'s share should match the model's walks
   times about 360 µs a walk.

**The fix, by site.** Each keeps every deadline's instant.
- **`client_ack_compress_timer`** (the local lead): R14's `armBy`, as `sequenceDeadlineTimer` does. In the connect
  library, that's `transfer.go`'s `armAckCompressTimer`.
  - For an earlier deadline, stop the pending timer and arm a new one. That's an insert, and the stopped one leaves at
    its time.
  - For a later one, keep the pending timer. The wait already treats an early fire as a recheck: it reads the clock
    against the compression deadline and the quiet head, and arms again.
- **`client_window_pacing_timer`:** don't Reset a timer still armed for the same wake, as `sendPackWait.until` already
  doesn't, and use `armBy` for an earlier wake.
- **`client_receive_delivery_timer`, `client_timer`:** `armBy` at the caller.
- **`quic_stream_*`:** after a woken wait, arm a new timer instead of the Reset, as the run loop's `resetTimer` does.
- **`redis_*` under 100 ms:** a model call with a short context.
  - Find it: the horizon names the bucket, and the callers of `RedisWithDeadline` are few.
  - Lengthen or drop its socket deadline only where the owner's rule allows. Socket deadlines otherwise stay native
    (CPUOPT-KEEPALIVE).
- **`front_*`, `h1_*`, `exchange_*` under 100 ms:** a timeout configured short in production, such as the route file's
  `SendTimeout`. That's a config change, not code.
- **No site differs:** the trigger is in what isn't counted (§11.2), most likely net/http's h2 timers. The next step
  would be a runtime build that counts walks by the modifying caller, which isn't proposed.

### 11.7 Tests

Every changed path has a test. Each test below was run with its change faithfully reverted (the count call removed, the
call site back to its plain Reset or deadline, the loop back on `time.After`), and failed; then it was restored and
passed.

**server root** (`timer_census_test.go`, `redis_timer_census_test.go`):
- `TestTimerCensusLabelsAreAFixedSet`: the 27 sites and 7 horizons are a fixed list of unique snake_case values, and a
  census makes all 189 series as it starts.
- `TestTimerCensusHorizonBuckets`: each bucket is half-open at its edges, and a non-positive horizon is `lt_1ms`.
- `TestTimerCensusCountsEachCallOnceInItsBucket`: every counting call adds exactly one in its bucket and nothing
  elsewhere. A cleared or past deadline counts as `stop`, and a library's sampled record adds its count.
- `TestTimerCensusSamplesOneCallInThePeriod`: at a period of 64 every count is a multiple of 64, and 262,144 calls count
  within 10% of themselves (six standard deviations).
- `TestTimerCensusOffCountsNothing`: no census counts nothing; a period of 0 starts none, and 48 is refused.
- `TestTimerCensusStopLeavesALaterCensus`.
- `TestRedisTimerCensusConnCountsEachDeadline`: each go-redis deadline counts once in its bucket. The socket keeps
  `syscall.Conn` and `NetConn`; a pipe isn't given `syscall.Conn`; a failed dial passes through.
- `TestRedisTimerCensusSocketsKeepGoRedisSocketCheck` (local Redis):
  - the pool's sockets are the census's;
  - go-redis's socket check peeks through their `syscall.Conn` at every one of 5 checkouts;
  - the 5 GETs count exactly 5 write deadlines, 5 read deadlines and 5 clears of each.
- `TestPgWriteTrackedConnCountsEachDeadline`.
- The existing `TestRedisDeadPooledConnectionIsHandled` and `TestRedisPoolsSkipTheConnCheckOnlyWhereCommandsRetry` now
  dial through the census's sockets and pass.

**server connect** (`timer_census_sites_test.go`, `timer_census_transport_db_test.go`, `provider_intent_sweep_test.go`,
`run_test.go`, `timer_census_db_test.go`):
- `TestTimerCensusMapsEveryLibrarySiteToItsOwn`: every connect library and quic-go site maps to a distinct server site
  under its prefix.
- `TestStartTimerCensusRecordsQuicSites`: the service installs quic-go's hook, and its stop removes it.
- `TestStartTimerCensusRecordsConnectLibrarySites`: the service installs the connect library's hook. A real route
  writer that waits twice counts one Reset at `client_route_write_timer`.
- `TestStartTimerCensusOffAndRefused`: the default period is 64.
- `TestTimerCensusCountsTheExchangeDeadlines`: a write and a batch count 2, and three reads count 3, each in its
  timeout's bucket.
- `TestTimerCensusCountsTheForwardWriteTimer`: a send into a full queue counts one; a free queue counts none.
- `TestTimerCensusCountsTheFrontDeadlines`, `TestTimerCensusCountsTheFrontBoundAnOwnersClearLeaves`: the bound's set and
  clear, an owner's deadline and clear, read deadlines, `SetDeadline`'s pair, and an owner's clear under a bound.
- `TestTimerCensusCountsTheH1Deadlines`, `TestTimerCensusCountsThePingTimerResets`.
- `TestTimerCensusCountsTheTransportSites` (local stack): through the service's census, a real h1 session counts
  heartbeat resets and read deadlines, and a real h3 session counts heartbeat resets and quic-go's stream read timer. The
  heartbeat is an hour, so each reset counts in `ge_10s` and nowhere else.
  - Each of the h1 heartbeat, h1 read and h3 heartbeat call sites was reverted on its own, and each revert failed it.
- `TestRunCountsTimerChangesWhileItRuns`: `Run` runs the census while it serves, and stops it as it returns.
- `TestProviderIntentCheckObservesWithinASweepTickOfEachInterval`: in a synctest bubble, with every deadline between two
  ticks, three observations each come at the tick the sweep fires their deadline, never before. The timer is on the
  sweep while the check runs, and leaves it when the check ends.
- `TestConnectionTimerCensus` (local stack), extended to the listeners: no runtime timers for either poll, and 12.00
  sweep timers a connection. With the listeners back on `time.After`, it fails.

**server model** (`listener_timer_sweep_test.go`): `TestStreamHopListenerPollWaitsOnTheSweep` and
`TestNetworkPeerListenerPollWaitsOnTheSweep`. In a synctest bubble, with deadlines between ticks:
- each poll comes at its deadline's tick, never before;
- a kick, a delta or a snapshot leaves the deadline, and a resync moves it, with no poll at the replaced deadline;
- the timer is on the sweep while the listener runs, and leaves it when the listener ends.

**server nginx** (`timer_census_test.go`):
- the front's response write deadlines count 7 arms and 6 clears for three chunks, matching
  `TestRouteHttpSendTimeoutBoundsEachWrite`'s count;
- `copyToClient` counts its arms and clear;
- an upstream connection counts its write and its reads, a read only once it's armed;
- a forwarded request, a tunnel, a refusal's and an own answer's lingering reads, and a streamed body's read deadlines
  each count at their site in their bucket.

**The dashboard:** the whole `./grafana` suite passes with promtool 3.5.0,
`TestInternalDashboardsCoverEveryApplicationMetric` included.

**Suites run:**
- in full: `./nginx`, `./grafana` and `./internal/timersweep`;
- on the local stack: model's stream hop and network peer tests, the touched root tests (Redis, pg, randomness, the
  dial guard), and connect's front, transport, exchange, provider intent, run, timer sweep and census tests;
- `GOOS=linux go vet` on root, connect, model and nginx;
- the connect library's focused suite (114 tests) and quic-go's `go test -short .` (765 of 768 pass, 3 Linux-only
  skipped) and `./http3` (301), as each repo's commit records.

### 11.8 Risks

- **Every service's Redis sockets change type.** The census's wrapper is on the pools' dial in every service, not only
  connect.
  - It keeps `syscall.Conn` for go-redis's socket check and `NetConn` for its unwrap, and the go-redis tests pass
    through it.
  - Where the census is off, it adds an interface call and an atomic load per deadline set.
  - A go-redis upgrade that asserts another interface on the socket would need the wrapper to keep that one too.
- **The cost on the hottest sites.** It's about 8 ns a call at the default period, about 0.05% of a process
  *(inferred)*.
  - The period is a code default in `ExchangeSettings.TimerCensus`, not a warp setting. Turning the census off means a
    build with a period of 0.
  - If the canary shows a cost, raise the period. The counts keep their meaning at any power of two.
- **What a count is.**
  - A count is an upper bound on moves: an insert that the census can't tell from a move counts too. Examples are
    window pacing's first Reset after `NewTimer(0)`, and a quic-go Read's Reset after a wait that never blocked.
  - Sites at different layers overlap: `front_*` with `h1_*`, and `front_response_deadline` with `front_*` on h1.
  - Neither changes which site's sub-10 ms rate differs between blocks, which is the reading.
- **The libraries' hooks are process-wide.** A process with two exchanges would count both in one census. Only connect
  starts it.
- **The polls on the sweep.**
  - The stream hop and network peer backstops, and the provider intent observation, come up to 100 ms later than
    before, never earlier. That's against 8–12-minute backstops and a 2-minute observation interval, whose presence
    lasts 6 minutes.
  - The sweep takes on about 230k more timers per g2 process, about 1 ms a second of its ticks *(inferred)*.
  - Key-event delivery, which carries the changes, is untouched.
- **`Run` builds its exchange settings before readiness** to start the census there. `exchangeSettingsForRun` builds
  settings and reads only the process's host and block, so nothing changes for a start that isn't ready.
- **Merges:**
  - C2 changes `connect/resident.go` in `ExchangeBuffer.WriteHeader` and `ReadHeader`, which this branch doesn't touch.
    This branch's hunks there are `exchangeDeadline.refresh` and its three callers, `sendPooledMessage`, the listeners'
    constructor calls, and `ExchangeSettings`.
  - `CPUOPT7.md`'s §12, from another agent, will sit beside this §11 at the file's end.
