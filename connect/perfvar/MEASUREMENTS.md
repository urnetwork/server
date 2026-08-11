# PERFVAR measurements

## Status

Historical baseline captured on 2026-08-10. These are userspace, same-host
results, not physical-device or Internet measurements.

The historical baseline consists of five attempts per scenario for the
clean path, the 500 ms and 1 s single-region paths, and the production H1
extender path. A separate deterministic probe records the current fast-P2P MTU
failure. Older one-run development logs are not mixed into the baseline.

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

### 1. Fix high-RTT route readiness before tuning high-RTT throughput

This is the highest-priority result. In 35 of the 36 non-extender failures, the
destination server accepted the inner TCP connection but the first response
never returned within the readiness boundary; the remaining legacy-P2P attempt
reached stage 3 and reset. All 12 extender failures had the first shape.
Increasing readiness payload and waiting for multiple modeled RTTs did not
remove the failure during harness development. Instrument the shared TUN/NAT
TCP path at packet enqueue, NAT translation, provider write, return
translation, TUN delivery, ACK, retransmission, and RTO boundaries. Keep the
existing failure records and zero-drop checks so a fix can be verified with
repeated 500 ms and 1 s runs.

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

### 5. Investigate the directional upload ceiling

Fast P2P improves download much more than upload: 69.3% versus legacy on
download, but 15.8% on upload. All clean uploads are substantially below their
corresponding downloads. Attribute the asymmetry across inner TCP flow control,
device/provider TUN buffering, return ACK delivery, message batching, and
shared-host scheduling. Parallel-flow and CPU/allocation profiles can separate
a single-flow congestion-window limit from a processing limit.

### 6. Make fast P2P safe on 1,280-byte outer paths

Either negotiate a conservative payload size, implement path-MTU discovery
with a reliable fallback, or fragment below the WebRTC/SRTP packet boundary.
The deterministic blackhole test must change from reproducing failure to
requiring successful, exact delivery with zero oversized writes before this is
considered resolved.

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

Run those as curated axes rather than one large Cartesian product. First make
the high-RTT readiness gate deterministic; otherwise failure censoring will
dominate the impaired-profile analysis.

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
