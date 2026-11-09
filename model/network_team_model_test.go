//go:build sidecar_pending

package model

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// §7.5's team networks, which need MG6 (TEAM-M6-01).

// The team routes with the dependencies a test sets.
func testNetworkTeams(now func() time.Time) *networkTeams {
	teams := newNetworkTeams()
	teams.now = now
	return teams
}

// A personal network the test owns, as a signup makes one, and its owner's
// session: a network credential, never a client's.
func testTeamPersonalNetwork(t testing.TB, ctx context.Context, label string) (networkId server.Id, userId server.Id, userAuth string, clientSession *session.ClientSession) {
	t.Helper()
	networkId = server.NewId()
	userId = server.NewId()
	name := fmt.Sprintf("%s-%s", label, strings.ToLower(networkId.String()[:8]))
	userAuth = Testing_CreateNetwork(ctx, networkId, name, userId)
	clientSession = session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId:   networkId,
		UserId:      userId,
		NetworkName: name,
	})
	return
}

// A team name no other test uses: the label and 8 random hex characters.
func testTeamName(label string) string {
	return fmt.Sprintf("%s-%s", label, strings.ReplaceAll(server.NewId().String(), "-", "")[24:32])
}

// Creates a team with the session, failing the test on a refusal.
func createTestTeam(t testing.TB, teams *networkTeams, clientSession *session.ClientSession, name string) *CreateNetworkTeamResult {
	t.Helper()
	result, err := teams.create(&CreateNetworkTeamArgs{NetworkName: name}, clientSession)
	if err != nil || result.Error != nil || result.NetworkId == nil || result.MemberId == nil {
		t.Fatalf("team-create %q: %v %+v", name, err, result)
	}
	return result
}

func requireTeamCreateRefused(t testing.TB, result *CreateNetworkTeamResult, err error, code string) {
	t.Helper()
	if err != nil || result == nil || result.Error == nil || result.Error.Code != code || result.NetworkId != nil {
		t.Fatalf("team-create answered %v %+v, want a %s refusal", err, result, code)
	}
}

// The network's admin_user_id, nil for no network.
func testNetworkAdminUserId(ctx context.Context, networkId server.Id) (adminUserId *server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT admin_user_id FROM network WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&adminUserId))
			}
		})
	})
	return
}

// The count of a query's one-number answer.
func testTeamCount(ctx context.Context, sql string, args ...any) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, sql, args...).Scan(&count))
	})
	return
}

// team-create makes a new network with one member, the caller, as its Owner, its
// admin_user_id a synthetic user apart from every person, a network_team row
// and its two admin events, and leaves the caller's own network as it was: a
// personal network is never converted (Q-TEAM-2).
func TestTeamCreateMakesOneOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		personalNetworkId, userId, userAuth, clientSession := testTeamPersonalNetwork(t, ctx, "team-one-owner")
		teams := testNetworkTeams(server.NowUtc)

		name := testTeamName("team-one")
		created := createTestTeam(t, teams, clientSession, name)
		teamNetworkId := *created.NetworkId
		if created.NetworkName != name || created.Role != NetworkMemberRoleOwner || teamNetworkId == personalNetworkId {
			t.Fatalf("team-create answered %+v", created)
		}

		// one member, the caller, the Owner, not pending, with its verified email
		members := []*NetworkMember{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT `+networkMemberColumns+` FROM network_member WHERE network_id = $1`, teamNetworkId)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					members = append(members, scanNetworkMember(result))
				}
			})
		})
		if len(members) != 1 {
			t.Fatalf("the team has %d members, want 1", len(members))
		}
		owner := members[0]
		if owner.MemberId != *created.MemberId || owner.UserId != userId || owner.Role != NetworkMemberRoleOwner ||
			owner.Pending || owner.ReauthDeadline != nil || owner.Email == nil || *owner.Email != userAuth {
			t.Fatalf("the first member is %+v", owner)
		}
		// member_id is new, not the user's id as in a personal network
		if owner.MemberId == userId {
			t.Fatal("the member id is the user id")
		}

		// admin_user_id is a synthetic owner of record, never the caller
		adminUserId := testNetworkAdminUserId(ctx, teamNetworkId)
		if adminUserId == nil || *adminUserId == userId {
			t.Fatalf("the team's admin_user_id is %v", adminUserId)
		}
		if team := GetNetworkTeam(ctx, teamNetworkId); team == nil || team.NetworkId != teamNetworkId || team.SsoRequired {
			t.Fatalf("the team's row is %+v", team)
		}
		if GetNetworkTeam(ctx, personalNetworkId) != nil {
			t.Fatal("the caller's personal network became a team")
		}

		// the caller's own network is unchanged
		if personalAdmin := testNetworkAdminUserId(ctx, personalNetworkId); personalAdmin == nil || *personalAdmin != userId {
			t.Fatalf("the personal network's admin is now %v", personalAdmin)
		}
		if count := testTeamCount(ctx, `SELECT count(*) FROM network_member WHERE network_id = $1`, personalNetworkId); count != 0 {
			t.Fatalf("the personal network has %d member rows", count)
		}

		// the two admin events, in the team's own chain, which verifies
		events := GetNetworkAdminEvents(ctx, teamNetworkId, 0, 0)
		if len(events) != 2 || events[0].Action != AdminEventActionTeamCreate || events[0].TargetId == nil || *events[0].TargetId != teamNetworkId ||
			events[1].Action != AdminEventActionMemberAdd || events[1].TargetKind != AdminEventTargetMember || events[1].TargetId == nil || *events[1].TargetId != owner.MemberId {
			t.Fatalf("the team's admin events are %+v", events)
		}
		for _, event := range events {
			if event.ActorKind != AdminEventActorUser || event.ActorId == nil || *event.ActorId != userId {
				t.Fatalf("an admin event's actor is %s %v", event.ActorKind, event.ActorId)
			}
		}
		if err := VerifyNetworkAdminEventChain(ctx, teamNetworkId); err != nil {
			t.Fatal(err)
		}
		if events := GetNetworkAdminEvents(ctx, personalNetworkId, 0, 0); len(events) != 0 {
			t.Fatalf("the personal network has admin events %+v", events)
		}
	})
}

// The synthetic owner of record has no sign-in method and can never
// authenticate: no sign-in row names it, and the state check every credential
// passes refuses a session minted for it, whatever minted it, while the caller's
// own session passes the same check (D-TEAM-2).
func TestTeamOwnerOfRecordCannotSignIn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		personalNetworkId, userId, _, clientSession := testTeamPersonalNetwork(t, ctx, "team-no-sign-in")
		name := testTeamName("team-no-sign-in")
		created := createTestTeam(t, testNetworkTeams(server.NowUtc), clientSession, name)
		teamNetworkId := *created.NetworkId
		ownerOfRecordUserId := *testNetworkAdminUserId(ctx, teamNetworkId)

		// no sign-in method at all
		server.Db(ctx, func(conn server.PgConn) {
			var authType string
			var userAuth, walletAddress *string
			var passwordHash []byte
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT auth_type, user_auth, password_hash, wallet_address FROM network_user WHERE user_id = $1`,
				ownerOfRecordUserId,
			).Scan(&authType, &userAuth, &passwordHash, &walletAddress))
			if authType != AuthTypeTeamOwnerOfRecord || userAuth != nil || passwordHash != nil || walletAddress != nil {
				t.Fatalf("the owner of record holds a sign-in: %s %v %v %v", authType, userAuth, passwordHash != nil, walletAddress)
			}
		})
		for _, table := range []string{
			"network_user_auth_password",
			"network_user_auth_sso",
			"network_user_auth_wallet",
			"network_user_auth_seedphrase",
			"network_idp_identity",
			"network_member",
		} {
			if count := testTeamCount(ctx, `SELECT count(*) FROM `+table+` WHERE user_id = $1`, ownerOfRecordUserId); count != 0 {
				t.Fatalf("%s holds %d rows of the owner of record", table, count)
			}
		}

		// a session for the owner of record is refused at its mint and at the
		// state check, however it was minted
		ownerOfRecordJwt := session.NewByJwt(teamNetworkId, ownerOfRecordUserId, name, false, false)
		if _, err := session.MintNetworkSession(ctx, ownerOfRecordJwt, "password"); !errors.Is(err, session.ErrByJwtInactive) {
			t.Fatalf("a session for the owner of record minted: %v", err)
		}
		if err := session.ValidateByJwtState(ctx, session.NewByJwt(teamNetworkId, ownerOfRecordUserId, name, false, false), false); !errors.Is(err, session.ErrByJwtInactive) {
			t.Fatalf("a token for the owner of record passed the state check: %v", err)
		}
		// nor does it get an auth code, which AuthCodeCreate refuses by raising
		// in its transaction, as the router answers it
		var codeResult *AuthCodeCreateResult
		var codeErr error
		server.HandleError(func() {
			codeResult, codeErr = AuthCodeCreate(&AuthCodeCreateArgs{}, session.Testing_CreateClientSession(ctx, ownerOfRecordJwt))
		}, func(cause error) {
			codeErr = cause
		})
		if !errors.Is(codeErr, session.ErrByJwtInactive) {
			t.Fatalf("the owner of record's auth code answered %v %+v", codeErr, codeResult)
		}

		// the control: the caller's own session passes the same checks
		callerJwt := session.NewByJwt(personalNetworkId, userId, clientSession.ByJwt.NetworkName, false, false)
		if _, err := session.MintNetworkSession(ctx, callerJwt, "password"); err != nil {
			t.Fatalf("the caller's own session did not mint: %v", err)
		}
		if err := session.ValidateByJwtState(ctx, callerJwt, false); err != nil {
			t.Fatalf("the caller's own session was refused: %v", err)
		}
		// and the caller holds no session of the team through its own user: the
		// team's admin is not the caller, so its state check refuses, until
		// TEAM-M6-02's member tokens
		if err := session.ValidateByJwtState(ctx, session.NewByJwt(teamNetworkId, userId, name, false, false), false); !errors.Is(err, session.ErrByJwtInactive) {
			t.Fatalf("a token naming the Owner's user in the team passed the state check: %v", err)
		}
	})
}

// team-create's name meets a rename's rules, and a refused create writes
// nothing and spends nothing.
func TestTeamCreateNameRules(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		_, userId, _, clientSession := testTeamPersonalNetwork(t, ctx, "team-name-rules")
		teams := testNetworkTeams(server.NowUtc)
		// the user's budget: one create for the taken name, one after the refusals
		teams.settings.CreateLimit = 2

		for _, name := range []string{"abc", "has space!", "-edge-hyphen", "xn--confusable"} {
			result, err := teams.create(&CreateNetworkTeamArgs{NetworkName: name}, clientSession)
			requireTeamCreateRefused(t, result, err, CreateNetworkTeamRefusalInvalid)
		}

		// a network holds the name, or one a single edit from it
		taken := createTestTeam(t, testNetworkTeams(server.NowUtc), clientSession, testTeamName("team-taken"))
		result, err := teams.create(&CreateNetworkTeamArgs{NetworkName: taken.NetworkName}, clientSession)
		requireTeamCreateRefused(t, result, err, CreateNetworkTeamRefusalNotAvailable)
		near := taken.NetworkName[:len(taken.NetworkName)-1] + "x"
		if near == taken.NetworkName {
			near = taken.NetworkName[:len(taken.NetworkName)-1] + "y"
		}
		result, err = teams.create(&CreateNetworkTeamArgs{NetworkName: near}, clientSession)
		requireTeamCreateRefused(t, result, err, CreateNetworkTeamRefusalNotAvailable)

		// a released name's hold
		held := testTeamName("team-held")
		server.Tx(ctx, func(tx server.PgTx) {
			releaseNetworkNameInTx(ctx, tx, held, server.NowUtc().Add(time.Hour))
		})
		result, err = teams.create(&CreateNetworkTeamArgs{NetworkName: held}, clientSession)
		requireTeamCreateRefused(t, result, err, CreateNetworkTeamRefusalNotAvailable)

		// the taken name made one team and spent one attempt; no refusal wrote
		// a team or spent the budget, so its second attempt is left
		if count := testTeamCount(ctx, `SELECT count(*) FROM network_member WHERE user_id = $1`, userId); count != 1 {
			t.Fatalf("the user holds %d memberships, want 1", count)
		}
		if count := testTeamCount(ctx, `SELECT count(*) FROM network_user_action_attempt WHERE user_id = $1 AND action = $2`, userId, AccountActionTeamCreate); count != 1 {
			t.Fatalf("the user spent %d attempts, want 1", count)
		}
		createTestTeam(t, teams, clientSession, testTeamName("team-after-refusals"))
	})
}

// A user creates at most CreateLimit teams in CreateWindow, on a fake clock: the
// refusal writes nothing, and the budget refills as the window passes.
func TestTeamCreateBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		_, userId, _, clientSession := testTeamPersonalNetwork(t, ctx, "team-budget")
		t0 := server.NowUtc()
		now := t0
		teams := testNetworkTeams(func() time.Time { return now })
		teams.settings.CreateLimit = 2
		teams.settings.CreateWindow = 24 * time.Hour

		createTestTeam(t, teams, clientSession, testTeamName("team-budget-a"))
		now = t0.Add(time.Hour)
		createTestTeam(t, teams, clientSession, testTeamName("team-budget-b"))
		now = t0.Add(2 * time.Hour)
		result, err := teams.create(&CreateNetworkTeamArgs{NetworkName: testTeamName("team-budget-c")}, clientSession)
		requireTeamCreateRefused(t, result, err, CreateNetworkTeamRefusalRateLimited)
		if count := testTeamCount(ctx, `SELECT count(*) FROM network_member WHERE user_id = $1`, userId); count != 2 {
			t.Fatalf("the user holds %d memberships, want 2", count)
		}

		// the first attempt leaves the window at t0 + 24 h, and one fits again
		now = t0.Add(24*time.Hour - time.Second)
		result, err = teams.create(&CreateNetworkTeamArgs{NetworkName: testTeamName("team-budget-d")}, clientSession)
		requireTeamCreateRefused(t, result, err, CreateNetworkTeamRefusalRateLimited)
		now = t0.Add(24*time.Hour + time.Second)
		createTestTeam(t, teams, clientSession, testTeamName("team-budget-e"))
	})
}

// A client token never creates a team, and a binary route answers 503 before
// MG6; the router refuses client tokens on the route too (route_authz.go).
func TestTeamCreateRefusesAClientToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		networkId, userId, _, _ := testTeamPersonalNetwork(t, ctx, "team-client-token")
		clientId := server.NewId()
		deviceId := server.NewId()
		clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
			ClientId:  &clientId,
			DeviceId:  &deviceId,
		})
		result, err := testNetworkTeams(server.NowUtc).create(&CreateNetworkTeamArgs{NetworkName: testTeamName("team-client")}, clientSession)
		if err == nil || !strings.HasPrefix(err.Error(), fmt.Sprintf("%d ", http.StatusForbidden)) || result != nil {
			t.Fatalf("a client token's team-create answered %v %+v", err, result)
		}
		if count := testTeamCount(ctx, `SELECT count(*) FROM network_member WHERE user_id = $1`, userId); count != 0 {
			t.Fatalf("the user holds %d memberships", count)
		}
	})
}

// Every Team route refuses a network outside the team tier with a QuotaRefusal
// whose upgrade is set (402): a personal network, and a team before its checkout,
// whose tier is still free. team-create is no Team route, so it needs no tier.
func TestTeamRoutesRefusedOutsideTheTier(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		personalNetworkId, _, _, clientSession := testTeamPersonalNetwork(t, ctx, "team-tier")
		created := createTestTeam(t, testNetworkTeams(server.NowUtc), clientSession, testTeamName("team-tier"))
		teamNetworkId := *created.NetworkId

		requireTierRefusal := func(err error) {
			t.Helper()
			refusal := &QuotaRefusalError{}
			if !errors.As(err, &refusal) {
				t.Fatalf("answered %v, want a QuotaRefusal", err)
			}
			if refusal.HttpStatus() != http.StatusPaymentRequired || !refusal.Refusal.Upgrade || refusal.Refusal.LimitKey != TeamTierLimitKey ||
				!strings.HasPrefix(refusal.Error(), "402 ") {
				t.Fatalf("the refusal is %d %+v", refusal.HttpStatus(), refusal.Refusal)
			}
		}
		for _, networkId := range []server.Id{personalNetworkId, teamNetworkId} {
			if NetworkTierOf(ctx, networkId) == NetworkTierTeam {
				t.Fatalf("%s is on the team tier before any checkout", networkId)
			}
			requireTierRefusal(RequireTeamTier(ctx, networkId))
			server.Tx(ctx, func(tx server.PgTx) {
				requireTierRefusal(RequireTeamTierInTx(ctx, tx, networkId))
			})
		}
		for _, tier := range []NetworkTier{NetworkTierFree, NetworkTierPro, NetworkTier("")} {
			requireTierRefusal(requireTeamTierOf(tier))
		}
		if err := requireTeamTierOf(NetworkTierTeam); err != nil {
			t.Fatalf("the team tier is refused: %v", err)
		}

		// the member list, through the real tier, refuses the team's own Owner
		ownerSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: teamNetworkId,
			UserId:    clientSession.ByJwt.UserId,
		})
		result, err := GetNetworkMembers(&GetNetworkMembersArgs{}, ownerSession)
		if result != nil {
			t.Fatalf("the member list answered %+v", result)
		}
		requireTierRefusal(err)
		// and a personal network, whose own owner calls it
		result, err = GetNetworkMembers(&GetNetworkMembersArgs{}, clientSession)
		if result != nil {
			t.Fatalf("the member list answered %+v", result)
		}
		requireTierRefusal(err)
	})
}

// On the team tier the member list is the Owner's: in member_id order, a page at
// a time, refused to a non-member, a Member, a pending Owner and a client token.
func TestTeamMembersOwnerReads(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		_, ownerUserId, ownerEmail, ownerPersonalSession := testTeamPersonalNetwork(t, ctx, "team-members-owner")
		created := createTestTeam(t, testNetworkTeams(server.NowUtc), ownerPersonalSession, testTeamName("team-members"))
		teamNetworkId := *created.NetworkId

		_, memberUserId, _, _ := testTeamPersonalNetwork(t, ctx, "team-members-member")
		_, outsiderUserId, _, _ := testTeamPersonalNetwork(t, ctx, "team-members-outsider")
		var member *NetworkMember
		server.Tx(ctx, func(tx server.PgTx) {
			var err error
			member, err = AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{
				NetworkId: teamNetworkId,
				UserId:    memberUserId,
				Role:      NetworkMemberRoleMember,
			})
			server.Raise(err)
		})

		teams := testNetworkTeams(server.NowUtc)
		teams.tierOf = func(ctx context.Context, networkId server.Id) NetworkTier {
			if networkId == teamNetworkId {
				return NetworkTierTeam
			}
			return NetworkTierFree
		}
		sessionOf := func(userId server.Id) *session.ClientSession {
			return session.Testing_CreateClientSession(ctx, &session.ByJwt{
				NetworkId: teamNetworkId,
				UserId:    userId,
			})
		}

		listed, err := teams.getMembers(&GetNetworkMembersArgs{}, sessionOf(ownerUserId))
		if err != nil || listed.More || len(listed.Members) != 2 {
			t.Fatalf("the Owner's list: %v %+v", err, listed)
		}
		byMemberId := map[server.Id]*NetworkMemberResult{}
		for i, result := range listed.Members {
			byMemberId[result.MemberId] = result
			if 0 < i && strings.Compare(listed.Members[i-1].MemberId.String(), result.MemberId.String()) >= 0 {
				t.Fatal("the list is not in member_id order")
			}
		}
		if owner := byMemberId[*created.MemberId]; owner == nil || owner.UserId != ownerUserId || owner.Role != NetworkMemberRoleOwner ||
			owner.Email == nil || *owner.Email != ownerEmail || owner.Pending {
			t.Fatalf("the Owner reads as %+v", owner)
		}
		if listedMember := byMemberId[member.MemberId]; listedMember == nil || listedMember.UserId != memberUserId ||
			listedMember.Role != NetworkMemberRoleMember || listedMember.Email != nil {
			t.Fatalf("the Member reads as %+v", listedMember)
		}

		// a page at a time
		teams.settings.MemberReadLimit = 1
		first, err := teams.getMembers(&GetNetworkMembersArgs{}, sessionOf(ownerUserId))
		if err != nil || !first.More || len(first.Members) != 1 || first.Members[0].MemberId != listed.Members[0].MemberId {
			t.Fatalf("the first page: %v %+v", err, first)
		}
		second, err := teams.getMembers(&GetNetworkMembersArgs{After: &first.Members[0].MemberId}, sessionOf(ownerUserId))
		if err != nil || second.More || len(second.Members) != 1 || second.Members[0].MemberId != listed.Members[1].MemberId {
			t.Fatalf("the second page: %v %+v", err, second)
		}

		// refused to anyone but the Owner until TEAM-M6-03's roles
		requireForbidden := func(clientSession *session.ClientSession) {
			t.Helper()
			result, err := teams.getMembers(&GetNetworkMembersArgs{}, clientSession)
			if err == nil || !strings.HasPrefix(err.Error(), fmt.Sprintf("%d ", http.StatusForbidden)) || result != nil {
				t.Fatalf("answered %v %+v, want 403", err, result)
			}
		}
		requireForbidden(sessionOf(memberUserId))
		requireForbidden(sessionOf(outsiderUserId))
		clientId := server.NewId()
		deviceId := server.NewId()
		requireForbidden(session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: teamNetworkId,
			UserId:    ownerUserId,
			ClientId:  &clientId,
			DeviceId:  &deviceId,
		}))
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `UPDATE network_member SET pending = true WHERE member_id = $1`, *created.MemberId))
		})
		requireForbidden(sessionOf(ownerUserId))
	})
}

// A team's rows go with its network, and a deleted account's memberships and
// IdP identities with it, while the team it belonged to stays, still under its
// owner of record.
func TestTeamRowsGoWithTheirNetworkAndUser(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		_, ownerUserId, _, ownerSession := testTeamPersonalNetwork(t, ctx, "team-rows-owner")
		memberNetworkId, memberUserId, _, _ := testTeamPersonalNetwork(t, ctx, "team-rows-member")

		removedTeam := createTestTeam(t, testNetworkTeams(server.NowUtc), ownerSession, testTeamName("team-rows-removed"))
		keptTeam := createTestTeam(t, testNetworkTeams(server.NowUtc), ownerSession, testTeamName("team-rows-kept"))
		removedTeamNetworkId := *removedTeam.NetworkId
		keptTeamNetworkId := *keptTeam.NetworkId
		removedOwnerOfRecord := *testNetworkAdminUserId(ctx, removedTeamNetworkId)
		keptOwnerOfRecord := *testNetworkAdminUserId(ctx, keptTeamNetworkId)

		idpId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			for _, teamNetworkId := range []server.Id{removedTeamNetworkId, keptTeamNetworkId} {
				_, err := AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{
					NetworkId: teamNetworkId,
					UserId:    memberUserId,
					Role:      NetworkMemberRoleAuditor,
				})
				server.Raise(err)
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_idp (idp_id, network_id, issuer, client_id, client_auth) VALUES ($1, $2, $3, $4, $5)`,
				idpId, removedTeamNetworkId, "https://idp.example", "team-rows-client", "private_key_jwt",
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_idp_identity (idp_id, sub, user_id) VALUES ($1, $2, $3)`,
				idpId, "team-rows-sub", ownerUserId,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_invite (invite_id, network_id, token_sha256, email, role, expire_time) VALUES ($1, $2, $3, $4, $5, $6)`,
				server.NewId(), removedTeamNetworkId, []byte("team-rows-token-sha256-000000000"), "invitee@example.com", "member", server.NowUtc().Add(time.Hour),
			))
		})

		if removed, _ := RemoveNetwork(ctx, removedTeamNetworkId, nil); !removed {
			t.Fatal("the team network was not removed")
		}
		for _, table := range []string{"network_team", "network_member", "network_invite", "network_idp"} {
			if count := testTeamCount(ctx, `SELECT count(*) FROM `+table+` WHERE network_id = $1`, removedTeamNetworkId); count != 0 {
				t.Fatalf("%s holds %d rows of the removed team", table, count)
			}
		}
		if count := testTeamCount(ctx, `SELECT count(*) FROM network_idp_identity WHERE idp_id = $1`, idpId); count != 0 {
			t.Fatalf("the removed team's IdP keeps %d identities", count)
		}
		if count := testTeamCount(ctx, `SELECT count(*) FROM network_user WHERE user_id = $1`, removedOwnerOfRecord); count != 0 {
			t.Fatal("the removed team's owner of record remains")
		}

		// the member deletes its own account: its memberships go, the team stays
		if removed, _ := RemoveNetwork(ctx, memberNetworkId, nil); !removed {
			t.Fatal("the member's network was not removed")
		}
		if count := testTeamCount(ctx, `SELECT count(*) FROM network_member WHERE user_id = $1`, memberUserId); count != 0 {
			t.Fatalf("the deleted user keeps %d memberships", count)
		}
		if GetNetworkTeam(ctx, keptTeamNetworkId) == nil {
			t.Fatal("the kept team went with its member")
		}
		if adminUserId := testNetworkAdminUserId(ctx, keptTeamNetworkId); adminUserId == nil || *adminUserId != keptOwnerOfRecord {
			t.Fatalf("the kept team's admin_user_id is %v", adminUserId)
		}
		if owner := GetNetworkMemberOfUser(ctx, keptTeamNetworkId, ownerUserId); owner == nil || owner.Role != NetworkMemberRoleOwner {
			t.Fatalf("the kept team's Owner is %+v", owner)
		}
	})
}
