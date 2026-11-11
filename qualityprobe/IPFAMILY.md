# IP families in the provider quality probe

Owner requests, 2026-11-11:

- "The provider quality probes should also check dualstack/ipv4/ipv6. If a
  provider advertises ipv4, we should use an A record on that provider for the
  url check. If the provider advertises ipv6 we should use an AAAA record. If
  the provider is dualstack we should make separate probes, one using the A
  record and one using the AAAA record. Pass means passing all probes. This
  applies to TCP probes and UDP probes."
- "The probe config should have an optional field per country that lets udp
  probes be disabled for the country, if there is some national firewall
  issue."

This builds on the HTTP/3 leg (H3.md); both branches are meant to land
together.

## What a provider advertises

The source of truth is `network_client_location_reliability.ipv4_proven` and
`ipv6_proven`, per client, which every reader turns into one category by the
same rule (`model.ProviderIpFamilyOfProven`, `ClientScore.IpFamily`):

| ipv4_proven | ipv6_proven | category | the probe's families |
|---|---|---|---|
| true | true | dualstack | IPv4 and IPv6 |
| true | false | v4-only | IPv4 |
| false | false | v4-only (legacy) | IPv4 |
| false | true | v6-only | none: not claimed |

- **How a family is proven** (connect/IPV6.md A2-A3, A8). A provider since the
  family-pinned transports (sdk `newProviderPlatformTransport`,
  `connect.FamilyPlatformTransportGroup`) connects to `connect-v4` and
  `connect-v6` with a transport per family, each declaring its family
  (`X-UR-IpFamily` on H1, `protocol.Auth.ip_family` on H3). The connect
  service records the declared family (`network_client_connection.ip_family_intent`)
  and the observed one (`ip_version`), and a connection proves a family only
  when the two agree (`model.ConnectionProvenIpFamily`). The reliability pass
  ORs that over the client's connected rows. This is proof by work: a provider
  advertises IPv6 because it holds a live IPv6 connection to the platform.
- **Old clients.** A connection with no intent (every transport before the
  pinned ones, and the family-agnostic standby) proves IPv4, whatever family
  it rides, so an old client is v4-only. On Main on 2026-11-11, 3,044 clients
  were connected over IPv6 with no intent; they read v4-only, by design
  ("IPv6 eligibility must not be widened on socket shape alone", FP2FIX).
- **Who reads it.** find-providers2 serves the category as `ip_family` from the
  cached score (`ClientScore.IpFamilies`, the rank cache's family facets);
  connect routes a flow of each IP version only to exits whose category can
  carry it (IPV6.md B3, `multiClientChannel.supportsIpVersion`), and downgrades
  an exit to v4-only locally after v6 dial strikes (B5). The verify egress
  index keeps one live egress per family per provider for SN25 trails
  (IPV6.md A10, C7). Extender `ip_versions` are about extenders, not
  providers.
- **The URL probe.** Its claim, census and eligibility already admit only
  providers that carry IPv4: `(ipv4_proven OR NOT ipv6_proven)`, since FP2
  ("IPv6 provider admission remains outside this rollout"). The claim now also
  returns each provider's category (`ProviderUrlProbeDue.IpFamily`, JSON
  `ip_family`: dualstack or v4-only), read from the same row as its
  eligibility; a server that predates it sends none, which the prober reads
  as legacy IPv4.

## Design

**Legs.** One URL turn makes, in order:

1. the IPv4 HTTPS leg, through the URL's A record (today's leg, unchanged);
2. its HTTP/3 leg, once it passed, for a URL that declares h3 (H3.md);
3. for a provider that advertises IPv6, on a URL that declares `ipv6: true`,
   the IPv6 HTTPS leg through the URL's AAAA record, once the IPv4 HTTPS leg
   passed;
4. its HTTP/3 leg over IPv6, once that passed, for an h3 URL.

A silent IPv4 HTTP/3 leg does not stop the IPv6 legs: whether that silence
counts is the server's place judgement. Every leg after the first gets the
ordinary per-request timeout (10 s), as the HTTP/3 leg does.

**Forcing a family.** Each leg carries its family on its request context, and
the provider transport's grant (`ProviderUrlProbeFamilyContext`, the URL
grant per family) scopes every hop, redirects included. The tunnel's resolver
then asks only that family's record (A or AAAA), keeps only that family's
answers (an IPv4-mapped AAAA answer is IPv4, and dropped), refuses any that is
not public in the family (for IPv6: 2000::/3 outside 2001::/23, 2001:db8::/32,
2002::/16, 3ffe::/16, 3fff::/20), and dials on `tcp4`/`tcp6` or `udp4`/`udp6`,
so the tunnel's own dial admits no address of the other family. There is no
fallback across families: a URL with no AAAA record fails its IPv6 request at
DNS, and nothing is dialed. A transport without family control (a test's bare
client) cannot make an IPv6 leg, which is then not measured.

**The tunnel.** A provider that advertises IPv6 gets a dual-stack inner tun
(`createDualStackTun`: connect's tun with IPv6 on, at the shared 1280-byte link
MTU, which is IPv6's minimum). Everything else about the tunnel is the same:
the multi-client hands a fixed destination every flow, whatever its family
(`supportsIpVersion`), the platform relays inner packets without reading them,
and the provider's NAT dials a v6 destination with a v6 socket of its host, as
it does for any user's v6 flow. The resolver of a dual-stack tunnel keeps to
IPv4's DoH and DNS servers, so a dual-stack provider's DNS rides the same IPv4
path it always did, and AAAA answers come back over it; only the URL's own
request is IPv6. So does the regional recommendation's resolver that races the
tunnel's own in cn, ru, ir, tr, kz and tm (main fe47f8506): its IPv6 servers
are left out for a provider that advertises IPv4. Every other request on the tunnel resolves IPv4 alone. QUIC
datagrams over IPv6 are 1232 bytes (1280 less 40 and 8). A provider that does
not advertise IPv6, legacy included, keeps the IPv4-only tun exactly as it
was. No connect or sdk change is needed: a real local dual-stack provider
carries all four legs end to end (`TestIpv6LegsThroughALocalDualStackProvider`).

**Choosing the URL.** The draw is the catalog's, as today, whatever the
provider advertises, less URLs that serve no IPv4 (`ipv4: false`). Each family
the URL serves then gets its legs; a family it does not serve is not measured
on that turn (`no_url`), never failed. So a URL without AAAA is never used for
an IPv6 leg, the IPv4 judgement of a dual-stack provider rests on the same
catalog as everyone's, and marking URLs `ipv6: true` raises the share of a
dual-stack provider's turns that test IPv6 gradually, which is how the rollout
is staged. (Drawing only URLs that serve both families would put every
dual-stack turn on the handful of URLs marked first and fail every turn of a
provider with broken IPv6, which `no_success` turns into an Online removal.)

**Pass means every leg passes.** A load passes when its IPv4 HTTPS leg passed,
its IPv4 HTTP/3 leg proved UDP (passed or answered, H3.md), and its IPv6 legs
passed or were not measured; the prober reports the first failed leg as the
load's stage (`h3`, then `ipv6`). An IPv6 leg is **failed** when it resolved
the AAAA record and dialed the site over IPv6 and did not get the URL's
contract (connect failure, TLS failure, timeout, content or performance), or
when its HTTP/3 leg was silent. It is **not measured** when it never reached
the site: an empty AAAA answer from the provider's place (CDNs answer AAAA in
some regions only), a resolver failure (it runs over the provider's IPv4 path
the load just used), a non-public answer, an instrument with no IPv6 path, or
the tunnel's loss or the run's end cutting it short. Unlike an HTTP/3 leg that
is not measured, which leaves the whole load unmeasured (H3.md), IPv6 never
leaves a load unmeasured: the load stands on its IPv4 legs, so a dual-stack
provider never loses its IPv4 measurement to a URL's AAAA.

**IPv6-only providers and IPv6-only URLs.** Every load's first leg is its IPv4
HTTPS leg, because that leg is what site tallies, the D3/D4 country-site pair,
the refresh's prober-fault line, TLS findings and site retirement read. A
provider that advertises IPv6 alone has none: the claim does not issue such a
provider (1 public v6-only provider was connected on Main on 2026-11-11), and
the prober reports it not measured should one be asked for, using no A record
and making no request. For the same reason a URL with `ipv4: false` is not
drawn. Probing them needs an outcome whose primary leg is IPv6, which none of
those readers knows; that is an owner decision (below).

## Judging

**IPv6 failures are judged by place** (`model/provider_egress_ipv6_place.go`),
exactly as a silent HTTP/3 leg is: a failed IPv6 HTTPS leg counts against the
provider only where at least 3 established networks (14 days old) other than
its own passed the same URL over IPv6 from the same country that day or the
day before; a silent IPv6 HTTP/3 leg only where they reached it over QUIC over
IPv6. Elsewhere the verdict is `place` and the outcome counts as its IPv4 legs.
The evidence is per URL, country, UTC day and transport
(`provider_egress_ipv6_place`, migration 958, at most 8 networks a day per
row; expired with the HTTP/3 evidence after two days). Networks that passed
over IPv6 TCP are no evidence for QUIC over IPv6. The ingest judges the legs
together (`JudgeProviderEgressLegs`): the outcome passes when every failed
leg after the IPv4 HTTPS leg, a silent IPv4 HTTP/3 leg included, was judged
the place's. An outcome with no IPv6 leg is judged exactly as before. A prober
that sends a verdict is refused.

**IPv4 stays as today.** A failed IPv4 HTTPS leg counts as it always has, with
no place judgement: IPv4 is what every provider is admitted on, and the TLS
judge already judges TLS findings by place. TLS findings and site retirement
read the IPv4 HTTPS leg alone: an IPv6 leg's TLS outcome stays inside it, and
`HttpsOk` (site tallies, the country-site pair, the prober-fault line) counts
an outcome with an IPv6 leg as an IPv4 pass, which it is. Security rechecks
make no IPv6 leg.

**Common-mode guard** (`provider_url_probe_common_mode.go`). Two windows join
the HTTP/3 one: the latest IPv6 HTTPS leg and the latest IPv6 HTTP/3 leg of
each of the pass's 16 most recent networks. When a window is full and at most
2% of its legs reached their sites, a run whose only failures are later legs
is held (claim retried without credit) if the window of every leg it failed
is common-mode, so a prober-side IPv6 fault cannot fail every dual-stack
provider at once. A leg that reaches its site ends the episode.

## Country UDP setting

```yaml
country_settings:
  ru:
    udp_probes: false   # absent or true: UDP (HTTP/3) legs run as designed
```

A provider whose exit country (its published country, the claim's) has
`udp_probes: false` makes no UDP leg in any family; its URLs are judged on
their HTTPS legs alone, exactly as URLs without h3, and the legs are counted as
`country_disabled` (metric outcome, `h3=`/`ipv6_h3=` log words), never as
failures. With no UDP leg there is no place evidence from that country and no
silent leg to judge. The setting is served with the pool
(`Pool.CountrySettings`, JSON `country_settings`, only countries that differ
from the default, in the pool version), and the evidence records
`udp_probes_disabled`. Keys are read in either case (`RU` is `ru`). A key
that is no two-letter code, or that repeats a country another spelling already
set, is refused alone (`country_invalid` under `country_settings`, logged and
counted with the other catalog refusals) and the rest of the file stands: one
bad key never refuses the catalog and stops URL probing. A well-formed code
ISO 3166-1 does not assign (`zz`) is ignored; a value that is not a boolean is
a catalog error like any other. Binaries that predate the key ignore it
(gopkg.in/yaml.v3's Unmarshal is not strict; checked against the h3-udp
parser too). `urnetwork_url_probe_udp_country_disabled_total{country}` counts
the loads the switch turned UDP off for, by the claim's country.

This switch was built twice on 2026-11-11, here and on
`qualityprobe/udp-country-switch` (454e5724e, on h3-udp); that branch is merged
into this one (`--no-ff`), with this branch's prober side and its parse rule
for keys (per-entry refusal, from that branch; unassigned codes ignored, from
this one), and its tests, metric and dashboard target kept.

## The URL schema

- `ipv6: true`: the host has AAAA and serves the probe over IPv6.
- `ipv4: false`: the host has no A record or fails over IPv4; absent means
  true, and only false is served. `ipv4: false` without `ipv6: true` serves no
  family and refuses the file at load, like any other catalog error.
- `h3: true` holds in every family the entry serves.

## Evidence

`url_probe_evidence` gains, all omitted when absent, so an IPv4 provider's
document is unchanged:

- `ipv6`: the IPv6 HTTPS leg (`tcp`: passed, failed or not_measured; `verdict`;
  its classifications, TLS outcome, sizes and timings) and its HTTP/3 leg
  (`ipv6.h3`, shaped as `h3`);
- `ip_family`: `dualstack` for a provider that advertised IPv6;
- `udp_probes_disabled`: true when the country setting turned the UDP legs off.

## Metrics, logs, dashboard

- `urnetwork_url_probe_transport_outcomes_total{transport,family,outcome}`
  (taskworker), family `ipv4` or `ipv6`. ipv4 tcp: success, error. udp: as in
  H3.md plus `country_disabled`. ipv6 tcp: success, error, not_measured,
  skipped (the IPv4 HTTPS leg failed first), no_url. Queries that sum by
  transport, like the HTTP/3 row's, read as before.
- `urnetwork_egress_url_ipv6_verdicts_total{transport,verdict}` (ingest).
- `urnetwork_url_probe_ipv6_common_mode_held_total{transport}`.
- The probe log line gains `ipv6=` and `ipv6_h3=`, `h3=` can say
  `country_disabled`, and `failure_stages=` can count `ipv6`; the monitor's
  record pattern (`monitor/tailer.go`) knows all of them.
- Dashboard "urnetwork / provider quality probes", row "URL legs by IP family".

**Turn budget.** A dual-stack provider's turn can make four legs, so the run
budget counts four per-request timeouts for it (`Options.RunBudget`), and the
pass's turn reserve is the dual-stack one: 250 s (was 230 s), 340 s with
publication, inside the six-minute renewal headroom with a 20 s margin (was
40 s). An IPv4 provider's turn is bounded as before.

## Deploy chain

1. `db migrate` on Main: 957 (HTTP/3 place evidence) and 958 (IPv6 place
   evidence), each one new empty table created when absent under a five-second
   lock bound, after main's own pending migrations.
2. The monitor watcher's build, before the taskworker: its record pattern must
   know `ipv6=`, `ipv6_h3=`, the `ipv6` stage and `country_disabled`.
3. The taskworker: the URL lane, the claim and the in-process ingest. This is
   the switch; dual-stack providers get a dual-stack tunnel from here on, and
   IPv6 legs start only on URLs marked `ipv6: true`.
4. api and alt, for a standalone prober's HTTP ingest (old ones refuse an
   outcome carrying the new evidence fields) and the pool route (the country
   settings).
5. The config release with the marks. Old binaries ignore `ipv6`, `ipv4` and
   `country_settings`, so code and config can ship in either order.
6. The Grafana panels (`bringyourctl grafana load-defaults --dashboards_only`).

## Impact estimate (Main, read-only, 2026-11-11)

- **Who advertises what.** Connected, valid, active, non-hosted providers with
  a public key: 101,713 v4-only (92,036 in an eligible URL cycle), 972
  dual-stack (567 eligible), 1 v6-only (not eligible). Connected transports:
  126,636 with no intent over IPv4 and 3,052 over IPv6, 17,268 pinned IPv4 and
  1,423 pinned IPv6, no intent and observation that disagree.
- **Existing signal of broken IPv6.** No stored measurement sends IPv6 to a
  site through a provider, so the share with broken IPv6 egress is unknown. The
  one proxy: 17 of 1,335 dual-stack clients (1.3%) have their IPv6 transport
  geolocated to another country than their IPv4 one (us to nl 4, us to hk 3,
  us to ru 2, us to sg 2, and 6 single pairs): IPv6 that exits elsewhere, a
  country problem rather than a URL one, which this probe does not judge.
  Dual-stack providers pass IPv4 URLs more often than others (71.8% of turns
  against 59.3% over the last 8 hours).
- **Membership.** In the 8 hours before about 14:30Z, 570 dual-stack providers
  had URL outcomes (20.4 runs each) and 248 met the Quality ratio, against
  39,489 of 114,597 v4-only ones. With the place rule, a provider verdict
  needs 3 other established dual-stack networks in the country, which only us
  (12 established networks), de (7), fr (6) and gb (4) have; se (174 providers
  on 1 network), hk (2 networks), tw (1), sg (2), jp (3) and es (1) get place
  verdicts until more networks there prove IPv6. So at most about 117
  Quality-ratio providers (us 78, de 27, fr 9, gb 3) can leave Quality and
  Speed over IPv6, 0.3% of the Quality-ratio population, and only those whose
  IPv6 fails. Such a provider fails the share of its turns that draw an
  `ipv6: true` URL: config main f2a627a marks 74 of the 121 general
  destinations (61%) and 147 of the 211 candidates, so it falls below the 4/5
  ratio into the Online fallback; it is not removed, since its turns on URLs
  without ipv6 still pass.
- **Cost.** Each dual-stack turn on a marked URL adds one or two warm-tunnel
  requests; about 1% of turns.

## Measure before it counts

As for h3, with no code switch. Config main already carries the full set of
marks (f2a627a: 74 general destinations and 147 candidates `ipv6: true`, on
top of b2e0b76's h3 marks), so the first config release this code runs with
decides the stage:

1. Release a config that marks a handful of URLs `ipv6: true` (say 5 of the
   121, about 4% of a dual-stack provider's turns) for 48 hours; with
   f2a627a's marks released as they are, the requirement starts in full with
   the taskworker deploy.
2. Read the family row and, from the history, the IPv6 outcomes by country:

   ```sql
   SELECT url_probe_evidence->'ipv6'->>'tcp' AS tcp,
          url_probe_evidence->'ipv6'->>'verdict' AS verdict,
          url_probe_evidence->'ipv6'->'h3'->>'udp' AS udp, count(*)
   FROM provider_egress_health_history
   WHERE measured_at > (now() AT TIME ZONE 'UTC') - interval '24 hours'
     AND url_probe AND url_probe_evidence ? 'ipv6'
   GROUP BY 1, 2, 3;
   ```

3. Then mark the rest.

## Owner decisions

- Timing of the land, both migrations, the taskworker deploy and the config
  release, and whether to stage the marks as above.
- The IPv6 place rule mirrors the HTTP/3 one (3 other established networks in
  the country, 14 days, today and yesterday). Dual-stack providers sit on few
  networks per country (Sweden's 174 on one), so in most countries IPv6
  failures are excused; a regional or global fallback for countries with fewer
  than 4 dual-stack networks would apply the rule there.
- Whether to probe IPv6-only providers (and `ipv4: false` URLs): that needs
  their admission in the claim and eligibility (unchanged since FP2) and an
  outcome whose primary leg is IPv6, which site tallies, TLS findings and
  retirement would have to leave out.
- Whether an IPv6 transport geolocated to another country than the provider's
  IPv4 one (17 clients today) should affect the provider's place or category.
