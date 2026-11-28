# Epoch-1 approval: deployment handoff

Update, November 9: deployment and exact approval import succeeded. The public
result records review `01a1c088-ca60-8cf8-844e-6ff50fff8cb4` with integrity and
source approval. Reward-delivery and source-activation holds are revision 2,
inactive. The settlement event feed exposes the unchanged election; payment
is still pending. Fresh private source staging is running under
`urnetwork-epoch1-approved-source-20261109-v1.service`. Inspect its actual state
before any restart. Source build qualification and promotion remain separate.
See `EPOCH1-APPROVAL-RELEASE-2026-11-09.json`; the deployment instructions and
pending observations below are retained as historical handoff evidence.

The genuine Astra max review approved the exact elected submission. The user's
conditional winner approval is satisfied. See the official approval and import
qualification JSON records in this directory. Do not restart Qwen/Gemma for
epoch 1 or treat their interrupted run as completed.

## Deploy

- Server `15b91c1f4` or later: apply pending migrations through **949** with the
  normal main deployment pipeline, then deploy the main API.
- SN `6b561457`: corresponding additive OpenAPI contract.
- Config `4963f0d`: deploy config-updater; its exact official import grant is
  `a33d62757c9cf3d31c0ff72dd04f7885b20067b00581ba77d0a1414d85b5b4be`.
- No vault update is needed. Use the existing scoped `review_operator`
  credential, never the miner/submitter or general reviewer credential.

At 22:21:27 UTC, the new endpoint still returned 404. The user has said they
will deploy; that is not deployment-completion evidence. No main migration or
official review import has been performed by this agent.

## Import and verify

Before importing, verify that the public result still elects job
`01a1a440-fd2e-538a-365c-f4b23eed4739`, revision 1, decision digest
`a800ecdba04be602f61a904e9626819fa68fcf71191bdf8dda0b7c7989a94e81`,
and is not invalidated. Post the exact root-owned request at
`/var/lib/urnetwork/competition-epoch1-official-astra-20261108-v1/official-import.json`
to `/competition/v2/official-review-imports`. Its SHA-256 equals the grant above.
The existing credential is in
`/etc/urnetwork/competition-production-r2-20261029-v1/review-operator.token`.
Read it without printing it or enabling shell tracing.

If config/API/schema convergence is incomplete, retain the actual response;
do not alter the request, forge Snow attribution, backdate a native completion,
edit old rows, or bypass the deployment pipeline through PgBouncer.
The endpoint supports exact same-actor idempotent replay.

After a real successful import, retain the response/review ID and verify
`integrity_review: approved`, `source_approval: approved`, genuine
`gpt-6-astra/max` provenance, unchanged decision/ranking and exact evidence
identities. Reward and source holds remain separate. Release only after their
corresponding checks; do not claim source promotion, a new epoch, or payment
merely because the approval record exists.

## Prepared management binary

An inactive root-owned, single-link mode-0555 CLI is installed at:

`/usr/local/libexec/urnetwork/competition-official-review-15b91c1f4-20261108-v1/sim-latency`

SHA-256: `10299e315ed5e89076db08eba789ca360cea2ffd649c942702476df4597e0e4f`.
Build metadata identifies clean source `15b91c1f413671ccdd8e837cfe3a3ddc6ad50ff8`,
Go 1.27.3, Linux/amd64, CGO disabled. It has not replaced or restarted the
stopped production supervisor. This control-program build is not a new
qualified evaluator image or a source-build receipt.

All ten remote `sim-latency` branch heads were checked and still match the
epoch-1 checkpoint. Source promotion must continue using fresh temporary
checkouts and the supported reviewed-source/build/qualification flow. The
existing source-build qualifier separately requires actual fixed tests,
containment and host evidence; an honesty review is not that evidence. Keep
that distinction and the still-unaccepted calibration explicit.

The installed old source-build qualifier still references its legacy OpenAI
test key. An inactive replacement is staged alongside the new CLI as
`source-build-official` (SHA-256
`9fd65bde79d4ed3a177dff44d67d85a84ac7b5ec2ca00b614f15acbc7579154f`).
Its fixed-Go-test broker uses the existing two-member Qwen/Gemma configuration;
it does not replace or repeat Astra's official honesty review. Sixteen local
component tests passed, but no real winner build, tests, model calls, host
qualification or promotion were performed by this preparation. See
`EPOCH1-OFFICIAL-SOURCE-BUILD-STAGING-2026-11-08.json` for all pins and scope.

The bounded nonmutating readiness monitor is
`urnetwork-epoch1-official-import-readiness-20261108-v1.service` (started
22:29:54 UTC, 30-minute bound). Inspect its current process/journal rather than
assuming it remains live. It only detects the new route; actual import and
hold release still need the checks above.
