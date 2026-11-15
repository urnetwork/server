// Direct parity readers have the same census owner as a normal signal. The
// existing acquisition tripwire proves refusal precedes opening any DB pool.
package monitor

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// This caller is live and would otherwise be able to enter a DB branch. A
// missing marker is its own refusal, not a coincidental canceled acquisition.
func TestMigrationCensusFixtureUnownedRefusesBeforeDatabase(t *testing.T) {
	ctx := server.WithoutPostgres(t.Context())
	source := &migrationPingDatabaseSource{requireCensusOwnership: true}
	rows, err := source.PostgreSQL(ctx, "WITH version AS (SELECT 1 AS value) SELECT value FROM version;")
	if err == nil || err.Error() != "native migration parity fixture requires an owned census observation" || rows != nil ||
		server.PacketPostgresAttempts(ctx) != 0 || ctx.Err() != nil {
		t.Fatal("unowned full-query fixture reached storage or changed refusal provenance", err)
	}
}

// The same live tripwire is causal: a marked reader reaches the real shared
// acquisition boundary and cannot pass by fabricating a complete observation.
func TestMigrationCensusFixtureOwnedReachesDatabaseTripwire(t *testing.T) {
	parent := server.WithoutPostgres(t.Context())
	observation := &migrationCensusObservation{settings: DefaultMigrationCensusSettings()}
	ctx := context.WithValue(parent, migrationCensusContextKey{}, observation)
	source := &migrationPingDatabaseSource{requireCensusOwnership: true}
	recovered := func() (failure any) {
		defer func() { failure = recover() }()
		_, _ = source.PostgreSQL(ctx, "WITH version AS (SELECT 1 AS value) SELECT value FROM version;")
		return nil
	}()
	if recovered != server.ErrPacketPostgres || server.PacketPostgresAttempts(parent) != 1 || observation.catalogComplete ||
		observation.catalogRows != nil || ctx.Err() != nil {
		t.Fatal("marked fixture bypassed the real guarded acquisition or fabricated rows", recovered)
	}
}
