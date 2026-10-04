// The complete monitor observes both historical and upgraded receipt custody.
package monitor

import (
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Each additive version can be absent on an older deployment without breaking
// the catalog query; physical or logical weakening stays version-attributed.
func TestMigrationsOriginalCloseEvidenceCatalog(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		check := func(evidence, original bool) {
			t.Helper()
			server.Db(ctx, func(conn server.PgConn) {
				var actualEvidence, actualOriginal bool
				server.Raise(conn.QueryRow(ctx, "SELECT "+contractCloseEvidenceArtifactQuery+", "+contractCloseOriginalArtifactQuery).Scan(&actualEvidence, &actualOriginal))
				if actualEvidence != evidence || actualOriginal != original {
					t.Fatalf("close evidence catalog differs: got %t/%t want %t/%t", actualEvidence, actualOriginal, evidence, original)
				}
			})
		}
		server.ApplyDbMigrationsUpTo(ctx, 766)
		check(false, false)
		server.ApplyDbMigrationsUpTo(ctx, 767)
		check(true, false)
		server.ApplyDbMigrationsUpTo(ctx, 768)
		check(true, true)
		for _, fault := range []struct {
			apply, restore, artifact string
			evidence                 bool
		}{
			{apply: `ALTER TABLE contract_close_report_evidence ALTER COLUMN accepted_at DROP NOT NULL`, restore: `ALTER TABLE contract_close_report_evidence ALTER COLUMN accepted_at SET NOT NULL`, artifact: "original contract close evidence custody@v767"},
			{apply: `ALTER TABLE contract_close_report_evidence ALTER COLUMN unacked_byte_count TYPE numeric(21,0)`, restore: `ALTER TABLE contract_close_report_evidence ALTER COLUMN unacked_byte_count TYPE numeric(20,0)`, artifact: "original contract close evidence custody@v767"},
			{apply: `ALTER TABLE contract_close_report_evidence DISABLE TRIGGER contract_close_report_evidence_guard`, restore: `ALTER TABLE contract_close_report_evidence ENABLE TRIGGER contract_close_report_evidence_guard`, artifact: "original contract close evidence custody@v767"},
			{apply: `ALTER TABLE contract_close_report_evidence DISABLE TRIGGER contract_close_report_evidence_truncate_guard`, restore: `ALTER TABLE contract_close_report_evidence ENABLE TRIGGER contract_close_report_evidence_truncate_guard`, artifact: "original contract close evidence custody@v767"},
			{apply: `CREATE UNIQUE INDEX synthetic_global_close_report ON contract_close_report_evidence(report_id)`, restore: `DROP INDEX synthetic_global_close_report`, artifact: "original contract close evidence custody@v767"},
			{apply: `ALTER TABLE contract_close_report_evidence ADD CONSTRAINT synthetic_close_contract_fk FOREIGN KEY(contract_id) REFERENCES transfer_contract(contract_id) ON DELETE CASCADE`, restore: `ALTER TABLE contract_close_report_evidence DROP CONSTRAINT synthetic_close_contract_fk`, artifact: "original contract close evidence custody@v767"},
			{apply: `ALTER TABLE contract_close_report_evidence ALTER COLUMN original_key_issue TYPE varchar(65)`, restore: `ALTER TABLE contract_close_report_evidence ALTER COLUMN original_key_issue TYPE varchar(64)`, artifact: "original client close signature companions@v768", evidence: true},
			{apply: `ALTER TABLE contract_close_report_evidence DROP CONSTRAINT contract_close_original_key_requires_report`, restore: `ALTER TABLE contract_close_report_evidence ADD CONSTRAINT contract_close_original_key_requires_report CHECK(original_key_registration IS NULL OR original_report IS NOT NULL)`, artifact: "original client close signature companions@v768", evidence: true},
			{apply: `DROP INDEX contract_close_original_census`, restore: `CREATE INDEX contract_close_original_census ON contract_close_report_evidence(contract_id,client_id,report_id)`, artifact: "original client close signature companions@v768", evidence: true},
		} {
			server.Db(ctx, func(conn server.PgConn) { server.RaisePgResult(conn.Exec(ctx, fault.apply)) })
			check(fault.evidence, false)
			_, drift := migrationPingDatabaseCheck(t, ctx)
			if !strings.Contains(drift, fault.artifact) {
				t.Fatal("complete migration monitor lost the changed evidence artifact", fault.artifact, drift)
			}
			server.Db(ctx, func(conn server.PgConn) { server.RaisePgResult(conn.Exec(ctx, fault.restore)) })
			check(true, true)
			if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
				t.Fatal("restored complete evidence catalog still reports drift", drift)
			}
		}
	})
}
