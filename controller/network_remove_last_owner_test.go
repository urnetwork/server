//go:build sidecar_pending

package controller

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// §7.5's last Owner at POST /auth/network-delete (network_remove_providers.go; owner decision 329, QD-941), which
// needs MG6: the last Owner of a team is refused before any store step, so nothing is looked up or cancelled at
// Stripe, the App Store or Google Play and nothing is deleted; once another member is Owner the same deletion runs
// every store step and removes the account with its memberships and IdP identities.

// A person with a personal network and its credential there, and, once it joins a team, its membership and a
// credential in the team.
type lastOwnerRoutePerson struct {
	networkId   server.Id
	userId      server.Id
	session     *session.ClientSession
	member      *model.NetworkMember
	teamSession *session.ClientSession
}

func newLastOwnerRoutePerson(t testing.TB, ctx context.Context, label string) *lastOwnerRoutePerson {
	t.Helper()
	networkId, userId, clientSession := testSwitchPersonalNetwork(t, ctx, label)
	return &lastOwnerRoutePerson{
		networkId: networkId,
		userId:    userId,
		session:   clientSession,
	}
}

// A team the person creates, so it is its first Owner: the team's network id and name.
func (self *lastOwnerRoutePerson) createTeam(t testing.TB, label string) (server.Id, string) {
	t.Helper()
	name := fmt.Sprintf("%s-%s", label, strings.ReplaceAll(server.NewId().String(), "-", "")[24:32])
	created, err := model.CreateNetworkTeam(&model.CreateNetworkTeamArgs{NetworkName: name}, self.session)
	if err != nil || created.Error != nil || created.NetworkId == nil || created.MemberId == nil {
		t.Fatalf("team-create %q: %v %+v", name, err, created)
	}
	self.member = model.GetNetworkMemberOfUser(self.session.Ctx, *created.NetworkId, self.userId)
	return *created.NetworkId, created.NetworkName
}

// Joins the person to the team with role, as the config repo's mirror would set it, with a credential there.
func (self *lastOwnerRoutePerson) join(t testing.TB, ctx context.Context, teamNetworkId server.Id, teamName string, role model.NetworkMemberRole) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		var err error
		self.member, err = model.AddNetworkMemberInTx(ctx, tx, &model.AddNetworkMemberArgs{
			NetworkId: teamNetworkId,
			UserId:    self.userId,
			Role:      role,
		})
		server.Raise(err)
	})
	memberId := self.member.MemberId
	self.teamSession = session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId:   teamNetworkId,
		UserId:      self.userId,
		NetworkName: teamName,
		MemberId:    &memberId,
	})
}

// The count of a query's one-number answer.
func lastOwnerRouteCount(ctx context.Context, sql string, args ...any) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, sql, args...).Scan(&count))
	})
	return
}

// The users holding the team's live Owner role, in user id order.
func lastOwnerRouteOwners(ctx context.Context, teamNetworkId server.Id) []server.Id {
	userIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT user_id FROM network_member WHERE network_id = $1 AND role = $2 AND NOT pending ORDER BY user_id`,
			teamNetworkId,
			string(model.NetworkMemberRoleOwner),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var userId server.Id
				server.Raise(result.Scan(&userId))
				userIds = append(userIds, userId)
			}
		})
	})
	return userIds
}

// The last Owner's deletion through the route, with a renewal at each store: refused with its code and team before
// any store step, so no store is asked or told anything and nothing is deleted. Once the Admin is made Owner, the
// same deletion asks the App Store, cancels at Stripe and Google Play, and removes the account, its memberships and
// its IdP identities, leaving the team to its new Owner.
func TestNetworkRemoveOfTheLastTeamOwnerCancelsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		owner := newLastOwnerRoutePerson(t, ctx, "route-last-owner")
		admin := newLastOwnerRoutePerson(t, ctx, "route-last-admin")
		teamNetworkId, teamName := owner.createTeam(t, "route-last-team")
		admin.join(t, ctx, teamNetworkId, teamName, model.NetworkMemberRoleAdmin)
		idpId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_idp (idp_id, network_id, issuer, client_id, client_auth) VALUES ($1, $2, $3, $4, $5)`,
				idpId, teamNetworkId, "https://idp.example", "route-last-client", "private_key_jwt",
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_idp_identity (idp_id, sub, user_id) VALUES ($1, $2, $3)`,
				idpId, "route-last-sub", owner.userId,
			))
		})

		// a renewal at each store, each of which the deletion would ask or cancel
		now := server.NowUtc()
		stripe := newNetworkRemoveStripeFake(t)
		invoiceId, subscriptionId := "in_synthetic_last_owner", "sub_synthetic_last_owner"
		addNetworkRemoveStripeRenewal(t, ctx, owner.networkId, invoiceId, now.Add(-time.Hour), now.Add(time.Hour))
		stripe.add(invoiceId, subscriptionId, "active", http.StatusOK)
		play := newPlayCancelFake(t)
		yes := true
		play.subs["synthetic-play-last-owner"] = playCancelTestSubscription("SUBSCRIPTION_STATE_ACTIVE", &yes)
		appleLookups := []string{}
		previousAppleLookup := subscriptionStoreLookups.apple
		subscriptionStoreLookups.apple = func(ctx context.Context, originalTransactionId string) (*subscriptionStoreState, error) {
			appleLookups = append(appleLookups, originalTransactionId)
			no := false
			return &subscriptionStoreState{AutoRenew: &no, CancelAtPeriodEnd: true, Active: true}, nil
		}
		defer func() { subscriptionStoreLookups.apple = previousAppleLookup }()
		for i, renewal := range []*model.SubscriptionRenewal{
			{SubscriptionMarket: model.SubscriptionMarketApple, TransactionId: "synthetic-apple-last-owner"},
			{SubscriptionMarket: model.SubscriptionMarketGoogle, PurchaseToken: "synthetic-play-last-owner"},
		} {
			renewal.NetworkId = owner.networkId
			renewal.SubscriptionType = model.SubscriptionTypeSupporter
			renewal.StartTime = now.Add(-time.Hour)
			renewal.EndTime = now.Add(time.Duration(2+i) * time.Hour)
			if err := model.AddSubscriptionRenewal(ctx, renewal); err != nil {
				t.Fatalf("add %s renewal: %v", renewal.SubscriptionMarket, err)
			}
		}

		result, err := NetworkRemove(owner.session)
		requireNetworkRemoveLastTeamOwner(t, result, err, &model.LastOwnerTeam{NetworkId: teamNetworkId, NetworkName: teamName})
		if get, deleted := stripe.counts(invoiceId, subscriptionId); get != 0 || deleted != 0 {
			t.Fatalf("Stripe calls = get %d delete %d, want none", get, deleted)
		}
		if !networkRemoveStripeRenewalActive(ctx, owner.networkId, invoiceId) {
			t.Fatal("the refused deletion closed the Stripe renewal")
		}
		play.lock.Lock()
		playLooked, playCancelled := slices.Clone(play.looked), slices.Clone(play.cancelled)
		play.lock.Unlock()
		if len(playLooked) != 0 || len(playCancelled) != 0 || len(appleLookups) != 0 {
			t.Fatalf("store calls = Play looked %v cancelled %v, App Store %v, want none", playLooked, playCancelled, appleLookups)
		}
		if model.GetNetwork(owner.session) == nil ||
			lastOwnerRouteCount(ctx, `SELECT count(*) FROM network_member WHERE user_id = $1`, owner.userId) != 1 ||
			lastOwnerRouteCount(ctx, `SELECT count(*) FROM network_idp_identity WHERE user_id = $1`, owner.userId) != 1 {
			t.Fatal("the refused deletion deleted the owner's rows")
		}

		// the team's AdminSet names the Admin Owner, which the mirror writes
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_member SET role = $2 WHERE member_id = $1`,
				admin.member.MemberId,
				string(model.NetworkMemberRoleOwner),
			))
		})
		result, err = NetworkRemove(owner.session)
		if err != nil || result == nil || result.Error != nil {
			t.Fatalf("remove = %+v %v, want removed", result, err)
		}
		if get, deleted := stripe.counts(invoiceId, subscriptionId); get != 1 || deleted != 1 {
			t.Fatalf("Stripe calls = get %d delete %d, want 1/1", get, deleted)
		}
		play.lock.Lock()
		playCancelled = slices.Clone(play.cancelled)
		play.lock.Unlock()
		if !slices.Equal(playCancelled, []string{"synthetic-play-last-owner"}) || !slices.Equal(appleLookups, []string{"synthetic-apple-last-owner"}) {
			t.Fatalf("store calls = Play cancelled %v, App Store %v", playCancelled, appleLookups)
		}
		if model.GetNetwork(owner.session) != nil ||
			lastOwnerRouteCount(ctx, `SELECT count(*) FROM network_member WHERE user_id = $1`, owner.userId) != 0 ||
			lastOwnerRouteCount(ctx, `SELECT count(*) FROM network_idp_identity WHERE user_id = $1`, owner.userId) != 0 {
			t.Fatal("the deletion left the owner's rows")
		}
		if owners := lastOwnerRouteOwners(ctx, teamNetworkId); !slices.Equal(owners, []server.Id{admin.userId}) {
			t.Fatalf("the team's Owners are %v, want the new Owner alone", owners)
		}
	})
}
