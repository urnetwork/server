# Design Notes

Durable design notes for the urnetwork `server`, focused on the connect/exchange
relay path and the invariants that keep it both fast in production and
deterministic under test. These were distilled while debugging why the
"experimental: performance optimizations" branch hung the `connect/connect_test.go`
suite. Read this before changing the resident forward path, the exchange buffer
sizing, ack timing, or the connect test harness.

---

## 1. The connect relay architecture

Two clients (A and B) never talk directly. Each client is represented on a
connect host by a **`Resident`**. Client↔client traffic is relayed resident↔resident.

### Components (`connect/resident.go`)

- **`Exchange`** — per-host process owning residents and the internal
  resident↔resident transport (TCP on internal ports). Holds `ExchangeSettings`.
- **`Resident`** — represents one client on this host. Owns:
  - `transports` — live client connections (websocket/h3) via `ResidentTransport`.
  - `forwards` — `map[destinationId]*ResidentForward`, one outbound relay per peer destination.
  - an embedded `connect.Client` (`self.client`) used to route frames in/out of this client.
- **`ResidentTransport`** — bridges a client connection to the nominated resident
  (which may live on another host); nominates/keeps the resident record in the DB/redis.
- **`ResidentForward`** — an outbound relay from this resident to a *peer resident*
  (the resident of the destination client). Has a `send` channel and an
  `ExchangeConnection`.
- **`ExchangeConnection`** — a framed TCP connection between two residents.
  `Op` is `ExchangeOpTransport` or `ExchangeOpForward`.
- **`ExchangeBuffer`** — framing + buffered IO on an exchange connection.

### Message flow A → B (and the ack B → A)

```
A.client → A.transport(ws/h3) → A.resident
         handleClientForward (dest=B ≠ A ⇒ forward)
              → ResidentForward(A→B).send → ExchangeConnection(Op=Forward)
                   → B.resident  runForward (reads conn) → `forward` chan
                        → AddForward goroutine → B.client.ForwardWithTimeout(...)
                             → B.transport → B
```

The ack travels the **mirror path** B → A. This symmetry matters: a one-directional
data stream is actually bidirectional on the wire (data one way, acks the other),
so anything that stalls one direction stalls the conversation.

### glog tags (grep these when debugging)
`[rf]` resident forward, `[rt]` resident transport, `[ecrs]`/`[ecrr]`/`[ecrf]`
exchange-connection send/receive/forward (resident side), `[ecs]`/`[ecr]`
exchange-connection send/receive (peer side), `[ts]`/`[rtr]` transport send/recv,
`[s]`/`[r]` send/receive sequence (in the connect lib `transfer.go`).

---

## 2. The central principle: the receive loop must never block (in production)

A resident's client-receive/forward loop dispatches every inbound frame. **It must
never block on a single slow or stuck route**, because that one route would stall
*all* forwarding for the client — including acks — and under bidirectional load it
forms a backpressure cycle between two residents that gridlocks both directions
until a write deadline breaks it.

Therefore, on the hot path "read a frame off the client receive loop and forward it
to a peer resident" (`handleClientForward`'s dispatch into `ResidentForward.send`),
production **drops** rather than blocks: enqueue non-blocking, and if the queue is
full, increment a counter and drop. The sender's reliability layer will resend.

> Corollary: abuse/limit checks on the receive path also must not sleep. The old
> `abuseLimiter.delay()` (a 5s sleep per offending frame on the single forward
> dispatch goroutine) was a gridlock source; abuse rejections now just
> `abuseDroppedCounter.Inc()` and drop. Never reintroduce a sleep on a receive loop.

### But: drop-only loses messages when buffers are zero

A non-blocking send to a **zero-capacity** channel succeeds only if a receiver is
parked at that exact instant — otherwise it always drops. Production hides this with
deep buffers; the test harness deliberately uses zero-size buffers (§5) and so would
drop nearly every relayed frame. With reliable resends that *usually* still
converges, but combined with ack coalescing (§4) it stalls indefinitely.

### Resolution: `ForwardTimeout` (configurable, not hardcoded)

`ExchangeSettings.ForwardTimeout` governs the receive-loop→peer forward enqueue:

- **Production: `ForwardTimeout = 0`** → non-blocking; drop on full. The receive
  loop is never stalled by one route.
- **Tests (0-size buffers): `ForwardTimeout = WriteTimeout`** → block up to the
  timeout for deterministic, reliable delivery.

Implementation detail (important): you cannot express "non-blocking" as
`select { case ch<-x: ; case <-time.After(ForwardTimeout): }` with `ForwardTimeout=0`,
because `time.After(0)` fires immediately and *races* the send — it may drop even
when the send could proceed. The correct shape is a non-blocking fast path, then a
blocking wait **only when the timeout is positive**:

```go
select {
case <-forward.Done(): return false
case forward.send <- frame: return true
default:
}
if 0 < ForwardTimeout {
    select {
    case <-forward.Done(): return false
    case forward.send <- frame: return true
    case <-time.After(ForwardTimeout):
    }
}
// drop
```

### `AddForward` is different — it uses the normal `WriteTimeout`

`AddForward` delivers an *inbound* relayed frame to the local client. It is used by a
forward transport (route into the client), **not** the hot client-receive-forward
loop, so it blocks on the normal `WriteTimeout` rather than `ForwardTimeout`. Keep
this distinction: `ForwardTimeout` is only for "reading off the receive loop and
forwarding to a peer."

---

## 3. `WriteTimeout` is the single back-pressure deadline

"Message writes on all layers have a single `WriteTimeout`" — every channel handoff
in the resident uses `time.After(WriteTimeout)` as its drop deadline, because all
layers share the same back pressure. (Read timeouts may differ per layer due to
keep-alive/ping cadence.) When a handoff needs a finite blocking deadline, it should
be `WriteTimeout`, consistent with its siblings. The perf branch's bug was singling
out two forward handoffs to be non-blocking while every sibling stayed at
`WriteTimeout`; the fix re-aligns them (directly, or via `ForwardTimeout` whose test
value *is* `WriteTimeout`).

---

## 4. Ack coalescing vs. the resend floor (`AckCompressTimeout`)

`ReceiveBufferSettings.AckCompressTimeout` (connect lib `transfer.go`) coalesces acks:
after the first pending ack, wait this window and emit a single cumulative *head* ack
plus any selective acks. Without it, every received message emits its own ack frame,
which **doubles** per-pair message volume on the relay for one-way streams and feeds
resend storms under load. Production default: **10ms**.

**Invariant:** the coalesce window must be *far below* the sender's resend floor
(`SendBufferSettings.MinResendInterval`, production 2s). If the two are comparable,
ack timing becomes nondeterministic relative to resends.

This invariant is **violated in the test harness**, which sets `MinResendInterval=10ms`
to stress resend logic. A 10ms ack window then lines up with the 10ms resend interval;
fewer, coalesced acks plus any lossy relay drop (§2) leave the sender waiting on an
ack that never comes. **The test harness must set `AckCompressTimeout=0`** (per-message
acks) — frequent acks recover quickly from any single drop, keeping the test
deterministic. Production keeps 10ms.

---

## 5. The test harness philosophy: zero-size buffers expose deadlocks

`connect/connect_test.go`'s `testConnect` deliberately runs the whole stack with
**zero/aggressive settings** so that any head-of-line blocking or ordering bug
surfaces as a hang instead of being absorbed by a buffer:

- `ExchangeBufferSize = 0`, `ForwardBufferSize = 0`
- `SendBufferSettings.SequenceBufferSize = 0`, `AckBufferSize = 0`
- `ReceiveBufferSettings.SequenceBufferSize = 0`
- `ForwardBufferSettings.SequenceBufferSize = 0`
- `MinResendInterval = 10ms` (stress resends)
- encryption session `IdleTimeout = 0` (reap at refs==0 ⇒ fresh handshake per burst,
  exercising the restart→plaintext→upgrade path)

Because production tuning (non-blocking forwards, deep queues, coalesced acks) is
*wrong* for a zero-buffer deadlock test, **the harness must override the production
forward/ack knobs**:

```go
settings.ForwardBufferSize = 0
settings.ForwardTimeout     = settings.WriteTimeout   // reliable, blocking forwards
clientSettings{A,B}.ReceiveBufferSettings.AckCompressTimeout = 0   // per-message acks
```

General rule: any production optimization that *drops* or *delays* to protect
throughput needs a setting that the deadlock tests can flip to the
reliable/deterministic behavior. Hardcoding the optimization breaks the tests.

---

## 6. Buffer sizing & congestion control (`ForwardBufferSize`)

`ResidentForward.send` is the outbound relay queue to a peer resident. Its depth must
stay large in production (≥1024) so application-level congestion control has room to
work across the hop. This was conflated with `ExchangeBufferSize`; it is now a
separate `ExchangeSettings.ForwardBufferSize` (default `defaultForwardBufferSize=1024`)
so the deadlock tests can set it to 0 without shrinking production's queue.

Note on `defaultExchangeBufferSize` (1024, was 4096): a larger exchange buffer
mitigates head-of-line blocking when forward/client rates change, **but** it also
bounds hidden queueing per hop. The end-to-end queue across all hops must drain well
inside the client resend floor (`MinResendInterval`), or delayed acks trigger mass
spurious retransmission under load (bufferbloat → resend storm). Bigger is not
strictly better.

---

## 7. Other performance optimizations on this path (context)

These shipped on the same branch; understand them before touching the IO path:

- **writev batching** (`ExchangeBuffer.WriteMessages`): coalesce pending messages into
  one `net.Buffers` writev (a length-header iovec + body iovec per message) up to
  `ExchangeWriteBatchCount`/`ExchangeWriteBatchByteCount`, amortizing per-message
  syscall cost. The whole batch shares one `WriteTimeout`.
- **Buffered reads** (`ExchangeBuffer` `bufio.Reader`): amortize the framer's
  header+body reads. **Hazard:** a buffered reader may read past the header into
  message bytes, so a single connection must use *one* reader for *all* reads (header
  and messages). `NewExchangeConnection` therefore reads the echoed header with the
  `receiveBuffer` (which owns all reads), not the `sendBuffer`.
- **Fast-path selects**: a non-blocking `select { case ch<-m: ; default: }` before the
  full blocking select with the ping timer, so the hot path doesn't arm a timer every
  message. These do **not** drop (they fall through to the blocking select) — distinct
  from the deliberate forward drops in §2.
- **Immediate cold nomination** (`ResidentTransport.Run`, `skippedReconnectWait`): when
  there is no resident yet, nominate immediately instead of waiting
  `ExchangeReconnectAfterErrorTimeout`. Bounded to every other iteration so a
  repeatedly failing nomination still backs off.
- **Abuse drops are counters, not delays** (§2 corollary).
- **Passive speed measurement** (`transport_announce.go`): measure real throughput in
  5s windows (`max(send,receive)` bytes, ≥`PassiveSpeedMinByteCount` to sample) and
  update the speed result on a new max. A **synthetic** speed test runs only if the
  connection hasn't passively shown `SyntheticSpeedBytesPerSecond` (4 MiB/s, aligned
  with provider-ranking cutoff) after `SyntheticSpeedTimeout` (60s). Rationale: during
  a synthetic test the client transport *echoes all inbound messages upstream instead
  of delivering them*, so testing a busy connection blackholes live traffic and
  triggers resend storms. Busy connections prove speed passively and are never
  disrupted; idle ones still get tested.

### Observability — Prometheus drop counters
`urnetwork_connect_forward_dropped_messages` (peer-forward queue full),
`urnetwork_connect_forward_receive_dropped_messages` (local client forward full),
`urnetwork_connect_abuse_dropped_messages` (bad source / forward limit / no contract).
Nonzero forward-drop counters under normal load indicate buffer/timeout misconfig.

---

## 8. Debugging methodology (how this class of bug was found)

- **Timing-dependent hang ⇒ race/livelock, not a hard deadlock.** With glog `-v=1`
  the suite nearly passed (reached `burstSize=6`); with no logging it hung at
  `burstSize=1`. The added log latency let an unbuffered rendezvous succeed. If logging
  changes whether it hangs, suspect a silent drop / lost wakeup, not a mutex deadlock.
- **A quiescent goroutine dump means a message was silently dropped**, not a lock
  cycle. Nothing was blocked on a mutex; everything was idle-reading. The "missing"
  message simply never arrived.
- **Read the *main test* goroutine's blocked line to localize the failed direction.**
  Parked on the ack-collection loop ⇒ the data arrived at the peer but the ack never
  returned (ack path broken). Parked on the receive loop ⇒ the data itself never
  arrived (data path broken).
- **Isolate one variable at a time.** Setting `AckCompressTimeout=0` alone did *not*
  fix the hang; making the forward blocking *did* (even with the 10ms ack default).
  That proved the forward drop was the root cause and ack coalescing was only an
  amplifier — preventing a wrong fix.
- **Run the no-logging configuration to reproduce**, since `-v=1` masks the bug. Use a
  finite `go test -timeout` (not the harness's `-timeout 0`) so a hang dumps goroutines
  instead of running forever.

---

## 9. Running the tests

See the project memory `server-test-env-setup`. The canonical runner is `./test.sh`
(per-dir `go test -timeout 0 -v -race -cpuprofile profile/cpu -memprofile profile/memory`)
with `WARP_ENV=local` and the `BRINGYOUR_*_HOSTNAME` env vars exported. `test.sh` uses
`-timeout 0` (a hang runs forever) and exits on the first failing package; for
unattended full-suite debugging, prefer a finite per-package `-timeout` and
continue-past-failure so every package's result is captured. Each test dir needs a
`profile/` subdir to exist.

## 10. Wallet authentication: no backward compatibility for the legacy sign-in message

Wallet authentication now uses a server-issued challenge: `CreateWalletAuthChallenge`
generates a unique message and the client must sign that exact message in
`UseWalletAuthChallenge`. The old flow where the client supplied its own static
`"Welcome to URnetwork"` message is intentionally not supported. The server rejects
any `WalletAuthArgs.Message` that does not match a stored challenge row and binds the
signed timestamp to the stored `create_time`. Backward compatibility is not supported
for security reasons; callers must upgrade to the challenge flow.
