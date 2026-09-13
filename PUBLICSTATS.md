# PUBLICSTATS — the public network stats dashboard

2026-08-17. The grafana public dashboard "urnetwork / network stats"
(`grafana/dashboards/public-traffic.json`, uid `urnetwork-public-traffic`,
tagged `public`) was expanded from 20 panels to 55: a two-band KPI header,
live traffic and devices, the provider network (world map, top countries,
share, reach), growth, the weekly subnet block clock and ledger, and alpha
economics. It is published read-only, no login, by
`bringyourctl grafana load-defaults` (see `grafana/grafana.go`) and listed at
`<env>-grafana.<domain>/stats`; the uid is unchanged so the existing public
url (access token) is preserved.

## Data sources

Everything is PromQL against the warp mimir (`warp-mimir`), fed by the
service stats pushers (`grafana.go`; series are per process, keyed by
{env, service, block, host, instance}). Two families are public:

- connect-fleet series, always aggregated across the fleet with `sum(...)`
  and `{instance!=""}` (a redeploy's overlapping old+new processes stay
  separate series): `urnetwork_connect_transfer_bytes` (contract-settled,
  acknowledged bytes — "bytes carried"), `urnetwork_connect_resident_clients`
  (distinct devices), `urnetwork_connect_exchange_io_bytes_total{sent,data}`
  (live relay throughput), `urnetwork_connect_connection_new` (new
  connections per minute).
- the operator measurements `urnetwork_stats_*` from the taskworker stats
  collector (`controller/stats_collector.go`), read with `max(...)` because
  every taskworker publishes the same value. This work added, on the same
  5-minute db tick / 60-second chain tick:
  - `users_24h` — block-users predicate over a rolling 24h window
  - `online_providers`, `online_providers_by_country{country_code,country}`,
    `provider_regions`, `provider_cities` — one scan of the connected valid
    public provider population (`model.CountProvidersByCountry`; the same
    predicate as the public `/stats/providers-map`). `countries` is now the
    number of countries in that result, so the two can never disagree. The
    per-country vec deletes series for countries that lost their last
    provider (`statsGaugeVec.replace`) so they go stale instead of pushing a
    stale count forever.
  - `block_number`, `block_start_seconds`, `block_end_seconds` — the subnet
    block clock (`model.SubnetBlockStart` etc.), so the dashboard's progress
    gauge and countdown are `time()` arithmetic against exported truth
    rather than a genesis constant copied into json.
  - `block_miner_claims_alpha`, `block_miners_claimed` and their
    `prev_block_*` — MinerClaimed events in the st_event mirror windowed by
    chain block like deposits and emissions (`model.SumStMinerClaimedInBlockRange`).

### The extender network (2026-09-13, connect/EXTENDER.md M)

Four more public gauges on the same 5-minute db tick, and an `extender
network` row on the public dashboard between the provider network and growth:

- `online_extenders` and
  `online_extenders_by_country{country_code,country}` — extenders that are
  active with at least one active probed address
  (`model.CountExtendersByCountry`, one scan). An extender is a host that
  relays client traffic to the operator and to providers on paths that would
  otherwise be blocked; the directory already gives its address away, so its
  per-country count publishes nothing new. The country label is the name of
  the country location the activation resolved
  (`network_extender.country_location_id`, stored by the activation handler
  through `model.CreateLocation`), or the upper-case code for a row not yet
  filled by an activation. The vec replaces its whole label set like the
  provider one, so an emptied country goes stale rather than pushing forever.
  An extender whose activation resolved no country at all counts in the total
  and has no series.
- `online_providers_by_ip_family{ip_family}` and
  `online_extenders_by_ip_family{ip_family}`, with `ip_family` one of `ipv4`,
  `ipv6`, `dualstack` — **always all three series**, so a family with no
  members is a zero and never an absence. A provider's family comes from
  `network_client_location_reliability.ipv4_proven`/`ipv6_proven`: both is
  dualstack, v6 alone is ipv6, everything else is ipv4 (a row written before
  those columns reads as v4-only everywhere else in this schema). An
  extender's family is the families it has active address rows on. The family
  counts of either population sum to its total, and the provider split comes
  out of the existing per-country scan rather than a second pass.

`/stats/providers-map` gains `extender_count` on every region entry, always
present: `GetProvidersMap` runs a second aggregate of the online extenders by
`network_extender.region_location_id` and merges it with the provider
aggregate, so a region with extenders and no providers is exported with
`provider_count` 0. An extender located only to its country is placed under
the country location's own name at the country centroid; one with no location
at all is counted in the gauges only.

Derived numbers are plain PromQL: new networks in range/per day
(`total_networks` minus its `offset`), staked and block amounts in USD
(× `alpha_usd`), price change (`offset 24h`/`7d`), block progress and
countdown (`time()`), provider share (`topk(6)` plus the remainder).

## Internal measurements: the contract gauges

Six gauges from the same collector tick are deliberately **not** public
(connect/EXTENDER.md M3, M4). They are listed in `grafana_test.go` as
`internalMeasurementMetrics`, which requires them on `providers.json` and
keeps them out of `publicSafeMetrics`:

    urnetwork_stats_open_contracts                transfer contracts open now
    urnetwork_stats_contracts_24h                 contracts created in the trailing 24h
    urnetwork_stats_open_contracts_with_extender  of the open ones, those with an extender party
    urnetwork_stats_contracts_with_extender_24h   the same over the trailing 24h
    urnetwork_stats_open_disputes                 disputes raised and not yet decided
    urnetwork_stats_disputes_24h                  contracts created in the trailing 24h that are disputed

A gauge is a point in time, so each number comes in two forms. The open counts
read the partial indexes on `transfer_contract` (`open`, and `dispute AND
outcome IS NULL`) plus an existence probe of `contract_extender` over the open
set, which is cheap because the open set is small.

### The hour bucket cache

The 24 hour counts are bucketized in one hour blocks of `create_time`
(`model/contract_stats_model.go`). A complete bucket never changes, so it is
computed once — by whichever taskworker needs it first — and cached in redis
under `stats.contract_hour.<unix bucket start>` as json with a 26 hour ttl. A
cold cache is filled with **one grouped query per table** over the whole
missing range (`date_trunc('hour', create_time)`), not one query per bucket.
A bucket counts as complete only once its hour has been over for a minute, so
an insert still in flight when the hour turned can never freeze a short count
into the cache; until then that bucket is counted live, as the current partial
one always is. The 24 hour value is the 23 closed buckets before the current
one plus the current partial one, so the window is at most 24 hours long and
no contract is counted twice.

A bucket's contract and dispute counts are one range scan of
`transfer_contract_create_time` with `count(*) FILTER (WHERE dispute)`; its
extender count is a range scan of `contract_extender` by the
`(create_time, contract_id)` index the extender work added, so it is never a
probe per contract. Every party row of one contract is written in the
contract's own transaction, so a contract never straddles two buckets.

The clock is a parameter (`model.CountContracts(ctx, now)`), so the window is
placed by the caller and the tests never sleep.
`model.Testing_ContractHourCacheStats` reports what the last window did —
buckets from redis, buckets filled, fill queries, buckets counted live — which
is how the tests tell a consulted cache from a rescanned range.

## The providers dashboard

`grafana/dashboards/providers.json`, uid `urnetwork-providers`, title
`urnetwork / providers`, internal (no `public` tag). It has one template
variable, `env`, from
`label_values(urnetwork_stats_online_providers, env)`, and no block or host
variable: every gauge here is replicated by every taskworker and read with
`max(<metric>{env="$env"})`, so a fleet breakout would only split one
measurement. Rows: population (providers and extenders with their family
splits), contracts (each of the six gauges as a stat and a time series),
ratios (the share of open contracts with an extender party, the 24 hour
dispute rate), and the top 10 provider and extender countries.
`TestProvidersDashboardPinsInternalMeasurements` pins all of that and fails if
a contract gauge ever reaches a public dashboard.

## What is deliberately NOT public

`grafana/grafana_test.go` `TestPublicDashboardsQueryOnlyPublicSafeMetrics`
allowlists the metrics a `public`-tagged dashboard may query and rejects any
`by (host|instance|block|service|env)` breakout. Adding a metric to
`publicSafeMetrics` is a publication decision. Left out on purpose: error and
auth taxonomies (`contract_failures`, `control_frame_failures`,
`auth_jwt_*`), drain/deploy/readiness state, redis/pg/allocator internals,
exchange mesh topology, `build_info`, per-provider or per-network numbers,
payouts and balances (auth-gated on the api today), consumer geography
(only provider geography is published, as `/stats/providers-map` already
does).

## Grafana public dashboard constraints (verified against 13.x)

- No template variables (`$env`); built-ins `$__range`, `$__rate_interval`,
  `$__interval` are interpolated by the prometheus backend and work, as do
  `offset $__range` and `time()`. Frontend-only globals (`$__from`, …) do not.
- Hidden (`hide: true`) queries are not executed publicly — none are used.
- The world map is a geomap markers layer over an instant table query of the
  per-country gauge, placed by looking `country_code` up in grafana's
  bundled `public/gazetteer/countries.json` (upper-case ISO alpha-2, which is
  why the collector upper-cases the db's lower-case codes). Basemap tiles
  come from CARTO and are fetched by the viewer's browser.
- Daily bars (`increase(...[1d])`, min step 1d) align to UTC midnight and
  show complete days only.

### Missing points are not zero traffic

The public panels intentionally do not zero-fill or span missing samples. A
gap in `live throughput` means Mimir returned no stored evaluation for that
interval; it does not mean the network carried zero bits. This distinction is
especially important because the build-info control is independent of user
traffic.

The 2026-09-01 investigation found matching multi-hour gaps across Connect
throughput/resident-client metrics and independent taskworker provider/network
gauges. Every endpoint correlated with a Mimir fleet restart. The Grafana
bundle used an ephemeral local TSDB directory while Mimir's clean-shutdown
flush defaulted off, so removing a container discarded the recent unuploaded
head. Warp must render
`blocks_storage.tsdb.flush_blocks_on_shutdown: true`; `SIGNALS.md` §11.20 and
the `mimir-continuity` probe test the raw seven-day control range. Historical
holes are not reconstructable from Mimir and disappear from this dashboard
only when they age out of its range.

## Adding a public stat (checklist)

1. Export it: `newStatsGauge`/`newStatsGaugeVec` in the collector (a gauge
   that is never set is never exported — absence, not zero).
2. Put it on the internal `signals.json` measurements row as
   `max(<metric>{env="$env"})` — `TestInternalDashboardsCoverEveryApplicationMetric`
   and `TestInternalNetworkMeasurementsAreScopedAndReplicaSafe` enforce it.
3. Add it to `networkMeasurementMetrics` (or the labeled list) and, if it
   may be public, that is what admits it to `publicSafeMetrics`; then place it
   on `public-traffic.json` read with `max(...)`.
4. Render check: `grafana.LoadDefaults` against a local grafana
   (`docker run grafana/grafana-oss` + a prometheus backfilled with
   `promtool tsdb create-blocks-from openmetrics`) and
   `/render/d/urnetwork-public-traffic/x?kiosk&width=1600&height=4300` via
   `grafana/grafana-image-renderer` (`BROWSER_MAX_HEIGHT` above 3000).
   Prometheus's 5-minute instant lookback matters with backfilled data
   (`--query.lookback-delta`), not with the 15-second production push.

## Files

- `grafana/dashboards/public-traffic.json` — the dashboard (68 panels)
- `grafana/dashboards/providers.json` — the internal providers dashboard
- `grafana/dashboards/signals.json` — internal panels for the new gauges
- `grafana/dashboards/connect.json` — receive-queue drop counters on the
  dropped-messages panel (in-flight connect work; coverage test)
- `grafana/grafana_test.go` — measurement lists, replica-safe read checks,
  public allowlist + fleet-label guard, provider-map structure test
- `controller/stats_collector.go` (+ `_test.go`) — new gauges, block clock,
  labeled gauge vec with stale-series deletion
- `model/network_stats_model.go` — `CountProvidersByCountry` with its ip
  family split; `CountProviderCountries` derived from it
  (`providers_map_model_test.go`)
- `model/network_extender_stats_model.go` (+ `_test.go`) —
  `CountExtendersByCountry`
- `model/contract_stats_model.go` (+ `_test.go`) — the open contract counts
  and the hour bucket cache
- `model/providers_map_model.go` (+ `_test.go`) — `extender_count` on the map
- `model/network_extender_model.go` — the four location ids on an activation
  (`controller/extender_controller.go` resolves and creates the location)
- `model/st_model.go` (+ `st_model_db_test.go`) — `SumStMinerClaimedInBlockRange`
