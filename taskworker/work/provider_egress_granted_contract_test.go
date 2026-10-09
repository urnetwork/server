package work

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
	"google.golang.org/protobuf/proto"
)

// Both authenticated API routing and the pass-owned local controller use the
// actual escrow grant in their signed reply. This models control and accounting,
// not transport throughput or receiver packet accounting.
func TestProviderEgressGrantedContractAPIAndLocalParity(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		for _, mode := range []string{"api", "local"} {
			for _, requested := range []model.ByteCount{3 * model.Mib, 128 * model.Mib} {
				const granted = 3 * model.Mib
				network, user, parent, device := server.NewId(), server.NewId(), server.NewId(), server.NewId()
				model.Testing_CreateNetwork(ctx, network, "grant-parity-"+network.String(), user)
				model.Testing_CreateDevice(ctx, network, device, parent, "grant parent", "fixture")
				parentToken := session.NewByJwt(network, user, "grant parity", false, false).Client(device, parent).Testing_Sign()
				credentials, err := newProviderEgressCredentials(&model.ProberIdentity{NetworkId: &network, UserId: &user, ClientId: &parent, ByClientJwt: parentToken})
				server.Raise(err)
				server.Raise(model.AddBasicTransferBalance(ctx, network, granted, server.NowUtc(), server.NowUtc().Add(time.Hour)))
				balances := model.GetActiveTransferBalances(ctx, network)
				if len(balances) != 1 {
					t.Fatal("fixture must own exactly one funding grant")
				}
				balanceID := balances[0].BalanceId
				providerNetwork, provider := server.NewId(), server.NewId()
				model.Testing_CreateNetwork(ctx, providerNetwork, "grant-provider-"+providerNetwork.String(), server.NewId())
				model.Testing_CreateDevice(ctx, providerNetwork, server.NewId(), provider, "grant provider", "fixture")
				secret := bytes.Repeat([]byte{53}, 32)
				model.SetProvide(ctx, provider, map[model.ProvideMode][]byte{model.ProvideModePublic: secret})
				source := connect.Id(parent)
				minted, err := credentials.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{SourceClientId: &source})
				server.Raise(err)
				claims, err := session.ParseByJwtForAudience(ctx, minted.ByClientJwt, session.ByJwtAudienceApi)
				server.Raise(err)
				if claims.ClientId == nil {
					t.Fatal("derived client credential unavailable")
				}
				child := *claims.ClientId
				notifications := model.NewContractOriginNotifications(ctx, model.DefaultContractOriginNotificationSettings())
				defer notifications.Close()
				local, err := newProviderEgressControl(credentials, notifications)
				server.Raise(err)
				send := func(message proto.Message) []*protocol.Frame {
					frame, err := connect.ToFrame(message, connect.DefaultProtocolVersion)
					server.Raise(err)
					defer connect.MessagePoolReturn(frame.MessageBytes)
					packed, err := proto.Marshal(&protocol.Pack{Frames: []*protocol.Frame{frame}})
					server.Raise(err)
					args := &connect.ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(packed)}
					var result *connect.ConnectControlResult
					if mode == "local" {
						result, err = local.ConnectControl(ctx, minted.ByClientJwt, args)
						server.Raise(err)
					} else {
						body, err := json.Marshal(args)
						server.Raise(err)
						req := httptest.NewRequest(http.MethodPost, "/connect/control", bytes.NewReader(body)).WithContext(model.WithContractOriginNotifications(ctx, notifications))
						req.Header.Set("Authorization", "Bearer "+minted.ByClientJwt)
						req.Header.Set("Content-Type", "application/json")
						w := httptest.NewRecorder()
						router.WrapWithInputRequireClient(controller.ConnectControl, w, req)
						if w.Code != http.StatusOK {
							t.Fatal("authenticated API control refused", w.Code)
						}
						result = &connect.ConnectControlResult{}
						server.Raise(json.Unmarshal(w.Body.Bytes(), result))
					}
					if result == nil || result.Error != nil {
						t.Fatal("control operation did not complete", mode)
					}
					wire, err := base64.StdEncoding.DecodeString(result.Pack)
					server.Raise(err)
					pack := &protocol.Pack{}
					server.Raise(proto.Unmarshal(wire, pack))
					return pack.Frames
				}
				frames := send(&protocol.CreateContract{DestinationId: provider.Bytes(), TransferByteCount: uint64(requested)})
				if len(frames) != 1 {
					t.Fatal("contract reply count changed", mode)
				}
				message, err := connect.FromFrame(frames[0])
				server.Raise(err)
				result, ok := message.(*protocol.CreateContractResult)
				if !ok || result.Error != nil || result.Contract == nil {
					t.Fatal("funded grant request did not return a contract", mode)
				}
				mac := hmac.New(sha256.New, secret)
				_, _ = mac.Write(result.Contract.StoredContractBytes)
				if !hmac.Equal(mac.Sum(nil), result.Contract.StoredContractHmac) {
					t.Fatal("provider signature did not cover returned contract bytes")
				}
				stored := &protocol.StoredContract{}
				server.Raise(proto.Unmarshal(result.Contract.StoredContractBytes, stored))
				if stored.TransferByteCount != uint64(granted) || !bytes.Equal(stored.SourceId, child.Bytes()) || !bytes.Equal(stored.DestinationId, provider.Bytes()) {
					t.Fatal("signed grant exceeded actual funding or changed endpoint ownership", mode, requested == granted)
				}
				id := server.RequireIdFromBytes(stored.ContractId)
				server.Db(ctx, func(conn server.PgConn) {
					var issued, escrow model.ByteCount
					var marked bool
					server.Raise(conn.QueryRow(ctx, `SELECT transfer_byte_count,
 (SELECT sum(balance_byte_count) FROM transfer_escrow WHERE contract_id=$1),
 (SELECT bool_and(redis_reserved) FROM transfer_escrow WHERE contract_id=$1)
 FROM transfer_contract WHERE contract_id=$1`, id).Scan(&issued, &escrow, &marked))
					if issued != granted || escrow != granted || !marked {
						t.Fatal("signed reply and durable escrow disagree")
					}
				})
				_ = send(&protocol.CloseContract{ContractId: stored.ContractId, AckedByteCount: uint64(granted)})
				server.Raise(model.CloseContract(ctx, id, provider, granted, false))
				flushed, err := model.FlushTransferDebits(ctx, int(balanceID[15])%model.TransferDebitShardCount, nil, 1)
				if err != nil || flushed.Applied != 1 || flushed.Released != 1 || flushed.Failed != 0 || flushed.Busy != 0 {
					t.Fatal("actual granted usage did not debit and release once", mode, err)
				}
				check := func() {
					server.Db(ctx, func(conn server.PgConn) {
						var credit, swept, reported model.ByteCount
						var revenue model.NanoCents
						var pending int
						var terminal bool
						server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2),
 COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$1 AND network_id=$3),0),
 COALESCE((SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=$1 AND network_id=$3),0),
 (SELECT count(*) FROM transfer_debit_journal WHERE contract_id=$1),outcome='settled',
 (SELECT sum(used_transfer_byte_count) FROM contract_close WHERE contract_id=$1)
 FROM transfer_contract WHERE contract_id=$1`, id, balanceID, providerNetwork).Scan(&credit, &swept, &revenue, &pending, &terminal, &reported))
						if credit != 0 || swept != granted || revenue != 0 || pending != 0 || !terminal || reported != 2*granted {
							t.Fatal("grant settlement lost payer/provider conservation or original reports")
						}
					})
				}
				check()
				flushed, err = model.FlushTransferDebits(ctx, int(balanceID[15])%model.TransferDebitShardCount, nil, 1)
				if err != nil || flushed.Applied != 0 || flushed.Released != 0 || flushed.Failed != 0 {
					t.Fatal("worker replay repeated financial work")
				}
				check()
				_, err = credentials.RemoveNetworkClient(ctx, &connect.RemoveNetworkClientArgs{ClientId: connect.Id(child)})
				server.Raise(err)
			}
		}
	})
}
