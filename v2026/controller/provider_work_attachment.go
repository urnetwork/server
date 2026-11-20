// Publication transports retained live receipts. Its signer never reconstructs
// a missing session, reservation, stream cohort or outcome from current SQL.
package controller

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/v2026/payoutartifact"
	snprotocol "github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Include all retained SDK candidates and the separate whole window. An
// authority's proposed prior exclusions cannot decide which IDs are checked.
func providerWorkCandidateIds(ctx context.Context, inventory *payoutartifact.WholeWorkInventory) ([]server.Id, error) {
	if inventory == nil {
		return nil, payoutartifact.ErrClosedWorkUnavailable
	}
	seen := map[server.Id]bool{}
	add := func(id server.Id) error {
		if id == (server.Id{}) {
			return payoutartifact.ErrClosedWorkIntegrity
		}
		seen[id] = true
		if len(seen) > payoutartifact.MaxClosedWorkRecords {
			return payoutartifact.ErrClosedWorkCapacity
		}
		return nil
	}
	if inventory.Window != nil {
		for _, row := range inventory.Window.Records {
			id, err := server.ParseId(row.ContractId)
			if err != nil || id.String() != row.ContractId {
				return nil, errors.Join(payoutartifact.ErrClosedWorkIntegrity, err)
			}
			if err := add(id); err != nil {
				return nil, err
			}
		}
	}
	for _, owner := range inventory.Owners {
		for _, raw := range [][]byte{owner.Start, owner.End} {
			cut, err := protocol.DecodeOriginalWorkCut(ctx, raw)
			if err != nil {
				return nil, err
			}
			for _, record := range cut.Contracts {
				if err := add(server.Id(record.ContractId)); err != nil {
					return nil, err
				}
			}
		}
	}
	ids := make([]server.Id, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b server.Id) int { return a.Cmp(b) })
	return ids, ctx.Err()
}

// Source selection comes only from the original independently signed roster.
// Missing selected originals remain pending; a present foreign key is refused.
func attachProviderWorkOriginals(ctx context.Context, authority payoutartifact.WholeWorkAuthority, expected payoutartifact.WholeWorkExpectation, inventory *payoutartifact.WholeWorkInventory) (resultErr error) {
	if ctx == nil {
		return payoutartifact.ErrClosedWorkUnavailable
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				resultErr = errors.Join(resultErr, err)
			} else {
				panic(recovered)
			}
		}
		resultErr = errors.Join(resultErr, ctx.Err(), context.Cause(ctx))
	}()
	if len(authority.WorkSources) == 0 {
		return nil
	}
	if expected.AttributionSigner == (common.Address{}) || expected.AttributionSigner != authority.Signer {
		return payoutartifact.ErrClosedWorkUnavailable
	}
	ids, err := providerWorkCandidateIds(ctx, inventory)
	if err != nil {
		return err
	}
	// Observation may sign only after the separately approved boundary and
	// its exact original header clock are authenticated. The live model owns
	// the signer and settlement row fence; this publisher supplies no key.
	if err := payoutartifact.VerifyWholeWorkWindowClock(ctx, authority, inventory.Clock); err != nil {
		return err
	}
	openOriginals, err := model.RetainProviderWorkOpenObservations(ctx, ids, authority.Epoch, authority.End.Number, [32]byte(common.HexToHash(authority.End.Hash)), inventory.Clock.EndTime)
	if err != nil {
		return providerWorkOriginalFailure(err)
	}
	originals, err := model.ListProviderWorkOriginals(ctx, ids)
	if err != nil {
		return err
	}
	originals, err = mergeProviderWorkOriginalPools(ctx, originals, openOriginals)
	if err != nil {
		return err
	}
	domain, err := authority.Domain.Digest()
	if err != nil {
		return err
	}
	type identity struct{ source, generation string }
	approved := make(map[identity]snprotocol.ProviderWorkSourceAuthority, len(authority.WorkSources))
	for _, source := range authority.WorkSources {
		approved[identity{source: source.SourceId, generation: source.Generation}] = source
	}
	owned := make([][]byte, 0, len(originals))
	for _, raw := range originals {
		original, err := snprotocol.DecodeProviderWorkReceipt(ctx, raw)
		if err != nil {
			return providerWorkOriginalFailure(err)
		}
		if original.DomainHash != domain {
			return payoutartifact.ErrClosedWorkIntegrity
		}
		source, ok := approved[identity{source: original.SourceId, generation: original.Generation}]
		if !ok {
			return payoutartifact.ErrClosedWorkUnavailable
		}
		if err := snprotocol.VerifyProviderWorkReceiptAuthority(ctx, original, source); err != nil {
			return providerWorkOriginalFailure(err)
		}
		owned = append(owned, bytes.Clone(raw))
	}
	inventory.AttributionOriginals = owned
	return ctx.Err()
}

// Preserve actual cancellation and I/O causes; only authenticated source
// contradictions and explicit capacity/availability become component refusals.
func providerWorkOriginalFailure(err error) error {
	switch {
	case errors.Is(err, snprotocol.ErrProviderWorkIntegrity):
		return errors.Join(payoutartifact.ErrClosedWorkIntegrity, err)
	case errors.Is(err, snprotocol.ErrProviderWorkCapacity):
		return errors.Join(payoutartifact.ErrClosedWorkCapacity, err)
	case errors.Is(err, snprotocol.ErrProviderWorkUnavailable):
		return errors.Join(payoutartifact.ErrClosedWorkUnavailable, err)
	default:
		return err
	}
}

// An opted-in publication waits for complete participant replay before it can
// freeze immutable bytes. Legacy absence never silently enables this authority.
func requireProviderWorkPublication(ctx context.Context, inventory *payoutartifact.WholeWorkInventory, expected payoutartifact.WholeWorkExpectation, verified *payoutartifact.VerifiedWholeWorkInventory) error {
	if inventory == nil || verified == nil || !verified.Complete {
		return payoutartifact.ErrClosedWorkUnavailable
	}
	authority, err := payoutartifact.DecodeWholeWorkAuthority(ctx, inventory.Authority, expected.AuthoritySigner)
	if err != nil {
		return err
	}
	if len(authority.WorkSources) != 0 && (expected.AttributionSigner != authority.Signer || !verified.AttributionComplete) {
		return payoutartifact.ErrClosedWorkUnavailable
	}
	return ctx.Err()
}
