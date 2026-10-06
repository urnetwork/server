// Hotkey wallet mapping (sn/docs/OPERATOR-DISCOVERY.md 6.5): the global
// consent the coldkey and the hotkey sign once for every operator of the
// subnet, and this operator's per-network delegation the hotkey signs to one
// head of it. Only the network owner's session (a network JWT, no client)
// submits a global chain, which links the network to the hotkey within the
// model's bounds, or requests or submits a delegation. History reads are
// public and take the caller's pinned head.
package controller

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The complete global chain from generation 1, as the hotkey owner holds it.
type SnHotkeyWalletMappingConsentArgs struct {
	Originals []protocol.HotkeyWalletMappingConsent `json:"originals"`
}

// The head of the submitted chain, which the operator now retains. A
// delegation pins exactly this head.
type SnHotkeyWalletMappingConsentResult struct {
	HotkeySs58 string   `json:"hotkey_ss58"`
	HeadHash   [32]byte `json:"head_hash"`
	Generation uint64   `json:"generation"`
}

// Retains the chain when it verifies and names the subnet of the configured
// deployment. A replay is idempotent; a different original at a retained
// generation is a fork and is refused. A client JWT is refused, like the
// network consent challenge. A ninth new hotkey of the network is a 403 and a
// submission of too many new generations a 400, each with its message. It
// writes no wallet projection and attaches no coldkey to the network, so the
// ban list applies at delegation.
func SnHotkeyWalletMappingConsent(args *SnHotkeyWalletMappingConsentArgs, clientSession *session.ClientSession) (*SnHotkeyWalletMappingConsentResult, error) {
	if args == nil || len(args.Originals) == 0 {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	owner, err := snNetworkWalletMappingOwner(clientSession)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(clientSession.Ctx, 300*time.Second)
	defer cancel()
	stored, err := model.StoreHotkeyWalletMappingChain(ctx, owner, args.Originals)
	if err != nil {
		return nil, snHotkeyWalletMappingRefusal(err)
	}
	hotkeySs58, err := ss58.Encode(stored.Hotkey, ss58.BittensorPrefix)
	if err != nil {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	return &SnHotkeyWalletMappingConsentResult{HotkeySs58: hotkeySs58, HeadHash: stored.HeadHash, Generation: stored.Generation}, nil
}

// The submission bounds as coded refusals the router turns into their status:
// a new hotkey past the network's links is a 403 and too many new generations
// a 400, each with its message. Every other error passes unchanged.
func snHotkeyWalletMappingRefusal(err error) error {
	switch {
	case errors.Is(err, model.ErrHotkeyWalletMappingNetworkHotkeys):
		return fmt.Errorf("%d %w", http.StatusForbidden, err)
	case errors.Is(err, model.ErrHotkeyWalletMappingNewGenerations):
		return fmt.Errorf("%d %w", http.StatusBadRequest, err)
	}
	return err
}

// Readers supply their independently pinned head, such as a delegation's
// consent head; the API never selects the newest row as authority. The subnet
// is the configured deployment's.
type SnHotkeyWalletMappingHistoryArgs struct {
	HotkeySs58 string   `json:"hotkey_ss58"`
	HeadHash   [32]byte `json:"head_hash"`
	Generation uint64   `json:"generation"`
}

// The complete retained chain through the pinned head, verified downstream.
type SnHotkeyWalletMappingHistoryResult struct {
	Originals []protocol.HotkeyWalletMappingConsent `json:"originals"`
}

// Signature-bearing originals are public evidence. Missing history stays
// unavailable rather than known-empty.
func SnHotkeyWalletMappingHistory(args *SnHotkeyWalletMappingHistoryArgs, clientSession *session.ClientSession) (*SnHotkeyWalletMappingHistoryResult, error) {
	if args == nil || clientSession == nil || clientSession.Ctx == nil {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	hotkey, err := ss58.DecodeWithPrefix(strings.TrimSpace(args.HotkeySs58), ss58.BittensorPrefix)
	if err != nil {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	domain, ok := stClientKeyHistoryDomain()
	if !ok {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	originals, err := model.ReadHotkeyWalletMappingHistory(clientSession.Ctx, domain.HotkeySubnet(), hotkey, args.Generation, args.HeadHash)
	if err != nil {
		return nil, err
	}
	return &SnHotkeyWalletMappingHistoryResult{Originals: originals}, nil
}

// The retained global consent head the network delegates to and the
// inclusive earning interval. The user and network come from the session,
// never from the request.
type SnHotkeyNetworkDelegationChallengeArgs struct {
	HotkeySs58        string   `json:"hotkey_ss58"`
	ConsentHeadHash   [32]byte `json:"consent_head_hash"`
	ConsentGeneration uint64   `json:"consent_generation"`
	FromEpoch         uint64   `json:"from_epoch"`
	ThroughEpoch      uint64   `json:"through_epoch"`
}

// Issues the prospective delegation the hotkey signs, exactly like the network
// consent challenge. The global chain through the head must be retained for
// that hotkey, and the session user must still administer the network.
func SnHotkeyNetworkDelegationChallenge(args *SnHotkeyNetworkDelegationChallengeArgs, clientSession *session.ClientSession) (*SnWalletMappingChallengeResult, error) {
	if args == nil {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	owner, err := snNetworkWalletMappingOwner(clientSession)
	if err != nil {
		return nil, err
	}
	hotkey, err := ss58.DecodeWithPrefix(strings.TrimSpace(args.HotkeySs58), ss58.BittensorPrefix)
	if err != nil {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	ctx, cancel := context.WithTimeout(clientSession.Ctx, 300*time.Second)
	defer cancel()
	// a banned coldkey cannot be attached to the network through any
	// generation the delegation could select at settlement
	consents, err := model.ReadHotkeyWalletMappingHistory(ctx, owner.Domain.HotkeySubnet(), hotkey, args.ConsentGeneration, args.ConsentHeadHash)
	if err != nil {
		return nil, err
	}
	for _, consent := range consents {
		statement, err := protocol.DecodeHotkeyWalletMappingStatement(consent.Message)
		if err != nil {
			return nil, err
		}
		if SnWalletBanned(statement.Coldkey) {
			return nil, protocol.ErrWalletMappingIntegrity
		}
	}
	owner.Prospective, err = snWalletMappingProspective(ctx, owner.Domain)
	if err != nil {
		return nil, err
	}
	message, err := model.CreateHotkeyNetworkDelegationChallenge(ctx, owner, hotkey, args.ConsentHeadHash, args.ConsentGeneration, args.FromEpoch, args.ThroughEpoch)
	if err != nil {
		return nil, err
	}
	return &SnWalletMappingChallengeResult{Message: message}, nil
}

// A delegation submitted to POST /sn/wallet, with coldkey_ss58 naming the
// signing hotkey. It names no client, so a request that selects one is refused
// rather than reinterpreted.
func snAcceptHotkeyNetworkDelegation(args *SnSetWalletArgs, clientSession *session.ClientSession) (*SnSetWalletResult, error) {
	if args.ClientId != nil {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	owner, err := snNetworkWalletMappingOwner(clientSession)
	if err != nil {
		return nil, err
	}
	hotkeySs58 := strings.TrimSpace(args.ColdkeySs58)
	hotkey, err := ss58.DecodeWithPrefix(hotkeySs58, ss58.BittensorPrefix)
	if err != nil {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	signature, err := hex.DecodeString(strings.TrimPrefix(args.Signature, "0x"))
	if err != nil || len(signature) != 64 {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	// a delegation that names the entered hotkey, with a well-formed signature
	// that verifies for it under neither substrate transcript: in practice
	// another key signed it
	signatureMismatch := func() bool {
		statement, err := protocol.DecodeHotkeyNetworkDelegationStatement(args.Message)
		if err != nil || statement.Hotkey != hotkey {
			return false
		}
		valid, err := model.VerifyBittensorSignature(hotkeySs58, args.Message, args.Signature)
		return err == nil && !valid
	}
	if signatureMismatch() {
		return &SnSetWalletResult{Error: &SnSetWalletError{
			Code:    SnSetWalletErrorCodeSignatureMismatch,
			Message: snSetWalletSignatureMismatchMessage,
		}}, nil
	}
	original := protocol.WalletMappingConsent{Message: args.Message, Signature: [64]byte(signature)}
	ctx, cancel := context.WithTimeout(clientSession.Ctx, 300*time.Second)
	defer cancel()
	owner.Prospective, err = snWalletMappingProspective(ctx, owner.Domain)
	if err != nil {
		return nil, err
	}
	accepted, err := model.AcceptHotkeyNetworkDelegation(ctx, owner, original, hotkeySs58)
	if err != nil {
		return nil, err
	}
	return &SnSetWalletResult{MappingHash: hex.EncodeToString(accepted.OriginalHash[:]), MappingGeneration: accepted.Generation}, nil
}

// Readers supply their independently pinned head; the API never selects the
// newest row as authority. The domain is a namespace filter, not a grant.
type SnHotkeyNetworkDelegationHistoryArgs struct {
	Domain     protocol.ClientKeyHistoryDomain `json:"domain"`
	NetworkId  server.Id                       `json:"network_id"`
	HeadHash   [32]byte                        `json:"head_hash"`
	Generation uint64                          `json:"generation"`
}

// Signature-bearing originals are public evidence. Missing history stays
// unavailable rather than known-empty.
func SnHotkeyNetworkDelegationHistory(args *SnHotkeyNetworkDelegationHistoryArgs, clientSession *session.ClientSession) (*SnWalletMappingHistoryResult, error) {
	if args == nil || clientSession == nil || clientSession.Ctx == nil {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	originals, err := model.ReadHotkeyNetworkDelegationHistory(clientSession.Ctx, args.Domain, args.NetworkId, args.Generation, args.HeadHash)
	if err != nil {
		return nil, err
	}
	return &SnWalletMappingHistoryResult{Originals: originals}, nil
}

// The network's hotkey entry for GET /sn/wallet: the newest accepted
// delegation, with the coldkey of the global consent effective at the
// mirrored current epoch, else of the consent head it pins. Nil without a
// delegation.
func snHotkeyWallet(ctx context.Context, domain protocol.ClientKeyHistoryDomain, networkId server.Id) (*SnWallet, error) {
	var epoch uint64
	var epochKnown bool
	if cfg := stConfig(); cfg != nil && cfg.DeploymentKey() != "" {
		if summary := model.GetStEpochSummaryCache(ctx, cfg.DeploymentKey()); summary != nil {
			epoch, epochKnown = summary.Epoch, true
		} else if stEpoch := model.GetLatestStEpoch(ctx, cfg.DeploymentKey()); stEpoch != nil {
			epoch, epochKnown = stEpoch.Epoch, true
		}
	}
	delegation, err := model.GetHotkeyNetworkDelegation(ctx, domain, networkId, epoch, epochKnown)
	if err != nil || delegation == nil {
		return nil, err
	}
	return &SnWallet{
		ColdkeySs58:       delegation.ColdkeySs58,
		SetAtMillis:       delegation.AcceptedAt.UnixMilli(),
		ConsentScope:      SnWalletConsentScopeHotkey,
		FromEpoch:         delegation.FromEpoch,
		ThroughEpoch:      delegation.ThroughEpoch,
		HotkeySs58:        delegation.HotkeySs58,
		ConsentHeadHash:   "0x" + hex.EncodeToString(delegation.ConsentHeadHash[:]),
		ConsentGeneration: delegation.ConsentGeneration,
		MappingHash:       "0x" + hex.EncodeToString(delegation.OriginalHash[:]),
		MappingGeneration: delegation.Generation,
	}, nil
}
