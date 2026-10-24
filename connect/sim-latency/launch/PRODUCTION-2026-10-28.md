# Production launch — October 28, 2026, 00:00 UTC

Preparation started October 24. The public opening is fixed; preparation and
qualification start immediately. Current upstream development heads replace the
previous plan to wait for a Monday source freeze. Do not admit public submissions
before the opening or reinterpret an existing staging round as production.

## Candidate source checkpoint

Fetched October 24 from the development branches below. These are candidate
identities, not a claim of completed qualification, branch publication, production
activation, or a scoring baseline. Local operator checkouts remain untouched;
builds and evaluations use fresh temporary checkouts.

| Repository | Development branch | Candidate commit |
| --- | --- | --- |
| server | main | afaafaa521b0d1c6a88948628b00ba574c1674f7 |
| connect | main | e90d59eb404f973dba6e9d0689ecd94e2718c10e |
| sdk | main | d3824aa98c02e3c16ff7abcfe7a7088c8a470cdb |
| proxy | main | af1d364fe34b807c31936642bff7d3fd849ab2a4 |
| glog | master | 1c4954b017d9e8140a4e7020f914eb27410a41c6 |
| goidenticons | main | 39ccec1b69db3a6f67e7346d6f44863499e6241d |
| userwireguard | master | f9a92f0e5f5577a80d358065c4d39ca21f971a1c |
| sn | main | 6e686aabd53b7c45b154051fd268138988224eba |
| warp | main | 8dfa405ec380a60307952778921f05bba1c59a33 |
| gvisor | go | 57c9e137f7407ad6cd41dcca1ba304a446e09c79 |

Three dependency forks have no `main`: their actual upstream development branches
are listed explicitly instead of inventing a branch or silently retaining an old
staging pin. `operator-proxy` is not in the evaluation graph. The tracked SCTP
module is part of the connect commit, not a separate repository.

## Execution checklist

- [x] Fetch development heads and inspect operator worktrees without modifying them.
- [ ] Compile and test the complete candidate graph in private temporary clones.
- [ ] Audit transitive repository coverage against that graph.
- [ ] Publish the approved production source lock and competition branches without
  overwriting historical epoch records or force-pushing an existing branch.
- [ ] Build and qualify the exact production evaluator; retain authenticated
  containment, kill/cleanup, artifact, and host-readiness evidence.
- [ ] Collect the fresh production calibration and freeze its supported threshold;
  retain the configured nine-run shared scoring baseline for each official epoch.
- [ ] Deploy the additive winner-review migration and compatible API/worker/manager
  after publishing their changes. Qualify the real isolated Sol 6.1 max review
  broker and build/preparation integrations; test rejection, replacement,
  no-winner carry-forward, restart recovery, and successful next-round activation.
- [ ] Configure a separate production scope: six weekly admission windows, $20
  admission fee, production significance rules, live settlement, and the adopted
  elected-winner review policy. Preserve staging history and scoped credentials.
- [ ] Bind the first admission window to `2026-10-28T00:00:00Z`; verify restart
  cannot open early. Subsequent windows start after scoring/review/preparation.
- [ ] Verify MinIO capacity/retention/backup, production credential isolation,
  Grafana heartbeat and blocked-progression delivery to support@ur.xyz.
- [ ] Obtain Macrocosmos production adapter/settlement acceptance and publish the
  corrected participant instructions and duplicate-fee policy.
- [ ] Complete production preflight and leave the supervised system ready and
  waiting for the fixed opening.

Run independent preparation concurrently, but serialize scoring on sille and
avoid build/test load during timed measurements. Any repair changing evaluated
source or the evaluator requires a new exact identity and requalification, not
an unrecorded edit to the checkpoint. Failures are work to resolve before the
fixed opening, not permission to omit honesty review or invent readiness proof.

## Initial findings — October 24

- The candidate is staged in a private temporary sibling-repository layout;
  the simulator/manager compilation passed. It uses exact fetched commits, not
  local branch heads, which may lag upstream.
- The resulting local Linux binary SHA-256 is
  `f7f51be5c21ded30725602317924a7c11f34bea3b1a49b47537cb60c4ccc5c10`.
  This is a preparation build, not a published production release identity.
  Worker compilation also passed (SHA-256
  `d322efd144db3cb7e449968ab3305ae7d631e1a407ab57f3dbaa27cb9a0e0e83`).
  Focused lifecycle/source/review tests passed using the exact candidate graph:
  `go test -p 2 -timeout 10m -run '^Test(RunMainCompleteSixEpochLifecycle|WinnerReview|SourceReview|ReviewedSource|StagingSource|SourceGraph)' -count=1 ./connect/sim-latency`.
  This does not claim a full-suite, race-suite or live-integration pass.
- The production source protocol still requires eleven repositories, including
  the retired `operator-proxy`. The ten-repository retirement was implemented
  only for explicit staging. New production support must cover the Go source
  protocol, database guards, evaluator scripts and image builder, with tests
  preserving historical eleven-repository locks. Do not use a fake empty
  repository or relabel production as staging to bypass the checks.
- Public API HTTPS, the configured main PostgreSQL connection, and the host's
  configured MinIO endpoint refused connections during the initial checks.
  The existing staging supervisor reports `lease_unavailable` from PostgreSQL.
  Redis connectivity succeeded. These observations require live infrastructure
  verification after maintenance; the operator confirmed main is down for
  maintenance. They are not evidence that the deployed API code is defective.
- The staging supervisor runs the older installed release, not the newly
  published elected-winner review integration. No staging policy, production
  pointer, main database, live credential, or admission window was changed.
