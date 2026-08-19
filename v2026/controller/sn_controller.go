// sn_controller implements the `/sn/*` subnet control-plane routes
// (sn/PLAN.md §5, D-13): the provider claim wallet, on-demand pool claim
// proofs, and the mirrored contract epoch clock. This file only reads
// settled state — chain writes belong to the st pipeline — and hands a
// provider everything needed to call `claim` on the subnet contract itself
// (D-2: claims are permissionless, keyed by ss58 coldkey).
//
// Wire contract: the result structs here must stay field-for-field
// compatible with the client bindings in connect/api_verify.go
// (SnSetWalletResult, SnPoolClaimResult, SnEpochResult). Raw 32-byte values
// (coldkey, noId word, root, proof nodes) travel as `[]byte` — base64 in
// JSON — and the contract address as 0x-hex.
package controller

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"net/http"

	"github.com/urfoundation/sn/v2026/merkle"
	"github.com/urfoundation/sn/v2026/ss58"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

type SnSetWalletArgs struct {
	ColdkeySs58 string `json:"coldkey_ss58"`
}

type SnSetWalletError struct {
	Message string `json:"message"`
}

type SnSetWalletResult struct {
	Error *SnSetWalletError `json:"error,omitempty"`
}

// SnSetWallet sets the caller network's subnet claim wallet — the ss58
// coldkey its pool payouts are claimable by. Deliberately separate from
// `account_wallet`/`payout_wallet` (D-2) so the USDC payout planner never
// sees subnet wallets. The upsert is idempotent; the latest set wins and
// applies from the next committed epoch's leaf set.
func SnSetWallet(
	setWallet *SnSetWalletArgs,
	clientSession *session.ClientSession,
) (*SnSetWalletResult, error) {
	coldkeyPubkey, err := ss58.DecodeWithPrefix(setWallet.ColdkeySs58, ss58.BittensorPrefix)
	if err != nil {
		return &SnSetWalletResult{
			Error: &SnSetWalletError{
				Message: fmt.Sprintf("Invalid ss58 coldkey: %s", err),
			},
		}, nil
	}

	model.SetStWallet(
		clientSession.Ctx,
		clientSession.ByJwt.NetworkId,
		setWallet.ColdkeySs58,
		coldkeyPubkey,
	)

	return &SnSetWalletResult{}, nil
}

// SnPoolClaimArgs selects the payout epoch. A nil epoch means the latest
// finalized epoch — claims are served against finalized epochs by default.
// (Epoch 0 is a real contract epoch, so absence is a nil, not a zero.)
type SnPoolClaimArgs struct {
	Epoch *uint64
}

type SnPoolClaimError struct {
	Message string `json:"message"`
}

// SnPoolClaimResult is everything a provider needs to call `claim` on the
// subnet contract (PLAN.md §5). `NoId` is the abi uint256 word (32-byte
// big-endian) the contract keys operators by; `Coldkey`, `PayoutRoot`, and
// each `Proof` node are raw 32-byte values. A claimant that earned nothing
// in the epoch gets `ShareBps` 0 with no proof. `Error` is set — instead of
// the claim fields — when the caller has no wallet or the epoch is not
// claimable; it is additive relative to the connect binding, which ignores
// unknown fields.
type SnPoolClaimResult struct {
	Epoch           uint64            `json:"epoch"`
	NoId            []byte            `json:"no_id"`
	Coldkey         []byte            `json:"coldkey"`
	ShareBps        int               `json:"share_bps"`
	Proof           [][]byte          `json:"proof"`
	PayoutRoot      []byte            `json:"payout_root"`
	ContractAddress string            `json:"contract_address"`
	ChainId         uint64            `json:"chain_id"`
	ClaimOpenBlock  uint64            `json:"claim_open_block"`
	Error           *SnPoolClaimError `json:"error,omitempty"`
}

// SnPoolClaim rebuilds the caller network's merkle pool claim for an epoch:
// wallet coldkey -> payout leaf -> proof against the committed payout root.
// The tree is rebuilt from the stored leaves in leaf_index order — the
// exact input order the committed root was built from — and the proof is
// self-verified before it is returned.
func SnPoolClaim(
	poolClaim *SnPoolClaimArgs,
	clientSession *session.ClientSession,
) (*SnPoolClaimResult, error) {
	ctx := clientSession.Ctx

	wallet := model.GetStWallet(ctx, clientSession.ByJwt.NetworkId)
	if wallet == nil {
		return &SnPoolClaimResult{
			Error: &SnPoolClaimError{
				Message: "No subnet wallet set for this network. Set an ss58 coldkey with POST /sn/wallet.",
			},
		}, nil
	}

	var stEpoch *model.StEpoch
	if poolClaim.Epoch != nil {
		stEpoch = model.GetStEpoch(ctx, *poolClaim.Epoch)
	} else {
		stEpoch = model.GetLatestFinalizedStEpoch(ctx)
	}
	if stEpoch == nil {
		return &SnPoolClaimResult{
			Error: &SnPoolClaimError{
				Message: "No claimable epoch.",
			},
		}, nil
	}

	// leaves remain replaceable until the root is committed on chain
	// (SetStPayoutLeaves); only serve proofs once the tree can no longer
	// change. Claims execute at finalize — ClaimOpenBlock below.
	switch stEpoch.Status {
	case model.StEpochStatusCommitted, model.StEpochStatusFinalized:
	default:
		return &SnPoolClaimResult{
			Epoch: stEpoch.Epoch,
			Error: &SnPoolClaimError{
				Message: fmt.Sprintf("Epoch %d payout root is not committed yet.", stEpoch.Epoch),
			},
		}, nil
	}

	coldkey := wallet.ColdkeyPubkey

	noId, ok := getStPayoutNoIdForColdkey(ctx, stEpoch.Epoch, coldkey)
	if !ok {
		// the coldkey has no leaf: the provider earned nothing this epoch
		return &SnPoolClaimResult{
			Epoch:   stEpoch.Epoch,
			Coldkey: coldkey[:],
		}, nil
	}

	leaf := model.GetStPayoutLeafForColdkey(ctx, stEpoch.Epoch, noId, coldkey)
	if leaf == nil {
		// unreachable: noId was derived from this coldkey's leaf row
		return nil, fmt.Errorf("payout leaf missing for epoch %d no_id %d", stEpoch.Epoch, noId)
	}

	leaves := model.GetStPayoutLeaves(ctx, stEpoch.Epoch, noId)
	root, proof, err := snPoolClaimProof(leaves, coldkey, leaf.ShareBps)
	if err != nil {
		// a proof that does not self-verify is a server bug; never hand it out
		return nil, err
	}

	result := &SnPoolClaimResult{
		Epoch:          stEpoch.Epoch,
		NoId:           snNoIdBytes(noId),
		Coldkey:        coldkey[:],
		ShareBps:       leaf.ShareBps,
		Proof:          snProofBytes(proof),
		PayoutRoot:     root[:],
		ClaimOpenBlock: stEpoch.FinalizeBlock,
	}
	// contract coordinates ride along from the hot epoch mirror; on a cache
	// miss they are zero and the claimant falls back to its own config
	if summary := model.GetStEpochSummaryCache(ctx); summary != nil {
		result.ContractAddress = summary.ContractAddress
		result.ChainId = summary.ChainId
	}
	return result, nil
}

// getStPayoutNoIdForColdkey finds which operator pool (noId) holds a payout
// leaf for the coldkey in the epoch — the st_model leaf accessors all key by
// noId, and the claim route does not take one. With the single-NO bootstrap
// (PLAN.md §10) a coldkey has at most one pool leaf; should multiple
// operators ever pool the same coldkey, the lowest noId is served.
func getStPayoutNoIdForColdkey(ctx context.Context, epoch uint64, coldkey [32]byte) (uint64, bool) {
	var noId uint64
	var found bool
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT no_id
                FROM st_payout_leaf
                WHERE epoch = $1 AND coldkey = $2
                ORDER BY no_id ASC
                LIMIT 1
            `,
			int64(epoch),
			coldkey[:],
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var noIdInt int64
				server.Raise(result.Scan(&noIdInt))
				noId = uint64(noIdInt)
				found = true
			}
		})
	})
	return noId, found
}

// snPoolClaimProof rebuilds the canonical payout tree from the stored leaf
// rows and extracts the proof for (coldkey, shareBps). The proof is
// self-checked with merkle.Verify before it is returned: the rebuilt tree
// must reproduce the exact root committed on chain, so a proof that fails
// locally is a server bug and must never reach a claimant. A (coldkey,
// shareBps) pair not in the leaf set returns merkle.ErrUnknownLeaf.
func snPoolClaimProof(
	leaves []*model.StPayoutLeaf,
	coldkey [32]byte,
	shareBps int,
) ([32]byte, [][32]byte, error) {
	merkleLeaves := make([]merkle.Leaf, len(leaves))
	for i, leaf := range leaves {
		merkleLeaves[i] = merkle.PayoutLeaf(leaf.Coldkey, big.NewInt(int64(leaf.ShareBps)))
	}
	tree, err := merkle.NewTree(merkleLeaves)
	if err != nil {
		return [32]byte{}, nil, err
	}

	claimLeaf := merkle.PayoutLeaf(coldkey, big.NewInt(int64(shareBps)))
	proof, err := tree.Proof(claimLeaf)
	if err != nil {
		return [32]byte{}, nil, err
	}

	root := tree.Root()
	if !merkle.Verify(root, claimLeaf, proof) {
		return [32]byte{}, nil, fmt.Errorf("payout proof failed self-verification")
	}
	return root, proof, nil
}

// snNoIdBytes abi-encodes a noId as the uint256 word the contract keys
// operators by: 32 bytes, big-endian.
func snNoIdBytes(noId uint64) []byte {
	word := make([]byte, 32)
	binary.BigEndian.PutUint64(word[24:], noId)
	return word
}

// snProofBytes flattens proof nodes to the `[][]byte` wire shape (each node
// 32 bytes, base64 in JSON). An empty proof (single-leaf tree) marshals as
// `[]`, not `null`.
func snProofBytes(proof [][32]byte) [][]byte {
	out := make([][]byte, len(proof))
	for i := range proof {
		out[i] = proof[i][:]
	}
	return out
}

// SnEpoch returns the contract epoch clock mirrored from chain — the
// read-through Redis summary the chain sync task refreshes — so clients do
// not need their own RPC. On a cache miss it degrades to the newest
// `st_epoch` row: the block deadlines remain exact but `t_epoch_blocks`,
// `chain_id`, and `contract_address` are zero until the sync task
// repopulates the cache. With no epoch state at all it returns 503 so
// callers can tell "not synced yet" apart from a real epoch-0 mirror.
func SnEpoch(clientSession *session.ClientSession) (*model.StEpochSummary, error) {
	ctx := clientSession.Ctx

	if summary := model.GetStEpochSummaryCache(ctx); summary != nil {
		return summary, nil
	}

	if stEpoch := model.GetLatestStEpoch(ctx); stEpoch != nil {
		return &model.StEpochSummary{
			Epoch:               stEpoch.Epoch,
			StartBlock:          stEpoch.StartBlock,
			CommitDeadlineBlock: stEpoch.CommitDeadlineBlock,
			TrailsDeadlineBlock: stEpoch.TrailsDeadlineBlock,
			FinalizeBlock:       stEpoch.FinalizeBlock,
		}, nil
	}

	return nil, fmt.Errorf("%d Subnet epoch state is not synced yet.", http.StatusServiceUnavailable)
}
