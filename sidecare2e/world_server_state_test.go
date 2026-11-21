//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// Every read and write of server state the world makes outside the hosts and api's routes, in one file (the e2e
// conventions; QD-1028's layout move will move sidecar's model code): urnetwork-edge's network row, each block's Quota
// over api's internal quota surface, and the quota ledgers a scenario reconciles, read after the debit journal is
// flushed as taskworker's flush would.

package sidecare2e

import (
	"context"
	"fmt"
	"strings"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/quotahost"
)

// urnetwork-edge's network row, which api's edge client route mints the edges' clients in.
func worldCreateEdgeNetwork(ctx context.Context, networkId server.Id) {
	name := "e2eedge" + strings.ToLower(strings.ReplaceAll(networkId.String(), "-", "")[:12])
	model.Testing_CreateNetwork(ctx, networkId, name, server.NewId())
}

// A block's Quota as its main builds one when it reads the ledgers through api (quotahost.NewBlockQuota): an api-backed
// HostQuota on caller's bearer at api's origin through T3Api, with the process's Redis for its rates and reservations.
// The returned end drains it, sending its open usage window, then ends it: a run calls it once its Run has returned.
func (self *World) blockQuota(caller server.InternalGateCaller) (*quotahost.HostQuota, func(), error) {
	transport, err := quotahost.NewHttpQuotaApi(self.apiOrigin(), self.bearer(caller), self.apiClient(), nil)
	if err != nil {
		return nil, nil, err
	}
	settings := quotahost.DefaultQuotaSettings()
	rateSettings := settings.Rate
	if rateSettings == nil {
		rateSettings = model.DefaultQuotaRateSettings()
	}
	ctx, cancel := context.WithCancel(context.Background())
	quota, err := quotahost.NewApiQuota(ctx, transport, model.NewQuotaRateServerRedis(rateSettings.CallTimeout), nil, settings)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return quota, func() {
		quota.Drain(context.Background())
		cancel()
	}, nil
}

// Applies every service debit the usage batches journaled to the balances, each shard's pages until none is left, as
// taskworker's flush does on its schedule: what a ledger read runs first, so a balance shows every debit applied.
func worldFlushServiceDebits(ctx context.Context) error {
	for shard := range model.ServiceDebitShardCount {
		var after *server.Id
		for {
			result, err := model.FlushServiceDebits(ctx, shard, after, sidecar.RealClock{})
			if err != nil {
				return fmt.Errorf("shard %d's debits: %w", shard, err)
			}
			if 0 < result.Failed {
				return fmt.Errorf("shard %d: %d balances' debits failed to apply", shard, result.Failed)
			}
			if !result.More {
				break
			}
			after = result.LastBalanceId
		}
	}
	return nil
}

// networkId's quota ledger as server holds it: its in-window transfer balances, what each started with and what is
// left, and network_quota_state's stored bytes and spent mark.
func worldNetworkLedger(ctx context.Context, networkId server.Id) (*WorldNetworkLedger, error) {
	ledger := &WorldNetworkLedger{}
	for _, balance := range model.GetActiveTransferBalances(ctx, networkId) {
		ledger.StartBalanceByteCount += int64(balance.StartBalanceByteCount)
		ledger.BalanceByteCount += int64(balance.BalanceByteCount)
	}
	state, err := model.GetNetworkQuotaState(ctx, networkId)
	if err != nil {
		return nil, err
	}
	if state != nil {
		ledger.StoredByteCount = state.StoredByteCount
		ledger.Spent = state.SpentTime != nil
	}
	return ledger, nil
}
