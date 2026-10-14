package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// Pause the real census at its driver boundary, not at a synthetic row update.
// Only the marked publisher pauses; the independent writer uses a second owner.
type locationProbeCensusPause struct {
	once          sync.Once
	reached       chan struct{}
	resume        chan struct{}
	needles       []string
	publishedRows int64
}

func (self *locationProbeCensusPause) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	needles := self.needles
	if len(needles) == 0 {
		needles = []string{"provider_reliability", "provider_location", "provider_intent_probe_priority"}
	}
	matched := ctx.Value(self) == true
	for _, needle := range needles {
		matched = matched && strings.Contains(data.SQL, needle)
	}
	if matched {
		self.once.Do(func() {
			close(self.reached)
			select {
			case <-self.resume:
			case <-ctx.Done():
			}
		})
	}
	if ctx.Value(self) == true && strings.Contains(data.SQL, "UPDATE provider_egress_probe_cycle AS cycle SET eligible=prepared.eligible") {
		return context.WithValue(ctx, locationProbePublishedRowsKey{self}, true)
	}
	return ctx
}

type locationProbePublishedRowsKey struct{ pause *locationProbeCensusPause }

func (self *locationProbeCensusPause) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(locationProbePublishedRowsKey{self}) == true && data.Err == nil {
		self.publishedRows += data.CommandTag.RowsAffected()
	}
}

func locationProbeCatch(fn func()) (err error) {
	server.HandleError(fn, func(value error) { err = value })
	return
}

func locationProbeConnectedFixture(t testing.TB, ctx context.Context, suffix int) (server.Id, server.Id, *Location) {
	t.Helper()
	network, client := server.NewId(), server.NewId()
	Testing_CreateDevice(ctx, network, server.NewId(), client, "", "")
	city := &Location{LocationType: LocationTypeCity, City: "Lock Census City", Region: "Lock Census Region", Country: "Lock Census Country", CountryCode: "zz"}
	CreateLocation(ctx, city)
	connection, _, _, _, err := ConnectNetworkClient(ctx, client, fmt.Sprintf("192.0.2.%d:20001", suffix), CreateNetworkClientHandler(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if err := SetConnectionLocation(ctx, connection, city.LocationId, &ConnectionLocationScores{}); err != nil {
		t.Fatal(err)
	}
	SetProvide(ctx, client, map[ProvideMode][]byte{ProvideModePublic: []byte("synthetic-public")})
	return network, client, city
}

// On the baseline, the paused full eligibility census retains the earlier
// location upsert's row lock. The replacement computes it before that upsert,
// so the independent exact-row writer can commit while the census is paused.
func TestLocationProbeEligibilityCensusDoesNotRetainLocationRow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		_, client, _ := locationProbeConnectedFixture(t, ctx, 41)
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		pause := &locationProbeCensusPause{reached: make(chan struct{}), resume: make(chan struct{})}
		scope, err := server.NewTestPgQueryScope(ctx, pause)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		done := make(chan error, 1)
		go func() {
			done <- locationProbeCatch(func() {
				UpdateClientLocationReliabilities(context.WithValue(ctx, pause, true), now.Add(-time.Hour), now.Add(time.Minute))
			})
		}()
		select {
		case <-pause.reached:
		case err := <-done:
			t.Fatalf("production eligibility census was not reached: %v", err)
		case <-ctx.Done():
			close(pause.resume)
			<-done
			t.Fatal("production eligibility pause timed out")
		}
		writeErr := locationProbeCatch(func() {
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout='150ms'`))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability
					SET update_block_number=update_block_number WHERE client_id=$1`, client))
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		close(pause.resume)
		if err := <-done; err != nil {
			t.Fatalf("publisher did not join normally after the pause: %v", err)
		}
		if writeErr != nil {
			var pg *pgconn.PgError
			if errors.As(writeErr, &pg) && pg.Code == "55P03" {
				t.Fatal("LOCATION_ELIGIBILITY_CENSUS_RETAINS_ROW_LOCK")
			}
			t.Fatalf("independent writer failed outside the causal row-lock boundary: %v", writeErr)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var connected, valid, eligible bool
			server.Raise(conn.QueryRow(ctx, `SELECT location.connected,location.valid,cycle.eligible
				FROM network_client_location_reliability AS location
				JOIN provider_egress_probe_cycle AS cycle USING(client_id)
				WHERE location.client_id=$1`, client).Scan(&connected, &valid, &eligible))
			if !connected || !valid || !eligible {
				t.Fatal("committed location/eligibility publication changed its normal result")
			}
		})
	})
}
