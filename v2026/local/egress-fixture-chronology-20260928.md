# Egress fixture chronology qualification — 2026-09-28

The diagnostic model run on server `b6f49bdbe6a61ef0cca3392b3a3ee47fe4806ec2`
found `TestGetProviderEgressLocationDueOrderingIsStableAcrossLimits` constructing
old locations after capturing the timestamp for their fresh health. The ordinary
location setter correctly stamps a new `update_time`. For substantially
backdated locations, that sequence means a later client verdict has requested a
full probe, so the scheduler correctly bypassed fresh-health suppression.

The ordering fixture now records the historical location's original update time
before supplying newer health. The adjacent stale-location fixture uses the same
explicit chronology. The helper changes exactly one matching location row and
does not depend on sleeps. No production query or scheduling rule changes.

`TestProviderEgressDueRechecksLaterClientVerdictWithFreshHealth` separately uses
the real reprioritisation path: fresh health suppresses an ordinary old location;
a later client verdict makes it due while retaining the usable location; health
at the verdict's update time acknowledges the request and suppresses it again.
This protects the client-verdict exception independently of the corrected
historical fixture.

The isolated worktree is
`/mnt/data/sn-testnet/worktrees/server-egress-fixture-chronology-20260928/server`,
based on server `4468a6961c00cf0ff8b84986259fa9698a7a8441`. Actual local module
resolution is captured and fenced by the maintained sn `scripts/qualification`
runner: sn `615a76753e57c11ad688f128642882b71e35559a`, connect/SCTP
`c68689c420e45bcf07ecd4713e5de6e6bab5437f`, and the other frozen physical
worktrees in `server-model-final-20260927`. The full model runs and their
services were not changed.

Go 1.26.6, `GOWORK=off`, `GOTOOLCHAIN=local`, `GOFLAGS=-mod=readonly`,
`GOMAXPROCS=2`, `-count=1 -parallel=2 -timeout=10m`, and temporary files under
`/mnt/data` were used with owned disposable PostgreSQL 18 and Redis 8 images
pinned by the frozen sn release lock. A synthetic empty GeoLite database avoids
external data dependence. The physical Go cache path is recorded explicitly;
an earlier runner attempt rejected the cache's symlink before any selected test
body ran and is retained separately as failed setup evidence.

Evidence is retained under
`/mnt/data/sn-testnet/evidence/server-egress-fixture-chronology-20260928/physical-cache`.
The maintained runner checks the actual package directory and module paths,
retains binaries and build metadata, joins their owners, and verifies complete
declared root outcomes. All 15 selected roots passed in both normal (74.307s)
and race (98.379s) runs, with no skips. `go vet . ./model` passed.

Two independent controls compiled and reached their intended failures in both
normal and race runs. Restoring the exact pre-fix ordering fixture fails the
expected due-order assertion. Disabling only the production query's later-verdict
exception makes the new regression fail its recheck assertion. Each control has
one expected failed root and retained binary exit 1, verified by maintained
`qualification replay`; setup or compiler failures do not count as proof.

The source manifests remained unchanged during qualification and controls,
actual module resolution matched before and after, and `git diff --check`
passed. The runner and service cleanup both exited 0. This is a bounded fixture
qualification, not a claim that the full model suite or mainnet admission passed.
