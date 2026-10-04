// Payout production attaches independently admitted SDK coverage before signing.
// Missing historical components remain unknown; a SQL count never enrolls owners.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

// The caller supplies the already retained same-SQL window and independently
// read epoch boundaries. Every expected owner comes solely from the root roster.
func stPrepareWholeWorkInventory(ctx context.Context, cfg *StConfig, epoch *StPayoutEpochAuthority, window *payoutartifact.ClosedWorkWindow) (*payoutartifact.WholeWorkInventory, payoutartifact.WholeWorkExpectation, error) {
	approved, err := stLoadProviderWorkAuthority(ctx, cfg, epoch)
	if err != nil {
		return nil, payoutartifact.WholeWorkExpectation{}, err
	}
	return stPrepareApprovedWholeWorkInventory(ctx, approved, epoch, window)
}

// The production caller already used this exact authority for all provider
// input queries. The same retained bytes then own the signed inventory join.
func stPrepareApprovedWholeWorkInventory(ctx context.Context, approved *stProviderWorkAuthority, epoch *StPayoutEpochAuthority, window *payoutartifact.ClosedWorkWindow) (*payoutartifact.WholeWorkInventory, payoutartifact.WholeWorkExpectation, error) {
	if approved == nil {
		return nil, payoutartifact.WholeWorkExpectation{}, nil
	}
	raw, authority, expected := approved.Raw, approved.Authority, approved.Expectation
	domainHash, err := authority.Domain.Digest()
	if err != nil || epoch == nil {
		return nil, expected, errors.Join(model.ErrProviderWorkInvalid, err)
	}
	approver := authority.RequestPublicKey
	start := payoutartifact.Boundary{Number: epoch.Start.Block, Hash: common.Hash(epoch.Start.Hash).Hex()}
	end := payoutartifact.Boundary{Number: epoch.End.Block, Hash: common.Hash(epoch.End.Hash).Hex()}
	if authority.Epoch != epoch.Epoch || authority.Start != start || authority.End != end {
		return nil, expected, model.ErrProviderWorkConflict
	}
	// Historical absence cannot become an invented empty window or timestamp.
	if authority.ExpectedProviders == nil || window == nil || len(epoch.StartHeader) == 0 || len(epoch.EndHeader) == 0 {
		return nil, expected, nil
	}
	if authority.ClockProfile != epoch.ClockProfile {
		return nil, expected, model.ErrProviderWorkConflict
	}
	result := &payoutartifact.WholeWorkInventory{Schema: payoutartifact.WholeWorkInventorySchema, Authority: raw, Owners: make([]payoutartifact.WholeWorkOwnerCuts, 0, len(authority.Owners)), Window: window, Clock: &payoutartifact.ClosedWorkWindowClock{HeaderProfile: epoch.ClockProfile, Start: start, End: end, StartTime: epoch.StartTime, EndTime: epoch.EndTime, StartHeader: bytes.Clone(epoch.StartHeader), EndHeader: bytes.Clone(epoch.EndHeader)}}
	used := len(raw)
	for _, owner := range authority.Owners {
		if err := ctx.Err(); err != nil {
			return nil, expected, err
		}
		identity := model.ProviderWorkOwner{DomainHash: domainHash, ClientId: owner.ClientId, Generation: owner.Generation, PublicKey: owner.PublicKey}
		startPair, err := model.GetProviderWorkBoundary(ctx, identity, epoch.Epoch, "start", approver)
		if errors.Is(err, model.ErrProviderWorkMissing) {
			return nil, expected, nil
		}
		if err != nil {
			return nil, expected, err
		}
		endPair, err := model.GetProviderWorkBoundary(ctx, identity, epoch.Epoch, "end", approver)
		if errors.Is(err, model.ErrProviderWorkMissing) {
			return nil, expected, nil
		}
		if err != nil {
			return nil, expected, err
		}
		for _, original := range [][]byte{startPair.Request, startPair.Cut, endPair.Request, endPair.Cut} {
			if len(original) > payoutartifact.MaxWholeWorkInventoryBytes-used {
				return nil, expected, payoutartifact.ErrClosedWorkCapacity
			}
			used += len(original)
		}
		result.Owners = append(result.Owners, payoutartifact.WholeWorkOwnerCuts{StartRequest: startPair.Request, Start: startPair.Cut, EndRequest: endPair.Request, End: endPair.Cut})
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, expected, err
	}
	owned, err := payoutartifact.DecodeWholeWorkInventory(ctx, encoded)
	expected.AuthorityHash = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	if err != nil {
		return nil, expected, err
	}
	expected, err = providerWorkPriorExpectation(ctx, authority, expected)
	if err != nil {
		return nil, expected, err
	}
	return owned, expected, nil
}

// Publication first verifies the final signed artifact and retains the exact
// component under both hashes. An uncertain receipt is safely replayable.
func stRetainWholeWorkWindow(ctx context.Context, artifact *payoutartifact.Artifact, expected payoutartifact.WholeWorkExpectation) error {
	if artifact == nil || artifact.ClosedWork == nil || artifact.ClosedWork.WholeInventory == nil {
		return nil
	}
	if _, err := payoutartifact.VerifyWholeWorkInventory(ctx, artifact, expected); err != nil {
		return err
	}
	raw, err := json.Marshal(artifact.ClosedWork.WholeInventory)
	if err != nil {
		return err
	}
	digest, err := providerWorkPolicyHash(strings.TrimPrefix(artifact.ContentHash, "sha256:"))
	if err != nil {
		return err
	}
	authority := sha256.Sum256(artifact.ClosedWork.WholeInventory.Authority)
	return model.RetainProviderWorkWindow(ctx, digest, authority, raw)
}

// A public companion is read by exact artifact and optionally exact authority.
// Historical absence never triggers a new current-SQL reconstruction.
func ProviderWorkWindow(ctx context.Context, domainHash [32]byte, epoch uint64, artifactHash, authorityHash [32]byte) ([]byte, error) {
	domain, approver, expected, err := LoadProviderWorkAuthorityPolicy()
	if err != nil {
		return nil, err
	}
	if domain != domainHash {
		return nil, model.ErrProviderWorkInvalid
	}
	authorityRaw, authority, err := model.GetProviderWorkAuthority(ctx, domain, epoch, expected.AuthoritySigner)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(authorityRaw)
	if authorityHash != ([32]byte{}) && digest != authorityHash || authority.RequestPublicKey != approver {
		return nil, model.ErrProviderWorkConflict
	}
	raw, err := model.GetProviderWorkWindow(ctx, artifactHash, digest)
	if err != nil {
		return nil, err
	}
	inventory, err := payoutartifact.DecodeWholeWorkInventory(ctx, raw)
	if err != nil {
		return nil, errors.Join(model.ErrProviderWorkConflict, err)
	}
	if !bytes.Equal(inventory.Authority, authorityRaw) {
		return nil, model.ErrProviderWorkConflict
	}
	store, ok := server.LoadBlobStore()
	if !ok {
		return nil, errors.New("provider work artifact store unavailable")
	}
	artifact, _, err := startifact.Read(ctx, store, "sha256:"+hex.EncodeToString(artifactHash[:]))
	if err != nil {
		return nil, err
	}
	expected.AuthorityHash = "sha256:" + hex.EncodeToString(digest[:])
	expected, err = providerWorkPriorExpectation(ctx, authority, expected)
	if err != nil {
		return nil, err
	}
	if _, err := payoutartifact.VerifyWholeWorkInventoryWithWitness(ctx, artifact, inventory, expected); err != nil {
		return nil, err
	}
	return raw, nil
}
