// A same-network companion normalized to a network contract is funded by its
// source but serves its destination. Every contract insert, the PostgreSQL
// escrow, the Redis escrow and the zero escrow fallback, keeps that explicit
// usage origin for a plain contract between two clients of one network, and
// ignores it for a companion or a cross-network contract.
package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestContractUsageOriginInsertArgOnlyForSameNetworkPlainContracts(t *testing.T) {
	networkId, otherNetworkId := server.NewId(), server.NewId()
	companionId := server.NewId()
	for _, test := range []struct {
		name                 string
		ctx                  context.Context
		destinationNetworkId server.Id
		companionContractId  *server.Id
		want                 *bool
	}{
		{name: "no explicit origin", ctx: context.Background(), destinationNetworkId: networkId},
		{name: "same network, destination origin", ctx: WithContractUsageOrigin(context.Background(), false), destinationNetworkId: networkId, want: new(bool)},
		{name: "same network, source origin", ctx: WithContractUsageOrigin(context.Background(), true), destinationNetworkId: networkId, want: func() *bool { v := true; return &v }()},
		{name: "cross network", ctx: WithContractUsageOrigin(context.Background(), false), destinationNetworkId: otherNetworkId},
		{name: "companion", ctx: WithContractUsageOrigin(context.Background(), true), destinationNetworkId: networkId, companionContractId: &companionId},
	} {
		got := contractUsageOriginInsertArg(test.ctx, networkId, test.destinationNetworkId, test.companionContractId)
		if (got == nil) != (test.want == nil) || got != nil && *got != *test.want {
			t.Errorf("%s: usage origin %v, want %v", test.name, got, test.want)
		}
	}
}

func TestZeroEscrowPlainContractKind(t *testing.T) {
	networkId := server.NewId()
	if kind := zeroEscrowPlainContractKind(networkId, networkId); kind != "network" {
		t.Fatalf("same-network zero escrow contract kind %q, want network", kind)
	}
	if kind := zeroEscrowPlainContractKind(networkId, server.NewId()); kind != "public" {
		t.Fatalf("cross-network zero escrow contract kind %q, want public", kind)
	}
}

func TestContractInsertsKeepExplicitSameNetworkUsageOrigin(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, otherNetworkId := server.NewId(), server.NewId()
		consumerId, providerId, otherProviderId := server.NewId(), server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("network-escrow-origin-%s", networkId), server.NewId())
		Testing_CreateNetwork(ctx, otherNetworkId, fmt.Sprintf("network-escrow-origin-%s", otherNetworkId), server.NewId())
		Testing_CreateDevice(ctx, networkId, server.NewId(), consumerId, "", "")
		Testing_CreateDevice(ctx, networkId, server.NewId(), providerId, "", "")
		Testing_CreateDevice(ctx, otherNetworkId, server.NewId(), otherProviderId, "", "")
		for _, payerNetworkId := range []server.Id{networkId, otherNetworkId} {
			now := server.NowUtc()
			AddTransferBalance(ctx, &TransferBalance{
				NetworkId:             payerNetworkId,
				StartTime:             now.Add(-time.Minute),
				EndTime:               now.Add(24 * time.Hour),
				StartBalanceByteCount: 64 * Mib,
				BalanceByteCount:      64 * Mib,
				NetRevenue:            UsdToNanoCents(1),
			})
		}
		usageOrigin := func(contractId server.Id) *bool {
			var usageOriginIsSource *bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT usage_origin_is_source FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(&usageOriginIsSource))
			})
			return usageOriginIsSource
		}
		replyCtx := WithContractUsageOrigin(ctx, false)
		for _, test := range []struct {
			name                 string
			ctx                  context.Context
			create               func(ctx context.Context, sourceNetworkId, sourceId, destinationNetworkId, destinationId server.Id, byteCount ByteCount) (*TransferEscrow, error)
			sourceNetworkId      server.Id
			sourceId             server.Id
			byteCount            ByteCount
			wantUsageOriginIsSrc bool
		}{
			{name: "postgres escrow, same network reply", ctx: replyCtx, create: CreateTransferEscrow, sourceNetworkId: networkId, sourceId: providerId, byteCount: 0},
			{name: "redis escrow, same network reply", ctx: replyCtx, create: CreateTransferEscrow, sourceNetworkId: networkId, sourceId: providerId, byteCount: Mib},
			{name: "zero escrow, same network reply", ctx: replyCtx, create: CreateZeroEscrowContract, sourceNetworkId: networkId, sourceId: providerId, byteCount: Mib},
			{name: "redis escrow, same network forward", ctx: ctx, create: CreateTransferEscrow, sourceNetworkId: networkId, sourceId: providerId, byteCount: Mib, wantUsageOriginIsSrc: true},
			{name: "redis escrow, cross network ignores the origin", ctx: replyCtx, create: CreateTransferEscrow, sourceNetworkId: otherNetworkId, sourceId: otherProviderId, byteCount: Mib, wantUsageOriginIsSrc: true},
			{name: "zero escrow, cross network ignores the origin", ctx: replyCtx, create: CreateZeroEscrowContract, sourceNetworkId: otherNetworkId, sourceId: otherProviderId, byteCount: Mib, wantUsageOriginIsSrc: true},
		} {
			escrow, err := test.create(test.ctx, test.sourceNetworkId, test.sourceId, networkId, consumerId, test.byteCount)
			if err != nil {
				t.Fatalf("%s: %v", test.name, err)
			}
			if got := usageOrigin(escrow.ContractId); got == nil || *got != test.wantUsageOriginIsSrc {
				t.Fatalf("%s: usage origin is source=%v, want %t", test.name, got, test.wantUsageOriginIsSrc)
			}
		}
	})
}
