package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func controllerPayoutSchedule(t testing.TB, cutoff time.Time) {
	t.Helper()
	data := fmt.Sprintf("schema: urnetwork-provider-payout-transition-v1\ncutoff_utc: %q\nattribution: settled_contract_close_time\nlegacy_usdc: finish_pre_cutoff_obligations\nmainnet:\n  profile: mainnet\n  chain_id: 964\n  genesis_hash: %q\n  netuid: 25\n  activation: blocked\n", cutoff.UTC().Format(time.RFC3339Nano), "0x"+strings.Repeat("11", 32))
	t.Cleanup(server.Config.PushSimpleResource("sn.yml", []byte(data)))
	policyForPreparation, preparationErr := server.LoadProviderPayoutTransition(context.Background())
	if preparationErr != nil {
		t.Fatal(preparationErr)
	}
	if _, err := server.PrepareProviderPayoutBoundary(context.Background(), policyForPreparation.ConfigSha256); err != nil {
		t.Fatal(err)
	}
}

// An original, terminal contract and its exact escrow credit back the queued
// payment. Processing time is intentionally unrelated to the earning time.
func controllerPayoutFixture(t testing.TB, closed time.Time, historicalGross ...model.NanoCents) (*session.ClientSession, *model.AccountPayment) {
	t.Helper()
	ctx := context.Background()
	network, client, sourceNetwork, source := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	testingCreatePaymentClient(ctx, network, client)
	testingCreatePaymentClient(ctx, sourceNetwork, source)
	owner := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{NetworkId: network, ClientId: &client})
	t.Cleanup(owner.Cancel)
	wallet := model.CreateAccountWalletExternal(owner, &model.CreateAccountWalletExternalArgs{NetworkId: network, Blockchain: "MATIC", WalletAddress: "0x0000000000000000000000000000000000000012", DefaultTokenType: "USDC"})
	if wallet == nil {
		t.Fatal("missing fixture wallet")
	}
	if err := model.SetPayoutWallet(ctx, network, *wallet); err != nil {
		t.Fatal(err)
	}
	code, err := model.CreateBalanceCode(ctx, 1024*1024, 365*24*time.Hour, model.UsdToNanoCents(100), "transition-"+server.NewId().String(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	testingRedeemPaymentBalanceCode(t, ctx, sourceNetwork, code.Secret)
	balances := model.GetActiveTransferBalances(ctx, sourceNetwork)
	if len(balances) != 1 {
		t.Fatal("missing fixture balance")
	}
	paymentId, contract := server.NewId(), server.NewId()
	gross := model.UsdToNanoCents(10)
	if len(historicalGross) == 1 {
		gross = historicalGross[0]
	}
	usage := fmt.Sprintf(`{"version":1,"byte_count":100,"providers":[{"client_id":%q,"network_id":%q,"byte_count":100}]}`, client.String(), network.String())
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment(payment_id,payment_plan_id,network_id,wallet_id,payout_byte_count,payout_nano_cents,min_sweep_time)
		VALUES($1,$2,$3,$4,100,$5,$6)`, paymentId, server.NewId(), network, *wallet, gross, closed.Add(time.Hour)))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
		VALUES($1,$2,$3,$4,$5,100,$6,$7,'settled',$8,true)`, contract, sourceNetwork, source, network, client, closed.Add(-time.Hour), closed, usage))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time,payment_id)
		VALUES($1,$2,$3,$4,100,$5,$6,$7)`, contract, balances[0].BalanceId, network, client, model.UsdToNanoCents(10), closed.Add(time.Hour), paymentId))
	})
	payment, err := model.GetPayment(ctx, paymentId)
	if err != nil || payment == nil {
		t.Fatalf("fixture payment: %v", err)
	}
	return owner, payment
}

func controllerPayoutMock(t testing.TB, create func(context.Context, server.Id, float64, string, string) (*CreateTransferTransactionResult, error)) *mockCircleApiClient {
	t.Helper()
	previous := NewCircleClient()
	client := &mockCircleApiClient{CreateTransferTransactionFunc: create, GetTransactionFunc: defaultGetTransactionDataHandler, EstimateTransferFeeFunc: defaultEstimateFeeHandler}
	SetCircleClient(client)
	t.Cleanup(func() { SetCircleClient(previous) })
	return client
}

func TestProviderTransitionQueuedLegacyAmbiguousRetryKeepsKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		var keys []server.Id
		lostAck := errors.New("processor accepted; response unavailable")
		client := controllerPayoutMock(t, func(ctx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
			if err := requireCircleProviderPayment(ctx); err != nil {
				return nil, err
			}
			keys = append(keys, key)
			if len(keys) == 1 {
				return nil, lostAck
			}
			return &CreateTransferTransactionResult{Id: "synthetic-processor-transfer", State: "INITIATED"}, nil
		})
		args := &AdvancePaymentArgs{PaymentId: payment.PaymentId}
		if _, err := AdvancePayment(args, owner); err == nil {
			t.Fatal("lost acknowledgement hidden")
		}
		if _, err := AdvancePayment(args, owner); err != nil {
			t.Fatal(err)
		}
		if len(keys) != 2 || keys[0] != keys[1] {
			t.Fatal("legacy ambiguous retry created second transfer authority")
		}
		client.GetTransactionFunc = func(context.Context, string) (*GetTransactionResult, error) {
			return &GetTransactionResult{Transaction: CircleTransaction{State: "COMPLETE", Id: "synthetic-processor-transfer", Blockchain: "MATIC", TxHash: "0xsynthetic-confirmed"}, ResponseBodyBytes: []byte(`{"state":"COMPLETE"}`)}, nil
		}
		result, err := AdvancePayment(args, owner)
		if err != nil || !result.Complete || len(keys) != 2 {
			t.Fatalf("inflight legacy completion failed or resent: %+v %v", result, err)
		}
		if _, err := AdvancePayment(args, owner); err != nil || len(keys) != 2 {
			t.Fatal("terminal retry sent again")
		}
	})
}

func TestProviderTransitionQueuedPostCutoffRefusesBeforeSend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff)
		sends := 0
		controllerPayoutMock(t, func(context.Context, server.Id, float64, string, string) (*CreateTransferTransactionResult, error) {
			sends++
			return &CreateTransferTransactionResult{Id: "wrong-send"}, nil
		})
		result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
		if err == nil || result.Complete || result.Canceled || sends != 0 {
			t.Fatalf("queued post-cutoff usage sent or discarded: %+v %v sends=%d", result, err, sends)
		}
		got, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || got.Completed || got.Canceled || got.PaymentRecord != nil {
			t.Fatal("refusal changed retained obligation")
		}
	})
}

func TestProviderTransitionInflightReconcilesDespiteChangedPolicy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		owner, payment := controllerPayoutFixture(t, cutoff)
		if _, err := model.GetOrCreatePaymentIdempotencyKey(owner.Ctx, payment.PaymentId); err != nil {
			t.Fatal(err)
		}
		if err := model.SetPaymentRecord(owner.Ctx, payment.PaymentId, "USDC", 9.99, "existing-processor-transfer"); err != nil {
			t.Fatal(err)
		}
		controllerPayoutSchedule(t, cutoff)
		sends := 0
		client := controllerPayoutMock(t, func(context.Context, server.Id, float64, string, string) (*CreateTransferTransactionResult, error) {
			sends++
			return nil, errors.New("must not send")
		})
		client.GetTransactionFunc = func(context.Context, string) (*GetTransactionResult, error) {
			return &GetTransactionResult{Transaction: CircleTransaction{State: "COMPLETE", Id: "existing-processor-transfer", Blockchain: "MATIC", TxHash: "0xsynthetic-confirmed"}, ResponseBodyBytes: []byte(`{"state":"COMPLETE"}`)}, nil
		}
		result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
		if err != nil || !result.Complete || sends != 0 {
			t.Fatalf("existing transfer not reconciled: %+v %v", result, err)
		}
	})
}

func TestProviderTransitionFinalCircleAdmissionAndCustomerIsolation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		owner, payment := controllerPayoutFixture(t, cutoff)
		// Force the ordering at the actual shared final-send guard: original
		// legacy eligibility, then a declaration loaded while admission waits.
		wallet := model.GetAccountWallet(owner.Ctx, *payment.WalletId)
		basis, err := model.ReserveProviderPaymentBasis(owner.Ctx, payment, wallet)
		if err != nil {
			t.Fatal(err)
		}
		args := circleTransferArguments{IdempotencyKey: basis.IdempotencyKey, Amount: 9.99, Destination: wallet.WalletAddress, Network: "MATIC"}
		if err := model.RetainProviderPaymentRequest(owner.Ctx, basis, args.Amount, args.Network); err != nil {
			t.Fatal(err)
		}
		ctx := context.WithValue(owner.Ctx, providerUsdcPaymentContextKey{}, providerPaymentSubmission{Basis: *basis, Amount: args.Amount, Network: args.Network})
		if err := requireCircleProviderPayment(ctx); err != nil {
			t.Fatal(err)
		}
		sends := 0
		_, err = circleTransferAfterAdmission(ctx, args, func(context.Context) error { controllerPayoutSchedule(t, cutoff); return nil }, func(context.Context) (*CreateTransferTransactionResult, error) {
			sends++
			return &CreateTransferTransactionResult{}, nil
		})
		if err == nil || sends != 0 {
			t.Fatal("post-wait declaration was ignored before actual send callback")
		}
		if err := requireCircleProviderPayment(owner.Ctx); err != nil {
			t.Fatalf("customer/admin transfer inherited provider gate: %v", err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := requireCircleProviderPayment(canceled); err != context.Canceled {
			t.Fatalf("current canceled send admitted: %v", err)
		}
	})
}

func TestProviderTransitionPublicStWritersBlockedButReadMirrorContinues(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		controllerPayoutSchedule(t, time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC))
		cfg := &StConfig{Profile: "mainnet", Enabled: true, ChainId: 964, Netuid: 25, NoId: 1, DeploymentId: "synthetic-transition", ContractAddress: common.HexToAddress("0x0000000000000000000000000000000000000011"), SettlementVault: common.HexToAddress("0x0000000000000000000000000000000000000022"), BlockSeconds: 12}
		oldCfg := stConfigInstance
		oldClient := stClientInstance
		SetStConfig(cfg)
		client := newStubStClient(&StEpochState{Epoch: 7, PendingEpoch: 8, EpochStartBlock: 100, TEpochBlocks: 20, CommitWindowBlocks: 4, FinalizeOffsetBlocks: 10, HeadBlock: 125, HeadBlockTime: server.NowUtc()})
		SetStClient(client)
		t.Cleanup(func() { stConfigInstance = oldCfg; stClientInstance = oldClient })
		if _, err := StCloseOperatorEpoch(ctx, 6); err == nil {
			t.Fatal("unready mainnet close admitted")
		}
		if _, _, err := StComputeEpochPayout(ctx, 6); err == nil {
			t.Fatal("unready mainnet earning artifact admitted")
		}
		if _, err := StCommitEpochRoot(ctx, 6); err == nil {
			t.Fatal("unready mainnet commit admitted")
		}
		if _, err := StDepositForEpoch(ctx, 7, nil); err == nil {
			t.Fatal("unready mainnet deposit admitted")
		}
		if _, err := StFinalizeEpochPoke(ctx, 6); err == nil {
			t.Fatal("unready mainnet finalize admitted")
		}
		if _, err := StSyncChainState(ctx); err != nil {
			t.Fatalf("read-only epoch mirror stopped: %v", err)
		}
		if client.rollCount != 0 || client.commitCount != 0 || client.depositCount != 0 || client.finalizeCount != 0 {
			t.Fatal("blocked schedule still wrote chain")
		}
		status, err := GetProviderPayoutTransitionStatus(ctx)
		if err != nil || status.NewMainnetWritesAdmitted || !status.LegacyPreCutoffPaymentsAllowed || status.ReadinessReason == "" || status.DeploymentVerified {
			t.Fatalf("misleading transition status: %+v %v", status, err)
		}
	})
}
