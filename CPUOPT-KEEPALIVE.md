# CPUOPT-KEEPALIVE: TCP and transport keepalive changes, for peer review (2026-10-30)

The CPU work in CPUOPT2.md turned keepalive off or down in several places,
on the reasoning that our own heartbeats already detect a dead peer. That
holds only where the heartbeat detects it at least as fast as keepalive did.
This document goes through every keepalive change made on 10-29 and 10-30,
checks it against that test, and records what was kept, restored or retuned.

**Rule (owner, 2026-10-30):** "We can't sacrifice correctness of performance
timings for the cpu savings. The idea was the keepalive was redundant with
heartbeats and could be optimized out. If that is not true we have to keep the
correctness that keepalive gave us."

**How to read the timings:**
- **Keepalive** notices a dead peer Idle + Interval × Count after the last
  packet it received.
- **A heartbeat** notices it only when the read deadline cuts the connection.

## Summary

| # | Socket | Keepalive before | Detection before | Heartbeat-only | Verdict | Now |
|---|---|---|---|---|---|---|
| 1 | Server exchange, dialed side | 1 s idle, 1 s × 15 | 16 s | 20–30 s (read-deadline cut) | not redundant | **Restored, retuned:** idle = ExchangePingTimeout + 1 s, 1 s × 4, so 15 s |
| 2 | Server exchange, accepted side | Go default, 15 s / 15 s × 9 | ~150 s | 20–30 s | redundant | Gets the same 15 s keepalive as #1 (an improvement) |
| 3 | Connect service's platform-transport listener (server side of client transports) | Go default | ~150 s | 30 s read deadline | redundant | **Off** (unchanged) |
| 4 | Client platform-transport dials (apps, providers, proxy hosted devices; extender and VLESS carriers) | 5 s idle, 5 s × 1 | ~10 s | 30 s read deadline | not redundant | **Restored:** 5 s / 5 s × 1 |
| 5 | Provider upstream TCP (relayed flows) | 5 s / 5 s × 1, via the dial | ~10 s | none: 300 s flow timeouts | not redundant | **Restored:** 5 s / 5 s × 1 |
| 6 | Extender server, accepted relay connections | Go default | ~150 s | 30 s relay read and write bounds | redundant | **Off** (unchanged) |
| 7 | Extender feed dial (subscription and sample) | 5 s / 5 s × 1 | ~10 s | 30 s server keepalive frame, 90 s client drop | not redundant | **Restored:** 5 s / 5 s × 1 |
| 8 | Extender QUIC carrier owned by a platform transport | QUIC keepalive 5 s | 24.9 s (test) | inner app ping | slower (29.8 s) | **Restored:** 5 s on every extender carrier |
| 9 | Platform-transport H3 (QUIC keepalive) | PingTimeout (5 s) | P + 4P | inner app ping | slower | **Restored:** PingTimeout |
| 10 | p2p ICE keepalive | 2 s | 2 s check cadence | — | slower: 5 s checks noticed a lost path up to 3 s later | **Restored:** 2 s |

Unchanged throughout: pooled API connections keep keepalive. They have no
heartbeat.

## Details

### 1–2. Server exchange sockets

- **Original change:** server 282565213 (10-29), deployed in connect
  `2026.10.29-outerwerld-1085173010` (10-30 02:26–03:05Z). It turned keepalive
  off on dialed and accepted exchange sockets. The owner had asked for
  keepalive at half the read timeout. It was turned off instead, on the
  heartbeat reasoning below.
- **Why it seemed safe:** each end pings every ExchangePingTimeout
  (ReadTimeout / 3 = 10 s) against an ExchangeReadTimeout (30 s) read deadline.
- **What it cost to keep:** at the dialer's 1 s idle, probes and their ACKs
  were about a fifth of all TCP segments on a services edge (edge-3: 170k
  probes/s out, 166k ACKs/s in).
- **Why it wasn't safe:** the read deadline cuts a dead connection 20–30 s
  after its last frame. The dialed side's keepalive cut it at 16 s.
- **Restore, without the probe cost:** server 24b202b09 (pushed, **not
  deployed**). Both sides carry `exchangeKeepAliveConfig(ExchangePingTimeout)`:
  idle = ping + 1 s, then 4 probes 1 s apart.
  - **Quiet while live:** a live connection receives a ping or data at least
    every ping interval, so it is never quiet for idle and sends no probe.
  - **Bounded when dead:** a dead peer is cut at most 15 s after its last
    packet at the default 10 s ping, within the former 16 s. That is half the
    30 s ExchangeReadTimeout, the bound the owner first asked for.
- **Tests:** server/connect/exchange_tcp_keepalive_test.go. They read
  SO_KEEPALIVE, TCP_KEEPINTVL and TCP_KEEPCNT back from real dialed and
  accepted sockets, and pin idle > ping, the 16 s bound and detection before
  the read deadline. Both socket tests failed first on main.
- **Review asks:**
  1. Is ExchangePingTimeout + 1 s enough margin for a late ping under load?
     A late ping only causes a probe, which a live kernel answers. It can't
     cause a false cut.
  2. Should the bound be tightened below 15 s?

### 3. Connect service's platform-transport listener

- **Change:** server 282565213 set `DisableTcpKeepAlive` on connect's listener.
- **Before:** Go's default listener keepalive noticed a dead client after about
  150 s.
- **Now:** the server's 30 s transport read deadline (ConnectHandlerSettings),
  which is faster. **Kept off.**

### 4. Client platform-transport dials

- **Original change:** connect b096a4a1 (merged 01030ccd, 10-29). It turned
  keepalive off on every platform-transport dial, including extender and VLESS
  carriers. That shipped in the proxy release's connect cherry-pick and is in
  connect main.
- **Why it wasn't safe:** the client's own heartbeat (a ping about every 10 s,
  a 30 s read deadline) notices a dead connect server 30 s after the last
  frame, against about 10 s with keepalive.
- **Restore:** connect 239c8eef, plus 005fd3ca, which removes the now-unused
  dial mark. Every platform dial keeps the ConnectSettings cadence again.
- **Test:** `TestPlatformTransportDialKeepsTcpKeepAlive` reads SO_KEEPALIVE on
  the transport's real dial context. It failed first.

### 5. Provider upstream TCP

- **Original change:** connect 6f39aa0f (CPUOPT2 §9, round 1; merged d16f4a40,
  not deployed). It turned upstream keepalive off, relying on the flow's 300 s
  ReadTimeout and IdleTimeout.
- **Why it wasn't safe:** a relayed flow carries arbitrary TCP with no heartbeat
  of ours. An upstream that vanishes while the flow is idle is noticed only by
  keepalive.
- **Restore:** connect c6e21898. `ProviderKeepAliveConfig` defaults to nil
  again, which keeps the dial's 5 s / 5 s × 1.
- **Test:** `TestProviderEgressKeepAliveKeepsTheDialCadence` reads the real
  upstream socket under each default profile. It failed first with keepalive
  off. The 300 s reaping tests stay as the backstop.
- **Review asks:**
  1. Count 1 resets a healthy idle flow after a single lost probe, for example
     on a dormant phone radio. Should providers move to a count of 2–3? That
     would be a behavior change, so it was not made here.
  2. The per-flow probes keep phone radios awake. Should mobile providers get
     a host-specific config?

### 6. Extender server, accepted relay connections

- **Change:** connect 9db6e4cd set `ListenConfig{KeepAlive: -1}`.
- **Before:** Go's default noticed a dead peer after about 150 s.
- **Now:** the relay's own read and write bounds (30 s), made exact again by
  cd373778, notice it sooner. **Kept off.**

### 7. Extender feed dial

- **Original change:** connect b5a10fea marked feed dials so they skipped
  keepalive.
- **Why it wasn't safe:** the feed's own liveness is a server keepalive frame
  every 30 s and a client drop after 90 s idle, against about 10 s with kernel
  keepalive.
- **Restore:** connect 7f01141f. Feed dials are unmarked, and
  net_extender_network.go is identical to caad1de6.
- **Test:** `TestExtenderFeedDialKeepsTcpKeepAlive`.

### 8–9. QUIC keepalive (extender carriers, platform H3)

- **Original changes:**
  - connect 03c6af3c set `KeepAlivePeriod` 0 on extender carriers a platform
    transport owns.
  - connect 08ecfbfb doubled the platform H3 keepalive to 2 × PingTimeout.
- **Why it wasn't safe:** a QUIC connection idles out MaxIdleTimeout (4 ×
  PingTimeout) after its last received packet. The app ping is due up to two
  ping timeouts after the last write, so the QUIC keepalive does bound
  detection:
  - a dead extender was noticed at 29.8 s instead of 24.9 s (real quic-go on
    simnet under the synctest clock);
  - a dead H3 peer at 2.97 s instead of 2.50 s, at test scale P = 0.5 s.
- **Restore:** connect 98825c78 (every extender carrier keeps PingTimeout) and
  6e735a44 (platform H3 back to PingTimeout).

### 10. p2p ICE keepalive

- **Original change:** connect 5d01d497 raised it from 2 s to 5 s.
- **Why it wasn't safe:** pion checks its 30 s disconnect and failure windows
  only when it wakes for a keepalive, so a lost path was noticed up to 3 s
  later.
- **Restore:** connect 835fb249.

## Related: deadline and parking changes that also delayed detection

These weren't keepalive, but they moved detection the same way. All are
restored to the exact pre-change bounds of connect caad1de6, without
per-frame timer work:

| Area | Round-1 change | Delay | Restore |
|---|---|---|---|
| Platform transport H1, H1+ and H3 reads and writes | lazy deadlines with slack | read up to 4T/3, write up to 1.5T | per-connection I/O watchdog (one `time.AfterFunc`; interrupts only a read or write that has really run T): f0a1fa84 |
| Extender relay and camouflage splice | deadline slack | 1.5T (45 s for 30 s) | per-direction watchdog that cancels the relay: cd373778 |
| Legacy p2p SCTP write | deadline slack | 22.5 s for 15 s | kept deadline never later than the write's own, retried once if it fires early: 1cda306b |
| Idle channel, lost carrier | parked health loop polled every 10 s | retirement at 70 s for 60 s | route manager `ActiveTransportNotify` wakes it on the carrier change: 75db68d3 |
| Prober, a loaded exit with a stale proof | sleep until due (60 s cap) | re-probe 60 s for 5 s | 5 s recheck while such an exit is held, and a wake on record eviction: 24e9b649, 697135b5 |

Each restore has a test that passes on caad1de6, fails on d16f4a40 and passes
on the fix. The round-1 operation-count tests still pass, so the CPU savings
on the hot paths stay.

## Status

- **server:** 24b202b09 pushed to main. It takes effect with the next connect
  service release. Until then, the running connect (1085173010) has keepalive
  off on exchange sockets.
- **connect:** the restores above are in connect main at 005fd3ca (pushed
  10-30). They ship with each service's next build; the proxy and apps carry
  #4.
- **Tests on 005fd3ca:** the full connect suite passes on Linux (go 1.27.3:
  vet clean, 506 pass, 4 skip) and on macOS. On macOS one load-sensitive
  throughput floor, `TestWindowPathSdkBidirectionalMedium`, missed by 0.3%
  once under machine load and passed 3 of 3 when rerun alone.

## For reviewers

1. **#1:** is ExchangePingTimeout + 1 s the right idle, and is the 15 s bound
   right?
2. **#5:** should provider keepalive keep count 1 (fast, with false resets), or
   move to 2–3 (slower, fewer false resets)?
3. **#3 and #6:** are there clients whose platform-transport or relay
   connections rely on server-side keepalive for anything other than dead-peer
   detection, such as NAT mapping refresh?
