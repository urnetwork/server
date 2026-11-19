// The complete monitor observes both historical and upgraded receipt custody.
package monitor

import (
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Each additive version can be absent on an older deployment without breaking
// the catalog query; physical or logical weakening stays version-attributed.
func TestMigrationsOriginalCloseEvidenceCatalog(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		check := func(phase string, evidence, original bool) {
			t.Helper()
			server.Db(ctx, func(conn server.PgConn) {
				var actualEvidence, actualOriginal bool
				server.Raise(conn.QueryRow(ctx, "SELECT "+contractCloseEvidenceArtifactQuery+", "+contractCloseOriginalArtifactQuery).Scan(&actualEvidence, &actualOriginal))
				if actualEvidence != evidence || actualOriginal != original {
					// Only fixed catalog metadata is retained. A type round trip may
					// restore its width while leaving a changed default or check.
					var columns, constraints, index string
					server.Raise(conn.QueryRow(ctx, `SELECT
					 COALESCE((SELECT json_agg(c ORDER BY c.column_name)::text FROM (
					  SELECT column_name, data_type, is_nullable, column_default, character_maximum_length
					  FROM information_schema.columns WHERE table_schema='public'
					  AND table_name='contract_close_report_evidence'
					  AND column_name IN ('original_report','original_key_registration','original_key_issue')
					 ) c),'[]'),
					 COALESCE((SELECT json_agg(c ORDER BY c.conname)::text FROM (
					  SELECT conname, convalidated, condeferrable, pg_get_constraintdef(oid) AS definition
					  FROM pg_constraint WHERE conrelid=to_regclass('public.contract_close_report_evidence')
					  AND contype='c'
					 ) c),'[]'),
					 COALESCE(pg_get_indexdef(to_regclass('public.contract_close_original_census')),'absent')`).Scan(&columns, &constraints, &index))
					t.Fatalf("%s: close evidence catalog differs: got %t/%t want %t/%t; original columns=%s checks=%s census=%s", phase, actualEvidence, actualOriginal, evidence, original, columns, constraints, index)
				}
			})
		}
		server.ApplyDbMigrationsUpTo(ctx, 766)
		check("prefix 766", false, false)
		server.ApplyDbMigrationsUpTo(ctx, 767)
		check("prefix 767", true, false)
		server.ApplyDbMigrationsUpTo(ctx, 768)
		check("prefix 768", true, true)
		server.ApplyDbMigrationsUpTo(ctx, 769)
		check("prefix 769", true, true)
		for _, fault := range []struct {
			name, apply, restore, artifact string
			evidence                       bool
		}{
			{name: "accepted timestamp nullability", apply: `ALTER TABLE contract_close_report_evidence ALTER COLUMN accepted_at DROP NOT NULL`, restore: `ALTER TABLE contract_close_report_evidence ALTER COLUMN accepted_at SET NOT NULL`, artifact: "original contract close evidence custody@v767"},
			{name: "unacked byte precision", apply: `ALTER TABLE contract_close_report_evidence ALTER COLUMN unacked_byte_count TYPE numeric(21,0)`, restore: `ALTER TABLE contract_close_report_evidence ALTER COLUMN unacked_byte_count TYPE numeric(20,0)`, artifact: "original contract close evidence custody@v767"},
			{name: "row custody guard", apply: `ALTER TABLE contract_close_report_evidence DISABLE TRIGGER contract_close_report_evidence_guard`, restore: `ALTER TABLE contract_close_report_evidence ENABLE TRIGGER contract_close_report_evidence_guard`, artifact: "original contract close evidence custody@v767"},
			{name: "truncate custody guard", apply: `ALTER TABLE contract_close_report_evidence DISABLE TRIGGER contract_close_report_evidence_truncate_guard`, restore: `ALTER TABLE contract_close_report_evidence ENABLE TRIGGER contract_close_report_evidence_truncate_guard`, artifact: "original contract close evidence custody@v767"},
			{name: "global report uniqueness", apply: `CREATE UNIQUE INDEX synthetic_global_close_report ON contract_close_report_evidence(report_id)`, restore: `DROP INDEX synthetic_global_close_report`, artifact: "original contract close evidence custody@v767"},
			{name: "mutable contract foreign key", apply: `ALTER TABLE contract_close_report_evidence ADD CONSTRAINT synthetic_close_contract_fk FOREIGN KEY(contract_id) REFERENCES transfer_contract(contract_id) ON DELETE CASCADE`, restore: `ALTER TABLE contract_close_report_evidence DROP CONSTRAINT synthetic_close_contract_fk`, artifact: "original contract close evidence custody@v767"},
			{
				name:  "original issue width",
				apply: `ALTER TABLE contract_close_report_evidence ALTER COLUMN original_key_issue TYPE varchar(65)`,
				// ALTER TYPE reparses the enum check with per-element text casts.
				// Restore its published expression as well as the column width.
				restore: `ALTER TABLE contract_close_report_evidence ALTER COLUMN original_key_issue TYPE varchar(64);
				 ALTER TABLE contract_close_report_evidence
				 DROP CONSTRAINT contract_close_report_evidence_original_key_issue_check,
				 ADD CONSTRAINT contract_close_report_evidence_original_key_issue_check
				 CHECK (original_key_issue IN ('','history_not_found','history_capacity','history_read_unavailable'))`,
				artifact: "original client close signature companions@v768",
				evidence: true,
			},
			{name: "original key report custody", apply: `ALTER TABLE contract_close_report_evidence DROP CONSTRAINT contract_close_original_key_requires_report`, restore: `ALTER TABLE contract_close_report_evidence ADD CONSTRAINT contract_close_original_key_requires_report CHECK(original_key_registration IS NULL OR original_report IS NOT NULL)`, artifact: "original client close signature companions@v768", evidence: true},
			{name: "original census index", apply: `DROP INDEX contract_close_original_census`, restore: `CREATE INDEX contract_close_original_census ON contract_close_report_evidence(contract_id,client_id,report_id)`, artifact: "original client close signature companions@v768", evidence: true},
		} {
			server.Db(ctx, func(conn server.PgConn) { server.RaisePgResult(conn.Exec(ctx, fault.apply)) })
			check("fault "+fault.name, fault.evidence, false)
			_, drift := migrationPingDatabaseCheck(t, ctx)
			if !strings.Contains(drift, fault.artifact) {
				t.Fatal("complete migration monitor lost the changed evidence artifact", fault.name, fault.artifact, drift)
			}
			server.Db(ctx, func(conn server.PgConn) { server.RaisePgResult(conn.Exec(ctx, fault.restore)) })
			check("restored "+fault.name, true, true)
			if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
				t.Fatal("restored complete evidence catalog still reports drift", fault.name, drift)
			}
		}
	})
}
