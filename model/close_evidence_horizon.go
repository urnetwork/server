// Close evidence follows the contract horizon (owner, 2026-11-01; see
// server/db_contract_close_evidence_horizon.go for the rule and its readers).
package model

import (
	"context"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// ctx carrying the contract horizon (server.WithContractHorizon), which close
// evidence retention follows: the horizon reaper's, as its runtime control sets
// it now (GetContractHorizonControl with the reaper task's settings), while the
// reaper's mode is on. One bounded Redis read. With the mode off or in a dry
// run, or when the control cannot be read, ctx is returned as is, so the
// evidence keeps server.ContractRetention: the reaper's brake stops the
// evidence deletes below 91 days too, from the next run. Call it once per task
// run, before any transaction.
func WithContractHorizonControl(ctx context.Context) context.Context {
	control, err := GetContractHorizonControl(ctx, DefaultContractHorizonSettings())
	if err != nil {
		glog.Warningf("[cp]could not read the contract horizon control; close evidence keeps %s: %s\n", server.ContractRetention, err)
		return ctx
	}
	if control.Mode != ContractHorizonModeOn {
		return ctx
	}
	return server.WithContractHorizon(ctx, control.Horizon)
}
