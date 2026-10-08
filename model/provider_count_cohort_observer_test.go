// Driver completion observes the actual export queries without privileged diagnostics.
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Only matching query completions are retained; arguments never leave pgx.
type cohortQueryObserver struct {
	stateLock sync.Mutex
	calls     int64
	rows      int64
	unscoped  int64
	inflight  int64
	queryErr  error
}

// An instance-specific context key prevents overlapping observers from mixing state.
func (self *cohortQueryObserver) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.HasPrefix(strings.TrimSpace(data.SQL), "WITH failed_reliability") {
		if ctx.Value(self) != nil {
			return context.WithValue(ctx, self, nil)
		}
		return ctx
	}
	self.stateLock.Lock()
	self.inflight++
	self.stateLock.Unlock()
	return context.WithValue(ctx, self, strings.Contains(data.SQL, "ANY($1)"))
}

// A failed or incomplete statement cannot masquerade as a successful zero-row read.
func (self *cohortQueryObserver) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	scoped, matched := ctx.Value(self).(bool)
	if !matched {
		return
	}
	queryErr := data.Err
	if queryErr == nil && !data.CommandTag.Select() {
		queryErr = fmt.Errorf("cohort query completed without a select command tag")
	}
	rows := data.CommandTag.RowsAffected()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.inflight--
	self.calls++
	self.rows += rows
	if !scoped {
		self.unscoped++
	}
	self.queryErr = errors.Join(self.queryErr, queryErr)
}

// The caller closes its traced pool before reading this terminal snapshot.
func (self *cohortQueryObserver) snapshot() (calls, rows, unscoped int64, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	err = self.queryErr
	if self.inflight != 0 {
		err = errors.Join(err, fmt.Errorf("cohort observation has %d incomplete queries", self.inflight))
	}
	return self.calls, self.rows, self.unscoped, err
}

// Register cleanup in the active fixture frame, including Fatal/Goexit paths.
func cohortObserveQueries(t testing.TB, ctx context.Context) (*cohortQueryObserver, func()) {
	t.Helper()
	observer := &cohortQueryObserver{}
	scope, err := server.NewTestPgQueryScope(ctx, observer)
	if err != nil {
		t.Fatal(err)
	}
	return observer, func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}
}
