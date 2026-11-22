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

