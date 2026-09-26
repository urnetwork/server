# operator-proxy

Operator tooling for urnetwork network operators.

## egress-prober

Measures what each provider's exit actually carries, and where it is, without
asking anyone but the operator's own server.

For every due provider, the prober opens a tunnel pinned to that provider and
runs one **egress-health run** through it: first the operator's own `/ip` echo
(`GET <api>/my-ip-info`) — the warm-up, whose answer is the provider's exit
address — then a random sample of real sites from the server's destination
pool, every load shaped like a browser and retried at spaced intervals. The run
is submitted as the provider's egress health, and the exit address as its
probed location; the server places the address with its own GeoLite2. No
ip-intelligence source is consulted for anything (GEOMAP §11.3).

- A probe request never receives a host-network dialer. Every request egresses
  through the selected provider, including DNS; the allowlist is closed and
  there is no local fallback. Main runs the library from durable taskworker
  shards on hosts with ordinary LAN egress. The standalone command retains the
  confinement check below as defense in depth.
- Every host is verified by WebPKI, and a host the server serves a certificate
  pin for is pinned on top (see "Certificate pinning").
- A tunnel that dies part-way is re-created for the loads it had not finished
  (at most `TunnelRecreateAttempts`, 2, per run); a load whose tunnel could not
  be re-created in time is **not measured**, never failed.
- A provider that refuses to carry the probe is simply not placed; the server
  falls back to its own database.

### Build

This module resolves `github.com/urnetwork/connect` and
`github.com/urnetwork/glog` through `replace ../` directives, so a lone
checkout does **not** build — it fails with a `missing go.sum entry` that names
nothing about the real cause. Check the siblings out beside it, under exactly
these directory names:

```bash
git clone https://github.com/urnetwork/connect.git ../connect
git clone https://github.com/urnetwork/glog.git ../glog
go build ./cmd/egress-prober
```

### Run

```bash
./egress-prober \
  -api-url https://api.example.net \
  -platform-url wss://connect.example.net \
  -operator-secret "$UR_OPERATOR_SECRET" \
  -public-api-url https://api.example.net \
  -cache-ttl 24h \
  -interval 1h
```

That run fetches the prober's own identity from the server. To supply one you
provisioned yourself instead, add `-by-jwt "$UR_PROBER_BY_JWT"` (or export
`UR_PROBER_BY_JWT`); it takes precedence and the fetch is skipped entirely.

`-by-jwt` and `-operator-secret` may also be supplied via the
`UR_PROBER_BY_JWT` and `UR_OPERATOR_SECRET` environment variables instead of
flags, which is the recommended way to run this under systemd (keeps secrets
out of `ps`/shell history). `-api-url`, `-platform-url` and `-operator-secret`
are required; the prober exits immediately with a message naming the missing
flag(s) if any are absent, rather than starting in a broken state.

The prober needs its own network client identity (`-by-jwt`). **Leave it empty
and the prober fetches one for itself** from the server's
`/network/prober-credential` endpoint, authenticating with `-operator-secret` —
no hand-provisioned identity, and one less secret to place. The server mints
that identity in a bootstrap task which runs immediately and then every 6h, so
a prober brought up alongside a fresh deployment may start before its
credential exists: it waits for it, logging one line per attempt on a backoff
capped at 5 minutes, rather than exiting into a restart loop. The wait has no
deadline of its own — impose one with the supervisor's start timeout if a
deployment wants it.

An explicitly supplied `-by-jwt` (or `UR_PROBER_BY_JWT`) always wins and the
endpoint is never contacted, so an existing deployment that provisions the
identity by hand is unaffected and acquires no dependency on it. Either way the
jwt goes through the same startup check, so a credential the process cannot use
stops it at startup instead of leaving a prober that looks healthy and probes
nothing.

`-operator-secret` must match `ingest_secret` in the server's
`provider_egress.yml` vault resource — it authenticates the credential fetch,
the pin fetch and the pool fetch as well as ingest, so a wrong secret stops the
prober at startup rather than only having its submissions rejected.

The prober fetches the server's certificate pins at startup and **refuses to
run if the fetch fails**; an empty set is fine (see "Certificate pinning").
Each pass then fetches the destination pool (`-pool-url`, default
`<api-url>/network/provider-egress-destinations`) and runs the built-in table
for that pass if it cannot. The `/ip` echo (`-ip-echo-url`) is reached
**through** each provider's tunnel, so it must be the api's public address: it
defaults to `<public-api-url>/my-ip-info`, or `<api-url>/my-ip-info` when
`-public-api-url` is empty.

Run `./egress-prober -h` for the full flag list, including `-probe-timeout`
(the cold-start allowance: the warm-up's own timeout and the ceiling of any
request; each load attempt gets the smaller of it and 10s, so it must be at
least 10s) and `-interval 0` (run a single pass and exit, useful for driving
the prober from an external cron/systemd timer instead of its own sleep loop;
`-interval` must not be negative). `-concurrency` and `-blackhole-concurrency`
default to 16 each: a run spends most of its wall clock waiting out spaced
retries, so tunnels mostly wait, and what bounds the number is the host's
**memory** — every tunnel carries its own network stack.

### Confinement (required)

The prober **refuses to start** unless it is confined: at startup it attempts a
direct TCP connection to every probe destination of the built-in table and
exits non-zero if any of them accepts one.

The confinement itself is supplied by the deployment, not by this process —
under Docker Compose a restricted network, otherwise a systemd unit with

```ini
IPAddressDeny=any
IPAddressAllow=<the operator's api/platform addresses>
```

Both mechanisms are outside this process and neither is portably inspectable, so
the prober tests the *property* instead of the mechanism: if a direct connection
succeeds, the confinement is missing and the prober will not run. Without it, a
probe that fails to tunnel could fall back to the host's own egress and measure
**the operator's** reachability for that provider — certifying a provider that
carries nothing as healthy. The addresses tested are derived from
`egresshealth.DestinationHosts()`, so there is no second endpoint list to drift.
The server's pool is fetched per pass, after this check, and changes daily;
the built-in table is its seed, so a host confined against the table's hosts is
confined against essentially everything the pool holds, and a pooled host
beyond it is kept off the direct route by the Go-level boundary every request
has (the tunnel client is the only dialer a probe gets). The operator's own api
host, the `/ip` echo included, is deliberately not tested: a deployment may let
the prober reach it directly, and the echo only ever goes through a tunnel.

**Inability to verify is not evidence of confinement.** The check only reports a
pass when it obtained real evidence, and refuses to start otherwise:

- A host that will not resolve is **not** dialed by name. That dial would be
  re-resolved by the resolver that just failed, so it fails at resolution and
  says nothing about whether the address behind the name is reachable.
- If *some* hosts resolved, those are checked and a `WARNING` names the ones
  that did not — a degraded check never reads like a complete one.
- If *no* host resolved, the prober **exits non-zero**. Under a deny-all
  confinement DNS is blocked too, which used to make every host fall back to a
  name, every dial fail at resolution, and the check log "passed" having tested
  nothing at all — on a host that might have full egress.
- `-confinement-timeout` must be at least **500ms**. Anything shorter expires
  before a connection could have completed, so every address looks blocked
  whether or not it is. (Measured on one unconfined host, in the same second:
  `10ms` → correctly "not confined"; `1ms` → "passed".)

For a jail where DNS legitimately cannot work, supply the addresses instead of
disabling the check: `-confinement-address <ip:port>`, repeated once per probe
endpoint — every egress-health destination of the built-in table (the error
message lists them all; `egresshealth.DestinationHosts()` is the source of
truth). Resolution is then skipped and exactly those addresses are dialed. The
host part must be an IP literal — a name there would put the same hole back.

That list is **139 destinations**, and the self-check dials every resolved
address for them **sequentially** with `-confinement-timeout` each. Against a
firewall that REJECTs (or a docker `internal: true` network, where there is no
route at all) each dial fails immediately and startup is quick. Against one
that silently DROPs, startup costs up to 139 × the timeout before the first
probe, and `-confinement-address` is no longer something an operator can
reasonably maintain by hand at that size — prefer a deployment where resolution
works, or where refusals are immediate. The whole table is covered because
**any** destination can be drawn on any run (see sampling below).

`-skip-confinement-check` disables it. It defaults to **false**, logs two
`WARNING` lines when set, and exists only for an operator running a one-shot
manual probe from a host that is not the operator's. Do not set it in a
deployment.

### Egress health

Provider reliability scoring on the server is **presence-based**:
`reliabilityRunningAggSql` counts reported time blocks and sums
`1.0/valid_client_count`, and never consults delivered bytes. A provider that
stays connected 24/7 while blackholing every byte therefore scores perfectly and
stays selectable — observed on mainnet: one provider accepted 87 KB and returned
0 bytes with `connected = true AND valid = true`.

So every probe is an **egress-health run over the provider's tunnel**. The
built-in table is **139 destinations**, the seed of the server's pool; a run
draws a bounded **random sample** of each class — **50 loads**, the minimum the
server's one-in-ten rule and egress index read (GEOMAP §10.5) — and logs one
line per provider:

```
egress-health: provider=<id> ok=47/50 dns=6/6 connectivity=8/8 cdn=9/10 site=24/26 table=139 retried=4 failed=cachefly,reddit,etsy
```

The classes are what make a partial failure diagnosable. `dns=6/6 cdn=0/10`
is a provider whose egress range is refused by CDNs; `ok=0/50` is a blackhole.
A flat count could not tell them apart.

| class | table | per run | what it proves |
| --- | --- | --- | --- |
| `dns` | 7 | 6 | DoH JSON across seven operators, two of them Chinese. The **answer is parsed**: a 200 with a body proves nothing, since a captive portal returns exactly that. |
| `connectivity` | 14 | 8 | The OS captive-portal endpoints (`generate_204`, `success.txt`, …) and the echo services. Unauthenticated, no anti-bot, 8–69 bytes — the cheapest useful signal in the table. |
| `cdn` | 18 | 10 | CDN edges, distribution mirrors and bulk-download hosts. The class that fails when an egress range is on a CDN blocklist. |
| `site` | 100 | 26 | Ordinary web properties, including the regional ones and the large sites that refuse addresses they take for datacenters. |

**Every class is scored.** The eight sites that used to form an unscored
"reputation" class (akamai, ecosia, reddit, etsy, stack-overflow, reuters,
canva, epic-games) are ordinary site destinations now, with their contracts
unchanged: a site a user cannot reach through an exit counts against it,
whatever the site's reason.

**Every load gets its tries, spaced.** A destination is fetched up to 3 times
(`LoadAttempts`) and fails only when every attempt failed. After a failed
attempt the next waits a random delay — exponential, mean 5 minutes
(`LoadRetryMeanInterval`), capped at three times the mean — so a momentary
block, a rate limit or a flapping path is not hit three times in one second,
and the requests to a site look like a person coming back to it. A
TLS-authentication failure is terminal: a forged certificate is a failure
whatever a retry does. The loads interleave (a load waiting to retry holds no
concurrency slot), so a run whose loads all pass takes one round, and one with a
site that keeps failing about ten to fifteen minutes; its budget is derived
from the schedule (about 35 minutes worst case at the defaults).

**The warm-up.** The first fetch of every run is the operator's `/ip` echo,
with its own timeout (`-probe-timeout`). A tunnel's open returns before any
path to the provider exists; the warm-up pays that cold start, so no scored
load does. Its answer is the exit address, the only thing the location
submission carries. A tunnel re-created mid-run is warmed up again before its
loads use it.

**Browser-shaped requests.** Every request carries a current desktop browser's
top-level navigation headers (a Firefox user agent, Accept, Accept-Language,
the Sec-Fetch family, Upgrade-Insecure-Requests); the profile is data, served
with the pool so it can follow browser releases. `Accept-Encoding` is left to
Go's transport, which asks for gzip and decodes it transparently, and **no
request carries a `Range` header** — a byte-range request is one of the things
bot managers refuse. The TLS fingerprint and the HTTP version (1.1) stay Go's.

**Where a site does not work.** A destination can carry `incompatible` places
(countries, or country and region), and a provider published there is never
asked to load it; the sample sizes are met from the compatible destinations,
and a class too thin for a place is reported as `short=` on the line (and to
the server) rather than padded. A destination the server marks `canary` is
loaded from its incompatible places anyway, unscored, so the server can tell
when it works there again.

**A tunnel that dies part-way is re-created.** The next attempt of every
pending load re-opens the tunnel to the same provider — once for all of them,
at most twice a run — and continues through it. A load whose last attempt had
no live tunnel under it is **not measured**: out of every count, reported as
`not_measured=` and to the server by name. A run that measured nothing is not
submitted at all.

**Sampling, not a smaller table.** The draw happens at run time from the
prober's own crypto-seeded randomness, so **a provider cannot know which
destinations it will be asked for**: to pass reliably it has to carry traffic to
essentially the whole table, which is the thing being measured. There is **one
sampling constant for every deployment** — no beta/mainstream branch, because a
knob only one environment exercises is a knob nobody tests. Sample sizes never
go below **3** per class: below that, one flaky endpoint is half the class.

The `dns` class is **DoH over 443**, not resolvers as such. The 23 bare resolver
addresses in the source list (`8.8.8.8`, `1.1.1.1`, …) are ignored: a resolver
is queried over UDP/53, and this package is handed an `*http.Client` and nothing
else. Genuine resolver coverage needs a UDP path through the tunnel and is not
possible today.

Each destination declares what success means. The default is a 2xx **and a
non-empty body** — the rule that catches a blackhole, since a status line with
no data behind it is exactly what a blackholing provider produces. Endpoints
where an empty body is *correct* (every `generate_204`, and the redirects this
client refuses to follow) declare an exact status instead, which is stricter,
not looser: a provider that synthesizes a bare `200` fails them. A 4xx is never
declared — a refusal must stay a failure — and a `200` with a **zero-length**
body is never declared either, because that is the blackhole signature itself:
the two endpoints measured that way are pointed at a URL on the same operator
that serves a real body.

One measured caveat, recorded because it decided four entries: a redirect status
on a consumer site is **not stable**. Three of the 21 zero-body endpoints
answered differently days apart from the same host and address (`netflix`
302→200, `cnn` 302→200, `hulu` 302→301), and the exits that actually run this
are providers in arbitrary countries. Those, plus `timesofindia`, are pointed at
`/robots.txt` — a real body that does not move — rather than having a
geography-dependent status declared for them.

- Runs are **submitted** (`POST /network/provider-egress-health`) over the loads
  they measured: `ok_count`/`total_count` mean "passed on some attempt" and
  "loaded, after every retry", which is what the server's index and one-in-ten
  rule count. The reputation fields stay on the wire, zero, for one release.
- Destinations are spread across **different operators within each class**, so a
  provider that whitelists one vendor cannot pass a class.
- Each attempt is a small GET with a **per-destination** body cap (768 B for
  DoH, 256 B for connectivity, 1 KiB for everything else, never above 1 KiB),
  read and then closed: one attempt of every sampled load reads at most
  **43,520 bytes ≈ 42.5 KiB**, and a run in which every load fails every
  attempt at most three times that. Without `Range` the cap bounds what is
  *kept*, not what is sent: a server can put up to one TCP window in flight
  before the close, which matters for the two large CDN assets (cachefly's
  10 MB test file and the AWS SDK bundle) — the first entries the pool should
  replace.
- The destinations are reached under ordinary **WebPKI** verification, pinned
  only where the server serves a pin (see below). Pinning 139 leaves that
  rotate on 139 schedules would turn every routine certificate rotation into a
  failure indistinguishable from the provider refusing the destination.
- A run is skipped, and logged as skipped, when the probe has no budget left,
  and a run the pass's context ends under is discarded, not submitted: a run
  on a dead deadline would fail every destination and read as a blackhole.

`egresshealth.DestinationHosts()` is the one place the built-in hosts are
written down — the confinement check and `-confinement-address` guidance both
derive from it.

### Active bandwidth measurement

After a successful probe, the same tunnel carries a throughput measurement —
never a second tunnel — against **two independent targets**:

| target | url | source tag |
| --- | --- | --- |
| operator | `<api-url>/network/provider-bandwidth-test?bytes=N` | `active-operator` |
| cdn | `https://speed.cloudflare.com/__down?bytes=N` | `active-cdn` |

Both take the identical URL shape and run through the identical measurement
code, so neither figure is advantaged by a different request shape.

**Each measurement opens 8 parallel streams of 2 MiB, and the figure is their
aggregate.** This is not a throughput optimisation, it is the difference
between measuring the provider and measuring nothing. A single TCP flow cannot
exceed (send window ÷ RTT), and `connect`'s `MaxWindowSize` is
`scaledPow2WindowSize(mib(1), …)` — so one flow ceilings at 1 MiB ÷ RTT, which
is 11.2 MiB/s at the fleet's median 89 ms. The single-stream version of this
probe measured exactly that ceiling: bandwidth-delay product (measured
throughput × measured RTT) came out at ~1 MiB for **eleven of twelve** beta
providers, and a provider independently measured at 79 MB/s on its own host
reported 4.8 MB/s through the tunnel. Eleven independent providers on eleven
hosts do not coincidentally have capacity equal to one window over their own
RTT.

One flow gets one window; N flows get N windows — the same reason Cloudflare
and Ookla use 4–16 connections. Raising `connect`'s `MaxWindowSize` is *not*
the fix: that is the data path every real user rides.

All at 2 MiB per stream; a sweep is 40 providers × 2 targets = 80 reservations.

| streams | ceiling at 89 ms RTT | per target | full 40-provider sweep | averaged over the hour |
| --- | --- | --- | --- | --- |
| 1 | 11.2 MiB/s | 2 MiB | 0.16 GiB | 0.04 MiB/s |
| 4 | 44.9 MiB/s | 8 MiB | 0.62 GiB | 0.18 MiB/s |
| **8** | **89.9 MiB/s** | **16 MiB** | **1.25 GiB** | **0.36 MiB/s** |
| 16 | 179.8 MiB/s | 32 MiB | 2.50 GiB | 0.71 MiB/s |

8 × 2 MiB puts the ceiling (89.9 MiB/s ≈ 94 MB/s) above the fastest provider
capacity we have independently confirmed (79 MB/s), at 1.25 GiB per sweep —
0.36 MiB/s averaged over the hour a sweep is spread across, against a measured
28–120 MB/s uplink. 16 streams would double both the ceiling and the cost for
no provider we can currently show is being clipped.

The streams are only 8 windows if they are 8 *transport connections*: the
tunnel's client sets `DisableKeepAlives` and offers no ALPN, so HTTP/2 can
never multiplex them onto one. That is asserted directly, on accepted
connections rather than on requests, in
`TestHTTPClientForHostsOpensOneConnectionPerConcurrentRequest`.

A measurement streams until **5 s elapsed or 16 MiB transferred**, whichever
comes first, discarding the first **500 ms** so TCP slow start does not depress
the figure. Throughput is the total bytes all streams moved inside one common
wall-clock window divided by that window — per-stream rates are never summed,
which would report throughput the link never simultaneously carried. A transfer
that finishes inside the warmup window (16 MiB above ~32 MiB/s) reports the
warmup-inclusive rate instead, flagged `(lower-bound)` — reporting nothing
there would exclude exactly the fastest providers, which are the ones most
worth measuring. Parallel streams make that case rarer than the single-stream
probe's ~10 MiB/s threshold, not impossible.

The steady window must also cover at least a quarter of the transfer's wall
clock, or the figure falls back to the same lower bound. Discarding the warmup
can only raise a rate by `total ÷ steady` elapsed — the case where the warmup
carried nothing — so bounding that ratio bounds how far a "steady" figure can
exceed the whole-transfer aggregate, which is the physical ceiling for bytes
that demonstrably moved in that wall clock. Without it, a provider whose
delivery stalls across the 500 ms boundary and then bursts (a windowed tunnel
transport refilling just after it) has the burst's bytes divided by the burst's
own spread: measured at **17× the true aggregate**, published as a steady
figure rather than a bound. It is a ratio and not a fixed floor deliberately:
the ratio puts the steady-path ceiling at ~24 MiB/s (measured ~21), where a
fixed 500 ms floor would have put it at ~16 and given back most of the band the
parallel-stream rewrite exists to have unlocked. Above that ceiling the
lower-bound figure is reported. The old ~32 MiB/s ceiling was partly illusory —
at 31 MiB/s the steady window is already under 20 ms, and 16 MiB divided by a
window that thin is the inflation this bound exists to stop.

Note that `(lower-bound)` is visible only in the prober's log: the ingest body
carries the rate and the byte count, so the server currently stores a
lower-bound figure indistinguishably from a steady one. Closing that needs a
server-side field.

**The two figures are stored and logged separately and are never averaged.**
That is the entire point of having two: a provider that prioritises one path
and not the other is invisible in a combined number and obvious in a pair. The
server row is keyed on `(client_id, source)`, so each target keeps its own row.

One line per provider:

```
bandwidth: provider=<id> operator=12.4MB/s cdn=11.8MB/s
bandwidth: provider=<id> operator=41.2MB/s(lower-bound) cdn=skipped(no byte budget this hour)
```

Every provider is measured, not only those without passive history. The spend
is regulated server-side by an **hourly byte budget**: the prober reserves
before it measures (`POST /network/provider-bandwidth-reserve`), and once the
current hour's bucket is full the server answers 429 and that provider is
skipped cleanly — explicitly, in the log, rather than silently reporting
nothing. A full fleet is therefore covered across successive hours instead of
in one expensive pass. Two targets consume two reservations per provider, so a
given hourly budget covers half as many providers as a single-target probe
would.

**Concurrency, not only bytes.** A byte budget does not bound simultaneous
transfers, and that is the dimension that loads the api. The fan-out is bounded
explicitly at both ends: a measurement opens exactly `bandwidth.StreamCount`
streams and this package has no path to more, and the prober runs
`-concurrency` provider tunnels at a time. Worst case simultaneous transfers
served by the api is therefore `StreamCount × -concurrency` — **8 × 2 = 16** at
beta's deployed `-concurrency=2`, and up to 128 at the flag's default of 16,
though measurements only coincide when runs finish together (a run takes
minutes, a measurement seconds). It scales with `-concurrency`, not with fleet
size; lower `-concurrency`, or set `-skip-bandwidth`, where the api cannot take
it.

Flags: `-skip-bandwidth` turns it off, `-bandwidth-timeout` (default 5s) is the
per-target cap so the added wall clock per provider is at most twice it, and
`-bandwidth-cdn-url` points the second target elsewhere.

Two other CDN targets were measured and rejected: `proof.ovh.net` at 1.0 MB/s
would dominate the time budget and measure the target rather than the provider,
and `cachefly.cachefly.net/10mb.test` is fast (81 MB/s) but fixed-size with no
byte parameter, so it could not share the byte cap.

Both target hosts are added to the tunnel's host allowlist. Note that the
operator target carries `X-UR-Operator-Secret` through a provider-controlled
path; the connection is ordinary WebPKI-verified TLS (a provider on the path
cannot read it without a mis-issued certificate for the operator's own api
host), but that secret gates location ingest fleet-wide, so pinning the api
host is the way to close it if a deployment wants to.

### Which providers get probed

When the server implements `GET /network/provider-egress-due`, it chooses: those
whose stored egress location has gone stale, those never probed, and those not
attempted within its backoff, oldest first. That schedule lives in the database,
so it survives a prober restart instead of re-probing the whole population after
one. `-due-limit` (default 100, server-clamped to 500) sizes the batch;
`-due-url` overrides the derived endpoint.

The due list carries each provider's place (`country_code`, `region`), which
decides the destinations its sample may draw from; a server that predates that
sends bare `client_ids`, which read as providers with no place.

The prober reports **every attempt** back to
`POST /network/provider-egress-attempt`, success or failure, with a short failure
class (`tunnel_failed`, `health_not_run`, `run_not_measured`, `no_exit_ip`,
`submit_failed`). This is load-bearing, not telemetry: the server defers a
provider from the due queue when a probe was recently *attempted*, and a
provider that can never be probed successfully never gets a location row — so
without the report it sorts to the head of the queue on every poll forever and
starves every healthy provider, silently, because the endpoint keeps returning a
full plausible batch.

If the due endpoint returns **404** the server has not deployed it, and the
prober falls back to enumerating providers itself (below) with the `-cache-ttl`
in-memory window applied, exactly as before. A **401** does *not* fall back: that
is a wrong `-operator-secret`, and degrading quietly would produce a
full-looking pass whose every submission the same secret rejects.

The enumeration fallback is broad and stable: it fetches every location that
currently has at least one provider from the operator's server, then asks for
the providers at each of those locations and unions the results. This is
deliberate — asking for only the server's own "best available" guess would
enumerate exactly the providers the server's geo database *already* believes are
in a given place, which is the inverse of what a location-correcting probe needs
to cover.

**Exit codes**, relevant when driving `-interval 0` from cron/systemd:
the process exits non-zero if the provider list could not be fetched at
all, or if a pass completed having submitted nothing while recording at
least one failure (e.g. every probe failed the same way — a wrong
`-platform-url`, a revoked jwt). A pass with nothing to do (no providers,
no failures) still exits 0. In long-running mode (`-interval` > 0) neither
condition stops the process — it logs and keeps retrying on the next
interval, since a systemd-managed service should ride out a transient
server blip rather than die.

### Certificate pinning

Every request a probe makes rides the tunnel of the provider being measured,
so the provider is on the path of every TLS handshake. Ordinary **WebPKI**
verification already means it cannot answer for a host without a certificate a
public CA issued for that host — which is the bar every destination, pooled or
built-in, and the operator's own `/ip` echo are held to by default. A
**certificate pin** narrows that, for a host the server serves one for, to the
keys the server itself observed for it on a direct connection, so even a
mis-issued, chain-valid certificate from another issuer is refused. No host is
required to carry one: the three geolocation sources that were are gone.

**The pins come from the server, not from this repository.** At startup the
prober fetches them from `GET /network/geolocation-source-pins` (operator
secret, the same header as the due list) and re-fetches every
`-pin-refresh-interval` (default 1h). The server observes them itself, every
six hours, by connecting to each host **directly** — its own network, no
provider anywhere in the path — under full WebPKI, and recording the leaf and
issuing-intermediate SPKI from the *verified* chain. That is what makes a
server-chosen pin trustworthy for a request that rides a provider's tunnel: a
provider cannot influence what the server saw. The one host most worth
pinning now is the operator's own api host, whose `/ip` answer places the
provider; the server serves a pin for it once its observation job covers it.

**The contract:**

- A pin set that cannot be fetched at startup — server unreachable, 404, 401,
  500 — and the prober **does not start probing**: a pin the server serves is
  one it wants enforced, and starting without the set would drop every one. An
  **empty** set is a valid answer.
- Half a pin (no leaf or no intermediate) makes the whole set an error; the
  server never writes one, so the shape means something is wrong upstream.
- A **refresh** failure keeps the last good set and logs. A stale pin still
  rejects a provider substituting its own certificate; what it eventually stops
  doing is matching the legitimate host after a CA change, which fails closed
  and loudly.
- The served set can never widen a tunnel's allowlist: the pin map is part of
  it, so each probe cuts the set down to the hosts it dials (its pool's, the
  echo's, the bandwidth targets') before the tunnel opens.

Each host's entry lists a leaf pin and an intermediate-CA pin, and
`providertunnel`'s pin check accepts a match against **either** — it walks
every certificate in the verified chain and accepts as soon as one matches an
allowed pin. That makes the intermediate entry a real safety net between two
server observations: a routine leaf rotation still chains to the same
intermediate, so probing keeps working. The tradeoff is that pinning an
intermediate trusts that CA, not one specific certificate, for that host.

### Design

This tool measures a network provider by routing HTTPS requests *through that
provider's own tunnel*, rather than trusting the provider to self-report: the
prober host never reaches a probe destination directly, only through a tunnel
pinned to exactly one provider, so each answer reflects that provider's actual
egress. The operator's own `/ip` echo gives the exit address, which the server
places with its own GeoLite2; the sampled real sites give the egress health
the server's index and one-in-ten rule read. Both are submitted to the
operator's server (see `ingest/`). The spec is `connect/GEOMAP.md` §11; this
README plus the package doc comments in `egresshealth/`, `fleetprobe/`,
`providertunnel/`, `prober/`, and `ingest/` are the design reference.
