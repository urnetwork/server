# APIDRAIN1 — api drain: already nearly seamless; make the last gaps honest

How an api service deploy affects clients today, why the api is structurally
the easiest service in the fleet to drain (and is mostly there already), and
the small set of fixes that close the remaining client-visible gaps.

Companion to CONNECTDRAIN2.md (connect drain) and PROXYDRAIN1.md (proxy
drain), reusing the warpctl scaffold facts documented there.

Status: P0-P3 IMPLEMENTED 2026-07-19 (same day as the audit) — see §7 for
the as-shipped inventory and the deltas from the design below.

## 1. How an api deploy works today

### 1.1 The warpctl scaffold (same make-before-break as everywhere)

api runs behind nginx on the 5 edge hosts, blocks beta:1, g1:24, g2:25,
g3:25, g4:25 (`vault/main/services.yml:301-335`). Deploy per block
(`warp/warpctl/run.go:582-665`):

1. New container starts on rotated internal ports; health-polled up to 120s
   on `GET /status` directly against the container (`run.go:1200-1292`). The
   poll PARSES the JSON and fails on a `status` matching `^error\s`
   (`docker.go:659-663`) — so real readiness plumbing exists end-to-end; a
   failing poll reverts the deploy (`KillWorker`, `run.go:595-599`) and the
   old container keeps serving.
2. `redirect()` swaps DNAT: NEW connections go to the new container;
   ESTABLISHED connections keep flowing to the old one (`run.go:609`).
3. `cleanupStaleConntrack()` is udp-only — a no-op for the tcp-only api.
4. Old containers drain asynchronously, serialized one-block-per-host by the
   host drain flock plus a settle delay (`run.go:675-708`,
   `host_drain_lock.go`): `docker update --restart=no` + `docker stop -t
   3600` → SIGTERM with a 60min SIGKILL ceiling (`run.go:48,2112-2141`).

### 1.2 The nginx layer (the reason clients never see api deploys)

Client TLS/h2/h3 connections terminate at nginx; nginx reaches only LOCAL
api blocks through stable per-(service, block) external ports
(`config.go:2019`), so an api deploy changes NO nginx config and touches NO
client connection — the DNAT swap is invisible to both sides.

- Upstreams: `least_conn`, `max_fails=0 max_conns=32768` (no passive
  ejection; every request just tries) (`config.go:2007-2027`).
- Keepalive pool lb→api: 1024 idle conns, 1m idle timeout, 15m conn life,
  8192 requests/conn (`config.go:505-512,2029-2041`), engaged via
  `proxy_http_version 1.1` + `Connection: keep-alive` (`config.go:1849,2351`).
  After the flip, pooled conns KEEP carrying new requests to the old
  container until they age out or the old server closes them — overlap
  continuity, ended by the SIGTERM below.
- Retries: `proxy_next_upstream error timeout http_500 http_502 http_503
  http_504 non_idempotent`, `proxy_next_upstream_tries 2`
  (`config.go:1851-1852`). 30s connect/read/send timeouts; request bodies
  fully buffered before forwarding, responses UNBUFFERED straight to the
  client (`config.go:1845-1855`).

### 1.3 What the api binary does on SIGTERM: mostly the right thing

`cli/api/main.go:43-57` — SIGTERM/SIGQUIT → cancel root ctx →
`server.Shutdown(30s)` (`http.go:420-427`): the listener closes, idle conns
close, in-flight requests run to completion. Crucially:

- Request/session contexts derive from `req.Context()`, NOT the root ctx
  (`session/client_session.go:41-64`; `http.Server.BaseContext` unset in
  `http.go:401-407`) — canceling the root ctx does not abort in-flight
  handlers. The graceful semantics are real.
- During Shutdown, Go stamps `Connection: close` on in-flight responses
  (net/http `doKeepAlives()` is false while shutting down), so nginx retires
  each pooled conn cleanly after its current response.
- Listen starts only after `server.Warmup()` (`main.go:86`): maxmind load
  (`ip.go:32`), search impl creation, and a pg query
  (`model/network_client_location_model.go:38-58`) — so "listening" already
  implies "pg was reachable at startup". Search queries fall back to
  db-backed search until the local index initial sync completes
  (`search/search_local.go:196-234`) — no post-deploy correctness dip.
- pg pool pre-establishes `pool_min_conns` (`db.go:176-184`), and the
  handler cache is redis-backed (`router/handler_cache.go:17-51`) so it
  survives deploys — post-flip cold start is genuinely small.

### 1.4 What a client experiences

Nothing, in the common case. In-flight requests complete; pooled-conn races
at the Shutdown instant are absorbed by nginx's retry; new requests are on
the new container from the flip onward. The residual client-visible cases
are the subject of §2.

Client-side resilience facts (why the residuals matter): every Go client
funnels api calls through `connect.ClientStrategy` — transport-level
failures (refused/reset) are retried with dialer failover for up to ~15s
(`../connect/net_http.go:422-720,1074-1098`), but a surfaced HTTP 5xx is NOT
retried by ANY caller and even scores the dialer as healthy
(`net_http.go:1139-1141,1267-1275`). The one exception: `/auth/refresh`
retries any non-401 forever with 0–15min random backoff
(`../sdk/device_token_manager.go:134-158`). The JS SDK is a bare `fetch`
with no retry at all (`../sdk/js/src/api.ts`). So: whatever escapes nginx as
a 5xx lands on app code unretried — keeping failures inside nginx's
retryable classes IS the seamlessness mechanism.

## 2. The gaps (ranked by expected client impact)

### 2.1 P0 — `/status` is a constant "ok": the flip gate is fake

`collectStatus` returns "ok" unconditionally — the pg/redis pings are
commented out (`router/warp_handlers.go:69-91`). The 120s health poll
therefore gates the flip on "process listens", not "process can serve".

Warmup narrows this (listen implies pg-at-startup), but NOT redis: a
container with broken redis (vault drift, auth, network) passes health and
takes 100% of its block's new traffic while `/network/find-providers2`,
handler caches, and api-key auth fail. Per §1.4, those 5xx are not retried
by clients; if a bad version rolls to all blocks, nginx's 2 tries exhaust on
equally-broken siblings and clients eat raw errors — the one scenario the
otherwise-excellent scaffold cannot save you from, and the plumbing to catch
it (poll parses `status`, `error ...` reverts the deploy) already exists.

Fix (small, server-only): a LATCHED readiness bit. At startup, after
`Warmup()` and before listen, run the deep checks ONCE — pg `SELECT 1`,
redis `PING`, anything else a route hard-depends on — and latch the result;
`collectStatus` returns the latch (plus `error: draining` once draining
starts, see §2.3). `/status` stays O(1) per poll; a failed check keeps the
container un-ready → poll times out → deploy reverts, old container keeps
serving. Zero warpctl changes.

### 2.2 P1 — the 30s shutdown ceiling, and panic as an exit path

Two defects in the same few lines:

- `ShutdownTimeout` (30s, `cli/api/main.go:105`) equals `WriteTimeout` — a
  conforming request that started just before SIGTERM can need up to
  ~ReadTimeout+WriteTimeout (45s) to finish, so the deadline CAN fire and
  hard-cut live connections. A cut mid-response is unrecoverable downstream:
  `proxy_buffering off` means bytes already streamed to the client
  (truncation, unretriable); a cut sent-but-unanswered POST gets RE-EXECUTED
  on a sibling via `non_idempotent` — the double-execution worst case exactly
  in the drain window.
- When Shutdown times out, `HttpListenAndServeWithReusePort` returns
  `context.DeadlineExceeded` and main PANICS (`cli/api/main.go:115-117`) —
  drain overrun exits via panic with a stack trace instead of a logged
  outcome.

Fix: raise `ShutdownTimeout` above the max request lifetime (60–90s; docker
grants 3600s and Shutdown returns the instant conns drain, so the higher
ceiling costs nothing in the common case — the CONNECTDRAIN2 §3.4 prompt-exit
lesson still holds). Treat a Shutdown deadline as a logged+counted drain-cut
event and exit cleanly, not a panic.

### 2.3 P2 — keepalives-off grace: retire pooled conns instead of racing them

Today Shutdown snap-closes all idle pooled conns; nginx can be writing a
request into one at that instant. The failure is absorbed by
`proxy_next_upstream ... non_idempotent` retry — i.e. correctness for POSTs
currently leans on a retry mode that is itself a double-execution hazard
(§2.4).

Fix: a drain grace phase in `HttpListenAndServeWithReusePort` (opt-in field
on `HttpServerOptions`, so connect/proxy/taskworker are unaffected until
they opt in): on ctx cancel, first `SetKeepAlivesEnabled(false)` and keep
serving for `KeepaliveDrainTimeout`; every pooled conn that carries one more
request receives `Connection: close` and retires cleanly; then `Shutdown()`
as today. `/status` reports `error: draining` during the grace (harmless:
the flip already happened; only observers see it).

- Default ~10s: drains every ACTIVE pooled conn cleanly; the residual race
  (a conn idle for the whole grace, reused at the closing instant) stays
  retry-covered.
- 70s variant: outlives nginx's 1m pool idle-timeout, so every idle conn is
  closed by NGINX first (client-initiated close = no race at all) — buys a
  literally-empty race window at the cost of ~70s×(blocks/host) of
  serialized rollout time. Tunable either way.

### 2.4 P2 — narrow the nginx retry classes (deploy-adjacent correctness)

`http_500 ... non_idempotent` (`config.go:1851`) means: every POST that
returns 500 is re-executed on a sibling, and every POST that exceeds 30s is
re-executed WHILE the first is still running (the Go handler doesn't stop
when nginx walks away). That is an at-least-once landmine in steady state,
not just at deploys — payment paths survive it only because they are
idempotency-keyed.

Recommendation, sequenced: drop `http_500 http_504` now (deterministic app
errors should not re-execute POSTs or double load; readiness (§2.1) removes
the bad-container scenario that made 500-retry attractive). Keep `error
timeout http_502 http_503`. Revisit `non_idempotent` only AFTER §2.3 lands
(the grace phase empties the race it papers over); until then it stays,
because per §1.4 a surfaced 502 on a POST is not retried by any client.

### 2.5 P3 — drain observability

Today a deploy leaves no record of whether anything was cut:

- `StartStatsPusher(ctx)` and `RouterStats` hang off the ROOT ctx
  (`cli/api/main.go:88`, `router/router.go:65`) — both stop at SIGTERM,
  so the drain window's requests (up to 30s of traffic per block) are
  dropped from stats. Flush a final snapshot at drain end instead.
- Add a drain summary log + metrics, mirroring CONNECTDRAIN2 §3.5:
  `urnetwork_api_ready` (0/1 + reason), `urnetwork_api_drain_seconds`,
  `urnetwork_api_drain_inflight` (at SIGTERM), `urnetwork_api_drain_cut`
  (conns alive at deadline — should be 0 forever), and a
  `monitor/SIGNALS.md` entry: "api deploy → drain_cut > 0" is the page.

### 2.6 P3 — client-side retry for surfaced 5xx (separate track)

The nginx layer can't cover everything (both-tries-bad, truncation, lb host
loss). A small client-side policy — one retry with jitter on 502/503 for
idempotent GETs in `connect.ClientStrategy`, and minimal retry in the JS
SDK — hardens clients against ALL brief outages, not just deploys. Noted
here because §1.4's asymmetry (transport errors retried, 5xx never) was a
finding of this audit; belongs to the connect/sdk repos.

### 2.7 Explicitly not problems (audited, leave alone)

- In-flight requests surviving SIGTERM: correct (session ctx from
  `req.Context()`).
- Client connection continuity: api deploys cannot drop a client conn (nginx
  terminates; nginx config/ports stable across service deploys).
- Search warm-up: db fallback until initial sync — graceful.
- Handler cache: redis-backed, deploy-immune.
- Concurrent block flips (all 5 within ~a minute): fine given warm-up
  ordering + pool min_conns; flips are make-before-break. Only revisit
  (stagger flips) if deploy-time p99 says otherwise.
- The 3600s docker grace: api uses ≤ ~1min of it; drains are flock-serialized
  with a settle delay — rollout time stays bounded.

## 3. Hazard review

| Concern | Answer |
|---|---|
| Latched readiness misses a runtime pg/redis outage (container says ok while degraded) | Deliberate: `/status` gates DEPLOYS, and nothing else routes on it (nginx has no active checks, `max_fails=0`). Runtime health is monitoring's job; a per-poll deep check would couple every deploy poll (and any monitor scrape) to db load and shared-fate flapping. |
| Readiness check blocks all deploys when redis is down fleet-wide | Correct behavior: a container that cannot serve must not take traffic. Failure mode = poll timeout → revert → old containers keep serving; same as any unhealthy container today, now truthful. |
| Grace phase extends the dual-version window | By `KeepaliveDrainTimeout` (seconds), bounded and stateless — api has no version-coupled server state; body/handler compat across one version is already required by the overlap that exists today. |
| Longer ShutdownTimeout slows rollouts | Only when requests genuinely run that long; Shutdown returns the moment the last conn drains. The flock's serialization already dominates rollout pacing. |
| Dropping http_500 retry unmasks single-container bugs | Readiness (§2.1) removes the broken-container-takes-traffic case; a code bug 500s identically on every sibling, where retry only doubled the damage. |
| `Connection: close` responses during grace confuse nginx | That is the designed signal: nginx retires the conn after the response and re-balances — the same mechanism any nginx-fronted service uses to drain. |
| Drain-cut metric never exercised → rots | It is the point: alert on nonzero. The e2e test (§5) exercises it synthetically. |

## 4. Rollout

1. **P0 readiness latch** (`router/warp_handlers.go` + a startup check in
   `cli/api/main.go`) — ships alone; verify with a deliberately
   redis-broken container in a beta block: poll must time out and revert.
2. **P1 shutdown ceiling + clean exit** (`cli/api/main.go`,
   `http.go`) — trivial; verify drain logs show clean outcome.
3. **P2 grace phase** (`http.go` opt-in + `cli/api/main.go`) — flag-style
   via `HttpServerOptions.KeepaliveDrainTimeout` (0 = off), per the
   no-globals settings convention. Verify: deploy-window nginx error/retry
   counts drop to ~0.
4. **P2 nginx retry narrowing** (`../warp/warpctl/config.go:1851`) — after
   (1)-(3) are observed healthy across a few deploys; lb redeploy required.
5. **P3 observability** ships with (2)/(3); SIGNALS.md entry.
6. **P3 client retry** — separate connect/sdk track.

## 5. Implementation inventory (files)

- `router/warp_handlers.go`: latched readiness + draining state in
  `collectStatus` (the commented-out pings become the one-shot startup
  check).
- `cli/api/main.go`: run readiness checks post-`Warmup()` pre-listen; raise
  `ShutdownTimeout`; drain summary log; no panic on drain deadline.
- `http.go`: `HttpServerOptions.KeepaliveDrainTimeout` + grace phase
  (`SetKeepAlivesEnabled(false)` → wait → `Shutdown`); distinguish
  clean-vs-deadline shutdown in the return.
- `router/router.go` / `grafana.go`: final stats flush at drain end; drain
  counters.
- `../warp/warpctl/config.go:1851`: retry-class narrowing (phase 4; lb
  deploy).
- `monitor/SIGNALS.md`: api drain section.
- Tests: an e2e in the family of `connect/exchange_drain_e2e_test.go` —
  in-flight request at SIGTERM completes; pooled idle conn reused during
  grace gets `Connection: close` and succeeds; request outliving the
  (test-shortened) ceiling is counted as cut; `/status` transitions
  ok→draining→gone.

## 6. Open decisions

1. `KeepaliveDrainTimeout` default — 10s (drains active conns; residual race
   stays retry-covered) vs ~70s (outlives nginx's 1m pool idle-timeout, race
   window empty, slower serialized rollouts). Start 10s; the §2.5 metrics
   decide whether 70s buys anything real.
2. Whether `non_idempotent` is dropped in phase 4 or kept until §2.6 client
   retry exists (a surfaced POST 502 is unretried by every current client).
3. Whether the readiness latch should also verify vault-materialized
   configs beyond pg/redis (jwt keys parse, maxmind loaded — the latter is
   already implied by warmup ordering).
4. Whether taskworker/connect/proxy adopt the same grace phase once proven
   on api (the `HttpServerOptions` field makes it a one-liner each; proxy's
   drain design in PROXYDRAIN1 §3 subsumes it).

## 7. Implementation inventory (as shipped, 2026-07-19)

**P0 — readiness latch (server repo).**
- `api/readiness.go`: `ReadinessCheck` — one-shot pg `SELECT 1` + redis
  `PING` (15s ceiling), run in `cli/api/main.go` before warmup/listen
  (taskworker's check is the sibling, TASKDRAIN1 §2.2). On failure the
  container serves the latched `error not ready: <check>: ...` status —
  warpctl's poll reads it, times out, and reverts to the old container —
  skips warmup (it hits the same dependencies), and keeps serving
  best-effort in case traffic already points here (a restart in place,
  where the DNAT still targets this container). On success: warmup →
  `SetWarpStatusReady`. `urnetwork_api_ready` gauge (0 on a failed check
  and from drain start), mirroring the taskworker/proxy ready gauges.

**P1+P2 — drain sequence (server repo).**
- `http_drain.go` (new): the shared drain machinery for
  `HttpListenAndServeWithReusePort` (+ TLS variant) — on serve-ctx cancel:
  flip the drain flag (every http/1 response stamped `Connection: close` —
  `SetKeepAlivesEnabled(false)` was NOT usable, it snap-closes idle conns,
  the exact race the grace avoids), keep serving `KeepaliveDrainTimeout`,
  then `Shutdown(ShutdownTimeout)`. A ConnState tracker counts open/active
  conns; outcome is clean (nil) or a typed `*HttpDrainCutError` carrying the
  hard-cut count. Gauges are service-neutral `urnetwork_http_server_*`
  (draining, drain_seconds, drain_inflight, drain_cut_connections) — the
  stats pusher's {env, service, block, host} grouping identifies the service,
  and any service that opts into `KeepaliveDrainTimeout` gets them for free
  (§6.4 resolved: adoption is a two-field options change per service).
- `cli/api/main.go`: serve ctx (ends at SIGTERM) split from process ctx
  (ends at exit) so the router stats reporter and stats pusher publish
  THROUGH the drain and flush at exit; `ShutdownTimeout` 30s→60s (>
  ReadTimeout+WriteTimeout=45s, so the deadline can only cut a
  non-conforming connection); `KeepaliveDrainTimeout` 10s (§6.1: start
  low; `drain_seconds`/retry metrics decide whether ~70s — outliving
  nginx's 1m pool idle timeout — buys anything); drain-cut exits cleanly
  via `errors.As` instead of the old `panic(context.DeadlineExceeded)`.
- `router/warp_handlers.go`: /status now serves a LATCHED status
  (`SetWarpStatusReady` / `SetWarpStatusNotReady` / `SetWarpStatusDraining`,
  shipped jointly with the taskworker drain session). DELTA from §2.3/§3:
  draining is the 200-json status "draining", deliberately NOT an error and
  NOT a 503 — the deploy poll never targets the draining container, and
  fleet status sampling must not count an operator drain as a service
  error. The api flips it at SIGTERM before the serve ctx cancels.
- `router/router_stats.go` + `router/router.go`: `RouterStats.Flush()` /
  `Router.FlushStats()` — the periodic reporter's body extracted and
  callable once at drain end. `grafana.go`: `StartStatsPusher` returns a
  final-flush func (mutex-shared pusher; no-op when not under warp).

**P2 — warp repo.**
- `warpctl/config.go`: `proxy_next_upstream error timeout http_502
  http_503 non_idempotent` — http_500 and http_504 dropped (§2.4), so a
  deterministic application error is never re-executed on a sibling.
  `non_idempotent` KEPT for now (§6.2): the grace phase empties most of the
  race it covers, but a surfaced POST 502 is still unretried by clients, so
  it stays until the client-retry adoption data says otherwise.
- `warpctl/docker.go`: `IsError` regex `^(?i)error\s` → `^(?i)error(\s|:)`.
  AUDIT CORRECTION to §1.1/§2.1: the old regex missed the natural
  `collectStatus` formatting "error: ..." — an erroring status JSON would
  have PASSED the deploy poll. Both separator forms now fail it, which is
  what makes the P0 wiring purely server-side.

**P3 — client retry for surfaced 5xx.**
- `../connect` `net_http.go`: `HttpGetWithStrategyRaw` retries a GET whose
  response status is in `GetRetryStatusCodes` ([502, 503]) `GetRetryCount`
  (1) times after a jittered `GetRetryMin/MaxTimeout` (100ms-1s) pause —
  GETs only (already re-issued by the parallel dialer racing; idempotent by
  convention); POSTs are never replayed. Settings in
  `ClientStrategySettings`, so the gomobile sdk inherits it.
- `../sdk/js`: `src/utils/fetch_retry.ts` `fetchWithGetRetry` (one jittered
  retry on 502/503 or a network-level failure, GET only), wired into the
  api client's GET call sites; `npm test` now runs `node --test` over
  `test/*.test.ts` (node ≥23.6 type stripping — no new dependencies).

**Tests.**
- server `http_drain_test.go`: grace serves + stamps `Connection: close`
  (asserted via `response.Close` — the go client strips the hop-by-hop
  header itself), in-flight completes across a no-grace drain with prompt
  exit, deadline overrun returns the typed cut error + gauge, ConnState
  tracker transition table, drain-handler stamp (http/1 only).
- server `router/warp_handlers_status_test.go`: the latch sequence
  (ok → not-ready → ready → draining) and a pinned copy of warpctl's
  IsError regex proving not-ready fails the poll while ok/draining pass;
  `RouterStats.Flush` preserves the live bucket.
- server `api/readiness_test.go`: nil against a healthy env; a canceled
  ctx surfaces as a `pg: ...` error VALUE (the latch input), never a
  crash.
- warp `warpctl/docker_test.go`: IsError matrix (both separator forms, case
  insensitivity, "draining"/"erroneous" negatives).
- connect `net_http_retry_test.go`: 502→200 retried (exactly 2 hits via a
  single-dialer strategy), 503 exhaustion, 500 not retried, POST never
  replayed.
- js `test/fetch_retry.test.ts`: the same matrix for the fetch wrapper,
  plus network-failure retry and default-method-GET.

