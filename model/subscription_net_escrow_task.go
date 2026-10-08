// Legacy settlement mirrors have a coalesced durable owner outside the financial page.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The pending task contains only immutable scope. Revisions and amounts are
// read together from current committed PostgreSQL state on every execution.
type legacyNetEscrowMirrorPayload struct {
	Private   bool      `json:"_private_task_arguments"`
	Version   int       `json:"version"`
	BalanceId server.Id `json:"balance_id"`
}

// This revision was acknowledged by the existing absolute Redis publisher.
// It is a source generation, never an amount predicted by the producer.
type LegacyNetEscrowMirrorResult struct {
	Revision int64 `json:"revision"`
}

func decodeLegacyNetEscrowMirror(data []byte) (legacyNetEscrowMirrorPayload, error) {
	var payload legacyNetEscrowMirrorPayload
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil || decoder.Decode(new(any)) != io.EOF ||
		!payload.Private || payload.Version != 1 || payload.BalanceId == (server.Id{}) {
		return payload, errors.New("invalid legacy net escrow mirror scope")
	}
	return payload, nil
}

// Financial and completion transactions own the exact mirror queue key before
// touching its pending row. The upsert retains generation/custody semantics;
// financial producers predeclare this key with all their grant keys.
func queueLegacyNetEscrowMirrorsInTx(ctx context.Context, tx server.PgTx, balanceIds []server.Id) {
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	ids := slices.Clone(balanceIds)
	slices.SortFunc(ids, server.Id.Cmp)
	for _, id := range slices.Compact(ids) {
		payload, err := json.Marshal(legacyNetEscrowMirrorPayload{Private: true, Version: 1, BalanceId: id})
		server.Raise(err)
		task.ScheduleTaskInTx(tx, ApplyLegacyNetEscrowMirror, json.RawMessage(payload), owner,
			task.RunOnce("legacy_net_escrow_mirror", id), task.MaxTime(netEscrowMirrorTimeout), task.RequireQueueOwnership(tx))
	}
}

// A foreground post publishes only current durable cache entries. Every miss
// already has the task queued by the same financial commit; it cannot turn
// this page's next contract into a wait for historical escrow.
func refreshCachedLegacyNetEscrow(ctx context.Context, balanceIds []server.Id) {
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var pending map[server.Id]netEscrowSnapshot
	server.Db(bounded, func(conn server.PgConn) {
		pending = readCachedNetEscrowSnapshots(bounded, conn, balanceIds)
	})
	ids := make([]server.Id, 0, len(pending))
	for _, id := range balanceIds {
		if _, ok := pending[id]; ok {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		reconcileNetEscrowBatch(bounded, pending, ids, true)
	}
}

// The durable task owns only this balance's optional mirror. No grant, intent,
// outcome or pending-task row lock is held during history reads or Redis I/O.
// A lost Redis acknowledgement repeats the same fenced absolute write.
func ApplyLegacyNetEscrowMirror(_ json.RawMessage, clientSession *session.ClientSession) (result *LegacyNetEscrowMirrorResult, returnErr error) {
	identity, ok := task.ExecutionIdentityFromContext(clientSession.Ctx)
	if !ok {
		return nil, errors.New("legacy net escrow mirror requires durable task execution")
	}
	bounded, cancel := context.WithTimeout(clientSession.Ctx, netEscrowMirrorTimeout)
	defer cancel()
	server.HandleError(func() {
		var data []byte
		server.Db(bounded, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(bounded, `SELECT args_json FROM pending_task WHERE task_id=$1 AND function_name=$2`,
				identity.TaskId, task.NewTaskTarget(ApplyLegacyNetEscrowMirror).TargetFunctionName()).Scan(&data))
		})
		payload, err := decodeLegacyNetEscrowMirror(data)
		server.Raise(err)
		ids := []server.Id{payload.BalanceId}
		pending := readMirrorNetEscrowSnapshots(bounded, ids)
		reconcileNetEscrowBatch(bounded, pending, ids, true)
		result = &LegacyNetEscrowMirrorResult{Revision: pending[payload.BalanceId].revision}
	}, func(err error) { returnErr = err })
	return
}

// Completion owns the mirror queue key before copying/deleting its pending row
// and invoking this post. A producer is admitted on the same key before its
// financial writes, so this exact revision check either retains the current
// owner or publishes its successor. No grant lock, census or Redis I/O runs here.
func ApplyLegacyNetEscrowMirrorPost(args json.RawMessage, result *LegacyNetEscrowMirrorResult, clientSession *session.ClientSession, tx server.PgTx) error {
	payload, err := decodeLegacyNetEscrowMirror(args)
	if err != nil {
		return err
	}
	if result == nil || result.Revision < 0 {
		return errors.New("legacy net escrow mirror completion revision absent")
	}
	var revision int64
	if err := tx.QueryRow(clientSession.Ctx, `SELECT COALESCE((SELECT revision FROM transfer_balance_net_escrow_revision WHERE balance_id=$1),0)`, payload.BalanceId).Scan(&revision); err != nil {
		return err
	}
	if revision != result.Revision {
		queueLegacyNetEscrowMirrorsInTx(clientSession.Ctx, tx, []server.Id{payload.BalanceId})
	}
	return nil
}
