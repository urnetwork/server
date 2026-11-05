# Provider egress probing in taskworker

The main environment runs provider egress probing as durable recurring tasks,
not as a host service. `taskworker.InitTasks` seeds exactly `shard_count` rows.
Each row carries its shard index/count, both bounded batch configurations, the
control-plane endpoints, and its scheduler deadlines as ordinary task JSON.

Any healthy taskworker may lease any shard. A full batch schedules its
successor immediately; a partial or empty batch waits `idle_delay_seconds`.
Errors use the task system's durable retry and capped exponential backoff. This
means an offline edge or taskworker cannot own—and therefore cannot strand—a
slice of providers.

The settings live in `config/<env>/provider_egress_probe.yml`. Treat actual
deployed task arguments and process evidence as authoritative; a checked-in
example or tag selection does not prove current worker capacity.

The bounded Main task read at 2026-10-02 13:54 UTC found all eight canonical
URL shards with limit/concurrency 64, outer probe timeout 60 seconds, maximum
pass time 900 seconds, idle delay five seconds and result version 1. This is
512 configured URL slots, not 512 continuously busy or successful probes. Each
URL turn attempts one provider measurement. The target is ten accepted
measured outcomes per eligible provider in the rolling four hours, including
both measured successes and failures; local setup, control or contract failures
are not provider failures or quota credit. The URL success threshold is
inclusive `successes / measured outcomes >= 0.8`; three successes and two
measured failures do not pass it.

Taskworker release `2026.10.2-probe-timeouts-headroom+1060864100` implements
DNS five seconds, TCP connect three seconds, TLS handshake three seconds and
read-idle five seconds. The read-idle budget is not a total response-duration
limit. The request and pass owners remain bounded, and normal TLS authentication
is required. Each private shard's funding includes at least tenfold conservative
anticipated usage without depending on reclamation during its pass. The local
authenticated controller owns contract control; route admission and a ready
constructor still do not prove provider contact or successful contract creation.
The 16:14 UTC process witness qualified eight slots against that release's
source/build/configuration; it did not establish readiness, predecessor
retirement, useful throughput or rolling quota completion.

The configuration and capacity calculation below are retained **historical
2026-09-15 full/blackhole examples**, not the current Main URL geometry:

```yaml
enabled: true
shard_count: 4
idle_delay_seconds: 300
max_time_seconds: 1800
api_url: https://api.bringyour.com
platform_url: wss://connect.bringyour.com
public_api_url: https://api.bringyour.com
bandwidth_cdn_url: https://speed.cloudflare.com/__down

full:
  limit: 8
  concurrency: 2
  probe_timeout_seconds: 60
  all_destinations: false
  bandwidth: true
  bandwidth_timeout_seconds: 5

blackhole:
  limit: 250
  concurrency: 52
  probe_timeout_seconds: 15
```

That historical Main configuration kept four durable rows with a combined peak
of 52 probe workers per row,
or 208 slots without consuming more taskworker executor slots. A blackhole-only
pass can use all 52. While both queues are due, the independent drain reserves
the two full-probe workers and uses 50 blackhole workers per row: 200 blackhole
slots plus eight full slots fleet-wide, not 208 plus eight. At the 15-second
request deadline those two conditional timeout-only models are 49,920 and
48,000 blackhole checks per hour before setup and teardown. The latter retains
about 34% nominal capacity above the 35,796/hour rate required by the
107,387-provider fleet observed on 2026-09-15. The measured fleet rate remains
authoritative because overhead, queue residence, and fast successes change
realized throughput. Monitor §2.19 reports these sizing bounds separately from
the measured complete-sweep projection.

`enabled` defaults to `true` to preserve existing deployments. Set it
explicitly to `false` in a simulation or environment that must not contact the
provider control plane. Disabled startup seeds no probe shards and removes any
pending shard rows left by an older enabled generation, including claimed rows.
An already-dispatched row exits before reading `provider_egress.yml` or creating
any network client, and its post-step schedules no successor. Re-enabling and
running `taskworker init-tasks` seeds a fresh canonical shard set.

Each shard execution owns its network, client, and grant. When a pass ends it
drains its shard (every client deactivated, its credit ended) and settles the
contracts whose two final reports exist; it deletes nothing. `ProberBootstrap`
runs that settle sweep (`SettleDueProberShards`) immediately at startup and
every five minutes after a successful pass, fencing any live pass past its
deadline; it no longer replenishes the legacy shared identity. The shard's rows
stay until its generation drops (see "Retention by shard generation" below).
The operator ingest secret remains in `vault/<env>/provider_egress.yml`; it is
never serialized into task arguments.

The production `ProberBootstrap` target caps failure backoff at five minutes,
including its two-minute execution timeout. Failed attempts retain the same
pending task, error text, and increasing error count; only a successful attempt
runs the post-step that schedules its successor. Initial shorter retries,
drain/version-skew behavior, and all other targets keep their existing policy.
This cap requires only a taskworker binary update, with no schema change.

That delay begins when the worker finalizes the failed attempt. A worker can
still retain a completed Bootstrap's claim while another task in the same
evaluation batch runs. Inspect the unique `pending_task.run_once_key` row for
`["prober_bootstrap"]` to distinguish a future `run_at` after failure from a
live claim awaiting batch finalization. One fleet-wide recurring task can leave
Bootstrap metrics absent on the other workers. The retry cap does not release
live claims or shorten the remaining tasks' execution budgets.

Cleanup retains unresolved contracts, including disputed and zero-byte ones,
and retries normal settlement only when both actual final reports exist. Its
contract reads take the shard payer's contracts that have no outcome from the
two partial indexes that hold exactly those: a payer range of
`transfer_contract_unresolved_payer_transfer_byte_count` (undisputed) and a
scan of `transfer_contract_dispute_partial_create_time` (the open disputes of
every payer, a handful), followed by exact `contract_close` primary-key reads.
Because the generated `open` column is `dispute = false AND outcome IS NULL`,
the two arms are every unresolved contract of the payer. Neither index holds a
contract once it has an outcome, so a drained shard whose contracts settled
reads nothing. The reads no longer use
`transfer_contract_open_payer_network_id_transfer_byte_count`; before
2026-11 they walked both `open` values of it, which read the payer's whole
settled history on every retry (459 ms and about 5.5M tuples per call on Main).

These reads visit only the payer's unresolved contracts and the global open
disputes, even though settlement returns at most 32 contracts per pass. Do not
reuse them as a cheap negative check for the old shared account: its
unresolved set may be large. A bounded diagnostic can report per-balance unsettled
escrow blockers, but absence of those blockers does not exclude unanchored
zero-byte contracts or authorize removal of the shared identity.

The ordinary balance retention task also preserves every legacy singleton
grant for explicit retirement. Other expired grants are deleted only after a
locked, fresh PostgreSQL check finds no unsettled escrow, including zero-byte
and terminal-contract rows. It discovers the existing expiry range once and
processes up to 256 explicit balance IDs per transaction. This uses the existing
`transfer_balance_end_time`, balance primary key, and
`transfer_escrow_unsettled_balance_contract` indexes; no migration is required.
The streaming reader retains at most 256 IDs and holds a read snapshot for the
pass; the maintenance pool must allow at least two concurrent connections for
that reader and the short delete transactions.

## Retention by shard generation

The owner's rule (2026-11-01): the probe retention window is sharded, old
shards are dropped, and the retention is bounded, not cut. Probe volume is
unchanged.

**The shard key.** Every row a prober writes belongs to the private network of
one probe pass (`prober_shard_run`): the pass's network, user, authentication,
device and credit; the clients it mints with their provide keys, connections,
TLS certificates, roles and location reliability rows; and every contract it
funds, whose payer is that network and whose source (an origin) or destination
(a reply companion) is one of its clients.

**The window.** A generation is the passes created in one UTC day. The
retention keeps the newest eight generations, today's included, and drops the
oldest as a unit once a day begins. A pass ends by draining its shard and
settling its reported contracts (`SettleProberShard`); nothing deletes it
before its generation drops. A settled shard waits with `next_cleanup_time`
`'infinity'`.

**The drop** (`model.RemoveProberShardGenerations`, task
`remove_prober_shard_generations`) takes the passes of dropped generations
oldest first and, for each:

1. deletes its zero-byte contracts: resolved and closed before the cutoff,
   every escrow settled, no sweep and no destination close with bytes. Such a
   contract adds nothing to any byte sum (the daily transfer rollup, the clock
   backfill, the subnet usage weight, payouts, payment plans, operator block
   awards). The capture trigger archives a credit-bearing contract's usage in
   the same statement. The rows deleted are the contract horizon reaper's own.
2. deletes its identity in `ReapProberShard`'s transaction, which refuses a pass
   with an unresolved contract, an unsettled escrow, a pending debit, a
   connected client or a balance that is not a plain grant. A refused pass is
   retried after an hour, and a pass whose drop failed after a minute.
3. deletes its registry row.

A run ends when its four-minute budget does; a pass whose contracts the budget
interrupted keeps its identity until the next run finishes them, since a pass's
contracts are found through its clients.

It keeps, for the retention that owns each: contracts that carry bytes (the
contract horizon reaper, because the transfer rollup can re-sum their day until
`TransferAuditRollupFirstDay` passes it); close evidence (its own retention);
provider work events and heads (guarded; the C4 partition's retention).

**Rotation.** A pass in flight never loses its identity: a generation drops only
after seven more have begun (the control cannot keep fewer than two), a pass
drops only once its deadline is six hours past, and the identity transaction
refuses anything still unresolved or connected.

**The control.** The drop is off until an operator sets the Redis hash
`{prober_retention}control` (read before every pass; a run that cannot read it
stops):

```
HSET {prober_retention}control mode dry_run          # count what a drop would delete
HSET {prober_retention}control mode on               # drop
HSET {prober_retention}control mode off              # stop within one pass
HSET {prober_retention}control retainedGenerations 8 # 2..30 UTC days kept
HSET {prober_retention}control maxContractsPerSecond 200   # up to 2000; 0 is off
HSET {prober_retention}control maxShardsPerMinute 30        # passes dropped a minute, up to 600; 0 is off
HSET {prober_retention}control contracts off         # drop identities only
DEL  {prober_retention}control                       # code defaults (off)
```

**Metrics.** `urnetwork_prober_retention_*`: the mode, the kept generations,
the cutoff, the oldest registered pass, the due backlog and the run's clock
(gauges); passes dropped, refused (by reason) and failed; contracts deleted,
kept for bytes, kept unsettled or busy; archived contracts; deleted rows by
table; step durations; runs by why they stopped. While the drop keeps up, the
oldest registered pass is within a generation of the cutoff. Each taskworker
keeps the gauges of the last run it made, and a run publishes its clock with
the backlog it read, so a reader takes the taskworker with the newest clock.
The taskworker dashboard's prober shard retention row does: its stats read the
taskworker whose run started last, and only while that run started within 30
minutes.

**Settling.** `ProberBootstrap` runs `SettleDueProberShards` every five minutes:
it fences a live pass past its deadline in any generation and settles drained
passes of kept generations. It deletes nothing.

## Network boundaries

Direct control-plane calls to `api.bringyour.com` and
`connect.bringyour.com` are forced to IPv4, matching the hosted proxy. Probe
destinations never use that client. Their HTTP dialer is backed only by the
selected provider's userspace TUN, its host allowlist is closed, plaintext HTTP
and redirects are refused, and DNS has no local fallback. The host's normal LAN
default route is therefore safe: there is no socket-level path from a probe
request to that route.

## Changing shard count

Run `taskworker init-tasks` as normal after deploying configuration. Existing
rows whose stored shard count is stale perform no network work. Their post-step
replaces still-valid indices with current arguments and retires indices outside
the new range. The new `RunOnce` rows cover every index in the new range, so a
change converges without a permanent duplicate or gap.

The URL probe's `shard_count` is at most 254: prober shard slot 254 is the
active bandwidth probe's and slot 255 the capability probe's.

## Active bandwidth samples

URL probes are sampled-only (FP2FIX.md): a quality tunnel carries randomized
destination-pool requests and never a fixed target, so a modified provider
cannot recognize the prober and serve it better. Provider capacity is sampled
by a separate task, `ProviderBandwidthProbe`
(`taskworker/work/provider_bandwidth_probe_work.go`), which measures eight
parallel 2 MiB streams per target through a provider's tunnel against the
operator's `/network/provider-bandwidth-test` and Cloudflare's public speed
test (`qualityprobe/bandwidth`).

- **Apart from the quality probes.** Each pass runs in prober shard slot 254
  with a private network, credential parent and balance minted for the pass;
  every tunnel mints its own derived client. Its tunnels are ordinary
  clients' tunnels: none of the URL probe's traits (data-only admission
  without provider pings, unpublished client keys), and an allowlist of the
  two target hosts alone. Its providers are a random draw of the connected,
  valid providers that are due (no CDN figure in seven days) and Online as
  the score export judges it, never the URL probe's due claims.
- **Advisory.** Figures land in `provider_bandwidth` (one row per provider and
  source). Nothing that ranks or selects providers reads that table;
  `model/provider_bandwidth_readers_test.go` fails if anything but the probe's
  own due draw does.
- **Budget.** Every target is reserved before it is measured, against the
  deployment-wide hourly budget (`provider_bandwidth.yml` `probes_per_bucket`,
  default 40, 16 MiB each). A pass every fifteen minutes takes its share
  (default five providers, two targets each) and stops at the first refusal.
  Probe traffic is paid from the pass's free shard credit; providers earn
  for it as for any traffic.
- **Settings.** The probe reads `provider_egress_probe.yml`: `full.bandwidth`
  (default true) switches it on or off, `full.bandwidth_timeout_seconds` is
  the per-target cap, `bandwidth_cdn_url` and `public_api_url` are its
  targets. The operator target needs the ingest secret from
  `vault/<env>/provider_egress.yml`; without it the CDN is sampled alone.
- **Metrics.** `urnetwork_egress_probe_bandwidth_bytes_per_second{source}`
  (histogram), `urnetwork_egress_probe_bandwidth_country_bytes_per_second{source,country}`
  (sum and count), `urnetwork_egress_probe_bandwidth_samples_total{source,result}`,
  `urnetwork_egress_probe_bandwidth_passes_total{result}`. The URL probe's own
  timing of its sampled request is exported by country as
  `urnetwork_url_probe_ttfb_seconds{country}` and
  `urnetwork_url_probe_body_bits_per_second{country}`, with no new traffic.
