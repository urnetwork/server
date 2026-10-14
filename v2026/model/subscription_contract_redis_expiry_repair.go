// Explicit Redis expiry adds a bounded custody fence to the ordinary proof and
// report owners. Durable debit workers finish metadata and reservation release;
// returned status acknowledges neither their completion nor a Redis release.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

const contractRedisExpiryRepairMaxEscrows = 8

// The snapshot advances only after an acknowledged transaction. Grant credit is
// deliberately absent: independent contracts may consume the same owned grant.
type contractRedisExpiryRepairScope struct {
	cutoff   time.Time
	custody  *contractRedisExpiryRepairCustody
	observed *contractRedisExpiryRepairCustody
}

// Billing direction and original usage direction are separate retained facts.
// Stream participants need a different bounded scope and are refused here.
type contractRedisExpiryRepairCustody struct {
	sourceNetworkId, destinationNetworkId server.Id
	companionContractId                   *server.Id
	usageOriginIsSource                   *bool
	capacity                              ByteCount
	created                               time.Time
	escrows                               []contractRedisExpiryRepairEscrow
}

// These are reservation identities and amounts, not mutable grant credit.
type contractRedisExpiryRepairEscrow struct {
	balanceId server.Id
	amount    ByteCount
}

// Pointer presence is part of the retained provenance, including unknown usage.
func (self *contractRedisExpiryRepairCustody) equals(other *contractRedisExpiryRepairCustody) bool {
	if self.sourceNetworkId != other.sourceNetworkId || self.destinationNetworkId != other.destinationNetworkId ||
		self.capacity != other.capacity || !self.created.Equal(other.created) || len(self.escrows) != len(other.escrows) ||
		(self.companionContractId == nil) != (other.companionContractId == nil) ||
		(self.usageOriginIsSource == nil) != (other.usageOriginIsSource == nil) {
		return false
	}
	if self.companionContractId != nil && *self.companionContractId != *other.companionContractId {
		return false
	}
	if self.usageOriginIsSource != nil && *self.usageOriginIsSource != *other.usageOriginIsSource {
		return false
	}
	for i, escrow := range self.escrows {
		if escrow != other.escrows[i] {
			return false
		}
	}
	return true
}

// The contract owner is already checked and locked on apply. The full contract
// journal predicate includes pairs outside the bounded escrow head.
func (self *contractExpiryRepairScope) checkRedisInTx(ctx context.Context, tx server.PgTx, contractId server.Id, lock bool) error {
	var intent, debit, extender bool
	if err := tx.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1),
		EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1),
		EXISTS(SELECT 1 FROM contract_extender WHERE contract_id=$1)`, contractId).Scan(&intent, &debit, &extender); err != nil {
		return err
	}
	if intent {
		return contractExpiryRepairRefusal("legacy_intent_present")
	}
	if debit {
		return contractExpiryRepairRefusal("debit_present")
	}
	if extender {
		return contractExpiryRepairRefusal("extender_scope_unsupported")
	}
	state := &contractRedisExpiryRepairCustody{}
	var streamId *server.Id
	if err := tx.QueryRow(ctx, `SELECT source_network_id,destination_network_id,
		companion_contract_id,stream_id,usage_origin_is_source,transfer_byte_count,create_time
		FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(
		&state.sourceNetworkId, &state.destinationNetworkId, &state.companionContractId,
		&streamId, &state.usageOriginIsSource, &state.capacity, &state.created); err != nil {
		return err
	}
	if streamId != nil {
		return contractExpiryRepairRefusal("stream_scope_unsupported")
	}
	if state.capacity < 0 || (self.expectedPayer != state.sourceNetworkId && self.expectedPayer != state.destinationNetworkId) {
		return contractExpiryRepairRefusal("custody_invalid")
	}
	// An old retained proof is not fresh quiet-period admission. Only this
	// invocation's acknowledged report-state fence admits its own new finals.
	latest := state.created
	reports, err := tx.Query(ctx, `SELECT party,close_time FROM contract_close WHERE contract_id=$1 LIMIT 3`, contractId)
	if err != nil {
		return err
	}
	defer reports.Close()
	reportCount := 0
	for reports.Next() {
		var party ContractParty
		var closeTime time.Time
		if err := reports.Scan(&party, &closeTime); err != nil {
			return err
		}
		reportCount++
		if reportCount > 2 || (party != ContractPartySource && party != ContractPartyDestination) {
			return contractExpiryRepairRefusal("custody_invalid")
		}
		if closeTime.After(latest) {
			latest = closeTime
		}
	}
	if err := reports.Err(); err != nil {
		return err
	}
	reports.Close()
	if self.redis.custody == nil && (self.redis.cutoff.IsZero() || latest.After(self.redis.cutoff)) {
		return contractExpiryRepairRefusal("recent_report")
	}
	lockClause := ""
	if lock {
		lockClause = " FOR UPDATE"
	}
	// LIMIT is inside the escrow relation, before grant point lookups. The ninth
	// row is only a refusal sentinel. OFFSET 0 keeps each grant lookup dependent.
	rows, err := tx.Query(ctx, `SELECT escrow.balance_id,escrow.balance_byte_count,
		escrow.redis_reserved,escrow.settled,grant_row.network_id
		FROM (SELECT balance_id,balance_byte_count,redis_reserved,settled
			FROM transfer_escrow WHERE contract_id=$1 ORDER BY balance_id LIMIT $2`+lockClause+`) AS escrow
		LEFT JOIN LATERAL (SELECT network_id FROM transfer_balance
			WHERE balance_id=escrow.balance_id OFFSET 0) AS grant_row ON true
		ORDER BY escrow.balance_id`, contractId, contractRedisExpiryRepairMaxEscrows+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var escrow contractRedisExpiryRepairEscrow
		var redisReserved, settled bool
		var ownerNetworkId *server.Id
		if err := rows.Scan(&escrow.balanceId, &escrow.amount, &redisReserved, &settled, &ownerNetworkId); err != nil {
			return err
		}
		if !redisReserved || settled {
			return contractExpiryRepairRefusal("reservation_mode_changed")
		}
		if ownerNetworkId == nil || *ownerNetworkId != self.expectedPayer || escrow.amount < 0 {
			return contractExpiryRepairRefusal("custody_invalid")
		}
		state.escrows = append(state.escrows, escrow)
		if len(state.escrows) > contractRedisExpiryRepairMaxEscrows {
			return contractExpiryRepairRefusal("escrow_scope_capped")
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(state.escrows) == 0 {
		return contractExpiryRepairRefusal("reservation_mode_changed")
	}
	if self.redis.custody != nil && !self.redis.custody.equals(state) {
		return contractExpiryRepairRefusal("custody_changed")
	}
	self.redis.observed = state
	return nil
}

// Preview and apply retain the existing scoped repair's five-minute quiet policy
// and the ordinary expiry proof owner. Every write rechecks payer, reports and the
// bounded Redis custody tuple. Its no-stream scope needs no stream cleanup.
// Debit workers finish metadata and reservation release independently of this
// observation deadline. Clock posts retain their existing synchronous attempt.
func RepairRedisContractExpiry(ctx context.Context, request ContractExpiryRepairRequest) (ContractExpiryRepairResult, error) {
	return repairContractExpiry(ctx, request, true)
}
