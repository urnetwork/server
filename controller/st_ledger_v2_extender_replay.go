package controller

// st_ledger_v2_extender_replay.go — the SN25 extender pool's offline replay
// (sn WHITEPAPER.md §8.6.7): an epoch whose rows a root recorded, such as epoch
// 4, read again as if the amendment covered it, from E_x = the epoch itself.
// The replay checks that its rows under the new rule differ from the recorded
// ones by exactly the pooled shares: for every client, its recorded usage is its
// usage under the rule plus its pooled shares, the same-network shares and any
// registered platform endpoint's; and coldkeys and head flags do not move. It
// reads only, records and retains nothing, and needs the epoch's contracts live,
// inside the 35-day contract horizon.

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// StLedgerV2ExtenderReplayReport is an offline replay's result.
type StLedgerV2ExtenderReplayReport struct {
	Epoch uint64 `json:"epoch"`
	// the recorded rows, and the rows under the amendment
	RecordedRows  int    `json:"recorded_rows"`
	RecordedBytes uint64 `json:"recorded_bytes"`
	AmendedRows   int    `json:"amended_rows"`
	AmendedBytes  uint64 `json:"amended_bytes"`
	// the pooled shares: Z's parts, and those Z leaves out
	IntraPlatformBytes   uint64 `json:"intra_platform_bytes"`
	IntraNetworkBytes    uint64 `json:"intra_network_bytes"`
	ExtenderPoolBytes    uint64 `json:"extender_pool_bytes"`
	ExtenderContracts    uint64 `json:"extender_pool_contracts"`
	ExtenderPayers       uint64 `json:"extender_pool_payers"`
	UnfundedBytes        uint64 `json:"unfunded_bytes"`
	UnfundedContracts    uint64 `json:"unfunded_contracts"`
	MeasurementBytes     uint64 `json:"measurement_bytes"`
	MeasurementContracts uint64 `json:"measurement_contracts"`
	PooledBytes          uint64 `json:"pooled_bytes"`
	// PooledOutsideRows are pooled shares of clients that have no row either
	// way, outside a signed roster's provider universe.
	PooledOutsideRows uint64 `json:"pooled_outside_rows"`
	PooledClients     int    `json:"pooled_clients"`
	PlatformEndpoints int    `json:"platform_endpoints"`
	// Mismatches lists every client whose rows do not differ by exactly its
	// pooled shares, or whose coldkey or head flag moved.
	Mismatches []StLedgerV2ExtenderReplayMismatch `json:"mismatches"`
	// Exact holds when there are no mismatches and the recorded rows exceed the
	// amended ones by exactly the pooled shares inside them.
	Exact bool `json:"exact"`
}

// StLedgerV2ExtenderReplayMismatch is one client the replay cannot reconcile.
type StLedgerV2ExtenderReplayMismatch struct {
	ClientId      string `json:"client_id"`
	RecordedBytes uint64 `json:"recorded_bytes"`
	AmendedBytes  uint64 `json:"amended_bytes"`
	PooledBytes   uint64 `json:"pooled_bytes"`
	Reason        string `json:"reason"`
}

// StLedgerV2ExtenderReplay replays a recorded epoch under the amendment, from
// the epoch itself, with the platform endpoints registered now and the extender
// pool's configured networks. It is refused for an epoch with no recorded rows,
// one declared unrecoverable and one already built with an extender component.
func StLedgerV2ExtenderReplay(ctx context.Context, epoch uint64) (*StLedgerV2ExtenderReplayReport, error) {
	cfg, _, err := stRequire()
	if err != nil {
		return nil, err
	}
	recorded := model.GetStLedgerV2EpochRows(ctx, cfg.DeploymentKey(), cfg.NoId, epoch, epoch)[epoch]
	switch {
	case recorded == nil:
		return nil, fmt.Errorf("st ledger v2: epoch %d has no recorded rows to replay", epoch)
	case recorded.Null():
		return nil, fmt.Errorf("st ledger v2: epoch %d is declared unrecoverable", epoch)
	case recorded.Extenders != nil:
		return nil, fmt.Errorf("st ledger v2: epoch %d was built with an extender component; it has nothing to replay", epoch)
	}
	recordedRows, err := stLedgerV2RecordedRows(ctx, epoch, recorded.ProviderChunks)
	if err != nil {
		return nil, err
	}
	pool, err := stExtenderPoolSettings(cfg)
	if err != nil {
		return nil, err
	}
	platformEndpoints, err := stPlatformEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	amendment := model.StExtenderAmendment{FirstEpoch: epoch, PlatformEndpoints: platformEndpoints}
	rows, read, err := stLedgerV2AmendedEpochRows(ctx, epoch, amendment, pool, false)
	if err != nil {
		return nil, err
	}
	report := stExtenderReplayCompare(epoch, recordedRows, rows, read)
	report.PlatformEndpoints = platformEndpoints.Count()
	return report, nil
}

// stExtenderReplayCompare reconciles an epoch's recorded rows with its rows
// under the amendment and the epoch reader's pool: each client's recorded usage
// must be its amended usage plus its pooled shares, with the same coldkey and
// head flag, and a client with no row either way contributes its pooled shares
// only to PooledOutsideRows.
func stExtenderReplayCompare(epoch uint64, recorded []protocol.LedgerUsageRow, amended []protocol.LedgerUsageRow, pool *model.StEpochExtenderPool) *StLedgerV2ExtenderReplayReport {
	report := &StLedgerV2ExtenderReplayReport{
		Epoch: epoch, RecordedRows: len(recorded), AmendedRows: len(amended),
		IntraPlatformBytes: uint64(pool.IntraPlatform), IntraNetworkBytes: uint64(pool.IntraNetwork), ExtenderPoolBytes: pool.Bytes(),
		ExtenderContracts: pool.Contracts, ExtenderPayers: pool.Payers,
		UnfundedBytes: uint64(pool.UnfundedBytes), UnfundedContracts: pool.UnfundedContracts,
		MeasurementBytes: uint64(pool.MeasurementBytes), MeasurementContracts: pool.MeasurementContracts,
		PooledBytes: pool.PooledBytes(), PooledClients: len(pool.PooledByClient),
		Mismatches: []StLedgerV2ExtenderReplayMismatch{},
	}
	byClient := func(rows []protocol.LedgerUsageRow, total *uint64) map[[16]byte]protocol.LedgerUsageRow {
		out := make(map[[16]byte]protocol.LedgerUsageRow, len(rows))
		for _, row := range rows {
			out[row.ClientID] = row
			*total += row.UsageBytes
		}
		return out
	}
	recordedBy := byClient(recorded, &report.RecordedBytes)
	amendedBy := byClient(amended, &report.AmendedBytes)
	clients := make([][16]byte, 0, len(recordedBy))
	for clientId := range recordedBy {
		clients = append(clients, clientId)
	}
	for clientId := range amendedBy {
		if _, ok := recordedBy[clientId]; !ok {
			clients = append(clients, clientId)
		}
	}
	slices.SortFunc(clients, func(a, b [16]byte) int { return bytes.Compare(a[:], b[:]) })
	var pooledInRows uint64
	for _, clientId := range clients {
		before, after := recordedBy[clientId], amendedBy[clientId]
		pooled := uint64(pool.PooledByClient[server.Id(clientId)])
		pooledInRows += pooled
		mismatch := func(reason string) {
			report.Mismatches = append(report.Mismatches, StLedgerV2ExtenderReplayMismatch{ClientId: server.Id(clientId).String(),
				RecordedBytes: before.UsageBytes, AmendedBytes: after.UsageBytes, PooledBytes: pooled, Reason: reason})
		}
		_, hadRow := recordedBy[clientId]
		_, hasRow := amendedBy[clientId]
		switch {
		case after.UsageBytes > before.UsageBytes || before.UsageBytes-after.UsageBytes != pooled:
			mismatch(fmt.Sprintf("the recorded usage is not the amended usage plus the pooled shares (a change of %d bytes)", int64(before.UsageBytes)-int64(after.UsageBytes)))
		case hadRow && hasRow && (before.Coldkey != after.Coldkey || before.HeadExcluded != after.HeadExcluded):
			mismatch("the coldkey or head flag moved")
		}
	}
	report.PooledOutsideRows = report.PooledBytes - min(report.PooledBytes, pooledInRows)
	report.Exact = len(report.Mismatches) == 0 && pooledInRows <= report.PooledBytes &&
		report.RecordedBytes >= report.AmendedBytes && report.RecordedBytes-report.AmendedBytes == pooledInRows
	return report
}
