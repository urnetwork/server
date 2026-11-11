// The SN25 extender amendment's epoch reader (sn WHITEPAPER.md §8.6.2). From
// the amendment's first epoch E_x, an epoch's provider usage is its contracts'
// hop shares less the ones the amendment pools, and the pooled shares of the
// contracts a balance funded, less the operator's declared measurement and
// test traffic, are the epoch's extender pool bytes Z. Before E_x the epoch is
// read exactly as before (GetStEpochProviderUsageAtEpoch).
package model

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// StExtenderAmendment is the extender amendment as the epoch reader applies
// it (stExtenderAmendment): its first epoch E_x, from the signed amendment
// entry in force or its configured fallback, and the operator's registered
// platform endpoints.
type StExtenderAmendment = stExtenderAmendment

// StEpochExtenderPool is an epoch's extender pool as the amended epoch reader
// counts it. Each contract's pooled shares go to exactly one of its funded
// parts, the unfunded bytes or the measurement bytes, and never to a provider.
type StEpochExtenderPool struct {
	// IntraPlatform and IntraNetwork are Z's parts: the platform endpoints'
	// shares, and the shares on each contract's origin network, of the
	// contracts a balance funded, free or paid, whose payer is not a declared
	// measurement or test network.
	IntraPlatform ByteCount
	IntraNetwork  ByteCount
	// Contracts and Payers count the contracts behind Z and their distinct
	// payer networks (M4).
	Contracts uint64
	Payers    uint64
	// The pooled shares Z leaves out, which enter neither the provider rows
	// nor Z: contracts no balance funded (the degradation valve's zero escrow
	// fallback, and contracts opened before network contracts escrowed), and
	// contracts paid by a declared measurement or test network.
	UnfundedBytes        ByteCount
	UnfundedContracts    uint64
	MeasurementBytes     ByteCount
	MeasurementContracts uint64
}

// Bytes is Z.
func (self *StEpochExtenderPool) Bytes() uint64 {
	return uint64(self.IntraPlatform) + uint64(self.IntraNetwork)
}

// The amended epoch reader's totals, added to one row at a time in the usage
// statement's order. It is pure: the rows carry everything it reads.
type stEpochAmendedUsage struct {
	epoch     uint64
	amendment stExtenderAmendment
	// the payer networks the operator declares for measurement or testing
	measurement map[server.Id]bool
	usages      map[server.Id]*StProviderUsage
	pool        StEpochExtenderPool
	payers      map[server.Id]bool
}

func newStEpochAmendedUsage(epoch uint64, amendment stExtenderAmendment, measurement map[server.Id]bool) *stEpochAmendedUsage {
	return &stEpochAmendedUsage{epoch: epoch, amendment: amendment, measurement: measurement, usages: map[server.Id]*StProviderUsage{}, payers: map[server.Id]bool{}}
}

func addStEpochPoolBytes(total *ByteCount, bytes ByteCount) error {
	if bytes < 0 || bytes > math.MaxInt64-*total {
		return fmt.Errorf("subnet extender pool bytes overflow")
	}
	*total += bytes
	return nil
}

// Adds one contract of the epoch's earning window. A contract's shares are
// read once (stContractSharesForEpoch): each provider share is added to its
// provider's usage, and the pooled shares to one part of the pool by the
// contract's payer. A contract without shares adds nothing. One with shares
// needs its usage origin network, which only the live contract row holds.
func (self *stEpochAmendedUsage) add(row *stEpochUsageRow, snapshot *contractUsageSnapshot) error {
	if len(snapshot.Providers) == 0 {
		return nil
	}
	if !row.live {
		return fmt.Errorf("subnet contract %s is archived without the origin network the extender amendment classifies its shares by; epoch %d must be built within the contract horizon", row.contractId, self.epoch)
	}
	if row.originNetworkId == nil {
		return fmt.Errorf("subnet contract %s has usage shares but no usage origin", row.contractId)
	}
	shares, err := stContractSharesForEpoch(snapshot, self.epoch, *row.originNetworkId, self.amendment)
	if err != nil {
		return fmt.Errorf("subnet contract %s: %w", row.contractId, err)
	}
	for _, provider := range shares.Providers {
		usage := self.usages[provider.ClientId]
		if usage == nil {
			usage = &StProviderUsage{ClientId: provider.ClientId, NetworkId: provider.NetworkId}
			self.usages[provider.ClientId] = usage
		}
		if usage.NetworkId != provider.NetworkId || int64(provider.ByteCount) > math.MaxInt64-usage.PayoutByteCount {
			return fmt.Errorf("subnet provider usage has ambiguous network or overflowing total")
		}
		usage.PayoutByteCount += int64(provider.ByteCount)
	}
	pooled := shares.ExtenderPoolBytes()
	if pooled == 0 {
		return nil
	}
	switch {
	case row.payerNetworkId == nil:
		self.pool.UnfundedContracts++
		return addStEpochPoolBytes(&self.pool.UnfundedBytes, pooled)
	case self.measurement[*row.payerNetworkId]:
		self.pool.MeasurementContracts++
		return addStEpochPoolBytes(&self.pool.MeasurementBytes, pooled)
	}
	if err := addStEpochPoolBytes(&self.pool.IntraPlatform, shares.IntraPlatform); err != nil {
		return err
	}
	if err := addStEpochPoolBytes(&self.pool.IntraNetwork, shares.IntraNetwork); err != nil {
		return err
	}
	if self.pool.IntraPlatform > math.MaxInt64-self.pool.IntraNetwork {
		return fmt.Errorf("subnet extender pool bytes overflow")
	}
	self.pool.Contracts++
	self.payers[*row.payerNetworkId] = true
	return nil
}

// The epoch's provider usages, one per client in client id order, and its
// pool.
func (self *stEpochAmendedUsage) result() ([]*StProviderUsage, *StEpochExtenderPool) {
	usages := make([]*StProviderUsage, 0, len(self.usages))
	for _, usage := range self.usages {
		usages = append(usages, usage)
	}
	slices.SortFunc(usages, func(a, b *StProviderUsage) int { return a.ClientId.Cmp(b.ClientId) })
	pool := self.pool
	pool.Payers = uint64(len(self.payers))
	return usages, &pool
}

// The usage statement (stEpochProviderUsageSql) with each live contract's
// usage origin network and payer. The origin is the endpoint the usage
// snapshot left out of the contract's hops, so its network is the one the
// amendment calls the origin's; it is NULL for a legacy direction, which
// writes no snapshot with shares. The rows, their order and their snapshot
// are exactly the usage statement's: the archived and undated arms carry no
// origin, and only a live row is marked live.
const stEpochAmendedUsageSql = `
	WITH missing_time AS MATERIALIZED (
		(SELECT contract_id, NULL::jsonb AS provider_usage, close_time, false AS duplicate
		 FROM transfer_contract WHERE close_time IS NULL
			AND outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
		 ORDER BY contract_id LIMIT 1)
		UNION ALL
		(SELECT contract_id, NULL::jsonb AS provider_usage, close_time, false AS duplicate
		 FROM st_provider_usage_archive WHERE close_time IS NULL
		 ORDER BY close_time, contract_id LIMIT 1)
	)
	SELECT contract_id, provider_usage, close_time, duplicate,
		false AS live, NULL::uuid AS origin_network_id, NULL::uuid AS payer_network_id
	FROM missing_time
	UNION ALL
	SELECT source.contract_id, source.provider_usage, source.close_time,
		archive.contract_id IS NOT NULL AS duplicate,
		true AS live,
		CASE WHEN source.usage_origin_is_source THEN source.source_network_id
			WHEN NOT source.usage_origin_is_source THEN source.destination_network_id END,
		source.payer_network_id
	FROM transfer_contract AS source
	LEFT JOIN st_provider_usage_archive AS archive ON archive.contract_id=source.contract_id
		AND $1 <= archive.close_time AND archive.close_time < $2
	WHERE NOT EXISTS (SELECT 1 FROM missing_time)
		AND $1 <= source.close_time AND source.close_time < $2
		AND source.outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
	UNION ALL
	SELECT contract_id, provider_usage, close_time, false,
		false, NULL::uuid, NULL::uuid
	FROM st_provider_usage_archive
	WHERE NOT EXISTS (SELECT 1 FROM missing_time)
		AND $1 <= close_time AND close_time < $2
		AND outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
`

func scanStEpochAmendedUsageRow(rows pgx.Rows, row *stEpochUsageRow) error {
	return rows.Scan(&row.contractId, &row.data, &row.closedAt, &row.duplicate, &row.live, &row.originNetworkId, &row.payerNetworkId)
}

// GetStEpochAmendedUsageAtEpoch reads an epoch's provider usage under the
// extender amendment, and its extender pool. An epoch the amendment does not
// cover is read exactly as GetStEpochProviderUsageAtEpoch reads it, with no
// pool. One it covers reads the same usage statement in one REPEATABLE READ
// snapshot, planned the same way, with each contract's usage origin network
// and payer: its provider shares are summed per client, its pooled shares
// counted by payer, the payers in measurement left out of Z.
func GetStEpochAmendedUsageAtEpoch(
	ctx context.Context,
	epoch uint64,
	startTime time.Time,
	endTime time.Time,
	amendment StExtenderAmendment,
	measurement map[server.Id]bool,
) ([]*StProviderUsage, *StEpochExtenderPool, error) {
	if !amendment.InForce(epoch) {
		usages, err := GetStEpochProviderUsageAtEpoch(ctx, epoch, startTime, endTime)
		return usages, nil, err
	}
	if !startTime.Before(endTime) {
		return nil, nil, fmt.Errorf("invalid subnet usage window")
	}
	transition, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return nil, nil, err
	}
	reader := newStEpochAmendedUsage(epoch, amendment, measurement)
	earningStart, earningEnd := transition.SnWindow(startTime, endTime)
	if !earningStart.Before(earningEnd) {
		usages, pool := reader.result()
		return usages, pool, nil
	}
	usageRows := 0
	var returnErr error
	check := func(row *stEpochUsageRow) (*contractUsageSnapshot, error) {
		return checkStEpochUsageRow(row, epoch)
	}
	add := func(row *stEpochUsageRow, snapshot *contractUsageSnapshot, _ []byte) error {
		if row.closedAt.Before(earningStart) || !row.closedAt.Before(earningEnd) {
			return nil
		}
		return reader.add(row, snapshot)
	}
	read := func(tx server.PgTx) {
		rows, err := tx.Query(ctx, stEpochAmendedUsageSql, earningStart, earningEnd)
		if err != nil {
			returnErr = fmt.Errorf("read epoch provider usage: %w", err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var row stEpochUsageRow
			if err := scanStEpochAmendedUsageRow(rows, &row); err != nil {
				returnErr = err
				return
			}
			usageRows++
			snapshot, err := check(&row)
			if err != nil {
				returnErr = err
				return
			}
			if err := add(&row, snapshot, nil); err != nil {
				returnErr = err
				return
			}
		}
		returnErr = rows.Err()
	}
	readWhole := func(tx server.PgTx) {
		queryCtx, cancelQuery := context.WithCancel(ctx)
		defer cancelQuery()
		rows, err := tx.Query(queryCtx, stEpochAmendedUsageSql, earningStart, earningEnd)
		if err != nil {
			returnErr = fmt.Errorf("read epoch provider usage: %w", err)
			return
		}
		defer rows.Close()
		next := func(row *stEpochUsageRow) (bool, error) {
			if !rows.Next() {
				return false, rows.Err()
			}
			if err := scanStEpochAmendedUsageRow(rows, row); err != nil {
				return false, err
			}
			usageRows++
			return true, nil
		}
		returnErr = addStEpochUsageRowsInOrder(stEpochUsageCheckWorkers, cancelQuery, next, check, add)
	}
	readStart := time.Now()
	// The same session, snapshot and plans as getStEpochProviderUsageWithCensus
	// without a census.
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.WithoutTransactionTimeout(ctx, conn, func() {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if err != nil {
				returnErr = fmt.Errorf("read epoch provider usage: %w", err)
				return
			}
			// read only, so a rollback ends it
			defer tx.Rollback(context.WithoutCancel(ctx))
			var bounded int
			if err := tx.QueryRow(ctx, stEpochProviderUsageBoundSql, earningStart, earningEnd, payoutartifact.MaxClosedWorkRecords+1).Scan(&bounded); err != nil {
				returnErr = fmt.Errorf("read epoch provider usage: %w", err)
				return
			}
			if bounded <= payoutartifact.MaxClosedWorkRecords {
				read(tx)
				return
			}
			if err := stEpochProviderUsageWholeWindowPlanInTx(ctx, tx); err != nil {
				returnErr = fmt.Errorf("read epoch provider usage: %w", err)
				return
			}
			readWhole(tx)
			if returnErr == nil && usageRows <= payoutartifact.MaxClosedWorkRecords {
				// one snapshot cannot return fewer rows than it counted
				returnErr = fmt.Errorf("read epoch provider usage: %d rows read after %d were counted", usageRows, bounded)
			}
		})
	})
	if returnErr != nil {
		return nil, nil, returnErr
	}
	usages, pool := reader.result()
	glog.Infof(
		"[st]epoch %d amended usage read %d closed contracts for %d providers in %s: Z = %d bytes (%d intra-platform, %d intra-network) from %d contracts of %d payers; left out %d unfunded bytes in %d contracts and %d measurement bytes in %d contracts\n",
		epoch, usageRows, len(usages), time.Since(readStart).Round(time.Millisecond), pool.Bytes(), pool.IntraPlatform, pool.IntraNetwork,
		pool.Contracts, pool.Payers, pool.UnfundedBytes, pool.UnfundedContracts, pool.MeasurementBytes, pool.MeasurementContracts,
	)
	return usages, pool, nil
}
