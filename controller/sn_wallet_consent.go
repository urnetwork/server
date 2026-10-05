// Mapping consent is issued for the authenticated provider and deployment.
// Login challenges remain supported but never enter this signed mapping history.
package controller

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Epochs are an explicit wallet-approved earning interval, independent of the
// five-minute acceptance expiry; the consumer still needs an approved head.
type SnWalletMappingChallengeArgs struct {
	ClientId     *server.Id `json:"client_id,omitempty"`
	ColdkeySs58  string     `json:"coldkey_ss58"`
	FromEpoch    uint64     `json:"from_epoch"`
	ThroughEpoch uint64     `json:"through_epoch"`
}

// The exact displayed bytes must be passed unchanged to the coldkey signer.
type SnWalletMappingChallengeResult struct {
	Message string `json:"message"`
}

// The authenticated session selects user/network; an optional client must
// belong to that network at both issuance and acceptance in the actual database.
func snWalletMappingOwner(clientId *server.Id, clientSession *session.ClientSession) (model.WalletMappingOwner, error) {
	if clientSession == nil || clientSession.Ctx == nil || clientSession.ByJwt == nil {
		return model.WalletMappingOwner{}, protocol.ErrWalletMappingUnavailable
	}
	var err error
	clientId, err = snWalletClientOwner(clientId, clientSession)
	if err != nil {
		return model.WalletMappingOwner{}, err
	}
	if clientId == nil || *clientId == (server.Id{}) || clientSession.ByJwt.UserId == (server.Id{}) || clientSession.ByJwt.NetworkId == (server.Id{}) {
		return model.WalletMappingOwner{}, protocol.ErrWalletMappingIntegrity
	}
	domain, ok := stClientKeyHistoryDomain()
	if !ok {
		return model.WalletMappingOwner{}, protocol.ErrWalletMappingUnavailable
	}
	return model.WalletMappingOwner{Domain: domain, UserId: clientSession.ByJwt.UserId, ClientId: *clientId, NetworkId: clientSession.ByJwt.NetworkId}, nil
}

// Resolve one actual finalized coordinator boundary under the original
// deployment owner. Public epoch fields cannot select a historical read.
func snWalletMappingProspectiveOwner(ctx context.Context, caller model.WalletMappingOwner) (model.WalletMappingOwner, error) {
	owner, err := newStClientKeyAuthorityOwner()
	if err != nil {
		return model.WalletMappingOwner{}, err
	}
	if owner.domain != caller.Domain {
		return model.WalletMappingOwner{}, protocol.ErrWalletMappingIntegrity
	}
	boundary, operator, err := owner.readBoundary(ctx, nil)
	if err != nil {
		return model.WalletMappingOwner{}, err
	}
	if !operator.Active || operator.RootSigner != crypto.PubkeyToAddress(owner.rootKey.PublicKey) {
		return model.WalletMappingOwner{}, protocol.ErrWalletMappingIntegrity
	}
	caller.Prospective = &model.WalletMappingProspectiveOwner{Boundary: boundary, RootKey: owner.rootKey}
	return caller, ctx.Err()
}

// This public authenticated request cannot override the configured deployment,
// JWT identities, original predecessor or bounded challenge expiry.
func SnWalletMappingChallenge(args *SnWalletMappingChallengeArgs, clientSession *session.ClientSession) (*SnWalletMappingChallengeResult, error) {
	if args == nil {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	owner, err := snWalletMappingOwner(args.ClientId, clientSession)
	if err != nil {
		return nil, err
	}
	coldkey, err := ss58.DecodeWithPrefix(strings.TrimSpace(args.ColdkeySs58), ss58.BittensorPrefix)
	if err != nil || SnWalletBanned(coldkey) {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	ctx, cancel := context.WithTimeout(clientSession.Ctx, 300*time.Second)
	defer cancel()
	owner, err = snWalletMappingProspectiveOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	message, err := model.CreateWalletMappingChallenge(ctx, owner, coldkey, args.FromEpoch, args.ThroughEpoch)
	if err != nil {
		return nil, err
	}
	return &SnWalletMappingChallengeResult{Message: message}, nil
}

// A domain-specific mapping cannot fall back to generic wallet login or the
// unsigned compatibility gate when its identity, nonce or signature is wrong.
func snAcceptWalletMapping(args *SnSetWalletArgs, clientSession *session.ClientSession) (*SnSetWalletResult, error) {
	owner, err := snWalletMappingOwner(args.ClientId, clientSession)
	if err != nil {
		return nil, err
	}
	coldkey, err := ss58.DecodeWithPrefix(strings.TrimSpace(args.ColdkeySs58), ss58.BittensorPrefix)
	if err != nil || SnWalletBanned(coldkey) {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	signature, err := hex.DecodeString(strings.TrimPrefix(args.Signature, "0x"))
	if err != nil || len(signature) != 64 {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	if snWalletMappingSignatureMismatch(args.Message, coldkey, strings.TrimSpace(args.ColdkeySs58), args.Signature) {
		// coded like a login challenge signed by another account, before any
		// mapping state is read
		return &SnSetWalletResult{Error: &SnSetWalletError{
			Code:    SnSetWalletErrorCodeSignatureMismatch,
			Message: snSetWalletSignatureMismatchMessage,
		}}, nil
	}
	original := protocol.WalletMappingConsent{Message: args.Message, Signature: [64]byte(signature)}
	ctx, cancel := context.WithTimeout(clientSession.Ctx, 300*time.Second)
	defer cancel()
	owner, err = snWalletMappingProspectiveOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	accepted, err := model.AcceptWalletMappingConsent(ctx, owner, original, strings.TrimSpace(args.ColdkeySs58))
	if err != nil {
		return nil, err
	}
	return &SnSetWalletResult{MappingHash: hex.EncodeToString(accepted.OriginalHash[:]), MappingGeneration: accepted.Generation}, nil
}

// A consent that names the entered coldkey, with a well-formed signature that
// verifies for it under neither substrate transcript: in practice another
// account signed it. The same verification AcceptWalletMappingConsent starts
// with; a consent for another coldkey, an undecodable message or signature,
// and a signature that verifies are not a mismatch and keep its refusals.
func snWalletMappingSignatureMismatch(message string, coldkey [32]byte, coldkeySs58 string, signature string) bool {
	statement, err := protocol.DecodeWalletMappingStatement(message)
	if err != nil || statement.Coldkey != coldkey {
		return false
	}
	valid, err := model.VerifyBittensorSignature(coldkeySs58, message, signature)
	return err == nil && !valid
}

// Readers must supply their independent head; the API never selects latest as
// authority. The original domain is a namespace filter, not an approval grant.
type SnWalletMappingHistoryArgs struct {
	Domain     protocol.ClientKeyHistoryDomain `json:"domain"`
	ClientId   server.Id                       `json:"client_id"`
	HeadHash   [32]byte                        `json:"head_hash"`
	Generation uint64                          `json:"generation"`
}

// The complete retained original chain is independently verified downstream.
type SnWalletMappingHistoryResult struct {
	Originals []protocol.WalletMappingConsent `json:"originals"`
}

// Signature-bearing originals are public evidence, with bounded database and
// response work. Missing history stays unavailable rather than known-empty.
func SnWalletMappingHistory(args *SnWalletMappingHistoryArgs, clientSession *session.ClientSession) (*SnWalletMappingHistoryResult, error) {
	if args == nil || clientSession == nil || clientSession.Ctx == nil {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	originals, err := model.ReadWalletMappingHistory(clientSession.Ctx, args.Domain, args.ClientId, args.Generation, args.HeadHash)
	if err != nil {
		return nil, err
	}
	return &SnWalletMappingHistoryResult{Originals: originals}, nil
}

// A scoped provider can name only itself. A network owner may still select a
// network-owned provider; the existing database ownership/proof checks apply.
func snWalletClientOwner(clientId *server.Id, clientSession *session.ClientSession) (*server.Id, error) {
	if clientSession == nil || clientSession.Ctx == nil || clientSession.ByJwt == nil {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	if authenticated := clientSession.ByJwt.ClientId; authenticated != nil {
		if *authenticated == (server.Id{}) || (clientId != nil && *clientId != *authenticated) {
			return nil, protocol.ErrWalletMappingIntegrity
		}
		return authenticated, nil
	}
	return clientId, nil
}
