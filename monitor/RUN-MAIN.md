# Main monitor root-cause agent harness

This is the standing operating contract for an agent that runs the production
monitor, validates every finding, fixes root causes, extends the signal catalog,
and repeats. The Go monitor described in [MONITOR.md](MONITOR.md) remains a
deterministic, read-only detector. The agent using this harness is the separate
diagnostic and repair system that consumes its structured Markdown alerts.

The harness is intentionally continuous. A quiet snapshot, a code commit, or a
successful deployment is not an end condition. Keep one authoritative monitor
loop alive, re-evaluate open findings after their required observation windows,
and continue until the operator ends the run or its requested duration expires.

## Agent contract

Act as the production monitor root-cause agent. Repeatedly:

1. run every registered monitor signal against the intended environment;
2. preserve the emitted alert and its observation time;
3. corroborate the finding at the direct source of truth;
4. distinguish a real incident from observation loss, expected operational
   state, a secondary symptom, stale alert guidance, or a monitor defect;
5. establish the causal mechanism and the exact affected boundary;
6. implement the smallest durable root-cause fix in the owning repository;
7. add deterministic regression coverage, including a synthetic reproduction;
8. update [SIGNALS.md](SIGNALS.md) and the relevant `signal_<key>.go` probe when
   the learned healthy/broken contract or diagnostic playbook changed;
9. pass the focused, package, race, and release-relevant test gates;
10. identify the exact artifact, migration, configuration, hardware, or
    operator action required for production verification;
11. after that boundary is present in production, observe for the signal's
    full verification window and prove recovery from direct evidence; and
12. checkpoint the tested work, promote the new monitor binary without an
    observation gap, and continue with the remaining findings.

Do useful independent work while a build, rollout, migration, router change,
capacity addition, or observation window is pending. A blocked finding does
not pause unrelated probes or investigations.

## Non-negotiable safety boundaries

- Monitoring and diagnosis are read-only in production. Do not deploy, push,
  restart a service, change a router, alter Vault, apply a migration, mutate
  PostgreSQL or Redis, cancel work, fund an account, or correct a payout unless
  the operator explicitly authorizes that production mutation.
- `vault/<env>/monitor.yml` is authoritative for operator-disabled hosts. Never
  contact a host whose current inventory entry has `disabled: true`. A user's
  live pause or offline declaration is stronger than stale repository state;
  use the focused exclusion flag until inventory catches up.
- Before any authorized privileged host mutation, resolve the connection
  address from the current owning inventory rather than an alert, narrative
  note, remembered endpoint, or `root/servers/table` (which is a reference
  catalog, not an endpoint authority). Require the remote hostname to match
  the selected inventory name before invoking `sudo`; an absent, ambiguous,
  or mismatched result fails closed without running the mutation.
- Never weaken, exclude, or suppress a signal merely to make the alert file
  quiet. A temporary exclusion must name its operator reason, owner, start
  time, and re-enable condition in the run ledger.
- Never print credentials, private keys, bearer tokens, signed URLs, raw
  customer identifiers, balances, contract IDs, or stream labels into a shell
  transcript, alert, test failure, commit, or agent response. Feed credentials
  on stdin through the existing monitor transports.
- Treat dashboards as navigation aids, not proof. Deployment state comes from
  the running unit/container and its immutable artifact identity; database,
  Redis, network, kernel, and process state come from those systems directly.
- Use bounded queries and commands with hard timeouts. Do not hot-loop a
  failing command or implement `while true` around `-once`.
- Do not use a restart, larger timeout, larger queue, broader retry, alert
  suppression, or manual data correction as a root-cause fix without evidence
  that it repairs the causal mechanism and preserves correctness.
- Preserve unrelated and pre-existing working-tree changes. Do not amend,
  force-push, rebase, or push by default. Keep changes in different repositories
  in separate tested commits.
- Software can reduce resource use and prevent unsafe overlap; it cannot create
  RAM, CPU, host slots, liquidity, routable addresses, or per-proxy active-client
  slots. Keep software, operator, finance, network, and hardware closure gates
  distinct.

## Bootstrap and preflight

Run from the server repository and make the target explicit:

```sh
cd /path/to/urnetwork/server
export WARP_ENV=main
```

Before the first production command:

1. inspect the current branch and `git status --short --branch` in every
   repository likely to be changed;
2. confirm `WARP_ENV`, the standard WARP home/config resolution, SSH identity
   paths, and whether this machine reaches hosts over `overlay` or `lan`;
3. load the current monitor inventory and enumerate disabled hosts without
   displaying secrets;
4. verify that the current `services.yml` version is the intended active
   topology; and
5. resolve each host endpoint from its current owning inventory and verify its
   remote hostname before any privileged action; and
6. run the local monitor tests before trusting a newly built detector.

The normal workstation mode is `overlay`; use `lan` only from a host with the
configured LAN routes. An explicit `-ssh-key` may be repeated when the SSH
configuration does not already select the identities.

```sh
go test ./monitor
go test -race ./monitor
go vet ./monitor
go build -o /tmp/urnetwork-monitor-preflight ./cli/monitor
```

Treat a failing preflight as a monitor/repository problem to diagnose, not as a
production alert.

## One-shot diagnostic snapshot

Use one-shot mode for an initial inventory, a focused before/after snapshot, or
manual diagnosis:

```sh
monitor_snapshot_dir=$(mktemp -d /tmp/urnetwork-monitor-snapshot.XXXXXX)
go build -o "$monitor_snapshot_dir/monitor" ./cli/monitor
WARP_ENV=main "$monitor_snapshot_dir/monitor" -mode overlay -once \
  >"$monitor_snapshot_dir/alerts.md" \
  2>"$monitor_snapshot_dir/stderr.log"
```

`-once` runs every registered signal serially, emits every current band
violation, and exits nonzero if any probe itself could not execute. It
deliberately bypasses consecutive-cadence sustain gating, so label its output a
snapshot rather than a page. Preserve both files even on a nonzero exit: the
Markdown contains structured visibility alerts and stderr can distinguish an
observation-path failure.

The CLI does not have an include-only flag. For reusable focused execution,
call `Monitor.RunSignal` by semantic key from Go. From the CLI, repeated
`-exclude-signal <key-or-number-or-id>` is a deliberate diagnostic tool, not a
way to certify overall health.

## Start the authoritative continuous watcher

Build a unique immutable binary and keep its artifacts together:

```sh
monitor_run_dir=$(mktemp -d /tmp/urnetwork-monitor-watch.XXXXXX)
go build -o "$monitor_run_dir/monitor" ./cli/monitor
shasum -a 256 "$monitor_run_dir/monitor" >"$monitor_run_dir/binary.sha256"
WARP_ENV=main "$monitor_run_dir/monitor" -mode overlay \
  >"$monitor_run_dir/alerts.md" \
  2>"$monitor_run_dir/stderr.log"
```

Run that final command in a durable attached execution session so the agent can
poll it, send a graceful signal, and prove it remains alive. Record in a run
ledger outside the repository:

- binary path and SHA-256;
- process ID and execution-session handle;
- environment and address mode;
- start time in UTC and operator timezone;
- alert and stderr paths;
- current server commit and dirty state;
- active service-tail count derived from the current `services.yml` inventory;
- every signal or edge-IPv6 exclusion and its reason; and
- pending deployment boundaries and verification deadlines.

Continuous mode runs signals at their own cadences, limits ordinary probe
concurrency, applies each alert's sustain count, and replaces the bounded log
probe with one standing `warpctl logs ... -f` stream per active service. Confirm
the watcher is alive and owns every expected standing stream. A live parent
with missing children is not healthy observation coverage.

For a temporary exact-edge pause, preserve all other coverage:

```sh
WARP_ENV=main "$monitor_run_dir/monitor" -mode overlay \
  -exclude-edge-ipv6-host HOSTNAME
```

Unknown exclusions fail closed. Inventory-disabled hosts are excluded across
all probes automatically; `-exclude-edge-ipv6-host` disables only that host's
exact public IPv6 paths.

## Safe watcher promotion

Any monitor code, catalog, inventory-loading, tailer, or alert-rendering change
requires a newly built watcher. Promote it as a controlled handoff:

1. run focused tests, `go test ./monitor`, `go test -race ./monitor`, and
   `go vet ./monitor`;
2. build a new uniquely named binary and record its hash;
3. start it alongside the old watcher;
4. prove the new parent remains alive, loads the expected signals, and starts
   every active standing service tail;
5. wait for its first complete standing-log cadence and at least one ordinary
   probe result;
6. gracefully stop the old watcher through its execution session;
7. prove the new watcher remains alive and the old watcher and all of its tail
   children are gone; and
8. update the ledger so there is exactly one authoritative watcher.

Overlap is allowed only for this bounded handoff. Prolonged duplicate watchers
distort log coverage and add production load; stopping the old watcher before
the new one is proven creates an observation gap.

## Alert validation loop

Process each new alert identity and each material update in this order:

1. **Freeze the claim.** Record alert identity, class, target, frame, observed
   time, baseline, observed value, evidence, active artifact, and monitor hash.
2. **Prove visibility.** Confirm the probe reached the intended source and
   parsed the current identity. `cannot-observe` is unknown state and may itself
   be an incident; it is never evidence that the underlying service is healthy.
3. **Classify.** Mark the finding as a confirmed fault, expected operator state,
   secondary symptom, stale/historical guidance, monitor false attribution, or
   unresolved. Do not erase a confirmed observation just because its diagnosis
   changed.
4. **Corroborate independently.** Re-run a bounded read-only check at the direct
   source, preferably through a different observation path. Match target,
   timestamps, process/container identity, and units.
5. **Establish the boundary.** Identify the last known healthy sample, first
   broken sample, actual process start/deployment time, configuration generation,
   and whether all relevant blocks or hosts converged.
6. **Build the causal chain.** Explain how the proposed cause produces the
   measured symptom. Timestamp correlation alone is insufficient; seek a
   discriminator or negative control that rules out competing causes.
7. **Assign ownership.** Separate software, migration, configuration, router,
   operator, finance/provider, external dependency, and hardware capacity work.
8. **Define closure before changing anything.** State the exact action and a
   measurable post-boundary verification window that would falsify or confirm
   the fix.

Useful source-of-truth pairings are:

| Finding | Primary source | Independent corroboration |
|---|---|---|
| deployed version or rollout | running unit/container, image digest, embedded source revision | publish clock plus per-host process start and artifact ancestry |
| PostgreSQL state | direct primary connection on 5432 and `pg_stat_*` | bounded task/service logs at the same timestamp; probe 6432 separately |
| Redis state | each node's own `INFO`, `CLUSTER NODES`, and key metadata | host listener/process/cgroup state and PostgreSQL durable owner state |
| host memory, OOM, or UDP loss | kernel journal, `/proc`, cgroup files, `nstat`, and socket queues | process generations, overlap timeline, swap and application metrics |
| host or edge address | live interface and policy-routing state | active `services.yml`, host config, router path, and exact-origin probes |
| log loss | standing tail health and privacy-safe drop summaries | bounded absolute-window reconciliation per service/block and direct host journal |
| metrics identity | fresh process-emitted series with host/block/instance labels | live process start, listener, ring membership, and scrape age |

Use the existing SSH and Warpctl transports and the commands documented in
`SIGNALS.md`; do not improvise a less safe secret path. Never contact a disabled
host while trying to improve denominator coverage.

## Root-cause bar

A root cause is established only when the record contains all of the following:

- the precise failing component and generation;
- the causal mechanism, not merely a correlated alert;
- a bounded production discriminator or deterministic local reproduction;
- an explanation for the healthy control or why only the framed targets fail;
- the first-bad or deploy/config boundary when one exists;
- a fix that removes the cause while retaining correctness and safety guards;
- prerequisites outside software; and
- a post-fix measurement capable of proving both recovery and non-regression.

If evidence instead shows that the monitor selected the wrong target, used a
stale denominator, conflated historical and current artifacts, inferred a cause
from non-specific text, leaked sensitive context, or prescribed an already
deployed fix, repair the monitor and catalog as a first-class root-cause issue.

## Implementing fixes

For a product defect, change the owning repository and add a deterministic test
that fails for the reproduced mechanism and passes for the fix. Keep a monitor
probe as the production guard; do not move application behavior into
`server/monitor`.

For a monitor defect or new production invariant:

1. assign or retain the numbered `SIGNALS.md` section;
2. give it a short descriptive one-word or compound-word `Probe:` key;
3. use `signal_<key>.go` and `signal_<key>_test.go`, converting hyphens in the
   key to underscores and never putting the catalog number in the filename;
4. put `// SIGNALS.md §X.Y ...` in the probe's documentation;
5. implement the reusable `Signal` contract and return structured `Alert`
   values with human-readable Markdown fields;
6. keep shared connectors, parsers, settings, renderers, and other reusable
   utilities inside `server/monitor`; keep `server/cli/monitor` as wiring only;
7. register the constructor explicitly in `NewSignals`; and
8. add a synthetic failing fixture plus healthy and ambiguity controls.

The registry tests enforce the semantic file/key/source-comment convention.
Reuse an existing signal when a new class shares the same observation and
cadence; add a new signal when it measures a distinct source, target, or health
contract.

For a database fix, add and test the migration or query change in code, but do
not apply it to production without authorization. Verify migration presence
read-only before attributing a post-migration result.

For an operational or hardware root cause, record the code's limit and the
non-software closure action in `SIGNALS.md`. Supply a safe command sequence or
capacity calculation when useful, but wait for explicit authority before
changing production. Continue validating other findings during that wait.

## Updating `SIGNALS.md`

Treat the catalog as the durable incident and measurement specification, not a
chronological chat log. Every relevant learning should update:

- the short `Probe:` key and healthy/broken band;
- the exact direct measurement and privacy boundary;
- sustain, cadence, target, denominator, and warmup behavior;
- the causal discriminator and common false attributions;
- software, operational, finance/network, and hardware ownership;
- the exact action, artifact ancestry requirement, prerequisite order, and
  verification window; and
- a dated incident/control note when production evidence materially changes
  the model.

Historical observations must be labeled historical. Never hard-code an old
version as though it were the current runtime or unconditionally tell an
operator to deploy a fix. Guidance must first compare the running artifact's
ancestry with the required commit; already-current targets proceed to the next
discriminator. Keep non-software alert classes explicit so a code rollout is
not reported as closing missing hardware, route ownership, liquidity, or
operator work.

Update the Go alert text and deterministic expectations in the same change when
the catalog changes an automated diagnosis, action, or verification contract.

## Deterministic verification gates

Every confirmed problem needs a synthetic regression. The test should construct
the smallest input that reproduces the causal failure without production access
or wall-clock sleeps. Cover, as applicable:

- the failing observation and exact alert identity;
- a healthy boundary and threshold edge;
- missing, malformed, stale, or ambiguous evidence;
- counter reset, boot/process-generation change, warmup, and zero denominator;
- old versus current artifact ancestry;
- disabled-host and partial-rollout behavior;
- Markdown detail, stable identity, and redaction; and
- a negative assertion that rejects the prior false diagnosis or stale action.

Run the narrow test first, then the package and race suite:

```sh
go test ./monitor -run 'TestRelevantName' -count=1
go test ./monitor
go test -race ./monitor
go vet ./monitor
git diff --check
```

Run the owning repository's full release-relevant tests for product fixes. A
focused test is not sufficient when the change affects generated service
configuration, networking, concurrency, memory ownership, migrations, or
artifact construction.

## Deployment and production verification

Report the exact service(s) and repository commit(s) that must be built. Do not
claim a fix is deployed from a human version label alone. Prove:

1. the built source contains the required commit by ancestry or immutable
   source metadata;
2. every relevant running unit/container uses that artifact;
3. process start time is after the actual rollout/config/migration boundary;
4. prerequisite migrations, resident worker restarts, router changes, or
   capacity changes are complete; and
5. the monitor's full signal-specific verification window begins after the
   last relevant target converges.

Re-run the direct discriminator as well as the monitor signal. Require the
healthy control to remain healthy and check for displaced failure modes. A
single later success, green `/hello`, quiet minute, or desired-version control
plane row does not prove fleet convergence.

When deployment is not yet authorized or an external measurement must not be
interrupted, state what is ready, what must be deployed, and the earliest valid
verification time. Schedule the check automatically when the operator has
already given a time boundary; do not require another signal merely to begin a
read-only verification.

## Checkpoints and continuous cadence

Before each checkpoint:

1. inspect all affected worktrees and preserve unrelated changes;
2. review the diff for causal scope, secrets, generated artifacts, and stale
   guidance;
3. pass the required deterministic and release-relevant tests;
4. commit each repository separately with a root-cause-oriented message;
5. record commit hashes and remaining deploy/operation gates; and
6. if the server monitor changed, perform the safe watcher promotion above.

Do not push or deploy merely because a checkpoint exists.

Then continue the loop. Poll the authoritative process/session and new alert
output at a cadence that cannot leave the agent silent or blind. Reconcile each
open identity against its last material state rather than repeatedly reporting
the same prose. During long waits, investigate other alerts, improve synthetic
coverage, validate artifact provenance, or refine the catalog. Quiet periods
are observation windows, not permission to stop.

## Status handoff

Each operator update should lead with the outcome and contain only the material
state:

- confirmed current failures and their direct evidence;
- root cause, confidence, and alternatives still open;
- tested commits and exact services or non-software actions required;
- deployments/migrations/configurations actually verified by provenance;
- observation windows still running and their deadlines;
- disabled or deferred targets and why; and
- authoritative watcher hash, process/session, and alert path when ownership is
  handed to another agent.

Never say “fixed” until the post-boundary production gate passes. Use “code fix
ready,” “deployed, verification pending,” “operationally blocked,” or “hardware
capacity required” when those are the true states.
