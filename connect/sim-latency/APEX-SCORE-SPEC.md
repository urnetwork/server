# Apex `ur_latency` score contract

Status: implementation contract

Score schema: `1`

Scorer version: `sim-latency-score/1`

This document is the complete scoring contract for an Apex `ur_latency`
round. Normative words such as MUST and MUST NOT are requirements. A round is
not open for submissions until its signed baseline manifest fixes all
round-specific values described below. Calibration chooses those values; it
does not change the formulas in this document.

## 1. Frozen inputs and evaluation identity

Every candidate replicate has one results CSV, schema-2 `run.json`, final
marker, stderr file, accounting snapshot, FindProviders2 sample corpus, and
resource report. Every metadata-bearing artifact carries the same non-empty
`evaluation_id`. The sidecar records a SHA-256 and byte count over the exact
CSV header and records emitted by the driver, and the final marker
authenticates that sidecar. The samples path MUST be the `stats_root` recorded
by that run and is isolated per job by the evaluator. These bindings prevent a
CSV or sample corpus from another job being combined with matching JSON
artifacts.

The detached-signature-verified same-round baseline manifest has
`score_schema: 1`, scorer version,
round id, providers-file SHA-256, exact run flags, request-timeout ceiling,
takeover margin, and an odd positive number of baseline replicates. It contains
the per-replicate baseline diagnostics defined in section 5. A candidate MUST
have exactly the same number of replicates. The replicate count and takeover
margin are immutable after the round opens. The first production manifest MUST
NOT be published until `APEX-CALIBRATION.md` shows that the chosen aggregation
satisfies the noise and separability launch gates in `FINALIZE.md`.

The scorer rejects a config hash, flag, timeout, schema, scorer-version, or
replicate-count mismatch. Official runs MUST carry a non-empty VCS revision,
`official: true`, and `build_modified: false`.

The scorer is deliberately file-only and does not hold control-plane signing
keys. Before invoking it, the trusted worker MUST verify the detached round
signature and supply the verified manifest's pinned SHA-256. The official
runner checks that hash again immediately before scoring and finalization.

## 2. Measured-window observations

A CSV row is included exactly when

```text
measure_start_ms <= t_start_ms < measure_end_ms
```

where the half-open window comes from that row's schema-2 sidecar. Rows whose
requests started before or after the window are excluded even if their bodies
overlap it.

At `measure_end_ms` the runner MUST stop admitting new crawls without
canceling the run context. Every crawl admitted before the boundary MUST be
allowed to finish or reach its own request-timeout deadline before the CSV is
sealed. A duration timer that cancels those in-flight crawls and then charges
them as failures is non-conforming. External TERM/INT interruption still
cancels immediately and makes the evaluation incomplete.

One row represents one request actually attempted by a crawl. A crawl canceled
at its deadline emits rows for requests already attempted; it does not create
synthetic rows for queued jobs or undiscovered descendants. G2 compares that
same attempted-request population, so cancellation cannot silently change the
volume definition.

A successful observation has all of these properties:

1. the HTTP status is exactly 200;
2. the leading line is newline-terminated valid JSON with a non-negative
   declared `size`;
3. exactly `size` bytes follow the JSON line;
4. when `Content-Length` is present it equals JSON-line bytes plus `size`;
5. header read, body read, and body close all finish without error; and
6. `total_ms` is finite, positive, and no greater than the request-timeout
   ceiling.

The immutable schema-2 simulator records any violation of items 2-5 with
`status=0`, preserving the bytes actually received. Non-200 responses,
request/dial errors, cancellations, timeouts, incomplete bodies, and short
bodies are failures. A schema-1 sidecar cannot prove body validation and is
therefore never officially scoreable.

For each included row, define its latency observation as

```text
L(row) = total_ms                 if the row is successful
       = request_timeout_ms       otherwise
```

The request-timeout ceiling is the positive integer in the signed baseline
manifest and MUST equal every candidate sidecar. Values above the ceiling,
negative values, empty measured windows, and NaN or infinity are malformed
inputs, not score values.

## 3. Quantiles

Every quantile in this schema uses Hyndman-Fan type 7 (the R and NumPy default).
For sorted values `x[0] ... x[n-1]`, `n > 0`, and `0 <= q <= 1`:

```text
h = q * (n - 1)
k = floor(h)
Q(q) = x[k]                                      if k = n - 1
     = x[k] + (h - k) * (x[k + 1] - x[k])       otherwise
```

No rounding occurs before JSON serialization.

## 4. Raw score and replicate aggregation

The raw score of one replicate is `Q(0.95)` over all of its `L(row)` values.
Lower is better. Failures are included at the timeout ceiling; timing is never
computed over successes alone.

For each performance scalar diagnostic, the baseline value is `Q(0.50)` over
baseline replicate values and the candidate value is `Q(0.50)` over candidate
replicate values. Because the signed replicate count is odd, this is the middle
order statistic. G3 accounting coverage and G4 sample-span coverage are safety
diagnostics and aggregate as the minimum replicate value. The official
`raw_score` is the candidate median. No best-of-run, retry, trimming, or
replacement of a failed replicate is allowed. Identical canonical patch bytes
in one round reuse the cached aggregate result.

## 5. Gates

All comparisons below are inclusive. Each gate is returned independently;
`placeable` is true only when G1-G6 all pass and `eval_error` is null.

Let `C(x)` and `B(x)` be the candidate and same-round baseline medians for
diagnostic `x`.

### G1 — success

```text
C(success_rate) >= 0.97
C(success_rate) >= B(success_rate) - 0.01
```

`success_rate` is successful included rows divided by all included rows.

### G2 — volume

For both attempted request count and received bytes:

```text
0.95 * B(value) <= C(value) <= 1.05 * B(value)
```

Received bytes are the non-negative `bytes` values of every included row,
including failed/incomplete rows. Baseline count and bytes MUST both be
positive.

### G3 — path integrity

For every candidate replicate:

```text
provider_egress_bytes / client_received_bytes >= 0.95
```

The numerator comes from the evaluator's immutable server-side accounting
snapshot, whose `measure_start_ms` and `measure_end_ms` MUST exactly equal the
run sidecar. A zero client denominator has coverage zero. The aggregate
diagnostic is the minimum replicate coverage. Missing, incomplete, negative,
window-mismatched, or identity-mismatched accounting is an infrastructure
evaluation error.

### G4 — matchmaking

Every measured-window FindProviders2 sample MUST have `pool_count > 0` and a
non-empty retained `candidates` list, and every replicate MUST contain at least
one measured-window sample. Its `load_millis` MUST be finite and non-negative.
For every replicate, the first-to-last in-window sample span MUST cover at
least 90% of the measured window:

```text
(max(sample_time_ms) - min(sample_time_ms)) /
    (measure_end_ms - measure_start_ms) >= 0.90
```

The baseline builder rejects a replicate below this bound. A candidate below
it fails G4 and is non-placeable. This prevents a submission from suppressing
matchmaking work after an early valid observation. For each replicate, compute
type-7 p05 of `pool_count`; the cross-replicate median MUST preserve at least
90% of the same-round baseline. This also prevents a submission from buying
lower matchmaking latency by collapsing the eligible provider pool. The other
two inclusive conditions are

```text
C(per_replicate_load_millis_p95) <=
    1.25 * B(per_replicate_load_millis_p95)

C(per_replicate_pool_count_p05) >=
    0.90 * B(per_replicate_pool_count_p05)
```

where each within-replicate quantile and cross-replicate median uses type 7.

### G5 — stability

Every replicate MUST have a hash-valid final marker written after joined
teardown, `completion_state: "complete"`, a fully established non-empty warm
pool, a clean official build, and no panic, recovered panic, fatal runtime
error, restart, missing service, or unclean-drain signature in stderr. A real
stability violation is a typed submission evaluation error. A missing or
malformed mandatory artifact is an infrastructure evaluation error.

### G6 — resources

Every resource report MUST cover the complete evaluation and show exit code
zero, no OOM, no hard kill, no cgroup/limit escape, and no missing measurement.
Its evaluation id and non-empty cgroup id identify the job, its measurement
start MUST be no later than the measured-window start, and its measurement end
MUST be no earlier than the sidecar completion time.
A recorded violation is a typed submission evaluation error; a missing or
malformed report is an infrastructure evaluation error.

## 6. Display normalization and takeover

For a placeable candidate with positive raw score:

```text
display_score_unclamped = 100 * B(raw_score) / C(raw_score)
display_score = min(200, max(1, display_score_unclamped))
```

Thus the same-round baseline displays as exactly 100 and every valid display
score is finite and non-zero. A candidate is eligible to take over an incumbent
with aggregate raw score `I` exactly when

```text
C(raw_score) <= I * (1 - takeover_margin)
```

The signed round manifest fixes `0 < takeover_margin <= 0.5` before submission.
The scorer reports the raw threshold and eligibility against the no-op baseline
as diagnostics. Apex applies the same formula to the current cached incumbent;
that incumbent score is control-plane state, not a candidate scorer input.
Non-placeable results never take over regardless of their numeric raw score.

## 7. Missing, empty, and non-finite inputs

The scorer fails closed for a missing file, empty measured population, empty
baseline replicate set, invalid JSON/protobuf/CSV, unknown artifact kind,
schema mismatch, non-positive timeout, negative count/bytes, out-of-range rate,
or any NaN/infinity. It emits `raw_score: 0`, `placeable: false`, and a typed
`eval_error`; it never substitutes zero, drops the bad value, or returns a
partial placeable result.

## 8. Result document and error classes

The scorer emits one deterministic JSON document with these top-level fields:

```json
{
  "score_schema": 1,
  "raw_score": 0,
  "normalized_score": 0,
  "placeable": false,
  "gates": {},
  "diagnostics": {},
  "eval_error": null
}
```

`eval_error.kind` is `submission` for an evaluated build that panics, cannot
drain, or violates its resource boundary. Patch syntax, allowlist, build, vet,
and contract-test failures are also submission errors but are produced by the
evaluation worker before this scorer. `eval_error.kind` is `infrastructure`
for missing/malformed artifacts, identity/config mismatch, disabled stats,
baseline corruption, host failure, or scorer failure; Apex may retry those
without giving the patch a new noise draw. Ordinary G1-G4 quality failures have
`eval_error: null` and `placeable: false`.

The JSON contains no current time, random id, absolute input path, or map-order
dependency. Identical input artifact bytes and the same scorer version produce
byte-identical output.

## 9. Diagnostic visibility

The scorer always records aggregate and per-replicate raw score, success rate,
request count, received bytes, accounting coverage, sample count, empty-pool
count, minimum first-to-last sample-span fraction, FindProviders2 load p95 and
pool-count p05, resource flags, gate thresholds, normalized score, and takeover
threshold.

During an active hidden-seed round Apex returns only `score_schema`,
`placeable`, normalized score (when placeable), gate booleans, and the typed
error code. Raw/per-replicate values, stderr findings, artifact hashes, sample
details, workload commitment inputs, and exact failing observations become
visible only after the round reveal. Evaluator operators may access them
earlier for incident response under audit logging.
