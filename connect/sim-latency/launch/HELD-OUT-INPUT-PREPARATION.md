# Held-out input preparation

The separately approved
[October 26 amendment](CALIBRATION-AMENDMENT-2026-10-26.md) is selected only by
`launch-3a-bridge-3b-2026-10-26-v1`. Its executable extension is documented at
the end of this file. The original five-pair/five-seed default and its retained
qualification snapshot below are historical contracts, not silently changed
three-pair results.

This is an execution checklist for Stage B of the
[frozen statistical protocol](CALIBRATION-PROTOCOL-2026-10-25.md), not a new
statistical rule, accepted calibration or authorization to start measurements.
Stage A must finish and its accepted margin must be frozen first. A faster
reference still needs correctness and independent directional qualification.

## Bind seeds to the workload actually executed

The simulator consumes the seed and fleet in `providers.yml`. The evaluator
validates `round_seed_hex` but does not regenerate that file from the request.
Changing only that request field would not create an independent workload.
The public-seed Stage A fixture intentionally supplies its fixed public
workload; do not copy those provider bytes into five supposedly independent
Stage B rounds.

For each fresh private 32-byte seed, use the frozen source's
`controller.ControlPlaneWorkloadGenerator.Generate` with the frozen evaluation
policy. This is the production generator, including its domain-separated
`workloadSeed` derivation; do not truncate the seed or treat its last bytes as
the simulator's integer seed. Retain the generated exact provider bytes, their
SHA-256 and decoded simulator seed. Independently verify that all five decoded
seeds and provider digests differ and exclude every prior pilot/public/campaign
workload seed. Generate no replacement after seeing results.

The same generated provider bytes must be used by the control and all three
references within one seed's round. Every request binds their digest; every
replicate's scorer manifest must bind the same digest and workload settings.
Different seeds must not share a baseline. Seed commitment/reveal records must
bind the private seed, its round, resulting workload digest and fixed source
and policy identities, not merely a human-readable seed label.

## Commit before observing held-out results

Freeze all five seed commitments, randomized per-seed reference order, round/job
identities, exact no-op/better/worse patch and build identities, source/image,
workload digests, accepted margin and typed policy before the first result.
Keep raw seeds private until the campaign ends. Preserve the generator source,
toolchain identity, independently checked input census and publication receipt.

Each seed's first reference invocation creates its one fresh R9 baseline.
That baseline's content digest cannot be known before measurement. Predeclare
this dependency rather than inventing a digest or claiming every final request
hash existed beforehand: the other two requests may bind only the exact
authenticated first invocation's baseline bytes in that same round. Preserve
their finalized request/provenance hashes before invoking them. No late-bound
field may change the seed, patch, reference order, policy, workload or source.

Authenticate the baseline's round, workload, source/scorer contract, R9 count,
margin and integrity before copying it read-only as `round-baseline.json`.
Require each consuming request's `round_baseline_sha256` and emitted baseline
digest to match the original. Never remeasure, replace or choose a favorable
control for a later reference. Retain all original control artifacts as well
as the shared baseline manifest.

Candidate success is not a condition for choosing the control. A slow,
non-placeable or non-significant first candidate does not authorize discarding
its authenticated control or generating another one. If the first invocation
does not yield a complete authenticated control and the required evidence,
stop for diagnosis under the frozen retry/deadline rules; do not silently
advance to another reference to obtain a replacement control. A recovered
baseline alone does not turn a failed candidate into a completed reference.

The live worker's validation and append-only storage are separate steps:
`controller/competition_controller.go` validates baseline bytes against the
frozen round in `validateRoundBaselineJson`, authenticates returned artifacts
before accepting them in the evaluator path, and freezes supplied baseline
bytes independently of candidate eligibility in the completion transaction.
The offline Stage B wrapper must enforce equivalent provenance and first-control
identity; mere existence of `baseline.json` is insufficient. These source
observations are design constraints, not a qualified offline implementation.

## Qualification and serial execution

Before using a preparation tool on real held-out inputs, test its positive
path and deterministic refusals for repeated effective seeds, unchanged
provider bytes despite changed request seeds, wrong workload/policy, reordered
references, and baseline substitution or cross-round reuse. A compile-only or
unset-environment pass is insufficient.

Use actual Go 1.27.3 and the canonical exclusive host slot. Do not overlap
builds, tests, pressure probes or other scoring with measured runs. Keep every
outcome, original whole-job deadline and cleanup receipt. Apply the original
four-of-five ordering/no-op acceptance rules without optional stopping,
favorable-draw retries or post-result patch/margin tuning.

No Stage B generator or input set is qualified by this checklist. Its purpose
is to make the next executable preparation bind real independent workloads
and the deliberately shared same-round control correctly.

## Original operator qualification snapshot

The original fourteen-file private snapshot is being independently qualified;
the amended source below is a different, still **uncompiled and unexecuted**
source set. Do not treat an original-snapshot pass as qualification of changed
bytes. No actual Stage A margin, real hidden seeds or held-out measurements
have been created by these drafts. Do not run synthetic qualification alongside
a timed evaluation.

| Draft | Responsibility |
| --- | --- |
| [Operator core](held-out-operator.go.txt) | Exact release/report/policy/reference pins, five native workloads, fifteen committed jobs, exclusive private/public publication and complete re-verification |
| [Exclusion census](held-out-exclusion-census.go.txt) | Checks every raw/effective seed, provider digest and retired commitment against its pinned retained source, preserving full int64 precision |
| [Serial executor](held-out-execution.go.txt) | Original per-job deadlines, trusted-script invocation, native replay, first-control provenance, baseline-only finalization, cleanup and fixed-screen acceptance |
| [Executable entry](held-out-command.go.txt) | Explicit `prepare`, `verify`, `execute` and closure-gated `reveal` operations in a separately built test executable |
| [Preparation tests](held-out-operator-tests.go.txt), [execution tests](held-out-execution-tests.go.txt) | Synthetic composed positive/refusal paths, immutable publication, changed report/source/order/control, deadline and failure retention |
| [Workload binding](held-out-workload-binding.go.txt), [tests](held-out-workload-binding-tests.go.txt) | Actual control-plane generator, independent seed derivation, exact native encoder/YAML reconstruction and five-workload uniqueness |
| [Reference order](held-out-reference-order.go.txt), [tests](held-out-reference-order-tests.go.txt) | All six permutations with unbiased, bounded rejection sampling |
| [Margin pin](held-out-margin-pin.go.txt), [tests](held-out-margin-pin-tests.go.txt) | Native report decoding, independent byte pins, complete accepted sample and supported margin grid |
| [Request finalization](held-out-request-finalization.go.txt), [tests](held-out-request-finalization-tests.go.txt) | Only the two declared baseline fields may change from exact committed request bytes |

The real entry point has no alternate PRNG, reduced seed count, replicate,
window, margin, image, source, retry or deadline option. It always uses
`crypto/rand.Reader`, the native generator, the existing canonical scoring
lock, management CPUs 20,22, root and actual Go 1.27.3. It invokes only the
unchanged installed isolated evaluator bundle. It never mutates main services,
databases, source pins or production admission.

The executor makes one attempt per job and never automatically retries. A
statistical/gate failure is retained while remaining committed jobs proceed.
Infrastructure, identity, timeout or cleanup failure stops execution with an
incomplete retained result. An interrupted campaign cannot be restarted by
reinvoking the command: its exclusive start/result files refuse that action.
Any proposed genuine infrastructure recovery needs independent diagnosis and
review under the existing three-attempt ceiling and the original three-hour
deadline; this draft provides no retry path that could reset either identity
or clock. Do not re-run `prepare` into another root to replace a failed draw.

## Qualification workspace and exact command

Use a separate checkout of the qualified analyzer source
`30f340a2779699225ab40ec7f3c5d978525276d2`, with the same independently checked
nine dependency pins from [the earlier tool qualification](INPUT-TOOL-QUALIFICATION-2026-10-26.md).
The evaluated c1 checkout alone does **not** contain the calibration report,
loader or replay APIs used by these drafts. Do not copy new code into the
evaluated application or install it over the trusted analyzer.

Before compiling, verify that `controller/competition_controller.go` and
`controller/competition_sim_latency_controller.go` in that analyzer checkout
byte-match clean evaluated server c1. Their two full-file pins are required
by the operator too. Preserve the source/revision/workspace census and every
draft's exact digest. The **original fourteen-file snapshot** excludes the
three new `held-out-amendment`, `held-out-bridge` and
`held-out-amendment-tests` files. Copy those original fourteen pinned files into the
isolated simulator package, replacing `-` with `_` in each basename and adding
`_test.go`; the command file deliberately has no second `main` function.

At an explicitly authorized exclusive idle slot, with root, `taskset -c 20,22`,
the existing canonical lock held by the qualification shell, Go 1.27.3/local,
offline dependencies and only the previously qualified synthetic environment:

```bash
# Explicit private paths; this is a future qualification command, not a live handle.
export GOTOOLCHAIN=local GOMAXPROCS=2 GOFLAGS=-p=2 GOPROXY=off GOSUMDB=off
export GOWORK=/explicit/private/frozen-nine.work
unset SIM_LATENCY_HELD_OUT_COMMAND SIM_LATENCY_HELD_OUT_REAL_OPT_IN
unset SIM_LATENCY_HELD_OUT_CONFIG SIM_LATENCY_HELD_OUT_CONFIG_SHA256
cd /explicit/private/qualified-analyzer/server/connect/sim-latency
go test -race -c -o /explicit/private/qualification/held-out-operator.test .
for pass in 1 2 3; do
  go tool test2json -t -p github.com/urnetwork/server/connect/sim-latency \
    /explicit/private/qualification/held-out-operator.test \
    -test.v -test.count=1 -test.run '^TestHeldOut' -test.timeout=15m \
    > "/explicit/private/qualification/pass-$pass.json" \
    2> "/explicit/private/qualification/pass-$pass.stderr"
done
```

Use a fail-fast shell; retain any failed build/pass and do not select the first
three convenient successes. Expected selection is **62 top-level passes** per
run: 61 substantive deterministic guards plus the explicitly inert command
entry test, with zero skips. Inspect each JSON stream for an overall package
pass, exactly 62 test passes, zero skips and no failures. Recheck source,
workspace, generator and binary hashes after all three runs. Build the exact
executable first and run that same binary three times; qualifying one binary
then silently rebuilding another is not sufficient.

These tests use synthetic acceptance/qualification shapes and explicit host,
process and replay hooks. Their composed preparation tests also call the real
native workload generator with fixed synthetic seeds and a small 25-provider
profile. They do not create real held-out entropy, score actual references,
prove live containment or constitute accepted Stage A analysis. Native Docker
execution remains the unchanged previously qualified evaluator/replay path;
the real artifacts from every Stage B job must still authenticate successfully.

## Required private configuration and receipts

Every input file uses `{ "path": "/canonical/absolute/path", "sha256":
"<independently published lowercase digest>" }`. Expected digests come from
published/reviewed receipts, not a tool that hashes an arbitrary input and
calls that same value an independent pin. Configuration files must be owned
mode 0600, under canonical private directories. No symlink, FIFO, traversal,
overwrite or unknown/duplicate JSON field is accepted.

The `sim-latency-held-out-preparation` configuration has schema 1 and these
fields, all required:

| Field | Required input |
| --- | --- |
| `private_root`, `public_plan_path` | A new campaign directory under an existing operator-owned 0700 parent, and a distinct new public plan destination |
| `template` | Exact first accepted Stage A request bytes; the complete policy/patch scope and offline fixture labels remain frozen |
| `stage_a_report`, `stage_a_inputs` | Genuine published accepted native analysis and its five-position input manifest; ten successful native replay/cleanup observations are required |
| `source_lock` | The full ten-repository lock, independently canonicalized to `5e00bf5e…` and evaluated c1 |
| `generator_files` | Two `{ "analyzer": <file pin>, "frozen": <file pin> }` pairs for the controller files named above |
| `exclusions` | A reviewed, complete-through-preparation seed/provider/retirement census with pinned original sources |
| `references` | Exactly three `heldOutReference` records, roles `no-op`, `better`, `worse`, with exact patch, native build, image/binary/commit and qualification receipt pins |
| `qualification` | Published qualification of this exact executable, actual Go/analyzer/source identities, all source pins and three complete race-pass evidence pins |
| `scripts` | Exact installed evaluator, network helper, Compose and hash-local-mount file pins; known release digests/directory cannot be overridden |

The Go structs in the linked drafts define exact snake-case JSON keys and
receipt schemas. `heldOutToolQualification` requires schema 1, kind
`sim-latency-held-out-operator-qualification`, accepted=true, race=true,
consecutive_passes=3, skips=0 and the exact executable SHA. Do not issue that
receipt until the above qualification really passes and its evidence is
published.

Each reference separately requires pinned native build output plus normalized,
reviewed `sim-latency-held-out-reference-build`, `-correctness` and `-direction`
receipts. Each binds evaluated c1, the canonical base source lock/image, role,
patch, candidate commit/image/binary, Go 1.27.3 and original evidence files.
Direction must be `faster`, `no-op` or `slower` respectively. These are reviewed
summaries of actual preserved evidence, not permission to write accepted=true
for a proposed image. The existing [reference build readbacks](INPUT-TOOL-QUALIFICATION-2026-10-26.md)
alone do not supply missing correctness or independent directional evidence.

The exclusion receipt's exact schema is `heldOutExclusions`. Its sources may
be retained JSON seed-reveal/campaign/retirement records, actual provider YAML,
or 32-byte seed `.bin` files. The census recognizes native raw/effective seed,
provider and commitment keys, verifies every identity in both directions and
automatically excludes native derivations of all known raw seeds as well as
legacy integers. It must include public seed 20261024, both current pilots,
all prior campaigns, failed/retired/precommitted seeds and their provider
digests—not only successful old runs. A complete=true assertion cannot hide
an identity present in a supplied source; independent review remains responsible
for including every relevant prior source in the first place.

## Prepare, publish, execute, close and reveal

For every operation, supply a private configuration and its independent digest,
and run the already-qualified executable, not `go run` or a rebuilt package:

```bash
env SIM_LATENCY_HELD_OUT_REAL_OPT_IN=1 \
  SIM_LATENCY_HELD_OUT_COMMAND=prepare \
  SIM_LATENCY_HELD_OUT_CONFIG=/explicit/private/preparation.json \
  SIM_LATENCY_HELD_OUT_CONFIG_SHA256=<published-configuration-digest> \
  taskset -c 20,22 /explicit/private/qualification/held-out-operator.test \
  -test.run '^TestHeldOutOperatorCommand$' -test.count=1 -test.timeout=0 -test.v
```

Capture stdout/stderr into new private evidence files. Summaries print only
plan digests or final screen status, never seeds/provider bytes/requests.
The command itself opens and holds the existing canonical scoring lock. Do
not wrap execution in a shorter suite timeout or overlap any other test,
build, probe or scoring. Each original job is independently capped at 10,800
seconds, including invocation/authentication/cleanup; kill grace cannot convert
an over-deadline outcome into success.

1. `prepare` authenticates the accepted Stage A pin before and after generation,
   draws all cryptographic secrets/ids/orders once, calls the native generator,
   verifies five distinct effective seeds/provider payloads and exclusions,
   then writes all fifteen exact private requests before a public plan exists.
   Private directories/files are 0700/0600 with exclusive creation and fsync.
   Failed preparations retain their started marker and partial private evidence.
2. Inspect and independently verify the output using `verify`. Its configuration
   is schema 1, kind `sim-latency-held-out-execution`, with `private_plan`,
   `public_plan` and `publication` file-pin objects. `verify` does not need a
   publication receipt yet and never starts a job; an empty path/digest pair in
   that required field is allowed for this operation only.
3. Commit, pull and push the **complete public plan** before any Stage B result.
   Do not publish `private-plan.json`, `configuration.json`, any provider file,
   precommitted/final request or private log. Preserve a separately reviewed
   `sim-latency-held-out-plan-publication` receipt binding the exact plan digest,
   commit/time, canonical repository, relative plan path, remote ref and original
   pull/push evidence. `execute` checks both its pins and the committed Git bytes/
   ancestry; it performs no fetch, commit or push itself.
4. Use `execute` with that execution configuration. All fifteen positions run
   serially in the committed order. First jobs have no baseline input. Later
   jobs receive only their same-round first control after native reconstruction,
   frozen source/workload/policy/R9/telemetry validation and evidence checks.
   The finalized request/provenance digest is persisted before invocation.
   Original worker artifacts, native replay receipts, exact control/candidate
   ids, p-values, deadlines and before/after cleanup inventory remain private.
5. Retain `campaign-result.json` whether accepted, failed or interrupted. The
   fixed rule requires four of five ordered median scores, all five placeable
   non-takeover no-ops, complete jobs and integrity/cleanup evidence. A low
   no-op p-value remains visible even if the practical margin prevents takeover.
   Review/archive/publish the non-secret results and an explicit
   `sim-latency-held-out-campaign-closure` receipt; an interrupted campaign needs
   an explicit no-further-evaluations decision, not an automatic seed reveal.
6. `reveal` uses schema 1, kind `sim-latency-held-out-reveal`, containing
   `execution`, `result`, `closure` and a new `reveal_path`. It rechecks the
   original commitments, closed-result/closure pins and accepted result where
   complete, then publishes those original raw/effective seeds and their
   commitment checks. A second reveal cannot overwrite the first file.

Preserve all phase inputs/receipts, original evidence, source and executable
digests, then commit/pull/push each public phase. No accepted tool qualification,
margin or held-out screen waives exact-image host/live API/worker/provider,
durable artifact storage, monitoring or production activation gates. Opening
remains 2026-10-28T00:00:00Z; this draft does not change timing or acceptance.

## Approved named amendment: original three A, one fresh bridge, three B

Only selector `launch-3a-bridge-3b-2026-10-26-v1` enables this section. Omission
continues to require the historical five A/five B sample and four-of-five B
ordering. The amendment's approval bytes are independently pinned, not inferred
from three available results:

- Approval: `CALIBRATION-AMENDMENT-2026-10-26.md`,
  `a794618968d840163952b7b2058100fd87b71ef7ccc44919ba590c5a253f643c`.
- Full retained original five-position plan:
  `24f9b6ea9fe6fbfb4381ca29535b81a1cd4966b397c701b04f5b09ac74eb11d8`.
- Selected A positions are exactly 01/02/03, in original order and original
  source/image. Original 04/05 requests/provenance remain authenticated and
  must have no started, terminal or result markers. This is deadline-based
  sample reduction, not selection by direction or p-value.

New source extensions are [protocol/release binding](held-out-amendment.go.txt),
[fixed-margin native bridge verification](held-out-bridge.go.txt),
[amendment/bridge guards](held-out-amendment-tests.go.txt), and the separately
selectable [three-A assembler](held-out-three-a-input-assembly.go.txt) with
[tests](held-out-three-a-input-assembly-tests.go.txt). Current amended source
is static only; no compilation, test pass, formal Mini review or actual provider
qualification is claimed by this document.

### Fast independent original-three assembly

Copy only the two `held-out-three-a-input-assembly*.go.txt` files as distinct
`*_test.go` files into an isolated checkout of the newly qualified native
analyzer source. They depend on existing native calibration APIs and
`retainedCalibrationQualificationInputs`, not the other held-out drafts.
Build one race test binary, then run that exact binary three times selecting
`^TestHeldOutThreeAAssembly`: expected **five passes per run**, four substantive
guards plus the inert command, zero skips. Unset every
`SIM_LATENCY_ASSEMBLE_THREE_A_*` variable during qualification. Synthetic
discovery shapes are not score authenticity; native replay remains mandatory.

Only after qualification and explicit exclusive-idle authority, invoke that
same helper binary as root on CPUs 20,22 with:

```bash
export SIM_LATENCY_ASSEMBLE_THREE_A_PROTOCOL=launch-3a-bridge-3b-2026-10-26-v1
export SIM_LATENCY_ASSEMBLE_THREE_A_APPROVAL=/home/by/urnetwork/server/connect/sim-latency/launch/CALIBRATION-AMENDMENT-2026-10-26.md
export SIM_LATENCY_ASSEMBLE_THREE_A_APPROVAL_SHA256=a794618968d840163952b7b2058100fd87b71ef7ccc44919ba590c5a253f643c
export SIM_LATENCY_ASSEMBLE_THREE_A_PLAN=/home/by/urnetwork/server/connect/sim-latency/baseline/prelaunch-corrected-same-seed-r9-2026-10-25/predeclared/plan.json
export SIM_LATENCY_ASSEMBLE_THREE_A_PLAN_SHA256=24f9b6ea9fe6fbfb4381ca29535b81a1cd4966b397c701b04f5b09ac74eb11d8
export SIM_LATENCY_ASSEMBLE_THREE_A_LOCK=/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock
taskset -c 20,22 /explicit/private/qualified/assembly.test \
  -test.run '^TestHeldOutThreeAAssemblyCommand$' -test.count=1 -test.v
```

The helper independently owns/releases the existing canonical lock; do not
also hold a distinct outer descriptor while invoking it. It reads only the
original `cohort-01`, `cohort-02`, `cohort-03` roots under
`/home/by/urnetwork/temp/canceled-stop-calibration-20261025.0G7LKkuh`, never an
extracted/rewritten replacement. It exclusively creates mode-0400
`analysis-inputs-three-a-v1.json` there, fsyncs it and its directory, and prints
the actual new digest. Existing output or any partial/changed position refuses;
do not delete a failed output and retry without an independently diagnosed
correction. No bootstrap, scorer replay or margin is produced by assembly.

### Exact prospective native analysis command

Use the separately qualified **new native analyzer**, not the unchanged
installed `30f340a27` executable. Preserve new stdout/stderr files exclusively;
never overwrite an original five-pair record. Replace only the manifest digest
placeholder with the actual authenticated assembly output:

```bash
taskset -c 20,22 /explicit/private/qualified/sim-latency calibrate \
  --calibration-inputs=/home/by/urnetwork/temp/canceled-stop-calibration-20261025.0G7LKkuh/analysis-inputs-three-a-v1.json \
  --calibration-inputs-sha256=<actual-assembled-manifest-sha256> \
  --calibration-lock=/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock \
  --calibration-cpuset=20,22 \
  --calibration-protocol=launch-3a-bridge-3b-2026-10-26-v1 \
  --calibration-approval=/home/by/urnetwork/server/connect/sim-latency/launch/CALIBRATION-AMENDMENT-2026-10-26.md \
  --calibration-approval-sha256=a794618968d840163952b7b2058100fd87b71ef7ccc44919ba590c5a253f643c \
  --calibration-original-plan=/home/by/urnetwork/server/connect/sim-latency/baseline/prelaunch-corrected-same-seed-r9-2026-10-25/predeclared/plan.json \
  --calibration-original-plan-sha256=24f9b6ea9fe6fbfb4381ca29535b81a1cd4966b397c701b04f5b09ac74eb11d8
```

Require genuine native `accepted:true`, `cohorts:3`, 54 validated observations,
100,000 draws, unchanged PCG seeds/floor/0.94/noise/significance/max/grid rules,
all six successful original-C1 native replay/cleanup observations and exact
protocol/approval/original-position pins. Three pairs do not become five
same-image pairs. Publish the exact report, inputs and accepted margin before
any fresh bridge starts.

### Frozen margin, new C2 release and one precommitted bridge

The existing opt-in operator gains `freeze-margin` and `verify-bridge`.
`freeze-margin` consumes `heldOutMarginFreezeConfig`: schema 1, kind
`sim-latency-amended-margin-freeze`, the named protocol, approval/original-plan
pins, original source/image, accepted A report/inputs and a new output path.
It authenticates A before and after producing a new public margin receipt;
it never selects another margin. `readHeldOutMarginFreeze` later checks the
receipt against the exact accepted original A again.

Publish a schema-1 `sim-latency-prospective-c2-instrument` descriptor matching
`heldOutAmendedRelease`, plus its independently qualified raw build/test
evidence. It must bind old C1 and the new source/image/binary/policy, both raw
source locks and canonical digests, and the minimal source delta. The operator
reads Git objects directly: C2 must be exactly one C1 child, with **only**
`connect/sim-latency/source_epoch.go` and `source_epoch_mono_test.go` changed.
The first file must equal the exact four approved replacements, not a current
main transplant; the prescribed test-file bytes are pinned too. All other nine
repository commits remain identical. The new raw source lock must explicitly
contain `candidate_path_base:"mono_root_v1"`; C1's omission is preserved.
Existing installed scripts, management bounds and scoring lock cannot be
redirected through this descriptor.

The public `heldOutBridgePlan` binds original A, margin freeze, new release,
source lock, exact complete fresh no-op request/build and margin. Supply a
separate pushed-publication receipt using the existing publication schema.
The existing qualified one-attempt evaluator runner, not this verifier, owns
the fresh R9/R9 execution and its original 10,800-second deadline. Freeze the
plan/publication before invocation; no old job/round/control may be reused.
The bridge uses the unchanged public workload/profile, not a hidden B seed.

`verify-bridge` consumes schema 1, kind
`sim-latency-prospective-c2-bridge-verification`, exact `bridge_plan`,
`bridge_publication`, `bridge_inputs` pins and a new `receipt_path`. The inputs
reuse the native one-cohort artifact schema, with the **new** source/image and
already frozen M. This is artifact verification, not a new margin analysis.
The verifier authenticates every original byte, native source/build provenance,
all six gates, typed null error, placeability, no no-op takeover, fresh execution
identities against all 54 old observations, original interval/cleanup, seven
worker bindings, all 465 evidence entries and all five completion-map bindings.
It runs actual immutable-image baseline/candidate replay and retains each
owner/cleanup observation. Exact duplicate replay inputs may reuse the same
already authenticated replay bytes; they are not claimed as extra measurements.

A gate/no-op failure retains the rejected receipt and stops; malformed or
infrastructure failure retains raw artifacts and cannot produce an accepted
receipt. No replacement, larger margin or reset clock is authorized. The
receipt's explicit claim is
`operational-compatibility-witness-not-statistical-equivalence`.

### Amended B preparation and qualification

An amended B preparation config adds `protocol` and complete `amendment` pins:
approval, original plan/source/image, new release, frozen margin, bridge plan,
bridge publication, bridge inputs and accepted bridge compatibility receipt.
`template` is the exact successful new-C2 bridge request, **not** an old A
request. The new typed policy must preserve every original evaluation field
except simulator/scorer binary identities and the already selected margin.
The exact operator qualification receipt also binds the named protocol and
approval/original-plan pins. No old executable qualification covers new bytes.

Copy all **nineteen** current `held-out-*.go.txt` files as `*_test.go` into an
isolated qualified analyzer checkout with the committed named-protocol native
files. Preserve a new source/binary/workspace census; do not overwrite the old
fourteen-file/62-test qualification snapshot. Run the same newly built race
binary three consecutive times with `^TestHeldOut`, all real operation/assembly
variables unset: expected **87 passes**, 85 substantive guards plus two inert
commands, zero skips. The native `^TestCalibration` selection separately has
63 tests, including the eight new protocol guards; validate it three times too.
Do not compile or test while a timed evaluation is active.

Successful preparation draws exactly three fresh cryptographic seeds, calls
the actual native generator for each, checks all exclusion/uniqueness/canonical
provider-byte invariants, and commits all three randomized orders and nine job
requests before a B result. Public projection and seed commitments bind old A,
new C2, margin and bridge explicitly, without hidden seed/effective seed/provider
bytes or private paths. Execute all nine jobs in original order and unchanged
original budgets; share only each round's first authenticated R9 baseline with
its other two references. Require **all three** ordered median triples, all
three placeable non-takeover no-ops and complete integrity/cleanup. No favorable
two-of-three subset or seed replacement is accepted. Original five/four
acceptance remains separate and unchanged.
