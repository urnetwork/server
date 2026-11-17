# Epoch 2 recovery progress

Observation: 2026-11-11 22:55:46 UTC. This is a historical snapshot, not an
activation authorization or a replacement for authenticated runtime evidence.

The round-scoped database observation reports two successfully scored jobs,
one infrastructure failure, eleven queued jobs, and two claimed jobs. Sille is
executing `01a1c62f-28e4-84aa-5ba5-de4cae9f8ade`, with its original deadline
2026-11-12 01:45:16.918094 UTC. Goofe's claimed job
`01a1c4af-c928-3e6a-d183-6c9f319cc119` remains blocked before execution;
its original deadline is 2026-11-11 23:52:17.050115 UTC.

Both hosts now run the independently tested persistent management-cache service.
Unlike the retired oneshot timer, child passes remain inside one persistent
cgroup. The old timers remain masked. Native qualification and exact signed
runtime checks passed without changing frozen evaluation identities or scores.

Root-protected deployment receipts:

- Sille: `/mnt/data/competition-cache-stable-deploy-20261111.WeUOrzyY/deployment-native-enabled.private.json`,
  SHA-256 `ee7a991668acd6001746f3c71cc39ce72a6b214fe45606775e5f4890a06a16e3`.
- Goofe: `/home/by/sim-latency/competition-cache-stable-deploy-20261111.YDJYIQJF/deployment-native-enabled.private.json`,
  SHA-256 `3193b4c4df047a7abf90d83d2376c8727ba1b72c1b327b4e888fc5e6aabf8fe2`.

The Goofe-only legacy prelaunch recovery package is prepared but **not activated**
at this observation. Original and archived configuration views both authenticated
the complete manifest, cleanup plan and original clock. The sealed state snapshot
has fencing token 13, launch attempted, execution not started, and recovery active.
The preparation does not prove cleanup or authorize a new attempt or deadline.

Prepared evidence on Goofe:

- `/etc/urnetwork/competition-goofe-prelaunch-recovery-20261111-v1/preparation-receipt.json`,
  SHA-256 `ce67291162574ba68aa48994499007952d2dd8157066d7c879b558a0057165c8`.
- Authorization SHA-256:
  `e634d67b20ea04db5a4e2602f8603012090268140cb48ffe9c246744b09f01ec`.
- Native inert-manager canary receipt SHA-256:
  `d43e158572d201588a35693aca3197120d42b7aebf72cf56fa1d0cde81f64d8c`;
  canary passed, fixture cleanup passed, and no inert payload executed.

Next: review and apply the already-approved Goofe-only launcher fence, perform
fresh positive launch-surface checks and full cleanup, retain the infrastructure
failure proof, and restore ordinary ingress only after the original recovery
service is restored. Sille continues uninterrupted. No API, configuration, vault,
database, frozen product source, scoring policy or baseline change is needed for
this native recovery operation.

## Follow-up at 23:13 UTC

Sille completed two of nine candidate replicates for its next submission. Its
previous successful score remains committed. The queue observation remains two
scored, one infrastructure failure and eleven queued; Goofe is claimed but has
not executed its candidate.

One reviewed Goofe activation was attempted with source SHA-256
`cfbde0852686875fbc62359584ff2575797006e773a262a464102084383eec77`.
It installed the in-place ingress fence and exact-run launch denials, positively
audited and retired 37 inactive historical control units, then stopped on the
native no-launcher check: a control-user process was present. It did not start
the canonical recovery, perform the full cleanup, restore ingress or change the
evaluation clock. The failed receipt and all per-unit fenced/retired audits are
retained under the prepared profile's `audit` directory. Do not replay this
activation or delete its receipt.

Read-only follow-up found the exact control user's user manager and PAM process,
with no sessions and linger disabled. A continuation must suppress new control
connections and positively join that manager, preserve the old attempt and
audits, and pass the existing native proof before performing full cleanup.
No cleanup success is claimed by this snapshot.

## Follow-up at 23:31 UTC

The exact temporary root-owned coordinator SSH connection hold was installed,
and the control-user manager was stopped with its native cgroup independently
absent. Operator access remained working. State and original clock hashes were
unchanged.

One separately reviewed continuation then authenticated the 37 retained
retirements and started the canonical recovery. That service failed before
cleanup verification. Its original starting receipt and the failed continuation
receipt remain retained; neither may be overwritten or removed to permit replay.

A read-only probe using the frozen native SDK reproduced the exact error:
`legacy effective SSH ingress is ambiguous`. The ingress parser incorrectly
rejects legitimate repeated non-authority `sshd -T` keys such as `hostkey` and
`listenaddress`. The correction must preserve strict duplicate/missing-value
checks for `forcecommand`, `authorizedkeysfile` and `authorizedkeyscommand`.
Because the original recovery executable is pinned, a corrected executable also
needs a narrowly authenticated one-hop amendment preserving all original attempt,
clock, source, launch-denial and starting-receipt evidence. Implementation and
deterministic regression tests are in progress. No cleanup success is claimed.
