// Until an independent roster signer is provisioned, the newest retained
// original of each observed provider's own, network and delegation chain stands
// in for the roster's pinned heads. Only the head source changes: every chain is
// verified, selected at the epoch and gated exactly as a roster-pinned one.
package model

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

// One observed provider of the epoch's payout usage.
type StProviderWalletIdentity struct {
	ClientId  server.Id
	NetworkId server.Id
}

// Owners per head query; each batch reads the primary key of one table.
const stRetainedWalletHeadBatch = 4096

// The newest retained generation of each owner's chain. Originals are
// append-only, so a later history read through this head sees the same chain.
const (
	stRetainedProviderWalletHeadsSql   = `SELECT DISTINCT ON (client_id) client_id,generation,original_hash FROM wallet_mapping_consent WHERE domain_hash=$1 AND client_id=ANY($2) ORDER BY client_id,generation DESC`
	stRetainedNetworkWalletHeadsSql    = `SELECT DISTINCT ON (network_id) network_id,generation,original_hash FROM network_wallet_mapping_consent WHERE domain_hash=$1 AND network_id=ANY($2) ORDER BY network_id,generation DESC`
	stRetainedHotkeyDelegationHeadsSql = `SELECT DISTINCT ON (network_id) network_id,generation,original_hash FROM hotkey_network_delegation WHERE domain_hash=$1 AND network_id=ANY($2) ORDER BY network_id,generation DESC`
)

type stRetainedWalletHead struct {
	generation uint64
	hash       [32]byte
}

func readStRetainedWalletHeads(ctx context.Context, query string, domain [32]byte, ids []server.Id) map[server.Id]stRetainedWalletHead {
	heads := make(map[server.Id]stRetainedWalletHead, len(ids))
	for start := 0; start < len(ids); start += stRetainedWalletHeadBatch {
		batch := ids[start:min(start+stRetainedWalletHeadBatch, len(ids))]
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, query, domain[:], batch)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var id server.Id
					var head stRetainedWalletHead
					var hash []byte
					server.Raise(rows.Scan(&id, &head.generation, &hash))
					if len(hash) != len(head.hash) {
						server.Raise(fmt.Errorf("retained wallet head hash is invalid"))
					}
					copy(head.hash[:], hash)
					heads[id] = head
				}
			})
		})
	}
	return heads
}

// The population is the operator's observed usage, not an independent roster,
// and no wallet comes from the account projection. A provider without accepted
// evidence, or whose evidence is refused, is unmapped instead of holding every
// other provider's payout. A failed read returns no partial map.
func GetStRetainedProviderWalletsForEpoch(ctx context.Context, rootSigner common.Address, scope StProviderWalletEpochScope, providers []StProviderWalletIdentity) (walletClientIds map[server.Id]*StProviderWallet, resultErr error) {
	defer providerWorkRecover(&resultErr)
	if ctx == nil || rootSigner == (common.Address{}) || scope.StartTime.Unix() <= 0 {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scope.Domain.Validate(); err != nil {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	domain, err := scope.Domain.Digest()
	if err != nil {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	clientIds := make([]server.Id, 0, len(providers))
	networkIds := make([]server.Id, 0, len(providers))
	clients := make(map[server.Id]bool, len(providers))
	networks := map[server.Id]bool{}
	for _, provider := range providers {
		if provider.ClientId == (server.Id{}) || provider.NetworkId == (server.Id{}) || clients[provider.ClientId] {
			return nil, protocol.ErrWalletMappingIntegrity
		}
		clients[provider.ClientId] = true
		clientIds = append(clientIds, provider.ClientId)
		if !networks[provider.NetworkId] {
			networks[provider.NetworkId] = true
			networkIds = append(networkIds, provider.NetworkId)
		}
	}
	providerHeads := readStRetainedWalletHeads(ctx, stRetainedProviderWalletHeadsSql, domain, clientIds)
	networkHeads := readStRetainedWalletHeads(ctx, stRetainedNetworkWalletHeadsSql, domain, networkIds)
	delegationHeads := readStRetainedWalletHeads(ctx, stRetainedHotkeyDelegationHeadsSql, domain, networkIds)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pinned := make([]payoutartifact.WholeWorkExpectedProvider, 0, len(providers))
	for _, provider := range providers {
		expected := payoutartifact.WholeWorkExpectedProvider{ClientId: [16]byte(provider.ClientId), NetworkId: [16]byte(provider.NetworkId)}
		if head, ok := providerHeads[provider.ClientId]; ok {
			expected.WalletHeadHash, expected.WalletGeneration = hex.EncodeToString(head.hash[:]), head.generation
		}
		pinned = append(pinned, expected)
	}
	heads := stProviderWalletHeads{
		providers: pinned,
		network: func(networkId [16]byte) (payoutartifact.WholeWorkNetworkWallet, bool) {
			head, ok := networkHeads[server.Id(networkId)]
			if !ok {
				return payoutartifact.WholeWorkNetworkWallet{}, false
			}
			return payoutartifact.WholeWorkNetworkWallet{NetworkId: networkId, WalletHeadHash: hex.EncodeToString(head.hash[:]), WalletGeneration: head.generation}, true
		},
		delegation: func(networkId [16]byte) (payoutartifact.WholeWorkHotkeyDelegation, bool) {
			head, ok := delegationHeads[server.Id(networkId)]
			if !ok {
				return payoutartifact.WholeWorkHotkeyDelegation{}, false
			}
			return payoutartifact.WholeWorkHotkeyDelegation{NetworkId: networkId, DelegationHeadHash: hex.EncodeToString(head.hash[:]), DelegationGeneration: head.generation}, true
		},
	}
	return selectStProviderWalletsForEpoch(ctx, rootSigner, scope, heads, true)
}
