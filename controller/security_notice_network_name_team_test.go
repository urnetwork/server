//go:build sidecar_pending

package controller

// A team's security notices name the team (QD-1216), on Postgres with MG0, MG1 and MG6: an auth key, an API key or
// an authority change in a team reaches each of its Owners and Admins at their own sign-in addresses (QD-1215), and
// each of them has a personal network beside it, so a notice that named no network left them to guess which one
// changed. Each notice the team owes says "your URnetwork network <the team's name>", and none names a recipient's
// own network.

import (
	"context"
	"slices"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// An auth key the Owner creates in its team owes the Owner and the team's Admin a notice each, at their own
// addresses, that names the team and neither person's own network; and a key the Owner then creates in its own
// network names that one.
func TestTeamAuthKeyNoticeNamesTheTeam(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		// a person with a network of its own, signing in at the address that network's creation gives it
		newPerson := func(label string) (networkName string, userId server.Id, address string, personal *session.ClientSession) {
			networkId := server.NewId()
			userId = server.NewId()
			networkName = securityNoticeTestNetworkName(label)
			address, _ = model.NormalUserAuth(model.Testing_CreateNetwork(ctx, networkId, networkName, userId))
			personal = session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, NetworkName: networkName})
			return
		}
		ownerNetworkName, ownerUserId, ownerAddress, ownerPersonal := newPerson("named-owner")
		adminNetworkName, adminUserId, adminAddress, _ := newPerson("named-admin")

		teamName := securityNoticeTestNetworkName("named-team")
		created, err := model.CreateNetworkTeam(&model.CreateNetworkTeamArgs{NetworkName: teamName}, ownerPersonal)
		if err != nil || created.Error != nil || created.NetworkId == nil || created.MemberId == nil {
			t.Fatalf("team-create: %v %+v", err, created)
		}
		teamNetworkId := *created.NetworkId
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := model.AddNetworkMemberInTx(ctx, tx, &model.AddNetworkMemberArgs{
				NetworkId: teamNetworkId,
				UserId:    adminUserId,
				Role:      model.NetworkMemberRoleAdmin,
			})
			server.Raise(err)
		})
		// whatever joining owed goes out on its own
		deliverSecurityNoticesOf(t, ctx, "")

		ownerTeamSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId:   teamNetworkId,
			UserId:      ownerUserId,
			NetworkName: teamName,
			MemberId:    created.MemberId,
		})
		key, err := model.CreateAuthKey(&model.CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1}, ownerTeamSession)
		if err != nil || key.Error != nil || key.AuthKeyId == nil {
			t.Fatalf("the Owner's auth key in the team: %v %+v", err, key)
		}
		sends := deliverSecurityNoticesOf(t, ctx, model.SecurityAuthKeyTemplateName)
		recipients := []string{}
		for _, send := range sends {
			recipients = append(recipients, send.userAuth)
			requireSecurityNoticeNamesNetwork(t, send, teamName, ownerNetworkName, adminNetworkName)
			subject, _, _, err := RenderEmailTemplate(send.template)
			if err != nil {
				t.Fatal(err)
			}
			if want := "An auth key was created for your URnetwork network " + teamName; subject != want {
				t.Fatalf("the notice to %s: subject %q, want %q", send.userAuth, subject, want)
			}
		}
		slices.Sort(recipients)
		want := []string{ownerAddress, adminAddress}
		slices.Sort(want)
		if !slices.Equal(recipients, want) {
			t.Fatalf("the team's auth key notice went to %v, want the Owner and the Admin, %v", recipients, want)
		}

		// the Owner's own network is named for itself
		personalKey, err := model.CreateAuthKey(&model.CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1}, ownerPersonal)
		if err != nil || personalKey.Error != nil {
			t.Fatalf("the Owner's auth key in its own network: %v %+v", err, personalKey)
		}
		sends = deliverSecurityNoticesOf(t, ctx, model.SecurityAuthKeyTemplateName)
		if len(sends) != 1 || sends[0].userAuth != ownerAddress {
			t.Fatalf("the personal network's auth key notice went to %+v, want its owner alone", sends)
		}
		requireSecurityNoticeNamesNetwork(t, sends[0], ownerNetworkName, teamName)
	})
}
