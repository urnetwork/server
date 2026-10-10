// Global expiry recovery publishes existing bounded tasks; it performs no close.
package main

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/urnetwork/server/taskworker/work"
)

// Both fresh and already-pending work enter the ordinary durable owners.
func invokeContractExpiryRecovery(ctx context.Context) (work.ContractExpiryRecoveryResult, error) {
	return work.QueueContractExpiryRecovery(ctx)
}

// The command acknowledges only a committed queue request. Its finite envelope
// carries no customer identities or raw database/configuration error strings.
func runContractExpiryRecovery(parent context.Context, writer io.Writer,
	queue func(context.Context) (work.ContractExpiryRecoveryResult, error),
) (exitCode int) {
	started := time.Now().UTC()
	status := "queue_failed"
	var result *work.ContractExpiryRecoveryResult
	defer func() {
		if recover() != nil {
			status = "queue_failed"
			result = nil
			exitCode = 1
		}
		if err := json.NewEncoder(writer).Encode(struct {
			Schema      int                                `json:"schema"`
			Kind        string                             `json:"kind"`
			StartedAt   time.Time                          `json:"started_at"`
			CompletedAt time.Time                          `json:"completed_at"`
			Status      string                             `json:"status"`
			Request     *work.ContractExpiryRecoveryResult `json:"request"`
		}{1, "contract-expiry-queue-v1", started, time.Now().UTC(), status, result}); err != nil {
			exitCode = 1
		}
	}()
	ctx, cancel := context.WithTimeout(parent, contractExpiryRepairCommandBudget)
	defer cancel()
	if ctx.Err() != nil {
		status = "parent_canceled"
		return 1
	}
	requested, err := queue(ctx)
	if err != nil {
		return 1
	}
	status = "queued"
	result = &requested
	return 0
}
