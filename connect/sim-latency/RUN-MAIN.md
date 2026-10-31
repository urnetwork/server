# Sim-latency season agent runbook

`run-main.sh` is the fail-closed agent harness for the six-epoch competition.
Legacy `run`/`approve`/`reject` commands retain their v1 review pause. Explicit
v2 competitions use `supervise` below; never mix these progression paths.
It drives the continuously deployed main API and one-shot worker while keeping
the production evaluator source graph—`server`, `connect`, `sdk`, `proxy`,
`glog`, `goidenticons`, `userwireguard`, `sn`, `warp`, and `gvisor`—isolated
in temporary clones. Production source is pinned on `sim-latency`; explicit v2
staging uses only `sim-latency-staging` and its separate ten-repository source
manifest. Both modes omit the deleted `operator-proxy`; no clone, placeholder,
branch, or pin for it is required. All historical records remain unchanged.
Neither evaluation nor promotion changes the operator's product checkouts.

Before production activation, start with
[`launch/current-state.json`](launch/current-state.json), the compact preparation
checkpoint, then verify its live process/session and linked evidence. It is a
dated operator snapshot, not score/runtime authority or permission to restart an
active qualification. Live season state remains authoritative in the API,
database and the supervisor's state directory described below.

## Agent model roles

Go 1.27.3 is mandatory for competition builds and tests. Select the actual
installed 1.27.3 toolchain before invoking launchers and set `GOTOOLCHAIN=local`;
do not rely on a newer toolchain being downloaded or automatically selected.
Evaluator images and promoted binary metadata must bind exactly Go 1.27.3.

Use Sol 6.1 (`gpt-6.1-sol`) with low reasoning for all test execution,
including preflight, submission validation, post-promotion smoke tests, and
reruns. A test failure or suspected flake must be handed to Sol 6.1 (`gpt-6.1-sol`)
with max reasoning to diagnose the root cause, implement the correction, and add
a deterministic regression test. A separate Sol 6.1 low agent then reruns the
affected test and required suite; do not accept a debugging agent's test run as
the independent completion result.

Winner-review SQL guard qualification also requires
`COMPETITION_WINNER_REVIEW_SQL_TEST=true` in the selected test binaries'
environment. These guards own fresh, networkless PostgreSQL 18 fixture
containers; they never use a live database. Before execution, verify that the
local `postgres:18` tag resolves to the same image ID as the qualification's
digest-pinned `postgres:18@sha256:…` image: the helper uses `--pull=never`.
Qualification must fail on any skipped test as well as any failure. Compilation
and non-SQL passes do not qualify a skipped SQL body. Retain each attempt's
pre/post container inventory and owned cleanup evidence; preserve failed or
skipped attempts without relabeling them as accepted qualification.

The prospective production policy uses Snow's two reviewers:
`qwen3.8-27b` (`xhigh`) and `gemma-4-26b-a4b` (`high`).
Run them serially because Snow has one resident
model at a time; allow model-swap latency inside the original shared deadline.
Both must approve. Retain the union of all attributed findings; a concrete
substantiated rejection rejects, while missing, malformed, unavailable or
uncertain evidence holds progression without inventing approval or dishonesty.
Snow replaces external model calls for automated test attestations too: the
prescribed deterministic Go suite runs once, and both models attest to
the same authenticated result. Models do not choose commands or rerun tests.
The selected endpoint requires no API key. Qualification, fixed egress,
immutable settings, member journals and protected evidence are still mandatory.
Historical Mini and Sol records retain their original provider identities.
Sol 6.1 max retains implementation, debugging, and source/config merge/push
ownership after the required independent review. The reviewer must inspect the
exact materialized patch and score evidence before invoking this harness. The
low-reasoning test agent must not approve candidates, merge branches, or push
source/config refs. These roles apply to all six epochs and must survive an
agent handoff.

Legacy v1 candidate honesty review and source promotion apply only to production.
Legacy staging automatically names the highest-ranked placeable candidate passing
every gate after close and drain, without a significance or takeover-margin
requirement, human-review pause, or source promotion.
This legacy staging path is not the v2 staging supervisor described below.

## V2 durable supervision — disabled until qualified

### Adopted automatic closing policy (review-provider update 2026-10-28)

New seasons should freeze `winner_review_policy: elected_winner_snow_union_v2`.
Qwen and Gemma must both approve; retain both members' complete findings.
Historical Snow v1 records retain their three-member policy and evidence.
This is a prospective opt-in, not a reinterpretation of an already-open round.
The empty policy retains the historical independent source-choice flow described
below. Do not change an existing season's policy approval or running worker pins
to activate this flow in place.

Historical `elected_winner_sol61max_v1` records remain bound to their original
`gpt-6.1-sol`/`max` provenance. Policy-aware readers preserve their meaning and
digests; neither these nor historical Mini reviews are relabeled as Snow reviews. The empty policy still
means the earlier independent source-choice lifecycle. A new provider requires
a fresh approved policy and qualified release, not a field edit to a live round.

For the adopted policy the durable supervisor performs:

1. Close admission and finish every accepted evaluation.
2. Seal and publish the provisional reward election after the reveal boundary.
3. Automatically dispatch the current elected candidate to the qualified
   two-model Snow honesty/safety broker, with exact source/patch/score evidence.
4. Record a substantiated rejection through the scoped review API, publish its
   reasons and replacement decision, and review the next elected candidate.
   An unavailable model, timeout or uncertainty is not a rejection.
5. Once approved, independently build/test that exact winner, publish its source,
   qualify the exact runtime, activate it, and open the next full-window epoch.
   No unrelated ready candidate can replace the elected winner.
6. If the eligible ranking is exhausted (including an initial no-winner result),
   carry the incumbent commits and margin forward. Review the sixth epoch too,
   then stop without opening epoch seven.

The review deadline is persisted before invoking the broker and survives restart.
Missing/expired review pauses progression and alerts support; it never extends
itself or manufactures approval. Build/readiness failures likewise pause and
alert rather than silently carry forward. Source build/readiness retries have
a ten-minute cooldown. An expired review window requires explicitly reviewed
operator recovery; restarting the supervisor does not reset its deadline.
Ordinary append-only appeals remain separate: restoration can re-elect a
recipient, but a still-closing round requires a fresh review of that elected
decision before source promotion. Already-open source pins never change.

Prospectively approve these values together with the rest of the season policy:

```yaml
automation_policy:
  winner_review_policy: elected_winner_snow_union_v2
  winner_review_timeout_seconds: 1800
  source_choice_delay_seconds: 0
  staging_preparation_seconds: 0
season_policy:
  preparation_window_seconds: 0
```

The timeout may be 60..86400 seconds; 1800 is the example review budget, not an
execution timeout for candidates. Actual isolated build/testing and host
qualification still must succeed; zero removes the old fixed 16-hour wait, not
these gates. The normal per-submission scoring budget and statistical rules are
unchanged. Freeze a fresh approved scope/baseline before bootstrap.

Deploy the API/worker/manager code and the additive winner-review/provider DB migrations
before activating the policy. As usual, commit, pull/merge and push before a main
DB migration; never run a main migration from an unpublished local tree.
Historical review reports and existing sealed/open rounds remain unchanged.

In addition to the worker/preparation pins below, configure:

```sh
export SIM_LATENCY_REVIEW_COMMAND=/secure/automation/isolated-snow-review
export SIM_LATENCY_REVIEW_SHA256=REPLACE_WITH_QUALIFIED_BROKER_SHA256
export SIM_LATENCY_REVIEWER_TOKEN_FILE=/secure/automation/reviewer.token
export SIM_LATENCY_BUILD_COMMAND=/secure/automation/isolated-source-build
export SIM_LATENCY_BUILD_SHA256=REPLACE_WITH_QUALIFIED_BUILD_BROKER_SHA256
```

The review broker is a deployment-qualified agent integration, not a fake
template responder or an arbitrary candidate command. It must actually run
both pinned Snow models for the new policy in isolated contexts. The
manager invokes its pinned executable without a shell and with an allowlisted
environment containing no main/database/vault/reviewer/Git-push credentials.
The broker must have no ambient access to those credentials either: do not
run an unconstrained agent with filesystem access to the control host.
Only its independently provisioned model access and bounded source/evidence
input belong in its model-execution boundary. No candidate code may execute.

The strict stdin protocol is `review_elected_winner`: frozen request, exact
authenticated candidate packet/template, and full pinned base files for touched
paths. Candidate comments, patches and diagnostics are untrusted data, never
instructions. A valid stdout response has schema 1, model/reasoning pins,
`no_main_credentials: true`, `no_candidate_execution: true`, a complete
structured `review`, and real bounded `private_evidence`. These identity
claims are trusted broker provenance, not independent model attestation.
All six criteria for each of the two models (12 attributed findings) and
exact revision/digests must validate. Uncertainty blocks;
it must not be fabricated as either approval or misconduct. The manager caches
accepted response bytes for identical idempotent retries, retains the report in
versioned COMPLIANCE MinIO storage, and alone posts to the scoped reviewer API.
Completed broker output is retained separately before validation, including
uncertain, empty, or malformed output. Supervisor retries replay that response;
they must not resample until a model approves. Such a response remains blocked
and requires operator investigation, not deletion of the response to force a
different answer. Changed authenticated context also blocks. A transport failure
with no completed broker output may retry within the original review deadline.
The reviewer token is never sent to the model broker. Private attempt records
and retention receipts remain in the mode-0700 state directory.

Source construction reuses `stage-reviewed-source` automatically with the
qualified isolated build broker. Tests retain independent Sol 6.1 low ownership.
The tool-free provider client is `sim-latency review-broker`; its scratch-image
wrapper and deployment/qualification procedure are in
[`evaluator/review/README.md`](evaluator/review/README.md). Implementation is not
live provider access or genuine qualification. Configure the adopted Qwen/Gemma
endpoints and their network boundary; current Snow access is keyless and does
not require a provider-key file. Build/install the contained broker and qualify
it, and independently
provision/qualify the build broker before enabling the new season. A test receipt
or this runbook alone does not satisfy those requirements.

The supervisor-facing offline build entry point is
`sim-latency source-build-broker --build-settings=<private-json> --build-settings-sha256=<digest>`.
Its immutable wrapper, private settings and independent qualifier contract are
documented in [`evaluator/source-build/README.md`](evaluator/source-build/README.md). The broker
builds artifacts first, then requires a pinned qualifier receipt for that exact
image and attempt. A missing qualifier, changed pin, cancellation, or incomplete
model/containment evidence returns no usable receipt. Do not substitute the
synthetic offline pipeline probe for genuine winner review or qualification.

The evaluator host's private `/etc/urnetwork/competition-qualification.json`
must bind `reviewed_build_state_directory` to that same mode-0700, root-owned
build state. After the API acknowledges the exact prepared source, the updated
host qualifier holds the global evaluation slot and authenticates the original
review-indexed reservation, owner receipts, tests/containment and prospective
host profile. It checks the actual new fingerprint using the pinned checker's
root-only `--json --config` interface, rechecks the supervisor fence and unchanged
active file, then atomically updates only the reviewed image/fingerprint pins.
The old reset/readiness markers are not promoted into new-image proof. The real
runtime qualification and independent worker acknowledgment must still pass
before source activation or the next epoch opening. Missing inventory, stale
approval, boot drift, lost authority or changed local leaves block and alert.
Deploy the updated qualification executable and checker together; an older
checker that lacks the protected alternate-config interface cannot prepare a
promoted winner. Initial checkpoint provisioning remains an explicit root step.

The `automation_progression_blocked` metric/dashboard and
`CompetitionAutomaticClosingBlocked` alert use the existing
`service=sim-latency` routing to support@ur.xyz. Deploy the accompanying
warp Grafana rule; blocked state contains no candidate labels or private reasons.

### Historical independent-source v2 policy

V2 separates the immutable evaluation seal, provisional reward decisions, and
reviewed source. Neither payment nor reward approval authorizes source changes.
The supervisor uses a renewable fenced database lease and append-only checkpoints.
Existing v1 rounds and schema-1 ledger entries are not converted in place.

For frozen policies without the adopted opt-in, after close and full authoritative drain, the v2 worker seals/elects. At the
frozen source-choice deadline (zero extra review wait by default), a ready,
separately authorized exact-source intent may be selected. Otherwise the
supervisor stages `reviewed_source_carry_forward`, including when a provisional
reward winner exists. It retains the approved source generation, exact mode-specific commits,
and threshold. It never calls `promote --no-winner` for an unshipped reward
winner, fabricates approval, or uses that winner's variance recommendation.

Schema 2 retains historical `evaluation_source.epochs` entries unchanged and
adds `generations` and explicit `rounds` mappings. The evaluator's existing
numeric `source_epoch` and `source-check --epoch N` identify the source generation
in v2, not competition round minus one. A carried generation supports fresh
round/seed/baseline identities. The canonical v2 lock digest hashes compact JSON
`{schema:1,development_snapshot:false,repositories:{...}}` with sorted repository
keys; the digest of a pretty-printed artifact file is separate. Each evaluated
round still gets its own trusted R=9 scoring baseline; preparation is not that
scoring control.

For a future approved v2 season, freeze policy and integration digests and use
qualified binaries. The one-time bootstrap requires an already published,
prospectively approved schema-2 baseline-only config commit, complete source lock,
and explicit policy-approval digest in the frozen competition configuration:

```sh
sim-latency bootstrap-source --enable-automation \
  --owner-id=synthetic-season-supervisor --state-dir=/secure/automation/state \
  --manifest-commit=REPLACE_WITH_APPROVED_CONFIG_COMMIT
```

For an announced initial launch, also pass `--opens-at=YYYY-MM-DDTHH:MM:SSZ`.
The timestamp must be in the future when first staged and have at most
microsecond precision. It is persisted in the immutable source intent. Recovery
without the flag preserves that pin; a different flag cannot move it. Once
qualified, the supervisor can create a `scheduled` round before that instant;
admission remains start-inclusive/end-exclusive. The database independently
requires the exact pinned opening and the full admission window. Later unpinned
rounds still open after actual review/build/readiness, not on a guessed date.

Bootstrap registers generation 1/source generation 0 for epoch 1 and persists
recovery material. It does not activate a pointer, create a round, or invent a
review. Repeating the same command resumes the exact commit. The supervisor below
performs the same runtime acknowledgments and the frozen policy's preparation
interval for this baseline as for later transitions. The adopted elected-winner
policy permits a zero interval; the historical fixed 16-hour delay is not an
additional requirement. Use the same state directory and owner.
These examples do not authorize enabling an existing live season:

```sh
export SIM_LATENCY_AUTOMATION_ENABLED=true
export SIM_LATENCY_SUPERVISOR_ID=synthetic-season-supervisor
export SIM_LATENCY_STATE_DIR=/secure/automation/state
export SIM_LATENCY_WORKER_COMMAND=/secure/automation/competitionworker
export SIM_LATENCY_WORKER_SHA256=REPLACE_WITH_REVIEWED_EXECUTABLE_SHA256
export SIM_LATENCY_PREPARE_COMMAND=/secure/automation/propagate-and-qualify
export SIM_LATENCY_PREPARE_SHA256=REPLACE_WITH_REVIEWED_EXECUTABLE_SHA256
./run-main.sh supervise
# One bounded reconciliation through the same guarded path:
./run-main.sh supervise --once
```

Reviewed configuration must also enable v2; either missing opt-in fails closed.
Resolve/build trusted binaries once per reviewed release, not once per poll.
Do not leave a reviewer agent acting as the scheduler. The supervisor's default
choice is safe carry-forward; separately prepared promotion intents require the
real source-review, exact-base, patch and deadline interlocks before selection.

#### Preparation-only qualification boundary — installed inactive

The new native CLI adds `supervise --once --prepare-only` for one explicit
reconciliation of an **already reserved** source intent. This is not ordinary
`--once`: ordinary reconciliation may activate ready source immediately when
the frozen preparation interval is zero. Preparation-only may publish the exact
intent and obtain genuine API/worker readiness, but stops before activation,
round creation, source election/review, or ordinary evaluation. Repeating mode
without `--once` is refused. Do not infer that a `preparation_ready` journal
entry proves all launch gates or grants activation permission.

The 25 incremental controller/CLI/private-policy guards passed three consecutive
runs with zero skips; a compiled old-behavior control reproduced seven unsafe
failures. This does not repeat the older full control suite. The matching
native manager is installed inactive at
`/usr/local/libexec/urnetwork/competition-production-supervisor-09cc-0a396c4a-20261028-v1/sim-latency`,
SHA-256 `0a396c4ab7516c5c642521fa7026e8e9319e074ea94946495c739ddd9a00ba37`.
Its bounded help invocation verified the flag; no bootstrap or supervision
invocation is thereby proved. Older installed CLIs and wrappers do not gain
support from this installation; do not relabel their digests or assume forwarding.
Use only the qualified native CLI with all required pinned worker/preparation
arguments. Preserve the exact bootstrap intent, state directory and owner.
After independent phase-two acceptance, normal supervision still performs its
unchanged fenced activation checks. A preparation-only invocation cannot stand
in for provider, calibration, actual-round rebaseline, or readiness evidence.

### Isolated v2 staging

Use a new, separately approved competition/season and an explicit frozen
`automation_policy.staging: true`; an environment name, zero margin, or the
legacy `run-main.sh staging` command does not select v2 staging. The policy also
requires `settlement_mode: shadow`, `staging_round_window_seconds` in
60..604800, and `staging_preparation_seconds` in 0..57600. Staging evaluation and
source margins are exactly zero, with no actual admission fee or live payout.
Production remains a positive-margin seven-day round with no staging timing fields.
Its historical flow has a fixed 16-hour preparation gate; the adopted closing
policy instead waits for actual qualification, as above. Stage timing and mode are bound
into the activation and runtime envelope; changing them cannot resume old state.

Prepare a NEW ten-repository schema-2 staging baseline at
`config/main/sim-latency-staging.yml` with top-level `staging: true`, branch
`sim-latency-staging`, generation 0, an epoch-one mapping, and zero margins.
It must not contain relabeled legacy `epochs`. Prospectively review the exact
pins, policy, image, and qualification identities. Publish only authorized
normal fast-forwards to the ten staging refs; never contact the retired
`operator-proxy` remote or update production
`sim-latency` refs, force a ref, or rewrite the historical ten-repository
checkpoint in `main/sim-latency.yml`. New staging contains `server`, `connect`,
`sdk`, `proxy`, `glog`, `goidenticons`, `userwireguard`, `sn`, `warp`, and `gvisor`.
The historical ten-repository checkpoint included `operator-proxy` and lacked
`gvisor`; equal counts do not make these identities interchangeable. The old
checkpoint/image remains historical evidence, not qualification for new staging.

After those distinct review/publication steps, these commands verify the frozen
record and build its new evaluator. `source-record` verifies existing refs; it
does not freeze, publish, or approve them. The new builder pins Go 1.27.3 and
marks the image explicitly as staging. Sol 6.1 low executes all build/test and
qualification steps; a successful image build alone is not runtime readiness.

```sh
sim-latency source-record --staging --epoch=0 \
  --source-config=/workspace/config/main/sim-latency-staging.yml \
  --repos-root=/workspace
./evaluator/container/build-base.sh --staging --epoch 0 \
  --source-config /workspace/config/main/sim-latency-staging.yml \
  --tag urnetwork/sim-latency-evaluator-base:REPLACE_WITH_REVIEWED_STAGE_TAG
```

Publish and independently qualify that exact new image on the real contained
worker. Preserve the actual registry/image digests and qualification receipts;
do not copy old digests or substitute self-asserted proof. Stage requests require
a staging image and temporary `sim-latency-staging` source checkouts. Production
requests reject staging images. An explicit stage image cannot be produced from
`--include-worktree` or an unmarked legacy `staging-source-check` alias record.
The existing image smoke and build-isolation entrypoints accept explicit mode:

```sh
./evaluator/container/smoke-test.sh REPLACE_WITH_STAGE_IMAGE --staging
./evaluator/container/test-build-isolation.sh --staging \
  --base-image REPLACE_WITH_STAGE_REGISTRY_DIGEST \
  --policy /secure/automation/approved-patch-policy.json
```

Once the isolated policy, migration route, real deployment/preparation adapter,
and independently qualified binaries are installed, use the ordinary v2
bootstrap/readiness/preparation/round path, not legacy generate-staging routes:

```sh
export SIM_LATENCY_SOURCE_CONFIG=/workspace/config/main/sim-latency-staging.yml
export SIM_LATENCY_STATE_DIR=/secure/automation/synthetic-v2-stage
export SIM_LATENCY_SUPERVISOR_ID=synthetic-v2-stage-supervisor
sim-latency bootstrap-source --enable-automation \
  --owner-id="$SIM_LATENCY_SUPERVISOR_ID" --state-dir="$SIM_LATENCY_STATE_DIR" \
  --source-config="$SIM_LATENCY_SOURCE_CONFIG" \
  --manifest-commit=REPLACE_WITH_APPROVED_STAGE_CONFIG_COMMIT
# Configure the pinned worker/preparation executables as above, then:
export SIM_LATENCY_AUTOMATION_ENABLED=true
./run-main.sh supervise --once
./run-main.sh supervise
```

All six v2 staging rounds retain real readiness, drain, seal, revision, and
review obligations. A stage reward winner never automatically changes source.
Only a genuine separately reviewed, still-applicable patch with independent
isolated staging build proof may promote; its authenticated significance data
is preserved, but its positive statistical recommendation never changes the
staging threshold from zero. Otherwise the next round carries the approved
source unchanged. Dedicated staging config publication preserves production
manifest bytes, including unrelated production config advances. Mode, ref, or
config-path mismatches fail closed on recovery before any push or adapter call.

Publication uses private durable clones of the exact ten-repository source set
for both modes. It freezes
expected/new refs and exact manifest bytes, records intent before any push,
publishes source first and config last, and requires runtime readiness. Ordinary
fast-forward pushes only; no forced update. Lost replies and partial pushes
resume the exact commits. Published but unactivated source is staged work, not an
incumbent. Drift, revoked approval, a different patch tree, or changed protected
runner/dependencies blocks progress. Recovery never selects current remote heads
or modifies product checkouts. Incomplete unpublished staging remains private
audit material; completed recovery directories are installed atomically.

For a **fresh** intent only, config staging fetches `origin/main` and preserves
unrelated config updates made since the active manifest was published. The
active manifest commit must remain an ancestor, and the mode's manifest
(`main/sim-latency.yml` or `main/sim-latency-staging.yml`) must
still match the active manifest's exact bytes and digest, including whitespace.
The fetched head becomes the immutable expected config parent; only the source
manifest changes in its successor. This does not advance any evaluation source
pin. Once an intent is staged or recorded, later config drift blocks publication:
retries never fetch a new parent or rebase a partially published intent.

The control-plane checkout need not contain newly promoted source objects.
Bootstrap and reviewed staging audit the exact private server checkout. An
exact pinned manifest/module read missing from a local checkout uses a new
mode-0700 bare repository and a bounded exact-commit fetch from its origin;
it never fetches into product checkouts or substitutes the moving branch head.

Git stores immutable manifest content; the database pointer selects runtime
authority. The adopted immutable season policy determines the preparation
interval: the historical policy reserved 16 hours, while current production
sets `preparation_window_seconds: 0`. Zero removes that fixed delay, not any
readiness or qualification gate. Independent
API and worker acknowledgments and production qualification precede pointer
activation; only then is one full seven-day round committed. An unadvertised
start is chosen after readiness, so slow preparation does not shorten admission.
Committed schedules/source cannot change. Failed preparation remains visibly
`preparation_blocked` and retries the same intent. Each immutable intent reserves
`target_round_id` before preparation. The trusted host rebaseline uses that exact
UUID and source/image; the eventual round must commit the reserved UUID. This is
a real host-readiness check, not the scoring baseline: the immutable R=9 scoring
control is still established after creation using the fresh round workload.
There is no additional fixed delay after readiness. If a chosen promotion cannot
use an authorized pre-commit fallback, it stays blocked rather than rewriting its
intent. A failed promotion whose complete mode-specific remote refs remain at their expected
predecessors may be durably abandoned; the next activation attempt carries the
incumbent, with fresh preparation and the original source-choice deadline. Any
advanced ref or runtime receipt forbids this automatic fallback. Automatic
creation stops after epoch six; review/settlement obligations
continue through their independent services.

### Required deployment/preparation integration

The repository supplies the concrete `cli/competitionprepare` adapter,
`cli/competitionqualify` host qualifier, exact runtime manifest loader,
authenticated API readiness boundary, and pinned-executable integration contract.
They still require exact-release installation and live production qualification.
This is an activation blocker: absent that installed adapter and
real receipts, the supervisor stays `preparation_blocked`. A successful no-op,
self-asserted JSON, human `git pull`, or manual per-epoch rollout is not a substitute.

Configured executables must be absolute regular, non-group/world-writable files;
their exact SHA-256 is verified before execution without shell interpolation.
The pinned `competitionworker --supervisor-request` receives strict stdin JSON
with `operation: run_one_epoch`, round, and lease. It authenticates the current
round, installed runtime manifest, and live supervisor fence, then starts/resumes
the existing one-shot worker. It cannot switch into another round or mode;
explicit v2 staging is authenticated from the same frozen policy and envelope.
Claims and evaluation heartbeats retain the singleton slot and check the bound
round/fence; a read-only lease monitor stops an orphaned launcher without renewing
its dead parent. Cancellation signals cleanup before a bounded process stop.
The preparation adapter gets
`operation: propagate_and_qualify`, immutable activation intent, and lease. It
idempotently verifies the intent and live lease, publishes the exact
`CompetitionRuntimeManifest` into versioned COMPLIANCE storage, authenticates its
retained-version readback, and obtains the independent API acknowledgment before
host qualification. `competition.reward.v2.runtime-blob.v1` never falls back to
`generation-N.json` filesystem envelopes; that directory loader is legacy-only.
The adapter must run production
host/containment/archive/worker qualification, and obtain independently durable
API and worker readiness receipts. Receipts bind the exact manifest commit/digest,
reserved round UUID, canonical source-lock digest, runtime image and qualification. The loader reads
installed bytes, not request-body claims. Both roles must acknowledge before
activation. All Git/network work remains outside database transactions.

For the worker acknowledgment the adapter runs the concrete qualified command
`competitionworker --source-readiness=N`. This derives source-only settings from
the persisted intent, loads the installed envelope, probes the real host and
archive, and requires its host-rebaseline marker to name `target_round_id` before
recording a worker receipt. The API acknowledges its own installation through
the authenticated source-readiness endpoint. The adapter must obtain both; a
successful exit without those exact durable receipts is retried, not cached as
`preparation_ready`. Main competition configuration is not hand-edited per epoch.

In the trusted preparation and host-qualification settings, `api_image_digest`
is an optional deployment restriction, not a requirement to freeze main for a
season. Leave it empty when main API deployments advance independently. The
adapter still authenticates the HTTPS response against the separately read,
immutable API readiness row, binds its actual runtime digest for this intent,
and rejects a different identity at completion. Host qualification records that
observed digest in its retained profile/result. An existing intent reuses its
original valid acknowledgment without rewriting it after a rollout; a new intent
can acknowledge the new main release. If an explicit digest is configured, it
remains a strict restriction. Worker identity, pinned programs, exact source and
both-role quorum requirements are unchanged.

The repository now supplies that concrete worker stdin mode; point the worker
setting at its independently qualified release binary and pin the binary digest.
It still requires its normal production evaluator configuration and privileges.
No isolated-build broker or production adapter is installed merely by checking
out these changes. Install root-owned, hash-pinned release binaries and separate
production preparation/qualification/build inventories and state; the existing
staging unit/inventories are not production credentials or readiness evidence.
Until these integrations are independently qualified, enablement remains blocked.
Under elected-winner policy a missing build broker blocks the approved winner;
it must not silently substitute carry-forward. Missing preparation blocks opening.

Source-promoter, reviewer, scheduler, submitter and settlement authority stay
separate. The two-model Snow union owns code review and honesty/safety judgment; Sol 6.1
max implements and performs authorized source/config merges, while independent
Sol 6.1 low owns tests. Unattended operation grants no review authority.
Before approving anything, the trusted main-environment reviewer can materialize
the exact sealed patch and original archived score bytes without an existing review:

```sh
sim-latency source-review-packet --round-id=ROUND_ID --job-id=JOB_ID \
  --out-dir=/secure/review/new-candidate-directory
```

This read-only command requires existing trusted database/archive access. It
creates a new mode-0700 directory with read-only evidence, verifies archive
identities, exports no seed/request secrets, and never records approval. Inspect
the code and all six criteria. Copy the packet's `review-template.json` into a
new private `review.json`; do not edit the read-only evidence packet. Fill every
actual finding, explicit dispositions, public reasons and private-evidence
digest before submitting with `review-v2`. The template includes exact evidence
identities and expected revision but is deliberately invalid until real review
is complete; it never supplies passing findings. After a
genuine separate source approval, Sol 6.1 max may prepare a proposal ahead of the deadline:

```sh
sim-latency stage-reviewed-source --review-id=REVIEW_ID \
  --state-dir=/secure/automation/state \
  --build-command=/secure/automation/isolated-source-build \
  --build-sha256=REPLACE_WITH_REVIEWED_EXECUTABLE_SHA256
```

This clones all ten pinned repositories, applies the exact authenticated patch,
checks its tree and protected runner, and invokes a separately qualified isolated
build/test broker. It pushes nothing. The broker receives only an allowlisted
environment (no inherited main, database, vault, Git-push or seed credentials),
explicit `config/local`/`vault/local` paths, and the
`offline_evaluator_containment_v1` contract. It must run candidate builds/tests
inside the existing offline evaluator/Docker containment boundary, never ordinary
host `go test`, and must not expose source-origin credentials or host Git config.
Its strict receipt binds review/patch/full-source identities, rebuilt evaluator
image, qualification, containment proof, independent test-evidence digest, and
Sol 6.1 low test ownership. Missing or failed isolation evidence does not produce
a selectable proposal. The trusted completion time cannot be backdated.

For historical policies only, at the deadline the supervisor selects a still-approved, unchanged-base,
build-ready proposal (raw score, original admission time, then job id); late/obsolete/unready proposals
leave safe carry-forward. Build readiness is distinct from the later full
16-hour preparation interval. Completed runtime preparation is checkpointed so
the supervisor does not rerun expensive qualification every poll while waiting.
The v2 review score digest is the exact authenticated archived `score.json`
byte digest. Parsed score projections never substitute a reserialized hash.

For a genuine structured v2 review, put a private scoped reviewer token in
`SIM_LATENCY_REVIEWER_TOKEN_FILE` and use the complete request body, including
idempotency key, expected revision, exact evidence identities, six findings, and
distinct reward/source dispositions:

```sh
./run-main.sh review-v2 --round-id ROUND_ID --evidence /secure/review/request.json
```

The authenticated principal supplies reviewer identity; the controller computes
replacements from the original sealed ranking. Keep legacy `epoch-review` commands
for v1. V2 review does not require an operator token or acquire `RUN-MAIN.lock`;
it remains available while the supervisor runs. Supervision uses its fenced
database lease, and review uses API idempotency/revision guards. Sol 6.1 low
verification must cover carry-forward margin/generation mapping,
review-independent progression, exact blob/patch binding, partial-push/lost-receipt
recovery, drift/revocation, failed readiness, fencing, crash after round creation,
and the six-epoch limit. Sol 6.1 max diagnoses/fixes failures, then Sol 6.1 low reruns.
The schema-2 source-lock reader needs separate evaluator release qualification.

## Before starting

The main environment must contain the reviewed `competition.yml` config and
vault resources, PostgreSQL/Redis migrations, MinIO retention and replication,
and the Grafana alert route. Port-forward PostgreSQL and Redis to localhost when
the worker is run from this host. Set an exact control-plane image identity in
`WARP_IMAGE_DIGEST`; this is recorded per evaluation but is not a scoring input.

The following requirements describe production and the legacy staging-alias
workflow, not explicit v2 staging. Every newly eligible production epoch
checkpoint in `config/main/sim-latency.yml` must list exactly these
ten repositories: `server`, `connect`, `sdk`, `proxy`, `glog`,
`goidenticons`, `userwireguard`, `sn`, `warp`, and `gvisor`. Each commit
must be reachable from that repository's remote `sim-latency` branch, and the active epoch commit must
be the branch head before promotion begins. Never fill a missing dependency
from a local checkout, its default branch, or the evaluator builder's current
`HEAD`; those sources are deliberately excluded from the evaluation identity.
Before legacy staging, each repository must also expose a `sim-latency-staging` branch
whose head exactly equals its epoch-zero commit. The harness verifies all ten
remote aliases before it creates, evaluates, or advances a staging epoch.
Staging branches never receive a winner or advance automatically; they isolate
the name used for pre-production coordination while retaining the exact frozen
baseline source content and evaluator protocol. A reviewed evaluator repair is
a separate release operation at a drained staging-round boundary, not a
winner promotion or a change to a round already accepting/evaluating work.

The active production and staging protocols require exactly ten repositories,
never a retired placeholder pin. Server's `go.mod` replaces `gvisor.dev/gvisor`
with the `../gvisor` sibling. Current tools reject mode-mismatched, extra, or
incomplete checkpoints and source locks; they never infer a missing
commit. Prepare a reviewed complete checkpoint, matching remote aliases, and a
new evaluator image at the drained release boundary. Preserve old eight- and
ten-repository images, locks, baseline artifacts, and round evidence as
historical records; do not rewrite them to claim the new source identity.

The continuously deployed main control-plane graph can later add or remove a
module without changing that frozen evaluator checkpoint. Coverage checks inspect
the exact evaluated server commit's `go.mod`, never moving main. A newly frozen
base that requires an unpinned local repository fails closed and requires a
separately reviewed source-protocol/image expansion; it cannot borrow main's
checkout. Current source loading, shell validators, image copies, and new epoch
records require the exact ten-repository set, including Warp and gvisor, in
both modes. Historical
ten-repository locks and images remain unchanged evidence; the local
gVisor fork must not be inserted into their source identities. Those checkpoints
fail closed for new activations until a separately frozen and qualified
mode-correct checkpoint, matching config, and evaluator image are approved.
The legacy `staging-source-check` command rejects explicit v2 staging manifests;
use `source-record --staging` for the separate v2 ledger.

Create a mode-0600 file containing the operator bearer token, then export:

```sh
export SIM_LATENCY_OPERATOR_TOKEN_FILE=/secure/path/operator.token
export SIM_LATENCY_REVIEWER_ID=agent-reviewer-id
export SIM_LATENCY_STATE_DIR=/var/lib/urnetwork/sim-latency
export WARP_IMAGE_DIGEST=sha256:REPLACE_WITH_THE_RUNNING_WORKER_DIGEST
```

For epoch 1, optionally set `SIM_LATENCY_FIRST_OPENS_AT` to an RFC3339 instant.
Every round is exactly seven days and uses an end-exclusive admission window.
Later rounds open after the previous epoch drains, is reviewed, and is promoted.
For these legacy manual commands, `SIM_LATENCY_PREPARATION_SECONDS=57600`
is the historical 16-hour helper default, not the adopted v2 production policy.
Current v2 production sets `preparation_window_seconds: 0`; a legacy environment
variable must not override that immutable policy or waive readiness. This
readiness rebaseline is not the
scoring control. The worker establishes the epoch's one immutable nine-run
`baseline.json` before it builds the first accepted submission.

Run launch preflight and retain its passing JSON before the first epoch. Then:

```sh
cd /home/by/urnetwork/server/connect/sim-latency
./run-main.sh staging
./run-main.sh advance-staging
```

The staging era can contain any number of sequential epochs, beginning at
epoch zero. `staging` creates a 48-hour round by default, or returns the current
scheduled/open/grading round. Share its `round_id` with the Apex integration.
Fee-free staging patches traverse the real FIFO, isolation, evaluation,
scoring, embargo, and authenticated polling paths. Every staging epoch uses
frozen source epoch zero and automatically finalizes once admission closes and
the worker drains all accepted jobs. It names the highest-ranked placeable
candidate passing every gate, regardless of significance or takeover margin.
If none qualifies, there is no winner. The default leaderboard remains
production-only; its
`include_staging=true` view publishes the finalized staging epoch with
`staging: true`, its `winner_job_id`, and a matching winning entry. Staging
entries, including the winner, initially retain
`honesty_review: not_reviewed`. An agent may separately run
`staging-review --epoch N next` and, after real honesty/safety review,
`staging-review --epoch N approve` to mark the exact finalized winner
`approved` for Apex testing. This is append-only and never promotes source or
changes production-winner state. New staging rounds freeze a zero takeover
margin; historical rounds, including epoch 9, retain their original margin.
The [release record](launch/STAGING-5-RELEASE.md) retains the historical
epoch-5 opening worker identity and policy.

For the polling proof, record both compatibility `state` and additive
`evaluation_status`. Before publication, `state` remains `completed` for both
terminal outcomes, while `evaluation_status` distinguishes `completed` scoring
from terminal `failed` work. A failed response may add only the message-free
reviewed `evaluation_failure` tuple (`kind`, `code`, `retriable:false`); score,
gates, diagnostics, full `eval_error`, and readiness remain embargoed. Absence
of that tuple for an unknown code is valid. A rolling-deployment response with
no `evaluation_status` leaves legacy `completed` outcome-neutral. Neither the
new failure signal nor staging cancellation publishes an epoch early; capture
the finalized polling/leaderboard reconciliation separately.

Staging uses the same append-only one-baseline-per-round score bundle as
production, but it does not require the separately promoted host-readiness
identity used as a production launch gate. Production retains that exact-round
requirement. A staging worker may bootstrap a newly pinned evaluator from the
complete prior root-owned containment record when only the frozen
qualification/image identity is stale. Every new attempt must still pass all
current-image containment, isolation, cleanup, and artifact-integrity gates;
this exception never makes the host production-eligible.

### Evaluator repair between staging rounds

Test the exact frozen source, not only the operator's `main` checkout. The
[epoch-4 G5 investigation](launch/STAGING-4-G5-INCIDENT.md) found that a source
branch omitted a database fix already present in the qualified baseline and
on `main`. Pulling the API/worker cannot repair such an evaluator.

For the approved epoch-5 repair, Sol 6.1 max owns the source pull/merge and release
review; Sol 6.1 low owns independent regression and image validation. Require
the relevant tests to exist and pass in the image build, review all ten
pinned repositories, and retain the new source-lock and image digests. Do not
change active source aliases, configuration, or installed evaluator commands
while epoch 4 is open or draining. After it finalizes, verify the replacement
source/image deployment before creating epoch 5; do not use `advance-staging`
to create that next round against the old pin. Preserve every historical
round's policy and outcomes, and require a fresh production rebaseline before
public launch.

Before qualifying the repaired evaluator, Sol 6.1 low must independently
exercise the deterministic regressions for process-group cancellation,
retained-evidence memory budgeting, PostgreSQL/Redis failure detection, terminal
candidate exits, typed cancellation classification, and durable failed-attempt
archival. Sol 6.1 max reviews both the fixes and their red/green evidence. Keep
the existing G5/G6 thresholds and replicate counts; these are correctness and
containment repairs, not statistical relaxations. Recreate the containment
record for the new evidence-memory boundary instead of reusing the prior host
attestation.

`advance-staging` is the ordinary staging handoff. It atomically closes new
admission without rewriting the published schedule or canceling accepted work.
Before closing, it runs a root-owned evaluator/database preflight; a failure
therefore leaves the round open and retryable. It then runs the worker until
every FIFO submission is terminal, finalizes and reveals the round with its
automatic eligible winner or no winner, and creates the next open staging
epoch. It never exits 20 for staging review or invokes source promotion.
The command returns only after the next `round_id` is ready to share. `staging-worker`
remains available when the original admission window should run to its natural
end. Set `SIM_LATENCY_STAGING_WINDOW_SECONDS` to 60 through 604800 seconds when
a different test window is needed. `staging --replace-current` explicitly
cancels a legacy or unwanted current staging epoch and its queued jobs before
creating the next one; it fails closed if any job is running. Starting
production epoch 1 ends the staging era and atomically cancels queued staging
work; it refuses to race an evaluation that is still running.

After the staging admission and polling proof is captured, start the season:

```sh
cd /home/by/urnetwork/server/connect/sim-latency
./run-main.sh run
```

Starting early is safe. The worker begins its 15-second heartbeat immediately,
but the database claim boundary admits no evaluation before `opens_at`. Jobs
submitted before `opens_at` or at/after `closes_at` are terminally discarded.
After close, the worker drains every accepted FIFO job (each bounded to three
hours) and exits. Results remain embargoed until all jobs are terminal and the
review process finalizes the epoch.

## Mandatory candidate review

This section and its commands apply only to production epochs 1 through 6.
They must not be used to review or promote a staging result.

Exit status 20 means the harness has materialized the next ranked significant
candidate in a private mode-0700 temporary directory. Inspect `candidate.json`,
`score.json`, and `canonical.patch`, and review the patched code for all of the
following:

- no changes to `connect/sim-latency` or any trusted scoring/evaluator input;
- no fabricated measurements, disabled checks, special-cased workload data,
  hidden-seed inference, input gaming, or benchmark detection;
- no filesystem, network, namespace, Docker-socket, host, or secret escape;
- no credential access or exfiltration path;
- no unsafe behavior, persistence, denial of service, or unrelated product
  change; and
- a plausible causal connection between the allowed Go change and the measured
  improvement.

Write a JSON object containing the evidence inspected and an explicit boolean
finding for every item. Keep it mode 0600. If honest and safe:

```sh
./run-main.sh approve --epoch N --job-id ID \
  --evidence /secure/path/review.json \
  --reason 'honest allowed-path optimization; no scoring or sandbox tampering'
```

If any check fails:

```sh
./run-main.sh reject --epoch N --job-id ID \
  --evidence /secure/path/review.json \
  --reason 'specific dishonest or unsafe behavior found'
```

Rejection is append-only and advances to the next ranked significant candidate
without materializing a second temporary directory. The harness pauses again
with status 20; run `./run-main.sh candidate --epoch N` to materialize that
candidate before reviewing it. If candidates are exhausted, it records
a no-winner transition and carries the exact incumbent commits and significance
threshold into the next epoch. Approval authenticates the reviewed score and
patch, clones all ten locked repositories into a new temporary directory,
checks out the frozen `sim-latency` branch heads, applies the winner to the
evaluated server surface, verifies every dependency and the protected runner
tree are unchanged, pushes changed source branches, and pushes the config ledger
last. It then starts the next epoch. Temporary candidate and promotion
directories are deleted after use.

After epoch 6 is reviewed, `run-main.sh` exits zero. A canceled round, missing
source commit, source/API epoch mismatch, failed worker, incomplete drain,
invalid review evidence, failed push, or unavailable dependency exits nonzero
without advancing the source ledger.
