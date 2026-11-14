// This bridge exists only in the root test variant. External consumer tests
// compose the public controller with the existing owned pgx wire fixture.
package server

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Owns the first actual UPDATE reply; all SQL and identities are synthetic.
type TestingSupervisorUpdateWire struct {
	Arrived <-chan struct{}
	Release func(bool)
	Queries func() []string
}

// No production API is exported for private fixture access. The callback must
// join all controller users before returning; this is not a parallel scope.
func Testing_WithSupervisorUpdateWire(t *testing.T, expiry time.Time, configure func(*pgxpool.Config), run func(TestingSupervisorUpdateWire)) {
	t.Helper()
	arrived, reply := make(chan struct{}), make(chan bool, 1)
	var firstUpdate, releaseOnce sync.Once
	release := func(ok bool) { releaseOnce.Do(func() { reply <- ok }) }
	fixture, pool := newPgPoolWireFixture(t, func(_ int, sql string) bool {
		if strings.HasPrefix(sql, "UPDATE competition_supervisor_lease SET") {
			held := false
			firstUpdate.Do(func() { held = true; close(arrived) })
			if held {
				return <-reply
			}
		}
		return true
	}, neverPingTestPool, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		fixture.queryRows = func(_ int, sql string) ([]pgproto3.FieldDescription, [][][]byte) {
			if strings.HasPrefix(sql, "SELECT lease_expires_at FROM competition_supervisor_lease") {
				return []pgproto3.FieldDescription{{Name: []byte("lease_expires_at"), DataTypeOID: 1184}},
					[][][]byte{{[]byte(expiry.Format("2006-01-02 15:04:05.999999999Z07:00"))}}
			}
			if strings.HasPrefix(sql, "SELECT EXISTS(SELECT 1 FROM competition_supervisor_lease") {
				return []pgproto3.FieldDescription{{Name: []byte("exists"), DataTypeOID: 16}}, [][][]byte{{[]byte("t")}}
			}
			return nil, nil
		}
		if configure != nil {
			configure(config)
		}
	})
	t.Cleanup(func() { release(false) })
	prior := safePool
	safePool = pool
	defer func() { safePool = prior }()
	run(TestingSupervisorUpdateWire{Arrived: arrived, Release: release, Queries: func() []string {
		fixture.stateLock.Lock()
		defer fixture.stateLock.Unlock()
		return append([]string(nil), fixture.queries...)
	}})
}
