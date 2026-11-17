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
stay until its generation drops (see "Retention by shard generation" below),
except its window clients, which go an hour past its deadline (see "Window
clients" below).
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
settling its reported contracts (`SettleProberShard`); nothing but its window
clients goes before its generation drops. A settled shard waits with
`next_cleanup_time` `'infinity'`.

**The drop** (`model.RemoveProberShardGenerations`, task
`remove_prober_shard_generations`) takes the dropped generations oldest first:

1. It walks the generation's contracts once and deletes its passes' zero-byte
   contracts: resolved and closed before the cutoff, every escrow settled, no
   sweep and no destination close with bytes. Such a contract adds nothing to
   any byte sum (the daily transfer rollup, the clock backfill, the subnet
   usage weight, payouts, payment plans, operator block awards). The capture
   trigger archives a credit-bearing contract's usage in the same statement.
   The rows deleted are the contract horizon reaper's own. The walk reads the
   primary key over the generation's contract id range, from 15 minutes before
   its first pass's creation to 15 minutes past its last pass's deadline, in
   batches of 5,000, and keeps the contracts whose payer is one of the
   generation's pass networks, which every prober contract's payer is. It
   reads each contract of the day once (about 116M on Main, at 3,000 a second
   by default: about 11 hours) and needs no other index; it replaced the read
   of each pass client's closed contracts from `transfer_contract_open_source_id`
   (`OPEN-SOURCE-ID-RETIRE.md`, option A).
2. Then, for each of its passes, it deletes the pass's identity in
   `ReapProberShard`'s transaction, which refuses a pass with an unresolved
   contract, an unsettled escrow, a pending debit, a connected client or a
   balance that is not a plain grant. A refused pass is retried after an hour,
   and a pass whose drop failed after a minute.
3. And it deletes the pass's registry row.

A run ends when its four-minute budget does. The walk's cursor goes into the
task's args, so the next run resumes where it stopped, and the retry of a
failed run resumes after its last committed batch. A pass waits, untaken, until
its generation's walk completes: the walk keeps contracts by the network its
registry row names. The walk serves the oldest dropped generation that still
has a pass to drop, so a refused pass never holds a newer generation's walk,
and a refused pass retried after its generation's walk completed drops without
a second walk; a contract the walk kept (for its bytes or an unsettled escrow)
is the contract horizon reaper's.

It keeps, for the retention that owns each: contracts that carry bytes (the
contract horizon reaper, because the transfer rollup can re-sum their day until
`TransferAuditRollupFirstDay` passes it); close evidence (its own retention);
provider work events and heads (guarded; the C4 partition's retention).

**Rotation.** A pass in flight never loses its identity: a generation drops only
after seven more have begun (the control cannot keep fewer than two), a pass
drops only once its deadline is six hours past, and the identity transaction
refuses anything still unresolved or connected.

**The control.** The drop is off until an operator sets the Redis hash
`{prober_retention}control` (read before every pass and every walk batch; a
run that cannot read it stops):

```
HSET {prober_retention}control mode dry_run          # count what a drop would delete
HSET {prober_retention}control mode on               # drop
HSET {prober_retention}control mode off              # stop within one pass or walk batch
HSET {prober_retention}control retainedGenerations 8 # 2..30 UTC days kept
HSET {prober_retention}control maxContractsPerSecond 200   # pass contracts the walk keeps a second, up to 2000; 0 is off
HSET {prober_retention}control maxWalkRowsPerSecond 3000   # contracts of any payer the walk reads a second, up to 20000; 0 is off
HSET {prober_retention}control maxShardsPerMinute 30        # passes dropped a minute, up to 600; 0 is off
HSET {prober_retention}control contracts off         # drop identities only, without a walk
HSET {prober_retention}control windows off           # stop the window step alone
HSET {prober_retention}control maxWindowShardsPerMinute 4  # passes the window step clears a minute, up to 120; 0 is off
DEL  {prober_retention}control                       # code defaults (off)
```

A dry run reads one page of due passes and walks the next generation read
only, from the walk's cursor when it is in progress, for one budget; it changes
nothing, the walk included. With contracts off, a generation's passes drop
without a walk, and that generation is never walked once contracts are on
again.

**Metrics.** `urnetwork_prober_retention_*`: the mode, the kept generations,
the cutoff, the oldest registered pass, the due backlog, the run's clock and
the creation time the walk has read up to (gauges); passes dropped, refused (by
reason) and failed; contracts deleted, kept for bytes, kept unsettled or busy;
contracts of any payer the walk read; archived contracts; deleted rows by
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

## Window clients

Each tunnel of a probe pass mints a derived client of the pass's own client
(the taskworker's in-process credentials) and retires it when the tunnel
closes. Measured on Main on 2026-11-17: about 4,700 a minute, 5,500 a pass and
6.8M a day, about 1.14 for each URL turn; a window lives about 16 seconds
(p50; p99 36 s). Until 2026-11-17 every retired window stayed until its pass's
generation dropped, about 54M rows at the eight-day steady state and about
166M waiting on the walk that day, with about 0.9 location reliability rows
each and a TLS certificate for each post-quantum turn's window.

**Why the windows are not reused.** A provider sees the window's client id in
its contracts and packets. Reusing a window for another provider would let
providers that compare notes recognize the prober as the one client that
visits many of them, and reusing it for the same provider's next turn would
let that provider recognize the prober when it returns; connect's own rule is
that an identity a provider may have seen never follows the window to another
provider. The extra mints within a turn (about one in eight) stay fresh
identities too: the tunnel retires each identity with its exact residency
(`ReuseFixedDestinationIdentities` off, `providertunnel`). So the step bounds
how long windows are kept, not how many are minted.

**The window step** (`model.RemoveProberShardWindows`, task
`remove_prober_shard_windows`, a chain beside the drop's) takes the passes in
the order of their creation. A pass is due an hour past its deadline. A due
pass that drained, whose funded contracts are all resolved and whose
balances' escrows are all settled (read directly: the drop's postponements
overwrite the settle sweep's `'infinity'` mark), loses its window clients and
the rows keyed by them: `client_tls_certificate`, `network_client_role`,
`network_client_provider_intent`, `network_client_location_reliability` and
`provide_key`, then their Redis state after the commit. At most 10,000 windows
go in one transaction. Their connections stay for the connection reap, eight
hours after they closed. A window with a connected connection or a hosted proxy
resource stays for the drop, which refuses such a pass as before. The pass's
own client, identity, contracts and registry row stay for its generation, so
the drop's walk, which finds a pass's contracts by its payer network, is
unchanged.

Nothing reads a retired window once its pass settled: its contracts carry
their own network ids (close, expiry, settlement and payout never read its
row, as for the hosted proxy's retired windows, which go a day after their
deactivation), it parents no client and earns nothing, and the location fold
cannot write its row again once it is gone. Its key history keeps its own
retention, which deletes a client's history only once its row is gone (the
delete trigger records a child gone at its deactivation). A post-quantum
turn's window publishes its key, about 0.95M rows a day since 2026-11-14 at
about 2.2 KB each; that history now goes seven days after the window's
deactivation instead of after its generation's drop.

A due pass that is not settled, or still live, is waited at: the run stops
there and the next run reads it again, after the idle interval. Six hours past
its deadline it is passed over, and its windows wait for its generation's
drop. A pass whose registry row another transaction holds ends the run, and
the next run comes back to it after the resume interval. A failed run hands its
cursor, before the failed pass, to the retry.

The step runs in the control's mode: off does nothing, dry run reads one page
and counts the windows a run would delete without moving its cursor, and on
deletes, at most `maxWindowShardsPerMinute` passes a minute (4 by default, 120
at most). `windows off` stops it alone. Main creates about one pass a minute,
so the default keeps up four times over, at about 380 window deletes a second
while behind; the backlog of about 39,000 passes of 2026-11-17 clears in about
nine days at the default, faster at a higher rate. Caught up, Main keeps about
an hour and a quarter of windows, about 0.35M rows instead of about 54M.

**Metrics** (the taskworker dashboard's prober shard retention row):
`urnetwork_prober_retention_window_shards_total{outcome}` (removed, waiting,
skipped, busy, gone, error, dry_run), `urnetwork_prober_retention_window_clients_total`,
the cursor's pass creation time
`urnetwork_prober_retention_window_cursor_timestamp_seconds` (caught up, about
an hour and a quarter behind now), the rows it deleted in
`urnetwork_prober_retention_deleted_rows_total{table}` and its transactions in
`urnetwork_prober_retention_batch_duration_seconds{step="windows"}`.

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
parallel 2 MiB streams through a provider's tunnel against Cloudflare's
public speed test (`qualityprobe/bandwidth`).

- **Apart from the quality probes.** Each pass runs in prober shard slot 254
  with a private network, credential parent and balance minted for the pass;
  every tunnel mints its own derived client. Its tunnels are ordinary
  clients' tunnels: none of the URL probe's traits (data-only admission
  without provider pings, unpublished client keys), and an allowlist of the
  CDN host alone. Its providers are a random draw of the connected, valid
  providers that are due (no CDN figure in seven days) and Online as the
  score export judges it, never the URL probe's due claims.
- **No credential in a tunnel.** The operator's own download endpoint
  (`/network/provider-bandwidth-test`) is not sampled. It authorizes with
  `X-UR-Operator-Secret`, the operator ingest secret, which authorizes every
  operator ingest call; even inside TLS it does not belong on a
  provider-controlled path. Sampling it would need a secret scoped to that
  one route, which the route does not accept today, and the target wired
  back (`bandwidth.OperatorTarget`) with that secret alone.
  `public_api_url` is not read by this probe.
- **Advisory.** Figures land in `provider_bandwidth` (one row per provider and
  source). Nothing that ranks or selects providers reads that table;
  `model/provider_bandwidth_readers_test.go` fails if anything but the probe's
  own due draw does.
- **Budget.** Every sample is reserved before it is measured, against the
  deployment-wide hourly budget (`provider_bandwidth.yml` `probes_per_bucket`,
  default 40, 16 MiB each). A pass every fifteen minutes takes its share
  (default ten providers) and stops at the first refusal. Probe traffic is
  paid from the pass's free shard credit; providers earn for it as for any
  traffic.
- **Settings.** The probe reads `provider_egress_probe.yml`: `full.bandwidth`
  (default true) switches it on or off, `full.bandwidth_timeout_seconds` is
  the per-target cap and `bandwidth_cdn_url` the target.
- **Metrics.** `urnetwork_egress_probe_bandwidth_bytes_per_second{source}`
  (histogram), `urnetwork_egress_probe_bandwidth_country_bytes_per_second{source,country}`
  (sum and count), `urnetwork_egress_probe_bandwidth_samples_total{source,result}`,
  `urnetwork_egress_probe_bandwidth_passes_total{result}`.

The URL probe's own timing of its sampled request is exported with no new
traffic: `urnetwork_url_probe_ttfb_seconds` and
`urnetwork_url_probe_body_bits_per_second` are fleet histograms without
labels, and `urnetwork_url_probe_country_ttfb_seconds{country}` and
`urnetwork_url_probe_country_body_bits_per_second{country}` a sum and a count
per country, two series per country rather than one per bucket.
