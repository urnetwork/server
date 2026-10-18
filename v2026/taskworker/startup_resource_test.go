// Real startup scheduling uses normalized direct-PostgreSQL resources too.
package taskworker

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

// TestEnv supplies only isolated credentials. Re-express its exact endpoint
// through the supported list/template form, then open both real pools and run
// each production initializer. The old raw-map ownership parser refused after
// ordinary schedules committed, before either accounting family was seeded.
func TestTaskworkerStartupSchedulesExpiryWithNormalizedMaintenanceResource(t *testing.T) {
	t.Setenv("WARP_DOMAIN", "startup-resource.example")
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		controller.SetVerifySettings(model.DefaultVerifySettings())
		defer controller.SetVerifySettings(nil)
		resource := server.Vault.RequireSimpleResource(server.MaintenancePgVaultResourceName)
		values := map[string]any{}
		for key, value := range resource.Parse() {
			values[key] = value
		}
		for key, variable := range map[string]string{
			"authority": "SYNTHETIC_STARTUP_PG_AUTHORITY",
			"db":        "SYNTHETIC_STARTUP_PG_DATABASE",
			"user":      "SYNTHETIC_STARTUP_PG_USER",
		} {
			resolved := resource.String(key)
			if len(resolved) != 1 {
				t.Fatal("isolated database resource does not define one required value")
			}
			t.Setenv(variable, resolved[0])
			values[key] = []string{"{{ env:" + variable + " }}"}
		}
		raw, err := json.Marshal(values)
		server.Raise(err)
		defer server.Vault.PushSimpleResource(server.MaintenancePgVaultResourceName, raw)()
		server.PgReset()
		defer server.PgReset()
		for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
			for range 2 {
				if err := initTaskScheduleForProfile(ctx, profile); err != nil {
					t.Fatal("normalized startup resource failed initialization", profile, err)
				}
			}
			server.Db(ctx, func(conn server.PgConn) {
				var closes, debits, legacy int
				var runAt time.Time
				server.Raise(conn.QueryRow(ctx, `SELECT count(*),min(run_at) FROM pending_task
					WHERE function_name=$1`, task.NewTaskTarget(work.CloseExpiredContracts).TargetFunctionName()).Scan(&closes, &runAt))
				server.Raise(conn.QueryRow(ctx, `SELECT
					count(*) FILTER(WHERE run_once_key LIKE '["flush_transfer_debits_%'),
					count(*) FILTER(WHERE run_once_key LIKE '["flush_legacy_settlements_%')
					FROM pending_task`).Scan(&debits, &legacy))
				if closes != 1 || runAt.After(server.NowUtc()) || debits != model.TransferDebitShardCount || legacy != model.LegacySettlementShardCount {
					t.Fatal("startup omitted or duplicated expiry/accounting tasks", profile, closes, debits, legacy)
				}
			})
		}
	})
}
