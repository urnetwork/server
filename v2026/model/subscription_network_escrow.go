// Network and friends-and-family contracts escrow from their source's balance,
// like public contracts (owner, 2026-11-10): "all intra-traffic should count
// against the network data balance", "a yes this deducts from the sender's
// transfer balance" and "all traffic shares the same network transfer
// balance". The controller creates them with CreateTransferEscrow, and with the
// zero escrow fallback of the contract degradation valve exactly as for public
// contracts (subscription_zero_escrow.go). So:
//   - A network contract is paid by the network both endpoints belong to, and a
//     friends-and-family contract by the dialing network. Escrow admission
//     applies in full: the Redis reservation, shrink-to-fit, the escrow
//     priority, the test drain and per-client data caps of the paying client
//     (the source), and the ordinary "Insufficient balance" refusal. There is
//     no separate limit for these contracts: the payer's one transfer balance
//     funds every kind of contract.
//   - They settle through the escrow path. The payer is debited for the bytes
//     used, free plan or paid. A participant on the payer's network takes no
//     provider earnings (contractShareIsIntraNetwork), so the receiving client
//     of a network contract and any same-network hop earn nothing, while a
//     participant on another network, such as a friends-and-family provider
//     or an extender, earns under the normal rules.
//   - Network contracts created before this change have no payer and no
//     escrow. They still settle through the no-escrow path, which decides by
//     the contract row alone, never by how a new contract is created.
//
// SN usage does not change: a contract's usage snapshot still lists every
// participant, same-network ones included, as provider usage
// (contract_usage.go), independent of escrow. The SN25 extender amendment (sn
// WHITEPAPER.md §8.6.2) moves same-network shares out of the provider rows into
// the extender pool from its activation epoch, in the epoch reader
// (stContractSharesForEpoch). That reader identifies them with
// contractShareIsIntraNetwork from the snapshot's provider networks and the
// contract's origin network, which for these contracts is now also the
// recorded payer.
package model

import (
	"context"

	"github.com/urnetwork/server/v2026"
)

type contractUsageOriginContextKey struct{}

// The usage origin, the endpoint whose service the contract carries and which
// is never its provider, is derived at insert from the companion link: a
// plain contract's origin is its source, a companion's its destination. A
// same-network companion that the controller normalizes to a plain network
// contract is funded like the forward direction, by its source, but serves its
// destination. This keeps that destination as its usage origin for the
// request's next contract insert, so SN usage never credits the consumer as
// its own provider.
func WithContractUsageOrigin(ctx context.Context, usageOriginIsSource bool) context.Context {
	return context.WithValue(ctx, contractUsageOriginContextKey{}, usageOriginIsSource)
}

// The usage origin an insert writes in place of the one its companion link
// implies, or nil to keep that. Only a plain contract between two clients of
// one network takes the request's explicit origin: a companion, and every
// cross-network contract, keep the derived one whatever the context says.
func contractUsageOriginInsertArg(
	ctx context.Context,
	sourceNetworkId server.Id,
	destinationNetworkId server.Id,
	companionContractId *server.Id,
) *bool {
	usageOriginIsSource, ok := ctx.Value(contractUsageOriginContextKey{}).(bool)
	if !ok || companionContractId != nil || sourceNetworkId != destinationNetworkId {
		return nil
	}
	return &usageOriginIsSource
}

// Whether a participant's even share of a contract is intra-network: the
// participant is on the contract's origin network, which pays for the
// contract. Monetary settlement omits such a share rather than transferring it
// to the other participants (allocateContractParticipantPayouts). This is the
// classification the SN25 extender amendment's epoch reader applies to usage
// snapshot shares (see the file comment).
func contractShareIsIntraNetwork(participantNetworkId server.Id, originNetworkId server.Id) bool {
	return participantNetworkId == originNetworkId
}
