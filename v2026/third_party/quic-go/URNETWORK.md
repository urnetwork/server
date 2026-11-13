# Local quic-go dependency

This directory is the unmodified source distribution of
`github.com/quic-go/quic-go v0.61.0` (module zip
`h1:ui88A53s8MSVYLC56en0KQ17HARk+9986Dn0SBfKNvA=`), except for the narrowly
scoped change in `connection.go`, the added `urnetwork_conn_timer_test.go`,
and the removed upstream `.github/` directory. The upstream MIT license and
source headers are preserved.

## The change

A connection's run loop calls `maybeResetTimer` on each iteration, and
upstream re-arms the loop's timer with `c.timer.Reset(monotime.Until(deadline))`
whether the deadline changed or not. The loop waits on the timer's channel in
a select, so the channel timer is in the runtime's timer heap while the loop
waits, and stays there between waits until it fires or is stopped. A `Reset`
of a timer in the heap to another time makes its P scan every timer in the
heap (`runtime.(*timers).adjust`). In a connect process, with about 32,000
timers per P, these resets were 37–54% of the re-arms that cause those scans
(CPUOPT4 Part A §3.4).

The change, all in `connection.go`:

- `resetTimer(deadline)` replaces the three `Reset` calls in
  `maybeResetTimer`. A timer still pending for the same deadline is kept: it
  fires at that deadline, which is when the `Reset` would have armed it for,
  and if the deadline has passed its value is received at once, as the `Reset`
  made it fire at once. Any other deadline, or the same deadline after the
  timer fired, stops the timer and arms a new one with `time.NewTimer` for that
  deadline. The stopped timer leaves the heap at its time; the new one is an
  insert.
- The `Conn` keeps `timerDeadline` and `timerPending`, set where the timer is
  armed; the run loop clears `timerPending` when it receives from the timer's
  channel.

No deadline computation, wake-up condition or timer instant changes.

## Tests

`urnetwork_conn_timer_test.go`, run with `go test -run '^TestUrnetwork' .` in
this directory:

- under virtual time, a loop like the connection's, with deadlines that stay,
  move earlier or later, or are already due, and events that wake the loop
  first, fires at the same instants with `resetTimer` as with upstream's
  `Reset`, over 100 seeds of 1,000 iterations;
- an unchanged pending deadline keeps its timer; a changed one, or one whose
  timer fired, gets a new timer and the last is stopped.

The upstream suite passes: `go test -short .` here ran 742 tests with this
change.

Every main module that wants the change must replace
`github.com/quic-go/quic-go` with this directory; Go does not inherit
replacements. The server module does.
