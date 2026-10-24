# Exact compiler policy and provide-write regression

## Go 1.27.3 is mandatory

The operator explicitly requires Go 1.27.3. Base-image construction pins the
digest-qualified Go 1.27.3 image, disables automatic toolchain switching and
checks `go env GOVERSION` before dependency/build work. Offline reviewed-source
check, correctness and compile phases do the same; compiled-artifact validation
also requires `debug.BuildInfo.GoVersion == "go1.27.3"`. A clean Git identity
alone does not authorize another compiler.

Host Makefile, test script, local-main launcher and harness worker construction
refuse a different PATH compiler and use `GOTOOLCHAIN=local`. Install/select the
actual Go 1.27.3 binary before running them. A Go 1.26.6 launcher with automatic
selection disabled is not equivalent to Go 1.27.3.

Actual image `sha256:79400f3375c64767f5021e43ef852e9ff9da3ca5a4b79dc03e8e9ed381247eb9`
returned `go version go1.27.3 linux/amd64` with networking disabled. The installed
inactive native control release `competition-control-822902437-61f7a622` also
reports Go 1.27.3 in binary build metadata. That older control release does not
yet contain these additional enforcement checks; a new release is still needed
before production activation. Existing evaluator identities are not relabeled.

## Confirmed frozen-source concurrency gap

The failed corrected-image 15,000-provider impaired trial's service diagnostics
contain provide-key uniqueness errors and deadlocks during overlapping inserts.
Current main already serializes same-client provides, beginning with upstream
`a7ed792d0d9683007d768c4d770ad9a51acd9b9a`. Frozen server
`3d96d16ddb4fc2fb08d3f9003c52e30b9a47241d` does not.

A private candidate adds only the same per-client transaction advisory lock
before the frozen implementation's delete/insert replacement. It does not take
the broader upstream difference-write, deactivation or maintenance changes.
The candidate's regression runs the real public `SetProvide` path with explicit
SQL barriers, proves a same-client second call waits before deletion, and proves
an unrelated client can complete. The identical regression deterministically
fails on the unchanged checkpoint with:
`overlapping replacement reached delete before acquiring its client lock`.

This proves the write-serialization defect, not that it caused the fatal
matchmaking probe or forced shard kill. The separate parent/child prewarm-order
observation remains unconfirmed as a root cause. The active source ledger,
competition branch and evaluator were not switched during these tests. A new
checkpoint would need its own image, qualification and fresh calibration.

## Independent smoke evidence

Sol 6.1 low executed all tests with the actual installed Go 1.27.3 binary,
`GOTOOLCHAIN=local`, two management CPUs, `GOMAXPROCS=2` and `GOFLAGS=-p=2`.
No timed scoring overlapped. Only disposable PostgreSQL/Redis fixtures were
used; cleanup exited 0 and left unrelated persistent services unchanged.

| Run | Outcome | Log SHA-256 |
| --- | --- | --- |
| Unchanged frozen regression, count 1 | Expected failure at the ordering assertion | `440f6ee814cd690c98b780a0b31231782fb8cde8d08d5871dd893b82ca2ef8cc` |
| Candidate regression, race/count 3 | 3 passes | `d5cad7c5396a8c94d2709bd347c1eea7d44df701a55bff2b9ada5fc0d908deea` |
| Five existing provide/mirror tests, race/count 3 | 15 passes | `15ee2cac68e5e9aba9ddb37a772e30f6bb1b33f671121d1da6a57cbf271183b4` |
| Main exact-toolchain, entrypoint, lifecycle and staging tests, race/count 3 | 36 passes | `0e649315bbb26bacf6a0cc963757d1d7cc0fe6d29bd4b28011aac27c663500d5` |

Complete logs are retained relative to the mono root in
`temp/provide-serialization-independent-retry-20261024.WahomWci`.
The first disposable launcher failed before tests because its PATH still named
Go 1.26.6 while `GOTOOLCHAIN=local`; its cleanup passed. That failure is retained
separately under `temp/provide-serialization-independent-20261024.ME89EqvP`, log
SHA-256 `0efb020fd720452d0f8c9552b9438cf87b4807b044a92f3d78a84ee2db8e813f`.
The corrected launcher explicitly selected the installed toolchain before any
fixture compilation. Broader simulator validation is still required after
publishing this smoke-tested phase. Main maintenance remains a write hold.

## Broader gate and retained fixture failure

Main `a128580b6` subsequently passed three consecutive complete simulator
`tests.sh -count=1` invocations under independent Sol 6.1 low execution and
Go 1.27.3. Outer durations were 210, 141 and 140 seconds; race-package durations
were 137.502, 132.446 and 132.466 seconds. Each invocation reported 433 top-level
passes including the initial six-epoch gate. Archive verification also passed
exact coverage and hashes for 2,866 files. Private root:
`temp/provide-full-independent-20261024.mY1WhXgV`.

| Log | SHA-256 |
| --- | --- |
| `main-1.log` | `7f22efffc1cd81d3e41b1949a87f456f963d386472431b1eaabe576cfcc68e54` |
| `main-2.log` | `fc20a5e5a72deebea832db2d7556f92fbb35732ec81cc1fec39084c4a9f97ac7` |
| `main-3.log` | `dd1cc2cb6641238f2801c8386cba4384288f2faea7d228d64529b422081406e8` |
| `main-baseline.log` | `8f48a53341d83a88ec173416c60e4ce65350114eea6af0596520f85c10f65c61` |

The preparation-branch candidate `395207e9b` failed its first full invocation
because two older tests supplied an unspecified caller address. That falls
through the IP override lookup to `mmdb/geolite2.mmdb`, which the portable suite
fixture intentionally does not materialize. The same fixture is sufficient for
main's tests because main uses an explicit reserved-subnet caller instead.
This is a deterministic fixture mismatch, not a measured performance failure
and not evidence that the SQL serialization backport regressed.

The invocation's built-in retries were allowed to terminate without edits or
external resampling. It exited 1 after 326 seconds (race package 188.078), with
329 top-level passes and two failing tests. No second or third candidate
invocation ran. Cleanup exited 0. The retained `candidate-1.log` SHA-256 is
`4a3d9cacfb398413f5417c966495332027c310f01c374a0b10ef987d16baebe2`.

Preparation commit `6261c15454adec5a00680e4c3332f628a0aed1e7` backports only
main's existing `client_session_fixture_test.go` helper/regression and its two
test call sites from `529be0f76`. It uses the portable fixture's documentation
subnet instead of depending on a production location database. No fixture
generation policy, runtime code or gate was relaxed. A new independent focused
smoke and three-pass candidate gate started in
`temp/provide-full-candidate-fixed-20261024.xyJiO9y2`; its result is not claimed
here. Both preparation commits are published only on
`preparation/sim-latency-provide-20261024`, not activated as the competition
checkpoint. The successful main gate remains valid and unchanged.

## Completed candidate gate and prepared checkpoint publication

The corrected candidate passed its fresh independent gate with Go 1.27.3:
focused smoke 9/9 passes, then three consecutive complete suite invocations,
each with 332 top-level passes (one initial lifecycle plus 331 race tests).
Outer durations were 140, 117 and 116 seconds; race-package durations were
114.326, 109.513 and 109.254 seconds. Archive verification covered all 2,706
files in that frozen tree. Its smaller archive is not main's 2,866-file archive.
Cleanup exited 0 and the candidate remained clean at `6261c15454...`.

| Log | SHA-256 |
| --- | --- |
| `smoke.log` | `224cea63a39ca1773edce3317e2c2a0e6d01089dfe3d4a30e4a7ea904e821900` |
| `full-1.log` | `741fa0c14db454aeedc1d24da886ab4cb8e6481646bcbdb660031ef0c9e047ad` |
| `full-2.log` | `186eb8e0a947195b4f5d3cf7bd18ca8e9463c694b6c687c78427f7c45e79d083` |
| `full-3.log` | `b6040fb6ddac9cebd651b3426b7794cb71cd3fae1ac0dde69d7fff114da925cf` |
| `baseline.log` | `fb9006c5b441d37e42e754ec19ecd5276ae075db7450e94bde548537da9d6906` |

Only after this gate, the published server `sim-latency` branch fast-forwarded
from `3d96d16...` to `6261c15454adec5a00680e4c3332f628a0aed1e7`. All other nine
repository pins remain unchanged. Config main
`cf8b9b584c93137a44a618bd9adf589f8b23749f` publishes the preparation ledger:

- Canonical model source lock:
  `52b6d63a32ba21cb011d122de80ea8d51d82cb164a165d906223fb07c6b7b694`.
- Source manifest file SHA-256:
  `838b132d2c19112e50fefaabfabda81e22127638e8ba281f0c0141ab82d30612`.
- Preparatory source-choice record SHA-256:
  `6f6203d143af29238b0cd2e30126b10b175d1f990ad598a9c019e7961b429966`.
- Prior ledger archived byte-for-byte as
  `history/sim-latency-2026-10-24-182809.yml`, retaining SHA-256
  `6fca07712cc22b9bb3f9c30ef974392c8e49eff808cab491a8a2ffca08eeeb81`.

A clean Linux/amd64, CGO-disabled, greenteagc smoke executable built with
Go 1.27.3 and `vcs.modified=false`, SHA-256
`9e967c54d65da7e0225b27ae818f3bd38acb6bcb3bc8f2885f703325362df759`.
It is not the future image's simulator digest. The new evaluator build started
in `temp/provide-evaluator-build-20261024.Aq1Q7w99`, using a fresh ten-repository
metadata workspace and sparse source-ledger checkout. Actual image identity,
container qualification and new calibration are still pending.

The config push first failed because Git LFS's optional locking endpoint was
unreachable. Every changed file was verified to have no LFS filter; a per-command
lock-verification override allowed the normal pre-push hook to complete. No LFS
object upload, commit hook, force push or unrelated worktree was bypassed.
Remote config main was read back at the exact published commit.

The new descriptor explicitly says `prepared_not_activated`,
`runtime_qualified:false` and `hidden_baseline_committed:false`. The 16.1%
historical floor remains provisional pending fresh calibration. The active
staging competition policy, main database and production admission were not
changed. Prior image/CSV/report identities remain historical evidence for their
original source; they do not qualify this checkpoint.
