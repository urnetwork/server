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

After the retained provider TCP timestamp/MSS/window changes, H3 storage
reuse, all-route packet-group verification, and WireGuard measurements were
complete, the same complete serial PERFVAR command ran again. All 342 tests
passed in 830.667 seconds. This exact retained-tree gate is
`/tmp/perfvar-full-serial-20260813-final-retained.log`.

The complementary non-DB ownership and concurrency tier then passed under the
race detector in 477.537 seconds with `-short`, `-p=1`, and `-parallel=1`. Its
log is `/tmp/perfvar-short-race-20260813-final-retained.log`. An attempted
full DB PERFVAR run under `-race` was rejected as invalid validation after its
instrumented regional route and application deadlines expired. PERFVAR's
documented contract deliberately uses the complete serial non-race tier for
production-shaped timing and the short race tier for deterministic ownership
and concurrency kernels. `server/test.sh` now enforces that split instead of
running the wall-clock-sensitive campaign under race instrumentation.

The retained tree also passed the complete Connect repository race suite and
the complete proxy repository suite. The complete SDK suite also passed: the
root race package, build and cgo modules, and all 28 JavaScript tests. The
server validation was completed in segments: the root, API, API handler,
API-key, proxy, and exhaustive
`server/connect` route matrix passed before the obsolete singular full-stack
performance bridge was stopped; both corrected grouped full-stack TCP tests
then passed normally and under the race detector; and every package after
`server/connect` passed under race. This is complete package and changed-path
coverage, but it is not represented as one uninterrupted `server/test.sh`
invocation.

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

## 2026-08-13 optimization synthesis

This section records the architectural lessons from the focused boundary and
full-TUN measurements. A result is called an improvement only when the changed
boundary was measured against its previous behavior. Provider timestamp and
peer-MSS handling, H3 retained storage, and the upward fast-P2P path-MTU
candidate were evaluated independently. Rejected SACK, CUBIC, and fast-P2P
upward-probing candidates were removed rather than retained on theoretical
grounds.

### Measured decisions

| Area | Measured result | Production conclusion |
|---|---|---|
| H1 client/server write batching | Raising the ready-only maximum from four to eight improved saturated boundary throughput by 13.0–60.3%, depending on TLS and CPU count. Sparse medians changed by 0.6–2.6%, with overlapping ranges and exactly one frame/write/TLS record. | Keep depth eight. Do not add a coalescing delay. |
| Exact-five-tuple logical sends | The original H1 mobile comparison improved from 336.497 to 416.331 Mbit/s (+23.73%). A final paired five-run all-route gate also improved every carrier: H1 +2.61%, H3 +15.86%, legacy P2P +2.84%, and fast P2P +0.35%. Route-selection/send time fell 12.0–49.9% and carrier bytes fell 4.0–6.4%. | Keep. Preserve a homogeneous packet group through policy, provider selection, candidate race, and SendSequence admission. Physical wire chunks remain an internal transport detail. |
| Full-stack TCP TUN bridge | On the same non-race full-stack test, singular `SendPacket` delivery completed one of four 100 MiB samples at 41.15 MiB/s. Preserving each `Tun.ReadBatch` through `SendPacketBatch` completed two of three samples at 44.70 and 44.47 MiB/s. Best goodput improved 8.63%, and allocated bytes fell from 4,629 to about 3,893 KiB/MiB (15.9%). | Keep the batch bridge in both full-stack TCP harnesses. Besides measuring the production architecture accurately, it avoids leaking a rejected singular packet owner. |
| Inner TCP auto-tuning | Raising only the modeled maximum from a fixed 256 KiB to 2 MiB improved the same H1 workload from 41.045 to 402.276 Mbit/s, or 9.80 times. Raising 2 MiB to 4 MiB added only 3.45% on that low-RTT workload. | A fixed small window is a route-independent ceiling. Keep a modest initial allocation and allow growth; size high-BDP maxima from RTT/rate and memory rather than from clean-path results alone. |
| gVisor TUN maximum, 4 versus 16 MiB | With the provider held at 16 MiB, the original 4 MiB TUN completed the exact 1 s RTT / 20 MiB H1 workload correctly in 18.284 s at 9.176 Mbit/s. Raising only the TUN maximum to 16 MiB overflowed the profile's 4,096-packet queue, produced 2,304 queue drops, and timed out. | Reverted. Retain the 4 MiB TUN maximum; a larger advertised window is harmful until the sender and modeled/real path queues can absorb it. |
| Provider NAT TCP maximum, 1 versus 16 MiB | Three exact traces with a 1 MiB maximum had a 27.016 s / 6.210 Mbit/s median. The same traces at 16 MiB had an 11.238 s / 14.928 Mbit/s median: 2.404 times faster and 58.40% less transfer time. Every paired trace improved; all six were correct with zero queue drops. | Keep the 16 MiB demand-driven maximum. Its three results reached about 94% of untunneled calibration and were therefore excluded from comparative route aggregates by the harness's 10% headroom rule; the raw correct transfer times remain the one-variable A/B evidence. |
| Provider NAT TCP initial window, 64 KiB versus 1 MiB | Holding the 16 MiB maximum fixed, a 64 KiB post-handshake start measured 18.270 s / 9.183 Mbit/s. A memory-scaled 1 MiB start measured 11.238 s / 14.928 Mbit/s: 1.626 times faster, 38.49% less transfer time, and 28.79% less median warmup time. All six runs were correct with zero queue drops. | Keep the memory-scaled 1 MiB initial window. The SYN still advertises the literal 16-bit window; scaling applies only after the handshake. |
| Provider NAT TCP timestamps | Negotiating and echoing TCP timestamps changed a three-run, correct and calibration-valid 64 KiB H1 upload at 1 s RTT from 25.087 to 3.081 seconds: 8.14 times faster, or 87.72% less time. Median goodput rose from 0.0209 to 0.1702 Mbit/s. At 500 ms, time changed only 0.92%, from 1.562 to 1.548 seconds. | Keep timestamps. They repair the one-second retransmission/RTT-estimation cliff without claiming a general 8.14-times bulk-throughput gain. |
| Provider NAT TCP peer MSS | With a provider MTU of 1,440 and peer MTU of 640, ignoring peer MSS produced 48 packets per 64 KiB operation, of which 47 were oversized and dropped. Honoring the 600-byte peer MSS produced 112 accepted packets and no drops. On the equal-1,440-MTU control, median packetization time was effectively unchanged: 24.785 versus 24.710 microseconds at one CPU and 24.123 versus 24.082 microseconds at ten CPUs. | Keep peer-MSS enforcement. It prevents a deterministic asymmetric-MTU blackhole with no measurable equal-MTU regression. |
| Provider NAT TCP SACK | Enabling SACK increased clean packet count from 756 to 772 because gVisor reserves larger option space. It was slower in clean, one-loss, three-loss, and delayed-three-loss comparisons. The delayed case was 1.95% slower and retransmitted the same three packets. | Reverted. Do not pay permanent option overhead when the measured loss patterns show no recovery reduction. |
| Inner TCP CUBIC | On exact paired WAN seeds, CUBIC versus Reno measured -5.40%, +14.16%, and -22.46% goodput. The paired median was -5.40%; other attempts failed calibration before both variants produced comparable route samples. | Reverted. Retain gVisor Reno rather than choosing CUBIC from an unpaired aggregate or theory. |
| H3 framing storage | Reusing one 64 KiB writer buffer improved the five-sample H3 socket median from 125.46 to 135.65 MB/s at one CPU (+8.12%) and from 120.43 to 123.25 MB/s at ten CPUs (+2.34%). | Retain writer-owned storage across H3 batches. The depth remains 16 messages / 64 KiB. |
| H3 allocation pressure | Median bytes allocated per H3 operation fell from about 249 KiB to 50 KiB at one CPU and from about 259 KiB to 61 KiB at ten CPUs, reductions of about 80% and 77%. Allocation counts fell only modestly because QUIC/TLS owns most remaining objects. | The retained buffer removes one large repeated copy/allocation, but it is a partial H3 fix rather than evidence that H3 now matches H1. |
| Exchange TCP writes | A 64-message ready `writev` measured 2,575–3,459 MB/s. The real outbound `[1,63]` bridge formation cost one extra onset flush and only a noisy 4.29 microseconds / 13.4% in the isolated comparison. | Preserve the current 256-message / 256 KiB writer. A new batch channel is not justified. |
| Exchange TCP reads | An added depth-eight dispatch layer saved only 4–9% of synthetic dispatch CPU and did not reduce socket reads because the existing 64 KiB `bufio.Reader` already reads ahead. | Keep one logical frame per downstream dispatch and immediate backpressure. |
| P2P route queue | Capacity eight ranged from -3.1% to +2.7% versus capacity four. | Keep capacity four; this is backpressure, not a coalescing bound. |
| Fast-P2P UDP drain | Depth 64 had mixed one-hop results but generally won the two-hop and parallel cases and retains Linux `sendmmsg` opportunity. | Keep depth 64. |
| Fast-P2P path MTU | A synthetic 32 MiB / 4 KiB-message carrier loop improved by 9.0% at one CPU and 11.5% at ten CPUs when an authenticated probe grew the outer packet from 1,280 to 1,500 bytes; fragment count fell 25%. Production full-TUN messages averaged about 2.95 KiB, however, so both payload geometries required three fragments. Three exact 32 MiB P2P-fast downloads measured 0.8620 Gbit/s with the safe fixed size and 0.8620 Gbit/s with probing; fragment totals were the same ~36.36k. | Reverted upward probing. Keep the fixed 1,188-byte payload and exact 1,280-byte worst-case IPv6 packet: the end-to-end workload showed no positive improvement, so the version/geometry/control complexity is not justified. |
| WireGuard proxy upload | The proxy-side boundary improved 10.85% and 17.80% in operations/s at one and ten CPUs. At the exact server `ProxyDevice` boundary—including activity accounting, message-pool copies, and owned outer slices—depth eight cut median time by 17.24% and 29.83%, or raised operations/s by 20.83% and 42.51%. Depth 64 was 1.3–2.6% slower than eight. | Keep source-group activation and `SendBorrowedBatch`, with an upload bound of eight. The batch carries userwireguard's shared offset and copies every packet before asynchronous DeviceLocal admission. |
| WireGuard proxy download | Ready-only depth 64 reduced the 128-packet median from 7.792 to 5.967 microseconds at one CPU and from 8.170 to 6.341 microseconds at ten CPUs: 30.59% and 28.84% more operations per second. Depth 64 was only 1.3–1.8% faster than depth 8, but depth 128 regressed 4.4–7.0% versus 64. | Keep depth 64. It adds no coalescing wait, matches the DeviceLocal/TUN batch boundary, and avoids the depth-128 regression. |
| TUN/local-NAT drains | Depth 64 won seven of eight TCP/UDP direction comparisons, by 1.5–13.3%, and reached 478.69 MB/s on the ten-CPU TCP-upload boundary. | Keep depth 64. Do not normalize every queue to the H1 depth of eight. |

The retained-H3 raw log is
`/tmp/urnetwork-h3-retained-storage-20260813.log`. Its focused framing tests
passed 100 repetitions normally and 100 repetitions with the race detector.
The earlier buffer and full-TUN logs remain listed in their source sections.

The retention rule used throughout is deliberately strict: a microbenchmark
can identify a promising boundary, but an architectural change is kept only
when the closest production-shaped benchmark also improves. This is why the
synthetic fast-P2P MTU result was reverted after a neutral full-TUN result,
while the WireGuard batch path was retained only after its gain survived the
real `ProxyDevice` copy/admission boundary.

### TCP option and local-control audit

The synthetic provider endpoint and the gVisor TUN were audited separately.
The provider now parses the three negotiated options it needs: MSS, window
scale, and timestamps. Unknown or malformed option tails remain ignored rather
than invalidating an otherwise valid IP packet. The focused retained tests
passed 20 repetitions normally and 20 under the race detector.

| TCP mechanism | Current state | Benchmark decision |
|---|---|---|
| Window scale | Retained. The SYN window stays a literal 16-bit value and scaling starts after the handshake. | Required for the measured high-BDP window work; covered by deterministic packet tests. |
| Timestamps | Provider negotiates and echoes timestamp values on SYN-ACK, data, and ACK packets. | Keep: 8.14-times faster on the measured 1 s RTT short upload; neutral at 500 ms. |
| Peer MSS | Provider caps return payload to peer MSS after subtracting negotiated timestamp bytes. | Keep: removes 47 oversized drops per 48 old packets on the asymmetric-MTU benchmark; neutral control. |
| SACK-permitted and SACK blocks | gVisor supports SACK/RACK, but the provider candidate was measured with zero, one, three, and delayed-three losses. | Revert: all useful comparisons were flat or slower, while packet count rose 2.12%. |
| Reno versus CUBIC | gVisor offers both; the historical/default TUN controller is Reno. | Keep Reno: paired CUBIC results were inconsistent and had a -5.40% paired median. |
| Nagle / `TCP_NODELAY` | gVisor delayed-send mode defaults off; the provider's host TCP socket explicitly calls `SetNoDelay(true)`. | No candidate change: the desired low-latency setting is already active. |
| Quick ACK | New gVisor endpoints enable quick ACK by default. | No candidate change. Connect's separate provider ACK-compression policy still needs its own instrumented experiment before changing its 50 ms bound. |
| Receive auto-tuning | gVisor moderate receive buffering defaults on; production supplies demand-driven min/default/max ranges. | Keep. The provider 16 MiB maximum was 2.404 times faster than 1 MiB at 1 s RTT. The separate TUN 16 MiB candidate overflowed its path queue and was reverted to 4 MiB. |
| Initial congestion window | gVisor uses ten segments. | No isolated change justified; increasing it would trade short-transfer startup against burst loss and queue pressure. |
| RTO bounds | gVisor minimum is 200 ms; production caps the otherwise 120 s maximum at 8 s. | Treat as outage-tail policy, not a steady-goodput option. No new change was introduced by this audit. |
| RACK loss detection | gVisor enables RACK internally, but its useful selective-loss evidence depends on SACK negotiation. | No separate provider toggle was changed; the measured provider SACK candidate was negative. |
| PMTU discovery | gVisor can reduce sender payload after packet-too-big feedback; the fixed TUN/link MTU and provider peer-MSS cap remain the primary inner-path bounds. | No TCP-option change. Fast-P2P upward probing was measured separately and reverted because it did not improve the production full-TUN workload. |
| Keepalive, retry count, linger, and TIME-WAIT | These defaults control idle failure detection, terminal retry duration, and socket retirement. | They do not raise steady bulk goodput. Leave unchanged unless a lifecycle/outage benchmark identifies a specific tail. |
| ECN | This gVisor revision explicitly does not support TCP ECN. | Not benchmarkable as a configuration toggle; a provider-only implementation could not negotiate end-to-end ECN. |
| TCP Fast Open | This gVisor revision has no TCP Fast Open option/cookie path. | Not benchmarkable without a separate stack feature implementation and replay/security design. |
| PAWS | The provider uses wrap-safe timestamp ordering, while this gVisor revision does not expose a separate PAWS option to enable. | No independent toggle. Timestamp retention is based on the measured RTT/retransmission result, not a PAWS claim. |

The standards context is [RFC 6691 for sender MSS use](https://datatracker.ietf.org/doc/rfc6691/),
[RFC 7323 for window scaling and timestamps](https://datatracker.ietf.org/doc/rfc7323/),
[RFC 2018 for SACK](https://datatracker.ietf.org/doc/rfc2018/), and
[RFC 3168 for ECN](https://datatracker.ietf.org/doc/rfc3168/). These references
explain the mechanisms; retain/revert decisions above come from the repository
benchmarks.

### General rules supported by the measurements

1. Preserve semantic batches at the highest layer that knows they are one
   directional flow. Reconstructing a batch after per-packet policy, routing,
   race, and admission work recovers socket efficiency but not the repeated
   CPU and scheduling work above it.
2. Ready-drain only what is already queued. The H1 gain came without a timer,
   sleep, or scheduler yield, so sparse latency stayed a singleton operation.
3. Batch bounds describe different resources. H1's value of eight bounds TLS
   coalescing, H3's 16/64 KiB bounds a QUIC stream write, exchange's 256/256
   KiB bounds `writev`, and the P2P route's four bounds retained ownership.
   They should not share one arbitrary number.
4. Avoid copying merely to reduce syscall count when the underlying connection
   already exposes a vectored write. Reusing unavoidable framing storage is a
   win; replacing raw-TCP `writev` with a copied aggregate is not.
5. Window ceilings must be evaluated at the route's bandwidth-delay product.
   A buffer that is ample on a same-host path can cap throughput by orders of
   magnitude at 500 ms or 1 s RTT.
6. A same-host boundary over 1 Gbit/s is necessary but not sufficient. The
   end-to-end route can still be limited by repeated selection, Transfer
   framing, ACK cadence, carrier allocations, or the simulator calibration.


### Final paired packet-group route gate

A new exact-tree comparison alternated packet-at-a-time and packet-group modes
for each of five clean-LAN seeds on every production carrier. Each sample moved
32 MiB in the upload direction with the same mobile resource profile; only the
bridge handoff mode changed.

| Route | Packet-at-a-time Mbit/s | Packet-group Mbit/s | Goodput gain | Send-time reduction | Carrier-byte reduction |
|---|---:|---:|---:|---:|---:|
| Exchange H1 | 246.600 | 253.032 | +2.61% | 15.37% | 3.96% |
| Exchange H3 | 142.728 | 165.368 | +15.86% | 19.71% | 4.53% |
| P2P legacy | 247.087 | 254.094 | +2.84% | 49.87% | 6.43% |
| P2P fast | 253.964 | 254.849 | +0.35% | 12.01% | 4.93% |

All 40 transfers completed correctly. This isolates the group handoff on the
current tree and supersedes an earlier cross-tree comparison that appeared to
show a P2P upload regression. That older comparison changed several other
components and is not valid evidence against grouping.

### H1 exchange on the LTE surrogate

Three complete paired 20 MiB runs on the synthetic LTE profile measured a raw
median of 2.025 Mbit/s download and 2.669 Mbit/s upload. A fourth completed
download was 2.177 Mbit/s. Every transfer was byte-exact, but all were excluded
from comparative aggregates because the separately impaired underlay
calibration was not at least 10% faster than the tunnel. The command was stopped
after the three complete pairs rather than letting the five-pair campaign hit
its 30-minute package limit.

The result is not the profile's nominal radio capacity. The same 0.5%
independent loss, 30 ms base delay, and jitter are applied on both user and
provider access legs. It therefore measures H1 plus inner TCP recovery under a
compounded two-leg cellular surrogate. Treat 2.0/2.7 Mbit/s as a reproducible
stress result, not a prediction for one physical LTE connection.

## `server/proxy` packet-flow audit

The hosted proxy is a client of the Connect network. It does not run provider
NAT and, because `HostedIncompatible` forces direct mode off, it cannot use
legacy or fast P2P. Provider NAT TCP is analyzed separately below.

### HTTP and SOCKS traffic

The HTTP/SOCKS path already has the desired batch shape:

1. The proxy relay dials through a private gVisor `Tun` owned by one
   `ProxyDevice`.
2. `ProxyDevice.Run` reads up to 64 packets with `Tun.ReadBatch` and calls
   `DeviceLocal.SendPacketsNoCopy` once.
3. `DeviceLocal` takes one immutable route snapshot. The production
   `RemoteUserNatMultiClient` groups the burst by exact directional five tuple,
   inspects the ordered payloads, selects a provider once per group, and admits
   one logical Transfer group.
4. Hosted devices normally use Auto, whose current production runner is H1.
   The client H1 writer and `server/connect` H1 writer both use the measured
   ready-only depth of eight. The exchange writer retains its larger `writev`
   bound.
5. Return traffic reaches `AddReceivePacketsCallback` as a borrowed batch.
   The ordinary proxy mode calls `Tun.WriteBatch` once, enabling gVisor TCP GRO
   before the proxy relay reads application bytes.

Consequently, the exact-five-tuple grouping, H1 depth-eight coalescing,
exchange `writev`, TUN depth-64 drain, and TUN GRO findings already apply to
HTTP/SOCKS proxy traffic. There is no remaining packet-at-a-time downgrade in
the `ProxyDevice.Run` bridge. `UpdateActivity` is atomic and runs once around a
TUN batch; it is not a per-device mutex on the data path.

The H3 retained-storage change applies only when a stored performance profile
explicitly selects H3; current Auto uses H1. Fast-P2P path-MTU growth does not
apply because hosted devices prohibit direct P2P. The memory-scaled gVisor
ceiling applies to the proxy's private TUN stack; CUBIC was measured and
rejected, so the stack retains Reno. The Provider NAT TCP SYN/window,
timestamp, and peer-MSS changes do not apply to the proxy's private TUN: they
are implemented by `TcpSequence` on the provider side and evaluated separately
below. A proxy
hosts many devices and flows, so its demand-allocated maximum still needs a
multi-device load measurement before it receives a proxy-specific speedup
claim.

### WireGuard traffic

WireGuard had two proxy-specific batch-to-single downgrades that HTTP/SOCKS
did not have. A deeper audit corrected the meaning of its former batch-size
one setting:

- On the production Linux host, `userwireguard` uses the maximum of the TUN
  batch size and its UDP bind batch size. The bind reports 128, so decrypted
  packets could already reach `WgProxy.Write` as a same-peer batch even though
  `WgProxy.BatchSize` returned one. The old `Write` then looped over that batch,
  parsed the source, activated the same proxy device, and called singular
  `WgTun.Send` / `ProxyDevice.Send` for every member.
- The DeviceLocal return callback already received a batch, but the old
  WireGuard mode shared each member into a `chan []byte` and `WgProxy.Read`
  filled only the first supplied buffer. That prevented one ready burst from
  reaching WireGuard's peer grouping, parallel encryption, and Linux UDP
  batching together.

There was also a correctness prerequisite. Buffers passed to
`WgProxy.Write` are borrowed from `userwireguard`; its receive worker returns
them to the WireGuard pool as soon as `Write` returns. The old singular
`ProxyDevice.SendPacketNoCopy` handoff transferred those borrowed bytes to an
asynchronous Connect send. The retained singular and batch paths now copy each
borrowed packet into Connect's message pool before DeviceLocal takes ownership.

The implemented proxy-specific path now:

1. exposes a borrowed-batch operation through the optional `WgBatchTun`
   capability;
2. groups one decrypted WireGuard call by source proxy device, activates each
   device once, copies its packets into Connect-owned buffers, and calls
   `DeviceLocal.SendPacketsNoCopy` once so DeviceLocal performs exact-tuple
   grouping; and
3. ready-drains queued return packets into the buffers already supplied by
   WireGuard, with no coalescing wait.

Upload and download use independently measured bounds. Upload depth eight wins
at the exact `server/proxy` boundary: versus singular sends it reduces median
128-packet time from 11.428 to 9.458 microseconds at one CPU and from 11.415 to
8.010 microseconds at ten CPUs. Depths 64 and 128 remain positive versus
singular, but are slower than eight. Download depth 64 wins the ready-drain
benchmark; depth 128 regresses. `BatchSize` remains 64 so userwireguard can
supply the larger ready receive burst, while `Write` chunks that burst into
upload groups of eight.

Deterministic normal and race gates cover homogeneous and mixed source groups,
borrowed-buffer reuse immediately after `Write`, partial output capacity,
undersized buffers, ready-only read depth, cancellation, FIFO order, and exact
pooled ownership. The retained result measures the boundary around, but not the
ChaCha20-Poly1305 cipher itself; encrypted WireGuard correctness tests pass and
the batching change does not alter cipher work per packet.

## Provider NAT TCP: separate optimization goal

Provider NAT TCP is not part of `server/proxy`, but it is a high-value core
path: every TCP byte supplied by a provider crosses it in one direction or the
other. Its current pipeline is:

1. `RemoteUserNatProvider.ClientReceive` validates frames, groups admitted IP
   packets by exact directional five tuple, and uses one nonblocking
   `LocalUserNat` queue admission per group.
2. `LocalUserNat.runSendShard` immediately loops over that homogeneous group,
   reparses each packet, repeats the TCP-buffer lookup/admission, and enqueues
   one `TcpSendItem` per packet.
3. `TcpSequence` applies sequence/reorder/window state per packet and hands one
   `writePayload` per packet to a second channel. Its socket writer later
   ready-drains up to 64 payloads and uses `net.Buffers`/`writev`.
4. In the return direction, one upstream socket read can be 64 KiB. It is
   packetized to the inner MTU, ready-drained in batches of up to 64, and then
   encoded into route-compatible Transfer chunks of at most two frames / 3
   KiB. Socket-owned TCP return data retries Transfer admission on that flow's
   dedicated reader because those consumed upstream bytes cannot be recreated.

The existing provider/TUN microbenchmark is already above the gigabit target
on a clean same-host path. With the production depth of 64 it measured 368.89
MB/s at one CPU and 478.69 MB/s at ten CPUs for TCP upload, and 427.17 and
378.85 MB/s respectively for TCP download. That means provider NAT is not the
clean single-flow line-rate ceiling in isolation. It remains a priority for
high RTT, CPU efficiency, mobile providers, and many simultaneous flows.

The high-value provider-NAT targets, in order, are:

1. **High-BDP receive-window and RTT warmup.** The provider SYN advertises a
   literal 64 KiB window, then the first post-handshake ACK can advertise a
   memory-scaled 1 MiB window and grow to a 16 MiB ceiling. It also negotiates
   timestamps so the source can obtain useful RTT samples through a
   retransmission. On the measured 1 s RTT short upload, the timestamp-capable
   path reduced 25.087 seconds to 3.081 seconds. On the warmed 20 MiB workload,
   increasing the provider ceiling from 1 to 16 MiB reduced the median from
   27.016 to 11.238 seconds, while increasing the post-handshake start from 64
   KiB to 1 MiB reduced it from 18.270 to 11.238 seconds. At 100 Mbit/s and 1 s
   RTT the BDP is 12.5 MB, so the ceiling remains memory-target aware for
   providers with hundreds of live flows. The separate gVisor TUN maximum
   remains 4 MiB because its 16 MiB candidate overflowed the path queue.
2. **Preserve the ingress group through flow lookup and sequence admission.**
   The current code has already paid to prove that every member has one five
   tuple, then discards that fact. A group-aware TCP-buffer/sequence operation
   can use one map/LRU lookup, one channel admission, and one wakeup while still
   applying TCP sequence, ACK, and reorder state to members in order under the
   existing per-flow synchronization. No new global or send lock is needed.
3. **Measure ACK cadence during warmup.** ACK compression is bounded at 50 ms
   and sends early at half a window. That is reasonable at a large steady
   window but can interact with a 64 KiB start and long RTT. Instrument ACK
   delay, bytes acknowledged, advertised-window steps, and upstream write
   progress before changing the timer; a shorter unconditional timer could
   increase packets and CPU without improving bulk throughput.
4. **Keep the socket batching that already works.** The upstream writer already
   uses ready-only `writev(64)`, and the reader already reads 64 KiB and batches
   return delivery. Replacing either with another channel or copied aggregate
   is not a first-tier target. The optimization opportunity is above those
   boundaries, where a known group is currently flattened.

Required measurements are provider-specific rather than proxy measurements:
singleton versus homogeneous groups of 1/8/64 packets; one and ten CPUs;
single-flow and 8/64-flow load; clean and 100/500/1,000 ms RTT; time to first
64 KiB, warmed 32 MiB goodput, ACK count, advertised-window history, flow-map
lookups, channel admissions, socket writes, allocations, and pool balance.
Barrier-driven ownership and cancellation tests must precede any group-aware
production change.

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

### 6. Keep the safe 1,280-byte fast-P2P geometry

The fixed-MTU defect is resolved. Fast P2P v2 fragments at 1,188 bytes, which
produces an exact worst-case 1,280-byte IPv6 packet. Real-Pion and full-TUN
tests require complete delivery and zero oversized writes at both 1,400- and
1,280-byte outer MTUs. The measured upward-probing candidate improved a
synthetic 4 KiB-message carrier loop but did not reduce production full-TUN
fragment count or move 32 MiB P2P-fast goodput. It was reverted. Revisit growth
only if the logical Transfer message distribution changes enough to cross a
fragment-count threshold in the end-to-end workload.

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
| `/tmp/perfvar-full-serial-20260813-final-retained.log` | Final 342-test serial correctness gate after retained optimizations |
| `/tmp/perfvar-short-race-20260813-final-retained.log` | Final PERFVAR non-DB ownership/concurrency race gate |
| `/tmp/connect-full-race-20260813-final-retained-rerun2.log` | Complete Connect repository race suite |
| `/tmp/proxy-full-race-20260813-final-retained.log` | Complete proxy repository suite |
| `/tmp/sdk-full-race-20260813-final-retained.log` | Complete SDK race, module, and JavaScript suite |
| `/tmp/server-full-race-20260813-final-retained.log` | Server race run through the exhaustive route matrix and old singular full-stack bridge |
| `/tmp/server-full-race-tail-20260813-final-retained-env.log` | Race-tested server packages after `server/connect`; the leading full-DB PERFVAR attempt is intentionally invalid and superseded by the serial/short-race split |
| `/tmp/urnetwork-client-h1-tls-batch4-vs-8-20260812.log` | Client H1 saturated depth-four/eight comparison |
| `/tmp/urnetwork-server-h1-batch4-vs-8-20260812.log` | Server cleartext and TLS H1 depth-four/eight comparison |
| `/tmp/urnetwork-client-h1-depth8-sparse-20260812.log` | Client H1 sparse depth-eight latency control |
| `/tmp/urnetwork-server-h1-depth8-sparse-20260812.log` | Server H1 sparse depth-eight latency control |
| `/tmp/urnetwork-h3-batch8-vs-16-fixed-20260812.log` | Real QUIC H3 depth-eight/sixteen comparison |
| `/tmp/urnetwork-exchange-depth8-evaluation-20260812.log` | Exchange writer and read-dispatch depth evaluation |
| `/tmp/urnetwork-p2p-fast-udp-depth-evaluation-20260812.log` | Fast-P2P UDP drain depth comparison |
| `/tmp/urnetwork-p2p-route-channel4-vs-8-20260812.log` | Real WebRTC route-channel depth comparison |
| `/tmp/urnetwork-tun-nat-batch8-vs-64-20260812.log` | Shared TUN/NAT depth-eight/64 comparison |
| `/tmp/perfvar-final-baseline-rtt-h1-64k-20260813.log` | Three-run 500 ms / 1 s H1 control before provider timestamp negotiation |
| `/tmp/perfvar-final-timestamp-rtt-h1-64k-20260813.log` | Three-run 500 ms / 1 s H1 result with provider timestamps |
| `/tmp/urnetwork-tcp-mss-packetization-20260813.log` | Peer-MSS asymmetric/equal-MTU packetization comparison |
| `/tmp/urnetwork-tcp-sack-recovery-20260813.log` | Clean and one-loss SACK comparison; rejected |
| `/tmp/urnetwork-tcp-sack-three-drop-20260813.log` | Three-loss SACK comparison; rejected |
| `/tmp/urnetwork-tcp-sack-delayed-three-drop-20260813.log` | Delayed three-loss SACK comparison; rejected |
| `/tmp/perfvar-tcp-options-reno-h1-wan-warmed-20m-20260813.log` | Reno half of exact-seed WAN comparison |
| `/tmp/perfvar-tcp-options-cubic-h1-wan-warmed-20m-20260813.log` | CUBIC half of exact-seed WAN comparison; rejected |
| `/tmp/perfvar-tcp-tun-window4m-h1-1s-warmed-20m-20260813.log` | Correct 4 MiB gVisor TUN control for the isolated maximum-window comparison |
| `/tmp/perfvar-tcp-tun-window16m-h1-1s-warmed-20m-20260813.log` | Rejected 16 MiB gVisor TUN candidate; 2,304 queue drops and workload timeout |
| `/tmp/perfvar-tcp-provider-window1m-h1-1s-warmed-20m-count3-20260813.log` | Three-trace 1 MiB provider maximum control |
| `/tmp/perfvar-tcp-provider-window16m-h1-1s-warmed-20m-count3-20260813.log` | Three-trace retained 16 MiB provider maximum and 1 MiB initial-window result |
| `/tmp/perfvar-tcp-provider-initial64k-max16m-h1-1s-warmed-20m-count3-20260813.log` | Three-trace 64 KiB provider initial-window control with the 16 MiB maximum held fixed |
| `/tmp/urnetwork-wireguard-upload-depth-final-20260813.log` | Correct singular versus grouped WireGuard upload and depth comparison |
| `/tmp/urnetwork-wireguard-download-depth-final-20260813.log` | WireGuard ready-only download depth comparison |
| `/tmp/urnetwork-server-proxy-wireguard-borrowed-upload-20260813.log` | Exact `ProxyDevice` borrowed-to-owned upload depth comparison |
| `/tmp/perfvar-mobile-packet-group-all-routes-final-32m-count5-20260813.log` | Final paired packet-group A/B on every carrier |
| `/tmp/server-connect-mctcp-singular-control.log` | Same-build full-stack TCP singular-bridge control |
| `/tmp/server-connect-mctcp-group-final.log` | Same-build full-stack TCP retained packet-group bridge |
| `/tmp/server-connect-mctcp-group-final-race-short.log` | Retained grouped full-stack TCP race correctness gate |
| `/tmp/server-connect-mctcp-directional-group-final-race-short.log` | Retained grouped directional TCP race correctness gate |
| `/tmp/perfvar-final-retained-h1-lte-20m-run5-20260813.log` | Three complete paired H1 LTE-surrogate runs plus one fourth download |
| `/tmp/urnetwork-p2p-fast-path-mtu-growth-20260813.log` | Synthetic fast-P2P 1,280/1,400/1,500-byte geometry comparison |
| `/tmp/perfvar-p2p-fast-pmtu-baseline-20260813.log` | Three-run fixed-1,280-byte full-TUN P2P-fast control |
| `/tmp/perfvar-p2p-fast-pmtu-final-20260813.log` | Three-run upward-probing full-TUN P2P-fast candidate; rejected |
