# PERFVAR measurements

## Status

Historical baseline captured on 2026-08-10. These are userspace, same-host
results, not physical-device or Internet measurements.

The historical baseline consists of five attempts per scenario for the clean
path, the 500 ms and 1 s single-region paths, and the production H1 extender
path. A separate historical probe records the then-current fast-P2P MTU
failure. The current v2 carrier fragments at 1,188 bytes, reaches exactly a
1,280-byte worst-case IPv6 packet in the real-wire test, and completes focused
1,400- and 1,280-byte full-TUN MTU scenarios with zero oversize drops. Older
one-run development logs are not mixed into the baseline.

The clean table below is the original 8 MiB schema-1 baseline. It remains
useful as historical evidence, but 8 MiB can sit largely inside transport and
test buffers. It also predates exact provider-return recovery ownership and the
P2P receive-admission credit guard, so it does not describe the final source
tree. The schema-3 campaign therefore uses 32 MiB for clean, Wi-Fi,
and WAN controls and 20 MiB for LTE and poor-mobile profiles. The two extreme
single-region cold cases remain 64 KiB because they deliberately measure
startup and short-transfer behavior. A separate warmed workload sends one
route-local bandwidth-delay product on the same TCP connection before timing a
32 MiB payload. New schema-3 results will not be mixed into the older tables
without an explicit side-by-side label.

An exploratory 32 MiB LTE calibration exposed the old fixed-duration boundary:
packet activity continued until the socket closed at exactly 8 minutes 57
seconds. The incomplete run is not a throughput sample. It motivated
context-bound, rate-scaled workload deadlines, a 12–45 minute per-run bound,
structured calibration failures, and the 20 MiB impaired-profile payload. The
replacement long-transfer campaign is recorded separately below once complete.

## 2026-08-12 exact-tree correctness and socket follow-up

A serial, non-measurement correctness pass ran every `connect/perfvar` test on
one process with `GOMAXPROCS=10`, `-p=1`, and `-parallel=1`. It passed in
988.277 seconds. This includes all four routes, the 500 ms and 1 s
single-region cases, P2P legacy and fast paths, 32 MiB regional TCP, outage,
loss, rate, queue, reorder, MTU, migration, forced-probe, topology, ownership,
and teardown gates. The raw log is
`/tmp/perfvar-full-serial-20260812-mtu-resident.log`. This is correctness
evidence, not a new throughput baseline.

After the final ACK cancellation, TCP child-worker join, pure-ACK ownership,
OOB callback join, resident-controller lifecycle, H3 receive-drain, fast-P2P
wire-version/MTU, forced-probe barrier, 10 Mbps profile, and H1 depth-eight
changes were all present, the canonical command was run again from a clean
process. Every package test passed serially in 1,016.613 seconds. The final log
is `/tmp/perfvar-full-serial-20260812-depth8-final.log`. Focused normal and race
gates for the newly changed Connect and server ownership boundaries also
passed; their logs are listed in the buffer-depth section. This second result
was the completion gate for the source tree used by the buffer-depth
measurement campaign.

After the mobile TCP auto-tuning correction and end-to-end five-tuple group
path landed, the complete serial gate was run a third time. All 342 test
invocations passed in 934.771 seconds with `GOMAXPROCS=10`, `-p=1`, and
`-parallel=1`. The log is
`/tmp/perfvar-full-serial-20260812-mobile-group-final.log`. This is the final
correctness gate for the exact source tree used by the mobile measurements
below.

The server H1 optimization was then exercised through the production full-TUN
exchange route. `TestFullTunExchangeH1Correctness` passed five of five normal
runs and one race-detector run. The logs are
`/tmp/perfvar-exchange-h1-server-batch-normal-count5-20260812.log` and
`/tmp/perfvar-exchange-h1-server-batch-race-20260812.log`.

Socket microbenchmarks isolate the scheduling changes without attributing the
result to the full tunnel. Each table entry is the median of five one-second
samples on the Apple M1 Max.

| H1 socket boundary | CPUs | Singleton MB/s | Ready-coalesced MB/s | Gain |
|---|---:|---:|---:|---:|
| Client TLS | 1 | 361.40 | 560.34 | +55.0% |
| Client TLS | 10 | 216.61 | 548.90 | +153.4% |
| Server cleartext | 1 | 578.17 | 1,283.79 | +122.0% |
| Server cleartext | 10 | 273.13 | 869.67 | +218.4% |
| Server TLS | 1 | 405.15 | 580.17 | +43.2% |
| Server TLS | 10 | 228.87 | 604.72 | +164.2% |

Saturated H1 traffic formed about four frames per ready batch and reduced TCP
writes from one per frame to about 0.25. Allocation counts per frame did not
increase. Sparse traffic remained one frame, one deadline, one TCP write, and
one TLS record per batch. Server TLS sparse medians changed from 15.545 to
15.656 microseconds at one CPU and from 20.122 to 20.300 microseconds at ten
CPUs; the five-sample ranges overlap. The client sparse ranges also overlap.
No batching wait is used.

The existing exchange TCP writer already gathers at most 256 ready messages or
256 KiB and uses `writev`. Its earlier five-sample benchmark measured a
2,816.07 MB/s median for a 64-message ready batch versus 335.95 MB/s for 64
separate framed writes. The actual outbound resident bridge does not regress to
64 singleton writes. An exact barrier test observes `[1, 63]` socket-flush
formation through both transport and forward bridges, versus `[64]` for a
prefilled connection queue. A new five-sample, one-second real-TCP comparison
measured 32.090 microseconds for `[64]` and 36.380 microseconds for `[1, 63]`,
or 4.290 microseconds per burst. That isolated 13.4% difference does not justify
a new batch-channel ownership protocol without production syscall evidence.

Raw socket logs:

- `/tmp/urnetwork-client-h1-tls-controls-20260812.log`;
- `/tmp/urnetwork-client-h1-tls-coalesced-saturated-20260812.log`;
- `/tmp/server-h1-batch-authoritative-20260812.log`;
- `/tmp/server-h1-batch-sparse-authoritative-fixed-20260812.log`;
- `/tmp/exchange-buffer-authoritative-before-20260812.log`; and
- `/tmp/exchange-writev-bridge-authoritative-1s-20260812.log`.

## 2026-08-12 buffer-depth evaluation

This focused evaluation asks whether a ready-drain depth of eight improves the
production socket and packet boundaries. It is separate from the full-TUN
PERFVAR campaign: these are same-host boundary benchmarks, not end-to-end
Internet throughput claims. Each reported value is the median of five
one-second samples on the Apple M1 Max, with `GOMAXPROCS` set to either one or
ten. Payload rate is useful payload bytes per second. For benchmarks that move
different numbers of bytes per operation, MB/s—not raw ns/op—is the valid
throughput comparison.

### H1 write coalescing: depth four versus eight

The client and server H1 writers drain only messages that are already ready;
they do not wait to fill a batch. Raising the maximum from four to eight
roughly halves the saturated write and deadline rate again.

| H1 boundary | CPUs | Depth 4 ns/frame | Depth 8 ns/frame | Time change | Depth 4 MB/s | Depth 8 MB/s | Rate change |
|---|---:|---:|---:|---:|---:|---:|---:|
| Client TLS | 1 | 2,858 | 2,119 | -25.9% | 482.85 | 651.39 | +34.9% |
| Client TLS | 10 | 2,861 | 1,978 | -30.9% | 482.31 | 697.82 | +44.7% |
| Server cleartext | 1 | 1,308 | 969 | -25.9% | 1,055.18 | 1,423.63 | +34.9% |
| Server cleartext | 10 | 1,922 | 1,199 | -37.6% | 717.98 | 1,151.09 | +60.3% |
| Server TLS | 1 | 2,408 | 2,130 | -11.5% | 573.05 | 647.78 | +13.0% |
| Server TLS | 10 | 2,668 | 1,844 | -30.9% | 517.21 | 748.17 | +44.7% |

At ten CPUs the client formed a median of about 3.99 frames per depth-four
batch and 7.91 per depth-eight batch. TCP writes and write deadlines fell from
about 0.2505 to 0.1265 per frame. The server fixtures formed exactly four and
eight frames and reduced both values from 0.25 to 0.125 per frame. Allocation
counts were unchanged: three allocations per client frame and two per server
frame.

Sparse delivery stayed a singleton rather than waiting for more traffic:

| Sparse TLS boundary | CPUs | Singleton median | Depth-8 ready-drain median | Change | Observed formation |
|---|---:|---:|---:|---:|---|
| Client | 1 | 20.691 us | 21.200 us | +2.5% | 1 frame, 1 write, 1 TLS record |
| Client | 10 | 28.245 us | 28.420 us | +0.6% | 1 frame, 1 write, 1 TLS record |
| Server | 1 | 19.818 us | 20.060 us | +1.2% | 1 frame, 1 write, 1 TLS record |
| Server | 10 | 26.289 us | 26.976 us | +2.6% | 1 frame, 1 write, 1 TLS record |

The five-sample sparse ranges overlap. No coalescing timer, sleep, or scheduler
yield was added. Based on the consistent saturated gain and unchanged sparse
behavior, the production H1 maximum is eight on both the client and server.
The exact byte-bound, abort, short-write, flush-error, allocation, and
ownership tests passed 100 repetitions normally and 100 repetitions under the
race detector at the new bound.

### H3, exchange, P2P, and TUN/NAT boundaries

Depth eight is not a universal optimum. Every other candidate was measured at
its actual ownership or socket boundary before deciding whether to change it.

#### H3 QUIC stream writer

| CPUs | Depth 8 ns/op | Depth 8 MB/s | Depth 16 ns/op | Depth 16 MB/s | Result |
|---|---:|---:|---:|---:|---|
| 1 | 1,440,281 | 122.64 | 1,417,388 | 124.62 | Depth 16 is 1.6% faster |
| 10 | 1,419,624 | 124.43 | 1,427,647 | 123.73 | Depth 8 is 0.6% faster |

The rates are effectively tied, while depth 16 used about 40 fewer allocations
per operation. H3 remains at 16 messages and its existing 64 KiB byte bound.

#### Exchange TCP writer and reader dispatch

| Exchange write boundary | CPUs | Depth 8 MB/s | Ready 64 MB/s | Ready-64 gain |
|---|---:|---:|---:|---:|
| TCP `writev` | 1 | 1,696.37 | 2,575.58 | +51.8% |
| TCP `writev` | 10 | 1,493.56 | 3,459.35 | +131.6% |

The production exchange writer already has a larger bound: at most 256 ready
messages or 256 KiB. Restricting it to eight would discard a material
`writev` advantage, so its bound remains unchanged.

A prototype also grouped complete exchange frames before downstream dispatch:

| Exchange read dispatch | CPUs | Median ns/op | Change from singleton | Messages/dispatch |
|---|---:|---:|---:|---:|
| Singleton | 1 | 81,053 | baseline | 1.00 |
| Batch 8 | 1 | 77,594 | -4.3% | 7.89 |
| Batch 64 | 1 | 88,275 | +8.9% | 47.1 |
| Singleton | 10 | 41,323 | baseline | 1.00 |
| Batch 8 | 10 | 37,805 | -8.5% | 7.89 |
| Batch 64 | 10 | 47,706 | +15.4% | 47.4 |

The depth-eight dispatch prototype has a modest synthetic CPU benefit, but it
does not reduce socket reads: the existing 64 KiB `bufio.Reader` already reads
ahead from TCP. Adding another production batch would retain more pooled
messages, weaken immediate backpressure, and add partial-batch teardown rules.
No exchange read-path change is justified by a 4–9% dispatch-only result.

#### P2P route queue

The P2P route channel is capacity and backpressure, not a socket coalescing
batch. A real WebRTC loopback comparison found no consistent depth-eight gain:

| P2P carrier | CPUs | Channel 4 MB/s | Channel 8 MB/s | Channel-8 change |
|---|---:|---:|---:|---:|
| Legacy SCTP | 1 | 25.76 | 25.26 | -1.9% |
| Legacy SCTP | 10 | 30.82 | 29.86 | -3.1% |
| Fast SRTP | 1 | 65.10 | 63.13 | -3.0% |
| Fast SRTP | 10 | 90.63 | 93.06 | +2.7% |

The production capacity remains four. Raising it would double queued packet
ownership and worst-case queueing without a repeatable throughput gain.

#### P2P fast UDP ready drain

The fast carrier's UDP writer was compared at depths four, eight, and the
current 64. The most relevant eight-versus-64 medians are:

| Topology and producer | CPUs | Depth 8 MB/s | Depth 64 MB/s | Depth-64 change |
|---|---:|---:|---:|---:|
| One hop, serial | 1 | 165.01 | 176.89 | +7.2% |
| One hop, serial | 10 | 165.08 | 154.49 | -6.4% |
| One hop, pipelined | 1 | 173.48 | 168.97 | -2.6% |
| One hop, pipelined | 10 | 192.42 | 199.38 | +3.6% |
| Two hops, serial | 1 | 87.04 | 91.65 | +5.3% |
| Two hops, serial | 10 | 82.71 | 84.66 | +2.4% |
| Two hops, pipelined | 1 | 91.20 | 90.83 | -0.4% |
| Two hops, pipelined | 10 | 108.64 | 113.47 | +4.4% |

Results are mixed, but depth 64 wins most two-hop and parallel-host cases and
preserves larger Linux `sendmmsg` opportunities. It remains unchanged.

#### Shared TUN/NAT packet drains

| Direction and workload | CPUs | Depth 8 MB/s | Depth 64 MB/s | Depth-64 change |
|---|---:|---:|---:|---:|
| TCP upload | 1 | 325.53 | 368.89 | +13.3% |
| TCP upload | 10 | 447.84 | 478.69 | +6.9% |
| TCP download | 1 | 439.43 | 427.17 | -2.8% |
| TCP download | 10 | 353.30 | 378.85 | +7.2% |
| UDP upload | 1 | 155.24 | 161.57 | +4.1% |
| UDP upload | 10 | 238.88 | 242.53 | +1.5% |
| UDP download | 1 | 107.15 | 110.42 | +3.1% |
| UDP download | 10 | 180.29 | 184.65 | +2.4% |

Depth 64 wins seven of eight comparisons and remains the shared TUN/NAT
limit. Transfer-frame limits are protocol and ownership bounds rather than
generic socket read-ahead; they were not changed merely to make every number
eight.

### Buffer-depth decisions

| Boundary | Decision |
|---|---|
| Client H1 writer | Raise ready-drain/coalescing maximum from 4 to 8 |
| Server H1 writer | Raise ready-drain/coalescing maximum from 4 to 8 |
| H3 QUIC writer | Keep 16 messages / 64 KiB |
| Exchange TCP writer | Keep 256 messages / 256 KiB |
| Exchange TCP reader | Keep singleton dispatch; `bufio.Reader` already reads ahead |
| P2P route queue | Keep capacity 4 |
| P2P fast UDP drain | Keep 64 |
| Shared TUN/NAT drains | Keep 64 |

Raw logs for this evaluation:

- `/tmp/urnetwork-client-h1-tls-batch4-vs-8-20260812.log`;
- `/tmp/urnetwork-server-h1-batch4-vs-8-20260812.log`;
- `/tmp/urnetwork-client-h1-depth8-sparse-20260812.log`;
- `/tmp/urnetwork-server-h1-depth8-sparse-20260812.log`;
- `/tmp/urnetwork-h3-batch8-vs-16-fixed-20260812.log`;
- `/tmp/urnetwork-exchange-depth8-evaluation-20260812.log`;
- `/tmp/urnetwork-p2p-fast-udp-depth-evaluation-20260812.log`;
- `/tmp/urnetwork-p2p-route-channel4-vs-8-20260812.log`; and
- `/tmp/urnetwork-tun-nat-batch8-vs-64-20260812.log`.

Focused production-bound correctness logs are
`/tmp/connect-h1-batch-depth8-focused-normal-20260812.log`,
`/tmp/connect-h1-batch-depth8-focused-race-20260812.log`,
`/tmp/server-h1-batch-depth8-focused-normal-20260812.log`, and
`/tmp/server-h1-batch-depth8-focused-race-20260812.log`.

## 2026-08-12 mobile upload ceiling and packet grouping

The previously observed route-independent mobile-surrogate upload ceiling was
real in the harness, but it combined two independent common-path effects:

1. the resource profile set both the initial and maximum gVisor TCP buffers to
   256 KiB, disabling normal TCP buffer growth; and
2. the application bridge preserved an eight-packet TUN read only as a loop of
   singleton sends, repeating flow lookup, client selection, and Transfer
   admission for every packet.

The application delay itself was not the 41 Mbit/s limiter after it was moved
to the correct boundary. A 32 MiB H1 upload formed about 3,084 application
batches containing about 24,085 packets, or 7.81 packets per batch. Its
cumulative 100-microsecond application delays took about 407 milliseconds and
its packet-group send calls took about 245 milliseconds, while the transfer
still took 6.53 seconds at the fixed 256 KiB TCP ceiling.

### TCP auto-tuning sweep

The initial TCP buffer remained 256 KiB while only the auto-tuning maximum was
changed. Each entry is one correct, calibration-valid 32 MiB H1 upload with the
same group path and all other settings held constant.

| TCP buffer maximum | Goodput Mbit/s | Duration ms | Application batches | Maximum batch |
|---:|---:|---:|---:|---:|
| 256 KiB | 41.045 | 6,540.103 | 3,086 | 8 |
| 512 KiB | 226.832 | 1,183.413 | 3,063 | 8 |
| 1 MiB | 372.007 | 721.586 | 3,026 | 8 |
| 2 MiB | 402.276 | 667.292 | 3,025 | 8 |
| 4 MiB | 416.155 | 645.038 | 3,026 | 8 |

Allowing growth to 2 MiB raised goodput by 9.80 times relative to the fixed
256 KiB ceiling. Raising the maximum again to 4 MiB added only 3.45%, so the
mobile surrogate now uses a 256 KiB initial value and a finite 2 MiB maximum.
This corrects the userspace test model; it is not an application setting
change. Physical applications already use production TCP auto-tuning, with
the maximum scaled by their process memory budget.

### Whole-group send comparison

The production change preserves each exact directional-five-tuple group
through one policy result, update lookup, provider/client selection, candidate
race, and logical Transfer admission. It adds no send lock. Transfer can split
the admitted group into bounded wire Packs, but those chunks stay on the same
selected SendSequence and never rerun the client race.

Five paired 32 MiB H1 comparisons alternated which mode ran first. Every group
sample was faster than every singleton sample.

| Bridge mode | Median goodput Mbit/s | Median duration ms | Median bridge-send ms | Median carrier bytes |
|---|---:|---:|---:|---:|
| Packet at a time | 336.497 | 797.736 | 288.611 | 79,225,056 |
| Five-tuple packet group | 416.331 | 644.765 | 238.567 | 76,073,838 |

The group path improved median goodput by 23.73%, reduced time inside the
bridge send by 17.34%, and reduced observed carrier bytes by 3.98%. The latter
is consistent with carrying multiple ordered provider frames through one
logical group rather than reconstructing batches only opportunistically after
singleton admission.

### Final four-route mobile-surrogate upload

The corrected 2 MiB auto-tuning maximum and production group path were then
measured five times on every route with a 32 MiB upload. All 20 runs were
correct and calibration-valid.

| Route | Correct and valid | Median Mbit/s | p95 Mbit/s | Worst Mbit/s |
|---|---:|---:|---:|---:|
| P2P fast | 5/5 | 453.382 | 454.987 | 445.840 |
| Exchange H1 | 5/5 | 412.005 | 415.503 | 382.783 |
| P2P legacy | 5/5 | 383.776 | 388.778 | 371.458 |
| Exchange H3 | 5/5 | 213.393 | 217.416 | 196.660 |

The common 41 Mbit/s ceiling is gone. The remaining spread is route-specific:
fast P2P is now 10.0% faster than H1 and 18.1% faster than legacy P2P, while H3
is 48.2% slower than H1. Those differences should be investigated at their
carrier boundaries rather than by changing the shared mobile/TUN bridge again.
This remains a same-host userspace result, not a physical phone, radio, thermal,
or battery measurement.

Raw logs:

- `/tmp/perfvar-mobile-packet-group-h1-timing-probe-32m-20260812.log`;
- `/tmp/perfvar-mobile-tcp-window-sweep-h1-32m-20260812.log`;
- `/tmp/perfvar-mobile-packet-group-vs-singular-h1-32m-count5-20260812.log`;
- `/tmp/perfvar-mobile-packet-group-all-routes-32m-count5-20260812.log`; and
- `/tmp/perfvar-mobile-tcp-autotune-2m-h1-32m-20260812.log`.

## Historical main findings

These findings describe the schema-1/schema-2 source state only. They are leads
for the exact-tree rerun, not current acceptance results.

1. Fast P2P delivered a median 850.190 Mbps download and 282.826 Mbps upload on
   the clean profile. Relative to legacy P2P, that is 69.3% more download
   goodput and 15.8% more upload goodput.
2. The 850.190 Mbps fast-P2P download is calibration-limited. The untunneled
   userspace calibration measured only 852.267–857.322 Mbps, less than the
   required 10% headroom. Treat 850.190 Mbps as a same-host lower bound, not a
   validated route ceiling or proof of gigabit line rate.
3. High user-to-`server/connect` latency exposes a route-readiness reliability
   problem. At 500 ms RTT, only 22 of 40 non-extender attempts reached and
   completed the workload. At 1 s RTT, 22 of 40 attempts completed. Across both
   profiles and the extender matrix, 52 of 100 attempts completed and only 40
   were calibration-valid. Of 48 failures, 47 reached the destination TCP
   server but timed out waiting for the first probe response; one reset later
   in the probe. Their simulator counters recorded no loss, MTU, queue, outage,
   receiver, or P2P-network drops.
4. A 64 KiB upload that completes at 1 s RTT takes about 25.1 seconds on every
   route and delivers about 20.9 Kbps. This is startup- and recovery-dominated,
   not steady-state bulk throughput. The similarity across carriers points to
   the shared inner TCP/TUN path rather than H1, H3, fast P2P, or legacy P2P as
   the first bottleneck to investigate.
5. At 1 s RTT, valid H1 and legacy-P2P downloads both measured about 0.150
   Mbps. H3 and fast P2P conditionally measured 0.273 and 0.349 Mbps, but both
   exceeded their calibrations; those values cannot support a claim that they
   are faster.
6. Fast P2P emitted an observed maximum outer datagram of 1,444 bytes in the
   historical clean-inner-MTU probe. An unannounced 1,280-byte outer-MTU
   reduction deterministically dropped 20 packets and reset the inner TCP
   transfer. This diagnoses missing dynamic path-MTU adaptation: the separate
   static `mtu-1280` profile lowers the inner MTU and has a correctness gate.
7. Fast P2P also materially reduces allocation pressure relative to legacy
   P2P. In the clean runs it used 74–75% fewer allocated bytes and about 71%
   fewer allocations. Exchange H3 showed roughly twice H1's allocation count,
   which is a concrete profiling lead for its clean-path deficit.
8. The real H1 extender path reproduced the shared high-RTT readiness problem:
   8 of 20 attempts completed, and all 12 failures were stage-1 probe timeouts
   with zero simulated drops. Conditional upload goodput was nearly identical
   to direct H1, so these samples identify readiness—not steady-state extender
   throughput—as the first issue to fix.

## Measurement method

Each scenario forces one production route from the application-side TUN to the
provider-side destination:

- exchange H1;
- exchange H3;
- fast P2P over the SRTP data plane; or
- legacy P2P over the data channel.

The untunneled calibration runs the same workload through the resolved
userspace network profile. `tunnel/underlay` is tunneled goodput divided by
calibration goodput. A correct run is calibration-valid only when calibration
is at least 10% faster than the tunneled result. `useful/wire` is useful
application bytes divided by observed carrier bytes. Aggregate throughput and
duration include only correct runs; completion and calibration validity are
reported separately.

Five samples are enough to expose large regressions and failures, but the
reported p95 is only an upper order statistic from five attempts. It is not a
production tail-latency percentile. The high-RTT runs use 64 KiB specifically
to measure setup and short-transfer behavior, so their goodput must not be
compared directly with the 8 MiB clean bulk runs.

### Host and build identity

| Property | Recorded value |
|---|---|
| Host | Apple M1 Max, 10 logical CPUs |
| Operating system | macOS (`darwin/arm64`) |
| Go | `go1.26.5` |
| `GOMAXPROCS` | 10 |
| Race detector | Off for performance measurements |
| Server revision | `27ec598569259798ce50fb9d765fcff888d912cd` |
| Connect revision | `b5f93a9e28a1d48c1cb708fae770e37f1fa0fc6a` |
| Source state | Both worktrees recorded as modified |
| Measurement kind | `userspace-same-host` |
| Seed | `20260810` |

Because both worktrees were modified, the revisions alone do not reconstruct
the measured source. The old records retain the complete scenario, scenario
hash, profile hash, source revisions, and modified flags, but no patch content.

The clean matrix uses record schema 1. Regional and extender matrices use
schema 2, which records failed attempts and separates the application and
provider access profiles. The clean route semantics are unchanged because both
segments use `clean-lan`, but comparisons must use the resolved fields and
hashes rather than assuming schema-1 and schema-2 scenario hashes match.

Schema 3 is used for new measurements. It adds extended-topology identity and
observations, explicit valid-run counts, long-payload bounds, structured
calibration failure records, and latency-probe accounting. Its numeric
aggregates include only runs that are both correct and calibration-valid;
correct but calibration-limited runs remain in the individual record vectors.
For schema 3, valid, calibration-invalid, and failed counts are mutually
exclusive and sum to the attempted run count.

Schema 3 also records a deterministic content hash for each complete dirty
server and Connect worktree: tracked binary diffs plus sorted untracked paths,
modes, and contents. Matching revision and state hashes prove that two records
used the same captured source state. A hash is still an identity, not a copy of
the patch, so retaining or committing the source remains necessary to
reconstruct it.

Each schema-3 repetition also records a versioned trace identity and distinct
application/direct, provider, and internal-link seeds. Trace derivation excludes
the route, so carriers compared in the same repetition begin with the same
route-excluded seed family. Route-specific setup consumes different prefixes of
each scheduler's random stream and every-N packet sequence. The measured
workload is therefore reproducible for the same route and source state, but
cross-route samples are not packet-for-packet common-random-number pairs. Runs
execute in run-major order, with comparable routes adjacent and their first
position rotated on each repetition. P2P delay, jitter, loss, reorder, rate,
queue, and MTU behavior use the deterministic directional scheduler rather than
Pion router randomness. If future analysis needs identical post-setup stochastic
decisions, the harness can add an explicit measurement-epoch scheduler reset.

### Common test environment

Commands ran from `/Users/brien/urnetwork/server`. Database and Redis secrets
were loaded by the normal local test configuration; no credential values are
part of this report. The commands below make every recorded scenario input
explicit, including inputs that equal a harness default.

```sh
set -o pipefail
env \
  WARP_ENV=local \
  WARP_SERVICE=test \
  WARP_DOMAIN=bringyour.com \
  WARP_BLOCK=test \
  WARP_VERSION=0.0.0 \
  BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com \
  BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com \
  GOMAXPROCS=10 \
  CONNECT_PERFVAR_MEASURE=1 \
  CONNECT_PERFVAR_SEED=20260810 \
  CONNECT_PERFVAR_ROUTE=exchange-h1,exchange-h3,p2p-fast,p2p-legacy \
  CONNECT_PERFVAR_PROFILE=clean-lan \
  CONNECT_PERFVAR_WORKLOAD=tcp \
  CONNECT_PERFVAR_DIRECTION=upload,download \
  CONNECT_PERFVAR_TOPOLOGY=one-hop \
  CONNECT_PERFVAR_EXTENDERS=0 \
  CONNECT_PERFVAR_RESOURCE=default \
  CONNECT_PERFVAR_RUN_COUNT=5 \
  CONNECT_PERFVAR_BYTE_COUNT=8388608 \
  go test ./connect/perfvar -run '^TestPerformanceVariations$' \
    -count=1 -timeout=90m -v 2>&1 \
  | tee /tmp/perfvar-clean-matrix-5-v2.log
```

The 500 ms single-region matrix used the same environment and `go test`
invocation, with these selection values, and wrote
`/tmp/perfvar-regional-single-500-all-record5.log`:

```sh
CONNECT_PERFVAR_PROFILE=single-region-500ms-rtt
CONNECT_PERFVAR_ROUTE=exchange-h1,exchange-h3,p2p-fast,p2p-legacy
CONNECT_PERFVAR_WORKLOAD=tcp
CONNECT_PERFVAR_DIRECTION=upload,download
CONNECT_PERFVAR_TOPOLOGY=one-hop
CONNECT_PERFVAR_EXTENDERS=0
CONNECT_PERFVAR_RESOURCE=default
CONNECT_PERFVAR_RUN_COUNT=5
CONNECT_PERFVAR_BYTE_COUNT=65536
```

The 1 s upload matrix was split into two commands so partial failures could not
hide the other routes. Both retained the common environment, TCP workload,
64 KiB payload, five attempts, and 1 s profile:

```sh
CONNECT_PERFVAR_PROFILE=single-region-1000ms-rtt
CONNECT_PERFVAR_DIRECTION=upload
CONNECT_PERFVAR_TOPOLOGY=one-hop
CONNECT_PERFVAR_EXTENDERS=0
CONNECT_PERFVAR_RESOURCE=default
CONNECT_PERFVAR_RUN_COUNT=5
CONNECT_PERFVAR_BYTE_COUNT=65536

# /tmp/perfvar-regional-single-1000-h3-record5.log
CONNECT_PERFVAR_ROUTE=exchange-h3

# /tmp/perfvar-regional-single-1000-other-upload-record5.log
CONNECT_PERFVAR_ROUTE=exchange-h1,p2p-fast,p2p-legacy
```

The 1 s download matrix used the common environment and test invocation with
these exact selections and wrote
`/tmp/perfvar-regional-single-1000-download-record5.log`:

```sh
CONNECT_PERFVAR_PROFILE=single-region-1000ms-rtt
CONNECT_PERFVAR_ROUTE=exchange-h1,exchange-h3,p2p-fast,p2p-legacy
CONNECT_PERFVAR_WORKLOAD=tcp
CONNECT_PERFVAR_DIRECTION=download
CONNECT_PERFVAR_TOPOLOGY=one-hop
CONNECT_PERFVAR_EXTENDERS=0
CONNECT_PERFVAR_RESOURCE=default
CONNECT_PERFVAR_RUN_COUNT=5
CONNECT_PERFVAR_BYTE_COUNT=65536
```

The regional commands returned a failing `go test` status after emitting every
record because any incorrect attempt deliberately fails
`TestPerformanceVariations`. Their aggregate JSON remains complete.

## P2P receive-admission wrapper benchmark

On 2026-08-11, a focused five-sample microbenchmark measured the P2P outer-link
wrapper before and after enabling the directional receive-credit path. Both
variants include the deterministic scheduler. The credit variant also includes
a bounded delegate socket, a concurrent wrapper reader, credit acquisition
before every delegate write, receive release after every read, and a combined
link-and-credit terminal barrier. It does not include ICE, DTLS, SRTP, SCTP,
TUN, or application work, so these numbers are not end-to-end route results.

```sh
go test ./connect/perfvar -run '^$' \
  -bench '^BenchmarkP2pLinkUDPConnWrite(WithReceiveCredits)?$' \
  -benchmem -benchtime=2s -count=5
```

Each datagram carried a 1,200-byte payload and a modeled 28-byte IPv4/UDP
header. The benchmark joined every 512-packet batch; the directional receive
pool capacity was 1,024 packets.

| Wrapper path | Median ns/op | Median outer packets/s | Median outer Gbit/s | B/op | allocs/op |
|---|---:|---:|---:|---:|---:|
| Scheduler, no receive credits | 829.7 | 1,205,279 | 11.84 | 1,424 | 4 |
| Scheduler + receive credits + concurrent reader | 959.6 | 1,042,106 | 10.24 | 1,428 | 4 |

The receive-admission safety path reduced this isolated median packet and bit
rate by 13.5% and added 4 allocated bytes per operation, with no additional
allocation. Its median 10.24 Gbit/s remains more than ten times the 1 Gbit/s
target, so this bookkeeping is not the expected end-to-end bottleneck. The
raw no-credit samples were 12.95, 11.94, 11.84, 11.41, and 11.68 Gbit/s. The
raw credit-enabled samples were 10.31, 10.96, 10.24, 8.568, and 9.657 Gbit/s.
Host scheduling noise is visible, and the median is the appropriate summary of
these five runs. An independent 3-second rerun measured 12.45–14.34 Gbit/s
without credits and 10.22–10.45 Gbit/s with credits (about 1.04–1.06 million
packets/s), again with four allocations per operation.

## Clean-path baseline

The clean profile uses a 1 Gbps symmetric configured rate, 1 ms one-way delay,
no loss or jitter, a 1,500-byte outer MTU, and a 1,440-byte inner MTU. The
nonlimiting 32 MiB directional queue avoids modeled queue loss. Every historical
workload transfers 8 MiB. These samples predate the independent receive-credit
guard that now prevents Pion's fixed UDP read channel from silently overflowing;
the wrapper benchmark above measures that guard separately, but it does not
retroactively add credit observations to these route records.

| Route | Direction | Correct | Calibration-valid | Median Mbps | p95 Mbps | Worst Mbps | Median duration ms | Workload setup ms | Tunnel/underlay | Useful/wire |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| Exchange H1 | Download | 5/5 | 5/5 | 407.570 | 413.230 | 398.954 | 164.656 | 5.348 | 58.72% | 40.47% |
| Exchange H1 | Upload | 5/5 | 5/5 | 225.779 | 228.031 | 223.915 | 297.232 | 5.513 | 32.18% | 43.99% |
| Exchange H3 | Download | 5/5 | 5/5 | 159.358 | 165.271 | 153.885 | 421.120 | 5.721 | 22.75% | 38.92% |
| Exchange H3 | Upload | 5/5 | 5/5 | 154.872 | 157.564 | 154.863 | 433.317 | 5.749 | 22.19% | 44.13% |
| P2P fast | Download | 5/5 | 0/5 | 850.190 | 851.183 | 847.012 | 78.934 | 2.862 | 99.38% | 90.04% |
| P2P fast | Upload | 5/5 | 5/5 | 282.826 | 284.613 | 280.308 | 237.280 | 3.175 | 33.12% | 93.50% |
| P2P legacy | Download | 5/5 | 5/5 | 502.041 | 507.132 | 499.891 | 133.672 | 3.036 | 58.83% | 89.87% |
| P2P legacy | Upload | 5/5 | 5/5 | 244.210 | 244.405 | 241.701 | 274.800 | 2.985 | 28.53% | 93.53% |

The clean fast-path improvement is consistent in all five samples. Its median
gain over legacy is 69.3% for download and 15.8% for upload. Exchange H1 is
155.8% faster than exchange H3 on download and 45.8% faster on upload in this
harness. These comparisons share one host and profile; they do not predict a
different host's absolute capacity.

Fast-P2P download's five calibration results were 852.267–857.322 Mbps while
the tunneled results were 847.012–851.183 Mbps. That leaves only about 0.6%
median headroom. The result demonstrates that the fast route reaches the
current harness ceiling, but the harness must provide substantially more than
1 Gbps of calibrated capacity before it can validate a gigabit target.

The workload records also captured process-wide runtime allocation deltas.
These include concurrent harness work and are not a route-only heap profile,
but the five-run medians give a stable attribution signal under identical
conditions:

| Route | Direction | Median allocated MiB | Median allocations | Median garbage collections |
|---|---|---:|---:|---:|
| Exchange H1 | Download | 53.1 | 447,144 | 1 |
| Exchange H1 | Upload | 50.4 | 257,656 | 1 |
| Exchange H3 | Download | 99.6 | 976,544 | 5 |
| Exchange H3 | Upload | 83.8 | 556,535 | 2 |
| P2P fast | Download | 41.6 | 446,790 | 1 |
| P2P fast | Upload | 39.9 | 330,990 | 1 |
| P2P legacy | Download | 166.8 | 1,554,216 | 8 |
| P2P legacy | Upload | 154.8 | 1,158,978 | 5 |

Relative to legacy P2P, fast P2P reduced allocated bytes by 75.1% on download
and 74.2% on upload, and reduced allocation counts by 71.3% and 71.4%. H3's
allocation count was 118% greater than H1's on download and 116% greater on
upload.

## Single-region high-latency baseline

The regional profiles put the added constant latency only between the
application user and `server/connect`. The provider uses the clean profile to
model colocation with the service. The 500 ms profile applies 250 ms in each
direction; the 1 s profile applies 500 ms in each direction. Both use a 100
Mbps configured access rate, no loss or jitter, a 1,500-byte outer MTU, and a
1,400-byte inner MTU. The provider's clean segment adds about 2 ms to the
exchange calibration RTT.

### 500 ms application-user-to-connect RTT

| Route | Direction | Correct | Calibration-valid | Median Mbps | p95 Mbps | Worst Mbps | Median duration ms | Workload setup ms | Tunnel/underlay | Useful/wire |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| Exchange H1 | Download | 3/5 | 3/5 | 0.298 | 0.298 | 0.298 | 1,759.844 | 504.122 | 71.90% | 38.46% |
| Exchange H1 | Upload | 4/5 | 4/5 | 0.253 | 0.254 | 0.253 | 2,068.278 | 504.798 | 61.11% | 40.65% |
| Exchange H3 | Download | 3/5 | 0/5 | 0.561 | 0.564 | 0.555 | 933.937 | 507.138 | 135.33% | 36.39% |
| Exchange H3 | Upload | 2/5 | 2/5 | 0.252 | 0.252 | 0.252 | 2,083.375 | 506.956 | 60.65% | 39.78% |
| P2P fast | Download | 1/5 | 0/5 | 0.695 | 0.695 | 0.695 | 754.565 | 501.390 | 166.64% | 90.85% |
| P2P fast | Upload | 2/5 | 2/5 | 0.255 | 0.261 | 0.255 | 2,010.872 | 501.473 | 61.05% | 89.86% |
| P2P legacy | Download | 5/5 | 5/5 | 0.230 | 0.232 | 0.230 | 2,276.749 | 504.624 | 55.58% | 87.16% |
| P2P legacy | Upload | 2/5 | 2/5 | 0.163 | 0.174 | 0.163 | 3,010.324 | 501.983 | 39.16% | 80.34% |

Completion was 22/40, or 55%. The 18 failures all occurred during route
readiness, before the timed 64 KiB workload. Every failure reported
`server-stage=1`: the destination accepted the inner TCP connection, but the
application side timed out reading the first probe response.

The three successful H3 downloads and the one successful fast-P2P download are
not calibration-valid. Their tunneled results exceeded their calibrations, so
neither their absolute values nor a cross-route ranking is defensible. The
small successful sample sizes make the remaining high-RTT throughput values
diagnostic rather than performance claims.

### 1 s application-user-to-connect RTT

| Route | Direction | Correct | Calibration-valid | Median Mbps | p95 Mbps | Worst Mbps | Median duration ms | Workload setup ms | Tunnel/underlay | Useful/wire |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| Exchange H1 | Download | 3/5 | 3/5 | 0.149555 | 0.149572 | 0.149532 | 3,505.649 | 1,004.592 | 71.58% | 37.97% |
| Exchange H1 | Upload | 3/5 | 3/5 | 0.020904 | 0.020904 | 0.020903 | 25,081.091 | 1,004.056 | 10.01% | 35.33% |
| Exchange H3 | Download | 2/5 | 0/5 | 0.272702 | 0.284226 | 0.272702 | 1,844.619 | 1,005.102 | 130.52% | 35.80% |
| Exchange H3 | Upload | 3/5 | 3/5 | 0.020898 | 0.020901 | 0.020898 | 25,087.571 | 1,006.322 | 10.01% | 34.88% |
| P2P fast | Download | 2/5 | 0/5 | 0.349034 | 0.349078 | 0.349034 | 1,501.921 | 1,001.159 | 166.73% | 89.97% |
| P2P fast | Upload | 4/5 | 4/5 | 0.020921 | 0.020925 | 0.020901 | 25,060.663 | 1,001.050 | 10.00% | 84.38% |
| P2P legacy | Download | 1/5 | 1/5 | 0.149597 | 0.149597 | 0.149597 | 3,504.665 | 1,002.067 | 71.48% | 87.85% |
| P2P legacy | Upload | 4/5 | 4/5 | 0.020908 | 0.020915 | 0.019949 | 25,070.179 | 1,001.727 | 10.00% | 84.53% |

Completion was 22/40, or 55%, and 18/40 attempts were calibration-valid. The
upload subset completed 14/20 attempts and all 14 were calibration-valid. The
download subset completed 8/20 attempts, but only the three H1 completions and
one legacy-P2P completion were valid. H3 and fast-P2P download exceeded their
untunneled calibrations, so their higher conditional values cannot support a
speed ranking.

Seventeen failures were stage-1 response timeouts. One legacy-P2P upload
reached `server-stage=3` and then reset the inner TCP connection. The aggregate
completion and validity ratios happen to equal the 500 ms matrix's 55% and
45%; five attempts per route are too few to claim equal reliability, and the
failure remains nondeterministic.

The near-identical 25.1-second transfer durations and approximately 10%
tunnel/underlay efficiencies are the strongest attribution signal in this
set. Carrier choice barely changes the completed upload. Shared inner TCP/TUN
startup and recovery behavior dominates.

### Failure-counter audit

Across the 36 failed non-extender regional attempts, aggregate recorded drops
were exactly zero in every relevant category:

| Counter category | Total |
|---|---:|
| Simulated link loss drops | 0 |
| Simulated link MTU drops | 0 |
| Simulated link queue drops | 0 |
| Simulated outage drops | 0 |
| Simulator receiver drops | 0 |
| P2P forward drops | 0 |
| P2P reverse drops | 0 |
| P2P MTU drops | 0 |

This does not prove where the packet stopped, but it rules out the configured
impairment and bounded-queue drop paths as the proximate cause in these runs.

## Extender measurements

The production H1 extender was inserted on both client paths. Each access
profile's constant delay is divided across client-to-extender and
extender-to-edge segments, preserving the configured end-to-end delay instead
of doubling it. The provider remains on its clean colocated profile. The
scenario supports one extender per client path; it does not model a serial
chain of several extenders.

The matrix used the common environment and test invocation with these exact
scenario selections:

```sh
CONNECT_PERFVAR_ROUTE=exchange-h1
CONNECT_PERFVAR_PROFILE=single-region-500ms-rtt,single-region-1000ms-rtt
CONNECT_PERFVAR_WORKLOAD=tcp
CONNECT_PERFVAR_DIRECTION=upload,download
CONNECT_PERFVAR_TOPOLOGY=one-hop
CONNECT_PERFVAR_EXTENDERS=1
CONNECT_PERFVAR_RESOURCE=default
CONNECT_PERFVAR_RUN_COUNT=5
CONNECT_PERFVAR_BYTE_COUNT=65536
```

Output was captured in `/tmp/perfvar-regional-extender-record5.log`.

| User-to-connect RTT | Direction | Correct | Calibration-valid | Median Mbps | p95 Mbps | Worst Mbps | Median duration ms | Workload setup ms | Tunnel/underlay | Useful/wire |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 500 ms | Download | 2/5 | 0/5 | 0.521 | 0.522 | 0.521 | 1,005.208 | 505.488 | 125.22% | 19.37% |
| 500 ms | Upload | 3/5 | 3/5 | 0.253 | 0.254 | 0.227 | 2,069.950 | 506.148 | 60.90% | 19.93% |
| 1 s | Download | 2/5 | 0/5 | 0.260 | 0.262 | 0.260 | 2,003.277 | 1,004.796 | 124.60% | 19.06% |
| 1 s | Upload | 1/5 | 1/5 | 0.020874 | 0.020874 | 0.020874 | 25,116.875 | 1,006.218 | 9.99% | 16.96% |

Completion was 5/10 at 500 ms and 3/10 at 1 s, or 8/20 overall. All 12
failures were stage-1 route-readiness timeouts. As in the direct regional
matrix, link loss, MTU, queue, outage, and receiver-drop totals were all zero.
Both successful download groups failed calibration because the tunneled result
was faster than the untunneled baseline, so no extender download-speed claim is
valid.

Conditional upload performance did not show a material extender penalty:
500 ms direct H1 and extender H1 measured 0.253475 and 0.253285 Mbps; 1 s direct
H1 and extender H1 measured 0.020904 and 0.020874 Mbps. Completion was lower
with the extender in these five-attempt samples, but the direct path was itself
nondeterministic, so the sample is too small to assign a causal reliability
penalty to the extender.

`Useful/wire` is expected to be lower for the extender topology because the
carrier observation counts bytes on both serial segments. It is an
end-to-end simulated-network cost, not an indication that one segment lost or
duplicated the payload.

## MTU diagnostic

The dedicated failure probe ran with the production fast-P2P path, a 1,440-byte
inner MTU, and a silent 1,280-byte outer-MTU blackhole. It is intentionally a
passing test only when the currently known limitation is reproduced:

```sh
set -o pipefail
env \
  WARP_ENV=local \
  WARP_SERVICE=test \
  WARP_DOMAIN=bringyour.com \
  WARP_BLOCK=test \
  WARP_VERSION=0.0.0 \
  BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com \
  BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com \
  CONNECT_PERFVAR_FAILURE_PROBE=1 \
  go test ./connect/perfvar \
    -run '^TestFullTunP2pFastMtuBlackholeDetection$' \
    -count=1 -timeout=5m -v 2>&1 \
  | tee /tmp/perfvar-p2p-mtu-failure.log
```

Observed result:

- inner TCP ended with `connection reset by peer`;
- 20 outer datagrams were dropped for exceeding the MTU;
- the maximum observed outer datagram was 1,444 bytes; and
- the diagnostic passed because it deterministically attributed the expected
  failure to oversized fast-P2P datagrams.

This is not evidence that all 1,280-byte paths fail. It specifically shows that
fast P2P does not dynamically constrain or adapt an already configured clean
inner MTU after an unannounced outer-path reduction. Static `mtu-1280`
scenarios lower the inner MTU before construction and are tested separately.

## Exploratory RTT trend

One early H1 upload sweep used one 64 KiB run per focused profile. It predates
the authoritative five-run regional records and applies the focused profile
symmetrically to both endpoint access paths, unlike the single-region profile.
It is included only because the monotonic trend is useful; it is not a baseline
or statistical claim.

| Focused profile label | H1 upload Mbps | Runs |
|---|---:|---:|
| `rtt-0ms` | 5.000 | 1 |
| `rtt-10ms` | 3.544 | 1 |
| `rtt-25ms` | 1.988 | 1 |
| `rtt-50ms` | 1.257 | 1 |
| `rtt-100ms` | 0.642 | 1 |
| `rtt-150ms` | 0.411 | 1 |

The log is `/tmp/perfvar-h1-rtt-sweep-run1.log`. Repeat this sweep five times
with the final schema before using it for regression thresholds.

## Improvement candidates

### 1. Re-measure high-RTT throughput after the readiness fix

The historical readiness failure was the highest-priority result: 47 attempts
reached the destination server but did not return their first response. The
current harness uses explicit route/probe boundaries, truthful child joins,
and corrected return-route ownership. Its serial correctness run passed the
500 ms, 1 s, legacy-P2P, forced-probe, and H1-extender gates. The defect is no
longer reproducible in the deterministic suite. The next step is a new
five-sample schema-3 throughput campaign; the historical conditional values
must not be relabeled as post-fix performance.

### 2. Compare short-transfer startup with steady-state high-BDP throughput

The 64 KiB regional workload intentionally measures user-visible startup, but
it cannot answer how a warmed connection fills a large bandwidth-delay
product. The harness now exposes the two phases as separate workload records:

- time to first byte and 64 KiB completion; and
- `tcp-warmed`, which first sends one route-local BDP on the same connection and
  then measures a 32 MiB multi-window bulk transfer behind a fresh exact
  boundary.

At 100 Mbps and 1 s RTT, one BDP is 12.5 MB. A 64 KiB payload is less than 1%
of one BDP, so its 20.9 Kbps result describes startup pathology, not achievable
steady-state bandwidth.

### 3. Raise clean calibration capacity above the gigabit target

Fast-P2P download already consumes about 99.4% of the measured underlay. A
useful gigabit acceptance gate needs at least 10% calibrated headroom above the
target, preferably more. Optimize or bypass simulator bookkeeping in the clean
calibration, isolate the measurement from shared-host CPU contention, and add a
calibration-only gate before route timing. Do not optimize the route against a
ceiling the harness cannot distinguish.

### 4. Reduce exchange H3 packet and allocation cost

On the clean path, H3 reached 159.358 Mbps download and 154.872 Mbps upload,
well below H1's 407.570 Mbps and 225.779 Mbps. Profile packet batching, QUIC
datagram sizes, copy counts, encryption calls, wakeups, and allocation hot
spots. Preserve the same full-TUN workload and underlay calibration when
testing changes so improvements cannot come from bypassing route work.

### 5. Profile the remaining route-specific upload costs

The shared 41 Mbit/s mobile-surrogate ceiling is resolved. A fixed 256 KiB TCP
maximum had disabled auto-tuning, and singleton application sends repeated
flow/client/Transfer work. With a 2 MiB maximum and whole-group sends, the
four-route mobile upload matrix reaches 213–453 Mbit/s and all 20 runs are
correct and calibration-valid. The next upload work is route-specific: profile
H3's packet/allocation path first, then compare fast-P2P and H1 CPU, copy, ACK,
and carrier-write costs. Physical-device runs remain necessary to find any
mobile operating-system, radio, power, or thermal ceiling not represented by
the same-host surrogate.

### 6. Add path-aware growth above the safe 1,280-byte baseline

The fixed-MTU defect is resolved. Fast P2P v2 fragments at 1,188 bytes, which
produces an exact worst-case 1,280-byte IPv6 packet. Real-Pion and full-TUN
tests require complete delivery and zero oversized writes at both 1,400- and
1,280-byte outer MTUs. Mixed v1/v2 peers reject mismatched readiness markers
and fall back to SCTP. Future path-MTU work can grow above this conservative
baseline when the selected path proves a larger datagram size.

### 7. Improve measurement validity before comparing high-RTT downloads

At both 500 ms and 1 s, H3 and fast-P2P download calibrations were slower than
their tunneled results. The extender H1 downloads had the same validity
failure. Inspect direction composition, startup boundaries, and the
calibration transport. A comparison is invalid until the untunneled path has
at least 10% headroom. Keep incorrect runs out of throughput aggregates and
show the number of correct and valid samples exactly as this report does.

## Coverage and remaining runs

This historical baseline exercised all four routes with full-TUN TCP, both clean
directions, both single-region profiles in both directions, and the fast-P2P
MTU-blackhole diagnostic. It also exercised production H1 extenders in both
directions at 500 ms and 1 s. The harness contains deterministic correctness
coverage for simulator models, loss, H3 MTU, outage recovery,
rebinding/migration, workloads, and route selection.

The following performance matrices were not part of this historical baseline
and should not be inferred from the tables above:

- `wifi-good`, `lte`, `mobile-poor`, and `wan` five-run throughput;
- focused loss, rate, queue, jitter, reorder, and non-blackhole MTU sweeps;
- UDP, inner QUIC, web, parallel TCP, and latency-under-load workloads;
- the mobile resource surrogate;
- warmed regional TCP on the final source tree.

Run those as curated axes rather than one large Cartesian product. The
high-RTT readiness gate is now deterministic and green; retain it beside every
new impaired-profile campaign so failure censoring cannot re-enter the
throughput aggregates.

## Source logs

| Log | Use |
|---|---|
| `/tmp/perfvar-clean-matrix-5-v2.log` | Authoritative clean five-run matrix |
| `/tmp/perfvar-regional-single-500-all-record5.log` | Authoritative 500 ms five-run matrix |
| `/tmp/perfvar-regional-single-1000-h3-record5.log` | Authoritative 1 s H3 upload records |
| `/tmp/perfvar-regional-single-1000-other-upload-record5.log` | Authoritative 1 s H1 and P2P upload records |
| `/tmp/perfvar-regional-single-1000-download-record5.log` | Authoritative 1 s all-route download records |
| `/tmp/perfvar-regional-extender-record5.log` | H1 extender regional matrix |
| `/tmp/perfvar-p2p-mtu-failure.log` | Deterministic fast-P2P MTU diagnostic |
| `/tmp/perfvar-h1-rtt-sweep-run1.log` | Exploratory one-run H1 RTT trend only |
| `/tmp/perfvar-full-serial-20260812-depth8-final.log` | Final exact-tree complete serial correctness gate |
| `/tmp/urnetwork-client-h1-tls-batch4-vs-8-20260812.log` | Client H1 saturated depth-four/eight comparison |
| `/tmp/urnetwork-server-h1-batch4-vs-8-20260812.log` | Server cleartext and TLS H1 depth-four/eight comparison |
| `/tmp/urnetwork-client-h1-depth8-sparse-20260812.log` | Client H1 sparse depth-eight latency control |
| `/tmp/urnetwork-server-h1-depth8-sparse-20260812.log` | Server H1 sparse depth-eight latency control |
| `/tmp/urnetwork-h3-batch8-vs-16-fixed-20260812.log` | Real QUIC H3 depth-eight/sixteen comparison |
| `/tmp/urnetwork-exchange-depth8-evaluation-20260812.log` | Exchange writer and read-dispatch depth evaluation |
| `/tmp/urnetwork-p2p-fast-udp-depth-evaluation-20260812.log` | Fast-P2P UDP drain depth comparison |
| `/tmp/urnetwork-p2p-route-channel4-vs-8-20260812.log` | Real WebRTC route-channel depth comparison |
| `/tmp/urnetwork-tun-nat-batch8-vs-64-20260812.log` | Shared TUN/NAT depth-eight/64 comparison |
