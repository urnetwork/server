// Exact scheduled closes register retained routing and publish it atomically.
package model

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// The caller admits the actual owner's queue key before its transaction. A
// successful handoff makes this intent selectable and publishes that owner in
// the same commit; accepted outcome, proof, reports and backoff remain intact.
// A busy, retired or changed owner returns false so the child can yield under
// its own queue key and resolve fresh custody on its next ordinary invocation.
func QueueRegisteredLegacyCloseContractInTx(clientSession *session.ClientSession, tx server.PgTx,
	contractId server.Id, owner ContractCloseOwner) (bool, error) {
	if contractId == (server.Id{}) || !owner.valid() {
		return false, fmt.Errorf("invalid exact legacy close publication")
	}
	if !server.TxOwnsKeys(tx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(owner.runOnce())}) {
		return false, fmt.Errorf("exact legacy close publication requires its admitted queue owner")
	}
	ctx := clientSession.Ctx
	var payerHint, sourceHint *server.Id
	err := tx.QueryRow(ctx, `SELECT payer_network_id,source_client_id FROM legacy_settlement_intent
		WHERE contract_id=$1 FOR UPDATE SKIP LOCKED`, contractId).Scan(&payerHint, &sourceHint)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var locked server.Id
	err = tx.QueryRow(ctx, `SELECT contract_id FROM transfer_contract
		WHERE contract_id=$1 AND outcome IS NULL FOR UPDATE SKIP LOCKED`, contractId).Scan(&locked)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	actual, sourceId, err := readContractCloseOwnerInConn(ctx, tx, contractId)
	if errors.Is(err, errContractCloseOwnerUnresolved) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if actual != owner {
		return false, nil
	}
	var payer *server.Id
	if owner.Kind == ContractCloseOwnerPayerNetwork {
		payer = &owner.Id
	}
	payerMatches := payer == nil && payerHint == nil || payer != nil && payerHint != nil && *payer == *payerHint
	if !payerMatches || sourceHint == nil || *sourceHint != sourceId {
		_, err = tx.Exec(ctx, `UPDATE legacy_settlement_intent
			SET payer_network_id=$2,source_client_id=$3 WHERE contract_id=$1`, contractId, payer, sourceId)
		if err != nil {
			return false, err
		}
	}
	QueueLegacyCloseSettlementsInTx(clientSession, tx, owner)
	return true, nil
}
