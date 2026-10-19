# V2 staging deployment

This is a control-plane release, not a new evaluated source checkpoint. Preserve
the ten-repository `sim-latency-staging` references and frozen config ledger.
The replacement staging scope uses evaluator image
`sha256:27bda2a8877f3d1841d5868c198bcc1441fc2363c32ae4f3b5cc3cd817e1e5b4`.
It adds only the corrected trusted entrypoint to the original evaluator image
`sha256:8d1206bdc25ce758ad537a0f959e7b16dc2c7c5ffaca4c15636d9f364e0fea08`;
the frozen source tree and simulator/scorer bytes are unchanged.
The evaluated server remains `661759b163c9e4c473d63403734fdf4486bc25c5`.

## Operator deployment

Deploy only after the release changes have been committed, merged into `main`,
pulled, and pushed. Record the actual deployed release identifiers in the handoff;
do not substitute the evaluator digest for an API or worker image identity.

1. Apply additive migration **851** from the pushed server release through the
   normal main migration environment. Migrations 842–850 remain unchanged. This
   extends the existing exact-round guard to the explicit runtime-blob capability;
   it does not loosen timing, staging, source, fee, or qualification checks.
2. Securely provision the reviewed `vault/main/competition.yml` to the main API
   hosts, preserving the seed key and existing credentials. It contains scoped
   scheduler, reviewer, settlement, and source-promoter credentials. Config-updater
   does **not** provision vault. Never publish this file or delivery tokens in
   public config, logs, runtime envelopes, or Macrocosmos documentation.
3. Build and roll the new main API across every serving deployment group. The new
   API must support `competition.reward.v2.runtime-blob.v1`. A load-balanced
   success is not proof that every replica has finished rolling out.
4. Deploy config-updater from the new config commit. The active policy is
   **`config/main/competition.yml`**, not the disabled historical
   `competition-v2-staging.prepared.yml`. Use the normal explicit rollout without
   an uncontrolled restart of every main service. The active policy selects six
   48-hour rounds, zero fee/margin, nine replicates, and shadow settlement.
5. Report completion and the actual main API image digest. The host preparation
   inventory pins this real release identity; it cannot be filled from the
   evaluator image or a guessed build version.

The old and new runtime capabilities are intentionally distinct. Old API
processes must refuse the new policy rather than participate with incomplete
manifest validation. Do not claim staging readiness during this transition.

## Evaluator-host work

The trusted runner uses `main` config/vault. Candidate containers receive only
the exact `config/local` and `vault/local` leaves read-only, never `all` or `main`.
The local public MMDB resources are independently versioned under `config/local`.

On sille, install the tested, SHA-pinned main control release containing
`competitionprepare`, `competitionqualify`, `competitionworker`, and the
sim-latency supervisor. Build the preparer stripped (`-ldflags='-s -w'` plus
normal release flags) to meet the supervisor's existing 64 MiB command limit.
Do not use the old evaluator image's worker as the new control-plane worker.

Provision private root-owned inventories at:

- `/etc/urnetwork/competition-preparation.json`;
- `/etc/urnetwork/competition-qualification.json`;
- a separate private source-promoter token file; and
- the durable supervisor service, state directory, and exact command hashes.

The root service PATH must include the trusted Go installation as well as Git,
SSH, Docker and the normal system tools. Bootstrap verifies frozen `go.mod`
graphs by invoking `go mod edit -json`; the restricted default sudo PATH is
insufficient. Verify this command under the actual service environment before
bootstrap, not only from the operator's interactive shell.

The qualifier additionally requires the real current-boot 72 GiB adversarial
cleanup report, pinned promotion script, exact host configuration and genuine
API receipt. Qualification paths and their ancestors must be root-owned,
non-writable by group/other, and symlink-free. Run the trusted host manager as
root with reserved management CPUs; candidate processes retain containment and
unprivileged isolation.

The frozen runtime evaluator/self-check release is installed at
`/usr/local/libexec/urnetwork/competition-v2stage-8d1206bd`. The separately pinned
promotion helper is at
`/usr/local/libexec/urnetwork/competition-promoter-89d6e660/promote-host-containment.sh`.
Installed files and a measured host identity are not a passed qualification.

## Bootstrap and acceptance

Only after the operator rollout, bootstrap from an explicit pushed config-main
commit whose ledger bytes match the frozen SHA-256
`085dbd51908fe08d57ff7fb397aac7e85c2918511c25f89dd84fa3e363b5ad5d`.
For the replacement staging scope that commit is
`14229f9ef5a81aca3d5992a1d5ffdc8fe793b267`;
the original snapshot was `587f764beaea20332493bd7e349d415ecc39edb5`.
The published config ref must match the requested commit: do not move `main`
backwards to the historical snapshot or alter the frozen ledger to accommodate
control-plane changes. Then start the durable supervisor.
The preparation sequence must perform all of the following without fabricated
receipts or manual successful-marker files:

1. Reload the durable source intent and live supervisor fence.
2. Publish the canonical runtime envelope with protected retention, an immutable
   publication receipt, and exact-version readback in MinIO.
3. Obtain a genuine API acknowledgment for that intent and actual API release.
4. Run bounded, separately labeled host-readiness evaluation. It does not create
   a scored job or substitute for the official nine-run epoch baseline.
5. Authenticate actual completion, evidence and cleanup, promote containment,
   and install the exact reserved-round rebaseline marker.
6. Obtain an independent genuine worker acknowledgment, then activate source and
   let the supervisor open the full admission window.

Every blob-capable API and worker independently validates the retained envelope
on use, including late-joining replicas. Backend failure must fail closed; the
historical filesystem mode must not silently fall back to a different transport.

Tell Macrocosmos the round is ready only after `/competition/info` advertises the
new season's real open staging round, `/competition/readyz` succeeds, scoped
authentication works, and a safe submission proves receive → evaluate → reveal.
Automatic provisional reward selection is not honesty approval, source approval,
or payment. Staging settlement remains shadow-only.

## Replacement-scope status (2026-10-19)

The original scope `sim-latency-v2-staging-20261019` remains held, with its failed
bootstrap evidence preserved. Do not replace its immutable intent or readiness
receipt. The replacement scope is `sim-latency-v2-staging-20261019-r2`, using
separate root-owned state under `/var/lib/urnetwork/competition-v2-staging-r2`.

The replacement config (`14229f9`) and scoped vault credentials (`dd39358`)
have been deployed and verified through the live API. Migration 851 and the
runtime-blob guard are live. Bootstrap completed against the published config
commit above. Round `01a15630-5f90-8b15-19e3-74ce7177cb50` is **open**:

- staging epoch 1 of the replacement six-round season;
- opens `2026-10-19T22:40:05.671124Z`;
- closes `2026-10-21T22:40:05.671124Z`;
- decision mode `provisional_reward_v2`, zero fee and zero takeover margin;
- nine scoring replicates; the separate qualification probe does not replace
  the round's authenticated scoring baseline.

The first completed probe exposed a trusted qualifier decoder defect: real
scorer gate identifiers such as `G1_success` were rejected as noncanonical keys.
Server `858a067fa3413ca1fdf89fc1de9b197167925019` fixes this with narrowly
scoped scorer-data decoding, exact G1–G6 requirements, and deterministic
regressions. It also resumes the exact genuine persisted API acknowledgment
after a later API rollout, without changing immutable evidence. Independent
focused tests passed 31/31 normally and 31/31 with the race detector; a read-only
replay authenticated the original preserved probe without altering state.

The fixed host-side `competitionprepare` and `competitionqualify` are installed
root-owned under `/usr/local/libexec/urnetwork/competition-qualification-858a067fa`.
A fresh real probe then completed, authenticated its artifacts, retained evidence
in MinIO, promoted containment and exact-round rebaseline checks, and obtained
the independent worker acknowledgment. The supervisor activated source and
created the round through its normal fenced state machine. At 22:40 UTC,
`/competition/info` advertised the open round and authenticated
`/competition/readyz` returned `ready: true` with every check passing.

`competition-v2-staging.service` is enabled and running with its durable
supervisor and worker. The source checkpoint, simulator/scorer bytes and
evaluator image did not change during the qualifier fix. Admission readiness
does not establish the full public submission → evaluation → automatic winner
→ next-round lifecycle; that remains the purpose of Macrocosmos's staging test.
