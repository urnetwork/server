# Local utls dependency

This directory is the unmodified source distribution of
`github.com/refraction-networking/utls v1.8.2` (module zip
`h1:j4Q1gJj0xngdeH+Ox/qND11aEfhpgoEvV+S9iJ2IdQo=`), except for the narrowly
scoped change in `u_conn.go`, the added `urnetwork_uconn_write_test.go`, the
removed upstream `.github/` directory and `.travis.yml` (upstream's CI), and
gofmt's formatting of doc comments in `testenv/testenv.go` and
`u_roller.go` (the repository's pre-commit hook formats every added Go file).
The upstream BSD license and source headers are preserved.

## The change

`(*UConn).Write` takes a share of the interlock with `Close` in a
compare-and-swap loop, and upstream registers the release,
`defer c.activeCall.Add(-2)`, inside that loop. The compiler does not open-code
a defer inside a loop: each call puts a defer record on the goroutine with a
heap-allocated closure, one allocation per write. A proxy process writes every
frame of its window clients' TLS connections through it: with its GC assists
it was 1.1–1.9% of a proxy process (server CPUOPT7 §10.3.2, P3).

The change registers the same release right after the loop, as utls's own
`(*Conn).Write` and Go's `crypto/tls` do. It runs at the same point, once
the compare-and-swap succeeded, and the loop's only other exit returns
`net.ErrClosed` before taking a share, as before. Nothing else changes.

## Tests

`urnetwork_uconn_write_test.go`, run with `go test -run '^TestUrnetwork' .` in
this directory, over a client `UConn` handshaken with a local server:

- `TestUrnetworkUConnWriteAllocatesNothing`: a small write allocates nothing
  (1 allocation a write with upstream's loop);
- `TestUrnetworkUConnWriteReleasesItsInterlock`: a write leaves `activeCall`
  as it found it, and a write after `Close` is refused with `net.ErrClosed`.

Upstream's suite passes with `go test -short ./...` here.

Every main module that wants the change must replace
`github.com/refraction-networking/utls` with this directory; Go does not
inherit replacements. The server module does. Others, connect's tests and the
sdk among them, build upstream's v1.8.2, which behaves the same.
