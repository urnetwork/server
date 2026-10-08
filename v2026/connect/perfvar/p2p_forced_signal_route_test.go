// Peer-addressed signaling uses the same destination selector as application
// data. These tests characterize the forced-P2P fixture's recovery boundary;
// they do not change payload fallback or claim to reproduce SCTP packet loss.
package perfvar

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

type forcedP2pSignalReceipt struct {
	kind        protocol.SignalType
	reset       bool
	transferKey clientconnect.TransferKey
}

// Checks each original Pack separately, including when one sequence owns both
// the signal and application control. A failed first-write event is a terminal
// disposition, not evidence that a route accepted the Pack.
func requireForcedP2pSignalLifecycle(t *testing.T, events []clientconnect.SendPackLifecycleObservation, wantType protocol.MessageType, wantError bool) {
	t.Helper()
	if len(events) != 3 {
		t.Fatalf("Pack lifecycle has %d events, want exactly three", len(events))
	}
	for i, event := range events {
		if event.Token != events[0].Token || event.Phase != clientconnect.SendPackLifecyclePhase(i+1) || event.MessageType != wantType || !event.AckRequired || (event.Err != nil) != (wantError && i != 0) {
			t.Fatalf("unexpected Pack lifecycle: phase=%d token=%d type=%d err=%v", event.Phase, event.Token, event.MessageType, event.Err)
		}
	}
}

// Both policies start with a received, acknowledged, encrypted peer signal.
// A live P2P route is then installed and withdrawn behind a route-state fence.
// The ordinary H1 policy still delivers peer signaling; the forced fixture
// admits the same signal but expires it unwritten, even though ControlId has
// a live H1 writer. A concurrent application Pack must also stay unwritten.
func TestPlatformSendRouteControllerPeerSignalsAfterP2pWithdrawal(t *testing.T) {
	// The process-global pool worker must live outside the virtual-time bubble.
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	for _, forced := range []bool{false, true} {
		policy := "ordinary-h1"
		if forced {
			policy = "forced-p2p"
		}
		for _, signalCase := range []struct {
			name string
			kind protocol.SignalType
			key  clientconnect.TransferKey
		}{
			{"active-offer", protocol.SignalType_SdpOffer, clientconnect.TransferKey{ForceStream: true}},
			{"passive-waiting", protocol.SignalType_WaitingForSdpOffer, clientconnect.TransferKey{CompanionContract: true}},
			{"passive-answer", protocol.SignalType_SdpAnswer, clientconnect.TransferKey{CompanionContract: true}},
		} {
			t.Run(policy+"/"+signalCase.name, func(t *testing.T) {
				poolBefore := captureRouteMessagePoolSnapshot(nil)
				defer func() {
					if poolAfter, balanced := routeMessagePoolBalance(poolBefore.outstanding); !balanced {
						t.Errorf("signal fixture retained pooled messages: %d -> %d", poolBefore.outstanding, poolAfter.outstanding)
					}
				}()
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
					defer cancel()
					ids := [2]clientconnect.Id{clientconnect.NewId(), clientconnect.NewId()}
					streamId := clientconnect.NewId()
					var clients [2]*clientconnect.Client
					var controllers [2]*platformSendRouteController
					var h1, p2p [2]clientconnect.Route
					var p2pTransports [2]clientconnect.Transport
					var writers [2]clientconnect.MultiRouteWriter
					var observers [2]*clientconnect.TestingMultiRouteWriterRouteStateObserver
					events := make(chan clientconnect.SendPackLifecycleObservation, 32)
					receipts := make(chan forcedP2pSignalReceipt, 4)
					applicationReceipts := make(chan struct{}, 1)
					var ackLifetime time.Duration
					defer func() {
						cancel()
						cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cleanupCancel()
						for i := range clients {
							if observers[i] != nil {
								observers[i].Close()
								clients[i].RouteManager().CloseMultiRouteWriter(writers[i])
							}
							if controllers[i] != nil {
								closePlatformSendRouteController(t, controllers[i])
							}
							if clients[i] != nil {
								if err := clients[i].CloseAndWait(cleanupCtx); err != nil {
									t.Errorf("join signal client: %v", err)
								}
							}
						}
						for _, routes := range [][2]clientconnect.Route{h1, p2p} {
							for _, route := range routes {
								for len(route) != 0 {
									clientconnect.MessagePoolReturn(<-route)
								}
							}
						}
					}()

					for i := range clients {
						settings := clientconnect.DefaultClientSettings()
						settings.Log = clientconnect.NewNoopLogger()
						settings.ControlPingTimeout = 0
						// The forced fixture pre-authorizes no-contract peers after
						// promotion. Normal Connect sequence encryption and identity
						// verification still run, using the peers' real public keys.
						settings.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Now().Add(time.Hour)
						settings.EncryptionSettings.Mode = clientconnect.EncryptionModeRequired
						settings.EncryptionSettings.NewPeerClientPublicKeyFetcher = func(peerId clientconnect.Id) func(context.Context) ([]byte, error) {
							return func(context.Context) ([]byte, error) {
								if peerId != ids[1-i] {
									return nil, fmt.Errorf("unexpected peer identity")
								}
								return clients[1-i].ClientKeyManager().PublicKey(), nil
							}
						}
						if i == 0 {
							ackLifetime = settings.SendBufferSettings.AckTimeout
							settings.SendBufferSettings.SendPackLifecycleObserver = func(event clientconnect.SendPackLifecycleObservation) {
								if event.DestinationId == ids[1] && (event.MessageType == protocol.MessageType_TransferExchangeSignals || event.MessageType == protocol.MessageType_IpIpPacketToProvider) {
									select {
									case events <- event:
									default:
										t.Error("signal lifecycle event buffer overflow")
									}
								}
							}
						}
						clients[i] = clientconnect.NewClient(ctx, ids[i], clientconnect.NewNoContractClientOob(), settings)
						clients[i].ContractManager().AddNoContractPeer(ids[1-i])
						controllers[i] = newPlatformSendRouteController(ids[1-i])
						controllers[i].setRouteManager(clients[i].RouteManager())
						h1[i] = make(clientconnect.Route, 32)
						p2p[i] = make(clientconnect.Route, 32)
						h1Transport, _ := controllers[i].newTransportPair()
						controllers[i].observe(h1Transport, h1[i], true)
						if !controllers[i].waitForIdle() {
							t.Fatal("platform controller closed during setup")
						}
						p2pTransports[i] = clientconnect.NewSendClientTransport(clientconnect.DestinationId(ids[1-i]), clientconnect.StreamId(streamId))
					}
					for i, client := range clients {
						client.RouteManager().UpdateTransport(clientconnect.NewReceiveGatewayTransport(), []clientconnect.Route{h1[1-i], p2p[1-i]})
						writers[i] = client.RouteManager().OpenMultiRouteWriter(clientconnect.DestinationId(ids[1-i]))
						observers[i] = clientconnect.TestingObserveMultiRouteWriterRouteState(writers[i])
					}
					removeCallback := clients[1].AddReceiveCallback(func(_ clientconnect.TransferPath, frames []*protocol.Frame, peer clientconnect.Peer) {
						for _, frame := range frames {
							if frame.MessageType == protocol.MessageType_IpIpPacketToProvider {
								select {
								case applicationReceipts <- struct{}{}:
								default:
									t.Error("duplicate application receipt")
								}
							}
							if frame.MessageType == protocol.MessageType_TransferExchangeSignals {
								message, err := clientconnect.FromFrame(frame)
								if err != nil {
									t.Error("known signal could not be decoded")
									continue
								}
								signal := message.(*protocol.ExchangeSignals)
								if len(signal.Signals) != 1 {
									t.Error("known signal has an unexpected batch size")
									continue
								}
								select {
								case receipts <- forcedP2pSignalReceipt{signal.Signals[0].SignalType, signal.ResetSignals, peer.TransferKey}:
								default:
									t.Error("signal receipt buffer overflow")
								}
							}
						}
					})
					defer removeCallback()
					sendSignal := func(reset bool) {
						frame := clientconnect.RequireToFrameWithDefaultProtocolVersion(&protocol.ExchangeSignals{
							StreamId: streamId.Bytes(), SenderGenerationId: clientconnect.NewId().Bytes(), ResetSignals: reset,
							Signals: []*protocol.ExchangeSignal{{SignalType: signalCase.kind}},
						})
						clientconnect.NewClientSignalSender(clients[0]).SendSignal(ids[1], frame, signalCase.key)
					}
					requireSignalReceipt := func(reset bool) {
						t.Helper()
						select {
						case receipt := <-receipts:
							if receipt.kind != signalCase.kind || receipt.reset != reset || receipt.transferKey.ForceStream != signalCase.key.ForceStream || receipt.transferKey.CompanionContract != signalCase.key.CompanionContract {
								t.Fatalf("signal receipt=%+v does not preserve known signal and route options", receipt)
							}
						case <-ctx.Done():
							t.Fatal("known signal did not reach the peer")
						}
					}
					requireLifecycle := func(wantType protocol.MessageType, wantError bool) {
						t.Helper()
						var observed []clientconnect.SendPackLifecycleObservation
						for range 3 {
							select {
							case event := <-events:
								observed = append(observed, event)
							case <-ctx.Done():
								t.Fatalf("signal lifecycle stalled after %d phases", len(observed))
							}
						}
						requireForcedP2pSignalLifecycle(t, observed, wantType, wantError)
					}

					sendSignal(false)
					requireSignalReceipt(false)
					requireLifecycle(protocol.MessageType_TransferExchangeSignals, false)
					for i, client := range clients {
						client.RouteManager().UpdateTransport(p2pTransports[i], []clientconnect.Route{p2p[i]})
						removeAlias := client.RouteManager().AddWriterDestinationAlias(clientconnect.DestinationId(ids[1-i]), clientconnect.StreamId(streamId))
						defer removeAlias()
						controllers[i].observeP2pRoute(clientconnect.P2pRouteState{PeerId: ids[1-i], StreamId: streamId, Send: true, Connected: true})
						controllers[i].setDisabled(forced)
						wantRoutes := 2
						if forced {
							wantRoutes = 1
						}
						if err := waitForRouteCount(ctx, observers[i], wantRoutes); err != nil {
							t.Fatal(err)
						}
					}
					sendSignal(false)
					requireSignalReceipt(false)
					requireLifecycle(protocol.MessageType_TransferExchangeSignals, false)
					for i, client := range clients {
						barrier := observers[i].Snapshot()
						controllers[i].observeP2pRoute(clientconnect.P2pRouteState{PeerId: ids[1-i], StreamId: streamId, Send: true, Connected: false})
						client.RouteManager().RemoveTransport(p2pTransports[i])
						if !controllers[i].waitForIdle() {
							t.Fatal("platform controller closed during withdrawal")
						}
						wantRoutes := 1
						if forced {
							wantRoutes = 0
						}
						if _, err := waitForRouteCountAfter(ctx, observers[i], barrier, wantRoutes); err != nil {
							t.Fatal(err)
						}
						controlWriter := client.RouteManager().OpenMultiRouteWriter(clientconnect.DestinationId(clientconnect.ControlId))
						if routes := controlWriter.GetActiveRoutes(); len(routes) != 1 || routes[0] != h1[i] {
							t.Fatal("withdrawal removed the independent ControlId H1 route")
						}
						client.RouteManager().CloseMultiRouteWriter(controlWriter)
					}

					reset := signalCase.kind == protocol.SignalType_SdpOffer
					sendSignal(reset)
					if !forced {
						requireSignalReceipt(reset)
						requireLifecycle(protocol.MessageType_TransferExchangeSignals, false)
						return
					}
					synctest.Wait()
					if len(events) != 1 || len(receipts) != 0 {
						t.Fatal("forced peer signal was not admitted and parked before its first route write")
					}
					frame := clientconnect.RequireToFrameWithDefaultProtocolVersion(&protocol.IpPacketToProvider{IpPacket: &protocol.IpPacket{PacketBytes: []byte("application-negative-control")}})
					if !clients[0].SendWithTimeout(frame, ids[1], nil, 0, signalCase.key) {
						clientconnect.MessagePoolReturn(frame.MessageBytes)
						t.Fatal("application negative control was not admitted")
					}
					synctest.Wait()
					if len(events) != 2 || len(applicationReceipts) != 0 {
						t.Fatal("concurrent application Pack did not remain pending with the known signal")
					}
					byToken := map[uint64][]clientconnect.SendPackLifecycleObservation{}
					time.Sleep(ackLifetime - time.Millisecond)
					synctest.Wait()
					for len(events) != 0 {
						event := <-events
						byToken[event.Token] = append(byToken[event.Token], event)
						if event.Phase == clientconnect.SendPackLifecyclePhaseTerminal || (event.Phase == clientconnect.SendPackLifecyclePhaseFirstRouteWrite && event.Err == nil) {
							t.Fatalf("pending peer traffic progressed before acknowledgment expiry: phase=%d err=%v", event.Phase, event.Err)
						}
					}
					time.Sleep(time.Second + time.Millisecond)
					synctest.Wait()
					if ctx.Err() != nil || clients[0].Ctx().Err() != nil || clients[1].Ctx().Err() != nil {
						t.Fatal("client cancellation substituted for normal acknowledgment expiry")
					}
					for len(events) != 0 {
						event := <-events
						byToken[event.Token] = append(byToken[event.Token], event)
					}
					if len(byToken) != 2 {
						t.Fatalf("signal and application lifecycle owners=%d, want two", len(byToken))
					}
					seenTypes := map[protocol.MessageType]bool{}
					for _, observed := range byToken {
						wantType := observed[0].MessageType
						seenTypes[wantType] = true
						requireForcedP2pSignalLifecycle(t, observed, wantType, true)
					}
					if !seenTypes[protocol.MessageType_TransferExchangeSignals] || !seenTypes[protocol.MessageType_IpIpPacketToProvider] {
						t.Fatal("signal and application lifecycle identities were conflated")
					}
					if len(receipts) != 0 || len(applicationReceipts) != 0 {
						t.Fatal("forced peer traffic reached H1 after withdrawal")
					}
					for _, controller := range controllers {
						if controller.fallbackViolationCount.Load() != 0 {
							t.Fatal("peer signaling characterization weakened payload fallback policy")
						}
					}
				})
			})
		}
	}
}
