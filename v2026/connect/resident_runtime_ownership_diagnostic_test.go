package connect

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// This local-only control includes real H1 handlers, exchange TCP connections,
// full residents and acknowledged traffic. Its process heap/goroutine deltas
// include the synthetic remote clients; only the resident client census names
// storage owned by the service side. No runtime stack/profile is collected.
func TestResidentRuntimeOwnershipChurnControl(t *testing.T) {
	if testing.Short() {
		t.Skip("requires the attested local database and Redis fixture")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		env := testing_newPeerDiscoveryEnvWithSettings(ctx, t, func(settings *ExchangeSettings) {
			// Keep the production resident/transport storage sizes. Peer lists
			// are a separate population cost, excluded from this control.
			settings.EnableNetworkPeers = false
			settings.ExchangeResidentTtl = time.Minute
		})
		envClosed := false
		defer func() {
			if !envClosed {
				env.Close()
			}
		}()

		type owner struct {
			client    *clientconnect.Client
			transport *clientconnect.PlatformTransport
			strategy  *clientconnect.ClientStrategy
			resident  *Resident
		}
		var retained []*Resident
		for cycle := 0; cycle < 3; cycle++ {
			var owners []owner
			func() {
				defer func() {
					for _, entry := range owners {
						entry.transport.Close()
						entry.client.Close()
						if entry.resident != nil {
							entry.resident.Close()
						}
					}
					for _, entry := range owners {
						if err := entry.transport.CloseAndWait(ctx); err != nil {
							t.Errorf("transport owner join: %v", err)
						}
						if err := entry.client.CloseAndWait(ctx); err != nil {
							t.Errorf("remote client owner join: %v", err)
						}
						entry.strategy.Close()
						if entry.resident != nil {
							if err := entry.resident.CloseAndWait(ctx); err != nil {
								t.Errorf("resident client owner join: %v", err)
							}
							retained = append(retained, entry.resident)
						}
					}
				}()

				for member := 0; member < 8; member++ {
					clientID, token := env.authClient(&model.AuthNetworkClientArgs{Description: "synthetic ownership control"})
					settings := newPeerDiscoveryClientSettings()
					settings.ControlPingTimeout = 0 // Explicit ACK barriers below.
					client := clientconnect.NewClient(ctx, clientconnect.Id(clientID),
						Testing_NewControllerOutOfBandControl(ctx, clientID, settings.ContractManagerSettings), settings)
					strategy := clientconnect.NewClientStrategyWithDefaults(ctx)
					transportSettings := clientconnect.DefaultPlatformTransportSettings()
					transportSettings.QuicTlsConfig.InsecureSkipVerify = true
					transportSettings.H3Port = env.h3Port
					transportSettings.DnsPort = 0
					transport := clientconnect.NewPlatformTransportWithTargetMode(ctx, strategy, client.RouteManager(),
						fmt.Sprintf("ws://127.0.0.1:%d", env.port),
						&clientconnect.ClientAuth{ByJwt: token, InstanceId: clientconnect.NewId(), AppVersion: "0.0.0"},
						clientconnect.TransportModeH1, transportSettings)
					owners = append(owners, owner{client: client, transport: transport, strategy: strategy})
					residentRuntimeControlAck(t, ctx, client, clientconnect.ControlId)
					env.exchange.stateLock.Lock()
					resident := env.exchange.residents[clientID]
					env.exchange.stateLock.Unlock()
					if resident == nil {
						t.Fatal("acknowledged local control lacks its resident")
					}
					owners[len(owners)-1].resident = resident
					residentRuntimeControlAck(t, ctx, resident.client, clientconnect.Id(clientID))
				}

				var aggregate clientconnect.TransferOwnerCensus
				for _, entry := range owners {
					census := entry.resident.client.MemoryOwnerCensus()
					if census.SendWorkers == 0 || census.ReceiveWorkers == 0 || census.SendAckSlots == 0 || census.KnownChannelSlotBytes == 0 {
						t.Fatalf("full resident control did not activate both transfer owners: %+v", census)
					}
					aggregate.Clients += census.Clients
					aggregate.SendIndexed += census.SendIndexed
					aggregate.SendWorkers += census.SendWorkers
					aggregate.ReceiveIndexed += census.ReceiveIndexed
					aggregate.ReceiveWorkers += census.ReceiveWorkers
					aggregate.SendCanceledWorkers += census.SendCanceledWorkers
					aggregate.ReceiveCanceledWorkers += census.ReceiveCanceledWorkers
					aggregate.SendPackSlots += census.SendPackSlots
					aggregate.SendAckSlots += census.SendAckSlots
					aggregate.ReceivePackSlots += census.ReceivePackSlots
					aggregate.PacingServices += census.PacingServices
					aggregate.EncryptionSessions += census.EncryptionSessions
					aggregate.ContractDestinations += census.ContractDestinations
					aggregate.ContractStatsEntries += census.ContractStatsEntries
					aggregate.ContractStatsSequences += census.ContractStatsSequences
					aggregate.QueuedPacks += census.QueuedPacks
					aggregate.KnownChannelSlotBytes += census.KnownChannelSlotBytes
					aggregate.KnownSequenceStructBytes += census.KnownSequenceStructBytes
					aggregate.KnownPacingStructBytes += census.KnownPacingStructBytes
				}
				data, err := json.Marshal(aggregate)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("resident_runtime_control cycle=%d population=%d census=%s process_goroutines=%d", cycle, len(owners), data, runtime.NumGoroutine())
			}()

			for _, resident := range retained {
				census := resident.client.MemoryOwnerCensus()
				if census.SendWorkers != 0 || census.ReceiveWorkers != 0 || census.SendIndexed != 0 || census.ReceiveIndexed != 0 || census.KnownChannelSlotBytes != 0 || census.PacingServices != 0 {
					t.Fatalf("joined resident retained transfer worker/storage ownership after cycle %d: %+v", cycle, census)
				}
			}
			t.Logf("resident_runtime_control cycle=%d retired_clients=%d joined_sequence_owners=0", cycle, len(retained))
		}
		env.Close()
		envClosed = true
		for _, resident := range retained {
			if resident.TransportCount() != 0 || resident.client.RouteManager().HasActiveTransport() {
				t.Fatal("joined exchange retained a retired resident transport route")
			}
		}
		t.Logf("resident_runtime_control retired_clients=%d joined_transport_routes=0", len(retained))
	})
}

func residentRuntimeControlAck(t testing.TB, ctx context.Context, client *clientconnect.Client, destination clientconnect.Id) {
	frame := clientconnect.RequireToFrameWithDefaultProtocolVersion(&protocol.ControlPing{})
	acked := make(chan error, 1)
	if !client.SendWithTimeout(frame, destination, func(err error) { acked <- err }, time.Second) {
		clientconnect.MessagePoolReturn(frame.MessageBytes)
		t.Fatal("synthetic control frame was not admitted")
	}
	select {
	case err := <-acked:
		if err != nil {
			t.Fatalf("synthetic control frame delivery: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("synthetic control acknowledgement deadline")
	}
}
