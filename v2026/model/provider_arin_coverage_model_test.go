// Aggregate provenance controls preserve unknowns without hiding live deficits.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Default false flags, lookup overrides, and mixed-generation connections do
// not prove that all of a provider's currently connected addresses were read.
func TestProviderArinClassificationCoverageRequiresActualCurrentLookup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Coverage", "Coverage", "Coverage", "zz")
		cutover := server.NowUtc().Add(-time.Minute).Truncate(time.Microsecond)
		currentAt := cutover.Add(time.Second)
		oldAt := cutover.Add(-time.Second)
		const epoch int64 = 123456789
		current := &ConnectionLocationScores{ArinLookupAt: &currentAt, ArinDatabaseBuildEpoch: epoch}
		oldGeneration := &ConnectionLocationScores{ArinLookupAt: &currentAt, ArinDatabaseBuildEpoch: epoch - 1}
		oldTime := &ConnectionLocationScores{ArinLookupAt: &oldAt, ArinDatabaseBuildEpoch: epoch}
		fresh := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, current)
		egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		egressTestConnect(ctx, t, city, egressTestUnsampled, nil, oldGeneration)
		egressTestConnect(ctx, t, city, egressTestUnsampled, nil, oldTime)
		mixed := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, current)
		missingLocation := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, current)
		disconnected := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		inactive := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		staleHandler := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		child := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		egressTestConnect(ctx, t, city, egressTestUnsampled, map[ProvideMode][]byte{ProvideModeNetwork: []byte("private")}, nil)
		secondConnection, _, _, _, err := ConnectNetworkClient(ctx, mixed.clientId, "203.0.113.250:0", CreateNetworkClientHandler(ctx))
		if err != nil {
			t.Fatal(err)
		}
		if err := SetConnectionLocation(ctx, secondConnection, city.LocationId, oldGeneration); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_location WHERE connection_id=$1`, missingLocation.connectionId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET connected=false WHERE connection_id=$1`, disconnected.connectionId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, inactive.clientId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET source_client_id=$2 WHERE client_id=$1`, child.clientId, fresh.clientId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_handler SET heartbeat_time=$2
				WHERE handler_id=(SELECT handler_id FROM network_client_connection WHERE connection_id=$1)`, staleHandler.connectionId, server.NowUtc().Add(-3*NetworkClientHandlerHeartbeatTimeout)))
		})
		coverage := GetProviderArinClassificationCoverage(ctx, epoch, cutover)
		want := ProviderArinClassificationCoverage{Connections: 7, ClassifiedConnections: 2, UnclassifiedConnections: 2,
			OutdatedConnections: 3, Providers: 6, FullyClassifiedProviders: 1}
		if coverage != want {
			t.Fatalf("coverage=%+v want=%+v", coverage, want)
		}
		if err := SetConnectionLocation(ctx, secondConnection, city.LocationId, current); err != nil {
			t.Fatal(err)
		}
		coverage = GetProviderArinClassificationCoverage(ctx, epoch, cutover)
		if coverage.ClassifiedConnections != 3 || coverage.FullyClassifiedProviders != 2 || coverage.OutdatedConnections != 2 {
			t.Fatalf("a real current lookup did not advance coverage: %+v", coverage)
		}
		// Explicit IP overrides cannot inherit a previously attested lookup.
		if err := SetConnectionLocation(ctx, fresh.connectionId, city.LocationId, &ConnectionLocationScores{}); err != nil {
			t.Fatal(err)
		}
		coverage = GetProviderArinClassificationCoverage(ctx, epoch, cutover)
		if coverage.ClassifiedConnections != 2 || coverage.UnclassifiedConnections != 3 || coverage.FullyClassifiedProviders != 1 {
			t.Fatalf("an override incorrectly retained lookup provenance: %+v", coverage)
		}
		coverage = GetProviderArinClassificationCoverage(ctx, 0, cutover)
		if coverage.ClassifiedConnections != 0 || coverage.FullyClassifiedProviders != 0 {
			t.Fatalf("an unspecified database generation passed attestation: %+v", coverage)
		}
	})
}
