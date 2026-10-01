# MG03 custody census source handoff

Source base: server `5dc11761373580b5a0ddd9757cd6e4eb94140e27`.
Candidate branch: `codex/mg03-operator-census-20260928`.
Implementation worktree:
`/home/by/urnetwork/temp/server-mg03-census-20260928`.

The additive implementation is confined to `strecovery/` and `cli/strecovery/`.
There are no controller, model, database-migration, mainnet or module changes.
The public command provides full status-independent read-only database/store
collection, an offline replayable private archive and create-only original-byte
restoration. See `README.md` for the precise source authority, nonce-coverage,
file ownership and quiescence requirements. Actual canonical receipt/finality,
actual fee accounting and automated live service recovery remain open PF03 work.

Implementation formatted the source and checked the staged diff. It did not
compile, execute a test body, connect to a database, sign a live transaction,
change chain state or restore any live custody. All behavioral qualification is
owned by Sol on the exact frozen candidate commit supplied with this handoff.

Use an isolated qualification source graph. A known aligned physical graph is
`/mnt/data/sn-testnet/qualification/mg08-ur-offline-admission-20260928/source`:

| Module | Pinned commit |
| --- | --- |
| connect | `b163f9dd9ac374942fe97331f26631248a9c1f81` |
| sdk | `516521fb16da46c9f4bff0b58221e1941694f616` |
| sn | `5198f6c96a363b86c7a9951ccefe395a2b54ef56` |
| glog | `892ade4a6be396b32ea82a550f243190b5992180` |
| goidenticons | `325750b38314313dc5f44c880ab6f12f6c1ecb3c` |
| proxy | `6204ae7df2a9868bbb3a7b61231917a36e4f5c9f` |
| userwireguard | `85fb1ca4086fa5dbfcda526bec7a17a894e691b9` |
| warp | `7498864c7cd3605aad3c43eabfab9008ed7f7228` |

Replace only the graph's server checkout with the frozen candidate in a new
isolated parent directory. Do not change the shared physical graph. The main
module's existing `replace` directives require sibling paths and
`github.com/pion/sctp => ../connect/sctp`; record resolved module directories,
commits, worktree status and go.mod/go.sum hashes before and after. Use
`GOWORK=off` and the existing isolated PostgreSQL/Redis harness with
`WARP_TEST_ENV_FAIL_FAST=1`. The three `TestCensusDatabase*` roots require that
harness; all other new roots use synthetic signatures and private temp files.

Required gate scope:

1. All `./strecovery ./cli/strecovery` roots normally and under race. The source
   currently contains 23 top-level roots, no ordinary subtest tables.
2. Adjacent existing controller account/receipt recovery roots
   `^(TestStAccountRecovery.*|TestStReplacementWaitReadErrorCannotAuthorizeReplacement|TestStAccountReconcile.*|TestStReceiptCandidates.*|TestStReceiptObservation.*)$`,
   and relevant `./model` durable transaction-intent/attempt recovery roots,
   normal and race on isolated fixtures. Bound/shard the jobs as needed rather
   than weakening a timeout or masking a partial run.
3. `go vet ./strecovery ./cli/strecovery`, formatting and diff checks, with
   source/module fences unchanged.

The deterministic causal controls already embedded in the roots are the live
status-filtered query losing terminal rows, a committed insert after the
snapshot counts and before row scans, and an explicit interruption after one
durable restoration file. Additional isolated mutants can filter the snapshot
query to active statuses, change repeatable-read to read-committed, or replace
the full restoration conflict preflight with per-file-only checks. First require
the original associated roots to pass. The corresponding roots must then fail
for the intended observation: missing original signatures, changed snapshot
row count/content, or a published prefix before a later conflict. A compiler or
harness failure is not a discriminating control.

Retain every failed positive, incomplete timeout and environmental mismatch in
the qualification evidence. Report source defects back for a new frozen commit;
do not adjust expectations merely to obtain a passing receipt. Do not treat any
of these source/test gates as live network or spending authorization.
