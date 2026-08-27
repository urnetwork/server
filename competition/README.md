# sim-latency competition service

`server/api` serves both `connect/api/bringyour.yml` and
`sn/api/competition.yml`. The competition routes use independent opaque bearer
tokens; raw tokens are never stored. A global PostgreSQL slot enforces one
evaluation at a time across all workers, while `FOR UPDATE SKIP LOCKED`, leases,
and append-only job events provide FIFO claim and failover. The unique cache key
binds the round UUID to exact canonical patch bytes, and an ACL records every
principal that submitted that cached identity.

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
[`PATCH-SURFACE.md`](PATCH-SURFACE.md). Its policy hash remains a pre-freeze
identity until the pending main-branch fixes are merged and pushed.

## Deployment sequence

1. Apply the server migrations. They create encrypted round storage, the FIFO
   queue/ACL/event log, singleton worker slot, and evaluator heartbeat table.
2. Build the API and `cli/competitionworker` from one clean release. Build the
   trusted evaluator base with `competition/container/build-base.sh`; its
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
   fixed Dockerfile and authenticated cache reuse. Mount no production
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
5. Start one worker on the host with a stable `--worker_id`. The DB singleton
   means only one worker can own a job; an expired owner is resumed under the
   same job/cache identity after the worker recovers. Pin the worker process to
   the two management CPUs. The host self-check independently counts the 12
   online physical CPUs and requires its inherited worker affinity to equal the
   management set, so management pinning cannot masquerade as a two-CPU host.
6. Confirm authenticated `/competition/readyz` returns every check `true`.
   Generate a round only after this point.

The API process can continue serving the ordinary BringYour routes when the
competition is unconfigured; competition `/healthz` stays live while every
other competition operation returns a typed, non-secret 503.

## Same-round re-baseline

Round creation deliberately does not claim a baseline that cannot exist yet.
After an operator generates a round, queue admission remains closed until the
authoritative host completes this sequence:

1. Stop the ordinary competition worker and acquire the root-owned host
   single-job operational lock. Confirm that no evaluation container or queued
   job is running.
2. Run `cli/competitionrebaseline` on the two management CPUs with the generated
   round UUID, the no-op reference patch, and its canonical SHA-256 through the
   mandatory `--patch_sha256` argument. The command loads the
   encrypted round record through the trusted store, checks the current-round
   policy and host containment identity, and runs the ordinary evaluator. It
   succeeds only when the pristine baseline, the no-op candidate, every score
   gate, and the complete evaluator security/artifact chain pass. Its output
   contains hashes and paths only—never the hidden seed or API credentials.
3. As root, run `competition/promote-round-rebaseline.sh` with that output, the
   root-owned host config, the frozen production resource-bomb report, and the
   pinned self-check executable. The promotion replays
   `promote-host-containment.sh`, atomically installs the round-bound marker,
   updates its expected hash in the host config, and requires a fresh
   management-CPU self-check naming the exact round.
4. Restart the worker. Its heartbeat and queue claim both independently require
   `rebaseline_round_id` to equal the active job's round. Only then may
   `/competition/score` admit submissions.

Failed and non-placeable re-baseline attempts remain retained evidence; they
are never promoted and cannot make readiness true. The operational lock is
mandatory because the re-baseline is intentionally outside the submission FIFO
until it has established the prerequisite that allows that FIFO to open.

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
them to the evaluator. At reveal, the API re-verifies the seed commitment and
returns the seed plus a public immutable download URL; that endpoint re-hashes
the file before serving it and returns the digest in `ETag` and
`X-Content-SHA256`. Active-round score polling strips raw scores, exact gate
observations, and diagnostics for non-operator callers.

Every evaluator result must retain an authenticated `baseline.json` created
from the same round workload and frozen replicate policy, in addition to the
candidate score, accounting, resources, and completion marker. A host is
eligible for submissions only when its fresh self-check names that exact round
in `rebaseline_round_id`; round generation intentionally checks the authoritative-host
containment boundary before this round-scoped attestation can exist.

## Verification

```bash
go test -race ./competition ./api
go test . -run TestApplyDbMigrations -count=1
go vet ./competition ./api ./cli/competitionworker
cd ../sn && python3 -m unittest discover -s competition/tests -v
cd ../server && ./competition/container/test-build-isolation.sh \
  --allow-local-base \
  --base-image urnetwork/sim-latency-evaluator-base:dev \
  --policy competition/container/policy.example.json
cd ../server && ./competition/container/smoke-test.sh \
  urnetwork/sim-latency-evaluator-base:dev
cd ../server && ./competition/container/test-resource-bomb-cleanup.sh \
  --production-memory-limit
cd ../server && ./competition/test-promote-host-containment.sh
```

Production readiness is evidence, not a config flag: missing/old host
heartbeats, a policy/digest mismatch, a failed re-baseline, a full queue, or any
containment/reset/storage check makes readiness false. A host cannot manufacture
qualification with configuration alone; it must produce the live authenticated evidence.

The current public Apex sandbox/spec contract does not directly expose an
external scoring-service adapter and its standard resource ceilings are below
this evaluator's host boundary. The precise staging decision that must be made
with Macrocosmos is recorded in
[`APEX-INTEGRATION-GAP.md`](APEX-INTEGRATION-GAP.md); do not claim Apex registry
or staging completion until one of those integration paths is accepted.
