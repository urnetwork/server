// Observe actual pool custody in a serial completion path. No callback owns a
// second connection while another one is checked out, including final sealing.
package controller

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urnetwork/server/v2026"
)

type competitionCompletionConnectionTrace struct {
	lock                                                                      sync.Mutex
	held                                                                      map[*pgx.Conn]bool
	transactions                                                              map[*pgx.Conn]bool
	acquired, released, nested, unknownRelease, pollReads, transactionalPolls int
}

func (self *competitionCompletionConnectionTrace) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	self.lock.Lock()
	defer self.lock.Unlock()
	if len(self.held) != 0 {
		self.nested++
	}
	return ctx
}
func (self *competitionCompletionConnectionTrace) TraceAcquireEnd(_ context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if data.Err == nil && data.Conn != nil {
		self.held[data.Conn] = true
		self.acquired++
	}
}
func (self *competitionCompletionConnectionTrace) TraceRelease(_ *pgxpool.Pool, data pgxpool.TraceReleaseData) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if !self.held[data.Conn] {
		self.unknownRelease++
	}
	delete(self.held, data.Conn)
	delete(self.transactions, data.Conn)
	self.released++
}
func (self *competitionCompletionConnectionTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	self.lock.Lock()
	defer self.lock.Unlock()
	verb := strings.ToLower(strings.TrimSpace(data.SQL))
	if verb == "begin" || strings.HasPrefix(verb, "begin ") {
		self.transactions[conn] = true
	}
	if verb == "commit" || verb == "rollback" {
		self.transactions[conn] = false
	}
	if strings.HasPrefix(data.SQL, "SELECT r.closes_at,r.automation_sealed_at,r.canceled,") {
		self.pollReads++
		if self.transactions[conn] {
			self.transactionalPolls++
		}
	}
	return ctx
}
func (self *competitionCompletionConnectionTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func TestCompetitionCompletionPgReturnsReadConnectionBeforeFinalSeal(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionAutomationFixture(tb)
		store := newCompetitionCompletionWorkerPgStore(fixture)
		worker, err := newWorkerWithImageDigest(fixture.settings, store, &fakeEvaluator{}, "synthetic-connection-owner", testWorkerImageDigest())
		if err != nil {
			tb.Fatal(err)
		}
		trace := &competitionCompletionConnectionTrace{held: map[*pgx.Conn]bool{}, transactions: map[*pgx.Conn]bool{}}
		scope, err := server.NewTestPgQueryScope(tb.Context(), trace)
		if err != nil {
			tb.Fatal(err)
		}
		defer scope.Close()
		if finished, err := worker.finishEpoch(tb.Context()); finished || err != nil || store.seals.Load() != 0 {
			tb.Fatal("open completion did not remain a read-only poll", err)
		}
		store.clock.Store(fixture.round.ClosesAt.UnixNano())
		if finished, err := worker.finishEpoch(tb.Context()); !finished || err != nil || store.seals.Load() != 1 {
			tb.Fatal("returned poll did not reach the genuine final seal", err)
		}
		trace.lock.Lock()
		defer trace.lock.Unlock()
		if trace.nested != 0 || trace.unknownRelease != 0 || len(trace.held) != 0 || trace.acquired != trace.released || trace.acquired < 5 || trace.pollReads != 2 || trace.transactionalPolls != 0 {
			tb.Fatal("completion polling nested, retained or mis-scoped actual connection custody", trace.acquired, trace.released, trace.nested, trace.pollReads, trace.transactionalPolls)
		}
	})
}
