package controller

// st_controller.go — all subtensor (st) chain coordination for the UR subnet
// (sn/PLAN.md §6): the swappable `StClient` chain client (ethclient +
// sn/stabi bindings with ordered rpc failover), the epoch payout-share
// computation (usage × reliability → per-coldkey shareBps leaves → merkle
// root), the idempotent publish flows (deposit push+credit, commitOperator,
// finalizeEpoch, rollEpochs poke), the chain event sync, and the `/sn/*`
// control-plane implementations (wallet set, pool-claim proofs, epoch
// mirror).
//
// Conventions:
//   - The contract clock is authoritative. Block numbers are never mapped to
//     wall-clock except to (a) pick the usage/reliability SQL window for a
//     closed epoch (header timestamps, ~12s/block estimate fallback) and
//     (b) derive task RunAt hints. Every deadline decision is made in blocks.
//   - All α amounts are rao. Config caps/rates are uint64 rao; chain values
//     are *big.Int rao (the contract is uint256 — stctl precedent).
//   - Idempotency before EVERY send: each publish flow reads the on-chain
//     state first (root already committed? epoch finalized? deposit already
//     credited? push already sitting unaccounted?) and records a
//     model.StPublishStatusSkipped row instead of sending. Every attempt is
//     recorded via AddStPublish/UpdateStPublish.
//   - `st.yml` is loaded lazily (sync.OnceValue) and a missing/broken vault
//     resource never crashes non-st code paths: stConfig() returns nil and
//     every entry point checks it.

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	bind "github.com/ethereum/go-ethereum/accounts/abi/bind/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sn/merkle"
	"github.com/urnetwork/sn/ss58"
	"github.com/urnetwork/sn/stabi"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	stconn "github.com/urnetwork/server/st"
)

const (
	// stDialTimeout bounds each per-endpoint dial + chain id probe.
	stDialTimeout = 15 * time.Second
	// stCallTimeout bounds each view call.
	stCallTimeout = 30 * time.Second
	// stSendTimeout bounds building + broadcasting one transaction.
	stSendTimeout = 60 * time.Second
	// stWaitMinedTimeout bounds waiting for a receipt (~12s blocks). A send
	// that broadcasts but never confirms in this window is recorded failed
	// with its tx hash; the next attempt's on-chain idempotency check
	// resolves it to skipped if it actually landed.
	stWaitMinedTimeout = 3 * time.Minute

	// stDefaultBlockSeconds is the subtensor block cadence used for
	// block <-> wall-clock estimates when st.yml does not override it.
	stDefaultBlockSeconds = 12
	// stDefaultReliabilityAMin is the default assignments floor `a_min` in
	// the v1 reliability signal (VALIDATOR.md §7): a provider network
	// observed fewer than a_min times has its reliability computed against
	// a_min, damping low-sample estimates toward zero.
	stDefaultReliabilityAMin = 8

	// stCommitAlertLead is the D-11 T-2h ops alert: an unconfirmed commit
	// within this much (estimated) time of the commit deadline logs at
	// error level on every attempt.
	stCommitAlertLead = 2 * time.Hour
)

// ---------------------------------------------------------------------------
// st.yml config
// ---------------------------------------------------------------------------

// StConfig is the parsed `st.yml` vault resource:
//
//	enabled: true
//	rpc_urls:                      # ordered failover list (§11.1)
//	  - https://test.chain.opentensor.ai
//	chain_id: 945
//	contract_address: "0x..."      # STSubnet proxy
//	netuid: 350
//	no_id: 1                       # this operator's noId in the contract
//	treasury_hotkey: "0x<64 hex>"  # custody hotkey (bytes32)
//	deposit_key: "<hex privkey>"   # signs deposit push + credit
//	ops_key: "<hex privkey>"       # signs roll/commit/finalize pokes
//	deposit_alpha_rao_per_gib: 0   # α (rao) deposited per GiB of settled
//	                               # epoch payout usage; 0 disables sizing
//	deposit_epoch_cap_rao: 0       # hard per-epoch deposit cap in rao
//	                               # (D-3 custody blast radius); 0 disables
//	                               # automated deposits entirely
//	reliability_a_min: 8           # assignments floor in the v1 signal
//	block_seconds: 12              # block cadence for time estimates
//	deploy_block: 0                # first block of interest for event sync
//	                               # (contract deploy block); 0 = start at
//	                               # the head on first sync
//
// Deposit sizing (PLAN.md §6 deposit policy): the automated per-epoch
// deposit is `min(deposit_epoch_cap_rao, usage_gib × deposit_alpha_rao_per_gib)`
// where usage is the previous epoch window's total settled payout bytes
// (`transfer_escrow_sweep`). The rate is the off-chain reference rate of
// WHITEPAPER §7.1 expressed directly in rao per GiB — config, not oracle.
type StConfig struct {
	Enabled               bool
	RpcUrls               []string
	ChainId               uint64
	ContractAddress       common.Address
	Netuid                uint64
	NoId                  uint64
	TreasuryHotkey        [32]byte
	DepositKey            *ecdsa.PrivateKey
	OpsKey                *ecdsa.PrivateKey
	DepositAlphaRaoPerGib uint64
	DepositEpochCapRao    uint64
	ReliabilityAMin       int64
	BlockSeconds          int64
	DeployBlock           uint64
}

// stConfigFromVault lazily loads `st.yml`. Unlike verify.yml the resource is
// optional: a missing or malformed file logs once and yields nil, so the
// taskworker and api keep running with the st subsystem disabled.
var stConfigFromVault = sync.OnceValue(func() (cfg *StConfig) {
	defer func() {
		if r := recover(); r != nil {
			glog.Infof("[st]st.yml unavailable (st subsystem disabled): %v\n", r)
			cfg = nil
		}
	}()

	res, err := server.Vault.SimpleResource("st.yml")
	if err != nil {
		glog.Infof("[st]st.yml unavailable (st subsystem disabled): %s\n", err)
		return nil
	}
	var conf struct {
		Enabled               bool   `yaml:"enabled"`
		ChainId               uint64 `yaml:"chain_id"`
		ContractAddress       string `yaml:"contract_address"`
		Netuid                uint64 `yaml:"netuid"`
		NoId                  uint64 `yaml:"no_id"`
		TreasuryHotkey        string `yaml:"treasury_hotkey"`
		DepositKey            string `yaml:"deposit_key"`
		OpsKey                string `yaml:"ops_key"`
		DepositAlphaRaoPerGib uint64 `yaml:"deposit_alpha_rao_per_gib"`
		DepositEpochCapRao    uint64 `yaml:"deposit_epoch_cap_rao"`
		ReliabilityAMin       int64  `yaml:"reliability_a_min"`
		BlockSeconds          int64  `yaml:"block_seconds"`
		DeployBlock           uint64 `yaml:"deploy_block"`
	}
	// UnmarshalYaml panics when the file is missing; the recover above turns
	// that into a disabled subsystem.
	res.UnmarshalYaml(&conf)

	// The subtensor connection is resolved by server/st — the one place the
	// endpoints live — which threads BRINGYOUR_SUBTENSOR_HOSTNAME through
	// st.yml's `authority` (and honors an explicit rpc_urls override). Any
	// failure (missing host / authority) panics into the recover above and
	// leaves the st subsystem disabled.
	conn, connErr := stconn.GetConnection()
	if connErr != nil {
		panic(fmt.Errorf("st.yml subtensor connection: %w", connErr))
	}
	if len(conn.RpcUrls) == 0 {
		panic(fmt.Errorf("st.yml must define authority or rpc_urls"))
	}
	if !common.IsHexAddress(conf.ContractAddress) {
		panic(fmt.Errorf("st.yml contract_address is not a valid EVM address"))
	}
	treasuryHotkey, err := stParseHex32(conf.TreasuryHotkey)
	if err != nil {
		panic(fmt.Errorf("st.yml treasury_hotkey: %s", err))
	}

	cfg = &StConfig{
		Enabled:               conf.Enabled,
		RpcUrls:               conn.RpcUrls,
		ChainId:               conf.ChainId,
		ContractAddress:       common.HexToAddress(conf.ContractAddress),
		Netuid:                conf.Netuid,
		NoId:                  conf.NoId,
		TreasuryHotkey:        treasuryHotkey,
		DepositAlphaRaoPerGib: conf.DepositAlphaRaoPerGib,
		DepositEpochCapRao:    conf.DepositEpochCapRao,
		ReliabilityAMin:       conf.ReliabilityAMin,
		BlockSeconds:          conf.BlockSeconds,
		DeployBlock:           conf.DeployBlock,
	}
	if cfg.ReliabilityAMin <= 0 {
		cfg.ReliabilityAMin = stDefaultReliabilityAMin
	}
	if cfg.BlockSeconds <= 0 {
		cfg.BlockSeconds = stDefaultBlockSeconds
	}
	// keys are optional for read-only deployments; the publish flows error
	// per-call when the required key is absent
	if conf.DepositKey != "" {
		key, err := stParsePrivateKey(conf.DepositKey)
		if err != nil {
			panic(fmt.Errorf("st.yml deposit_key: %s", err))
		}
		cfg.DepositKey = key
	}
	if conf.OpsKey != "" {
		key, err := stParsePrivateKey(conf.OpsKey)
		if err != nil {
			panic(fmt.Errorf("st.yml ops_key: %s", err))
		}
		cfg.OpsKey = key
	}
	return cfg
})

// stConfigInstance, when set, overrides the vault config (the swappable
// instance pattern of `SetCoinbaseClient`, for tests).
var stConfigInstance *StConfig

func SetStConfig(cfg *StConfig) {
	stConfigInstance = cfg
}

func stConfig() *StConfig {
	if stConfigInstance != nil {
		return stConfigInstance
	}
	return stConfigFromVault()
}

// StEnabled reports whether the st subsystem is configured and enabled.
// Every st task checks this so a missing `st.yml` never crashes the worker.
func StEnabled() bool {
	cfg := stConfig()
	return cfg != nil && cfg.Enabled
}

func stParseHex32(value string) ([32]byte, error) {
	var out [32]byte
	cleaned := strings.TrimPrefix(strings.TrimSpace(value), "0x")
	raw, err := hex.DecodeString(cleaned)
	if err != nil {
		return out, err
	}
	if len(raw) != 32 {
		return out, fmt.Errorf("expected 32 bytes, got %d", len(raw))
	}
	copy(out[:], raw)
	return out, nil
}

func stParsePrivateKey(value string) (*ecdsa.PrivateKey, error) {
	cleaned := strings.TrimPrefix(strings.TrimSpace(value), "0x")
	return crypto.HexToECDSA(cleaned)
}

// ---------------------------------------------------------------------------
// StClient — the swappable chain client
// ---------------------------------------------------------------------------

// StEpochState is the contract epoch machine plus the chain head, read in
// one pass. Deadline blocks for the open epoch derive from
// `EpochStartBlock + TEpochBlocks + <window>`.
type StEpochState struct {
	Epoch                uint64
	PendingEpoch         uint64
	EpochStartBlock      uint64
	TEpochBlocks         uint64
	CommitWindowBlocks   uint64
	TrailsWindowBlocks   uint64
	FinalizeOffsetBlocks uint64
	HeadBlock            uint64
	HeadBlockTime        time.Time
}

// StPoolState is the per-(epoch, noId) contract state used for publish
// idempotency and reconciliation.
type StPoolState struct {
	// CommittedRoot is noCommit[e][noId].payoutRoot (zero when uncommitted).
	CommittedRoot [32]byte
	// Finalized is finalized[e].
	Finalized bool
	// PoolTotalRao is poolTotal[e][noId] (zero until finalized).
	PoolTotalRao *big.Int
	// ClaimedRao is claimedMiner[e][noId].
	ClaimedRao *big.Int
	// v0.4 (D25): the contract keeps no per-NO deposit ledger (DT/totalDT
	// dropped) — per-epoch deposits are read off the mirrored Deposited event
	// log via model.SumStDepositedRao, never from contract state here.
}

// StClient is the single swappable interface behind which all subtensor
// coordination happens (PLAN.md §6). The core implementation is ethclient +
// sn/stabi with ordered rpc failover; tests stub it via SetStClient.
type StClient interface {
	// Epoch reads the contract epoch machine + chain head.
	Epoch(ctx context.Context) (*StEpochState, error)
	// PendingEpoch reads pendingEpoch() — the epoch the chain is actually
	// in, ignoring unrolled lazy state.
	PendingEpoch(ctx context.Context) (uint64, error)
	// EpochCloseBlock reads epochCloseBlock(e) — the intended boundary of a
	// rolled (closed) epoch; 0 when the roll has not reached e yet.
	EpochCloseBlock(ctx context.Context, epoch uint64) (uint64, error)
	// BlockTime reads a mined block header's timestamp.
	BlockTime(ctx context.Context, block uint64) (time.Time, error)
	// RollEpochs pokes the permissionless rollEpochs() (ops key).
	RollEpochs(ctx context.Context) (txHash string, err error)
	// DepositPush moves alphaRao of the deposit wallet's mirror stake on
	// treasuryHotkey to the contract's coldkey via the StakingV2 precompile
	// transferStake — the PUSH half of push-then-credit (see stctl).
	DepositPush(ctx context.Context, alphaRao *big.Int) (txHash string, err error)
	// DepositCredit calls STSubnet.deposit(noId, alphaRao) — the CREDIT half.
	DepositCredit(ctx context.Context, noId uint64, alphaRao *big.Int) (txHash string, err error)
	// UnaccountedStakeRao reads getStake(treasuryHotkey, selfColdkey) -
	// accountedStake: α already pushed but not yet credited by deposit().
	UnaccountedStakeRao(ctx context.Context) (*big.Int, error)
	// CommitPayoutRoot calls commitOperator(e, noId, root, off) (ops key).
	CommitPayoutRoot(ctx context.Context, epoch uint64, noId uint64, root [32]byte, off []byte) (txHash string, err error)
	// FinalizeEpoch calls the permissionless finalizeEpoch(e) (ops key).
	FinalizeEpoch(ctx context.Context, epoch uint64) (txHash string, err error)
	// NextFinalizeEpoch reads nextFinalizeEpoch (finalize is in-order).
	NextFinalizeEpoch(ctx context.Context) (uint64, error)
	// SyncEvents reads + decodes the contract logs in [fromBlock, toBlock]
	// (bounded ranges — SP-4) and returns the next high-water block.
	SyncEvents(ctx context.Context, fromBlock uint64, toBlock uint64) ([]*model.StChainEvent, uint64, error)
	// PoolState reads the per-(epoch, noId) settlement state.
	PoolState(ctx context.Context, epoch uint64, noId uint64) (*StPoolState, error)
}

// stClientInstance, when set, overrides the core client (for tests).
var stClientInstance StClient

func SetStClient(client StClient) {
	stClientInstance = client
}

var coreStClientFromConfig = sync.OnceValue(func() StClient {
	cfg := stConfig()
	if cfg == nil {
		return nil
	}
	return &CoreStClient{
		cfg:     cfg,
		st:      stabi.NewSTSubnet(),
		clients: map[string]*ethclient.Client{},
	}
})

// stClient returns the active StClient, or nil when the subsystem is not
// configured.
func stClient() StClient {
	if stClientInstance != nil {
		return stClientInstance
	}
	return coreStClientFromConfig()
}

// CoreStClient implements StClient over ethclient + sn/stabi, trying the
// configured rpc urls in order per call (§11.1 ordered failover).
type CoreStClient struct {
	cfg *StConfig
	st  *stabi.STSubnet

	stateLock sync.Mutex
	// clients caches one dialed (chain-id-verified) client per rpc url
	clients map[string]*ethclient.Client
}

// client returns a dialed, chain-id-verified client for one rpc url.
func (self *CoreStClient) client(ctx context.Context, url string) (*ethclient.Client, error) {
	self.stateLock.Lock()
	client, ok := self.clients[url]
	self.stateLock.Unlock()
	if ok {
		return client, nil
	}

	dialCtx, cancel := context.WithTimeout(ctx, stDialTimeout)
	defer cancel()
	client, err := ethclient.DialContext(dialCtx, url)
	if err != nil {
		return nil, err
	}
	chainId, err := client.ChainID(dialCtx)
	if err != nil {
		client.Close()
		return nil, err
	}
	if chainId.Cmp(new(big.Int).SetUint64(self.cfg.ChainId)) != 0 {
		client.Close()
		return nil, fmt.Errorf("rpc %s reports chain id %s, st.yml expects %d", url, chainId, self.cfg.ChainId)
	}

	self.stateLock.Lock()
	if existing, ok := self.clients[url]; ok {
		// another goroutine won the dial race
		client.Close()
		client = existing
	} else {
		self.clients[url] = client
	}
	self.stateLock.Unlock()
	return client, nil
}

// dropClient forgets a cached client after a transport-level failure so the
// next call re-dials.
func (self *CoreStClient) dropClient(url string, client *ethclient.Client) {
	self.stateLock.Lock()
	if self.clients[url] == client {
		delete(self.clients, url)
	}
	self.stateLock.Unlock()
	client.Close()
}

// eachRpc runs op against each rpc url in order until one succeeds. Safe
// for reads and idempotent probes; sends acquire a single endpoint instead
// (see send) so a transaction is never broadcast twice.
func (self *CoreStClient) eachRpc(ctx context.Context, op func(client *ethclient.Client) error) error {
	var errs []error
	for _, url := range self.cfg.RpcUrls {
		client, err := self.client(ctx, url)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			continue
		}
		if err := op(client); err != nil {
			self.dropClient(url, client)
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			continue
		}
		return nil
	}
	return fmt.Errorf("st: no rpc endpoint answered: %w", errors.Join(errs...))
}

// view performs one contract read with rpc failover.
func stView[T any](self *CoreStClient, ctx context.Context, calldata []byte, unpack func([]byte) (T, error)) (T, error) {
	var out T
	err := self.eachRpc(ctx, func(client *ethclient.Client) error {
		callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
		defer cancel()
		bound := self.st.Instance(client, self.cfg.ContractAddress)
		value, err := bind.Call(bound, &bind.CallOpts{Context: callCtx}, calldata, unpack)
		if err != nil {
			return err
		}
		out = value
		return nil
	})
	return out, err
}

// send signs and broadcasts calldata to `to` from `key`, waits for the
// receipt, and returns the tx hash. It picks the FIRST answering endpoint
// and never retries a broadcast on another endpoint (a timed-out
// broadcast may still land; the callers' on-chain idempotency checks
// absorb that).
func (self *CoreStClient) send(ctx context.Context, key *ecdsa.PrivateKey, to common.Address, calldata []byte) (string, error) {
	if key == nil {
		return "", fmt.Errorf("st: signing key not configured in st.yml")
	}

	var client *ethclient.Client
	var errs []error
	for _, url := range self.cfg.RpcUrls {
		dialed, err := self.client(ctx, url)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			continue
		}
		client = dialed
		break
	}
	if client == nil {
		return "", fmt.Errorf("st: no rpc endpoint answered: %w", errors.Join(errs...))
	}

	// RawTransact never consults the ABI, so an empty one binds any target
	// (contract or precompile) — the stctl precedent.
	bound := bind.NewBoundContract(to, abi.ABI{}, client, client, client)
	opts := bind.NewKeyedTransactor(key, new(big.Int).SetUint64(self.cfg.ChainId))
	sendCtx, cancelSend := context.WithTimeout(ctx, stSendTimeout)
	defer cancelSend()
	opts.Context = sendCtx

	tx, err := bound.RawTransact(opts, calldata)
	if err != nil {
		return "", fmt.Errorf("st: send tx to %s: %w", to, err)
	}
	txHash := tx.Hash().Hex()

	waitCtx, cancelWait := context.WithTimeout(ctx, stWaitMinedTimeout)
	defer cancelWait()
	receipt, err := bind.WaitMined(waitCtx, client, tx.Hash())
	if err != nil {
		return txHash, fmt.Errorf("st: wait mined %s: %w", txHash, err)
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return txHash, fmt.Errorf("st: transaction %s reverted", txHash)
	}
	return txHash, nil
}

func (self *CoreStClient) Epoch(ctx context.Context) (*StEpochState, error) {
	state := &StEpochState{}

	epoch, err := stView(self, ctx, self.st.PackEpoch(), self.st.UnpackEpoch)
	if err != nil {
		return nil, fmt.Errorf("epoch(): %w", err)
	}
	state.Epoch = epoch.Uint64()
	pending, err := stView(self, ctx, self.st.PackPendingEpoch(), self.st.UnpackPendingEpoch)
	if err != nil {
		return nil, fmt.Errorf("pendingEpoch(): %w", err)
	}
	state.PendingEpoch = pending.Uint64()
	if state.EpochStartBlock, err = stView(self, ctx, self.st.PackEpochStartBlock(), self.st.UnpackEpochStartBlock); err != nil {
		return nil, fmt.Errorf("epochStartBlock(): %w", err)
	}
	if state.TEpochBlocks, err = stView(self, ctx, self.st.PackTEpoch(), self.st.UnpackTEpoch); err != nil {
		return nil, fmt.Errorf("tEpoch(): %w", err)
	}
	if state.CommitWindowBlocks, err = stView(self, ctx, self.st.PackCommitWindowBlocks(), self.st.UnpackCommitWindowBlocks); err != nil {
		return nil, fmt.Errorf("commitWindowBlocks(): %w", err)
	}
	if state.TrailsWindowBlocks, err = stView(self, ctx, self.st.PackTrailsWindowBlocks(), self.st.UnpackTrailsWindowBlocks); err != nil {
		return nil, fmt.Errorf("trailsWindowBlocks(): %w", err)
	}
	if state.FinalizeOffsetBlocks, err = stView(self, ctx, self.st.PackFinalizeOffsetBlocks(), self.st.UnpackFinalizeOffsetBlocks); err != nil {
		return nil, fmt.Errorf("finalizeOffsetBlocks(): %w", err)
	}

	err = self.eachRpc(ctx, func(client *ethclient.Client) error {
		callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
		defer cancel()
		header, err := client.HeaderByNumber(callCtx, nil)
		if err != nil {
			return err
		}
		state.HeadBlock = header.Number.Uint64()
		state.HeadBlockTime = time.Unix(int64(header.Time), 0).UTC()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return state, nil
}

func (self *CoreStClient) PendingEpoch(ctx context.Context) (uint64, error) {
	pending, err := stView(self, ctx, self.st.PackPendingEpoch(), self.st.UnpackPendingEpoch)
	if err != nil {
		return 0, err
	}
	return pending.Uint64(), nil
}

func (self *CoreStClient) EpochCloseBlock(ctx context.Context, epoch uint64) (uint64, error) {
	return stView(self, ctx, self.st.PackEpochCloseBlock(new(big.Int).SetUint64(epoch)), self.st.UnpackEpochCloseBlock)
}

func (self *CoreStClient) BlockTime(ctx context.Context, block uint64) (time.Time, error) {
	var blockTime time.Time
	err := self.eachRpc(ctx, func(client *ethclient.Client) error {
		callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
		defer cancel()
		header, err := client.HeaderByNumber(callCtx, new(big.Int).SetUint64(block))
		if err != nil {
			return err
		}
		blockTime = time.Unix(int64(header.Time), 0).UTC()
		return nil
	})
	return blockTime, err
}

func (self *CoreStClient) RollEpochs(ctx context.Context) (string, error) {
	return self.send(ctx, self.cfg.OpsKey, self.cfg.ContractAddress, self.st.PackRollEpochs())
}

// stStakingPrecompileAddress is the subtensor StakingV2 precompile
// (vendored at sn/evm/src/interfaces/stakingV2.sol, subtensor v3.2.7; ABI
// unverified against the live runtime — SP-1 gated).
var stStakingPrecompileAddress = common.HexToAddress("0x0000000000000000000000000000000000000805")

var stAbiBytes32Type = func() abi.Type {
	t, err := abi.NewType("bytes32", "", nil)
	if err != nil {
		panic(err)
	}
	return t
}()

var stAbiUint256Type = func() abi.Type {
	t, err := abi.NewType("uint256", "", nil)
	if err != nil {
		panic(err)
	}
	return t
}()

// stPackTransferStake hand-packs IStaking.transferStake — the push half of
// push-then-credit (crib of stctl/staking.go).
func stPackTransferStake(destinationColdkey [32]byte, hotkey [32]byte, netuid uint64, amount *big.Int) ([]byte, error) {
	arguments := abi.Arguments{
		{Name: "destination_coldkey", Type: stAbiBytes32Type},
		{Name: "hotkey", Type: stAbiBytes32Type},
		{Name: "origin_netuid", Type: stAbiUint256Type},
		{Name: "destination_netuid", Type: stAbiUint256Type},
		{Name: "amount", Type: stAbiUint256Type},
	}
	netuidBig := new(big.Int).SetUint64(netuid)
	packed, err := arguments.Pack(destinationColdkey, hotkey, netuidBig, netuidBig, amount)
	if err != nil {
		return nil, fmt.Errorf("pack transferStake: %w", err)
	}
	selector := crypto.Keccak256([]byte("transferStake(bytes32,bytes32,uint256,uint256,uint256)"))[:4]
	return append(selector, packed...), nil
}

// stPackGetStake hand-packs IStaking.getStake(hotkey, coldkey, netuid).
func stPackGetStake(hotkey [32]byte, coldkey [32]byte, netuid uint64) ([]byte, error) {
	arguments := abi.Arguments{
		{Name: "hotkey", Type: stAbiBytes32Type},
		{Name: "coldkey", Type: stAbiBytes32Type},
		{Name: "netuid", Type: stAbiUint256Type},
	}
	packed, err := arguments.Pack(hotkey, coldkey, new(big.Int).SetUint64(netuid))
	if err != nil {
		return nil, fmt.Errorf("pack getStake: %w", err)
	}
	selector := crypto.Keccak256([]byte("getStake(bytes32,bytes32,uint256)"))[:4]
	return append(selector, packed...), nil
}

func (self *CoreStClient) DepositPush(ctx context.Context, alphaRao *big.Int) (string, error) {
	// destination coldkey = mirror(contract proxy): the contract's own
	// substrate account (push-then-credit; see STSubnet.deposit)
	destColdkey := ss58.EvmMirrorPubkey(self.cfg.ContractAddress)
	calldata, err := stPackTransferStake(destColdkey, self.cfg.TreasuryHotkey, self.cfg.Netuid, alphaRao)
	if err != nil {
		return "", err
	}
	return self.send(ctx, self.cfg.DepositKey, stStakingPrecompileAddress, calldata)
}

func (self *CoreStClient) DepositCredit(ctx context.Context, noId uint64, alphaRao *big.Int) (string, error) {
	calldata := self.st.PackDeposit(new(big.Int).SetUint64(noId), alphaRao)
	return self.send(ctx, self.cfg.DepositKey, self.cfg.ContractAddress, calldata)
}

func (self *CoreStClient) UnaccountedStakeRao(ctx context.Context) (*big.Int, error) {
	// selfColdkey is read from the contract (authoritative — the
	// setSelfColdkey SP-1 escape hatch may have overridden the mirror)
	selfColdkey, err := stView(self, ctx, self.st.PackSelfColdkey(), self.st.UnpackSelfColdkey)
	if err != nil {
		return nil, fmt.Errorf("selfColdkey(): %w", err)
	}
	accounted, err := stView(self, ctx, self.st.PackAccountedStake(), self.st.UnpackAccountedStake)
	if err != nil {
		return nil, fmt.Errorf("accountedStake(): %w", err)
	}
	calldata, err := stPackGetStake(self.cfg.TreasuryHotkey, selfColdkey, self.cfg.Netuid)
	if err != nil {
		return nil, err
	}
	var stake *big.Int
	err = self.eachRpc(ctx, func(client *ethclient.Client) error {
		callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
		defer cancel()
		out, err := client.CallContract(callCtx, ethereum.CallMsg{
			To:   &stStakingPrecompileAddress,
			Data: calldata,
		}, nil)
		if err != nil {
			return err
		}
		if len(out) < 32 {
			return fmt.Errorf("getStake returned %d bytes", len(out))
		}
		stake = new(big.Int).SetBytes(out[:32])
		return nil
	})
	if err != nil {
		return nil, err
	}
	unaccounted := new(big.Int).Sub(stake, accounted)
	if unaccounted.Sign() < 0 {
		unaccounted.SetInt64(0)
	}
	return unaccounted, nil
}

func (self *CoreStClient) CommitPayoutRoot(ctx context.Context, epoch uint64, noId uint64, root [32]byte, off []byte) (string, error) {
	calldata := self.st.PackCommitOperator(
		new(big.Int).SetUint64(epoch),
		new(big.Int).SetUint64(noId),
		root,
		off,
	)
	return self.send(ctx, self.cfg.OpsKey, self.cfg.ContractAddress, calldata)
}

func (self *CoreStClient) FinalizeEpoch(ctx context.Context, epoch uint64) (string, error) {
	return self.send(ctx, self.cfg.OpsKey, self.cfg.ContractAddress, self.st.PackFinalizeEpoch(new(big.Int).SetUint64(epoch)))
}

func (self *CoreStClient) NextFinalizeEpoch(ctx context.Context) (uint64, error) {
	next, err := stView(self, ctx, self.st.PackNextFinalizeEpoch(), self.st.UnpackNextFinalizeEpoch)
	if err != nil {
		return 0, err
	}
	return next.Uint64(), nil
}

func (self *CoreStClient) PoolState(ctx context.Context, epoch uint64, noId uint64) (*StPoolState, error) {
	e := new(big.Int).SetUint64(epoch)
	n := new(big.Int).SetUint64(noId)

	commit, err := stView(self, ctx, self.st.PackNoCommit(e, n), self.st.UnpackNoCommit)
	if err != nil {
		return nil, fmt.Errorf("noCommit(): %w", err)
	}
	finalized, err := stView(self, ctx, self.st.PackFinalized(e), self.st.UnpackFinalized)
	if err != nil {
		return nil, fmt.Errorf("finalized(): %w", err)
	}
	poolTotal, err := stView(self, ctx, self.st.PackPoolTotal(e, n), self.st.UnpackPoolTotal)
	if err != nil {
		return nil, fmt.Errorf("poolTotal(): %w", err)
	}
	claimed, err := stView(self, ctx, self.st.PackClaimedMiner(e, n), self.st.UnpackClaimedMiner)
	if err != nil {
		return nil, fmt.Errorf("claimedMiner(): %w", err)
	}
	// v0.4 (D25): no DT()/totalDT read — the contract dropped the deposit
	// ledger; deposits come from the Deposited event log (SumStDepositedRao).
	return &StPoolState{
		CommittedRoot: commit.PayoutRoot,
		Finalized:     finalized,
		PoolTotalRao:  poolTotal,
		ClaimedRao:    claimed,
	}, nil
}

// stEventDecoder adapts one generated Unpack<Name>Event into (kind, args).
type stEventDecoder func(log *types.Log) (string, map[string]any, bool)

func stEvent[E any](kind string, unpack func(*types.Log) (*E, error), format func(*E) map[string]any) stEventDecoder {
	return func(log *types.Log) (string, map[string]any, bool) {
		parsed, err := unpack(log)
		if err != nil {
			return "", nil, false
		}
		return kind, format(parsed), true
	}
}

// stEventDecoders lists the settlement-relevant STSubnet events mirrored
// into `st_event`. DataJson number fields are decimal strings (uint256-safe);
// 32-byte values and addresses are 0x-hex.
func (self *CoreStClient) stEventDecoders() []stEventDecoder {
	st := self.st
	return []stEventDecoder{
		stEvent("EpochRolled", st.UnpackEpochRolledEvent, func(e *stabi.STSubnetEpochRolled) map[string]any {
			return map[string]any{
				"closed_epoch": e.ClosedEpoch.String(),
				"new_epoch":    e.NewEpoch.String(),
				"close_block":  fmt.Sprintf("%d", e.CloseBlock),
			}
		}),
		stEvent("Deposited", st.UnpackDepositedEvent, func(e *stabi.STSubnetDeposited) map[string]any {
			return map[string]any{
				"e":      e.E.String(),
				"no_id":  e.NoId.String(),
				"from":   e.From.Hex(),
				"amount": e.Amount.String(),
			}
		}),
		// Buyback reserve audit trail (WHITEPAPER §7.4 / D23): every deposit's
		// full amount moves onto the locked reserve hotkey; buyback_total is
		// the running locked reserve.
		stEvent("BuybackReserved", st.UnpackBuybackReservedEvent, func(e *stabi.STSubnetBuybackReserved) map[string]any {
			return map[string]any{
				"e":             e.E.String(),
				"no_id":         e.NoId.String(),
				"amount":        e.Amount.String(),
				"buyback_total": e.BuybackTotal.String(),
			}
		}),
		stEvent("OperatorCommitted", st.UnpackOperatorCommittedEvent, func(e *stabi.STSubnetOperatorCommitted) map[string]any {
			return map[string]any{
				"e":           e.E.String(),
				"no_id":       e.NoId.String(),
				"payout_root": fmt.Sprintf("0x%x", e.PayoutRoot),
				"off":         fmt.Sprintf("0x%x", e.Off),
			}
		}),
		// v0.4 (D25): EpochFinalized lost its totalDT field (the contract no
		// longer weights deposits) — emit just the epoch.
		stEvent("EpochFinalized", st.UnpackEpochFinalizedEvent, func(e *stabi.STSubnetEpochFinalized) map[string]any {
			return map[string]any{
				"e": e.E.String(),
			}
		}),
		stEvent("PoolFinalized", st.UnpackPoolFinalizedEvent, func(e *stabi.STSubnetPoolFinalized) map[string]any {
			return map[string]any{
				"e":          e.E.String(),
				"no_id":      e.NoId.String(),
				"pool_total": e.PoolTotal.String(),
			}
		}),
		stEvent("PoolCarried", st.UnpackPoolCarriedEvent, func(e *stabi.STSubnetPoolCarried) map[string]any {
			return map[string]any{
				"e":       e.E.String(),
				"no_id":   e.NoId.String(),
				"carried": e.Carried.String(),
			}
		}),
		stEvent("PoolSwept", st.UnpackPoolSweptEvent, func(e *stabi.STSubnetPoolSwept) map[string]any {
			return map[string]any{
				"no_id":    e.NoId.String(),
				"measured": e.Measured.String(),
				"swept":    e.Swept.String(),
				"move_ok":  e.MoveOk,
			}
		}),
		stEvent("MinerClaimed", st.UnpackMinerClaimedEvent, func(e *stabi.STSubnetMinerClaimed) map[string]any {
			return map[string]any{
				"e":         e.E.String(),
				"no_id":     e.NoId.String(),
				"coldkey":   fmt.Sprintf("0x%x", e.Coldkey),
				"share_bps": e.ShareBps.String(),
				"amount":    e.Amount.String(),
				"caller":    e.Caller.Hex(),
			}
		}),
		// Head-binding registry (WHITEPAPER §8.4/§11.4): `ckey` is the promoted
		// provider's client public key (the contract's bytes32 clientId, not a
		// server UUID). The sync task mirrors these into st_head_binding to
		// exclude promoted providers from pool payouts.
		stEvent("HeadBound", st.UnpackHeadBoundEvent, func(e *stabi.STSubnetHeadBound) map[string]any {
			return map[string]any{
				"ckey":       fmt.Sprintf("0x%x", e.ClientId),
				"hotkey":     fmt.Sprintf("0x%x", e.Hotkey),
				"uid":        fmt.Sprintf("%d", e.Uid),
				"registrant": e.Registrant.Hex(),
			}
		}),
		stEvent("HeadUnbound", st.UnpackHeadUnboundEvent, func(e *stabi.STSubnetHeadUnbound) map[string]any {
			return map[string]any{
				"ckey":       fmt.Sprintf("0x%x", e.ClientId),
				"hotkey":     fmt.Sprintf("0x%x", e.Hotkey),
				"uid":        fmt.Sprintf("%d", e.Uid),
				"registrant": e.Registrant.Hex(),
			}
		}),
	}
}

func (self *CoreStClient) SyncEvents(ctx context.Context, fromBlock uint64, toBlock uint64) ([]*model.StChainEvent, uint64, error) {
	if toBlock < fromBlock {
		return nil, fromBlock, nil
	}
	var logs []types.Log
	err := self.eachRpc(ctx, func(client *ethclient.Client) error {
		callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
		defer cancel()
		out, err := client.FilterLogs(callCtx, ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(fromBlock),
			ToBlock:   new(big.Int).SetUint64(toBlock),
			Addresses: []common.Address{self.cfg.ContractAddress},
		})
		if err != nil {
			return err
		}
		logs = out
		return nil
	})
	if err != nil {
		return nil, fromBlock, err
	}

	decoders := self.stEventDecoders()
	events := []*model.StChainEvent{}
	for i := range logs {
		log := &logs[i]
		if log.Removed {
			continue
		}
		kind := ""
		var args map[string]any
		for _, decode := range decoders {
			if k, a, ok := decode(log); ok {
				kind = k
				args = a
				break
			}
		}
		if kind == "" {
			// governance/registry events are not mirrored — state polling
			// covers parameter changes
			continue
		}
		argsJson, err := json.Marshal(args)
		if err != nil {
			return nil, fromBlock, err
		}
		events = append(events, &model.StChainEvent{
			BlockNumber: log.BlockNumber,
			LogIndex:    int(log.Index),
			TxHash:      log.TxHash.Hex(),
			Kind:        kind,
			DataJson:    string(argsJson),
		})
	}
	return events, toBlock + 1, nil
}

// ---------------------------------------------------------------------------
// Pure epoch computation (unit-testable without pg/redis/chain)
// ---------------------------------------------------------------------------

// StShareEntry is one (network, coldkey) input row of the payout-share
// computation: the network's settled usage in the epoch window and its
// summed verify counters.
type StShareEntry struct {
	NetworkId     server.Id
	Coldkey       [32]byte
	UsageBytes    int64
	Assignments   int64
	Confirmations int64
}

// StPayoutShare is one computed leaf value: a coldkey's share of the epoch
// pool in basis points. Results are ordered by ascending coldkey bytes —
// the canonical leaf order (LeafIndex = slice index).
type StPayoutShare struct {
	NetworkId server.Id
	Coldkey   [32]byte
	ShareBps  int
}

// stBuildShareEntries assembles the per-network payout inputs for an epoch:
// one entry per network that has settled usage AND a claim wallet (networks
// without a wallet drop out of the normalization, redistributing their share
// over the claimable set rather than burning it), carrying the network's
// summed verify counters.
//
// It also applies the head-tier exclusion (WHITEPAPER §8.4/§11.4): a network
// whose contributing verify client resolves (client_id -> ckey) to an active
// head binding is dropped entirely, so a provider promoted to the head tier —
// already paid natively by Yuma — never contributes to a pool payoutRoot
// (never paid twice). Usage is per-network (transfer_escrow_sweep is not
// client-attributed), so the network is the finest unit at which a promoted
// provider can be removed; a promoted provider only ever earns a leaf when it
// has verify confirmations, and those confirmations name the client_id, so the
// reliability set is exactly the right exclusion driver.
//
// Pure: the caller hoists the usage/reliability reads, the claim-wallet map,
// the contributing client_id -> ckey map, and the active head-binding ckey
// set. With no head bindings (nil/empty ckey set) the result matches the
// unfiltered computation exactly.
func stBuildShareEntries(
	usages []*model.StNetworkUsage,
	reliabilities []*model.StClientReliability,
	wallets map[server.Id][32]byte,
	clientCkeys map[server.Id][32]byte,
	headBoundCkeys map[[32]byte]bool,
) []*StShareEntry {
	// networks with a head-bound contributing client are excluded wholesale
	excludedNetworkIds := map[server.Id]bool{}
	if 0 < len(headBoundCkeys) {
		for _, reliability := range reliabilities {
			if ckey, ok := clientCkeys[reliability.ClientId]; ok && headBoundCkeys[ckey] {
				excludedNetworkIds[reliability.NetworkId] = true
			}
		}
	}

	// aggregate the per-(client, network) verify counters per network
	type networkReliability struct {
		assignments   int64
		confirmations int64
	}
	byNetwork := map[server.Id]*networkReliability{}
	for _, reliability := range reliabilities {
		if excludedNetworkIds[reliability.NetworkId] {
			continue
		}
		aggregate, ok := byNetwork[reliability.NetworkId]
		if !ok {
			aggregate = &networkReliability{}
			byNetwork[reliability.NetworkId] = aggregate
		}
		aggregate.assignments += reliability.Assignments
		aggregate.confirmations += reliability.Confirmations
	}

	entries := []*StShareEntry{}
	for _, usage := range usages {
		if excludedNetworkIds[usage.NetworkId] {
			continue
		}
		coldkey, ok := wallets[usage.NetworkId]
		if !ok {
			continue
		}
		entry := &StShareEntry{
			NetworkId:  usage.NetworkId,
			Coldkey:    coldkey,
			UsageBytes: usage.PayoutByteCount,
		}
		if aggregate, ok := byNetwork[usage.NetworkId]; ok {
			entry.Assignments = aggregate.assignments
			entry.Confirmations = aggregate.confirmations
		}
		entries = append(entries, entry)
	}
	return entries
}

// stComputePayoutShares implements the WHITEPAPER §8.2 v1 share basis:
//
//	weight_n   = usage_bytes_n × reliability_n
//	reliability_n = confirmations_n / max(assignments_n, a_min)
//
// aggregated per coldkey (the contract dedups claims by (noId, coldkey) —
// exactly one leaf per coldkey), then floored to basis points of the total
// weight: shareBps = floor(weight × 10000 / Σ weight). Plain floor — no
// largest-remainder correction; Σ shareBps <= 10000 and the sub-bps
// remainder stays in the pool (rolls over on-chain by design, §8.3).
//
// Zero-weight and zero-bps entries produce no leaf (the contract rejects
// shareBps == 0 claims). Exact big.Rat arithmetic + canonical internal
// ordering make the output deterministic regardless of input order. A
// coldkey backing multiple networks keeps the smallest network id as its
// representative (informational only).
func stComputePayoutShares(entries []*StShareEntry, aMin int64) []*StPayoutShare {
	if aMin < 1 {
		aMin = 1
	}

	type coldkeyWeight struct {
		weight    *big.Rat
		networkId server.Id
	}
	byColdkey := map[[32]byte]*coldkeyWeight{}
	for _, entry := range entries {
		if entry.UsageBytes <= 0 || entry.Confirmations <= 0 {
			continue
		}
		confirmations := entry.Confirmations
		if entry.Assignments < confirmations {
			// defensive: confirmations can never exceed assignments
			confirmations = entry.Assignments
		}
		denominator := entry.Assignments
		if denominator < aMin {
			denominator = aMin
		}
		weight := new(big.Rat).SetFrac(
			new(big.Int).Mul(big.NewInt(entry.UsageBytes), big.NewInt(confirmations)),
			big.NewInt(denominator),
		)
		if weight.Sign() <= 0 {
			continue
		}
		if aggregate, ok := byColdkey[entry.Coldkey]; ok {
			aggregate.weight.Add(aggregate.weight, weight)
			if entry.NetworkId.Less(aggregate.networkId) {
				aggregate.networkId = entry.NetworkId
			}
		} else {
			byColdkey[entry.Coldkey] = &coldkeyWeight{
				weight:    weight,
				networkId: entry.NetworkId,
			}
		}
	}
	if len(byColdkey) == 0 {
		return nil
	}

	// canonical leaf order: ascending coldkey bytes
	coldkeys := make([][32]byte, 0, len(byColdkey))
	totalWeight := new(big.Rat)
	for coldkey, aggregate := range byColdkey {
		coldkeys = append(coldkeys, coldkey)
		totalWeight.Add(totalWeight, aggregate.weight)
	}
	if totalWeight.Sign() <= 0 {
		return nil
	}
	sort.Slice(coldkeys, func(a, b int) bool {
		return string(coldkeys[a][:]) < string(coldkeys[b][:])
	})

	shares := []*StPayoutShare{}
	for _, coldkey := range coldkeys {
		aggregate := byColdkey[coldkey]
		// shareBps = floor(weight * 10000 / totalWeight), exact
		scaled := new(big.Rat).Mul(aggregate.weight, big.NewRat(10000, 1))
		scaled.Quo(scaled, totalWeight)
		shareBps := new(big.Int).Quo(scaled.Num(), scaled.Denom())
		if shareBps.Sign() <= 0 {
			continue
		}
		shares = append(shares, &StPayoutShare{
			NetworkId: aggregate.networkId,
			Coldkey:   coldkey,
			ShareBps:  int(shareBps.Int64()),
		})
	}
	return shares
}

// stEstimateBlockTime maps a block number to an estimated wall-clock time
// from a (headBlock, headTime) anchor at ~blockSeconds per block. Works for
// past and future blocks. Estimates only — the contract clock (blocks) is
// authoritative for every deadline decision.
func stEstimateBlockTime(headBlock uint64, headTime time.Time, block uint64, blockSeconds int64) time.Time {
	if blockSeconds <= 0 {
		blockSeconds = stDefaultBlockSeconds
	}
	if headBlock <= block {
		return headTime.Add(time.Duration(block-headBlock) * time.Duration(blockSeconds) * time.Second)
	}
	return headTime.Add(-time.Duration(headBlock-block) * time.Duration(blockSeconds) * time.Second)
}

// StEstimateBlockTime is stEstimateBlockTime with the configured block
// cadence — the block -> RunAt hint used by the st tasks.
func StEstimateBlockTime(headBlock uint64, headTime time.Time, block uint64) time.Time {
	blockSeconds := int64(stDefaultBlockSeconds)
	if cfg := stConfig(); cfg != nil {
		blockSeconds = cfg.BlockSeconds
	}
	return stEstimateBlockTime(headBlock, headTime, block, blockSeconds)
}

// stDepositSizeRao sizes the automated epoch deposit:
// min(capRao, usageBytes × alphaRaoPerGib / GiB), in rao. A zero rate or a
// zero cap disables automated deposits (returns 0).
func stDepositSizeRao(usageBytes int64, alphaRaoPerGib uint64, capRao uint64) *big.Int {
	if usageBytes <= 0 || alphaRaoPerGib == 0 || capRao == 0 {
		return big.NewInt(0)
	}
	amount := new(big.Int).Mul(big.NewInt(usageBytes), new(big.Int).SetUint64(alphaRaoPerGib))
	amount.Quo(amount, big.NewInt(1<<30))
	capBig := new(big.Int).SetUint64(capRao)
	if 0 < amount.Cmp(capBig) {
		amount.Set(capBig)
	}
	return amount
}

// stBuildPayoutTree rebuilds the canonical payout Merkle tree from stored
// leaves. Leaves MUST be in LeafIndex order (GetStPayoutLeaves order) so
// Tree.ProofAt(leaf.LeafIndex) addresses the right leaf.
func stBuildPayoutTree(leaves []*model.StPayoutLeaf) (*merkle.Tree, error) {
	merkleLeaves := make([]merkle.Leaf, len(leaves))
	for i, leaf := range leaves {
		merkleLeaves[i] = merkle.PayoutLeaf(leaf.Coldkey, big.NewInt(int64(leaf.ShareBps)))
	}
	return merkle.NewTree(merkleLeaves)
}

// stEpochRowFromState maps the live contract state to the open epoch's
// mirror row (all fields in contract block numbers).
func stEpochRowFromState(state *StEpochState) *model.StEpoch {
	closeBlock := state.EpochStartBlock + state.TEpochBlocks
	return &model.StEpoch{
		Epoch:               state.Epoch,
		StartBlock:          state.EpochStartBlock,
		CommitDeadlineBlock: closeBlock + state.CommitWindowBlocks,
		TrailsDeadlineBlock: closeBlock + state.TrailsWindowBlocks,
		FinalizeBlock:       closeBlock + state.FinalizeOffsetBlocks,
		Status:              model.StEpochStatusOpen,
	}
}

// ---------------------------------------------------------------------------
// Publish flows (st_work + bringyourctl st entry points)
// ---------------------------------------------------------------------------

// StPublishOutcome is the result of one idempotent publish flow.
type StPublishOutcome struct {
	// Status is the model.StPublishStatus* the flow resolved to.
	Status string
	TxHash string
	Reason string
	// Retry is set when the flow should be re-attempted later (transient
	// failure or a window that has not opened yet).
	Retry bool
	// RetryAt optionally hints when (block-deadline derived).
	RetryAt time.Time
}

func (self *StPublishOutcome) String() string {
	parts := []string{self.Status}
	if self.TxHash != "" {
		parts = append(parts, self.TxHash)
	}
	if self.Reason != "" {
		parts = append(parts, self.Reason)
	}
	if self.Retry {
		parts = append(parts, "retry")
	}
	return strings.Join(parts, " ")
}

var errStNotConfigured = errors.New("st: not configured (st.yml missing or disabled)")

// stRequire returns the config + client or the not-configured error.
func stRequire() (*StConfig, StClient, error) {
	cfg := stConfig()
	client := stClient()
	if cfg == nil || client == nil {
		return nil, nil, errStNotConfigured
	}
	return cfg, client, nil
}

// stBlockTimeOrEstimate reads a mined block's header time, falling back to
// the ~12s/block estimate from the head anchor.
func stBlockTimeOrEstimate(ctx context.Context, client StClient, state *StEpochState, block uint64, blockSeconds int64) time.Time {
	if block <= state.HeadBlock {
		if blockTime, err := client.BlockTime(ctx, block); err == nil {
			return blockTime
		}
	}
	return stEstimateBlockTime(state.HeadBlock, state.HeadBlockTime, block, blockSeconds)
}

// stEpochWindow resolves a closed epoch's [startTime, endTime) wall-clock
// usage window and its boundary blocks [startBlock, closeBlock] (the latter
// drives the head-tier exclusion, which must key off blocks, not wall time).
func stEpochWindow(ctx context.Context, client StClient, state *StEpochState, epoch uint64, blockSeconds int64) (startTime time.Time, endTime time.Time, startBlock uint64, closeBlock uint64, err error) {
	closeBlock, err = client.EpochCloseBlock(ctx, epoch)
	if err != nil {
		return time.Time{}, time.Time{}, 0, 0, err
	}
	if row := model.GetStEpoch(ctx, epoch); row != nil {
		startBlock = row.StartBlock
		if closeBlock == 0 {
			// contract roll has not reached e yet: the intended boundary is
			// still start + tEpoch
			closeBlock = row.StartBlock + state.TEpochBlocks
		}
	} else if closeBlock != 0 {
		if closeBlock < state.TEpochBlocks {
			startBlock = 0
		} else {
			startBlock = closeBlock - state.TEpochBlocks
		}
	} else {
		return time.Time{}, time.Time{}, 0, 0, fmt.Errorf("st: epoch %d window unknown (no mirror row, not rolled)", epoch)
	}
	startTime = stBlockTimeOrEstimate(ctx, client, state, startBlock, blockSeconds)
	endTime = stBlockTimeOrEstimate(ctx, client, state, closeBlock, blockSeconds)
	return startTime, endTime, startBlock, closeBlock, nil
}

// stContributingClientCkeys preloads the client_id -> ckey (client Ed25519
// public key) map for every distinct verify client contributing to an epoch,
// so the head-tier exclusion resolves membership without a per-leaf lookup. It
// is skipped entirely when there is no active head binding (the common case),
// and otherwise issues a single batched read (one Redis MGET).
func stContributingClientCkeys(ctx context.Context, reliabilities []*model.StClientReliability, hasHeadBindings bool) map[server.Id][32]byte {
	if !hasHeadBindings {
		return nil
	}
	seen := map[server.Id]bool{}
	clientIds := make([]server.Id, 0, len(reliabilities))
	for _, reliability := range reliabilities {
		if !seen[reliability.ClientId] {
			seen[reliability.ClientId] = true
			clientIds = append(clientIds, reliability.ClientId)
		}
	}
	return model.GetStContributingClientCkeys(ctx, clientIds)
}

// StComputeEpochPayout computes and stores the payout leaves for a closed
// epoch (the StEpochClose step): map the epoch boundary blocks to a
// wall-clock window, read usage (`transfer_escrow_sweep`) × reliability
// (`verify_provider_stats`), join claim wallets, aggregate per coldkey,
// floor to shareBps, and store the canonical leaf set. Returns the Merkle
// root (zero when there are no leaves).
//
// Recomputation before the root is committed on chain is idempotent
// (SetStPayoutLeaves replaces the set).
func StComputeEpochPayout(ctx context.Context, epoch uint64) (root [32]byte, leafCount int, returnErr error) {
	cfg, client, err := stRequire()
	if err != nil {
		return root, 0, err
	}
	state, err := client.Epoch(ctx)
	if err != nil {
		return root, 0, err
	}
	if epoch >= state.PendingEpoch {
		return root, 0, fmt.Errorf("st: epoch %d is not closed yet (pending %d)", epoch, state.PendingEpoch)
	}

	startTime, endTime, startBlock, closeBlock, err := stEpochWindow(ctx, client, state, epoch, cfg.BlockSeconds)
	if err != nil {
		return root, 0, err
	}

	usages := model.GetStEpochNetworkUsage(ctx, startTime, endTime)
	reliabilities := model.GetStEpochClientReliability(ctx, startTime, endTime)
	wallets := model.GetAllStWalletColdkeys(ctx)

	// Head-tier exclusion (WHITEPAPER §8.4/§11.4): a provider promoted to the
	// head tier is paid natively by Yuma, so it must never also earn a pool
	// share (never paid twice). Head bindings are keyed by the provider's
	// client public key (ckey); preload the contributing clients' ckeys (one
	// batched read, and only when a head binding actually exists) and drop any
	// network with a head-bound client before aggregation, so a promoted
	// provider's usage/reliability never enters a leaf.
	//
	// HF-1: exclude any ckey head-bound at ANY block in the epoch's
	// [startBlock, closeBlock] window — NOT just the ones still bound now. The
	// validator pays head emission per tempo across the epoch, so a provider
	// bound for the epoch that calls unbindHead just before close still earned
	// head emission and must stay out of the pool for that epoch.
	headBoundCkeys := model.GetHeadBoundCkeysInEpoch(ctx, startBlock, closeBlock)
	clientCkeys := stContributingClientCkeys(ctx, reliabilities, 0 < len(headBoundCkeys))

	entries := stBuildShareEntries(usages, reliabilities, wallets, clientCkeys, headBoundCkeys)

	shares := stComputePayoutShares(entries, cfg.ReliabilityAMin)
	leaves := make([]*model.StPayoutLeaf, len(shares))
	for i, share := range shares {
		leaves[i] = &model.StPayoutLeaf{
			Epoch:     epoch,
			NoId:      cfg.NoId,
			NetworkId: share.NetworkId,
			Coldkey:   share.Coldkey,
			ShareBps:  share.ShareBps,
			LeafIndex: i,
		}
	}
	model.SetStPayoutLeaves(ctx, epoch, cfg.NoId, leaves)

	if len(leaves) == 0 {
		glog.Infof("[st]epoch %d close: no payout leaves (usage networks=%d, wallets=%d)\n", epoch, len(usages), len(wallets))
		return root, 0, nil
	}
	tree, err := stBuildPayoutTree(leaves)
	if err != nil {
		return root, 0, err
	}
	root = tree.Root()
	glog.Infof("[st]epoch %d close: %d leaves, root 0x%x, window [%s, %s)\n", epoch, len(leaves), root, startTime, endTime)
	return root, len(leaves), nil
}

// stResolvePublish records the outcome of a publish attempt.
func stResolvePublish(ctx context.Context, publishId server.Id, outcome *StPublishOutcome) {
	var txHash *string
	if outcome.TxHash != "" {
		txHash = &outcome.TxHash
	}
	var errorMessage *string
	if outcome.Reason != "" {
		errorMessage = &outcome.Reason
	}
	model.UpdateStPublish(ctx, publishId, outcome.Status, txHash, errorMessage)
}

// StCommitEpochRoot publishes the payout root for a closed epoch
// (commitOperator, the ≤ +commitWindow deadline path — D-11). Idempotent:
// a root already on chain resolves to skipped. Emits the T-2h alert when
// the (estimated) time to the commit deadline is under stCommitAlertLead
// and the root is still unconfirmed.
func StCommitEpochRoot(ctx context.Context, epoch uint64) (*StPublishOutcome, error) {
	cfg, client, err := stRequire()
	if err != nil {
		return nil, err
	}

	// idempotency: the root may already be on chain (previous attempt,
	// manual bringyourctl/stctl commit, or an ambiguous timed-out send)
	pool, err := client.PoolState(ctx, epoch, cfg.NoId)
	if err != nil {
		return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: err.Error(), Retry: true}, nil
	}
	if pool.CommittedRoot != ([32]byte{}) {
		model.SetStEpochStatus(ctx, epoch, model.StEpochStatusCommitted)
		outcome := &StPublishOutcome{
			Status: model.StPublishStatusSkipped,
			Reason: fmt.Sprintf("root already committed on chain: 0x%x", pool.CommittedRoot),
		}
		publishId := model.AddStPublish(ctx, epoch, model.StPublishKindCommit)
		stResolvePublish(ctx, publishId, outcome)
		return outcome, nil
	}
	if pool.Finalized {
		outcome := &StPublishOutcome{
			Status: model.StPublishStatusSkipped,
			Reason: "epoch already finalized without a commit (pool total carried)",
		}
		publishId := model.AddStPublish(ctx, epoch, model.StPublishKindCommit)
		stResolvePublish(ctx, publishId, outcome)
		return outcome, nil
	}

	leaves := model.GetStPayoutLeaves(ctx, epoch, cfg.NoId)
	if len(leaves) == 0 {
		outcome := &StPublishOutcome{
			Status: model.StPublishStatusSkipped,
			Reason: "no payout leaves for epoch (nothing to commit; pool total carries)",
		}
		publishId := model.AddStPublish(ctx, epoch, model.StPublishKindCommit)
		stResolvePublish(ctx, publishId, outcome)
		return outcome, nil
	}
	tree, err := stBuildPayoutTree(leaves)
	if err != nil {
		return nil, err
	}
	root := tree.Root()

	// window checks — in blocks, per the contract clock
	state, err := client.Epoch(ctx)
	if err != nil {
		return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: err.Error(), Retry: true}, nil
	}
	closeBlock, err := client.EpochCloseBlock(ctx, epoch)
	if err != nil {
		return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: err.Error(), Retry: true}, nil
	}
	if closeBlock == 0 {
		if epoch >= state.PendingEpoch {
			return &StPublishOutcome{
				Status: model.StPublishStatusSkipped,
				Reason: fmt.Sprintf("epoch %d not closed yet (pending %d)", epoch, state.PendingEpoch),
				Retry:  true,
			}, nil
		}
		// closed by chain time but not rolled: commitOperator rolls lazily
		// on entry, so the send below is still valid; use the projected
		// boundary for the deadline math
		closeBlock = state.EpochStartBlock + state.TEpochBlocks
	}
	deadlineBlock := closeBlock + state.CommitWindowBlocks
	if deadlineBlock < state.HeadBlock {
		outcome := &StPublishOutcome{
			Status: model.StPublishStatusFailed,
			Reason: fmt.Sprintf("commit window closed at block %d (head %d); pool total carries to the next epoch", deadlineBlock, state.HeadBlock),
		}
		publishId := model.AddStPublish(ctx, epoch, model.StPublishKindCommit)
		stResolvePublish(ctx, publishId, outcome)
		glog.Errorf("[st]epoch %d commit MISSED: %s\n", epoch, outcome.Reason)
		return outcome, nil
	}

	// D-11 T-2h alert
	blocksLeft := deadlineBlock - state.HeadBlock
	timeLeft := time.Duration(blocksLeft) * time.Duration(cfg.BlockSeconds) * time.Second
	if timeLeft < stCommitAlertLead {
		glog.Errorf("[st]epoch %d commit DEADLINE ALERT: unconfirmed with ~%s left (deadline block %d, head %d)\n", epoch, timeLeft, deadlineBlock, state.HeadBlock)
	}

	publishId := model.AddStPublish(ctx, epoch, model.StPublishKindCommit)
	// off = empty: the full payout list is served by GET /sn/pool/claim,
	// not an off-chain pointer, in v1
	txHash, err := client.CommitPayoutRoot(ctx, epoch, cfg.NoId, root, []byte{})
	if err != nil {
		outcome := &StPublishOutcome{
			Status: model.StPublishStatusFailed,
			TxHash: txHash,
			Reason: err.Error(),
			Retry:  true,
		}
		stResolvePublish(ctx, publishId, outcome)
		glog.Errorf("[st]epoch %d commit attempt failed: %s\n", epoch, err)
		return outcome, nil
	}
	outcome := &StPublishOutcome{
		Status: model.StPublishStatusConfirmed,
		TxHash: txHash,
	}
	stResolvePublish(ctx, publishId, outcome)
	model.SetStEpochStatus(ctx, epoch, model.StEpochStatusCommitted)
	glog.Infof("[st]epoch %d commit confirmed: root 0x%x tx %s\n", epoch, root, txHash)
	return outcome, nil
}

// StDepositForEpoch executes the push-then-credit deposit for the CURRENT
// open epoch (deposit() credits the contract's current epoch — a deposit
// for a passed epoch is skipped). Sizing (when overrideRao is nil) is the
// documented reference-rate policy over the PREVIOUS epoch's usage window,
// hard-capped by deposit_epoch_cap_rao (D-3). Both the push and the credit
// are recorded as publish rows (kinds deposit_push / deposit).
func StDepositForEpoch(ctx context.Context, epoch uint64, overrideRao *big.Int) (*StPublishOutcome, error) {
	cfg, client, err := stRequire()
	if err != nil {
		return nil, err
	}

	state, err := client.Epoch(ctx)
	if err != nil {
		return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: err.Error(), Retry: true}, nil
	}
	if state.Epoch != epoch || state.PendingEpoch != epoch {
		// deposit() lazily rolls, so a send now would credit a different
		// epoch than intended — never send
		outcome := &StPublishOutcome{
			Status: model.StPublishStatusSkipped,
			Reason: fmt.Sprintf("epoch %d is not the current epoch (rolled %d, pending %d)", epoch, state.Epoch, state.PendingEpoch),
		}
		publishId := model.AddStPublish(ctx, epoch, model.StPublishKindDeposit)
		stResolvePublish(ctx, publishId, outcome)
		return outcome, nil
	}

	// idempotency: one automated deposit per epoch. v0.4 (D25) removed the
	// contract's per-NO deposit ledger, so "already deposited this epoch" is
	// read from the mirrored Deposited event log (the authoritative per-NO
	// record) instead of contract state. Seam: this trails the event sync; the
	// push half's UnaccountedStakeRao check below is the second guard, and the
	// D-3 per-epoch cap bounds the blast radius of any double-send.
	deposited := model.SumStDepositedRao(ctx, epoch, cfg.NoId)
	if 0 < deposited.Sign() {
		outcome := &StPublishOutcome{
			Status: model.StPublishStatusSkipped,
			Reason: fmt.Sprintf("epoch %d already has %s rao deposited", epoch, deposited),
		}
		publishId := model.AddStPublish(ctx, epoch, model.StPublishKindDeposit)
		stResolvePublish(ctx, publishId, outcome)
		return outcome, nil
	}

	// sizing
	var amount *big.Int
	if overrideRao != nil {
		amount = new(big.Int).Set(overrideRao)
		// the config cap is the custody blast-radius control (D-3) and
		// applies to manual sizes too
		capBig := new(big.Int).SetUint64(cfg.DepositEpochCapRao)
		if cfg.DepositEpochCapRao == 0 || 0 < amount.Cmp(capBig) {
			return nil, fmt.Errorf("st: deposit %s rao exceeds deposit_epoch_cap_rao %d", amount, cfg.DepositEpochCapRao)
		}
	} else {
		if epoch == 0 {
			amount = big.NewInt(0)
		} else {
			prevStart, prevEnd, _, _, err := stEpochWindow(ctx, client, state, epoch-1, cfg.BlockSeconds)
			if err != nil {
				return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: err.Error(), Retry: true}, nil
			}
			var usageBytes int64
			for _, usage := range model.GetStEpochNetworkUsage(ctx, prevStart, prevEnd) {
				usageBytes += usage.PayoutByteCount
			}
			amount = stDepositSizeRao(usageBytes, cfg.DepositAlphaRaoPerGib, cfg.DepositEpochCapRao)
		}
	}
	if amount.Sign() <= 0 {
		outcome := &StPublishOutcome{
			Status: model.StPublishStatusSkipped,
			Reason: "deposit size is zero (no usage, or deposit rate/cap not configured)",
		}
		publishId := model.AddStPublish(ctx, epoch, model.StPublishKindDeposit)
		stResolvePublish(ctx, publishId, outcome)
		return outcome, nil
	}

	// PUSH: move α onto (coldkey = mirror(contract), hotkey = treasury) via
	// the staking precompile. Skip (partially) when unaccounted α already
	// covers the amount — e.g. a previous push whose credit failed. NOTE:
	// v1 assumes UR is the only pusher; a concurrent third-party push (a
	// plain transferStake to the treasury) could still be mis-attributed
	// here (documented seam).
	unaccounted, err := client.UnaccountedStakeRao(ctx)
	if err != nil {
		return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: err.Error(), Retry: true}, nil
	}
	pushAmount := new(big.Int).Sub(amount, unaccounted)
	pushPublishId := model.AddStPublish(ctx, epoch, model.StPublishKindDepositPush)
	if pushAmount.Sign() <= 0 {
		stResolvePublish(ctx, pushPublishId, &StPublishOutcome{
			Status: model.StPublishStatusSkipped,
			Reason: fmt.Sprintf("%s rao already pushed and unaccounted on the treasury hotkey", unaccounted),
		})
	} else {
		txHash, err := client.DepositPush(ctx, pushAmount)
		if err != nil {
			outcome := &StPublishOutcome{
				Status: model.StPublishStatusFailed,
				TxHash: txHash,
				Reason: fmt.Sprintf("push transferStake: %s", err),
				Retry:  true,
			}
			stResolvePublish(ctx, pushPublishId, outcome)
			glog.Errorf("[st]epoch %d deposit push failed: %s\n", epoch, err)
			return outcome, nil
		}
		stResolvePublish(ctx, pushPublishId, &StPublishOutcome{
			Status: model.StPublishStatusConfirmed,
			TxHash: txHash,
		})
	}

	// CREDIT: deposit() attributes the pushed α to (epoch, noId)
	creditPublishId := model.AddStPublish(ctx, epoch, model.StPublishKindDeposit)
	txHash, err := client.DepositCredit(ctx, cfg.NoId, amount)
	if err != nil {
		outcome := &StPublishOutcome{
			Status: model.StPublishStatusFailed,
			TxHash: txHash,
			Reason: fmt.Sprintf("deposit credit: %s", err),
			Retry:  true,
		}
		stResolvePublish(ctx, creditPublishId, outcome)
		glog.Errorf("[st]epoch %d deposit credit failed: %s\n", epoch, err)
		return outcome, nil
	}
	outcome := &StPublishOutcome{
		Status: model.StPublishStatusConfirmed,
		TxHash: txHash,
	}
	stResolvePublish(ctx, creditPublishId, outcome)
	glog.Infof("[st]epoch %d deposit confirmed: %s rao tx %s\n", epoch, amount, txHash)
	return outcome, nil
}

// stMaxFinalizePerPoke bounds the in-order finalize catch-up loop per run.
const stMaxFinalizePerPoke = 8

// StFinalizeEpochPoke pokes the permissionless finalizeEpoch for epoch (and
// any unfinalized epochs before it — the contract finalizes strictly in
// order). Before the finalize block it returns a retry outcome with a
// block-derived RetryAt hint.
func StFinalizeEpochPoke(ctx context.Context, epoch uint64) (*StPublishOutcome, error) {
	cfg, client, err := stRequire()
	if err != nil {
		return nil, err
	}

	// idempotency: already finalized (by anyone — the call is permissionless)
	pool, err := client.PoolState(ctx, epoch, cfg.NoId)
	if err != nil {
		return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: err.Error(), Retry: true}, nil
	}
	if pool.Finalized {
		model.SetStEpochStatus(ctx, epoch, model.StEpochStatusFinalized)
		outcome := &StPublishOutcome{
			Status: model.StPublishStatusSkipped,
			Reason: "epoch already finalized on chain",
		}
		publishId := model.AddStPublish(ctx, epoch, model.StPublishKindFinalize)
		stResolvePublish(ctx, publishId, outcome)
		return outcome, nil
	}

	state, err := client.Epoch(ctx)
	if err != nil {
		return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: err.Error(), Retry: true}, nil
	}
	closeBlock, err := client.EpochCloseBlock(ctx, epoch)
	if err != nil {
		return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: err.Error(), Retry: true}, nil
	}
	if closeBlock == 0 {
		// the lazy roll has not registered the epoch boundary yet; poke it
		// and retry
		if epoch < state.PendingEpoch {
			if _, err := client.RollEpochs(ctx); err != nil {
				glog.Infof("[st]rollEpochs poke failed: %s\n", err)
			}
		}
		return &StPublishOutcome{
			Status: model.StPublishStatusSkipped,
			Reason: fmt.Sprintf("epoch %d boundary not rolled yet", epoch),
			Retry:  true,
		}, nil
	}
	finalizeBlock := closeBlock + state.FinalizeOffsetBlocks
	if state.HeadBlock < finalizeBlock {
		return &StPublishOutcome{
			Status:  model.StPublishStatusSkipped,
			Reason:  fmt.Sprintf("finalize window opens at block %d (head %d)", finalizeBlock, state.HeadBlock),
			Retry:   true,
			RetryAt: stEstimateBlockTime(state.HeadBlock, state.HeadBlockTime, finalizeBlock, cfg.BlockSeconds),
		}, nil
	}

	// finalize is strictly in order: catch up from nextFinalizeEpoch
	next, err := client.NextFinalizeEpoch(ctx)
	if err != nil {
		return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: err.Error(), Retry: true}, nil
	}
	if epoch < next {
		// raced with another finalizer
		model.SetStEpochStatus(ctx, epoch, model.StEpochStatusFinalized)
		return &StPublishOutcome{
			Status: model.StPublishStatusSkipped,
			Reason: "epoch already finalized on chain",
		}, nil
	}
	if epoch-next+1 > stMaxFinalizePerPoke {
		// deep backlog: finalize a bounded batch this run and retry
		epoch = next + stMaxFinalizePerPoke - 1
	}
	var lastOutcome *StPublishOutcome
	for e := next; e <= epoch; e++ {
		publishId := model.AddStPublish(ctx, e, model.StPublishKindFinalize)
		txHash, err := client.FinalizeEpoch(ctx, e)
		if err != nil {
			outcome := &StPublishOutcome{
				Status: model.StPublishStatusFailed,
				TxHash: txHash,
				Reason: err.Error(),
				Retry:  true,
			}
			stResolvePublish(ctx, publishId, outcome)
			glog.Errorf("[st]epoch %d finalize failed: %s\n", e, err)
			return outcome, nil
		}
		stResolvePublish(ctx, publishId, &StPublishOutcome{
			Status: model.StPublishStatusConfirmed,
			TxHash: txHash,
		})
		model.SetStEpochStatus(ctx, e, model.StEpochStatusFinalized)
		glog.Infof("[st]epoch %d finalize confirmed: tx %s\n", e, txHash)
		lastOutcome = &StPublishOutcome{
			Status: model.StPublishStatusConfirmed,
			TxHash: txHash,
		}
	}
	return lastOutcome, nil
}

// stEpochSummaryTtl bounds staleness of the redis epoch mirror if the sync
// task stalls (the task refreshes it about every minute).
const stEpochSummaryTtl = 5 * time.Minute

// StSyncChainState refreshes the epoch mirror from the contract: pokes
// rollEpochs when the lazy epoch counter is behind, upserts the open
// epoch's `st_epoch` row, and refreshes the redis epoch summary cache.
// Returns the state for the caller's scheduling decisions.
func StSyncChainState(ctx context.Context) (*StEpochState, error) {
	cfg, client, err := stRequire()
	if err != nil {
		return nil, err
	}
	state, err := client.Epoch(ctx)
	if err != nil {
		return nil, err
	}

	if state.Epoch < state.PendingEpoch {
		// permissionless poke; the contract measures pool emission at each
		// roll, so keeping the roll current keeps the D-4 measurement tight
		if txHash, err := client.RollEpochs(ctx); err != nil {
			glog.Infof("[st]rollEpochs poke failed: %s\n", err)
		} else {
			glog.Infof("[st]rollEpochs poked: tx %s (rolled %d -> pending %d)\n", txHash, state.Epoch, state.PendingEpoch)
			if rolled, err := client.Epoch(ctx); err == nil {
				state = rolled
			}
		}
	}

	model.UpsertStEpoch(ctx, stEpochRowFromState(state))
	model.SetStEpochSummaryCache(ctx, &model.StEpochSummary{
		Epoch:               state.Epoch,
		StartBlock:          state.EpochStartBlock,
		CommitDeadlineBlock: state.EpochStartBlock + state.TEpochBlocks + state.CommitWindowBlocks,
		TrailsDeadlineBlock: state.EpochStartBlock + state.TEpochBlocks + state.TrailsWindowBlocks,
		FinalizeBlock:       state.EpochStartBlock + state.TEpochBlocks + state.FinalizeOffsetBlocks,
		TEpochBlocks:        state.TEpochBlocks,
		ChainId:             cfg.ChainId,
		ContractAddress:     cfg.ContractAddress.Hex(),
	}, stEpochSummaryTtl)
	return state, nil
}

const (
	// stSyncEventsBlockRange bounds each eth_getLogs range (SP-4: public
	// endpoints reject wide ranges).
	stSyncEventsBlockRange = 2000
	// stSyncEventsMaxRanges bounds the catch-up work per sync run.
	stSyncEventsMaxRanges = 5
)

// StSyncChainEvents advances the contract event mirror from the high-water
// block toward headBlock in bounded ranges, and applies the status
// transitions the events prove (our OperatorCommitted -> committed,
// EpochFinalized -> finalized).
func StSyncChainEvents(ctx context.Context, headBlock uint64) (int, error) {
	cfg, client, err := stRequire()
	if err != nil {
		return 0, err
	}

	fromBlock := model.GetStHighWaterBlock(ctx)
	if fromBlock == 0 {
		// first sync: start at the deploy block when configured, else at
		// the head (the epoch pipeline reconciles from state, not history)
		if cfg.DeployBlock != 0 {
			fromBlock = cfg.DeployBlock
		} else {
			fromBlock = headBlock
		}
	}

	synced := 0
	for range stSyncEventsMaxRanges {
		if headBlock < fromBlock {
			break
		}
		toBlock := fromBlock + stSyncEventsBlockRange - 1
		if headBlock < toBlock {
			toBlock = headBlock
		}
		events, nextBlock, err := client.SyncEvents(ctx, fromBlock, toBlock)
		if err != nil {
			return synced, err
		}
		model.UpsertStEvents(ctx, events)
		stApplyEventStatuses(ctx, cfg, events)
		stApplyHeadBindings(ctx, events)
		model.SetStHighWaterBlock(ctx, nextBlock)
		synced += len(events)
		fromBlock = nextBlock
	}
	return synced, nil
}

// StBackfillEpochRow mirrors a closed epoch that was never observed while
// open (e.g. the worker was down across the boundary), so the close/commit
// pipeline can still pick it up. Returns true when a row was created.
func StBackfillEpochRow(ctx context.Context, state *StEpochState, epoch uint64) (bool, error) {
	_, client, err := stRequire()
	if err != nil {
		return false, err
	}
	if model.GetStEpoch(ctx, epoch) != nil {
		return false, nil
	}
	closeBlock, err := client.EpochCloseBlock(ctx, epoch)
	if err != nil {
		return false, err
	}
	if closeBlock == 0 {
		return false, nil
	}
	var startBlock uint64
	if state.TEpochBlocks < closeBlock {
		startBlock = closeBlock - state.TEpochBlocks
	}
	model.UpsertStEpoch(ctx, &model.StEpoch{
		Epoch:               epoch,
		StartBlock:          startBlock,
		CommitDeadlineBlock: closeBlock + state.CommitWindowBlocks,
		TrailsDeadlineBlock: closeBlock + state.TrailsWindowBlocks,
		FinalizeBlock:       closeBlock + state.FinalizeOffsetBlocks,
		Status:              model.StEpochStatusOpen,
	})
	return true, nil
}

// stEventEpochNo is the (e, no_id) subset of a mirrored event's DataJson.
type stEventEpochNo struct {
	E    string `json:"e"`
	NoId string `json:"no_id"`
}

// stApplyEventStatuses advances `st_epoch` statuses from mirrored events.
// Status only ever advances (UpsertStEpoch/SetStEpochStatus guarantee), so
// replays are harmless.
func stApplyEventStatuses(ctx context.Context, cfg *StConfig, events []*model.StChainEvent) {
	for _, event := range events {
		switch event.Kind {
		case "OperatorCommitted":
			var data stEventEpochNo
			if err := json.Unmarshal([]byte(event.DataJson), &data); err != nil {
				continue
			}
			epoch, epochErr := stParseUint(data.E)
			noId, noIdErr := stParseUint(data.NoId)
			if epochErr != nil || noIdErr != nil || noId != cfg.NoId {
				continue
			}
			model.SetStEpochStatus(ctx, epoch, model.StEpochStatusCommitted)
		case "EpochFinalized":
			var data stEventEpochNo
			if err := json.Unmarshal([]byte(event.DataJson), &data); err != nil {
				continue
			}
			epoch, err := stParseUint(data.E)
			if err != nil {
				continue
			}
			model.SetStEpochStatus(ctx, epoch, model.StEpochStatusFinalized)
		}
	}
}

func stParseUint(value string) (uint64, error) {
	parsed, ok := new(big.Int).SetString(value, 10)
	if !ok || !parsed.IsUint64() {
		return 0, fmt.Errorf("st: not a uint64: %q", value)
	}
	return parsed.Uint64(), nil
}

// stHeadBindingEvent is the (ckey, hotkey, uid) subset of a mirrored
// HeadBound/HeadUnbound event's DataJson.
type stHeadBindingEvent struct {
	Ckey   string `json:"ckey"`
	Hotkey string `json:"hotkey"`
	Uid    string `json:"uid"`
}

// stApplyHeadBindings mirrors HeadBound/HeadUnbound events into the
// st_head_binding registry in block/log order (a later HeadUnbound
// deactivates). The model upsert's update_block guard is block-granular
// (HF-6), so same-block bind/unbind resolution rests on application order —
// sort explicitly by (block, log) rather than trusting eth_getLogs ordering
// end-to-end; the guard then makes a conservative re-scan idempotent (replays
// never regress a newer state). Note the epoch payout exclusion does NOT read
// this registry — it replays the st_event log directly with its own ORDER BY
// (GetHeadBoundCkeysInEpoch), so st_head_binding is an ops/debug mirror.
func stApplyHeadBindings(ctx context.Context, events []*model.StChainEvent) {
	ordered := make([]*model.StChainEvent, len(events))
	copy(ordered, events)
	sort.SliceStable(ordered, func(a, b int) bool {
		if ordered[a].BlockNumber != ordered[b].BlockNumber {
			return ordered[a].BlockNumber < ordered[b].BlockNumber
		}
		return ordered[a].LogIndex < ordered[b].LogIndex
	})
	for _, event := range ordered {
		var active bool
		switch event.Kind {
		case "HeadBound":
			active = true
		case "HeadUnbound":
			active = false
		default:
			continue
		}
		var data stHeadBindingEvent
		if err := json.Unmarshal([]byte(event.DataJson), &data); err != nil {
			continue
		}
		ckey, err := stParseHex32(data.Ckey)
		if err != nil {
			continue
		}
		hotkey, err := stParseHex32(data.Hotkey)
		if err != nil {
			continue
		}
		uid, err := stParseUint(data.Uid)
		if err != nil {
			continue
		}
		model.UpsertStHeadBinding(ctx, ckey, hotkey, uid, active, event.BlockNumber)
	}
}

// ---------------------------------------------------------------------------
// Ops accessors (bringyourctl st)
//
// The `/sn/*` control-plane implementations (SnSetWallet, SnPoolClaim,
// SnEpoch) live in sn_controller.go — they only read settled state; chain
// writes stay here.
// ---------------------------------------------------------------------------

// StNoId exposes the configured operator id (bringyourctl st).
func StNoId() (uint64, error) {
	cfg := stConfig()
	if cfg == nil {
		return 0, errStNotConfigured
	}
	return cfg.NoId, nil
}

// StGetEpochState reads the live contract epoch state (bringyourctl st).
func StGetEpochState(ctx context.Context) (*StEpochState, error) {
	_, client, err := stRequire()
	if err != nil {
		return nil, err
	}
	return client.Epoch(ctx)
}

// StGetPoolState reads the live per-(epoch, noId) pool state
// (bringyourctl st).
func StGetPoolState(ctx context.Context, epoch uint64) (*StPoolState, error) {
	cfg, client, err := stRequire()
	if err != nil {
		return nil, err
	}
	return client.PoolState(ctx, epoch, cfg.NoId)
}
