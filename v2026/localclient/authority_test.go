package localclient

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"google.golang.org/protobuf/proto"
)

func authorityTestOwner(t testing.TB, ctx context.Context) (*Authority, *jwt.ByJwt) {
	t.Helper()
	network, user, device, client := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, network, "local-authority-test", user)
	model.Testing_CreateDevice(ctx, network, device, client, "test", "test")
	claims := jwt.NewByJwt(network, user, "local-authority-test", false, false).Client(device, client)
	owner, err := New(ctx, claims.Sign(), "https://control.example")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	return owner, claims
}

func authorityTestMint(t testing.TB, ctx context.Context, owner *Authority) (*connect.AuthNetworkClientResult, *jwt.ByJwt) {
	t.Helper()
	parent := connect.Id(owner.clientId)
	result, err := owner.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{SourceClientId: &parent, Description: "derived-test", DeviceSpec: "test"})
	if err != nil || result == nil || result.Error != nil || result.ByClientJwt == "" {
		t.Fatal("local mint failed", err)
	}
	claims, err := jwt.ParseByJwtForAudience(ctx, result.ByClientJwt, jwt.ByJwtAudienceApi)
	if err != nil {
		t.Fatal(err)
	}
	return result, claims
}

func authorityTestUnauthorized(t testing.TB, err error) {
	t.Helper()
	var status *connect.HttpStatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusUnauthorized {
		t.Fatal("auth refusal lost its 401 classification")
	}
}

// Durable source ownership survives reconstruction, while a different parent
// in the same network, a forged token and a retired child all remain refused.
func TestAuthorityDerivedScopeSurvivesReconstruction(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		owner, claims := authorityTestOwner(t, ctx)
		child, childClaims := authorityTestMint(t, ctx, owner)
		restored, err := New(ctx, owner.token(), owner.apiUrl)
		if err != nil {
			t.Fatal(err)
		}
		defer restored.Close()
		s, err := restored.authenticate(ctx, child.ByClientJwt, false)
		if err != nil {
			t.Fatal("restored identity lost durable ownership", err)
		}
		s.Cancel()
		otherDevice, otherClient := server.NewId(), server.NewId()
		model.Testing_CreateDevice(ctx, claims.NetworkId, otherDevice, otherClient, "other", "other")
		otherClaims := jwt.NewByJwt(claims.NetworkId, claims.UserId, claims.NetworkName, false, false).Client(otherDevice, otherClient)
		other, err := New(ctx, otherClaims.Sign(), owner.apiUrl)
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		foreign, _ := authorityTestMint(t, ctx, other)
		_, err = owner.authenticate(ctx, foreign.ByClientJwt, false)
		authorityTestUnauthorized(t, err)
		_, err = owner.authenticate(ctx, "invalid.signature.token", false)
		authorityTestUnauthorized(t, err)
		wrongAudience := *claims
		wrongAudience.Audience = []string{jwt.ByJwtAudienceConnect}
		_, err = owner.authenticate(ctx, wrongAudience.Sign(), false)
		authorityTestUnauthorized(t, err)
		_, err = owner.authenticate(ctx, child.ByClientJwt, true)
		authorityTestUnauthorized(t, err)
		if _, err = owner.RemoveNetworkClient(ctx, &connect.RemoveNetworkClientArgs{ClientId: connect.Id(owner.clientId)}); err == nil {
			t.Fatal("parent was retired as a derived client")
		}
		if _, err = owner.RemoveNetworkClient(ctx, &connect.RemoveNetworkClientArgs{ClientId: connect.Id(*childClaims.ClientId)}); err != nil {
			t.Fatal(err)
		}
		_, err = restored.authenticate(ctx, child.ByClientJwt, false)
		authorityTestUnauthorized(t, err)
		if _, err = owner.Get(ctx, "https://other.example/auth/refresh", owner.token()); err == nil {
			t.Fatal("foreign API origin accepted")
		}
		owner.Close()
		if _, err = owner.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{SourceClientId: (*connect.Id)(claims.ClientId)}); !errors.Is(err, context.Canceled) {
			t.Fatal("closed local owner admitted a new credential")
		}
	})
}

// Password rotation and caller cancellation keep distinct outcomes; only
// authoritative authentication refusals become 401, never a timeout.
func TestAuthorityCredentialRotationAndCancellation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		owner, claims := authorityTestOwner(t, ctx)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := owner.Get(canceled, owner.apiUrl+"/auth/refresh", owner.token())
		if !errors.Is(err, context.Canceled) {
			t.Fatal("caller cancellation lost", err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time=$2 WHERE user_id=$1`, claims.UserId, server.NowUtc().Add(time.Second)))
		})
		_, err = owner.Get(ctx, owner.apiUrl+"/auth/refresh", owner.token())
		authorityTestUnauthorized(t, err)
	})
}

type authorityTestReply struct {
	messages []proto.Message
	err      error
}

func authorityTestSend(ctx context.Context, owner *connect.ApiOutOfBandControl, message proto.Message) authorityTestReply {
	f, err := connect.ToFrame(message, connect.DefaultProtocolVersion)
	if err != nil {
		return authorityTestReply{err: err}
	}
	done := make(chan authorityTestReply, 1)
	owner.SendControlWithCtx(ctx, []*protocol.Frame{f}, func(frames []*protocol.Frame, err error) {
		r := authorityTestReply{err: err}
		for _, f := range frames {
			m, e := connect.FromFrame(f)
			if e != nil {
				r.err = errors.Join(r.err, e)
			} else {
				r.messages = append(r.messages, proto.Clone(m))
			}
		}
		done <- r
	})
	select {
	case r := <-done:
		return r
	case <-ctx.Done():
		return authorityTestReply{err: ctx.Err()}
	}
}

// The local dispatch issues a provider-authenticated real contract, settles
// both parties' actual usage and returns the same published key wire schema.
func TestAuthorityRealContractAccountingAndPublicKeyReads(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		owner, claims := authorityTestOwner(t, ctx)
		code, err := model.CreateBalanceCode(ctx, 1<<30, 24*time.Hour, 0, "local-authority-balance", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if r, e := model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{Secret: code.Secret, NetworkId: claims.NetworkId}, ctx); e != nil || r.Error != nil {
			t.Fatal("funding failed", e)
		}
		providerDevice, provider := server.NewId(), server.NewId()
		providerNetwork, providerUser := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, providerNetwork, "local-provider-test", providerUser)
		model.Testing_CreateDevice(ctx, providerNetwork, providerDevice, provider, "provider", "provider")
		key := bytes.Repeat([]byte{41}, 32)
		model.SetProvide(ctx, provider, map[model.ProvideMode][]byte{model.ProvideModePublic: key})
		child, childClaims := authorityTestMint(t, ctx, owner)
		settings := connect.DefaultClientStrategySettings()
		settings.EnableResilient = false
		strategy := connect.NewClientStrategy(ctx, settings)
		defer strategy.Close()
		oob := connect.NewApiOutOfBandControlWithLocalControl(ctx, strategy, child.ByClientJwt, owner.apiUrl, owner)
		defer oob.CloseAndWait(context.Background())
		reply := authorityTestSend(ctx, oob, &protocol.CreateContract{DestinationId: provider.Bytes(), TransferByteCount: 4096})
		if reply.err != nil || len(reply.messages) != 1 {
			t.Fatal("contract reply missing", reply.err)
		}
		created, ok := reply.messages[0].(*protocol.CreateContractResult)
		if !ok || created.Error != nil || created.Contract == nil {
			t.Fatal("real contract refused")
		}
		stored := &protocol.StoredContract{}
		if err := proto.Unmarshal(created.Contract.StoredContractBytes, stored); err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, key)
		mac.Write(created.Contract.StoredContractBytes)
		if !hmac.Equal(mac.Sum(nil), created.Contract.StoredContractHmac) || !bytes.Equal(stored.SourceId, childClaims.ClientId.Bytes()) || !bytes.Equal(stored.DestinationId, provider.Bytes()) {
			t.Fatal("contract authentication or endpoints changed")
		}
		id, err := server.IdFromBytes(stored.ContractId)
		if err != nil {
			t.Fatal(err)
		}
		var balanceId server.Id
		var initialCredit, reserved int64
		server.Db(ctx, func(c server.PgConn) {
			var marked bool
			var escrowRows int
			server.Raise(c.QueryRow(ctx, `SELECT escrow.balance_id,balance.balance_byte_count,
 escrow.balance_byte_count,escrow.redis_reserved,
 (SELECT count(*) FROM transfer_escrow WHERE contract_id=$1)
 FROM transfer_escrow AS escrow JOIN transfer_balance AS balance USING(balance_id)
 WHERE escrow.contract_id=$1`, id).Scan(&balanceId, &initialCredit, &reserved, &marked, &escrowRows))
			if escrowRows != 1 || !marked || reserved != int64(stored.TransferByteCount) || reserved < 1024 {
				t.Fatal("local contract did not retain its exact Redis-backed grant")
			}
		})
		if reply = authorityTestSend(ctx, oob, &protocol.CloseContract{ContractId: stored.ContractId, AckedByteCount: 1024}); reply.err != nil {
			t.Fatal(reply.err)
		}
		if err = model.CloseContract(ctx, id, provider, 1024, false); err != nil {
			t.Fatal(err)
		}
		// Foreground close commits usage and a durable debit. The production
		// debit worker publishes escrow metadata and releases the reservation.
		// An immediate metadata read is not a completed accounting boundary.
		server.Db(ctx, func(c server.PgConn) {
			var debit, credit int64
			var applied bool
			server.Raise(c.QueryRow(ctx, `SELECT debit_byte_count,applied,
 (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)
 FROM transfer_debit_journal WHERE contract_id=$1 AND balance_id=$2`, id, balanceId).Scan(&debit, &applied, &credit))
			if debit != 1024 || applied || credit != initialCredit {
				t.Fatal("foreground close lost its unapplied exact debit")
			}
		})
		if got := model.Testing_NetEscrowByteCount(ctx, balanceId); int64(got) != reserved {
			t.Fatal("foreground close released its reservation before debit", got)
		}
		shard := int(balanceId[15]) % model.TransferDebitShardCount
		flushed, err := model.FlushTransferDebits(ctx, shard, nil, 1)
		if err != nil || flushed.Applied != 1 || flushed.Released != 1 || flushed.Balances != 1 || flushed.Busy != 0 || flushed.Failed != 0 {
			t.Fatal("actual debit worker did not settle and release exactly once", flushed, err)
		}
		checkAccounting := func() {
			server.Db(ctx, func(c server.PgConn) {
				var settled, terminal bool
				var payout, credit, providerPayout, reports int64
				var pending int
				server.Raise(c.QueryRow(ctx, `SELECT bool_and(settled),sum(payout_byte_count) FROM transfer_escrow WHERE contract_id=$1`, id).Scan(&settled, &payout))
				server.Raise(c.QueryRow(ctx, `SELECT
 (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2),
 (SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$1 AND network_id=$3),
 (SELECT count(*) FROM transfer_debit_journal WHERE contract_id=$1),
 (SELECT sum(used_transfer_byte_count) FROM contract_close WHERE contract_id=$1),
 outcome='settled' FROM transfer_contract WHERE contract_id=$1`, id, balanceId, providerNetwork).Scan(&credit, &providerPayout, &pending, &reports, &terminal))
				if !settled || payout != 1024 || credit != initialCredit-1024 || providerPayout != 1024 || pending != 0 || reports != 2048 || !terminal {
					t.Fatal("local contract lost exact payer/provider accounting and drain")
				}
			})
			if got := model.Testing_NetEscrowByteCount(ctx, balanceId); got != 0 {
				t.Fatal("completed debit retained a reservation", got)
			}
		}
		checkAccounting()
		flushed, err = model.FlushTransferDebits(ctx, shard, nil, 1)
		if err != nil || flushed.Applied != 0 || flushed.Released != 0 || flushed.Balances != 0 || flushed.Busy != 0 || flushed.Failed != 0 {
			t.Fatal("debit worker replay repeated financial work", flushed, err)
		}
		checkAccounting()
		public := bytes.Repeat([]byte{7}, 32)
		model.SetClientPublicKey(ctx, provider, public)
		raw, err := owner.Get(ctx, owner.apiUrl+"/key/"+provider.String(), "")
		if err != nil {
			t.Fatal(err)
		}
		var result connect.GetClientKeyResult
		if json.Unmarshal(raw, &result) != nil || !bytes.Equal(result.PublicKey, public) {
			t.Fatal("local public key schema changed")
		}
		raw, err = owner.Get(ctx, owner.apiUrl+"/key/"+provider.String()+"/history", "")
		if err != nil {
			t.Fatal(err)
		}
		var history connect.GetClientKeyHistoryResult
		if json.Unmarshal(raw, &history) != nil {
			t.Fatal("local history schema changed")
		}
	})
}
