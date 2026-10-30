# CPUOPT-KEEPALIVE: correctness review and fixes (2026-10-30)

Keepalive and timer work can be reduced only while preserving failure detection,
healthy-operation tolerance, and connection ownership. The review found six
issues in the first restoration. This document records their fixes and replaces
the earlier claims that watchdogs preserved native I/O semantics and that TCP
probe arithmetic guaranteed a 15-second exchange failure bound.

Reviewed baselines: server `f57d67e3` and connect `2b697716`.

**Owner requirement:** "We can't sacrifice correctness of performance timings
for the cpu savings. The idea was the keepalive was redundant with heartbeats
and could be optimized out. If that is not true we have to keep the correctness
that keepalive gave us."

## What the timings mean

TCP idle + interval × count describes the nominal idle-probe schedule. It is
not a universal dead-peer bound. TCP does not send keepalive probes while data
is outstanding; retransmission and application deadlines can govern instead.
See [RFC 9293 §3.8.4](https://www.rfc-editor.org/rfc/rfc9293.html#section-3.8.4).
A successful buffered heartbeat write does not prove the peer received it.
QUIC has its own acknowledgement and idle rules and needs separate testing.

Preserve both sides of a timeout contract: a stalled operation must stop on
time, and a healthy operation may use its entire configured budget. A native
deadline applies to physical I/O. A watchdog that sees an uncleared start
timestamp can close a connection after I/O completed but before its goroutine
resumes. Generation validation alone cannot close that scheduling gap.

## Findings and resolutions

| Finding | Root cause | Resolution |
|---|---|---|
| Accepted extender services lost their idle backstop | Listener-wide keepalive removal also covered feeds and HTTP services. Feed records filtered out of a subscriber's partition restarted its heartbeat timer without writing anything. | Restore configurable accepted TCP keepalive (15 s idle, 15 s interval, 9 probes by default). Anchor feed heartbeats to the last successful frame; filtered input cannot postpone them. Check an already-due heartbeat before accepting another record. Nonpositive heartbeat settings disable it without spinning. |
| Exchange deadlines rejected healthy operations early | A cached deadline with two thirds of its budget remaining was reused. A healthy second read arriving in 22.5 s failed at 20 s; a write resuming in 11.25 s failed at 10 s. | Install now + timeout for every read and singleton/batched write. Only an identical absolute deadline may be elided. Keep the full 30 s read and 15 s write budgets. |
| Platform watchdogs could expire a healthy successor | A callback could act on an old operation's timestamp after that operation completed. Even a validated timestamp cannot distinguish completed I/O from a descheduled caller. | Remove the watchdog and restore native H1, H1+ and H3 deadlines. Remove the WebSocket adapter's permanent expired-deadline latch; a successful operation permits a fresh successor deadline. Actual WebSocket I/O errors remain terminal. |
| Relay/splice watchdogs canceled completed I/O | A read or write could finish within budget, then its caller resumed after the watchdog canceled the whole relay. The camouflage hello had the same boundary. | Restore native read/write deadlines, including the initial splice hello. Preserve the existing short-write budget, cancellation and worker joins. Deadline installation failures terminate before unbounded I/O; zero-progress relay writes terminate. |
| One logger could stop the shared pause detector | The detector called client loggers serially while publishing recovery holds. A blocked logger prevented siblings and later passes from receiving protection. | Publish every hold independently of logging. One worker per detector processes immutable scalar records. Each live registration has at most one pending record; repeated pauses coalesce with a count. Unsubscribe and cancellation release pending references. Uplink logs also run outside the hold's state lock. The probe interval is owner-configured with the unchanged one-second default. |
| Exchange keepalive's claimed 15 s guarantee was false | Socket-option readback and idle + interval × count did not exercise outstanding heartbeat data. | Correct the guarantee and retain the independent read deadline. Keep the probe tuning for quiet healthy connections; expose its idle margin, interval and count through ExchangeSettings. Add an opt-in real Linux blackhole regression using the production exchange framer. |

The adjacent connect HTTP listener also served `/status` with a five-minute
HTTP idle timeout and no protocol heartbeat. Its listener-wide keepalive disable
is removed. A composed test drives the real status route through the server
helper and inspects the accepted socket. Heartbeat redundancy must be established
for every route sharing a listener.

The pause logging worker cannot cancel a `Logger` call already entered: that
interface has no cancellation contract. Detector shutdown does not join it.
Pending records and client registrations are released immediately; only the
in-flight logger and scalar record may remain until the sink returns. There is
no goroutine per event or per client on a shared detector, and no logging work
on ordinary no-pause ticks.

## Retained keepalive policy

| Socket/path | Current policy | Meaning |
|---|---|---|
| Exchange TCP, accepted and dialed | Idle = ExchangePingTimeout + configured margin; defaults 11 s / 1 s × 4 | Regular 10 s received pings normally prevent idle probes. Outstanding data can suppress probing, so the independent 30 s read timeout remains necessary. |
| Connect shared HTTP/platform listener | Go default TCP keepalive | Retains the idle backstop for ordinary HTTP as well as upgraded transports. |
| Client platform dials, including extender/VLESS | Restored ConnectSettings cadence: 5 s / 5 s × 1 | Retains idle kernel detection; not an unconditional 10 s active-path guarantee. |
| Provider upstream TCP | Nil ProviderKeepAliveConfig retains the dial's 5 s / 5 s × 1 | Arbitrary upstream flows have no application heartbeat of ours. |
| Extender accepted TCP services/relays | Configurable, defaults 15 s / 15 s × 9 | Retains service liveness independently of buffered writes. Injected listeners own their socket policy. |
| Extender feed dials | Restored 5 s / 5 s × 1 | The feed's 30 s heartbeat and 90 s client idle cutoff do not replace all idle kernel behavior. |
| Extender QUIC carriers | KeepAlivePeriod = PingTimeout on every carrier | Prior real quic-go/simnet loss tests showed 29.8 s detection with removal versus 24.9 s before. |
| Platform H3 | KeepAlivePeriod = PingTimeout | Prior scaled loss test showed 2.97 s after doubling versus 2.50 s before, at P = 0.5 s. |
| P2P ICE | Restored 2 s cadence | Raising it to 5 s delayed checks of the existing failure windows. |

Pooled API connections retain keepalive. Provider probe-count tuning and
host-specific mobile policy are separate behavior changes and are unchanged.
Legacy SCTP deadline retry, carrier-change notifications, prober wakeups and
client idle-loop parking remain as previously restored.

## CPU implications

There is real redundant work to remove. The measurements in [CPUOPT2.md](CPUOPT2.md)
recorded about 170k outgoing exchange probes and 166k incoming ACKs per second
on one edge. Moving idle probing beyond normal ping traffic avoids most of that
traffic on healthy connections. The earlier 2–5% busy-CPU estimate was an estimate,
not an isolated before/after measurement of this change.

Retained savings include exchange pings at 10 s, pacing from completed writes,
timer reuse, ready batching, unchanged-deadline elision, and one pause timer per
shared detector. The relay/platform watchdog savings and exchange deadline
throttling are deliberately reverted. They were not compatible with the original
I/O contract. Historical operation-count results for those mechanisms must not
be presented as savings of this fixed version. An isolated production CPU and
throughput comparison is still needed to quantify the combined result.

A real TCP loopback smoke benchmark after the fixes retained the ready-batch
benefit: 38.95 µs per burst versus 384.35 µs for singleton writes (200 bursts,
Apple M1 Max, Go 1.27.3). This compares framing modes on the fixed version;
it does not measure production CPU savings or isolate the cost of restoring
native deadlines.

## Regression evidence

Final focused race suites passed for connect (12.311 s), extender (1.735 s),
gossip (3.205 s), server (3.744 s), and server/connect (3.681 s). These cover the
changed owners and their timeout, cancellation, batching and ping controls;
they are not a claim that every repository integration test or a production
throughput soak ran. Formatting and diff checks also passed.

Deterministic virtual-clock tests force the relevant ordering rather than relying
on scheduler luck. New composed tests fail against the reviewed production code
and pass after the fixes:

- `resident_deadline_tolerance_test.go`: real pipes accept the delayed healthy
  read, singleton write and batch write. Exact stalled-write and setter-rejection
  tests retain the other side of the budget.
- `transport_io_deadline_test.go`: successful physical H1/H1+ I/O is held behind
  an explicit continuation barrier, then a successor uses the same carrier.
  H3 uses real quic-go over simnet. Native expired-deadline replacement, genuine
  hung I/O, backpressure, cancellation, ping cadence and batching are controls.
- `extender_relay_completed_io_test.go`: completed reads/writes in both relay
  and splice directions and the initial hello survive their scheduling gap.
  Listener tests inspect real accepted socket options, including custom and
  disabled settings. Splice cancellation and blocked-I/O controls remain.
- `gossip/feed_server_keepalive_test.go`: valid signed records outside the
  subscriber's partition cannot suppress its heartbeats; mixed traffic and
  disabled settings are covered.
- `ip_remote_multi_client_pause_log_test.go`: a held logger cannot delay sibling
  holds or a second detection pass. Coalescing, cancellation, reference release
  and independent client tolerance/hold settings are exercised.

The initial isolated Linux 7.0.14 probe measured approximately 15.10 s with the
retuned keepalive and no heartbeat, 30.00 s with the retuned keepalive and 10 s
heartbeats, and 30.01 s with the former 1 s / 1 s × 15 keepalive and heartbeats.
Both active cases ended at the application read deadline. This disproves the
unconditional 15 s claim; it does not demonstrate a new active-path slowdown
relative to the former configuration.

The production-framer Linux regression also passed: 15.585 s for idle probing,
30.004 s with the retuned configuration and heartbeats, and 30.001 s with the
former configuration and heartbeats. The two active cases each buffered three
successful ping writes before the independent read timeout ended the read.

The Linux integration test uses complete exchange frames and installs loss only
after the per-connection firewall rule is acknowledged. Run it only in a
disposable network namespace. On an arm64 host with Docker:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -tags=keepaliveintegration \
  -o /tmp/exchange-blackhole.test ./connect
docker run --rm --network none --cap-add=NET_ADMIN \
  -e EXCHANGE_TEST_ISOLATED_NETWORK=1 \
  -e WARP_ENV=local -e WARP_SERVICE=test -e WARP_DOMAIN=example.test \
  -e WARP_BLOCK=test -e WARP_VERSION=0.0.0 \
  --mount type=bind,src=/tmp/exchange-blackhole.test,dst=/review.test,readonly \
  --entrypoint /review.test docker:29.8.2-dind \
  -test.run '^TestExchangeTcpBlackhole' -test.v -test.timeout=90s
```

Use GOARCH=amd64 on an amd64 runtime. The tests are excluded from ordinary builds
and also require the explicit environment opt-in. They inspect real kernel
behavior; option arithmetic alone is not a detection test.

## Source and deployment status

These review fixes are developed on `fix/keepalive-correctness` in connect and
server. No deployment is performed by this task. Earlier deployment timestamps
in CPUOPT2 are historical observations, not evidence that a running release
contains these fixes. Consumers must build with the corrected connect library
as well as the server changes.
