// The new audited cycle transition is appended after published history, with
// unchanged legacy branches and explicit expiry/atomic-fence constraints.
package server

import (
	"strings"
	"testing"
)

// The captured tail keeps its exact ordinals after recovery publishes. Later
// appends are valid; omissions, reordered entries and prefix inserts are not.
func competitionCapturedRecoveryTailValid(versions []int, migrationCount int) bool {
	const firstVersion = 974
	const lastVersion = 985
	if len(versions) != lastVersion-firstVersion+1 || migrationCount < lastVersion {
		return false
	}
	for index, version := range versions {
		if version != firstVersion+index {
			return false
		}
	}
	return true
}

// Captured main 35a86775c's published tail keeps its exact order before recovery
// 985. Resolve positions from the actual registered objects, not the current
// head, so publishing a subsequent migration never shifts these identities.
func TestCompetitionDistributedRecoveryMigrationFollowsCapturedPublishedTail(t *testing.T) {
	versions := []int{CtfSchemaMigrationVersion(), NetworkPingTargetEvidenceSchemaMigrationVersion(),
		NetworkExtenderAddressPortsSchemaMigrationVersion(), CtfAwardNetworkMigrationVersion(),
		StLedgerV2ExtenderMigrationVersion(), SolanaReceiverSweepCursorVersion(),
		ProviderEgressFp2EvidenceMigrationVersion(),
		StoreNotificationFlowVersion(), CryptoAddressSweepVersion(),
		SolanaShopifyOrderPaymentVersion(), NetworkMailContextVersion(),
		CompetitionDistributedReevaluationMigrationVersion(), GiftSchemaVersion(), GiftRefundSchemaVersion(),
		CompetitionOperatorRequestedPatchRetryMigrationVersion(), OperatorBlockTrafficVersion(),
		OperatorBlockPointsExpiredVersion(), GiftGammaSchemaVersion(),
		ProviderWorkSessionEventLegacyDropGuardMigrationVersion(), ProviderWorkSessionProviderAppendMigrationVersion(),
		ProviderWorkSessionHeadRetentionGuardMigrationVersion(), ClientKeyHistoryRetentionSevenDayFloorMigrationVersion(),
		AuditDeviceDayVersion(), TransferDebitJournalDropGuardMigrationVersion(),
		ClientKeyHistoryRegistrationHashUniqueDropMigrationVersion(), CompetitionSecondRecoveryMigrationVersion()}
	for index, version := range versions {
		if version < 1 || index > 0 && version != versions[index-1]+1 {
			t.Fatal("captured published migration tail was moved, omitted or overwritten", versions)
		}
	}
	if versions[8] != 968 || versions[9] != 969 || versions[10] != 970 {
		t.Fatal("captured main's published address-sweep, Shopify or mail migrations moved", versions)
	}
	if !competitionCapturedRecoveryTailValid(versions[14:], MigrationCount()) {
		t.Fatal("captured published migration tail or recovery ordinal changed", versions, MigrationCount())
	}
	t.Logf("registered_tail ctf=%d ping_evidence=%d extender_ports=%d ctf_award=%d ledger_extenders=%d receiver_sweep=%d fp2_evidence=%d notification_flow=%d address_sweep=%d shopify_order=%d mail_context=%d recovery=%d gift=%d gift_refund=%d operator_retry=%d operator_traffic=%d expired_points=%d gift_gamma=%d legacy_drop_guard=%d provider_append=%d head_retention=%d history_floor=%d audit_device_day=%d debit_journal_drop_guard=%d registration_hash_unique_drop=%d second_recovery=%d head=%d", versions[0], versions[1], versions[2], versions[3], versions[4], versions[5], versions[6], versions[7], versions[8], versions[9], versions[10], versions[11], versions[12], versions[13], versions[14], versions[15], versions[16], versions[17], versions[18], versions[19], versions[20], versions[21], versions[22], versions[23], versions[24], versions[25], MigrationCount())
}

// The same helper used by both real tail assertions accepts future appends
// without allowing an omission, duplicate, gap, reorder or prefix insertion.
func TestCompetitionCapturedRecoveryTailAllowsOnlyAppendOnlySuccessors(t *testing.T) {
	versions := []int{CompetitionOperatorRequestedPatchRetryMigrationVersion(), OperatorBlockTrafficVersion(),
		OperatorBlockPointsExpiredVersion(), GiftGammaSchemaVersion(), ProviderWorkSessionEventLegacyDropGuardMigrationVersion(),
		ProviderWorkSessionProviderAppendMigrationVersion(), ProviderWorkSessionHeadRetentionGuardMigrationVersion(),
		ClientKeyHistoryRetentionSevenDayFloorMigrationVersion(), AuditDeviceDayVersion(),
		TransferDebitJournalDropGuardMigrationVersion(), ClientKeyHistoryRegistrationHashUniqueDropMigrationVersion(),
		CompetitionSecondRecoveryMigrationVersion()}
	if !competitionCapturedRecoveryTailValid(versions, MigrationCount()) {
		t.Fatal("actual registered tail does not match its captured ordinals", versions, MigrationCount())
	}
	gap := append([]int(nil), versions...)
	gap[4]++
	duplicate := append([]int(nil), versions...)
	duplicate[4] = duplicate[3]
	reordered := append([]int(nil), versions...)
	reordered[3], reordered[4] = reordered[4], reordered[3]
	shifted := append([]int(nil), versions...)
	for index := range shifted {
		shifted[index]++
	}
	omitted := append(append([]int(nil), versions[:4]...), versions[5:]...)
	withoutAudit := append(append([]int(nil), versions[:8]...), versions[9:]...)
	withoutDebitGuard := append(append([]int(nil), versions[:9]...), versions[10:]...)
	withoutRegistrationDrop := append(append([]int(nil), versions[:10]...), versions[11:]...)
	recoveryBeforeAudit := append([]int(nil), versions...)
	recoveryBeforeAudit[8], recoveryBeforeAudit[len(versions)-1] = recoveryBeforeAudit[len(versions)-1], recoveryBeforeAudit[8]
	recoveryBeforePublishedTail := append([]int(nil), versions...)
	recoveryBeforePublishedTail[9], recoveryBeforePublishedTail[len(versions)-1] = recoveryBeforePublishedTail[len(versions)-1], recoveryBeforePublishedTail[9]
	for _, test := range []struct {
		name     string
		versions []int
		head     int
		valid    bool
	}{
		{name: "captured head", versions: versions, head: 985, valid: true},
		{name: "subsequent append", versions: versions, head: 986, valid: true},
		{name: "incomplete head", versions: versions, head: 984, valid: false},
		{name: "gap", versions: gap, head: 986, valid: false},
		{name: "duplicate", versions: duplicate, head: 985, valid: false},
		{name: "reorder", versions: reordered, head: 985, valid: false},
		{name: "prefix insertion", versions: shifted, head: 986, valid: false},
		{name: "omission", versions: omitted, head: 985, valid: false},
		{name: "published audit omission", versions: withoutAudit, head: 985, valid: false},
		{name: "published debit guard omission", versions: withoutDebitGuard, head: 985, valid: false},
		{name: "published registration drop omission", versions: withoutRegistrationDrop, head: 985, valid: false},
		{name: "recovery before published audit", versions: recoveryBeforeAudit, head: 985, valid: false},
		{name: "recovery before published tail", versions: recoveryBeforePublishedTail, head: 985, valid: false},
	} {
		if valid := competitionCapturedRecoveryTailValid(test.versions, test.head); valid != test.valid {
			t.Errorf("%s validity = %t, want %t", test.name, valid, test.valid)
		}
	}
}

// Deleting only each explicit addition recovers the complete reviewed old
// function. A broad replacement or silent gate removal changes these bytes.
func TestCompetitionDistributedRecoveryMigrationPreservesPredecessorGuards(t *testing.T) {
	_, _, oldImmutable, _ := CompetitionJobReevaluationBodiesSql()
	oldExecutor := CompetitionExecutorAdmissionClaimGuardBodySql()
	var oldIntake string
	for _, function := range CompetitionIntakeFirstFunctionsSql() {
		if function.Signature == "competition_v2_intake_execution_guard()" {
			oldIntake = function.Body
		}
	}
	immutable, executor, intake := competitionDistributedReevaluationSuccessorBodies()
	for _, body := range []struct{ old, next, marker string }{
		{old: oldImmutable, next: immutable, marker: " IF competition_distributed_recovery_transition_allows(OLD,NEW) THEN RETURN NEW; END IF;"},
		{old: oldExecutor, next: executor, marker: "distributed cycle transition requires its separately audited closure"},
		{old: oldIntake, next: intake, marker: " IF TG_OP='UPDATE' AND competition_distributed_recovery_transition_allows(OLD,NEW) THEN RETURN NEW; END IF;"},
	} {
		if body.old == "" || strings.Count(body.next, body.marker) != 1 {
			t.Fatal("reviewed predecessor/addition absent or ambiguous")
		}
		// Every original line remains in order, including every denial branch.
		position := 0
		for _, line := range strings.Split(body.old, "\n") {
			offset := strings.Index(body.next[position:], line)
			if offset < 0 {
				t.Fatal("distributed recovery lost an original guard line")
			}
			position += offset + len(line)
		}
	}
	version := CompetitionDistributedReevaluationMigrationVersion()
	if GiftSchemaVersion() != version+1 || GiftRefundSchemaVersion() != GiftSchemaVersion()+1 ||
		CompetitionOperatorRequestedPatchRetryMigrationVersion() != GiftRefundSchemaVersion()+1 || version <= CompetitionExecutorAdmissionClaimMigrationVersion() {
		t.Fatal("distributed recovery was not appended after origin migration prefix")
	}
}

// Missing/future/expired proof dates must fail closed, and deferred publication
// must bind all empty host fences rather than only a singleton row.
func TestCompetitionDistributedRecoveryMigrationPinsAtomicExpiryAndWholeHostCensus(t *testing.T) {
	transition, audit, commit, _, _, _ := CompetitionDistributedReevaluationBodiesSql()
	for _, required := range []string{"pg_current_xact_id()", "audit.original_terminal_json IS DISTINCT FROM to_jsonb(previous_job)", "next_job.execution_deadline_at IS NOT NULL", "next_job.attempt_count<>previous_job.attempt_count", "competition_distributed_recovery_proof"} {
		if !strings.Contains(transition, required) {
			t.Fatal("missing immutable transition authority", required)
		}
	}
	for _, required := range []string{"clock_timestamp()", "database_bracket_started_at", "database_bracket_expires_at", "IS DISTINCT FROM true", "competition_supervisor_lease", "competition_execution_slot", "competition_worker_slot", "competition_automation_hold", "original_terminal_json", "proof->'availability'", "native->'identity'->>'cycle'", "legacy_prelaunch_infrastructure_failure"} {
		if !strings.Contains(audit, required) {
			t.Fatal("missing exact distributed closure authority", required)
		}
	}
	for _, required := range []string{"expired before commit", "fencing_token=(expected->>'fencing_token')::bigint+1", "j.cycle_started_at IS NULL", "j.execution_deadline_at IS NULL"} {
		if !strings.Contains(commit, required) {
			t.Fatal("missing atomic deferred recovery check", required)
		}
	}
	if !strings.Contains(CompetitionDistributedReevaluationMigrationSql(), "DEFERRABLE INITIALLY DEFERRED") {
		t.Fatal("new cycle and whole-host fences lost their atomic commit guard")
	}
}
