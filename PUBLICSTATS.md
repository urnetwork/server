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

Derived numbers are plain PromQL: new networks in range/per day
(`total_networks` minus its `offset`), staked and block amounts in USD
(× `alpha_usd`), price change (`offset 24h`/`7d`), block progress and
countdown (`time()`), provider share (`topk(6)` plus the remainder).

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

- `grafana/dashboards/public-traffic.json` — the dashboard (55 panels)
- `grafana/dashboards/signals.json` — internal panels for the new gauges
- `grafana/dashboards/connect.json` — receive-queue drop counters on the
  dropped-messages panel (in-flight connect work; coverage test)
- `grafana/grafana_test.go` — measurement lists, replica-safe read checks,
  public allowlist + fleet-label guard, provider-map structure test
- `controller/stats_collector.go` (+ `_test.go`) — new gauges, block clock,
  labeled gauge vec with stale-series deletion
- `model/network_stats_model.go` — `CountProvidersByCountry`;
  `CountProviderCountries` derived from it (`providers_map_model_test.go`)
- `model/st_model.go` (+ `st_model_db_test.go`) — `SumStMinerClaimedInBlockRange`
