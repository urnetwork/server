package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// One zero-byte fixture contract: settled, closed by both parties with no
// bytes, its escrow settled with nothing paid out, one close report, a usage
// snapshot of zero bytes, no sweep and no extender party, unless a field says
// otherwise.
type zeroByteTestContract struct {
	contractId server.Id
	createTime time.Time
	closeTime  *time.Time
	// "" is a NULL outcome; with no dispute the contract is open (the
	// generated column)
	outcome string
	dispute bool
	// each party's close, by its bytes; a party absent never closed
	closes        map[ContractParty]ByteCount
	escrowSettled bool
	escrowPayout  ByteCount
	sweep         bool
	// the sweep's payment; nil is unpaid
	paymentId *server.Id
	extender  bool
	// the usage snapshot's bytes; a credited outcome always has a snapshot
	// (the usage guard)
	usageBytes ByteCount
}

// A zero-byte contract minted at createTime and closed ten minutes later.
func newZeroByteTestContract(createTime time.Time) *zeroByteTestContract {
	closeTime := createTime.Add(10 * time.Minute)
	return &zeroByteTestContract{
		contractId:    horizonTestContractId(createTime),
		createTime:    createTime,
		closeTime:     &closeTime,
		outcome:       "settled",
		closes:        map[ContractParty]ByteCount{ContractPartySource: 0, ContractPartyDestination: 0},
		escrowSettled: true,
	}
}

func insertZeroByteTestContracts(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, contracts ...*zeroByteTestContract) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		for _, contract := range contracts {
			var outcome *string
			var usage *contractUsageSnapshot
			if contract.outcome != "" {
				outcome = &contract.outcome
				usage = &contractUsageSnapshot{Version: 1, ByteCount: contract.usageBytes, Providers: []contractProviderUsage{
					{ClientId: f.destinationId, NetworkId: f.destinationNetworkId, ByteCount: contract.usageBytes},
				}}
			}
			var closeTime *time.Time
			if contract.closeTime != nil {
				utc := contract.closeTime.UTC()
				closeTime = &utc
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,
				 transfer_byte_count,create_time,close_time,outcome,dispute,provider_usage,usage_origin_is_source)
				VALUES($1,$2,$3,$4,$5,$2,1048576,$6,$7,$8,$9,$10,true)`,
				contract.contractId, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId,
				contract.createTime.UTC(), closeTime, outcome, contract.dispute, usage))
			for party, byteCount := range contract.closes {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count)
					VALUES($1,$2,$3)`, contract.contractId, party, byteCount))
			}
			if _, ok := contract.closes[ContractPartySource]; ok {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close_report
					(contract_id,party,report_id,used_transfer_byte_count,checkpoint)
					VALUES($1,'source',$2,$3,false)`, contract.contractId, server.NewId(), contract.closes[ContractPartySource]))
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count,settled,payout_byte_count)
				VALUES($1,$2,0,$3,$4)`, contract.contractId, f.balanceId, contract.escrowSettled, contract.escrowPayout))
			if contract.sweep {
				sweepTime := contract.createTime.UTC()
				if closeTime != nil {
					sweepTime = *closeTime
				}
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
					(contract_id,balance_id,network_id,payout_byte_count,payout_net_revenue_nano_cents,payment_id,sweep_time)
					VALUES($1,$2,$3,$4,0,$5,$6)`, contract.contractId, f.balanceId, f.destinationNetworkId,
					contract.closes[ContractPartyDestination], contract.paymentId, sweepTime))
			}
			if contract.extender {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_extender(contract_id,extender_id,party,client_id,network_id)
					VALUES($1,$2,'source',$3,$4)`, contract.contractId, server.NewId(), f.sourceId, f.sourceNetworkId))
			}
		}
	}, server.TxReadCommitted, server.OptNoRetry())
}

// The zero-byte contracts minted ten days before now, each past the 8-day
// zero-byte horizon and inside the 35-day horizon, and those beside them that
// the zero-byte pass must keep, by the reason it keeps them, and one minted
// inside the zero-byte horizon.
func newZeroByteTestScenario(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, now time.Time) (
	zeroByte []*zeroByteTestContract,
	kept map[string][]*zeroByteTestContract,
	young *zeroByteTestContract,
) {
	t.Helper()
	const day = 24 * time.Hour
	minted := now.Add(-10 * day)
	at := func(i int) time.Time { return minted.Add(time.Duration(i) * time.Second) }
	// a payment completed under attribution review: its contracts stay
	reviewPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
		completed: true, completeTime: horizonTestPointer(now.Add(-30 * day)), attributionReview: true})

	zeroByte = []*zeroByteTestContract{newZeroByteTestContract(at(0)), newZeroByteTestContract(at(1))}

	withBytes := newZeroByteTestContract(at(2))
	withBytes.closes = map[ContractParty]ByteCount{ContractPartySource: 4096, ContractPartyDestination: 4096}
	withBytes.usageBytes = 4096
	withBytes.sweep = true
	// same-network traffic: bytes without a sweep or usage credit
	unsweptBytes := newZeroByteTestContract(at(3))
	unsweptBytes.closes = map[ContractParty]ByteCount{ContractPartySource: 512, ContractPartyDestination: 0}
	usageBytes := newZeroByteTestContract(at(4))
	usageBytes.usageBytes = 7
	// a zero-byte sweep of a payment under attribution review
	review := newZeroByteTestContract(at(6))
	review.sweep = true
	review.paymentId = &reviewPaymentId

	open := newZeroByteTestContract(at(7))
	open.outcome = ""
	open.closeTime = nil
	open.closes = map[ContractParty]ByteCount{}
	open.escrowSettled = false
	// in review: disputed, no outcome
	inReview := newZeroByteTestContract(at(8))
	inReview.outcome = ""
	inReview.dispute = true
	inReview.escrowSettled = false
	disputed := newZeroByteTestContract(at(9))
	disputed.outcome = "dispute_resolved_to_source"
	disputed.dispute = true
	oneSided := newZeroByteTestContract(at(10))
	oneSided.closes = map[ContractParty]ByteCount{ContractPartySource: 0}
	extender := newZeroByteTestContract(at(11))
	extender.extender = true
	unsettled := newZeroByteTestContract(at(12))
	unsettled.escrowSettled = false
	paidOut := newZeroByteTestContract(at(13))
	paidOut.escrowPayout = 1
	recentClose := newZeroByteTestContract(at(14))
	recentClose.closeTime = horizonTestPointer(now.Add(-7 * day))

	young = newZeroByteTestContract(now.Add(-7 * day))

	// by the zero-byte pass's reasons, which are metric labels
	kept = map[string][]*zeroByteTestContract{
		"bytes":        {withBytes, unsweptBytes, usageBytes, review},
		"open":         {open},
		"unresolved":   {inReview},
		"disputed":     {disputed},
		"one_sided":    {oneSided},
		"extender":     {extender},
		"unsettled":    {unsettled, paidOut},
		"recent_close": {recentClose},
	}
	all := append([]*zeroByteTestContract{young}, zeroByte...)
	for _, contracts := range kept {
		all = append(all, contracts...)
	}
	insertZeroByteTestContracts(t, ctx, f, all...)
	return
}
