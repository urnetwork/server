package model

// st_ledger_v2_activation.go — the durable record of an st deployment's
// settlement v2 activation (SV-9): vault2's registration of the configured
// pool, as the server first observed it, with the vault2 and STSettlement2 it
// was configured with. Registration is permanent on chain, so the record is
// written once and never changes; only the operator's explicit command, naming
// it exactly, clears one that is wrong.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// ErrStLedgerV2ActivationRecorded refuses an activation record that differs
// from the one already recorded for the deployment.
var ErrStLedgerV2ActivationRecorded = errors.New("st ledger v2: the deployment's activation is already recorded")

// StLedgerV2Activation is vault2's registration of pool NoId from FirstEpoch.
// The addresses are the lowercase 0x forms st.yml declares.
type StLedgerV2Activation struct {
	NoId                uint64
	FirstEpoch          uint64
	VaultV2Address      string
	SettlementV2Address string
	CreateTime          time.Time
}

// RecordStLedgerV2Activation records a deployment's activation once.
// Recording the same activation again is a no-op; a different one is
// ErrStLedgerV2ActivationRecorded, naming the recorded activation.
func RecordStLedgerV2Activation(ctx context.Context, deploymentKey StDeploymentKey, activation *StLedgerV2Activation) error {
	key := requireStDeploymentKey(deploymentKey)
	var existing *StLedgerV2Activation
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO st_ledger_v2_activation (deployment_key, no_id, first_epoch, vault_v2_address, settlement_v2_address, create_time)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (deployment_key) DO NOTHING
			`,
			key, int64(activation.NoId), int64(activation.FirstEpoch), activation.VaultV2Address, activation.SettlementV2Address, server.NowUtc(),
		))
		if tag.RowsAffected() == 0 {
			existing = getStLedgerV2Activation(ctx, tx, key)
		}
	})
	if existing != nil && (existing.NoId != activation.NoId || existing.FirstEpoch != activation.FirstEpoch ||
		existing.VaultV2Address != activation.VaultV2Address || existing.SettlementV2Address != activation.SettlementV2Address) {
		return fmt.Errorf("%w: pool %d from epoch %d with vault_v2_address %s and settlement_v2_address %s", ErrStLedgerV2ActivationRecorded,
			existing.NoId, existing.FirstEpoch, existing.VaultV2Address, existing.SettlementV2Address)
	}
	return nil
}

// GetStLedgerV2Activation returns a deployment's recorded activation, or nil.
func GetStLedgerV2Activation(ctx context.Context, deploymentKey StDeploymentKey) *StLedgerV2Activation {
	key := requireStDeploymentKey(deploymentKey)
	var activation *StLedgerV2Activation
	server.Tx(ctx, func(tx server.PgTx) {
		activation = getStLedgerV2Activation(ctx, tx, key)
	})
	return activation
}

// GetStLedgerV2ActivationIfInstalled is GetStLedgerV2Activation for a
// database that may predate the record's table (migration 832): without the
// table there is no activation. A deployment without the v2 pair reads the
// record this way, so settlement v1 alone needs nothing that v2 added.
func GetStLedgerV2ActivationIfInstalled(ctx context.Context, deploymentKey StDeploymentKey) *StLedgerV2Activation {
	key := requireStDeploymentKey(deploymentKey)
	var activation *StLedgerV2Activation
	server.Tx(ctx, func(tx server.PgTx) {
		var installed bool
		server.Raise(tx.QueryRow(ctx, `SELECT to_regclass('public.st_ledger_v2_activation') IS NOT NULL`).Scan(&installed))
		if installed {
			activation = getStLedgerV2Activation(ctx, tx, key)
		}
	})
	return activation
}

// ClearStLedgerV2Activation removes a deployment's recorded activation when
// it is exactly the named one, and reports whether it did.
func ClearStLedgerV2Activation(ctx context.Context, deploymentKey StDeploymentKey, named *StLedgerV2Activation) bool {
	key := requireStDeploymentKey(deploymentKey)
	cleared := false
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM st_ledger_v2_activation
				WHERE deployment_key = $1 AND no_id = $2 AND first_epoch = $3 AND vault_v2_address = $4 AND settlement_v2_address = $5
			`,
			key, int64(named.NoId), int64(named.FirstEpoch), named.VaultV2Address, named.SettlementV2Address,
		))
		cleared = tag.RowsAffected() == 1
	})
	return cleared
}

func getStLedgerV2Activation(ctx context.Context, tx server.PgTx, key string) *StLedgerV2Activation {
	var activation *StLedgerV2Activation
	result, err := tx.Query(
		ctx,
		`
			SELECT no_id, first_epoch, vault_v2_address, settlement_v2_address, create_time
			FROM st_ledger_v2_activation
			WHERE deployment_key = $1
		`,
		key,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			activation = &StLedgerV2Activation{}
			var noId, firstEpoch int64
			server.Raise(result.Scan(&noId, &firstEpoch, &activation.VaultV2Address, &activation.SettlementV2Address, &activation.CreateTime))
			activation.NoId, activation.FirstEpoch = uint64(noId), uint64(firstEpoch)
		}
	})
	return activation
}
