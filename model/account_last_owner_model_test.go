//go:build sidecar_pending

package model

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// §7.5's last Owner (account_last_owner_model.go; owner decision 329, QD-941), which needs MG6: an account deletion is
// refused while its user is a team's last live Owner, deleting nothing and naming the team, and goes ahead once
// another member is Owner; the check's locks hold against a second Owner's deletion and a role change at once. The
// leg without MG6 runs account_model_test.go's deletions, which read no team table.

// A person with a personal network, as a signup makes one, and a session of its credential there.
type lastOwnerTestPerson struct {
	networkId server.Id
	userId    server.Id
	userAuth  string
	session   *session.ClientSession
}

func newLastOwnerTestPerson(t testing.TB, ctx context.Context, label string) *lastOwnerTestPerson {
	t.Helper()
	networkId, userId, userAuth, clientSession := testTeamPersonalNetwork(t, ctx, label)
	return &lastOwnerTestPerson{
		networkId: networkId,
		userId:    userId,
		userAuth:  userAuth,
		session:   clientSession,
	}
}

// A team the person creates, so it is the team's first Owner: the team's network id and name.
func (self *lastOwnerTestPerson) createTeam(t testing.TB, label string) (server.Id, string) {
	t.Helper()
	created := createTestTeam(t, testNetworkTeams(server.NowUtc), self.session, testTeamName(label))
	return *created.NetworkId, created.NetworkName
}

// Joins the person to the team with role, as the config repo's mirror would set it.
func (self *lastOwnerTestPerson) join(t testing.TB, ctx context.Context, teamNetworkId server.Id, role NetworkMemberRole, pending bool) *NetworkMember {
	t.Helper()
	var member *NetworkMember
	server.Tx(ctx, func(tx server.PgTx) {
		var err error
		member, err = AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{
			NetworkId: teamNetworkId,
			UserId:    self.userId,
			Role:      role,
			Pending:   pending,
		})
		server.Raise(err)
	})
	return member
}

// An IdP of the team that has signed the person in, as an SSO sign-in links it.
func (self *lastOwnerTestPerson) linkIdp(t testing.TB, ctx context.Context, teamNetworkId server.Id) {
	t.Helper()
	idpId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_idp (idp_id, network_id, issuer, client_id, client_auth) VALUES ($1, $2, $3, $4, $5)`,
			idpId, teamNetworkId, "https://idp.example", "last-owner-client", "private_key_jwt",
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_idp_identity (idp_id, sub, user_id) VALUES ($1, $2, $3)`,
			idpId, "last-owner-sub-"+self.userId.String(), self.userId,
		))
	})
}

// What the person's deletion would delete, counted: its network, its user, its sign-ins, its memberships and its
// IdP identities.
type lastOwnerTestRows struct {
	network     int
	user        int
	signIns     int
	memberships int
	identities  int
}

func (self *lastOwnerTestPerson) rows(ctx context.Context) lastOwnerTestRows {
	return lastOwnerTestRows{
		network:     testTeamCount(ctx, `SELECT count(*) FROM network WHERE network_id = $1`, self.networkId),
		user:        testTeamCount(ctx, `SELECT count(*) FROM network_user WHERE user_id = $1`, self.userId),
		signIns:     testTeamCount(ctx, `SELECT count(*) FROM network_user_auth_password WHERE user_id = $1`, self.userId),
		memberships: testTeamCount(ctx, `SELECT count(*) FROM network_member WHERE user_id = $1`, self.userId),
		identities:  testTeamCount(ctx, `SELECT count(*) FROM network_idp_identity WHERE user_id = $1`, self.userId),
	}
}

// The users holding the team's live Owner role, in user id order.
func lastOwnerTestOwners(ctx context.Context, teamNetworkId server.Id) []server.Id {
	userIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT user_id FROM network_member WHERE network_id = $1 AND role = $2 AND NOT pending ORDER BY user_id`,
			teamNetworkId,
			string(NetworkMemberRoleOwner),
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

// Requires the deletion's refusal naming the team, and no sign-ins handed back.
func requireLastOwnerRefusal(t testing.TB, removal *RemoveNetworkResult, teamNetworkId server.Id, teamName string) {
	t.Helper()
	if removal.Outcome != RemoveNetworkLastTeamOwner || removal.LastOwnerTeam == nil ||
		removal.LastOwnerTeam.NetworkId != teamNetworkId || removal.LastOwnerTeam.NetworkName != teamName {
		t.Fatalf("removal = %+v (team %+v), want %s naming %s %q", removal, removal.LastOwnerTeam, RemoveNetworkLastTeamOwner, teamNetworkId, teamName)
	}
	if removal.UserAuths != nil {
		t.Fatalf("a refused removal handed back sign-ins %v", removal.UserAuths)
	}
}

// The backend pid of tx's connection.
func lastOwnerTestBackendPid(ctx context.Context, tx server.PgTx) (pid int32) {
	server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	return
}

// Waits until a backend waits on a lock that blockerPid holds, and returns that backend's pid: an observed lock
// wait, the barrier the interleavings below step on.
func waitForLastOwnerLockWaiter(ctx context.Context, blockerPid int32) (waiterPid int32, returnErr error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		found := false
		if r := server.HandleError(func() {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
						SELECT pid
						FROM pg_stat_activity
						WHERE
							datname = current_database() AND
							pid <> pg_backend_pid() AND
							wait_event_type = 'Lock' AND
							$1::integer = ANY(pg_blocking_pids(pid))
						ORDER BY pid
						LIMIT 1
					`,
					blockerPid,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&waiterPid))
						found = true
					}
				})
			})
		}); r != nil {
			return 0, fmt.Errorf("observe the waiters of %d: %v", blockerPid, r)
		}
		if found {
			return waiterPid, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("no backend waited on a lock of %d: %w", blockerPid, ctx.Err())
		case <-ticker.C:
		}
	}
}

// The last Owner's deletion is refused with the team it would leave ownerless, and nothing is deleted: its network,
// user and sign-ins, its memberships in every team and its IdP identities stay. The read before the store steps
// finds the same team.
func TestRemoveNetworkRefusesTheLastTeamOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		owner := newLastOwnerTestPerson(t, ctx, "last-owner")
		member := newLastOwnerTestPerson(t, ctx, "last-owner-member")
		other := newLastOwnerTestPerson(t, ctx, "last-owner-other")
		teamNetworkId, teamName := owner.createTeam(t, "last-owner-team")
		member.join(t, ctx, teamNetworkId, NetworkMemberRoleMember, false)
		owner.linkIdp(t, ctx, teamNetworkId)
		// an Auditor in another team, whose Owner is someone else
		otherTeamNetworkId, _ := other.createTeam(t, "last-owner-other-team")
		owner.join(t, ctx, otherTeamNetworkId, NetworkMemberRoleAuditor, false)
		before := owner.rows(ctx)
		if before != (lastOwnerTestRows{network: 1, user: 1, signIns: 1, memberships: 2, identities: 1}) {
			t.Fatalf("the owner's rows before the deletion are %+v", before)
		}

		if team := GetLastOwnerTeam(ctx, owner.userId, owner.networkId); team == nil || team.NetworkId != teamNetworkId || team.NetworkName != teamName {
			t.Fatalf("the check before the store steps found %+v, want %s %q", team, teamNetworkId, teamName)
		}
		requireLastOwnerRefusal(t, RemoveNetworkWithStoreSnapshot(ctx, owner.networkId, &owner.userId, nil), teamNetworkId, teamName)
		if removed, userAuths := RemoveNetwork(ctx, owner.networkId, &owner.userId); removed || userAuths != nil {
			t.Fatalf("RemoveNetwork answered %v %v, want refused", removed, userAuths)
		}
		if after := owner.rows(ctx); after != before {
			t.Fatalf("the refused deletion left %+v of %+v", after, before)
		}
		if owners := lastOwnerTestOwners(ctx, teamNetworkId); !slices.Equal(owners, []server.Id{owner.userId}) {
			t.Fatalf("the team's Owners are %v", owners)
		}
		if GetNetworkTeam(ctx, teamNetworkId) == nil || GetNetworkMemberOfUser(ctx, teamNetworkId, member.userId) == nil {
			t.Fatal("the refused deletion changed the team")
		}
		// the member, the other team's Owner and the other team's Auditor are no team's last Owner
		if team := GetLastOwnerTeam(ctx, member.userId, member.networkId); team != nil {
			t.Fatalf("a Member is the last Owner of %+v", team)
		}
	})
}

// Once the log makes another member Owner (the role mirror), the deletion goes ahead and takes the user's
// memberships in every team and its IdP identities with it; the team stays, its new Owner its only one.
func TestRemoveNetworkOfTheLastOwnerAfterAnotherIsMadeOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		owner := newLastOwnerTestPerson(t, ctx, "handed-owner")
		admin := newLastOwnerTestPerson(t, ctx, "handed-admin")
		other := newLastOwnerTestPerson(t, ctx, "handed-other")
		teamNetworkId, teamName := owner.createTeam(t, "handed-team")
		adminMember := admin.join(t, ctx, teamNetworkId, NetworkMemberRoleAdmin, false)
		owner.linkIdp(t, ctx, teamNetworkId)
		otherTeamNetworkId, _ := other.createTeam(t, "handed-other-team")
		owner.join(t, ctx, otherTeamNetworkId, NetworkMemberRoleAuditor, false)

		requireLastOwnerRefusal(t, RemoveNetworkWithStoreSnapshot(ctx, owner.networkId, &owner.userId, nil), teamNetworkId, teamName)

		// a verified AdminSet names the Admin Owner, which the server mirrors
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(mirrorConfigMemberRolesInTx(ctx, tx, teamNetworkId, map[server.Id]bridge.MemberRole{
				adminMember.MemberId: bridge.MemberRole_MEMBER_ROLE_OWNER,
			}))
		})
		if team := GetLastOwnerTeam(ctx, owner.userId, owner.networkId); team != nil {
			t.Fatalf("the check still finds the owner the last Owner of %+v", team)
		}

		removal := RemoveNetworkWithStoreSnapshot(ctx, owner.networkId, &owner.userId, nil)
		if removal.Outcome != RemoveNetworkRemoved || removal.LastOwnerTeam != nil || !removal.UserAuths[owner.userAuth] {
			t.Fatalf("removal = %+v, want removed with the owner's sign-in", removal)
		}
		if after := owner.rows(ctx); after != (lastOwnerTestRows{}) {
			t.Fatalf("the deleted owner keeps %+v", after)
		}
		if owners := lastOwnerTestOwners(ctx, teamNetworkId); !slices.Equal(owners, []server.Id{admin.userId}) {
			t.Fatalf("the team's Owners are %v, want the new Owner alone", owners)
		}
		if GetNetworkTeam(ctx, teamNetworkId) == nil || GetNetworkTeam(ctx, otherTeamNetworkId) == nil {
			t.Fatal("a team went with the deleted owner")
		}
	})
}

// An Admin deletes as before, and so does one of two Owners, leaving the other; the remaining Owner is then the last
// one.
func TestRemoveNetworkOfAnAdminOrOneOfTwoOwners(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		owner := newLastOwnerTestPerson(t, ctx, "two-owners-first")
		secondOwner := newLastOwnerTestPerson(t, ctx, "two-owners-second")
		admin := newLastOwnerTestPerson(t, ctx, "two-owners-admin")
		teamNetworkId, teamName := owner.createTeam(t, "two-owners-team")
		secondOwner.join(t, ctx, teamNetworkId, NetworkMemberRoleOwner, false)
		admin.join(t, ctx, teamNetworkId, NetworkMemberRoleAdmin, false)
		admin.linkIdp(t, ctx, teamNetworkId)

		for _, person := range []*lastOwnerTestPerson{admin, secondOwner} {
			removal := RemoveNetworkWithStoreSnapshot(ctx, person.networkId, &person.userId, nil)
			if removal.Outcome != RemoveNetworkRemoved || removal.LastOwnerTeam != nil {
				t.Fatalf("removal = %+v (team %+v), want removed", removal, removal.LastOwnerTeam)
			}
			if after := person.rows(ctx); after != (lastOwnerTestRows{}) {
				t.Fatalf("the deleted user keeps %+v", after)
			}
		}
		if owners := lastOwnerTestOwners(ctx, teamNetworkId); !slices.Equal(owners, []server.Id{owner.userId}) {
			t.Fatalf("the team's Owners are %v", owners)
		}
		requireLastOwnerRefusal(t, RemoveNetworkWithStoreSnapshot(ctx, owner.networkId, &owner.userId, nil), teamNetworkId, teamName)
	})
}

// A deletion that reaches no team is the deletion it was: a personal network with no membership, and a team network
// deleted with its owner of record, even where that synthetic user holds the team's only Owner row, since a team
// that goes with the deletion needs no Owner.
func TestRemoveNetworkWithNoTeamToKeepIsUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		person := newLastOwnerTestPerson(t, ctx, "no-team")
		if team := GetLastOwnerTeam(ctx, person.userId, person.networkId); team != nil {
			t.Fatalf("a user of no team is the last Owner of %+v", team)
		}
		removal := RemoveNetworkWithStoreSnapshot(ctx, person.networkId, &person.userId, nil)
		if removal.Outcome != RemoveNetworkRemoved || removal.LastOwnerTeam != nil || !slices.Equal(mapsKeys(removal.UserAuths), []string{person.userAuth}) {
			t.Fatalf("removal = %+v, want removed with the user's sign-in", removal)
		}
		if after := person.rows(ctx); after != (lastOwnerTestRows{}) {
			t.Fatalf("the deleted user keeps %+v", after)
		}

		owner := newLastOwnerTestPerson(t, ctx, "team-itself")
		teamNetworkId, _ := owner.createTeam(t, "team-itself")
		ownerOfRecord := *testNetworkAdminUserId(ctx, teamNetworkId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_member SET role = $2 WHERE network_id = $1`, teamNetworkId, string(NetworkMemberRoleAdmin)))
			_, err := AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{NetworkId: teamNetworkId, UserId: ownerOfRecord, Role: NetworkMemberRoleOwner})
			server.Raise(err)
		})
		if removal := RemoveNetworkWithStoreSnapshot(ctx, teamNetworkId, nil, nil); removal.Outcome != RemoveNetworkRemoved {
			t.Fatalf("the team's own deletion = %+v, want removed", removal)
		}
		if GetNetworkTeam(ctx, teamNetworkId) != nil {
			t.Fatal("the team outlived its deletion")
		}
	})
}

// The sorted keys of a set of strings.
func mapsKeys(set map[string]bool) []string {
	keys := []string{}
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// Only live Owners keep a team: a pending Owner (§7.6's JIT state) reaches nothing, so beside it the live Owner is
// still the last, and a pending Owner's own deletion leaves the team's live Owners as they were.
func TestRemoveNetworkCountsOnlyLiveOwners(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		owner := newLastOwnerTestPerson(t, ctx, "live-owner")
		pendingOwner := newLastOwnerTestPerson(t, ctx, "pending-owner")
		teamNetworkId, teamName := owner.createTeam(t, "live-owner-team")
		pendingOwner.join(t, ctx, teamNetworkId, NetworkMemberRoleOwner, true)

		requireLastOwnerRefusal(t, RemoveNetworkWithStoreSnapshot(ctx, owner.networkId, &owner.userId, nil), teamNetworkId, teamName)
		if removal := RemoveNetworkWithStoreSnapshot(ctx, pendingOwner.networkId, &pendingOwner.userId, nil); removal.Outcome != RemoveNetworkRemoved {
			t.Fatalf("the pending Owner's removal = %+v, want removed", removal)
		}
		if owners := lastOwnerTestOwners(ctx, teamNetworkId); !slices.Equal(owners, []server.Id{owner.userId}) {
			t.Fatalf("the team's Owners are %v", owners)
		}
	})
}

// A team's two Owners delete their accounts at once. The first, holding the team's Owners, is held at its first
// delete while the second waits on its locks; once the first commits, the second finds itself the last Owner and
// refuses, so the team keeps one Owner. Without the locks the second would pass the check on its own snapshot and
// leave the team ownerless.
func TestRemoveNetworkByTwoOwnersAtOnceLeavesOne(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		first := newLastOwnerTestPerson(t, ctx, "race-first")
		second := newLastOwnerTestPerson(t, ctx, "race-second")
		teamNetworkId, teamName := first.createTeam(t, "race-team")
		second.join(t, ctx, teamNetworkId, NetworkMemberRoleOwner, false)

		// the holder keeps the first owner's user row, which its deletion
		// deletes first, after its last Owner check
		holderPids := make(chan int32, 1)
		releaseHolder := make(chan struct{})
		holderDone := runPaymentModelTest(func() error {
			var returnErr error
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SELECT 1 FROM network_user WHERE user_id = $1 FOR UPDATE`, first.userId))
				holderPids <- lastOwnerTestBackendPid(ctx, tx)
				select {
				case <-releaseHolder:
				case <-ctx.Done():
					returnErr = ctx.Err()
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			return returnErr
		})
		var holderPid int32
		select {
		case holderPid = <-holderPids:
		case <-ctx.Done():
			t.Fatal("the holder did not lock")
		}

		var firstRemoval, secondRemoval *RemoveNetworkResult
		firstDone := runPaymentModelTest(func() error {
			firstRemoval = RemoveNetworkWithStoreSnapshot(ctx, first.networkId, &first.userId, nil)
			return nil
		})
		firstPid, waitErr := waitForLastOwnerLockWaiter(ctx, holderPid)
		var secondDone <-chan error
		if waitErr == nil {
			secondDone = runPaymentModelTest(func() error {
				secondRemoval = RemoveNetworkWithStoreSnapshot(ctx, second.networkId, &second.userId, nil)
				return nil
			})
			// the second waits on the Owners the first holds
			_, waitErr = waitForLastOwnerLockWaiter(ctx, firstPid)
		}
		if waitErr != nil {
			cancel()
		}
		close(releaseHolder)
		holderErr := awaitPaymentModelTest(holderDone)
		firstErr := awaitPaymentModelTest(firstDone)
		var secondErr error
		if secondDone != nil {
			secondErr = awaitPaymentModelTest(secondDone)
		}
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		if holderErr != nil || firstErr != nil || secondErr != nil {
			t.Fatalf("holder %v, first deletion %v, second deletion %v", holderErr, firstErr, secondErr)
		}

		if firstRemoval.Outcome != RemoveNetworkRemoved {
			t.Fatalf("the first deletion = %+v, want removed", firstRemoval)
		}
		requireLastOwnerRefusal(t, secondRemoval, teamNetworkId, teamName)
		if owners := lastOwnerTestOwners(ctx, teamNetworkId); !slices.Equal(owners, []server.Id{second.userId}) {
			t.Fatalf("the team's Owners are %v, want the second owner alone", owners)
		}
		if after := second.rows(ctx); after.network != 1 || after.user != 1 || after.memberships != 1 {
			t.Fatalf("the refused owner keeps %+v", after)
		}
	})
}

// A role change racing the deletion: the log demotes the team's other Owner in a transaction still open, the
// deletion waits on that Owner's row, and once the demotion commits it reads the row's new role, finds its user the
// last Owner and refuses. Without the lock it would pass on the Owner its snapshot still saw.
func TestRemoveNetworkWaitsForAnOwnersRoleChange(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		owner := newLastOwnerTestPerson(t, ctx, "demote-owner")
		otherOwner := newLastOwnerTestPerson(t, ctx, "demote-other")
		teamNetworkId, teamName := owner.createTeam(t, "demote-team")
		otherMember := otherOwner.join(t, ctx, teamNetworkId, NetworkMemberRoleOwner, false)

		mirrorPids := make(chan int32, 1)
		releaseMirror := make(chan struct{})
		mirrorDone := runPaymentModelTest(func() error {
			var returnErr error
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(mirrorConfigMemberRolesInTx(ctx, tx, teamNetworkId, map[server.Id]bridge.MemberRole{
					otherMember.MemberId: bridge.MemberRole_MEMBER_ROLE_ADMIN,
				}))
				mirrorPids <- lastOwnerTestBackendPid(ctx, tx)
				select {
				case <-releaseMirror:
				case <-ctx.Done():
					returnErr = ctx.Err()
					server.Raise(returnErr)
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			return returnErr
		})
		var mirrorPid int32
		select {
		case mirrorPid = <-mirrorPids:
		case <-ctx.Done():
			t.Fatal("the demotion did not write")
		}

		var removal *RemoveNetworkResult
		removeDone := runPaymentModelTest(func() error {
			removal = RemoveNetworkWithStoreSnapshot(ctx, owner.networkId, &owner.userId, nil)
			return nil
		})
		_, waitErr := waitForLastOwnerLockWaiter(ctx, mirrorPid)
		if waitErr != nil {
			cancel()
		}
		close(releaseMirror)
		mirrorErr := awaitPaymentModelTest(mirrorDone)
		removeErr := awaitPaymentModelTest(removeDone)
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		if mirrorErr != nil || removeErr != nil {
			t.Fatalf("demotion %v, deletion %v", mirrorErr, removeErr)
		}

		requireLastOwnerRefusal(t, removal, teamNetworkId, teamName)
		if owners := lastOwnerTestOwners(ctx, teamNetworkId); !slices.Equal(owners, []server.Id{owner.userId}) {
			t.Fatalf("the team's Owners are %v, want the deleting owner alone", owners)
		}
		if after := owner.rows(ctx); after.network != 1 || after.memberships != 1 {
			t.Fatalf("the refused owner keeps %+v", after)
		}
	})
}
