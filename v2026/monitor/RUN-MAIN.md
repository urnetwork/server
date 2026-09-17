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

## Model roles and handoff

Use two explicit, long-lived agent roles for every pass; reuse the same agents
so their evidence context and open causal boundaries remain intact:

- A `gpt-5.6-terra` agent at `medium` reasoning owns monitor execution and
  evidence work: preflight, immutable binary, authoritative watcher and tails,
  alert capture, bounded read-only investigation, fact collection,
  evidence-authority checks, failure triage, focused reruns, source identity,
  and every verification gate. The Go watcher remains model-neutral; Terra
  operates and interprets it. The primary agent must retain
  the actual durable execution-session handle unless Terra remains active for
  the entire watcher lifetime. Process ownership is not session ownership: a
  watcher started inside a sub-agent tool session can disappear when that agent
  returns even after its PID and tails passed a liveness check.
- A `gpt-5.6-sol` agent at `max` reasoning owns root-cause debugging and repair
  for every new, changed, or unresolved causal boundary. Sol consumes Terra's
  fact and triage manifests, inspects the bounded relevant source, and runs
  local deterministic controls. When another production fact is required, Sol
  specifies the missing discriminator for Terra to collect rather than
  independently recollecting production evidence.

For each such boundary, Terra sends deterministic delta, investigation, fact,
and triage manifests referencing the prior ledger record and object hashes
described below; group shared dependency/artifact/rollout failures and keep
unrelated causes separate. Never paste full all-signal Markdown, logs, raw
evidence, or test output into a model handoff. Sol pulls the complete Alert and
only bounded relevant source objects by hash on demand. If the packet is
insufficient, Sol returns a precise missing-fact request to Terra; otherwise Sol
returns cause/patch/regressions/prerequisites/window and Terra returns gate
manifests. Keep both agents and the watcher alive unless safe promotion requires
a handoff.

The primary agent owns the append-only run ledger. Terra and Sol produce
immutable, privacy-reviewed manifests and name the intended `prior_record`, but
must not append ledger records themselves. This keeps process/session ownership,
production mutation authority, and the single-writer chain in one place.

## Agent contract

Run every signal; freeze, corroborate, classify, and diagnose each finding;
make the smallest owning fix with synthetic coverage and catalog updates; pass
all gates; then verify the exact production boundary, checkpoint, promote
without an observation gap, and continue.

Every investigation and root-cause handoff must explicitly report
`false_positive_qualifiers` and `false_negative_qualifiers`. Terra records the
candidate discriminator, its observation authority, source completeness, and
healthy control in the fact/triage manifest; Sol decides whether the evidence
establishes a real probe boundary. When it does, update the owning numbered
`SIGNALS.md` entry and the probe reducer/tests in the same correction. Cover
the misleading-but-healthy input, the true failure, and unavailable,
partial, stale, corrected, or ambiguous evidence as applicable. Missing
authority must remain unknown or `cannot-observe`, never healthy. Do not add a
qualifier from speculation, suppress the original symptom, or wait for the
daily research pass to record a discriminator learned during an active
incident.

This is context-delivery optimization only: use Terra at medium for
deterministic monitoring, investigation, fact collection, and failure triage;
retain Sol at max for root-cause debugging and repair. Never silently change
those roles or efforts, skip a severity, lose warnings or `cannot-observe`, or
delete/truncate evidence.
Do not raise production `runLoopMaxConcurrentSignals`, add watchers outside
bounded promotion, shorten windows, or persist sustain counters/ticket state.

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
- Bind diagnostic-request fields by JSON name or a NUL-safe typed encoding;
  never unpack optional or secret-bearing fields with whitespace/TSV `read`.
  Before contact, assert the rendered argv separately from execution: an empty
  optional identity list emits no `-i`, the target equals the inventory-owned
  `user@endpoint`, and a password can appear only in the intended remote stdin
  or environment channel, never a local argv, address, filename, or DNS lookup.
  A binding mismatch, missing result schema, or nonzero transport exit stops
  the discriminator without retry and leaves the production fact unknown. If
  a secret may have entered an unintended channel, restrict the evidence,
  report the exposure without quoting it, and require credential disposition
  before issuing a fresh request.
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
- Fix correctness bugs within the existing product architecture. A monitor
  diagnosis is not authority to change how builds work, replace the deployment
  workflow, or redesign the system. In particular, do not add clean-worktree
  gates, freeze Git HEAD across a build, reject `modified=true`, or make
  Warpctl build/deploy success contingent on service-binary provenance without
  an explicit operator architecture decision. Local checkouts and deliberate
  diffs remain valid deployment inputs; observe and record them without
  changing that contract.
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
test -n "$BRINGYOUR_HOME"
install -d -m 700 "$BRINGYOUR_HOME/monitor"
umask 077
go test ./monitor
go test -race ./monitor
go vet ./monitor
monitor_preflight_dir=$(mktemp -d "$BRINGYOUR_HOME/monitor/server-monitor.preflight.XXXXXXXX")
go build -o "$monitor_preflight_dir/monitor" ./cli/monitor
chmod 700 "$monitor_preflight_dir/monitor"
test -x "$monitor_preflight_dir/monitor"
```

In zsh-backed wrappers, never assign shell-special or generic process names
such as `status`, `pipestatus`, `HOME`, or `CODEX_HOME`. Use a task-specific
name such as `probe_exit` and record it immediately after the child returns.
If wrapper bookkeeping fails after a bounded production probe already emitted
valid evidence, preserve the evidence and its missing exit-code boundary; do
not repeat production contact solely to reconstruct wrapper metadata.

Keep every disposable repository checkout or Git worktree under a per-run
workspace in `$BRINGYOUR_HOME/temp`, for example
`$BRINGYOUR_HOME/temp/server-monitor-source.<revision>.<suffix>/server`. Go
modules in this repository use sibling `replace ../...` paths, so put only the
required dependency symlinks beside that `server` checkout inside the same
per-run workspace; do not create shared dependency symlinks directly in
`$BRINGYOUR_HOME/temp`. Keep monitor evidence and immutable binaries under
`$BRINGYOUR_HOME/monitor`; do not place a repository checkout beside the normal
repositories in `$BRINGYOUR_HOME` or inside the monitor evidence directory.
Create the temp parent with mode 0700 before adding a worktree, and move or
remove the registered worktree with Git rather than renaming or deleting it
behind Git's back.

Treat a failing preflight as a monitor/repository problem to diagnose, not as a
production alert.

Before any service release build, run the registered `release-builder` signal
against the intended environment. The local repositories are intentionally the
authoritative deployment source, including deliberate uncommitted changes.
Enumerate every local module replacement used by the target (`replace ../...`
in the module graph) before the build and coordinate ownership of those sibling
checkouts. A stable, intentional dependency diff is a valid input and must be
recorded; a sibling that another agent or process is still changing is not yet
a defined artifact input. Wait for that edit to settle, then record the base,
tracked diff, and participating untracked bytes for the primary repository and
each local replacement. Do not infer dependency cleanliness from the primary
binary's `vcs.modified` bit: Go reports that bit for the main module, while code
from a dirty local replacement can still be compiled into the service. This is
coordination and evidence for the existing local-checkout workflow, not a
Warpctl clean-worktree gate or a replacement build design.
The exact local `warpctl` resolved for the build and every installed
managed-host copy must expose a parseable full Go VCS base revision and Boolean
modified bit; `modified=true` is context, not a fault. A desired version label,
a checkout beside another executable, or an install timestamp is not equivalent
evidence. If identity is missing or malformed, rebuild the workstation
executable through the current `warp/warpctl/Makefile`, and rerun the local
`xops/main/ansible/run-edges.sh` to build and install managed-host copies from
that local Warp checkout. Do not substitute a published or cached Warpctl, and
do not discard an intentional diff merely to make a signal green. Record and
preserve the checkout base plus participating diff when exact replay matters;
§8.12 independently verifies the resulting running service artifact.
This is an observation contract, not a build-admission policy. Missing identity
may alert, but it must not cause the monitor agent to redesign Warpctl's build
pipeline without explicit operator direction.

## One-shot diagnostic snapshot

Use one-shot mode for an initial inventory, a focused before/after snapshot, or
manual diagnosis:

```sh
test -n "$BRINGYOUR_HOME"
install -d -m 700 "$BRINGYOUR_HOME/monitor"
umask 077
monitor_snapshot_dir=$(mktemp -d "$BRINGYOUR_HOME/monitor/server-monitor.snapshot.XXXXXXXX")
go build -o "$monitor_snapshot_dir/monitor" ./cli/monitor
chmod 700 "$monitor_snapshot_dir/monitor"
test -x "$monitor_snapshot_dir/monitor"
WARP_ENV=main "$monitor_snapshot_dir/monitor" -mode overlay -once \
  >"$monitor_snapshot_dir/alerts.md" \
  2>"$monitor_snapshot_dir/stderr.log"
```

`-once` runs selected signals serially, emits every current violation, bypasses
sustain gating, and exits nonzero on probe failure. Preserve stdout and stderr
even then: visibility alerts retain findings and stderr distinguishes an
observation-path failure. Label this a snapshot, not a page.

`monitor -list-signals` lists selector values without loading the environment,
Vault, or settings. After required full coverage, use focused reruns by
repeating `-include-signal` with a key, number, or probe ID. Include and exclude
selectors are mutually exclusive; empty, unknown, or ambiguous includes fail
closed. Repeated `-exclude-signal` remains diagnostic and cannot certify health.

```sh
WARP_ENV=main "$monitor_snapshot_dir/monitor" -mode overlay -once \
  -include-signal edge-ipv6 -include-signal 20.2 \
  -format jsonl >"$monitor_snapshot_dir/alerts.jsonl"
```

`-format markdown` is the default. `-format jsonl` writes one complete alert
object per line in deterministic severity/identity order; a healthy JSONL
snapshot is an empty file.

## Start the authoritative continuous watcher

Build a unique immutable binary and keep its artifacts together:

```sh
test -n "$BRINGYOUR_HOME"
install -d -m 700 "$BRINGYOUR_HOME/monitor"
umask 077
monitor_run_dir=$(mktemp -d "$BRINGYOUR_HOME/monitor/server-monitor.watch.XXXXXXXX")
go build -o "$monitor_run_dir/monitor" ./cli/monitor
chmod 700 "$monitor_run_dir/monitor"
test -x "$monitor_run_dir/monitor"
shasum -a 256 "$monitor_run_dir/monitor" >"$monitor_run_dir/binary.sha256"
monitor_warpctl=$(command -v warpctl)
test -n "$monitor_warpctl"
test -x "$monitor_warpctl"
go version -m "$monitor_warpctl" >"$monitor_run_dir/warpctl.version.txt"
rg -q 'vcs\.revision=[0-9a-f]{40}$' "$monitor_run_dir/warpctl.version.txt"
rg -q 'vcs\.modified=(true|false)$' "$monitor_run_dir/warpctl.version.txt"
shasum -a 256 "$monitor_warpctl" >"$monitor_run_dir/warpctl.sha256"
monitor_warpctl_dir=$(dirname "$monitor_warpctl")
PATH="$monitor_warpctl_dir:$PATH" WARP_ENV=main \
  "$monitor_run_dir/monitor" -mode overlay \
  >"$monitor_run_dir/alerts.md" \
  2>"$monitor_run_dir/stderr.log"
```

Treat the exact Warpctl resolved by the watcher as part of the immutable
observation boundary. Before launch, require its `go version -m` output to carry
a full `vcs.revision` and Boolean `vcs.modified`, record its hash, and verify
that its source revision contains every collector capability assumed by the
active catalog. In particular, §1.5 requires the Loki live-tail cursor guard at
Warp commit `d857872c4cae8e4768ed2314fdb53fc96b4fdbdb`. Do not infer capability
from the HEAD of a nearby checkout while an older executable appears first on
ambient `PATH`. Build through the existing local Warp checkout convention when
the resolved executable is stale, then pin only this watcher invocation's
`PATH` to the validated executable directory. This does not alter Warpctl's
release/build architecture or install anything on managed hosts.

During candidate validation, inspect the executable image of every standing
`warpctl logs ... -f` child, not only its abbreviated process command, and
require it to match the recorded watcher-side Warpctl hash. A candidate that
silently resolves a different copy cannot be promoted.

Run the final command in a durable attached session. Give the run a stable ID
and append only to `$BRINGYOUR_HOME/monitor/runs/<run-id>/ledger.jsonl`. Keep
complete Alert JSONL, raw bounded source evidence, logs, patches, and test
transcripts immutable at
`$BRINGYOUR_HOME/monitor/objects/sha256/<first-two-hex>/<sha256>`. Each compact,
deterministic handoff manifest carries `identity`, `severity`, `signal_key`,
`class`, `target`, `observed_at`, object hash/length, `prior_record`, and only
field/evidence deltas; its ledger record also names UTC time, causal boundary,
kind, and source/producer. Exclude secrets and unredacted customer data. On
resume, verify objects and recover each boundary's last record, never delete or
recollect evidence. Watcher records also carry binary path/hash, PID/session,
environment/mode/start timezone, alert/stderr objects, server commit/dirty
state, expected tails, exclusions/reasons, boundaries, and deadlines. The
session must support polling, graceful stop, and liveness proof.

Delta reducers must use the exact monitor identity returned by
`Alert.Identity()`: `SignalID + Class + Target + Frame`. Severity is not part
of that identity. Report a WARN-to-PAGE or PAGE-to-WARN transition only when
all four identity fields match exactly before and after; never pair alerts by
`signal_key`, family, sort position, timestamp adjacency, or latest row. Two
concurrent findings from one signal remain distinct even when they alternate in
the JSONL stream. A diagnostic relationship between different classes may be
reported separately as a semantic reassignment only with both complete
before/after identities and `causal_or_recovery_claim=false`; it is not a
severity transition, a resolution, or a new identity replacement.

Gate the severity comparison on authoritative predecessor existence. A
missing, null, empty, or default prior severity is not a transition state:
classify that exact identity as new and do not also increment transition or
downgrade counts. If the prior prefix is incomplete or unsealed, newness and
transition absence are unknown rather than zero. Every reducer regression must
include prior `A/PAGE` with window `B/WARN, A/PAGE`
(`new=1, transitions=0, downgrades=0`) and prior `A/PAGE` with window `A/WARN`
(`new=0, transitions=1, downgrades=1`). This existence gate must run before
evaluating `before_severity != after_severity`.

The alert artifact is append-only while the watcher is live. Snapshot the
inclusive ending row once, then extract with both lower and upper row bounds
and verify the exact expected row count before hashing or reducing it. Never
hash `tail -n +START` or another open-ended reader: rows appended during that
pipeline produce a digest that cannot describe the recorded window. JSONL
identity reducers use the lowercase wire keys `signal_id`, `class`, `target`,
and `frame`; record the canonicalization algorithm alongside its digest so the
primary agent can reproduce it without reading private evidence into a handoff.

The ledger has one writer: the primary agent that owns the authoritative watcher
session and production-mutation boundary. Each record is exactly one complete
compact JSON object on one physical line. Canonicalize a prepared,
privacy-reviewed record with `jq -ce .` before appending it, append the resulting
single line once, then parse the exact final line and verify its `record_id` and
`prior_record`. Never append `jq .` output or another indented object. If a
historical producer already appended pretty-printed records, preserve those
bytes: `jq -c . ledger.jsonl` can stream the whitespace-separated objects for
recovery. Append a compact format-defect/correction record and use compact
records thereafter; never rewrite or truncate the evidence ledger merely to
make its old physical layout valid JSONL. Terra and Sol may prepare manifests;
they do not append. Do not allow two agents to append in parallel.

The agent that owns the attached execution session must not return, complete,
or release that session while its watcher is authoritative. Prefer a
primary-agent-owned session handle with Terra validating the binary, parent,
tails, files, and cadence through read-only checks. A handoff is complete only
after the receiver can poll the exact session handle; sharing a PID, run
directory, lock file, or narrative status is insufficient. If this execution
environment cannot transfer a live handle, keep the existing owner alive or
promote a receiver-owned candidate with the overlap procedure below before the
owner exits. Record any interval without a pollable authoritative handle as an
observation gap rather than silently recreating the watcher.

Continuous mode runs signals at their own cadences, limits ordinary probe
concurrency, applies each alert's sustain count, and replaces the bounded log
probe with one standing `warpctl logs ... -f` stream per active service. Confirm
the watcher is alive and owns every expected standing stream. A live parent
with missing children is not healthy observation coverage.

Classify standing tails by the exact child command containing `logs` and `-f`,
not by the `warpctl logs` prefix alone. The independent bounded overlap
reconciliation also creates short-lived `warpctl logs --since ... --limit ...`
children without `-f`; those and transient SSH helpers are dynamic context, not
additional standing tails. Process enumeration can race their normal exit, so
an ended non-tail child does not invalidate otherwise matching parent, binary,
stdout/stderr descriptors, zero standing-tail zombies, and one live `-f` child
per expected service. A missing standing service, or a persistent duplicate
`-f` stream for one service, remains an observation defect; investigate it
rather than weakening the expected-tail invariant.

When recovering or auditing an existing watcher, resolve its actual stdout and
stderr file descriptors before checking file age. The immutable binary's
directory may belong to a predecessor run; stale files there do not prove the
current watcher stopped observing.

For a temporary exact-edge pause, preserve all other coverage:

```sh
WARP_ENV=main "$monitor_run_dir/monitor" -mode overlay \
  -exclude-edge-ipv6-host HOSTNAME
```

Unknown exclusions fail closed. Inventory-disabled hosts are excluded across
all probes automatically; `-exclude-edge-ipv6-host` disables only that host's
exact public IPv6 paths.

For a whole-host pause, use repeatable exact inventory names:

```sh
WARP_ENV=main "$monitor_run_dir/monitor" -mode overlay \
  -exclude-host HOSTNAME
```

`-exclude-host` is an immutable transport policy: retain desired topology,
service blocks, and expected denominators while denying excluded
inventory-target contact. Record its operator reason, owner, UTC start, and
re-enable condition in the ledger. `monitor-host-scope-partial` keeps excluded
coverage explicitly unknown; it is not full-fleet recovery. Whole-environment
service tails and permitted targets remain observed. Empty, wildcard, unknown,
or ambiguous names fail closed. Reapply exactly the same host/IPv6 exclusions,
mode, and SSH overrides to every settings-freshness reload and the pre-promotion
one-shot. Do not remove a selector merely to quiet this operational WARN.

`-exclude-signal` excludes only probe constructors, not hosts selected by other
probes. A separately host-filtered helper proves its own scope only; never
transfer that proof to the current authoritative watcher. Re-enable a paused
host only when its recorded operator condition is met and a current-generation
validation and controlled handoff succeed.

## Safe watcher promotion

Any monitor code, catalog, inventory-loading, tailer, alert-rendering, or
effective Config/Vault settings-generation change requires a newly built
watcher. A `settings-freshness` finding means this boundary has already been
crossed; validate the current generation with a fresh one-shot before
promotion. Promote it as a controlled handoff:

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

Before step 6, prove that the candidate's execution-session handle is pollable
from the agent that will own the continuous run after promotion. Do not allow a
sub-agent to report success and finish while its private session owns the only
candidate. Immediately after promotion, poll that same handle as well as the
parent and tail processes; a PID-only check cannot certify durable ownership.

Overlap is allowed only for this bounded handoff. Prolonged duplicate watchers
distort log coverage and add production load; stopping the old watcher before
the new one is proven creates an observation gap.

Sustain counters and alert gates are process-local. During overlap, compare the
candidate's raw probe observations and direct source measurements with every
mature predecessor alert; absence from the candidate before its required number
of cadences is not recovery. Before stopping the predecessor, record its alert
path/hash and any still-failing direct observation in the ledger. Keep that
evidence until the candidate either re-emits the identity or completes the
documented healthy resolution window. Do not persist sustain counters or
introduce persistent ticket state as part of a watcher handoff without a
separate design decision.

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
| log loss | standing tail health and privacy-safe drop summaries | current-watcher `monitor-log-reconcile ` diagnostic receipt, bounded absolute-window reconciliation per service/block, and direct host journal |
| metrics identity | fresh process-emitted series with host/block/instance labels | live process start, listener, ring membership, and scrape age |
| metrics restart durability | exact loopback Mimir `/config`, reduced remotely to `flush_blocks_on_shutdown`, `query_store_after`, `query_ingesters_within`, blocks-storage `ignore_blocks_within`, bucket-store `sync_interval`, and compactor `cleanup_interval` | separate exact-process lifecycle proof plus a controlled replacement with no new bounded §11.20 gap through the complete handoff and discovery window; config and sustain state are process-local |

Use the existing SSH and Warpctl transports and the commands documented in
`SIGNALS.md`; do not improvise a less safe secret path. Never contact a disabled
host while trying to improve denominator coverage.

For a two-reconciliation closure gate, retain the exact `monitor-log-reconcile `
prefix plus schema-1 JSON from the current watcher generation's stderr artifact.
Require `collectors=enabled=fresh=consecutive_two > 0`, matching the intended
collector inventory. Check `collector_started_at`, `previous_window_start`,
`latest_window_start`, `previous_completed_at`, and `latest_completed_at`:
their oldest/newest ranges must be non-null, ordered, advancing, and meet the
incident's post-boundary window. Bind the receipt to the immutable binary,
durable session, and stderr object; never combine predecessor and candidate
histories. Query lower bounds and local completions do not establish fresh
source-record timestamps or live-stream liveness; retain those independent
controls and the required no-loss window. Missing, malformed, stale, or
partial-scope receipts leave completion unknown; alert absence alone is
insufficient. This diagnostic does not change alert-only JSONL or invent
healthy Alerts. See `SIGNALS.md` §1.5 and `MONITOR.md` §3.7 for the fixed schema.

## Daily three-way improvement research

Run this research at least once per UTC day, and again after any material
incident diagnosis. Perform a three-way reconciliation of the monitor
specification, executable coverage, and accumulated production evidence. Record
the completed UTC time, the exact catalog/source/ledger identities, the prior
ledger record, and the next due UTC boundary. A busy incident does not cancel
the daily pass: do the parts needed for that incident immediately and complete
the remaining catalog research before the day closes. A live incident discovery
triggers this reconciliation immediately; the daily cadence does not defer an
active incident.

Research all three directions:

1. **catalog → implementation:** enumerate every `Probe:` declaration and every
   explicitly required alert family in `SIGNALS.md`. Require a registered
   `Signal`, the semantic `signal_<key>.go` and `_test.go` pair, the numbered
   source comment, a real bounded source adapter, and synthetic violated,
   healthy, unknown/ambiguous, cancellation, and Markdown/redaction coverage.
   A documentation-only or reserved class is an open coverage alert, not an
   implemented probe.
2. **implementation → catalog:** enumerate every registered probe, emitted
   class, parser branch, threshold, sustain/page gate, target/frame, cadence,
   source query/command, and recovery rule. Require the catalog to describe the
   same behavior and ownership. Flag orphan files, registrations, classes,
   stale actions, hidden fallbacks, unsafe cardinality, and tests that exercise
   only strings instead of the actual reducer.
   Include shared monitoring utilities, fixed constants/local class
   alternatives, and actual log/burst emission, not only `signal_*.go`
   literals. Canonical metric and correlation-only names are not emitted Alert
   classes. Unresolved computed/interprocedural producers remain explicit
   audit limits, not a green completeness verdict.
3. **ledger → catalog and implementation:** reduce the append-only ledger by
   causal identity and follow corrections/closures without rewriting history.
   For every new discriminator, false attribution, visibility gap, operational
   prerequisite, or escaped failure class, either update the owning catalog and
   probe with a deterministic regression or record why an existing probe
   already covers it. Historical class names remain documented as aliases when
   removing them would make old evidence uninterpretable. Review each causal
   finding for both false-positive and false-negative qualifiers: aggregation,
   denominator, timing, stale or corrected data, partial source coverage, and
   observation failure are explicit candidates, but become probe rules only
   when a bounded discriminator and healthy control establish them.

Terra owns the reproducible crosswalk, registry/test inventory, current watcher
delta, bounded investigation and fact collection, observation-authority review,
failure triage, and verification manifests. Sol owns root-cause debugging,
causal discrimination, false-positive/false-negative boundaries, and the
smallest source/test corrections. The primary agent reviews their manifests,
writes the single chained ledger record, and owns all source and production
authority decisions.

The pass is complete only when every catalog signal is implemented or remains
an explicit active coverage finding with a named missing source prerequisite;
every implementation is documented and tested; every still-relevant ledger
learning is represented; focused and full monitor gates pass; and any monitor
change is used to build and promote the watcher through the safe handoff above.
Do not claim the audit is clean by relabeling an intended signal as a runbook,
trusting an exporter verdict that should be independently replayed, or treating
an unavailable source as a healthy zero.

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

For every iOS/Apple issue, cross-validate the same failure mechanism on Android,
Windows, Linux, macOS, `mmm/ur.io`, and `extension` before closing the finding.
This applies to the initial cause, adjacent defects, and the proposed fix.
Trace shared Go/SDK implementation and each platform's actual caller, startup, callback, persistence,
and teardown boundaries; a platform-specific entry point does not make the
underlying bug platform-specific. Run deterministic regressions through the
relevant shared and platform-specific paths, with an old-behavior reproduction
and a healthy control. Record each platform as reproduced, ruled out with named
evidence, not applicable with a source-backed reason, or unverified with the
missing test/device/build prerequisite. Compilation alone is not behavioral
validation, and lack of access is not proof that a platform is unaffected.
Preserve these cross-platform findings in `SIGNALS.md` and the incident handoff,
including each affected release artifact and any remaining verification gates.
Device installation, VPN-profile changes, and other privileged actions still
require the operator authority described above.

For device incidents, verify actual capture coverage before interpreting a
missing record. Compare full archive bounds and original collection arguments
with the incident time, and identify the exact PID and bundle/component of a
termination message. Membership in a Jetsam process table is not proof that
the process was killed. A Jetsam-only download is not a complete crash-report
inventory; a later one-hour unified-log capture may exclude overnight startup.
Preserve those evidence gaps explicitly. Broader or new device collection still
requires the operator's authority; a source-supported mechanism is not proof
that its branch executed in the captured incident.

For connection/preference durability, identify the actual writer and store in
each process; an app's saved location does not prove the extension or daemon
saved it. Exercise first mutation before listener registration, early RPC Sync,
equal-value replay, checked read/write failures, and replacement with the UI
closed. A listener without initial replay cannot be the only persistence owner.
The user explicitly approved `DeviceLocal.Load()` and separately opt-in,
default-off save-on-mutation on 2026-09-05. Load must not implicitly enable
saving; enabling saving must not load or save constructor defaults. Adopt the
mode before exposing RPC or accepting new preferences, with checked durable
commit before live mutation. Keep shutdown/transient state separate from an
explicit disconnect, and preserve each platform's existing startup policy.
Remove native preference writers only when the SDK actually owns that same
store and preference. DeviceRemote app mirrors and browser/OS session state
are not automatically duplicates. A hosted DeviceLocal without an owned store
needs an explicitly reviewed tenant-safe integration, not writes to a shared
NetworkSpace directory or an incidental new schema or storage framework.

For client authentication fixes, preserve the existing credential roles on
every platform: the login JWT is the admin credential, and a separately
derived client JWT is used by the provider. Client refresh must not overwrite
the stored admin JWT, and provider-only process storage must not manufacture
an admin credential from its client token. Use distinct tokens in deterministic
login, refresh, restart, and replacement tests; equal-token fixtures cannot
validate this separation. See `SIGNALS.md` section 13.6.

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

Honor each test harness's interpreter and preserve the actual test exit code.
In particular, source Server's `test-env.sh` inside Bash, not the workstation's
default zsh, and launch the test from that same shell so the verified resource
exports reach it. Stop immediately if preflight fails. Use `exec` for the final
test command or explicitly propagate its captured status; a later successful
status message must never turn a failed preflight or test into a passing gate.
If preflight finds a launcher lock without a readable current attestation, do
not delete the lock, rewrite `/etc/hosts`, or stop an owner merely because it is
old. Follow `local/README.md`: first prove whether a live `run-local.sh` and its
children still own the exact healthy repository Compose services. When they
do, preserve that launcher and run `local/run-suite-proxy.sh` in a durable
primary-agent-owned session, with an absent private state path under
`$BRINGYOUR_HOME/monitor` and explicit complete Vault and Config roots. Keep
that proxy owner live through every test, revalidate its attestation before and
after the gate, and stop it gracefully only after the last consumer exits. If
there is no live owner, use the stale-state procedure in `local/README.md` rather
than synthesizing readiness. A direct test that happened to pass without this
preflight remains diagnostic evidence, not a formal gate.
For piped commands, preserve required stage failures with `pipefail` and the
appropriate captured `PIPESTATUS`; `exec` on one pipeline stage is not enough.

Batch ready gates by causal boundary. Reuse a successful result only for an
exact fingerprint covering repository bases, dependency locks, toolchain,
command/arguments, relevant environment and harness/config, and a consistent
Git executable's `git diff --binary --full-index` plus participating untracked
paths and bytes. Record Git version, exit status, and transcript object; handoffs
reference an exact-fingerprint pass instead of repeating its output. Any changed
input invalidates reuse, and production observations are never cached. Git
versions can render different abbreviated `index` lines,
so compare full-index diffs and untracked bytes before declaring a mutation;
preserve the original metadata and correction. This is test evidence, not a
clean-checkout or build-admission rule.

## Deployment and production verification

Report the exact service(s) and repository commit(s) that must be built. Do not
claim a fix is deployed from a human version label alone. Prove:

1. §8.13 recorded a parseable identity for the exact local-checkout `warpctl`
   that performed the build, including its Boolean modified state and any
   participating diff needed for replay;
2. the built source contains the required commit by ancestry or immutable
   source metadata;
3. every relevant running unit/container uses that artifact;
4. process start time is after the actual rollout/config/migration boundary;
5. prerequisite migrations, resident worker restarts, router changes, or
   capacity changes are complete; and
6. the monitor's full signal-specific verification window begins after the
   last relevant target converges.

Re-run the direct discriminator as well as the monitor signal. Require the
healthy control to remain healthy and check for displaced failure modes. A
single later success, green `/hello`, quiet minute, or desired-version control
plane row does not prove fleet convergence.

Before using `warpctl ls versions --sample` as live provenance, determine
whether the service has at least one non-transparent load-balancer allocation.
Transparent-only services have no load-balancer block-status route. The command
must report that limitation without polling, and `deploy --only-older` must
fail closed because it cannot compare an unobserved running version. Verify
such services directly on every host assigned by the current `services.yml`;
an HTTP 404 from a normal edge proves the request arrived but not that the
transparent service or its version was sampled.

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

Then continue. For blocked work, record its owner, unblocking event, and earliest
meaningful UTC deadline; wait on that build/session/rollout event or schedule
one bounded deadline check instead of polling unchanged state. Meanwhile handle
other boundaries. Poll the authoritative watcher often enough to remain
observable; unchanged periodic samples become ledger/liveness records without
repeated prose. Use quiet windows for other alerts, synthetic coverage,
provenance, or catalog work rather than stopping.

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
