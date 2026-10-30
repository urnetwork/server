# WGONDEMAND — WireGuard peers on demand (2026-10-30)

The proxy no longer starts a WireGuard peer for every configured client.
userwireguard asks the proxy for a peer only when an initiation arrives from a
key it doesn't hold. The proxy answers from its in-memory client index, and
peers that go idle are evicted. Device peers now follow use, not signups.

The owner's design: "add a 'miss' callback to userwireguard to allow
registration of a new wireguard client on demand". The owner's bar: "any
wireguard miss callback change needs to be thoroughly tested". The background
is in CPUOPT2.md §8.

| Repo | Branch | Commit |
|---|---|---|
| userwireguard | `perf/peer-miss-callback` | 4524829 "Create peers on demand from a miss handler" |
| proxy | `perf/peer-on-demand` | 05b4ade "Create WireGuard peers on demand" |
| server | `perf/proxy-wg-peer-on-demand` | d97ceb783 "Register WireGuard clients without device peers" |

## Why

On 2026-10-29 both proxy hosts sat at 99.9% CPU for hours. Each proxy
process's heap plus stacks (≈6.99 GB) exceeded its GOMEMLIMIT (6.82 GB), so the
GC ran continuously at its CPU limiter, about 57% of proxy CPU.

The largest memory owner was WireGuard peers:
- **Registration:** the proxy registered every WG-configured client of its
  block, about 41.3k per process, 824k across 20 processes.
- **Start:** userwireguard starts each peer when it's registered, so every
  peer held 2 goroutines, 1024/1024/128-slot queues with finalizers, two
  128-slot batch slices and 5 timers.
- **Cost per process:** about 82.6k goroutines, 1.1 GB of heap and 0.45 GB of
  stacks, whether or not the client ever connects.

## How it works

### userwireguard: the miss hook

1. **Admission checks first:** the handshake worker checks MAC1 as before, and
   under load the cookie (MAC2) and the per-source rate limiter. Then it
   decrypts the initiator's static key.
2. **The miss:** if no peer exists for that key, `consumeMessageInitiation`
   reports a miss instead of dropping the packet. Only a client holding the
   server's public key can get this far. Random packets and floods without a
   cookie never reach the handler.
3. **`resolvePeerMiss`** then applies, in order:
   1. a refusal cache: 10 s TTL, bounded at 65,536 keys;
   2. one in-flight resolution per key, so concurrent initiations coalesce;
   3. a token bucket: 1,024/s, burst 2,048 (`PeerMissSettings`);
   4. the handler, called on its own goroutine.
4. **The worker wait:** a worker waits at most `HandlerTimeout`; the proxy
   sets 250 ms.
   - On timeout the initiation is dropped. The late answer still creates the
     peer, so the client's retry about 5 s later succeeds.
   - A panicking handler counts as a failure and is not cached.
5. **Creating the peer:** an accepted miss runs `AddPeerOnDemand`.
   - It holds `ipcMutex.RLock`, so it can't overlap IpcSet, Up, Down or Close.
   - It re-checks and inserts the peer under the peer-map lock, sets the
     allowed IPs and starts it.
   - The worker then consumes the same initiation again, so the first
     handshake completes without a retry.
6. **Smaller lock windows:** `NewPeer` now computes the static-static key and
   allocates the queues before taking the peer-map lock.

**New API:**
- `PeerMissHandler`, `PeerMissConfig`, `PeerMissSettings`
  (`RatePerSecond`, `Burst`, `HandlerTimeout`, refusal TTL);
- `SetPeerMissSettings`, `PeerMissStats`, `PeerSnapshots`;
- idle-peer removal.

**Compatibility:** with no handler set, the device behaves as before (unknown
keys are dropped).

### proxy (`WgProxy`, `wg.go`)

- **The setting:** `WgProxySettings.PeersOnDemand`, off by default in the
  library. Other knobs: `PeerIdleTimeout` (10 min), `PeerEvictInterval`
  (1 min), `PeerMissNegativeTtl`, `PeerMissBurst`, `PeerMissHandlerTimeout`
  (250 ms).
- **Indexing:** `AddClients` only indexes clients, mapping address and public
  key to address and preshared key. The miss handler reads the index under an
  RLock: no Redis or PG on the receive path.
- **Client changes:** removing a client, changing an address's key, or moving a
  key to another address removes the live peer and detaches its tun.
- **Eviction:** every minute, peers whose newest handshake (or creation, if
  they never completed one) is over 10 minutes old are removed. Any traffic
  needs a handshake at least every 180 s, so an active session is never
  evicted. An evicted client's tun is detached, not cancelled, and its next
  packet re-attaches it.
- **Server-first traffic:** the deploy handoff is the only path where the
  proxy sends first. A client added with an `Endpoint`, a `SeedEndpoints`
  call, and an `InitiateHandshake` call each create the peer.
- **Status:** `PeerStatuses` reads `Device.PeerSnapshots`, so its cost follows
  live peers, not configured clients. New `PeerStats()` reports peers, miss
  outcomes and evictions.

### server (`proxy/server.go`)

- **Default:** on-demand peers are on.
- **Kill switch:** `wg_peers_on_demand: false` in proxy.yml restores eager
  registration at the block's next start. A non-boolean value keeps the
  default.
- **Unchanged:** the per-id cooldown in `wgTunFactory`. The 15 s session
  sampler is unchanged too, but it now reads only live peers instead of an
  `IpcGet` of every configured peer.
- **Metrics:**
  - `wg_peers` still counts configured clients.
  - `wg_device_peers` counts the device's actual peers.
  - `wg_peer_misses{outcome}` and `wg_peer_evictions` are new.

## Behavior changes and risks

- **Peer creation without key proof.** Anyone who knows the server's public
  key can trigger peer creation for a *configured* client's key without
  holding that client's private key.
  - It is bounded by the cookie check, the per-source limiter, the 1,024/s
    budget and eviction.
  - In the worst case the memory equals what eager registration costs today.
- **Orphan peer.** If a client is removed while its handshake is in flight, its
  peer can survive up to about 11 minutes. It carries no traffic, because the
  client is no longer in the index.
- **Cold connects on a saturated host.** If the handler wait exceeds 250 ms,
  the initiation is dropped and the connect takes about 5 s longer.
- **Not tested:** an old eager-mode instance handing off to a new on-demand
  instance. Both export through the same `PeerStatuses` semantics.
- **Rekey and expiry test:** it triggers the 120 s and 180 s thresholds by
  message count, not age. Both conditions sit in the same branches of
  `keepKeyFreshSending` and `SendStagedPackets`.

## Tests

Every new test failed first against stubs with the new API and no behavior.
Then it passed: 5× without race and 3× with `-race` (userwireguard), and the
same under `-race` in proxy and server. No existing test was edited.

**userwireguard** (`device/peer_miss_test.go`, 18 tests): an unknown key
creates a peer and data moves both ways.
- **Off and refusals:**
  - with no handler set, unknown keys are still dropped;
  - a refusal is cached for its TTL, and that cache is bounded.
- **Concurrency:**
  - 33 concurrent initiations from one key make 1 call and 1 peer;
  - 64 concurrent `AddPeerOnDemand` calls produce 1 peer;
  - 1,000 distinct keys at once;
  - 24 real UDP clients handshake at once.
- **Handler failures:**
  - a panic is contained and not cached;
  - a slow handler never blocks workers for other keys;
  - the rate limit bounds handler calls.
- **Eviction and session life:**
  - an evicted peer re-handshakes through the handler;
  - rekey and expiry keep the same peer with no new handler call;
  - endpoint roaming works;
  - only idle peers are evicted, and their goroutines and routes are released;
  - peer snapshots report sessions.
- **Load:**
  - under load the cookie is required before the handler runs;
  - an initiation flood is checked before the handler.

The full module suite passes, and `-race ./device` passes.

**proxy** (`wg_on_demand_test.go`, 9 tests):
- **No pre-registration:** a real client with none moves UDP and TCP both ways.
- **Refusals and reconnects:**
  - unknown keys are refused;
  - an evicted client reconnects;
  - the eviction loop removes idle peers.
- **Only active clients create peers:**
  - 2,000 configured clients: eager +4,000 goroutines and +55 MB, on-demand
    +0 and +0.49 MB;
  - 3 active clients give 3 peers.
- **Status cost:** `PeerStatuses` follows live peers, with 8 allocations at
  both 301 and 3,301 configured.
- **Client changes:**
  - removing a client removes its live peer;
  - a rekeyed client replaces its peer.
- **Handoff:** seeded peers carry traffic to the new instance.

All 27 proxy WireGuard tests (18 existing and 9 new) pass under `-race`.

**server** (`proxy/wg_peers_on_demand_test.go`):
- **Config:** on by default, can be turned off, invalid values keep the
  default.
- **Metrics:** published.
- **2,000 configured clients (DB-backed):** 0 device peers and +0 goroutines.
- **A hosted session without pre-registration:** an HTTP GET over WireGuard
  returns 200, with 1 peer and 1 accepted miss.

The 42 WireGuard-related server tests pass with and without `-race`. They
include TestProxy (all four legs), restart reconnect, handoff fast reconnect
and deploy overlap.

**Soak** (`WG_SOAK=1`): 3,000 keys, 41,000 configured, 16 drivers, 10 s idle
bound.

| Phase | Peers | Goroutines | Heap |
|---|---|---|---|
| 41k configured | 0 | 43 | 20.8 MiB |
| Wave connected | 3,000 | 6,683 | ≈113 MiB |
| After eviction | 0 | 683 | ≈32–44 MiB |

- Three waves gave the same numbers each time, so nothing leaks per peer.
- Each live peer costs 2 goroutines and about 29 KiB of heap.
- At 500/s with burst 250, every key still connected, in 2 passes.

## Expected effect, per process (41.3k configured, 1–3k active)

| | Before | After |
|---|---|---|
| Goroutines | ≈82.6k | ≈2–6k |
| Peer heap | ≈1.1 GB | ≈30–90 MB, plus ≈10 MB of index (245 B per configured client) |
| Stacks | ≈0.45 GB | small |

- **Memory:** about 1.4–1.5 GB less, so Go memory falls from about 6.99 GB to
  about 5.5 GB, about 1.3 GB under the 6.82 GB limit. That takes the process
  out of the continuous-GC regime.
- **CPU, estimated:** GC from about 1.34 to about 0.35 cores per process,
  roughly −10 cores per proxy host. Production will confirm.

## Rollout

1. Ship all three together in one proxy release, built with sibling commits
   that contain userwireguard 4524829 and proxy 05b4ade. Server d97ceb783
   won't compile against the old proxy.
   - userwireguard and proxy are inert alone: no handler set, and the mode off
     by default.
   - server turns the mode on.
2. **Watch, per block:**
   - `wg_device_peers` against `wg_peers`;
   - `wg_peer_misses{outcome}`, especially timeouts and rate-limited misses;
   - `wg_peer_evictions`;
   - goroutines and heap per proxy process;
   - GC cycles/s;
   - WireGuard session counts and handshake success.
3. **Kill switch:** `wg_peers_on_demand: false` in proxy.yml, effective at
   each block's next start.
4. **Afterwards:** revisit the proxy `GOMEMLIMIT` raise (6500 → 7000 MiB,
   2026-10-30) and the 4-core setting in vault services.yml (CPUOPT2.md §8.6
   #2, #9).
