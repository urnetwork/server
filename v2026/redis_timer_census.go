package server

import (
	"context"
	"net"
	"syscall"
	"time"
)

// The socket of a pooled Redis connection, counting the deadlines go-redis
// sets on it in the timer census (timer_census.go). go-redis sets a write and
// a read deadline for every command it sends: the earlier of the command
// context's deadline and the pool's WriteTimeout or ReadTimeout, from a clock
// it caches for up to 50 ms (pool.Conn.deadline). A checkout's socket check
// clears both. The deadlines and the I/O are the socket's, unchanged.
//
// go-redis checks a pooled socket through syscall.Conn and reads a socket it
// can't check differently, so a socket that has it is wrapped by
// redisTimerCensusSyscallConn, which keeps it. NetConn unwraps the socket, as
// go-redis unwraps a tls.Conn.
type redisTimerCensusConn struct {
	net.Conn
}

// Both deadlines: one count at each site.
func (self *redisTimerCensusConn) SetDeadline(deadline time.Time) error {
	CountTimerDeadline(TimerCensusSiteRedisReadDeadline, deadline)
	CountTimerDeadline(TimerCensusSiteRedisWriteDeadline, deadline)
	return self.Conn.SetDeadline(deadline)
}

func (self *redisTimerCensusConn) SetReadDeadline(deadline time.Time) error {
	CountTimerDeadline(TimerCensusSiteRedisReadDeadline, deadline)
	return self.Conn.SetReadDeadline(deadline)
}

func (self *redisTimerCensusConn) SetWriteDeadline(deadline time.Time) error {
	CountTimerDeadline(TimerCensusSiteRedisWriteDeadline, deadline)
	return self.Conn.SetWriteDeadline(deadline)
}

// The socket under the census.
func (self *redisTimerCensusConn) NetConn() net.Conn {
	return self.Conn
}

// A counted socket go-redis can still check (connCheck).
type redisTimerCensusSyscallConn struct {
	redisTimerCensusConn
	syscallConn syscall.Conn
}

func (self *redisTimerCensusSyscallConn) SyscallConn() (syscall.RawConn, error) {
	return self.syscallConn.SyscallConn()
}

// Counts the deadlines on conn, keeping every interface go-redis asserts on it.
func newRedisTimerCensusConn(conn net.Conn) net.Conn {
	if syscallConn, ok := conn.(syscall.Conn); ok {
		return &redisTimerCensusSyscallConn{
			redisTimerCensusConn: redisTimerCensusConn{Conn: conn},
			syscallConn:          syscallConn,
		}
	}
	return &redisTimerCensusConn{Conn: conn}
}

// A pool's dial whose sockets count their deadlines.
func redisTimerCensusDialer(dial redisDialFunc) redisDialFunc {
	return func(ctx context.Context, network string, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil || conn == nil {
			return conn, err
		}
		return newRedisTimerCensusConn(conn), nil
	}
}
