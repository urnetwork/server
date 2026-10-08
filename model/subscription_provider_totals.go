// Legacy provider totals are a replay-safe projection of already-committed earnings.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type legacyProviderTotal struct {
	NetworkId server.Id `json:"network_id"`
	Bytes     ByteCount `json:"bytes"`
	Revenue   NanoCents `json:"revenue"`
}

// The exact payload survives contract/sweep retention. The private marker is
// understood by task logging, including workers that do not register this target.
// Deploy those readers everywhere before enabling the producer.
type legacyProviderTotalsPayload struct {
	Private    bool                  `json:"_private_task_arguments"`
	Version    int                   `json:"version"`
	ContractId server.Id             `json:"contract_id"`
	Totals     []legacyProviderTotal `json:"totals"`
	Applied    bool                  `json:"applied"`
}

func queueLegacyProviderTotalsInTx(ctx context.Context, tx server.PgTx, contractId server.Id, payouts map[server.Id]*contractPayout) server.Id {
	payload := legacyProviderTotalsPayload{Private: true, Version: 1, ContractId: contractId}
	for networkId, payout := range payouts {
		payload.Totals = append(payload.Totals, legacyProviderTotal{NetworkId: networkId, Bytes: payout.payoutByteCount, Revenue: payout.payout})
	}
	slices.SortFunc(payload.Totals, func(a, b legacyProviderTotal) int { return a.NetworkId.Cmp(b.NetworkId) })
	data, err := json.Marshal(payload)
	server.Raise(err)
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	scheduled, taskId := task.ScheduleTaskInTxIfAbsent(tx, ApplyLegacyProviderTotals, json.RawMessage(data), owner,
		task.RunOnce("legacy_provider_totals", contractId), task.MaxTime(10*time.Second))
	if !scheduled {
		// Never merge a second allocation into an existing task's immutable
		// arguments. The contract outcome must admit exactly one insertion.
		server.Raise(errors.New("legacy provider total task already exists"))
	}
	return taskId
}

// ApplyLegacyProviderTotals uses its durable pending row, not the invocation's
// possibly-stale arguments. Totals and the replay marker share one transaction;
// there is deliberately no RunPost whose retry/retention could repeat or lose it.
func ApplyLegacyProviderTotals(_ json.RawMessage, clientSession *session.ClientSession) (result *struct{}, returnErr error) {
	identity, ok := task.ExecutionIdentityFromContext(clientSession.Ctx)
	if !ok {
		return nil, errors.New("legacy provider totals require durable task execution")
	}
	if batch, ok := clientSession.Ctx.Value(legacyProviderTotalsBatchKey{}).(*legacyProviderTotalsBatch); ok {
		if err := batch.apply(clientSession.Ctx); err != nil {
			return nil, err
		}
		return &struct{}{}, nil
	}
	bounded, cancel := context.WithTimeout(clientSession.Ctx, 5*time.Second)
	defer cancel()
	networkIds, err := readLegacyProviderOwnership(bounded, identity.TaskId)
	if err != nil {
		return nil, err
	}
	returnErr = runLegacyProviderTotalsOwnedTx(bounded, networkIds, func(tx server.PgTx) error {
		return applyLegacyProviderTotalsWithOwnershipInTx(bounded, tx, identity.TaskId, networkIds)
	})
	if returnErr == nil {
		result = &struct{}{}
	}
	return
}

func applyLegacyProviderTotalsInTx(ctx context.Context, tx server.PgTx, taskId server.Id) error {
	return applyLegacyProviderTotalsWithOwnershipInTx(ctx, tx, taskId, nil)
}

// The production owner supplies its complete pre-admitted account set. Amounts
// and replay status always come from the locked durable payload.
func applyLegacyProviderTotalsWithOwnershipInTx(ctx context.Context, tx server.PgTx, taskId server.Id, networkIds []server.Id) error {
	var data string
	err := tx.QueryRow(ctx, `SELECT args_json FROM pending_task WHERE task_id=$1 AND function_name=$2 FOR UPDATE`,
		taskId, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, errors.New("legacy provider total task ownership missing"))
	}
	if err != nil {
		return withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, err)
	}
	payload, err := decodeLegacyProviderTotals(data)
	if err != nil {
		return withLegacyProviderTotalsPhase(legacyProviderTotalsAllocation, err)
	}
	if networkIds != nil {
		if err := validateLegacyProviderOwnership(payload, networkIds); err != nil {
			return err
		}
	}
	if payload.Applied {
		return nil
	}
	for _, total := range payload.Totals {
		if err := writeLegacyProviderTotalInTx(ctx, tx, total); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE pending_task SET args_json=jsonb_set(args_json::jsonb,'{applied}','true'::jsonb)::text WHERE task_id=$1`, taskId)
	if err != nil {
		return withLegacyProviderTotalsPhase(legacyProviderTotalsAppliedMarker, err)
	}
	if tag.RowsAffected() != 1 {
		return withLegacyProviderTotalsPhase(legacyProviderTotalsAppliedMarker, fmt.Errorf("legacy provider total marker ownership missing"))
	}
	return nil
}

// Decode the retained representation equally for admission hints and locked rows.
func decodeLegacyProviderTotals(data string) (payload legacyProviderTotalsPayload, returnErr error) {
	decoder := json.NewDecoder(bytes.NewBufferString(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil || decoder.Decode(new(any)) != io.EOF ||
		!payload.Private || payload.Version != 1 || payload.ContractId == (server.Id{}) || len(payload.Totals) == 0 {
		return payload, errors.New("invalid legacy provider total payload")
	}
	for i, total := range payload.Totals {
		if total.NetworkId == (server.Id{}) || total.Bytes < 0 || total.Revenue < 0 || (total.Bytes == 0 && total.Revenue == 0) ||
			(i > 0 && payload.Totals[i-1].NetworkId.Cmp(total.NetworkId) >= 0) {
			return payload, errors.New("invalid legacy provider total allocation")
		}
	}
	return payload, nil
}

// The caller owns every contributing pending row until the total and markers commit.
func writeLegacyProviderTotalInTx(ctx context.Context, tx server.PgTx, total legacyProviderTotal) error {
	_, err := tx.Exec(ctx, `INSERT INTO account_balance
            (network_id,provided_byte_count,provided_net_revenue_nano_cents) VALUES($1,$2,$3)
            ON CONFLICT(network_id) DO UPDATE SET
            provided_byte_count=account_balance.provided_byte_count+EXCLUDED.provided_byte_count,
            provided_net_revenue_nano_cents=account_balance.provided_net_revenue_nano_cents+EXCLUDED.provided_net_revenue_nano_cents`,
		total.NetworkId, total.Bytes, total.Revenue)
	if err == nil {
		observeAccountBalanceWriteForTest(ctx, tx, total.NetworkId)
	}
	return withLegacyProviderTotalsPhase(legacyProviderTotalsAccountWrite, err)
}
