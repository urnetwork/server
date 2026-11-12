package controller

// st_ctf_client.go — the capture the flag chain surface (sn CAPTURETHEFLAG.md
// §3.1, §4.3, §4.6) beside the v2 one. The CTF pool is the vault2 pool
// P = 2^32 + n, so every vault2 and STSettlement2 call that takes a pool id
// (state, close, commit, finalize, answer, fundedThrough, claimed) is the v2
// client's with P (StLedgerV2Client). What is new is the census commitment on
// STSettlement2Ctf, which takes the operator id n, and the carry contract
// (STCtfCarry): its records of returned carry, what it holds of its own, and
// its recycle. Writes are durable intents of the existing engine, signed by
// the root key.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/holiman/uint256"
	"golang.org/x/crypto/blake2b"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/ss58"
	"github.com/urfoundation/sn/stabi"
)

// Logical operation keys of the CTF driver's own writes. The close, commit,
// finalize and answer of the CTF pool use the v2 keys with the pool id P.
func stCtfCensusOperation(epoch uint64, noId uint64) string {
	return fmt.Sprintf("ctfcensus:%d:%d", epoch, noId)
}

func stCtfRecycleOperation(rootIndex uint64, poolId uint64) string {
	return fmt.Sprintf("ctfrecycle:%d:%d", rootIndex, poolId)
}

// An absorb names the root whose recycle it clears the way for and the
// hotkey it frees, so a later fill of the same entry is absorbed again only
// for a later root.
func stCtfAbsorbOperation(rootIndex uint64, poolId uint64, hotkey [32]byte) string {
	return fmt.Sprintf("ctfabsorb:%d:%d:%x", rootIndex, poolId, hotkey[:8])
}

// StCtfCarry is the carry contract's immutable identity as it reads it: its
// vault2, pool and coldkey, and the coldkey, escrow hotkey and pool hotkey of
// that vault2 it moves stake between.
type StCtfCarry struct {
	Vault        common.Address
	PoolId       uint64
	Coldkey      [32]byte
	VaultColdkey [32]byte
	EscrowHotkey [32]byte
	PoolHotkey   [32]byte
}

// StCtfCarryHeld is what the carry contract holds of its own (§4.6), read at
// one finalized block: its residual, its coldkey's position on vault2's
// escrow hotkey, which bounds what a recycle keeps, its least seed, and the
// TAO its address can spend, which seed() stakes with any value sent.
type StCtfCarryHeld struct {
	Residual     uint256.Int
	Position     uint256.Int
	SeedMinimum  uint256.Int
	SpendableRao uint256.Int
}

// Kept is what a recycle keeps on the escrow hotkey: the residual, or the
// position where the runtime lowered it below, as the contract follows it.
func (self *StCtfCarryHeld) Kept() uint256.Int {
	if self.Position.Lt(&self.Residual) {
		return self.Position
	}
	return self.Residual
}

// SeedShortfall is the least TAO, in rao, a seed() has to send to restore the
// residual: the least seed less what the carry's address can already stake.
func (self *StCtfCarryHeld) SeedShortfall() uint256.Int {
	var shortfall uint256.Int
	if self.SpendableRao.Lt(&self.SeedMinimum) {
		shortfall.Sub(&self.SeedMinimum, &self.SpendableRao)
	}
	return shortfall
}

// StCtfClient is the swappable capture the flag chain client; CoreStClient
// implements it. Views read the finalized head.
type StCtfClient interface {
	// CtfCensus reads ctfCensus(noId, epoch): the committed census hash and
	// the block B_k it was committed at, zero while none is committed.
	CtfCensus(ctx context.Context, noId uint64, epoch uint64) (hash [32]byte, block uint64, err error)
	// CtfBlockTime reads a block's EVM timestamp, in unix seconds.
	CtfBlockTime(ctx context.Context, block uint64) (uint64, error)
	// CtfCarry reads the carry contract's vault, pool id and coldkey.
	CtfCarry(ctx context.Context, carry common.Address) (*StCtfCarry, error)
	// CtfRecycledFor reads the carry contract's recycledFor(epoch), R_k's
	// record (§4.5).
	CtfRecycledFor(ctx context.Context, carry common.Address, epoch uint64) (uint256.Int, error)
	// CtfCarryHeld reads what the carry holds of its own at one finalized
	// block: residual(), its position on the escrow hotkey that identity
	// names, seedMinimum() and its address's spendable TAO.
	CtfCarryHeld(ctx context.Context, carry common.Address, identity *StCtfCarry) (*StCtfCarryHeld, error)
	// CtfCommitCensus sends commitCtfCensus(epoch, noId, hash) under
	// ctfcensus:e:no.
	CtfCommitCensus(ctx context.Context, epoch uint64, noId uint64, hash [32]byte) (string, error)
	// CtfRecycle sends the carry contract's recycle(rootIndex, account,
	// proof) under ctfrecycle:rootIndex:P, the carry account at index of the
	// final root rootIndex with its claim proof.
	CtfRecycle(ctx context.Context, carry common.Address, poolId uint64, rootIndex uint64, index uint64, account *merkle.LedgerAccount, proof []merkle.LedgerNode) (string, error)
	// CtfStakingHotkeys reads the runtime's StakingHotkeys list of a
	// coldkey: the hotkeys it holds stake on.
	CtfStakingHotkeys(ctx context.Context, coldkey [32]byte) ([][32]byte, error)
	// CtfAbsorbStake sends vault2's absorbStake(hotkey), which moves
	// vault2's stake on a hotkey no pool uses into its escrow, under
	// ctfabsorb:rootIndex:P:hotkey.
	CtfAbsorbStake(ctx context.Context, poolId uint64, rootIndex uint64, hotkey [32]byte) (string, error)
}

var stCtfClientLock sync.Mutex
var stCtfClientInstance StCtfClient

// SetStCtfClient overrides the core capture the flag client (for tests); nil
// restores it.
func SetStCtfClient(client StCtfClient) {
	stCtfClientLock.Lock()
	defer stCtfClientLock.Unlock()
	stCtfClientInstance = client
}

func stCtfClient() StCtfClient {
	stCtfClientLock.Lock()
	instance := stCtfClientInstance
	stCtfClientLock.Unlock()
	if instance != nil {
		return instance
	}
	if core, ok := stClient().(*CoreStClient); ok {
		return core
	}
	return nil
}

var stCtfCarryAbi = stabi.NewSTCtfCarry()

// stCtfRecycleSelector is the selector of the carry contract's recycle.
var stCtfRecycleSelector = crypto.Keccak256([]byte("recycle(uint256,(uint256,bytes32,uint256),(bytes32,uint256)[])"))[:4]

func (self *CoreStClient) CtfCensus(ctx context.Context, noId uint64, epoch uint64) ([32]byte, uint64, error) {
	census, err := stViewAt(self, ctx, self.cfg.SettlementV2, stLedgerV2SettlementAbi.PackCtfCensus(stBig(noId), stBig(epoch)), stLedgerV2SettlementAbi.UnpackCtfCensus)
	if err != nil {
		return [32]byte{}, 0, fmt.Errorf("ctfCensus(%d, %d): %w", noId, epoch, err)
	}
	return census.CensusHash, census.BlockNumber, nil
}

func (self *CoreStClient) CtfBlockTime(ctx context.Context, block uint64) (uint64, error) {
	at, err := self.BlockTime(ctx, block)
	if err != nil {
		return 0, fmt.Errorf("block %d time: %w", block, err)
	}
	if at.Unix() <= 0 {
		return 0, fmt.Errorf("block %d time %s is not after 1970", block, at)
	}
	return uint64(at.Unix()), nil
}

func (self *CoreStClient) CtfCarry(ctx context.Context, carry common.Address) (*StCtfCarry, error) {
	out := &StCtfCarry{}
	var err error
	if out.Vault, err = stViewAt(self, ctx, carry, stCtfCarryAbi.PackVault(), stCtfCarryAbi.UnpackVault); err != nil {
		return nil, fmt.Errorf("carry %s vault(): %w", carry.Hex(), err)
	}
	poolId, err := stViewAt(self, ctx, carry, stCtfCarryAbi.PackPoolId(), stCtfCarryAbi.UnpackPoolId)
	if err == nil {
		out.PoolId, err = stUint64(poolId)
	}
	if err != nil {
		return nil, fmt.Errorf("carry %s poolId(): %w", carry.Hex(), err)
	}
	for _, read := range []struct {
		name     string
		calldata []byte
		unpack   func([]byte) ([32]byte, error)
		out      *[32]byte
	}{
		{"coldkey()", stCtfCarryAbi.PackColdkey(), stCtfCarryAbi.UnpackColdkey, &out.Coldkey},
		{"vaultColdkey()", stCtfCarryAbi.PackVaultColdkey(), stCtfCarryAbi.UnpackVaultColdkey, &out.VaultColdkey},
		{"escrowHotkey()", stCtfCarryAbi.PackEscrowHotkey(), stCtfCarryAbi.UnpackEscrowHotkey, &out.EscrowHotkey},
		{"poolHotkey()", stCtfCarryAbi.PackPoolHotkey(), stCtfCarryAbi.UnpackPoolHotkey, &out.PoolHotkey},
	} {
		if *read.out, err = stViewAt(self, ctx, carry, read.calldata, read.unpack); err != nil {
			return nil, fmt.Errorf("carry %s %s: %w", carry.Hex(), read.name, err)
		}
	}
	return out, nil
}

func (self *CoreStClient) CtfRecycledFor(ctx context.Context, carry common.Address, epoch uint64) (uint256.Int, error) {
	recycled, err := stViewAt(self, ctx, carry, stCtfCarryAbi.PackRecycledFor(stBig(epoch)), stCtfCarryAbi.UnpackRecycledFor)
	if err != nil {
		return uint256.Int{}, fmt.Errorf("carry %s recycledFor(%d): %w", carry.Hex(), epoch, err)
	}
	return stUint256(recycled)
}

func (self *CoreStClient) CtfCarryHeld(ctx context.Context, carry common.Address, identity *StCtfCarry) (*StCtfCarryHeld, error) {
	ctx, stopRead, err := beginStRpcRead(ctx, self.readHooks)
	if err != nil {
		return nil, err
	}
	defer stopRead()
	finalized, err := self.finalizedBlock(ctx)
	if err != nil {
		return nil, fmt.Errorf("finalized read head: %w", err)
	}
	block, out := finalized.Number, &StCtfCarryHeld{}
	for _, read := range []struct {
		name     string
		calldata []byte
		unpack   func([]byte) (*big.Int, error)
		out      *uint256.Int
	}{
		{"residual()", stCtfCarryAbi.PackResidual(), stCtfCarryAbi.UnpackResidual, &out.Residual},
		{"seedMinimum()", stCtfCarryAbi.PackSeedMinimum(), stCtfCarryAbi.UnpackSeedMinimum, &out.SeedMinimum},
	} {
		value, err := stViewAtBlock(self, ctx, carry, block, read.calldata, read.unpack)
		if err == nil {
			*read.out, err = stUint256(value)
		}
		if err != nil {
			return nil, fmt.Errorf("carry %s %s: %w", carry.Hex(), read.name, err)
		}
	}
	position, err := self.stakeAtBlock(ctx, block, identity.EscrowHotkey, identity.Coldkey)
	if err == nil {
		out.Position, err = stUint256(position)
	}
	if err != nil {
		return nil, fmt.Errorf("carry %s position on the escrow hotkey: %w", carry.Hex(), err)
	}
	var balance *big.Int
	err = self.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
		defer cancel()
		value, err := client.BalanceAt(callCtx, carry, new(big.Int).SetUint64(block))
		balance = value
		return err
	})
	if err == nil {
		// The runtime reports the balance the EVM can spend, above the
		// existential deposit; a seed stakes whole rao of it.
		out.SpendableRao, err = stUint256(new(big.Int).Div(balance, big.NewInt(1_000_000_000)))
	}
	if err != nil {
		return nil, fmt.Errorf("carry %s balance: %w", carry.Hex(), err)
	}
	return out, nil
}

func (self *CoreStClient) CtfCommitCensus(ctx context.Context, epoch uint64, noId uint64, hash [32]byte) (string, error) {
	return self.sendLedgerV2(ctx, stCtfCensusOperation(epoch, noId), self.cfg.SettlementV2, stLedgerV2SettlementAbi.PackCommitCtfCensus(stBig(epoch), stBig(noId), hash))
}

func (self *CoreStClient) CtfRecycle(ctx context.Context, carry common.Address, poolId uint64, rootIndex uint64, index uint64, account *merkle.LedgerAccount, proof []merkle.LedgerNode) (string, error) {
	argument := stabi.LedgerTreeAccount{Index: stBig(index), Coldkey: account.Coldkey, Cumulative: account.Cumulative.ToBig()}
	nodes := make([]stabi.LedgerTreeProofNode, len(proof))
	for i, node := range proof {
		nodes[i] = stabi.LedgerTreeProofNode{Hash: node.Hash, Sum: node.Sum.ToBig()}
	}
	return self.sendLedgerV2(ctx, stCtfRecycleOperation(rootIndex, poolId), carry, stCtfCarryAbi.PackRecycle(stBig(rootIndex), argument, nodes))
}

func (self *CoreStClient) CtfAbsorbStake(ctx context.Context, poolId uint64, rootIndex uint64, hotkey [32]byte) (string, error) {
	return self.sendLedgerV2(ctx, stCtfAbsorbOperation(rootIndex, poolId, hotkey), self.cfg.VaultV2, stLedgerV2VaultAbi.PackAbsorbStake(hotkey))
}

// stCtfStakingHotkeysKey is the storage key of SubtensorModule's
// StakingHotkeys map at a coldkey: twox128("SubtensorModule") ‖
// twox128("StakingHotkeys") ‖ blake2_128(coldkey) ‖ coldkey.
func stCtfStakingHotkeysKey(coldkey [32]byte) []byte {
	key := make([]byte, 0, 16+16+16+32)
	key = append(key, twox128([]byte("SubtensorModule"))...)
	key = append(key, twox128([]byte("StakingHotkeys"))...)
	h, err := blake2b.New(16, nil)
	if err != nil {
		panic(err)
	}
	h.Write(coldkey[:])
	key = append(key, h.Sum(nil)...)
	return append(key, coldkey[:]...)
}

// stCtfDecodeAccounts decodes a SCALE Vec<AccountId32>: a compact length,
// then 32 bytes an account.
func stCtfDecodeAccounts(value []byte) ([][32]byte, error) {
	if len(value) == 0 {
		return nil, nil
	}
	var n uint64
	var header int
	switch value[0] & 3 {
	case 0:
		n, header = uint64(value[0]>>2), 1
	case 1:
		if len(value) < 2 {
			return nil, errors.New("st ctf: short compact length")
		}
		n, header = uint64(binary.LittleEndian.Uint16(value[:2])>>2), 2
	case 2:
		if len(value) < 4 {
			return nil, errors.New("st ctf: short compact length")
		}
		n, header = uint64(binary.LittleEndian.Uint32(value[:4])>>2), 4
	default:
		return nil, errors.New("st ctf: an account list longer than 2^30")
	}
	if uint64(len(value)-header) != 32*n {
		return nil, fmt.Errorf("st ctf: %d bytes for %d accounts", len(value)-header, n)
	}
	accounts := make([][32]byte, n)
	for i := range accounts {
		copy(accounts[i][:], value[header+32*i:])
	}
	return accounts, nil
}

// CtfStakingHotkeys reads StakingHotkeys(coldkey) with state_getStorage from
// the subtensor endpoints, which serve the substrate JSON-RPC beside the EVM
// one (sn_substrate.go).
func (self *CoreStClient) CtfStakingHotkeys(ctx context.Context, coldkey [32]byte) ([][32]byte, error) {
	urls := snSubstrateRpcUrls()
	if len(urls) == 0 {
		return nil, errors.New("subtensor rpc is not configured")
	}
	key := "0x" + hex.EncodeToString(stCtfStakingHotkeysKey(coldkey))
	var lastErr error
	for _, url := range urls {
		value, err := stCtfSubstrateStorage(ctx, url, key)
		if err == nil {
			return stCtfDecodeAccounts(value)
		}
		lastErr = err
	}
	return nil, lastErr
}

// stCtfSubstrateStorage reads one storage value, nil when the key has none.
func stCtfSubstrateStorage(ctx context.Context, url string, key string) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "state_getStorage", "params": []string{key}})
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, snSubstrateRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := snSubstrateHttpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("substrate rpc: http %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out snJsonRpcResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("substrate rpc: %s", out.Error.Message)
	}
	if !snStorageValuePresent(out.Result) {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(out.Result, &value); err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.TrimPrefix(value, "0x"))
}

// stCtfCarryColdkey is the carry contract's coldkey, the carry account's leaf
// coldkey: blake2b-256("evm:" ‖ address), the coldkey every EVM contract has.
func stCtfCarryColdkey(carry common.Address) [32]byte {
	return ss58.EvmMirrorPubkey(carry)
}
