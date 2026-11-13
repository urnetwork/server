package controller

// The demand deposit against the database and the durable transaction engine: what a
// crash at the reservation leaves across a rollover, the sweep's close behind demand
// activity, an attempt's publish rows and the usage statement's steps.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/urfoundation/sn/v2026/amendment"
	"github.com/urfoundation/sn/v2026/demand"
	"github.com/urfoundation/sn/v2026/demand/bridge"
	"github.com/urfoundation/sn/v2026/demand/pricesheet"
	"github.com/urfoundation/sn/v2026/demand/usage"
	"github.com/urfoundation/sn/v2026/ledger"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urfoundation/sn/v2026/ss58"
	"github.com/urfoundation/sn/v2026/stabi"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/startifact"
)

// stDemandDbSetup prepares a database test's database. The migration list creates the
// tables, so it does nothing here. Until the branch merges, a harness overlay creates
// them in the test's scratch database instead.
var stDemandDbSetup = func(context.Context) {}

var errStDemandProcessStopped = errors.New("process stopped between the reservation and the signature")

// Stops the next send between its reservation and its signature.
func stDemandStopAtSignature(client *CoreStClient) {
	client.transactionSigner = func(context.Context, *types.Transaction, types.Signer, *ecdsa.PrivateKey) (*types.Transaction, error) {
		return nil, errStDemandProcessStopped
	}
}

func stDemandEnoughGas(policy *server.StOperatorGasPolicy) {
	policy.MaximumLifetimeLiabilityWei, policy.MaximumLifetimeAttempts = "100000000", 100
}

// Epoch 7's staging transfer, reserved and never signed.
func stDemandStrandedStaging(tb testing.TB, client *CoreStClient, epoch uint64) (*model.StTransactionIntent, *model.StTransactionGasReservation) {
	tb.Helper()
	ctx := context.Background()
	stDemandStopAtSignature(client)
	if _, err := client.stageDepositPrincipal(ctx, epoch, big.NewInt(100)); !errors.Is(err, errStDemandProcessStopped) {
		tb.Fatalf("staging: %v", err)
	}
	client.transactionSigner = nil
	logical, err := stTransactionLogicalKey(client.cfg, fmt.Sprintf("deposit-fund:%d:%d:3", epoch, client.cfg.NoId))
	if err != nil {
		tb.Fatal(err)
	}
	intent := model.GetStTransactionIntent(ctx, logical)
	if intent == nil || intent.AttemptCount != 0 || intent.Status != model.StTxPrepared {
		tb.Fatalf("staging intent %+v", intent)
	}
	pending, err := model.GetPendingStTransactionGasReservation(ctx, intent.IntentId)
	if err != nil || pending == nil || pending.Kind != model.StTxAttemptExecution || pending.SignedTxHash != nil {
		tb.Fatalf("staging reservation %+v, %v", pending, err)
	}
	return intent, pending
}

// A crash at the exact point between a staging transfer's reservation and its
// signature, with no run before the epoch rolls, used to stop every later send of the
// deposit key for good. Now the next send cancels it at its nonce without moving any
// stake, and the next epoch's deposit lands behind it. This signer has already sent
// from outside the engine, as mainnet's has, so its first nonce here is 1.
func TestStOperatorGasPendingStagingOfARolledEpochIsCancelled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.accountNonce = 1
		policy, _, _ := stGasFixturePolicy(tb, client, stDemandEnoughGas)
		stranded, _ := stDemandStrandedStaging(tb, client, 7)
		if stranded.Nonce != 1 {
			tb.Fatalf("the signer's first intent took nonce %d", stranded.Nonce)
		}
		stDepositCustodyExpire(fixture)
		restarted := stGasFixtureRestart(client)

		stage, err := restarted.stageDepositPrincipal(ctx, 8, big.NewInt(100))
		if err != nil || stage == "" {
			tb.Fatalf("the next epoch's staging: %q, %v", stage, err)
		}
		credit, err := restarted.DepositCredit(ctx, 8, 1, big.NewInt(100))
		if err != nil || credit == "" {
			tb.Fatalf("the next epoch's deposit: %q, %v", credit, err)
		}
		sent, staged, source, deposited, depositNonce := stGasFixtureChain(fixture)
		if len(sent) != 3 {
			tb.Fatalf("%d sent", len(sent))
		}
		signer := crypto.PubkeyToAddress(client.cfg.DepositKey.PublicKey)
		cancel := sent[0]
		if cancel.Nonce() != 1 || *cancel.To() != signer || len(cancel.Data()) != 0 || cancel.Gas() != 21_000 || cancel.Value().Sign() != 0 {
			tb.Fatalf("the cancellation: nonce %d to %s gas %d data %x", cancel.Nonce(), cancel.To(), cancel.Gas(), cancel.Data())
		}
		if sent[1].Nonce() != 2 || *sent[1].To() != stStakingPrecompileAddress || sent[2].Nonce() != 3 || *sent[2].To() != client.cfg.ContractAddress {
			tb.Fatalf("after it: nonce %d to %s, nonce %d to %s", sent[1].Nonce(), sent[1].To(), sent[2].Nonce(), sent[2].To())
		}
		// Only the next epoch's transfer moved stake: 103 once, not twice.
		if source != 9_897 || staged != 0 || deposited != 100 || depositNonce != 4 {
			tb.Fatalf("%d source, %d staged, %d deposited, deposit nonce %d", source, staged, deposited, depositNonce)
		}
		retired := model.GetStTransactionIntent(ctx, stranded.LogicalKey)
		if retired.Status != model.StTxCanceled || !bytes.Equal(retired.Calldata, stranded.Calldata) || retired.AttemptCount != 1 {
			tb.Fatalf("the retired intent %+v", retired)
		}
		attempts := model.GetStTransactionAttempts(ctx, stranded.IntentId)
		if len(attempts) != 1 || attempts[0].Kind != model.StTxAttemptCancellation {
			tb.Fatalf("the retired intent's attempts %+v", attempts)
		}
		// Each new send's 60,000 gas and the cancellation's 21,000 at 10 wei.
		stGasFixtureSnapshot(tb, policy, "1410000", 3)
	})
}

// With no gas policy, as on a testnet, an intent holds its nonce with nothing reserved at
// all. The same transfer of a rolled epoch is cancelled the same way, and one of the
// current epoch still goes out.
func TestStDepositStagingOfARolledEpochIsCancelledWithoutAGasPolicy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		data, err := stPackTransferStake(ss58.EvmMirrorPubkey(client.cfg.ContractAddress), client.cfg.DepositHotkey, client.cfg.Netuid, big.NewInt(103))
		if err != nil {
			tb.Fatal(err)
		}
		logical, err := stTransactionLogicalKey(client.cfg, "deposit-fund:7:1:3")
		if err != nil {
			tb.Fatal(err)
		}
		prior := model.ReserveStTransactionIntent(ctx, logical, client.cfg.Profile, client.cfg.DeploymentId, client.cfg.DeploymentKey(), client.cfg.ChainId, fmt.Sprintf("0x%x", client.cfg.GenesisHash), strings.ToLower(crypto.PubkeyToAddress(client.cfg.DepositKey.PublicKey).Hex()), strings.ToLower(stStakingPrecompileAddress.Hex()), strings.ToLower(crypto.Keccak256Hash(data).Hex()), data, 7)
		stDepositCustodyExpire(fixture)
		if _, err := client.stageDepositPrincipal(ctx, 8, big.NewInt(100)); err != nil {
			tb.Fatal(err)
		}
		sent, staged, source, _, _ := stGasFixtureChain(fixture)
		if len(sent) != 2 || sent[0].Nonce() != 7 || sent[0].Gas() != 21_000 || len(sent[0].Data()) != 0 || sent[1].Nonce() != 8 || source != 9_897 || staged != 102 {
			tb.Fatalf("%d sent, %d staged, %d source", len(sent), staged, source)
		}
		if retired := model.GetStTransactionIntent(ctx, prior.LogicalKey); retired.Status != model.StTxCanceled {
			tb.Fatalf("%+v", retired)
		}
	})
}

// Reservation and signature apart inside the epoch is the ordinary case: the next send
// signs exactly what was reserved, and nothing is cancelled.
func TestStOperatorGasPendingStagingInItsEpochIsSignedAsReserved(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		stGasFixturePolicy(tb, client, stDemandEnoughGas)
		stranded, pending := stDemandStrandedStaging(tb, client, 7)
		if _, err := stGasFixtureRestart(client).stageDepositPrincipal(ctx, 7, big.NewInt(100)); err != nil {
			tb.Fatal(err)
		}
		sent, staged, source, _, _ := stGasFixtureChain(fixture)
		signer := types.LatestSignerForChainID(new(big.Int).SetUint64(client.cfg.ChainId))
		if len(sent) != 1 || sent[0].Nonce() != stranded.Nonce || !bytes.Equal(sent[0].Data(), stranded.Calldata) || strings.ToLower(signer.Hash(sent[0]).Hex()) != pending.SigningHash || staged != 102 || source != 9_897 {
			tb.Fatalf("%d sent, %d staged, %d source", len(sent), staged, source)
		}
		requireLedgerV2Intent(tb, ctx, client.cfg, "deposit-fund:7:1:3", model.StTxFinalized, sent[0])
	})
}

// An intent with an attempt is never touched, whatever its epoch: its bytes may already
// be out there. Its stored bytes are sent again, as they always were, and land.
func TestStOperatorGasStagingWithAnAttemptIsNeverRetired(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		stGasFixturePolicy(tb, client, stDemandEnoughGas)
		fixture.stateLock.Lock()
		fixture.balance = big.NewInt(1)
		fixture.stateLock.Unlock()
		stopCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := client.stageDepositPrincipal(stopCtx, 7, big.NewInt(100))
		cancel()
		logical, _ := stTransactionLogicalKey(client.cfg, "deposit-fund:7:1:3")
		signed := model.GetStTransactionIntent(ctx, logical)
		if err == nil || signed == nil || signed.AttemptCount != 1 {
			tb.Fatalf("%v, %+v", err, signed)
		}
		stDepositCustodyExpire(fixture)
		fixture.stateLock.Lock()
		fixture.balance = nil
		fixture.stateLock.Unlock()
		if _, err := stGasFixtureRestart(client).stageDepositPrincipal(ctx, 8, big.NewInt(100)); err != nil {
			tb.Fatal(err)
		}
		sent, _, source, _, _ := stGasFixtureChain(fixture)
		if len(sent) == 0 || sent[0].Nonce() != signed.Nonce || !bytes.Equal(sent[0].Data(), signed.Calldata) || source >= 10_000 {
			tb.Fatalf("%d sent, %d source", len(sent), source)
		}
		attempts := model.GetStTransactionAttempts(ctx, signed.IntentId)
		if len(attempts) == 0 || attempts[len(attempts)-1].Kind != model.StTxAttemptExecution {
			tb.Fatalf("attempts %+v", attempts)
		}
		for _, attempt := range attempts {
			if attempt.Kind != model.StTxAttemptExecution {
				tb.Fatalf("the signed staging was cancelled: %+v", attempt)
			}
		}
		if latest := model.GetStTransactionIntent(ctx, logical); latest.Status != model.StTxFinalized {
			tb.Fatalf("%+v", latest)
		}
	})
}

// The sequence the settle before sizing exists for. A staging transfer signed in epoch
// 7 is still in flight when epoch 8's attempt starts, so the wallet still shows the alpha
// it is about to move and the staged position does not show it yet. Read that way, the
// shortfall is the whole deposit, and the funder, which settles as its first act, would
// see alpha fall under that stale request and buy all of it again. Settled first, the
// funder is asked only for what the transfer leaves short.
func TestStDemandInFlightStagingSettlesBeforeTheShortfallIsRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		stGasFixturePolicy(tb, client, stDemandEnoughGas)
		fixture.stateLock.Lock()
		fixture.source.SetInt64(103)
		fixture.balance = big.NewInt(1)
		fixture.stateLock.Unlock()
		stopCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := client.stageDepositPrincipal(stopCtx, 7, big.NewInt(100))
		cancel()
		logical, _ := stTransactionLogicalKey(client.cfg, "deposit-fund:7:1:3")
		signed := model.GetStTransactionIntent(ctx, logical)
		if err == nil || signed == nil || signed.AttemptCount != 1 {
			tb.Fatalf("%v, %+v", err, signed)
		}
		stDepositCustodyExpire(fixture)
		fixture.stateLock.Lock()
		fixture.balance = nil
		fixture.stateLock.Unlock()
		restarted := stGasFixtureRestart(client)
		useStDemandWallets(tb, restarted.cfg)

		// The hazard: without the settle, the funder is asked for the whole deposit.
		unsettled := &stDemandFakePayer{result: &demand.Result{Funded: true}}
		restarted.demandPayer = unsettled
		restarted.demandSettle = func(context.Context) error { return nil }
		if outcome := restarted.fundDemandDeposit(ctx, 8, big.NewInt(150)); outcome != nil || len(unsettled.requests) != 1 || unsettled.requests[0].NeedRao.Int64() != 153 {
			tb.Fatalf("%+v, %+v", outcome, unsettled.requests)
		}
		if sent, staged, _, _, _ := stGasFixtureChain(fixture); len(sent) != 0 || staged != 0 {
			tb.Fatalf("%d sent, %d staged", len(sent), staged)
		}

		// With the real settle, the transfer lands first and the funder is asked for the rest.
		payer := &stDemandFakePayer{result: &demand.Result{Funded: true}}
		restarted.demandPayer = payer
		restarted.demandSettle = nil
		if outcome := restarted.fundDemandDeposit(ctx, 8, big.NewInt(150)); outcome != nil {
			tb.Fatalf("%+v", outcome)
		}
		// 150 and the reserve's 2, less the 102 the transfer staged, and the next transfer's 1.
		if len(payer.requests) != 1 || payer.requests[0].Epoch != 8 || payer.requests[0].NeedRao.Int64() != 51 {
			tb.Fatalf("%+v", payer.requests)
		}
		sent, staged, source, _, _ := stGasFixtureChain(fixture)
		if !bytes.Equal(sent[len(sent)-1].Data(), signed.Calldata) || staged != 102 || source != 0 {
			tb.Fatalf("%d sent, %d staged, %d source", len(sent), staged, source)
		}
		if latest := model.GetStTransactionIntent(ctx, logical); latest.Status != model.StTxFinalized {
			tb.Fatalf("%+v", latest)
		}

		// A wallet that transfer funded in full asks the funder for nothing.
		enough := &stDemandFakePayer{result: &demand.Result{Funded: true}}
		restarted.demandPayer = enough
		if outcome := restarted.fundDemandDeposit(ctx, 8, big.NewInt(100)); outcome != nil || len(enough.requests) != 0 {
			tb.Fatalf("%+v, %+v", outcome, enough.requests)
		}
	})
}

// A swap that needs a person is the funder's own event, raised as an alert too. An alert
// of the server's own writes its event where the monitor reads it.
func TestStDemandAlertsReachTheEventTable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		cfg := stDemandAttemptConfig()
		lines := []string{}
		previousSink := stLedgerV2AlertSink
		stLedgerV2AlertSink = func(name, subject, message string) { lines = append(lines, name+"|"+subject+"|"+message) }
		tb.Cleanup(func() { stLedgerV2AlertSink = previousSink })
		stDemandAlerted.Clear()
		tb.Cleanup(func() { stDemandAlerted.Clear() })
		store := &stDemandStore{deploymentKey: cfg.DeploymentKey()}
		if err := store.Record(ctx, demand.Event{Time: server.NowUtc(), Epoch: 7, Kind: "swapped", Data: map[string]any{"usdc_micro": 5}}); err != nil {
			tb.Fatal(err)
		}
		if len(lines) != 0 {
			tb.Fatalf("%q", lines)
		}
		if err := store.Record(ctx, demand.Event{Time: server.NowUtc(), Epoch: 7, Kind: stDemandEventSwapNeedsPerson, Data: map[string]any{"signature": "sig", "endpoint": "https://rpc.example/SECRET"}}); err != nil {
			tb.Fatal(err)
		}
		if len(lines) != 1 || !strings.HasPrefix(lines[0], "demand_swap_needs_person|epoch 7|") || !strings.Contains(lines[0], "sig") || strings.Contains(lines[0], "SECRET") {
			tb.Fatalf("%q", lines)
		}
		stDemandFunderLog(cfg)("[demand]epoch %d ALERT swap %s: no TAO has come", 7, "sig")
		stDemandAlert(cfg, 8, stDemandEventHotkeyNotReady, "demand: the deposit hotkey cannot take stake")
		kinds := map[string]uint64{}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT kind, epoch FROM st_demand_event WHERE deployment_key = $1`, string(cfg.DeploymentKey()))
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var kind string
					var epoch int64
					server.Raise(rows.Scan(&kind, &epoch))
					kinds[kind] = uint64(epoch)
				}
			})
		})
		for kind, epoch := range map[string]uint64{"swapped": 7, stDemandEventSwapNeedsPerson: 7, stDemandEventFunderAlert: 7, stDemandEventHotkeyNotReady: 8} {
			if kinds[kind] != epoch {
				tb.Fatalf("%v", kinds)
			}
		}
	})
}

// The one change to a reservation refuses everything but a never-signed execution, and
// a racing signer that comes to record afterwards is refused before any broadcast.
func TestStTransactionGasRetireRefusesAnythingElse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		policy, authority, _ := stGasFixturePolicy(tb, client, stDemandEnoughGas)
		stranded, pending := stDemandStrandedStaging(tb, client, 7)
		from := crypto.PubkeyToAddress(client.cfg.DepositKey.PublicKey)
		cancellation := func(nonce uint64, to common.Address, data []byte, gas uint64) *types.Transaction {
			return types.NewTx(&types.LegacyTx{Nonce: nonce, To: &to, Gas: gas, GasPrice: big.NewInt(10), Value: new(big.Int), Data: data})
		}
		for name, unsigned := range map[string]*types.Transaction{
			"another nonce":   cancellation(stranded.Nonce+1, from, nil, 21_000),
			"another account": cancellation(stranded.Nonce, stStakingPrecompileAddress, nil, 21_000),
			"with calldata":   cancellation(stranded.Nonce, from, []byte{1}, 21_000),
			"more gas":        cancellation(stranded.Nonce, from, nil, 30_000),
		} {
			if _, err := model.RetireStTransactionGasExecution(ctx, policy, authority, stranded.IntentId, unsigned); err == nil {
				tb.Fatalf("%s was accepted", name)
			}
		}
		if unchanged, err := model.GetPendingStTransactionGasReservation(ctx, stranded.IntentId); err != nil || unchanged.Kind != model.StTxAttemptExecution || unchanged.SigningHash != pending.SigningHash {
			tb.Fatalf("a refusal changed the reservation: %+v, %v", unchanged, err)
		}
		good := cancellation(stranded.Nonce, from, nil, 21_000)
		retired, err := model.RetireStTransactionGasExecution(ctx, policy, authority, stranded.IntentId, good)
		if err != nil || retired.Kind != model.StTxAttemptCancellation || retired.MaximumLiabilityWei != "210000" {
			tb.Fatalf("%+v, %v", retired, err)
		}
		// A crash after the change retries without changing anything.
		again, err := model.RetireStTransactionGasExecution(ctx, policy, authority, stranded.IntentId, cancellation(stranded.Nonce, from, nil, 21_000))
		if err != nil || !again.Reused || again.SigningHash != retired.SigningHash {
			tb.Fatalf("%+v, %v", again, err)
		}
		// A signer that read the old reservation before the change.
		var unsigned types.Transaction
		if err := unsigned.UnmarshalBinary(pending.UnsignedTransaction); err != nil {
			tb.Fatal(err)
		}
		raced, err := types.SignTx(&unsigned, types.LatestSignerForChainID(new(big.Int).SetUint64(client.cfg.ChainId)), client.cfg.DepositKey)
		if err != nil {
			tb.Fatal(err)
		}
		raw, err := raced.MarshalBinary()
		if err != nil {
			tb.Fatal(err)
		}
		price := "10"
		func() {
			defer func() {
				if recover() == nil {
					tb.Fatal("a signature of the old bytes was recorded")
				}
			}()
			model.AddStTransactionAttempt(ctx, &model.StTransactionAttempt{IntentId: stranded.IntentId, Attempt: 1, Kind: model.StTxAttemptExecution, TxHash: strings.ToLower(raced.Hash().Hex()), RawTransaction: raw, GasLimit: raced.Gas(), GasPrice: &price})
		}()
		fixture.stateLock.Lock()
		sentCount := len(fixture.sent)
		fixture.stateLock.Unlock()
		if sentCount != 0 {
			tb.Fatalf("%d sent", sentCount)
		}
		// The model refuses anything but a staging transfer by itself, whatever the
		// caller has proved: a deposit call, and the conversion's stake on the same
		// precompile, each reserved and never signed.
		for noId, reserve := range map[uint64]func(*CoreStClient, *stDepositCustodyRpc) error{
			2: func(other *CoreStClient, otherFixture *stDepositCustodyRpc) error {
				otherFixture.stateLock.Lock()
				otherFixture.staged.SetInt64(102)
				otherFixture.stateLock.Unlock()
				_, err := other.DepositCredit(ctx, 7, 2, big.NewInt(100))
				return err
			},
			3: func(other *CoreStClient, _ *stDepositCustodyRpc) error {
				wallets := useStDemandWallets(tb, other.cfg)
				_, err := (&stDemandSender{core: other, key: wallets.AlphaKey()}).SendPrepared(ctx, func(context.Context) (string, common.Address, []byte, error) {
					return "demand:stake:7:5", stStakingPrecompileAddress, stDemandPackStake(other.cfg.DepositHotkey, 5, 7, other.cfg.Netuid), nil
				})
				return err
			},
		} {
			other, _, otherFixture := newStDepositCustodyFixture(tb, noId)
			otherPolicy, otherAuthority, _ := stGasFixturePolicy(tb, other, stDemandEnoughGas)
			stDemandStopAtSignature(other)
			if err := reserve(other, otherFixture); !errors.Is(err, errStDemandProcessStopped) {
				tb.Fatalf("operator %d: %v", noId, err)
			}
			otherFrom := crypto.PubkeyToAddress(other.cfg.DepositKey.PublicKey)
			var reserved *model.StTransactionIntent
			server.Db(ctx, func(conn server.PgConn) {
				var intentId server.Id
				server.Raise(conn.QueryRow(ctx, `SELECT intent_id FROM st_transaction_intent WHERE from_address = $1 ORDER BY nonce DESC LIMIT 1`, strings.ToLower(otherFrom.Hex())).Scan(&intentId))
				reserved = &model.StTransactionIntent{IntentId: intentId}
			})
			before, err := model.GetPendingStTransactionGasReservation(ctx, reserved.IntentId)
			if err != nil || before == nil || before.Kind != model.StTxAttemptExecution {
				tb.Fatalf("operator %d: %+v, %v", noId, before, err)
			}
			var execution types.Transaction
			if err := execution.UnmarshalBinary(before.UnsignedTransaction); err != nil {
				tb.Fatal(err)
			}
			if _, err := model.RetireStTransactionGasExecution(ctx, otherPolicy, otherAuthority, reserved.IntentId, cancellation(execution.Nonce(), otherFrom, nil, 21_000)); err == nil || !strings.Contains(err.Error(), "staging transfer") {
				tb.Fatalf("operator %d: a non-staging intent was retired: %v", noId, err)
			}
			if after, err := model.GetPendingStTransactionGasReservation(ctx, reserved.IntentId); err != nil || after.Kind != model.StTxAttemptExecution || after.SigningHash != before.SigningHash {
				tb.Fatalf("operator %d: the refusal changed the reservation: %+v, %v", noId, after, err)
			}
		}

		// Once the epoch rolls the next send signs the cancellation as reserved, and
		// with an attempt recorded nothing is retired again.
		stDepositCustodyExpire(fixture)
		if _, err := stGasFixtureRestart(client).stageDepositPrincipal(ctx, 8, big.NewInt(100)); err != nil {
			tb.Fatal(err)
		}
		if latest := model.GetStTransactionIntent(ctx, stranded.LogicalKey); latest.AttemptCount != 1 || latest.Status != model.StTxCanceled {
			tb.Fatalf("%+v", latest)
		}
		if _, err := model.RetireStTransactionGasExecution(ctx, policy, authority, stranded.IntentId, good); err == nil {
			tb.Fatal("an intent with an attempt was retired")
		}
	})
}

// No new staging transfer is reserved so close to the epoch's end that it and its
// deposit could not finish. An amount already staged can still be deposited, up to the
// deadline itself.
func TestStDepositStagingStopsBeforeItsDeadline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		if margin := stDepositStagingMarginBlocks(&StConfig{BlockSeconds: 12}); margin != 150 {
			tb.Fatalf("margin %d", margin)
		}
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		at := func(block uint64) {
			fixture.stateLock.Lock()
			defer fixture.stateLock.Unlock()
			fixture.boundary.Block, fixture.boundary.Hash = block, [32]byte(stTransactionReconcileBlockHash(block))
		}
		logical, _ := stTransactionLogicalKey(client.cfg, "deposit-fund:7:1:3")
		// The deadline is block 1999, so the last block a new staging may start at is 1848.
		at(1_849)
		if _, err := client.stageDepositPrincipal(ctx, 7, big.NewInt(100)); err == nil || !strings.Contains(err.Error(), "staging stops") || model.GetStTransactionIntent(ctx, logical) != nil {
			tb.Fatalf("staging inside the margin: %v", err)
		}
		fixture.stateLock.Lock()
		fixture.staged.SetInt64(102)
		fixture.stateLock.Unlock()
		// Nothing left to stage: nothing is refused, and the deposit goes out.
		if hash, err := client.stageDepositPrincipal(ctx, 7, big.NewInt(100)); err != nil || hash != "" {
			tb.Fatalf("%q, %v", hash, err)
		}
		at(1_998)
		if hash, err := client.DepositCredit(ctx, 7, 1, big.NewInt(100)); err != nil || hash == "" {
			tb.Fatalf("the deposit in the epoch's last sendable block: %q, %v", hash, err)
		}
		// Past that it is not sent at all.
		other, _, otherFixture := newStDepositCustodyFixture(tb, 2)
		otherFixture.stateLock.Lock()
		otherFixture.staged.SetInt64(102)
		otherFixture.boundary.Block, otherFixture.boundary.Hash = 1_999, [32]byte(stTransactionReconcileBlockHash(1_999))
		otherFixture.stateLock.Unlock()
		if _, err := other.DepositCredit(ctx, 7, 2, big.NewInt(100)); err == nil || !strings.Contains(err.Error(), "nothing is sent") {
			tb.Fatalf("a deposit at its deadline: %v", err)
		}
		if len(otherFixture.sent) != 0 {
			tb.Fatalf("%d sent", len(otherFixture.sent))
		}
		at(1_848)
	})
}

// The funder's unwrap and stake, reserved and never signed, sign as reserved across a
// rollover, demand.yml or no demand.yml. Neither depends on the epoch, and neither can
// move value out of the wallet. The next epoch's deposit lands behind them.
func TestStDemandIntentsCrossARollover(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		for index, reserved := range []struct {
			name      string
			operation string
			to        common.Address
			data      func(client *CoreStClient) []byte
		}{
			{"unwrap", "demand:unwrap:7:5", bridge.WTAO, func(*CoreStClient) []byte { return stDemandPackWithdraw(5) }},
			{"stake", "demand:stake:7:5", stStakingPrecompileAddress, func(client *CoreStClient) []byte {
				return stDemandPackStake(client.cfg.DepositHotkey, 5, 7, client.cfg.Netuid)
			}},
		} {
			noId := uint64(index + 1)
			client, _, fixture := newStDepositCustodyFixture(tb, noId)
			stGasFixturePolicy(tb, client, stDemandEnoughGas)
			wallets := useStDemandWallets(tb, client.cfg)
			sender := &stDemandSender{core: client, key: wallets.AlphaKey()}
			stDemandStopAtSignature(client)
			if _, err := sender.SendPrepared(ctx, func(context.Context) (string, common.Address, []byte, error) {
				return reserved.operation, reserved.to, reserved.data(client), nil
			}); !errors.Is(err, errStDemandProcessStopped) {
				tb.Fatalf("%s: %v", reserved.name, err)
			}
			client.transactionSigner = nil
			stDepositCustodyExpire(fixture)
			withoutStDemandWallets(tb)
			fixture.stateLock.Lock()
			fixture.depositNonce.SetInt64(3)
			fixture.stateLock.Unlock()
			restarted := stGasFixtureRestart(client)
			if _, err := restarted.stageDepositPrincipal(ctx, 8, big.NewInt(100)); err != nil {
				tb.Fatalf("%s: staging behind it: %v", reserved.name, err)
			}
			if _, err := restarted.DepositCredit(ctx, 8, noId, big.NewInt(100)); err != nil {
				tb.Fatalf("%s: deposit behind it: %v", reserved.name, err)
			}
			sent, _, _, deposited, _ := stGasFixtureChain(fixture)
			if len(sent) != 3 || sent[0].Nonce() != 7 || *sent[0].To() != reserved.to || !bytes.Equal(sent[0].Data(), reserved.data(client)) || deposited != 100 {
				tb.Fatalf("%s: %d sent, %d deposited", reserved.name, len(sent), deposited)
			}
		}
	})
}

// A chain nonce above the intent rows, from a transaction sent outside the engine as
// mainnet's association was, is nothing to the sender: its first intent takes the chain's
// next nonce.
func TestStDepositSignerWithAnEarlierOutsideTransaction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.accountNonce = 1
		stGasFixturePolicy(tb, client, stDemandEnoughGas)
		if _, err := client.stageDepositPrincipal(ctx, 7, big.NewInt(100)); err != nil {
			tb.Fatal(err)
		}
		if _, err := client.DepositCredit(ctx, 7, 1, big.NewInt(100)); err != nil {
			tb.Fatal(err)
		}
		sent, _, _, deposited, _ := stGasFixtureChain(fixture)
		if len(sent) != 2 || sent[0].Nonce() != 1 || sent[1].Nonce() != 2 || deposited != 100 {
			tb.Fatalf("%d sent, %d deposited", len(sent), deposited)
		}
	})
}

// The sweep's close is the deposit key's send that has no second chance. With demand.yml
// absent, broken or present, and with the sheet site down, it gets the key's next nonce
// and its whole gas floor, and reads no sheet. With a retained demand intent ahead of it
// and demand.yml gone, the intent goes first and the close takes the nonce after.
func TestStV1SweepCloseIsUnaffectedByDemandDeposits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		fixture := newLedgerV2RPC(tb)
		client, cfg := newLedgerV1RPCClient(tb, fixture)
		client.vault = stabi.NewSTSettlementVault()
		ledgerV2GasPolicy(tb, client, nil)
		cfg.DepositHotkey, cfg.Netuid = [32]byte{7, 7}, 25
		sheetKey := stDemandSheetKey(tb, 0x51)
		site, _ := useStDemandSheet(tb, sheetKey)
		site.down()
		closeOf := func(epoch uint64) []byte {
			return client.coordinator.PackCloseOperatorEpoch(stBig(epoch), big.NewInt(1))
		}
		check := func(name string, epoch uint64, index int, nonce uint64) {
			tb.Helper()
			cfg.V1SweepEpoch = epoch
			fixture.estimates[string(closeOf(epoch))] = 361_258
			hash, err := client.V1SweepClose(ctx, epoch, 1)
			if err != nil {
				tb.Fatalf("%s: %v", name, err)
			}
			if len(fixture.sent) <= index {
				tb.Fatalf("%s: %d sent", name, len(fixture.sent))
			}
			tx := fixture.sent[index]
			if tx.Gas() != stV1CloseGasFloor || tx.Nonce() != nonce || *tx.To() != cfg.ContractAddress || !bytes.Equal(tx.Data(), closeOf(epoch)) || !strings.EqualFold(hash, tx.Hash().Hex()) {
				tb.Fatalf("%s: nonce %d gas %d to %s", name, tx.Nonce(), tx.Gas(), tx.To())
			}
			signer, err := types.Sender(types.LatestSignerForChainID(new(big.Int).SetUint64(cfg.ChainId)), tx)
			if err != nil || signer != crypto.PubkeyToAddress(cfg.DepositKey.PublicKey) {
				tb.Fatalf("%s: signed by %s, %v", name, signer, err)
			}
		}
		withoutStDemandWallets(tb)
		check("demand.yml absent", 7, 0, 7)
		stDemandConfig = func() (*demand.Config, error) { return nil, errors.New("demand: demand.yml does not parse") }
		check("demand.yml broken", 8, 1, 8)
		wallets := useStDemandWallets(tb, cfg)
		check("demand.yml present", 9, 2, 9)
		if fetches := site.fetches.Load(); fetches != 0 {
			tb.Fatalf("the close read the sheet %d times", fetches)
		}
		// A reserved stake, retained when demand.yml goes away.
		stDemandStopAtSignature(client)
		stake := stDemandPackStake(cfg.DepositHotkey, 5, 7, cfg.Netuid)
		sender := &stDemandSender{core: client, key: wallets.AlphaKey()}
		if _, err := sender.SendPrepared(ctx, func(context.Context) (string, common.Address, []byte, error) {
			return "demand:stake:7:5", stStakingPrecompileAddress, stake, nil
		}); !errors.Is(err, errStDemandProcessStopped) {
			tb.Fatal(err)
		}
		client.transactionSigner = nil
		withoutStDemandWallets(tb)
		// Epoch 7 to 9 are closed already, and the fixture answers one more.
		parsed, err := stabi.STCoordinatorMetaData.ParseABI()
		if err != nil {
			tb.Fatal(err)
		}
		policy, err := parsed.Methods["policyAt"].Outputs.Pack(stabi.STCoordinatorPolicySnapshot{EpochBlocks: 50_400, RootCommitWindowBlocks: 1_200, CloseGraceBlocks: 120, EpochDepositCapRao: big.NewInt(0), CampaignDepositCapRao: big.NewInt(0)})
		if err != nil {
			tb.Fatal(err)
		}
		end, err := parsed.Methods["epochEndBlock"].Outputs.Pack(big.NewInt(50))
		if err != nil {
			tb.Fatal(err)
		}
		fixture.results[string(client.coordinator.PackPolicyAt(big.NewInt(10)))] = policy
		fixture.results[string(client.coordinator.PackEpochEndBlock(big.NewInt(10)))] = end
		check("behind a retained demand intent", 10, 4, 11)
		stakeTx := fixture.sent[3]
		if stakeTx.Nonce() != 10 || !bytes.Equal(stakeTx.Data(), stake) || stakeTx.Gas() != stV1DepositStagingGasFloor {
			tb.Fatalf("the retained stake: nonce %d gas %d", stakeTx.Nonce(), stakeTx.Gas())
		}
	})
}

// A process stopped between a staging retirement's turn and its cancellation's attempt
// record, then a run that cannot read custody at all: the cancellation is signed from
// the turned reservation all the same, and the sweep's close goes out behind it at the
// next nonce. The fixture serves no custody read, so any would fail the run.
func TestStV1SweepCloseGoesAfterATurnedStagingCancellation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		fixture := newLedgerV2RPC(tb)
		client, cfg := newLedgerV1RPCClient(tb, fixture)
		client.vault = stabi.NewSTSettlementVault()
		ledgerV2GasPolicy(tb, client, nil)
		cfg.DepositHotkey, cfg.Netuid = [32]byte{7, 7}, 25
		var rpcClient *ethclient.Client
		for _, candidate := range client.clients {
			rpcClient = candidate
		}
		from := crypto.PubkeyToAddress(cfg.DepositKey.PublicKey)
		gasPolicy, gasAuthority, err := client.operatorGasAdmission(ctx, from)
		if err != nil || gasPolicy == nil {
			tb.Fatal(gasPolicy, err)
		}
		chainId := new(big.Int).SetUint64(cfg.ChainId)
		// Epoch 6's staging transfer at the account's next nonce, reserved and never signed.
		calldata, err := stPackTransferStake(ss58.EvmMirrorPubkey(cfg.ContractAddress), cfg.DepositHotkey, cfg.Netuid, big.NewInt(103))
		if err != nil {
			tb.Fatal(err)
		}
		logical, err := stTransactionLogicalKey(cfg, "deposit-fund:6:1:3")
		if err != nil {
			tb.Fatal(err)
		}
		genesis, fromAddress, toAddress := hexutil.Encode(cfg.GenesisHash[:]), strings.ToLower(from.Hex()), strings.ToLower(stStakingPrecompileAddress.Hex())
		calldataHash := strings.ToLower(crypto.Keccak256Hash(calldata).Hex())
		planned := &model.StTransactionIntent{LogicalKey: logical, Profile: cfg.Profile, DeploymentId: cfg.DeploymentId, DeploymentKey: cfg.DeploymentKey(), ChainId: cfg.ChainId,
			GenesisHash: genesis, FromAddress: fromAddress, ToAddress: toAddress, CalldataHash: calldataHash, Calldata: calldata}
		execution, err := client.transactionEnvelope(ctx, rpcClient, planned, from, stStakingPrecompileAddress, calldata, 0, nil, model.StTxAttemptExecution, gasPolicy)
		if err != nil {
			tb.Fatal(err)
		}
		intent, err := model.ReserveStTransactionIntentGas(ctx, gasPolicy, gasAuthority, logical, cfg.Profile, cfg.DeploymentId, cfg.DeploymentKey(), cfg.ChainId, genesis, fromAddress, toAddress, calldataHash, calldata, 7,
			func(nonce uint64) *types.Transaction {
				return execution.transaction(chainId, nonce, stStakingPrecompileAddress, calldata)
			})
		if err != nil {
			tb.Fatal(err)
		}
		// The retirement's turn, made durable before the process stopped short of the
		// cancellation's attempt record.
		to, cancelData, gasLimit, err := stTransactionAttemptPayload(intent, from, model.StTxAttemptCancellation)
		if err != nil {
			tb.Fatal(err)
		}
		cancellation, err := client.transactionEnvelope(ctx, rpcClient, intent, from, to, cancelData, gasLimit, nil, model.StTxAttemptCancellation, gasPolicy)
		if err != nil {
			tb.Fatal(err)
		}
		if _, err := model.RetireStTransactionGasExecution(ctx, gasPolicy, gasAuthority, intent.IntentId, cancellation.transaction(chainId, intent.Nonce, to, cancelData)); err != nil {
			tb.Fatal(err)
		}
		if attempts := model.GetStTransactionAttempts(ctx, intent.IntentId); len(attempts) != 0 {
			tb.Fatalf("%d attempts", len(attempts))
		}

		closeData := client.coordinator.PackCloseOperatorEpoch(stBig(7), big.NewInt(1))
		cfg.V1SweepEpoch = 7
		fixture.estimates[string(closeData)] = 361_258
		hash, err := client.V1SweepClose(ctx, 7, 1)
		if err != nil {
			tb.Fatal(err)
		}
		if len(fixture.sent) != 2 {
			tb.Fatalf("%d sent", len(fixture.sent))
		}
		cancel, closed := fixture.sent[0], fixture.sent[1]
		if cancel.Nonce() != intent.Nonce || *cancel.To() != from || len(cancel.Data()) != 0 || cancel.Gas() != 21_000 || cancel.Value().Sign() != 0 {
			tb.Fatalf("the cancellation: nonce %d to %s gas %d", cancel.Nonce(), cancel.To(), cancel.Gas())
		}
		if closed.Nonce() != intent.Nonce+1 || *closed.To() != cfg.ContractAddress || !bytes.Equal(closed.Data(), closeData) || closed.Gas() != stV1CloseGasFloor || !strings.EqualFold(hash, closed.Hash().Hex()) {
			tb.Fatalf("the close: nonce %d gas %d to %s", closed.Nonce(), closed.Gas(), closed.To())
		}
		if latest := model.GetStTransactionIntent(ctx, logical); latest.Status != model.StTxCanceled || !bytes.Equal(latest.Calldata, calldata) {
			tb.Fatalf("%+v", latest)
		}
	})
}

// A view behind the epoch, as from a node that lags after a failover, writes no row and
// comes again shortly. Only a view past the epoch ends its chain.
func TestStDepositLaggingViewWritesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		cfg := stDemandAttemptConfig()
		client := useStDemandStubClient(tb, cfg, 7)
		client.state = &StEpochState{Epoch: 6, PendingEpoch: 6, EpochStartBlock: 100, TEpochBlocks: 50_400, HeadBlock: 150, HeadBlockTime: server.NowUtc()}
		behind, err := StDepositForEpoch(ctx, 7, nil)
		if err != nil || behind.Status != model.StPublishStatusFailed || !behind.Retry || behind.RetryAt.IsZero() || 2*time.Minute < time.Until(behind.RetryAt) {
			tb.Fatalf("%+v, %v", behind, err)
		}
		if rows := model.GetStPublishes(ctx, cfg.DeploymentKey(), 7); len(rows) != 0 {
			tb.Fatalf("a lagging view wrote %+v", rows)
		}
		if passed, err := StDepositEpochPassed(ctx, 7); err != nil || passed {
			tb.Fatal(passed, err)
		}
		client.state = &StEpochState{Epoch: 8, PendingEpoch: 8, EpochStartBlock: 50_500, TEpochBlocks: 50_400, HeadBlock: 50_510, HeadBlockTime: server.NowUtc()}
		over, err := StDepositForEpoch(ctx, 7, nil)
		if err != nil || over.Status != model.StPublishStatusSkipped || over.Retry {
			tb.Fatalf("%+v, %v", over, err)
		}
		if rows := model.GetStPublishes(ctx, cfg.DeploymentKey(), 7); len(rows) != 1 || rows[0].Status != model.StPublishStatusSkipped {
			tb.Fatalf("%+v", rows)
		}
	})
}

// The conversion's deadline ends the funder's own work, never a send's database work: a
// send made after the deadline has passed settles as any other, where it would panic
// under the expired context.
func TestStDemandSendsPastTheConversionDeadline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		client, fixture := stDemandShortClient(tb)
		// At a block a second, the staging window closes at block 199, one block away.
		client.cfg.BlockSeconds = 1
		stDemandSetBlock(fixture, 198)
		sender := &stDemandSender{core: client, key: client.cfg.DepositKey}
		var sendErr error
		expired := false
		client.demandPayer = stDemandPayerFunc(func(ctx context.Context, request demand.Request) (*demand.Result, error) {
			<-ctx.Done()
			expired = errors.Is(ctx.Err(), context.DeadlineExceeded)
			_, sendErr = sender.SendPrepared(ctx, func(context.Context) (string, common.Address, []byte, error) {
				return "", common.Address{}, nil, nil
			})
			return &demand.Result{Funded: true}, nil
		})
		if outcome := client.fundDemandDeposit(ctx, 7, big.NewInt(100)); outcome != nil {
			tb.Fatalf("%+v", outcome)
		}
		if !expired || sendErr != nil {
			tb.Fatalf("expired %t, the send: %v", expired, sendErr)
		}
	})
}

// stDemandCountingV2 counts the epoch window reads of the usage sync.
type stDemandCountingV2 struct {
	*fakeLedgerV2Chain
	windows int
}

func (self *stDemandCountingV2) LedgerV2EpochWindow(ctx context.Context, epoch uint64) (uint64, uint64, error) {
	self.windows++
	return self.fakeLedgerV2Chain.LedgerV2EpochWindow(ctx, epoch)
}

// stDemandEventsClient serves the chain's DistributionCommitted events.
type stDemandEventsClient struct {
	*stubStClient
	commits []*model.StChainEvent
	reads   int
}

func (self *stDemandEventsClient) SyncEvents(_ context.Context, fromBlock uint64, toBlock uint64) ([]*model.StChainEvent, uint64, error) {
	self.reads++
	events := []*model.StChainEvent{}
	for _, event := range self.commits {
		if fromBlock <= event.BlockNumber && event.BlockNumber <= toBlock {
			events = append(events, event)
		}
	}
	return events, toBlock + 1, nil
}

// The sync leaves an epoch alone for 15 minutes once it has alerted that the epoch can
// have no statement, and once its committed header has failed to read three times in a
// row. A header that lists the epoch twice is refused at once. A commit the mirror never
// saw is placed by its DistributionCommitted event, read from chain over the window.
func TestStLedgerV2UsageSyncBacksOffAndReadsUnplacedCommits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		cfg := ledgerV2TestConfig()
		key := cfg.DeploymentKey()
		previousCfg, previousClient := stConfigInstance, stClientInstance
		SetStConfig(cfg)
		chain := &stDemandCountingV2{fakeLedgerV2Chain: newFakeLedgerV2Chain(&[]string{})}
		SetStLedgerV2Client(chain)
		events := &stDemandEventsClient{stubStClient: newStubStClient(&StEpochState{Epoch: 9, PendingEpoch: 9})}
		SetStClient(events)
		SetStLedgerV2ObjectStore(server.NewLocalBlobStore(tb.TempDir(), "test"))
		clear := func() {
			stLedgerV2Alerted.Clear()
			stLedgerV2UsageBackoff.Clear()
			stLedgerV2UsageHeaderFailures.Clear()
			stLedgerV2Pools.Clear()
		}
		clear()
		alerts := 0
		previousSink := stLedgerV2AlertSink
		stLedgerV2AlertSink = func(name, subject, message string) {
			if name == StLedgerV2AlertUsageStatement {
				alerts++
			}
		}
		tb.Cleanup(func() {
			stConfigInstance, stClientInstance = previousCfg, previousClient
			SetStLedgerV2Client(nil)
			SetStLedgerV2ObjectStore(nil)
			stLedgerV2AlertSink = previousSink
			clear()
		})
		turn := func(epoch, head uint64) (string, error) {
			steps, err := StLedgerV2UsageSync(ctx, &StEpochState{Epoch: epoch, HeadBlock: head})
			if err != nil || len(steps) == 0 {
				return "", err
			}
			return steps[0].Kind, nil
		}
		expire := func(epoch uint64) {
			stLedgerV2UsageBackoff.Store(fmt.Sprintf("%s|%d|%d", key, cfg.NoId, epoch), server.NowUtc().Add(-time.Second))
		}

		// No root in epoch 5's window, past it: one alert, then no reads for a while.
		stDemandCaptureEpochUsage(ctx, key, 5)
		if got, err := turn(6, 5*50_400+1_201); got != "" || err != nil || alerts != 1 || chain.windows != 1 {
			tb.Fatalf("%q, %v, %d alerts, %d window reads", got, err, alerts, chain.windows)
		}
		if got, err := turn(6, 5*50_400+1_201); got != "" || err != nil || alerts != 1 || chain.windows != 1 {
			tb.Fatalf("backed off: %q, %v, %d alerts, %d window reads", got, err, alerts, chain.windows)
		}
		expire(5)
		if _, err := turn(6, 5*50_400+1_201); err != nil || chain.windows != 2 {
			tb.Fatalf("after the back-off: %v, %d window reads", err, chain.windows)
		}

		// A committed header that cannot be read: read again twice, then given up on.
		stDemandCaptureEpochUsage(ctx, key, 6)
		stDemandCommitRoot(tb, cfg, 6, 1, 6*50_400+10, [32]byte{0xee})
		for attempt := 1; attempt <= 2; attempt++ {
			if got, err := turn(7, 6*50_400+20); got != "" || err == nil || alerts != 1 {
				tb.Fatalf("attempt %d: %q, %v, %d alerts", attempt, got, err, alerts)
			}
		}
		if got, err := turn(7, 6*50_400+20); got != "" || err != nil || alerts != 2 {
			tb.Fatalf("the third: %q, %v, %d alerts", got, err, alerts)
		}
		reads := chain.windows
		if got, err := turn(7, 6*50_400+20); got != "" || err != nil || chain.windows != reads {
			tb.Fatalf("backed off: %q, %v, %d window reads", got, err, chain.windows-reads)
		}

		// A header that lists the epoch twice: no statement, at once, and the step
		// refuses it for good.
		stDemandCaptureEpochUsage(ctx, key, 7)
		twice, err := ledger.EncodeHeader(&ledger.Header{NoID: 1, Epoch: 7, Epochs: []ledger.HeaderEpoch{{Epoch: 7, Providers: []ledger.Ref{}}, {Epoch: 7, Providers: []ledger.Ref{}}}, Accounts: []ledger.Ref{ledger.RefOf([]byte("accounts"))}})
		if err != nil {
			tb.Fatal(err)
		}
		if _, err := PutStLedgerV2Object(ctx, twice); err != nil {
			tb.Fatal(err)
		}
		stDemandCommitRoot(tb, cfg, 7, 2, 7*50_400+10, [32]byte(ledger.RefOf(twice)))
		if got, err := turn(8, 7*50_400+20); got != "" || err != nil || alerts != 3 {
			tb.Fatalf("%q, %v, %d alerts", got, err, alerts)
		}
		if outcome := stLedgerV2PublishStatement(ctx, cfg, events, chain, 7); outcome.Retry || outcome.Status != model.StPublishStatusSkipped || !strings.Contains(outcome.Reason, "twice") {
			tb.Fatalf("%+v", outcome)
		}

		// A root the mirror saw only finalized: its commit is read from chain, and the
		// statement is due from that commit's header.
		stDemandCaptureEpochUsage(ctx, key, 8)
		hash := stDemandHeaderEpoch(tb, 8, []string{}, false)
		rootIndex := uint64(3)
		model.SetStLedgerV2RootFinalized(ctx, key, &model.StLedgerV2Root{NoId: 1, Seq: 3, Epoch: 8, Root: [32]byte{3}, LeafCount: 1, LedgerHash: hash, RootIndex: &rootIndex}, 8*50_400+900)
		if got, err := turn(9, 8*50_400+20); got != "" || err != nil || events.reads == 0 {
			tb.Fatalf("no commit on chain yet: %q, %v, %d reads", got, err, events.reads)
		}
		data, err := json.Marshal(map[string]string{"no_id": "1", "seq": "3", "e": "8", "root": fmt.Sprintf("0x%x", [32]byte{3}), "total": "0", "leaf_count": "1", "ledger_hash": fmt.Sprintf("0x%x", hash), "replaced_seq": "0"})
		if err != nil {
			tb.Fatal(err)
		}
		events.commits = []*model.StChainEvent{{BlockNumber: 8*50_400 + 10, Kind: "V2DistributionCommitted", DataJson: string(data)}}
		if got, err := turn(9, 8*50_400+20); got != StLedgerV2UsageStepStatement || err != nil {
			tb.Fatalf("%q, %v", got, err)
		}
	})
}

// stDemandStubClient serves a deposit attempt everything from the test's own values,
// and sends nothing but counts.
type stDemandStubClient struct {
	*stubStClient
	authority  map[uint64]*StPayoutEpochAuthority
	conviction *big.Int
	deposited  *big.Int
	credited   []*big.Int
}

func (self *stDemandStubClient) PayoutEpochAuthority(_ context.Context, epoch uint64) (*StPayoutEpochAuthority, error) {
	if authority := self.authority[epoch]; authority != nil {
		return authority, nil
	}
	return nil, errors.New("no authority for this epoch")
}

func (self *stDemandStubClient) ConvictionBeforeEpoch(context.Context, uint64, uint64) (*big.Int, error) {
	return self.conviction, nil
}

func (self *stDemandStubClient) EpochDeposit(context.Context, uint64, uint64) (*big.Int, error) {
	return self.deposited, nil
}

func (self *stDemandStubClient) DepositCredit(ctx context.Context, epoch uint64, noId uint64, alphaRao *big.Int) (string, error) {
	self.credited = append(self.credited, new(big.Int).Set(alphaRao))
	return self.stubStClient.DepositCredit(ctx, epoch, noId, alphaRao)
}

func stDemandAttemptConfig() *StConfig {
	return &StConfig{Profile: "testnet", Enabled: true, ChainId: stDemandSheetChain, Netuid: uint64(stDemandSheetNetuid), NoId: 1, BlockSeconds: 12, DeploymentId: "attempt-fixture", GenesisHash: [32]byte{1},
		ContractAddress: common.HexToAddress("0x00000000000000000000000000000000000000d1"), SettlementVault: common.HexToAddress("0x00000000000000000000000000000000000000d2"),
		DepositEpochCapRao: 1_000_000, DepositZeroRateAction: protocol.DepositZeroRateEqualDemand, DepositTiers: []StDepositTier{{RateDenominator: 1}}, PolicyHash: [32]byte{9}}
}

func useStDemandStubClient(tb testing.TB, cfg *StConfig, epoch uint64) *stDemandStubClient {
	tb.Helper()
	stub := newStubStClient(&StEpochState{Epoch: epoch, PendingEpoch: epoch, EpochStartBlock: 100, TEpochBlocks: 50_400, HeadBlock: 150, HeadBlockTime: server.NowUtc()})
	client := &stDemandStubClient{stubStClient: stub, authority: map[uint64]*StPayoutEpochAuthority{}, conviction: big.NewInt(0), deposited: big.NewInt(0)}
	previousCfg, previousClient := stConfigInstance, stClientInstance
	SetStConfig(cfg)
	SetStClient(client)
	tb.Cleanup(func() { stConfigInstance, stClientInstance = previousCfg, previousClient })
	return client
}

func stDemandAuthority(epoch uint64, policyHash [32]byte) *StPayoutEpochAuthority {
	return &StPayoutEpochAuthority{Epoch: epoch, PolicyHash: policyHash, Start: protocol.ClientKeyEffectiveBoundary{Block: 100, Hash: [32]byte{1}}, End: protocol.ClientKeyEffectiveBoundary{Block: 200, Hash: [32]byte{2}},
		StartTime: time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC), EndTime: time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)}
}

func stDemandPublishes(ctx context.Context, cfg *StConfig, epoch uint64, kind string) []*model.StPublish {
	rows := []*model.StPublish{}
	for _, publish := range model.GetStPublishes(ctx, cfg.DeploymentKey(), epoch) {
		if publish.Kind == kind {
			rows = append(rows, publish)
		}
	}
	return rows
}

// At zero price with no signed entry the attempt sends nothing and writes the one
// skipped row it always did. With the site down and nothing verified, the price is
// unknown: it still sends nothing, writes nothing, and tries again.
func TestStDepositZeroPriceIsUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		key := stDemandSheetKey(tb, 0x61)
		site, _ := useStDemandSheet(tb, key)
		cfg := stDemandAttemptConfig()
		client := useStDemandStubClient(tb, cfg, 7)
		site.down()
		unknown, err := StDepositForEpoch(ctx, 7, nil)
		if err != nil || !unknown.Retry || unknown.RetryAt.IsZero() || !strings.Contains(unknown.Reason, "the price is unknown") {
			tb.Fatalf("%+v, %v", unknown, err)
		}
		if rows := model.GetStPublishes(ctx, cfg.DeploymentKey(), 7); len(rows) != 0 || client.depositCount != 0 {
			tb.Fatalf("%d rows, %d sends", len(rows), client.depositCount)
		}
		site.serve([]byte("sn: 25\ntiered_per_block_demand_deposits:\n  0:\n    alpha_per_gib: 0\n    alpha_per_user: 0\n"))
		outcome, err := StDepositForEpoch(ctx, 7, nil)
		if err != nil || outcome.Retry || outcome.Status != model.StPublishStatusSkipped || !strings.Contains(outcome.Reason, "price is zero") {
			tb.Fatalf("%+v, %v", outcome, err)
		}
		rows := model.GetStPublishes(ctx, cfg.DeploymentKey(), 7)
		if len(rows) != 1 || rows[0].Kind != model.StPublishKindDeposit || rows[0].Status != model.StPublishStatusSkipped || client.depositCount != 0 {
			tb.Fatalf("%+v, %d sends", rows, client.depositCount)
		}
		// With no pinned signer nothing is fetched, and the outcome is the same.
		other := *cfg
		other.Netuid = 521
		other.ContractAddress = common.HexToAddress("0x00000000000000000000000000000000000000d3")
		useStDemandStubClient(tb, &other, 7)
		fetches := site.fetches.Load()
		same, err := StDepositForEpoch(ctx, 7, nil)
		if err != nil || same.Retry || same.Status != model.StPublishStatusSkipped || site.fetches.Load() != fetches {
			tb.Fatalf("%+v, %v", same, err)
		}
		if rows := model.GetStPublishes(ctx, other.DeploymentKey(), 7); len(rows) != 1 {
			tb.Fatalf("%d rows", len(rows))
		}
	})
}

// A priced sheet prices a v2 source from its statement, once the statement is recorded,
// and until then the attempt waits and sends nothing. A sweep root on the epoch changes
// nothing: only the pool's first epoch decides. The amount comes from the decision
// epoch's entry, and the rollover ends the old epoch's chain.
func TestStDepositPricedFromAV2Statement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		key := stDemandSheetKey(tb, 0x62)
		site, _ := useStDemandSheet(tb, key)
		site.serve(stDemandSheetFile(tb, key, stDemandSheetPrice{7, 100, 5, 1000}))
		cfg := stDemandAttemptConfig()
		cfg.SettlementV2, cfg.VaultV2 = common.HexToAddress(testLedgerV2Settlement), common.HexToAddress(testLedgerV2Vault)
		cfg.ArtifactKey, cfg.V1SweepEpoch = stDemandSheetKey(tb, 0x63), 6
		if err := model.RecordStLedgerV2Activation(ctx, cfg.DeploymentKey(), &model.StLedgerV2Activation{NoId: 1, FirstEpoch: 5, VaultV2Address: strings.ToLower(cfg.VaultV2.Hex()), SettlementV2Address: strings.ToLower(cfg.SettlementV2.Hex())}); err != nil {
			tb.Fatal(err)
		}
		client := useStDemandStubClient(tb, cfg, 7)
		client.authority[6] = stDemandAuthority(6, cfg.PolicyHash)

		pending, err := StDepositForEpoch(ctx, 7, nil)
		if err != nil || !pending.Retry || pending.RetryAt.IsZero() || 2*time.Minute < time.Until(pending.RetryAt) || !strings.Contains(pending.Reason, "usage statement is not recorded") {
			tb.Fatalf("%+v, %v", pending, err)
		}
		if rows := model.GetStPublishes(ctx, cfg.DeploymentKey(), 7); len(rows) != 0 || client.depositCount != 0 {
			tb.Fatalf("%d rows, %d sends", len(rows), client.depositCount)
		}

		model.CaptureStLedgerV2EpochUsers(ctx, cfg.DeploymentKey(), 1, 6, 7)
		chunks := []string{ledger.RefOf([]byte("chunk")).String()}
		_, payload, envelope, err := stLedgerV2SealStatement(cfg, &stLedgerV2StatementInput{Epoch: 6, Authority: client.authority[6], Chunks: chunks, TotalBytes: 3 << 30, Rows: 2, Users: 7})
		if err != nil {
			tb.Fatal(err)
		}
		if err := model.RecordStLedgerV2EpochStatement(ctx, cfg.DeploymentKey(), 1, 6, 3<<30, 2, envelope.ContentHash, payload); err != nil {
			tb.Fatal(err)
		}
		fetches := site.fetches.Load()
		outcome, err := StDepositForEpoch(ctx, 7, nil)
		if err != nil || outcome.Status != model.StPublishStatusConfirmed || outcome.Retry {
			tb.Fatalf("%+v, %v", outcome, err)
		}
		if len(client.credited) != 1 || client.credited[0].Int64() != 335 {
			tb.Fatalf("credited %v", client.credited)
		}
		if site.fetches.Load() != fetches+1 {
			tb.Fatalf("%d fetches in one attempt", site.fetches.Load()-fetches)
		}
		// Once it landed, a later attempt sends nothing.
		client.deposited = big.NewInt(335)
		landed, err := StDepositForEpoch(ctx, 7, nil)
		if err != nil || landed.Status != model.StPublishStatusSkipped || len(client.credited) != 1 {
			tb.Fatalf("%+v, %v", landed, err)
		}
		// The epoch has rolled: the old chain writes its skipped row and ends.
		client.state = &StEpochState{Epoch: 8, PendingEpoch: 8, EpochStartBlock: 100, TEpochBlocks: 50_400, HeadBlock: 150, HeadBlockTime: server.NowUtc()}
		over, err := StDepositForEpoch(ctx, 7, nil)
		if err != nil || over.Retry || over.Status != model.StPublishStatusSkipped || len(client.credited) != 1 {
			tb.Fatalf("%+v, %v", over, err)
		}
		if passed, err := StDepositEpochPassed(ctx, 7); err != nil || !passed {
			tb.Fatal(passed, err)
		}
		if passed, err := StDepositEpochPassed(ctx, 8); err != nil || passed {
			tb.Fatal(passed, err)
		}
	})
}

// stDemandRefusedCredit refuses its first deposits, as a chain that reverts them does.
type stDemandRefusedCredit struct {
	*stDemandStubClient
	refusals int
}

func (self *stDemandRefusedCredit) DepositCredit(ctx context.Context, epoch uint64, noId uint64, alphaRao *big.Int) (string, error) {
	if 0 < self.refusals {
		self.refusals--
		return "", errors.New("execution reverted")
	}
	return self.stDemandStubClient.DepositCredit(ctx, epoch, noId, alphaRao)
}

// A price published after the epoch's zero-price skip is paid, exactly once, and only
// before the deadline. The skip row stands, so the regular chain stays concluded; the
// late price check sees the verified entry and seeds the deposit; a failed attempt
// leaves the epoch open and the next check seeds it again; once it has landed, or once
// the deadline has been reached, nothing more is seeded and nothing more is sent.
func TestStDepositLateEntryIsPaidOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		key := stDemandSheetKey(tb, 0x71)
		site, _ := useStDemandSheet(tb, key)
		site.serve(stDemandSheetFile(tb, key))
		cfg := stDemandAttemptConfig()
		cfg.SettlementV2, cfg.VaultV2 = common.HexToAddress(testLedgerV2Settlement), common.HexToAddress(testLedgerV2Vault)
		cfg.ArtifactKey = stDemandSheetKey(tb, 0x72)
		if err := model.RecordStLedgerV2Activation(ctx, cfg.DeploymentKey(), &model.StLedgerV2Activation{NoId: 1, FirstEpoch: 5, VaultV2Address: strings.ToLower(cfg.VaultV2.Hex()), SettlementV2Address: strings.ToLower(cfg.SettlementV2.Hex())}); err != nil {
			tb.Fatal(err)
		}
		stub := useStDemandStubClient(tb, cfg, 7)
		stub.authority[6] = stDemandAuthority(6, cfg.PolicyHash)
		model.CaptureStLedgerV2EpochUsers(ctx, cfg.DeploymentKey(), 1, 6, 7)
		_, payload, envelope, err := stLedgerV2SealStatement(cfg, &stLedgerV2StatementInput{Epoch: 6, Authority: stub.authority[6], Chunks: []string{ledger.RefOf([]byte("chunk")).String()}, TotalBytes: 3 << 30, Rows: 2, Users: 7})
		if err != nil {
			tb.Fatal(err)
		}
		if err := model.RecordStLedgerV2EpochStatement(ctx, cfg.DeploymentKey(), 1, 6, 3<<30, 2, envelope.ContentHash, payload); err != nil {
			tb.Fatal(err)
		}
		client := &stDemandRefusedCredit{stDemandStubClient: stub, refusals: 1}
		// A second deployment, skipped at zero price too, whose deadline will be reached.
		late := *cfg
		late.ContractAddress = common.HexToAddress("0x00000000000000000000000000000000000000d5")
		lateStub := useStDemandStubClient(tb, &late, 7)
		if skipped, err := StDepositForEpoch(ctx, 7, nil); err != nil || skipped.Status != model.StPublishStatusSkipped {
			tb.Fatalf("%+v, %v", skipped, err)
		}
		use := func(config *StConfig, chain StClient) {
			SetStConfig(config)
			SetStClient(chain)
		}

		use(cfg, client)
		skipped, err := StDepositForEpoch(ctx, 7, nil)
		if err != nil || skipped.Status != model.StPublishStatusSkipped || !strings.HasPrefix(skipped.Reason, stDepositZeroPriceSkip) {
			tb.Fatalf("%+v, %v", skipped, err)
		}
		if !StDepositLateEntryWatched(ctx, 7) {
			tb.Fatal("a zero-price skip is not watched")
		}
		fetches := site.fetches.Load()
		if due, err := StDepositLateEntryDue(ctx, 7); err != nil || due || site.fetches.Load() != fetches+1 {
			tb.Fatalf("still zero price: %t, %v", due, err)
		}

		// The entry is published after the skip.
		site.serve(stDemandSheetFile(tb, key, stDemandSheetPrice{7, 100, 5, 1000}))
		if due, err := StDepositLateEntryDue(ctx, 7); err != nil || !due {
			tb.Fatalf("the late entry is not due: %t, %v", due, err)
		}
		// The first attempt fails. The epoch stays open, and the next check seeds it again.
		failed, err := StDepositForEpoch(ctx, 7, nil)
		if err != nil || failed.Status != model.StPublishStatusFailed || !failed.Retry || len(stub.credited) != 0 {
			tb.Fatalf("%+v, %v, %d credited", failed, err, len(stub.credited))
		}
		if !StDepositLateEntryWatched(ctx, 7) {
			tb.Fatal("a failed attempt ended the watch")
		}
		if due, err := StDepositLateEntryDue(ctx, 7); err != nil || !due {
			tb.Fatalf("a failed attempt is not seeded again: %t, %v", due, err)
		}
		landed, err := StDepositForEpoch(ctx, 7, nil)
		if err != nil || landed.Status != model.StPublishStatusConfirmed || landed.Retry {
			tb.Fatalf("%+v, %v", landed, err)
		}
		if len(stub.credited) != 1 || stub.credited[0].Int64() != 335 {
			tb.Fatalf("credited %v", stub.credited)
		}
		// Landed: nothing more is watched or seeded, so nothing more is sent.
		if StDepositLateEntryWatched(ctx, 7) {
			tb.Fatal("a landed deposit is still watched")
		}
		if due, err := StDepositLateEntryDue(ctx, 7); err != nil || due {
			tb.Fatalf("a landed deposit is due again: %t, %v", due, err)
		}
		rows := model.GetStPublishes(ctx, cfg.DeploymentKey(), 7)
		skips, confirmed := 0, 0
		for _, row := range rows {
			if row.Kind == model.StPublishKindDeposit && row.Status == model.StPublishStatusSkipped {
				skips++
			}
			if row.Kind == model.StPublishKindDeposit && row.Status == model.StPublishStatusConfirmed {
				confirmed++
			}
		}
		if skips != 1 || confirmed != 1 {
			tb.Fatalf("%d skipped and %d confirmed deposit rows", skips, confirmed)
		}

		// The other deployment: up to its last block before the deadline it is due, and from
		// the deadline on nothing is seeded.
		use(&late, lateStub)
		lateStub.state.HeadBlock = lateStub.state.EpochStartBlock + lateStub.state.TEpochBlocks - 2
		if due, err := StDepositLateEntryDue(ctx, 7); err != nil || !due {
			tb.Fatalf("before the deadline: %t, %v", due, err)
		}
		lateStub.state.HeadBlock++
		if due, err := StDepositLateEntryDue(ctx, 7); err != nil || due {
			tb.Fatalf("at the deadline: %t, %v", due, err)
		}
		if !StDepositLateEntryWatched(ctx, 7) || len(lateStub.credited) != 0 || lateStub.depositCount != 0 {
			tb.Fatalf("%d credited", len(lateStub.credited))
		}
		// And a rolled epoch is never due.
		lateStub.state = &StEpochState{Epoch: 8, PendingEpoch: 8, EpochStartBlock: 50_500, TEpochBlocks: 50_400, HeadBlock: 50_510, HeadBlockTime: server.NowUtc()}
		if due, err := StDepositLateEntryDue(ctx, 7); err != nil || due {
			tb.Fatalf("a rolled epoch: %t, %v", due, err)
		}
	})
}

// A priced sheet prices a v1 source from its artifact, as ever, and its rates replace
// the baseline's.
func TestStDepositPricedFromAV1Artifact(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		stDemandDbSetup(ctx)
		fixture, _, cfg, restart := newStPayoutPolicyFixture(tb, 2)
		stRetainPolicyPayout(tb, cfg)
		client := restart()
		fixture.configure(225, "")
		priced := protocol.DepositPolicy{EpochCapRaoPerOperator: 1_000_000, Tiers: []protocol.DepositTier{{RateNumeratorRaoPerGiB: 100, RateNumeratorRaoPerUser: 5, RateDenominator: 1}}}
		sizing, failed, err := stEpochDepositSizingUnder(ctx, cfg, client, nil, 1, priced)
		if err != nil || failed != nil || sizing.ZeroPrice || sizing.RequiredRao.Int64() != 215 || sizing.UsageBytes != 2<<30 || sizing.Users != 3 {
			tb.Fatalf("%+v, %+v, %v", sizing, failed, err)
		}
	})
}

// A statement that is not this operator's, or does not decode, waits like an absent one
// and never prices anything.
func TestStDepositRefusesAStatementItCannotVerify(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		cfg := stDemandAttemptConfig()
		cfg.SettlementV2, cfg.VaultV2 = common.HexToAddress(testLedgerV2Settlement), common.HexToAddress(testLedgerV2Vault)
		cfg.ArtifactKey = stDemandSheetKey(tb, 0x64)
		client := useStDemandStubClient(tb, cfg, 7)
		client.authority[6] = stDemandAuthority(6, cfg.PolicyHash)
		model.CaptureStLedgerV2EpochUsers(ctx, cfg.DeploymentKey(), 1, 6, 7)
		if err := model.RecordStLedgerV2EpochStatement(ctx, cfg.DeploymentKey(), 1, 6, 1, 1, "sha256:bad", []byte(`{"not":"a statement"}`)); err != nil {
			tb.Fatal(err)
		}
		_, _, failed := stDepositStatementTotals(ctx, cfg, client, 6)
		if failed == nil || !failed.Retry || failed.Status != model.StPublishStatusFailed {
			tb.Fatalf("%+v", failed)
		}
	})
}

// A deposit below the runtime minimum, found wherever it is found, ends the epoch with
// a terminal row and sends nothing.
func TestStDepositBelowMinimumIsTerminal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		for name, convert := range map[string]bool{"found by funding": true, "found by staging": false} {
			noId := uint64(1)
			if convert {
				noId = 2
			}
			client, _, fixture := newStDepositCustodyFixture(tb, noId)
			fixture.minimumTao = 1_000
			client.demandPayer = &stDemandFakePayer{result: &demand.Result{Funded: true}}
			client.demandSettle = func(context.Context) error { return nil }
			client.demandRetained = func(context.Context, uint64, *big.Int) (bool, error) { return false, nil }
			withoutStDemandWallets(tb)
			outcome := stDepositFundAndSend(ctx, client.cfg, client, 7, big.NewInt(100), convert)
			if outcome == nil || outcome.Retry || outcome.Status != model.StPublishStatusRefused {
				tb.Fatalf("%s: %+v", name, outcome)
			}
			rows := stDemandPublishes(ctx, client.cfg, 7, model.StPublishKindDeposit)
			if len(rows) == 0 || rows[len(rows)-1].Status != model.StPublishStatusRefused {
				tb.Fatalf("%s: %+v", name, rows)
			}
			fixture.stateLock.Lock()
			sent := len(fixture.sent)
			fixture.stateLock.Unlock()
			if sent != 0 {
				tb.Fatalf("%s: %d sent", name, sent)
			}
		}
	})
}

// The ordinary deposit from a wallet that can pay: no conversion, the staging and then
// the deposit, and the row that concludes the chain. A short wallet converts first. A
// size named by hand does not, unless its caller says so.
func TestStDepositFundAndSend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		payer := &stDemandFakePayer{result: &demand.Result{Funded: true}}
		client.demandPayer = payer
		outcome := stDepositFundAndSend(ctx, client.cfg, client, 7, big.NewInt(100), true)
		if outcome.Status != model.StPublishStatusConfirmed || outcome.Retry || len(payer.requests) != 0 {
			tb.Fatalf("%+v, %d requests", outcome, len(payer.requests))
		}
		fixture.stateLock.Lock()
		sent, deposited := len(fixture.sent), fixture.deposited.Int64()
		fixture.stateLock.Unlock()
		if sent != 2 || deposited != 100 {
			tb.Fatalf("%d sent, %d deposited", sent, deposited)
		}

		short, _, shortFixture := newStDepositCustodyFixture(tb, 2)
		shortFixture.source.SetInt64(0)
		refusing := &stDemandFakePayer{err: demand.ErrInsufficientUSDC}
		short.demandPayer = refusing
		refused := stDepositFundAndSend(ctx, short.cfg, short, 7, big.NewInt(100), false)
		if len(refusing.requests) != 0 || !refused.Retry || !strings.Contains(refused.Reason, "push transferStake") {
			tb.Fatalf("a named size converted: %+v, %d requests", refused, len(refusing.requests))
		}
		converting := stDepositFundAndSend(ctx, short.cfg, short, 7, big.NewInt(100), true)
		if len(refusing.requests) != 1 || !converting.Retry || !strings.Contains(converting.Reason, "cannot cover") {
			tb.Fatalf("%+v, %d requests", converting, len(refusing.requests))
		}
	})
}

// A long refusal writes a bounded number of rows however long it lasts, keeping its
// latest state. A row an attempt left pending and a reason the column cannot hold both
// resolve, on a context of their own even when the attempt's has been cancelled.
func TestStDepositPublishRowsAreBounded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		cfg := stDemandAttemptConfig()
		refuse := func(reason string) {
			stDepositRecordFailure(ctx, cfg, 7, model.StPublishKindDepositPush, &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: reason, Retry: true})
		}
		for range 200 {
			refuse("the same refusal")
		}
		if rows := stDemandPublishes(ctx, cfg, 7, model.StPublishKindDepositPush); len(rows) != 1 {
			tb.Fatalf("%d rows for one reason", len(rows))
		}
		for i := range 200 {
			refuse(fmt.Sprintf("refusal %d", i))
		}
		rows := stDemandPublishes(ctx, cfg, 7, model.StPublishKindDepositPush)
		if len(rows) != stDepositMaxAttemptRows || rows[len(rows)-1].Error == nil || *rows[len(rows)-1].Error != "refusal 199" {
			tb.Fatalf("%d rows", len(rows))
		}
		// Past the bound, a row that records a transaction keeps it.
		sent := "0x" + strings.Repeat("ab", 32)
		model.UpdateStPublish(ctx, rows[len(rows)-1].PublishId, model.StPublishStatusFailed, &sent, nil)
		if next := stDepositPublishRow(ctx, cfg, 7, model.StPublishKindDepositPush); next == rows[len(rows)-1].PublishId {
			tb.Fatal("the row recording a transaction was reused")
		}
		rows = stDemandPublishes(ctx, cfg, 7, model.StPublishKindDepositPush)
		if len(rows) != stDepositMaxAttemptRows+1 || rows[len(rows)-2].TxHash == nil {
			tb.Fatalf("%d rows", len(rows))
		}
		// A row left pending by an attempt that is gone.
		stale := model.AddStPublish(ctx, cfg.DeploymentKey(), 8, model.StPublishKindDeposit)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE st_publish SET create_time=$2, update_time=$2 WHERE publish_id=$1`, stale, server.NowUtc().Add(-2*stDepositStaleRowAge)))
		})
		fresh := stDepositPublishRow(ctx, cfg, 8, model.StPublishKindDeposit)
		rows = stDemandPublishes(ctx, cfg, 8, model.StPublishKindDeposit)
		if len(rows) != 2 || fresh == stale || rows[0].Status != model.StPublishStatusFailed || rows[1].Status != model.StPublishStatusPending {
			tb.Fatalf("%+v", rows)
		}
		// A row an attempt has just made pending again, created long ago, is not taken
		// for one left behind; and reusing a row makes it fresh.
		reused := model.AddStPublish(ctx, cfg.DeploymentKey(), 11, model.StPublishKindDeposit)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE st_publish SET create_time=$2 WHERE publish_id=$1`, reused, server.NowUtc().Add(-2*stDepositStaleRowAge)))
		})
		stDepositPublishRow(ctx, cfg, 11, model.StPublishKindDeposit)
		if rows := stDemandPublishes(ctx, cfg, 11, model.StPublishKindDeposit); len(rows) != 2 || rows[0].PublishId != reused || rows[0].Status != model.StPublishStatusPending {
			tb.Fatalf("a fresh pending row was resolved: %+v", rows)
		}
		for i := range stDepositMaxAttemptRows {
			stDepositRecordFailure(ctx, cfg, 12, model.StPublishKindDepositPush, &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: fmt.Sprintf("refusal %d", i), Retry: true})
		}
		full := stDemandPublishes(ctx, cfg, 12, model.StPublishKindDepositPush)
		newest := full[len(full)-1]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE st_publish SET update_time=$2 WHERE publish_id=$1`, newest.PublishId, server.NowUtc().Add(-2*stDepositStaleRowAge)))
		})
		if again := stDepositPublishRow(ctx, cfg, 12, model.StPublishKindDepositPush); again != newest.PublishId {
			tb.Fatal("the newest row was not reused")
		}
		full = stDemandPublishes(ctx, cfg, 12, model.StPublishKindDepositPush)
		if row := full[len(full)-1]; row.Status != model.StPublishStatusPending || time.Minute < server.NowUtc().Sub(row.UpdateTime) {
			tb.Fatalf("the reused row is not fresh: %+v", row)
		}
		stDepositPublishRow(ctx, cfg, 12, model.StPublishKindDepositPush)
		if row := stDemandPublishes(ctx, cfg, 12, model.StPublishKindDepositPush)[len(full)-1]; row.Status != model.StPublishStatusPending || row.Error != nil {
			tb.Fatalf("a concurrent attempt resolved the reused row: %+v", row)
		}
		// A killed attempt still resolves its row, whatever its reason holds.
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		stResolvePublish(cancelled, fresh, &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: strings.Repeat("é", 2_000) + "\x00\xff"})
		rows = stDemandPublishes(ctx, cfg, 8, model.StPublishKindDeposit)
		if rows[1].Status != model.StPublishStatusFailed || rows[1].Error == nil || len([]rune(*rows[1].Error)) != stPublishReasonMaxChars {
			tb.Fatalf("%+v", rows[1])
		}
		// A send that panics, as a read does once its task is stopped.
		panicked := model.AddStPublish(ctx, cfg.DeploymentKey(), 9, model.StPublishKindDeposit)
		func() {
			defer func() { _ = recover() }()
			defer stDepositResolveOnPanic(cancelled, panicked)
			panic("context canceled")
		}()
		rows = stDemandPublishes(ctx, cfg, 9, model.StPublishKindDeposit)
		if rows[0].Status != model.StPublishStatusFailed {
			tb.Fatalf("%+v", rows[0])
		}
	})
}

// The usage statement's steps: users captured once, never counted again; the statement
// sums the recorded rows, publishes and records, and a repeat publishes the same bytes;
// a failure records nothing and retries.
func TestStLedgerV2UsageStatementSteps(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		cfg := stDemandAttemptConfig()
		cfg.SettlementV2, cfg.VaultV2 = common.HexToAddress(testLedgerV2Settlement), common.HexToAddress(testLedgerV2Vault)
		cfg.ArtifactKey = stDemandSheetKey(tb, 0x65)
		store := server.NewLocalBlobStore(tb.TempDir(), "test")
		SetStLedgerV2ObjectStore(store)
		tb.Cleanup(func() { SetStLedgerV2ObjectStore(nil) })
		client := useStDemandStubClient(tb, cfg, 7)
		key := cfg.DeploymentKey()

		// A failed window read captures nothing.
		if outcome := stLedgerV2CaptureUsers(ctx, cfg, client, 6); !outcome.Retry || model.GetStLedgerV2EpochUsage(ctx, key, 1, 6) != nil {
			tb.Fatalf("%+v", outcome)
		}
		client.authority[6] = stDemandAuthority(6, cfg.PolicyHash)
		if outcome := stLedgerV2CaptureUsers(ctx, cfg, client, 6); outcome.Status != model.StPublishStatusConfirmed {
			tb.Fatalf("%+v", outcome)
		}
		captured := model.GetStLedgerV2EpochUsage(ctx, key, 1, 6)
		if captured == nil || len(captured.Statement) != 0 {
			tb.Fatalf("%+v", captured)
		}
		// Whatever is counted later, the first count stands.
		if recorded := model.CaptureStLedgerV2EpochUsers(ctx, key, 1, 6, captured.TotalUsers+50); recorded != captured.TotalUsers {
			tb.Fatalf("recounted to %d", recorded)
		}
		// The step does not count again: it reads no window at all.
		delete(client.authority, 6)
		if outcome := stLedgerV2CaptureUsers(ctx, cfg, client, 6); outcome.Status != model.StPublishStatusSkipped {
			tb.Fatalf("%+v", outcome)
		}
		client.authority[6] = stDemandAuthority(6, cfg.PolicyHash)

		v2 := newFakeLedgerV2Chain(&[]string{})
		// No root committed yet: it waits, whatever rows are recorded.
		if outcome := stLedgerV2PublishStatement(ctx, cfg, client, v2, 6); !outcome.Retry || !strings.Contains(outcome.Reason, "no root is committed") {
			tb.Fatalf("%+v", outcome)
		}
		put := func(rows []protocol.LedgerUsageRow) []string {
			chunks := ledger.EncodeProviderChunks(rows)
			refs := make([]string, len(chunks))
			for i, chunk := range chunks {
				ref, err := PutStLedgerV2Object(ctx, chunk)
				if err != nil {
					tb.Fatal(err)
				}
				refs[i] = ref
			}
			return refs
		}
		// The epoch's first build, which a rebuild then replaced before the commit.
		first := put([]protocol.LedgerUsageRow{{ClientID: [16]byte{9}, UsageBytes: 7 << 30}})
		if err := model.RecordStLedgerV2EpochRows(ctx, key, 1, 6, first); err != nil {
			tb.Fatal(err)
		}
		if outcome := stLedgerV2PublishStatement(ctx, cfg, client, v2, 6); !outcome.Retry || !strings.Contains(outcome.Reason, "no root is committed") {
			tb.Fatalf("recorded rows alone built a statement: %+v", outcome)
		}
		rows := []protocol.LedgerUsageRow{{ClientID: [16]byte{1}, UsageBytes: 1 << 30}, {ClientID: [16]byte{2}, Coldkey: [32]byte{2}, UsageBytes: 2 << 30, HeadExcluded: true}}
		refs := put(rows)
		// A root committed for the epoch outside its root window is not one validators match.
		end6, window := uint64(6*50_400), uint64(1_200)
		stDemandCommitRoot(tb, cfg, 6, 1, end6+window+1, stDemandHeaderEpoch(tb, 6, refs, false))
		if outcome := stLedgerV2PublishStatement(ctx, cfg, client, v2, 6); !outcome.Retry || !strings.Contains(outcome.Reason, "no root is committed") {
			tb.Fatalf("%+v", outcome)
		}
		stDemandCommitRoot(tb, cfg, 6, 2, end6+window, stDemandHeaderEpoch(tb, 6, refs, false))
		// A store that refuses the statement leaves nothing recorded.
		SetStLedgerV2ObjectStore(&refusingBlobStore{BlobStore: store})
		if outcome := stLedgerV2PublishStatement(ctx, cfg, client, v2, 6); !outcome.Retry {
			tb.Fatalf("%+v", outcome)
		}
		if usageRecord := model.GetStLedgerV2EpochUsage(ctx, key, 1, 6); len(usageRecord.Statement) != 0 {
			tb.Fatal("a failed publish recorded a statement")
		}
		SetStLedgerV2ObjectStore(store)
		// A crash between the publish and the record: the retry's bytes are the same.
		_, _, earlier, err := stLedgerV2SealStatement(cfg, &stLedgerV2StatementInput{Epoch: 6, Authority: client.authority[6], Chunks: refs, TotalBytes: 3 << 30, Rows: 2, Users: captured.TotalUsers})
		if err != nil {
			tb.Fatal(err)
		}
		published, err := startifact.PublishEvidence(ctx, store, earlier)
		if err != nil {
			tb.Fatal(err)
		}
		outcome := stLedgerV2PublishStatement(ctx, cfg, client, v2, 6)
		if outcome.Status != model.StPublishStatusConfirmed {
			tb.Fatalf("%+v", outcome)
		}
		record := model.GetStLedgerV2EpochUsage(ctx, key, 1, 6)
		if record.ContentHash != published.ContentHash || record.TotalUsageBytes != 3<<30 || record.ProviderRows != 2 || record.TotalUsers != captured.TotalUsers || record.StatementTime == nil {
			tb.Fatalf("%+v", record)
		}
		statement, err := usage.Decode(record.Statement)
		if err != nil {
			tb.Fatal(err)
		}
		if err := statement.CheckRows(rows); err != nil {
			tb.Fatal(err)
		}
		// The committed header's chunks, not the first build's.
		if statement.Epoch != 6 || !slices.Equal(statement.ProviderChunks, refs) || slices.Equal(statement.ProviderChunks, first) {
			tb.Fatalf("%+v", statement)
		}
		if outcome := stLedgerV2PublishStatement(ctx, cfg, client, v2, 6); outcome.Status != model.StPublishStatusSkipped {
			tb.Fatalf("%+v", outcome)
		}
		// A different statement cannot replace the recorded one.
		if err := model.RecordStLedgerV2EpochStatement(ctx, key, 1, 6, 1, 1, "sha256:other", []byte("{}")); !errors.Is(err, model.ErrStLedgerV2EpochUsageFixed) {
			tb.Fatal(err)
		}
		if err := model.RecordStLedgerV2EpochStatement(ctx, key, 1, 6, record.TotalUsageBytes, record.ProviderRows, record.ContentHash, record.Statement); err != nil {
			tb.Fatal(err)
		}
		// The latest statement is what the status command shows.
		if latest := model.GetLatestStLedgerV2EpochStatement(ctx, key, 1); latest == nil || latest.Epoch != 6 {
			tb.Fatalf("%+v", latest)
		}
		// An epoch with no provider rows has a statement of zero bytes.
		model.CaptureStLedgerV2EpochUsers(ctx, key, 1, 5, 0)
		stDemandCommitRoot(tb, cfg, 5, 3, 5*50_400, stDemandHeaderEpoch(tb, 5, []string{}, false))
		client.authority[5] = stDemandAuthority(5, cfg.PolicyHash)
		if outcome := stLedgerV2PublishStatement(ctx, cfg, client, v2, 5); outcome.Status != model.StPublishStatusConfirmed {
			tb.Fatalf("%+v", outcome)
		}
		empty := model.GetStLedgerV2EpochUsage(ctx, key, 1, 5)
		decoded, err := usage.Decode(empty.Statement)
		if err != nil || decoded.TotalUsageBytes != 0 || decoded.ProviderChunks == nil || len(decoded.ProviderChunks) != 0 {
			tb.Fatalf("%+v, %v", decoded, err)
		}
		// An epoch its committed header declares unrecoverable has none, and nothing
		// retries it.
		model.CaptureStLedgerV2EpochUsers(ctx, key, 1, 4, 0)
		stDemandCommitRoot(tb, cfg, 4, 4, 4*50_400+1, stDemandHeaderEpoch(tb, 4, nil, true))
		if outcome := stLedgerV2PublishStatement(ctx, cfg, client, v2, 4); outcome.Retry || outcome.Status != model.StPublishStatusSkipped {
			tb.Fatalf("%+v", outcome)
		}
	})
}

// stDemandHeaderEpoch stores the header of a root for epoch that covers only that
// epoch, listing refs as its provider chunks, or declaring it unrecoverable, and
// returns its ledger hash.
func stDemandHeaderEpoch(tb testing.TB, epoch uint64, refs []string, null bool) [32]byte {
	tb.Helper()
	covered := ledger.HeaderEpoch{Epoch: epoch, Null: null}
	if !null {
		covered.Providers = make([]ledger.Ref, len(refs))
		for i, text := range refs {
			ref, err := ledger.ParseRef(text)
			if err != nil {
				tb.Fatal(err)
			}
			covered.Providers[i] = ref
		}
	}
	data, err := ledger.EncodeHeader(&ledger.Header{NoID: 1, Epoch: epoch, Epochs: []ledger.HeaderEpoch{covered}, Accounts: []ledger.Ref{ledger.RefOf([]byte("accounts"))}})
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := PutStLedgerV2Object(context.Background(), data); err != nil {
		tb.Fatal(err)
	}
	return [32]byte(ledger.RefOf(data))
}

// Captures an ended epoch's users and its extender census, as the two usage
// steps the sync plans for it do (st_ledger_v2_extenders.go); an operator
// without extender records captures an empty census.
func stDemandCaptureEpochUsage(ctx context.Context, key model.StDeploymentKey, epoch uint64) {
	model.CaptureStLedgerV2EpochUsers(ctx, key, 1, epoch, 3)
	model.CaptureStLedgerV2ExtenderCensus(ctx, key, 1, epoch, model.EncodeStExtenderCensus(nil), 0, server.NowUtc())
}

// stDemandCommitRoot mirrors a DistributionCommitted event of pool 1 at block.
func stDemandCommitRoot(tb testing.TB, cfg *StConfig, epoch uint64, seq uint64, block uint64, ledgerHash [32]byte) {
	tb.Helper()
	model.AddStLedgerV2Commit(context.Background(), cfg.DeploymentKey(), &model.StLedgerV2Root{NoId: 1, Seq: seq, Epoch: epoch, Root: [32]byte{byte(seq)}, LeafCount: 1, LedgerHash: ledgerHash}, block, 0)
}

// A sync turn plans the ended epoch's next usage step, on a registered pool, and only
// from the pool's first epoch on: users and the extender census at once, each until it
// is captured, the statement once the root committed for the epoch in its root window
// is mirrored, and, for one of the two epochs before E_x (here 7, configured) once its
// rows are recorded without an extender component, its extender shadow report until one
// is recorded. An epoch that can have no statement, declared unrecoverable, with no root
// in its window or with more than one, alerts once and plans nothing.
func TestStLedgerV2UsageSync(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		cfg := ledgerV2TestConfig()
		cfg.Netuid = 7
		line, err := amendment.NewExtenders(cfg.ChainId, uint16(cfg.Netuid), cfg.NoId, 7).Line()
		if err != nil {
			tb.Fatal(err)
		}
		if err := cfg.Amendments.Encode([]string{line}); err != nil {
			tb.Fatal(err)
		}
		stExtenderShadowConcluded.Clear()
		tb.Cleanup(stExtenderShadowConcluded.Clear)
		key := cfg.DeploymentKey()
		previousCfg := stConfigInstance
		SetStConfig(cfg)
		chain := newFakeLedgerV2Chain(&[]string{})
		SetStLedgerV2Client(chain)
		stLedgerV2Alerted.Clear()
		tb.Cleanup(func() {
			stConfigInstance = previousCfg
			SetStLedgerV2Client(nil)
			stLedgerV2Pools.Clear()
			stLedgerV2Alerted.Clear()
		})
		plan := func(epoch uint64) string {
			tb.Helper()
			steps, err := StLedgerV2UsageSync(ctx, &StEpochState{Epoch: epoch})
			if err != nil {
				tb.Fatal(err)
			}
			planned := []string{}
			for _, step := range steps {
				planned = append(planned, fmt.Sprintf("%s %d", step.Kind, step.Epoch))
			}
			return strings.Join(planned, ", ")
		}
		if got := plan(5); got != "" {
			tb.Fatalf("the epoch before the pool's first: %q", got)
		}
		// the extender census is captured beside the users, each once
		if got := plan(6); got != StLedgerV2UsageStepUsers+" 5, "+StLedgerV2UsageStepExtenderCensus+" 5" {
			tb.Fatalf("%q", got)
		}
		store := server.NewLocalBlobStore(tb.TempDir(), "test")
		SetStLedgerV2ObjectStore(store)
		tb.Cleanup(func() { SetStLedgerV2ObjectStore(nil) })
		model.CaptureStLedgerV2EpochUsers(ctx, key, 1, 5, 3)
		if got := plan(6); got != StLedgerV2UsageStepExtenderCensus+" 5" {
			tb.Fatalf("the census after the users: %q", got)
		}
		model.CaptureStLedgerV2ExtenderCensus(ctx, key, 1, 5, model.EncodeStExtenderCensus(nil), 0, server.NowUtc())
		if got := plan(6); got != "" {
			tb.Fatalf("waiting on the commit: %q", got)
		}
		if err := model.RecordStLedgerV2EpochRows(ctx, key, 1, 5, nil); err != nil {
			tb.Fatal(err)
		}
		// rows recorded without an extender component, with the census
		// captured: the epoch's shadow report is due, and not the statement
		if got := plan(6); got != StLedgerV2UsageStepExtenderShadow+" 5" {
			tb.Fatalf("recorded rows planned the statement: %q", got)
		}
		stDemandCommitRoot(tb, cfg, 5, 1, 5*50_400+10, stDemandHeaderEpoch(tb, 5, []string{}, false))
		if got := plan(6); got != StLedgerV2UsageStepStatement+" 5, "+StLedgerV2UsageStepExtenderShadow+" 5" {
			tb.Fatalf("%q", got)
		}
		if err := model.RecordStLedgerV2EpochStatement(ctx, key, 1, 5, 0, 0, "sha256:x", []byte("{}")); err != nil {
			tb.Fatal(err)
		}
		if got := plan(6); got != StLedgerV2UsageStepExtenderShadow+" 5" {
			tb.Fatalf("the statement recorded: %q", got)
		}
		model.RecordStLedgerV2ExtenderShadow(ctx, key, 1, 5, "sha256:"+strings.Repeat("ab", 32), []byte("{}"))
		if got := plan(6); got != "" {
			tb.Fatalf("done: %q", got)
		}
		alerts := 0
		previousSink := stLedgerV2AlertSink
		stLedgerV2AlertSink = func(name, subject, message string) {
			if name == StLedgerV2AlertUsageStatement {
				alerts++
			}
		}
		tb.Cleanup(func() { stLedgerV2AlertSink = previousSink })
		stDemandCaptureEpochUsage(ctx, key, 6)
		stDemandCommitRoot(tb, cfg, 6, 2, 6*50_400+10, stDemandHeaderEpoch(tb, 6, nil, true))
		if got := plan(7); got != "" || alerts != 1 {
			tb.Fatalf("%q, %d alerts", got, alerts)
		}
		if got := plan(7); got != "" || alerts != 1 {
			tb.Fatalf("%q, %d alerts", got, alerts)
		}
		// No root in the window: nothing until the window has closed, then an alert.
		stDemandCaptureEpochUsage(ctx, key, 7)
		turn := func(epoch, head uint64) string {
			tb.Helper()
			steps, err := StLedgerV2UsageSync(ctx, &StEpochState{Epoch: epoch, HeadBlock: head})
			if err != nil {
				tb.Fatal(err)
			}
			if len(steps) != 0 {
				return steps[0].Kind
			}
			return ""
		}
		if got := turn(8, 7*50_400+1_200); got != "" || alerts != 1 {
			tb.Fatalf("%q, %d alerts", got, alerts)
		}
		if got := turn(8, 7*50_400+1_201); got != "" || alerts != 2 {
			tb.Fatalf("%q, %d alerts", got, alerts)
		}
		// Two roots in the window: validators cannot match one statement, so none.
		stDemandCaptureEpochUsage(ctx, key, 8)
		stDemandCommitRoot(tb, cfg, 8, 3, 8*50_400+1, stDemandHeaderEpoch(tb, 8, []string{}, false))
		stDemandCommitRoot(tb, cfg, 8, 4, 8*50_400+2, stDemandHeaderEpoch(tb, 8, []string{}, false))
		if got := turn(9, 8*50_400+3); got != "" || alerts != 3 {
			tb.Fatalf("%q, %d alerts", got, alerts)
		}
		// Nothing is planned without settlement v2.
		off := *cfg
		off.SettlementV2, off.VaultV2 = common.Address{}, common.Address{}
		SetStConfig(&off)
		if got := plan(8); got != "" {
			tb.Fatalf("%q", got)
		}
	})
}

// A real funding read of a retained staging intent: staging will not send another, so
// funding must not buy for it.
func TestStDemandRetainedFundingIntentIsRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		client, _, _ := newStDepositCustodyFixture(tb, 1)
		if retained, err := client.retainedDepositFunding(ctx, 7, big.NewInt(3)); err != nil || retained {
			tb.Fatal(retained, err)
		}
		logical, err := stTransactionLogicalKey(client.cfg, "deposit-fund:7:1:3")
		if err != nil {
			tb.Fatal(err)
		}
		data, err := stPackTransferStake(ss58.EvmMirrorPubkey(client.cfg.ContractAddress), client.cfg.DepositHotkey, client.cfg.Netuid, big.NewInt(103))
		if err != nil {
			tb.Fatal(err)
		}
		model.ReserveStTransactionIntent(ctx, logical, client.cfg.Profile, client.cfg.DeploymentId, client.cfg.DeploymentKey(), client.cfg.ChainId, fmt.Sprintf("0x%x", client.cfg.GenesisHash), strings.ToLower(crypto.PubkeyToAddress(client.cfg.DepositKey.PublicKey).Hex()), strings.ToLower(stStakingPrecompileAddress.Hex()), strings.ToLower(crypto.Keccak256Hash(data).Hex()), data, 7)
		if retained, err := client.retainedDepositFunding(ctx, 7, big.NewInt(3)); err != nil || !retained {
			tb.Fatal(retained, err)
		}
	})
}

// The store's price sheet row, as the source reads and saves it.
func TestStDemandSheetStore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		stDemandDbSetup(ctx)
		key := stDemandSheetKey(tb, 0x66)
		cfg := stDemandSheetConfig()
		store := &stDemandSheetStore{deploymentKey: cfg.DeploymentKey()}
		if raw, err := store.Load(ctx); err != nil || raw != nil {
			tb.Fatal(raw, err)
		}
		first := stDemandSheetFile(tb, key, stDemandSheetPrice{1, 100, 5, 1000})
		if err := store.Save(ctx, first); err != nil {
			tb.Fatal(err)
		}
		next := stDemandSheetFile(tb, key, stDemandSheetPrice{1, 100, 5, 1000}, stDemandSheetPrice{2, 100, 5, 1000})
		if err := store.Save(ctx, next); err != nil {
			tb.Fatal(err)
		}
		raw, err := store.Load(ctx)
		if err != nil || !bytes.Equal(raw, next) {
			tb.Fatal(err)
		}
		sheet, err := pricesheet.Parse(raw)
		if err != nil || len(sheet.Entries) != 2 {
			tb.Fatal(err)
		}
		// A cancelled context is an error to the source, not a panic through it.
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := store.Load(cancelled); err == nil {
			tb.Fatal("a cancelled read succeeded")
		}
		funder := &stDemandStore{deploymentKey: cfg.DeploymentKey()}
		if _, _, err := funder.Load(cancelled); err == nil {
			tb.Fatal("a cancelled funder read succeeded")
		}
		if _, err := funder.Save(cancelled, &demand.State{Epoch: 1}, 0); err == nil {
			tb.Fatal("a cancelled funder write succeeded")
		}
		if err := funder.Record(cancelled, demand.Event{Time: server.NowUtc(), Epoch: 1, Kind: "x"}); err == nil {
			tb.Fatal("a cancelled funder record succeeded")
		}
	})
}
