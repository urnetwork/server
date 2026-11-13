// Runs the contract partition signals' real catalog query (SIGNALS.md
// §2.4h-§2.4j) on the PostgreSQL 18 TestEnv, on plain tables, a real prepared
// and finalized cutover, and the current-week layout of
// server.Testing_PartitionContracts. The psql/SSH transport stays outside.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

const contractPartitionNativeHost = "pg-contract-partition-fixture"

// The only SQL adaptation binds the psql hostname variable.
func contractPartitionNativeSql(t testing.TB, query string) string {
	t.Helper()
	prologue, body, found := strings.Cut(query, "\n")
	if !found || prologue != pgPrimaryHostnameMetaCommand || strings.Count(body, ":'monitor_primary_hostname'") != 1 {
		t.Fatal("native read lost the exact producer SQL boundary")
	}
	return strings.Replace(body, ":'monitor_primary_hostname'", "$1", 1)
}

// Routes the public signals to the test database; every cell is text.
type contractPartitionNativeSource struct {
	*syntheticSource
	t     testing.TB
	calls int
}

func (self *contractPartitionNativeSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	self.calls++
	rows := []Row{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, contractPartitionNativeSql(self.t, query), contractPartitionNativeHost)
		server.WithPgResult(result, err, func() {
			width := len(result.FieldDescriptions())
			for result.Next() {
				row := make(Row, width)
				destinations := make([]any, width)
				for i := range row {
					destinations[i] = &row[i]
				}
				server.Raise(result.Scan(destinations...))
				rows = append(rows, row)
			}
		})
	})
	return rows, nil
}

func contractPartitionNativeRun(t testing.TB, ctx context.Context, signal Signal) Alerts {
	t.Helper()
	source := &contractPartitionNativeSource{syntheticSource: &syntheticSource{}, t: t}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: contractPartitionNativeHost, Roles: []string{"pg-primary"}}}
	settings.Now = time.Now
	alerts, err := signal.Run(ctx, settings)
	if err != nil || source.calls != 1 {
		t.Fatalf("%s did not read the database exactly once: %v", signal.Key(), err)
	}
	return alerts
}

func contractPartitionNativeHealthy(t testing.TB, ctx context.Context, layout string) {
	t.Helper()
	for _, signal := range []Signal{NewContractPartitionRunwaySignal(), NewContractPartitionRetentionBlockedSignal(), NewContractPartitionCutoverPendingSignal()} {
		if alerts := contractPartitionNativeRun(t, ctx, signal); len(alerts) != 0 {
			t.Fatalf("%s: %s alerted: %s %s", layout, signal.Key(), alerts[0].Class, alerts[0].Observed)
		}
	}
}

func contractPartitionNativeExec(t testing.TB, ctx context.Context, sql string) {
	t.Helper()
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, sql))
	}, server.OptReadWrite(), server.OptNoRetry())
}

func contractPartitionNativeAlert(t testing.TB, alerts Alerts, class string) Alert {
	t.Helper()
	for _, alert := range alerts {
		if alert.Class == class {
			return alert
		}
	}
	t.Fatalf("no %s alert in %d alerts", class, len(alerts))
	return Alert{}
}

// Plain tables are healthy; a cutover prepared two weeks ahead warns only
// inside its window; after the swap the weeks before the cutover week route to
// the legacy partition, so the runway is healthy without weekly partitions for
// them, and dropping the cutover's weekly partitions pages.
func TestContractPartitionNativeCutoverAhead(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if server.IsContractPartitioned(ctx) {
			// URNETWORK_TEST_CONTRACT_PARTITIONS cut over at the current week;
			// the future cutover week is covered by the synthetic controls
			contractPartitionNativeHealthy(t, ctx, "current-week layout")
			return
		}
		contractPartitionNativeHealthy(t, ctx, "plain")

		currentWeek := server.ContractPartitionWeekStart(server.NowUtc())
		cutoverWeek := currentWeek.Add(2 * contractPartitionWeek)
		logf := func(format string, args ...any) { t.Logf(format, args...) }
		if err := server.MigrateContractsToPartitions(ctx, server.ContractPartitionMigrateOptions{CutoverWeek: cutoverWeek, Logf: logf}); err != nil {
			t.Fatal(err)
		}
		contractPartitionNativeHealthy(t, ctx, "prepared outside the window")
		settings := DefaultContractPartitionCutoverSettings()
		settings.Window = 21 * 24 * time.Hour
		alert := contractPartitionNativeAlert(t, contractPartitionNativeRun(t, ctx, NewContractPartitionCutoverPendingSignalWithSettings(settings)), "contract-partition-cutover-pending")
		if !strings.Contains(alert.Observed, "cutover_week="+cutoverWeek.Format(time.DateOnly)+" ") ||
			!strings.Contains(alert.Observed, "bounded_tables=["+strings.Join(server.ContractPartitionTables, " ")+"]") ||
			!strings.Contains(alert.Observed, "staged_parents=["+strings.Join(server.ContractPartitionTables, " ")+"]") {
			t.Fatalf("the prepared cutover was misread: %s", alert.Observed)
		}

		if err := server.MigrateContractsToPartitions(ctx, server.ContractPartitionMigrateOptions{Finalize: true, Logf: logf}); err != nil {
			t.Fatal(err)
		}
		contractPartitionNativeHealthy(t, ctx, "finalized two weeks ahead")

		for week := 0; week <= server.ContractPartitionAheadWeeks; week++ {
			contractPartitionNativeExec(t, ctx, fmt.Sprintf("DROP TABLE public.%s", server.ContractPartitionName("contract_extender", cutoverWeek.Add(time.Duration(week)*contractPartitionWeek))))
		}
		alert = contractPartitionNativeAlert(t, contractPartitionNativeRun(t, ctx, NewContractPartitionRunwaySignal()), "contract-partition-runway")
		if alert.Severity != SeverityPage || alert.Frame != "contract_extender" ||
			!strings.Contains(alert.Observed, "worst_future_weeks=1 worst_last_covered_week="+currentWeek.Add(contractPartitionWeek).Format(time.DateOnly)) ||
			!strings.Contains(alert.Observed, "transfer_contract=6 ") {
			t.Fatalf("the dropped weeks did not page: %s", alert.Observed)
		}
	})
}

// The current-week layout is healthy; a shortened horizon judges the legacy
// partitions, and dropping future weeks pages.
func TestContractPartitionNativeCurrentWeekLayout(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if !server.IsContractPartitioned(ctx) {
			server.Testing_PartitionContracts(ctx)
		}
		contractPartitionNativeHealthy(t, ctx, "current-week layout")
		currentWeek := server.ContractPartitionWeekStart(server.NowUtc())

		settings := DefaultContractPartitionRetentionSettings()
		settings.Retention, settings.Grace = 0, 0
		alert := contractPartitionNativeAlert(t, contractPartitionNativeRun(t, ctx, NewContractPartitionRetentionBlockedSignalWithSettings(settings)), "contract-partition-retention-blocked")
		if !strings.Contains(alert.Observed, "blocked_partitions=6 ") ||
			!strings.Contains(alert.Observed, "oldest_blocked=transfer_contract_legacy oldest_upper_bound="+currentWeek.Format(time.DateOnly)+" ") {
			t.Fatalf("the legacy partitions were misjudged: %s", alert.Observed)
		}

		for week := 2; week <= server.ContractPartitionAheadWeeks; week++ {
			contractPartitionNativeExec(t, ctx, fmt.Sprintf("DROP TABLE public.%s", server.ContractPartitionName("contract_close", currentWeek.Add(time.Duration(week)*contractPartitionWeek))))
		}
		alert = contractPartitionNativeAlert(t, contractPartitionNativeRun(t, ctx, NewContractPartitionRunwaySignal()), "contract-partition-runway")
		if alert.Frame != "contract_close" || !strings.Contains(alert.Observed, "worst_future_weeks=1 ") {
			t.Fatalf("the dropped weeks did not page: %s", alert.Observed)
		}
	})
}
