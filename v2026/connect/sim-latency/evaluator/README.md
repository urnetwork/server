# sim-latency competition service

`server/api` serves both `connect/api/bringyour.yml` and
`sn/api/competition.yml`. The competition routes use independent opaque bearer
tokens; raw tokens are never stored. A Redis list dispatches immediate FIFO
wake-ups, while PostgreSQL remains authoritative for ordering and recovery. A
global PostgreSQL slot enforces one evaluation at a time across all workers;
`FOR UPDATE SKIP LOCKED`, leases, and append-only job events provide durable
claim and failover. The unique cache key binds the round UUID to exact canonical
patch bytes, and an ACL records every principal that submitted that cached
identity.

Each seven-day epoch accepts an unbounded number of unique submissions. The
Apex adapter collects the fixed $20 USD fee exactly once before forwarding an
admission; transport retries of that durable adapter record are not recharged.
Evaluation begins immediately, continues past `closes_at` until all accepted
jobs are terminal, and publishes no result or hidden workload until the ranked
significant candidates pass the operator-controlled honesty-review gate (or are
all rejected) and the epoch is finalized.

The API remains fail-closed until `config/competition.yml` and
`vault/competition.yml` pass all frozen-policy checks and the single
authoritative worker host heartbeats with the configured hardware/image
identity. It must attest 12 exposed physical CPUs split into 10 evaluation and
2 management CPUs, at
least 24 GiB reserved outside the active job ceilings, fixed
SMT/governor/turbo/NUMA/IRQ policy, cgroup v2 containment
of runner plus PostgreSQL/Redis, default-deny job networking, offline cache,
template database and Redis reset, no production secrets, immutable accounting
and resource reports, cleanup, artifact storage, and same-round re-baseline.
The host row must also match the frozen host-qualification SHA-256 (including
the exact host image, BIOS/microcode, kernel, tuning, sysctls, and service
versions), not merely advertise the same CPU model.
See the example resources beside this file.

The current literal editable-file review is recorded in
[`PATCH-SURFACE.md`](PATCH-SURFACE.md). Freeze its policy hash with the final
evaluator image and round policy before opening epoch 1.

## Deployment sequence

1. Apply the server migrations. They create encrypted round storage, the FIFO
   queue/ACL/event log, singleton worker slot, evaluator heartbeat table, and
   append-only ordered candidate-review records. Database triggers prevent a
   skipped rank, an unresolved no-winner finalization, or publication of a
   winner without that exact job's approved review.
2. Build the API and `cli/competitionworker` from one clean release. Build the
   trusted evaluator base with
   `connect/sim-latency/evaluator/container/build-base.sh`; its
   source-lock records the clean server/connect/proxy/sdk/glog/goidenticons/
   userwireguard/sn commits and it freezes the toolchain/module cache. The API
   Dockerfile pins the Docker Official Ubuntu index rather than a floating base
   tag. Both release targets preserve BuildKit provenance, SBOM, and the pushed
   repository digest in their respective `build/image-metadata.json`; the
   worker image is `FROM scratch`. Place the exact published worker/evaluator
   repository digest (`containerimage.digest`), never a tag, in ordinary config
   and the signed round policy. Retain the API digest beside the deployment
   manifest and calibration evidence as a separate identity.
3. Install the independently reviewed simulator, Docker/Compose evaluator, and
   self-check executables at the absolute paths in config and pin each file
   SHA-256. The API re-hashes the simulator before every round generation. The
   evaluator accepts canonical patch bytes, not miner images or URLs, and
   derives one image per `(base, patch, policy, builder)` identity with the
   fixed Dockerfile and authenticated cache reuse. Each attempt copies the
   eight locked repositories from the authenticated base image into fresh
   temporary baseline/candidate checkouts, applies the patch only to the
   evaluated server surface there, and
   mounts the selected checkout read-only at `/workspace`; the main runner
   checkout is neither inspected nor changed. Mount no production
   credentials or Docker socket into a candidate container.
4. Provision the authoritative evaluator machine and its root-owned Docker
   parent slice, user-namespace/rootless daemon, default-deny firewall,
   artifact/WORM, and re-baseline controls described in
   `EVALUATOR-PROTOCOL.md`. Docker creates the per-container cgroups; the
   worker verifies their common parent and immutable resource/accounting
   evidence instead of writing cgroupfs itself.
   Install `host-self-check.sh` as the configured self-check and install a
   root-owned, non-group/world-writable
   `/etc/urnetwork/competition-host.json` derived from
   `host-config.example.json`. The script hashes live kernel/microcode, CPU,
   SMT/governor/turbo, NUMA, IRQ, cgroup v2, Docker version/security mode,
   digest-pinned PostgreSQL/Redis image ids, and sysctl facts. Its frozen
   runtime marker set uses `host-containment.example.json` for the cleanup
   record and must be derived from a successful live evaluator qualification;
   it binds the common service
   cgroup, 10+2 CPU split, management-memory reserve, resource limits, cleanup,
   service images, 32 GiB artifact quota, successful CPU-/memory-bomb cleanup,
   the internal/no-published-port candidate network, networkless scorer, exact
   direct read-only local-leaf mounts and their frozen content hashes, plus the
   authenticated completion/evidence hashes.
   The host route table is not used as a proxy for candidate isolation.
   Promote those artifacts only with `promote-host-containment.sh`; it replays
   every evidence hash and declared size, authenticates the worker/completion
   chain, checks every baseline/candidate runner's exact local mounts and the
   authenticated local-leaf digest record, combines
   the production resource-bomb result, and atomically recreates the transient
   template-database, Redis-reset, cleanup, and immutable-report markers plus
   their hashes in the root-owned host config. This restores the complete
   readiness proof after reboot. A failed promotion leaves no new marker set.
   Run `install-authoritative-host-controls.sh --install` before Docker. It
   installs the CPU-control executable, its resource-boundary dependency, the
   exact-device-IRQ executable, and both root-owned systemd units. The first
   fail-closed unit requires exactly 12 online physical CPUs with SMT off, the
   `performance` governor, turbo off, `vm.overcommit_memory=1`, and the derived
   10+2 split. It normalizes the kernel's transient numeric SMT state and
   retries the `off` transition during early boot. The dependent unit routes
   every movable device IRQ to the two management CPUs and verifies the
   resulting affinity. Both units are required dependencies of Docker, so a
   failed host control prevents the daemon from admitting evaluations. The
   qualification digest binds the stable IRQ policy while every heartbeat
   revalidates all currently discovered IRQs; transient IRQ numbers are kept
   only as diagnostic evidence. Install a reviewed
   `docker-daemon.example.json` as `/etc/docker/daemon.json` during a controlled
   restart; the host check authenticates its bytes, hardening semantics, and a
   live non-identity container UID/GID map. The firewall remains a separate
   deployment gate.
   Idle host checks do not look for native PostgreSQL/Redis processes because
   those services exist only inside fresh per-job Compose projects.
5. Start one worker for the epoch on the host with a stable `--worker_id`. The DB singleton
   means only one worker can own a job; an expired owner is resumed under the
   same job/cache identity after the worker recovers, within the submission's
   single three-hour `score_timeout_seconds` execution budget across all attempts and
   retry backoff. Each accepted job is dispatched immediately. The Redis list
   is rebuildable from PostgreSQL, so a flush or interrupted post-commit push
   cannot lose or reorder durable work. After admission closes and the FIFO
   drains, this worker seals the epoch for honesty review and exits
   successfully. If no significant candidate exists, it finalizes no winner.
   Otherwise the external agent harness uses `sim-latency epoch-review` to
   inspect the exact patch and score, reject dishonest candidates in rank order,
   and approve the first honest candidate. The promotion loop can merge only
   that database-approved patch, advance the source ledger, generate the next
   round, and then start a fresh worker. Pin the worker process to
   the two management CPUs. The host self-check independently counts the 12
   online physical CPUs and requires its inherited worker affinity to equal the
   management set, so management pinning cannot masquerade as a two-CPU host.
6. Confirm authenticated `/competition/readyz` returns every check `true`.
   Generate a round only after this point.

The API process can continue serving the ordinary BringYour routes when the
competition is unconfigured; competition `/healthz` stays live while every
other competition operation returns a typed, non-secret 503.

## Hidden-seed lifecycle

Round creation draws 256 bits from the OS CSPRNG. The seed is AES-256-GCM
encrypted at rest with round/base/competition-bound associated data. The API
derives the simulator's positive 63-bit seed as the first eight bytes of
`SHA256("urnetwork-sim-latency-generator-v1\\0" || seed)`, invokes the pinned
simulator with the frozen scale, and stores `providers.yml` in the shared
immutable artifact root. The public seed commitment is

```text
SHA256("urnetwork-sim-latency-round-v1\\0" || round_uuid_bytes || seed_bytes)
```

The exact providers-file SHA-256 is returned beside that commitment before
`reveal_at`. The worker authenticates the stored path and bytes before passing
them to the evaluator. Reveal requires both the configured reveal time and the
atomic post-review epoch finalization. The API then re-verifies the seed commitment and
returns the seed plus a public immutable download URL; that endpoint re-hashes
the file before serving it and returns the digest in `ETag` and
`X-Content-SHA256`. Before finalization, non-operator polling exposes only
processing state: terminal jobs appear as `completed`, with score and failure
results omitted.

Every evaluator result must retain an authenticated `baseline.json` created
from the same round workload and frozen replicate policy, in addition to the
candidate score, accounting, resources, and completion marker. A host is
eligible for submissions only when its fresh self-check names that exact round
in `rebaseline_round_id`; round generation intentionally checks the authoritative-host
containment boundary before this round-scoped attestation can exist.

## Verification

```bash
go test -race ./controller ./model ./api
go test . -run TestApplyDbMigrations -count=1
go vet ./controller ./model ./api ./cli/competitionworker
(cd connect/sim-latency && ./tests.sh)
./connect/sim-latency/evaluator/container/test-build-isolation.sh \
  --allow-local-base \
  --base-image urnetwork/sim-latency-evaluator-base:dev \
  --policy connect/sim-latency/evaluator/container/policy.example.json
./connect/sim-latency/evaluator/container/smoke-test.sh \
  urnetwork/sim-latency-evaluator-base:dev
./connect/sim-latency/evaluator/container/test-resource-bomb-cleanup.sh \
  --production-memory-limit
./connect/sim-latency/evaluator/test-promote-host-containment.sh
```

Production readiness is evidence, not a config flag: missing/old host
heartbeats, a policy/digest mismatch, a failed re-baseline, a full queue, or any
containment/reset/storage check makes readiness false. A host cannot manufacture
qualification with configuration alone; it must produce the live authenticated evidence.

During an active evaluation the worker also watches the trusted evaluator's
atomically replaced `evaluation-progress.json`. The internal competition
Grafana dashboard plots authenticated replicate-level TTFB p50/p95 and
throughput p50/p95 values with provisional one-sided significance coloring.
The progress record is retained with the attempt, but it is not served by the
public competition API; public results still appear only after post-review
epoch finalization, and the completed sealed score remains authoritative.

The current public Apex sandbox/spec contract does not directly expose an
external scoring-service adapter and its standard resource ceilings are below
this evaluator's host boundary. The precise staging decision that must be made
with Macrocosmos is recorded in
[`APEX-INTEGRATION-GAP.md`](APEX-INTEGRATION-GAP.md); do not claim Apex registry
or staging completion until one of those integration paths is accepted.
