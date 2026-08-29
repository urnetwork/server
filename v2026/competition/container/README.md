# Container evaluator

The evaluator uses one immutable image per canonical submission patch. It does
not accept a miner Dockerfile, build command, repository URL, dependency
change, or runtime command.

The trusted release builds a base image containing clean pinned source clones,
the Go toolchain, a warmed module cache, the simulator/scorer, the structural
patch validator, and the fixed entrypoint. `build-submission.sh` derives a
candidate from that base using only `canonical.patch` and `policy.json`. Every
candidate build step has `network=none`, revalidates both input hashes, applies
the patch, creates a deterministic clean Git commit, runs the frozen compile
checks, and records the base/patch/policy/builder/build identities in OCI
labels and `/opt/urnetwork/image-identity.json`. Its cache key covers the base
image id, canonical patch, patch policy, and fixed submission Dockerfile; an
existing tag is reused only after every identity label authenticates.

`connect/sim-latency/**` is a protected evaluator tree, not submission code.
The API validator rejects it from a hard-coded denylist even if a malformed
operator policy allowlists a file there, the frozen policy must repeat that
deny rule explicitly, and the builder requires the directory's Git tree id to
remain identical before and after the candidate commit. A direct edit, nested
edit, rename, copy, new file, deletion, mode change, or symlink attempt is a
terminal invalid submission. Runtime source is mounted read-only, and the
trusted baseline and scorer continue to execute from the pristine base image.

For every evaluation attempt, `prepare-evaluation-source.sh` copies fresh
`server`, `connect`, `sdk`, and `proxy` repositories out of that authenticated
base image into a bounded temporary directory. It checks out a local
`sim-latency` branch at each epoch source-lock commit without reading or
changing any host repository checkout. Baseline and candidate have distinct
trees; the builder applies the canonical patch only to the candidate tree.
The selected tree is mounted read-only at `/workspace` in each runner and is
removed before evidence is retained. This keeps continuously updated control
code on `main` separate from the measured source.

This replaces custom per-job cgroup filesystem code. Docker creates the
container cgroups; Compose puts runner, PostgreSQL, and Redis below one
root-owned `cgroup_parent` and applies explicit CPU-set, memory/swap, PID, and
file-descriptor limits. It also drops all Linux capabilities, enables
`no-new-privileges`, uses read-only root filesystems and tmpfs state, publishes
no ports, and gives the run stack an `internal` bridge. The file-only scorer
runs later with `network_mode: none`.

The frozen host split gives untrusted builds and evaluations 10 physical cores
and reserves 2 physical cores for the worker, Docker control plane, and cleanup.
The active stack is capped at 96 GiB total (runner 72 GiB, PostgreSQL 16 GiB,
Redis 8 GiB), leaving at least 24 GiB outside its declared ceilings on a
qualified host. Submission builds use the same 10-core set in a separate
cgroup parent, with a 12 GiB no-swap limit, a 600-second TERM/KILL deadline,
and bounded, fully drained build logs. `resource-boundary.sh` derives the exact
logical CPU ids from topology and rejects overlapping sets or insufficient
memory capacity.

The evaluator mounts its complete untrusted work/evidence tree as a root-owned
32 GiB `tmpfs` with `nosuid,nodev,noexec`. Candidate output, repeated run
copies, and scorer inputs therefore share one hard aggregate ENOSPC ceiling.
Compose binds the frozen host `config/local` and `vault/local` directories
directly, independently, and read-only. Their sorted-file manifest digests are
part of the evaluation policy and are checked before and after every run. No parent
`/runtime`, `config`, or `vault` bind crosses the container boundary, so sibling
`main`, `all`, and `site` paths remain unreachable. A dedicated key now makes
`vault/local` self-contained; no key is resolved from `vault/all`. Per-stage
throwaway store passwords use an explicit local-evaluator-only override. After
candidate execution ends, the worker validates the tree, copies only the
bounded evidence to durable storage, authenticates it, and seals it read-only.
A compile/vet failure returns the terminal typed
`candidate_build_failed` result with build-boundary evidence; it does not forge
runtime reset or accounting claims for a measurement that never started.

Docker does not replace host qualification. The authoritative host still needs
exactly 12 physical CPUs exposed (10 evaluation + 2 management), fixed
SMT/governor/turbo/NUMA/IRQ policy, frozen kernel and microcode, Docker
user-namespace remapping or an equivalently
rootless daemon, a default-deny host firewall, and a root-owned parent slice.
The evaluator must verify the actual container cgroup ids, limits, network,
image ids, exit/OOM state, and accounting after every stage.

## Trust and data flow

1. The API authenticates a text patch, canonicalizes it, stores its SHA-256,
   and queues the immutable patch/round identity.
2. The trusted worker creates fresh baseline/candidate source directories from
   the base image's authenticated repositories, then writes only the patch and
   frozen policy into a new build context. `build-submission.sh` patches only
   the temporary candidate checkout and produces one content-addressed image.
   Cache reuse is permitted only for the same base image id, patch SHA-256,
   policy SHA-256, and trusted Dockerfile SHA-256. Candidate builds disable
   BuildKit's variable default attestation and retain the authenticated build
   identity in the worker's immutable evidence instead.
3. A pristine base-image stack measures baseline replicates in its own Compose
   project with fresh tmpfs PostgreSQL/Redis.
4. A candidate-image stack measures candidate replicates in another project
   with another fresh store pair. It receives the authenticated providers file
   and throwaway job credentials, never production credentials or baseline
   artifacts. The workload is necessarily visible to code while it executes;
   default-deny networking prevents exfiltration.
5. A pristine base-image scorer receives read-only baseline/candidate
   artifacts, has no network namespace, and writes the score. The worker then
   authenticates and seals the resource/accounting/score manifests.

Separating those three projects is important. Putting the trusted baseline,
candidate process, hidden workload control material, and scorer into one
container would let a malicious patch read or alter trusted evidence.

## Build

Build the season base only from reviewed commits:

```bash
./competition/container/build-base.sh \
  --epoch 0 \
  --tag registry.example/urnetwork/sim-latency-evaluator-base:season-1
sudo docker push registry.example/urnetwork/sim-latency-evaluator-base:season-1
```

Record the pushed `repository@sha256:...` digest in the frozen competition
policy. `--include-worktree` creates deterministic synthetic commits in a
temporary context and is only for local smoke testing; its image is labeled as
a development snapshot.

Build a submission from API-authenticated files:

```bash
attempt_source="$(mktemp -d /var/lib/urnetwork/competition/source.XXXXXXXX)"
./competition/container/prepare-evaluation-source.sh \
  --base-image registry.example/urnetwork/sim-latency-evaluator-base@sha256:... \
  --destination "$attempt_source/candidate"
./competition/container/build-submission.sh \
  --base-image registry.example/urnetwork/sim-latency-evaluator-base@sha256:... \
  --source-root "$attempt_source/candidate" \
  --patch /var/lib/urnetwork/jobs/JOB/canonical.patch \
  --policy /var/lib/urnetwork/jobs/JOB/policy.json \
  --tag registry.example/urnetwork/sim-latency-submission:JOB
```

The production form refuses a tag-only base. `--allow-local-base` exists for a
daemon-local smoke image. The script emits strict JSON containing the image
id/key, base SHA, deterministic candidate SHA, patch SHA-256, policy SHA-256,
and trusted Dockerfile SHA-256.

## Run

Freeze and hash the direct host `config/local` and `vault/local` directories
with `hash-local-mount.sh`; record those digests in both competition and host
policy. Those two leaf directories are mounted read-only. `prepare-runtime.sh`
is only a smoke-fixture generator and is not called by the production evaluator.
The empty output is writable only to UID/GID 65532 inside the containers.
`docker-id-map.sh` resolves that container identity through a short live probe;
the evaluator uses the translated host UID/GID for bind ownership and retains
the mapping hashes as evidence. This supports daemon `userns-remap` and rootless
split-root mappings without assuming host UID/GID 65532. Production promotion
rejects an identity mapping. Use a unique Compose project and cgroup parent for
every stage.

Start the stores, run the trusted migration gate, authenticate its strict JSON,
and only then start the runner:

```bash
sudo docker compose \
  --env-file /var/lib/urnetwork/jobs/JOB/candidate/compose.env \
  -f competition/container/compose.yml \
  --profile run up --detach --wait postgres redis

sudo docker compose \
  --env-file /var/lib/urnetwork/jobs/JOB/candidate/compose.env \
  -f competition/container/compose.yml \
  --profile run run --rm --no-deps --no-tty migrate \
  > /var/lib/urnetwork/jobs/JOB/candidate/migration.json

jq -e '.schema == 1 and .database_version > 0 and
       .database_version == .migration_count' \
  /var/lib/urnetwork/jobs/JOB/candidate/migration.json

sudo docker compose \
  --env-file /var/lib/urnetwork/jobs/JOB/candidate/compose.env \
  -f competition/container/compose.yml \
  --profile run up --no-deps --abort-on-container-exit --exit-code-from runner runner
```

Do not put the one-shot migrator and runner in the same
`--abort-on-container-exit` invocation: a successful migrator exit would stop
the measurement runner. The trusted worker treats a migration command failure,
malformed result, or version mismatch as an infrastructure failure.

After capturing container inspect data and cgroup counters, remove only that
fully resolved project:

```bash
sudo docker compose \
  --env-file /var/lib/urnetwork/jobs/JOB/candidate/compose.env \
  -f competition/container/compose.yml \
  --profile run down --volumes --remove-orphans
```

Run baseline and candidate with distinct project/input/output paths. Invoke
the `score` profile with the pristine base image and `EVALUATION_ACTION` set to
`baseline` or `score` as appropriate. `job.env.example` documents every frozen
limit and runner variable; a production worker writes separate Compose and
container env files so unrelated orchestration values are not exposed to the
candidate.

The Compose file deliberately sets `pull_policy: never`. A trusted setup step
pre-pulls every digest and verifies it before the host is taken offline. Builds
of the base image use the host build network to freeze dependencies (and must
retain provenance/SBOM evidence); submission builds and all evaluation stages
may not.

## Live development smoke

Run `./competition/container/smoke-test.sh` after building a development base.
The test derives a harmless allowed patch twice, requires authenticated cache
reuse of the same image id, starts fresh tmpfs PostgreSQL/Redis and the
candidate below a common cgroup parent on the 10 evaluation cores while the
test orchestration stays on the 2 management cores,
applies and verifies every repository migration before runner launch, and
inspects the live non-root/read-only/capability/limit/internal-network settings.
It then runs the exact authenticated official preflight, completes a real
simulator lifecycle, authenticates the run manifest and final marker, and
starts the pristine scorer with `network_mode: none`. Runtime JWT, client,
password, proxy, WireGuard, PostgreSQL, and Redis secrets are generated anew
when the default throwaway fixture is used and never come from a production
vault.

Run the same lifecycle against the exact host leaves that production will bind:

```bash
SMOKE_CONFIG_LOCAL_DIR=/srv/warp/config/local \
SMOKE_VAULT_LOCAL_DIR=/srv/warp/vault/local \
  ./competition/container/smoke-test.sh \
  urnetwork/sim-latency-evaluator-base:dev
```

Both variables are mandatory in direct mode. The test canonicalizes the paths,
requires the exact `config/local` and `vault/local` suffixes, authenticates both
directory manifests before execution and after every Compose stage, inspects
the live read-only leaf binds, and fails if either source changes. Any value in
those two leaves must be treated as readable by hostile candidate code and
therefore must be evaluator-safe local material, never a production secret.
The per-stage PostgreSQL and Redis passwords remain independently randomized.

The plumbing fixture deliberately uses 8 providers, 2 clients, 30 arrivals per
minute, the simulator's 3-second test and 2-second announce timeouts, and the
seeded multi-minute provider uptime schedule. The old fixture compressed every
provider to 10-second churn, forced both platform timeouts to 10 milliseconds,
and drove 120 arrivals per minute; that tiny-fleet overload made the unchanged
97% scorer floor intermittently fail. A source regression test freezes the
corrected profile. Two consecutive live runs on 2026-08-17 shared config
SHA-256 `857b6e1b03b694987a31af21a1c2b9b6dc8dac6b487ac8fcd0dba051ec5e5045`
and each produced exactly 910 measured rows, zero failures, a non-empty
matchmaking pool, and a passing baseline/candidate score. Cleanup left no
smoke container or network behind.

Before accepting a submission builder, also run the adversarial build-isolation
gate:

```bash
./competition/container/test-build-isolation.sh \
  --allow-local-base \
  --base-image urnetwork/sim-latency-evaluator-base:dev \
  --policy competition/container/policy.example.json
```

It injects candidate package initialization that attempts to overwrite the
trusted entrypoint, runner, validator, database migrator, source lock, and
`go.mod`. Vet/tests execute as UID/GID 65532 in a discarded stage; compilation
uses a separate fresh stage; and the final image copies only the resulting
binary into the authenticated patched-source stage. The gate requires every
protected file to remain byte-identical to the base and removes its exact test
image afterward.

Run the hostile-resource cleanup gate as a separate mandatory qualification:

```bash
./competition/container/test-resource-bomb-cleanup.sh
./competition/container/test-resource-bomb-cleanup.sh --production-memory-limit
```

It starts simultaneous CPU and memory bombs on the evaluation CPU set. The CPU
bomb must be observed on every evaluation CPU and remain live until the
management-only cleanup path kills it; observing any execution outside that set
is fatal. The default fast mode uses a 128 MiB no-swap OOM probe. Host
qualification must additionally run `--production-memory-limit`, which derives
the actual 72 GiB runner ceiling from `resource-boundary.sh`. Both modes require
exit 137 and `OOMKilled=true`; label-resolved cleanup must remove every container
and network within 10 seconds. The 2026-08-17 production-pressure run saturated
the exact 10-core set, reached the 72 GiB limit, completed management-only
cleanup in 1,019 ms, and left zero residual objects. This proves the local
mechanism, not the still-missing authoritative-host qualification.

The latest 2026-08-17 local production-evaluator integration used the full
1,800-provider/200-client/80-arrival, three-minute profile against the rebuilt
10-evaluation/2-management boundary and corrected identity/Connect code, with
base image
`sha256:22547cd4e19214b1f4688f0eb969d57c70b3f7dc47e02ad9647c4faf7b16a296`
and candidate image
`sha256:09b219eb9a5a321f6552ebbbb201295bfdc8bae69c8cf86588ae486b370c189d`.
Before candidate execution, the evaluator independently re-authenticated the
strict builder record, base/patch/policy/builder/image-key tuple, final OCI
labels, image id, and runtime identity. It ran separate baseline and candidate
stacks, authenticated database version 580 of 580, sampled live cgroup
counters, ran the networkless pristine scorer, replayed all 54 evidence hashes
and all six published artifact hashes, verified the completion chain, reported
all 15 security booleans true, and left no job containers, networks, or mounted
work tree.

The scorer returned `eval_error: null`, `placeable: true`, and all G1-G6 gates
passed. The single baseline and comment-only candidate raw scores were
27,174.12 ms and 27,051.04 ms; the baseline's paired provisional
significant-better line was 19,957.81 ms. Success was 99.609% and 99.430%;
matchmaking pool p05 was 1,165 in both runs with no empty sample. Live inspection
showed the runner on exactly the 10 evaluation CPUs and the evaluator on the
management set. Peak stack RSS was 14.31/14.54 GB; neither run was OOM-killed,
hard-killed, or outside its limit. These are development artifacts and one
directional pair, not a stable baseline or published season digests. They also
predate the direct-local-mount policy and cannot qualify that new boundary.

At 2026-08-17 19:56 UTC the root-owned CPU and IRQ systemd units were installed,
enabled, active, and passed their live checks. Exactly 12 physical CPUs are
online, SMT is off, the governor is `performance`, turbo is disabled,
`vm.overcommit_memory=1`, and the live split is 10+2. All 49 discovered movable
device IRQs are pinned to management CPUs `20,22`; the complete affinity digest
is `487d80c7b6b6446e323c5bf3096a1d05b6bc16262627f94d1fc703cb54637cbf`.
A reboot proof remains open. The evaluator and promotion path are
user-namespace aware and fail closed, but the current Docker daemon still uses
an identity mapping and reports its pre-control 24-CPU view. Installing the
hardened daemon config and restarting Docker must occur in a controlled window;
host-firewall qualification also remains open.
