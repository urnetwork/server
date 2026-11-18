# P90SPEED: hosted proxy setup time on Main (2026-11-18)

A customer asked us to bring down the p90 of hosted-proxy setup time, especially for US
exits; they found the p50 good enough. This document is the deep dive behind that
request.

It draws on:
- the proxy-test runner's 19,000 runs since 11-07;
- five read-only investigations on 11-18 between 01:50 and 03:00Z:
  - the regression timeline;
  - server-side traces of single setups;
  - the hosted proxy's code path and builds;
  - the connect window's code path;
  - the provider pool and exit selection;
- a separate investigation of stuck-provider recycles.

Nothing was changed in production while measuring.

Marks: **[M]** measured on Main (the runner, Mimir, Loki, read-only SQL); **[C]** read
in code; **[I]** inferred. Times are UTC. Provider and customer networks are not named;
"fleet J" and similar labels stand in for them.

## Summary

- **The p90 is a regression from 11-14, not a long-standing tail [M].**
  - Before 11-14 ~14:30Z, setup p90 over all countries was 4.3–6.9 s in every 6-hour
    bucket, and the US p90 was 2.3 s (146 runs).
  - From 11-14 15:17Z the all-country p90 rose to 16–37 s by the hour. It stood at 16.4 s
    over the last 24 h, and the US p90 since 11-14 12Z is 24.0 s (58 runs).
  - The p50 rose too, from ~2.2 s to ~3.7 s.
- **What a slow setup waits on [M][C].**
  - The first CONNECT builds the proxy's device and then waits in `WaitForRoute`
    until one window exit has answered.
  - The wait is long because the hosted mint admission, at 2 concurrent mints per proxy
    process, refuses window-client mints with a 429. Connect treats that as a rate limit
    and backs off 5 s, doubling each time.
  - One refused mint costs ~3–5 s, two cost 9–13 s and three cost 24–31 s.
  - 96% of fast setups (<3 s) met no refusal; 85% of slow ones (≥15 s) met at least one.
  - Setups that met none still have the old profile: p50 2.6 s, p90 6.7 s.
- **Why the refusals started: load growth on proxy processes that hit their memory limit [M].**
  - **The load:** from 11-14 13:51Z, bot sweeps began reopening thousands of hosted
    proxies at once. Device builds went from ~500 per 5 min (11-13) to 1.9–4k per 5 min,
    with bursts of 6–8k every 40–45 min, and at least 80% of them are reopens. Every
    reopen mints its window from scratch.
  - **The memory limit:** the proxy processes have a 6500 MiB Go memory limit.
  - **The result:** under the new load they sat at that limit, GC ran 10–13 times a minute
    per process, and both proxy hosts went from 3–5 to 16–23 busy cores of 24.
  - **Why that makes refusals:** a CPU-starved mint holds its slot longer, so the 2-slot
    admission refuses more.
  - **The proof:** raising crisp's limit to 8000 MiB on 11-15 was the only change that
    improved setup. At the same demand, crisp now refuses 6.8 mints/s against fireside's
    96.8/s. Setup p90 is 9.5 s on crisp against 20.2 s on fireside.
- **Not the cause [M]:**
  - no build;
  - the stuck-provider recycle;
  - the connect key-event storm, which was load, not a gate;
  - the size of the US pool;
  - Main.
- **The fixes, in order:**
  1. **Restore headroom on the proxy hosts.** Fireside is still at 6500 MiB, and crisp is
     nearing its 8000. *Owner decision.*
  2. **Stop refusing a window's first mints.** Use a reserved slot or a short wait for
     windows with no exit, or more slots, and have connect retry fast while a window is
     empty. Expected p90 ~10–11 s at today's load, toward the 6.7 s of refusal-free setups.
  3. **Make reopens reuse their window identities** instead of minting again. This cuts
     25–30% of mints and is the customer's reuse case.
  4. **Keep exit selection decisive under load.**
  5. **Measure the customer's real first-CONNECT time,** and weight the runner's US lane
     so the US p90 is no longer two or three samples.
- **For the customer's use case:**
  - US supply isn't the limit: 22k US exits pass every filter.
  - Selection never leaves a pinned country. The 2% of US setups that exited elsewhere
    were geolocation disagreements on a few provider networks, and a country check that
    would catch them has been unused since 09-27.
  - One hosted proxy spreads a session over up to 6 exits, which matters for
    multi-account browsing (§7).

## 1. The number

**What it measures [C].** The proxy-test runner on planetoid loops two workers over
~100 countries, round-robin. Each run:
1. Creates a hosted proxy pinned to the country (POST auth-client; ~0.08 s).
2. Probes through `https://<token>.<proxy host>:443` until the first request succeeds.
   *Setup* runs from just before auth-client to that success.
   - Attempts retry every 1 s and last up to 35 s each.
   - The proxy works on one CONNECT for up to 30 s, then answers 504.
   - Past 60 s the run is a `ready_timeout`.
3. Loads a site in headless Chrome, then removes the proxy.

The runner has been unchanged since 11-10 00:17Z (bfb44c9).

**The dashboard's US p90 is noisy [M][C].**
- **How it's computed:** the panel takes a nearest-rank p90 over the last 24 h of
  *completed* US setups. A restart empties the window.
- **Missing failures:** ready_timeouts are left out, so the panel understates the tail.
  Over all countries 12–15% of runs time out today.
- **Too few samples:**
  - The US gets one slot in ~100, so the window holds 13–22 samples, and the p90 is the
    2nd or 3rd slowest run of the day.
  - It read 3.6 s at 11-17 18Z and 24.0 s six hours later.
  - A bootstrap over a 20-sample window after the regression spans 7.0–26.7 s.
- **Sample size needed:** a p85–p95 band needs about 100 samples (§8).

## 2. The regression

**All countries** (single-attempt runs; change points on log(setup)) [M]:

| Period | Setup p50 | Setup p90 | Notes |
|---|---|---|---|
| 11-07 → 11-14 14Z | 2.1–2.8 s | 4.3–6.9 s | ready_timeout 5–14% in 6-h buckets |
| Rise 1, change point 11-14 15:17Z | 2.24 → 4.38 s | 6.4 → 15.9 s | 11-14 20Z: p90 36.8 s, 42 of 53 runs ready_timeout |
| Easing, change point 11-15 21:48Z | 5.75 → 3.17 s | 17.4 → 9.8 s | crisp's memory limit raised (P1115) |
| Rise 2, gradual from 11-17 ~02Z | ~3.5 s | 9.1 → 13–35 s by the hour | load growth on a fleet without headroom |
| Last 24 h (11-17 03Z → 11-18 03Z) | 3.66 s | 16.4 s | |

**US** (15–22 runs a day) [M]:
- **Daily p90:** 1.4–3.3 s from 11-07 to 11-13, then 17.9, 18.0, 12.3 and 24.0 s from 11-14
  to 11-17.
- **Before and after 11-14 12Z:**
  - before: p50 1.13 s, p90 2.27 s, 1% over 8 s, 0 ready_timeouts (146 runs);
  - after: p50 1.98 s, p90 24.0 s, 25% over 8 s, 3 ready_timeouts (58 runs).

**Events around the steps [M]** (Mimir build_info and process start times; full table in
Appendix A):
- **Proxy builds:**
  - D47p rolled 11-14 10:44 → 13:18Z;
  - D49p 18:33–18:39Z;
  - P1115 11-15 20:31 → 22:56Z, with crisp's limit raised to 8000 MiB from 21:39Z;
  - D52p 11-17 06:06 → 12:43Z;
  - the nightly 1101053110 restarted all 20 processes at 11-17 14:04–14:07Z.
- **The load ramp:**
  - live devices went 16.9k → 36.6k between 13:51 and 14:03Z on 11-14;
  - the connect key-event resubscribe storm started at 13:51–13:55Z, with the ramp;
  - the D48 api roll's first new process came up at 13:59Z.
- **The key-event storm cleared** during the D51kes connect roll, 11-15 18:16 → 20:46Z,
  with no setup step.
- **The stuck-provider recycle:** beta 11-15 22:00Z, all connect blocks 11-16 14:27Z, with
  no setup step.
- **Restarts:** after every proxy restart (11-14 18:40Z, 11-15 01:14Z, 11-17 14:07Z) cores
  halved and p50 fell to 2.5–3.0 s for about an hour, then relapsed within 1–4 h.
  P1115 on crisp is the only step that held.

## 3. What a slow setup waits on

### 3.1 The path of a first CONNECT [C]
At server 1accf89b5 and connect f04d9a22, the nightly running on every proxy:

1. **auth-client** picks a random host and block for the proxy and writes the device
   config's Redis mirror (`model/network_client_proxy_model.go:395, 712-720`). Since D47p
   the proxy block's notification only adds WireGuard peers; it opens no device.
2. **TLS:** `GetTlsConfig` (proxy lib `tls.go:118-219`) handles each new SNI under one
   global mutex, a minor cost. Each CONNECT gets one 30 s budget (`http.go:98, 464`).
3. **`connectDial`** (`proxy/server.go:714-784`) does three things in turn:
   1. checks the caller (`proxyLock`: a 30 s cache, then Redis, then the DB);
   2. **opens the device** (`OpenProxyDeviceWithContext`, `proxy_device.go:496-613`).
      `NewProxyDevice` (`:1396-1562`) runs `LoadByJwtFromClientId` (DB),
      `MintHostedSession` (a DB transaction plus a session Lua), the local authority, the
      DeviceLocal, `SetConnectLocation` (which starts the windows) and the tun;
   3. waits in **`WaitForRoute`** (`proxy_device.go:2197-2223`) until the window is
      satisfied or its **first exit is added** (`:2276-2278`), within the 30 s budget.
4. **The window**, in the hosted 32 MiB profile, is a quality window of 5 exits plus a speed
   window of 1, with ipv4/any selectors (`proxy_device.go:1657-1661`). Keys stay local and
   encryption is off unless post-quantum is on.
   1. **find-providers2** runs in the proxy process, with no cache or batching
      (`localclient/authority.go:356-377`).
   2. Each candidate needs a **mint**: a new window client.
   3. An exit counts as added only once its ping is acknowledged
      (connect `ip_remote_multi_client.go`).
5. **The dial:** `DialContext` → the tun (connect `tun.go:1412`). DNS is DoH through the
   tunnel first, then the TCP SYN through the first exit.

### 3.2 Traces of single setups [M]
Recent runs were mapped to their proxy and window clients through Main: the test
account's `network_client` rows, window clients with `source_client_id`, contract ids that
encode milliseconds, and exits' return contracts. This covered 3,350 of 3,354 runs from 11-16
01:29Z to 11-18 02:03Z, with full per-exit timelines for 100 runs.

**Fast setups:** the device exists by +0.2–0.4 s, and 4–5 mints land in the same second.
The first exit answers at +0.5–1.0 s, and the probe succeeds ~1.0–1.4 s later.

**Slow setups**, seconds after the proxy's create time:

| Run | Setup | Device by | First mint (same second) | First data back | Success | What happened |
|---|---|---|---|---|---|---|
| us, fireside g6 | 25.5 s, 1 attempt | +3.3 | +6.4 (2) | +7.0 | +25.4 | one of two exits never answered; 3rd mint at +15.9 |
| mh, crisp g2 | 23.5 s | +2.5 | +7.1 (4) | +17.8 | +23.4 | 3 of 4 exits never sent data back |
| do, fireside g3 | 35.9 s, 2 attempts | +0.6 | +31.4 (1) | +32.4 | +35.8 | no exit at all for 31 s; attempt 1 used up its budget |
| bh, fireside g4 | 35.0 s | +0.3 | +22.9 (1) | +30.4 | +35.0 | first exit dead; second wave at +29.8 |
| vn, crisp g10 | 15.2 s | +0.3 | +0.3 (5) | +1.3 | +15.2 | 4 exits answered within 2.4 s; the flow still lagged 13.8 s |

**Counting refusal waits per run [M]:** a mint gap of 2.3–45 s, or a first mint ≥1.5 s after
create, counts as a wait. Last 24 h:

| Setup | Runs | 0 waits | 1 | 2 | 3 | 4+ |
|---|---|---|---|---|---|---|
| fast (<3 s) | 561 | 96% | 4% | 0 | 0 | 0 |
| mid (3–15 s) | 658 | 50% | 44% | 6% | 0 | 0 |
| slow (≥15 s) | 156 | 15% | 26% | 34% | 19% | 6% |
| ready_timeout | 138 | 7% | 27% | 35% | 14% | 17% |

- **Share of runs over 15 s by number of waits:** 3% with none, 11% with one, 58% with two,
  94% with three.
- **Runs with no wait** (65% of all runs): p50 2.61 s and p90 6.66 s. Before the regression
  the figures were 2.37 s and 5.46 s.

**By time of first mint [M]** (11-17 → 11-18):

| First mint | Setup p50 | Setup p90 |
|---|---|---|
| under 1 s | 2.9 s | 9.3 s |
| 2.5–6 s | 8.3 s | 20.2 s |
| 6–16 s | 15.3 s | 25.5 s |
| 16 s or later | 34.8 s | — |

### 3.3 The mint admission and connect's backoff [C][M]
- **The admission:** `defaultHostedMintConcurrency = 2` per proxy process
  (`proxy/proxy_device.go:55`).
  - It was 4 before c2040f152 (10-22), which lowered it to stop a churn loop.
  - Past the bound, a mint is refused at once with "429 Too Many Requests"
    (`localclient/authority.go:248`, `localclient/authority_mint_admission.go:72-78`).
- **Connect's response:** it classifies the 429 as a rate limit
  (`ip_remote_multi_client_outcome.go:104-113`) and backs off.
  - The backoff starts at 5 s and doubles up to 2 min, jittered to [d/2, d) (`:361-399`;
    defaults `ip_remote_multi_client.go:410-411`).
  - The streak belongs to the window, and only a successful mint resets it.
  - A pass that admits nothing also defers the next pass by 5–10 s, doubling
    (`ip_remote_multi_client_discovery_retry.go:46-98`).
- **Local experiment** (production connect, hosted-like settings, fake time). Time to the
  first live exit by refused mints:

  | refused mints | first live exit |
  |---|---|
  | 0 | 10 ms |
  | 1 | 3.2–4.6 s |
  | 2 | 9.4–12.5 s |
  | 3 | 24.3–30.9 s |
  | 4 | 49 to over 60 s |

  The results are identical at D39's connect (2e9e9d88), so no connect change moved these
  timers.
- **Fleet refusals [M]:**
  - 0.1–0.6/s before 11-14;
  - 43/s at 16Z on 11-14, 305/s at 20Z and ~800/s at 22Z (1-minute windows peaked near
    1,100/s);
  - 50–420/s since.
- **Correlation:** hourly Spearman ρ against setup p90 is 0.88 over 119 h.
- **Dose-response** (3,350 runs, by fleet refusals at proxy creation):
  - under 25/s: p90 8.6 s, 8% ready_timeout;
  - 75–150/s: p90 15.3 s;
  - 150–300/s: p90 25.5 s;
  - 300/s or more: p90 34.5 s, 23% ready_timeout.

### 3.4 Smaller contributors [M][C]
- **Exits that never answer.**
  - 34% of the 396 window clients in traced runs never got data back from their exit.
    Most of these are concentrated in a few provider networks, which fail 50–100% of the
    time.
  - They decide a run when the first wave is thin. After a fast first contract, a 1-exit
    first wave has p90 15.9 s against 8.1 s for a 5-exit wave.
  - In 8 of 12 traced ready_timeouts, mostly small countries, no exit ever sent data back.
  - Today's cool-off for a dead exit is per window, not fleet-wide.
- **The flow lags after an exit is ready** in some runs (13–46 s in vn, cd, us and iq).
  - A native first SYN offered to an empty window is refused at once, and it isn't
    re-offered when the first exit is published. The flow then waits for the tun's next SYN
    retransmit (connect native ingress).
  - The first tunnel flow is usually the DoH lookup. The hosted tun has no upgrade mux, so
    connect's 4 s warm-attempt cap doesn't apply, and a stalled first exit can hold DNS for
    the whole budget.
  - These two can't be told apart from the DB.
- **30 s evaluation timeouts:** a first batch that never answers runs out the budget, and
  the client's second CONNECT reuses the device, which has more exits by then.

## 4. Why it started on 11-14: load times a memory cliff

### 4.1 Load: sweeps and reopens [M]
- **11-13 19–22Z, a sign-up wave:** networks created per 40 min went from 290–400 to 3,226.
  These were seed-phrase networks, each with the free plan's 5 hosted proxies. The D39
  proxy opened a device for each proxy at creation and absorbed the wave with setup p90 at
  4.8–6.3 s.
- **From 11-14 13:51Z, recurring sweeps:** they open thousands of existing proxies at once.
  Since D47p (5bca96398) a device opens only on first use. Since D49p (9de48db46) a device
  that carried no traffic closes after 10 minutes. So a sweep that touches a proxy every
  pass now rebuilds its device every pass. The test
  `TestProxyDeviceWalkWithoutTrafficOpensOneDevicePerPass` pins that behaviour.
- **Every rebuild starts from nothing:**
  - 2 DB transactions;
  - ~6 window-client mints;
  - at least 2 find-providers2 calls.

  Connect can restore window identities at reopen, but their snapshot expires 10 minutes
  after the window's last change (`proxyWindowIdentityTtl`,
  `model/network_client_proxy_window_model.go:32`).
- **Volume** (per second, fleet) [M]:

  | Period | Live devices | Builds | find-providers2 | Mints | Refused | Refill removals |
  |---|---|---|---|---|---|---|
  | 11-12/13 (D39) | 13.5–14.6k | 2.4–3.4 | 2–4 | 12–18 | 0.1–0.4 | 9–13 |
  | 11-13 20Z (sign-up wave) | 21–38k | 5.1 | 38 | 48 | 1.6 | 33 |
  | 11-15 | ~40k | 8.7–13 | 119–163 | 148–198 | 124–362 | 116–156 |
  | 11-16 → 11-18 | 50–58k | 10.5–14.9 | 107–185 | 155–218 | 47–194 | 98–137 |

  - About 40% of mints come from builds and 60% from refills.
  - Refills make up ~80% of find-providers2 calls: a refill lists for 1–2 candidates, while
    a build lists once for ~6.
- **Who is sweeping isn't measured yet.** The bot vs customer split by network is still
  open. Customer proxies follow the same cycle, so any pause over 45 minutes, or over
  10 minutes without traffic, makes the next request a full cold build.

### 4.2 The memory limit [M]
- **The setup:** 10 proxy processes per host (crisp, fireside: 24 cores each), with
  GOMAXPROCS 4, GOGC 100 and a 6500 MiB Go memory limit. The limit is per host, from the
  vault's `proxy_sizing.yml` (`proxy/host_go_memory_limit.go`, bd753e29d).
- **What happened:**
  - From 11-14 15Z, heap plus stacks reached ~6.31 GiB.
  - GC went from 0.6–1 to 10–13 cycles a minute per process.
  - Proxy CPU went from 3–5 to 16–23 cores per host.
- **The pure-CPU find-providers2 stages slowed** 30–100× (filter 0.05 → 1.7 ms, select
  0.05 → 5.7 ms). That's CPU starvation, not the DB.
- **Dose-response** (runs binned by proxy cores per host), setup p90:
  - under 4 cores: 5.2 s;
  - 16–19: 12.6 s;
  - 19–21: 19.5 s;
  - over 22.5: 29.5 s.
- **What a mint costs:** at the DB a mint is cheap (its INSERT averages 0.22 ms), but its
  wall time is p50 28 ms, p90 127 ms and p99 1.75 s. A starved mint holds its slot longer,
  and the 2-slot admission refuses the rest.

### 4.3 crisp against fireside: the natural experiment [M]
P1115 (bd753e29d) raised crisp to 8000 MiB on 11-15. Fireside stayed at 6500 MiB because it
has 93.7 GiB of RAM, with 18.1 GiB available at its daily minimum; crisp has 125 GiB, with
35.6 GiB available. Both hosts use the same build, the same Main and the same PgBouncer.

| 11-16 06Z | crisp (8000 MiB) | fireside (6500 MiB) |
|---|---|---|
| Window admissions/s | 75 | 97 |
| Mint refusals/s | 6.8 | 96.8 |
| Proxy DB pool wait | 0.5 ms | 15.7 ms |
| Contract creation | 38 ms | 291 ms |
| CPU (share of host) | 0.56 | 0.93 |
| GC per process-minute | 3.3–5.7 | 7.9–10.7 |

- **Setup by host** for runs attributed through Main (11-16 01:30Z → 11-18):
  - crisp: p50 2.9 s, p90 9.5 s (n=274);
  - fireside: p50 4.4 s, p90 20.2 s (n=317).
  - A 3,277-run split agrees: 9.8 s against 18.3 s. Fireside is slower on all 10 blocks
    and in every 6–12 h period.
- **Crisp's own rise:** its p90 rose on 11-17 (7–10 → 12.5 s) as heap plus stacks reached
  7.4 GiB of its 8000 MiB, with GC at 7 a minute. Crisp is running out of headroom too.

### 4.4 What saturation does downstream [M]
- **find-providers2 slows:** hosted calls went from 3.5–15k/h (11-10 → 11-13) to
  370–730k/h, and the mean cost per call from ~32 ms to 300–540 ms.
- **The online check fails open:** each candidate's liveness lease read has 150 ms, and
  after one shard error the shard reads "unknown" for 5 s in the whole process. Unknown
  candidates are admitted.
  - The unknown share went from ~1% (11-12/13) to 29% at 11-14 14Z, 59% at 16Z and 94% at
    21Z, and has been 33–81% since. Lease read errors went from 16–52/h to 5–17k/h.
  - Hourly setup p90 by unknown share: 5.0 s under 5%, 11.9 s at 20–50%, 18.5 s at
    50–80%, 26.0 s at 80% and over (r 0.82).
  - So under load, windows admit more dead exits, which feeds §3.4.
- **The proxy's DB pool:** mean acquire wait went from 1–3 µs to 4–45 ms, and acquires from
  ~900/s to 4–7k/s. Connections are held longer by descheduled goroutines; Main itself is
  not the bottleneck.

## 5. Ruled out [M][C]

- **Builds:**
  - No proxy, connect or sdk commit from 11-14 adds the seconds by itself.
  - The CONNECT path (`connectDial`, `WaitForRoute`) is unchanged since 11-08 (bed1489be).
  - Connect's window timers are identical at D39 and now.
  - D52p's blocks weren't slower during its roll: p90 7.6 s, against 19.0 s on blocks not
    yet rolled.
  - D47p's lazy open and D49p's 10-minute close did change the *shape* of sweep load
    (§4.1). That makes them part of the fix, not the trigger.
- **The stuck-provider recycle:**
  - It wasn't on anywhere until 11-15 22:00Z, 31 h after the regression, and switching it
    on across all blocks (11-16 14:27Z) moved nothing. ρ 0.11.
  - Its own counter rise was an artifact of how it's read.
  - It does recycle one large fleet's exits every ~73 min (6–7 s outage each), but those
    exits aren't over-represented in slow first exits: 20% of slow runs against 30% of
    fast.
- **The key-event storm:**
  - It started with the device ramp, and its fix (11-15 18:16–20:46Z) brought no setup
    step. ρ 0.15.
  - Hosted windows keep their keys local, so no key event gates a first contract or flow.
  - It added load: each resync runs a PostgreSQL session check.
  - Hosted churn plausibly fed it: each device build registers a network session, and
    each window-client resident takes a session listener.
- **The US pool:**
  - The number of US providers passing all filters fell from 30–41k to ~21k on 11-12, when
    the probe's HTTP/3 legs began counting. US setup p90 stayed at 2.1–2.3 s on 11-12/13.
  - 22,244 pass today, and the fleets carrying the most hosted traffic don't predict
    setup.
- **Main and PgBouncer:**
  - Both hosts use them, and crisp is fine.
  - PgBouncer max wait has ρ 0.26 and lock waits 0.35.

## 6. Fixes, ranked

The effects are estimates [I] at today's load unless marked. "All" means the all-country
setup p90, now 16.4 s; "US" means the US p90, now ~24 s on few samples.

| # | Fix | Where | Expected p90 effect | Risk | Effort |
|---|---|---|---|---|---|
| 1 | **Restore GC headroom.** Fireside is at 6500 MiB with 18 GiB available, crisp is nearing 8000. Options: raise fireside as far as its RAM allows, move blocks from fireside to crisp, add RAM, or add a third proxy host. | ops, owner | Fireside toward crisp: all ~10 s. Restarts showed what headroom does: p50 2.5–3.0 s while it lasted. | Memory pressure on fireside if it's raised too far | small to medium |
| 2 | **Don't refuse a window's first mints.** Choose one: a reserved slot for windows with no exit (pass the device's routable flag into the authority), a bounded FIFO wait of ≤1 s and ≤32 waiters instead of the instant 429, or 2 → 4 slots. | server proxy/localclient | All 16.4 → ~10–11 s; US toward ~5–7 s. Erlang model: cold first-mint demand is 0.04–0.16 Erlang, so a reserved slot refuses 4–14%, and two refuse 0.1–1.1%. | +2 concurrent mint transactions per process at peaks; bots' cold windows share the lane. The bound was cut from 4 to 2 on 10-22 to stop a churn loop, so re-check that loop. | small |
| 3 | **Retry a refused mint fast while the window has no exits** (250–500 ms jittered, a few tries), and don't count a mint-refused pass as unproductive. | connect | k refusals cost ~3.9, 11 or 27 s today; this makes them ~0.5·k s. Pair with #2. | Retry pressure on a full admission; needs #2 | small |
| 4 | **Make reopens reuse what they had.** Raise `proxyWindowIdentityTtl` from 10 min to ≥2 h, write the snapshot at close, and cache the hosted session per proxy. | server model, proxy | Mints −25–30%, find-providers2 −10–15%, 2 fewer DB transactions per reopen. A reopened proxy's first exit needs no mint. | Restored exits may be stale (that path falls back to minting); Redis grows ~1 KB per proxy | small to medium |
| 5 | **Keep the online check decisive under load.** One lease cache per process refreshed ~1 s; serve the last verdict ≤30 s old on read errors; scope the 5 s backoff to the request. | server find-providers2 | All −2 to −5 s; US −1 to −3 s | Verdicts up to 30 s stale | small |
| 6 | **Cut hosted find-providers2 volume:** singleflight plus a 2–5 s cache of ≥100 candidates per (location, rank mode, selector), drawn and excluded locally. | server find-providers2 | Call cost back toward ~30 ms; all −3 to −8 s if load is the common cause | Less diversity across windows (concentration) | medium |
| 7 | **Bound bulk reopens.** Find the sweeping networks, then cap cold device builds per network per minute, or queue them behind interactive opens. | server proxy, owner | Protects everyone else from one network's sweep; size unmeasured | A cap must not hurt customers who open many profiles at once | medium |
| 8 | **Fleet-wide cool-off for exits that never send data back**, instead of per window. | server and connect | ~10% of the tail (thin first waves) and most small-country ready_timeouts | Exits flap in and out | medium |
| 9 | **Re-offer a refused first SYN** when the first exit is published, and port the 4 s DNS warm-attempt cap to the hosted tun's DoH. | connect | From ≤1 s on a subset to 1–7 s in the 3–15 s band (the investigations differ); measure with #10 first | Low | small |
| 10 | **Measure it** (§8). | server, runner | none by itself; makes each fix verifiable and the customer tail visible | none | small |

**Not recommended now:**
- Racing the first flow across exits: it already races every exit; the slow case has no
  exit yet.
- Shorter ping or expand timeouts: they make more false negatives and more mints.
- A larger initial fan-out: every candidate is a mint, so it makes saturation worse.
- Pre-minting at auth-client: ~6 rows per proxy, and most proxies are never reused.
- Longer idle holds: they add memory, which feeds GC.

**Order:**
1. Do #1 and #10 first. #1 is the cause; #10 proves each step.
2. Then #2 with #3 and #4. These are the code fixes that make setup robust to load rather
   than dependent on headroom.
3. Then #5 and #6.
4. #7 is a policy choice.

## 7. The customer's use case: multi-account browsing through US exits

- **Where US stands:** the US p90 was ~2.3 s before 11-14 and ~24 s since, on 58 runs. The
  cause is fleet-wide and the US pool isn't short, so expanding the pool alone wouldn't
  bring the p90 down. The fixes in §6 apply to every country.
- **Cold opens:** a profile's first use after a pause is a cold build. That's a pause of
  more than 45 minutes after traffic, more than 10 minutes without traffic, or any first
  use. A multi-account browser that opens profiles now and then meets that path often.
  - #4 removes most of a reopen's cost.
  - Until then, `POST /warmup` on the proxy API (`proxy/proxy_api.go:205-266`) opens a
    device ahead of use and waits up to `timeout_seconds` for it to be ready. The device
    then holds 10 minutes without traffic.
  - Calling it a few seconds before launching a profile moves the wait off the browser's
    first request. Confirm it's in the public API docs before offering it.
- **Exit-country accuracy (their opsec concern):**
  - **Selection never leaves a pinned country.** The location fallback climbs only
    city → region → country, and no traced window held an exit published outside its pin.
  - **US mismatches** were 4 of 201 runs (2%): 3 to Saudi Arabia and 1 to Canada.
    - All three Saudi runs went through one provider network whose exit IPs rotate. Its
      published location came from a ping-derived estimate with a 22,296 km residual.
    - The Canadian one was a stable Virginia address that Cloudflare places in Canada.
  - **Across all countries** 15.7% of runs mismatch. 34 countries mismatch at 50% or more,
    mostly through one network whose rare-country providers really leave from Vietnam,
    the US or Germany, and through stable addresses that GeoLite2 and Cloudflare place
    differently. Without those countries the rate is 1.9–5.6% a day.
  - **What's missing:**
    - The country gate, which excluded a provider whose fresh probe saw another country,
      has been computed but never read since d479eccdf (09-27).
    - The prober's "suspect" verdict and its exit-IP stability feed only the unique-exit
      feature, not selection.
    - A provider's location changes only when it reconnects.
  - **Fixes:**
    - restore the country gate, and keep suspect and rotating-IP exits out of pinned
      windows (removes 3 of the 4 US mismatches; US pool −3 to −15%);
    - have the prober read Cloudflare's `loc=` through the provider and exclude or
      republish an exit on disagreement;
    - a `strict_location` option for pins that answers `no_supply` rather than a
      wrong-country exit.
- **One session, several exits:** a hosted proxy's window spreads flows over up to 6 exits
  (5 quality + 1 speed). A browser profile's requests can therefore leave from different
  addresses, which matters for multi-account browsing beyond setup time.
  - The unique-exit option (`proxy_config.unique_exit`, Pro, see
    `HOSTED-PROXY-UNIQUE-EXIT.md`) pins one exit per proxy.
  - Its setup then depends on that one exit answering, so measure it before recommending
    it for this use.
- **ru and cn:**
  - Online providers: RU 459–573, down from 773–1,110 before 11-13; CN 58–171.
  - Passing every filter: RU 13–37 since the probe's HTTP/3 legs began counting (many probe
    sites are blocked from Russia); CN 61–98.
  - Setup p90 since 11-15: RU 7–20 s, CN 8.7–24 s.
  - Exit mismatch: RU 20%, mostly to Finland; CN 11%, mostly to Hong Kong.
  - The customer's caution about those countries is warranted by the data.

## 8. Measurement changes

**Server:**
- **Histograms** per host and block, split cold build / reopen / warm:
  - the `WaitForRoute` wait;
  - the first dial of each new device;
  - time to first added exit.

  No server metric measures a customer's first-CONNECT time today.
- **Per-host series:**
  - mint refusals against admissions, with an alert when refusals pass ~5/s per host;
  - GC cycles per minute;
  - heap plus stacks against the Go memory limit.
- **Track:** CONNECTs the client abandoned. They rose from 5–20/s to 100–720/s since 11-16,
  which suggests users giving up.

**Runner** (`tests/proxy-test`) and the dashboards:
- **Log the proxy host suffix and port** from the proxy URL (the token label stays secret),
  so every roll and host difference becomes visible.
- **Weight the US:** give it 1 in 4 schedule slots (~400 runs a day), or add a probe-only
  US lane every 3–4 minutes. Probe-only means no Chrome, so it costs almost no data. Label
  the lane so the round-robin history stays comparable.
- **Use a count-based window and export n.** Hide a percentile while n < 50.
- **Count ready_timeouts:** as 60 s in the percentile, or export the share ready within
  5 s and 10 s. The customer's shared dashboard should show the same.

## 9. Decisions for the owner

1. **Proxy-host headroom (#1):** raise fireside's limit as far as its RAM allows, move
   blocks to crisp, add RAM, or add a host? Crisp also needs headroom soon.
2. **Code fixes #2 + #3 + #4, then #5/#6:** implement, test, and roll to the proxy with
   the usual canary per block (validator-idle gate, 30 min canary, g6 as the WireGuard
   canary).
3. **Bulk reopens (#7):** cap per network, queue behind interactive opens, or leave as is?
4. **Pinned-country accuracy:** restore the country gate now, and should
   `strict_location` be the default for pins or opt-in?
5. **What to tell the customer now:** the regression and its cause, the warm-up option,
   and the plan, without dates.
6. **Recycles** (from the separate investigation): add a repeat backoff before the
   stuck-provider bake-in merges. 64% of recycles hit the same providers every ~73 min,
   and the bake-in removes the runtime off switch.

## 10. Validating a fix

- **Compare within the same window:** crisp against fireside, blocks rolled against not
  yet rolled, and runs with refusal waits against runs without (§3.2).
- **Targets, back to the pre-11-14 profile:**
  - all-country setup p90 ≤ 7 s and US ≤ 3 s, on ≥100 samples each;
  - refusals under 5/s per host;
  - proxy CPU under 12 cores per host;
  - GC under 3 cycles a minute per process;
  - liveness "unknown" share under 5%.
- **Re-measure after a sweep burst,** because restarts give an hour of false relief.

## Appendix A: builds and events

| Proxy build | Live | Server | Connect | sdk |
|---|---|---|---|---|
| D39 2026.11.11-…-1096461840 | 11-12 ~04:41Z → 11-14 | 89be8b3b8 | 2e9e9d88 | 089b7c3a |
| D47p …-1098378410 | 11-14 10:44 → 13:18Z (g6 canary 09:44, reverted) | aa7142599 | 79c35f3a | 066d31b0 |
| D49p …-1098770070 | 11-14 18:33–18:39Z | f43488da1 | fc15cc59 | d9ad8a26 |
| P1115 …-1099720290 | 11-15 20:31 → 22:56Z; crisp 8000 MiB from 21:39Z | ac8e90b89 | 86266e7a | aefdc519 |
| D52p …-1100763350 | 11-17 06:06 → 12:43Z (canary 03:48–04:22Z) | ab831632f | 0e81ab7b | 269f65fd |
| nightly …-1101053110 | all 20 at once, 11-17 14:04–14:07Z | 1accf89b5 | f04d9a22 | 7df152e3 |

Other events:
- **11-14:**
  - connect D47 07:10–07:40Z;
  - D48 api 14:00 → 14:26Z, and alt up 13:50–13:53Z;
  - the key-event storm from 13:51–13:55Z.
- **11-15:**
  - all services restarted with config 1099044420 at 01:11–01:14Z;
  - D51kes connect 18:16 → 20:46Z, which cleared the storm;
  - stuck-provider recycle on beta 22:00Z.
- **11-16:**
  - recycle on g1 13:43Z and g2–g4 14:27Z;
  - legacy events table dropped 01:14Z;
  - PG 18.7 at 02:02Z.
- **11-18:** D55 api, alt and taskworker 23:18Z (11-17) → 00:28Z.

## Appendix B: code references

| What | Where |
|---|---|
| Mint admission bound | `proxy/proxy_device.go:55` (`defaultHostedMintConcurrency = 2`); refusal `localclient/authority_mint_admission.go:72-78` |
| Contract admission | `proxy/proxy_device.go:67` (256 per process) |
| Device open and wait | `proxy/server.go:714-784`; `proxy_device.go:496-613`, `:1396-1562`, `:2197-2223` |
| Idle closes | `proxy/proxy_device.go:1210-1211` (45 min after traffic, 10 min unused) |
| Window identity snapshot | `model/network_client_proxy_window_model.go:32` (`proxyWindowIdentityTtl = 10m`) |
| Warm-up API | `proxy/proxy_api.go:172, 205-266` (`POST /warmup`) |
| Per-host memory limit | `proxy/host_go_memory_limit.go` (vault `proxy_sizing.yml`, bd753e29d) |
| Connect rate-limit backoff | connect `ip_remote_multi_client_outcome.go:104-122, 361-418`; defaults `ip_remote_multi_client.go:410-411` |
| Unproductive-pass deferral | connect `ip_remote_multi_client_discovery_retry.go:46-98` |
| Window timers | connect `ip_remote_multi_client.go:347-348` (setup/ping 30 s), `:400` (expand 15 s), `:434` (generator 20 s) |

## Appendix C: data and method

- **Runner:** planetoid's `proxy-test` journal, 19,000 runs from 11-07 00:19Z to
  11-18 02:03Z, plus its Mimir series (`urnetwork_proxy_test_*`).
- **Run → proxy → exits:**
  - the test account's `network_client` rows; window clients have description
    `resident proxy` and `source_client_id`;
  - `proxy_client.block`;
  - `contract_origin_linger`, `contract_close`, and exits' return contracts (by
    destination, with a contract-id floor).

  Hosted devices log nothing per device, so traces come from rows, not logs. Rows are
  reaped 2 days after a proxy's removal, so pre-11-16 runs can't be traced.
- **Mimir series:**
  - mint admissions and refusals;
  - window expand outcomes;
  - find-providers2 stages, liveness outcomes and lease reads;
  - proxy Go memory, GC and CPU;
  - the proxy DB pool;
  - build_info.
- **Loki:** `[pd]using api` lines for device constructions.
- **Local experiments:** connect at production and D39 commits, fake time, hosted-like
  settings.
- **Database load:**
  - about 700 indexed read-only SELECTs, most under 2 s;
  - three exploratory queries ran long (18 s, 30 s and 140 s) and were cancelled;
  - nothing was left running.
- **Working notes** for each investigation are kept outside the repo.
