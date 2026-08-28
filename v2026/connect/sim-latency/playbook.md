# Sim-latency competition live-deployment playbook

Status date: 2026-08-28  
Local technical finalization: **complete — 10/10 required gates pass**  
Deployment model: one authoritative 12-physical-core host; 10 evaluation cores,
2 management cores; one content-addressed image per canonical submission patch.

This playbook launches the authenticated UR competition scoring service. It
does not claim that the external Apex registry, a public leaderboard, rewards,
or organizational operations have been activated. Those remaining pieces are
listed explicitly below.

Read these first:

- [Apex production calibration](APEX-CALIBRATION.md)
- [Final baseline infographic](final-baseline.html)
- [Finalization contract](FINALIZE.md)
- [Competition service README](../../competition/README.md)
- [Evaluator protocol](../../competition/EVALUATOR-PROTOCOL.md)
- [Competition OpenAPI](../../../sn/api/competition.yml)
- [Apex integration gap](../../competition/APEX-INTEGRATION-GAP.md)

## 1. Go-live position

### What is already frozen and qualified

| Item | Frozen value / state |
|---|---|
| Public patch-authoring tag | `apex-season-1` at `eb697281cbe0a19a27d7771fe69fb24c2c3dab8c` |
| Evaluator source | `46515d82fe98ff666c61b2b5bb1d34a89cf4dad8` |
| Control-plane source | `2ee4883f2b77cccfcbc69b3bcf1cb4ee613dad36` |
| Evaluator image | `sha256:2abcf145c0f914899debbd2fd52e57a16cf20072165c8d13f04a0ba487198a4c` |
| Host qualification | `acf226db6b8e50d67f8957cddb3903d5d4e9e82566935d61d270ccb5b03463a3` |
| Simulator / scorer | `bc843ce2b9cdcc41459362c7a682b08e7a12a8ac896443fe1e8aad94d4b17997` |
| Workload | 1,800 providers; 200 clients; 80 arrivals/min; quality window 2; 4 exchange hosts; 4 shards |
| Measurement | 180 seconds; impairment on; median of `R=9` |
| Takeover rule | candidate raw score `<= same-round baseline * 0.839`, plus G1–G6 |
| Queue / timeout | one admitted job; 49,392-second bounded score timeout; three infrastructure attempts |
| Patch surface | only `connect/resident_contract_manager.go`; maximum 262,144 bytes |
| Evaluation leaves | `/home/by/urnetwork/config/local` and `/home/by/urnetwork/vault/local`, direct and read-only |
| Evaluation leaf hashes | config `f2fd41f07258389a5b8cbfd12af69c7e71124755432e48e115933a66f835962d`; vault `f84b7bdd1976c5e404c196584025287ab346f4bcfd60196da9ca46191a39f3fa` |
| Local audit | 10 passed, 0 pending, 0 failed |

The host controls, hardened Docker boundary, trusted commands, `/etc` host
manifest, production-pressure CPU/memory-bomb cleanup, API/worker release
artifacts, API staging, FIFO/cache/failover, and reveal path have all passed.
The second image-identical host is not a launch requirement.

### Current machine state

At the time this playbook was written:

- the CPU and IRQ control units and Docker are installed and active;
- the host self-check exits zero with all required booleans true;
- both evaluator-mounted local-leaf hashes still match the frozen values;
- the isolated API configuration exists under
  `/etc/urnetwork/competition-api` and authenticates against its deployment
  manifest;
- the sealed API, worker, migration, and rebaseline binaries and OCI archives
  exist in the final evidence release;
- no competition API, worker, control-plane PostgreSQL, or control-plane Redis
  process is currently running; and
- no evaluation container or network remains from staging.

### Trust-boundary rule that must not be weakened

There are two different `local` resource trees:

1. The API reads its private resources from
   `/etc/urnetwork/competition-api/config/local` and
   `/etc/urnetwork/competition-api/vault/local`. The seed-encryption key and
   bearer-token hashes live here.
2. Candidate containers receive only the evaluator-safe
   `/home/by/urnetwork/config/local` and `/home/by/urnetwork/vault/local`
   leaves, directly and read-only.

Do **not** add the API's `competition.yml`, seed key, raw credentials, or any
`config/all`, `config/main`, `vault/all`, `vault/main`, parent config/vault
directory, Docker socket, or host control material to the candidate mounts.
The absence of `competition.yml` in the candidate-readable leaves is correct.

## 2. Pre-launch decisions and configuration

Do not open public submissions until every launch-blocking item in this table
has an owner and a recorded value.

| Area | Current state | Required before public launch |
|---|---|---|
| Season identity and dates | Installed API bundle says `sim-latency-season-1`, ends 2027-01-01, retains to 2027-02-01 | Approve or replace the real season id, end date, round open/close/reveal schedule, and retention date. These defaults were used for qualification, not organizational approval. |
| Credentials | Three root-only staging tokens and a seed key exist | Rotate or explicitly approve promotion of the staging key material; deliver submitter/operator tokens out of band; record revocation and emergency rotation procedure. Never log raw tokens. |
| Control-plane data services | Staging services were removed | Select and deploy durable PostgreSQL and Redis, backups, restore test, capacity, and service ownership. Per-run evaluator PostgreSQL/Redis remain ephemeral and separate. |
| Service supervision | Release binaries are sealed; processes are stopped | Add reviewed API, worker, and one-shot migration service definitions to the deployment system. No production API/worker unit is shipped by this repository today. |
| Public ingress | OpenAPI names `https://api.bringyour.com`; no live route is evidenced here | Add DNS, TLS, reverse-proxy route for `/competition/*`, rate limits, request-size limits compatible with 262,144-byte patches, and firewall rules. |
| Release distribution | Local OCI archives, provenance, SBOMs, and platform digests are sealed | Either load the sealed archives directly on this host or publish them to an approved registry and record full `repository@sha256:...` identities and registry credentials. A bare local image id is not a registry reference. |
| Artifact retention | Local immutable/quota behavior passed | Confirm durable capacity under `/var/lib/urnetwork/competition`, 32 GiB per-job transient ceiling, backup/WORM destination, retention deletion authority, and low-space response. |
| Monitoring and on-call | Failure/recovery mechanisms passed | Add actual metric/log destinations, alerts, on-call roster, reviewer access, escalation contacts, and incident channel. |
| Submission integration | Authenticated generate/submit/poll API and miner tooling exist | Decide whether miners call UR directly or through Apex; distribute endpoint/token/package instructions. |
| Leaderboard, fees, and rewards | Not implemented by this score API | Provide an external leaderboard/orchestrator and decide submission fees, scoring cadence, takeover publication, rewards, eligibility, terms, and abuse handling. |
| Apex | External adapter/registry decision remains pending | If Apex is the launch surface, obtain Macrocosmos acceptance, stage credentials, signed image identities, adapter semantics, and registry activation. This is external follow-through, not a failed local evaluator gate. |

The installed provisioner authenticates an existing bundle and intentionally
does not rotate it in place. A supported credential/date rotation or clean
season-bundle promotion procedure is still missing. Do not hand-edit one file:
the vault, raw-credential file, permissions, and deployment-manifest hashes
must change atomically. Until that procedure exists, treat credential rotation
and final season-date approval as a launch blocker.

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

Authenticate the installed API bundle without printing secrets:

```bash
sudo stat -c '%a %U:%G %n' \
  /etc/urnetwork/competition-api/config/local/competition.yml \
  /etc/urnetwork/competition-api/vault/local/competition.yml \
  /etc/urnetwork/competition-api/credentials.json \
  /etc/urnetwork/competition-api/deployment-manifest.json

/home/by/urnetwork/server-finalization-evidence/connect/sim-latency/eval-12c/\
final-calibration-p1800-cf0fd3a9/provision-competition-api.sh --preflight-only
```

Expected modes are `0440 root:by` for API resources and manifest, and `0400
root:root` for raw credentials. The provisioner must be run as the `by` release
operator, not copied or modified. Its authenticated SHA-256 is
`5a887a605d8ff7f9407800e4ac586d0e5ed82c2cebf84d570e91c7d7f6819d26`.

## 4. Deploy data services and the frozen release

The final release root is:

```text
/home/by/urnetwork/server-finalization-evidence/connect/sim-latency/
  eval-12c/final-calibration-p1800-cf0fd3a9/control-plane-release/final
```

Use that path directly; do not substitute a newly built checkout:

```bash
COMPETITION_RELEASE_ROOT=/home/by/urnetwork/server-finalization-evidence/connect/\
sim-latency/eval-12c/final-calibration-p1800-cf0fd3a9/control-plane-release/final
```

Before service start, verify at least these hashes against
`release-build.json`:

| Artifact | SHA-256 / identity |
|---|---|
| `binaries/api` | `ae0bf6cd7fa6142b1e9ac40b06ac3c475cbe90f2fe82f45cb0ce6e4bbac7a611` |
| `binaries/competitionworker` | `ae0591753be1436cb258c7d1c6776b6ea1b44c512e2dade906771628d20834e3` |
| `binaries/competitionrebaseline` | `95bbbe458ea8ea3fddb7a1416070f82b3860ca6488a7d49e19b95ffe8f1dcdfa` |
| `binaries/competitiondbinit` | `a842fe9d36ce53f213e3b8754bd164007f40d5ab9e476f391bbb450b95664267` |
| API platform manifest | `sha256:363e9b53ab8f2e7fa4700a59ad9b58c9bac56079652dec816745d7b9e0c0e6a6` |
| Worker platform manifest | `sha256:ed3c7d0bc63c66d1a874b270a47d7074a024be631c27a342f185d049d9ffb80d` |

Use one root-owned environment file for API, migration, rebaseline, and worker.
It must point the API at the isolated resource roots:

```text
WARP_CONFIG_HOME=/etc/urnetwork/competition-api/config
WARP_VAULT_HOME=/etc/urnetwork/competition-api/vault
WARP_ENV=local
WARP_SERVICE=api
WARP_DOMAIN=bringyour.com
WARP_HOST=127.0.0.1
WARP_BLOCK=competition
```

Add the selected durable PostgreSQL and Redis hostnames through the normal
server environment. Do not put raw bearer tokens in this environment file.

Apply migrations once, from the frozen release, before the API starts:

```bash
taskset -c 20,22 "$COMPETITION_RELEASE_ROOT/binaries/competitiondbinit" | \
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
3. durable PostgreSQL/Redis and storage;
4. successful `competitiondbinit` one-shot;
5. API on the two management CPUs;
6. one worker with a stable identity on the same two management CPUs.

The API command is the frozen `api` binary with its selected listen port. The
worker command is:

```bash
taskset -c 20,22 "$COMPETITION_RELEASE_ROOT/binaries/competitionworker" \
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

Prepare a strict JSON request with `opens_at < closes_at < reveal_at`. Create
the round far enough before opening to complete the same-round R=9 rebaseline;
the configured job bound is 49,392 seconds, so use at least a 15-hour launch
buffer unless an operator explicitly accepts a smaller window.

```json
{
  "opens_at": "REPLACE_WITH_UTC_TIME",
  "closes_at": "REPLACE_WITH_LATER_UTC_TIME",
  "reveal_at": "REPLACE_WITH_POST_CLOSE_UTC_TIME"
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
taskset -c 20,22 "$COMPETITION_RELEASE_ROOT/binaries/competitionrebaseline" \
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

- one job is admitted/evaluated at a time; HTTP 429 includes `Retry-After`;
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

Monitor at least:

- unauthenticated `/competition/healthz` and authenticated `/competition/readyz`;
- authoritative host-heartbeat age and identity;
- queued/running job age, attempt count, lease owner, and lease expiry;
- API/worker exits, evaluator typed errors, OOM/timeout events, and cleanup;
- PostgreSQL and Redis health/latency/backups;
- `/var/lib/urnetwork/competition` bytes, inodes, immutable modes, and retention;
- Docker objects with `com.urnetwork.competition.job-id` labels; and
- drift in host, command, image, local-leaf, workload, and scorer hashes.

## 8. Reveal, close, and retain

At `closes_at`, reject new jobs but let already accepted work reach a terminal
state. At `reveal_at`, verify `/competition/info` exposes the seed and provider
URL, then download the workload and authenticate both response headers:

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
- [x] authenticated API, FIFO, cache, failover, rebaseline, reveal, and
  immutable retention staging;
- [x] release binaries/images, provenance, SBOMs, OpenAPI, and final reports.

Still to add or approve before a public competition starts:

- [ ] final season id, opens/closes/reveal cadence, season end, and retention
  dates;
- [ ] atomic live credential/seed-key rotation or explicit approval to promote
  the staging-generated bundle;
- [ ] durable control-plane PostgreSQL/Redis deployment and restore proof;
- [ ] reviewed API/migration/worker service definitions and boot ordering;
- [ ] public DNS/TLS/reverse-proxy/firewall/rate-limit configuration;
- [ ] approved registry publication or sealed-archive distribution record;
- [ ] artifact capacity, backup/WORM target, and retention deletion owner;
- [ ] monitoring destinations, alert thresholds, on-call roster, and incident
  contacts;
- [ ] miner/submission onboarding, token distribution, and revocation flow;
- [ ] leaderboard/takeover publication, fees, rewards, eligibility, legal
  terms, and abuse process; and
- [ ] if using Apex, Macrocosmos adapter/staging/registry acceptance and signed
  public handoff artifacts.

Once those boxes are checked, run sections 3–6 in order, require `/readyz` to
return every check true for the new round, and only then enable public ingress.
