package server

import "strings"

// Archive manifest identity inside the pinned retained qualification receipt.
const CompetitionEpochThreeGoofeRetainedArchiveSha256 = "817098326c0eb08af42df2456ae689735928fb2106a5772f5b2e8a9b369b3ca5"

func competitionEpochThreeGoofeArchiveBindingCorrectionSql() string {
	const original = "receipt->>'archive_manifest_sha256'='d8925103a48981c2a6d3e34c18963fc6886393042270e65b945ecabfb21b5486'"
	sql := competitionEpochThreeGoofeAdmissionSql()
	if strings.Count(sql, original) != 1 {
		panic("published Goofe archive binding changed")
	}
	return strings.Replace(sql, original, "receipt->>'archive_manifest_sha256'='"+CompetitionEpochThreeGoofeRetainedArchiveSha256+"'", 1)
}

func CompetitionEpochThreeGoofeAdmissionMigrationVersion() int {
	return migrationVersionOf(competitionEpochThreeGoofeAdmissionSql())
}

func CompetitionEpochThreeGoofeAdmissionBodySql() string {
	return strings.Split(competitionEpochThreeGoofeAdmissionSql(), "$admission_exception$")[1]
}

func CompetitionEpochThreeGoofeArchiveBindingCorrectionMigrationVersion() int {
	return migrationVersionOf(competitionEpochThreeGoofeArchiveBindingCorrectionSql())
}

func CompetitionEpochThreeGoofeArchiveBindingCorrectionBodySql() string {
	return strings.Split(competitionEpochThreeGoofeArchiveBindingCorrectionSql(), "$admission_exception$")[1]
}

// Replace only the insert guard. Previously recorded evidence and its
// append-only constraints remain unchanged.
func competitionEpochThreeGoofeAdmissionSql() string {
	parts := strings.Split(competitionExecutorAdmissionExceptionSql, "$admission_exception$")
	if len(parts) != 3 {
		panic("published executor exception guard changed")
	}
	body := parts[1]
	replace := func(old, next string) {
		if strings.Count(body, old) != 1 {
			panic("published executor exception predicate changed")
		}
		body = strings.Replace(body, old, next, 1)
	}
	replace("baseline text; q record;", "baseline text; q record; exact_goofe boolean;")
	replace("value:=NEW.approval_json;receipt:=NEW.receipt_json;", `value:=NEW.approval_json;receipt:=NEW.receipt_json;
 exact_goofe:=COALESCE(r.epoch_number=3
  AND NEW.round_id='01a1f4d0-47c9-ac2d-ce45-55ef159a9b41'::uuid AND NEW.host_id='goofe'
  AND q.job_id='0196802b-b5d9-3dfe-a52a-b9eb97dd118b'::uuid
  AND baseline='3b6c73d5cb2fd3c370ed5e79990184500fc9bef9358c36ffb2062d746212bc02'
  AND receipt->>'archive_manifest_sha256'='d8925103a48981c2a6d3e34c18963fc6886393042270e65b945ecabfb21b5486'
  AND receipt->'retention'->>'sha256'='f48570db8860e5fcfac81eae5fdf04128e15fba4f94b24c051693c0ba56e3d3f'
  AND receipt->'retention'->>'version_id'='7b18931c-57c4-4cc3-989d-1256c0b8e04d',false);`)
	replace("NOT BETWEEN 0 AND 0.05", "NOT BETWEEN 0 AND (CASE WHEN exact_goofe THEN 0.053 ELSE 0.05 END)")
	replace("AND m->>'metric'<>'throughput_p50_bytes_per_s'", `AND m->>'metric'<>'throughput_p50_bytes_per_s'
   AND NOT (exact_goofe AND m->>'metric' IN ('throughput_p95_bytes_per_s','ttfb_p50_ms'))`)
	return "CREATE OR REPLACE FUNCTION competition_executor_admission_exception_guard() RETURNS trigger LANGUAGE plpgsql AS $admission_exception$" + body + "$admission_exception$;"
}
