# Local quic-go dependency

This directory is the unmodified source distribution of
`github.com/quic-go/quic-go v0.61.0` (module zip
`h1:ui88A53s8MSVYLC56en0KQ17HARk+9986Dn0SBfKNvA=`), except for two narrowly
scoped changes, in `connection.go` and in `http3/server.go`, the added
`urnetwork_conn_timer_test.go` and `http3/urnetwork_graceful_goaway_test.go`,
and the removed upstream `.github/` directory. The upstream MIT license and
source headers are preserved.

## The run loop timer

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

### Tests

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

## One GOAWAY per connection in a graceful http3 shutdown

`Server.handleConn` accepts a connection's request streams on the server's
grace context until a graceful shutdown (`Shutdown`) cancels it. The accept
then fails, and the loop's graceful branch starts a goroutine that writes a
GOAWAY on the control stream and switches the accept to the server's close
context, so later streams are refused while the requests in flight finish.

quic-go closes a closing connection's streams before it cancels the
connection's context. Between the two, an accept fails at once while the
connection's context and the close context are both live, and upstream's loop
took the graceful branch again on every pass: another GOAWAY writer each time,
for as long as the window stayed open. Every connection that closes during a
graceful shutdown passes through that window: the client closing after the
GOAWAY, the idle timeout, or a transport destroyed under it. The window stays
open while the transport's close holds its mutex over its destroy fan-out, and
while every core is busy, which the spinning loops make worse. URnetwork's alt
had an exit burst of at least 700k goroutines this way (10-24 to 10-26), and
the connect front's drain (server LBALT.md D12) shuts each api connection down
gracefully while its transport is live.

The change, in `http3/server.go`: once the graceful branch has run, so the
GOAWAY was sent and the accept waits on the close context, an accept failure
with the close context live can only be the connection closing, and the loop
ends as it does once the connection's context is canceled, with the same error
classification. A second GOAWAY is never created. Nothing changes before a
graceful shutdown, or for a close of the server.

### Tests

`http3/urnetwork_graceful_goaway_test.go`, run with
`go test -run '^TestUrnetwork' ./http3/` in this directory. A server recorder
holds the connection inside its close window (quic-go records the close between
closing the streams and canceling the context) and counts the GOAWAYs the
server creates. After a graceful shutdown's GOAWAY, the client closes its
connection in one test and the server's transport loses its socket in the
other. `ServeQUICConn` must return while the window is held, with one GOAWAY;
upstream's loop creates a second one at once and never returns while held.

The upstream http3 suite passes with this change: `go test -short ./http3/...`
here, under `GOFLAGS=-trimpath`, skipping the six tests that read source paths
at run time (`TestErrorCodes`, `TestServerAltSvcFromUnixSocket`,
`TestServerClosing`, `TestServerConcurrentServeAndClose`,
`TestServerImmediateGracefulShutdown`, `TestServerListenAndServeErrors`).

Every main module that wants these changes must replace
`github.com/quic-go/quic-go` with this directory; Go does not inherit
replacements. The server module does.
