# STATS3 — real data for the public stats feed (/stats/last-90)

2026-08-07. The provider stats feed reported one city ("Palo Alto") because
its data source was fake: the ONLY writer of `audit_provider_event` and
`audit_contract_event` in the codebase was the sample-data generator
(`controller.AddSampleEvents`, hardcoded United States/California/Palo Alto,
random ids), invoked from `bringyourctl stats add`. This work adds real
producers, guards the generator, reconstructs what recent history the real
tables still hold, and audits every series in the feed.

## Consumer chain (what actually reads ComputeStats)

```
audit_provider_event / audit_extender_event / audit_network_event /
audit_device_event / audit_contract_event
        │
        ▼
model.ComputeStats90 / ComputeStats          model/audit_model.go
        │  callers:
        │   - taskworker work.ExportStats    taskworker/work/audit_work.go
        │     (re-enabled 2026-08-08 at an hourly cadence,
        │      exportStatsInterval; the old gate was about its 30s cadence)
        │   - bringyourctl stats compute / stats export
        ▼
model.ExportStats → redis key "stats.last-90" (no ttl)
        ▼
GET /stats/last-90                            api/api.go →
                                              api/handlers/stats_handlers.go
        ▼
public web dashboard / any external chart (grafana JSON/infinity-style panel)
```

Grafana proper (server/grafana.go) pushes the **prometheus** registry
(`urnetwork_connect_transfer_bytes`, contract failure counters, …) to the warp
grafana service — that path was never fake and is untouched. The
city/region/country provider series reach dashboards only through the
`/stats/last-90` blob above, which is what this work fixes.

Sibling public stats that do NOT go through ComputeStats (already real, from
live tables; unchanged):

- `/stats/providers-map` — reliability tables (`GetProvidersMap`).
- `/stats/providers`, `/stats/providers-last-n`, `/stats/provider-last-n`,
  `/stats/providers-overview-last-n` — live aggregates over
  `transfer_contract ⋈ contract_close` etc. (provider_model.go).
- `CountProviderCountries` (network_stats_model.go) — reliability tables.

## Per-series accuracy audit of the /stats/last-90 payload

| series (json) | source events | before | now / verdict |
|---|---|---|---|
| `providers_data`, `providers_summary` | provider online/offline | FAKE (sample only) | REAL — SweepProviderAuditEvents + backfill. (Historical `online_superspeed` events still count as online.) |
| `countries_data`, `regions_data`, `cities_data` (+summaries) | provider event geo | FAKE ("Palo Alto") | REAL — geo from `network_client_location` → `location` names; empty (unlocated) names no longer counted as a place |
| `providers_superspeed_data`, `providers_summary_superspeed` | online_superspeed events | FAKE | **REMOVED** (2026-08, user decision) — no real superspeed signal is wired |
| `all_transfer_data`, `all_transfer_summary(_rate)` | contract_closed_success bytes | FAKE (1 GiB/sample) | REAL — RollupTransferAuditEvents: one row/day of settled destination-party bytes from `transfer_contract ⋈ contract_close` (same join as provider payout stats) |
| `all_packets_data`, `all_packets_summary(_rate)` | contract event packets | FAKE (bytes/1500) | **REMOVED** — no real packet count is recorded anywhere |
| `extender_transfer_data` (+summaries) | contract events with extender_id | never populated | **REMOVED** — no extender producer exists anywhere |
| `extenders_data`, `extenders_superspeed_data` (+summaries) | extender online/offline | never populated | **REMOVED** — `audit_extender_event` has no writer at all (table + reaper kept) |
| `networks_data`, `networks_summary` | live `network` count anchored backward with network created/deleted deltas | HALF-REAL — created emitted (network_model.go), deleted never emitted, so the active count only grew | REAL current count — summary/today come directly from `COUNT(network)`; daily history walks retained deltas backward. Deletes are exact from the 2026-08-10 `NetworkDeleted` deploy forward; older unrecorded deletion times cannot be reconstructed and age out of the 90-day view |
| `devices_data`, `devices_summary` | device added/removed | never populated | REAL (FIXED 2026-08, user decision) — **connected-per-day: distinct devices with ≥1 connection that day**. SweepDeviceAuditEvents (all connected clients) + touched-union aggregation in computeStatsDevice. **Public-claim guidance: this measures REACH — every connected client id, including pure consumers and hosted-proxy child clients — do NOT quote it as a provider count** (that is `providers_data`). |

Removal mechanics: the fields were deleted from the `Stats` struct and their
compute passes removed (ComputeStats now runs 4 passes, not 7). Removal is
**output-side, at export time**: `/stats/last-90` serves the stored redis blob
verbatim, so an already-exported blob keeps the old keys until the next export
overwrites it (hourly via the re-enabled ExportStats loop, or immediately via
`bringyourctl stats export`). Decoding an old blob is unaffected — unknown
JSON keys are ignored on unmarshal, and the API contract explicitly tells
clients to ignore unknown fields.

Consumers updated for the removed keys:

- `mmm/ur.io/api/bringyour.yml`, `connect/api/bringyour.yml` (canonical spec
  embedded into the ur.io docs page), `web/web2/ur.xyz/astro/public/openapi.yml`
  — `StatsResult` schema fields removed; `mmm/ur.io/react/src/data/openapi.js`
  regenerated (`react/scripts/generate-docs2.mjs`) and
  `mmm/ur.io/api/build/api.html` rebuilt (redocly).
- `web/web/bringyour.com/stats.js` and its copy
  `mmm/ur.io/examples/web/web/bringyour.com/stats.js` — Packets / Extender
  Transfer / Extenders tiles and the Providers "superfast" substat removed.
- `mmm/ur.io/astro/build/**` (api.html per-locale, PageIslands bundle) are
  stale build artifacts; they refresh on the next astro site build.
- No grafana dashboard JSON exists in these repos; if a warp grafana
  dashboard reads the blob, manually remove panels using:
  `all_packets_data`, `all_packets_summary`, `all_packets_summary_rate`,
  `providers_superspeed_data`, `providers_summary_superspeed`,
  `extender_transfer_data`, `extender_transfer_summary`,
  `extender_transfer_summary_rate`, `extenders_data`,
  `extenders_superspeed_data`, `extenders_summary`,
  `extenders_summary_superspeed`.

## What was built

### 1. Real provider emission — `model.SweepProviderAuditEvents`
(model/audit_provider_sweep_model.go; task in taskworker/work/audit_work.go,
15-minute cadence, `RunOnce("sweep_provider_audit_events")`.)

Definition of online: `provide_key` row with `ProvideModePublic` AND a
`network_client_connection` with `connected = true`. `device_id` on the event
is the client id. Geo: the located connection's `network_client_location`
resolved to `location` names (preferring a located connection, then newest).

The sweep diffs that live set against `audit_provider_state` (new table, last
emitted state per device) and appends only transitions:

- offline→online, geo change, or 30-day re-assert → `provider_online_not_superspeed`
- online→absent → `provider_offline`

Why a state diff instead of call-site hooks: every provider path folds into
the same observable state — clean disconnect (`DisconnectNetworkClient`),
handler crash sweep (`CloseExpiredNetworkClientHandlers` flips `connected`),
provide toggle (`SetProvide` via the Provide control frame), multi-connection
clients, proxy clients — so **no path can skip emission by construction**.
Detection latency is the sweep cadence (15 min), far finer than the feed's
daily resolution. ComputeStats carries per-device state across days (per-day
latest event via `MAX(event_id)`, ids are time-ordered), so a provider online
for weeks emits nothing and stays counted; the 30-day re-assert keeps its
latest event inside the 180-day audit retention. The first sweep on an empty
state table emits the full current snapshot — that is the day-one seed
(`bringyourctl stats sweep-providers` runs it immediately at deploy).

Geo coverage: `network_client_location` is written per connection at connect
(`SetConnectionLocation`, with retry while the connection lives), so located
coverage is near-total for stable connections; a client whose lookup has not
landed yet emits empty names, which the aggregation now skips (an unlocated
provider counts as a provider but not as a city/region/country — previously an
empty name would have added one phantom bucket), and the sweep re-emits with
real geo once located, never downgrading known geo to empty.

### 1b. Real device emission — `model.SweepDeviceAuditEvents`
(same file; runs inside the same 15-minute taskworker task.)

Devices series semantics (user decision): **a device counts on day D iff it
had at least one connection to the network that day** — reach, not provider
count. Route decision: computed from audit events, NOT from
`network_client_connection` windows, because disconnected connection rows are
reaped 8 HOURS after disconnect (`RemoveDisconnectedNetworkClients`), so a
90-day lookback can only be served by the audit table's 180-day retention.
The sweep diffs ALL clients with a connected connection (no provide-key
filter; a multi-connection client is one device, removed only when its last
connection drops) against `audit_device_state` (new table) and appends
`device_added`/`device_removed` transitions, with the same first-run snapshot
seed and 30-day online re-assert as providers.

`computeStatsDevice` implements connected-per-day as a union: devices whose
carried state is "added" (connected across the day) plus devices touched by
ANY event that day — so a same-day connect+disconnect still counts that day,
and a session spanning midnight counts both days. Carry-in (pre-window) rows
establish state only, never same-day evidence. Approximation: a session
that starts and ends between two sweeps (< ~15 min) is missed.

Backfill (`BackfillDeviceAuditEvents`, part of `stats backfill` /
`migrate-202608`): one added+removed pair per device-day of
`client_reliability` evidence (any row implies a connection that minute).
Reach matches the provider backfill (~30 days) but **coverage within those
days is narrower**: the announce path records reliability rows only for
providing / provide-changed clients, so backfilled device history covers
roughly the provider population; pure consumers enter the series from deploy
forward. This is stated because no other per-day evidence exists
(connection rows: 8 h; `auth_time`: a single last-seen marker).

### 2. Aggregation fixes (model/audit_model.go)
- Empty geo names are not counted as distinct places.
- The trailing day-pack loops now include `endDay`: with transitions-only
  emission, a day with zero events (including today) must still export the
  carried state. The sample generator's constant event stream had masked this
  — a real latent bug for the networks series too.
- Network history is anchored to the authoritative current `network` table and
  reconstructed backward from created/deleted events inside the requested
  window. The former forward replay silently lost every still-live network
  when its only creation event crossed the 180-day audit retention boundary
  (359,365 live networks were missing on main on 2026-08-26). Backward replay
  is compatible with bounded retention and keeps `networks_summary` exact.
  Deletion events did not exist before 2026-08-10, so historical points before
  that date remain best-effort until they leave the lookback; the current
  count is exact immediately.
- Provider/country/region/city summaries are current selectable-supply counts
  from `network_client_location_reliability` plus Public provide keys, not the
  historical series' three-day maximum. This is the same population used by
  the public provider map and the Prometheus public-stats collector.
- Honesty comment at `ComputeStats`: series are real from deploy forward plus
  labeled reconstruction; unrecorded history is not fabricated.

### 3. Backfill — `model.BackfillProviderAuditEvents`
(`bringyourctl stats backfill [--start=<date>] [--end=<date>]`)

Source: `client_reliability` per-minute blocks. `provide_enabled_count >= 1`
is recorded only while a client was connected AND provide-public — exactly
"provider online this minute". Reconstruction: one online per provider-day (at
the day's first providing minute) and one offline closing each run of
consecutive providing days. Provenance `event_details = "backfill:v1"`;
idempotent by replacement (rerun deletes prior backfill rows in the window
first). Stated approximations, also in the code comment:

- reach ≈ 30 days (`ClientExpiration` retention) — nothing older exists;
- geo is the client's CURRENT `network_client_location_reliability` location
  applied to its whole reconstructed history;
- the window is clamped to whole UTC days strictly before the first live
  sweep event (and before today), so backfill days never share a stats day
  with live rows (ids are insertion-ordered, not event-time-ordered, so
  mixing within a day would let a later-inserted backfill row shadow a live
  row);
- a client still providing on the last backfilled day gets no fabricated
  trailing offline; its state row is seeded (`ON CONFLICT DO NOTHING`, never
  clobbering live sweep state) so the next sweep records a real offline if it
  is gone.

### 4. Transfer rollup — `model.RollupTransferAuditEvents`
(task every 6h re-rolling the last 3 days; also `bringyourctl stats
rollup-transfer`, and `stats backfill` rolls the whole requested range.)

One aggregate `audit_contract_event` per complete UTC day (noon-stamped),
summing `contract_close.used_transfer_byte_count` for `party = 'destination'`
by `transfer_contract.close_time` — the settled-bytes join the provider payout
stats already use. Party/network/device ids on the rollup row are zero ids and
`event_details = "transfer-rollup:v1"`; per-day idempotent by replacement.
This satisfies ComputeStats (which SUMs per day) without mirroring the
per-contract close firehose into the audit table. Backward reach is contract
retention: ~7 days after payout completion (`CompletedContractExpiration`), up
to 90 days for stragglers.

### 5. Guarded sample generator
`controller.AddSampleEvents` → `AddSampleEventsForTesting`: returns an error
unless `WARP_ENV` ∈ {local, test}; every sample row is marked
`event_details = "sample:v1"`. ctl command renamed `stats add` →
`stats add-sample`.

### 6. Purge of the fake history — `model.PurgeSampleAuditEvents`
(`bringyourctl stats purge-samples`). Deletes provider/contract audit rows
with `event_details IS NULL` (the legacy sample population — the generator was
the only historical writer, so in production NULL-details is exactly the fake
"Palo Alto" data) or `= "sample:v1"`. Real producers all write other markers.
Operator-invoked, batched.

## Volume estimate

- Provider sweep: transitions only. P providers with s sessions/day ≈
  `2·P·s` rows/day (+ P/30 re-asserts/day). At P=10k, s=2 → ~40k rows/day ≈
  7M rows per 180-day retention window — well inside what the existing hourly
  50k-batch reaper (`RemoveOldAuditEvents`) drains. Flapping is bounded by
  the 15-min observation cadence (≤ ~96 pairs/device/day worst case).
- Device sweep: same formula over ALL connected clients — D devices, s
  sessions/day ≥15 min ≈ `2·D·s` rows/day. At D=100k, s=2 → ~400k rows/day ≈
  72M rows/180d; inflow is well under the reaper's ~1.2M/day drain capacity,
  and the day-one seed writes one row per currently-connected device in a
  single batch tx. If volume ever bites, the per-day union semantics
  tolerate a coarser sweep cadence.
- Backfill: providers ≈ P×30 events one-time (~300k at P=10k); devices ≈ 2×
  that population's device-days (pairs).
- Transfer rollup: 1 row/day.
- Sweep query cost per 15 min: one indexed pass over connected providers +
  one DISTINCT pass over connected clients + full reads of the two small
  state tables.

## Deploy runbook

One line:

    bringyourctl stats migrate-202608

It executes the steps below in order with per-step outcome + timing, stops on
the first failure (reporting which steps completed), and is safe to re-run
from the top — every step is idempotent: migrations are versioned no-ops once
applied, purge deletes only whatever sample rows remain, the sweep emits only
unrecorded transitions, backfill replaces its own provenance-marked rows, and
export overwrites the blob. Exploded form:

1. `bringyourctl db migrate` (creates `audit_provider_state`; note this
   applies ALL pending migrations — there is no per-feature subset).
2. `bringyourctl stats purge-samples` — remove the fake history.
3. `bringyourctl stats sweep-providers` — seed today's real provider AND
   connected-device snapshots (the taskworker sweep task takes over on its
   own after this).
4. `bringyourctl stats backfill` — reconstruct ~30 days of provider history,
   device-days (provider-population coverage only pre-deploy), and roll
   settled-transfer days still in the contract tables.
5. `bringyourctl stats export` — refresh the redis blob now; this is also the
   moment the removed keys disappear from `/stats/last-90` (the handler serves
   the stored blob verbatim). The re-enabled hourly ExportStats loop keeps it
   fresh afterwards.

## Files changed

- `model/audit_provider_sweep_model.go` (new) — provider + device sweeps, backfills, rollup, purge
- `model/audit_provider_sweep_model_test.go` (new)
- `model/audit_model.go` — honesty comment; empty-geo bucket fix; endDay-inclusive packing; connected-per-day devices aggregation
- `db_migrations.go` — `audit_provider_state`, `audit_device_state`
- `model/account_model.go` — emit `NetworkDeleted` in `RemoveNetwork`
- `controller/audit_controller.go` — guarded, renamed, provenance-marked sample generator
- `controller/audit_controller_test.go` (new)
- `taskworker/work/audit_work.go`, `taskworker/taskworker.go` — sweep + rollup tasks (+ hourly re-enabled export loop)
- `bringyourctl/main.go` — `stats migrate-202608 | sweep-providers | rollup-transfer | backfill | purge-samples | add-sample`
- consumer repos (removed-key cleanup): `connect/api/bringyour.yml`,
  `mmm/ur.io/api/bringyour.yml` (+ regenerated `react/src/data/openapi.js`,
  `api/build/api.html`), `mmm/ur.io/examples/web/web/bringyour.com/stats.js`,
  `web/web/bringyour.com/stats.js`, `web/web2/ur.xyz/astro/public/openapi.yml`
