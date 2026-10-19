# V2 staging deployment

This is a control-plane release, not a new evaluated source checkpoint. Preserve
the ten-repository `sim-latency-staging` references, frozen config ledger, and
evaluator image `sha256:8d1206bdc25ce758ad537a0f959e7b16dc2c7c5ffaca4c15636d9f364e0fea08`.
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

Only after the operator rollout, bootstrap the exact frozen ledger from config
commit `587f764beaea20332493bd7e349d415ecc39edb5`, then start the durable supervisor.
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
