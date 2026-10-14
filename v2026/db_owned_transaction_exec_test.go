// Unnamed admission keeps exact key/backend custody while removing cold
// statement preparation and the separate backend-identity query.
package server

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The execution mode cannot bypass the existing isolation and replay guards.
func TestTryTxOwnershipExecRejectsDefaultAndRetryingTransactions(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		key := NewPgOwnershipKey("synthetic-exec-refusal", NewId())
		events := 0
		ctx = Testing_WithPgOwnershipObservation(ctx, func(PgOwnershipEvent) { events++ })
		for _, test := range []struct {
			name    string
			options []any
		}{
			{name: "default isolation", options: []any{OptNoRetry()}},
			{name: "read-only default isolation", options: []any{pgx.ReadOnly, OptNoRetry()}},
			{name: "retrying read committed", options: []any{TxReadCommitted}},
		} {
			refused := false
			Tx(ctx, func(tx PgTx) {
				admitted, err := TryTxOwnershipExec(ctx, tx, []PgOwnershipKey{key})
				refused = !admitted && err != nil && !TxOwnsKeys(tx, []PgOwnershipKey{key})
			}, test.options...)
			if !refused || events != 0 {
				t.Fatal(test.name, "unnamed admission bypassed its transaction guard", refused, events)
			}
		}
	})
}

// Actual PostgreSQL decodes the two integer arrays and returns the transaction
// backend for every chunk. Commit and rollback retain ordinary close custody.
func TestTryTxOwnershipExecRealChunksCommitAndRollback(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		keys := make([]PgOwnershipKey, pgOwnershipQueryLimit+3)
		for index := range keys {
			keys[index] = NewPgOwnershipKey("synthetic-exec-chunks", NewId())
		}
		keys = normalizePgOwnershipKeys(keys)
		if len(keys) != pgOwnershipQueryLimit+3 {
			t.Fatal("synthetic ownership fixture lost a distinct key")
		}
		firsts, seconds := make([]int32, len(keys)), make([]int32, len(keys))
		for index, key := range keys {
			firsts[index], seconds[index] = key.first, key.second
		}
		var events []PgOwnershipEvent
		var reruns atomic.Int32
		ctx = Testing_WithPgOwnershipObservation(ctx, func(event PgOwnershipEvent) { events = append(events, event) })
		ctx = Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		var counter TxCommitCounter
		failure := errors.New("synthetic exec rollback after financial write")
		for index, rollback := range []bool{false, true} {
			var retired PgTx
			var actualBackendPid uint32
			beforeEvents := len(events)
			got := captureDbErrorPanic(func() {
				Tx(ctx, func(tx PgTx) {
					retired = tx
					admitted, err := TryTxOwnershipExec(ctx, tx, append(slices.Clone(keys), keys[0]))
					Raise(err)
					if !admitted || !TxOwnsKeys(tx, keys) {
						panic(errors.New("complete unnamed ownership scope was not admitted"))
					}
					var held int
					Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid(),count(*)
 FROM unnest($1::integer[],$2::integer[]) AS requested(first,second)
 JOIN pg_locks AS held_lock ON held_lock.locktype='advisory' AND held_lock.pid=pg_backend_pid()
 AND held_lock.classid=(requested.first::bigint & 4294967295)::oid
 AND held_lock.objid=(requested.second::bigint & 4294967295)::oid
 AND held_lock.objsubid=2 AND held_lock.granted`, firsts, seconds).Scan(&actualBackendPid, &held))
					if held != len(keys) || len(events) != beforeEvents+1 || events[beforeEvents].BackendPid != actualBackendPid {
						panic(errors.New("admission did not retain its complete key set on the actual transaction backend"))
					}
					admitted, err = TryTxOwnershipExec(ctx, tx, keys[:1])
					Raise(err)
					if !admitted || len(events) != beforeEvents+1 {
						panic(errors.New("owned subset created another admission interval"))
					}
					RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES($1,37)`, index+1))
					if !AddTxCommitCount(tx, &counter, 1) {
						panic(errors.New("unnamed transaction lost commit counter custody"))
					}
					if rollback {
						panic(failure)
					}
				}, TxReadCommitted, OptNoRetry())
			})
			if (!rollback && got != nil) || (rollback && got != failure) || TxOwnsKeys(retired, keys) || len(events) != beforeEvents+2 {
				t.Fatal("unnamed admission changed confirmed end semantics", rollback, got, events)
			}
			for offset, kind := range []PgOwnershipEventKind{PgOwnershipAdmitted, PgOwnershipReleased} {
				event := events[beforeEvents+offset]
				if event.Kind != kind || !event.TransactionScoped || event.BackendPid == 0 || event.BackendPid != actualBackendPid || !slices.Equal(event.Keys, keys) {
					t.Fatal("unnamed ownership observation lost actual backend or complete keys", event)
				}
			}
		}
		requireTxCommitSnapshot(t, &counter, 1, 0, 0)
		if reruns.Load() != 0 {
			t.Fatal("unnamed ownership reran financial work", reruns.Load())
		}
		Db(ctx, func(conn PgConn) {
			var exact bool
			Raise(conn.QueryRow(ctx, `SELECT count(*)=1 AND min(id)=1 AND sum(amount)=37 FROM owned_tx_effect`).Scan(&exact))
			if !exact {
				t.Fatal("commit or rollback changed exact financial effects")
			}
		}, OptNoRetry())
	})
}

// A later refused chunk must release every earlier partial key. The held actor
// owns its own connection; the caller never retains a checkout across a probe.
func TestTryTxOwnershipExecLateChunkRefusalReleasesEarlierKeys(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		keys := make([]PgOwnershipKey, pgOwnershipQueryLimit+3)
		for index := range keys {
			keys[index] = NewPgOwnershipKey("synthetic-exec-held-chunk", NewId())
		}
		keys = normalizePgOwnershipKeys(keys)
		if len(keys) != pgOwnershipQueryLimit+3 {
			t.Fatal("synthetic chunk refusal needs every distinct key")
		}
		held := keys[len(keys)-1]
		ready, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		releaseHolder := func() { once.Do(func() { close(release) }) }
		holder := startOwnedTransactionTest(func() {
			Tx(ctx, func(tx PgTx) {
				admitted, err := TryTxOwnership(ctx, tx, []PgOwnershipKey{held})
				Raise(err)
				if !admitted {
					panic(errors.New("synthetic prior owner did not admit its held key"))
				}
				close(ready)
				select {
				case <-release:
				case <-ctx.Done():
					Raise(ctx.Err())
				}
			}, TxReadCommitted, OptNoRetry())
		})
		defer func() {
			releaseHolder()
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			holder.join(t, cleanup)
		}()
		ownedTransactionTestAwait(t, ctx, ready)
		var events []PgOwnershipEvent
		observed := Testing_WithPgOwnershipObservation(ctx, func(event PgOwnershipEvent) { events = append(events, event) })
		Tx(observed, func(tx PgTx) {
			admitted, err := TryTxOwnershipExec(observed, tx, keys)
			Raise(err)
			if admitted || TxOwnsKeys(tx, keys[:1]) {
				panic(errors.New("later refusal authorized an earlier partial scope"))
			}
		}, TxReadCommitted, OptNoRetry())
		if len(events) != 1 || events[0].Kind != PgOwnershipRefused || !events[0].TransactionScoped || events[0].BackendPid == 0 || !slices.Equal(events[0].Keys, keys) {
			t.Fatal("partial refusal created an admitted interval or lost complete backend proof", events)
		}
		Tx(ctx, func(tx PgTx) {
			admitted, err := TryTxOwnership(ctx, tx, keys[:len(keys)-1])
			Raise(err)
			if !admitted {
				panic(errors.New("refused unnamed admission retained an earlier chunk"))
			}
		}, TxReadCommitted, OptNoRetry())
		releaseHolder()
		if err := holder.join(t, ctx); err != nil {
			t.Fatal("held owner did not acknowledge release", err)
		}
		events = nil
		Tx(observed, func(tx PgTx) {
			admitted, err := TryTxOwnershipExec(observed, tx, keys)
			Raise(err)
			if !admitted {
				panic(errors.New("retired last-key owner did not permit exact progress"))
			}
			RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(1,71)`))
		}, TxReadCommitted, OptNoRetry())
		if len(events) != 2 || events[0].Kind != PgOwnershipAdmitted || events[1].Kind != PgOwnershipReleased || events[0].BackendPid != events[1].BackendPid {
			t.Fatal("retired blocker changed confirmed admission custody", events)
		}
		Db(ctx, func(conn PgConn) {
			var exact bool
			Raise(conn.QueryRow(ctx, `SELECT count(*)=1 AND min(id)=1 AND sum(amount)=71 FROM owned_tx_effect`).Scan(&exact))
			if !exact {
				t.Fatal("refused chunks changed or duplicated business work")
			}
		}, OptNoRetry())
	})
}

// Reply seams retain only finite synthetic identity/boolean rows. The real
// PostgreSQL controls above independently establish actual lock semantics.
type ownershipExecTestReply struct {
	backendPid uint32
	admitted   bool
	scanErr    error
}

type ownershipExecTestRows struct {
	PgResult
	replies  []ownershipExecTestReply
	position int
	closed   bool
	closeErr error
}

func (self *ownershipExecTestRows) Next() bool {
	if self.closed || self.position == len(self.replies) {
		self.Close()
		return false
	}
	self.position++
	return true
}

func (self *ownershipExecTestRows) Scan(destinations ...any) error {
	reply := self.replies[self.position-1]
	if reply.scanErr != nil {
		return reply.scanErr
	}
	if len(destinations) != 2 {
		return errors.New("synthetic ownership reply requires identity and admission")
	}
	pid, pidOk := destinations[0].(*uint32)
	admitted, admittedOk := destinations[1].(*bool)
	if !pidOk || !admittedOk {
		return errors.New("synthetic ownership reply changed destination types")
	}
	*pid, *admitted = reply.backendPid, reply.admitted
	return nil
}

func (self *ownershipExecTestRows) Close() { self.closed = true }

func (self *ownershipExecTestRows) Err() error {
	if self.closed {
		return self.closeErr
	}
	return nil
}

type ownershipExecTestQuery func(context.Context, string, ...any) (PgResult, error)

func (self ownershipExecTestQuery) Query(ctx context.Context, sql string, arguments ...any) (PgResult, error) {
	return self(ctx, sql, arguments...)
}

// Validate exact mode, array types, boundaries and identity before supplying a
// reply. Query text is inspected synchronously and never retained or logged.
func ownershipExecTestPage(t testing.TB, sql string, arguments []any, exec bool, keys []PgOwnershipKey, offset int) []ownershipExecTestReply {
	t.Helper()
	if !strings.HasPrefix(sql, "SELECT pg_backend_pid(),pg_try_advisory_xact_lock(") || !strings.Contains(sql, "unnest($1::integer[],$2::integer[])") {
		t.Fatal("admission changed its bounded transactional key query")
	}
	if exec {
		if len(arguments) != 3 || arguments[0] != pgx.QueryExecModeExec {
			t.Fatal("unnamed admission omitted its explicit local execution mode")
		}
		arguments = arguments[1:]
	} else if len(arguments) != 2 {
		t.Fatal("default admission inherited another caller's query mode")
	}
	firsts, firstsOk := arguments[0].([]int32)
	seconds, secondsOk := arguments[1].([]int32)
	count := min(pgOwnershipQueryLimit, len(keys)-offset)
	if !firstsOk || !secondsOk || len(firsts) != count || len(seconds) != count || count == 0 {
		t.Fatal("admission changed exact integer-array chunk bounds", len(firsts), len(seconds), count)
	}
	replies := make([]ownershipExecTestReply, count)
	for index := range replies {
		if firsts[index] != keys[offset+index].first || seconds[index] != keys[offset+index].second {
			t.Fatal("admission probed another key or changed global order", offset+index)
		}
		replies[index] = ownershipExecTestReply{backendPid: 37, admitted: true}
	}
	return replies
}

// Correct pages use one exact reply per key. Refusal on a later page retains
// the actual backend identity; default callers keep their existing argument list.
func TestTryTxOwnershipExecReplyChunksAndDefaultModeRemainExact(t *testing.T) {
	keys := make([]PgOwnershipKey, pgOwnershipQueryLimit+3)
	for index := range keys {
		keys[index] = PgOwnershipKey{first: -1701, second: int32(index + 1)}
	}
	for _, test := range []struct {
		name    string
		exec    bool
		refused bool
	}{
		{name: "unnamed complete", exec: true},
		{name: "unnamed later refusal", exec: true, refused: true},
		{name: "default complete"},
	} {
		var rows []*ownershipExecTestRows
		query := ownershipExecTestQuery(func(ctx context.Context, sql string, arguments ...any) (PgResult, error) {
			replies := ownershipExecTestPage(t, sql, arguments, test.exec, keys, len(rows)*pgOwnershipQueryLimit)
			if test.refused && len(rows) == 1 {
				replies[len(replies)-1].admitted = false
			}
			page := &ownershipExecTestRows{replies: replies}
			rows = append(rows, page)
			return page, nil
		})
		admitted, pid, err := tryPgOwnershipKeyReplies(t.Context(), query, keys, true, 0, test.exec)
		if err != nil || admitted == test.refused || len(rows) != 2 || test.exec && pid != 37 {
			t.Fatal(test.name, "chunked admission changed exact reply custody", admitted, pid, len(rows), err)
		}
		for _, page := range rows {
			if !page.closed || page.position != len(page.replies) {
				t.Fatal(test.name, "admission reused its route before completing every reply")
			}
		}
	}
}

// Missing, extra, malformed or mixed-backend replies cannot authorize a key
// scope. The last row's error is part of admission, even after true rows.
func TestTryTxOwnershipExecRejectsIncompleteOrChangedReplyIdentity(t *testing.T) {
	keys := make([]PgOwnershipKey, pgOwnershipQueryLimit+1)
	for index := range keys {
		keys[index] = PgOwnershipKey{first: -1702, second: int32(index + 1)}
	}
	failure := errors.New("synthetic incomplete ownership reply")
	for _, test := range []struct {
		name      string
		page      int
		mutate    func(*ownershipExecTestRows)
		wantCause error
	}{
		{name: "zero identity", mutate: func(rows *ownershipExecTestRows) { rows.replies[0].backendPid = 0 }},
		{name: "mixed identity within page", mutate: func(rows *ownershipExecTestRows) { rows.replies[1].backendPid = 38 }},
		{name: "mixed identity across pages", page: 1, mutate: func(rows *ownershipExecTestRows) { rows.replies[0].backendPid = 38 }},
		{name: "zero identity after earlier page", page: 1, mutate: func(rows *ownershipExecTestRows) { rows.replies[0].backendPid = 0 }},
		{name: "no replies", mutate: func(rows *ownershipExecTestRows) { rows.replies = nil }},
		{name: "missing reply", mutate: func(rows *ownershipExecTestRows) { rows.replies = rows.replies[:len(rows.replies)-1] }},
		{name: "extra reply", mutate: func(rows *ownershipExecTestRows) {
			rows.replies = append(rows.replies, ownershipExecTestReply{backendPid: 37, admitted: true})
		}},
		{name: "scan error", mutate: func(rows *ownershipExecTestRows) { rows.replies[0].scanErr = failure }, wantCause: failure},
		{name: "late close error", mutate: func(rows *ownershipExecTestRows) { rows.closeErr = failure }, wantCause: failure},
	} {
		var pages []*ownershipExecTestRows
		query := ownershipExecTestQuery(func(ctx context.Context, sql string, arguments ...any) (PgResult, error) {
			page := &ownershipExecTestRows{replies: ownershipExecTestPage(t, sql, arguments, true, keys, len(pages)*pgOwnershipQueryLimit)}
			if len(pages) == test.page {
				test.mutate(page)
			}
			pages = append(pages, page)
			return page, nil
		})
		admitted, _, err := tryPgOwnershipKeyReplies(t.Context(), query, keys, true, 0, true)
		if admitted || err == nil || len(pages) != test.page+1 || test.wantCause != nil && !errors.Is(err, test.wantCause) {
			t.Fatal(test.name, "incomplete reply authorized ownership or lost its cause", admitted, len(pages), err)
		}
		for _, page := range pages {
			if !page.closed {
				t.Fatal(test.name, "rejected ownership reply retained a live reader")
			}
		}
	}
}

// A canceled parent admits no query, and an initial query failure is not a
// successful busy result that a caller could mistake for complete observation.
func TestTryTxOwnershipExecPreservesQueryAndParentErrors(t *testing.T) {
	key := PgOwnershipKey{first: -1703, second: 1}
	failure := errors.New("synthetic admission query failure")
	calls := 0
	query := ownershipExecTestQuery(func(context.Context, string, ...any) (PgResult, error) { calls++; return nil, failure })
	if admitted, _, err := tryPgOwnershipKeyReplies(t.Context(), query, []PgOwnershipKey{key}, true, 0, true); admitted || err != failure || calls != 1 {
		t.Fatal("admission query failure changed ownership or cause", admitted, calls, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if admitted, _, err := tryPgOwnershipKeyReplies(ctx, query, []PgOwnershipKey{key}, true, 0, true); admitted || !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal("canceled admission issued another query or hid parent error", admitted, calls, err)
	}
}

type ownershipExecTestTransaction struct {
	PgTx
	query      PgCanQuery
	pidQueries int
}

func (self *ownershipExecTestTransaction) Query(ctx context.Context, sql string, arguments ...any) (PgResult, error) {
	return self.query.Query(ctx, sql, arguments...)
}

func (self *ownershipExecTestTransaction) QueryRow(context.Context, string, ...any) pgx.Row {
	self.pidQueries++
	return ownershipExecTestPidRow{}
}

func (self *ownershipExecTestTransaction) Rollback(context.Context) error { return nil }

type ownershipExecTestPidRow struct{}

func (ownershipExecTestPidRow) Scan(destinations ...any) error {
	if len(destinations) != 1 {
		return errors.New("synthetic separate identity query has unexpected destinations")
	}
	pid, ok := destinations[0].(*uint32)
	if !ok {
		return errors.New("synthetic separate identity query changed its type")
	}
	*pid = 37
	return nil
}

// The public owner consumes the PID from its one admission query, normalizes
// duplicates, and reuses an admitted subset without preparing or querying again.
func TestTryTxOwnershipExecUsesOnlyItsAdmissionReply(t *testing.T) {
	keys := []PgOwnershipKey{{first: -1704, second: 1}, {first: -1704, second: 2}}
	calls := 0
	query := ownershipExecTestQuery(func(ctx context.Context, sql string, arguments ...any) (PgResult, error) {
		calls++
		return &ownershipExecTestRows{replies: ownershipExecTestPage(t, sql, arguments, true, keys, 0)}, nil
	})
	inner := &ownershipExecTestTransaction{query: query}
	owner := &postCommitPgTx{PgTx: inner, ownershipAllowed: true}
	var events []PgOwnershipEvent
	ctx := Testing_WithPgOwnershipObservation(t.Context(), func(event PgOwnershipEvent) { events = append(events, event) })
	admitted, err := TryTxOwnershipExec(ctx, owner, []PgOwnershipKey{keys[1], keys[0], keys[1]})
	if err != nil || !admitted || calls != 1 || inner.pidQueries != 0 || !TxOwnsKeys(owner, keys) || len(events) != 1 || events[0].BackendPid != 37 {
		t.Fatal("unnamed owner added a separate identity query or lost complete admission", admitted, calls, inner.pidQueries, events, err)
	}
	if admitted, err := TryTxOwnershipExec(ctx, owner, keys[:1]); err != nil || !admitted || calls != 1 || len(events) != 1 {
		t.Fatal("owned subset repeated an admission query", admitted, calls, events, err)
	}
	if admitted, err := TryTxOwnershipExec(ctx, owner, []PgOwnershipKey{{first: -1704, second: 3}}); err == nil || admitted || calls != 1 {
		t.Fatal("unnamed owner expanded its complete key set", admitted, calls, err)
	}
	if err := owner.Rollback(ctx); err != nil || TxOwnsKeys(owner, keys) || len(events) != 2 || events[1].Kind != PgOwnershipReleased || events[1].BackendPid != 37 {
		t.Fatal("acknowledged end lost the observed admission identity", events, err)
	}
}

// Admission through the new entry point still ends with uncertainty when the
// real transaction wrapper cannot obtain a normal commit return.
func TestTryTxOwnershipExecCommitPanicRetainsUncertainty(t *testing.T) {
	keys := []PgOwnershipKey{{first: -1705, second: 1}}
	failure := errors.New("synthetic unnamed owner commit panic")
	calls := 0
	query := ownershipExecTestQuery(func(ctx context.Context, sql string, arguments ...any) (PgResult, error) {
		calls++
		return &ownershipExecTestRows{replies: ownershipExecTestPage(t, sql, arguments, true, keys, 0)}, nil
	})
	inner := &ownershipExecTestTransaction{PgTx: txCommitObservationPanicTx{failure: failure}, query: query}
	owner := &postCommitPgTx{PgTx: inner, ownershipAllowed: true}
	var events []PgOwnershipEvent
	ctx := Testing_WithPgOwnershipObservation(t.Context(), func(event PgOwnershipEvent) { events = append(events, event) })
	admitted, err := TryTxOwnershipExec(ctx, owner, keys)
	if err != nil || !admitted {
		t.Fatal("synthetic unnamed owner did not admit before its commit control", admitted, err)
	}
	got := captureDbErrorPanic(func() { _ = owner.Commit(ctx) })
	if got != failure || calls != 1 || inner.pidQueries != 0 || TxOwnsKeys(owner, keys) || len(events) != 2 ||
		events[0].Kind != PgOwnershipAdmitted || events[1].Kind != PgOwnershipUncertain ||
		events[0].BackendPid != 37 || events[1].BackendPid != 37 {
		t.Fatal("unnamed admission fabricated a confirmed commit release or repeated its scope", got, calls, events)
	}
}
