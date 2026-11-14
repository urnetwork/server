package model

// These controls characterize the actual lifecycle writer. They do not remove
// the lock or authorize changing repeated-deactivation timestamp semantics.
import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

func TestDeactivationBatchRetainsEarlierClientWhileLastClientIsHeld(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		networkID, sourceNetworkID := server.NewId(), server.NewId()
		clients := []server.Id{server.NewId(), server.NewId()}
		slices.SortFunc(clients, func(a, b server.Id) int { return a.Cmp(b) })
		sourceID, independentID := server.NewId(), server.NewId()
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{
			clients[0]: networkID, clients[1]: networkID,
			sourceID: sourceNetworkID, independentID: networkID,
		})
		holderConn := acquireContractLifecycleTestConnection(t, ctx)
		defer holderConn.Release()
		holder, err := holderConn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = holder.Rollback(cleanup)
		}()
		var locked server.Id
		if err := holder.QueryRow(ctx, `SELECT client_id FROM network_client WHERE client_id=$1 FOR SHARE`, clients[1]).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		holderPID := contractLifecycleTestBackendPid(t, ctx, holder)

		writerConn := acquireContractLifecycleTestConnection(t, ctx)
		defer writerConn.Release()
		writer, err := writerConn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			t.Fatal(err)
		}
		writerPID := contractLifecycleTestBackendPid(t, ctx, writer)
		writerResult := make(chan contractLifecycleTestResult, 1)
		writerDone := make(chan struct{})
		creationResult := make(chan contractLifecycleTestResult, 1)
		creationDone := make(chan struct{})
		creationStarted := false
		defer func() {
			cancel()
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = holder.Rollback(cleanup)
			select {
			case <-writerDone:
			case <-cleanup.Done():
				t.Error("writer cleanup did not join")
			}
			if creationStarted {
				select {
				case <-creationDone:
				case <-cleanup.Done():
					t.Error("creation cleanup did not join")
				}
			}
		}()
		go func() {
			defer close(writerDone)
			result := contractLifecycleTestResult{}
			defer func() {
				if value := recover(); value != nil {
					result.err = fmt.Errorf("deactivation panic: %v", value)
				}
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				_ = writer.Rollback(cleanup)
				writerResult <- result
			}()
			// Reverse and duplicate inputs exercise the real sort/dedup boundary.
			result.rowCount, result.err = deactivateNetworkClientsInTx(ctx, writer, []server.Id{clients[1], clients[0], clients[0]}, networkID)
			if result.err == nil {
				result.err = writer.Commit(ctx)
			}
		}()
		if got := requireContractLifecycleBlockedBy(t, ctx, holder, holderPID); got != writerPID {
			t.Fatalf("wrong first wait edge: got=%d want=%d", got, writerPID)
		}

		// An independent endpoint completes while the exact writer wait is held.
		independentContract, err := CreateContractNoEscrow(ctx, sourceNetworkID, sourceID, networkID, independentID, 1024)
		if err != nil || independentContract == (server.Id{}) {
			t.Fatalf("independent creation: id=%s err=%v", independentContract, err)
		}
		creationStarted = true
		go func() {
			defer close(creationDone)
			result := contractLifecycleTestResult{}
			defer func() {
				if value := recover(); value != nil {
					result.err = fmt.Errorf("creation panic: %v", value)
				}
				creationResult <- result
			}()
			result.contractId, result.err = CreateContractNoEscrow(ctx, sourceNetworkID, sourceID, networkID, clients[0], 1024)
		}()
		// The first endpoint has no original holder. Its only blocker is the
		// deactivation already waiting for the last endpoint, proving propagation.
		if got := requireContractLifecycleBlockedBy(t, ctx, holder, writerPID); got == holderPID || got == writerPID {
			t.Fatalf("invalid second wait edge: %d", got)
		}
		select {
		case result := <-creationResult:
			t.Fatalf("creation crossed held writer: %+v", result)
		default:
		}
		var release time.Time
		if err := holder.QueryRow(ctx, `SELECT clock_timestamp() AT TIME ZONE 'UTC'`).Scan(&release); err != nil {
			t.Fatal(err)
		}
		if err := holder.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		var written, created contractLifecycleTestResult
		select {
		case written = <-writerResult:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		select {
		case created = <-creationResult:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if written.err != nil || written.rowCount != 2 {
			t.Fatalf("deactivation result: %+v", written)
		}
		if !errors.Is(created.err, ErrContractDestinationInactive) || created.contractId != (server.Id{}) {
			t.Fatalf("creation after deactivation: %+v", created)
		}
		if count := contractLifecycleTestCount(t, ctx, sourceID, clients[0]); count != 0 {
			t.Fatalf("inactive destination contracts=%d", count)
		}
		if count := contractLifecycleTestCount(t, ctx, sourceID, independentID); count != 1 {
			t.Fatalf("independent contracts=%d", count)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var inactive, clockAfter int
			var clockCount int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FILTER(WHERE NOT active),count(*) FILTER(WHERE deactivate_time >= $2),count(DISTINCT deactivate_time) FROM network_client WHERE client_id=ANY($1)`, clients, release).Scan(&inactive, &clockAfter, &clockCount))
			if inactive != 2 || clockAfter != 2 || clockCount != 1 {
				t.Fatalf("lifecycle state: inactive=%d after=%d clocks=%d", inactive, clockAfter, clockCount)
			}
		})
		t.Log("actual held-last-row edge, propagated earlier-row creation wait, independent completion, and post-lock clock observed")
	})
}

func TestDeactivationRepeatedRequestRewritesInactiveRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		networkID, foreignNetworkID := server.NewId(), server.NewId()
		clients := make([]server.Id, 64)
		rows := map[server.Id]server.Id{}
		for i := range clients {
			clients[i] = server.NewId()
			rows[clients[i]] = networkID
		}
		foreign := server.NewId()
		rows[foreign] = foreignNetworkID
		insertContractLifecycleTestClients(t, ctx, rows)
		arguments := append(slices.Clone(clients), clients[0], foreign, server.NewId())
		previousXmin := ""
		var previousTime time.Time
		for attempt := 0; attempt < 3; attempt++ {
			var count int64
			server.Tx(ctx, func(tx server.PgTx) {
				var err error
				count, err = deactivateNetworkClientsInTx(ctx, tx, arguments, networkID)
				server.Raise(err)
			}, server.TxReadCommitted)
			if count != 64 {
				t.Fatalf("attempt%d affected=%d want64", attempt, count)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var xmin string
				var at time.Time
				var tupleGenerations, inactive, timestamps int
				server.Raise(conn.QueryRow(ctx, `SELECT min(xmin::text),count(DISTINCT xmin::text),count(*) FILTER(WHERE NOT active),min(deactivate_time),count(DISTINCT deactivate_time) FROM network_client WHERE client_id=ANY($1)`, clients).Scan(&xmin, &tupleGenerations, &inactive, &at, &timestamps))
				if inactive != 64 || tupleGenerations != 1 || timestamps != 1 || xmin == previousXmin || at.Before(previousTime) {
					t.Fatalf("attempt%d inactive=%d generations=%d clocks=%d new_tuple=%t", attempt, inactive, tupleGenerations, timestamps, xmin != previousXmin)
				}
				previousXmin, previousTime = xmin, at
				var foreignActive bool
				server.Raise(conn.QueryRow(ctx, `SELECT active FROM network_client WHERE client_id=$1`, foreign).Scan(&foreignActive))
				if !foreignActive {
					t.Fatal("foreign network row changed")
				}
			})
		}
		t.Log("three actual requests wrote64 owned tuples each, including two repeats; this characterizes current timestamp semantics")
	})
}
