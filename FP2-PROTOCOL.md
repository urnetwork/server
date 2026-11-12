# Protocol-aware FindProviders2

Owner, 2026-11-12 (~16:00Z), rejecting the plan to let Speed and the Online
URL ratio ignore failed UDP legs:

> the split also affects how "auto" in connect/multi client works. In auto,
> any udp flow should be kept on quality not speed. I think the hard udp gate
> is correct right now. A lot of speed/streaming use cases use udp, and a
> provider not routing udp is an issue. Perhaps instead what we can do is have
> a protocol selector "all" or "tcp-only" in the find providers 2 index, and
> the ip multi client makes sure that the provider window selected can
> actually route the traffic. For example a window that is tcp-only is not
> eligible to route a udp flow. Let's explore the update to find providers 2
> to make it more protocol aware

Status: exploration and design. Nothing here is built. References are to
server origin/main cbb7d7306, connect origin/main fc949b78 and sdk
origin/main 447ed899, all read on 2026-11-12. Paths without a repo prefix are
in this repo (server).

## Summary

- Today every bucket (Quality, Speed, Online) applies the hard UDP gate. Since
  11-11, a silent HTTP/3 leg judged the provider's fails the whole URL run.
  That lowers the one ratio that Quality, Speed and the 0.8 Online URL ratio
  all read.
- The connect multi client never reads a flow's protocol when it places it.
  In auto it sends UDP 443 (QUIC) quality-first and every other UDP flow
  speed-first.
- Proposal: keep today's buckets as the `all` view and the default. Add a
  `tcp-only` view that applies the same rules but counts a run whose only
  failure is a UDP leg as a pass. FP2 serves it only to requests that set
  `protocol: "tcp-only"`, and labels each provider it admits only through
  that view.
- Connect labels every window entry with its protocol. A `tcp-only` entry
  never carries a UDP flow. In auto, UDP flows stay on the quality window,
  which keeps asking for `all`. Only the speed window asks for `tcp-only`,
  because in auto it carries TCP alone.
- No new windows: one window per type, with a label per entry. This mirrors
  the IPv6 design (connect IPV6.md B1-B6).
- The signal needs a per-run flag that the ingest sets. The cheapest robust
  version is one boolean column on `provider_egress_health_history`, a
  metadata-only migration. There is also a no-migration fallback that reads
  the evidence jsonb; it was measured and is affordable today.
- Measured on Main over 8 hours on 11-12: the `tcp-only` view would add about
  20,400 providers to the Quality/Speed ratio population and about 20,300 to
  the Online ratio population. 89% of them never proved UDP in the window.
  Expected pools are Speed about 46k (from 26.4k) and Online about 44-45k
  (from 26.7k) for `tcp-only` requests. Old clients get exactly today's
  answers.

## 1. Today's path

### 1.1 Server: FindProviders2 and the buckets

**Route.**
- `POST /network/find-providers2` (api/api.go:224) goes through
  `NetworkFindProviders2WithOwners`
  (api/handlers/network_client_location_handlers.go:57) and
  `router.WrapWithInputNoAuth` to `model.FindProviders2`
  (model/network_client_location_model.go:6335).
- The body is decoded with `json.Unmarshal` (router/handler_utils.go:287), so
  a server ignores request fields it does not know.
- Hosted devices (the proxy service) discover in process through localclient
  (localclient/authority.go:434), which serves the same function.

**Request and answer.**
- `FindProviders2Args` (model/network_client_location_model.go:5081) carries:
  - specs and count;
  - exclusions;
  - `rank_mode`: quality (the default), speed or online (:5066-5079);
  - `force_minimum`, `ip_family`, `post_quantum_encryption`,
    `require_modern_provider` and `strict`.
- Each answered provider is a `FindProvidersProvider` (:5131), built from the
  cached score by `findProvidersProviderFromClientScore` (:6301). It carries:
  - client id, tier and estimated bytes per second;
  - `network_only` and reputation names;
  - location;
  - `ip_family`, the precedent for a per-provider capability label;
  - `match`.

**The bucket rule.** It is decided once per provider per score export, and
FP2 reads the result from the cache.
- `egressFacts` (model/egress_index.go:827) reads two things:
  - the provider's accepted URL outcomes in the 8-hour window
    (`ProviderEgressHealthCounts`, model/provider_egress_health_model.go:643);
  - the general catalog's half of them for the Online rule (`onlineUrlIndex`,
    model/egress_index.go:883).
- `decideProviderEgress` (model/egress_index.go:894) applies these rules in
  order:
  - The common gates (reliability, ARIN risk, TLS, zero-carry; :895-921)
    remove a provider from everything.
  - `no_success` (:925) and, with the Online URL ratio on, `low_success`
    (:933) remove it from every bucket, Online included.
  - A provider with no evidence goes to Online only (:942).
  - "A measured failing ratio belongs to online, not quality or speed"
    (:952-959).
  - A passing ratio admits the provider to Quality and Speed. Quality needs
    at least 3 runs spanning at least 30 minutes; Speed needs 2 runs
    (model/egress_native_min_runs.go:29, 40, 46, 51-62). Quality also needs
    the provider not to be ARIN non-quality.
- The ratio is one verdict, `ComputeEgressIndex` (model/egress_index.go:681):
  4·total ≤ 5·ok over the accepted URL outcomes of 8 hours. Quality and Speed
  share it. The ranking index and the draw weight (0.1 + 0.9·ok/total,
  :699-705) come from the same counts.
- The Online rule:
  - Functions: `providerCarriesNoUrl` and `providerBelowOnlineUrlRatio`
    (model/egress_online_success.go:87-100).
  - Country guard: `urlRatioAppliesIn` and `belowUrlRatioIn` (:174-190) apply
    it under the guard, which `newProviderOnlineSuccessBaseline` builds
    (:269-313).
  - Config: provider.yml sets `online_url_success_ratio: 0.8`.
- The counts:
  - window rows: model/provider_egress_health_model.go:680-703;
  - aggregate: :730-736;
  - loader: :753-802.

**The score export.**
- `UpdateClientScores` (model/network_client_location_model.go:5417) runs
  again 30 s after the previous run ends
  (taskworker/work/network_client_location_work.go:20-31). That is about 15
  runs an hour (model/network_client_score_rank.go:140-150), with a 300-minute
  cache TTL (taskworker/work/network_client_location_work.go:37).
- It loads the 8-hour counts (model/network_client_location_model.go:5667) and
  decides every provider (:5748-5765).
- `rankClientScore` (model/network_client_score_rank.go:174) sets on each
  score:
  - `PassesMinimums` per mode (:202);
  - `Online` (:215);
  - `UrlProbeSuccessWeight` (:219);
  - `ScaledWeights` (:233).
- `exportClientScores` (model/network_client_score_targets.go:298) writes,
  for every target, caller location and mode:
  - the union sample (natives plus online members);
  - one page set per family facet (`d`, `4`, `6`;
    model/network_client_ip_family.go:94-144);
  - the native facet pages.
- The facet is part of the key (`{cs_<force>_<mode>_<caller>_<target>}...`,
  model/network_client_location_model.go:5282-5384) and of the native
  manifest (model/network_client_score_native.go:65-100).
- A verdict vouches for its evidence until the earlier of two times
  (model/network_client_score_rank.go:151-167):
  - its newest run leaving the window;
  - 30 minutes past the window the export read.

**A request.**
1. `FindProviders2` maps `ip_family` to facets
   (model/network_client_location_model.go:6351;
   model/network_client_ip_family.go:129).
2. It loads the requested mode's pool (:6473-6498). Online reads the quality
   union pool (:6098-6122). Ordinary requests read facet pages in priority
   order, and online in one shuffled order (`loadClientScoresInFacetOrder`,
   :6124).
3. It keeps the natives: `PassesMinimums[mode]`, or `Online` for an online
   request (:7103-7114).
4. It draws by tier bands.
5. A quality or speed request then borrows in two steps:
   - first the other mode's natives, at tier +11 (:7131-7213);
   - then the Online union, at tier +22 (:7214-7281).

Two kinds of request skip parts of this:
- Named providers (client-id specs) pass only the common gates.
- `force_minimum` keeps only the common gates. Only the standalone egress
  prober sends it (qualityprobe/cmd/egress-prober/main.go:1100); connect and
  the sdk never do.

**Where UDP enters.**
- Since 2026-11-11 a URL marked `h3: true` is loaded over HTTPS, then over
  HTTP/3 (qualityprobe/H3.md, IPFAMILY.md).
- A silent HTTP/3 leg judged the provider's makes the run's `ok_count` 0. So
  every bucket admits only providers not shown to fail UDP: that is the hard
  UDP gate.
- The rejected split would have relaxed Speed and the Online URL ratio. Auto
  sends every non-443 UDP flow to the speed window first
  (connect/ip_remote_multi_client.go:3571-3574), so those flows would have
  landed on providers that do not carry UDP.

### 1.2 Connect and sdk: windows, auto and placement

**Windows.**
- A multi client has two windows, quality and speed
  (connect/ip_remote_multi_client.go:3025-3085). A fixed destination keeps
  only quality.
- Each window entry is a window client: a full client with its own transport
  graph and contracts to one provider.
- Sizes:

  | Platform | Quality | Speed |
  |---|---|---|
  | Desktop defaults (:217-247) | min, max and hard max 6, soft v6-capable minimum 1 | min 1, max 2, hard max 4, fixed 1 |
  | Mobile (sdk/mobile_memory_policy.go:237-238, 544-558) | 4 | 1 |

- `WindowType.RankMode()` (connect/ip_remote_multi_client.go:101) is the
  request's `rank_mode`. Auto is not a rank mode on the wire: it decides only
  which window a flow uses.

**Refill.**
- Each window's `resize` loop (:13895) holds the window at target, retires
  warned entries and calls `expand` (:14845).
- Candidates come from `enumerateDestinations` and `discoverDestinations`
  (connect/ip_remote_multi_client_family.go:447-560). Each asks the generator
  for `WindowExpandBlockCount` (4) candidates
  (connect/ip_remote_multi_client.go:313):
  - first a `v6-capable` request sized to an open v6 shortfall;
  - then the main `v4-capable` fill (IPV6.md B1-B2).
- The API generator turns each into one FP2 request
  (connect/ip_remote_multi_client_api.go:876-895). It maps every answered
  provider to `DestinationStats` (:1000-1023;
  connect/ip_remote_multi_client.go:72-96). That struct carries `IpFamily`;
  nothing carries a protocol.

**Auto.** `selectWindowTypes` (connect/ip_remote_multi_client.go:3552-3576)
decides which windows a flow may use:

| Case | Windows, in order |
|---|---|
| Fixed destination | quality |
| Fixed profile | the profile's window |
| Auto, destination port 443 (TCP or UDP, so QUIC too) or a streaming classification | quality, then speed |
| Auto, everything else | speed, then quality |

The sdk maps "auto" to `WindowTypeAuto` (sdk/device_local.go:9011-9031) and
defaults to it (:9077-9078).

**Placement.**
- For each packet group, `coalesceOrderedClients`
  (connect/ip_remote_multi_client.go:8577-8691) walks the selected windows in
  order. It takes `raceCandidates(window, ipVersion)` (:4094), filtered by
  `supportsIpVersion` (connect/ip_remote_multi_client_family.go:58).
- The family predicate guards about twenty placement sites:
  - affinity donors (connect/ip_remote_multi_client.go:4698, 4790, 5487,
    5595);
  - SYN affinity and retry
    (connect/ip_remote_multi_client_affinity_syn.go:72,
    connect/ip_remote_multi_client_syn_retry_publication.go:249);
  - discovery demand
    (connect/ip_remote_multi_client_discovery_demand.go:439);
  - DNS affinity (connect/ip_remote_multi_client_dns_affinity.go:172);
  - service refresh
    (connect/ip_remote_multi_client_service_refresh.go:65, 87);
  - the prober (connect/ip_remote_multi_client_prober.go:392);
  - flow adoption (connect/ip_remote_multi_client.go:6477, 10141);
  - the ordered lists (:16311, 16455, 16478).
- QUIC rebind on exit loss gathers candidates from every window, so it
  crosses windows (`rebindCandidates`, :6222-6260).
- When the windows have formed and none can carry v6, a v6 flow gets an
  ICMPv6 no-route reply (:8721-8733;
  connect/ip_remote_multi_client_family.go:605-658). This is IPV6.md B6.

**UDP versus TCP today.**
- No placement site reads the protocol. A UDP flow may use any entry of the
  window it is offered.
- UDP 443 (QUIC) goes quality-first. QUIC is "56% of traffic"
  (connect/ip_remote_multi_client.go:1539).
- Every other UDP flow goes speed-first: games, VoIP, STUN and WebRTC, a
  WireGuard or other VPN inside the tunnel, and any DNS that is not
  intercepted.
- The apps intercept DNS on UDP/TCP 53 and resolve it over DoH through the
  tunnel (connect/ip_mux_upgrade.go:3-7, 167-180), so app DNS is TCP 443. A
  host without the mux passes DNS through as UDP 53.
- An established QUIC flow re-pins to another exit when its exit dies
  (`QuicRebindOnExitLoss`, connect/ip_remote_multi_client.go:1531-1545).
- A torn-down UDP flow gets an ICMP port unreachable (`UdpTeardownSignal`,
  :1524-1529; `ipOosUnreachable`, connect/ip_packet.go:157).

## 2. The capability signal

**What a run records.**
- `url_probe_evidence.h3.udp` is one of four values
  (qualityprobe/egresshealth/url_h3.go:74-105, 125-140):

  | Value | Meaning |
  |---|---|
  | `passed` | the leg met the URL contract |
  | `answered` | the site answered over QUIC, so UDP is proven; the run passes |
  | `silent` | the only evidence against the provider |
  | `not_measured` | the whole load is not measured, so it never counts |

- The ingest judges a silent leg by place before storing it
  (`h3.verdict`; model/provider_egress_h3_place.go:149-197; with IPv6 legs,
  model/provider_egress_ipv6_place.go:150-257):
  - `place` stores the run as a pass (`ok_count` 1).
  - `provider` stores it as a failure (`ok_count` 0, `failure_stage: h3`).
- The IPv6 legs record the following
  (qualityprobe/egresshealth/url_evidence.go:180-201, 500-587):
  - `ipv6.tcp`: passed, failed or not_measured;
  - `ipv6.verdict`;
  - `ipv6.h3`, shaped like `h3`;
  - `failure_stage: ipv6` when the IPv4 legs passed and an IPv6 leg failed.
- `udp_probes_disabled: true` means no UDP leg was made in any family. That
  is the case for exits in cn, ir, ru and tm.
- No scalar column records which leg failed. A history row stores `ok_count`,
  `total_count`, `class_results` and `url_probe_evidence`, among others
  (model/provider_egress_health_model.go:401-408).

**A UDP-leg-only failure** is a measured URL run (`total_count` 1) that meets
all of these:
- `ok_count` is 0 after the place judgement;
- every leg judged the provider's is an HTTP/3 leg: the IPv4 HTTP/3 leg is
  `silent` with verdict `provider`, or the IPv6 HTTP/3 leg is, after a passed
  IPv6 HTTPS leg;
- no IPv6 HTTPS leg failed with verdict `provider`.

The IPv4 HTTPS leg passed by construction: the later legs are only made after
it passes. Every other failed run stays a failure in both views. How the
other cases map:
- Answered legs and place verdicts already pass in the `all` view.
- Not-measured legs never count.
- In countries with `udp_probes: false` no run is a UDP-leg-only failure, so
  those countries are identical in both views. Their providers stay `all`
  without UDP proof, exactly as today.

**Storage, recommended: a flag the ingest sets.**

```sql
SET LOCAL lock_timeout = '5s';
ALTER TABLE provider_egress_health_history
    ADD COLUMN IF NOT EXISTS udp_only_failure boolean NOT NULL DEFAULT false;
```

- **The migration.**
  - On PostgreSQL 18, adding a column with a constant default is a catalog
    change on the parent and every daily partition. No partition is
    rewritten.
  - It holds the brief ACCESS EXCLUSIVE locks that ingest waits behind. The
    5-second bound applies, as in other migrations here.
  - It is appended after main's head when it lands. Main is at 963, and
    §8.6's 964 is pending on another branch.
- **The store.** It sets the flag from the judged evidence, before the
  insert (model/provider_egress_health_model.go:401-408). It uses a pure
  method on `UrlProbeEvidence` that the judge tests can pin.
- **The aggregate.** It adds `COUNT(*) FILTER (WHERE udp_only_failure)`
  overall and for the general half (:730-736).
  `ProviderEgressHealthCounts` gains a `UdpOnlyFailures` count, on itself and
  on `General`.
- **Rows from before the migration** read false, which counts them as
  failures. The `tcp-only` view therefore starts equal to the `all` view and
  fills in over 8 hours. That is the safe direction.
- **No backfill.** A backfill would write about 220,000 rows of the hot
  table.

**Storage, fallback: read the jsonb, with no migration.** The same predicate
can be evaluated in the 8-hour aggregate, on failed rows only.
- Measured on Main on 11-12 (read-only queries): the 8-hour aggregate with
  that filter took 4.7-4.9 s. The plain aggregate took
  6.7 s and 14.4 s in the same quarter hour, so noise dominates.
- The evidence is stored inline and uncompressed. A 10-minute sample averaged
  1,146 bytes and peaked at 1,834; none passed the ~2 KB TOAST threshold.
- The risk is growth. Evidence with dual-stack legs crosses that threshold,
  is pglz-compressed, and from then on every read decompresses it. The column
  costs nothing at read time.
- Recommendation: the column. The jsonb read is the fallback if the owner
  wants no migration.

**Per provider, per export.**
- Each provider gets two decisions from the same facts:
  - the `all` view on `ok`;
  - the `tcp-only` view on `ok + udp_only_failures`, using the general half
    for the Online rule.
- Each view admits a set of providers per bucket, and `all ⊆ tcp-only` for
  every bucket (§3).
- An answered entry carries its view: `all` when the `all` view admits it to
  the band it was drawn from, `tcp-only` otherwise. The label is per bucket,
  but nearly always the same across buckets: the two views differ only by
  the UDP-only count.

**Freshness.** It is the same as today's verdict.
- A provider gets a URL turn about every 24 minutes, with a median of 20
  accepted runs in 8 hours. The export decides about 15 times an hour.
- **A provider that stops carrying UDP**, where its place carries QUIC, fails
  about 53% of its turns, the share of turns that draw an h3 URL. It falls
  under 4/5 once failures pass a
  fifth of the runs in its window. Failed turns are retried sooner than
  passed ones, so that is typically a few hours.
- **A provider that starts carrying UDP** returns to `all` as its silent runs
  leave the window: up to 8 hours.
- The connect-side local downgrade (§5) covers the first lag.

**Measured on Main**, read-only, over 8-hour windows ending about 15:15Z and
15:20Z on 2026-11-12:
- **Runs:**
  - 114,657 providers had accepted URL outcomes, with a median of 20 runs
    each.
  - 1,960,099 runs: 964,173 passed (49.2%), and 221,852 (11.3%) failed only
    on an HTTP/3 leg.
  - 52,396 providers had at least one UDP-leg-only failure.
- **The Quality/Speed ratio** (4/5 or better, at least 2 runs):
  - 27,386 providers pass in the `all` view.
  - The `tcp-only` view adds 20,445.
  - Of those additions, 18,257 (89%) proved no UDP in the window: no passed
    or answered IPv4 HTTP/3 leg. Another 2,143 (10%) proved UDP at least as
    often as it went silent.
  - Of the 27,386 in the `all` view, 25,876 (94%) proved UDP and 2,521 (9%)
    had at least one UDP-leg-only failure.
- **The Online URL ratio** (general outcomes; at least 0.8 or fewer than 2
  runs, before the country guard and the common gates): 30,286 providers
  pass in the `all` view, 50,543 in the `tcp-only` view.
- **Estimated pools.** The gauge at 15:30Z read Speed 26.4k and Online 26.7k.
  That is 96% of the `all`-view Quality/Speed ratio count and 88% of the
  `all`-view Online ratio count. At the same proportions, the `tcp-only`
  pools would be:
  - Speed: about 46k;
  - Online: about 44-45k;
  - before the h3 requirement (11-11 08Z), Online was ~50k and Speed ~49k.
- **Who the additions are.** The silent UDP sits in particular builds. From
  the h3 measurement of 11-12, 06:30-14:30Z:
  - the fork `2026.10.22+N-localfix3`: about 11.5k providers, all silent;
  - our July `2026.7.2+N` build: about 12.2k providers, 88% silent, probably
    held back by the updater '+' bug fixed on 11-11;
  - the forks `v2026.11.1-N-meso` and `2026.11.1-N-fix.35.3`.

  Our current builds pass UDP 96-99% of the time.

## 3. Bucket semantics per protocol

The `all` view is today's rule, unchanged; it is the default for every
request. The `tcp-only` view is the same rule with U, the window's
UDP-leg-only failures, counted as passes.

| | all (default, today) | tcp-only |
|---|---|---|
| URL ratio, Quality and Speed | 4·T ≤ 5·OK | 4·T ≤ 5·(OK + U) |
| Quality | the ratio, at least 3 runs spanning at least 30 min, not ARIN non-quality, the common gates | the same, with the tcp-only ratio |
| Speed | the ratio, at least 2 runs, the common gates | the same, with the tcp-only ratio |
| Online: no_success | out when at least 2 general runs and OKg = 0 | out when at least 2 general runs and OKg + Ug = 0 |
| Online: URL ratio 0.8 | out when at least 2 general runs, OKg > 0 and 5·OKg < 4·Tg, unless the country is exempt | the same with OKg + Ug |
| Country guard | computed from the `all` view and published as today | the same published guard |
| Ranking index and URL weight | from OK | from OK + U, for the tcp-only additions |
| Common gates, zero-carry, modern-first, TLS findings | unchanged | unchanged |

How the two views relate:
- **IPv6.** An IPv6 HTTPS failure judged the provider's fails the run in both
  views. "Every advertised family" stands; only the UDP leg is relaxed, in
  either family.
- **Containment.** For every bucket, `all ⊆ tcp-only`. A bucket's
  **tcp-only additions** are the providers the `tcp-only` view admits and the
  `all` view does not.
- **One guard.** One country guard serves both views. The guard counts
  providers with any success, and UDP barely moves it: a UDP-less provider
  still passes about 47% of its runs. A separate guard would add a published
  document for no measured gain.
- **Inspection and status.** Both views appear in provider inspection
  (`bringyourctl`, model/egress_index.go:1547-1600) and in the provider
  status reasons (model/provider_status_model.go:777).
- **Exclusion reasons.** The `all` view's reasons stay as they are
  (`urnetwork_stats_provider_excluded{reason}`). A provider that only the
  `tcp-only` view admits keeps its `health` or `low_success` reason in that
  gauge. It is counted separately (§6).

## 4. The FP2 API change

**Request: `protocol`.**
- Values:
  - `""` or `"all"`: today's answer, byte for byte.
  - `"tcp-only"`: the `tcp-only` view.
  - Anything else: 400, as for `ip_family`. A filter the server cannot
    interpret must not widen silently.
- An older server ignores the field (router/handler_utils.go:287). It answers
  the `all` view with no labels, which is correct and conservative.
- Old clients never send the field, so they get exactly today's answers.

**What `tcp-only` changes.**
- **Natives.** The requested mode's natives are drawn from both views.
- **Borrowing.** The other mode's natives and the Online fallback are both
  drawn from the `tcp-only` view.
- **Online.** `rank_mode: online` with `tcp-only` draws the Online members of
  both views.
- **Everything else is unchanged:** `ip_family`, `require_modern_provider`,
  `post_quantum_encryption`, `strict` with location fallback, exclusions and
  tiers.
- **`force_minimum`** answers keep only the common gates, as today. They read
  no URL verdict, so `protocol` changes nothing there.
- **Named providers.** They pass the common gates and are returned as today.
  They are labeled when the cache holds a score for them.

**Response.**
- Each provider gains `protocol` (string, omitempty). It is `"tcp-only"` when
  only the `tcp-only` view admitted the provider to the band it was drawn
  from. Otherwise it is absent, which means `all`.
- The result gains a top-level `protocol` echoing the view the server
  applied, absent for `all`. A client can then tell an older server, which
  ignores the request field, from a new one that found no `tcp-only`
  additions. That exactness feeds the client metrics.

**Server implementation sketch.**
- **The flag and counts:** as in §2.
- **The export.**
  - It runs `decideProviderEgress` twice per provider, once on the `tcp-only`
    facts. That is in memory and cheap next to the pass's reads.
  - It keeps both decisions, next to `clientIdEgressDecisions`
    (model/network_client_location_model.go:5741).
- **`ClientScore` gains two fields:**
  - a small bitmask of `tcp-only` admissions (quality native, speed native,
    online);
  - the additions' draw weights, computed from the tcp-only URL weight.

  Zero must mean "no tcp-only admission". A score from an older exporter, or
  read from an older cache entry, then decodes to today's behaviour, as
  `NetworkOnly` and `IpFamilies` do.
- **`exportClientScores`** (model/network_client_score_targets.go:298) writes
  the tcp-only additions into new facets of the same snapshots:
  - one new facet per family category, beside `d`, `4` and `6`;
  - in the union sample of each mode, which the Online fallback and online
    requests read;
  - in the speed native pages.

  Quality native additions are not needed in v1: no client asks for Quality
  with `tcp-only` (§5). The `ClientFilter`, and so the location stables and
  counts, stays `all`-only.
- **Old readers** never request the new facets, so their pages and answers
  are unchanged.
- **The reader.** A `tcp-only` request reads, for each requested family, the
  `all` facet and its `tcp-only` facet.
  - Within a family it reads them in one shuffled order, with no priority, as
    online requests read their facets today
    (model/network_client_location_model.go:6124).
  - Dualstack-first priority across families stays.
  - It admits a native on `PassesMinimums[mode]` or the mode's `tcp-only`
    bit, and an online member on `Online` or the online `tcp-only` bit. It
    labels the entry from the bit that admitted it.
- **Cost.** Additions are about 20k providers in Speed and Online.
  - Redis pages grow by about that share of the speed and union pages, at
    most about 75% for those pages.
  - The quality pages and the forced pools are untouched.

## 5. Connect multi client

**Per-entry capability.**
- `DestinationStats` gains `Protocol`, read from the answer. Absent means
  `all`.
- `multiClientChannel.supportsProtocol(ipProtocol)` decides what an entry may
  carry:
  - **UDP** needs `all`.
  - **TCP** may use any entry.
  - **ICMP** may use any entry: no probe measures it.
  - **A fixed destination** carries every protocol: it is the user's choice,
    with no alternative to prefer. That matches `supportsIpVersion`
    (connect/ip_remote_multi_client_family.go:53-66).
- One predicate over the path, family and protocol together, replaces
  `supportsIpVersion(path.Version)` at every placement site in §1.2. Sites
  that gather by version alone take the protocol as well. Two need explicit
  care:
  - **`rebindCandidates`** (connect/ip_remote_multi_client.go:6222) gathers
    across windows. A QUIC flow must never rebind onto a `tcp-only` entry of
    the speed window.
  - **Affinity donors** (:4698 and the v6 twin): a UDP flow must never inherit
    a `tcp-only` donor. A QUIC flow can then egress from a different address
    than its site's TCP flows when those fell back to a `tcp-only` exit, as
    already happens when an exit dies.

**Window protocol.**
- **The quality window** keeps asking for `all`, today's request. It carries
  QUIC and, in auto, every UDP flow.
- **The speed window** asks for `tcp-only` in auto. In auto it carries TCP
  alone (below). It fills from both views, labels its entries, and the
  roughly 20k additions return to its draws.
- **Fixed profiles** (`WindowType` quality or speed) keep asking for `all` in
  v1. A fixed speed window carries everything, QUIC included, on one or two
  entries, and a soft minimum of UDP-capable entries in so small a window
  buys little. See the open questions.

**Auto.** `selectWindowTypes` (connect/ip_remote_multi_client.go:3552) adds
one rule:

| Flow | Windows, in order |
|---|---|
| Any UDP flow | quality only (the owner's rule) |
| TCP 443, or a streaming classification | quality, then speed (unchanged) |
| Other TCP | speed, then quality (unchanged) |

UDP 443 loses only its fallback to speed. Other UDP moves from speed-first to
quality. The speed window is then TCP-only by construction, which is the
owner's "a window that is tcp-only is not eligible to route a udp flow". The
per-entry labels make that hold for every cross-window path as well.

**One window per type, with per-entry capability (recommended), against two
windows per protocol.**
- Two windows per type would double the window clients. Desktop would go
  from 6 + up to 2 to 12 + up to 4, and mobile from 4 + 1 to 8 + 2.
  - Each window client is a full client with its own transport graph and
    buffers. For example, on mobile the resend queue holds up to 512 KiB, and
    the receive queue up to 768 KiB per sequence without a pool
    (sdk/mobile_memory_policy.go:225, 234).
  - Each holds its own contracts: one open contract per direction to its
    provider.
  - The desktop quality window is held at six, rather than grown to twelve,
    for its memory cost (connect/ip_remote_multi_client.go:219-227). A second
    set would also double the platform's contract creation from every client.
- One window per type with a label per entry adds one atomic per entry and no
  clients or contracts. IPv6 made the same choice ("B1. No new windows").

**Fallbacks when no `all` entry exists for a UDP flow.**
- **While the windows form,** a UDP flow waits on window readiness like a v6
  flow (the formation poll), since the fill may land an `all` entry any
  moment.
- **Once the windows have formed** and the windows auto allows for the flow
  hold no UDP-capable entry, the flow is answered with an ICMP port
  unreachable (`ipOosUnreachable`, connect/ip_packet.go:157). It is counted
  (`UdpUnroutable()`, `udpNoRouteCount`), mirroring
  `Ipv6Unroutable()`/`replyIpv6NoRoute`
  (connect/ip_remote_multi_client_family.go:605-658). QUIC stacks then fall
  back to TCP at once instead of waiting out their handshake timeout.
- **Optional:** while UDP is unroutable, the DNS interceptor answers
  HTTPS/SVCB queries without the `h3` ALPN hint, so browsers start over TCP.
  This mirrors the empty AAAA of B6 (`SetIpv6Unroutable`,
  sdk/device_local.go:5611).
- **Not in v1:** a UDP flow borrowing an `all` entry of the speed window when
  the quality window has none (open question 2).
- **The quality window's own request** borrows only from the `all` view:
  Speed natives, then Online. It runs dry only where every online provider
  of the location has been shown to fail UDP.

**Local UDP downgrade**, mirroring B5 (`maybeDowngradeIpv6`,
connect/ip_remote_multi_client_family.go:68-85).
- **Trigger:** an `all` entry whose UDP flows to several destinations got no
  inbound UDP within the blackhole bound, while its TCP flows did.
- **Effect:** the entry is relabeled `tcp-only` for the life of the window.
  Its UDP flows are re-raced; QUIC flows are rebound. The quality window
  refills.
- **Why:** this covers the probe's lag (§2) and providers that change builds.
  It ships with the rest or in a follow-up, with its own counters.

**Cases.**
- **QUIC and HTTP/3** (UDP 443, 56% of traffic): quality only, `all` entries
  only. Rebinding stays within UDP-capable entries. With no `all` entry, the
  port unreachable reply sends the browser to TCP 443, which may then use the
  speed window's `tcp-only` entries.
- **DNS over UDP:**
  - The apps' interceptor resolves over DoH (TCP 443), so app DNS is
    unaffected.
  - Pass-through DNS is a UDP flow: quality, `all` only. That covers a host
    without the mux and a resolver the user set outside it.
  - Hosted proxy devices turn the mux off (proxy/proxy_device.go:1338-1340)
    and pass DNS through. A hosted user's plain DNS is therefore a UDP flow
    unless the resolver its proxy tun was built with answers it
    (proxy/proxy_device_regional_dns.go).
- **WireGuard or another VPN inside the tunnel:** a long-lived UDP flow, on
  quality and `all` entries only. Today such a flow goes speed-first.
- **Hosted WireGuard proxies** terminate WireGuard at the proxy. Its inner
  flows go through the device's multi client under the same rules.
- **Games, VoIP, STUN and WebRTC:** UDP, so quality and `all` only. They lose
  their speed-first placement, which the owner's rule intends.

**sdk.**
- Add the UDP unroutable signal to the mux, as `SetIpv6Unroutable` is
  (sdk/device_local.go:5611).
- Add `UdpAvailable` to the window status, beside `Ipv6Available`
  (sdk/device.go:429, sdk/device_local.go:5776).
- No app API change: auto stays the default.
- The Connected status stays count-based (`gridConnectionStatus`,
  sdk/connect_view_controller.go:1215).

## 6. Rollout

1. **Server**, one build after `db migrate`.
   - **Order:**
     - the migration (§2);
     - the taskworker, whose in-process ingest writes the flag and whose
       export writes both views;
     - api, alt and the proxy, the FP2 readers. Alt also takes the
       standalone prober's ingest.
   - **Old clients** are unchanged: they never send `protocol`, and the `all`
     keys and pages are as today.
   - **Mixed versions are safe.** A reader deployed before the new export
     finds no `tcp-only` facets and answers the `all` view.
   - **The flag starts at deploy**, so the `tcp-only` view grows over the
     first 8 hours.
   - **Tests:**
     - the predicate over every leg combination: IPv4 silent with each
       verdict; IPv6 TCP failed with each verdict; IPv6 h3 silent; answered;
       not measured; `udp_probes_disabled`;
     - the two decisions;
     - `all`-view answers byte-identical with and without the new facets;
     - `tcp-only` answers labeled;
     - the 400 on an unknown value;
     - the readiness head.
   - **Dashboards:** "urnetwork / providers" gets the additions per bucket.
     No new alert class is needed.
2. **connect and sdk:**
   - the protocol label;
   - one placement predicate at every site;
   - the speed window's `tcp-only` request in auto;
   - UDP to quality only in auto;
   - the UDP no-route reply;
   - the local downgrade;
   - the metrics.

   The proxy service picks this up from its sdk dependency, since its hosted
   devices discover in process.
3. **Apps** through the nightly; the builder stamps versions. Providers need
   nothing; their builds decide which view admits them. Any July-build
   provider that the 11-11 updater fix lets update becomes an `all` provider
   again.

**Switches.** The owner prefers default-on code timed by its release, with no
off-by-default provider.yml knobs. The switch is therefore the client
release, plus a client setting that makes the speed window ask for `all`
again, as a kill switch. The server's `tcp-only` facets change no answer
until a client asks for them; they cost only export time and Redis memory.

**Metrics.**
- **Server:**
  - `urnetwork_stats_provider_tcp_only{bucket}`: the tcp-only additions per
    bucket, from the export's decisions.
  - `urnetwork_findproviders2_protocol_requests_total{rank_mode,protocol}`.
  - `urnetwork_findproviders2_answered_protocol_total{rank_mode,band,protocol}`:
    answered entries by label and band (native, borrowed, online).
  - A stats gauge of the tcp-only additions that opened contracts in the
    trailing 24 hours.
- **Client**, in the window status and `[rel]` events:
  - speed window entries by label;
  - UDP flows per window;
  - `udpNoRouteCount`;
  - local UDP downgrades;
  - TCP connect success and stalls on `tcp-only` entries against `all`
    entries.

**Measuring the gain.**
- **Baseline** (above): Speed 26.4k and Online 26.7k under today's rule;
  27,386 and about 20,445 at the ratio level for `all` and the additions.
- **After the server deploy:** `urnetwork_stats_provider_tcp_only` should
  settle near 19-20k in Speed and Online within 8 hours.
- **What the gain is.** In auto the speed window carries only TCP that is
  neither port 443 nor streaming. QUIC alone is 56% of traffic, and TCP 443
  goes to quality. So the user-side gain is the speed windows' TCP; most of
  the gain is on the provider side, where about 20k providers return to
  draws and earn from TCP traffic.
- **After the client release:**
  - the share of speed-window draws that land on additions: about 43%
    (20.4k of 47.8k) if they are drawn without preference;
  - the 24-hour count of additions carrying contracts;
  - their TCP failure rates against `all` entries;
  - UDP no-route counts, which should stay near zero outside locations
    without `all` supply.

## 7. Open questions for the owner

1. **Fixed Speed profile.**
   - *Question:* should a user's fixed Speed window ask for `tcp-only`, with a
     soft minimum of one `all` entry for its UDP flows?
   - *Recommendation:* not in v1. Keep fixed profiles on `all`; with one or
     two entries the soft minimum would often be the whole window.
2. **UDP fallback to the speed window.**
   - *Question:* when the quality window holds no `all` entry, may a UDP flow
     use an `all`-labeled speed entry?
   - *Recommendation:* no in v1, per "kept on quality not speed". Revisit if
     `udpNoRouteCount` is material.
3. **Draw preference inside a `tcp-only` request.**
   - *Question:* should `all` providers be drawn first, or should both views
     be drawn together?
   - *Recommendation:* together. Speed windows in auto carry TCP only, an
     `all` provider is no better for TCP, and all-first would leave the
     additions idle in most locations.
4. **Weight of the additions.**
   - *Recommendation:* the tcp-only URL weight, computed from OK + U. The
     strict weight would draw them at about half the rate of `all` providers
     with the same TCP record.
5. **Migration or jsonb read.**
   - *Recommendation:* the migration, one boolean column, metadata-only.
     Keep the jsonb read as the fallback (measured and affordable today, but
     it degrades as evidence grows).
6. **Label granularity.**
   - *Question:* should UDP be labeled per family?
   - *Recommendation:* one label. A provider whose UDP fails in either family
     is `tcp-only`. Per-family UDP routing is not worth its placement sites.
7. **Local downgrade in v1.**
   - *Recommendation:* yes. The probe signal lags by hours, and a UDP flow on
     a provider that just lost UDP blackholes until then.
8. **Tell providers.**
   - *Question:* should the provider status say a provider serves TCP only
     because its UDP fails?
   - *Recommendation:* yes. A status line naming the cause, such as an old or
     forked build, helps the July-build and fork operators fix it.
9. **Location counts and Connected.**
   - *Recommendation:* keep the location list counts and the Connected status
     on the `all` view. TCP fallback keeps browsing working when UDP has no
     exit; `UdpAvailable` is reported for diagnostics.
10. **/mainnet note.**
    - *Recommendation:* yes, when it ships. SN25 scoring and payouts read no
      URL outcome, but providers that carry TCP only regain traffic, so they
      earn more in both.
11. **Quality `tcp-only` view.**
    - *Recommendation:* define it (the table in §3) but do not export it in
      v1. No client asks for Quality with `tcp-only`, because the quality
      window carries UDP.
12. **TCP 443 on `tcp-only` entries.**
    - *Question:* should the quality window mix in `tcp-only` entries for TCP
      443, keeping a soft minimum of `all` entries for QUIC?
    - *Recommendation:* no in v1. The quality window carries QUIC, 56% of
      traffic, so it needs UDP-capable entries throughout. Revisit with the
      speed-window metrics if the provider-side gain looks too small.
