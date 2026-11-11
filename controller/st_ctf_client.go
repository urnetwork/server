package controller

// st_ctf_client.go — the capture the flag chain surface (sn CAPTURETHEFLAG.md
// §3.1, §4.3, §4.6) beside the v2 one. The CTF pool is the vault2 pool
// P = 2^32 + n, so every vault2 and STSettlement2 call that takes a pool id
// (state, close, commit, finalize, answer, fundedThrough, claimed) is the v2
// client's with P (StLedgerV2Client). What is new is the census commitment on
// STSettlement2Ctf, which takes the operator id n, and the carry contract
// (STCtfCarry): its records of returned carry and its recycle. Writes are
// durable intents of the existing engine, signed by the root key.

import (
	"context"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

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

// StCtfCarry is the carry contract's immutable identity as it reads it.
type StCtfCarry struct {
	Vault   common.Address
	PoolId  uint64
	Coldkey [32]byte
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
	// CtfCommitCensus sends commitCtfCensus(epoch, noId, hash) under
	// ctfcensus:e:no.
	CtfCommitCensus(ctx context.Context, epoch uint64, noId uint64, hash [32]byte) (string, error)
	// CtfRecycle sends the carry contract's recycle(rootIndex, account,
	// proof) under ctfrecycle:rootIndex:P. A nil account names none (zero
	// coldkey): the carry account was claimed already, and only what the
	// carry holds moves.
	CtfRecycle(ctx context.Context, carry common.Address, poolId uint64, rootIndex uint64, index uint64, account *merkle.LedgerAccount, proof []merkle.LedgerNode) (string, error)
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
	if out.Coldkey, err = stViewAt(self, ctx, carry, stCtfCarryAbi.PackColdkey(), stCtfCarryAbi.UnpackColdkey); err != nil {
		return nil, fmt.Errorf("carry %s coldkey(): %w", carry.Hex(), err)
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

func (self *CoreStClient) CtfCommitCensus(ctx context.Context, epoch uint64, noId uint64, hash [32]byte) (string, error) {
	return self.sendLedgerV2(ctx, stCtfCensusOperation(epoch, noId), self.cfg.SettlementV2, stLedgerV2SettlementAbi.PackCommitCtfCensus(stBig(epoch), stBig(noId), hash))
}

func (self *CoreStClient) CtfRecycle(ctx context.Context, carry common.Address, poolId uint64, rootIndex uint64, index uint64, account *merkle.LedgerAccount, proof []merkle.LedgerNode) (string, error) {
	argument := stabi.LedgerTreeAccount{Index: stBig(index), Cumulative: new(big.Int)}
	nodes := []stabi.LedgerTreeProofNode{}
	if account != nil {
		argument.Coldkey, argument.Cumulative = account.Coldkey, account.Cumulative.ToBig()
		for _, node := range proof {
			nodes = append(nodes, stabi.LedgerTreeProofNode{Hash: node.Hash, Sum: node.Sum.ToBig()})
		}
	} else {
		argument.Index = new(big.Int)
	}
	return self.sendLedgerV2(ctx, stCtfRecycleOperation(rootIndex, poolId), carry, stCtfCarryAbi.PackRecycle(stBig(rootIndex), argument, nodes))
}

// stCtfCarryColdkey is the carry contract's coldkey, the carry account's leaf
// coldkey: blake2b-256("evm:" ‖ address), the coldkey every EVM contract has.
func stCtfCarryColdkey(carry common.Address) [32]byte {
	return ss58.EvmMirrorPubkey(carry)
}
