# Model fixture qualification — 2026-09-27

The full model run on server `b6f49bdbe6a61ef0cca3392b3a3ee47fe4806ec2`
exposed fixtures that no longer matched installed custody and probe scheduling
rules. These corrections change test inputs and assertions only. No production
writer, reader, migration, retention rule, scheduler or service configuration is
changed.

`TestPlanPaymentsMaxDuration` and `TestPlanPaymentsMaxDurationLoop` used to
settle contracts and then move their terminal timestamps into historical payment
slices. Migration 725 correctly rejects that reassignment. The fixtures now
insert the payment planner's historical contract/sweep inputs with their original
times and complete provider usage in the first transaction. They still use a
real purchased balance and payout wallet, run the actual dry-run/bounded/loop
planners, and check the persisted payment allocation. Each test also attempts a
terminal-time rewrite, requires the installed guard to refuse it, and verifies
that the original time and usage remain intact. This setup supplies finalized
planner inputs; it does not replace settlement writer qualification.

The adjacent `TestRemoveStragglerContracts` and `TestBackfillContractReapTime`
fixtures used to mark a contract settled without its usage proof. They now close
both actual parties through `CloseContract`, then delete only the billing sweep
to construct the missing-billing-row corruption case those retention tests
exercise. The completed contract's terminal custody remains unchanged.

`TestProbeDueQueueIgnoresTheEgressHealthGate` used fresh negative health but
expected an immediate full probe based on its older location. The scheduler now
refreshes sampled-site health after half its lifetime. The revised fixture keeps
two providers excluded from public selection: one has negative health old enough
to require refresh and must enter the queue; the other's fresh negative health
must remain deferred. Existing recovery, health-age, missing-health, attempt
backoff and blackhole controls run alongside it.

An audit of direct contract mutations found intentional pre-migration missing
proof fixtures, explicit guard refusal tests, and mutable retention/open-report
timestamps. Those cases remain unchanged. The independent pending-participant
retention design is outside this fixture correction.

Qualification uses Go 1.26.6, `GOWORK=off`, `GOMAXPROCS=2`,
`WARP_TEST_ENV_FAIL_FAST=1`, `-count=1 -parallel=2 -timeout=10m`, and owned
temporary directories under `/mnt/data`. Disposable PostgreSQL 18 and Redis 8
images are pinned by the sn release lock at
`32b6a19c91e5275bc44a6407ecedf1a8b58e1e16`. Both batches use the synthetic
empty GeoLite database used by the initial full suite; they make no live service
or chain changes. Each runner owns and cleans its own two containers. The full
suite checkout and its independent services are untouched.

Retained scripts, JSON results, old source overlays, source digests and cleanup
receipts are under
`/mnt/data/sn-testnet/evidence/server-model-fixtures-20260927`; probe scheduling
results are in its `egress` directory. Old-source controls substitute only the
exact corresponding test files from `b6f49bdbe6a61ef0cca3392b3a3ee47fe4806ec2`
using `go test -overlay`. A reproduced assertion/SQL refusal is required;
compiler or setup failures do not count as causal evidence.

| Command scope | Normal | Race |
| --- | --- | --- |
| Four payment/retention tests | 4 passed, 34.498s | 4 passed, 44.063s |
| `TestContractUsageGuard*` | 6 passed, 30.835s | 6 passed, 42.233s |
| Eight probe scheduling/recovery tests | 8 passed, 43.072s | 8 passed, 58.929s |
| Original payment/retention test files | 4 expected failures, 32.650s | 4 expected failures, 42.402s |
| Original probe fixture file | 1 expected failure, 5.314s | 1 expected failure, 7.767s |

Both `go vet . ./model` commands and `git diff --check` passed. Both runners
and both service cleanup receipts exited 0. The original payment fixtures fail
with `contract terminal usage attribution is immutable`; the original retention
fixtures fail with `new contract settlement requires immutable provider usage`.
The original probe fixture reaches its excluded-provider due-queue assertion.
All causal controls compiled and reached those intended failures in normal and
race runs. No test in the affected selectors was skipped.

The focused worktree is a real checkout at
`/mnt/data/sn-testnet/worktrees/server-model-fixtures-workspace-20260927/server`.
Actual `go list -m -json` resolution and Git states are retained in
`resolved-dependencies.json`: sn resolves to the clean frozen commit above and
connect/SCTP to clean `c68689c420e45bcf07ecd4713e5de6e6bab5437f`. Other inherited
module links resolve to clean live sibling checkouts: proxy `6204ae7d`, sdk
`42241118`, glog `892ade4a`, goidenticons `325750b3`, and userwireguard `85fb1ca4`.
This focused result is not represented as a fully frozen dependency graph.

The earlier full model run is used only for failure collection. Its command
entered a checkout outside its prepared workspace, so relative module
replacements resolved to live siblings. It cannot qualify the intended frozen
composition. A separate full run must use real isolated worktrees for every
local replacement and verify actual module resolution, exact revisions and
clean state before compiling and after completion. This receipt does not claim
that the complete model suite or mainnet admission has passed.
