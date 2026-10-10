// Zero escrow contract creation, used while the contract degradation valve
// makes new contracts zero cost (network_degradation_model.go). While
// contracts close too slowly, every open contract keeps the escrow it reserved
// from its payer's balance, and a payer whose balance is held that way is
// refused new contracts. While the valve is open, public and companion
// contracts are created the way network contracts are
// (createContractNoEscrowInTx): no transfer_escrow row, no payer_network_id and
// no change to any transfer_balance. They settle through the existing no-escrow
// path, which claims a terminal outcome with no sweep, payout, debit or
// provider earnings, also after the valve closes.
//
// Creation is one short read committed transaction of PostgreSQL statements
// only. It never enters escrow admission: no transferEscrowTx or payer
// admission queue, no runRedisContractAdmission or Redis reservation, and no
// companion escrow transaction, so no transaction is held open across a Redis
// or network call. Redis is written only after commit, by the contract hole
// post and the origin notification, as for every contract.
//
// What still holds, in that transaction:
//   - the acceptance-test balance drain and per-client data caps refuse the
//     would-be payer with the ordinary "Insufficient balance", as escrow
//     admission does; both check in memory unless the payer is allowlisted,
//     drained or capped, and any fallback is a PostgreSQL read;
//   - a companion still requires its reverse origin, selected as
//     createCompanionTransferEscrow selects it, and records it in
//     companion_contract_id, which also makes the destination its usage origin;
//   - the probe shard fence admits a shard endpoint only while its shard is
//     active and only as the would-be payer, as escrow admission does,
//     including a shard reply carrier that inherits its shard anchor's payer;
//   - the endpoint lifecycle fence and the contract hole, extender and provider
//     work records match the other creation paths.
//
// The signed size is the caller's request: no balance shrinks it, and the
// internal prober's companion clamp, which bounds a reservation, does not
// apply. The priority is the would-be payer's normal escrow priority, read from
// its active grants without reserving them.
//
// What zero escrow gives up, beyond provider payouts:
//   - settlement meters the usage of escrowed contracts only, so zero escrow
//     traffic does not advance a per-client data cap: a capped client is
//     refused, but a client below its cap is not moved toward it;
//   - a probe shard's private grant no longer bounds the shard's traffic, and
//     shard cleanup, which finds a shard's open contracts by payer, does not
//     wait for its zero escrow contracts. They hold no grant, so deleting the
//     shard account leaves nothing unsettled; they close by report or deadline.
package model

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// createCompanionTransferEscrow's plain origin read without its prober
// reservation column. The two branches are disjoint on open, so the global
// earliest is the earlier of each branch's earliest, and each branch is a
// bounded index range. $1 and $2 are the origin's source and destination, the
// companion's reverse; $3 is the earliest close time of a lingering origin.
const zeroEscrowPlainOriginSql = `
	SELECT contract_id
	FROM (
		(
			SELECT contract_id, create_time
			FROM transfer_contract
			WHERE
				-- equivalent to the generated open flag but opaque to legacy
				-- false-zero open/outcome indexes
				(CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) AND
				COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
				source_id = $1 AND
				destination_id = $2 AND
				companion_contract_id IS NULL
			ORDER BY create_time ASC
			LIMIT 1
		)

		UNION ALL

		(
			SELECT contract_id, create_time
			FROM transfer_contract
			WHERE
				open = false AND
				$3 <= close_time AND
				COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
				source_id = $1 AND
				destination_id = $2 AND
				companion_contract_id IS NULL
			ORDER BY create_time ASC
			LIMIT 1
		)

		ORDER BY create_time ASC
		LIMIT 1
	) AS earliest_origin
`

// createCompanionTransferEscrow's chained origin read, the earliest companion
// in the reverse direction, without its prober reservation column. $4 and $5
// are the companion's source and destination networks. A probe shard's reply
// carrier inherits its anchor's private payer, the shard, as there; a zero
// escrow anchor has no payer, and its would-be payer is its destination.
const zeroEscrowChainedOriginSql = `
	SELECT
		contract_id,
		CASE WHEN (payer_network_id IS NULL OR payer_network_id = $4) AND
			source_network_id = $5 AND
			destination_network_id = $4 AND
			EXISTS (SELECT 1 FROM prober_shard_run WHERE network_id = $4)
		THEN $4::uuid END AS inherited_private_payer
	FROM (
		(
			SELECT contract_id, create_time, payer_network_id, source_network_id, destination_network_id
			FROM transfer_contract
			WHERE
				-- equivalent to the generated open flag but opaque to legacy
				-- false-zero open/outcome indexes
				(CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) AND
				COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
				source_id = $1 AND
				destination_id = $2 AND
				companion_contract_id IS NOT NULL
			ORDER BY create_time ASC
			LIMIT 1
		)

		UNION ALL

		(
			SELECT contract_id, create_time, payer_network_id, source_network_id, destination_network_id
			FROM transfer_contract
			WHERE
				open = false AND
				$3 <= close_time AND
				COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
				source_id = $1 AND
				destination_id = $2 AND
				companion_contract_id IS NOT NULL
			ORDER BY create_time ASC
			LIMIT 1
		)

		ORDER BY create_time ASC
		LIMIT 1
	) AS earliest_companion_origin
`

// The escrow insert of createTransferEscrowInTx without a payer. A null
// payer_network_id and no transfer_escrow row are what settle the contract on
// the no-escrow path (validateContractFreeSettlementOwnerInTx). $7 is the
// companion origin, which also decides the usage origin.
const zeroEscrowContractInsertSql = `
	WITH creation_clock AS MATERIALIZED (
		SELECT clock_timestamp() AT TIME ZONE 'UTC' AS create_time
	)
	INSERT INTO transfer_contract (
		contract_id,
		source_network_id,
		source_id,
		destination_network_id,
		destination_id,
		transfer_byte_count,
		companion_contract_id,
		usage_origin_is_source,
		create_time,
		priority,
		expiration_time
	)
	SELECT
		$1, $2, $3, $4, $5, $6, $7, ($7::uuid IS NULL),
		create_time, $8,
		date_trunc('milliseconds', create_time) + $9 * INTERVAL '1 millisecond'
	FROM creation_clock
	RETURNING expiration_time
`

// Creates a public contract that escrow would have charged to the source
// network. The result has no balances; its size is the request.
func CreateZeroEscrowContract(
	ctx context.Context,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
) (transferEscrow *TransferEscrow, returnErr error) {
	// Pin before acquisition, as the other creation paths do.
	ctx = providerWorkSessionContext(ctx)
	leaveTransaction := server.EnterContractCreationStage(ctx, server.ContractStageTransaction)
	server.Tx(ctx, func(tx server.PgTx) {
		transferEscrow, returnErr = createZeroEscrowContractInTx(
			ctx,
			tx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			// source is the would-be payer
			sourceNetworkId,
			contractTransferByteCount,
			nil,
		)
	}, server.TxReadCommitted, server.OptNoRetry())
	leaveTransaction()
	if returnErr != nil {
		return
	}
	leavePosts := server.EnterContractCreationStage(ctx, server.ContractStagePostCommit)
	notifyCommittedContractOrigin(ctx, sourceId, destinationId)
	leavePosts()
	defer server.EnterContractCreationStage(ctx, server.ContractStageClientStamp)()
	// the identity escrow would have charged, as createTransferEscrow stamps it
	StampTopLevelClientContractTime(ctx, sourceId)
	return
}

// Creates a companion contract answering the earliest reverse origin, which
// escrow would have charged to the destination network, the source of that
// origin, or to a probe shard whose reply carrier inherits it. With no origin
// it returns ErrMissingCompanionOrigin, which the controller waits out. The
// result has no balances; its size is the request.
func CreateZeroEscrowCompanionContract(
	ctx context.Context,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
	originContractTimeout time.Duration,
) (transferEscrow *TransferEscrow, returnErr error) {
	// Pin before acquisition, as the other creation paths do.
	ctx = providerWorkSessionContext(ctx)
	stampClientId := destinationId
	leaveTransaction := server.EnterContractCreationStage(ctx, server.ContractStageTransaction)
	server.Tx(ctx, func(tx server.PgTx) {
		transferEscrow, returnErr, stampClientId = nil, nil, destinationId
		originOutcome := server.ContractCompanionOriginError
		defer func() { server.RecordContractCompanionOriginOutcome(ctx, originOutcome) }()
		companionContractId, inheritedPayerNetworkId := zeroEscrowCompanionOriginInTx(
			ctx,
			tx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			originContractTimeout,
		)
		if companionContractId == nil {
			originOutcome = server.ContractCompanionOriginMissing
			returnErr = ErrMissingCompanionOrigin
			return
		}
		payerNetworkId := destinationNetworkId
		if inheritedPayerNetworkId != nil {
			payerNetworkId = *inheritedPayerNetworkId
			// stamp the identity that would have funded it, as
			// createCompanionTransferEscrow does after a payer handoff
			if payerNetworkId == sourceNetworkId {
				stampClientId = sourceId
			}
		}
		transferEscrow, returnErr = createZeroEscrowContractInTx(
			ctx,
			tx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			payerNetworkId,
			contractTransferByteCount,
			companionContractId,
		)
		if returnErr == nil {
			originOutcome = server.ContractCompanionOriginFound
		}
	}, server.TxReadCommitted, server.OptNoRetry())
	leaveTransaction()
	if returnErr != nil {
		return
	}
	leavePosts := server.EnterContractCreationStage(ctx, server.ContractStagePostCommit)
	notifyCommittedContractOrigin(ctx, sourceId, destinationId)
	leavePosts()
	defer server.EnterContractCreationStage(ctx, server.ContractStageClientStamp)()
	StampTopLevelClientContractTime(ctx, stampClientId)
	return
}

// Refuses and inserts in the order of escrow admission, without reserving: the
// drain and data cap refusals, the probe shard fence, the priority read, the
// endpoint lifecycle fence, the shard deadline and companion origin rechecks
// after that wait, then the contract. A refusal returns before any write.
func createZeroEscrowContractInTx(
	ctx context.Context,
	tx server.PgTx,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	payerNetworkId server.Id,
	contractTransferByteCount ByteCount,
	companionContractId *server.Id,
) (*TransferEscrow, error) {
	if contractTransferByteCount < 0 {
		return nil, fmt.Errorf("negative contract transfer byte count")
	}
	now := server.NowUtc()
	if 0 < contractTransferByteCount {
		// both refuse exactly as createTransferEscrowInTx refuses
		if err := testBalanceDrainEscrowError(
			testBalanceDrainActive(ctx, tx, payerNetworkId, now),
			contractTransferByteCount,
		); err != nil {
			return nil, err
		}
		payerClientId := sourceId
		if sourceNetworkId != payerNetworkId {
			payerClientId = destinationId
		}
		if err := clientDataCapEscrowError(ctx, tx, payerNetworkId, payerClientId, contractTransferByteCount, now); err != nil {
			return nil, err
		}
	}
	// A probe shard is admitted only while active and only as the would-be
	// payer, exactly as escrow admission fences it.
	shardDeadline, err := validateProberShardPayerInTx(ctx, tx, sourceNetworkId, destinationNetworkId, payerNetworkId)
	if err != nil {
		return nil, err
	}
	priority := zeroEscrowContractPriorityInTx(ctx, tx, payerNetworkId, contractTransferByteCount, now)
	if err := lockActiveContractClientsInTx(ctx, tx, sourceNetworkId, sourceId, destinationNetworkId, destinationId); err != nil {
		return nil, err
	}
	// the lifecycle fence can wait; the shard and the origin may expire meanwhile
	if err := validateProberShardAdmissionDeadlineInTx(ctx, tx, shardDeadline); err != nil {
		return nil, err
	}
	if err := validateCompanionContractExpirationInTx(ctx, tx, companionContractId); err != nil {
		return nil, err
	}

	contractId := server.NewId()
	var expirationTime time.Time
	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		batch.Queue(
			zeroEscrowContractInsertSql,
			contractId,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			contractTransferByteCount,
			companionContractId,
			priority,
			DefaultContractExpiration.Milliseconds(),
		).QueryRow(func(row pgx.Row) error { return row.Scan(&expirationTime) })
		batch.Queue(
			contractExtenderInsertSql,
			contractId,
			sourceId,
			destinationId,
			ContractPartySource,
			ContractPartyDestination,
		)
	})
	server.AddTxCommitCount(tx, &contractOpenedCounter, 1)
	providerWorkRetainReservationInTx(ctx, tx, contractId)
	contractHoleEventInTx(ctx, tx, contractId, sourceId, destinationId, "create", expirationTime)

	return &TransferEscrow{
		ContractId:          contractId,
		CompanionContractId: companionContractId,
		ExpirationTime:      expirationTime,
		TransferByteCount:   contractTransferByteCount,
		Priority:            priority,
		Balances:            []*TransferEscrowBalance{},
	}, nil
}

// The earliest open or lingering plain origin in the reverse direction, else
// the earliest such companion, which chains an asymmetric reply carrier (see
// createCompanionTransferEscrow). Both are nil when there is neither. The
// inherited payer is set only for a probe shard's chained reply carrier.
func zeroEscrowCompanionOriginInTx(
	ctx context.Context,
	tx server.PgTx,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	originContractTimeout time.Duration,
) (originContractId *server.Id, inheritedPayerNetworkId *server.Id) {
	lingerCloseTime := server.NowUtc().Add(-originContractTimeout)
	func() {
		leaveRead := server.BeginContractCompanionOriginRead(ctx, server.ContractCompanionPlainOrigin)
		defer leaveRead()
		// the origin direction is reversed
		result, err := tx.Query(ctx, zeroEscrowPlainOriginSql, destinationId, sourceId, lingerCloseTime)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&originContractId))
			}
		})
	}()
	if originContractId != nil {
		return
	}
	func() {
		leaveRead := server.BeginContractCompanionOriginRead(ctx, server.ContractCompanionFallbackOrigin)
		defer leaveRead()
		result, err := tx.Query(
			ctx,
			zeroEscrowChainedOriginSql,
			destinationId,
			sourceId,
			lingerCloseTime,
			sourceNetworkId,
			destinationNetworkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&originContractId, &inheritedPayerNetworkId))
			}
		})
	}()
	return
}

// The priority escrow admission would give the would-be payer, without
// reserving or locking a grant: the blend of paid and unpaid grants it would
// fund the request from, in its funding order (earliest expiry first). Nothing
// is reserved, so each active grant counts its whole balance. A zero-byte
// request takes the first grant, as a zero-byte anchor does, and a payer with
// no active grant is unpaid. It is never the trusted priority of network and
// friends-and-family contracts. One page of grants bounds the read; a request
// that page does not cover blends the page.
func zeroEscrowContractPriorityInTx(
	ctx context.Context,
	tx server.PgTx,
	payerNetworkId server.Id,
	contractTransferByteCount ByteCount,
	now time.Time,
) Priority {
	defer server.EnterContractCreationStage(ctx, server.ContractStageGrantSelection)()
	type grant struct {
		paid             bool
		balanceByteCount ByteCount
	}
	grants := []grant{}
	result, err := tx.Query(
		ctx,
		escrowTransferBalanceSql+` ORDER BY end_time, start_time, balance_id LIMIT $3`,
		payerNetworkId,
		now,
		redisGrantPageRows,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var balanceId server.Id
			var g grant
			var startTime time.Time
			var endTime time.Time
			server.Raise(result.Scan(&balanceId, &g.paid, &g.balanceByteCount, &startTime, &endTime))
			grants = append(grants, g)
		}
	})

	var priority Priority
	fundingCount := 0
	remaining := contractTransferByteCount
	for _, g := range grants {
		if g.paid {
			priority += PaidPriority
		} else {
			priority += UnpaidPriority
		}
		fundingCount += 1
		remaining -= min(remaining, g.balanceByteCount)
		if remaining <= 0 {
			break
		}
	}
	if fundingCount == 0 {
		return UnpaidPriority
	}
	return priority / Priority(fundingCount)
}
