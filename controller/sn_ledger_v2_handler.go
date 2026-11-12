package controller

// sn_ledger_v2_handler.go — the public settlement v2 ledger endpoints
// (VERSION2-FINAL.md §8.3), served beside /sn/artifact: a ledger object by its
// sha256, and a coldkey's account in every pool with its claim proof. Both are
// public: a proof only reveals published data and cannot redirect a payment.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/holiman/uint256"

	"github.com/urnetwork/glog"

	"github.com/urfoundation/sn/amendment"
	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/ss58"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

// stLedgerV2AccountTreeLock serializes the endpoint's tree reads, so that the
// requests that arrive together after a new root build its tree once and then
// share the cached one.
var stLedgerV2AccountTreeLock sync.Mutex

// ErrStLedgerV2Coldkey refuses a coldkey that is neither an ss58 address with
// the Bittensor prefix nor 0x and 64 hex digits.
var ErrStLedgerV2Coldkey = errors.New("coldkey is not an ss58 address or 0x and 64 hex digits")

// StLedgerV2AccountsResult is the answer of GET /sn/ledger/account (SPEC.md,
// sn/api/operator.yml): the coldkey as 0x and 64 lowercase hex digits, its
// accounts by no_id, and, for a partial answer, the pools whose ledger could
// not be read.
type StLedgerV2AccountsResult struct {
	Coldkey  string                    `json:"coldkey"`
	Accounts []*StLedgerV2AccountEntry `json:"accounts"`
	// UnavailablePools is [] but for an answer asked with partial=true: an
	// answer without it leaves a capture the flag pool that cannot be read out
	// of Accounts instead.
	UnavailablePools []*StLedgerV2UnavailablePool `json:"unavailable_pools"`
}

// StLedgerV2AccountEntry is the coldkey's account in one pool. Amounts are
// canonical decimal strings of rao.
type StLedgerV2AccountEntry struct {
	Vault string `json:"vault"`
	NoId  uint64 `json:"no_id"`
	// Kind is StLedgerV2PoolKind*, and Label the pool's English display name.
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Index uint64 `json:"index"`
	// RootIndex is the latest finalized root's index. It is nil for an account
	// only in the pending root, which has a zero Cumulative and Claimed and no
	// Proof.
	RootIndex  *uint64 `json:"root_index"`
	Cumulative string  `json:"cumulative"`
	// Proof holds the bottom-up siblings, none at depth 0.
	Proof   []StLedgerV2ProofNode `json:"proof"`
	Claimed string                `json:"claimed"`
	// PendingCumulative is nil when no pending root holds the account.
	PendingCumulative *string `json:"pending_cumulative"`
}

// StLedgerV2ProofNode is one sibling of a claim proof.
type StLedgerV2ProofNode struct {
	Hash string `json:"hash"`
	Sum  string `json:"sum"`
}

// StLedgerV2UnavailablePool is a pool a partial answer would cover whose
// ledger could not be read: whether the coldkey has an account there is
// unknown, never zero.
type StLedgerV2UnavailablePool struct {
	Vault string `json:"vault"`
	NoId  uint64 `json:"no_id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

// The kinds of pool (sn/api/operator.yml SnLedgerPoolKind): an operator's
// pool, no_id n below 2^32, which pays providers and, from E_x, extenders;
// and an operator's capture the flag pool, 2^32 + n (CAPTURETHEFLAG.md §3.1).
const (
	StLedgerV2PoolKindPool = "pool"
	StLedgerV2PoolKindCtf  = "ctf"
)

// StLedgerV2PoolKind is a pool's kind and its English display label: "Pool
// n", as the apps name an operator's pool, or "Capture the flag".
func StLedgerV2PoolKind(noId uint64) (kind string, label string) {
	if _, ok := amendment.CtfNoID(noId); ok {
		return StLedgerV2PoolKindCtf, "Capture the flag"
	}
	return StLedgerV2PoolKindPool, fmt.Sprintf("Pool %d", noId)
}

// stLedgerV2OwnPools are the pools this operator answers for whether or not
// its account record holds the coldkey: its pool, no_id n, and, when ctfOn
// (stCtfPoolOn), its capture the flag pool, 2^32 + n, in ascending no_id. A
// pool none of whose roots is mirrored holds no account.
func stLedgerV2OwnPools(cfg *StConfig, ctfOn bool) []uint64 {
	noIds := []uint64{cfg.NoId}
	if ctfPoolId, ok := stCtfPoolId(cfg); ok && ctfOn {
		noIds = append(noIds, ctfPoolId)
	}
	return noIds
}

// SnLedgerObject serves GET /sn/ledger/object?hash=sha256:<hex>: the immutable
// bytes of a ledger object, served only after their sha256 matches the hash.
func SnLedgerObject(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("hash")
	if _, err := startifact.ParseLedgerObjectRef(ref); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	object, err := GetStLedgerV2Object(r.Context(), ref)
	if errors.Is(err, startifact.ErrLedgerObjectNotFound) {
		http.Error(w, "Ledger object not found.", http.StatusNotFound)
		return
	}
	if err != nil {
		glog.Warningf("[sn]ledger object %s: %s\n", ref, err)
		http.Error(w, "Ledger object unavailable or failed integrity.", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+ref+`"`)
	w.Header().Set("Cache-Control", "public, immutable, max-age=31536000")
	_, _ = w.Write(object)
}

// SnLedgerAccount serves GET /sn/ledger/account?coldkey=<ss58|0x
// hex>[&partial=true]. With partial=true a pool whose ledger cannot be read is
// named in unavailable_pools. Without it, as old apps ask, a capture the flag
// pool that cannot be read is left out and the other pools are answered as
// before it existed, and any other pool that cannot be read is a 502. Errors
// are problem details (sn/api/operator.yml).
func SnLedgerAccount(w http.ResponseWriter, r *http.Request) {
	SnOperatorCors(w, r)
	query := r.URL.Query()
	coldkey, err := ParseStLedgerV2Coldkey(query.Get("coldkey"))
	if err != nil {
		WriteSnProblem(w, http.StatusBadRequest, SnProblemBadRequest, err.Error())
		return
	}
	partial, _ := strconv.ParseBool(query.Get("partial"))
	accounts, err := stLedgerV2Accounts(r.Context(), coldkey, partial)
	if err != nil {
		glog.Warningf("[sn]ledger account 0x%x: %s\n", coldkey, err)
		WriteSnProblem(w, http.StatusBadGateway, SnProblemLedgerUnavailable, "Ledger account unavailable.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Link", SnOperatorLink)
	_ = json.NewEncoder(w).Encode(accounts)
}

// ParseStLedgerV2Coldkey decodes an ss58 address with the Bittensor prefix,
// or 0x and 64 hex digits in either case.
func ParseStLedgerV2Coldkey(value string) ([32]byte, error) {
	if digits, ok := strings.CutPrefix(value, "0x"); ok {
		var coldkey [32]byte
		if len(digits) != 64 {
			return coldkey, ErrStLedgerV2Coldkey
		}
		if _, err := hex.Decode(coldkey[:], []byte(digits)); err != nil {
			return [32]byte{}, ErrStLedgerV2Coldkey
		}
		return coldkey, nil
	}
	coldkey, err := ss58.DecodeWithPrefix(value, ss58.BittensorPrefix)
	if err != nil {
		return [32]byte{}, ErrStLedgerV2Coldkey
	}
	return coldkey, nil
}

// StLedgerV2Accounts returns the coldkey's account in every pool whose latest
// finalized root or pending root holds one, by no_id: the finalized cumulative
// with its claim proof and vault2's claimed, read at the finalized head, and
// the pending cumulative. The pools are the server's own, its pool and, while
// capture the flag is on (stCtfPoolOn), its capture the flag pool, and those
// where the account index records the coldkey, but no capture the flag pool
// while it is off. Without settlement v2 the list is empty.
//
// It answers a client that does not ask for a partial answer, as every app
// that predates the capture the flag pool asks: the answer names no pool
// unavailable. A capture the flag pool that cannot be read is left out, so
// the other pools are answered exactly as before that pool existed and only
// its balance is missing until it can be read again (owner, 2026-11-11). Any
// other pool that cannot be read is an error, as before.
func StLedgerV2Accounts(ctx context.Context, coldkey [32]byte) (*StLedgerV2AccountsResult, error) {
	return stLedgerV2Accounts(ctx, coldkey, false)
}

// StLedgerV2AccountsPartial is StLedgerV2Accounts with each pool read on its
// own: a pool whose ledger cannot be read is left out of the accounts and
// named in UnavailablePools. It is still an error when the account record
// cannot be read, since then the pools to answer are unknown.
func StLedgerV2AccountsPartial(ctx context.Context, coldkey [32]byte) (*StLedgerV2AccountsResult, error) {
	return stLedgerV2Accounts(ctx, coldkey, true)
}

func stLedgerV2Accounts(ctx context.Context, coldkey [32]byte, partial bool) (*StLedgerV2AccountsResult, error) {
	result := &StLedgerV2AccountsResult{Coldkey: fmt.Sprintf("0x%x", coldkey), Accounts: []*StLedgerV2AccountEntry{}, UnavailablePools: []*StLedgerV2UnavailablePool{}}
	cfg := stConfig()
	if !cfg.LedgerV2Enabled() {
		return result, nil
	}
	recorded := map[uint64]uint64{}
	ctfOn := stCtfPoolOn(cfg)
	noIds := stLedgerV2OwnPools(cfg, ctfOn)
	var accounts []*model.StLedgerV2Account
	if err := stLedgerV2Guard(func() error {
		accounts = model.GetStLedgerV2AccountsForColdkey(ctx, cfg.DeploymentKey(), coldkey)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("st ledger v2: the account record of 0x%x: %w", coldkey, err)
	}
	for _, account := range accounts {
		if _, ctfPool := amendment.CtfNoID(account.NoId); ctfPool && !ctfOn {
			// capture the flag is off: none of its pools is read
			continue
		}
		recorded[account.NoId] = account.Index
		if !slices.Contains(noIds, account.NoId) {
			noIds = append(noIds, account.NoId)
		}
	}
	slices.Sort(noIds)
	for _, noId := range noIds {
		index, isRecorded := recorded[noId]
		var entry *StLedgerV2AccountEntry
		err := stLedgerV2Guard(func() (err error) {
			entry, err = stLedgerV2AccountEntry(ctx, cfg, noId, coldkey, index, isRecorded)
			return
		})
		if err != nil {
			kind, label := StLedgerV2PoolKind(noId)
			switch {
			case partial:
				glog.Warningf("[sn]ledger account 0x%x: pool %d is unavailable: %s\n", coldkey, noId, err)
				result.UnavailablePools = append(result.UnavailablePools, &StLedgerV2UnavailablePool{Vault: stLedgerV2Address(cfg.VaultV2), NoId: noId, Kind: kind, Label: label})
			case kind == StLedgerV2PoolKindCtf:
				// A client that knows no unavailable pool gets the others as
				// before the capture the flag pool, never an error for it.
				glog.Warningf("[sn]ledger account 0x%x: capture the flag pool %d is unavailable and left out: %s\n", coldkey, noId, err)
			default:
				return nil, err
			}
			continue
		}
		if entry != nil {
			result.Accounts = append(result.Accounts, entry)
		}
	}
	return result, nil
}

// stLedgerV2Guard runs a read whose store raises on a failure, as the model
// does, and returns the raise as its error.
func stLedgerV2Guard(read func() error) (err error) {
	server.HandleError(func() {
		err = read()
	}, func(raised error) {
		err = raised
	})
	return
}

// One pool's entry, or nil when neither root holds the coldkey. A recorded
// account has its index from the account index; any other account was
// appended after the recorded ones, by a finalized root not recorded yet or
// by the pending root.
func stLedgerV2AccountEntry(ctx context.Context, cfg *StConfig, noId uint64, coldkey [32]byte, index uint64, recorded bool) (*StLedgerV2AccountEntry, error) {
	deploymentKey := cfg.DeploymentKey()
	finalized := model.GetLatestStLedgerV2FinalizedRoot(ctx, deploymentKey, noId)
	pending := model.GetStLedgerV2PendingRoot(ctx, deploymentKey, noId)
	if finalized == nil && pending == nil && !recorded {
		// no root holds any account of the pool, like the capture the flag
		// pool's before its first
		return nil, nil
	}
	var tree *merkle.LedgerSumTree
	var finalizedAccounts, pendingAccounts []merkle.LedgerAccount
	var err error
	if finalized != nil {
		if tree, finalizedAccounts, err = stLedgerV2AccountTree(ctx, finalized); err != nil {
			return nil, err
		}
	}
	if pending != nil {
		if _, pendingAccounts, err = stLedgerV2AccountTree(ctx, pending); err != nil {
			return nil, err
		}
	}
	found := recorded
	if !found {
		index, found = stLedgerV2FindAccount(coldkey, model.GetStLedgerV2AccountCount(ctx, deploymentKey, noId), finalizedAccounts, pendingAccounts)
	}
	if !found {
		return nil, nil
	}
	kind, label := StLedgerV2PoolKind(noId)
	entry := &StLedgerV2AccountEntry{Vault: stLedgerV2Address(cfg.VaultV2), NoId: noId, Kind: kind, Label: label, Index: index, Cumulative: "0", Claimed: "0"}
	if index < uint64(len(finalizedAccounts)) && finalizedAccounts[index].Coldkey == coldkey {
		cumulative := finalizedAccounts[index].Cumulative
		client := stLedgerV2Client()
		if client == nil {
			return nil, errStLedgerV2NotConfigured
		}
		claimed, err := client.LedgerV2Claimed(ctx, noId, index)
		if err != nil {
			return nil, err
		}
		if cumulative.Lt(&claimed) {
			return nil, fmt.Errorf("st ledger v2: vault2 claimed(%d, %d) is %s, above the finalized cumulative %s", noId, index, claimed.Dec(), cumulative.Dec())
		}
		proof, _ := tree.Proof(index)
		entry.RootIndex, entry.Cumulative, entry.Claimed = finalized.RootIndex, cumulative.Dec(), claimed.Dec()
		entry.Proof = make([]StLedgerV2ProofNode, len(proof))
		for k, node := range proof {
			entry.Proof[k] = StLedgerV2ProofNode{Hash: fmt.Sprintf("0x%x", node.Hash), Sum: node.Sum.Dec()}
		}
	} else if recorded {
		return nil, fmt.Errorf("st ledger v2: the finalized root of pool %d does not hold coldkey 0x%x at its recorded index %d", noId, coldkey, index)
	}
	if index < uint64(len(pendingAccounts)) && pendingAccounts[index].Coldkey == coldkey {
		pendingCumulative := pendingAccounts[index].Cumulative.Dec()
		entry.PendingCumulative = &pendingCumulative
	}
	return entry, nil
}

func stLedgerV2AccountTree(ctx context.Context, root *model.StLedgerV2Root) (*merkle.LedgerSumTree, []merkle.LedgerAccount, error) {
	stLedgerV2AccountTreeLock.Lock()
	defer stLedgerV2AccountTreeLock.Unlock()
	return StLedgerV2Tree(ctx, root.LedgerHash, root.Root, &root.Total, root.LeafCount)
}

// The coldkey's position among each list's accounts from first on.
func stLedgerV2FindAccount(coldkey [32]byte, first uint64, lists ...[]merkle.LedgerAccount) (uint64, bool) {
	for _, accounts := range lists {
		for i := first; i < uint64(len(accounts)); i++ {
			if accounts[i].Coldkey == coldkey {
				return i, true
			}
		}
	}
	return 0, false
}

// LedgerV2Claimed reads vault2.claimed(noId, index) at the finalized head.
func (self *CoreStClient) LedgerV2Claimed(ctx context.Context, noId uint64, index uint64) (uint256.Int, error) {
	claimed, err := stViewAt(self, ctx, self.cfg.VaultV2, stLedgerV2VaultAbi.PackClaimed(stBig(noId), stBig(index)), stLedgerV2VaultAbi.UnpackClaimed)
	if err != nil {
		return uint256.Int{}, fmt.Errorf("vault2 claimed(%d, %d): %w", noId, index, err)
	}
	return stUint256(claimed)
}
