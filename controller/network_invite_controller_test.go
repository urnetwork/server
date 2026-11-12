//go:build sidecar_pending

package controller

// POST /auth/network-switch (network_invite_controller.go; TEAM-M6-04, §7.5), which needs MG6: a person's own
// credential buys one for another network it signs in to, honoring the target's sign-in rule.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A person's personal network and a session of its credential.
func testSwitchPersonalNetwork(t testing.TB, ctx context.Context, label string) (server.Id, server.Id, *session.ClientSession) {
	t.Helper()
	networkId := server.NewId()
	userId := server.NewId()
	name := fmt.Sprintf("%s-%s", label, strings.ToLower(networkId.String()[:8]))
	model.Testing_CreateNetwork(ctx, networkId, name, userId)
	clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, NetworkName: name})
	return networkId, userId, clientSession
}

// Switching across networks: from the person's own network into its team, as the team's member; never into a
// network it has no sign-in to, nor its own network again; a client token switches nothing; an SSO-required team
// refuses a switch, since no other network's credential came through its IdP.
func TestNetworkSwitchAcrossNetworks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		personalNetworkId, userId, personal := testSwitchPersonalNetwork(t, ctx, "switch-person")
		strangerNetworkId, _, _ := testSwitchPersonalNetwork(t, ctx, "switch-stranger")

		teamName := fmt.Sprintf("switch-team-%s", strings.ReplaceAll(server.NewId().String(), "-", "")[24:32])
		created, err := model.CreateNetworkTeam(&model.CreateNetworkTeamArgs{NetworkName: teamName}, personal)
		if err != nil || created.Error != nil || created.NetworkId == nil {
			t.Fatalf("team-create: %v %+v", err, created)
		}

		switched, err := NetworkSwitch(&NetworkSwitchArgs{NetworkId: *created.NetworkId}, personal)
		connect.AssertEqual(t, err, nil)
		if switched.Error != nil {
			t.Fatalf("the switch: %+v", switched.Error)
		}
		connect.AssertEqual(t, *switched.NetworkId, *created.NetworkId)
		connect.AssertEqual(t, *switched.MemberId, *created.MemberId)
		connect.AssertEqual(t, switched.Role, model.NetworkMemberRoleOwner)
		byJwt, err := session.ParseByJwt(ctx, switched.ByJwt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, byJwt.NetworkId, *created.NetworkId)
		connect.AssertEqual(t, byJwt.UserId, userId)
		connect.AssertEqual(t, *byJwt.MemberId, *created.MemberId)
		connect.AssertEqual(t, strings.Contains(switched.String()+switched.GoString(), switched.ByJwt), false)

		// and back to its own network, from the team's credential
		team := session.Testing_CreateClientSession(ctx, byJwt)
		back, err := NetworkSwitch(&NetworkSwitchArgs{NetworkId: personalNetworkId}, team)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, back.Error == nil && *back.NetworkId == personalNetworkId && back.MemberId == nil, true)

		refused, err := NetworkSwitch(&NetworkSwitchArgs{NetworkId: strangerNetworkId}, personal)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refused.Error.Message, networkSwitchNoAccessMessage)
		refused, err = NetworkSwitch(&NetworkSwitchArgs{NetworkId: personalNetworkId}, personal)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refused.Error.Message, networkSwitchSameMessage)

		clientId := server.NewId()
		client := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: personalNetworkId, UserId: userId, ClientId: &clientId})
		if _, err := NetworkSwitch(&NetworkSwitchArgs{NetworkId: *created.NetworkId}, client); err == nil {
			t.Fatal("a client token switched networks")
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_team SET sso_required = true WHERE network_id = $1`, *created.NetworkId))
		})
		refused, err = NetworkSwitch(&NetworkSwitchArgs{NetworkId: *created.NetworkId}, personal)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refused.Error.Message, model.ErrNetworkSsoRequired.Error())
	})
}
