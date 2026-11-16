// The SN25 extender amendment's epoch reader (sn WHITEPAPER.md §8.6.2). From
// the amendment's first epoch E_x, an epoch's provider usage is its contracts'
// hop shares less the ones the amendment pools, and the pooled shares of the
// contracts a balance funded, less the operator's declared measurement and
// test traffic, are the epoch's extender pool bytes Z. Before E_x the epoch is
// read exactly as before (GetStEpochProviderUsageAtEpoch).
package model

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/v2026/payoutartifact"
	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// The extender amendment as the epoch reader applies it (stExtenderAmendment):
// its first epoch E_x, from the signed amendment entry in force or its
// configured fallback, and the operator's registered platform endpoints.
type StExtenderAmendment = stExtenderAmendment

// An epoch's extender pool as the amended epoch reader counts it. Each
// contract's pooled shares go to exactly one of its funded parts, the
// unfunded bytes or the measurement bytes, and never to a provider.
type StEpochExtenderPool struct {
	// Z's parts: the platform endpoints' shares, and the shares on each
	// contract's origin network, of the contracts a balance funded, free or
	// paid, whose payer is not a declared measurement or test network
	IntraPlatform ByteCount
	IntraNetwork  ByteCount
	// the contracts behind Z and their distinct payer networks (M4)
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
	// each client's pooled shares, funded or not, the bytes the amendment
	// takes out of its provider usage: what an offline replay of a recorded
	// epoch checks its rows' change against
	ClientIdPooledBytes map[server.Id]ByteCount
}

// Every pooled share: Z and the bytes it leaves out.
func (self *StEpochExtenderPool) PooledBytes() uint64 {
	return uint64(self.IntraPlatform) + uint64(self.IntraNetwork) + uint64(self.UnfundedBytes) + uint64(self.MeasurementBytes)
}

// Z.
func (self *StEpochExtenderPool) Bytes() uint64 {
	return uint64(self.IntraPlatform) + uint64(self.IntraNetwork)
}

// The amended epoch reader's totals, added to one row at a time in the usage
// statement's order. It is pure: the rows carry everything it reads.
type stEpochAmendedUsage struct {
	epoch     uint64
	amendment stExtenderAmendment
	// the payer networks the operator declares for measurement or testing
	measurementNetworkIds map[server.Id]bool
	clientIdUsages        map[server.Id]*StProviderUsage
	pool                  StEpochExtenderPool
	payerNetworkIds       map[server.Id]bool
}

// A reader of one epoch under the amendment, with nothing counted yet.
func newStEpochAmendedUsage(epoch uint64, amendment stExtenderAmendment, measurementNetworkIds map[server.Id]bool) *stEpochAmendedUsage {
	return &stEpochAmendedUsage{epoch: epoch, amendment: amendment, measurementNetworkIds: measurementNetworkIds, clientIdUsages: map[server.Id]*StProviderUsage{}, payerNetworkIds: map[server.Id]bool{},
		pool: StEpochExtenderPool{ClientIdPooledBytes: map[server.Id]ByteCount{}}}
}

// Adds bytes to a pool total, refusing a sum that would overflow.
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
// contract's payer. A contract with bytes needs its usage origin network,
// which only the live contract row holds. A zero-byte contract moves nothing,
// so it is added exactly as the reader before the amendment adds it.
func (self *stEpochAmendedUsage) add(row *stEpochUsageRow, snapshot *contractUsageSnapshot) error {
	if snapshot.ByteCount == 0 {
		return self.addProviders(snapshot.Providers)
	}
	if !row.live {
		return fmt.Errorf("subnet contract %s is archived without the origin network the extender amendment classifies its shares by; epoch %d must be built within the contract horizon", row.contractId, self.epoch)
	}
	if row.originNetworkId == nil {
		return fmt.Errorf("subnet contract %s has usage shares but no usage origin", row.contractId)
	}
	if row.createTime == nil {
		return fmt.Errorf("subnet contract %s has usage shares but no create time", row.contractId)
	}
	shares, err := stContractSharesForEpoch(snapshot, self.epoch, *row.originNetworkId, *row.createTime, self.amendment)
	if err != nil {
		return fmt.Errorf("subnet contract %s: %w", row.contractId, err)
	}
	if err := self.addProviders(shares.Providers); err != nil {
		return err
	}
	pooled := shares.ExtenderPoolBytes()
	if pooled == 0 {
		return nil
	}
	for _, share := range shares.Pooled {
		total := self.pool.ClientIdPooledBytes[share.ClientId]
		if err := addStEpochPoolBytes(&total, share.ByteCount); err != nil {
			return err
		}
		self.pool.ClientIdPooledBytes[share.ClientId] = total
	}
	switch {
	case row.payerNetworkId == nil:
		self.pool.UnfundedContracts++
		return addStEpochPoolBytes(&self.pool.UnfundedBytes, pooled)
	case self.measurementNetworkIds[*row.payerNetworkId]:
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
	self.payerNetworkIds[*row.payerNetworkId] = true
	return nil
}

// Adds provider shares to their providers' usage, as the reader before the
// amendment adds a snapshot's shares.
func (self *stEpochAmendedUsage) addProviders(providers []contractProviderUsage) error {
	for _, provider := range providers {
		usage := self.clientIdUsages[provider.ClientId]
		if usage == nil {
			usage = &StProviderUsage{ClientId: provider.ClientId, NetworkId: provider.NetworkId}
			self.clientIdUsages[provider.ClientId] = usage
		}
		if usage.NetworkId != provider.NetworkId || int64(provider.ByteCount) > math.MaxInt64-usage.PayoutByteCount {
			return fmt.Errorf("subnet provider usage has ambiguous network or overflowing total")
		}
		usage.PayoutByteCount += int64(provider.ByteCount)
	}
	return nil
}

// The epoch's provider usages, one per client in client id order, and its
// pool.
func (self *stEpochAmendedUsage) result() ([]*StProviderUsage, *StEpochExtenderPool) {
	usages := make([]*StProviderUsage, 0, len(self.clientIdUsages))
	for _, usage := range self.clientIdUsages {
		usages = append(usages, usage)
	}
	slices.SortFunc(usages, func(a, b *StProviderUsage) int { return a.ClientId.Cmp(b.ClientId) })
	pool := self.pool
	pool.Payers = uint64(len(self.payerNetworkIds))
	pool.ClientIdPooledBytes = maps.Clone(self.pool.ClientIdPooledBytes)
	return usages, &pool
}

// The usage statement (stEpochProviderUsageSql) with each live contract's
// usage origin network, payer and create time. The origin is the endpoint the
// usage snapshot left out of the contract's hops, so its network is the one the
// amendment calls the origin's; it is NULL for a legacy direction, which
// writes no snapshot with shares. The create time fixes which platform
// endpoints were registered for the contract. The rows, their order and their
// snapshot are exactly the usage statement's: the archived and undated arms
// carry no origin, and only a live row is marked live.
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
		false AS live, NULL::uuid AS origin_network_id, NULL::uuid AS payer_network_id, NULL::timestamp AS create_time
	FROM missing_time
	UNION ALL
	SELECT source.contract_id, source.provider_usage, source.close_time,
		archive.contract_id IS NOT NULL AS duplicate,
		true AS live,
		CASE WHEN source.usage_origin_is_source THEN source.source_network_id
			WHEN NOT source.usage_origin_is_source THEN source.destination_network_id END,
		source.payer_network_id,
		source.create_time
	FROM transfer_contract AS source
	LEFT JOIN st_provider_usage_archive AS archive ON archive.contract_id=source.contract_id
		AND $1 <= archive.close_time AND archive.close_time < $2
	WHERE NOT EXISTS (SELECT 1 FROM missing_time)
		AND $1 <= source.close_time AND source.close_time < $2
		AND source.outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
	UNION ALL
	SELECT contract_id, provider_usage, close_time, false,
		false, NULL::uuid, NULL::uuid, NULL::timestamp
	FROM st_provider_usage_archive
	WHERE NOT EXISTS (SELECT 1 FROM missing_time)
		AND $1 <= close_time AND close_time < $2
		AND outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
`

// Scans one row of the amended usage read into row.
func scanStEpochAmendedUsageRow(rows pgx.Rows, row *stEpochUsageRow) error {
	return rows.Scan(&row.contractId, &row.data, &row.closedAt, &row.duplicate, &row.live, &row.originNetworkId, &row.payerNetworkId, &row.createTime)
}

// Adds the totals of other, a reader of the same epoch and amendment over rows
// this one has not read, as adding those rows here would: the same sums, and
// the same refusal of a client with two networks or a total that overflows.
func (self *stEpochAmendedUsage) merge(other *stEpochAmendedUsage) error {
	for clientId, usage := range other.clientIdUsages {
		total := self.clientIdUsages[clientId]
		if total == nil {
			copied := *usage
			self.clientIdUsages[clientId] = &copied
			continue
		}
		if total.NetworkId != usage.NetworkId || usage.PayoutByteCount > math.MaxInt64-total.PayoutByteCount {
			return fmt.Errorf("subnet provider usage has ambiguous network or overflowing total")
		}
		total.PayoutByteCount += usage.PayoutByteCount
	}
	for clientId, bytes := range other.pool.ClientIdPooledBytes {
		total := self.pool.ClientIdPooledBytes[clientId]
		if err := addStEpochPoolBytes(&total, bytes); err != nil {
			return err
		}
		self.pool.ClientIdPooledBytes[clientId] = total
	}
	for _, part := range []struct{ total, bytes *ByteCount }{
		{&self.pool.IntraPlatform, &other.pool.IntraPlatform}, {&self.pool.IntraNetwork, &other.pool.IntraNetwork},
		{&self.pool.UnfundedBytes, &other.pool.UnfundedBytes}, {&self.pool.MeasurementBytes, &other.pool.MeasurementBytes},
	} {
		if err := addStEpochPoolBytes(part.total, *part.bytes); err != nil {
			return err
		}
	}
	if self.pool.IntraPlatform > math.MaxInt64-self.pool.IntraNetwork {
		return fmt.Errorf("subnet extender pool bytes overflow")
	}
	self.pool.Contracts += other.pool.Contracts
	self.pool.UnfundedContracts += other.pool.UnfundedContracts
	self.pool.MeasurementContracts += other.pool.MeasurementContracts
	for networkId := range other.payerNetworkIds {
		self.payerNetworkIds[networkId] = true
	}
	return nil
}

// A row the amended read refuses for what it holds, which reading it again
// cannot change. Its text is the refusal's.
type stEpochUsageRefusal struct {
	err error
}

func (self *stEpochUsageRefusal) Error() string { return self.err.Error() }

func (self *stEpochUsageRefusal) Unwrap() error { return self.err }

// Reads the amended usage statement's rows that closed in [windowStart,
// windowEnd) into reader, in one REPEATABLE READ, read-only snapshot of one
// maintenance session, planned as the usage statement is: a window past the
// census record bound in one block-ordered pass, checked on workers and added
// in statement order. exempt lifts the session's transaction_timeout for a
// read that may run past it; without it the role's bound ends any snapshot that
// would. It returns the rows read, and a refusal of a row as an
// stEpochUsageRefusal.
func readStEpochAmendedUsageWindow(
	ctx context.Context,
	reader *stEpochAmendedUsage,
	windowStart time.Time,
	windowEnd time.Time,
	exempt bool,
) (int, error) {
	usageRows := 0
	var returnErr error
	check := func(row *stEpochUsageRow) (*contractUsageSnapshot, error) {
		snapshot, err := checkStEpochUsageRow(row, reader.epoch)
		if err != nil {
			return nil, &stEpochUsageRefusal{err: err}
		}
		return snapshot, nil
	}
	add := func(row *stEpochUsageRow, snapshot *contractUsageSnapshot, _ []byte) error {
		if row.closedAt.Before(windowStart) || !row.closedAt.Before(windowEnd) {
			return nil
		}
		if err := reader.add(row, snapshot); err != nil {
			return &stEpochUsageRefusal{err: err}
		}
		return nil
	}
	read := func(tx server.PgTx) {
		rows, err := tx.Query(ctx, stEpochAmendedUsageSql, windowStart, windowEnd)
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
		rows, err := tx.Query(queryCtx, stEpochAmendedUsageSql, windowStart, windowEnd)
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
	readInTx := func(conn server.PgConn) {
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			returnErr = fmt.Errorf("read epoch provider usage: %w", err)
			return
		}
		// read only, so a rollback ends it
		defer tx.Rollback(context.WithoutCancel(ctx))
		var bounded int
		if err := tx.QueryRow(ctx, stEpochProviderUsageBoundSql, windowStart, windowEnd, payoutartifact.MaxClosedWorkRecords+1).Scan(&bounded); err != nil {
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
	}
	// The same session, snapshot and plans as getStEpochProviderUsageWithCensus
	// without a census.
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		if !exempt {
			readInTx(conn)
			return
		}
		server.WithoutTransactionTimeout(ctx, conn, func() {
			readInTx(conn)
		})
	})
	return usageRows, returnErr
}

// Logs an amended read's totals, and how many snapshots it read them in when
// more than one.
func logStEpochAmendedUsage(epoch uint64, usageRows int, snapshots int, usages []*StProviderUsage, pool *StEpochExtenderPool, elapsed time.Duration) {
	read := fmt.Sprintf("%d closed contracts", usageRows)
	if 1 < snapshots {
		read = fmt.Sprintf("%d closed contracts in %d snapshots", usageRows, snapshots)
	}
	glog.Infof(
		"[st]epoch %d amended usage read %s for %d providers in %s: Z = %d bytes (%d intra-platform, %d intra-network) from %d contracts of %d payers; left out %d unfunded bytes in %d contracts and %d measurement bytes in %d contracts\n",
		epoch, read, len(usages), elapsed.Round(time.Millisecond), pool.Bytes(), pool.IntraPlatform, pool.IntraNetwork,
		pool.Contracts, pool.Payers, pool.UnfundedBytes, pool.UnfundedContracts, pool.MeasurementBytes, pool.MeasurementContracts,
	)
}

// Reads an epoch's provider usage under the extender amendment, and its
// extender pool. An epoch the amendment does not cover is read exactly as
// GetStEpochProviderUsageAtEpoch reads it, with no pool. One it covers reads
// the same usage statement in one REPEATABLE READ snapshot, planned the same
// way, with each contract's usage origin network and payer: its provider shares
// are summed per client, its pooled shares counted by payer, the payers in
// measurement left out of Z.
func GetStEpochAmendedUsageAtEpoch(
	ctx context.Context,
	epoch uint64,
	startTime time.Time,
	endTime time.Time,
	amendment StExtenderAmendment,
	measurementNetworkIds map[server.Id]bool,
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
	reader := newStEpochAmendedUsage(epoch, amendment, measurementNetworkIds)
	earningStart, earningEnd := transition.SnWindow(startTime, endTime)
	if !earningStart.Before(earningEnd) {
		usages, pool := reader.result()
		return usages, pool, nil
	}
	readStart := time.Now()
	usageRows, err := readStEpochAmendedUsageWindow(ctx, reader, earningStart, earningEnd, true)
	if err != nil {
		return nil, nil, err
	}
	usages, pool := reader.result()
	logStEpochAmendedUsage(epoch, usageRows, 1, usages, pool, time.Since(readStart))
	return usages, pool, nil
}

// How a sliced epoch read is cut and retried (GetStEpochAmendedUsageInSlices).
// Its owner's settings supply it.
type StEpochUsageSlices struct {
	// Close-time span of one slice, read in its own snapshot. Non-positive
	// reads the whole window in one snapshot.
	Span time.Duration
	// Reads of one slice that may fail in a row, each from nothing, before
	// the read fails. Below 1 reads each slice once.
	Attempts int
	// Wait before a failed slice is read again.
	RetryDelay time.Duration
}

// Called before each attempt of a slice of a sliced read is read, and again,
// with read set, once it is read and before it counts: an error then fails that
// attempt. A seam the tests set; nil otherwise.
var stEpochUsageSliceHook func(sliceStart time.Time, attempt int, read bool) error

// Sets the sliced read's seam for another package's test, and returns what
// restores it.
func Testing_SetStEpochUsageSliceHook(hook func(sliceStart time.Time, attempt int, read bool) error) (restore func()) {
	previous := stEpochUsageSliceHook
	stEpochUsageSliceHook = hook
	return func() { stEpochUsageSliceHook = previous }
}

// Reads an epoch's provider usage and extender pool as
// GetStEpochAmendedUsageAtEpoch does, to the same totals, in close-time slices
// of slicing.Span, each its own short REPEATABLE READ snapshot under the role's
// transaction_timeout, so no snapshot pins the primary for the whole epoch.
// Every row belongs to exactly one slice: its terminal close time never
// changes, and each slice reads its live and archived rows in one snapshot,
// refusing one that is both, as the whole read does. The totals are sums, so
// the order slices are read in does not change them. A slice counts only once
// it is read whole: one that fails is read again from nothing, up to
// slicing.Attempts times, so a failed slice neither restarts the read nor counts
// twice. A refused row fails the read at once. An epoch the amendment does not
// cover, or a non-positive span, reads as GetStEpochAmendedUsageAtEpoch does.
func GetStEpochAmendedUsageInSlices(
	ctx context.Context,
	epoch uint64,
	startTime time.Time,
	endTime time.Time,
	amendment StExtenderAmendment,
	measurementNetworkIds map[server.Id]bool,
	slicing StEpochUsageSlices,
) ([]*StProviderUsage, *StEpochExtenderPool, error) {
	if !amendment.InForce(epoch) || slicing.Span <= 0 {
		return GetStEpochAmendedUsageAtEpoch(ctx, epoch, startTime, endTime, amendment, measurementNetworkIds)
	}
	if !startTime.Before(endTime) {
		return nil, nil, fmt.Errorf("invalid subnet usage window")
	}
	transition, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return nil, nil, err
	}
	reader := newStEpochAmendedUsage(epoch, amendment, measurementNetworkIds)
	earningStart, earningEnd := transition.SnWindow(startTime, endTime)
	readStart := time.Now()
	usageRows, sliceCount := 0, 0
	for sliceStart := earningStart; sliceStart.Before(earningEnd); sliceCount++ {
		sliceEnd := sliceStart.Add(slicing.Span)
		if earningEnd.Before(sliceEnd) {
			sliceEnd = earningEnd
		}
		for attempt := 1; ; attempt++ {
			// a slice's own reader, merged only once the slice is read whole
			sliceReader := newStEpochAmendedUsage(epoch, amendment, measurementNetworkIds)
			rows, err := func() (int, error) {
				if hook := stEpochUsageSliceHook; hook != nil {
					if err := hook(sliceStart, attempt, false); err != nil {
						return 0, err
					}
				}
				rows, err := readStEpochAmendedUsageWindow(ctx, sliceReader, sliceStart, sliceEnd, false)
				if err != nil {
					return 0, err
				}
				if hook := stEpochUsageSliceHook; hook != nil {
					if err := hook(sliceStart, attempt, true); err != nil {
						return 0, err
					}
				}
				return rows, nil
			}()
			if err == nil {
				if err := reader.merge(sliceReader); err != nil {
					return nil, nil, err
				}
				usageRows += rows
				break
			}
			var refusal *stEpochUsageRefusal
			if errors.As(err, &refusal) || ctx.Err() != nil || max(1, slicing.Attempts) <= attempt {
				return nil, nil, fmt.Errorf("epoch %d usage slice from %s: %w", epoch, sliceStart.UTC().Format(time.RFC3339), err)
			}
			glog.Infof("[st]epoch %d amended usage slice from %s failed on attempt %d, read again: %s\n", epoch, sliceStart.UTC().Format(time.RFC3339), attempt, err)
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(slicing.RetryDelay):
			}
		}
		sliceStart = sliceEnd
	}
	usages, pool := reader.result()
	logStEpochAmendedUsage(epoch, usageRows, sliceCount, usages, pool, time.Since(readStart))
	return usages, pool, nil
}
