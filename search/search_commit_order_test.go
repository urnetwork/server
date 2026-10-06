package search

// The update poll reads a record of the update log once its transaction has
// finished, in (xid, update id) order, and never waits on a transaction that
// is still in progress (see SearchUpdatePosition). Each test drives the poll
// step (pollUpdates) directly on a search with no update loop, and holds a
// transaction open with a barrier on its own connection, so the interleaving
// is fixed without sleeps or the poll's timer.

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"

	"github.com/urnetwork/server"
)

// A local search over the realm's database index with no update loop: the
// test calls its poll step, and it answers queries from memory.
func pollTestSearch(ctx context.Context, realm string) (*SearchLocal, *SearchDb) {
	searchDb := NewSearchDb(realm, SearchTypeFull)
	cancelCtx, cancel := context.WithCancel(ctx)
	initialSync, initialSyncDone := context.WithCancel(cancelCtx)
	initialSyncDone()
	searchLocal := &SearchLocal{
		ctx:                       cancelCtx,
		cancel:                    cancel,
		impl:                      searchDb,
		settings:                  DefaultSearchLocalSettings(),
		initialSync:               initialSync,
		valueIdVariantProjections: map[server.Id]map[int]*localProjection{},
	}
	return searchLocal, searchDb
}

// A transaction held open on its own connection: write runs in it, the
// transaction then waits for end, and rolls back instead of committing when
// rollback is set. Its id (xid) is read after write.
type heldSearchTx struct {
	written chan struct{}
	release chan struct{}
	ended   chan struct{}
	xid     uint64
	err     error
}

// The error a held transaction raises to roll back.
var errHeldSearchTxRollback = errors.New("held transaction rolled back")

// Starts the transaction and returns once write has run in it.
func holdSearchTx(t testing.TB, ctx context.Context, write func(tx server.PgTx), rollback bool) *heldSearchTx {
	held := &heldSearchTx{
		written: make(chan struct{}),
		release: make(chan struct{}),
		ended:   make(chan struct{}),
	}
	go func() {
		defer close(held.ended)
		defer func() {
			if r := recover(); r != nil {
				if err, ok := r.(error); ok && errors.Is(err, errHeldSearchTxRollback) {
					return
				}
				held.err = errors.New("the held transaction failed")
			}
		}()
		server.Tx(ctx, func(tx server.PgTx) {
			write(tx)
			var xidText string
			server.Raise(tx.QueryRow(ctx, `SELECT pg_current_xact_id()::text`).Scan(&xidText))
			xid, err := strconv.ParseUint(xidText, 10, 64)
			server.Raise(err)
			held.xid = xid
			close(held.written)
			<-held.release
			if rollback {
				panic(errHeldSearchTxRollback)
			}
		})
	}()
	select {
	case <-held.written:
	case <-held.ended:
		t.Fatalf("the held transaction ended before it wrote: %v", held.err)
	}
	return held
}

// Lets the transaction commit (or roll back) and waits until it has ended.
// Safe to call again, from the same goroutine.
func (self *heldSearchTx) End() {
	select {
	case <-self.release:
	default:
		close(self.release)
	}
	<-self.ended
}

// Whether the search holds the value id under exactly the value.
func searchHolds(ctx context.Context, searchLocal *SearchLocal, value string, valueId server.Id) bool {
	_, ok := searchLocal.AroundIds(ctx, value, 0)[valueId]
	return ok
}

// The update id of the value's record in the realm's update log.
func searchUpdateIdOf(ctx context.Context, realm string, valueId server.Id) (updateId int64) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT update_id FROM search_value_update WHERE realm = $1 AND value_id = $2`,
			realm,
			valueId,
		).Scan(&updateId))
	})
	return
}

// Two transactions write to the update log: the first takes the lower update
// id and stays open, the second takes the higher id and commits. A poll runs
// past the second's record; the first then commits, and the next poll must
// read its record. Read by update id alone, the poll had moved past the
// first's id and never read it, so its value stayed out of this process's
// index.
func TestSearchLocalPollReadsRecordCommittedAfterALaterOne(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		realm := "commit_order"
		searchLocal, searchDb := pollTestSearch(ctx, realm)
		defer searchLocal.Close()
		firstValueId := server.NewId()
		secondValueId := server.NewId()

		first := holdSearchTx(t, ctx, func(tx server.PgTx) {
			searchDb.AddInTx(ctx, "first value", firstValueId, 0, tx)
		}, false)
		defer first.End()
		searchDb.Add(ctx, "second value", secondValueId, 0)

		searchLocal.pollUpdates(searchLocalUpdateLimit)
		if searchHolds(ctx, searchLocal, "first value", firstValueId) {
			t.Fatalf("the poll read a record of a transaction still in progress")
		}

		first.End()
		if first.err != nil {
			t.Fatal(first.err)
		}
		// the premise: the first record took the lower update id
		if firstUpdateId, secondUpdateId := searchUpdateIdOf(ctx, realm, firstValueId), searchUpdateIdOf(ctx, realm, secondValueId); secondUpdateId <= firstUpdateId {
			t.Fatalf("the first record has update id %d and the second %d, want the first lower", firstUpdateId, secondUpdateId)
		}
		searchLocal.pollUpdates(searchLocalUpdateLimit)
		if !searchHolds(ctx, searchLocal, "first value", firstValueId) {
			t.Fatalf("the record committed after a later one was never read")
		}
		if !searchHolds(ctx, searchLocal, "second value", secondValueId) {
			t.Fatalf("the later record was not read")
		}
	})
}

// A transaction that writes a record and rolls back leaves its update id as a
// permanent gap. The poll waits on no transaction: a record committed after
// the gap is read at once, the position stops listing the transaction once it
// has ended, and records written later are read as usual.
func TestSearchLocalPollPassesRolledBackRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		searchLocal, searchDb := pollTestSearch(ctx, "rolled_back")
		defer searchLocal.Close()
		rolledBackValueId := server.NewId()
		committedValueId := server.NewId()
		laterValueId := server.NewId()

		rolledBack := holdSearchTx(t, ctx, func(tx server.PgTx) {
			searchDb.AddInTx(ctx, "rolled back value", rolledBackValueId, 0, tx)
		}, true)
		defer rolledBack.End()
		searchDb.Add(ctx, "committed value", committedValueId, 0)

		searchLocal.pollUpdates(searchLocalUpdateLimit)
		if !searchHolds(ctx, searchLocal, "committed value", committedValueId) {
			t.Fatalf("the record committed after an open transaction's record was not read")
		}
		if !slices.Contains(searchLocal.updatePosition.inProgressXids, rolledBack.xid) {
			t.Fatalf("the position lists %v, want the open transaction %d below it", searchLocal.updatePosition.inProgressXids, rolledBack.xid)
		}

		rolledBack.End()
		if rolledBack.err != nil {
			t.Fatal(rolledBack.err)
		}
		searchLocal.pollUpdates(searchLocalUpdateLimit)
		if searchHolds(ctx, searchLocal, "rolled back value", rolledBackValueId) {
			t.Fatalf("the poll read a rolled-back record")
		}
		if slices.Contains(searchLocal.updatePosition.inProgressXids, rolledBack.xid) {
			t.Fatalf("the position still lists the rolled-back transaction %d", rolledBack.xid)
		}

		searchDb.Add(ctx, "later value", laterValueId, 0)
		searchLocal.pollUpdates(searchLocalUpdateLimit)
		if !searchHolds(ctx, searchLocal, "later value", laterValueId) {
			t.Fatalf("a record written after the gap was not read")
		}
	})
}

// A transaction that is still open and writes no search record holds nothing
// back: a record committed after it started is read at once.
func TestSearchLocalPollReadsPastAnOpenUnrelatedTransaction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		searchLocal, searchDb := pollTestSearch(ctx, "unrelated_open")
		defer searchLocal.Close()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE unrelated_write (id bigint NOT NULL)`))
		})
		valueId := server.NewId()

		unrelated := holdSearchTx(t, ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO unrelated_write (id) VALUES (1)`))
		}, false)
		defer unrelated.End()
		searchDb.Add(ctx, "committed value", valueId, 0)

		searchLocal.pollUpdates(searchLocalUpdateLimit)
		if !searchHolds(ctx, searchLocal, "committed value", valueId) {
			t.Fatalf("an open transaction with no search record held back a committed record")
		}
		unrelated.End()
		if unrelated.err != nil {
			t.Fatal(unrelated.err)
		}
		searchLocal.pollUpdates(searchLocalUpdateLimit)
		if slices.Contains(searchLocal.updatePosition.inProgressXids, unrelated.xid) {
			t.Fatalf("the position still lists the ended transaction %d", unrelated.xid)
		}
	})
}

// One transaction writes more records than one poll reads; the next poll goes
// on within the transaction from where the first stopped.
func TestSearchLocalPollContinuesWithinATransaction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		searchLocal, searchDb := pollTestSearch(ctx, "continue_within")
		defer searchLocal.Close()
		values := []string{"value one", "value two", "value three", "value four", "value five"}
		valueIds := []server.Id{}
		server.Tx(ctx, func(tx server.PgTx) {
			valueIds = []server.Id{}
			for _, value := range values {
				valueId := server.NewId()
				searchDb.AddInTx(ctx, value, valueId, 0, tx)
				valueIds = append(valueIds, valueId)
			}
		})

		for range 3 {
			searchLocal.pollUpdates(2)
		}
		for i, value := range values {
			if !searchHolds(ctx, searchLocal, value, valueIds[i]) {
				t.Errorf("%q was not read across polls of 2 records", value)
			}
		}
	})
}
