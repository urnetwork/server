# Sim-latency competition live-deployment playbook

Status date: 2026-08-29

Evaluator/baseline qualification: **complete — 10/10 required gates pass**

Launch-control validation: **complete locally; release deployment and external actions pending**

Deployment model: one authoritative 12-physical-core host; 10 evaluation cores,
2 management cores; one content-addressed image per canonical submission patch.

This playbook launches the authenticated UR competition scoring service. It
includes the main-API leaderboard, six weekly epochs, MinIO compliance
retention, and main Grafana signals/alerts. It does not claim Macrocosmos Apex
acceptance, business terms, live credentials, or a deployment record that has
not actually been signed; those remaining actions are listed explicitly below.

Read these first:

- [Preserved calibration evidence](baseline/README.md)
- [Final baseline infographic](baseline/final-baseline.html)
- [Official evaluator contract](OFFICIAL-RUN.md)
- [Competition controller](../../controller/competition_controller.go)
- [Evaluator protocol](evaluator/EVALUATOR-PROTOCOL.md)
- [Competition OpenAPI](../../../sn/api/competition.yml)
- [Apex integration gap](launch/APEX-INTEGRATION-GAP.md)
- [Apex handoff draft](launch/APEX-HANDOFF.md)
- [Apex open-question checklist](launch/APEX-OPEN-QUESTIONS.md)
- [Submitter onboarding](launch/ONBOARDING.md)
- [Incident response](launch/INCIDENT-RESPONSE.md)
- [Agent season harness](RUN-MAIN.md)
- [Machine-readable launch status](playbook.yml)

## 1. Go-live position

### What is already frozen and qualified

| Item | Frozen value / state |
|---|---|
| Public patch-authoring tag | `apex-season-1` at `eb697281cbe0a19a27d7771fe69fb24c2c3dab8c` |
| Evaluator source | Epoch ledger `config/main/sim-latency.yml` is the sole authority for branch, epoch commits, and the significant-improvement percentage |
| Control plane | API and worker follow `main`; their commits are not scoring inputs. Every job persists the exact API and worker runtime image digests. |
| Evaluator image | Local epoch-0 image `sha256:2cc50a579199dc111a9265d5a7e4840aba0b1b794ba82cdd741724c683f90f6b`; rebuild and record a new immutable digest whenever the measured source epoch changes |
| Host qualification | `acf226db6b8e50d67f8957cddb3903d5d4e9e82566935d61d270ccb5b03463a3` |
| Simulator / scorer | Epoch-0 image binary `a345375aa543839b49dff6bd4b663217902a7a924a373a1eb9ffdc8349c83b6b` |
| Workload | 1,800 providers; 200 clients; 80 arrivals/min; quality window 2; 4 exchange hosts; 4 shards |
| Measurement | 180 seconds; impairment on; median of `R=9` |
| Takeover rule | Epoch 1 starts at `candidate <= same-round baseline * 0.839`; every epoch also requires G1–G6 and one-sided Welch `p <= 0.05`. The source ledger supplies later percentages. |
| Epoch lifecycle | six epochs; exactly seven days of admission; immediate FIFO evaluation; accepted backlog drains past close; worker seals and exits; ranked significant candidates remain embargoed until the honesty-review harness approves the first honest candidate or rejects the list; only then do results reveal and the external loop promote/create the next epoch |
| Winner promotion | Round N evaluates source epoch N-1. The first honesty-approved significant winner's score variance sets source epoch N's threshold. With no significant candidate or after all are rejected, `--no-winner` carries commits and threshold forward unchanged. Promotion is bound to the approved database job, patch digest, and entire score document; the config ledger is always pushed last. |
| Queue / timeout | unbounded accepted submissions per epoch at a fixed $20 USD fee; Redis-list dispatch backed by authoritative PostgreSQL ordering/recovery; one active evaluation; three-hour total execution bound per submission across build, all attempts, retry backoff, scoring, and cleanup; timeout is terminal |
| Patch surface | only `connect/resident_contract_manager.go`; maximum 262,144 bytes. `connect/sim-latency/**` is hard-forbidden independently of policy and its Git tree is authenticated unchanged by the builder. |
| Evaluation leaves | `/home/by/urnetwork/config/local` and `/home/by/urnetwork/vault/local`, direct and read-only |
| Evaluation leaf hashes | config `f2fd41f07258389a5b8cbfd12af69c7e71124755432e48e115933a66f835962d`; vault `f84b7bdd1976c5e404c196584025287ab346f4bcfd60196da9ca46191a39f3fa` |
| Artifact retention | versioned MinIO object storage with compliance retention and post-upload SHA-256 authentication; score commit fails closed |
| Monitoring | main Grafana dashboard plus provisioned page/warn rules in `warp/grafana/alerting/competition.yml` |
| Local evaluator audit | 10 passed, 0 pending, 0 failed |
| Final Go validation | sim-latency and competition/API race suites, migration application/order, PostgreSQL/Redis lifecycle integration, vet, OpenAPI conformance, dashboards, and alerts pass on 2026-08-28 |
| Fresh hostile cleanup | CPU bomb covered all ten evaluation CPUs; memory bomb exited 137 with `OOMKilled=true`; management cleanup took 655 ms and left zero containers/networks |

The host controls, hardened Docker boundary, trusted commands, `/etc` host
manifest, production-pressure CPU/memory-bomb cleanup, API/worker release
artifacts, API staging, FIFO/cache/failover, and reveal path have all passed.
The second image-identical host is not a launch requirement.

### Production services supplied by the main environment

The operator has confirmed that the durable PostgreSQL/Redis deployment and
restore proof, API/migration/worker boot ordering, and public
DNS/TLS/reverse-proxy/firewall/rate-limit boundary are provided by the existing
main environment. The main API serves `/competition/*`; no parallel submission
API or separate durable competition database is required. Per-evaluation
PostgreSQL and Redis remain disposable services inside the evaluation Compose
boundary.

The final deployment still records runtime image digests per evaluation,
proves `/competition/readyz`, the MinIO object-lock check, and the Grafana rules
on the live main environment. Main API/worker source commits are deliberately
not scoring inputs. These are release verification steps, not new architectural
components.

### Trust-boundary rule that must not be weakened

There are two distinct resource boundaries:

1. The trusted main API and competition worker use the ordinary main
   config/vault environment. The competition policy, seed-encryption key, and
   bearer-token hashes are trusted service resources and never become
   candidate mounts.
2. Candidate containers receive only the evaluator-safe
   `/home/by/urnetwork/config/local` and `/home/by/urnetwork/vault/local`
   leaves, directly and read-only. Any values inherited historically from
   `all` that the simulator needs must be materialized into these local leaves
   before their frozen manifests are computed.

Do **not** add the API's `competition.yml`, seed key, raw credentials, or any
`config/all`, `config/main`, `vault/all`, `vault/main`, parent config/vault
directory, Docker socket, or host control material to the candidate mounts.
The absence of `competition.yml` in the candidate-readable leaves is correct.

## 2. Pre-launch decisions and configuration

Do not open public submissions until every launch-blocking item in this table
has an owner and a recorded value.

| Area | Current state | Required before public launch |
|---|---|---|
| Season identity and dates | Code freezes six epochs, a 604,800-second admission window, a $20 USD submission fee, immediate grading, post-close backlog drain, post-honesty-review reveal, and a one-shot worker that exits after sealing the drained epoch. The external agentic loop reviews candidates, promotes an approved winner (or carries forward no winner), and explicitly creates the next epoch. | Record the first `opens_at`, final `season_ends_at`, and `retain_until`. The cadence is decided; only calendar values remain. |
| Credentials | One season-wide AES-256 seed-encryption key is valid for all six epochs; every epoch independently draws a fresh 256-bit CSPRNG seed. Staging tokens/key exist root-only. | Rotate as one atomic season bundle or explicitly approve the staging bundle, deliver raw tokens out of band, and record revocation. There is no per-epoch seed-key rotation requirement. |
| Control-plane data services | **Complete by operator confirmation.** PostgreSQL is authoritative for admission, exact FIFO order, leases, results, and finalization. A main-Redis list is the rebuildable FIFO dispatch index; a flush or interrupted push recovers from PostgreSQL. | Run the normal migration verification for the final commit; no new durable data service is needed. |
| Service supervision | **Complete by operator confirmation.** Main API plus one competition worker per epoch use the reviewed main-environment migration and boot ordering. The worker exits zero after close and FIFO drain, leaving significant candidates embargoed for the separate honesty-review command. | Verify the final deployed versions, singleton worker heartbeat, clean one-shot exit handling, and review-harness handoff in the agentic controller. |
| Public ingress | **Complete by operator confirmation.** DNS/TLS/reverse proxy/firewall/rate limits are provided by main. | Smoke the final `/competition/*` routes, including the 262,144-byte request ceiling and ordinary ingress rate limiting. There is no epoch job-count rejection. |
| Release distribution | **Epoch-0 complete by local immutable load.** Docker resolves `sha256:2cc50a579199dc111a9265d5a7e4840aba0b1b794ba82cdd741724c683f90f6b`; the public info response exposes the current evaluator image, and each job response exposes its frozen evaluator plus exact API/worker runtime images. Main API/worker releases continue normally and are not scoring inputs. | Set the same evaluator digest in live `competition.yml`, then verify runtime API/worker digest injection on the deployed services. |
| Artifact retention | Implemented through `server/blob`: every workload and authenticated attempt artifact is uploaded to exact MinIO versions under compliance retention and read back/hash-verified before score commit. `/readyz` now fails unless object lock, versioning, and an enabled server-validated replication destination all pass. `support@ur.xyz` is the owner authorized to delete evidence after `retain_until`. | Run and retain the live protection/capacity preflight. Grafana warns at 75% used and pages at 90%. |
| Monitoring and on-call | Competition metrics, dashboard, MinIO capacity views, 15-second runner heartbeat, 30-second stale warning, service-labeled alert rules, and the `support@ur.xyz` contact-policy reconciler are implemented for main Mimir/Grafana. | Deploy the final server and warp commits and retain the live Grafana routing proof. |
| Submission integration | Main API implements authenticated generate/submit/poll plus public info, reveal, and leaderboard routes from `sn/api/competition.yml`. The Go-only onboarding and atomic token rotation/revocation flows are documented in `launch/ONBOARDING.md`. | Deliver the token through the private channel and exercise live revocation once. No separate API is required. |
| Leaderboard and winner | Implemented at public `GET /competition/leaderboard`; only finalized epochs appear and rows expose approved/rejected/not-reviewed disposition. Ranked significant candidates require append-only operator honesty review, and promotion is database-bound to the exact approved patch and score. The admission fee is fixed at $20 USD. | Publish rewards, eligibility, legal terms, and abuse/appeal handling. Exercise reject/advance, approve, exhausted-no-winner, and one dry-run promotion before opening epoch 1. |
| Apex | Adapter mapping and handoff fields are documented in `launch/APEX-HANDOFF.md`. | Macrocosmos must accept the asynchronous external-evaluator contract, stage it, record signed image identities, and activate the private registry entry. |

The installed provisioner authenticates an existing bundle and intentionally
does not rotate it in place. Do not hand-edit one file: the vault,
raw-credential file, permissions, and deployment-manifest hashes are one
season bundle. Either generate/promote a replacement bundle atomically or add
an explicit approval record for the staging-generated bundle. This remains a
human authorization gate, not missing evaluator code.

## 3. Preflight the authoritative host

Run preflight from the frozen local commits and evidence. Do not rebuild from a
moving `origin/main` during launch.

```bash
sudo systemctl is-active \
  urnetwork-authoritative-host-controls.service \
  urnetwork-authoritative-host-irqs.service \
  docker.service

sudo /usr/local/libexec/urnetwork/competition-2abcf145/competition-host-self-check \
  --json | jq -e '
    .logical_cpu_count == 12 and
    .smt_disabled and .governor_pinned and .turbo_pinned and
    .numa_pinned and .irq_pinned and .cgroup_v2 and
    .default_deny_network and .offline_build_cache and
    .resource_bomb_cleanup_verified and
    ([.checks[]] | all)'

sudo /usr/local/libexec/urnetwork/competition-2abcf145/container/hash-local-mount.sh \
  /home/by/urnetwork/config/local
sudo /usr/local/libexec/urnetwork/competition-2abcf145/container/hash-local-mount.sh \
  /home/by/urnetwork/vault/local

sudo docker ps -aq --filter label=com.urnetwork.competition.job-id
sudo docker network ls -q --filter label=com.urnetwork.competition.job-id
```

The two hash commands must print the exact hashes in section 1. The final two
commands must print nothing. Also verify UTC clock synchronization, free space
for the expected retained jobs, inode headroom, the database backup target,
and that no unrelated workload uses the ten evaluation CPUs.

Authenticate the main deployment manifest and secret-resource permissions
through the ordinary main release process without printing resource contents.
Verify that the candidate mount manifest still contains exactly the two local
leaf roots and explicitly excludes `competition.yml`; the trusted API resource
manifest and candidate resource manifest are intentionally different.

## 4. Deploy the final main-API release

The sealed calibration tree remains authoritative for the evaluator image,
host controls, baseline, and containment evidence. Its older API/worker
binaries predate the six-epoch lifecycle, MinIO archive, leaderboard, and
competition signals; do not deploy those two historical binaries as the live
control plane.

After the control-plane changes are pushed:

1. build and deploy the main API through the normal main-environment release;
2. build and deploy `cli/competitionworker` from `main`;
3. build the host simulator with `cd connect/sim-latency && make`;
4. run `(cd connect/sim-latency && ./tests.sh)` and the Go control-plane gates;
5. verify the deploy system injects `WARP_IMAGE_DIGEST=sha256:...` into both
   processes and that new jobs persist those two exact runtime identities; and
6. retain the OpenAPI bytes and SHA-256 beside that release record.

The evaluator image is immutable within a source epoch. Main API and worker code
may continue moving for correctness and operational improvements: their source
commits are deliberately not frozen scoring inputs. The pull/run boundary uses
the inspected image id, and each evaluation request, database row, event, and
artifact manifest records the exact API and worker runtime image digests. The
score baseline is not recomputed merely because control-plane code changes.

Use one root-owned environment file for API, migration, rebaseline, and worker.
It uses the normal main config/vault roots for the trusted processes. Candidate
containers still receive only the direct local leaves described in section 1:

```text
WARP_CONFIG_HOME=REPLACE_WITH_APPROVED_MAIN_CONFIG_ROOT
WARP_VAULT_HOME=REPLACE_WITH_APPROVED_MAIN_VAULT_ROOT
WARP_ENV=main
WARP_SERVICE=api
WARP_DOMAIN=bringyour.com
WARP_HOST=127.0.0.1
WARP_BLOCK=competition
```

Use the already approved main PostgreSQL, Redis, and MinIO endpoints. Do not put
raw bearer tokens or the seed key in this environment file.

Apply migrations once through the normal main release before API admission and
verify the repository migration count. Origin migrations must remain before the
competition lifecycle migration at the end of the list:

```bash
taskset -c 20,22 competitiondbinit | \
  jq -e '.schema == 1 and
         .database_version == .migration_count and
         .migration_count > 0'
```

This executes the repository migration order, in which origin migrations
precede local migrations. Do not maintain a second hand-written migration
list, and do not attempt a schema downgrade during rollback.

Service ordering must be:

1. authoritative CPU controls and IRQ controls;
2. hardened Docker daemon and host firewall;
3. durable main PostgreSQL/Redis and MinIO;
4. successful `competitiondbinit` one-shot;
5. API on the two management CPUs;
6. one worker with a stable identity on the same two management CPUs.

The API command is the normal digest-recorded main `api` service. The worker
command is the matching release binary:

```bash
taskset -c 20,22 competitionworker \
  --worker_id=sille-season-1
```

The service manager must send `SIGTERM`, allow graceful cleanup, restart on
infrastructure failure with backoff, preserve a stable worker id, and never
run a second active worker as a way to gain parallel evaluations. PostgreSQL's
singleton lease remains the final one-job guard.

## 5. Bring up the API and create a round

Set the private service URL for operator checks. Do not paste a raw token into
shell history or logs; inject it from the approved secret manager into a
restricted operator shell.

```bash
COMPETITION_API_BASE=http://127.0.0.1:18080/competition

curl -fsS "$COMPETITION_API_BASE/healthz" | \
  jq -e '.status == "alive"'
curl -fsS "$COMPETITION_API_BASE/info" | \
  jq -e '.enabled == true and
         .base_sha == "859be81191fafcc576b617ebec716fa49401643a" and
         .evaluation_policy.provider_count == 1800 and
         .evaluation_policy.replicates == 9 and
         .evaluation_policy.takeover_margin == 0.161'
```

The literal base and 0.161 checks above apply to the first competition round.
For every later round, derive both expected values from its selected source
epoch in `config/main/sim-latency.yml`; do not copy epoch 0 values forward.

On first boot, the worker must heartbeat before round generation. An
authenticated `/readyz` may still return 503 because the old staging round is
the last promoted rebaseline. That is expected; do not open submissions.

Prepare the first strict JSON request with `closes_at = opens_at + 7 days` and
`reveal_at = closes_at`. Create it far enough before opening to complete the
same-round R=9 rebaseline; the configured 16-hour preparation window covers
the three-hour submission execution bound. For epochs 2 through 6, the
agentic control loop waits for the worker to exit after the prior FIFO drains
past close, runs ordered honesty review until an honest significant candidate
is approved or the list is exhausted, promotes that approved winner or records
the no-winner carry-forward, and then submits the next strict round request.

```json
{
  "opens_at": "REPLACE_WITH_UTC_TIME",
  "closes_at": "REPLACE_WITH_OPENS_AT_PLUS_EXACTLY_7_DAYS",
  "reveal_at": "REPLACE_WITH_THE_SAME_VALUE_AS_CLOSES_AT"
}
```

```bash
curl -fsS \
  -H "Authorization: Bearer $COMPETITION_OPERATOR_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @round-request.json \
  "$COMPETITION_API_BASE/generate-round" | tee round-created.json

jq -e '
  (.round_id | test("^[0-9a-f-]{36}$")) and
  (.epoch == 1) and
  (.workload_commitment | test("^[0-9a-f]{64}$")) and
  (.providers_sha256 | test("^[0-9a-f]{64}$")) and
  (.revealed_seed == null)' round-created.json

COMPETITION_ROUND_ID="$(jq -er '.round_id' round-created.json)"
```

Store `round-created.json` in the immutable operator record. A round cannot be
edited or overlapped after creation.

## 6. Run and promote the mandatory same-round rebaseline

Stop the ordinary worker before rebaseline and confirm there is no running job.
Use the exact no-op patch:

```text
/home/by/urnetwork/server/connect/sim-latency/evaluator/references/noop.patch
SHA-256 8bd57a48ac82a6e846b607a9301c48145da5c66717c9e3a341138d034d1e0775
```

Hold `/run/urnetwork/competition-operational.lock`, run
`competitionrebaseline` as the worker service user on CPUs `20,22`, and write
to a new root-owned output directory:

```bash
taskset -c 20,22 competitionrebaseline \
  --round_id "$COMPETITION_ROUND_ID" \
  --patch /home/by/urnetwork/server/connect/sim-latency/evaluator/references/noop.patch \
  --patch_sha256 8bd57a48ac82a6e846b607a9301c48145da5c66717c9e3a341138d034d1e0775 \
  --output "/var/lib/urnetwork/competition/rebaseline/$COMPETITION_ROUND_ID/result.json"
```

Require `candidate_placeable: true`, then run the installed root-owned
`promote-round-rebaseline.sh` with the result, `/etc/urnetwork/competition-host.json`,
the sealed production resource-bomb report, installed self-check and its hash,
and a new promotion output directory. This is the only supported way to update
the rebaseline marker and host manifest. Preserve both result and promotion
evidence read-only.

The promotion invocation is:

```bash
COMPETITION_SELF_CHECK=/usr/local/libexec/urnetwork/competition-2abcf145/\
competition-host-self-check
COMPETITION_SELF_CHECK_SHA=d3c904313ebdd24edfaa6615e2b54e7c95367162661b4e873505a99fa016c8f7
COMPETITION_RESOURCE_BOMB_REPORT=/home/by/urnetwork/server/connect/sim-latency/\
eval-12c/final-calibration-p1800-cf0fd3a9/host-qualification/\
resource-bomb-cleanup-production.json

sudo /usr/local/libexec/urnetwork/competition-2abcf145/\
promote-round-rebaseline.sh \
  --result "/var/lib/urnetwork/competition/rebaseline/$COMPETITION_ROUND_ID/result.json" \
  --host-config /etc/urnetwork/competition-host.json \
  --resource-bomb-report "$COMPETITION_RESOURCE_BOMB_REPORT" \
  --self-check "$COMPETITION_SELF_CHECK" \
  --self-check-sha256 "$COMPETITION_SELF_CHECK_SHA" \
  --output-directory "/var/lib/urnetwork/competition/rebaseline-promotions/$COMPETITION_ROUND_ID"
```

Create the result parent for the worker service account with mode `0700` and
the promotion parent as root-owned mode `0700` before running these commands.

Restart the ordinary worker and require authenticated readiness:

```bash
curl -fsS \
  -H "Authorization: Bearer $COMPETITION_OPERATOR_TOKEN" \
  "$COMPETITION_API_BASE/readyz" | \
  jq -e '.ready == true and ([.checks[]] | all)'
```

Do not route public submissions until this succeeds for the newly created
round. Re-run rebaseline after any evaluator image, frozen local-leaf hash,
host qualification, scorer, workload, or round identity change.

## 7. Open submissions and operate the queue

The submission integration sends canonical text patches, never repositories,
URLs, miner Dockerfiles, or miner-built images:

```json
{
  "round_id": "ROUND_UUID",
  "patch": "diff --git ..."
}
```

`POST /competition/score` returns HTTP 202 with a job id and status URL.
`GET /competition/score/{jobId}` polls it. One canonical patch per round maps
to one cache identity even when multiple principals submit it. Before
post-review epoch finalization, submitter responses expose only processing state: terminal jobs
appear as outcome-neutral `completed`, with score and failure results omitted.

Operate with these expectations:

- any number of unique canonical patches may be admitted during the seven-day
  window after the Apex adapter collects the fixed $20 USD fee exactly once;
  duplicate patches remain cache hits and transport retries are not recharged;
- accepted jobs become claimable immediately. Redis-list dispatch and the
  authoritative PostgreSQL order feed one FIFO evaluation at a time;
- `closes_at` rejects only new admissions. Every queued/running job continues
  to a terminal result, so the grading interval can extend arbitrarily past
  the seven-day window as paid submissions require;
- one job may legitimately remain active for about 2.5 hours and is terminated
  as failed at the three-hour submission-wide execution deadline;
- infrastructure failures retry under the same job/cache identity, up to
  three attempts within that same three-hour deadline;
- structural/build/submission errors are terminal and do not get noise redraws;
- baseline and candidate each run nine repetitions with distinct fresh stores;
- every candidate build and run is offline/default-deny;
- accounting, resources, score, completion, and failure artifacts are retained
  and sealed; and
- `placeable` and `takeover_eligible` are different. A winner needs every
  G1–G6 gate, the epoch's raw-score margin, one-sided Welch `p <= 0.05`, a
  supported next-epoch threshold, and therefore `takeover_eligible: true`;
- statistical eligibility creates a ranked review candidate, not a winner. The
  first candidate that receives an append-only `approved` honesty review is the
  winner; `rejected` candidates are discarded and the next rank is presented;
- every successful result preserves baseline and candidate means and sample
  variances, the observed and required improvement percentages, p-value, and
  recommended next-epoch margin.

When the final accepted job becomes terminal after admission closes, the worker
seals the ranked significant-candidate set and exits successfully. It does not
publish a winner. The operator-controlled agent harness then reviews the exact
patch and score for each candidate in order. Approval atomically freezes the
winner and finalized timestamp; rejecting the final candidate atomically
freezes no winner. Only that post-review commit makes scores, failures, seed,
workload, and `GET /competition/leaderboard` public. The external control loop
then promotes the approved winner—or records a no-winner carry-forward—and creates the next
hidden round with its 16-hour rebaseline preparation window and exact seven-day
submission window. The operator must promote the next round's rebaseline before
its `opens_at`; readiness remains false otherwise.

Monitor at least:

- unauthenticated `/competition/healthz` and authenticated `/competition/readyz`;
- the runner-process heartbeat emitted every 15 seconds, warning when its age
  exceeds 30 seconds;
- authoritative host-heartbeat age and identity;
- one-hot round phase including `review`; stale-worker paging covers only
  `open|grading`, because the one-shot worker intentionally exits for review;
- durable FIFO size, current job identity and elapsed time, recent p75
  evaluation duration, estimated drain time, and whether the current epoch has
  produced a statistically significant submission;
- the internal live replicate plots for TTFB p50/p95 and throughput p50/p95.
  They update after each authenticated replicate: blue is a provisional
  significant improvement, red is a significant regression, gray is not
  significant, and green is the same-round baseline. These diagnostics never
  bypass the finalization-time public reveal or the sealed composite score;
- queued/running job age, attempt count, lease owner, and lease expiry;
- API/worker exits, evaluator typed errors, OOM/timeout events, and cleanup;
- PostgreSQL and Redis health/latency/backups;
- `/var/lib/urnetwork/competition` bytes, inodes, immutable modes, and retention;
- Docker objects with `com.urnetwork.competition.job-id` labels; and
- drift in host, command, image, local-leaf, workload, and scorer hashes.

Install `server/grafana/dashboards/competition.json` through the normal Grafana
dashboard sync and `warp/grafana/alerting/competition.yml` through Grafana file
provisioning. The provisioned thresholds are: archive readiness missing/below
1 for one minute (page), runner-process heartbeat age over 30 seconds (dashboard
warning), durable worker heartbeat absent or older than 60 seconds for five
minutes while open/grading (page), queued-without-running grading for five
minutes (page), any five-minute control-plane/archive/infrastructure error
(page), MinIO over 75% used for 15 minutes (warn), and MinIO over 90% used for
five minutes (page).

The live plot source is the evaluator-owned `evaluation-progress.json`, which
is atomically replaced after every completed replicate, retained with the
attempt in MinIO, and watched only by the competition worker. The ordinary API
does not expose this document or its metric series before finalization.

## 8. Close, drain, reveal, and retain

At `closes_at`, atomically reject new jobs and keep the immediate FIFO running
until every accepted job is terminal. Do not expose the seed, workload, scores,
failures, or leaderboard while that backlog drains or while honesty review is
pending. After post-review epoch finalization,
verify `/competition/info` exposes the seed and provider URL, then download the
workload and authenticate both response headers:

```bash
curl -fsS -D providers.headers \
  "$COMPETITION_API_BASE/round/$COMPETITION_ROUND_ID/providers.yml" \
  -o providers.yml
sha256sum providers.yml
```

The digest must equal the value committed at round generation and the
`X-Content-SHA256`/`ETag` headers. Retain the round request, commitment, seed,
providers file, API/worker release identities, job/event records, all attempts,
scores, and public leaderboard export through `retain_until`.

The epoch is not published merely because it closed or reached `reveal_at`.
Publication waits until every accepted job is terminal and the honesty-review
decision commits. The worker exits after sealing/draining; only an approved
candidate (or exhausted no-winner state) permits the external control loop to
promote and create the next epoch.

### Review candidates, then promote the finalized winner

Round N evaluates source epoch N-1. Once its backlog drains and the worker exits,
enumerate the current highest-ranked significant candidate. `epoch-review`
queries the main control plane, authenticates the immutable PostgreSQL patch
copy, and materializes `candidate.json`, `score.json`, and `canonical.patch` in
a fresh mode-0700 directory whose files are mode 0400:

```bash
cd /home/by/urnetwork/server/connect/sim-latency
review_json="$(./run-local-main.sh epoch-review --epoch "REPLACE_WITH_N" next)"
winner_tmp="$(jq -er '.candidate_directory' <<<"$review_json")"
candidate_job_id="$(jq -er '.state.candidate.job_id' <<<"$review_json")"

# Run the trusted agent-harness honesty analysis without applying the patch.
# It must write a bounded JSON object to honesty-report.json.
HONESTY_HARNESS_COMMAND \
  --candidate "$winner_tmp/candidate.json" \
  --score "$winner_tmp/score.json" \
  --patch "$winner_tmp/canonical.patch" \
  --out honesty-report.json
```

For a dishonest candidate, append a rejection. The response advances to the
next ranked candidate without creating another temporary directory. Invoke
`epoch-review next` to materialize that candidate, then repeat until a candidate
is approved or the state becomes `finalized` with no winner:

```bash
./run-local-main.sh epoch-review --epoch "REPLACE_WITH_N" reject \
  --job-id "$candidate_job_id" \
  --reviewer "REPLACE_WITH_STABLE_HARNESS_ID" \
  --reason "REPLACE_WITH_CONCISE_TAMPERING_FINDING" \
  --evidence honesty-report.json
rm -rf -- "$winner_tmp"
```

For an honest candidate, append approval. This is atomic with finalization and
is the only database path that can publish a winner:

```bash
./run-local-main.sh epoch-review --epoch "REPLACE_WITH_N" approve \
  --job-id "$candidate_job_id" \
  --reviewer "REPLACE_WITH_STABLE_HARNESS_ID" \
  --reason "honesty checks passed" \
  --evidence honesty-report.json
```

The review table and evidence are append-only. Database triggers reject rank
skips, decisions before close/drain, a no-winner finalization with an unresolved
significant candidate, and a winner without an approval for that exact job.
Keep the approved directory just long enough to promote it, then remove it;
MinIO and PostgreSQL remain durable.

When a winner exists, promote from the exact approved directory:

```bash

./run-local-main.sh source-check --epoch "REPLACE_WITH_N_MINUS_1"
./run-local-main.sh promote \
  --epoch "REPLACE_WITH_N" \
  --winner "$winner_tmp" \
  --winner-job-id "$candidate_job_id"
rm -rf -- "$winner_tmp"
```

If review finalized no winner, do not fabricate a patch or job id:

```bash
./run-local-main.sh promote --epoch "REPLACE_WITH_N" --no-winner
```

The winner directory must contain the evaluated `canonical.patch` and exact
reviewed `score.json`. Promotion queries the finalized round, requires the exact
approved job, rejects unevaluated repository patches, verifies the canonical
patch SHA-256 and the entire score document against the approved database
record, then rechecks G1–G6, placeability, margin, R=9 variance, one-sided
p-value, and supported next-epoch recommendation. The recommendation becomes
the new ledger percentage. A no-winner transition repeats both prior commits
and prior percentage unchanged, and is rejected unless the round finalized
without a winner.

The command creates one additional temporary root, freshly clones `connect`,
`sdk`, `server`, and `proxy`, checks out each `sim-latency` branch at the prior
epoch commit, applies the winner, and creates at most one commit per changed
repository. The long-lived local checkouts are verified preflight inputs and
are never patched. Repository branches are pushed first;
`config/main/sim-latency.yml` is cloned, committed, and pushed last, so an
interrupted cross-repository update never activates a partial source epoch.
`--dry-run` performs every staging and validation step without a push or local
fast-forward. The `winner_tmp` directory is disposable because MinIO remains
the durable submission and evaluation archive.

For rounds 1 through 5, build the next immutable evaluator image during the
16-hour preparation window and deploy its base SHA, image digest, and
simulator/scorer digests through the trusted main competition configuration:

```bash
cd /release-workspace/server
./connect/sim-latency/evaluator/container/build-base.sh \
  --epoch "REPLACE_WITH_N" \
  --source-config /home/by/urnetwork/config/main/sim-latency.yml \
  --tag urnetwork/sim-latency-evaluator-base:epoch-N
```

The build embeds only the sanitized epoch ledger at
`/opt/urnetwork/sim-latency.yml`; it does not mount or copy `config/main`,
`config/all`, `vault/main`, or `vault/all`. Docker runs `source-check` inside
the image before publishing it. Update the main evaluator policy, complete the
new round's same-round rebaseline, and require `/readyz` before `opens_at`.
Round 6 is promoted the same way to leave the final winning product at source
epoch 6, but it has no following competition round to rebaseline.

After the season:

1. close ingress and drain the queue;
2. reveal every eligible round and publish reproducibility material;
3. revoke submitter/operator tokens;
4. snapshot and verify PostgreSQL plus artifact storage;
5. archive provenance, SBOMs, OpenAPI, config manifest, host qualification,
   score results, and incident log; and
6. delete only under an approved retention ticket after `retain_until`.

## 9. Incident stop and recovery

For a non-hostile fault, stop new admissions at ingress, leave the worker up to
finish the active job, and preserve all evidence. For an active CPU/memory bomb
or stuck evaluator:

1. keep the two management CPUs and management-memory reserve untouched;
2. terminate the worker/evaluator through the service manager;
3. allow the evaluator's bounded TERM/KILL and label-resolved cleanup path;
4. inspect only exact competition labels—never use a broad Docker or cgroup
   deletion command;
5. retain container inspect, cgroup counters, stderr, partial artifacts, and
   the typed failed attempt;
6. confirm no labeled container/network remains; and
7. rerun host self-check and same-round rebaseline before reopening.

If the API or worker dies, the PostgreSQL lease permits recovery under the same
job and cache identity. Do not create a new job or patch identity to bypass an
expired lease. If the database is restored, restore the matching artifact
snapshot; mismatched database/artifact generations must fail closed.

Rollback means stopping API/worker ingress and restoring the last compatible
database plus release as a pair. There is no supported migration downgrade and
no permission to fall back to a moving source checkout or tag-only image.

## 10. Final launch checklist

Technical evidence already complete:

- [x] one-host hardware and containment qualification;
- [x] frozen p1800 scale, R=9 aggregation, epoch-0 16.1% margin, and
  per-evaluation variance/significance records that set later epoch margins;
- [x] same-seed baseline and independent reference screen;
- [x] adversarial CPU/memory-bomb cleanup;
- [x] isolated direct read-only local leaves and no parent/all/main mounts;
- [x] fixed per-submission Docker build and offline execution;
- [x] authenticated main-API generate/submit/poll/info/reveal/leaderboard
  routes, FIFO/cache/failover, and structural OpenAPI conformance;
- [x] six exact weekly admission epochs, unbounded paid submission count,
  immediate Redis-list FIFO dispatch with durable PostgreSQL recovery,
  post-close drain, honesty-review-gated result publication, and one-shot worker
  exit before the external review/promotion loop creates the next epoch;
- [x] append-only ordered honesty review with reject/advance, exact candidate
  approval, exhausted no-winner finalization, and promotion bound to the
  approved patch and significance record;
- [x] per-epoch measured-source ledger, mandatory run preflight, authenticated
  winner-score threshold update or no-winner carry-forward, isolated
  commit/push command, and epoch-bound evaluator image build;
- [x] MinIO versioned compliance retention, enabled-replication validation,
  capacity accounting, post-upload authentication, and fail-closed readiness;
- [x] main Grafana metrics/dashboard plus provisioned runner/worker, archive,
  evaluation, queue-stall, and MinIO capacity alerts, all routed by the
  `service=sim-latency` label;
- [x] operator-confirmed main PostgreSQL/Redis/restore boundary, service/migration
  ordering, and public ingress controls; and
- [x] evaluator release provenance/SBOMs, OpenAPI, baseline, and final reports.

Still to add or approve before a public competition starts:

- [ ] final season id, first `opens_at`, season end, and retention date (the
  six-epoch weekly cadence and post-review finalization reveal are already frozen);
- [ ] atomic live credential/seed-key rotation or explicit approval to promote
  the staging-generated bundle;
- [x] epoch-0 evaluator image locally loaded as immutable Docker image id
  `sha256:2cc50a579199dc111a9265d5a7e4840aba0b1b794ba82cdd741724c683f90f6b`;
  main API/worker releases remain on `main`, and every job API response persists
  and exposes its frozen evaluator plus exact API/worker runtime image digests;
- [ ] live MinIO `/readyz` proof, backup-replication record, and capacity check;
  `support@ur.xyz` is recorded as the owner authorized to delete evidence after
  `retain_until`;
- [ ] deploy the final server/warp monitoring commits and bind `severity` labels
  to the existing main Grafana contact policy routed to the recorded on-call
  and incident contact, `support@ur.xyz`;
- [ ] deliver the implemented submitter onboarding/token bundle and retain one
  live rotation/revocation proof;
- [ ] publish rewards, eligibility, legal terms, and abuse/appeal process (the
  submission fee is frozen at $20 USD);
- [ ] Macrocosmos asynchronous-adapter/staging/private-registry acceptance and
  signed public Apex handoff artifacts.

Once those boxes are checked, run sections 3–6 in order, require `/readyz` to
return every check true for the new round, and only then enable public ingress.
