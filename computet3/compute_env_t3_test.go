//go:build sidecar_pending

package computet3

// The ledger tests' shared env (BAL-M12-01): server.DefaultTestEnv's database of the test's own, every migration
// applied, MG-C1 among them under sidecar_pending, and its Redis database of the test's own, reached as sidecar/model
// reaches them in production, through an operator.Env: server/core's Env over a ServiceHost whose Redis is
// sidecarhost's adapter (production's Redis code, CROSSSLOT refused before Redis is asked), with the Db granted.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
	"github.com/urnetwork/server/sidecarhost"
)

// One ledger test's stores.
type computeT3 struct {
	t   testing.TB
	ctx context.Context
	// What the ledger's functions take first (owner decision 594).
	env   operator.Env
	db    operator.Db
	redis operator.Redis
}

// Runs callback in a database and a Redis database of its own, MG-C1 applied. A binary that links no MG-C1, one built
// without sidecar_pending, fails at once rather than testing nothing.
func runComputeT3(t *testing.T, callback func(t3 *computeT3)) {
	if !server.SidecarMgC1Linked() {
		t.Fatal("this binary links no MG-C1: build it under sidecar_pending")
	}
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		redis := sidecarhost.NewRedis(nil)
		env, err := core.NewEnv(&sidecar.ServiceHost{Redis: redis}, &core.Grants{Db: true})
		if err != nil {
			tb.Fatal(err)
		}
		db, err := env.Db()
		if err != nil {
			tb.Fatal(err)
		}
		callback(&computeT3{t: tb, ctx: context.Background(), env: env, db: db, redis: redis})
	})
}

// Runs one statement on its own connection, committed alone, answering its error.
func (self *computeT3) exec(sql string, args ...any) error {
	return self.db.Db(self.ctx, func(conn operator.DbConn) error {
		_, err := conn.Exec(self.ctx, sql, args...)
		return err
	}, operator.DbReadWrite, operator.DbRetryNone)
}

// Runs one statement, failing the test on its error.
func (self *computeT3) mustExec(sql string, args ...any) {
	self.t.Helper()
	if err := self.exec(sql, args...); err != nil {
		self.t.Fatalf("%s: %v", sql, err)
	}
}

// One int64 a query selects, failing the test where it selects none.
func (self *computeT3) queryInt(sql string, args ...any) int64 {
	self.t.Helper()
	var value int64
	found := false
	err := self.db.Db(self.ctx, func(conn operator.DbConn) error {
		rows, err := conn.Query(self.ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		if rows.Next() {
			found = true
			if err := rows.Scan(&value); err != nil {
				return err
			}
		}
		return rows.Err()
	})
	if err != nil {
		self.t.Fatalf("%s: %v", sql, err)
	}
	if !found {
		self.t.Fatalf("%s selects no row", sql)
	}
	return value
}

// Whether err is PostgreSQL's refusal with SQLSTATE code: 23505 a unique key, 23514 a check, 23P01 an exclusion,
// 23502 a NULL in a NOT NULL column.
func computeT3SqlState(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
