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

The settings live in `config/<env>/provider_egress_probe.yml`:

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

Main keeps four durable rows with a combined peak of 52 probe workers per row,
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
