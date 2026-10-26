# Held-out input preparation

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

## Integrated operator draft — not yet qualified

All files below are **uncompiled and unexecuted**. Only static editing and
`gofmt` parsing have occurred. No actual Stage A margin, real hidden seeds,
provider inputs, finalized requests or held-out measurements have been created.
Do not run even the synthetic qualification alongside a timed evaluation.

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
draft's exact digest. Copy **all fourteen** `held-out-*.go.txt` files into the
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
