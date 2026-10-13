// A signer deployed through provider_work_session.json needs the transaction
// lookup index that migration 824 drops; explicit fixture owners are not
// deployments. All identities and keys here are synthetic.
package model

import (
	"bytes"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
)

// The fresh head database has applied 824, so a deployed signer refuses: the
// connection owner keeps its session events unsigned rather than reading the
// client's whole event history for each signature. Building the index again
// lets the next operation sign every event it issues. Without the guard the
// first connection's events were signed by scanning the history.
func TestProviderWorkSessionDeployedSignerRequiresTransactionIndex(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		Testing_ResetExtenderAddressCache()
		t.Cleanup(Testing_ResetExtenderAddressCache)
		ctx := t.Context()
		now := server.NowUtc()

		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{113}, ed25519.SeedSize))
		config := providerWorkInspectionConfig{
			Schema: ProviderWorkSessionSourceSchema,
			Authority: protocol.ProviderWorkSourceAuthority{
				DomainHash:          [32]byte{107},
				SourceId:            server.NewId().String(),
				Generation:          server.NewId().String(),
				PublicKey:           [32]byte(key.Public().(ed25519.PublicKey)),
				FromUnixMicro:       now.Add(-time.Hour).UnixMicro(),
				ThroughUnixMicro:    now.Add(time.Hour).UnixMicro(),
				MaxEndpointEvents:   4096,
				MaxCohortMembers:    64,
				DirectoryPublicKeys: [][32]byte{},
			},
			PrivateKey: key,
		}
		pop := server.Vault.PushSimpleResource("provider_work_session.json", providerWorkInspectionBytes(t, config))
		defer pop()

		handlerId := CreateNetworkClientHandler(ctx)
		// connects a fresh client through the real connection owner, which
		// appends its session events and signs them when it can
		connectClient := func(address string) (eventCount int, receiptCount int) {
			clientId, networkId := server.NewId(), server.NewId()
			addContractPayoutTestClients(ctx, map[server.Id]server.Id{clientId: networkId})
			waitConnectDayWrites(recordConnectDayForTest(ctx, networkId, clientId, now))
			if _, _, _, _, err := ConnectNetworkClientWithIpFamily(ctx, clientId, address, handlerId, 4); err != nil {
				t.Fatal(err)
			}
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT
						(SELECT count(*) FROM provider_work_session_event WHERE client_id=$1),
						(SELECT count(*) FROM provider_work_session_receipt WHERE client_id=$1)`,
					clientId,
				).Scan(&eventCount, &receiptCount))
			})
			return
		}

		eventCount, receiptCount := connectClient("192.0.2.30:10030")
		if eventCount == 0 || receiptCount != 0 {
			t.Fatalf("deployed signer without the transaction index left %d of %d events signed; want all unsigned", receiptCount, eventCount)
		}

		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`CREATE INDEX provider_work_session_event_transaction ON provider_work_session_event(transaction_id,client_id,sequence)`,
			))
		})
		eventCount, receiptCount = connectClient("192.0.2.31:10031")
		if eventCount == 0 || receiptCount != eventCount {
			t.Fatalf("deployed signer with the transaction index signed %d of %d events; want all signed", receiptCount, eventCount)
		}
	})
}
