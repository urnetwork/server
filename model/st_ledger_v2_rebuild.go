package model

// st_ledger_v2_rebuild.go — the record side of the operator's explicit
// command to rebuild a settlement v2 epoch's provider rows (VERSION2-FINAL.md
// §6.4 fixed inputs).

import (
	"context"
	"errors"

	"github.com/urnetwork/server"
)

// RequestStLedgerV2EpochRebuild records the operator's explicit request,
// naming who made it, that an epoch's recorded provider rows or null
// declaration be built again. The record stays as it is until rows built for
// the request replace it (RecordStLedgerV2EpochRows). It reports whether the
// epoch has a record; without one nothing changes.
func RequestStLedgerV2EpochRebuild(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64, requestedBy string) (bool, error) {
	if requestedBy == "" {
		return false, errors.New("st ledger v2: a rebuild request must name who makes it")
	}
	key := requireStDeploymentKey(deploymentKey)
	requested := false
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE st_ledger_v2_epoch_rows SET rebuild_requested_by = $4 WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3`,
			key, int64(noId), int64(epoch), requestedBy,
		))
		requested = tag.RowsAffected() == 1
	})
	return requested, nil
}
