# Local pgx dependency

This directory is the unmodified source distribution of
`github.com/jackc/pgx/v5 v5.10.0` (module zip
`h1:VhSvgU2jSli8o3AqIEOTJr7rZwAEUVo4E4XhR94Zfr0=`), except for the narrowly
scoped change in `pgconn/pgconn.go`, the added
`pgconn/urnetwork_slow_write_timer_test.go`, and the removed upstream
`.github/` directory and `CLAUDE.md` (upstream's CI and assistant
instructions). The upstream MIT license and source headers are preserved.

## The change

Before each flush that could deadlock against a server that is also writing,
pgconn arms a 15ms timer that starts its background reader
(`enterPotentialWriteReadDeadlock`), and stops it after the flush
(`exitPotentialWriteReadDeadlock`). Upstream arms it with `Reset` of the one
timer the connection keeps. When a connection's writes follow each other
within 15ms, the last write's stopped timer is still in the runtime's timer
heap, and a `Reset` that moves a timer in the heap earlier makes its P scan
every timer in the heap (`runtime.(*timers).adjust`). In a connect process,
with about 32,000 timers per P, these resets were 36–56% of the re-arms that
cause those scans (CPUOPT4 Part A §3.4).

The change arms a new `time.AfterFunc(15*time.Millisecond, ...)` for each
write, with the same function, and `exit` stops that instance, so the timer
fires at the same instant, 15ms after the write began, and runs the same
function. A new timer is one heap insert; the stopped one leaves the heap when
its time passes. `Stop` of the last timer reports whether it was still active,
as `Reset` did, and an active one is armed again before the existing "slow
write timer already active" bug is raised, as `Reset` left it.

The constructors keep the timer's function in `slowWriteTimerFunc`. Nothing
else changes: the background reader, its start and stop, and the 15ms delay.

## Tests

`pgconn/urnetwork_slow_write_timer_test.go`, run with
`go test ./pgconn/ -run '^TestUrnetwork'` in this directory (no database):

- the background reader starts at the same virtual instants with the new
  arming as with upstream's `Reset`, over seeded writes that end before and
  after 15ms with gaps shorter and longer than 15ms;
- each write arms its own timer and leaves the last one stopped;
- the bug path leaves the timer armed for 15ms from the second arming, as the
  `Reset` did;
- `BenchmarkUrnetworkTimerHeapScanReset` and `...New` measure what one avoided
  heap scan saves, beside 32,768 timers spaced in memory on one P.

The upstream suite needs a PostgreSQL server (`PGX_TEST_DATABASE`).

Every main module that wants the change must replace
`github.com/jackc/pgx/v5` with this directory; Go does not inherit
replacements. The server module does.
