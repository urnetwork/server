# Sim-latency competition live-deployment playbook

Status date: 2026-08-28

Evaluator/baseline qualification: **complete — 10/10 required gates pass**

Launch-control implementation: **implemented; final commit/release validation in progress**

Deployment model: one authoritative 12-physical-core host; 10 evaluation cores,
2 management cores; one content-addressed image per canonical submission patch.

This playbook launches the authenticated UR competition scoring service. It
includes the main-API leaderboard, six weekly batch epochs, MinIO compliance
retention, and main Grafana signals/alerts. It does not claim Macrocosmos Apex
acceptance, business terms, live credentials, or a deployment record that has
not actually been signed; those remaining actions are listed explicitly below.

Read these first:

- [Apex production calibration](APEX-CALIBRATION.md)
- [Final baseline infographic](final-baseline.html)
- [Finalization contract](FINALIZE.md)
- [Competition service README](../../competition/README.md)
- [Evaluator protocol](../../competition/EVALUATOR-PROTOCOL.md)
- [Competition OpenAPI](../../../sn/api/competition.yml)
- [Apex integration gap](../../competition/APEX-INTEGRATION-GAP.md)
- [Apex handoff draft](../../competition/APEX-HANDOFF.md)
- [Machine-readable launch status](playbook.yml)

## 1. Go-live position

### What is already frozen and qualified

| Item | Frozen value / state |
|---|---|
| Public patch-authoring tag | `apex-season-1` at `eb697281cbe0a19a27d7771fe69fb24c2c3dab8c` |
| Evaluator source | `46515d82fe98ff666c61b2b5bb1d34a89cf4dad8` |
| Control-plane source | Record the final pushed server commit and main-API/worker image digests in `playbook.yml`; the older staging release is evidence, not the launch control plane |
| Evaluator image | `sha256:2abcf145c0f914899debbd2fd52e57a16cf20072165c8d13f04a0ba487198a4c` |
| Host qualification | `acf226db6b8e50d67f8957cddb3903d5d4e9e82566935d61d270ccb5b03463a3` |
| Simulator / scorer | `bc843ce2b9cdcc41459362c7a682b08e7a12a8ac896443fe1e8aad94d4b17997` |
| Workload | 1,800 providers; 200 clients; 80 arrivals/min; quality window 2; 4 exchange hosts; 4 shards |
| Measurement | 180 seconds; impairment on; median of `R=9` |
| Takeover rule | candidate raw score `<= same-round baseline * 0.839`, plus G1–G6 |
| Epoch lifecycle | six epochs; exactly seven days of submission; batch grading only after close; deterministic winner and next-epoch creation |
| Queue / timeout | ten distinct canonical patches per epoch; one active evaluation; 49,392-second bounded job timeout; three infrastructure attempts |
| Patch surface | only `connect/resident_contract_manager.go`; maximum 262,144 bytes |
| Evaluation leaves | `/home/by/urnetwork/config/local` and `/home/by/urnetwork/vault/local`, direct and read-only |
| Evaluation leaf hashes | config `f2fd41f07258389a5b8cbfd12af69c7e71124755432e48e115933a66f835962d`; vault `f84b7bdd1976c5e404c196584025287ab346f4bcfd60196da9ca46191a39f3fa` |
| Artifact retention | versioned MinIO object storage with compliance retention and post-upload SHA-256 authentication; score commit fails closed |
| Monitoring | main Grafana dashboard plus provisioned page/warn rules in `warp/grafana/alerting/competition.yml` |
| Local evaluator audit | 10 passed, 0 pending, 0 failed |

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

The final deployment still records the exact pushed commit and image digests,
proves `/competition/readyz`, the MinIO object-lock check, and the Grafana rules
on the live main environment. Those are release verification steps, not new
architectural components.

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
| Season identity and dates | Code freezes six epochs, a 604,800-second submission window, close-time reveal, post-close grading, and automatic next-epoch creation. | Record the first `opens_at`, final `season_ends_at`, and `retain_until`. The cadence is decided; only calendar values remain. |
| Credentials | One season-wide AES-256 seed-encryption key is valid for all six epochs; every epoch independently draws a fresh 256-bit CSPRNG seed. Staging tokens/key exist root-only. | Rotate as one atomic season bundle or explicitly approve the staging bundle, deliver raw tokens out of band, and record revocation. There is no per-epoch seed-key rotation requirement. |
| Control-plane data services | **Complete by operator confirmation.** Queue/round/result state uses the main PostgreSQL and the existing main Redis/restore boundary. | Run the normal migration verification for the final commit; no new durable data service is needed. |
| Service supervision | **Complete by operator confirmation.** Main API plus one competition worker use the reviewed main-environment migration and boot ordering. | Verify the final deployed versions and singleton worker heartbeat. |
| Public ingress | **Complete by operator confirmation.** DNS/TLS/reverse proxy/firewall/rate limits are provided by main. | Smoke the final `/competition/*` routes, including the 262,144-byte request ceiling and 429 behavior. |
| Release distribution | Evaluator and historical control-plane evidence are sealed. | Record the final pushed server commit and main API/worker image/archive digests after these lifecycle changes. A registry is optional on one host; a verified sealed-archive load record is sufficient. |
| Artifact retention | Implemented through `server/blob`: every workload and authenticated attempt artifact is uploaded to exact MinIO versions under compliance retention and read back/hash-verified before score commit. `/readyz` fails if object lock or versioning is absent. | Prove the live bucket check, capacity, backup replication, and the named post-`retain_until` deletion owner. Grafana warns at 75% used and pages at 90%. |
| Monitoring and on-call | Competition metrics, dashboard, MinIO capacity views, and provisioned Grafana alert rules are implemented for the main Mimir/Grafana pipeline. | Deploy the final server and warp commits and map `severity=page|warn` through the existing main contact policy; record the human roster/incident contact in the operator record. |
| Submission integration | Main API implements authenticated generate/submit/poll plus public info, reveal, and leaderboard routes from `sn/api/competition.yml`. | Distribute endpoint/token/onboarding instructions and exercise revocation once. No separate API is required. |
| Leaderboard and winner | Implemented at public `GET /competition/leaderboard`; only finalized epochs appear and winner selection is deterministic. | Publish fees, rewards, eligibility, legal terms, and abuse/appeal handling. Also approve a fixed-base six-epoch season or define a separately signed winner-source promotion/rebuild process; code does not silently mutate the trusted base. |
| Apex | Adapter mapping and handoff fields are documented in `competition/APEX-HANDOFF.md`. | Macrocosmos must accept the asynchronous external-evaluator contract, stage it, record signed image identities, and activate the private registry entry. |

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

After the final commits are pushed:

1. build and deploy the main API through the normal main-environment release;
2. build `cli/competitionworker` from the exact same server commit;
3. build the host simulator with `cd connect/sim-latency && make`;
4. run `(cd connect/sim-latency && ./tests.sh)` and the Go control-plane gates;
5. record commit, binary SHA-256, image/archive digest, host, UTC time, and
   operator in `playbook.yml` or its signed deployment copy; and
6. retain the OpenAPI bytes and SHA-256 beside that release record.

The evaluator image remains pinned to the qualified digest in section 1 unless
its trusted source changes, in which case the containment and calibration gates
must be repeated. Main API code may move for this control-plane finalization;
the score baseline data is not recomputed merely because API routing or
retention code changed.

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
         .base_sha == "46515d82fe98ff666c61b2b5bb1d34a89cf4dad8" and
         .evaluation_policy.provider_count == 1800 and
         .evaluation_policy.replicates == 9 and
         .evaluation_policy.takeover_margin == 0.161'
```

On first boot, the worker must heartbeat before round generation. An
authenticated `/readyz` may still return 503 because the old staging round is
the last promoted rebaseline. That is expected; do not open submissions.

Prepare the first strict JSON request with `closes_at = opens_at + 7 days` and
`reveal_at = closes_at`. Create it far enough before opening to complete the
same-round R=9 rebaseline; the configured 16-hour preparation window covers
the 49,392-second job bound. Epochs 2 through 6 are created automatically only
after the prior post-close FIFO drains and its winner is finalized.

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
/home/by/urnetwork/server/competition/references/noop.patch
SHA-256 8bd57a48ac82a6e846b607a9301c48145da5c66717c9e3a341138d034d1e0775
```

Hold `/run/urnetwork/competition-operational.lock`, run
`competitionrebaseline` as the worker service user on CPUs `20,22`, and write
to a new root-owned output directory:

```bash
taskset -c 20,22 competitionrebaseline \
  --round_id "$COMPETITION_ROUND_ID" \
  --patch /home/by/urnetwork/server/competition/references/noop.patch \
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
to one cache identity even when multiple principals submit it. During an
active round, submitter responses intentionally hide raw scores, gate details,
and diagnostics.

Operate with these expectations:

- up to ten distinct canonical patches are admitted during the seven-day
  submission window; duplicate patches remain cache hits;
- no admitted job is claimable until `closes_at`; post-close grading is one
  FIFO evaluation at a time and HTTP 429 includes `Retry-After`;
- one job may legitimately remain active for hours and is bounded by 49,392
  seconds;
- infrastructure failures retry under the same job/cache identity, up to
  three attempts;
- structural/build/submission errors are terminal and do not get noise redraws;
- baseline and candidate each run nine repetitions with distinct fresh stores;
- every candidate build and run is offline/default-deny;
- accounting, resources, score, completion, and failure artifacts are retained
  and sealed; and
- `placeable` and `takeover_eligible` are different. A winner needs
  `takeover_eligible: true` and every G1–G6 gate true.

When the final accepted job becomes terminal, the worker atomically freezes the
winner and finalized timestamp. `GET /competition/leaderboard` then publishes
that epoch. If fewer than six epochs exist, the worker creates the next hidden
round with its 16-hour rebaseline preparation window and exact seven-day
submission window. The operator must promote the next round's rebaseline before
its `opens_at`; readiness remains false otherwise.

Monitor at least:

- unauthenticated `/competition/healthz` and authenticated `/competition/readyz`;
- authoritative host-heartbeat age and identity;
- queued/running job age, attempt count, lease owner, and lease expiry;
- API/worker exits, evaluator typed errors, OOM/timeout events, and cleanup;
- PostgreSQL and Redis health/latency/backups;
- `/var/lib/urnetwork/competition` bytes, inodes, immutable modes, and retention;
- Docker objects with `com.urnetwork.competition.job-id` labels; and
- drift in host, command, image, local-leaf, workload, and scorer hashes.

Install `server/grafana/dashboards/competition.json` through the normal Grafana
dashboard sync and `warp/grafana/alerting/competition.yml` through Grafana file
provisioning. The provisioned thresholds are: archive readiness missing/below
1 for one minute (page), worker heartbeat absent or older than 60 seconds for
five minutes while open/grading (page), queued-without-running grading for five
minutes (page), any five-minute control-plane/archive/infrastructure error
(page), MinIO over 75% used for 15 minutes (warn), and MinIO over 90% used for
five minutes (page).

## 8. Reveal, close, and retain

At `closes_at`, atomically reject new jobs, reveal the committed workload, and
make the accepted batch claimable. Verify `/competition/info` exposes the seed
and provider URL, then download the workload and authenticate both response
headers while the FIFO grades:

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

The epoch is not published merely because it closed or revealed. Publication
waits until every accepted job is terminal and the deterministic winner update
commits; only then may the automatic next epoch be created.

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
- [x] frozen p1800 scale, R=9 aggregation, and 16.1% takeover margin;
- [x] same-seed baseline and independent reference screen;
- [x] adversarial CPU/memory-bomb cleanup;
- [x] isolated direct read-only local leaves and no parent/all/main mounts;
- [x] fixed per-submission Docker build and offline execution;
- [x] authenticated main-API generate/submit/poll/info/reveal/leaderboard
  routes, FIFO/cache/failover, and structural OpenAPI conformance;
- [x] six exact weekly submission epochs, post-close batch grading,
  deterministic winner finalization, and automatic next-epoch creation;
- [x] MinIO versioned compliance retention with post-upload authentication and
  fail-closed readiness;
- [x] main Grafana metrics/dashboard plus provisioned worker, archive,
  evaluation, queue-stall, and MinIO capacity alerts;
- [x] operator-confirmed main PostgreSQL/Redis/restore boundary, service/migration
  ordering, and public ingress controls; and
- [x] evaluator release provenance/SBOMs, OpenAPI, baseline, and final reports.

Still to add or approve before a public competition starts:

- [ ] final season id, first `opens_at`, season end, and retention date (the
  six-epoch weekly cadence and close-time reveal are already frozen);
- [ ] atomic live credential/seed-key rotation or explicit approval to promote
  the staging-generated bundle;
- [ ] recorded final main-API/worker release plus either a verified local load
  record or approved digest-pinned registry publication;
- [ ] live MinIO `/readyz` proof, backup-replication record, capacity check, and
  the owner authorized to delete evidence after `retain_until`;
- [ ] deploy the final server/warp monitoring commits and bind `severity` labels
  to the existing main Grafana contact policy/on-call record;
- [ ] miner/submission onboarding, token distribution, and revocation flow;
- [ ] fees, rewards, eligibility, legal terms, and abuse/appeal process;
- [ ] explicit approval that all six epochs use the frozen season base, or a
  separately reviewed signed winner-source promotion/rebuild process; and
- [ ] Macrocosmos asynchronous-adapter/staging/private-registry acceptance and
  signed public Apex handoff artifacts.

Once those boxes are checked, run sections 3–6 in order, require `/readyz` to
return every check true for the new round, and only then enable public ingress.
