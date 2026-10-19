// Consumer-shaped read of a client's signed key-registration history.
//
// `SnClientKeyObservation` is validator-shaped: it requires a validator hotkey,
// a native chain decision and an EVM decision boundary, and it SIGNS a fresh
// observation, which needs the operator's root key, its artifact key and a
// chain RPC. A consumer client has none of those and is not chain-aware.
//
// This read exists so an ordinary client can corroborate the identity key a
// contract handed it against evidence the operator already signed. It signs
// nothing at request time and touches no key material or RPC: every record it
// returns was signed when it was stored. See connect/DESIGNNOTES3 §6.1.
package controller

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"

	"github.com/urfoundation/sn/v2026/protocol"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// MaxPublicClientKeyHistoryRegistrations bounds a consumer read. It is far
// below the storage bound: a consumer corroborating one peer does not need a
// thousand generations, and a smaller ceiling bounds the work a hostile client
// id can ask of the API.
const MaxPublicClientKeyHistoryRegistrations = 64

// MaxPublicClientKeyHistoryBytes bounds the same read by size.
const MaxPublicClientKeyHistoryBytes = 1024 * 1024

// stClientKeyHistoryDomain builds the deployment domain from configuration
// alone. Unlike `newStClientKeyAuthorityOwner` it needs no signer and no chain
// reader, because nothing on this path signs.
func stClientKeyHistoryDomain() (protocol.ClientKeyHistoryDomain, bool) {
	cfg := stConfig()
	if cfg == nil || !cfg.Enabled || cfg.Netuid == 0 || math.MaxUint16 < cfg.Netuid {
		return protocol.ClientKeyHistoryDomain{}, false
	}
	domain := protocol.ClientKeyHistoryDomain{
		ChainID:          cfg.ChainId,
		GenesisHash:      cfg.GenesisHash,
		Netuid:           uint16(cfg.Netuid),
		Coordinator:      cfg.ContractAddress,
		SettlementVault:  cfg.SettlementVault,
		DeploymentIDHash: sha256.Sum256([]byte(cfg.DeploymentId)),
		PolicyHash:       cfg.PolicyHash,
		NoID:             cfg.NoId,
	}
	if err := domain.Validate(); err != nil {
		return protocol.ClientKeyHistoryDomain{}, false
	}
	return domain, true
}

type GetClientKeyHistoryArgs struct {
	ClientId server.Id `json:"client_id"`
}

// History is the ordered signed registrations, generation 1 first. Empty means
// this client has no signed history — a legacy client, or an operator that does
// not run the signed path. A consumer treats that as the unsigned tier, which
// is why it must be an empty list with HTTP 200 and never an error: an error is
// an availability signal and a consumer must not confuse the two.
type GetClientKeyHistoryResult struct {
	History [][]byte `json:"history"`
}

// GetClientKeyHistory backs `GET /key/<client_id>/history`. Unauthenticated,
// matching `GET /key/<client_id>`: every byte it returns is already-published
// signed evidence, and authenticating the read would protect nothing while
// costing the client a round trip.
func GetClientKeyHistory(
	args *GetClientKeyHistoryArgs,
	clientSession *session.ClientSession,
) (*GetClientKeyHistoryResult, error) {
	empty := &GetClientKeyHistoryResult{History: [][]byte{}}
	if args == nil || args.ClientId == (server.Id{}) {
		return nil, errors.New("client key history lookup identity is missing")
	}
	if clientSession == nil || clientSession.Ctx == nil {
		return nil, errors.New("client key history lookup session is missing")
	}
	domain, ok := stClientKeyHistoryDomain()
	if !ok {
		// the operator does not run the signed path at all
		return empty, nil
	}
	ctx, cancel := context.WithTimeout(clientSession.Ctx, stCallTimeout)
	defer cancel()
	records, err := model.LoadStClientKeyHistory(
		ctx,
		domain,
		args.ClientId,
		MaxPublicClientKeyHistoryRegistrations,
		MaxPublicClientKeyHistoryBytes,
	)
	if err != nil || len(records) == 0 {
		// No head, a retired head, or a census the loader refused. From a
		// consumer's point of view all of these are "no signed evidence for
		// this client", which is the unsigned tier and not a transport
		// failure. The ratchet on the client side is what stops an operator
		// using this branch as a silent downgrade.
		return empty, nil
	}
	history := make([][]byte, 0, len(records))
	for _, record := range records {
		history = append(history, record.RegistrationBytes)
	}
	return &GetClientKeyHistoryResult{History: history}, nil
}
