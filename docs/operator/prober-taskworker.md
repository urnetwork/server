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

Each shard execution owns its network, client, and grant. `ProberBootstrap`
runs cleanup immediately at startup and every five minutes after a successful
pass; it no longer replenishes the legacy shared identity. The operator ingest
secret remains in `vault/<env>/provider_egress.yml`; it is never serialized into
task arguments.

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
contract reads use both `open` values of the existing full
`transfer_contract_open_payer_network_id_transfer_byte_count` index, followed
by exact `contract_close` primary-key reads. Verify that full index is valid
and ready before deploying this query change; no new migration is required.
The unresolved-payer partial index excludes disputes and cannot prove that an
account is safe to remove.

These reads visit only the shard payer's history. Their work is proportional
to that history, even though settlement returns at most 32 contracts per pass.
Do not reuse them as a cheap negative check for the old shared account: its
history may be large. A bounded diagnostic can report per-balance unsettled
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
