package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
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

// TestProviderBandwidthTableExists asserts the schema shape the storage path
// depends on, separately from the storage path itself: a missing table here is a
// migration that was never appended, not a bug in StoreProviderBandwidth.
func TestProviderBandwidthTableExists(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		var exists bool
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT to_regclass('provider_bandwidth') IS NOT NULL`)
			connect.AssertEqual(t, err, nil)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&exists))
				}
			})
		})
		if !exists {
			t.Fatal("provider_bandwidth table does not exist")
		}
	})
}

// TestStoreProviderBandwidthUpsertsOneRowPerSource covers the round trip and
// the primary-key overwrite: a second measurement from the SAME source replaces
// the first rather than accumulating history.
func TestStoreProviderBandwidthUpsertsOneRowPerSource(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientId := server.NewId()
		windowStart := server.NowUtc().Add(-2 * time.Hour)
		windowEnd := server.NowUtc().Add(-1 * time.Hour)

		StoreProviderBandwidth(ctx, &ProviderBandwidth{
			ClientId:        clientId,
			BytesPerSecond:  1024 * 1024,
			Source:          ProviderBandwidthSourcePassive,
			SampleByteCount: ByteCount(32 * 1024 * 1024),
			WindowStart:     windowStart,
			WindowEnd:       windowEnd,
		})

		stored := testingReadProviderBandwidth(ctx, clientId)
		connect.AssertEqual(t, len(stored), 1)
		passive := stored[ProviderBandwidthSourcePassive]
		connect.AssertEqual(t, passive.BytesPerSecond, float64(1024*1024))
		connect.AssertEqual(t, passive.SampleByteCount, ByteCount(32*1024*1024))
		connect.AssertEqual(t, passive.WindowStart.UTC().Unix(), windowStart.Unix())
		connect.AssertEqual(t, passive.WindowEnd.UTC().Unix(), windowEnd.Unix())

		// a later measurement from the SAME source replaces it in place
		laterWindowStart := server.NowUtc().Add(-1 * time.Minute)
		laterWindowEnd := server.NowUtc()
		StoreProviderBandwidth(ctx, &ProviderBandwidth{
			ClientId:        clientId,
			BytesPerSecond:  4 * 1024 * 1024,
			Source:          ProviderBandwidthSourcePassive,
			SampleByteCount: ByteCount(8 * 1024 * 1024),
			WindowStart:     laterWindowStart,
			WindowEnd:       laterWindowEnd,
		})

		stored = testingReadProviderBandwidth(ctx, clientId)
		connect.AssertEqual(t, len(stored), 1)
		passive = stored[ProviderBandwidthSourcePassive]
		connect.AssertEqual(t, passive.BytesPerSecond, float64(4*1024*1024))
		connect.AssertEqual(t, passive.SampleByteCount, ByteCount(8*1024*1024))
		connect.AssertEqual(t, passive.WindowEnd.UTC().Unix(), laterWindowEnd.Unix())
	})
}

// TestStoreProviderBandwidthKeepsEverySourceSeparate is the property the whole
// two-target design rests on: the operator and cdn measurements for ONE
// provider are stored as two rows carrying two figures, and neither overwrites
// the other or the passive figure.
//
// Keyed on client_id alone -- as this table was before the (client_id, source)
// migration -- the second measurement of every pass would silently replace the
// first, so only one target could ever be stored and the divergence between
// them, which is the only reason a second target exists, would be
// unobservable.
func TestStoreProviderBandwidthKeepsEverySourceSeparate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()
		now := server.NowUtc()

		figures := map[string]float64{
			ProviderBandwidthSourcePassive:        1 * 1024 * 1024,
			ProviderBandwidthSourceActiveOperator: 12 * 1024 * 1024,
			ProviderBandwidthSourceActiveCDN:      3 * 1024 * 1024,
		}
		for source, bytesPerSecond := range figures {
			StoreProviderBandwidth(ctx, &ProviderBandwidth{
				ClientId:        clientId,
				BytesPerSecond:  bytesPerSecond,
				Source:          source,
				SampleByteCount: ByteCount(5 * 1024 * 1024),
				WindowStart:     now,
				WindowEnd:       now,
			})
		}

		stored := testingReadProviderBandwidth(ctx, clientId)
		if len(stored) != len(figures) {
			t.Fatalf("%d rows stored for one provider, want %d (one per source) -- the sources are overwriting each other", len(stored), len(figures))
		}
		for source, want := range figures {
			row, ok := stored[source]
			if !ok {
				t.Fatalf("no row for source %q", source)
			}
			if row.BytesPerSecond != want {
				t.Errorf("source %q stored %.0f B/s, want %.0f -- the figures are not being kept apart",
					source, row.BytesPerSecond, want)
			}
		}

		// specifically: the two active targets diverge, and both divergent
		// figures survive. An averaged pair would leave both at 7.5 MB/s.
		operator := stored[ProviderBandwidthSourceActiveOperator].BytesPerSecond
		cdn := stored[ProviderBandwidthSourceActiveCDN].BytesPerSecond
		if operator == cdn {
			t.Errorf("both active targets stored %.0f B/s; a provider prioritising one path is invisible once they collapse to one figure", operator)
		}
	})
}

// testingReadProviderBandwidth reads the stored rows back with sql, keyed by
// source, so the test asserts what is actually in the table rather than
// trusting a model reader that StoreProviderBandwidth's own writer would share.
func testingReadProviderBandwidth(
	ctx context.Context,
	clientId server.Id,
) map[string]*ProviderBandwidth {
	bySource := map[string]*ProviderBandwidth{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				client_id,
				bytes_per_second,
				source,
				sample_byte_count,
				window_start,
				window_end
			FROM provider_bandwidth
			WHERE client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				bw := &ProviderBandwidth{}
				server.Raise(result.Scan(
					&bw.ClientId,
					&bw.BytesPerSecond,
					&bw.Source,
					&bw.SampleByteCount,
					&bw.WindowStart,
					&bw.WindowEnd,
				))
				bySource[bw.Source] = bw
			}
		})
	})
	return bySource
}
