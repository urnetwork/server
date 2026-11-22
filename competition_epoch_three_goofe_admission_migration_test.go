package server

import (
	"strings"
	"testing"
)

func TestEpochThreeGoofeAdmissionCorrectionChangesOnlyArchiveBinding(t *testing.T) {
	const actual = "817098326c0eb08af42df2456ae689735928fb2106a5772f5b2e8a9b369b3ca5"
	const old = "d8925103a48981c2a6d3e34c18963fc6886393042270e65b945ecabfb21b5486"
	corrected := competitionEpochThreeGoofeArchiveBindingCorrectionSql()
	if strings.Count(corrected, actual) != 1 || strings.Contains(corrected, old) ||
		strings.Replace(corrected, actual, old, 1) != competitionEpochThreeGoofeAdmissionSql() {
		t.Fatal("correction must replace only the archive digest inside the exact retained receipt")
	}
	if CompetitionEpochThreeGoofeArchiveBindingCorrectionMigrationVersion() != CompetitionEpochThreeGoofeAdmissionMigrationVersion()+1 {
		t.Fatal("archive binding correction must be a new migration immediately after the original")
	}
}

func TestEpochThreeGoofeAdmissionMigrationScopesException(t *testing.T) {
	sql := competitionEpochThreeGoofeAdmissionSql()
	for _, predicate := range []string{
		"r.epoch_number=3",
		"NEW.round_id='01a1f4d0-47c9-ac2d-ce45-55ef159a9b41'::uuid",
		"NEW.host_id='goofe'",
		"q.job_id='0196802b-b5d9-3dfe-a52a-b9eb97dd118b'::uuid",
		"AND baseline='3b6c73d5cb2fd3c370ed5e79990184500fc9bef9358c36ffb2062d746212bc02'",
		"receipt->>'archive_manifest_sha256'='d8925103a48981c2a6d3e34c18963fc6886393042270e65b945ecabfb21b5486'",
		"receipt->'retention'->>'sha256'='f48570db8860e5fcfac81eae5fdf04128e15fba4f94b24c051693c0ba56e3d3f'",
		"receipt->'retention'->>'version_id'='7b18931c-57c4-4cc3-989d-1256c0b8e04d'",
		"CASE WHEN exact_goofe THEN 0.053 ELSE 0.05 END",
		"exact_goofe AND m->>'metric' IN ('throughput_p95_bytes_per_s','ttfb_p50_ms')",
		"attempt_count<>0 OR started_at IS NOT NULL",
		"receipt->'passed' IS DISTINCT FROM 'false'::jsonb",
		"receipt->'retention'->>'mode' IS DISTINCT FROM 'COMPLIANCE'",
	} {
		if !strings.Contains(sql, predicate) {
			t.Errorf("guard missing %q", predicate)
		}
	}
}
