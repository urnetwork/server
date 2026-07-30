package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func TestComputePassiveProviderBandwidthDerivesFromSettledBytes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destNetworkId := server.NewId()
		destId := server.NewId()
		Testing_CreateDevice(ctx, sourceNetworkId, server.NewId(), sourceId, "", "")
		Testing_CreateDevice(ctx, destNetworkId, server.NewId(), destId, "", "")

		// a contract that settled 32 MiB over exactly 10 seconds of wall time
		windowStart := server.NowUtc().Add(-1 * time.Hour)
		contractId := Testing_CreateSettledContract(ctx, sourceId, destId,
			windowStart, windowStart.Add(10*time.Second), 32*1024*1024)

		bw, err := ComputePassiveProviderBandwidth(ctx, destId, 2*time.Hour)
		connect.AssertEqual(t, err, nil)
		if bw == nil {
			t.Fatal("expected a passive bandwidth result, got nil")
		}
		connect.AssertEqual(t, bw.Source, "passive")
		// 32 MiB / 10s ~= 3355443 bytes/sec
		if bw.BytesPerSecond < 3_000_000 || 3_700_000 < bw.BytesPerSecond {
			t.Errorf("BytesPerSecond = %.0f, want ~3355443", bw.BytesPerSecond)
		}
		_ = contractId
	})
}

func TestComputePassiveProviderBandwidthNilWhenNoHistory(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		bw, err := ComputePassiveProviderBandwidth(ctx, server.NewId(), 2*time.Hour)
		connect.AssertEqual(t, err, nil)
		if bw != nil {
			t.Errorf("expected nil for a provider with no settled bytes, got %+v", bw)
		}
	})
}

// TestComputePassiveProviderBandwidthExcludesCompanionContracts is the load
// bearing case: a client's return traffic settles as a companion contract where
// the CLIENT is the destination. Counting it would read an ordinary user as a
// very fast provider. Only the companion leg exists here, so the correct answer
// is nil (no provider history at all) -- a merely smaller number would prove
// only dilution, not exclusion.
func TestComputePassiveProviderBandwidthExcludesCompanionContracts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		providerNetworkId := server.NewId()
		providerId := server.NewId()
		clientNetworkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, providerNetworkId, server.NewId(), providerId, "", "")
		Testing_CreateDevice(ctx, clientNetworkId, server.NewId(), clientId, "", "")

		// the client's own contract, which the companion leg pairs with
		createTime := server.NowUtc().Add(-1 * time.Hour)
		primaryContractId := Testing_CreateSettledContract(ctx, clientId, providerId,
			createTime, createTime.Add(10*time.Second), 1024)

		// the return leg: provider -> client, with the client as destination
		Testing_CreateSettledCompanionContract(ctx, providerId, clientId,
			createTime, createTime.Add(1*time.Second), 64*1024*1024, primaryContractId)

		bw, err := ComputePassiveProviderBandwidth(ctx, clientId, 2*time.Hour)
		connect.AssertEqual(t, err, nil)
		if bw != nil {
			t.Errorf(
				"return traffic must not be read as provider egress: got %.0f bytes/sec for a client that never provided",
				bw.BytesPerSecond,
			)
		}
	})
}

// Testing_CreateSettledContract inserts a closed contract and its
// destination-party close row, matching the shape real settlement writes
// (`CloseContract` in subscription_model.go). Returns the contract id.
func Testing_CreateSettledContract(
	ctx context.Context,
	sourceId server.Id,
	destinationId server.Id,
	createTime time.Time,
	closeTime time.Time,
	usedByteCount ByteCount,
) server.Id {
	return testingCreateSettledContract(
		ctx, sourceId, destinationId, createTime, closeTime, usedByteCount, nil,
	)
}

// Testing_CreateSettledCompanionContract is Testing_CreateSettledContract for
// the return-traffic leg of `companionContractId`.
func Testing_CreateSettledCompanionContract(
	ctx context.Context,
	sourceId server.Id,
	destinationId server.Id,
	createTime time.Time,
	closeTime time.Time,
	usedByteCount ByteCount,
	companionContractId server.Id,
) server.Id {
	return testingCreateSettledContract(
		ctx, sourceId, destinationId, createTime, closeTime, usedByteCount, &companionContractId,
	)
}

func testingCreateSettledContract(
	ctx context.Context,
	sourceId server.Id,
	destinationId server.Id,
	createTime time.Time,
	closeTime time.Time,
	usedByteCount ByteCount,
	companionContractId *server.Id,
) server.Id {
	contractId := server.NewId()

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO transfer_contract (
				contract_id,
				source_network_id,
				source_id,
				destination_network_id,
				destination_id,
				transfer_byte_count,
				create_time,
				close_time,
				outcome,
				companion_contract_id
			)
			VALUES (
				$1,
				(SELECT network_id FROM network_client WHERE client_id = $2),
				$2,
				(SELECT network_id FROM network_client WHERE client_id = $3),
				$3,
				$4,
				$5,
				$6,
				'success',
				$7
			)
			`,
			contractId,
			sourceId,
			destinationId,
			usedByteCount,
			createTime.UTC(),
			closeTime.UTC(),
			companionContractId,
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO contract_close (contract_id, close_time, party, used_transfer_byte_count)
			VALUES ($1, $2, 'destination', $3)
			`,
			contractId,
			closeTime.UTC(),
			usedByteCount,
		))
	})

	return contractId
}
