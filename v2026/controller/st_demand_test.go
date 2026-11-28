package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/v2026/demand"
	"github.com/urfoundation/sn/v2026/demand/bridge"
	"github.com/urfoundation/sn/v2026/demand/solana"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

var (
	stDemandWithdrawSelector = crypto.Keccak256([]byte("withdraw(uint256)"))[:4]
)

// stDemandFixtureCall is what the deposit fixture accepts for the conversion.
func stDemandFixtureCall(to common.Address, data []byte) bool {
	return (to == bridge.WTAO && len(data) == 36 && string(data[:4]) == string(stDemandWithdrawSelector)) ||
		stDemandStakeCall(to, data)
}

func stDemandPackWithdraw(wei int64) []byte {
	return append(append([]byte{}, stDemandWithdrawSelector...), common.LeftPadBytes(big.NewInt(wei).Bytes(), 32)...)
}

func stDemandPackStake(hotkey [32]byte, amount, limit, netuid uint64) []byte {
	data := append([]byte{}, stAddStakeLimitSelector...)
	data = append(data, hotkey[:]...)
	data = append(data, common.LeftPadBytes(new(big.Int).SetUint64(amount).Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(new(big.Int).SetUint64(limit).Bytes(), 32)...)
	data = append(data, make([]byte, 32)...)
	return append(data, common.LeftPadBytes(new(big.Int).SetUint64(netuid).Bytes(), 32)...)
}

// useStDemandWallets makes demand.yml name these wallets for the test.
func useStDemandWallets(t testing.TB, cfg *StConfig) *demand.Config {
	t.Helper()
	public, secret, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Appendf(nil, "alpha_wallet:\n  address: \"%s\"\n  private_key: \"%s\"\n  hotkey: \"0x%s\"\nusdc_wallet:\n  address: \"%s\"\n  private_key: \"%s\"\nconversion:\n  enabled: true\n",
		crypto.PubkeyToAddress(cfg.DepositKey.PublicKey).Hex(), hex.EncodeToString(crypto.FromECDSA(cfg.DepositKey)), hex.EncodeToString(cfg.DepositHotkey[:]),
		solana.Base58Encode(public), solana.Base58Encode(secret))
	wallets, err := demand.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	withoutStDemandWallets(t)
	stDemandConfig = func() (*demand.Config, error) { return wallets, nil }
	return wallets
}

// withoutStDemandWallets is a vault with no demand.yml, and restores the loader.
func withoutStDemandWallets(t testing.TB) {
	t.Helper()
	previous := stDemandConfig
	stDemandConfig = func() (*demand.Config, error) { return nil, nil }
	t.Cleanup(func() { stDemandConfig = previous })
}

type stDemandFakePayer struct {
	requests []demand.Request
	result   *demand.Result
	err      error
	// Runs as Ensure does, for what changes between the sizing and the funder.
	during func()
}

func (self *stDemandFakePayer) Ensure(_ context.Context, request demand.Request) (*demand.Result, error) {
	self.requests = append(self.requests, request)
	if self.during != nil {
		self.during()
	}
	return self.result, self.err
}

// The alpha wallet must be the deposit signer on the deposit hotkey: no other
// account's stake can pay a deposit.
func TestStDemandWalletsMustBeTheDepositSigner(t *testing.T) {
	client, _, _ := newStDepositCustodyFixture(t, 1)
	wallets := useStDemandWallets(t, client.cfg)
	if err := stDemandBinding(client.cfg, wallets); err != nil {
		t.Fatal(err)
	}
	other, _, _ := newStDepositCustodyFixture(t, 2)
	if err := stDemandBinding(other.cfg, wallets); err == nil || !strings.Contains(err.Error(), "not the deposit signer") {
		t.Fatal(err)
	}
	sameKeyOtherHotkey := *client.cfg
	sameKeyOtherHotkey.DepositHotkey = [32]byte{0xee}
	if err := stDemandBinding(&sameKeyOtherHotkey, wallets); err == nil || !strings.Contains(err.Error(), "not the deposit hotkey") {
		t.Fatal(err)
	}
}

func stDemandIntent(client *CoreStClient, operation string, to common.Address, data []byte) (*model.StTransactionIntent, error) {
	logicalKey, err := stTransactionLogicalKey(client.cfg, operation)
	if err != nil {
		return nil, err
	}
	return &model.StTransactionIntent{
		LogicalKey: logicalKey, Profile: client.cfg.Profile, DeploymentId: client.cfg.DeploymentId, DeploymentKey: client.cfg.DeploymentKey(),
		ChainId: client.cfg.ChainId, GenesisHash: fmt.Sprintf("0x%x", client.cfg.GenesisHash),
		FromAddress: strings.ToLower(crypto.PubkeyToAddress(client.cfg.DepositKey.PublicKey).Hex()), ToAddress: strings.ToLower(to.Hex()),
		CalldataHash: strings.ToLower(crypto.Keccak256Hash(data).Hex()), Calldata: data,
	}, nil
}

// The deposit key signs the conversion's two calls only with demand.yml present,
// and only in their exact form: its own wTAO, its own hotkey, this subnet, whole.
func TestStDemandSignerGate(t *testing.T) {
	client, rpcClient, _ := newStDepositCustodyFixture(t, 1)
	unwrap := stDemandPackWithdraw(5)
	stake := stDemandPackStake(client.cfg.DepositHotkey, 5, 7, client.cfg.Netuid)
	admitted := func(to common.Address, data []byte) error {
		intent, err := stDemandIntent(client, "demand:test", to, data)
		if err != nil {
			t.Fatal(err)
		}
		return client.validateDepositAttempt(t.Context(), rpcClient, intent, model.StTxAttemptExecution, false)
	}
	withoutStDemandWallets(t)
	if err := admitted(bridge.WTAO, unwrap); err == nil {
		t.Fatal("without demand.yml the deposit key signed an unwrap")
	}
	useStDemandWallets(t, client.cfg)
	if err := admitted(bridge.WTAO, unwrap); err != nil {
		t.Fatal("unwrap", err)
	}
	if err := admitted(stStakingPrecompileAddress, stake); err != nil {
		t.Fatal("stake", err)
	}
	partial := append([]byte{}, stake...)
	partial[131] = 1
	transfer := append(append([]byte{}, crypto.Keccak256([]byte("transfer(address,uint256)"))[:4]...), make([]byte, 64)...)
	for name, refused := range map[string]func() error{
		"a token transfer": func() error { return admitted(bridge.WTAO, transfer) },
		"another hotkey": func() error {
			return admitted(stStakingPrecompileAddress, stDemandPackStake([32]byte{1}, 5, 7, client.cfg.Netuid))
		},
		"another subnet": func() error {
			return admitted(stStakingPrecompileAddress, stDemandPackStake(client.cfg.DepositHotkey, 5, 7, client.cfg.Netuid+1))
		},
		"a partial fill":     func() error { return admitted(stStakingPrecompileAddress, partial) },
		"withdraw elsewhere": func() error { return admitted(common.HexToAddress("0x1"), unwrap) },
	} {
		if err := refused(); err == nil {
			t.Fatal("the deposit key signed", name)
		}
	}
}

func TestStDemandStakeTakesTheStagingGasFloor(t *testing.T) {
	client, _, _ := newStDepositCustodyFixture(t, 1)
	stake := stDemandPackStake(client.cfg.DepositHotkey, 5, 7, client.cfg.Netuid)
	if floor := client.stakeMoveGasFloor(stStakingPrecompileAddress, stake, model.StTxAttemptExecution); floor != stV1DepositStagingGasFloor {
		t.Fatal(floor)
	}
	if floor := client.stakeMoveGasFloor(bridge.WTAO, stDemandPackWithdraw(5), model.StTxAttemptExecution); floor != 0 {
		t.Fatal("an unwrap moves no stake", floor)
	}
}

// stDemandShortClient is a deposit client whose alpha wallet is short of the deposit,
// with the two database steps of funding replaced.
func stDemandShortClient(t testing.TB) (*CoreStClient, *stDepositCustodyRpc) {
	t.Helper()
	client, _, fixture := newStDepositCustodyFixture(t, 1)
	client.demandSettle = func(context.Context) error { return nil }
	client.demandRetained = func(context.Context, uint64, *big.Int) (bool, error) { return false, nil }
	fixture.source.SetInt64(0)
	return client, fixture
}

// Funding asks for exactly what staging will draw, and stops the deposit until
// the wallet can pay. USDC is never spent on a deposit its limits would refuse.
func TestStDemandFunding(t *testing.T) {
	ctx := context.Background()
	const epoch = 7
	t.Run("asks for what staging draws", func(t *testing.T) {
		client, _ := stDemandShortClient(t)
		payer := &stDemandFakePayer{result: &demand.Result{Funded: true}}
		client.demandPayer = payer
		if outcome := client.fundDemandDeposit(ctx, epoch, big.NewInt(100)); outcome != nil {
			t.Fatalf("%+v", outcome)
		}
		// The principal, the reserve's two rao and the staging transfer's one.
		if len(payer.requests) != 1 || payer.requests[0].Epoch != epoch || payer.requests[0].NeedRao.Int64() != 103 {
			t.Fatalf("%+v", payer.requests)
		}
	})
	t.Run("a refusal stops the deposit and retries after the long delay", func(t *testing.T) {
		client, _ := stDemandShortClient(t)
		client.demandPayer = &stDemandFakePayer{err: fmt.Errorf("%w: short by 5", demand.ErrInsufficientUSDC)}
		outcome := client.fundDemandDeposit(ctx, epoch, big.NewInt(100))
		if outcome == nil || !outcome.Retry || !outcome.RetryAt.IsZero() || outcome.Status != model.StPublishStatusFailed || !strings.Contains(outcome.Reason, "cannot cover") {
			t.Fatalf("%+v", outcome)
		}
	})
	t.Run("a conversion under way stops the deposit and retries soon", func(t *testing.T) {
		client, _ := stDemandShortClient(t)
		client.demandPayer = &stDemandFakePayer{result: &demand.Result{Waiting: "LayerZero delivery"}}
		outcome := client.fundDemandDeposit(ctx, epoch, big.NewInt(100))
		if outcome == nil || !outcome.Retry || !strings.Contains(outcome.Reason, "LayerZero delivery") || outcome.RetryAt.IsZero() || 2*time.Minute < time.Until(outcome.RetryAt) {
			t.Fatalf("%+v", outcome)
		}
	})
	t.Run("a deposit over its cap spends nothing", func(t *testing.T) {
		client, _ := stDemandShortClient(t)
		payer := &stDemandFakePayer{result: &demand.Result{Funded: true}}
		client.demandPayer = payer
		over := new(big.Int).Add(new(big.Int).SetUint64(client.cfg.DepositEpochCapRao), big.NewInt(1))
		outcome := client.fundDemandDeposit(ctx, epoch, over)
		if outcome == nil || !outcome.Retry || len(payer.requests) != 0 {
			t.Fatalf("%+v after %d requests", outcome, len(payer.requests))
		}
	})
	t.Run("without demand.yml the deposit goes on as before", func(t *testing.T) {
		client, _, _ := newStDepositCustodyFixture(t, 1)
		settled := false
		client.demandSettle = func(context.Context) error { settled = true; return nil }
		withoutStDemandWallets(t)
		if outcome := client.fundDemandDeposit(ctx, epoch, big.NewInt(100)); outcome != nil || settled {
			t.Fatalf("%+v, settled %t", outcome, settled)
		}
	})
	// A broken file must not refuse what the wallet can already pay. Staging, which
	// runs next, is what says whether it can.
	t.Run("wallets that are not the deposit signer leave the deposit to staging", func(t *testing.T) {
		client, _, _ := newStDepositCustodyFixture(t, 1)
		other, _, _ := newStDepositCustodyFixture(t, 2)
		useStDemandWallets(t, other.cfg)
		if outcome := client.fundDemandDeposit(ctx, epoch, big.NewInt(100)); outcome != nil {
			t.Fatalf("%+v", outcome)
		}
	})
	t.Run("a demand.yml that does not parse leaves the deposit to staging", func(t *testing.T) {
		client, _, _ := newStDepositCustodyFixture(t, 1)
		withoutStDemandWallets(t)
		stDemandConfig = func() (*demand.Config, error) {
			return nil, errors.New("demand: demand.yml does not parse as the expected fields")
		}
		if outcome := client.fundDemandDeposit(ctx, epoch, big.NewInt(100)); outcome != nil {
			t.Fatalf("%+v", outcome)
		}
	})
}

// The conversion's transactions run through the same durable sender as a deposit:
// a retried operation does not send twice, the account nonce stays in order with
// the deposit's own transactions, and anything else the prepare names is refused.
func TestStDemandSendsThroughTheDurableSender(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(t, 1)
		wallets := useStDemandWallets(t, client.cfg)
		sender := &stDemandSender{core: client, key: wallets.AlphaKey()}
		ctx := context.Background()
		send := func(operation string, to common.Address, data []byte) (string, error) {
			return sender.SendPrepared(ctx, func(context.Context) (string, common.Address, []byte, error) {
				return operation, to, data, nil
			})
		}
		unwrap, err := send("demand:unwrap:7:5", bridge.WTAO, stDemandPackWithdraw(5))
		if err != nil || unwrap == "" {
			t.Fatalf("unwrap = %q %v", unwrap, err)
		}
		if again, err := send("demand:unwrap:7:5", bridge.WTAO, stDemandPackWithdraw(5)); err != nil || again != unwrap {
			t.Fatalf("a retried unwrap = %q %v", again, err)
		}
		stake, err := send("demand:stake:8:5", stStakingPrecompileAddress, stDemandPackStake(client.cfg.DepositHotkey, 5, 7, client.cfg.Netuid))
		if err != nil || stake == "" {
			t.Fatalf("stake = %q %v", stake, err)
		}
		if none, err := sender.SendPrepared(ctx, func(context.Context) (string, common.Address, []byte, error) {
			return "", common.Address{}, nil, nil
		}); err != nil || none != "" {
			t.Fatalf("nothing due = %q %v", none, err)
		}
		// The deposit's own staging takes the next nonce after them.
		if staged, err := client.stageDepositPrincipal(ctx, 7, big.NewInt(100)); err != nil || staged == "" {
			t.Fatalf("stage = %q %v", staged, err)
		}
		// What a prepare refuses stays the funder's. Only what stops before it is unsettled.
		if _, err := sender.SendPrepared(ctx, func(context.Context) (string, common.Address, []byte, error) {
			return "", common.Address{}, nil, fmt.Errorf("%w: refused in prepare", demand.ErrInsufficientGas)
		}); !errors.Is(err, demand.ErrInsufficientGas) || errors.Is(err, errStDemandUnsettled) {
			t.Fatal(err)
		}
		if _, err := (&stDemandSender{core: client}).SendPrepared(ctx, func(context.Context) (string, common.Address, []byte, error) {
			t.Fatal("prepare ran without a key")
			return "", common.Address{}, nil, nil
		}); !errors.Is(err, errStDemandUnsettled) {
			t.Fatal(err)
		}
		if _, err := send("demand:other:1", bridge.WTAO, append(append([]byte{}, crypto.Keccak256([]byte("transfer(address,uint256)"))[:4]...), make([]byte, 64)...)); err == nil || errors.Is(err, errStDemandUnsettled) {
			t.Fatal("the deposit key sent a token transfer, or called its refusal unsettled:", err)
		}
		// That refused intent now holds its nonce, and every later send stops at it.
		if _, err := sender.SendPrepared(ctx, func(context.Context) (string, common.Address, []byte, error) {
			t.Fatal("prepare ran behind an intent that cannot settle")
			return "", common.Address{}, nil, nil
		}); !errors.Is(err, errStDemandUnsettled) {
			t.Fatal(err)
		}
		fixture.stateLock.Lock()
		defer fixture.stateLock.Unlock()
		if len(fixture.sent) != 3 {
			t.Fatalf("%d transactions sent", len(fixture.sent))
		}
		for index, tx := range fixture.sent {
			if tx.Nonce() != uint64(7+index) {
				t.Fatalf("transaction %d took nonce %d", index, tx.Nonce())
			}
		}
		if fixture.sent[0].Gas() >= stV1DepositStagingGasFloor || fixture.sent[1].Gas() != stV1DepositStagingGasFloor || fixture.sent[2].Gas() != stV1DepositStagingGasFloor {
			t.Fatal("gas limits", fixture.sent[0].Gas(), fixture.sent[1].Gas(), fixture.sent[2].Gas())
		}
	})
}
