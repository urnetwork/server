# Pinned evaluator protocol, schema 1

The worker executes no shell and passes a four-entry non-secret environment
(`PATH`, `LANG`, `LC_ALL`, `TZ`). Both command files are regular executables
whose bytes are re-hashed before every run. The self-check is invoked as
`SELF_CHECK --json`; the evaluator is invoked as

```text
EVALUATOR --request /absolute/worker-request.json --result /absolute/worker-result.json
```

The request and canonical patch are exclusive-create, mode 0400 files under a
new `job_uuid/attempt-NN` directory. The request binds job/round/attempt,
competition, base SHA, image digest, scorer version, hidden round seed, exact
providers path/SHA-256, patch path/SHA-256, artifact directory, and both frozen
policies. The evaluator must never copy the active seed into logs or public
diagnostics.

The pinned evaluator uses the fixed files in
`connect/sim-latency/evaluator/container`. The
season publishes one digest-pinned evaluator base, and the evaluator derives
one candidate image for each `(base image digest, canonical patch SHA-256,
patch-policy SHA-256, trusted Dockerfile SHA-256)` identity. It never accepts a
submission Dockerfile, repository URL, build command, dependency change, or
entrypoint.

Before any candidate process runs, the pinned evaluator must:

- authenticate the base image digest, canonical patch, policy, and providers
  file, then build a candidate with the fixed `Dockerfile.submission`; every
  candidate `RUN` has `network=none`, `GOPROXY=off`, and `GOSUMDB=off`, and uses
  only the base image's read-only warmed module cache;
- mount the complete untrusted work/evidence tree on a root-created 32 GiB
  tmpfs with `nosuid,nodev,noexec`. Candidate output and intermediate evidence
  share this aggregate ENOSPC boundary; only the bounded, sanitized tree is
  copied to durable retention after candidate execution has ended;
- independently verify the resulting image id, OCI identity labels,
  `/opt/urnetwork/image-identity.json`, deterministic clean candidate commit,
  simulator build metadata, and simulator file SHA-256;
- execute candidate vet/compile-only tests as UID/GID 65532 in a discarded
  networkless build stage, compile the binary from the authenticated source in
  a separate fresh stage, and copy only that binary into the final patched
  source image. Candidate initialization must never run as root or in an
  inherited final-image layer. BuildKit must run below its own cgroup parent on
  the 10 evaluation cores, with a 12 GiB no-swap ceiling, a 600-second
  TERM/KILL deadline, and bounded/drained output capture;
- start a unique Compose project below a root-owned `cgroup_parent`, with fresh
  tmpfs PostgreSQL/Redis, the frozen CPU set and memory/swap/PID/fd limits,
  read-only roots, dropped capabilities, `no-new-privileges`, no published
  ports, an internal-only bridge, and throwaway job credentials. Bind the
  frozen host `config/local` and `vault/local` leaf directories directly,
  separately, and read-only; authenticate their policy-pinned sorted-file
  digests before and after each run; never bind their parents or any `main`,
  `all`, or `site` tree;
- run the pristine baseline image and candidate image in different Compose
  projects and artifact directories. Both use the authenticated per-round
  providers file and identical replicate policy. The candidate receives no
  baseline artifact, scorer output, production credential, Docker socket, or
  host path beyond its explicit read-only local-leaf/input and writable
  output;
- construct `baseline.json` and score in later pristine-base scorer projects
  with `network_mode: none`, so candidate code never controls the scorer or
  baseline evidence;
- sample the live runner cgroup while it exists, because a short-lived
  one-shot container's leaf cgroup may disappear immediately after exit;
  authenticate the sample against the resolved container cgroup id;
- TERM then KILL the fully resolved Compose project on deadline, reap every
  container, snapshot container/cgroup counters, reset identities, image ids,
  exit status, health state, and escape/OOM flags, and durably sync all files
  before writing the completion marker last.

Docker owns per-container cgroup creation; no evaluator code writes cgroupfs.
The common parent and dedicated qualified host provide the aggregate job
boundary. The evaluator still verifies the live cgroup hierarchy and limits;
an OCI declaration alone is not an attestation. Host qualification also freezes
the authoritative host's SMT/governor/turbo/NUMA/IRQ/kernel/microcode and
user-namespace or rootless-Docker policy. The 12-core host exposes only 10
physical cores to builds and evaluation; the worker, Docker control plane, and
cleanup execute on the other 2. The active runner/PostgreSQL/Redis ceilings
total 96 GiB and must leave at least 24 GiB outside the active stack.

`worker-result.json` is strict JSON:

```json
{
  "schema": 1,
  "job_id": "uuid",
  "score": {
    "score_schema": 1,
    "raw_score": 100.0,
    "normalized_score": 100.0,
    "placeable": true,
    "takeover_eligible": false,
    "gates": {"G1": {"passed": true, "details": {}}},
    "diagnostics": {}
  },
  "eval_error": null,
  "security": {
    "template_database_reset": true,
    "redis_reset": true,
    "cgroup_contained": true,
    "resource_limits": true,
    "management_cpu_reserved": true,
    "management_memory_reserved": true,
    "default_deny_network": true,
    "offline_build": true,
    "offline_build_resource_limits": true,
    "no_production_secrets": true,
    "structural_patch_check": true,
    "accounting_complete": true,
    "resource_report_complete": true,
    "cleanup_complete": true,
    "immutable_reports": true,
    "cgroup_id": "urnetwork-evaluation.slice/compose-project",
    "template_database_id": "sha256-or-generation",
    "redis_generation_id": "random-generation"
  },
  "artifacts": [
    {"path": "accounting.json", "sha256": "64hex", "bytes": 123},
    {"path": "baseline.json", "sha256": "64hex", "bytes": 234},
    {"path": "resources.json", "sha256": "64hex", "bytes": 456},
    {"path": "score.json", "sha256": "64hex", "bytes": 789},
    {"path": "evaluation.complete.json", "sha256": "64hex", "bytes": 100}
  ]
}
```

Exactly one of `score` and `eval_error` is non-null. A submission error is
terminal and non-retriable. A `candidate_build_failed` result occurs before
any workload process starts, so it must authenticate `submission-error.json`,
the completion marker, the offline/default-deny build boundary, cleanup, and
immutable evidence; reset/cgroup/accounting booleans remain false rather than
pretending a measurement ran. Every other result requires the full runtime
security set and ordinary accounting/baseline/resources/score artifacts. An
infrastructure error is retried, up to the frozen attempt limit, under the same
job and patch cache key. Every required security
boolean, nonempty reset/cgroup identity, mandatory artifact, declared size, and
SHA-256 must authenticate or the worker replaces the result with a typed
infrastructure failure. It hashes its own request, patch, stderr, and result,
seals the attempt tree read-only, stores the manifest once behind a DB
immutability trigger, and retains every attempt through the season.

The self-check result uses `HostSelfCheck` from `types.go`. The trusted
self-check executable derives those booleans from root-owned provisioning state
and live kernel/cgroup/network/storage probes; it must not copy a caller-written
attestation. `qualification_sha256` must equal the season-frozen digest of the
host image, BIOS/microcode, kernel, SMT, governor/turbo, NUMA/affinity, IRQ,
cgroup/sysctl, and backing-service facts. `kernel_release`,
`microcode_revision`, and every named `checks` entry are mandatory.
`rebaseline_passed` binds a recent same-round re-baseline on that specific host
and image. A host may heartbeat as containment-eligible before a round exists;
round generation ignores this one round-scoped field, while queue admission
and the worker both require it to name the active round. One fresh, eligible
row for the season's authoritative host is required.

Host-online topology and worker affinity are separate facts. The self-check
counts online CPUs from `lscpu`, while `/proc/self/status` must show that the
worker inherited exactly the two management CPUs. Using affinity-sensitive
`nproc` as the host count is forbidden because it would report two CPUs once
the worker is correctly isolated.

`install-authoritative-host-controls.sh` installs the reversible CPU controls,
their resource-boundary dependency, exact device-IRQ placement, and two
root-owned systemd units. The first runs before Docker and fails unless SMT is
off, exactly 12 physical CPUs are online, every online CPU uses the performance
governor, turbo is off, Redis-safe overcommit is enabled, and the live topology
derives the frozen 10-evaluation/2-management split. Early boot may expose a
transient numeric SMT state; the control normalizes that state and retries the
`off` transition before failing closed. The dependent IRQ unit then fails
unless every discovered movable device IRQ is routed exactly to the management
set. The qualification binds a stable digest of the minimum IRQ and CPU-set
policy, while the live check separately verifies every currently numbered IRQ
and retains the number-sensitive affinity digest only as diagnostics. Both
units are required Docker dependencies. A reboot must prove both units reapply
before Docker. Firewall
qualification remains separate. Docker namespace qualification uses a live
container `/proc/*/{uid,gid}_map` probe and rejects an identity mapping even if
daemon configuration merely claims user-namespace support.

The repository implementation is `host-self-check.sh`; its root-owned input
schema is `host-config.example.json`; the authenticated last-live-containment
evidence schema is `host-containment.example.json`. A failed probe still emits
strict JSON and exits nonzero, allowing the worker to register a negative heartbeat. The live
qualification digest deliberately excludes the unique host id so a rebuilt
authoritative host can retain the frozen image identity while its database row
still has a stable host id.

`connect/sim-latency/evaluator/container/smoke-test.sh` is the development
integration gate. It
must pass against live Docker state, including authenticated image cache reuse,
the common cgroup parent, the 10-core evaluation CPU set, management-only
orchestration affinity, hard resource
limits, internal run network, fresh stores, cleanup, and a networkless pristine
scorer, before an evaluator build is eligible for authoritative-host
qualification. A passing local smoke is not a host qualification result.
Its fixed short-run profile uses 8 providers, 2 clients, 30 arrivals per minute,
the ordinary 3-second/2-second simulator test and announce timeouts, and no
synthetic 10-second fleet churn. This prevents the integration gate from
turning into an overloaded tiny-fleet performance lottery while retaining
hundreds of real rows and a mandatory in-window `FindProviders2` sample. A
regression test freezes those settings; two consecutive pre-reserve live runs
produced the same config digest and exactly 910/910 successful measured
requests each. A later rebuilt-boundary smoke passed the 10+2 split, exact
10-CPU preflight, fresh stores/migrations, real lifecycle, scorer, and cleanup.

`connect/sim-latency/evaluator/container/test-resource-bomb-cleanup.sh` is the
hostile-resource
gate. It runs simultaneous CPU and memory bombs on the evaluation set, observes
the CPU bomb on every evaluation core and none outside it, requires the memory
bomb to exit 137 with `OOMKilled=true` at a no-swap limit, then performs
label-resolved cleanup from management-only affinity. The fast 128 MiB mode is
for iteration. Qualification must use `--production-memory-limit`; the local
72 GiB production-pressure run removed every container and network in 1,019 ms
and left zero residual objects. The authoritative host must reproduce that mode
before it can attest `resource_bomb_cleanup_verified`.

The root-owned runtime marker set promoted during host qualification also
binds the successful evaluator worker-result, evidence-manifest, and completion
hashes. Its
network and mount booleans must prove an internal candidate network with no
published ports, a networkless scorer, exactly the two read-only local leaves,
and no production credentials. The host's own default route is intentionally
not treated as evidence about a container network namespace.
`promote-host-containment.sh` is the only supported promotion path. It
atomically recreates all four transient readiness markers and updates their
root-owned host-config hashes, including when `/run/urnetwork` disappeared in
a reboot. Its deterministic fixture authenticates a valid chain, simulates
that reboot boundary, then constructs a
hash-consistent adversarial chain containing a parent `/runtime/config` mount
and proves that no marker is written.

The stronger `connect/sim-latency/evaluator/container/evaluator.sh` gate
consumes the exact
worker request, derives the candidate, runs separate baseline/candidate and
scorer projects, emits the strict result above, and produces a hash manifest
for every retained evidence file. Its local end-to-end pass is required before
host qualification, but likewise cannot replace the official calibration campaign.
The latest rebuilt-boundary full-scale local execution rechecked the strict builder record,
base/patch/policy/builder/image-key tuple, image id, OCI labels, and runtime
identity before candidate launch; independently replayed 54/54 evidence files
and 6/6 published artifacts; reported all 15 security booleans true; and cleaned all
job resources. Its 1,800-provider/200-client/80-arrival, three-minute R=1
comment-only candidate returned `eval_error: null`, was placeable, and passed
all G1-G6 gates. Live inspection and retained evidence authenticated the 10
evaluation CPUs, 2 management cores, 72 GiB runner/no-swap limit, >=24 GiB
reserve, and zero residual objects. This closes the local protocol replay but
does not replace official baseline/noise/reference calibration on the
authoritative host.
`connect/sim-latency/evaluator/container/test-build-isolation.sh` is the
corresponding malicious
initialization gate: it attempts to corrupt six trusted base files during the
compile-only test process and requires every final-image digest to remain
unchanged.
