// Actual recurring-task transactions must publish client scores before unrelated network work.
package work

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// One connected synthetic provider has a complete old history and a recent three-block failure.
type testingReliabilityPublication struct {
	ctx           context.Context
	clientId      server.Id
	oldEnd        time.Time
	newEnd        time.Time
	clientSession *session.ClientSession
}

// Seed through the real history and score writers; only the drain watermark is pinned by the fixture.
func newTestingReliabilityPublication(t testing.TB) *testingReliabilityPublication {
	t.Helper()
	ctx := t.Context()
	newEnd := server.NowUtc().Truncate(model.ReliabilityBlockDuration).Add(-2 * model.ReliabilityBlockDuration)
	oldEnd := newEnd.Add(-10 * model.ReliabilityBlockDuration)
	networkId, clientId := server.NewId(), server.NewId()
	location := &model.Location{LocationType: model.LocationTypeCity, City: "Publication City", Region: "Publication Region", Country: "Publication Country", CountryCode: "zz"}
	model.CreateLocation(ctx, location)
	model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
	connectionId, _, _, addressHash, err := model.ConnectNetworkClient(ctx, clientId, "192.0.2.91:20001", model.CreateNetworkClientHandler(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if err := model.SetConnectionLocation(ctx, connectionId, location.LocationId, &model.ConnectionLocationScores{}); err != nil {
		t.Fatal(err)
	}
	valid := &model.ClientReliabilityStats{ConnectionEstablishedCount: 1, ProvideEnabledCount: 1, ReceiveMessageCount: 1}
	model.AddClientReliabilityStatsRange(ctx, networkId, clientId, addressHash, oldEnd.Add(-12*time.Hour), oldEnd, valid)
	model.UpdateClientLocationReliabilities(ctx, oldEnd.Add(-12*time.Hour), server.NowUtc())
	model.UpdateClientReliabilityScores(ctx, oldEnd, false)
	model.UpdateNetworkReliabilityWindowScores(ctx, oldEnd, false)
	model.AddClientReliabilityStatsRange(ctx, networkId, clientId, addressHash, oldEnd.Add(model.ReliabilityBlockDuration), newEnd, valid)
	model.AddClientReliabilityStatsRange(ctx, networkId, clientId, addressHash, newEnd.Add(-2*model.ReliabilityBlockDuration), newEnd,
		&model.ClientReliabilityStats{ConnectionNewCount: model.ReliabilityAllowDisconnectCountPerBlock + 1})
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_reliability_rollup(singleton_id,max_drained_block,update_time)
			VALUES (1,$1,$2) ON CONFLICT(singleton_id) DO UPDATE SET max_drained_block=EXCLUDED.max_drained_block,update_time=EXCLUDED.update_time`, newEnd.Unix()/60, server.NowUtc()))
	})
	return &testingReliabilityPublication{ctx: ctx, clientId: clientId, oldEnd: oldEnd, newEnd: newEnd, clientSession: session.Testing_CreateClientSession(ctx, nil)}
}

// Reject an exact later write with a named database exception, not a timeout or scheduler race.
func (self *testingReliabilityPublication) installFailure(t testing.TB, table string, predicate string) func() {
	t.Helper()
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, `CREATE FUNCTION testing_reliability_publication_failure() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'synthetic reliability publication failure' USING ERRCODE='ZX001'; END $$`))
		server.RaisePgResult(tx.Exec(self.ctx, fmt.Sprintf(`CREATE TRIGGER testing_reliability_publication_failure
			BEFORE INSERT OR UPDATE ON %s FOR EACH ROW %s EXECUTE FUNCTION testing_reliability_publication_failure()`, table, predicate)))
	})
	removed := false
	return func() {
		if !removed {
			server.Tx(self.ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(self.ctx, `DROP FUNCTION testing_reliability_publication_failure() CASCADE`))
			})
			removed = true
		}
	}
}

// The real evaluator reports SQL failure by panic; preserve the specific causal error.
func (self *testingReliabilityPublication) runFailure(t testing.TB) {
	t.Helper()
	failed := false
	func() {
		defer func() {
			if failure := recover(); failure != nil {
				failed = true
				if !strings.Contains(fmt.Sprint(failure), "synthetic reliability publication failure") {
					t.Fatalf("unexpected task failure: %v", failure)
				}
			}
		}()
		_, err := UpdateReliabilities(&UpdateReliabilitiesArgs{MinTime: self.oldEnd}, self.clientSession)
		if err != nil {
			t.Fatalf("unexpected returned task error: %v", err)
		}
	}()
	if !failed {
		t.Fatal("the exact injected publication boundary was never reached")
	}
}

// Bounds and changed weights distinguish a real publish from merely advancing running markers.
func (self *testingReliabilityPublication) assertScores(t testing.TB, fresh bool) {
	t.Helper()
	wantEnd := self.oldEnd.Unix()/60 + 1
	if fresh {
		wantEnd = self.newEnd.Unix()/60 + 1
	}
	seen := 0
	server.Db(self.ctx, func(conn server.PgConn) {
		rows, err := conn.Query(self.ctx, `SELECT lookback_index,min_block_number,max_block_number,
			independent_reliability_score,independent_reliability_weight,reliability_score,reliability_weight
			FROM client_connection_reliability_score WHERE client_id=$1 ORDER BY lookback_index`, self.clientId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var index int
				var minBlock, maxBlock int64
				var count, weight, normalizedCount, normalizedWeight float64
				server.Raise(rows.Scan(&index, &minBlock, &maxBlock, &count, &weight, &normalizedCount, &normalizedWeight))
				if index != seen || index >= 3 {
					t.Fatal("unexpected client score index")
				}
				width := [3]int64{6, 61, 721}[index]
				wantCount := float64(width)
				if fresh {
					wantCount -= 3
				}
				wantWeight := wantCount / float64(width)
				if minBlock != wantEnd-width || maxBlock != wantEnd || count != wantCount || normalizedCount != wantCount || math.Abs(weight-wantWeight) > 1e-9 || math.Abs(normalizedWeight-wantWeight) > 1e-9 {
					t.Errorf("index %d fresh=%t: bounds=[%d,%d) count=%g weight=%g; want [%d,%d) count=%g weight=%g", index, fresh, minBlock, maxBlock, count, weight, wantEnd-width, wantEnd, wantCount, wantWeight)
				}
				seen++
			}
		})
	})
	if seen != 3 {
		t.Fatalf("score publication contains %d windows, want three", seen)
	}
}

// Check every checkpoint independently; advancing one cannot stand in for publishing its siblings.
func (self *testingReliabilityPublication) assertRunning(t testing.TB, freshIndices map[int]bool) {
	t.Helper()
	seen := 0
	server.Db(self.ctx, func(conn server.PgConn) {
		rows, err := conn.Query(self.ctx, `SELECT lookback_index,max_block_number FROM client_reliability_running_window ORDER BY lookback_index`)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var index int
				var maxBlock int64
				server.Raise(rows.Scan(&index, &maxBlock))
				wantMax := self.oldEnd.Unix()/60 + 1
				if freshIndices[index] {
					wantMax = self.newEnd.Unix()/60 + 1
				}
				if maxBlock != wantMax {
					t.Errorf("running index %d max=%d, want %d", index, maxBlock, wantMax)
				}
				seen++
			}
		})
	})
	if seen != 4 {
		t.Fatalf("running windows=%d, want all four", seen)
	}
}

// The seven-day checkpoint may fail after the three client windows have durably published.
func TestUpdateReliabilitiesPublishesBeforeNetworkCheckpointFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newTestingReliabilityPublication(t)
		defer fixture.clientSession.Cancel()
		remove := fixture.installFailure(t, "client_reliability_running_window", "WHEN (NEW.lookback_index=1000)")
		defer remove()
		fixture.runFailure(t)
		fixture.assertRunning(t, map[int]bool{0: true, 1: true, 2: true})
		fixture.assertScores(t, true)
		remove()
		if _, err := UpdateReliabilities(&UpdateReliabilitiesArgs{MinTime: fixture.oldEnd}, fixture.clientSession); err != nil {
			t.Fatal(err)
		}
		fixture.assertScores(t, true)
		fixture.assertRunning(t, map[int]bool{0: true, 1: true, 2: true, 1000: true})
	})
}

// A later network-score write must not roll back or delay independent client score publication.
func TestUpdateReliabilitiesPublishesBeforeNetworkScoreFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newTestingReliabilityPublication(t)
		defer fixture.clientSession.Cancel()
		remove := fixture.installFailure(t, "network_connection_reliability_window_score", "")
		defer remove()
		fixture.runFailure(t)
		fixture.assertRunning(t, map[int]bool{0: true, 1: true, 2: true, 1000: true})
		fixture.assertScores(t, true)
	})
}

// A failed client checkpoint retains earlier running work but cannot relabel old scores as current.
func TestUpdateReliabilitiesFailedClientCheckpointDoesNotPublish(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newTestingReliabilityPublication(t)
		defer fixture.clientSession.Cancel()
		remove := fixture.installFailure(t, "client_reliability_running_window", "WHEN (NEW.lookback_index=1)")
		defer remove()
		fixture.runFailure(t)
		fixture.assertRunning(t, map[int]bool{0: true})
		fixture.assertScores(t, false)
	})
}

// Failure at the second client score leaves all three published windows in their old generation.
func TestUpdateReliabilitiesClientScorePublicationIsAtomic(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newTestingReliabilityPublication(t)
		defer fixture.clientSession.Cancel()
		remove := fixture.installFailure(t, "client_connection_reliability_score", "WHEN (NEW.lookback_index=1)")
		defer remove()
		fixture.runFailure(t)
		fixture.assertScores(t, false)
	})
}
