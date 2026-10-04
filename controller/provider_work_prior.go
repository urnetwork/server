// A newly approved exclusion is only a lookup hint. Previous contract
// checkpoints are derived again from retained signed artifacts and their full
// original inventory dependencies under the independently configured roots.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

const maximumProviderWorkPriorWindows = 64
const maximumProviderWorkPriorBytes = 64 * 1024 * 1024

// Only original bytes cross the reader boundary. No reader supplies a verified
// result or replaces a missing predecessor with the current proposal's fields.
type providerWorkPriorOriginal struct {
	Artifact  []byte
	Inventory []byte
}

// Per-call storage is bounded and never reused as authority for another call.
type providerWorkPriorOwner struct {
	domain   protocol.ClientKeyHistoryDomain
	expected payoutartifact.WholeWorkExpectation
	read     func(context.Context, uint64) (providerWorkPriorOriginal, error)
	locate   func(context.Context, uint64, []server.Id) ([]uint64, error)
	bytes    int
	windows  int
	verified map[uint64]*providerWorkPriorVerified
}

// Each indexed checkpoint comes from a full original verification in this call.
type providerWorkPriorVerified struct {
	inventoryHash string
	contracts     map[[16]byte]payoutartifact.WholeWorkPriorContract
	creations     map[[16]byte]payoutartifact.WholeWorkRetainedCreation
}

// Current lookup inputs carry the independently selected artifact domain and
// original witness. Neither value can supply a previously verified result.
type providerWorkPriorCurrent struct {
	artifact  *payoutartifact.Artifact
	inventory *payoutartifact.WholeWorkInventory
}

type providerWorkPriorClaims struct {
	contracts []payoutartifact.WholeWorkPriorContract
	creations []payoutartifact.WholeWorkRetainedCreation
}

// SQL identifies exact published artifacts; the verifier still proves all
// signed bytes, original identities and predecessor dependencies independently.
func providerWorkPriorExpectation(ctx context.Context, authority payoutartifact.WholeWorkAuthority, expected payoutartifact.WholeWorkExpectation, originals ...providerWorkPriorCurrent) (payoutartifact.WholeWorkExpectation, error) {
	var current providerWorkPriorCurrent
	if len(originals) > 1 {
		expected.PriorContracts, expected.PriorCreations = nil, nil
		return expected, model.ErrProviderWorkInvalid
	}
	if len(originals) == 1 {
		current = originals[0]
	}
	return providerWorkPriorExpectationWithInputs(ctx, authority, expected, current, func(ctx context.Context, epoch uint64) (providerWorkPriorOriginal, error) {
		return readProviderWorkPriorOriginal(ctx, authority.Domain, epoch, expected)
	}, func(ctx context.Context, before uint64, candidates []server.Id) ([]uint64, error) {
		domain, err := authority.Domain.Digest()
		if err != nil {
			return nil, err
		}
		deployment := model.StDeploymentKey(fmt.Sprintf("%d:%s", authority.Domain.ChainID, strings.ToLower(authority.Domain.Coordinator.Hex())))
		return model.ListProviderWorkPriorEpochs(ctx, domain, deployment, authority.Domain.NoID, before, candidates, maximumProviderWorkPriorWindows)
	})
}

// Reader substitution permits deterministic source-I/O boundaries in tests;
// the actual cryptographic verifier and finite graph owner are never replaced.
func providerWorkPriorExpectationWithReader(ctx context.Context, authority payoutartifact.WholeWorkAuthority, expected payoutartifact.WholeWorkExpectation, read func(context.Context, uint64) (providerWorkPriorOriginal, error)) (payoutartifact.WholeWorkExpectation, error) {
	return providerWorkPriorExpectationWithInputs(ctx, authority, expected, providerWorkPriorCurrent{}, read, nil)
}

// The index locates earlier original bytes for every candidate. It never
// supplies a checkpoint or asserts that missing historical custody is complete.
func providerWorkPriorExpectationWithInputs(ctx context.Context, authority payoutartifact.WholeWorkAuthority, expected payoutartifact.WholeWorkExpectation, current providerWorkPriorCurrent, read func(context.Context, uint64) (providerWorkPriorOriginal, error), locate func(context.Context, uint64, []server.Id) ([]uint64, error)) (result payoutartifact.WholeWorkExpectation, resultErr error) {
	result = expected
	result.PriorContracts, result.PriorCreations = nil, nil
	if ctx == nil || read == nil {
		return result, model.ErrProviderWorkInvalid
	}
	defer func() {
		resultErr = errors.Join(resultErr, ctx.Err(), context.Cause(ctx))
		if resultErr != nil {
			result.PriorContracts, result.PriorCreations = nil, nil
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	raw, err := authority.Bytes(ctx)
	if err != nil {
		return result, err
	}
	if _, err := payoutartifact.DecodeWholeWorkAuthority(ctx, raw, expected.AuthoritySigner); err != nil {
		return result, err
	}
	if expected.AuthorityHash != "" && expected.AuthorityHash != fmt.Sprintf("sha256:%x", sha256.Sum256(raw)) {
		return result, payoutartifact.ErrClosedWorkIntegrity
	}
	if current.inventory != nil && !bytes.Equal(raw, current.inventory.Authority) {
		return result, payoutartifact.ErrClosedWorkIntegrity
	}
	owner := &providerWorkPriorOwner{domain: authority.Domain, expected: result, read: read, locate: locate, verified: make(map[uint64]*providerWorkPriorVerified)}
	var claims providerWorkPriorClaims
	claims, resultErr = owner.claims(ctx, authority, current, 0)
	result.PriorContracts, result.PriorCreations = claims.contracts, claims.creations
	return
}

// Strictly decreasing signed epochs prohibit cycles. A lookup hint must match
// every field of an actual reconstructed checkpoint, including its inventory.
func (self *providerWorkPriorOwner) claims(ctx context.Context, authority payoutartifact.WholeWorkAuthority, current providerWorkPriorCurrent, depth int) (result providerWorkPriorClaims, resultErr error) {
	if authority.Domain != self.domain || len(authority.PriorContracts) > payoutartifact.MaxClosedWorkRecords {
		return result, payoutartifact.ErrClosedWorkIntegrity
	}
	creationIds := map[[16]byte]bool{}
	if current.inventory != nil && len(authority.WorkSources) != 0 {
		expected := self.expected
		expected.AuthorityHash = fmt.Sprintf("sha256:%x", sha256.Sum256(current.inventory.Authority))
		ids, err := payoutartifact.ReadWholeWorkPriorCreationRequests(ctx, current.artifact, current.inventory, expected)
		if err != nil {
			return result, err
		}
		for _, id := range ids {
			creationIds[id] = true
		}
	}
	contractKVs := make(map[[16]byte]payoutartifact.WholeWorkPriorContract)
	creationKVs := make(map[[16]byte]payoutartifact.WholeWorkRetainedCreation)
	retain := func(prior *providerWorkPriorVerified, actual payoutartifact.WholeWorkPriorContract) error {
		if retained, exists := contractKVs[actual.ContractId]; exists && retained != actual {
			return payoutartifact.ErrClosedWorkIntegrity
		}
		contractKVs[actual.ContractId] = actual
		if creationIds[actual.ContractId] {
			if creation, exists := prior.creations[actual.ContractId]; exists {
				creationKVs[actual.ContractId] = creation
			}
		}
		return nil
	}
	for _, proposed := range authority.PriorContracts {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if proposed.ReconciledEpoch >= authority.Epoch {
			return result, payoutartifact.ErrClosedWorkIntegrity
		}
		prior, err := self.window(ctx, proposed.ReconciledEpoch, depth+1)
		if err != nil {
			return result, err
		}
		actual, present := prior.contracts[proposed.ContractId]
		if !present || prior.inventoryHash != proposed.InventoryHash || actual != proposed {
			return result, payoutartifact.ErrClosedWorkIntegrity
		}
		if err := retain(prior, actual); err != nil {
			return result, err
		}
	}
	if current.inventory != nil && self.locate != nil {
		ids, err := providerWorkCandidateIds(ctx, current.inventory)
		if err != nil {
			return result, err
		}
		for id := range creationIds {
			ids = append(ids, server.Id(id))
		}
		slices.SortFunc(ids, func(a, b server.Id) int { return a.Cmp(b) })
		ids = slices.Compact(ids)
		if len(ids) > payoutartifact.MaxClosedWorkRecords {
			return result, payoutartifact.ErrClosedWorkCapacity
		}
		epochs, err := self.locate(ctx, authority.Epoch, ids)
		if err != nil {
			return result, err
		}
		if len(epochs) > maximumProviderWorkPriorWindows {
			return result, payoutartifact.ErrClosedWorkCapacity
		}
		for _, epoch := range epochs {
			if epoch >= authority.Epoch {
				return result, payoutartifact.ErrClosedWorkIntegrity
			}
			prior, err := self.window(ctx, epoch, depth+1)
			if err != nil {
				return result, err
			}
			for _, id := range ids {
				actual, exists := prior.contracts[[16]byte(id)]
				if !exists {
					continue
				}
				if err := retain(prior, actual); err != nil {
					return result, err
				}
			}
		}
	}
	if len(contractKVs) > payoutartifact.MaxClosedWorkRecords {
		return result, payoutartifact.ErrClosedWorkCapacity
	}
	result.contracts = make([]payoutartifact.WholeWorkPriorContract, 0, len(contractKVs))
	for _, contract := range contractKVs {
		result.contracts = append(result.contracts, contract)
	}
	slices.SortFunc(result.contracts, func(a, b payoutartifact.WholeWorkPriorContract) int {
		return server.Id(a.ContractId).Cmp(server.Id(b.ContractId))
	})
	used := 0
	for _, checkpoint := range result.contracts {
		creation, exists := creationKVs[checkpoint.ContractId]
		if !exists {
			continue
		}
		original := &creation.Original
		size := len(original.StoredContract) + len(original.LatestInventory) + len(original.OriginalCreation)
		if creation.Checkpoint != checkpoint {
			return providerWorkPriorClaims{}, payoutartifact.ErrClosedWorkIntegrity
		}
		if size > payoutartifact.MaxWholeWorkInventoryBytes-used {
			return providerWorkPriorClaims{}, payoutartifact.ErrClosedWorkCapacity
		}
		used += size
		original.StoredContract = bytes.Clone(original.StoredContract)
		original.LatestInventory = bytes.Clone(original.LatestInventory)
		original.OriginalCreation = bytes.Clone(original.OriginalCreation)
		result.creations = append(result.creations, creation)
	}
	return result, nil
}

// A capacity refusal retains unknown state. It cannot bless a partially
// traversed history or accept a supplied checkpoint as a shortcut.
func (self *providerWorkPriorOwner) window(ctx context.Context, epoch uint64, depth int) (*providerWorkPriorVerified, error) {
	if prior := self.verified[epoch]; prior != nil {
		return prior, nil
	}
	if depth > maximumProviderWorkPriorWindows || self.windows >= maximumProviderWorkPriorWindows {
		return nil, payoutartifact.ErrClosedWorkCapacity
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	self.windows++
	original, err := self.read(ctx, epoch)
	if err != nil {
		return nil, err
	}
	if len(original.Artifact) > maximumProviderWorkPriorBytes-self.bytes || len(original.Inventory) > maximumProviderWorkPriorBytes-self.bytes-len(original.Artifact) {
		return nil, payoutartifact.ErrClosedWorkCapacity
	}
	self.bytes += len(original.Artifact) + len(original.Inventory)
	artifact, err := startifact.DecodeWithContext(ctx, original.Artifact)
	if err != nil {
		return nil, err
	}
	domain, err := payoutartifact.ClosedWorkReportDomain(artifact)
	if err != nil || domain != self.domain || artifact.Epoch != epoch {
		return nil, errors.Join(payoutartifact.ErrClosedWorkIntegrity, err)
	}
	var inventory *payoutartifact.WholeWorkInventory
	if len(original.Inventory) != 0 {
		inventory, err = payoutartifact.DecodeWholeWorkInventory(ctx, original.Inventory)
		if err != nil {
			return nil, err
		}
	} else if artifact.ClosedWork != nil {
		inventory = artifact.ClosedWork.WholeInventory
	}
	if inventory == nil {
		return nil, payoutartifact.ErrClosedWorkUnavailable
	}
	authority, err := payoutartifact.DecodeWholeWorkAuthority(ctx, inventory.Authority, self.expected.AuthoritySigner)
	if err != nil {
		return nil, err
	}
	if authority.Domain != self.domain || authority.Epoch != epoch || authority.Start != artifact.Start || authority.End != artifact.End {
		return nil, payoutartifact.ErrClosedWorkIntegrity
	}
	expected := self.expected
	expected.AuthorityHash = fmt.Sprintf("sha256:%x", sha256.Sum256(inventory.Authority))
	claims, err := self.claims(ctx, authority, providerWorkPriorCurrent{artifact: artifact, inventory: inventory}, depth)
	if err != nil {
		return nil, err
	}
	expected.PriorContracts, expected.PriorCreations = claims.contracts, claims.creations
	verified, err := payoutartifact.VerifyWholeWorkInventoryWithWitness(ctx, artifact, inventory, expected)
	if err != nil {
		return nil, err
	}
	if !verified.Complete || verified.Domain != self.domain || verified.Epoch != epoch {
		return nil, payoutartifact.ErrClosedWorkUnavailable
	}
	if err := requireProviderWorkPublication(ctx, inventory, expected, verified); err != nil {
		return nil, err
	}
	result := &providerWorkPriorVerified{inventoryHash: verified.InventoryHash, contracts: make(map[[16]byte]payoutartifact.WholeWorkPriorContract, len(verified.ReconciledContracts)), creations: make(map[[16]byte]payoutartifact.WholeWorkRetainedCreation, len(verified.RetainedCreations))}
	for _, contract := range verified.ReconciledContracts {
		if _, exists := result.contracts[contract.ContractId]; exists {
			return nil, payoutartifact.ErrClosedWorkIntegrity
		}
		result.contracts[contract.ContractId] = contract
	}
	for _, creation := range verified.RetainedCreations {
		id := creation.Original.ContractId
		if _, exists := result.creations[id]; exists || creation.Checkpoint != result.contracts[id] || creation.Checkpoint.ReconciledEpoch != epoch {
			return nil, payoutartifact.ErrClosedWorkIntegrity
		}
		result.creations[id] = creation
	}
	self.verified[epoch] = result
	return result, nil
}

// The artifact row is only an address into immutable blob custody. An optional
// companion is read by that exact artifact hash and the original root authority.
func readProviderWorkPriorOriginal(ctx context.Context, domain protocol.ClientKeyHistoryDomain, epoch uint64, expected payoutartifact.WholeWorkExpectation) (result providerWorkPriorOriginal, resultErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				resultErr = errors.Join(resultErr, err)
			} else {
				panic(recovered)
			}
		}
		resultErr = errors.Join(resultErr, ctx.Err(), context.Cause(ctx))
		if resultErr != nil {
			result = providerWorkPriorOriginal{}
		}
	}()
	deployment := model.StDeploymentKey(fmt.Sprintf("%d:%s", domain.ChainID, strings.ToLower(domain.Coordinator.Hex())))
	record := model.GetStPayoutArtifact(ctx, deployment, epoch, domain.NoID)
	if record == nil {
		return result, payoutartifact.ErrClosedWorkUnavailable
	}
	store, ok := server.LoadBlobStore()
	if !ok {
		return result, errors.New("provider work predecessor artifact store unavailable")
	}
	artifact, raw, err := startifact.Read(ctx, store, record.ContentHash)
	if err != nil {
		return result, err
	}
	if artifact.Epoch != epoch || artifact.NoID != domain.NoID || artifact.PayoutRoot != record.PayoutRoot {
		return result, payoutartifact.ErrClosedWorkIntegrity
	}
	result.Artifact = raw
	if artifact.ClosedWork != nil && artifact.ClosedWork.WholeInventory != nil {
		return result, nil
	}
	domainHash, err := domain.Digest()
	if err != nil {
		return result, err
	}
	authority, _, err := model.GetProviderWorkAuthority(ctx, domainHash, epoch, expected.AuthoritySigner)
	if errors.Is(err, model.ErrProviderWorkMissing) {
		return result, payoutartifact.ErrClosedWorkUnavailable
	}
	if err != nil {
		return result, err
	}
	artifactHash, err := providerWorkPolicyHash(strings.TrimPrefix(record.ContentHash, "sha256:"))
	if err != nil {
		return result, err
	}
	result.Inventory, err = model.GetProviderWorkWindow(ctx, artifactHash, sha256.Sum256(authority))
	if errors.Is(err, model.ErrProviderWorkMissing) {
		return result, payoutartifact.ErrClosedWorkUnavailable
	}
	return result, err
}
