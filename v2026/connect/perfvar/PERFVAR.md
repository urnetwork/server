# PERFVAR — userspace performance variation harness

## Status

Implemented single-host harness. Physical-device and multi-host validation
remain outside this userspace test tier.

The harness runs in `server/connect/perfvar` on macOS inside `go test`. It
drives real application traffic through the Connect TUN boundary, provider
NAT, and each forced production carrier while applying deterministic userspace
network conditions. It requires no root access, network namespaces, Docker
networking, or `CAP_NET_ADMIN`.

The current implementation covers the four forced routes, seven workload shapes,
the initial and focused link profiles, a mobile resource surrogate, one
production extender per endpoint access path for exchange H1, bounded outage
recovery, live profile changes, direct-to-platform fallback and restoration,
address migration, one/three/five/nine-hop P2P streams, and two-edge exchange
routes with an independently conditioned internal link.

Measurements and conclusions belong in `MEASUREMENTS.md`. This document defines
what the harness actually measures, how to run it, and where its interpretation
must stop.

## Questions the harness can answer

1. How much useful throughput does each forced route preserve under controlled
   latency, loss, rate, queue, jitter, and MTU conditions?
2. What latency does interactive traffic experience while a route is busy?
3. How do P2P fast, P2P legacy, exchange H1, and exchange H3 compare under the
   same resolved profile?
4. Can fast P2P and exchange H3 preserve an exact inner TCP stream across a
   bounded outage?
5. How do smaller TUN buffers, smaller packet batches, and a paced application
   boundary affect the routes as a mobile-like resource surrogate?
6. How reliably do route setup and workloads complete at 500 ms and 1 s of
   application-user-to-`server/connect` round-trip latency?
7. How do throughput and wire cost compose across one, three, five, or nine P2P
   hops, and across two exchange edges with a separate internal profile?

It cannot answer physical radio, kernel VPN API, cross-host scheduling, real
NAT rebinding, or device thermal and battery behavior. Its transition results
cover controlled userspace address replacement and the production
network-change hooks; they are not measurements of every carrier or operating
system migration mechanism.

## Relationship to existing performance tools

PERFVAR complements the existing test systems.

- `stream_route_performance_test.go` measures comparable clean-path carrier
  capacity through a real server topology.
- `connect_multiclient_tcp_directional_perf_test.go` measures directional TCP
  traffic through the Connect TUN and provider path.
- Connect's WebRTC tests already use Pion `vnet` for focused carrier behavior.
- `server/connect/sim-latency` remains the large-fleet provider-selection and
  egress simulator. PERFVAR uses only a few clients so it can retain real
  packet-level protocols and full application paths.

## Interpretation boundary

These are userspace, same-host measurements. They provide repeatable evidence
about protocol behavior and relative route performance. They are not physical
Wi-Fi, cellular, iPhone, Pixel, or cross-host measurements.

| Area | What is modeled | Important limit |
|---|---|---|
| Exchange H1 and H3 | Real TLS/WebSocket or QUIC carrier over gVisor TUN access links | gVisor TCP is not the macOS or Ubuntu kernel TCP implementation |
| P2P fast and legacy | Real Pion ICE, DTLS, SRTP fast carrier, and SCTP DataChannel over `vnet`; a userspace wrapper conditions every physical UDP direction | The wrapper cannot reproduce radio, kernel, or cross-host behavior |
| Full application path | Real Connect TUN, route manager, contracts, Transfer, provider NAT, and host egress | All processes and clocks share one host |
| Mobile surrogate | Smaller buffers and batches plus application-boundary pacing | It does not model a radio, VPN platform API, battery, temperature, or mobile CPU |
| Outage and migration | Bidirectional blackhole/restoration, platform reconnect, P2P fallback/restore, and a controlled Pion address replacement | Real carrier NAT behavior and operating-system interface migration are not modeled |

Battery use, thermal throttling, radio scheduling, platform VPN API overhead,
hardware offload, and cross-machine CPU scaling require physical-device or
multi-host validation.

## Implemented topology

Every full-path workload enters through the application TUN and exits through
the provider NAT:

```text
application workload
  -> application TUN
  -> generated device client
       |-> exchange H1/H3 -> one server/connect edge + resident/exchange
       `-> P2P legacy/fast -------------------------------+
  -> provider client                                      |
  -> provider NAT <---------------------------------------+
  -> loopback workload server
```

The ordinary exchange fixture runs one real `ConnectHandler`, `Resident`, and
`Exchange` on one gVisor-backed edge. The split fixture runs two independently
addressed handlers and exchanges, pins the device and provider to different
edges, and conditions the production internal exchange TCP connection as a
separate link. Both endpoint access links remain independently observable.

The P2P fixture first uses the platform for signaling and promotion. After the
selected endpoint route is verified, a route controller suppresses the target
logical destination and every live endpoint `StreamId` alias on the platform
writer. On a multihop endpoint, `P2pRouteState.PeerId` names the adjacent
physical peer rather than the final application destination. The authenticated
`StreamId` alias is what lets the final-destination writer select that endpoint
route. The platform transport stays alive for control traffic, receive traffic,
and unrelated destinations. Suppression remains fail-closed if the selected P2P
route disconnects; only explicit test teardown restores platform payload
routing. The measured writer therefore has exactly one endpoint P2P payload
route, which may carry a stream across several physical hops, without destroying
the control plane. This setup does not measure promotion itself.

The P2P topology filter accepts `one-hop`, `three-hop`, `five-hop`, and
`nine-hop`. Extended paths use one independently shaped Pion network per
adjacency, reject non-adjacent application traffic, suppress the matching
platform payload aliases only after end-to-end stream readiness, and require
workload-bound counters at every physical hop. `split-exchange` is accepted for
H1 and H3 and requires an explicit internal exchange profile.

## High-latency topology semantics

The single-region profiles model latency between the application user and
`server/connect`, as requested:

- `single-region-500ms-rtt` adds a constant 250 ms in each direction on the
  application user's access path.
- `single-region-1000ms-rtt` adds a constant 500 ms in each direction on the
  application user's access path.
- The provider is treated as colocated with `server/connect` and receives the
  `clean-lan` access profile: 1 ms each way.
- An exchange calibration therefore has approximately 502 ms or 1,002 ms of
  composed end-to-end RTT, not two regional RTTs.
- A direct P2P route uses the selected 500 ms or 1 s profile on its direct
  application-to-provider `vnet` path.

The explicit `dual-region-500ms-rtt` and `dual-region-1000ms-rtt` profiles keep
the symmetric stress case. For exchange, both endpoint access paths receive the
named user-to-connect RTT, so their end-to-end delay is the sum of both access
paths. For direct P2P, the selected profile still describes the single direct
P2P link.

The scenario JSON makes this distinction visible with separate
`application_access_and_p2p_profile` and `provider_access_profile` fields. The
profile hash includes both.

Extreme RTT is also a completion/reliability test. Route-readiness and workload
timeouts are emitted as failed repetitions instead of disappearing from the
result set. A successful transfer alone must not be used to hide incomplete
repetitions.

## Extender semantics

`CONNECT_PERFVAR_EXTENDERS=1` is implemented only for `exchange-h1`, because
the production extender carries TCP/TLS H1 rather than H3 UDP. It means one
production extender on each endpoint's access path, not one extender total:

```text
application client -> extender -> server/connect
provider client    -> extender -> server/connect
```

The result field is therefore named `extender_count_per_user_path`.

For each access path, its base latency and processing delay are divided across
the client-to-extender and extender-to-edge segments. Adding the extender does
not multiply the configured end-to-end latency. A 500 ms application access
profile remains 500 ms round trip through the extender, and a 1 s profile
remains 1 s.

Rate, queue, loss, and MTU settings are currently applied to both extender
segments. Unlike delay, those effects can compose across the two segments. The
plain-HTTP test API uses a direct control link because the production extender
is TLS-only; the forced H1 data carrier uses the extender path.

Counts greater than one, serial extender chains, P2P through extenders, and H3
through extenders are not implemented. Unsupported route/extender combinations
are removed during scenario resolution; filters that leave no supported
scenario fail explicitly.

## Production protocols and test seams

The harness keeps the production protocols above narrow, optional network
injection seams. Nil settings retain production behavior.

Implemented Connect seams are:

- `PlatformTransportSettings.H3PacketConnFactory` for the client-side H3 UDP
  endpoint;
- `WebRtcSettings.Network` for native Pion socket enumeration and routing;
- the existing H1 `DialContextSettings`, including TLS over an injected
  `DialContext`;
- `ApiMultiClientGeneratorSettings.PlatformTransportSettingsGenerator`,
  `PlatformTransportMode`, and the nonblocking `PlatformTransportCreated`
  observer;
- exact `ClientStrategySettings.ExtenderConfigs` for a hermetic production
  extender path;
- `ExtenderSettings.Listen`, `DialContext`, and `ErrorHandler` for the two
  simulated extender segments and attributable failures;
- `ExchangeSettings.DialContext` for outbound internal exchange connections;
- the existing `NewConnectHandlerWithPacketConns` and
  `NewExchangeWithListeners` server constructors.

The full route and extender integration tests prove that the injected objects
carry traffic. Focused Connect and server tests pin the added seams' nil
defaults, settings immutability, successful and failed close ownership, and
host-network fallback. No package-level simulator flag is used, and every
added setting is inert when unset.

## Network model

### gVisor access and calibration links

Exchange access paths and untunneled calibration workloads use two production
gVisor TUN stacks joined by a custom directional packet scheduler. H3 UDP and
H1 TCP therefore see impairment below QUIC or TCP. H1 loss exercises gVisor TCP
retransmission and congestion control; it is not simulated by dropping bytes
from an already reliable stream.

Each direction owns one goroutine, a bounded nonblocking ingress queue, a token
bucket, a release-time heap, one reusable timer, a seeded random source, and
counters. Admission copies accepted bytes. A full receiver queue drops and
counts the packet rather than blocking a production receive callback.

The scheduler currently applies these operations:

1. reject or silently drop an oversized packet;
2. enforce packet and byte queue bounds;
3. apply a live blackhole or the selected seeded loss model;
4. calculate token-bucket serialization and burst allowance;
5. add base delay, processing delay, and bounded uniform jitter while
   preserving FIFO release order on that directional link;
6. delay selected packets for reordering and optionally schedule a duplicate;
7. deliver through a nonblocking TUN handoff or count receiver overflow.

Supported loss modes are none, seeded independent loss, deterministic every-N
loss, and seeded two-state burst loss. MTU modes are silent drop and a
synchronous packet-too-large error. Profile updates are serialized and expose
their actual application time.

The simulator unit tests pin seeds, loss vectors, token-bucket pacing, hard
queue bounds, delay and jitter bounds, duplication, reordering, MTU modes,
dynamic update boundaries, close/drain behavior, receiver isolation, and batch
equivalence.

### P2P `vnet` adapter

P2P uses Pion `vnet` so the real ICE, DTLS, SRTP, SCTP, and DataChannel stacks
remain above the simulated network. Each endpoint's UDP surface is wrapped by
the same directional scheduler used by the gVisor links. The wrapper implements
independent forward and reverse rate, burst, byte and packet queue bounds, base
delay, processing delay, seeded jitter, loss, duplication, reordering, MTU
behavior, and live outages. Pion's router delay, jitter, and queue are disabled,
so all configured impairment and its attribution live in the wrapper.

One `vnet` implementation limit requires an explicit ownership guard:

- Pion gives each `vnet` UDP socket a fixed 1,024-datagram read channel that
  silently drops overflow. The adapter exposes 1,023 tracked receive credits
  per physical direction and reserves the 1,024th physical queue slot for the
  one stale frame that can pass final router validation immediately before its
  destination closes. This shared bound covers all sockets in a direction, so
  it can apply backpressure earlier than independent per-socket queues would;
- each receive-credit lookup key is the destination socket plus, for connected
  UDP, its connected source. Exact and wildcard destination registrations use
  the same canonical endpoint keys. Missing destinations and wrong connected
  sources are rejected before `vnet` can silently discard a datagram and strand
  a credit;
- the scheduler owns and charges the original payload plus the modeled 28-byte
  IPv4/UDP outer overhead. A private 16-byte magic-and-generation frame occupies
  bytes already reserved inside that 28-byte overhead before the payload enters
  `vnet`; it adds no simulated link bytes and does not change MTU accounting;
- after scheduler impairment, the sender reserves the matching socket
  generation, creates the private frame, and marks that write router-pending.
  The direct router and every physical stream-hop router revalidate the framed
  generation, destination, and connected source at their final acceptance
  filter. A stale generation is rejected there. If close and rebind instead
  race the later Pion filter-to-NIC handoff, the receiver consumes the stale or
  malformed frame and continues reading until it finds its current generation.
  Current-generation reads strip the private header and release exactly one
  reservation;
- P2P calibration uses the same resolved profile but the gVisor
  simulator, not the same `vnet` implementation.

Route-wide measurement and post-workload fixed points join every directional
scheduler, then every router-pending write, and require a stable receive-credit
generation. Measurement snapshots also wait for both pending acquisitions and
router-pending writes to reach zero. They do not wait forever for Pion to
consume optional ICE or DTLS control datagrams already in a UDP socket. A
stable unread backlog is a valid baseline only when credit ownership is exact
and the admitted, read, canceled, outstanding, tracked-reservation,
stale-generation, and router-pending values remain unchanged across the
scheduler join. Any read, admission, cancellation, stale drop, router
completion, or replacement that crosses the candidate boundary changes that
generation and retries the complete source-to-carrier fixed point. Strict
carrier teardown still drains every credit to zero.

Snapshots record admitted, read, canceled, outstanding, blocked-acquire,
invalid-release, stale-generation, tracked-reservation, router-pending, and
high-water counts. Stable snapshot comparison includes stale-generation and
router-pending state; workload validation rejects any stale-generation delta or
nonzero router-pending ownership. A real `vnet` regression holds the receiver,
submits 1,024 unique datagrams, proves 1,023 are tracked while the 1,024th is
backpressured, and then verifies every identity after releasing the reader. A
separate close-and-rebind regression places one stale post-gate frame in the
reserved physical slot and verifies all 1,024 current-generation identities.
The direct and generalized stream tests deterministically pause before final
router revalidation and prove old generations cannot cross a same-tuple rebind.
Socket migration retains the same pools. Packet-size and outstanding-packet
high-water marks are lifetime diagnostics in live snapshots. After the
premeasurement fixed point, the harness atomically swaps fresh per-link and
per-packet-size epochs and a fresh receive-credit maximum epoch. Link and
packet-size interval maxima describe only work after the boundary. A
receive-credit epoch starts at the stable outstanding baseline, so
`MaximumOutstandingPackets` is the maximum absolute depth and may include an
inherited unread control backlog. Monotonic credit counters subtract the
baseline and remain workload-local; an unchanged backlog produces zero
outstanding delta. Larger setup maxima remain available in lifetime diagnostics.

Schema-3 run records carry distinct deterministic application/direct,
provider, and internal-link trace seeds. The route is excluded from trace
derivation, so compared carriers in one repetition begin with the same
route-excluded seed family. Each route performs different setup traffic before
the measured workload, however, and that traffic advances its scheduler's
random stream and every-N packet sequence. Workload behavior is reproducible
for the same route and source state, but current cross-route comparisons are not
packet-for-packet common-random-number pairs. P2P jitter and reordering remain
deterministic because the wrapper, not Pion's global router random source, owns
both. A future measurement-epoch scheduler reset could align post-setup random
decisions if stricter paired stochastic comparisons become necessary.

The clean profile uses a 32 MiB directional scheduler queue so its default
measured burst is non-limiting. This is separate from Pion's fixed per-socket
read channel, which is protected by the shared 1,023-credit pools and one
reserved physical queue slot per direction. Impaired queue profiles remain
deliberately bounded and attributable.

The current P2P fast carrier emits datagrams larger than a 1,280-byte outer
path. The ordinary P2P MTU correctness gate uses 1,500 bytes. The opt-in
`mtu-blackhole-1280` diagnostic expects the transfer to fail and requires the
failure to be attributable to oversized P2P datagrams. This remains a known
transport limitation, not a passing 1,280-byte P2P result.

## Network profiles

Profiles are synthetic starting points, not claims about every Wi-Fi or mobile
network. A profile contains the complete forward and reverse link settings,
inner and outer MTU, seed, and a source note. Results serialize the resolved
profiles and hash both endpoint profiles.

On client-edge links, forward means client to edge and reverse means edge to
client. The P2P fixture places the provider on `vnet`'s left side and the
application on its right side; its calibration swaps directions so reported
upload and download remain oriented from the application user's perspective.

### Initial profiles

| Profile | Forward/reverse rate | RTT per selected profile | Jitter | Loss | Queue intent |
|---|---:|---:|---:|---:|---|
| `clean-lan` | 1,000/1,000 Mbit/s | 2 ms | 0 ms | none | 32 MiB non-limiting clean control |
| `wifi-good` | 500/100 Mbit/s | 20 ms | 3 ms | 0.05% independent | about 50 ms at link rate |
| `lte` | 50/10 Mbit/s | 60 ms | 10 ms | 0.5% independent | about 100 ms at link rate |
| `mobile-poor` | 10/2 Mbit/s | 120 ms | 25 ms | two-state burst loss | about 200 ms at link rate |
| `wan` | 300/100 Mbit/s | 100 ms | 5 ms | 0.1% independent | about 100 ms at link rate |
| `single-region-500ms-rtt` | 100/100 Mbit/s | 500 ms application access | 0 ms | none | one RTT bandwidth-delay product |
| `single-region-1000ms-rtt` | 100/100 Mbit/s | 1,000 ms application access | 0 ms | none | one RTT bandwidth-delay product |
| `dual-region-500ms-rtt` | 100/100 Mbit/s | 500 ms on each endpoint access | 0 ms | none | one RTT bandwidth-delay product per access path |
| `dual-region-1000ms-rtt` | 100/100 Mbit/s | 1,000 ms on each endpoint access | 0 ms | none | one RTT bandwidth-delay product per access path |

### Focused profiles

Focused profiles start from `clean-lan` and change one primary axis:

- RTT: `rtt-0ms`, `rtt-10ms`, `rtt-25ms`, `rtt-50ms`, `rtt-100ms`, and
  `rtt-150ms`;
- independent loss: `loss-0bp`, `loss-1bp`, `loss-10bp`, `loss-50bp`,
  `loss-100bp`, and `loss-200bp`, where one basis point is 0.01%;
- jitter: `jitter-0ms`, `jitter-1ms`, `jitter-5ms`, and `jitter-25ms`;
- reorder: `reorder-0bp`, `reorder-10bp`, `reorder-100bp`, and
  `reorder-500bp`, where one basis point is 0.01%;
- rate: `rate-10mbps`, `rate-50mbps`, `rate-100mbps`, `rate-300mbps`,
  `rate-1000mbps`, and `rate-2500mbps`;
- outer MTU: `mtu-1280`, `mtu-1400`, and `mtu-1500`; these lower the inner
  MTU when needed to retain an 80-byte tunnel allowance;
- expected-failure MTU diagnostic: `mtu-blackhole-1280`, which leaves the
  clean inner MTU unchanged;
- queues: `queue-shallow`, `queue-one-bdp`, and `queue-deep`, with 5 ms,
  50 ms, and 500 ms queue targets at a 50 ms RTT;
- direction: `direction-asymmetric`, with 500 Mbit/s forward and 50 Mbit/s
  reverse capacity.

The harness intentionally selects a curated matrix rather than a full
Cartesian product.

The current authoritative route restrictions are:

| Axis | Routes used for comparisons |
|---|---|
| Three, five, or nine P2P hops | fast P2P only |
| Split exchange | exchange H1 and H3 only |
| Production extenders | one-hop exchange H1 only |
| Reorder and jitter | All routes; every physical direction uses the seeded wrapper scheduler. Jitter alone remains FIFO; only the reorder axis creates release inversions. |
| Queue | All routes; scheduler drops and P2P receive-credit admission are attributed separately |
| Rate | All routes |
| Direction asymmetry | P2P for a directional claim; applying it to both exchange access links creates the same end-to-end bottleneck in both directions |
| Outer MTU | Static `mtu-*` profiles lower the inner MTU and are correctness-gated on H3 and fast P2P; `mtu-blackhole-1280` deliberately retains the clean inner MTU to test missing dynamic path-MTU adaptation |

## Workloads

Every measured workload has an untunneled calibration through a paired gVisor
TUN path and a tunneled variant through the selected production route.

| Filter | Full-path behavior | Supported directions |
|---|---|---|
| `tcp` | One exact deterministic TCP stream with byte count and SHA-256 verification | upload, download |
| `tcp-warmed` | One TCP connection first carries one route-local bandwidth-delay product, crosses a fresh exact measurement boundary, then carries the measured payload | upload, download |
| `tcp-parallel` | Four independent exact TCP streams sharing one established route | upload, download |
| `quic` | Real inner QUIC stream with exact content verification | upload |
| `udp` | Sequence-numbered fixed-rate datagrams with delivery, duplicate, reorder, corruption, and same-clock latency accounting | upload, download |
| `latency-under-load` | UDP echo latency before, during, and after a concurrent TCP upload | upload |
| `web` | Three fresh HTTP responses: 16 KiB, 512 KiB, and 16 KiB, with first-byte and completion timing | download |

The default payload is 32 MiB. Without an explicit byte-count filter, it becomes
20 MiB for `lte` and `mobile-poor`. The two single-region profiles use 64 KiB
for cold `tcp`; `tcp-warmed` keeps the 32 MiB measured payload so its steady-state
sample spans many bandwidth-delay products.
Parallel TCP uses four flows and divides the selected payload across them, with
a 64 KiB minimum per flow. UDP defaults to one second at 5 Mbit/s with
1,000-byte datagrams.

The warmed workload derives one bandwidth-delay product from the route's
directional bottleneck and complete physical path. It sends those warmup bytes
on the same TCP connection before the timed payload and rejects a scenario if
warmup plus payload would exceed the explicit 256 MiB opening-contract bound.

Payload size affects TCP, parallel TCP, inner QUIC, and latency-under-load. The
web and UDP shapes have their own fixed scenario settings.

Workload records include useful bytes, duration, setup time where measured,
decimal MB/s and Gbit/s, latency distributions, UDP accounting, content hash,
allocation and garbage-collection deltas where implemented, and calibration
link snapshots.

## Mobile resource surrogate

`CONNECT_PERFVAR_RESOURCE=mobile-surrogate` changes explicit application/TUN
limits:

- channel capacity: 256 packets;
- TCP send and receive buffer default: 256 KiB;
- TCP send and receive auto-tuning maximum: 2 MiB;
- UDP send and receive buffers: 128 KiB;
- application boundary batch: eight packets;
- one application-boundary delay per nonempty read batch: 100 microseconds.

The application bridge preserves each read batch through one consuming
five-tuple-group send. Run records report its batch count, packet count, and
maximum observed batch size at the same carrier boundary as the workload.
They also report cumulative observed application-delay time and time blocked
inside the consuming group send, separating scheduler delay from downstream
backpressure.

The initial TCP buffer remains deliberately small, but its maximum must be
larger. Pinning both values to 256 KiB disables gVisor's normal send/receive
auto-tuning and creates a synthetic bandwidth-delay-product ceiling. A
controlled 32 MiB H1 sweep measured 41.04 Mbit/s at the fixed 256 KiB bound,
226.83 Mbit/s at 512 KiB, 372.01 Mbit/s at 1 MiB, 402.28 Mbit/s at 2 MiB, and
416.15 Mbit/s at 4 MiB. The 2 MiB bound captures nearly all of the measured
gain while retaining a finite per-connection limit. It also matches the
process-budget-scaled maximum used by a 32 MiB SDK process.

`default` uses a 4,096-packet channel and batches up to 64 packets while leaving
the production TCP and UDP buffer defaults intact.

The harness records `GOMAXPROCS`. Isolated helper processes validate both
resource profiles at `GOMAXPROCS=1` and `GOMAXPROCS=2`, so changing the runtime
scheduler setting cannot contaminate the parent test process. This is still a
scheduler surrogate, not an endpoint-specific CPU quota or a physical mobile
device. Suspend/resume and explicit operating-system memory ceilings are not
modeled.

## Filters and commands

The DB-backed route fixtures use the standard local server test environment:

```sh
export WARP_ENV=local
export WARP_SERVICE=test
export WARP_DOMAIN=bringyour.com
export WARP_BLOCK=test
export WARP_VERSION=0.0.0
export BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com
export BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com
export GOMAXPROCS=10
```

Correctness commands must not inherit an opt-in measurement, expected-failure
probe, or isolated resource-helper selection from an earlier shell:

```sh
unset CONNECT_PERFVAR_MEASURE CONNECT_PERFVAR_FAILURE_PROBE
unset CONNECT_PERFVAR_RESOURCE_HELPER CONNECT_PERFVAR_RESOURCE_HELPER_NAME
```

The canonical complete correctness gate runs the whole package and keeps every
DB-backed fixture serial:

```sh
go test -p=1 ./connect/perfvar -parallel=1 -count=1 -timeout=0
```

The single opt-in measurement entry point is:

```sh
CONNECT_PERFVAR_MEASURE=1 \
go test ./connect/perfvar -run '^TestPerformanceVariations$' \
  -count=1 -timeout=0 -v
```

Available controls are comma-separated sets unless stated otherwise:

```text
CONNECT_PERFVAR_MEASURE=1
CONNECT_PERFVAR_ROUTE=p2p-fast|p2p-legacy|exchange-h1|exchange-h3
CONNECT_PERFVAR_PROFILE=<one or more exact profile names>
CONNECT_PERFVAR_WORKLOAD=tcp|tcp-warmed|tcp-parallel|quic|udp|latency-under-load|web
CONNECT_PERFVAR_DIRECTION=upload|download
CONNECT_PERFVAR_TOPOLOGY=one-hop|three-hop|five-hop|nine-hop|split-exchange
CONNECT_PERFVAR_INTERNAL_PROFILE=<exact profile name for split-exchange>
CONNECT_PERFVAR_EXTENDERS=0|1
CONNECT_PERFVAR_RESOURCE=default|mobile-surrogate
CONNECT_PERFVAR_SEED=<decimal integer>
CONNECT_PERFVAR_RUN_COUNT=<positive decimal integer>
CONNECT_PERFVAR_BYTE_COUNT=<positive decimal byte count>
```

Defaults are all four routes, `clean-lan`, `tcp`, both directions, `one-hop`, no
extenders, default resources, seed `20260810`, five fresh repetitions, and a
32 MiB payload subject to the profile-specific reductions described above.

Unknown values fail. Unsupported workload/direction pairs are skipped. An
extender selection retains only exchange H1. If the filters leave no supported
scenario, scenario resolution fails instead of silently substituting another
one.

Performance measurements reject the race detector. The complete non-DB
correctness and ownership tier runs under `-race` with `-short`:

```sh
go test -race -p=1 ./connect/perfvar -short -parallel=1 \
  -count=1 -timeout=30m
```

Run the canonical production destination/stream-alias gate from the server
repository before a schema-3 campaign:

```sh
(
  cd ../connect
  go test -race . -count=1 -timeout=30m \
    -run '^(Test(SendSequenceContractUsesFinalDestinationWithStreamAlias|ReceiveAckUsesDestinationOnlyRoute|VerifiedReceiveContractAliasSurvivesSequenceAndStreamGenerations|IntermediaryStreamSequenceMatchesAdjacentDestinationsBothDirections)|TestP2pStreamProbe(ReadyAcrossOneThreeFiveAndNineHops|IncompleteMiddleHopIsNeverApplicationEligible|MultipleDestinationAliasesShareReadyTransport))$'
)
```

The focused serial DB gate consumes those semantics through real one-hop and
multihop P2P fixtures:

```sh
go test -p=1 ./connect/perfvar -parallel=1 -count=1 -timeout=0 -v \
  -run '^(TestFullTunConstructionRollbackClosesReady(OneHop|ThreeHop)P2pRoute|TestProductionStreamP2pExtendedTopology|TestFullTunP2pFast(OneHop|ThreeHop|FiveHop|NineHop)TopologyCorrectness|TestFullTunP2pFastThreeHopExtendedApplicationWorkloadsCorrectness)$'
```

The schema-3 campaign blockers are also an explicit serial DB gate:

```sh
go test -p=1 ./connect/perfvar -parallel=1 -count=1 -timeout=0 -v \
  -run '^(TestPerfvarCorrectnessFixtureJoinsPremeasurementPackPublication|TestPerfvarSingleRegion(500ms|1000ms)EveryRouteCorrectness|TestPerfvarEveryRoute(ApplicationWorkloadDirections|MobileSurrogate|WarmedTCPDirections)Correctness|TestPerfvarExtremeProfileRoutesCorrectness|TestPerfvarRegionalWarmedTCPThirtyTwoMiBCorrectness)$'
```

These focused commands diagnose the production alias and campaign contracts;
they do not replace the canonical complete correctness gate above.

The known P2P 1,280-byte MTU limitation has a separate, intentionally
failure-oriented diagnostic:

```sh
CONNECT_PERFVAR_FAILURE_PROBE=1 \
go test ./connect/perfvar -run '^TestFullTunP2pFastMtuBlackholeDetection$' \
  -count=1 -timeout=5m -v
```

## Measurement lifecycle

Each repetition is fresh and performs:

1. derivation and recording of distinct deterministic per-segment trace seeds;
2. untunneled calibration with the resolved end-to-end profile;
3. construction of a real server, exchange, device, provider, TUN, NAT, and
   selected carrier;
4. exact route-readiness traffic and P2P promotion when applicable;
5. an exact fixed-point boundary joining application and provider source
   publishers, live flow entries, send-Pack publication, every directional
   scheduler, and every P2P receive-admission generation while baselining only
   an unchanged unread Pion control backlog;
6. connection/NAT setup or an explicit warmup, followed by a second exact
   boundary, fresh route-local maximum epochs, and a workload-local carrier
   snapshot;
7. one timed, verified application workload;
8. an exact post-workload fixed-point join in the same
   source-to-Pack-to-carrier order and workload-local carrier, simulator,
   process, allocation, and timing snapshots;
9. ordered teardown, strict P2P receive-credit drain, and bounded
   handler/exchange idle checks.

Construction is transactional. Every acquisition boundary can inject a
failure, and rollback synchronously closes the exact partial graph while
continuing after independent cleanup errors. Transport callbacks publish route
events to an unbounded test-only queue and return without taking controller
locks; setup and measurement use ordered fences, and teardown first stops
callback producers, then joins the controller. Generated, provider, and
intermediary clients use `Client.CloseAndWait`, which closes admission and
joins stream, transfer, contract, encryption, and control workers before pool
reconciliation.

Execution is run-major rather than scenario-major. Compared routes stay
adjacent for one trace, and their first position rotates deterministically on
successive repetitions. This prevents every H1 sample, for example, from
always running on a colder host than every H3 or P2P sample.

The route-ready probe is outside the workload timer. Setup duration remains a
separate result. Each run has a topology-, workload-, RTT-, rate-, flow-, and
payload-aware outer context with a 12-minute floor and a 45-minute ceiling.
Workload deadlines scale with modeled RTT, bottleneck rate, and aggregate bytes
sharing that bottleneck.

The calibration profile is composed from the application and provider access
segments for exchange. For P2P it uses the direct profile oriented from the
application user's perspective.

A run is calibration-valid only when untunneled goodput is at least 10% above
tunneled goodput. A correct but calibration-limited run keeps its measurements
and receives an `invalid_reason`; it must not support a route-throughput claim.
This 10% rule identifies obvious simulator ceilings but is weaker than the
original aspirational two-times headroom target.

## Result format

Schema version 3 emits one compact JSON record per run and one aggregate JSON
record per scenario. Every line begins with `[perfvar]` so records can be
extracted from `go test -v` output.

A run record includes:

- record, schema, and run-schedule versions, run index, scenario hash, and
  combined profile hash;
- the per-run trace version, identity hash, and application/direct, provider,
  and internal-link seeds;
- the complete resolved scenario, including application and provider profiles;
- Go, OS, architecture, CPU, `GOMAXPROCS`, race mode, server revision and dirty
  state, Connect revision and dirty state, content hashes of both complete dirty
  worktrees, and the `userspace-same-host` label;
- untunneled and tunneled workload results;
- route setup duration, tunneled/underlay efficiency, and useful/wire
  efficiency;
- directional simulator snapshots and P2P fast/legacy carrier counters;
- application-bridge batch count, packet count, and maximum batch size;
- correctness, failure stage, failure reason, calibration-invalid reason, and
  process-wide goroutine point samples taken immediately before and after the
  run's synchronous lifecycle boundaries. These samples are diagnostic only;
  they are not a quiet-time guess or proof of leak freedom.

Failure stages include `calibration`, `route-readiness`,
`measurement-boundary`, `workload`, and `verification`. These failures produce
records and later repetitions still run. Invalid filters and failures that
prevent scenario identity or test-environment construction remain fatal.

The aggregate includes total, correct, failed, and invalid run counts;
individual correctness and validity vectors; median, p95, and worst goodput and
duration; median setup, latency, loaded latency, efficiency, and wire
efficiency. Failed and calibration-invalid runs are excluded from numeric
throughput aggregation but remain in completion and validity counts. Every
comparison must still inspect `individual_run_valid` and `invalid_run_count`.

After emitting every selected scenario, the test reports an error if any
repetition was incorrect. This preserves all diagnostic records without making
partial completion appear successful.

## Correctness coverage

Ordinary tests currently provide:

- deterministic simulator replay, loss, rate, queue, delay, duplication,
  reorder, MTU, update, close, receiver isolation, and batch gates;
- clean route-level delivery for exchange H1, exchange H3, P2P legacy, and P2P
  fast;
- exact bidirectional full-TUN TCP for all four forced routes;
- route-local bandwidth-delay-product warmup on the same TCP connection,
  followed by a fresh source-to-carrier boundary and exact measured payload;
- warmed TCP in both directions on all four carriers, plus representative
  32 MiB measured phases after 500 ms and 1 s route-local BDP warmups;
- exact bidirectional TCP on all four carriers with 500 ms and 1 s of
  application-user-to-`server/connect` round-trip latency;
- clean full-TUN parallel TCP, UDP in both directions, inner QUIC, web, and
  latency-under-load;
- P2P fast under the LTE loss model and exchange H3 under seeded independent
  loss;
- exchange H3 at a 1,280-byte outer MTU and P2P fast at a 1,500-byte outer MTU;
- exact inner TCP recovery across a 300 ms bidirectional outage for P2P fast
  and exchange H3;
- clean, 500 ms, and 1 s production extender carrier paths plus a clean
  full-TUN exchange H1 extender path;
- exact full-TUN TCP in both directions over one-, three-, five-, and nine-hop
  fast-P2P topologies, with per-hop carrier attribution and non-adjacent route
  rejection;
- exact full-TUN TCP in both directions over split H1 and H3 exchange
  topologies, with two real server edges and internal-link attribution;
- warmed TCP and inner QUIC over three-hop fast P2P, and inner QUIC over split
  H3 exchange;
- a scheduled rate/delay/jitter/loss change and restoration during one live
  exchange H3 TCP stream;
- platform network-change reconnect, P2P-to-platform fallback and restoration,
  and controlled P2P address migration through production hooks;
- isolated `GOMAXPROCS` sweeps and repeated goroutine, heap, and checked-out
  message-pool reconciliation for both resource profiles;
- deterministic scenario filters, hashes, high-latency access scope, and
  failure-aware aggregates, including dirty-worktree content hashing;
- exact application and provider flow-marker lifecycles, send-Pack publication,
  carrier and receive-credit fixed points, route-local maximum epochs,
  final-destination suppression through an adjacent peer's authenticated stream
  alias, generated platform-transport ownership, and deterministic cross-reset
  and stale-generation regressions;
- destination-plus-connected-source receive admission, 1,023-credit
  backpressure below Pion's 1,024-datagram queue, no-cost generation framing,
  final direct and per-hop stream router revalidation, the post-gate receiver
  fallback, and router-pending/stale-generation snapshot boundaries;
- every partial-construction acquisition and ready H1, H3, direct-P2P, and
  three-hop rollback, including cleanup-error continuation;
- nonblocking route callbacks, exact FIFO publication across a linked-queue
  gap, unbounded burst retention, close/admission ordering, route-state overlap
  refcounts, and observer close/wakeup behavior; and
- blackhole cancellation and held terminal-receipt barriers for TCP, QUIC,
  loaded latency, and both UDP directions.

UDP loss is a measured outcome, but corruption and invalid sequence accounting
are correctness failures. Forced P2P requires its selected fast or legacy
counters and zero fallback. Forced exchange requires zero P2P payload. Teardown
waits for the real handler, exchange, simulator, and route workers.

## Remaining interpretation limits

The following boundaries are intentionally outside this single-host harness
and must not be inferred from its results:

1. Two or more serial extenders and extender support for routes other than
   exchange H1.
2. Real carrier NAT rebinding, operating-system interface replacement,
   suspend/resume, and platform VPN API behavior. The implemented migration
   case replaces a Pion userspace address and invokes the production
   network-change hook.
3. Endpoint-specific CPU quotas, physical memory pressure, battery use,
   thermal throttling, radio scheduling, and hardware offload. The isolated
   `GOMAXPROCS` cases are scheduler surrogates only.
4. Cross-process and cross-host throughput. Split exchange uses two logical
   production edges, but every process, clock, and network scheduler still
   shares one macOS host.
5. A physical-device, real-radio, or multi-host validation tier.

Two measured boundary conditions also require care:

- The P2P fast path does not currently adapt when an unannounced 1,280-byte
  outer MTU is imposed while retaining the clean 1,440-byte inner MTU; use the
  opt-in `mtu-blackhole-1280` diagnostic. A statically configured 1,280-byte
  path lowers the inner MTU and has a separate correctness gate.
- Extreme 500 ms and 1 s access profiles can expose route-readiness or workload
  timeouts. Completion ratio is part of the result and must be reported beside
  throughput from successful repetitions.

## Implementation map

```text
server/connect/perfvar/
  PERFVAR.md                    implemented scope and interpretation
  profile_test.go               profiles, validation, and hashes
  link_test.go                  directional scheduler and counters
  network_test.go               gVisor TUN network and Pion vnet composition
  p2p_link_net_test.go          Pion UDP scheduler and receive admission
  p2p_link_net_surface_test.go  real Pion socket-to-scheduler seam
  p2p_link_net_benchmark_test.go focused scheduler/credit benchmarks
  p2p_route_state_trace_test.go exact nonblocking P2P route lifecycle trace
  workload_test.go              route-neutral calibration workloads
  route_test.go                 server carriers and production extender
  topology_test.go              complete TUN, route, NAT, and provider path
  extended_topology_test.go     multihop P2P and split exchange fixtures
  topology_workload_test.go     full-path workload variants
  campaign_correctness_test.go  all-route/profile/workload correctness gates
  campaign_warmed_correctness_test.go long BDP-warmed correctness gates
  full_tun_construction_test.go transactional rollback and fixed-point checks
  platform_route_direct_test.go platform suppression route tests
  platform_transport_owner_test.go generated transport/client ownership
  bridge_send_tracker_test.go   exact application-source lifecycle
  provider_return_tracker_test.go exact provider-source lifecycle
  send_pack_lifecycle_tracker_test.go exact Pack publication lifecycle
  measurement_boundary_test.go  source-to-Pack-to-carrier fixed point
  events_test.go                loss, MTU, and outage integration
  resource_validation_test.go   scheduler and lifecycle reconciliation
  scenario_test.go              filters, metadata, schema, and aggregation
  performance_test.go           opt-in measured matrix
  simulator_test.go             deterministic simulator validation
  race_enabled_test.go          race-build result marker
  race_disabled_test.go         ordinary-build result marker
  MEASUREMENTS.md               measured baselines and analysis
```

Production injection seams remain in the Connect and server packages that own
them. The server `connect` parent does not import this child package, and the
simulator is absent from production hot paths unless an explicit test setting
is supplied.

## Completion assessment

| Original phase | Current state |
|---|---|
| Freeze clean controls and identify results | Implemented |
| Simulator core and deterministic validation | Implemented for gVisor links; Pion adapter limitations documented |
| H3 and P2P network injection | Implemented |
| H1 and exchange integration | Implemented for one edge and two-edge split H1/H3 |
| Full TCP, QUIC, UDP, web, parallel, and loaded-latency workloads | Implemented |
| Path events and extended topologies | Implemented for live profile change, outage, reconnect, fallback/restore, controlled migration, multihop P2P, and split exchange |
| Resource surrogate and baseline campaign | Mobile surrogate, isolated scheduler sweep, and lifecycle reconciliation implemented; physical devices remain external |

The harness is complete for its userspace single-host scope: it compares all
four forced routes, extended P2P and exchange topologies, controlled path
events, and explicit resource surrogates. Physical-device and multi-host work
is a separate validation tier, not an omitted code path in this harness.
