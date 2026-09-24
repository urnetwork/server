# Sim-latency competition live-deployment playbook

Status date: 2026-09-23

Evaluator/baseline qualification: **historical measured-product qualification
preserved; shared-baseline staging epoch 8 open with four completed jobs and a
running singleton worker; results remain embargoed until finalization**

Launch-control validation: **staging API/evaluator active; best-safe staging
winner policy requires migration 691, API, and worker rollout; production
qualification and external launch actions pending**

After close and FIFO drain, staging selects the highest-ranked placeable
candidate passing every gate, regardless of significance or takeover margin,
or no winner when none is placeable. There is no staging honesty review or
review pause; all staging rows
remain `honesty_review: not_reviewed`. Naming a winner neither approves its
honesty/safety nor promotes source, config, thresholds, or a production winner.
Production retains mandatory review. Historical finalized results stay intact.

Rollout remains pending for this policy: deploy the current migration runner
(`bringyourctl` or `competitiondbinit`) and apply migration 691 before new API readiness or
worker startup, then deploy the main API built with the updated `sn` contract
and rebuild/deploy the competition worker on sille. Refresh the monitor CLI
separately to observe the new migration guard. None of these requires an
evaluator image rebuild, scoring-baseline reset, config-updater rollout, or
frozen-source change. The currently running worker still requires statistical
significance for a staging winner until that control-plane rollout occurs.

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
- [Staging epoch 4 G5 investigation](launch/STAGING-4-G5-INCIDENT.md)
- [Staging epoch 5 release and deployment order](launch/STAGING-5-RELEASE.md)
- [Agent season harness](RUN-MAIN.md)
- [Machine-readable launch status](playbook.yml)

## 1. Go-live position

### Current policy, release, and qualification evidence

| Item | Frozen value / state |
|---|---|
| Public patch-authoring tag | `apex-season-1` at `eb697281cbe0a19a27d7771fe69fb24c2c3dab8c` |
| Evaluator source | Epoch ledger `config/main/sim-latency.yml` is the sole authority for branch, epoch commits, and the significant-improvement percentage |
| Control plane | API and worker follow `main`; their commits are not scoring inputs. Every job persists the exact API and worker runtime image digests. |
| Evaluator image | Epoch-5 source `807b473c927d1ae09a03276bb9758afb715fac9e`; image `sha256:b0c07cf45c30adb483ee5c215b7426b2e098c0b35c7cf4c4abcb0166cb43a87e`, installed; clean-source build, Go validation, and full Docker smoke passed. Repeated live API checks match this tuple on 2026-09-19. [Release record](launch/STAGING-5-RELEASE.md). |
| Host qualification | Prior authenticated containment record `acf226db6b8e50d67f8957cddb3903d5d4e9e82566935d61d270ccb5b03463a3` is retained for staging only; the new image needs separate exact-image production qualification. |
| Simulator / scorer | `e27929e9f2ef45f9f23c2120b651fafe048f38d82f3ca308b1b200b48ea05cbf`; shared-baseline scoring and restored database recovery. New epoch measurements must use the new checkpoint, not historical baseline samples. |
| Workload | 1,800 providers; 200 clients; 80 arrivals/min; quality window 2; 4 exchange hosts; 4 shards |
| Measurement | 180 seconds; impairment on; one immutable epoch control at `R=9`, then `R=9` fresh candidate runs per submission |
| Takeover rule | Epoch 1 starts at `candidate <= same-round baseline * 0.839`; every epoch also requires G1–G6 and one-sided Welch `p <= 0.05`. The source ledger supplies later percentages. |
| Epoch lifecycle | six epochs; exactly seven days of admission; immediate FIFO evaluation; accepted backlog drains past close; worker seals and exits; ranked significant candidates remain embargoed until the honesty-review harness approves the first honest candidate or rejects the list; only then do results reveal and the external loop promote/create the next epoch |
| Winner promotion | Round N evaluates source epoch N-1. The first honesty-approved significant winner's score variance sets source epoch N's threshold. With no significant candidate or after all are rejected, `--no-winner` carries commits and threshold forward unchanged. Promotion is bound to the approved database job, patch digest, and entire score document; the config ledger is always pushed last. |
| Queue / timeout | unbounded accepted submissions per epoch at a fixed $20 USD fee; Redis-list dispatch backed by authoritative PostgreSQL ordering/recovery; one active evaluation; three-hour total execution bound per submission across build, all attempts, retry backoff, scoring, and cleanup; timeout is terminal |
| Patch surface | only `connect/resident_contract_manager.go`; maximum 262,144 bytes. `connect/sim-latency/**` is hard-forbidden independently of policy and its Git tree is authenticated unchanged by the builder. |
| Evaluation leaves | `/home/by/urnetwork/config/local` and `/home/by/urnetwork/vault/local`, direct and read-only |
| Evaluation leaf hashes | config `f2fd41f07258389a5b8cbfd12af69c7e71124755432e48e115933a66f835962d`; vault `f84b7bdd1976c5e404c196584025287ab346f4bcfd60196da9ca46191a39f3fa` |
| Artifact retention | versioned MinIO object storage with compliance retention and post-upload SHA-256 authentication; score commit fails closed |
| Monitoring | main Grafana dashboard plus provisioned page/warn rules in `warp/grafana/alerting/competition.yml` |
| Historical evaluator audit | 10 passed, 0 pending, 0 failed; preserved qualification evidence, not a new-image production audit |
| Latest Go validation | CSV/scorer precision regressions, simulator lifecycle database tests, full sim-latency race suite, `tests.sh`, offline candidate-surface vet, shared-control/historical-ranking migration and controller tests, and competition API/OpenAPI conformance passed on 2026-09-18. Historical dashboard/alert and qualification evidence retains its original dates. |
| Historical hostile cleanup | CPU bomb covered all ten evaluation CPUs; memory bomb exited 137 with `OOMKilled=true`; management cleanup took 655 ms and left zero containers/networks |

Historical qualification covers host controls, the hardened Docker boundary,
trusted commands, the `/etc` host manifest, production-pressure CPU/memory-bomb
cleanup, API staging, FIFO/cache/failover, and reveal. It does not automatically
qualify a changed evaluator image or source graph. The second image-identical
host is not a launch requirement.

### Current staging deployment

Verified on 2026-09-19: API version `2026.9.18+1049819730` advertises the
epoch-5 source/image above. Read-only database checks found migration audit
maximum 683, the shared round-baseline table, and both historical/shared-ranking
guards. No earlier staging jobs were queued or running before creation.

At `2026-09-19T09:57:30Z`, staging epoch 5 is open as round
`01a0b913-c344-27f3-cb64-338dbddc8b07`, opened at
`2026-09-19T09:57:00Z`. Admission closes at `2026-09-21T09:57:00Z`;
reveal waits for admission close and backlog drain; an explicit early staging
close can precede the scheduled reveal. This is a 48-hour staging window,
not the seven-day production cadence. The singleton service
`urnetwork-sim-latency-staging-epoch-5.service` started at
`2026-09-19T09:53:30Z` after independent staging preflight passed. Its
digest-pinned worker comes from server main `07b210bc76f15c714d2de8484153e407bbf65808`;
the worker image and binary identities are in the [release record](launch/STAGING-5-RELEASE.md).
The service remained active with the same PID/invocation and zero restarts;
authenticated staging host refreshes continued through `2026-09-19T09:57:19Z`.
Three further API checks at `2026-09-19T09:59:31Z` agreed on the open round,
schedule, and source/image. No production round has been created. The shared
baseline count is zero while the worker awaits the first submission; successful
shared-control scoring remains unproven. The worker installed at this opening
still enforced null staging winners. The automatic named-winner follow-up above
has not been deployed and does not change the frozen evaluator or baseline.

Staging epoch 4 remains finalized as round
`01a0a58b-9a3e-2e43-f612-0034ff7296ff`, from `2026-09-15T14:56:00Z` through
`2026-09-17T14:56:00Z` (end exclusive), finalized at
`2026-09-17T14:56:00.946063Z`. Its staging-inclusive leaderboard contains four
entries, and Macrocosmos reported that its downstream path worked end to end.
Its immutable policy, scores, and original ranking are unchanged.

Epoch 4's accepted scores do not qualify the new shared-baseline release or
prove production launch readiness. Staging uses authenticated prior
containment qualification; the new image still needs exact-image production
qualification. Redis's remote cluster ports again refused connections from
sille on September 19; the worker used the supported authoritative PostgreSQL
FIFO fallback. This is a degraded dispatch path, not a staging admission
blocker. A public-TLS authenticated Grafana/Prometheus query at
`2026-09-19T09:58:17Z` returned the exact worker image/revision and epoch-5
staging metrics for `host=sille`, `env=main`, `service=sim`: queue/backlog zero
and heartbeat age 25.955 seconds, below the 30-second stale threshold. A
read-only database check independently confirmed a fresh host heartbeat.
The active loopback-only metrics forwarder remains transient. On September 15
at 15:06 UTC, it restored authenticated pushes through crisp
(`172.28.208.58:3100`); the worker logged `push ok` without restarting.
Fireside (`172.28.208.3:3100`) also accepts connections and requires
authentication. The forwarder is a transient systemd service and does not
survive reboot. Persistent host routing, multi-endpoint failover, and complete
monitoring/deployment verification remain pending; the publisher currently
supports only its local endpoint.

On September 16, investigation of a comment-only submission's G5 rejection
proved that epoch 4's frozen source omitted database fix `46515d82`, which is
present on `main` and in the qualified August baseline. A wrapped pgx write
timeout therefore escaped the database recovery path during startup. All 18
replicates completed, but the lifecycle-wide G5 gate correctly rejected the
unexpected recovery. [The incident record](launch/STAGING-4-G5-INCIDENT.md)
contains the source lineage and authenticated evidence hashes. The runtime
repair and build guard passed independent regression, race, and vet checks,
plus three consecutive sim-latency suite runs. The repaired image is now built
from the operator-approved new source checkpoint; its release validation is
recorded [here](launch/STAGING-5-RELEASE.md). Epoch 4's source/image and results
remain unchanged. API/config rollout is complete; the new live scoring proof
and exact-image production qualification remain launch blockers.

An earlier config rollover also exposed a historical seed-reveal bug: decryption used
the current base commit instead of the round's immutable policy base. The
fix is committed at server `45215c8f1cd3c3ef5855734042c513706a179b53` and
passed deterministic unit and PostgreSQL lifecycle tests. The live epoch-4
`providers.yml` reveal succeeded under the September 19 deployment and matched
SHA-256 `3f3829a588e4c024459e2c4c653be8244e2e515c56da645e3c6447b9c1d99fae`.
No historical seed or ciphertext was changed.

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
| Release distribution | **Epoch 4 finalized with four entries; epoch 5 open with matching API/config and singleton worker.** Public info matches the new ten-repository checkpoint and evaluator image. Job responses expose frozen evaluator plus exact API/worker runtime images; main API/worker releases are not scoring inputs. | Verify successful shared-baseline scores and the finalized leaderboard, then complete exact-image production qualification. |
| Artifact retention | Implemented through `server/blob`: every workload and authenticated attempt artifact is uploaded to exact MinIO versions under compliance retention and read back/hash-verified before score commit. `/readyz` now fails unless object lock, versioning, and an enabled server-validated replication destination all pass. `support@ur.xyz` is the owner authorized to delete evidence after `retain_until`. | Run and retain the live protection/capacity preflight. Grafana warns at 75% used and pages at 90%. |
| Monitoring and on-call | Competition metrics, dashboard, MinIO capacity views, 15-second runner heartbeat, 30-second stale warning, service-labeled alert rules, and the `support@ur.xyz` contact-policy reconciler are implemented for main Mimir/Grafana. | Deploy the final server and warp commits and retain the live Grafana routing proof. |
| Submission integration | Main API implements authenticated generate/submit/poll plus public info, reveal, and leaderboard routes from `sn/api/competition.yml`. The Go-only onboarding and atomic token rotation/revocation flows are documented in `launch/ONBOARDING.md`. | Deliver the token through the private channel and exercise live revocation once. No separate API is required. |
| Leaderboard and winner | Public `GET /competition/leaderboard` defaults to finalized production epochs. Staging publishes its best placeable candidate passing every gate, or null when none qualifies, through `include_staging=true`, always with `honesty_review: not_reviewed` and no promotion; the best-safe policy requires migration 691/API/worker rollout. Production rows retain approved/rejected/not-reviewed disposition; ranked significant production candidates require append-only honesty review, and promotion binds the exact approved patch and score. The admission fee is fixed at $20 USD. | Publish rewards, eligibility, legal terms, and abuse/appeal handling. Exercise automatic staging reconciliation, then production reject/advance, approve, exhausted-no-winner, and one dry-run promotion before opening epoch 1. |
| Apex | Adapter mapping and handoff fields are documented in `launch/APEX-HANDOFF.md`. | Macrocosmos must accept the asynchronous external-evaluator contract, stage it, record signed image identities, and activate the private registry entry. |

The installed provisioner authenticates an existing bundle and intentionally
does not rotate it in place. Do not hand-edit one file: the vault,
raw-credential file, permissions, and deployment-manifest hashes are one
season bundle. Either generate/promote a replacement bundle atomically or add
an explicit approval record for the staging-generated bundle. This remains a
human authorization gate, not missing evaluator code.

## 3. Preflight the authoritative production host

Run preflight from the frozen local commits and evidence. Do not rebuild from a
moving `origin/main` during launch.
This section requires a separately qualified production release. The retained
staging checker does not satisfy it; staging epoch 5 follows the
[scheduled-round-first release sequence](launch/STAGING-5-RELEASE.md).

```bash
: "${COMPETITION_QUALIFIED_RELEASE:?Set the independently qualified digest-named release directory}"

sudo systemctl is-active \
  urnetwork-authoritative-host-controls.service \
  urnetwork-authoritative-host-irqs.service \
  docker.service

sudo "$COMPETITION_QUALIFIED_RELEASE/competition-host-self-check" \
  --json | jq -e '
    .logical_cpu_count == 12 and
    .smt_disabled and .governor_pinned and .turbo_pinned and
    .numa_pinned and .irq_pinned and .cgroup_v2 and
    .default_deny_network and .offline_build_cache and
    .resource_bomb_cleanup_verified and
    ([.checks[]] | all)'

sudo "$COMPETITION_QUALIFIED_RELEASE/container/hash-local-mount.sh" \
  /home/by/urnetwork/config/local
sudo "$COMPETITION_QUALIFIED_RELEASE/container/hash-local-mount.sh" \
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

The sealed calibration tree preserves historical measurements and containment
evidence. It does not select the current evaluator image or qualify a changed
source graph. The current source ledger and digest-verified release record
select that image. Do not deploy historical API/worker binaries as the live
control plane.

After the control-plane changes are pushed:

1. build and deploy the main API through the normal main-environment release;
2. build and deploy `cli/competitionworker` from `main`;
3. extract the host simulator from the exact evaluator image, verify its
   source/build identity and SHA-256, and install it in the root-owned,
   read-only digest-named release directory; do not rebuild the measured
   simulator from moving `main` (`make` remains the development/CLI build);
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
         .base_sha == "807b473c927d1ae09a03276bb9758afb715fac9e" and
         .evaluator_image_digest == "sha256:b0c07cf45c30adb483ee5c215b7426b2e098c0b35c7cf4c4abcb0166cb43a87e" and
         .evaluation_policy.provider_count == 1800 and
         .evaluation_policy.replicates == 9 and
         .evaluation_policy.takeover_margin == 0.161'
```

The literal source/image and 0.161 checks above apply to the refreshed source
epoch 0 for staging epoch 5, after API/migrations and config rollout.
For every later round, derive the source and margin from its selected source
epoch in `config/main/sim-latency.yml` and the image from the corresponding
verified release; do not copy epoch 0 values forward.

Do not create the next staging epoch while `/info` still advertises the
superseded image. Epoch 3 completed its first measurement pass, but the old
scorer omitted significance and triggered a retry that exhausted the total
budget. The repaired image and strict contract validation are mandatory;
finalized historical jobs must not be rewritten as scores from the new image.

The rest of sections 5–6 describe **production round generation and
qualification**, not the staging exception. For staging epoch 5, create the
scheduled round after API/config rollout, then preflight/start its worker
before admission; follow [the release sequence](launch/STAGING-5-RELEASE.md).

On production first boot, the worker must heartbeat before round generation. An
authenticated `/readyz` remains 503 until the host has an authenticated
rebaseline matching the current round and its selected source epoch. That is
expected during preparation; do not open submissions until it passes.

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

## 6. Run and promote the mandatory production same-round rebaseline

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
: "${COMPETITION_QUALIFIED_RELEASE:?Set the independently qualified digest-named release directory}"
: "${COMPETITION_SELF_CHECK_SHA:?Set the SHA-256 from that qualification record}"
COMPETITION_SELF_CHECK="$COMPETITION_QUALIFIED_RELEASE/competition-host-self-check"
COMPETITION_RESOURCE_BOMB_REPORT=/home/by/urnetwork/server/connect/sim-latency/\
eval-12c/final-calibration-p1800-cf0fd3a9/host-qualification/\
resource-bomb-cleanup-production.json

sudo "$COMPETITION_QUALIFIED_RELEASE/promote-round-rebaseline.sh" \
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
post-review epoch finalization, legacy `state` reports terminal work as
outcome-neutral `completed`. The additive `evaluation_status` distinguishes
`queued`, `running`, `completed`, `failed`, and `canceled`; a reviewed terminal
code may appear in message-free `evaluation_failure`. Terminal failures are not
retriable. Scores, significance, gates, rankings, and full error messages remain
embargoed. The new signal was verified on the live main API on 2026-09-15.

Operate with these expectations:

- any number of unique canonical patches may be admitted during the seven-day
  window after the Apex adapter collects the fixed $20 USD fee exactly once;
  duplicate patches remain cache hits and transport retries are not recharged;
- accepted jobs become claimable immediately. Redis-list dispatch and the
  authoritative PostgreSQL order feed one FIFO evaluation at a time;
- `closes_at` rejects only new admissions. Every queued/running job continues
  to a terminal result, so the grading interval can extend arbitrarily past
  the seven-day window as paid submissions require;
- the first full job may legitimately remain active for about 2.5 hours because
  it establishes the epoch's nine-run control before its nine candidate runs.
  Later jobs skip the control and are expected to consume roughly half that
  measurement time. Every job is still terminated as failed at the three-hour
  submission-wide execution deadline;
- the first complete 18-replicate staging pass reached scoring in about 2 hours
  33 minutes. Reusing its control makes later nine-replicate jobs approximately
  1 hour 16 minutes at the same observed rate, for a rough theoretical ceiling
  near 130 candidates per uninterrupted week before build, scoring,
  transition, and recovery overhead. The three-hour deadline still gives a
  conservative lower bound of 56 worst-case slots. Admission remains
  unbounded, so excess work extends the private grading interval after close
  rather than being dropped;
- infrastructure failures retry under the same job/cache identity, up to
  three attempts within that same three-hour deadline;
- structural/build/submission errors are terminal and do not get noise redraws;
- the first complete attempt measures nine control repetitions before any
  submitted build; PostgreSQL freezes its exact `baseline.json` bytes and
  SHA-256. Every candidate runs nine repetitions with distinct fresh stores
  against that same control, and later attempts never rerun it;
- every candidate build and run is offline/default-deny;
- accounting, resources, score, completion, and failure artifacts are retained
  and sealed; and
- `placeable` and `takeover_eligible` are different. A winner needs every
  G1–G6 gate, the epoch's raw-score margin, one-sided Welch `p <= 0.05`, a
  supported next-epoch threshold, and therefore `takeover_eligible: true`;
- statistical eligibility creates a ranked review candidate, not a winner. The
  first candidate that receives an append-only `approved` honesty review is the
  winner; `rejected` candidates are discarded and the next rank is presented.
  Ranking is absolute candidate median latency ascending; normalized score is
  display-only;
- every successful result preserves baseline and candidate means and sample
  variances, the epoch-baseline SHA-256, observed and required improvement
  percentages, p-value, and recommended next-epoch margin.

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

The worker performs the complete host self-check again after every completed
job and before the next claim. Any identity or policy drift stops the queue; it
does not redraw the round control. The separate promoted `rebaseline_passed`
host marker remains a production readiness gate and must not be confused with
the append-only score baseline.

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

The command creates one additional temporary root and freshly clones `server`,
`connect`, `sdk`, `proxy`, `glog`, `goidenticons`, `userwireguard`, `sn`,
`operator-proxy`, and `warp`.
It checks out every `sim-latency` branch at the prior epoch commit, applies the
winner only to the evaluated server-tree surface, and verifies all dependency
commits remain unchanged. The long-lived local checkouts are discovery-only
preflight inputs and are never patched. Changed source branches are pushed first;
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
- [x] deploy the epoch-5 API/migrations and matching evaluator configuration,
  explicitly create staging epoch 5, and start its preflighted singleton
  worker in the [recorded release order](launch/STAGING-5-RELEASE.md). Epoch 4's
  immutable policy, four published scores, and historical ranking are preserved;
- [x] verify epoch-5 opening and continued authenticated worker host refreshes;
- [ ] verify successful jobs sharing one authenticated epoch baseline and a
  finalized leaderboard under the new absolute-latency ranking, then complete
  exact-image production qualification and rebaseline. Epoch 4's successful
  downstream proof does not qualify the new shared-control path. Main
  API/worker releases remain on `main`, and every job response exposes its
  frozen evaluator plus exact API/worker runtime image digests;
- [x] verify epoch-4 round reveal under the deployed API, preserving the
  immutable-policy fix `45215c8f1cd3c3ef5855734042c513706a179b53`;
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
