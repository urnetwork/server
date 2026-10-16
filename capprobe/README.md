# capprobe — the modern provider gate's capability probe

The FP2 modern provider gate ([FP2FIX.md](../FP2FIX.md#modern-provider-gate-2026-10-14))
holds Quality and Speed to providers that negotiate end-to-end post-quantum
encryption with a client and advertise resizable transfer windows. This
package measures both, provider by provider, and records what it saw in
`provider_modern_evidence` (migration 836). It is a recurring taskworker task,
`work.CapabilityProbe`, and shares nothing with the URL probe but the
control-plane endpoints: its own prober shard (slot 255: a private network,
credential parent and balance), tunnels, each with a platform transport
budget of its own as each URL probe's tunnel has, concurrency and metrics,
and none of the URL probe's due queue, quota or run history.

It ships switched off: until `capability_probe.yml` says `enabled: true` and
a taskworker starts with it, nothing is scheduled (see "Switching it on").

## A pass

One task row runs one pass at a time. A pass continues a **sweep** over the
connected, valid providers in client id order (`Sweep`, carried in the task's
arguments), a page at a time (`model.GetProviderCapabilityProbePage`). A
provider is **due** without evidence, with modern evidence older than
`modern_refresh_hours`, or with other evidence older than
`other_refresh_hours`. Of the due ones, the pass keeps those the egress rules
admit to Quality or Speed **before** the gate
(`model.FilterProviderCapabilityProbeEligible`, judged in chunks of twice the
pass's remaining room), so a provider the gate takes out for want of
evidence is still measured and can return. It measures up to `limit` of
them, `concurrency` at once, starting none it could not finish before its
deadline, and the next pass resumes after the last one it started. The
selection reads the database only (`SelectPass`); a pass that waits or finds
nothing to measure finishes there (`FinishPass`) and takes no prober shard.
A sweep that reaches the population's end completes; the next starts
`min_sweep_interval_minutes` after this one started, which also spaces the
retries of a provider that gave no evidence. Between sweeps a waiting pass
runs every ten minutes and reads nothing, so the last pass time tells a
waiting probe from a stopped one. No other per-provider state is kept.

## A measurement

Each provider gets a short tunnel of its own (`providertunnel.Config.CapabilityObservation`):

- its window pings the provider as any provider window does (no data-only
  probe), which also opens the end-to-end session: the first send starts the
  ClientHello;
- its clients run `EncryptionModeOpportunistic` and record into the
  measurement's own `connect.PeerCapabilityRecorder` (connect DESIGNNOTES
  §3.13);
- its clients are admitted only after the platform has processed their key
  registration (`ClientKeyRegistrationRequired`), so the provider can resolve
  the probe client's identity key from a contract or its `/key/<client_id>`
  fetch from the first frame, and the probe resolves the provider's key from
  the platform's store when no contract carries it;
- its window dials the provider again after failing to admit a client no
  sooner than about `measure_timeout_seconds` later (`RedialMin`, which
  connect's jitter shortens by up to half), so a measurement attempts its
  provider about once rather than minting, registering and retiring a
  derived client at every resize pass. A client the window admitted and then
  lost is dialed again at once, and that measurement has lost its tunnel
  anyway. The URL tunnel's re-dial stays unpaced.

The recorder holds only what the probe's own ClientHello sessions saw: a
negotiated session is the provider's responder answering the probe, with the
group it chose from the probe's offer. An attempt is a ClientHello the send
path accepted, and an acknowledgement that advertises a window is recorded
with it in one step, so a window is never read as missing while it is on its
way. The measurement reads the recorder every second and stops once the
reading is conclusive: an acknowledgement read the window, and the session
negotiated (sealed, or given `seal_grace_seconds` to), or its ClientHello
went unanswered through a whole establishment window (`TlsTimeout`, 60 s)
counted from both the send path's acceptance of the first ClientHello and the
first observed acknowledgement. It stops at `measure_timeout_seconds`
whatever the reading, at the tunnel's loss and at the pass's end, then tears
the tunnel down. A provider that cannot open the probe's sealed frames
therefore holds a slot for at most the timeout and the teardown. The URL
tunnel's encryption stays off.

The pinned provider's window holds one client, and the recorder merges what
every client of the tunnel saw. So a capability tunnel is also lost when a
second client takes its window (`providertunnel.ErrTunnelClientReplaced`):
the first attempt and acknowledgement the measurement read belong to a
client that is leaving, and a replacement's ClientHello would be read
against a window that began before it was sent. A measurement whose tunnel
was lost by its final read keeps a negotiated session and the window, but
concludes no unanswered ClientHello (outcome `tunnel_lost`).

| Reading | Recorded as |
| --- | --- |
| sealed or unsealed session, post-quantum group | `E2eNegotiated`, `PostQuantum` |
| sealed or unsealed session, classical group | `E2eNegotiated`, not post-quantum |
| no handshake completed for the whole window after a ClientHello, pings acknowledged | not negotiated |
| acknowledgement with a window / without one | `WindowAdvertised` true / false |
| no acknowledgement, no attempt, or an attempt still in its window | nothing |

Evidence is recorded only when **both** capabilities read conclusively; a
capability that was not tested is never recorded, as a negative or
otherwise. The model reads an unsealed post-quantum session as post-quantum;
the metrics keep sealed and unsealed apart for the shadow review.

## Negative readings

A negative reading, evidence that is not modern, takes a provider out of
Quality and Speed once the gate is on, so two guards keep a passing fault
from recording one.

- **Breaker.** A pass records a negative reading only once one of its
  measurements has negotiated an end-to-end session with some provider.
  Until then the reading waits; a pass that negotiates none at all, which is
  how a fault on the probe's own side shows across the fleet, records none of
  them (outcome `negative_withheld`, `breaker_trips_total`). Those providers
  stay due and the next sweep measures them again. A small pass whose
  providers all lack the responder withholds too, which costs a measurement a
  sweep, never a wrong record.
- **Held downgrades.** A negative reading over modern evidence that is still
  fresh is held back (`model.RecordProviderModernEvidenceHoldingDowngrade`,
  outcome `downgrade_held`): the row keeps its modern reading and stays due,
  and the sweep carries the hold in the task's arguments (`HeldDowngrades`,
  at most 256). A negative reading of that provider in a later sweep, at
  least `min_sweep_interval_minutes` on, confirms the downgrade and records
  it; any reading recorded, a modern one included, retires the hold, and a
  hold as old as the seven-day staleness window, by when the evidence has
  gone stale anyway, is dropped. `downgrades_total{result}` counts both. A
  pass whose evidence write fails is retried a minute later from the same
  place, and the retry carries the holds as that pass left them, those it
  retired gone and those it held added. A failure the task cannot
  checkpoint, one with a cancellation among its causes, one joined with a
  prober shard cleanup failure, or arguments of about 38 holds or more,
  keeps the holds the pass started with, as a crashed pass does.

## Settings

`capability_probe.yml`, read by every taskworker's startup schedule and at
every pass; without it the defaults apply, and the probe is off. A pass
runs on the max time its task row was scheduled with, which rides in the
task's arguments.

| Key | Default | |
| --- | --- | --- |
| `enabled` | `false` | true: a taskworker starting with it schedules the probe; false: nothing is scheduled, and a chain already running ends at its next pass |
| `concurrency` | 16 | providers measured at once |
| `limit` | 64 | providers one pass measures |
| `page_size`, `max_pages_per_pass` | 1024, 16 | the sweep's bounded reads |
| `measure_timeout_seconds` | 120 | at least the 60 s establishment window plus 15 s |
| `seal_grace_seconds` | 10 | |
| `close_timeout_seconds` | 30 | a tunnel's graceful teardown |
| `modern_refresh_hours`, `other_refresh_hours` | 72, 72 | each plus the sweep interval must be under the seven-day staleness window |
| `min_sweep_interval_minutes` | 360 | |
| `pass_interval_seconds` | 5 | |
| `max_time_seconds` | 900 | a pass's budget and its task row's max time, both as of when the pass was scheduled |

Lowering `other_refresh_hours` returns an updated provider to Quality and
Speed sooner once the gate is on, at the cost of measuring every non-modern
provider that often; most measurements of a provider without the responder
take the whole establishment window.

## Switching it on

The probe ships off, and a switched-off probe schedules nothing: no task
row, no prober shard, no derived client, no reads but its settings. A
taskworker turns it on in its startup schedule (`ScheduleCapabilityProbe`),
which schedules the probe's one row due at once when it reads
`enabled: true`. A taskworker container keeps the config version it was
created with (warp mounts the newest published config only when it creates
a container, and config releases go out with `restart: false`), so a running
taskworker never sees a changed `capability_probe.yml`, in either direction:
switching the probe on or off takes a taskworker restart.

1. Add `main/capability_probe.yml` to the config repository; every key it
   leaves out keeps its default:

   ```yaml
   # server/capprobe, the FP2 modern provider gate's capability probe
   enabled: true
   ```

2. Release the config through the config-updater, built with
   `--config_restart=no` as usual, and wait until every taskworker host has
   published the new `/srv/warp/main/config/<version>` (not `.tmp`), about
   100-125 s per host.
3. Restart every taskworker block (`systemctl restart warp-main-taskworker-g<N>`
   on each host) and check that each container's `WARP_CONFIG_VERSION` is the
   new version. Until every taskworker runs the new config, a pass claimed by
   one still on the old config runs nothing and ends the chain; a taskworker
   with the new config schedules it again within ten minutes (below), and
   the monitor's `capability-probe` signal reports the disabled pass.
4. The first taskworker that starts with the new config schedules the row,
   due at once; a worker claims it a second or so later, and the first pass
   starts a sweep at once: it reads its pages, takes prober shard 255 and
   starts measuring. A modern provider reads in seconds, one without the
   responder after its 60 s establishment window, and none takes longer than
   `measure_timeout_seconds` (120). `passes_total{result="measured"}` and
   `last_pass_timestamp_seconds` show it (egress probes dashboard, capability
   probe row).

To switch it off, set `enabled: false` (or remove the file) and release and
restart the same way: the next pass on a restarted taskworker runs nothing
and schedules no successor, and its sweep and held downgrades go with it.
Until every taskworker has restarted, one still on the old config can
schedule the chain again, and the next pass on a restarted one ends it.

While the probe is switched on, each taskworker also checks every ten
minutes that the chain has its task row, and schedules a pass due at once
when it has none (`chain_restarts_total`, logged): a chain that a stale
taskworker, a lost row or maintenance ended starts a new sweep within ten
minutes. A row that exists, waiting or claimed, is left as it is.

So while any taskworker runs a config that switches the probe on, deleting
the chain's `pending_task` row does not stop the probe: the check schedules
it again within ten minutes. To stop it, switch it off as above, with a
config release that sets `enabled: false` and a restart of every taskworker.

## After maintenance

After any taskworker restart, config release, or maintenance that touches
`pending_task` (a task table cleanup, a database restore or failover),
while `capability_probe.yml` switches the probe on:

1. The probe's chain has exactly one row, without a stored error that
   repeats:

   ```sql
   SELECT task_id, run_at, reschedule_error_count, reschedule_error
   FROM pending_task
   WHERE run_once_key = '["capability_probe"]';
   ```

   No row means the chain has ended: a taskworker with the probe switched on
   schedules it again within ten minutes, or at once when one restarts.
2. Every taskworker runs the current config (`WARP_CONFIG_VERSION`) and
   reports `urnetwork_capability_probe_passes_total{result="disabled"}` at
   zero.
3. `urnetwork_capability_probe_last_pass_timestamp_seconds` advances within
   fifteen minutes, and the monitor's `capability-probe` signal is healthy
   (SIGNALS.md §2.19g).

## Metrics

`urnetwork_capability_probe_*`: passes by result, selection by stage,
providers by outcome, readings per capability, held and confirmed
downgrades, breaker trips, measurement duration, in flight, teardown errors,
funding readiness failures, prober shard cleanup errors, sweeps and the last
sweep's duration, the last pass, and the chains a taskworker found without
their row and scheduled again. The egress probes dashboard charts them
all; SIGNALS.md §2.19g says what each means. The probe's derived clients are minted and retired by the
same in-process credential owner as the URL probe's, so
`urnetwork_egress_probe_internal_credentials_total` counts both.
