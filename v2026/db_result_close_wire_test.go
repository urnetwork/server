// A real pgx result receives its row before the final reply times out.
package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The positive row callback and flushed CommandComplete precede the withheld
// ReadyForQuery. Only Close waits for the real query context's deadline.
func TestWithPgResultWireLateDeadlinePreventsNextQuery(t *testing.T) {
	held, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseReply := func() { releaseOnce.Do(func() { close(release) }) }
	fixture, pool := newPgPoolWireFixture(t, nil,
		func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
			fixture.queryRows = func(_ int, query string) ([]pgproto3.FieldDescription, [][][]byte) {
				if query != "SELECT 17" {
					return nil, nil
				}
				return []pgproto3.FieldDescription{{Name: []byte("value"), DataTypeOID: 23, DataTypeSize: 4}}, [][][]byte{{[]byte("17")}}
			}
			fixture.beforeReady = func(_ int, query string) {
				if query == "SELECT 17" {
					close(held)
					<-release
				}
			}
		})
	defer releaseReply()
	setupCtx, cancelSetup := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelSetup()
	conn, err := pool.open().Acquire(setupCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	queryCtx, cancelQuery := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancelQuery()
	done := make(chan struct{})
	var recovered any
	callbacks, continued := 0, false
	go func() {
		defer close(done)
		recovered = captureDbErrorPanic(func() {
			rows, err := conn.Query(queryCtx, "SELECT 17")
			WithPgResult(rows, err, func() {
				if !rows.Next() {
					panic(errors.New("late-reply fixture did not return its row"))
				}
				var value int
				Raise(rows.Scan(&value))
				if value != 17 {
					panic(errors.New("late-reply fixture changed its row"))
				}
				callbacks++
			})
			continued = true
			RaisePgResult(conn.Exec(queryCtx, "SELECT 99"))
		})
	}()
	defer func() {
		cancelQuery()
		releaseReply()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("late-reply result goroutine failed cleanup join")
		}
	}()
	select {
	case <-held:
	case <-done:
		t.Fatal("result ended before its positive final-reply barrier", recovered)
	case <-setupCtx.Done():
		t.Fatal("final-reply fixture did not reach its barrier", setupCtx.Err())
	}
	select {
	case <-done:
	case <-setupCtx.Done():
		t.Fatal("late result deadline did not join", setupCtx.Err())
	}
	cause, _ := recovered.(error)
	if !errors.Is(cause, context.DeadlineExceeded) || callbacks != 1 || continued {
		t.Fatalf("late wire deadline escaped the result helper: deadline=%t callbacks=%d continued=%t cause=%v", errors.Is(cause, context.DeadlineExceeded), callbacks, continued, recovered)
	}
	fixture.stateLock.Lock()
	queries := append([]string(nil), fixture.queries...)
	fixture.stateLock.Unlock()
	for _, query := range queries {
		if query == "SELECT 99" {
			t.Fatal("caller submitted a query after the failed result cleanup")
		}
	}
}
