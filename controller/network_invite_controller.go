package controller

// §7.5's one person in several networks (TEAM-M6-04; R-TEAM-3, D-TEAM-16, I-6;
// server/IDP.md §2): a credential names one network, so a person moves between
// its personal network and the teams it is a member of by exchanging the
// credential it holds for one of another network.
//
//	switch   POST /auth/network-switch {network_id}: the caller's own network
//	         credential, never a client's, an API key or an admin token, buys
//	         a network credential of the same user for the target, where the
//	         user signs in (model.ResolveNetworkSignIn): a personal network it
//	         owns or a team it is a live member of. The target's sign-in rule
//	         holds: an SSO-required team refuses every switch, since no
//	         credential of another network came through the team's IdP (§7.6),
//	         and its members sign in through the IdP alone
//
// The invitations themselves are model/network_invite_model.go's.

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The session row's kind for a switched credential.
const networkSwitchSessionKind = "network_switch"

const (
	networkSwitchCredentialMessage = "A network is switched with a person's own sign-in."
	networkSwitchSameMessage       = "You are signed in to this network."
	networkSwitchNoAccessMessage   = "You have no access to that network."
)

type NetworkSwitchArgs struct {
	NetworkId server.Id `json:"network_id"`
}

type NetworkSwitchError struct {
	Message string `json:"message"`
}

// Prints without the credential.
type NetworkSwitchResult struct {
	ByJwt       string                  `json:"by_jwt,omitempty"`
	NetworkId   *server.Id              `json:"network_id,omitempty"`
	NetworkName string                  `json:"network_name,omitempty"`
	MemberId    *server.Id              `json:"member_id,omitempty"`
	Role        model.NetworkMemberRole `json:"role,omitempty"`
	Error       *NetworkSwitchError     `json:"error,omitempty"`
}

// Never the credential.
func (self *NetworkSwitchResult) String() string {
	return "NetworkSwitchResult{}"
}

// As String, for %#v.
func (self *NetworkSwitchResult) GoString() string {
	return self.String()
}

// POST /auth/network-switch (see the file header).
func NetworkSwitch(args *NetworkSwitchArgs, clientSession *session.ClientSession) (*NetworkSwitchResult, error) {
	byJwt := clientSession.ByJwt
	if byJwt == nil || byJwt.ClientId != nil || clientSession.ApiKeyAuthenticated || clientSession.UnsignedIdentity {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, networkSwitchCredentialMessage)
	}
	refuse := func(message string) (*NetworkSwitchResult, error) {
		return &NetworkSwitchResult{Error: &NetworkSwitchError{Message: message}}, nil
	}
	if args.NetworkId == byJwt.NetworkId {
		return refuse(networkSwitchSameMessage)
	}
	signed, signIn, err := model.MintNetworkSignIn(clientSession.Ctx, byJwt.UserId, args.NetworkId, networkSwitchSessionKind, false)
	if errors.Is(err, model.ErrNetworkSsoRequired) {
		return refuse(model.ErrNetworkSsoRequired.Error())
	}
	if err != nil {
		return nil, err
	}
	if signIn == nil {
		return refuse(networkSwitchNoAccessMessage)
	}
	networkId := signIn.NetworkId
	return &NetworkSwitchResult{
		ByJwt:       signed,
		NetworkId:   &networkId,
		NetworkName: signIn.NetworkName,
		MemberId:    signIn.MemberId,
		Role:        signIn.Role,
	}, nil
}
