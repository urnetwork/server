package controller

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// This private diagnostic binds the unmodified native ControlSync retry to
// the actual resident frame dispatcher and PostgreSQL close accumulator. It
// does not assign the mechanism to any existing Main report.
func TestNativeCheckpointReportIdentityThroughController(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, test := range []struct{ loseAck, identity bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
			loseAck, identity := test.loseAck, test.identity
			func() {
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				sourceNetwork, source := server.NewId(), server.NewId()
				providerNetwork, provider := server.NewId(), server.NewId()
				model.Testing_CreateNetwork(ctx, sourceNetwork, "native-checkpoint-source-"+sourceNetwork.String(), server.NewId())
				model.Testing_CreateDevice(ctx, sourceNetwork, server.NewId(), source, "source", "fixture")
				model.Testing_CreateNetwork(ctx, providerNetwork, "native-checkpoint-provider-"+providerNetwork.String(), server.NewId())
				model.Testing_CreateDevice(ctx, providerNetwork, server.NewId(), provider, "provider", "fixture")
				server.Raise(model.AddBasicTransferBalance(ctx, sourceNetwork, 1000, server.NowUtc(), server.NowUtc().Add(time.Hour)))
				balances := model.GetActiveTransferBalances(ctx, sourceNetwork)
				if len(balances) != 1 {
					t.Fatal("fixture does not own one funding grant")
				}
				balance := balances[0].BalanceId
				escrow, err := model.CreateTransferEscrow(ctx, sourceNetwork, source, providerNetwork, provider, 200)
				server.Raise(err)
				if escrow == nil || escrow.TransferByteCount != 200 {
					t.Fatal("fixture did not receive its exact funded grant")
				}

				settings := connect.DefaultClientSettings()
				settings.EncryptionSettings.Mode = connect.EncryptionModeOff
				settings.ControlPingTimeout = 0
				settings.Log = connect.NewNoopLogger()
				settings.SendBufferSettings.AckTimeout = 150 * time.Millisecond
				settings.SendBufferSettings.UnreliableAckTimeout = 150 * time.Millisecond
				settings.SendBufferSettings.SelectiveAckTimeout = 150 * time.Millisecond
				settings.ReceiveBufferSettings.WriteTimeout = time.Millisecond
				nativeCtx, nativeCancel := context.WithCancel(ctx)
				sender := connect.NewClient(nativeCtx, connect.Id(provider), connect.NewNoContractClientOob(), settings)
				receiver := connect.NewClient(nativeCtx, connect.ControlId, connect.NewNoContractClientOob(), settings)
				sender.ContractManager().AddNoContractPeer(connect.ControlId)
				receiver.ContractManager().AddNoContractPeer(sender.ClientId())
				defer func() {
					nativeCancel()
					join, stop := context.WithTimeout(context.Background(), 3*time.Second)
					defer stop()
					if err := sender.CloseAndWait(join); err != nil {
						t.Error("native sender did not join")
					}
					if err := receiver.CloseAndWait(join); err != nil {
						t.Error("native receiver did not join")
					}
				}()
				sendRoute, ackRoute := make(chan []byte), make(chan []byte)
				sender.RouteManager().UpdateTransport(connect.NewSendGatewayTransport(), []connect.Route{sendRoute})
				receiver.RouteManager().UpdateTransport(connect.NewReceiveGatewayTransport(), []connect.Route{sendRoute})
				sender.RouteManager().UpdateTransport(connect.NewReceiveGatewayTransport(), []connect.Route{ackRoute})
				ackTransport := connect.NewSendGatewayTransport()
				if !loseAck {
					receiver.RouteManager().UpdateTransport(ackTransport, []connect.Route{ackRoute})
				}
				var committed atomic.Int32
				callbackErrors := make(chan string, 1)
				receiver.AddReceiveCallback(func(path connect.TransferPath, frames []*protocol.Frame, _ connect.Peer) {
					// The synthetic transport has no authenticated key-registration
					// setup. Retain only the close frames under investigation;
					// native key bootstrap is outside this accounting control.
					var closes []*protocol.Frame
					for _, frame := range frames {
						if frame.MessageType == protocol.MessageType_TransferCloseContract {
							closes = append(closes, frame)
						}
					}
					if len(closes) == 0 {
						return
					}
					out, err := ConnectControlFrames(ctx, server.Id(path.SourceId), closes, connect.DefaultContractManagerSettings())
					returnConnectControlFrames(out)
					if err != nil {
						select {
						case callbackErrors <- controlFrameErrorClass(err):
						default:
						}
						return
					}
					n := committed.Add(1)
					if loseAck && n == 2 {
						// Restore only after the second real SQL commit; the
						// original operation cannot receive its missing ACK.
						receiver.RouteManager().UpdateTransport(ackTransport, []connect.Route{ackRoute})
					}
				})
				sendCheckpoint := func() {
					report := &protocol.CloseContract{ContractId: escrow.ContractId.Bytes(), AckedByteCount: 100, Checkpoint: true}
					if identity {
						report.ReportId = connect.NewId().Bytes()
					}
					frame := connect.RequireToFrameWithDefaultProtocolVersion(report)
					sync := connect.NewControlSync(nativeCtx, sender, "checkpoint-accounting-control")
					defer sync.Close()
					acked := make(chan error, 1)
					sync.Send(frame, nil, func(err error) { acked <- err })
					select {
					case err := <-acked:
						if err != nil {
							t.Fatal("native control did not complete after ACK restoration")
						}
					case cause := <-callbackErrors:
						t.Fatal("native callback failed its actual controller transaction", cause)
					case <-ctx.Done():
						t.Fatal("native replay boundary timed out")
					}
				}
				sendCheckpoint()
				if !loseAck {
					// Equal payloads are two legitimate operations here. A
					// future fix must not dedupe by amount or payload hash.
					sendCheckpoint()
				}
				if committed.Load() != 2 {
					t.Fatal("control did not isolate exactly two committed deliveries")
				}
				var final uint64
				if loseAck {
					final = 100
				}
				closeParty := func(party server.Id, bytes uint64, reportId []byte) error {
					frame := connect.RequireToFrameWithDefaultProtocolVersion(&protocol.CloseContract{ContractId: escrow.ContractId.Bytes(), AckedByteCount: bytes, ReportId: reportId})
					defer connect.MessagePoolReturn(frame.MessageBytes)
					out, err := ConnectControlFrames(ctx, party, []*protocol.Frame{frame}, connect.DefaultContractManagerSettings())
					returnConnectControlFrames(out)
					return err
				}
				var providerReport, sourceReport []byte
				if identity {
					providerReport, sourceReport = connect.NewId().Bytes(), connect.NewId().Bytes()
				}
				server.Raise(closeParty(provider, final, providerReport))
				server.Raise(closeParty(provider, final, providerReport)) // Same final report must not add twice.
				err = closeParty(source, 200, sourceReport)
				refuse := loseAck && !identity
				if refuse {
					if err == nil || !strings.Contains(err.Error(), "Escrow does not have enough value to pay out the full amount.") {
						t.Fatal("native duplicated checkpoint did not reach the owning accounting refusal")
					}
				} else {
					server.Raise(err)
					flushed, err := model.FlushTransferDebits(ctx, int(balance[15])%model.TransferDebitShardCount, nil, 1)
					if err != nil || flushed.Applied != 1 || flushed.Released != 1 {
						t.Fatal("healthy distinct checkpoints did not settle exactly once")
					}
				}
				server.Db(ctx, func(conn server.PgConn) {
					var usedSource, usedDestination, issued, reserved, credit, payout int64
					var terminal, settled bool
					var checkpointCount int
					server.Raise(conn.QueryRow(ctx, `SELECT transfer_byte_count,outcome IS NOT NULL,
 (SELECT used_transfer_byte_count FROM contract_close WHERE contract_id=$1 AND party='source'),
 (SELECT used_transfer_byte_count FROM contract_close WHERE contract_id=$1 AND party='destination'),
 (SELECT count(*) FROM contract_close WHERE contract_id=$1 AND checkpoint),
 (SELECT balance_byte_count FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$2),
 (SELECT settled FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$2),
 (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2),
 COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$1),0)
 FROM transfer_contract WHERE contract_id=$1`, escrow.ContractId, balance).Scan(&issued, &terminal, &usedSource, &usedDestination, &checkpointCount, &reserved, &settled, &credit, &payout))
					wantDestination, wantCredit, wantPayout := int64(200), int64(800), int64(200)
					if refuse {
						wantDestination, wantCredit, wantPayout = 300, 1000, 0
					}
					if issued != 200 || reserved != 200 || usedSource != 200 || usedDestination != wantDestination || checkpointCount != 0 || terminal != !refuse || settled != !refuse || credit != wantCredit || payout != wantPayout {
						t.Fatal("native/controller/model close evidence or financial conservation changed")
					}
				})
			}()
		}
	})
}
