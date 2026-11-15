//go:build sidecar_pending

package oauth

// The device grant in a team (device_grant.go; TEAM-M6-08, D-TEAM-7), which needs MG6's tables: the admin token names
// the person's member_id, over production's store on the test's Redis (QD-830).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// In a team the admin token names the person's member_id, which the role gate reads, never the user; a person who is
// no live member of the team gets none.
func TestDeviceGrantNamesTheMemberInATeam(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installUrcliTest(t)
		SetDeviceAuthorizationStore(NewRedisDeviceAuthorizationStore(DefaultDeviceGrantSettings()))
		defer SetDeviceAuthorizationStore(nil)
		ctx := context.Background()
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		personalNetworkId, userId := server.NewId(), server.NewId()
		suffix := strings.ReplaceAll(server.NewId().String(), "-", "")[24:32]
		model.Testing_CreateNetwork(ctx, personalNetworkId, fmt.Sprintf("device-owner-%s", suffix), userId)
		personal := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: personalNetworkId, UserId: userId})
		team, err := model.CreateNetworkTeam(&model.CreateNetworkTeamArgs{NetworkName: fmt.Sprintf("device-team-%s", suffix)}, personal)
		if err != nil || team.Error != nil {
			t.Fatalf("team-create: %v %+v", err, team)
		}
		command := &sidecar.AdminCommand{Method: "POST", Path: "/network/member-remove", Body: []byte(`{"member_id":"` + server.NewId().String() + `"}`)}
		session.RegisterAdminCommandRoute("POST", "/network/member-remove")
		now := server.NowUtc()
		device := startTestDeviceLogin(t, ctx, command, now)
		user := &AuthorizingUser{UserId: userId, NetworkId: *team.NetworkId, AuthTime: server.NowUtc()}
		connect.AssertEqual(t, device.approve(ctx, user, DefaultDeviceGrantSettings()), nil)
		response, err := device.poll(ctx, now.Add(time.Minute), device.proof())
		connect.AssertEqual(t, err, nil)
		claims, err := VerifyAdminToken(response.AccessToken)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.MemberId, *team.MemberId)
		connect.AssertEqual(t, claims.Scope, UrcliScopeAll)

		stranger := startTestDeviceLogin(t, ctx, command, now)
		strangerUser := &AuthorizingUser{UserId: server.NewId(), NetworkId: *team.NetworkId, AuthTime: server.NowUtc()}
		connect.AssertEqual(t, stranger.approve(ctx, strangerUser, DefaultDeviceGrantSettings()), nil)
		if _, err := stranger.poll(ctx, now.Add(time.Minute), stranger.proof()); !errors.Is(err, ErrUnauthorizedUser) {
			t.Errorf("a person who is no member: %v", err)
		}
	})
}
