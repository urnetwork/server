package controller

import (
	"github.com/urnetwork/server/session"
	// "github.com/urnetwork/server/model"
)

type HelloResult struct {
	ClientAddress string `json:"client_address,omitempty"`
	// the extender root keys whose signatures this network space accepts
	// (connect/EXTENDER.md B4, C7). It arrives over the platform's pinned tls,
	// so it is trustworthy even when the request itself travelled through an
	// untrusted extender, and it replaces whatever list the client stored.
	// Empty when the operator has configured no extender network.
	ExtenderRootPublicKeys []string `json:"extender_root_public_keys,omitempty"`
	// the libp2p peer id of the operator's gossip node (C6, C7), derived from
	// gossip_identity_key_hex. A member dials the operator only when it knows
	// this id, since without it there is nothing to authenticate the far end
	// of the dial against (D3), so an operator running no gossip service
	// serves none rather than an address a member could not verify.
	GossipPeerId string `json:"gossip_peer_id,omitempty"`
}

func Hello(
	session *session.ClientSession,
) (*HelloResult, error) {
	result := &HelloResult{
		ClientAddress: session.ClientAddress,
	}
	// an unconfigured extender network is not a hello failure; the client
	// simply trusts no extender record
	if config, err := EnvExtenderConfig(); err == nil {
		result.ExtenderRootPublicKeys = config.RootPublicKeys()
		// likewise a configured extender network with no gossip service: the
		// records still verify, they simply arrive by feed rather than by mesh
		if gossipPeerId, err := config.GossipPeerId(); err == nil {
			result.GossipPeerId = gossipPeerId
		}
	}
	return result, nil
}
