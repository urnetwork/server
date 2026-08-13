# Connect glossary

This glossary defines the domain terms and semantic qualifiers used by tests
and benchmarks under `server/connect`, including `server/connect/perfvar`.
Read a compound test name as a base subject followed by independent axes. For
example, `TestConnectWithAsymmetricContractsWithForceStreamEncrypted` combines
the asymmetric contract model, forced stream routing, and required per-peer
encryption.

The test configuration is always the final authority. Older test names are not
perfectly normalized, so the absence of a qualifier must not be interpreted as
the opposite setting. In particular, use `No...`, `With...`, `Force...`,
`Required`, `Only`, and `AllowFallback` only for the axis they name.

When a new semantic qualifier is added to a Connect test or benchmark name, add
it here in the same change. Ordinary assertion verbs such as `Accepts`,
`Rejects`, `Preserves`, and `Returns` keep their normal English meanings.

## Core Connect test qualifiers

| Qualifier | Meaning in the Connect tests |
|---|---|
| `NoContract` or no-contract | The peers are explicitly allowed to exchange data without billable transfer contracts. Forward contract enforcement is disabled and no transfer balance is deducted. |
| `WithSymmetricContracts` or symmetric | Both peers advertise provide modes and ordinary contracts exist in both directions. Each side pays for the direction it sends through its own contract relationship. |
| `WithAsymmetricContracts` or asymmetric | The normal requester/provider relationship. One peer funds the relationship and the provider serves it. The provider-to-requester direction uses companion return traffic, so the requester pays for both directions and the provider does not. This describes contract/accounting direction, not equal or unequal link bandwidth. |
| `NoNack` | The test does not inject the historical Connect no-ack data messages called NACKs. Reliable ACK-required traffic is still present. See **ACK, NACK, and `NoAck()`** below. |
| `WithChaos` or chaos | The main Connect matrix enables `ExchangeChaosSettings.ResidentShutdownPerSecond`, which randomly cancels residents and forces route recovery. It does not mean packet loss unless a specific test says so. A focused test such as `PingTrackerChaos` may instead randomize that component's operation order or timing. |
| `TransportReform` | The main test repeatedly closes the current platform transports, pauses briefly, and creates new transports around message bursts. It exercises route removal, reconnection, sequence continuity, and retry behavior. |
| `NoTransportReform` | Keeps the platform transports stable instead of deliberately replacing them between bursts. It is useful for exposing bugs that fresh transports would otherwise mask. |
| `WithNewInstance` or new instance | A reconnect may use a new authenticated `InstanceId` while retaining the same `ClientId`. This models logout/login or process replacement and verifies that stale instance state does not supersede the new generation. |
| `Encrypted` | Enables Connect's end-to-end, per-peer `SendSequence`/`ReceiveSequence` encryption session. In the base `Encrypted` matrix this means `EncryptionModeRequired`. It is distinct from TLS protecting H1, H3, WebRTC, or an exchange socket. |
| `AllowFallback` | With `Encrypted`, selects `EncryptionModeOpportunistic`: use the per-peer cipher after it establishes, but allow plaintext application traffic when it cannot. It means encryption fallback, not route or carrier fallback. |
| `WithForceStream` or force stream | Sets the `ForceStream` transfer option. The transfer is keyed onto the stream lane and uses contract-stream routing even when no intermediary is required. It does not put a `StreamId` in the transfer path. |
| `H1` | Forces the platform H1 carrier: a WebSocket carried by HTTP/1.1 over TCP. Production normally protects it with TLS; some hermetic tests use plain loopback WebSocket. |
| `H3` | Forces the platform H3 carrier: HTTP/3/QUIC over UDP, with QUIC's TLS security. |
| `Auto` | Requests transport-mode election instead of pinning a carrier. Direct H1/H3 modes rank above DNS packet-translation modes; equal-ranked modes are sticky once selected. The current `PlatformTransport.run` starts only H1 for `Auto`, while the other auto runners remain disabled, so current integration tests observe H1 behavior. |
| `Dns` or `H3Dns` | Carries H3/QUIC UDP packets inside DNS request/response-shaped packet translation for networks where direct UDP is filtered. It is an availability carrier, not the DNS application workload. |
| `DnsPump` or `H3DnsPump` | The DNS packet-translation variant that sends empty DNS requests at a bounded rate so the server has requests against which it can return queued response data. |

### How the main axes compose

- Contract topology, reliability (`NoNack`), resident chaos, transport reform,
  instance replacement, carrier mode, forced stream routing, and encryption
  are independent axes.
- `Encrypted` without `AllowFallback` is required/fail-closed encryption.
- `EncryptedAllowFallback` is opportunistic encryption.
- `H1`, `H3`, `Auto`, `Dns`, and `DnsPump` choose only the client-to-platform
  carrier. They do not choose P2P fast versus legacy or exchange internals.
- `NoTransportReform` concerns deliberate test churn. It does not prohibit a
  transport from failing naturally or being closed during teardown.

## Identities, endpoints, and topology

| Term | Definition |
|---|---|
| application or user | The application-side workload whose packets enter the TUN. PERFVAR names upload and download from this endpoint's perspective. |
| client | A Connect protocol endpoint. A client owns sequences, contracts, route selection, receive callbacks, and one or more transports. It is not synonymous with the application user; provider and intermediary endpoints are clients too. |
| `ClientId` | Stable identity of one Connect endpoint. It can survive reconnects and a new `InstanceId`. |
| `InstanceId` | Identity of one authenticated running instance of a client. A new value establishes a newer connection generation for the same `ClientId`. |
| device client or generated client | The client generated for the application-side tunnel by `ApiMultiClientGenerator`. It is the Connect endpoint that represents the local app traffic. |
| provider | The client selected to provide network or public connectivity. In the full-TUN tests it owns the provider NAT and connects the tunnel to the workload server. |
| intermediary | A client that carries one physical segment of a multihop stream between the two endpoint clients. |
| edge | One `server/connect` service endpoint containing a `ConnectHandler` and an `Exchange`. A split-exchange topology has two distinct edges. |
| `ConnectHandler` | The server entry point for H1 and H3 client transports. It authenticates and hands an accepted connection to the exchange/resident layer. |
| exchange | The server-side fabric that owns residents, accepts or creates internal exchange connections, and forwards messages between the edges that currently host the source and destination clients. |
| resident | Server-side state for a connected client on one exchange. It joins the platform transport to internal forwarding and owns an internal Connect client for control/stream work. |
| accepted transport | An exchange connection accepted by the local edge. In I/O tests, “accepted” distinguishes this socket direction from one dialed outbound. |
| outbound transport | An exchange connection dialed from this edge to the edge hosting another resident. |
| forward | The internal exchange connection/queue used to forward a message toward another edge or resident. It is not the provider's Internet forwarding operation. |
| platform | The server-backed route through a `PlatformTransport`. In these tests “platform route” and “exchange route” usually describe successive parts of the same end-to-end path. |
| exchange route | Payload travels from a client platform transport into `server/connect`, through a resident and possibly an internal exchange connection, then out to the destination client. |
| P2P route | Payload travels over an endpoint WebRTC stream rather than through the exchange data plane. The platform route remains available for signaling, control, receive traffic, and unrelated destinations. |
| direct P2P or direct route | A P2P endpoint route without an exchange payload hop. “Direct” does not imply one physical hop: a Connect stream can wrap several adjacent P2P hops. |
| direct transport mode | In transport election, H1 or H3 as opposed to H3-over-DNS packet translation. This is a different use of “direct” from direct P2P. |
| `AllowDirect` | A performance-profile policy that allows a multi-client path to discover and promote a P2P stream. For same-network peers it can be forced on without fixing the rest of an auto profile. |
| host-direct, direct call, or direct write | Test wording meaning a dependency or socket is used without an injected wrapper. It has no P2P implication. |
| route | One selectable path by which a `MultiRouteWriter` can reach a logical destination. A route is installed only while its transport is live and eligible. |
| route manager | Publishes transports as routes and supplies destination-keyed multiroute writers. It also removes routes when transports disconnect. |
| multiroute writer | Selects among the live routes for a destination. Correctness must not require the reply to choose the same physical path on which the request arrived. |
| route promotion | Making a ready P2P route eligible for endpoint payload and suppressing the matching platform payload aliases. |
| route suppression | Test control that removes a platform payload route for a selected destination while retaining platform control traffic. It proves the intended carrier actually carries the measured payload. |
| route fallback | Moving payload to another eligible carrier after loss or incompatibility. This is distinct from encryption `AllowFallback`. |
| fail-closed route | When an explicitly selected P2P route disappears, the test leaves the target payload route unavailable rather than silently restoring platform payload. Restoration must be an explicit event. |
| one-hop | One physical P2P adjacency between the endpoint clients. For ordinary exchange scenarios it is also the default topology label, not a claim that no internal components exist. |
| three-hop, five-hop, nine-hop | A stream made from that many adjacent physical P2P carriers. Every hop is separately shaped, observed, and required to carry workload traffic. |
| split exchange | Device and provider connect to different server edges. Their endpoint access links and the internal edge-to-edge exchange TCP link are conditioned independently. |
| hop | One adjacent physical transport segment in a stream. The stream presents all hops as one logical endpoint path. |
| stream | A logical client-to-destination path that may contain one or more P2P hops. It is active only after end-to-end readiness and is withdrawn while establishing or disconnected. |
| `StreamId` | Authenticated identity of a stream. It can be a route-manager alias, but it is not part of the wire `TransferKey` and should not be required to route ordinary replies. |
| adjacent peer | The physical peer at the other end of one P2P hop. On a multihop route this need not be the final application destination. |

## Contracts and transfer lanes

| Term | Definition |
|---|---|
| contract | A bounded accounting and authorization object attached to a transfer sequence. It records provided bytes and closes or checkpoints as capacity is consumed or the sequence changes. |
| active contract | A contract currently valid for the transfer lane. The server can enforce that forwarded payload has one. |
| ordinary or provide contract | The contract used in the provider-facing direction of a requester/provider relationship. |
| companion contract or companion traffic | The return direction associated with an existing ordinary contract. `CompanionContract()` selects this lane and limits the destination to peers with a suitable active relationship. |
| contract enforcement | Server forwarding rejects data without an active matching contract. The no-contract tests disable this deliberately. |
| escrow | Transfer capacity reserved/funded for a public contract. Same-network `NetworkPeer` policy can use bounded no-escrow sizing; `ForceStream` alone does not grant that policy. |
| contract fill fraction | Fraction of a contract consumed before another contract is prepared. Tests lower it to force contract rollover and closure behavior. |
| contract checkpoint | An accounting record of acknowledged and still-unacknowledged bytes before a contract or sequence is replaced. |
| contract close | Finalizes a contract's accounting. Tests verify that teardown, drain, and route changes neither lose nor double-charge bytes. |
| `ContractKey` | Local key that separates contract queues by destination and lane fields such as companion, force-stream, network-peer, and encryption role. It is not the received `TransferKey`. |
| `TransferKey` | Path-independent wire identity of a transfer lane. It carries the lane fields needed to reply consistently, but not a redundant source ID or `StreamId`. The authenticated receive path supplies the source/destination relationship. |
| transfer lane | One independent reliable or no-ack sequence identity between peers. Force-stream, companion, network-peer, and encryption role can split traffic to the same destination into distinct lanes. |
| `TransferPath` | Authenticated receive path containing the source identity and path metadata. Receive callbacks can use it with the `TransferKey` to construct a path-independent reply. |
| destination | The logical peer ID passed to send. Route selection matches this destination and chooses any live eligible path. |
| source | The authenticated peer delivered to the receiver. It comes from the receive path, not from a redundant untrusted field in the transfer key. |
| `ForceStream` | Lane/routing option that requires contract-stream behavior even with zero intermediaries. It is independent of companion and network-peer policy. |
| `NetworkPeer` | Contract sizing/retention policy for an authenticated same-network destination. It is not a synonym for P2P or `ForceStream`. |

## Frames, packs, ACKs, and reliability

| Term | Definition |
|---|---|
| frame | One typed protocol message, such as application data or a control message. |
| Pack | The Transfer sequence unit containing one or more frames plus sequence, lane, contract, and ACK/NACK metadata. |
| `TransferFrame` | The outer encoded Transfer envelope that carries a Pack or Transfer control records over a transport. |
| send sequence | Sender state that assigns sequence positions, retains reliable data, manages contracts, consumes ACK records, and retries until completion or timeout. |
| receive sequence | Receiver state that orders Packs, detects gaps, dispatches frames, and emits ACK records for reliable data. |
| ACK | Acknowledgement of reliable Transfer sequence progress. A reliable Pack is retained and can be resent until its ACK arrives. The sender's completion callback follows that reliability outcome. |
| pure ACK | A transport/IP packet containing only acknowledgement state and no new application payload. Tests distinguish its ownership from data-bearing packets. |
| ACK compression or coalescing | Combining acknowledgement ranges/events for a short bounded period to reduce ACK traffic. Setting the compression timeout to zero emits per-message ACKs. |
| NACK, in historical Connect Transfer naming | A Pack sent with `TransferOptions.Ack=false`, encoded as `Pack.Nack=true`. It requests no acknowledgement and no Transfer retry. This is **not** the usual networking meaning “negative acknowledgement.” It may be dropped if its matching contract is not active. |
| `NoAck()` | Public send option that creates the historical Connect NACK/no-ack Pack. Its callback reports send admission/completion rather than a remote ACK. |
| `NoNack` test | A test variant with no injected `NoAck()`/`Pack.Nack` data. It does not disable ordinary ACK processing. |
| unknown-wrap NACK | Encryption control telling a sealer that the receiver could not decrypt a wrapped epoch. Unlike the historical data NACK, this is a genuine negative signal and can trigger bounded re-handshake recovery. |
| reliable | Transfer owns retransmission until acknowledgement. Exchange payload and provider-return TCP use this where Connect is the last component capable of reproducing the bytes. |
| unacknowledged or no-ack | Transfer does not retransmit. Direct device-originated IP traffic can use this so inner TCP/QUIC owns recovery and UDP/ICMP retain datagram semantics. |
| resend | Re-emission of a retained reliable Pack after loss, gap, or timeout. A resend keeps the original message identity. |
| gap | Missing sequence position at a receiver. Gap timeout and later sequence reform decide when stale state can be retired. |
| ordered | Delivery preserves sequence order. “Unordered” in the legacy DataChannel describes carrier ordering; Transfer can still impose its own ordering where configured. |
| FIFO | First-in, first-out ordering at one queue or batching boundary. Batching tests require FIFO across batches and errors. |
| provider return | Packet synthesized from traffic read by the provider-side socket and sent back to the application. TCP provider return remains Transfer-reliable because the upstream socket bytes have already been consumed. |

## Carrier and protocol terms

| Term | Definition |
|---|---|
| carrier | The concrete mechanism carrying encoded Connect Transfer data: platform H1/H3, P2P DataChannel, or the P2P fast SRTP lane. |
| control plane | Signaling, identity, contracts, keys, route readiness, pings, and management traffic. A platform route can remain active for control while P2P carries payload. |
| data plane or payload plane | Application Transfer/IP traffic whose throughput and routing are under test. |
| WebSocket or WS | Message-framed connection over HTTP/1.1/TCP used by H1. `WSS` is WebSocket protected by TLS. |
| TLS | Record protocol providing authentication/confidentiality for TCP-based carriers. Transport TLS and Connect per-peer sequence encryption are separate layers. |
| QUIC | Reliable, encrypted transport over UDP. H3 uses QUIC; a PERFVAR QUIC workload is inner application traffic and is independent of whether the outer route is H1 or H3. |
| WebRTC | P2P association stack used for discovery, security, migration, and both legacy and fast P2P data lanes. |
| ICE | WebRTC connectivity establishment and candidate-pair selection. Tests make candidate discovery hermetic where external STUN/interface selection would add nondeterminism. |
| DTLS | Datagram TLS layer inside the WebRTC association. |
| SRTP/RTP | Secure real-time transport used by the negotiated P2P fast lane to carry authenticated fragments. Despite the names, this lane carries Connect data, not media. |
| SCTP DataChannel | WebRTC reliable DataChannel used by the legacy P2P carrier and compatibility fallback. |
| P2P legacy | Transfer frames use the reliable unordered WebRTC SCTP DataChannel. |
| P2P fast | Transfer frames are fragmented into independently authenticated SRTP datagrams, bypassing SCTP. Inner protocols or Transfer retain the appropriate recovery responsibility. |
| P2P auto | Negotiate the fast lane and use it when ready; otherwise retain legacy DataChannel compatibility. This is distinct from platform `TransportModeAuto`. |
| fast-only | Test mode that requires the negotiated fast carrier and treats fallback as failure. |
| legacy-only | Test mode that forbids fast carrier use. |
| fast fallback | P2P auto could not use the fast lane and selected the legacy DataChannel. It is neither encryption fallback nor platform route fallback. |
| capability negotiation | Peers advertise compatible fast-lane codec/version support. Mixed versions intentionally have no common fast codec and fall back safely. |
| probe | Bounded traffic used to discover, validate, or promote a candidate route. Probe traffic is outside the measured workload boundary unless a test explicitly measures it. |
| discovery | Natural route establishment through production peer discovery and probing. |
| forced route | The harness suppresses competing payload routes after readiness so the named carrier must carry the workload. “Forced” does not bypass readiness or authentication. |
| prime | Complete route readiness and a bidirectional application probe before a workload or an exact measurement boundary begins. |
| readiness | Positive evidence that the intended route, carrier, and endpoint direction are active. Object construction alone is not readiness. |
| ping | Small liveness/control message used to observe a connection or candidate. It is not an application latency workload unless named as such. |
| connection announce | Server/client handshake that publishes a newly connected instance and synchronizes its state. |
| proxy protocol or PP | Header protocol that preserves the original source/destination address across a load balancer. `V1` identifies the text form in tests. |

## TUN, IP, and NAT terms

| Term | Definition |
|---|---|
| TUN | Layer-3 virtual network interface carrying IP packets between an application stack and Connect. PERFVAR uses a userspace gVisor-backed TUN boundary on one macOS host. |
| full TUN or full-TUN | End-to-end test path from the application workload through application TUN, generated device client, selected Connect route, provider client/NAT, and workload server. |
| gVisor | Userspace network stack used by the single-host test harness. Its TCP behavior is not a claim about macOS, Linux, iOS, or Android kernel TCP. |
| NAT | Network address/port translation between tunneled IP flows and provider-side sockets. |
| local user NAT | Provider-side component that terminates/creates host sockets for tunneled user flows. |
| remote user NAT | Application/device-side component that maps application TUN flows to Connect clients and selected providers. |
| multi-client | `RemoteUserNatMultiClient`; manages generated clients/providers and chooses among them for application flows. It does not mean multihop. |
| upload | Traffic from the application user toward the provider/workload server. |
| download | Traffic from the provider/workload server toward the application user. |
| bidirectional | Upload and download are active in the same measured phase. |
| inner protocol | TCP, UDP, QUIC, ICMP, or other IP traffic carried inside Connect. |
| outer protocol or underlay | H1/H3/WebRTC and their physical TCP/UDP packets carrying the inner traffic. |
| MTU | Maximum packet size accepted by a link. PERFVAR's `OuterMtu` applies to the simulated physical carrier packet, not directly to the inner TUN packet. |
| PMTU or path MTU | Smallest usable MTU across the physical path. H3 enables path MTU discovery; fast P2P uses a fragment payload sized for the IPv6 minimum outer MTU. |

## PERFVAR route, workload, and resource qualifiers

| Qualifier | Meaning |
|---|---|
| `exchange-h1` | Full-TUN route forced through platform H1 and the server exchange. |
| `exchange-h3` | Full-TUN route forced through platform H3 and the server exchange. |
| `p2p-legacy` | Full-TUN P2P route forced onto the legacy SCTP DataChannel carrier. |
| `p2p-fast` | Full-TUN P2P route forced onto the negotiated SRTP fast carrier. |
| `tcp` | One exact inner TCP bulk flow. |
| `tcp-warmed` or warmed | Sends an explicit warmup before the measurement boundary, then measures a distinct payload over the established flow/route. Warmup bytes are excluded. |
| `tcp-parallel` or parallel | Several concurrent inner TCP flows; payload and timeout accounting are per resolved flow count. |
| `udp` | Timed offered-rate datagram workload. Completion uses exact source publication and terminal receipt markers rather than TCP EOF. |
| `quic` | Inner application QUIC workload over the selected outer route. |
| `web` | HTTP-like request/response workload, including chunked/EOF completion cases. |
| `latency-under-load` | Interactive probes measured while a concurrent bulk flow loads the same path. Teardown must join both probe and bulk workers. |
| calibration | Untunneled or controlled reference workload used to establish what the modeled link itself can deliver. It is not counted as route throughput. |
| warmup | Traffic before the start boundary that establishes congestion windows, contracts, or route state. It must not leak into measured counters. |
| measured payload | Bytes deliberately placed between exact start and end measurement boundaries. |
| default resource | Normal test buffer, batch, and `GOMAXPROCS` settings. It is still a same-host userspace test. |
| mobile surrogate | Smaller buffers/batches and a paced application boundary used as a resource constraint. One nonempty TUN read batch pays one modeled application wake before the whole batch enters Connect; the delay is not charged once per packet. It is explicitly not a physical phone, radio, battery, or thermal model. |
| extender | Production H1/TLS forwarding hop inserted on each endpoint access path. Extender count is per user path; H3 and P2P extender routes are not implemented. |
| single region | The named high RTT is applied to the application user's access path while the provider is colocated with the edge on a clean LAN profile. |
| dual region | Both endpoint access paths receive the named RTT, so exchange end-to-end delay composes both access paths. |

## PERFVAR network-profile qualifiers

| Qualifier | Meaning |
|---|---|
| profile | Complete deterministic forward/reverse link configuration: rate, burst, queue, delay, processing delay, jitter, loss, duplication, reordering, MTU behavior, and seed. |
| clean LAN | High-capacity, low-delay profile intended as the non-limiting control. “Clean” means no configured impairment; it does not mean zero software overhead. |
| Wi-Fi good, LTE, mobile poor, WAN | Named synthetic profile bundles. They model link parameters, not a claim that the host is a real radio or WAN. |
| RTT | Round-trip time. A profile normally stores one-way delay per direction and the path composes those delays into RTT. |
| `rtt-Nms` | Focused profile changing the round-trip-delay axis while holding unrelated axes controlled. |
| latency or delay | Time added before packet delivery. Processing delay is an additional configured per-direction component. |
| jitter | Seeded bounded variation added to delay. `jitter-Nms` isolates that axis while preserving FIFO release order on each directional link; it does not silently add packet reordering. |
| loss | Intentional terminal packet drop. Models include seeded independent loss, deterministic every-N loss, and seeded two-state burst loss. |
| `loss-Nbp` | Loss probability in basis points; 100 basis points is 1 percent. |
| duplicate | A packet is deliberately delivered more than once. Counters distinguish duplication from retransmission. |
| reorder | A selected packet receives extra delay so later packets may arrive first. `reorder-Nbp` expresses its probability in basis points. |
| rate or bandwidth | Token-bucket serialization limit in bits per second. `rate-Nmbps` isolates this axis. |
| burst | Bytes the token bucket can admit immediately. It is not the same as application message burst size. |
| BDP | Bandwidth-delay product: bytes that can be in flight while filling a path at its configured rate and RTT. |
| queue | Bounded packet/byte storage before scheduled delivery. Queue overflow is an attributable drop, not backpressure. |
| shallow, one-BDP, deep queue | Focused queue profiles sized for short, approximately one-bandwidth-delay-product, or long buffering. |
| blackhole | Link accepts or observes traffic but delivers none during the blackhole interval. Restoration is a separate event. |
| outage | Time-bounded blackhole or route unavailability followed by restoration. Tests require the same logical flow to recover where the protocol promises recovery. |
| MTU blackhole | Oversized outer packets are silently dropped rather than returning packet-too-large. |
| oversize error | Oversized packet is rejected synchronously with a packet-too-large disposition. |
| direction-asymmetric | Forward and reverse directions intentionally use different link settings. It is unrelated to asymmetric contracts. |
| seed | Fixed pseudorandom input controlling loss, jitter, reordering, and scheduling traces. Same seed/profile means reproducible impairment decisions. |
| live profile change | Link settings change while a route is active. Snapshots identify the exact generation and application boundary. |

## Load and batching qualifiers

| Qualifier | Meaning |
|---|---|
| saturated | The writer has a continuously ready backlog, so throughput and batch formation can be measured. |
| sparse | The next message is released only after the preceding message reaches the receiver. It proves ready-only batching does not wait to manufacture a batch. |
| singleton | Writer dequeues and writes exactly one logical message per dispatch. |
| ready drain | After the first blocking dequeue, consume only messages already available without waiting, up to bounded message/byte limits. |
| separate | Ready messages are gathered but still written as separate underlying operations. It isolates scheduling/deadline savings from byte coalescing. |
| coalesced | Complete ready frames are bracketed and emitted in fewer socket/TLS writes without changing their WebSocket message boundaries. |
| batch | Bounded ordered group processed by one dispatch. A batch must preserve FIFO and explicit ownership on partial failure and cancellation. |
| batch size | Maximum ready message count, also bounded by a byte limit. It is a cap, not a delay target; sparse traffic remains singleton. |
| packet batch | Ordered packet list crossing one TUN, native callback, queue, or dispatch boundary. It can contain more than one flow until a five-tuple grouping boundary classifies it. |
| directional five-tuple or exact flow | IP version, protocol, source IP/port, and destination IP/port in the packet's current direction. Reversing source and destination is a different group. ICMP uses its parser-defined flow identifier in the port fields. |
| packet group or homogeneous group | Ordered members of one packet batch that share the same exact directional five-tuple. The group receives one policy outcome, route/update snapshot, provider selection, and logical Transfer admission; payload-aware inspection can still consume each member in order. |
| group decision | One conservative result for a homogeneous group. A later incident or drop prevents the whole group from reaching a provider; the implementation does not split allowed and blocked members onto different routes. |
| logical group admission | One all-or-none ownership handoff of a packet group to one selected SendSequence. Transfer may emit several bounded wire Packs afterward, but those chunks do not rerun provider selection or race independently. |
| semantic batch | Several application frames inside one logical Connect Pack or group. Its membership survives opaque forwarding even when physical socket writes split or combine messages. |
| physical I/O batch | Several already-complete messages or datagrams handled in one scheduling turn or syscall. It reduces dispatch/write overhead without changing their logical Pack/frame boundaries. |
| application boundary delay or `AppDelay` | PERFVAR's modeled cost of waking and handing one nonempty TUN read batch to Connect. It is paid once per batch; placing it inside a per-packet loop creates an artificial route-independent throughput ceiling. |
| TCP buffer default | Initial gVisor send or receive buffer for a new endpoint. It is not necessarily the endpoint's lifetime limit when auto-tuning is enabled. |
| TCP auto-tuning | gVisor growth of a TCP send or receive buffer as congestion-window and receive-rate evidence justify it, up to the configured maximum. A test that makes default and maximum equal disables useful growth. |
| TCP buffer maximum or window ceiling | Per-connection upper bound for an auto-tuned TCP buffer. If it is below the route's bandwidth-delay product, one flow becomes window-limited even when the carrier has unused capacity. |
| route-independent ceiling | Similar throughput across materially different carriers because a shared endpoint, application, scheduler, or harness boundary is limiting all of them. It is evidence to inspect the common path before optimizing one carrier. |
| read-ahead | Complete messages or packets consumed from an upstream buffer before the downstream consumer asks for each one. Ready-only read-ahead does not wait for more input, but it still transfers ownership and weakens immediate backpressure. It does not necessarily reduce socket reads when a buffered reader already fetched the bytes. |
| channel depth or buffer depth | Maximum queued items between independently scheduled producers and consumers. It controls burst absorption, ownership memory, backpressure, and queueing latency; it is not interchangeable with a writer's per-turn batch size. |
| write coalescing | Combining bytes from several complete logical frames into fewer lower-layer writes. Logical WebSocket or Framer boundaries remain unchanged. |
| `writev` | Vectored TCP write used by exchange `net.Buffers` to submit several framed messages without first copying them into one contiguous buffer. |
| `sendmmsg` or `recvmmsg` | Linux system calls sending or receiving several UDP datagrams per syscall. Non-Linux fallback loops can preserve batch scheduling without reducing datagram syscalls. |
| GRO, GSO, or offload | Packet aggregation/segmentation across the TUN or socket boundary. GRO combines received TCP segments for processing; GSO delegates segmentation on transmit. These are distinct from application message batching. |
| loopback | Real local TCP/UDP sockets on the same host. It exercises the socket stack but not a physical network. |
| goodput | Useful application payload bytes per second, excluding carrier framing and retransmission overhead. |
| throughput | Rate at the stated observation boundary. Tests should say whether this is useful payload, Connect bytes, or outer wire bytes. |
| wire efficiency | Useful bytes divided by observed outer-carrier bytes. Compression or framing can make different counter scopes look surprising, so scopes must match. |
| first message or cold path | Initial send includes route nomination, contract bootstrap, handshake, and other setup not present in a steady run. |
| warmup | Untimed work that establishes routes, handshakes, flow control, caches, and worker readiness before the measured boundary. Warmup results are not counted as workload throughput. |
| premeasurement or post-workload | Lifecycle phase before the exact measurement boundary or after the terminal workload boundary. Its counters and ownership must reconcile but must not be attributed to measured payload. |
| effective rate | Rate that remains after all configured serial links and schedulers are composed. It may be lower than any one nominal link rate. |
| advertised burst | Immediate byte allowance configured for a rate-limited link. A clean no-drop queue must hold at least this burst or the profile contradicts its own promise. |
| ping-pong | Application request/echo round trip used as a user-visible latency sample. It is distinct from transport ping. |
| drain time | Time for an admitted in-flight backlog to finish after the producer stops. It is not exchange service drain. |

## Deterministic correctness and lifecycle qualifiers

| Qualifier | Meaning |
|---|---|
| correctness | Exact content, direction, route, carrier, completion, and ownership assertions. A fast result that violates one of these is not a performance result. |
| deterministic | Outcome is controlled by explicit state and synchronization, not scheduler luck, arbitrary sleeps, or repeated probability. |
| barrier | Explicit synchronization edge that holds the production operation at a known lifecycle point. Timeouts around barriers are deadlock escapes, not evidence that “nothing happened.” |
| held | A barrier deliberately retains an operation or owner at the named point until the test releases it. |
| exact | The test identifies the specific packet, callback, generation, owner, or boundary rather than inferring it from aggregate timing. |
| admission | The component has accepted ownership or responsibility for work. Admission must occur before a close/join boundary can promise to wait for it. |
| reservation | Capacity or ownership is reserved before publication or execution. Tests distinguish reserved work from work merely waiting outside the owner. |
| publication | A state, route, packet, or result becomes visible to another component. Publication is often a separate barrier from allocation or admission. |
| in flight | Admitted work has not reached its terminal disposition. It may be queued, scheduled, executing, or awaiting acknowledgement. |
| terminal disposition | Final delivery, intentional drop, rejection, cancellation, or ownership return. Every admitted packet must reach exactly one terminal disposition. |
| terminal marker | Explicit end-of-workload datagram/control token whose source publication and downstream receipt establish UDP completion. |
| terminal idle or quiescent | All producers admitted before the boundary, schedulers, receiver credits, and terminal callbacks have reached a stable finished state. |
| boundary | Exact start/end cut used to separate setup, warmup, workload, and teardown counters. A boundary must join all earlier publications that can affect the interval. |
| snapshot | Coherent state captured at one synchronization point. Aggregate and per-class values must come from the same snapshot. |
| generation | Version of a route, transport, link profile, socket, client instance, or measurement interval. A stale generation must not publish into a newer one. |
| stale | Belongs to an older generation and must be ignored, retired, or attributed without mutating current state. |
| ownership | Exactly one component is responsible for a pooled buffer, socket, callback, queue item, or goroutine at each point. Transfer, return, and release must be explicit. |
| pool balance | Checked-out pooled buffers after a fully joined scope equal the baseline, with coherent per-size-class attribution. |
| return | Release one pooled reference to its pool. It does not mean provider-return traffic unless qualified. |
| drain | Remove queued owners and either process or return them after admission closes and workers join. |
| close | Stop new work and initiate teardown. Unless documented otherwise, `Close` alone need not wait. |
| join, `CloseAndWait`, or `WaitForIdle` | Wait until admitted workers/callbacks and their owned resources have completed. A joining API must not be called from the callback it is joining. |
| cancel | Signal work to stop through context cancellation. Cancellation is not a join and does not by itself prove ownership returned. |
| rollback | Reverse every successfully completed construction/acquisition step after a later step fails. Cleanup continues after an individual cleanup error and returns an aggregate. |
| construction stage | Named point immediately after acquiring or publishing one resource. Failure injection there verifies exact rollback ownership. |
| race test | Test run under Go's race detector, often with expanded deterministic time allowances. Passing `-race` does not replace lifecycle or ownership assertions. |
| timeout | Maximum wait/deadlock escape or modeled protocol deadline. A test must use a positive event/barrier for correctness rather than treating elapsed time as proof of absence. |
| retry | A bounded new attempt after a retryable failure. Tests pin whether identity/generation is retained or replaced. |
| recovery | Restoration of promised correctness after outage, loss, restart, or route change. It includes completion and ownership, not only reconnect. |
| migration | Live endpoint, resident, route, or address changes while preserving the logical flow/state promised by that layer. |
| replacement | A newer client, resident, route, or socket generation supersedes the old one. Cleanup of the old generation remains independently joinable. |
| backpressure | Producer is made to wait or reject because downstream capacity is full. It is different from a nonblocking queue-overflow drop. |
| deadlock | Progress cannot continue because components wait cyclically or an uninterruptible worker prevents teardown. Deterministic tests hold the exact wait edges. |
| leak | Ownership remains live beyond the component's documented join boundary. A late but eventually returned owner is still a join-order bug if the boundary promised quiescence. |

## Server service-drain and discovery qualifiers

| Qualifier | Meaning |
|---|---|
| service drain or exchange drain | Stop admitting new connections/nominations and close or migrate existing resident work within bounded deadlines. It is unrelated to draining a write queue. |
| drain excuse | Short-lived marker explaining that a client's reconnect/disconnect was caused by planned service drain, so reliability accounting does not treat it as an organic failure. |
| Track A / excuse | Drain mode that records excuse markers and lets clients reconnect to the same service. |
| migrate | Drain coordination directs clients/residents toward another service/edge while preserving stream and peer state. |
| straggler | Connection/resident that did not leave during the ordinary drain window and is handled by the bounded sweep. |
| nomination | Exchange decision selecting/creating the resident that should own a client. A draining exchange refuses new nominations. |
| peer discovery | Control plane that tells clients which same-network peers are available and tracks connect/disconnect changes. |
| provide change | A peer changes the modes/resources it offers; discovery must publish the updated capability without inventing a new identity. |
| key event | Database/control event used to invalidate or repair distributed discovery state. Drop-repair tests prove convergence after an event is missed. |
| resident replacement | A newer resident for the same client supersedes the previous one without leaving stale discovery or route state. |
| late join | A client appears after the observer/discovery stream is already active and must still be published. |
| network isolation | Clients in different logical networks must not appear as peers or receive one another's network-scoped traffic. |
| nomination refused | Classified reason a resident nomination was rejected; counters must attribute the exact cause. |

## Additional server and harness vocabulary

| Term | Definition |
|---|---|
| relay | The server payload path `transport -> resident -> forward -> resident -> transport`. Relay pool-balance tests load this whole path and join it before comparing ownership. |
| bridge | PERFVAR worker that moves packets between the application TUN and the generated multi-client path. Bridge trackers prove that the exact workload packet was admitted and reached a terminal disposition. |
| directional link | One independently configured source-to-receiver scheduler. A bidirectional network has separate forward and reverse links, queues, seeds, counters, and terminal-idle state. |
| forward/reverse direction | Direction relative to one link's construction, not necessarily application upload/download. The scenario resolves which physical link corresponds to each application direction. |
| receive credit | Reservation for one Pion `vnet` destination queue slot. Credits apply backpressure before Pion's fixed channel can silently overflow, and must be released exactly once by read, close, or cancellation. |
| connected socket | UDP socket constrained to one remote address. Packets from a different source must not consume its receive credit. |
| wildcard socket | UDP listener eligible to receive from any matching source until a more specific socket/generation owns the destination. |
| Pion | Go WebRTC implementation used by the production P2P association and PERFVAR's real ICE/DTLS/SRTP/SCTP stack. |
| `vnet` | Pion's userspace virtual network. PERFVAR wraps its UDP surfaces with directional schedulers; `vnet` itself is not the impairment model. |
| candidate | Possible connection, flow, provider, or route not yet selected. Tests distinguish candidates that are accepted, dormant, failed, or promoted. |
| winner | Candidate that positively claims the expected flow/readiness role and serves the workload. |
| loser | Candidate that did not claim the role. It still owns a worker/socket that must be canceled and joined. |
| accepted candidate | A socket connection accepted by a listener but not yet proven to be the expected logical flow. Accepted does not mean selected. |
| dormant candidate | Accepted candidate that makes no useful progress. It must not prevent a later valid candidate from winning or teardown from completing. |
| flow claim | Deterministic match of a connection to an expected logical TCP flow/tuple. Every expected flow must be claimed once, and substitutions are rejected. |
| flow window | Tracker for an exact provider-return flow, target byte range, and completion tokens. It rejects unrelated, overshooting, failed, or substituted completions. |
| socket tuple | Protocol plus local/remote IP addresses and ports that identify a provider flow. |
| carrier boundary | Coherent snapshots and lifecycle fences bracketing only traffic on the selected outer carrier. It excludes setup, later teardown, and unrelated global traffic. |
| tracker | Test observer that owns explicit lifecycle state for named events, such as Pack sends, provider returns, bridge sends, or no-ack sends. Closing a tracker joins admitted publications. |
| trace | Ordered, generation-aware event history used to prove route or carrier state transitions. A trace observes production callbacks but must not change their behavior. |
| watermark | Highest terminal/publication identity a tracker has fully joined. A failed terminal can advance it only according to that tracker's documented semantics. |
| attribution | Mapping a count/drop/failure to its exact physical link, direction, generation, and cause. A total without consistent per-cause attribution is invalid. |
| classification | Decision that an observed disposition is expected for the configured policy. An impairment can be correctly counted yet still invalidate a scenario if it was not configured. |
| filter drop | Harness intentionally rejects traffic outside a modeled P2P adjacency or destination. It is distinct from seeded loss, queue loss, or MTU loss and must be attributed separately. |
| hard queue admission | Packet/byte queue limits are enforced at admission; overflow drops immediately rather than allowing hidden extra capacity. |
| serialization queue | Token-bucket/wire scheduler state that models time consumed on a rate-limited link. Drops after scheduling can still consume wire time. |
| release inversion | Delivered packet order differs from submission order because a scheduled later packet is released first. Reordering counters must match actual inversions. |
| network change | Production signal that physical network conditions/interfaces changed. Tests require platform reconnection or P2P fallback/restoration as appropriate. |
| address migration | P2P endpoint address changes while the logical route is alive. The route must rebuild against the current socket generation without accepting stale packets. |
| reset | Full state snapshot that replaces the receiver's prior set, such as `StreamReset`; an empty legacy reset clears all. It is not always an error. |
| resume | Reuse/re-establish logical state after a disconnect. Encryption resume tests require a reset handshake rather than accepting stale cipher state. |
| unwrapped | Context-dependent: encryption uses it for plaintext outside an encrypted wrapper; H1 batch tests use “unwrapped fallback” for a connection without the batching wrapper. Neither meaning implies unencrypted transport TLS. |
| auth frame | First framed H3 client authentication message carrying JWT, app version, and instance identity. Pool tests require its decode buffer to return on success and failure. |
| out-of-band control or OOB | API-backed contract/key/control work outside the in-band Transfer route. Its admitted HTTP callbacks have their own close/join lifecycle. |
| passive speed | Connection capacity inferred from normal sent/received byte windows without adding synthetic traffic. The maximum proven rate is monotonic. |
| synthetic speed | Explicit speed-test traffic used when connection age and passive evidence do not already prove sufficient capacity. |
| synthetic gate | Policy deciding whether a synthetic speed test is allowed. A young connection waits; adequate passive evidence suppresses the synthetic test. |
| speed stop | Control ending one matching synthetic speed measurement. An unsolicited/malformed stop without reader state is rejected. |
| connection rate limit | Admission policy limiting new client connections in a window/burst. It is unrelated to PERFVAR link bandwidth. |
| minimum message-length limit | Lower bound large enough for mandatory protocol/control frames, including the encryption handshake, even when application test payloads are smaller. |
| maximum message length | Framer cap rejecting an encoded message above the configured size. It includes framing/control needs, not just application payload size. |
| prebound listener or packet connection | Socket created and passed in by the caller. The receiving component must close it on every accepted ownership path, including initialization failure. |
| host-network fallback | A nil injection seam makes the component dial/listen using ordinary host networking. Tests ensure a failed injected dependency cannot silently fall through to this path. |
| typed nil | Interface value whose dynamic type is set but whose underlying pointer is nil. Batch/connection helpers reject it rather than treating it as a usable object. |
| response writer upgrade | HTTP handler takes ownership of the underlying connection for WebSocket/QUIC setup. Pre-upgrade HTTP writes must pass through and batching starts only for post-upgrade framed traffic. |
| Nginx proxy-protocol path | Compatibility fixture that places an Nginx-shaped proxy-protocol preface in front of an exchange TCP connection. It verifies address/header handling, not the production deployment of Nginx itself. |
| chunked response | HTTP response with chunked transfer coding and no predeclared body length. The web workload completes at a clean EOF after all chunks. |
| EOF | End-of-file/read-stream completion. In web/TCP tests it is positive only after exact expected content; an early EOF is a failure. |
| proxy peer or hidden peer | Client configured as a proxy device. It counts as connected but is intentionally omitted from visible same-network peer discovery. |
| derivative client | Client with a `SourceClientId`. It is not a top-level network peer, receives no peer announcements, and cannot use the fast peer-discovery API as an ordinary client. |
| large network | More peers than fit in one peer-update frame. Discovery must converge through batched reset/replay and later diffs. |
| IPv4 or `udp4` | Four-byte IP-address form. Outer packet accounting includes IPv4 and UDP headers rather than only payload. |
| IPv6 minimum MTU | 1,280-byte outer path guarantee. Fast P2P fragmentation is sized so its worst-case IPv6/UDP/SRTP/RTP/custom-header datagram does not exceed it. |
| maximum payload | Largest application or carrier payload accepted at the named boundary after its framing and MTU constraints are applied. It is not automatically the maximum encoded wire packet. |
| headroom | Capacity above the measured workload needed so the harness or underlay does not become the ceiling. A throughput claim is invalid when calibration lacks the required headroom. |
| compressed wire bytes | Carrier bytes after any batching/coalescing or protocol compression at the named counter. Compare them only with useful bytes from the same boundary and interval. |
| dirty Git state | Source tree contains uncommitted changes. Performance records preserve this fact because a commit identifier alone cannot reproduce that binary. |
| `GOMAXPROCS` | Go scheduler limit on simultaneously executing Go code. Resource and buffer benchmarks sweep it because batching benefits can change with scheduling contention. |
| helper process | Child test binary used to measure process-level resource/lifecycle behavior under controlled environment settings. It must reconcile workers and baselines before exit. |
| local test environment | Repository's configured local Postgres, Redis, vault/config, and loopback services. “Local” describes deployment scope, not a mocked database. |

## `sim-latency` statistical qualifiers

These terms occur in tests under `server/connect/sim-latency`. That simulator
models fleet/provider-selection behavior; it is separate from PERFVAR's
packet-level full-TUN harness.

| Qualifier | Meaning |
|---|---|
| fleet | Generated population of sites, providers, clients, and workloads used by the latency simulator. A fixed configuration/seed must reproduce it. |
| site tree | Generated hierarchy/topology traversed by the simulator. Termination tests guard against recursive/cyclic generation. |
| crawl | Measurement traversal over the simulated fleet. Cancellation must join workers rather than leak them. |
| run artifact | CSV or `run.json` containing one measurement window, configuration identity, rows, and summaries. Round-trip tests require lossless parse/write semantics. |
| sidecar | Metadata JSON paired with a CSV by filename convention, including the exact measurement window and configuration hash. |
| configuration hash | Digest of every setting that affects comparability. Runs with mismatched hashes are not silently compared. |
| replicate | Independent repeat of the same resolved experiment used to estimate run-to-run variability. |
| sample | One observation included in a metric. The test names say when maximum connection counts or other values are sampled rather than continuously tracked. |
| quantile | Value at a distribution fraction. `p05`, `p50`, and `p95` mean the 5th percentile, median, and 95th percentile. |
| TTFB | Time to first response byte. It measures response start latency, not total transfer completion. |
| Student-t survival function (`StudentTSf`) | Upper-tail probability of Student's t distribution for a statistic and degrees of freedom. |
| Student-t critical value (`StudentTCrit`) | Threshold t statistic for a selected tail probability and degrees of freedom. |
| Welch comparison | Unequal-variance two-sample t comparison used when replicate samples, rather than a precomputed environmental baseline, support the verdict. |
| Holm adjustment | Step-down correction of multiple p-values that controls family-wise false positives across the metric family. |
| block bootstrap | Resampling contiguous time blocks rather than independent rows so the estimated standard error retains short-range correlation. A fixed seed makes the estimate deterministic. |
| baseline | Prior estimate of normal environmental mean/standard deviation or noise floor for each metric. It is not the same as a packet-counter measurement boundary. |
| verdict `A better` | A is significantly favorable on the required primary metrics under the configured comparison and multiple-testing guard. |
| indistinguishable | Evidence does not support a significant difference at the configured threshold. It does not prove exact equality. |
| fail-rate guard | Prevents a latency/throughput improvement from winning if it causes an unacceptable request failure rate. |
| block fallback | Comparison uses block-derived uncertainty when available and follows the documented fallback when block evidence is insufficient. It is unrelated to route/encryption fallback. |
| round trip, for config/artifact | Serialize then parse and recover the same semantic value. It is unrelated to network RTT. |

## Common test-name mechanics

| Word | Meaning in names |
|---|---|
| `E2E` | End to end across the production-shaped components named by the test, rather than a single helper. Read the test comment for the exact endpoints; it does not automatically mean a physical multi-host deployment. |
| `Production` | Uses the production implementation path with only documented nil-by-default test seams. It can still run in-process on loopback. |
| `Harness` or `Simulator` | Tests the deterministic test infrastructure itself. It is not a production-path performance result unless a separate integration test drives production components through it. |
| `Regression` | Pins a previously observed failure. The test should fail deterministically on the old behavior. |
| `Focused` | Runs one isolated profile, route, or lifecycle boundary rather than the full matrix. |
| `Default` | Uses the package's resolved default settings/profile, not necessarily zero values. |
| `Nil`, `Zero`, or `Unconfigured` | Exercises the explicitly absent/zero configuration and its production fallback behavior. |
| `Prebound` | Listener or packet connection is created by the caller and injected; ownership on success and failure is part of the test. |
| `Injected` | A dependency is supplied through a test/production seam. Nil must retain ordinary host behavior. |
| `Fallback` | Use the noun immediately before it: encryption fallback, fast-to-legacy fallback, constructor fallback, or route fallback are independent behaviors. |
| `Before` and `After` | Name the exact ordering edge the test asserts. They are not approximate wall-clock relations. |
| `Immediate` | No queued delay or retry precedes the disposition, normally pinned at an exact barrier. |
| `Delayed` or `Pending` | Work is admitted but intentionally cannot finish yet. Teardown must join or dispose it. |
| `Dormant` | Candidate or flow exists but has not produced useful progress; it must not prevent active work from being selected or joined. |
| `Stable` | The same generation/snapshot remains valid across the named operation. |
| `Historical` | Event happened before the measurement boundary and must be baselined rather than attributed to the current interval. |
| `Local` | Belongs to the current process/endpoint. It does not necessarily mean loopback transport. |
| `Remote` | Belongs to the peer/provider side of the logical operation. It does not necessarily mean another physical host. |
