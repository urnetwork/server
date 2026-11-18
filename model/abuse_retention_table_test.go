package model

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// TestEveryRetentionSweepHonorsHolds's table (ABUSE-M5-16; §21.9, R-ABUSE-5): every retention sweep and partition
// drop the server runs, each with §2.8's row it deletes from and how it honors §21.9's holds, why the row is outside
// a hold's reach, or which task owes its hold clause. A new sweep, a new retention label or a new partition family
// with no line fails the test, so no sweep of a listed row lands unread by a hold.

// How a sweep stands with holds.
type retentionSweepHoldHow string

const (
	// it reads holds before it deletes: HeldNetworkIds, the live part 0s in its SQL, AbuseHoldCopyPending, or
	// sidecar.HoldSet on evs's and gkm's side
	retentionSweepHonors retentionSweepHoldHow = "honors"
	// the row is outside a hold's reach, and detail says why
	retentionSweepOutside retentionSweepHoldHow = "outside"
	// the row is in reach and the sweep does not read holds yet: detail names the task that owes the clause
	retentionSweepGap retentionSweepHoldHow = "gap"
)

// One line of the table.
type retentionSweepHoldLine struct {
	// a retention label (retentionTables), a partition family's name, or an engine's pass
	sweep string
	// §2.8's row it deletes from, "" for a table no row lists
	row    string
	how    retentionSweepHoldHow
	detail string
}

var retentionSweepHoldLines = []retentionSweepHoldLine{
	// api-14, this task's pass
	{sweep: RetentionTableAbuseCase, row: "api-14", how: retentionSweepHonors, detail: "RemoveExpiredAbuseRows skips a case of a network HeldNetworkIds names"},
	{sweep: RetentionTableAbuseAction, row: "api-14", how: retentionSweepHonors, detail: "goes only with its case, which a hold keeps"},
	{sweep: RetentionTableAbuseAppeal, row: "api-14", how: retentionSweepHonors, detail: "goes only with its case, which a hold keeps"},
	{sweep: RetentionTableLegalRequest, row: "api-14", how: retentionSweepHonors, detail: "RemoveExpiredAbuseRows skips a request naming a held network"},
	{sweep: RetentionTableAbuseAccessLog, row: "api-14", how: retentionSweepHonors, detail: "RemoveExpiredAbuseRows skips a row naming a held network's case"},
	{sweep: "abuse_legal_hold", row: "api-14", how: retentionSweepHonors, detail: "a hold goes at its own deadline (AbuseHoldPass), its copied rows with it or to a later hold"},

	// the edges' rows
	{sweep: RetentionTableTlsConnectionDay, row: "tls-3", how: retentionSweepHonors, detail: "removeExpiredTlsConnectionDaysSql skips a network with a live part 0"},
	{sweep: "tls_connection_record", row: "tls-3", how: retentionSweepHonors, detail: "its hourly drop waits on AbuseHoldCopyPending until each hold has copied"},
	{sweep: "mail_connection_record", row: "mx-2", how: retentionSweepHonors, detail: "its hourly drop keeps a held hour while the hold lives, no copy being registered until ABUSE-M7-01"},
	{sweep: "smtp_send", row: "smtp-3", how: retentionSweepHonors, detail: "its daily drop keeps a held day while the hold lives, no copy being registered until ABUSE-M9-01"},
	{sweep: RetentionTableMailHostKey, row: "mx-3", how: retentionSweepHonors, detail: "removeRetiredMailHostKeysSql skips a network with a live part 0"},
	{sweep: RetentionTableSmtpComplaint, row: "smtp-5", how: retentionSweepHonors, detail: "removeExpiredSmtpComplaintsSql and the sealed copies' clearing skip a network with a live part 0"},
	{sweep: RetentionTableSmtpDmarcDaily, row: "smtp-5", how: retentionSweepHonors, detail: "removeExpiredSmtpDmarcDaysSql skips a network with a live part 0"},
	{sweep: RetentionTableSmtpHalt, row: "smtp-4", how: retentionSweepHonors, detail: "removeExpiredSmtpHaltsSql skips a network with a live part 0"},
	{sweep: RetentionTableSmtpDkimKey, row: "smtp-6", how: retentionSweepHonors, detail: "removeExpiredSmtpDkimKeysSql skips a network with a live part 0"},

	// connect-1 (QD-036: connect-1 joins copied_rows)
	{sweep: "contract", row: "connect-1", how: retentionSweepGap, detail: "the weekly drops and the horizon reaper call AbuseHoldCopyPending, and connect-1's tables register a copy by client, on QD-036; neither is built"},
	{sweep: "close_evidence", row: "connect-1", how: retentionSweepGap, detail: "RemoveExpiredCloseEvidence and its drops call AbuseHoldCopyPending on QD-036; not built"},

	// §7's and §8's sidecar rows, whose sweeps predate holds
	{sweep: RetentionTableNetworkAdminEvent, row: "api-8", how: retentionSweepGap, detail: "TEAM-M1-09's sweep deletes past 400 days whatever holds stand; its SQL owes a HeldNetworkIds clause"},
	{sweep: RetentionTableNetworkAuthKey, row: "api-11", how: retentionSweepGap, detail: "the expired-key sweep owes a HeldNetworkIds clause (TEAM)"},
	{sweep: RetentionTableAccountApiKey, row: "api-11", how: retentionSweepGap, detail: "the expired-key sweep owes a HeldNetworkIds clause (TEAM)"},
	{sweep: RetentionTableNetworkAclAck, row: "api-15", how: retentionSweepGap, detail: "the 7-day ack sweep owes a HeldNetworkIds clause (ACL)"},
	{sweep: RetentionTableNetworkAclShare, row: "api-9", how: retentionSweepGap, detail: "the invite-window sweep owes a HeldNetworkIds clause (ACL)"},
	{sweep: RetentionTableNetworkNameReclaim, row: "api-2", how: retentionSweepGap, detail: "the name tombstone sweep owes a HeldNetworkIds clause (TEAM)"},

	// evs and gkm, on their own side
	{sweep: "evs GC, purge, drops and shred phase 5", row: "evs-*", how: retentionSweepHonors, detail: "sidecar.HoldSet before every delete (EVSB-M2-11, EVS1-M2-19, EVS2's drop jobs)"},
	{sweep: "gkm reconciler", row: "gkm-*", how: retentionSweepHonors, detail: "sidecar.HoldSet (KEYS-M2-16)"},
	{sweep: "evs request log", row: "evs-7", how: retentionSweepOutside, detail: "a hold does not reach evs-7's logs, which go at 7 d (QD-133)"},
	{sweep: "ops request logs", row: "ops-1", how: retentionSweepOutside, detail: "a hold does not reach ops-1's logs, which go at 7 d (QD-133)"},
	{sweep: "finished_task", row: "api-14", how: retentionSweepOutside, detail: "a hold pass's task rows go 24 h after the pass runs, 7 d while its post retries (api-14, owner decision 206)"},

	// tables no §2.8 row lists: holds reach §2.8's rows
	{sweep: RetentionTableNetworkUserActionAttempt, how: retentionSweepOutside, detail: "no §2.8 row: a legacy rate limit's attempts"},
	{sweep: RetentionTableNetworkCreateAttempt, how: retentionSweepOutside, detail: "no §2.8 row: a legacy rate limit's attempts"},
	{sweep: RetentionTableUserAuthReset, how: retentionSweepOutside, detail: "no §2.8 row: a legacy password reset's codes"},
	{sweep: RetentionTableDeviceAddHistory, how: retentionSweepOutside, detail: "no §2.8 row: a legacy rate limit's history"},
	{sweep: RetentionTableProviderBlackholeCheck, how: retentionSweepOutside, detail: "no §2.8 row: provider measurement"},
	{sweep: RetentionTableProviderBandwidthQuota, how: retentionSweepOutside, detail: "no §2.8 row: provider measurement"},
	{sweep: RetentionTableNetworkExtenderReleaseRequest, how: retentionSweepOutside, detail: "no §2.8 row: the extender tier's ledger"},
	{sweep: RetentionTableNetworkExtenderRelease, how: retentionSweepOutside, detail: "no §2.8 row: the extender tier's ledger"},
	{sweep: RetentionTableNetworkExtenderBlockReport, how: retentionSweepOutside, detail: "no §2.8 row: the extender tier's ledger"},
	{sweep: RetentionTableStClientKeyHistory, how: retentionSweepOutside, detail: "no §2.8 row: gone clients' signed key history"},
	{sweep: RetentionTableStClientKeyHead, how: retentionSweepOutside, detail: "no §2.8 row: gone clients' signed key history"},
	{sweep: RetentionTableStClientKeyGone, how: retentionSweepOutside, detail: "no §2.8 row: gone clients' signed key history"},
	{sweep: "provider_work_session_event", how: retentionSweepOutside, detail: "no §2.8 row: provider measurement"},
	{sweep: "st_provider_usage_archive", how: retentionSweepOutside, detail: "no §2.8 row: provider usage"},
	{sweep: "client_reliability", how: retentionSweepOutside, detail: "no §2.8 row: provider measurement"},
	{sweep: "provider_egress_health_history", how: retentionSweepOutside, detail: "no §2.8 row: provider measurement"},
	{sweep: "network_ping", how: retentionSweepOutside, detail: "no §2.8 row: provider measurement"},
}

// The sweeps the table has no line for, and the lines that say nothing.
func retentionSweepHoldViolations(sweeps []string, lines []retentionSweepHoldLine) []string {
	violations := []string{}
	lineSweeps := map[string]bool{}
	for _, line := range lines {
		lineSweeps[line.sweep] = true
		if strings.TrimSpace(line.detail) == "" {
			violations = append(violations, fmt.Sprintf("%s's line says nothing", line.sweep))
		}
		switch line.how {
		case retentionSweepHonors, retentionSweepOutside, retentionSweepGap:
		default:
			violations = append(violations, fmt.Sprintf("%s's line has no standing", line.sweep))
		}
	}
	for _, sweep := range sweeps {
		if !lineSweeps[sweep] {
			violations = append(violations, fmt.Sprintf("%s has no line", sweep))
		}
	}
	slices.Sort(violations)
	return violations
}

// Every sweep this binary runs: each retention label, and each registered partition family.
func retentionSweepsOfThisBinary() []string {
	sweeps := slices.Clone(retentionTables)
	for _, family := range server.PartitionFamilies() {
		sweeps = append(sweeps, family.Name)
	}
	return sweeps
}

// Every sweep of the server has a line saying how it honors holds, why its row is outside their reach, or who owes
// its clause; a fixture sweep with no line fails; and the sweeps that say they read holds in their SQL do.
func TestEveryRetentionSweepHonorsHolds(t *testing.T) {
	if violations := retentionSweepHoldViolations(retentionSweepsOfThisBinary(), retentionSweepHoldLines); 0 < len(violations) {
		t.Fatalf("sweeps the hold table does not answer for:\n%s", strings.Join(violations, "\n"))
	}
	fixture := append(retentionSweepsOfThisBinary(), "fixture_sweep_of_a_listed_row")
	if violations := retentionSweepHoldViolations(fixture, retentionSweepHoldLines); !slices.Equal(violations, []string{"fixture_sweep_of_a_listed_row has no line"}) {
		t.Fatalf("the fixture sweep: %v", violations)
	}
	silent := append(slices.Clone(retentionSweepHoldLines), retentionSweepHoldLine{sweep: "quiet", how: retentionSweepHonors})
	if violations := retentionSweepHoldViolations(nil, silent); !slices.Equal(violations, []string{"quiet's line says nothing"}) {
		t.Fatalf("a line with no detail: %v", violations)
	}

	// what the honoring SQL sweeps say they read
	for name, sql := range map[string]string{
		RetentionTableTlsConnectionDay: removeExpiredTlsConnectionDaysSql,
		RetentionTableMailHostKey:      removeRetiredMailHostKeysSql,
		RetentionTableSmtpComplaint:    removeExpiredSmtpComplaintsSql,
		RetentionTableSmtpDmarcDaily:   removeExpiredSmtpDmarcDaysSql,
		RetentionTableSmtpHalt:         removeExpiredSmtpHaltsSql,
		RetentionTableSmtpDkimKey:      removeExpiredSmtpDkimKeysSql,
		"smtp_complaint sealed copies": clearExpiredSmtpSealedReportsSql,
		RetentionTableAbuseCase:        removeExpiredAbuseCasesSql,
		RetentionTableLegalRequest:     removeExpiredLegalRequestsSql,
		RetentionTableAbuseAccessLog:   removeExpiredAbuseAccessLogSql,
	} {
		if !strings.Contains(sql, "abuse_legal_hold") && !strings.Contains(sql, "$2") {
			t.Errorf("%s's sql reads no hold", name)
		}
	}
	if !HoldCopyRegistered("tls_connection_record") {
		t.Error("tls_connection_record honors holds by its copy, which is not registered")
	}
	// the outside lines name their reason in the design, or say no row lists the table
	for _, line := range retentionSweepHoldLines {
		if line.how == retentionSweepOutside && line.row != "" && !strings.Contains(line.detail, "QD-") && !strings.Contains(line.detail, "owner decision") {
			t.Errorf("%s is outside holds with no decision named", line.sweep)
		}
	}
}
