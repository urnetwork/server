// Registration-session evidence is an additive append, never a historical guess.
package server

import (
	"testing"
	"time"
)

// The existing published catalog retains its exact positions and SQL identity.
func TestClientRegistrationSessionMigrationFollowsPublishedCatalog(t *testing.T) {
	deliveryIndex := sqlMigrationIndex(t, "ADD COLUMN deliver_time")
	index := sqlMigrationIndex(t, "ADD COLUMN session_accept_until")
	windowIndex := sqlMigrationIndex(t, "ADD COLUMN head_window")
	if deliveryIndex != 839 || deliveryIndex != sqlMigrationIndex(t, "ADD COLUMN arin_old_quality_write_token")+1 || index != deliveryIndex+1 || windowIndex != index+1 {
		t.Fatal("registration session schema changed published migration order", deliveryIndex, index, windowIndex)
	}
	value, ok := migrations[index].(*SqlMigration)
	if !ok || value.sql != clientRegistrationSessionSchemaSql {
		t.Fatal("registration session migration differs from its complete schema")
	}
}

// Executing the exact append against an existing row leaves its provenance
// unchanged and all new evidence unknown. No mutable client row is consulted.
func TestClientRegistrationSessionMigrationPreservesUnknownHistory(t *testing.T) {
	env := DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `CREATE TEMP TABLE network_client_registration (registration_id varchar(64) PRIMARY KEY,client_id uuid NOT NULL,create_time timestamp NOT NULL) ON COMMIT DROP`))
			clientId := NewId()
			created := CodecTime(NowUtc().Add(-time.Hour))
			RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_registration VALUES ('synthetic-historical-registration',$1,$2)`, clientId, created))
			RaisePgResult(tx.Exec(ctx, clientRegistrationSessionSchemaSql))
			var retainedClient Id
			var retainedTime time.Time
			var sessionId *Id
			var sessionTime, sessionHorizon *time.Time
			Raise(tx.QueryRow(ctx, `SELECT client_id,create_time,session_id,session_create_time,session_accept_until FROM network_client_registration`).Scan(&retainedClient, &retainedTime, &sessionId, &sessionTime, &sessionHorizon))
			if retainedClient != clientId || !retainedTime.Equal(created) || sessionId != nil || sessionTime != nil || sessionHorizon != nil {
				t.Fatal("migration changed history or invented original session evidence")
			}
		})
	})
}
