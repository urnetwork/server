// Recurring shard tasks discover durable intents and wake bounded payer tasks.
// Discovery never debits grants and does not require a foreground shared row.
package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/server"
)

var errLegacySettlementDispatchBudget = errors.New("legacy settlement dispatch budget")
var errLegacySettlementDiscoveryBudget = errors.New("legacy settlement payer discovery budget")

const legacySettlementDispatchBudget = 5 * time.Second
const legacySettlementRegistrationBudget = 2 * time.Second
const legacySettlementDiscoveryBudget = legacySettlementDispatchBudget - legacySettlementRegistrationBudget

// Identity is private task data. Cursor is a bounded compatibility-registration
// traversal; PayerCursor controls fair service independently of contract density.
type LegacySettlementDispatchResult struct {
	Private            bool                         `json:"_private_task_arguments"`
	PayerNetworkIds    []server.Id                  `json:"payer_network_ids"`
	Cursor             *LegacySettlementCursor      `json:"cursor,omitempty"`
	PayerCursor        *LegacySettlementPayerCursor `json:"payer_cursor,omitempty"`
	Probes             int                          `json:"probes"`
	Registered         int                          `json:"registered"`
	RegistrationFailed bool                         `json:"registration_failed"`
	More               bool                         `json:"more"`
}

// A fixed payer round and at most sixteen probes prevent a dense account from
// concealing a later one. Accepted results and scheduled owners commit together
// in the task Post; a crash before that handoff simply repeats discovery.
func DispatchLegacySettlementPayers(ctx context.Context, shard int, after *LegacySettlementCursor,
	payerAfter *LegacySettlementPayerCursor) (result LegacySettlementDispatchResult, returnErr error) {
	result.Private = true
	if shard < 0 || shard >= LegacySettlementShardCount {
		return result, fmt.Errorf("invalid legacy settlement dispatch shard")
	}
	bounded, cancel := context.WithTimeoutCause(ctx, legacySettlementDispatchBudget, errLegacySettlementDispatchBudget)
	defer cancel()
	result, returnErr = dispatchLegacySettlementPayersPage(ctx, bounded, shard, after, payerAfter,
		nextLegacySettlementPayer, registerLegacySettlementPayerDispatchPage)
	return
}

// Registration owns a fixed missing-key batch within the caller's budget.
// Registered work enters the next bounded discovery turn.
func registerLegacySettlementPayerDispatchPage(ctx context.Context, shard int, after *LegacySettlementCursor) (*LegacySettlementCursor, int) {
	return after, registerLegacySettlementPayers(ctx, shard)
}

// Only this page owns its time budget. A completed discovery prefix can be
// handed back when that budget ends; parent cancellation remains an error.
// Invocation-local operations permit deterministic database barriers.
func dispatchLegacySettlementPayersPage(ctx, bounded context.Context, shard int, after *LegacySettlementCursor,
	payerAfter *LegacySettlementPayerCursor,
	next func(context.Context, int, *LegacySettlementPayerCursor) (*server.Id, *LegacySettlementPosition),
	register func(context.Context, int, *LegacySettlementCursor) (*LegacySettlementCursor, int),
) (result LegacySettlementDispatchResult, returnErr error) {
	result.Private = true
	result.Cursor = after
	if payerAfter != nil {
		cursor := *payerAfter
		result.PayerCursor = &cursor
	}
	server.HandleError(func() {
		// Reserve registration's existing allowance inside the same turn.
		// Otherwise slow registered payers can consume every page before any
		// missing key is enrolled. Completed discovery still precedes cleanup.
		discovery, discoveryCancel := context.WithTimeoutCause(bounded, legacySettlementDiscoveryBudget, errLegacySettlementDiscoveryBudget)
		var discoveryErr error
		server.HandleError(func() {
			defer discoveryCancel()
			if result.PayerCursor == nil {
				result.PayerCursor = beginLegacySettlementPayerRound(discovery, shard)
			}
			for probe := 0; probe < legacySettlementPayerProbeLimit && result.PayerCursor != nil; probe++ {
				payer, ready := next(discovery, shard, result.PayerCursor)
				if payer == nil {
					result.PayerCursor = nil
					break
				}
				result.Probes++
				if ready != nil {
					result.PayerNetworkIds = append(result.PayerNetworkIds, *payer)
				}
				result.PayerCursor.After = payer
			}
		}, func(err error) { discoveryErr = err })
		discoveryYielded := discoveryErr != nil && bounded.Err() == nil &&
			context.Cause(discovery) == errLegacySettlementDiscoveryBudget && isSettlementPageCancellation(discoveryErr)
		if discoveryErr != nil && !discoveryYielded {
			server.Raise(discoveryErr)
		}
		// Discover registered work before optional compatibility registration:
		// transaction cleanup can outlive its own query context. This prefix
		// retains scheduling custody if our page deadline expires in cleanup.
		registrationCtx, registrationCancel := context.WithTimeout(bounded, legacySettlementRegistrationBudget)
		server.HandleError(func() {
			defer registrationCancel()
			result.Cursor, result.Registered = register(registrationCtx, shard, after)
		}, func(error) {
			result.Cursor, result.Registered = after, 0
			result.RegistrationFailed = true
		})
		// HandleError contains rescue-handler panics. Propagate cancellation
		// from the normal path, including a cancellation during joined cleanup.
		// A preceding optional SQL refusal does not invalidate ready discovery.
		if bounded.Err() != nil {
			server.Raise(bounded.Err())
		}
		// Multiple dispatchers acquire payer task keys in the same order.
		slices.SortFunc(result.PayerNetworkIds, server.Id.Cmp)
		result.More = discoveryYielded || result.PayerCursor != nil || result.Registered > 0
	}, func(err error) {
		if ctx.Err() == nil && bounded.Err() != nil && context.Cause(bounded) == errLegacySettlementDispatchBudget &&
			result.Probes > 0 && isSettlementPageCancellation(err) {
			// The interrupted probe has not moved After. Publication of this
			// prefix and its successor belongs to the task's ordinary Post.
			slices.SortFunc(result.PayerNetworkIds, server.Id.Cmp)
			result.More = true
			return
		}
		returnErr = err
	})
	return
}
