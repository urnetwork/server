package monitor

import (
	"strings"

	"github.com/urnetwork/server"
)

const competitionEpochThreeSourceRecoveryCatalogName = "epoch-three source recovery with retained catalog custody"

var competitionEpochThreeSourceRecoveryMigrationContract = func() snMainnetMigrationContract {
	old := server.CompetitionOfficialReviewEpochTwoBodiesSql()
	binding, source, recovery := server.CompetitionEpochThreeSourceRecoveryBodiesSql()
	query := competitionSilleStorageRecoveryMigrationContract.query
	for _, change := range [][2]string{{old.PrereviewBinding, binding}, {old.WinnerSourceAuthorized, source}} {
		before := snMainnetMigrationLiteral(change[0])
		if strings.Count(query, before) != 1 {
			panic("epoch-three source recovery catalog predecessor absent or ambiguous")
		}
		query = strings.Replace(query, before, snMainnetMigrationLiteral(change[1]), 1)
	}
	// Later corrections replace individual guards. The source recovery index,
	// grading quorum and all other checks keep their original owner.
	beforeCorrection := func(version int, pin string) {
		if strings.Count(query, pin) != 1 {
			panic("epoch-three correction catalog predecessor absent or ambiguous")
		}
		query = strings.Replace(query, pin, migrationCompetitionBefore(version, pin), 1)
	}
	for _, function := range server.CompetitionIntakeFirstFunctionsSql() {
		switch function.Signature {
		case competitionIntakeRuntimeReadySignature, competitionIntakeBaselineValidSignature:
			pin := snMainnetMigrationFunction(function.Signature, function.Result, "plpgsql", "v", false, function.Body, function.Arguments...)
			beforeCorrection(competitionEpochThreeRuntimeCorrectionVersion, pin)
		}
	}
	beforeCorrection(competitionEpochThreeExecutorBuilderCorrectionVersion, snMainnetMigrationFunction(competitionExecutorAmendmentGuardSignature,
		"trigger", "plpgsql", "v", false, server.CompetitionExecutorControlAmendmentBodiesSql().AmendmentGuard))
	beforeCorrection(server.CompetitionEpochThreeGoofeAdmissionMigrationVersion(), snMainnetMigrationFunction("competition_executor_admission_exception_guard()",
		"trigger", "plpgsql", "v", false, server.CompetitionExecutorAdmissionExceptionBodiesSql().ExceptionGuard))
	return migrationCurrentSuffix(server.CompetitionEpochThreeSourceRecoveryMigrationVersion(), competitionEpochThreeSourceRecoveryCatalogName, query,
		snMainnetMigrationFunction("competition_epoch_three_source_recovery_matches(uuid,jsonb,jsonb,timestamp without time zone)",
			"boolean", "sql", "s", false, recovery, "round_uuid", "request", "checkpoint", "imported_at"),
		`EXISTS (SELECT 1 FROM pg_index i WHERE i.indexrelid=to_regclass('public.competition_epoch_three_source_recovery_once')
		 AND i.indrelid=to_regclass('public.competition_official_review_authorization')
		 AND i.indisunique AND i.indisvalid AND i.indisready AND i.indimmediate
		 AND i.indnatts=1 AND pg_get_indexdef(i.indexrelid,1,true)='round_id'
		 AND pg_get_expr(i.indpred,i.indrelid)='((round_id = ''01a1c110-585b-ce38-4f29-ca4860a1c058''::uuid) AND (((import_json -> ''review''::text) ->> ''private_evidence_sha256''::text) = ''ce3876303e142c95f3af55fe1c97dc11326f65f201baccf090fa8fac0e92e808''::text))')`)
}()
