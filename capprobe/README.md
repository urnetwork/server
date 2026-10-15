# capprobe — the modern provider gate's capability probe

The FP2 modern provider gate ([FP2FIX.md](../FP2FIX.md#modern-provider-gate-2026-10-14))
holds Quality and Speed to providers that negotiate end-to-end post-quantum
encryption with a client and advertise resizable transfer windows. This
package measures both, provider by provider, and records what it saw in
`provider_modern_evidence` (migration 836). It is a recurring taskworker task,
`work.CapabilityProbe`, and shares nothing with the URL probe but the
control-plane endpoints: its own prober shard (slot 255: a private network,
credential parent and balance), tunnels, transport budget, concurrency and
metrics, and none of the URL probe's due queue, quota or run history.

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
  the platform's store when no contract carries it.

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

## Settings

`capability_probe.yml`, read at every pass; without it the defaults apply.

| Key | Default | |
| --- | --- | --- |
| `enabled` | `true` | false runs no pass; the task rechecks every ten minutes and keeps its sweep |
| `concurrency` | 16 | providers measured at once |
| `limit` | 64 | providers one pass measures |
| `page_size`, `max_pages_per_pass` | 1024, 16 | the sweep's bounded reads |
| `measure_timeout_seconds` | 120 | at least the 60 s establishment window plus 15 s |
| `seal_grace_seconds` | 10 | |
| `close_timeout_seconds` | 30 | a tunnel's graceful teardown |
| `modern_refresh_hours`, `other_refresh_hours` | 72, 72 | each plus the sweep interval must be under the seven-day staleness window |
| `min_sweep_interval_minutes` | 360 | |
| `pass_interval_seconds` | 5 | |
| `max_time_seconds` | 900 | a pass's budget, the task's max time |

Lowering `other_refresh_hours` returns an updated provider to Quality and
Speed sooner once the gate is on, at the cost of measuring every non-modern
provider that often; most measurements of a provider without the responder
take the whole establishment window.

## Metrics

`urnetwork_capability_probe_*`: passes by result, selection by stage,
providers by outcome, readings per capability, measurement duration, in
flight, teardown errors, sweeps and the last sweep's duration, and the last
pass. The egress probes dashboard charts them all; SIGNALS.md §2.19g says
what each means. The probe's derived clients are minted and retired by the
same in-process credential owner as the URL probe's, so
`urnetwork_egress_probe_internal_credentials_total` counts both.
