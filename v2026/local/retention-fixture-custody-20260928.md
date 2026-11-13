# Retention fixture custody qualification — 2026-09-28

The diagnostic model run exposed two more historical terminal inserts that
predated the immutable provider usage boundary:
`TestRemoveContractBatchesDrainsDuplicateCandidates` and
`TestAssignStragglerReapTimeRespectsBudget`. Both tried to insert a settled
contract without its usage snapshot; the duplicate-candidate fixture also
omitted `close_time`. The installed guard correctly refused those rows.

The fixtures now supply their original close time, a complete provider usage
snapshot and explicit usage direction in the first insert. The duplicate
candidate's two billing sweeps divide its 1024 bytes into 512 bytes each. Its
three-contract/six-sweep drain assertion is unchanged. The budget fixture still
proves that an already spent budget assigns one batch of two, then a normal
budget assigns the remaining three. No terminal row is rewritten, and no
production retention rule, migration or custody guard changes.

The earlier adjacent audit concentrated on terminal updates and missed these
two inserts. The extended audit covers direct model-test inserts and updates:
other canonical settlement inputs already carry snapshots or explicitly run
before the guard migration; canceled and legacy `success` rows are non-credit
query fixtures. The independent pending-participant retention implementation
remains outside this change.

Qualification ran in
`/mnt/data/sn-testnet/worktrees/server-egress-fixture-chronology-20260928/server`
on `d62f6fcc4ba8d02d169b9476b6e49f870d4c81d0`, with only this fixture file
changed. The maintained sn `scripts/qualification` runner authenticated actual
package directories and module resolution against the same frozen physical
dependency graph as the preceding egress qualification: sn `615a7675`,
connect/SCTP `c68689c4`, and the other isolated final-qualification worktrees.
Go 1.26.6, `GOWORK=off`, `GOTOOLCHAIN=local`, `GOFLAGS=-mod=readonly`,
`GOMAXPROCS=2`, `-count=1 -parallel=2 -timeout=10m`, and temporary files under
`/mnt/data` were used. Disposable PostgreSQL 18 and Redis 8 were pinned by the
frozen sn release lock; a synthetic empty GeoLite database was used.

Evidence is retained under
`/mnt/data/sn-testnet/evidence/server-retention-fixture-custody-20260928/sorted-roots`.
An initial plan was refused for unsorted expected roots before any selected
body ran; its failed setup and successful service cleanup are retained in the
parent directory. The corrected capture retains the binaries, build metadata,
joined body exits, complete declared root outcomes and before/after manifests.

| Scope | Normal | Race |
| --- | --- | --- |
| Two corrected retention roots | 2 passed, 10.505s | 2 passed, 15.722s |
| Six database usage custody guards | 6 passed, 32.718s | 6 passed, 43.826s |
| Exact former retention fixture | 2 expected failures | 2 expected failures |

Every positive root passed with no skip and body exit 0. Both old-fixture
controls compiled, failed with `new contract settlement requires immutable
provider usage`, and matched maintained `qualification replay` with exactly
two expected failed roots and body exit 1. `go vet . ./model` passed. Actual
module resolution matched before/after, all source fences and
`git diff --check` passed, and runner/service cleanup exits were both 0.

The diagnostic full run ended at its package deadline, and the independent
frozen full model run remains separate. This receipt qualifies the bounded
fixture corrections and preserved guards; it does not claim a passing full
model suite or mainnet admission.
