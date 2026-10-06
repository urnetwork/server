// Network wallet mapping consent: the coldkey owner signs once for the network,
// and every provider client of the network earns to that coldkey unless the
// client has its own provider consent effective at the epoch. Only the network
// owner's session (a network JWT, no client) can request or submit it.
package controller

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urfoundation/sn/v2026/ss58"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The inclusive earning interval the wallet approves for the network. The
// user and network come from the session, never from the request.
type SnNetworkWalletMappingChallengeArgs struct {
	ColdkeySs58  string `json:"coldkey_ss58"`
	FromEpoch    uint64 `json:"from_epoch"`
	ThroughEpoch uint64 `json:"through_epoch"`
}

// The network owner and the configured deployment. A client JWT is refused,
// even of the same network: one client cannot sign for all of them.
func snNetworkWalletMappingOwner(clientSession *session.ClientSession) (model.NetworkWalletMappingOwner, error) {
	if clientSession == nil || clientSession.Ctx == nil || clientSession.ByJwt == nil {
		return model.NetworkWalletMappingOwner{}, protocol.ErrWalletMappingUnavailable
	}
	if clientSession.ByJwt.ClientId != nil || clientSession.ByJwt.UserId == (server.Id{}) || clientSession.ByJwt.NetworkId == (server.Id{}) {
		return model.NetworkWalletMappingOwner{}, protocol.ErrWalletMappingIntegrity
	}
	domain, ok := stClientKeyHistoryDomain()
	if !ok {
		return model.NetworkWalletMappingOwner{}, protocol.ErrWalletMappingUnavailable
	}
	return model.NetworkWalletMappingOwner{Domain: domain, UserId: clientSession.ByJwt.UserId, NetworkId: clientSession.ByJwt.NetworkId}, nil
}

// This public authenticated request cannot override the configured deployment,
// the session's network, the original predecessor or the challenge expiry.
func SnNetworkWalletMappingChallenge(args *SnNetworkWalletMappingChallengeArgs, clientSession *session.ClientSession) (*SnWalletMappingChallengeResult, error) {
	if args == nil {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	owner, err := snNetworkWalletMappingOwner(clientSession)
	if err != nil {
		return nil, err
	}
	coldkey, err := ss58.DecodeWithPrefix(strings.TrimSpace(args.ColdkeySs58), ss58.BittensorPrefix)
	if err != nil || SnWalletBanned(coldkey) {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	ctx, cancel := context.WithTimeout(clientSession.Ctx, 300*time.Second)
	defer cancel()
	owner.Prospective, err = snWalletMappingProspective(ctx, owner.Domain)
	if err != nil {
		return nil, err
	}
	message, err := model.CreateNetworkWalletMappingChallenge(ctx, owner, coldkey, args.FromEpoch, args.ThroughEpoch)
	if err != nil {
		return nil, err
	}
	return &SnWalletMappingChallengeResult{Message: message}, nil
}

// A network consent submitted to POST /sn/wallet. It names no client, so a
// request that selects one is refused rather than reinterpreted.
func snAcceptNetworkWalletMapping(args *SnSetWalletArgs, clientSession *session.ClientSession) (*SnSetWalletResult, error) {
	if args.ClientId != nil {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	owner, err := snNetworkWalletMappingOwner(clientSession)
	if err != nil {
		return nil, err
	}
	coldkeySs58 := strings.TrimSpace(args.ColdkeySs58)
	coldkey, err := ss58.DecodeWithPrefix(coldkeySs58, ss58.BittensorPrefix)
	if err != nil || SnWalletBanned(coldkey) {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	signature, err := hex.DecodeString(strings.TrimPrefix(args.Signature, "0x"))
	if err != nil || len(signature) != 64 {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	// a network consent that names the entered coldkey, with a well-formed
	// signature that verifies for it under neither substrate transcript: in
	// practice another account signed it
	signatureMismatch := func() bool {
		statement, err := protocol.DecodeNetworkWalletMappingStatement(args.Message)
		if err != nil || statement.Coldkey != coldkey {
			return false
		}
		valid, err := model.VerifyBittensorSignature(coldkeySs58, args.Message, args.Signature)
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
	accepted, err := model.AcceptNetworkWalletMappingConsent(ctx, owner, original, coldkeySs58)
	if err != nil {
		return nil, err
	}
	return &SnSetWalletResult{MappingHash: hex.EncodeToString(accepted.OriginalHash[:]), MappingGeneration: accepted.Generation}, nil
}

// Readers supply their independently pinned head; the API never selects the
// newest row as authority. The domain is a namespace filter, not a grant.
type SnNetworkWalletMappingHistoryArgs struct {
	Domain     protocol.ClientKeyHistoryDomain `json:"domain"`
	NetworkId  server.Id                       `json:"network_id"`
	HeadHash   [32]byte                        `json:"head_hash"`
	Generation uint64                          `json:"generation"`
}

// Signature-bearing originals are public evidence. Missing history stays
// unavailable rather than known-empty.
func SnNetworkWalletMappingHistory(args *SnNetworkWalletMappingHistoryArgs, clientSession *session.ClientSession) (*SnWalletMappingHistoryResult, error) {
	if args == nil || clientSession == nil || clientSession.Ctx == nil {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	originals, err := model.ReadNetworkWalletMappingHistory(clientSession.Ctx, args.Domain, args.NetworkId, args.Generation, args.HeadHash)
	if err != nil {
		return nil, err
	}
	return &SnWalletMappingHistoryResult{Originals: originals}, nil
}
