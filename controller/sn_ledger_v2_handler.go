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
	"strings"
	"sync"

	"github.com/holiman/uint256"

	"github.com/urnetwork/glog"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/ss58"

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

// StLedgerV2AccountsResult is the answer of GET /sn/ledger/account (SPEC.md):
// the coldkey as 0x and 64 lowercase hex digits, and its accounts by no_id.
type StLedgerV2AccountsResult struct {
	Coldkey  string                    `json:"coldkey"`
	Accounts []*StLedgerV2AccountEntry `json:"accounts"`
}

// StLedgerV2AccountEntry is the coldkey's account in one pool. Amounts are
// canonical decimal strings of rao.
type StLedgerV2AccountEntry struct {
	Vault string `json:"vault"`
	NoId  uint64 `json:"no_id"`
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

// SnLedgerAccount serves GET /sn/ledger/account?coldkey=<ss58|0x hex>.
func SnLedgerAccount(w http.ResponseWriter, r *http.Request) {
	coldkey, err := ParseStLedgerV2Coldkey(r.URL.Query().Get("coldkey"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	accounts, err := StLedgerV2Accounts(r.Context(), coldkey)
	if err != nil {
		glog.Warningf("[sn]ledger account 0x%x: %s\n", coldkey, err)
		http.Error(w, "Ledger account unavailable.", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
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
// the pending cumulative. The pools are the server's own and those where the
// account index records the coldkey. Without settlement v2 the list is empty.
func StLedgerV2Accounts(ctx context.Context, coldkey [32]byte) (*StLedgerV2AccountsResult, error) {
	result := &StLedgerV2AccountsResult{Coldkey: fmt.Sprintf("0x%x", coldkey), Accounts: []*StLedgerV2AccountEntry{}}
	cfg := stConfig()
	if !cfg.LedgerV2Enabled() {
		return result, nil
	}
	recorded := map[uint64]uint64{}
	noIds := []uint64{cfg.NoId}
	for _, account := range model.GetStLedgerV2AccountsForColdkey(ctx, cfg.DeploymentKey(), coldkey) {
		recorded[account.NoId] = account.Index
		if account.NoId != cfg.NoId {
			noIds = append(noIds, account.NoId)
		}
	}
	slices.Sort(noIds)
	for _, noId := range noIds {
		index, isRecorded := recorded[noId]
		entry, err := stLedgerV2AccountEntry(ctx, cfg, noId, coldkey, index, isRecorded)
		if err != nil {
			return nil, err
		}
		if entry != nil {
			result.Accounts = append(result.Accounts, entry)
		}
	}
	return result, nil
}

// One pool's entry, or nil when neither root holds the coldkey. A recorded
// account has its index from the account index; any other account was
// appended after the recorded ones, by a finalized root not recorded yet or
// by the pending root.
func stLedgerV2AccountEntry(ctx context.Context, cfg *StConfig, noId uint64, coldkey [32]byte, index uint64, recorded bool) (*StLedgerV2AccountEntry, error) {
	deploymentKey := cfg.DeploymentKey()
	finalized := model.GetLatestStLedgerV2FinalizedRoot(ctx, deploymentKey, noId)
	pending := model.GetStLedgerV2PendingRoot(ctx, deploymentKey, noId)
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
	entry := &StLedgerV2AccountEntry{Vault: stLedgerV2Address(cfg.VaultV2), NoId: noId, Index: index, Cumulative: "0", Claimed: "0"}
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
