# Local go-redis dependency

This directory is the unmodified source distribution of
`github.com/redis/go-redis/v9 v9.23.0` (module zip
`h1:/TTVdJa8BRNsybwwGGnmiYktHNlUXtbOQxlWpHBPL3s=`), except for the narrowly
scoped change below, the added
`internal/pool/urnetwork_conn_check_skip_test.go`, and the removed upstream
`.github/` and `.claude/` directories, `AGENTS.md` and `CLAUDE.md` (upstream's
CI and assistant instructions). The upstream BSD license and source headers
are preserved.

## The change

Every `ConnPool.Get` that takes a connection from the idle list checks its
socket first (`isHealthyConn`, then `connCheck`): it clears the deadlines and
peeks one byte with `recvfrom(MSG_PEEK|MSG_DONTWAIT)` through the
connection's `RawConn`, to find a connection the server closed or that holds
unexpected data. In a connect process on 2026-11-20 that check was about 0.8%
of the process's CPU (`pool.checkSyscallConn` 0.5% plus 0.3% around it,
LBALTOPT2 §3.3, R19), one syscall per command on top of the command's own.

The change adds an option, `SkipConnCheckWithin`, to `Options` and
`ClusterOptions` (each cluster node's pool) and to the pool's `Options`. Zero,
the default, is upstream's behavior. With a window set, `Get` hands out a
connection without the socket check when:

- it was put back into the pool (`Conn.LastPutAtNs`) less than the window ago,
  by a clock that hasn't gone back since; and
- the pool hasn't removed a connection, nor found one unusable at `Get`, within
  the last window.

The lifetime and idle-time checks still run (`isHealthyConnForGet` repeats
them: if upstream changes `isHealthyConn`'s checks before `connCheck`, change
both). Every removal (`removeConnInternal`: the client's removal of a
connection a command failed on, and the pool's own) and every connection `Get`
finds unusable sets `connCheckAllUntilNs`, so every `Get` checks again for the
next window.

What it gives up, and why that is bounded: a connection the server closed
within the window before its checkout is found by the command it serves, which
fails with `EOF` (the server's FIN is queued, so the read returns at once, and
the server, closed before the command, never ran it). A client with
`MaxRetries` retries `EOF`, single commands and pipelines alike, on another
connection, and the removal makes every later checkout check, so one command
per pool, not each, meets a closed connection. A client without retries would
return that error to its caller, so it should leave the window zero. The
server sets the window, a second, only on its pool with command retries
(`redis.go:redisSkipConnCheckWithin`); its pools without retries check every
checkout. A connection whose peer vanished without a FIN or RST is found by
neither, with or without the change.

`isHealthyConn` itself, the pool's `Close`, the CSC and push-notification
peeks (`checkForData`, `maybeHasData`) and the pub/sub pool are unchanged.

## Tests

`internal/pool/urnetwork_conn_check_skip_test.go`, run with
`GOFLAGS= go test ./internal/pool/ -run '^TestUrnetwork'` in this directory (no
Redis server), over loopback TCP sockets whose checks are counted through
`RawConn.Read`:

- a connection put back within the window is handed out unchecked, again and
  again; one put back longer ago is checked; without a window every `Get`
  checks;
- a connection the peer closed past the window is checked, closed and replaced
  by a new dial, and the next connection is checked too;
- a connection the peer closed within the window is handed out unchecked, its
  read finds the close, and its removal has every `Get` check for the window,
  after which `Get` skips again;
- a put time later than the clock never skips, and the lifetime and idle
  checks still close a connection put back within the window.

Each fails with the skip or the pause taken out. The upstream `internal/pool`
suite passes unchanged (`GOFLAGS= go test ./internal/pool/`).

`BenchmarkUrnetworkGetPut` times one checkout and return of an idle
connection, which nothing blocks, so ns/op is CPU. On 2026-11-20, an M1 Max
(darwin), over sockets connected to the local Redis
(`URNETWORK_BENCH_PEER_ADDR=10.213.0.1:6379`), mean of 6 runs of 200,000:

| | with the check | within the window | saved per checkout |
|---|---|---|---|
| serial, one connection | 868 ns, 4 allocs, 72 B | 402 ns, 1 alloc, 32 B | 466 ns, 3 allocs, 40 B |
| 10 goroutines, 10 connections | 1,391 ns | 883 ns | 508 ns of wall time |

The server's `redis_conn_check_test.go` runs the same cases against the local
Redis through the server's pools' options, with `CLIENT KILL` closing the
pooled connection, and `BenchmarkRedisConnCheck` measures the per-command
saving.

Every main module that wants the change must replace
`github.com/redis/go-redis/v9` with this directory; Go does not inherit
replacements. The server module does.
