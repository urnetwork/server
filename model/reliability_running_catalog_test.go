// Ordinary reliability owners use durable markers while migration owners
// verify trigger installation. Live maintenance pressure remains observable.
package model

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Observe actual application and maintenance pools after fixture setup.
// Dynamic activity views and their database identity lookup are permitted.
type reliabilityCatalogTripwire struct {
	catalogReads     atomic.Int64
	windowReads      atomic.Int64
	maintenanceReads atomic.Int64
	markerWrites     atomic.Int64
}

// Attempts count even if a statement fails; no query parameters are retained.
func (self *reliabilityCatalogTripwire) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := strings.ToLower(data.SQL)
	for _, catalog := range []string{"pg_catalog", "information_schema", "pg_trigger", "pg_proc", "pg_attribute", "pg_class", "pg_namespace", "pg_constraint", "pg_index", "pg_type", "to_regclass", "to_regprocedure"} {
		if strings.Contains(sql, catalog) {
			self.catalogReads.Add(1)
			break
		}
	}
	if strings.Contains(sql, "from client_reliability_running_window") && strings.Contains(sql, "degraded_classification_write_token is not null") {
		self.windowReads.Add(1)
	}
	if strings.Contains(sql, "pg_stat_progress_vacuum") && strings.Contains(sql, "pg_stat_progress_create_index") && strings.Contains(sql, "pg_stat_activity") {
		self.maintenanceReads.Add(1)
	}
	if strings.Contains(sql, "insert into client_reliability_running_window") {
		self.markerWrites.Add(1)
	}
	return ctx
}

// Completion status does not erase a catalog attempt.
func (self *reliabilityCatalogTripwire) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Exact row and pressure reads prevent an early return from passing vacuously.
func (self *reliabilityCatalogTripwire) assertReads(t testing.TB, windows, maintenance int64) {
	t.Helper()
	if catalog, actualWindows, actualMaintenance := self.catalogReads.Load(), self.windowReads.Load(), self.maintenanceReads.Load(); catalog != 0 || actualWindows != windows || actualMaintenance != maintenance {
		t.Fatalf("reliability catalog tripwire: catalog=%d windows=%d maintenance=%d want=0/%d/%d", catalog, actualWindows, actualMaintenance, windows, maintenance)
	}
}

// One valid and one observed-invalid client separate score and payout semantics.
type reliabilityCatalogFixture struct {
	ctx       context.Context
	maxTime   time.Time
	validId   server.Id
	invalidId server.Id
}

// Use the existing raw-stat and location writers before observing runtime work.
func newReliabilityCatalogFixture(t testing.TB) *reliabilityCatalogFixture {
	t.Helper()
	ctx := t.Context()
	f := &reliabilityCatalogFixture{
		ctx: ctx, maxTime: server.NowUtc().Truncate(ReliabilityBlockDuration).Add(-ReliabilityBlockDuration),
		validId: server.NewId(), invalidId: server.NewId(),
	}
	city := egressTestCity(ctx, "Synthetic Catalog City", "Synthetic Catalog Region", "Synthetic Catalog Country", "zz")
	networkId := server.NewId()
	for _, clientId := range []server.Id{f.validId, f.invalidId} {
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
	}
	// Complete optional analytics before connecting, so later admissions
	// deduplicate without leaving an independent owner beside the traced pools.
	connectTime := server.NowUtc()
	waitConnectDayWrites(
		recordConnectDayForTest(ctx, f.validId, connectTime),
		recordConnectDayForTest(ctx, f.invalidId, connectTime),
	)
	connectWithLocation := func(clientId server.Id, address string) [32]byte {
		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, addressHash, err := ConnectNetworkClient(ctx, clientId, address, handlerId)
		if err != nil {
			t.Fatal(err)
		}
		if err := SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{ArinQualityVerified: true}); err != nil {
			t.Fatal(err)
		}
		return addressHash
	}
	validAddress := connectWithLocation(f.validId, "192.0.2.20:1")
	invalidAddress := connectWithLocation(f.invalidId, "192.0.2.21:1")
	AddClientReliabilityStats(ctx, networkId, f.validId, validAddress, f.maxTime,
		&ClientReliabilityStats{ConnectionEstablishedCount: 1, ProvideEnabledCount: 1, ReceiveMessageCount: 1})
	AddClientReliabilityStats(ctx, networkId, f.invalidId, invalidAddress, f.maxTime,
		&ClientReliabilityStats{ConnectionEstablishedCount: 1, ProvideEnabledCount: 1})
	UpdateClientLocationReliabilities(ctx, f.maxTime.Add(-NetworkWindowLookback), f.maxTime)
	return f
}

// Known raw observations, rather than another running implementation, define
// the expected rows. Network payout excludes the observed-invalid client.
func (self *reliabilityCatalogFixture) assertRunningRows(t testing.TB, lookback int) {
	t.Helper()
	wantRows, wantZero, validObserved := 2, 1, 1
	if lookback == networkWindowLookbackIndex {
		wantRows, wantZero, validObserved = 1, 0, 0
	}
	server.Db(self.ctx, func(conn server.PgConn) {
		var rows, valid, zero int
		server.Raise(conn.QueryRow(self.ctx, `SELECT count(*),
 count(*) FILTER(WHERE client_id=$1 AND independent_sum=1 AND reliability_sum=1 AND observed_row_count=$2),
 count(*) FILTER(WHERE client_id=$3 AND independent_sum=0 AND reliability_sum=0 AND observed_row_count=1)
 FROM client_reliability_running WHERE lookback_index=$4`, self.validId, validObserved, self.invalidId, lookback).Scan(&rows, &valid, &zero))
		if rows != wantRows || valid != 1 || zero != wantZero {
			t.Fatalf("lookback %d lost current sums: rows=%d valid=%d zero=%d want=%d/1/%d", lookback, rows, valid, zero, wantRows, wantZero)
		}
	})
}

// Exercise the actual client checkpoint/publication and network task stages,
// including reuse of already committed bounds. Old trigger probes trip here.
func TestReliabilityPublicCheckpointsNeverReadCatalog(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newReliabilityCatalogFixture(t)
		tripwire := &reliabilityCatalogTripwire{}
		scope, err := server.NewTestPgQueryScope(f.ctx, tripwire)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		for pass := int64(1); pass <= 2; pass++ {
			UpdateClientReliabilityScoresCheckpointed(f.ctx, f.maxTime)
			UpdateNetworkReliabilityRunningCheckpointed(f.ctx, f.maxTime)
			UpdateNetworkReliabilityWindow(f.ctx, f.maxTime.Add(-ReliabilityBlockDuration), f.maxTime, false)
			tripwire.assertReads(t, pass*int64(2*(len(ClientLookbacks)+1)), pass*int64(len(ClientLookbacks)+3))
			if writes := tripwire.markerWrites.Load(); writes != int64(len(ClientLookbacks)+1) {
				t.Fatalf("unchanged checkpoint rewrote its current tokens: writes=%d want=%d", writes, len(ClientLookbacks)+1)
			}
		}
		if err := scope.Close(); err != nil {
			t.Fatal(err)
		}
		for _, lb := range reliabilityRunningLookbacks() {
			f.assertRunningRows(t, lb.lookbackIndex)
			window := testingReadRunningWindow(f.ctx, lb.lookbackIndex)
			if !window.exists || !window.degradedClassificationWriteTokenPresent || window.degradedClassificationVersion != reliabilityDegradedClassificationVersion || !reliabilityRunningObservationCurrent(window) {
				t.Fatalf("lookback %d did not publish current durable markers: %+v", lb.lookbackIndex, window)
			}
		}
		scores := GetAllClientReliabilityScores(f.ctx)
		if len(scores) != len(ClientLookbacks) {
			t.Fatal("client publication omitted a lookback", len(scores))
		}
		for lookback := range ClientLookbacks {
			valid, validExists := scores[lookback][f.validId]
			invalid, invalidExists := scores[lookback][f.invalidId]
			if len(scores[lookback]) != 2 || !validExists || !invalidExists || valid.IndependentReliabilityScore != 1 || valid.ReliabilityScore != 1 || invalid.IndependentReliabilityScore != 0 || invalid.ReliabilityScore != 0 {
				t.Fatalf("lookback %d publication lost valid or observed-zero scores", lookback)
			}
		}
		windowScores := testingSnapshotWindowScores(f.ctx)
		if len(windowScores) != 1 {
			t.Fatal("network publication did not retain one valid-only score", len(windowScores))
		}
		for _, score := range windowScores {
			if score.IndependentReliabilityScore != 1 || score.ReliabilityScore != 1 {
				t.Fatal("network publication changed valid-only sums", score)
			}
		}
	})
}

// Real trigger invalidation and absent tokens force repair under an observed
// pg_dump snapshot, while unchanged periodic work waits for that snapshot to end.
func TestReliabilityMandatoryMarkerRepairIgnoresMaintenanceWithoutCatalog(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newReliabilityCatalogFixture(t)
		ctx, cancel := context.WithTimeout(f.ctx, time.Minute)
		defer cancel()
		UpdateClientReliabilityRunningCheckpointed(ctx, f.maxTime)
		tripwire := &reliabilityCatalogTripwire{}
		scope, err := server.NewTestPgQueryScope(ctx, tripwire)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		// The ready signal follows a real independent backup snapshot.
		// No sleep or timing guess starts the owning maintenance work.
		releaseBackup := func() func() {
			ready := make(chan struct{})
			release := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				var ownerErr error
				server.HandleError(func() {
					server.Db(ctx, func(conn server.PgConn) {
						tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
						server.Raise(err)
						defer func() {
							cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
							defer cancel()
							_ = tx.Rollback(cleanupCtx)
						}()
						server.RaisePgResult(tx.Exec(ctx, `SET LOCAL application_name='pg_dump'`))
						var count int
						server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM client_reliability`).Scan(&count))
						close(ready)
						select {
						case <-release:
						case <-ctx.Done():
						}
					}, server.OptNoRetry())
				}, func(err error) { ownerErr = err })
				done <- ownerErr
			}()
			joined := false
			finish := func() {
				if !joined {
					close(release)
					if err := <-done; err != nil {
						t.Error(err)
					}
					joined = true
				}
			}
			select {
			case <-ready:
				return finish
			case err := <-done:
				t.Fatal("synthetic backup failed before retaining a snapshot", err)
			case <-ctx.Done():
				finish()
				t.Fatal("synthetic backup did not reach its snapshot boundary", ctx.Err())
			}
			return finish
		}()
		defer releaseBackup()
		if !PostgresLogicalBackupSnapshotActive(ctx) {
			t.Fatal("native backup snapshot was not observed before repair")
		}
		makePeriodicDue := func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE client_reliability_running_window
 SET last_recompute_block=max_block_number-$1,
 degraded_classification_write_token=gen_random_uuid(),observation_write_token=gen_random_uuid()`, ReliabilityRunningRecomputeBlocks))
			}, server.TxReadCommitted, server.OptNoRetry())
		}
		update := func(wantWrites int64) {
			beforeWindows, beforeMaintenance, beforeWrites := tripwire.windowReads.Load(), tripwire.maintenanceReads.Load(), tripwire.markerWrites.Load()
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				UpdateClientReliabilityRunningInTx(tx, ctx, f.maxTime)
			}, server.TxReadCommitted, server.OptNoRetry())
			tripwire.assertReads(t, beforeWindows+int64(len(ClientLookbacks)+1), beforeMaintenance+1)
			if writes := tripwire.markerWrites.Load() - beforeWrites; writes != wantWrites {
				t.Fatalf("maintenance changed mandatory/optional repair ownership: marker writes=%d want=%d", writes, wantWrites)
			}
		}
		makePeriodicDue()
		update(0)
		for _, check := range []struct {
			name                       string
			lookback                   int
			change                     string
			classificationVersion      int
			observationVersion         int
			missingClassificationToken bool
			missingObservationToken    bool
		}{
			{name: "legacy classification client", lookback: 0, change: "min_block_number=min_block_number"},
			{name: "legacy classification network", lookback: networkWindowLookbackIndex, change: "min_block_number=min_block_number"},
			{name: "missing classification token client", lookback: 0, change: "degraded_classification_write_token=NULL,observation_write_token=gen_random_uuid()", observationVersion: reliabilityObservationVersion, missingClassificationToken: true},
			{name: "missing classification token network", lookback: networkWindowLookbackIndex, change: "degraded_classification_write_token=NULL,observation_write_token=gen_random_uuid()", observationVersion: reliabilityObservationVersion, missingClassificationToken: true},
			{name: "legacy observation", lookback: 0, change: "degraded_classification_write_token=gen_random_uuid()", classificationVersion: reliabilityDegradedClassificationVersion},
			{name: "missing observation token", lookback: 0, change: "degraded_classification_write_token=gen_random_uuid(),observation_write_token=NULL", classificationVersion: reliabilityDegradedClassificationVersion, missingObservationToken: true},
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE client_reliability_running SET independent_sum=999,reliability_sum=999 WHERE lookback_index=$1`, check.lookback))
				server.RaisePgResult(tx.Exec(ctx, "UPDATE client_reliability_running_window SET "+check.change+" WHERE lookback_index=$1", check.lookback))
			}, server.TxReadCommitted, server.OptNoRetry())
			before := testingReadRunningWindow(ctx, check.lookback)
			if before.degradedClassificationVersion != check.classificationVersion || before.observationVersion != check.observationVersion || before.degradedClassificationWriteTokenPresent != !check.missingClassificationToken || before.observationWriteTokenPresent != !check.missingObservationToken {
				t.Fatalf("%s did not produce its expected native invalidation: %+v", check.name, before)
			}
			update(1)
			f.assertRunningRows(t, check.lookback)
			window := testingReadRunningWindow(ctx, check.lookback)
			if window.degradedClassificationVersion != reliabilityDegradedClassificationVersion || !window.degradedClassificationWriteTokenPresent || !reliabilityRunningObservationCurrent(window) || window.lastRecomputeBlock != window.maxBlockNumber {
				t.Fatalf("%s did not repair durable markers: %+v", check.name, window)
			}
		}
		makePeriodicDue()
		releaseBackup()
		if PostgresLogicalBackupSnapshotActive(ctx) {
			t.Fatal("joined backup retained maintenance pressure")
		}
		update(int64(len(ClientLookbacks) + 1))
		for _, lb := range reliabilityRunningLookbacks() {
			f.assertRunningRows(t, lb.lookbackIndex)
		}
	})
}

// Current version alone cannot authorize observed-zero reuse without its token.
func TestReliabilityObservationRequiresCurrentWriteToken(t *testing.T) {
	for _, check := range []struct {
		version int
		token   bool
		current bool
	}{
		{version: reliabilityObservationVersion, token: true, current: true},
		{version: reliabilityObservationVersion},
		{token: true},
		{},
	} {
		window := reliabilityRunningWindow{observationVersion: check.version, observationWriteTokenPresent: check.token}
		if reliabilityRunningObservationCurrent(window) != check.current {
			t.Fatalf("observation marker version=%d token=%t changed required repair", check.version, check.token)
		}
	}
}
