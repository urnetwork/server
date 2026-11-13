// Packet work carries an explicit PostgreSQL boundary. The guard is inherited
// by child and detached contexts, and refuses before a pool or credential opens.
package server

import (
	"context"
	"errors"
	"sync/atomic"
)

// Identifies a forbidden data-path dependency rather than a database outage.
var ErrPacketPostgres = errors.New("postgres is forbidden on packet flow")

type packetPostgresKey struct{}

type packetPostgresGuard struct {
	attempts atomic.Uint64
}

type packetPostgresTestPermitKey struct{}

// Only the native test helper can install this permit for its current oracle.
// State 0 is unused, 1 consumed and 2 revoked; one atomic transition admits at
// most one acquisition. A normal packet context remains forbidden regardless.
type packetPostgresTestPermit struct {
	guard *packetPostgresGuard
	state atomic.Uint32
}

// Installed only by the serial native test helper. It detects fresh Background
// escapes in a packet fixture without imposing a production process boundary.
var packetPostgresTestGuard atomic.Pointer[packetPostgresGuard]

// Marks a packet-only context. Repeated marking preserves the same counter;
// callers keep durable control operations on their separate owning context.
func WithoutPostgres(ctx context.Context) context.Context {
	if _, ok := ctx.Value(packetPostgresKey{}).(*packetPostgresGuard); ok {
		return ctx
	}
	return context.WithValue(ctx, packetPostgresKey{}, &packetPostgresGuard{})
}

// Reports forbidden attempts even when an outer worker handles the panic. It
// supports end-to-end packet regression controls without replacing model calls.
func PacketPostgresAttempts(ctx context.Context) uint64 {
	if guard, ok := ctx.Value(packetPostgresKey{}).(*packetPostgresGuard); ok {
		return guard.attempts.Load()
	}
	return 0
}

// Runs at the shared acquisition boundary for Db, ReplicaDb, Tx and maintenance
// wrappers, and at the separately exposed direct maintenance acquisition.
func checkPostgresAllowed(ctx context.Context) {
	forbidden := false
	if guard := packetPostgresTestGuard.Load(); guard != nil {
		guard.attempts.Add(1)
		permit, ok := ctx.Value(packetPostgresTestPermitKey{}).(*packetPostgresTestPermit)
		forbidden = !ok || permit.guard != guard || !permit.state.CompareAndSwap(0, 1)
	}
	if guard, ok := ctx.Value(packetPostgresKey{}).(*packetPostgresGuard); ok {
		guard.attempts.Add(1)
		forbidden = true
	}
	if forbidden {
		panic(ErrPacketPostgres)
	}
}
