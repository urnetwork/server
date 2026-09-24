# Sim-latency submitter onboarding draft

Status: technically complete draft; commercial and Apex publication fields
called out below still require partner approval.

## Competition shape

The competition runs through the main UR API at `https://api.bringyour.com`.
There are six sequential epochs. Each epoch accepts submissions for exactly
seven days over the half-open interval `[opens_at, closes_at)`. Evaluation
starts immediately, uses a single FIFO, and continues after close until every
accepted submission is terminal. Each evaluation has a three-hour end-to-end
limit. Results, hidden workload data, and leaderboard rows remain private until
the FIFO drains and mandatory honesty review selects a winner or establishes
that there is no winner.

Apex collects the fixed USD $20 fee once for each unique production
submission. There is no per-epoch submission-count cap. A duplicate canonical
patch reuses the existing immutable job/cache identity and must not collect a
second fee.

A continuously available evaluator host has 56 conservative full three-hour
slots per seven-day admission window. The first job also establishes the
nine-run epoch control. At the observed staging rate, later candidate-only jobs
are projected around 1 hour 16 minutes, for roughly 130 theoretical slots
before build, scoring, transition, and recovery overhead. Admission is
intentionally unbounded: the FIFO continues in a private grading period after
close until every accepted job is terminal.

Before production epoch 1, `GET /competition/info` may expose the current
`staging_round`. The staging era begins at epoch zero and can advance through
multiple sequential epochs. A response carrying `staging: true` is fee-free
and follows the production admission, cache, FIFO, isolated evaluation,
scoring, embargo, and authenticated polling paths against frozen source epoch
zero. After admission closes and every accepted job is terminal, it finalizes
automatically and makes each job's score or typed failure visible through its
status URL. The highest-ranked placeable candidate passing every gate is the
named staging winner, without a statistical-significance or takeover-margin
requirement; if none qualifies, there is no winner. Staging never pauses for
honesty review.
`GET /competition/leaderboard?include_staging=true` also publishes a clearly
marked staging epoch with the same `winner_job_id` and winning entry for
adapter-path testing. Every staging entry has `honesty_review: not_reviewed`,
including a winner: this is not an honesty or safety approval. It does not
create a production leaderboard row, candidate review, or promotion, and does
not change source epoch zero or its threshold. Historical finalized results
are preserved. The best-safe named-winner policy requires migration 691,
API, and worker rollout before epoch 8 closes. See the dated
[epoch-5 release record](STAGING-5-RELEASE.md).

UR creates or retrieves each identity with `run-main.sh staging`, then uses
`run-main.sh advance-staging` to stop admission, drain all accepted work,
publish the completed epoch, and create its successor; the operator token stays
inside UR. Apex receives the public `round_id` and uses its normal submitter
token for `POST /competition/score` and subsequent status polling. Apex must
prefer an open `active_round` once production exists. Creating production
epoch 1 ends the staging era and cancels queued staging work; it refuses to
race a running evaluation.

## Authentication and API flow

Apex receives a submitter bearer token through the approved out-of-band secret
channel. Never put a token in a URL, patch, log, issue, or public artifact.
The adapter sends it as `Authorization: Bearer TOKEN`.

1. Read `GET /competition/info` and retain the competition, evaluator image,
   patch policy, scoring policy, epoch window, workload commitment, and the
   selected round's `staging` identity.
2. Submit the canonical unified diff to `POST /competition/score` with the
   active `round_id`. Only the documented allowed Go paths are accepted;
   `connect/sim-latency/**` and all trusted evaluator/scoring inputs are always
   forbidden.
3. Retain the returned immutable `job_id`, `patch_sha256`, and `status_url`.
4. Poll the exact status URL. Preserve identity across HTTP 429 and retriable
   5xx responses; use bounded exponential backoff and never resubmit under a
   new identity to bypass FIFO order or the fee boundary.
5. Read `evaluation_status` as the evaluator lifecycle: `queued`, `running`,
   `completed`, `failed`, or `canceled`. `completed` says only that evaluation
   produced a private score; it reveals no value, placeability, gate,
   significance, or diagnostic. `failed` is terminal. Its optional
   `evaluation_failure` contains only a reviewed `kind`, `code`, and
   `retriable:false`; an unknown code produces no summary. It never contains
   the raw message or readiness detail. Keep using the legacy `state` field for
   compatibility: before publication both successful and failed evaluation
   retain `state: completed`.
6. During a rolling API update, a response may lack `evaluation_status`.
   Derive `queued`, `running`, and `canceled` from the same legacy state, but
   treat legacy `completed` as outcome-neutral and continue the normal epoch
   reconciliation path. An actual HTTP 429 or typed retriable 5xx response is
   still a transport/service retry; a polled `evaluation_status: failed` is
   not.
7. Before finalization, non-operator responses continue to omit `score` and
   full `eval_error`. A finalized staging epoch publishes each outcome at its
   status URL and in the opt-in staging leaderboard. Published historical
   terminal errors are returned with `retriable:false` without rewriting their
   retained evidence. After production review finalizes an epoch, read its
   default public leaderboard, reveal, and authenticated workload.

The authoritative schema is
[`sn/api/competition.yml`](../../../../sn/api/competition.yml). The Go Apex
adapter and conformance suite cover immutable job identity, FIFO polling,
429/5xx retry, embargo, reveal, and leaderboard reconciliation.

## Canonical patch bytes

The `patch` JSON string is the exact submission and cache identity. The API
validates it but never repairs or rewrites it. A canonical patch must:

- be nonempty UTF-8 text with no NUL byte;
- use LF (`0x0a`) line endings and contain no CR (`0x0d`) byte;
- end in exactly one LF: its last byte is `0x0a`, and it does not end in
  `0x0a 0x0a`;
- contain no control character other than LF and tab;
- use strict unified Git sections beginning `diff --git a/PATH b/PATH`, with
  matching `--- a/PATH`, `+++ b/PATH`, and valid `@@` hunk counts;
- sort multiple file sections lexicographically by path; and
- modify only existing regular 100644 files on the published allowlist, with
  no binary, create, delete, rename, copy, mode, submodule, or build-tag
  operation.

For this season the only allowed path is
`connect/resident_contract_manager.go`. Generate and JSON-encode the patch
without passing its contents through a shell variable:

```sh
git diff --no-ext-diff --src-prefix=a/ --dst-prefix=b/ -- \
  connect/resident_contract_manager.go > submission.patch

test -s submission.patch
test "$(tail -c 1 submission.patch | od -An -tu1 | tr -d ' ')" = 10
test "$(tail -c 2 submission.patch | od -An -tu1 | tr -s ' ' | sed 's/^ //')" != "10 10"
! LC_ALL=C grep -q $'\r' submission.patch

jq -n --arg round_id "$ROUND_ID" --rawfile patch submission.patch \
  '{round_id:$round_id,patch:$patch}' > submission.json
curl --fail-with-body --header "Authorization: Bearer $SUBMITTER_TOKEN" \
  --header 'Content-Type: application/json' --data-binary @submission.json \
  https://api.bringyour.com/competition/score
```

Do not use `patch=$(git diff ...)`, `printf %s`, or a JSON helper that trims
text: shell command substitution removes trailing newlines. A
`noncanonical_patch` response is terminal for those bytes; correct the byte
format and submit the intended canonical patch.

## What is scored

Each candidate is applied to the source commits frozen for that epoch and gets
a new content-addressed evaluation image. Before the first submitted build, the
evaluator freezes one authenticated sample of nine independently reset control
runs for the epoch. Every candidate contributes nine independently reset runs
and is compared with those exact same control bytes. Lower absolute p95
end-to-end latency is better and determines ranking; normalized score is
display-only. Placeability requires every correctness, traffic-volume,
path-integrity, matchmaking, stability, and resource gate to pass. Takeover
also requires the epoch margin and a one-sided Welch result at `p <= 0.05`.

In production, statistical eligibility enters the private honesty-review
ranking; it does not guarantee a win. A dishonest or unsafe candidate is rejected and review moves
to the next ranked significant candidate. If none remains, the epoch has no
winner and the incumbent code and threshold carry forward unchanged.

## Credential delivery, rotation, and revocation

Operators generate or rotate bundles atomically with the Go-only
`sim-latency credentials` commands. Raw tokens appear only in a newly created
mode-0600 delivery file; the live vault stores SHA-256 digests. Rotation adds a
new credential before the prior credential is revoked, permitting an explicit
overlap window. Operators exercise authentication with the new token, update
Apex, then revoke the old named credential and prove it receives HTTP 401.
Revocation refuses to remove the last token for either required role.

Report a suspected credential leak immediately to `support@ur.xyz`. Stop using
the token; do not paste it into the report.

## Fields still requiring Apex/business publication

- Apex competition and private-registry identifiers;
- epoch-1 `opens_at`, the resulting season end, and public announcement time;
- rewards and payout schedule;
- eligibility, geography, tax, legal terms, and privacy notice;
- fee settlement, failed-submission, refund, and chargeback policy;
- abuse, disqualification, appeal, and support-response policy; and
- the signed Macrocosmos adapter/staging/registry acceptance record.

Until those fields are approved and launch preflight is green, this document
is an integration draft and must not be represented as public terms.
