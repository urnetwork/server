// The namespace append upgrades a populated retained database without changing
// signed bytes, and keeps the database guards effective across policy segments.
package server

import (
	"bytes"
	"strings"
	"testing"
)

// Published migrations keep their identities; the policy namespace follows
// both usage snapshots and the restored extender activation schema.
func TestStClientKeyPolicyHistoryMigrationAppendsAtRetainedHead(t *testing.T) {
	index := sqlMigrationIndex(t, "ADD COLUMN is_current boolean NOT NULL DEFAULT true")
	if index != 683 {
		t.Fatalf("client-key policy migration index = %d, want 683", index)
	}
	if migration, ok := migrations[index].(*SqlMigration); !ok || migration.sql != clientKeyPolicyHistorySchemaSql {
		t.Fatal("client-key policy migration differs from its namespace schema")
	}
}

// The old schema stores exactly one segment per client. Upgrading must retain
// those bytes and make room for another generation one without weakening keys.
func TestStClientKeyPolicyHistoryMigrationPreservesPopulatedHistory(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		ApplyDbMigrationsUpTo(ctx, 683)
		clientId, networkId := NewId(), NewId()
		oldDomain, newDomain := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
		oldRegistration, oldEvidence := []byte("synthetic-retained-registration"), []byte("synthetic-retained-evidence")
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				INSERT INTO st_client_key_history (client_id, generation, domain_hash, registration_hash, registration, evidence_hash, evidence)
				VALUES ($1, 1, $2, $3, $4, $5, $6)
			`, clientId, oldDomain, bytes.Repeat([]byte{3}, 32), oldRegistration, "sha256:"+strings.Repeat("4", 64), oldEvidence))
			RaisePgResult(conn.Exec(ctx, `INSERT INTO st_client_key_head (client_id, domain_hash, network_id, generation) VALUES ($1, $2, $3, 1)`, clientId, oldDomain, networkId))
		})
		ApplyDbMigrations(ctx)
		if DbVersion(ctx) != MigrationCount() {
			tb.Fatal("namespace migration did not reach the complete catalog")
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			var registration, evidence []byte
			var current bool
			Raise(conn.QueryRow(ctx, `
				SELECT r.registration, r.evidence, h.is_current
				FROM st_client_key_history r JOIN st_client_key_head h
				ON h.client_id = r.client_id AND h.domain_hash = r.domain_hash AND h.generation = r.generation
				WHERE h.client_id = $1
			`, clientId).Scan(&registration, &evidence, &current))
			if !current || !bytes.Equal(registration, oldRegistration) || !bytes.Equal(evidence, oldEvidence) {
				tb.Fatal("namespace migration changed signed bytes or lost the current head")
			}
			RaisePgResult(conn.Exec(ctx, `
				INSERT INTO st_client_key_history (client_id, generation, domain_hash, registration_hash, registration, evidence_hash, evidence)
				VALUES ($1, 1, $2, $3, $4, $5, $6)
			`, clientId, newDomain, bytes.Repeat([]byte{5}, 32), []byte("synthetic-successor-registration"), "sha256:"+strings.Repeat("6", 64), []byte("synthetic-successor-evidence")))
			if _, err := conn.Exec(ctx, `INSERT INTO st_client_key_head (client_id, domain_hash, network_id, generation) VALUES ($1, $2, $3, 1)`, clientId, newDomain, networkId); err == nil {
				tb.Fatal("database allowed two current policy heads")
			}
			RaisePgResult(conn.Exec(ctx, `UPDATE st_client_key_head SET is_current = false WHERE client_id = $1`, clientId))
			RaisePgResult(conn.Exec(ctx, `INSERT INTO st_client_key_head (client_id, domain_hash, network_id, generation) VALUES ($1, $2, $3, 1)`, clientId, newDomain, networkId))
			for _, statement := range []string{
				`UPDATE st_client_key_history SET evidence = 'changed' WHERE client_id = $1`,
				`DELETE FROM st_client_key_history WHERE client_id = $1`,
				`UPDATE st_client_key_head SET is_current = true WHERE client_id = $1 AND NOT is_current`,
				`UPDATE st_client_key_head SET generation = 2 WHERE client_id = $1 AND NOT is_current`,
				`UPDATE st_client_key_head SET generation = 2 WHERE client_id = $1 AND is_current`,
			} {
				if _, err := conn.Exec(ctx, statement, clientId); err == nil {
					tb.Fatalf("namespace migration lost its guard for %s", statement)
				}
			}
			// Remove the unique-index conflict so the archived-head trigger
			// itself must refuse a return to a superseded policy.
			RaisePgResult(conn.Exec(ctx, `UPDATE st_client_key_head SET is_current = false WHERE client_id = $1 AND is_current`, clientId))
			if _, err := conn.Exec(ctx, `UPDATE st_client_key_head SET is_current = true WHERE client_id = $1 AND domain_hash = $2`, clientId, oldDomain); err == nil {
				tb.Fatal("archived head became current when no other current head existed")
			}
			RaisePgResult(conn.Exec(ctx, `UPDATE st_client_key_head SET retired = true WHERE client_id = $1`, clientId))
			if _, err := conn.Exec(ctx, `UPDATE st_client_key_head SET retired = false WHERE client_id = $1`, clientId); err == nil {
				tb.Fatal("namespace migration allowed a retired client to return")
			}
		})
	})
}
