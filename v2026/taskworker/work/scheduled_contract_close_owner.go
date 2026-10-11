// A due scheduled close also retires due siblings that share its financial
// owner. Each sibling's deadline comes from its own queued close; the sibling's
// task later finishes through the terminal fast path without any owner.
package work

import (
	"context"
	"encoding/json"
	"regexp"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/task"
)

// A held owner reschedules its close after this delay instead of waiting for
// admission. The deadline in the arguments is unchanged.
const scheduledCloseOwnerBusyDeferral = 5 * time.Second

// The canonical target name of a queued close, set once the function exists.
var scheduledCloseFunctionName string

var scheduledCloseFunctionVersion = regexp.MustCompile(`/v\d+`)

// Runs after package variables, so the name can refer to its own target.
func init() {
	scheduledCloseFunctionName = task.NewTaskTarget(CloseScheduledContract).TargetFunctionName()
}

// Selects the candidates that are due through their own queued close and uses
// that task's deadline, lowered to an earlier merged wake as its target would.
// A candidate without a queued close has no scheduled deadline and is skipped.
func dueScheduledCloses(now time.Time) model.ContractDeadlineDueFunc {
	return func(ctx context.Context, query server.PgCanQuery, contractIds []server.Id) (deadlines []model.ContractDeadline) {
		candidates := make(map[server.Id]bool, len(contractIds))
		keys := make([]string, 0, len(contractIds))
		for _, contractId := range contractIds {
			candidates[contractId] = true
			keys = append(keys, task.RunOnce("close_scheduled_contract", contractId).String())
		}
		rows, err := query.Query(ctx, `SELECT function_name,args_json,run_at FROM pending_task WHERE run_once_key=ANY($1::text[])`, keys)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var functionName, argsJson string
				var runAt time.Time
				server.Raise(rows.Scan(&functionName, &argsJson, &runAt))
				if scheduledCloseFunctionVersion.ReplaceAllString(functionName, "") != scheduledCloseFunctionName {
					continue
				}
				var args CloseScheduledContractArgs
				if json.Unmarshal([]byte(argsJson), &args) != nil || !args.Private || !candidates[args.ContractId] || args.Deadline.IsZero() {
					continue
				}
				deadline := args.Deadline
				if !runAt.IsZero() && runAt.Before(deadline) {
					deadline = runAt
				}
				if !now.Before(deadline) {
					deadlines = append(deadlines, model.ContractDeadline{ContractId: args.ContractId, Deadline: deadline})
				}
			}
		})
		return
	}
}
