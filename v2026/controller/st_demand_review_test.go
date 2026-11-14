package controller

// The independent review's findings on the demand deposit, kept as tests of the fixed
// behaviour. DB-free: every chain read is a local httptest fixture.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/urfoundation/sn/v2026/demand"
	"github.com/urfoundation/sn/v2026/ss58"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// stDemandSetBlock moves the custody fixture's finalized and latest block.
func stDemandSetBlock(fixture *stDepositCustodyRpc, block uint64) {
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	fixture.boundary.Block, fixture.boundary.Hash = block, [32]byte(stTransactionReconcileBlockHash(block))
}

// The margins' arithmetic at their exact boundaries. The fixture's epoch 7 ends at
// block 2000, so its deposit deadline is block 1999; latest is the finalized block.
func TestStDepositDeadlineBoundaries(t *testing.T) {
	ctx := context.Background()
	client, _, fixture := newStDepositCustodyFixture(t, 1)
	if margin := stDepositStagingMarginBlocks(client.cfg); margin != 150 {
		t.Fatalf("margin %d", margin)
	}
	for _, test := range []struct {
		latest           uint64
		staging, deposit bool
	}{
		{1_848, true, true},
		{1_849, false, true},
		{1_998, false, true},
		{1_999, false, false},
	} {
		stDemandSetBlock(fixture, test.latest)
		stagingErr := client.requireDepositStagingOpen(ctx, 7, 1_999)
		depositErr := client.requireDepositDeadlineOpen(ctx, 7, 1_999)
		if (stagingErr == nil) != test.staging || (depositErr == nil) != test.deposit {
			t.Fatalf("latest %d: staging %v, deposit %v", test.latest, stagingErr, depositErr)
		}
	}
	// No underflow when the margin exceeds the deadline.
	stDemandSetBlock(fixture, 10)
	if open, err := client.depositDeadlineOpen(ctx, 100, 150); err != nil || open {
		t.Fatalf("open %t %v", open, err)
	}
	other := *client.cfg
	for seconds, want := range map[int64]uint64{0: 150, 12: 150, 7: 257, 1_801: 0} {
		other.BlockSeconds = seconds
		if got := stDepositStagingMarginBlocks(&other); got != want {
			t.Fatalf("block seconds %d: margin %d, want %d", seconds, got, want)
		}
	}
}

// The sweep's hold stops only a conversion. With no demand.yml, and with a wallet that
// can already pay, the deposit goes on inside the hold as it always did; the hold is not
// even read. A conversion it holds asks the funder for nothing.
func TestStDemandSweepHoldGatesOnlyTheConversion(t *testing.T) {
	ctx := context.Background()
	held := func(client *CoreStClient) *int {
		reads := 0
		client.demandHold = func(context.Context) (string, error) {
			reads++
			return "held for the sweep's v1 close of epoch 7, until it is confirmed or block 170", nil
		}
		return &reads
	}
	t.Run("no demand.yml", func(t *testing.T) {
		client, _ := stDemandShortClient(t)
		withoutStDemandWallets(t)
		reads := held(client)
		if outcome := client.fundDemandDeposit(ctx, 7, big.NewInt(100)); outcome != nil || *reads != 0 {
			t.Fatalf("%+v, %d hold reads", outcome, *reads)
		}
	})
	t.Run("a wallet that can pay", func(t *testing.T) {
		client, fixture := stDemandShortClient(t)
		fixture.source.SetInt64(103)
		payer := &stDemandFakePayer{result: &demand.Result{Funded: true}}
		client.demandPayer = payer
		reads := held(client)
		if outcome := client.fundDemandDeposit(ctx, 7, big.NewInt(100)); outcome != nil || *reads != 0 || len(payer.requests) != 0 {
			t.Fatalf("%+v, %d hold reads, %d requests", outcome, *reads, len(payer.requests))
		}
	})
	t.Run("a conversion waits", func(t *testing.T) {
		client, _ := stDemandShortClient(t)
		payer := &stDemandFakePayer{result: &demand.Result{Funded: true}}
		client.demandPayer = payer
		reads := held(client)
		outcome := client.fundDemandDeposit(ctx, 7, big.NewInt(100))
		if outcome == nil || !outcome.Retry || !strings.Contains(outcome.Reason, "held for the sweep") || *reads != 1 || len(payer.requests) != 0 {
			t.Fatalf("%+v, %d hold reads, %d requests", outcome, *reads, len(payer.requests))
		}
	})
}

// No conversion step starts after the staging window closes: the funder runs under a
// deadline at the window's close, 49 blocks of 12 seconds from block 1800 here.
func TestStDemandConversionEndsWhenStagingCloses(t *testing.T) {
	ctx := context.Background()
	client, fixture := stDemandShortClient(t)
	stDemandSetBlock(fixture, 1_800)
	var deadline time.Time
	var bounded bool
	client.demandPayer = stDemandPayerFunc(func(ctx context.Context, request demand.Request) (*demand.Result, error) {
		deadline, bounded = ctx.Deadline()
		return &demand.Result{Funded: true}, nil
	})
	before := server.NowUtc()
	if outcome := client.fundDemandDeposit(ctx, 7, big.NewInt(100)); outcome != nil {
		t.Fatalf("%+v", outcome)
	}
	want := before.Add(49 * 12 * time.Second)
	if !bounded || deadline.Before(want.Add(-5*time.Second)) || want.Add(5*time.Second).Before(deadline) {
		t.Fatalf("the funder's deadline %s (bounded %t), want about %s", deadline, bounded, want)
	}
	// Past the window's close nothing converts at all.
	stDemandSetBlock(fixture, 1_849)
	called := false
	client.demandPayer = stDemandPayerFunc(func(context.Context, demand.Request) (*demand.Result, error) {
		called = true
		return &demand.Result{Funded: true}, nil
	})
	if outcome := client.fundDemandDeposit(ctx, 7, big.NewInt(100)); outcome == nil || !outcome.Retry || called {
		t.Fatalf("%+v, called %t", outcome, called)
	}
}

type stDemandPayerFunc func(context.Context, demand.Request) (*demand.Result, error)

func (self stDemandPayerFunc) Ensure(ctx context.Context, request demand.Request) (*demand.Result, error) {
	return self(ctx, request)
}

// stDemandLatestRpc serves the custody fixture with a latest block ahead of its
// finalized one.
type stDemandLatestRpc struct {
	*stDepositCustodyRpc
	latest uint64
}

func (self *stDemandLatestRpc) GetBlockByNumber(ctx context.Context, tag rpc.BlockNumber, full bool) (map[string]any, error) {
	if tag == rpc.LatestBlockNumber {
		return stTransactionReconcileBlock(self.latest), nil
	}
	return self.stDepositCustodyRpc.GetBlockByNumber(ctx, tag, full)
}

// Custody reads the finalized head. Once the latest block has reached a deposit's
// deadline, neither a fee replacement nor the signature of a reserved deposit is made,
// though the finalized head is still before it: the intent waits for the expiry
// handling. One block earlier both are still made.
func TestStDepositSigningStopsAtTheLatestDeadline(t *testing.T) {
	ctx := context.Background()
	client, _, fixture := newStDepositCustodyFixture(t, 1)
	stDemandSetBlock(fixture, 1_997)
	fixture.stateLock.Lock()
	fixture.staged.SetInt64(102)
	fixture.stateLock.Unlock()
	wrapper := &stDemandLatestRpc{stDepositCustodyRpc: fixture}
	rpcServer := rpc.NewServer()
	if err := rpcServer.RegisterName("eth", wrapper); err != nil {
		t.Fatal(err)
	}
	if err := rpcServer.RegisterName("chain", wrapper); err != nil {
		t.Fatal(err)
	}
	listener := httptest.NewServer(rpcServer)
	rpcClient, err := ethclient.Dial(listener.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rpcClient.Close(); listener.Close(); rpcServer.Stop() })
	client.cfg.RpcUrls = []string{listener.URL}
	client.clients = map[string]*ethclient.Client{listener.URL: rpcClient}
	amount := big.NewInt(100)
	calldata := client.coordinator.PackDeposit(new(big.Int).SetUint64(client.cfg.NoId), amount, big.NewInt(3), 1_999)
	logical, err := stTransactionLogicalKey(client.cfg, fmt.Sprintf("deposit:7:%d:3", client.cfg.NoId))
	if err != nil {
		t.Fatal(err)
	}
	from := crypto.PubkeyToAddress(client.cfg.DepositKey.PublicKey)
	intent := &model.StTransactionIntent{IntentId: server.NewId(), LogicalKey: logical, Profile: client.cfg.Profile, DeploymentId: client.cfg.DeploymentId, DeploymentKey: client.cfg.DeploymentKey(),
		ChainId: client.cfg.ChainId, GenesisHash: hexutil.Encode(client.cfg.GenesisHash[:]), FromAddress: strings.ToLower(from.Hex()), ToAddress: strings.ToLower(client.cfg.ContractAddress.Hex()),
		CalldataHash: strings.ToLower(crypto.Keccak256Hash(calldata).Hex()), Calldata: calldata, Nonce: 7, Status: model.StTxBroadcast, AttemptCount: 1}
	wrapper.latest = 1_998
	for _, reserved := range []bool{false, true} {
		if err := client.validateDepositAttempt(ctx, rpcClient, intent, model.StTxAttemptExecution, reserved); err != nil {
			t.Fatalf("before the deadline, reserved %t: %v", reserved, err)
		}
	}
	for _, latest := range []uint64{1_999, 2_003} {
		wrapper.latest = latest
		for _, reserved := range []bool{false, true} {
			if err := client.validateDepositAttempt(ctx, rpcClient, intent, model.StTxAttemptExecution, reserved); err == nil || !strings.Contains(err.Error(), "deadline") {
				t.Fatalf("latest %d, reserved %t: %v", latest, reserved, err)
			}
		}
	}
	// Once the finalized head has passed the deadline, into epoch 8, the reserved bytes
	// are admitted again, for the expiry handling to sign: they revert and free the
	// nonce.
	stDepositCustodyExpire(fixture)
	if err := client.validateDepositAttempt(ctx, rpcClient, intent, model.StTxAttemptExecution, true); err != nil {
		t.Fatalf("the expired reserved deposit: %v", err)
	}
}

// Only a view past an epoch makes it passed. A view behind it, from a node that lags,
// says nothing, so it cannot end the epoch's deposit chain.
func TestStDepositEpochPassedOnlyPastTheEpoch(t *testing.T) {
	previousCfg, previousClient := stConfigInstance, stClientInstance
	t.Cleanup(func() { stConfigInstance, stClientInstance = previousCfg, previousClient })
	SetStConfig(&StConfig{Enabled: true, Profile: "testnet", ChainId: 945, NoId: 1})
	for _, test := range []struct {
		view, pending, epoch uint64
		passed               bool
	}{
		{7, 7, 8, false},
		{8, 8, 8, false},
		{9, 9, 8, true},
		// The legacy contract: rolled behind, pending past.
		{8, 9, 8, true},
	} {
		SetStClient(newStubStClient(&StEpochState{Epoch: test.view, PendingEpoch: test.pending}))
		if passed, err := StDepositEpochPassed(context.Background(), test.epoch); err != nil || passed != test.passed {
			t.Fatalf("view %d/%d, epoch %d: passed %t, %v", test.view, test.pending, test.epoch, passed, err)
		}
	}
}

// A retirement that has turned a staging transfer's reservation into its cancellation
// can never be signed as the execution, and a custody read that fails proves nothing
// rolled. So a run after a crash between the turn and the cancellation's attempt record
// must take the cancellation from the reservation itself (turnedDepositStaging), never
// from custody: TestStV1SweepCloseGoesAfterATurnedStagingCancellation shows it does.
func TestStDemandTurnedStagingIsNeverSignedAsTheExecution(t *testing.T) {
	ctx := context.Background()
	client, rpcClient, fixture := newStDepositCustodyFixture(t, 1)
	cfg := client.cfg
	from := crypto.PubkeyToAddress(cfg.DepositKey.PublicKey)
	calldata, err := stPackTransferStake(ss58.EvmMirrorPubkey(cfg.ContractAddress), cfg.DepositHotkey, cfg.Netuid, big.NewInt(103))
	if err != nil {
		t.Fatal(err)
	}
	// The fixture is at epoch 7, deposit nonce 3: this staging is epoch 6's.
	logical, err := stTransactionLogicalKey(cfg, fmt.Sprintf("deposit-fund:%d:%d:%d", 6, cfg.NoId, 3))
	if err != nil {
		t.Fatal(err)
	}
	intent := &model.StTransactionIntent{IntentId: server.NewId(), IntentKey: logical + ":0", LogicalKey: logical, Profile: cfg.Profile, DeploymentId: cfg.DeploymentId,
		DeploymentKey: cfg.DeploymentKey(), ChainId: cfg.ChainId, GenesisHash: hexutil.Encode(cfg.GenesisHash[:]), FromAddress: strings.ToLower(from.Hex()),
		ToAddress: strings.ToLower(stStakingPrecompileAddress.Hex()), CalldataHash: strings.ToLower(crypto.Keccak256Hash(calldata).Hex()), Calldata: calldata, Nonce: 7, Status: model.StTxPrepared}
	if !client.depositStagingShape(intent, from) || !stDepositStagingRolled(cfg, logical, 7, big.NewInt(3)) {
		t.Fatal("fixture staging is not a rolled staging")
	}
	fixture.stateLock.Lock()
	fixture.custodyFault = "paused"
	fixture.stateLock.Unlock()
	if rolled, err := client.retireRolledDepositStaging(ctx, rpcClient, intent, from); err != nil || rolled {
		t.Fatalf("an unreadable custody proved a roll: %t %v", rolled, err)
	}
	turned := &model.StTransactionGasReservation{IntentId: intent.IntentId, Attempt: 1, Kind: model.StTxAttemptCancellation, Reused: true}
	if _, err := client.signReservedStTransaction(ctx, rpcClient, cfg.DepositKey, intent, model.StTxAttemptExecution, turned, nil, nil); err == nil || !strings.Contains(err.Error(), "cannot become a different request") {
		t.Fatalf("a turned reservation was signed as the execution: %v", err)
	}
}

// A panic out of the durable sender, as from a database that fails mid-send, is a wallet
// whose transactions have not settled, never a panic through the funder.
func TestStDemandSenderPanicIsUnsettled(t *testing.T) {
	key, err := crypto.HexToECDSA(strings.Repeat("5", 64))
	if err != nil {
		t.Fatal(err)
	}
	// No client at all: the engine panics on its first read.
	sender := &stDemandSender{key: key}
	hash, err := sender.SendPrepared(context.Background(), func(context.Context) (string, common.Address, []byte, error) {
		t.Fatal("prepare ran")
		return "", common.Address{}, nil, nil
	})
	if hash != "" || !errors.Is(err, errStDemandUnsettled) {
		t.Fatalf("%q, %v", hash, err)
	}
}
