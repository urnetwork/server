//go:build sidecar_localstack && sidecar_testhooks

// M10's format-2 schedules swept on T3 (EVS2-M10-35; §23.6's M10 row for §11's format 2, §22.8's rules): the T3 half
// of sidecar's sidecartest/fault/m10_format2_sweep_test.go, which holds the sweep's list of cases, the T2 report and
// the gate list for M10's Gn-out. Here, each case of FS-64 to FS-73 and FS-89 to FS-96 whose T3 run is this package's,
// with the clauses this package's own T3 tests run beside it, runs under every rule §22.8 gives its row on T3's Nets,
// where sidecart3 names this package's block as each world's instances, so every push, dry run, DROP and read goes
// through server/cli/evs's handler on the private Redis and the local stack's MinIO: the fixed schedule, its twin, the
// explorer at a merge's 10 seeds over both, every write's lost answer for a row on §22.8's unknown list, and each
// committed trace. The nightly 100 seeds are each case's own TestFs<n><Case>OnT3's (fs_format2_test.go,
// fs72_rollback_test.go). A case catalog.yml flags store_clock runs with the store clock, and its clauses with it.
//
// The report it logs gives each case of the sweep's rows on T3: run, with what ran; registered on T3 with its T3 tests
// in another package (ABUSE-M10-01's hold cases in server's model, once they land), by name; or missing, no builder
// registered. FS-73 is no builder and runs as TestFs73OnT3 alone, and FS-114's purge and drop cases run in server's
// file-cluster suite on local/file-cluster-t3's clusters (TestFileClusterSuitePassesOnT3, under
// sidecar_filecluster_t3), which this package does not host.
//
// What T3 proves is the commit side. A drop job's start and a purge sweep's deletes wait on each cluster's settle flag
// (FileCluster.SettlePassed, FS-114), and the local stack's sidecar.yml names no settle_passed_build for its clusters,
// so every schedule here holds the job to that wait, with no drop moved and no segment deleted, and the deletes, the
// job's refusals and what readers see after them run on T2 alone (fs_format2_test.go's header).
//
// The schedules hold instances at barriers, so this needs sidecar_testhooks; the stack must be up
// (sidecar/local/stack.sh), and this fails rather than skips without it.

package evshost_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server/sidecart3"
)

// The sweep's rows: sidecar's m10Format2RowIds, format 2's evs rows of §23.6's M10 row.
func m10Format2T3RowIds() []string {
	return []string{
		"FS-64", "FS-65", "FS-66", "FS-67", "FS-68", "FS-69", "FS-70", "FS-71", "FS-72", "FS-73",
		"FS-89", "FS-90", "FS-91", "FS-92", "FS-93", "FS-94", "FS-95", "FS-96", "FS-114",
	}
}

// A clause beside a case's registered schedule, by the name this package's own T3 test runs it under, so its traces
// are that test's.
type m10Format2T3Clause struct {
	name  string
	build fault.Builder
}

// A case this package runs on T3, with the clauses its own T3 tests run beside it.
type m10Format2T3Case struct {
	rowId    string
	caseName string
	clauses  []*m10Format2T3Clause
}

// The cases whose T3 run is this package's, as fs_format2_test.go and fs72_rollback_test.go run them.
func m10Format2T3Cases() []*m10Format2T3Case {
	return []*m10Format2T3Case{
		{
			rowId: "FS-68",
			clauses: []*m10Format2T3Clause{
				{name: "rules", build: fault.Fs68Rules},
				{name: "platform rule", build: fault.Fs68PlatformRule},
				{name: "config", build: fault.Fs68Kind(protocol.RepoKind_REPO_KIND_CONFIG)},
				{name: "mailcfg", build: fault.Fs68Kind(protocol.RepoKind_REPO_KIND_MAILCFG)},
				{name: "lock", build: fault.Fs68LockedPastTheWindow},
			},
		},
		{
			rowId:    "FS-72",
			caseName: "migration",
			clauses: []*m10Format2T3Clause{
				{name: "migration killed after step 6", build: fault.Fs72MigrationKilledAt(evsservice.BarrierPushStep6After)},
				{name: "migration killed before the raise", build: fault.Fs72MigrationKilledAt(evsservice.BarrierMigrateRaiseBefore)},
				{name: "migration killed after the raise", build: fault.Fs72MigrationKilledAt(evsservice.BarrierMigrateRaiseAfter)},
				{name: "migration killed before the head CAS", build: fault.Fs72MigrationKilledAt(evsservice.BarrierHeadCasBefore)},
				{name: "migration killed after the head CAS", build: fault.Fs72MigrationKilledAt(evsservice.BarrierHeadCasAfter)},
			},
		},
		{rowId: "FS-72", caseName: "rollback"},
		{rowId: "FS-91", clauses: []*m10Format2T3Clause{{name: "within the window", build: fault.Fs91ReleasedWithinTheWindow}}},
		{rowId: "FS-94", clauses: []*m10Format2T3Clause{{name: "record class", build: fault.Fs94RecordClass}}},
		{
			rowId: "FS-95",
			clauses: []*m10Format2T3Clause{
				{name: "locked", build: fault.Fs95Locked},
				{name: "config", build: fault.Fs95Kind(protocol.RepoKind_REPO_KIND_CONFIG)},
				{name: "mailcfg", build: fault.Fs95Kind(protocol.RepoKind_REPO_KIND_MAILCFG)},
				{name: "owner-bound", build: fault.Fs95OwnerBound},
				{name: "list missing a version", build: fault.Fs95ListMismatch(false)},
				{name: "list adding a version", build: fault.Fs95ListMismatch(true)},
			},
		},
	}
}

// T3's rules for testCase at a merge's seeds, with the store clock exactly when catalog.yml flags the case.
func m10Format2T3RuleSettings(tb testing.TB, testCase *fault.Case) *fault.RuleSettings {
	settings := sidecart3.DefaultSettings()
	if testCase.StoreClock {
		settings.StoreClock = sidecart3.DefaultStoreClockSettings()
	}
	ruleSettings := sidecart3.RuleSettings(tb, settings)
	ruleSettings.Seeds = fault.MergeSeeds()
	return ruleSettings
}

// Writes what one run under a row's rules did.
func m10Format2T3WriteRun(report *strings.Builder, label string, rowReport *fault.RowReport) {
	outcome := "passed"
	if rowReport.Failed() {
		outcome = fmt.Sprintf("FAILED, %d runs", len(rowReport.Failures)+len(rowReport.Errors))
	}
	fmt.Fprintf(report, "    %s: %s: %s\n", label, outcome, strings.ReplaceAll(rowReport.String(), "\n", "\n      "))
}

// The list this package runs names each case once, a case catalog.yml holds at M10's exit for the sweep's rows on T3,
// and each clause once.
func TestM10Format2SweepOnT3NamesItsCases(t *testing.T) {
	catalog := fault.DefaultCatalog()
	seen := []string{}
	for _, sweepCase := range m10Format2T3Cases() {
		testCase := catalog.Case(sweepCase.rowId, sweepCase.caseName)
		switch {
		case testCase == nil:
			t.Errorf("the T3 sweep names %s %q, which the catalog does not hold", sweepCase.rowId, sweepCase.caseName)
			continue
		case !slices.Contains(m10Format2T3RowIds(), testCase.Row.Id) || !testCase.AtGate("M10-out") || !slices.Contains(testCase.Tiers(), fault.TierT3):
			t.Errorf("%s is no case of format 2's rows on T3 at M10's exit", testCase)
		case slices.Contains(seen, testCase.String()):
			t.Errorf("the T3 sweep names %s twice", testCase)
		}
		seen = append(seen, testCase.String())
		clauseNames := []string{}
		for _, clause := range sweepCase.clauses {
			if clause.name == "" || clause.build == nil || slices.Contains(clauseNames, clause.name) {
				t.Errorf("%s's clause %q is unnamed, unbuilt or named twice", testCase, clause.name)
			}
			clauseNames = append(clauseNames, clause.name)
		}
	}
}

// §23.6's M10 rows for format 2 on T3: each case whose T3 run is this package's under every rule its row takes on T3's
// Nets at a merge's seeds, with the clauses beside it, and the report of every case of the sweep's rows on T3. A
// case or clause that fails fails this test; a case missing on T3 is reported, the gate reading it missing on its own.
func TestM10Format2SweepOnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		catalog := fault.DefaultCatalog()
		var report strings.Builder
		runCount, clauseCount, elsewhereCount, missingCount := 0, 0, 0, 0
		for _, testCase := range catalog.GateCases("M10-out") {
			if !slices.Contains(m10Format2T3RowIds(), testCase.Row.Id) || !slices.Contains(testCase.Tiers(), fault.TierT3) {
				continue
			}
			fmt.Fprintf(&report, "\n%s\n", testCase)
			index := slices.IndexFunc(m10Format2T3Cases(), func(sweepCase *m10Format2T3Case) bool {
				return sweepCase.rowId == testCase.Row.Id && sweepCase.caseName == testCase.Name
			})
			registration := fault.Lookup(testCase.Row.Id, testCase.Name, fault.TierT3)
			switch {
			case 0 <= index && registration == nil:
				tb.Errorf("this package runs %s on T3, and nothing registers it on T3", testCase)
			case 0 <= index:
				runCount += 1
				sweepCase := m10Format2T3Cases()[index]
				storeClock := ""
				if testCase.StoreClock {
					storeClock = ", the store clock translated"
				}
				fmt.Fprintf(&report, "  T3: run, %s and its clauses under §22.8's rules at a merge's seeds%s:\n", registration, storeClock)
				m10Format2T3WriteRun(&report, "registered case", sidecart3.RunRow(tb, testCase.Row.Id, testCase.Name, m10Format2T3RuleSettings(tb, testCase)))
				for _, clause := range sweepCase.clauses {
					clauseCount += 1
					clauseReport := fault.RunClause(tb, testCase.Row.Id, clause.name, clause.build, m10Format2T3RuleSettings(tb, testCase))
					m10Format2T3WriteRun(&report, "clause "+clause.name, clauseReport)
				}
			case registration != nil:
				elsewhereCount += 1
				fmt.Fprintf(&report, "  T3: %s is registered, and its T3 tests run in another package, which the sweep's gate list names\n", registration)
			case testCase.Row.Id == "FS-73":
				fmt.Fprintf(&report, "  T3: no builder: this package's TestFs73OnT3 drives stock git and git-lfs through evsgit's world (QD-995)\n")
			case testCase.Row.Id == "FS-114":
				fmt.Fprintf(&report, "  T3: no builder: server's file-cluster suite on local/file-cluster-t3 (TestFileClusterSuitePassesOnT3, sidecar_filecluster_t3) runs fs114-%s-delete-one-host-held and fs114-%s-delete-two-hosts-held\n", testCase.Name, testCase.Name)
			default:
				missingCount += 1
				fmt.Fprintf(&report, "  T3: missing, no builder registered\n")
			}
		}
		tb.Logf("§23.6's M10 rows for format 2 on T3 (EVS2-M10-35): %d cases run under §22.8's rules at a merge's seeds with %d clauses beside them, %d registered with their T3 tests in another package, %d missing on T3; the deletes wait on settle here, as this file's header says\n%s",
			runCount, clauseCount, elsewhereCount, missingCount, report.String())
	})
}
