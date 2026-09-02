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
  concurrency: 4
  probe_timeout_seconds: 15
```

The prober identity is created and refreshed by the immediate recurring
`ProberBootstrap` task. The operator ingest secret remains in
`vault/<env>/provider_egress.yml`; it is never serialized into task arguments.

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
