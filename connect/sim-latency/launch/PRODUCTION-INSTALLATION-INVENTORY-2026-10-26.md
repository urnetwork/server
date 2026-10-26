# Production installation inventory — October 26

Preparation only, inspected against server `87e1489b6` and actual root-owned
inventory schemas. No deployed service/config/credential/database was changed; no build,
test, runtime probe or provider request ran for this inventory. Commands below
are prospective, subject to root's release and exclusive-slot authorization.
Public opening remains **2026-10-28T00:00:00Z**.

## Actual installation is still old staging

| Existing file | Read-only observation | Production action |
| --- | --- | --- |
| `/etc/urnetwork/competition-preparation.json` | `staging:true`, old staging-r2 season, October 20 worker/qualifier, old API pin | Preserve; use separate production inventory below |
| `/etc/urnetwork/competition-qualification.json` | Same old scope; no `reviewed_build_state_directory` | Preserve; production must include that directory |
| `/etc/urnetwork/competition-host.json` | Old `27bda2…` evaluator, old qualification/rebaseline, 4 GiB artifact quota, real local leaves | Requires separately authorized, genuinely qualified new image/host/storage installation; not a JSON relabel |
| `/etc/systemd/system/competition-v2-staging.service` | Only competition unit found; native supervisor `bed450…`; `WARP_VERSION=2026.10.20-3eaf2bc08` | Do not rename/reuse its state as production |
| `/home/by/urnetwork/vault/main/competition.yml` | `by:by`, 0600; legacy submitter/operator and four **staging-only** v2 roles | Add four new production scopes; preserve seed/old tokens |
| `/etc/urnetwork/competition-api/credentials.json` | Legacy `tokens` map for submitter/operator, not a scoped `credentialDelivery` | Not a source of production v2 credentials |
| Dedicated review key | Present only in private main-vault leaf, `by:by` 0600 | Securely provision only dedicated root-protected runtime leaves; never mount a vault parent |

`/etc/urnetwork/competition-review.json`,
`/var/lib/urnetwork/competition-review/model.key` and the source-test runtime
key leaf were absent at inspection. The old installed source-qualification
settings refer to the latter; settings presence is not provider qualification.
The last real provider result is quota exhaustion, not approval. No blind retry.

Root separately confirmed the documented
[gpt-6.1-sol Responses/low contract](https://developers.openai.com/api/docs/models/gpt-6.1-sol)
and dedicated-key model-metadata HTTP 200 for that exact ID; private evidence is
`temp/source-test-model-access-20261026.yr7V2Emf`, response SHA-256
`6758fa4e5c9713796e25f54fa8982b68eade256fdb6828213ca5aae9ff740d92`.
That resolves the source-test model access/spec question only. Genuine low-model
test execution and funded inference remain unqualified; no model switch or
paid retry is inferred. This observation is historical. The user subsequently
selected Snow for all automated model calls: Qwen, Gemma and Muse, serially,
with an attributed union and unanimous approval. That prospective policy needs
no OpenAI key or funding; genuine Snow inference remains unqualified.

## Resolved policy versus release-dependent values

Resolved: environment `main`, competition `sim-latency-season-1`, new season
`sim-latency-production-20261028`, `staging:false`, six complete 604800-second
windows, $20 admission, `queue_limit:0`, R9, whole-job 10800-second deadline,
initial source generation 0/activation generation 1, ten-repository
`mono_root_v1`, protected instrument/metadata, and three-model Snow review policy
`elected_winner_snow_union_v1`. Required API capability is
`competition.reward.v2.runtime-blob.v1`, not a software version or image tag.

Original A completed at **2026-10-26T15:01:22Z**: accepted report SHA-256
`e78c1a965d485d0a183f95f72797aa75c5c0ba22e52acbcf28749cfe6ecb3adb`,
**M = 0.269 (26.9%)**, published in freeze commit `1797ec723`.
[The freeze](CALIBRATION-THREE-A-FREEZE-2026-10-26.md) binds 54 old-C1
observations, six native replays and 99.525% bootstrap placeability. This is
not C2 measurement or complete production qualification.

Not interchangeable or yet fixed by this inventory:

- Successful fresh C2 bridge and all-three B acceptance at the already frozen
  **0.269**, with exact accepted-A report/input pins; old C1 stays labeled C1.
- Qualified C2 image/binary/host receipt and raw/canonical source locks.
  Prospective C2 source is `a9e8e1a710b24c749e6499a0606938252c83e136`, canonical
  ten-lock `ec322ed2db023e15ea5dc3eea60a6ca5989661bba737c340fea3477d8d1300cf`;
  source preparation is not an image or runtime qualification.
- New control-plane source commit, clean exact-Go binaries and individual
  program hashes; actual worker OCI identity and Snow review OCI identity;
  rendered immutable wrapper/settings hashes. Do not use an evaluator image
  ID for API/worker identity or a native binary hash for an OCI ID.
- Current-boot host/bomb/rebaseline/artifact proofs, genuine three-model Snow
  review and once-executed Go-suite/three-model attestation, monitoring delivery.
  Independent prelaunch tests retain Sol 6.1 low ownership.
- Approved policy/criteria/settlement/burn/repeat identities, outer season end
  and retention. Six full weekly windows total at least 42 days, but unbounded
  FIFO drain can extend elapsed calendar time; do not truncate accepted work
  or silently cap the sixth window at December 9.

The frozen synthetic local-leaf content hashes are already known:
`b8a0996c29195cad1839fa6f7e32933f01f812d573df6e3bb5f9e7b6276430b7`
(config) and
`a68c0e07eaa6d831c9a562128950237e76b0cd0f942afd1fec3d7a32b3aed7c7`
(vault). Permanent production paths, copied-byte readback and qualification at
those exact paths still need proof. Do not use the old host file's main local
leaves as a substitute for the calibrated isolated fixture.

Render [competition-production.yml.template](competition-production.yml.template)
only from these genuine receipts. Its existing C1 source/image/binary/lock
values must move together to the approved C2 identities after the bridge; do
not merely fill its `${...}` tokens. The source ledger's
`significant_improvement_percent` is **M × 100**; typed evaluation
`takeover_margin` is **M**. Do not substitute the old zero-margin staging policy.

## Database and new main runtime release

Deploy the pushed migration chain through the selected control release using
the normal approved main migration workflow. Required named migrations include
`competitionAutomationRuntimeBlobMigration` (historical 851),
`competitionWinnerReviewMigration()`, `competitionSourceRetirementMigration()`,
`competitionScheduledOpeningMigration()`, and the newly appended
`competitionWinnerReviewProviderMigration()` /
`competitionRepositoryPatchMigration()`. The last two prospectively admit
Mini/xhigh and explicit broad-repository policy; they do not relabel old reviews.
The live database version was **not queried**. Do not guess new migration
numbers or tell the operator that replaying only 851 completes this release.

The actual local migration CLI is `competitiondbinit` with **no arguments**.
It applies all pending migrations, not a named subset, then reports
`database_version` and `migration_count`; their equality is required. Pending
unrelated migrations need the normal DBA rollout review too. Example only in
the approved main environment, using a separately built qualified binary:

```bash
env WARP_ENV=main WARP_SERVICE=competitionworker WARP_BLOCK=competition \
  WARP_HOME=/home/by/urnetwork WARP_VERSION="$production_runtime_version" \
  "$production_control_directory/competitiondbinit"
```

Build API/worker/control programs from the **qualified control-plane main**,
not minimal evaluated C2. The native control inventory needs `sim-latency`,
`competitionworker`, stripped `competitionprepare`, `competitionqualify`,
`competitiondbinit`, trusted host checker and exact build/qualifier/review
wrappers. Build and retain Go 1.27.3 metadata and `vcs.modified=false`.
`WARP_VERSION` must name the actual new staged release; its value is not an
OCI digest. Do not reuse the old October 20 staging version.

Current Warp CLI contracts, on an approved build/deployment host, are:

```bash
warpctl stage version next release --message='qualified competition production control release'
warpctl ls version -b
warpctl build main /home/by/urnetwork/server/cli/api/Makefile
warpctl build main /home/by/urnetwork/server/cli/competitionworker/Makefile
warpctl build main /home/by/urnetwork/warp/config-updater/Makefile --config_restart=no
warpctl deploy main config-updater "$production_runtime_version" --percent=100 --preflight
warpctl deploy main api "$production_runtime_version" --percent=100 --preflight
```

After all approved rollout prerequisites, use the same explicit version for
the corresponding deployment calls without `--preflight`, with a positive
`--timeout=15m`. Do not use `latest`, `--timeout=0`, or restart unrelated main
services. `--config_restart=no` prevents an uncontrolled config-wide restart;
the intended API blocks must actually roll to receive the new config. Verify
every serving block, including any newer hand-deployed block intentionally
skipped by a normal only-older rollout. Config-updater does **not** provision
vault. Keep evaluator-host builds/tests away from a live measured job.

## Exact additive credential commands

Only after the current-main credential guards pass and root authorizes real
generation, run as the **existing vault owner** (`by`, observed above) so the
CLI's atomic replacement does not unexpectedly change its owner. Use a new
private 0700 delivery directory and `umask 077`; no token belongs in terminal
output, arguments, logs, public config or this repository.

```bash
set -euo pipefail
set -o noclobber
umask 077
for production_role in scheduler reviewer settlement source_promoter; do
  "$production_control_directory/sim-latency" credentials rotate-token \
    --vault=/home/by/urnetwork/vault/main/competition.yml \
    --delivery="$production_delivery_directory/$production_role.delivery.json" \
    --role="$production_role" \
    --name="production-20261028-$production_role" \
    --environment=main \
    --competition-id=sim-latency-season-1 \
    --season-id=sim-latency-production-20261028 \
    > "$production_delivery_directory/$production_role.receipt.json"
done
```

`rotate-token` **adds** the new unique name/hash/scope under the vault lock;
seed, legacy credentials and staging roles remain unchanged. Existing delivery
or duplicate name refuses; do not delete either to mask a partial attempt.
Raw bytes go only to mode-0600 delivery JSON at `credentials[0].token`; stdout
is a receipt with names/roles/hashes/scopes, not token bytes. A safe metadata
inspection of a newly generated delivery is:

```bash
jq '{schema,credentials:[.credentials[]|{name,role,environment,competition_id,season_id}]}' \
  "$production_delivery_directory/reviewer.delivery.json"
```

Do not use `credentials generate`, `rotate-seed`, `revoke`, or staging names.
Legacy submitter/operator roles cannot claim v2 scope. Existing legacy raw
delivery files may be inspected for role names only; their tokens cannot be
promoted to another scope by changing a JSON label. Securely deploy the updated
main vault to all serving API hosts through the normal secret process,
preserving seed and all credentials. Provision only the new source-promoter
and reviewer **bare token bytes** into their new root-owned 0400 runtime
leaves below, without printing them. Scheduler/settlement delivery stays with
its separately authorized consumer. Keep the existing operator credential for
authenticated readiness unless a separately approved operator addition is needed.

## Separate protected production inventory

The proposed production namespace avoids changing staging files or state.
Render every token; templates are not runnable configuration. JSON key names
are the actual current structs, not invented flags:

| Template | Prospective installed path |
| --- | --- |
| [production-preparation.json.template](production-preparation.json.template) | `/etc/urnetwork/competition-production-20261028/preparation.json` |
| [production-qualification.json.template](production-qualification.json.template) | `/etc/urnetwork/competition-production-20261028/qualification.json` |
| [production-source-build.json.template](production-source-build.json.template) | New immutable control bundle's `source-build.json` |
| [production-source-qualification.json.template](production-source-qualification.json.template) | Same bundle's `source-qualification.json` |
| [production-review.json.template](production-review.json.template) | `/etc/urnetwork/competition-review.json` — this path is fixed by the review wrapper |
| [production-supervisor.env.template](production-supervisor.env.template) | `/etc/urnetwork/competition-production-20261028/supervisor.env` |
| [production-supervisor.service.template](production-supervisor.service.template) | `/etc/systemd/system/competition-production-20261028.service` |

Protect new `/etc`/`/var/lib` private directories as root:root 0700,
inventory/host-read credential leaves as root-owned 0400 and installed executable bundles as 0555, with
protected symlink-free ancestry. `reviewed_build_state_directory` must equal
both build/qualifier sandbox paths. Do not fill success markers by hand.
The shared active host file remains `/etc/urnetwork/competition-host.json`;
its new image/fingerprint/storage controls require real independent installation
and qualification before preparation, not copied old rebaseline markers.

The build wrapper's actual command is `sim-latency source-build-broker
--build-settings=PATH --build-settings-sha256=SHA`; the qualifier wrapper is
`sim-latency source-qualify-build --qualification-settings=PATH
--qualification-settings-sha256=SHA`. Render both wrappers immutably and pin
their **rendered** hashes in their consumers. The current review wrapper uses
the real immutable image ENTRYPOINT and only
`/var/lib/urnetwork/competition-review/model.key`: mode 0400, owned by the
**actual mapped host UID for container UID 65532**, not root or a guessed host
offset. Its parent remains root-owned 0700, denying host traversal. Determine
the mapping from already qualified daemon/namespace evidence before provisioning;
the broker may load the key at startup. The revised wrapper independently
captures the running child's complete `uid_map` plus stat-only key metadata and
rejects a wrong owner/map/mode before releasing provider input. No key bytes are
saved in evidence. Wrong provisioning is a fail-closed diagnostic, not grounds
to widen mode bits or fall back to a different user. The active old 16-test
private qualification snapshot is unchanged; this is a new source revision
requiring its separate 22-test qualification.
Provision the dedicated source-test key separately at
`/var/lib/urnetwork/competition-source-tests/model.key`. Never mount config/main,
vault/main, `all`, or any parent into candidate/review containers. Genuine Mini
fixtures and genuine prescribed low-model tests remain required after funding;
quota/refusal/uncertainty cannot produce approval or a readiness receipt.

## Actual API identity without freezing main

Keep `api_image_digest:""` in **both** preparation and qualification JSON.
That is an explicitly supported discovery setting, not a missing proof. Fenced
`competitionprepare` publishes the exact retained blob envelope, POSTs an empty
body to `/competition/v2/source/GENERATION/readiness` using the scoped token
file, requires HTTP 200 and header
`X-Competition-Api-Capability: competition.reward.v2.runtime-blob.v1`, and checks
the response against the independent durable role row before host qualification.
The first actual API OCI identity is retained for that intent; later normal
main rollouts do not rewrite its receipt or need a season-wide image freeze.

`/competition/healthz.version` is only `WARP_VERSION`. `/competition/info`'s
evaluator digest is not the API image. The runtime blob pins the exact source
intent/evaluator image; API/worker OCI identities live in their independent
readiness receipts and each evaluation. Do not invent an API digest from a tag.

Future read-only verification on **every** serving host can use:

```bash
docker ps --filter label=warp.env=main --filter label=warp.service=api --format '{{.ID}}'
docker inspect --format '{{.Id}} {{.Image}} {{index .Config.Labels "warp.image_digest"}} {{index .Config.Labels "version"}}' "$api_container_id"
```

Current Warp resolves `.Id` from the pulled platform image, executes that exact
ID, and sets both `warp.image_digest` and `WARP_IMAGE_DIGEST` from it. Record
actual per-host IDs; a multi-architecture registry index digest is different.
This describes inspected source; the installed Warp runner/version and actual
container observations still need confirmation during authorized rollout.
For a host-native worker, its pinned native binary must independently match
the qualified worker-image release; setting an arbitrary environment digest is
not qualification. Do not dump all container environment variables.

Using the normal read-only main database connection, the exact receipt query is:

```sql
SELECT generation, role,
       receipt_json->>'actor_id' AS actor_id,
       receipt_json->>'target_round_id' AS target_round_id,
       receipt_json->>'manifest_sha256' AS manifest_sha256,
       receipt_json->>'source_lock_sha256' AS source_lock_sha256,
       receipt_json->>'runtime_image_digest' AS runtime_image_digest,
       receipt_json->>'qualification_sha256' AS qualification_sha256,
       receipt_json->>'ready' AS ready
FROM competition_source_readiness
WHERE environment='main'
  AND competition_id='sim-latency-season-1'
  AND season_id='sim-latency-production-20261028'
  AND generation=1
ORDER BY generation, role;
```

Require exactly one real API and one real worker acknowledgment for the same
generation/target/manifest/lock/qualification. Never insert or copy rows manually.
Every blob-capable API/worker, including late joiners, independently authenticates
the retained exact-version envelope on use; filesystem fallback is not allowed.

## Fenced bootstrap, supervisor and final readiness

Only after qualified statistics/provider/build/host/storage/policy prerequisites,
approved main migration/rollout/vault deployment and controlled configuration
enablement, publish the exact production ledger/config commit. No old staging
round, scope or state directory may be reinterpreted. Bootstrap needs the
enabled v2 policy but creates a durable intent/checkpoint, not an active pointer:

```bash
taskset -c 20,22 "$production_control_directory/sim-latency" bootstrap-source \
  --enable-automation --owner-id=sille-production-20261028 \
  --state-dir=/var/lib/urnetwork/competition-production-20261028/supervisor \
  --manifest-commit="$production_config_commit" \
  --opens-at=2026-10-28T00:00:00Z \
  --source-config=/home/by/urnetwork/config/main/sim-latency.yml \
  --repos-root=/home/by/urnetwork
```

Run under the rendered production environment, including the actual qualified
Go bin directory in PATH. Bootstrap invokes `go mod edit -json`; an interactive
Go version is not proof of the service PATH. The future opening is immutable on
recovery. A late failed launch cannot silently move it or create a past opening.

After root authorizes runtime progression, install/start the **new** durable
service using the exact supervisor flags in the template. Do not run concurrent
`--once` reconcilers or a second worker. The service uses a database-fenced
lease, private durable state, management affinity and joined child cleanup.
Preparation itself drives runtime blob publication, genuine API acknowledgment,
separately labeled R1/R1 host-readiness probe, retained host/reset/rebaseline
proof, independent worker acknowledgment, pointer activation and a scheduled
full-week production round. That small probe is not the official R9 epoch baseline.

Finally, after the actual scheduled round exists, run the qualified preflight
(initial source generation is **0**, while the public epoch is **1**):

```bash
taskset -c 20,22 "$production_control_directory/sim-latency" launch-preflight \
  --epoch=0 --source-config=/home/by/urnetwork/config/main/sim-latency.yml \
  --repos-root=/home/by/urnetwork --evaluator-image="$qualified_c2_image_id" \
  --api-url=https://api.bringyour.com \
  --operator-token-file="$private_operator_token_file" \
  --grafana-url="$approved_grafana_origin" --grafana-token-file="$private_grafana_token_file" \
  --metrics-url="$approved_mimir_query_origin" --metrics-token-file="$private_metrics_token_file" \
  --openapi=/home/by/urnetwork/sn/api/competition.yml \
  --artifact-capacity-bytes=1099511627776 --preflight-timeout=10m \
  --out="$new_production_preflight_file"
```

Use a new output path and preserve failures. Require real protected MinIO
COMPLIANCE/versioning/replication/exact-version readback/retention and approved
capacity, all serving API capabilities, exact worker/host readiness and delivered
Grafana/Mimir heartbeat/alerts to `support@ur.xyz`. MinIO health 200 and an API
health version alone are insufficient. Publish the actual handoff with
`handoff-manifest --epoch=0 --evaluator-image=ID --openapi=PATH
--baseline-manifest=PATH --preflight=PATH --source-config=PATH --repos-root=PATH
--out=NEWPATH`; retain the API/worker OCI receipts with it. Public admission is
still bound to the fixed opening, not the time the supervisor process starts.
