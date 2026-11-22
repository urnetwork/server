//go:build sidecar_pending

package model

// A team's tagged and ephemeral enrollments (§7.5, §7.7, §7.8; TEAM-M1-04,
// TEAM-M6-02), on Postgres with MG0, MG1 and MG6. A team's admin_user_id is a
// synthetic owner of record that never signs in, so a credential that names a
// key's creator without its member_id is refused by the state check in a team.
// Every enrollment there runs on the creator's live membership, the
// credential §7.5 puts member_id in, whatever the key is; a key whose creator
// holds no live membership enrolls nothing and answers 401 auth_key_invalid,
// never 500. An untagged client's token, ephemeral or not, names that
// membership. A tagged client is the network's (owner decision 83), so its
// token names the team's owner of record with no member_id (QD-1194), which
// the state check takes only as a tagged lineage's client credential, and it
// outlives its sponsor's membership. A personal network's enrollment is
// unchanged.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A tag id as POST /network/auth-keys takes it: 32 lowercase hex characters.
const teamEnrollTagHex = "00112233445566778899aabbccddeeff"

// A team of one creator member, in sidecar.yml's preview_networks, whose log
// holds base 1.
type teamEnrollFixture struct {
	ctx             context.Context
	networkId       server.Id
	ownerOfRecordId server.Id
	creatorUserId   server.Id
	creator         *NetworkMember
	creatorSession  *session.ClientSession
}

func newTeamEnrollFixture(t testing.TB, role NetworkMemberRole) *teamEnrollFixture {
	t.Helper()
	ctx := t.Context()
	networkId, ownerOfRecordId := newTestSignInTeam(t, ctx, false)
	creatorUserId := server.NewId()
	// the creator's own account, its personal network beside the team
	Testing_CreateNetwork(ctx, server.NewId(), "creator-"+creatorUserId.String(), creatorUserId)
	creator := addTestSignInMember(t, ctx, networkId, creatorUserId, role, false)
	signIn := ResolveNetworkSignIn(ctx, creatorUserId, networkId, server.NowUtc())
	if signIn == nil || signIn.MemberId == nil || *signIn.MemberId != creator.MemberId {
		t.Fatalf("the creator's sign-in: %+v", signIn)
	}
	creatorSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", NewNetworkSignInByJwt(signIn, false))
	t.Cleanup(creatorSession.Cancel)
	t.Cleanup(server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", networkId)))
	t.Cleanup(Testing_SetNetworkEnrollHasGenesis(func(ctx context.Context, networkId server.Id) (bool, error) {
		return true, nil
	}))
	return &teamEnrollFixture{
		ctx:             ctx,
		networkId:       networkId,
		ownerOfRecordId: ownerOfRecordId,
		creatorUserId:   creatorUserId,
		creator:         creator,
		creatorSession:  creatorSession,
	}
}

// A new auth key the session's user creates, its bearer and id.
func newTeamEnrollAuthKey(t testing.TB, creatorSession *session.ClientSession, args *CreateAuthKeyArgs) (bearer string, authKeyId server.Id) {
	t.Helper()
	if args.DurationSeconds == 0 {
		args.DurationSeconds = 3600
	}
	if args.MaxUses == 0 {
		args.MaxUses = 10
	}
	result, err := CreateAuthKey(args, creatorSession)
	if err != nil || result.Error != nil || result.AuthKeyId == nil {
		t.Fatalf("create auth key: %v %+v", err, result)
	}
	return result.AuthKey, *result.AuthKeyId
}

func teamEnrollArgs(t testing.TB, bearer string) *NetworkEnrollArgs {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &NetworkEnrollArgs{AuthKey: bearer, ClientPublicKey: publicKey, DeviceSpec: "team sidecar"}
}

// The minted token's claims, which the state check takes as a client credential.
func requireTeamEnrollCredential(t testing.TB, ctx context.Context, result *NetworkEnrollResult) *session.ByJwt {
	t.Helper()
	byJwt, err := session.ParseByJwt(ctx, result.ByClientJwt)
	if err != nil {
		t.Fatal(err)
	}
	if byJwt.ClientId == nil || *byJwt.ClientId != result.ClientId || !byJwt.StrictExp || !byJwt.AdminRefused {
		t.Fatalf("the enrolled credential: client %v strict_exp %t admin_refused %t", byJwt.ClientId, byJwt.StrictExp, byJwt.AdminRefused)
	}
	if err := session.ValidateByJwtState(ctx, byJwt, true); err != nil {
		t.Fatalf("the state check refused the enrolled credential: %v", err)
	}
	return byJwt
}

type teamEnrollOwnerRow struct {
	ownerUserId   *server.Id
	memberId      *server.Id
	sponsorUserId *server.Id
	authKeyId     *server.Id
	tagged        bool
	ephemeral     bool
	adminRefused  bool
	strictExp     bool
}

func readTeamEnrollOwnerRow(t testing.TB, ctx context.Context, clientId server.Id) (row *teamEnrollOwnerRow) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT owner_user_id, member_id, sponsor_user_id, auth_key_id, tagged, ephemeral, admin_refused, strict_exp
				FROM network_client_owner
				WHERE client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				row = &teamEnrollOwnerRow{}
				server.Raise(result.Scan(&row.ownerUserId, &row.memberId, &row.sponsorUserId, &row.authKeyId, &row.tagged,
					&row.ephemeral, &row.adminRefused, &row.strictExp))
			}
		})
	})
	if row == nil {
		t.Fatalf("client %s has no owner row", clientId)
	}
	return
}

func readTeamEnrollKeyUseCount(t testing.TB, ctx context.Context, authKeyId server.Id) (useCount int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT use_count FROM network_auth_key WHERE auth_key_id = $1`, authKeyId).Scan(&useCount))
	})
	return
}

func countTeamEnrollClients(t testing.TB, ctx context.Context, networkId server.Id) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_client WHERE network_id = $1`, networkId).Scan(&count))
	})
	return
}

func readTeamEnrollClientActive(t testing.TB, ctx context.Context, clientId server.Id) (active bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT active FROM network_client WHERE client_id = $1`, clientId).Scan(&active))
	})
	return
}

func requireTeamEnrollRefusal(t testing.TB, err error, status int, code string) {
	t.Helper()
	refusal := &NetworkEnrollRefusal{}
	if !errors.As(err, &refusal) || refusal.Status != status || refusal.Code != code {
		t.Fatalf("refusal = %v, want %d %s", err, status, code)
	}
	if refusal.Status < 400 || 500 <= refusal.Status {
		t.Fatalf("a refused enrollment answered %d", refusal.Status)
	}
}

// The api's removal of the member (§7.10): its row, its untagged clients and
// its rev/ object, in one transaction.
func denyTeamEnrollMember(t testing.TB, ctx context.Context, networkId server.Id, memberId server.Id) *NetworkMemberDenied {
	t.Helper()
	var denied *NetworkMemberDenied
	server.Tx(ctx, func(tx server.PgTx) {
		var err error
		denied, err = DenyNetworkMemberInTx(ctx, tx, &NetworkMemberDenial{NetworkId: networkId, MemberId: memberId, Time: server.NowUtc()})
		server.Raise(err)
	})
	return denied
}

// A team's tagged enrollment runs on the key creator's membership and lands on
// the team's own identity (QD-1194): its token names the team's owner of
// record with no member_id, which the state check takes as a tagged client's
// credential, so the mint answers the client rather than 500; its owner row
// has no owner and the creator for its sponsor (§7.7); and the client's own
// token mints its window client, whose token names the owner of record too and
// validates, as a tagged sidecar's does in a personal network.
func TestTeamTaggedEnrollmentMintsOnTheOwnerOfRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTeamEnrollFixture(t, NetworkMemberRoleAdmin)
		bearer, authKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{Tags: []string{teamEnrollTagHex}})

		result, err := EnrollNetworkClient(self.ctx, teamEnrollArgs(t, bearer), "192.0.2.9:1")
		if err != nil {
			t.Fatalf("the team's tagged enrollment: %v", err)
		}
		connect.AssertEqual(t, result.AdminRefused, true)
		connect.AssertEqual(t, result.ApprovalState, NetworkEnrollApprovalStatePending)
		connect.AssertEqual(t, result.Close, "")
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, authKeyId), 1)

		byJwt := requireTeamEnrollCredential(t, self.ctx, result)
		connect.AssertEqual(t, byJwt.NetworkId, self.networkId)
		connect.AssertEqual(t, byJwt.UserId, self.ownerOfRecordId)
		if byJwt.MemberId != nil {
			t.Fatalf("the tagged credential names member %s, not the team's own identity", *byJwt.MemberId)
		}
		connect.AssertEqual(t, byJwt.RootClientId != nil && *byJwt.RootClientId == result.ClientId, true)

		row := readTeamEnrollOwnerRow(t, self.ctx, result.ClientId)
		connect.AssertEqual(t, row.tagged, true)
		connect.AssertEqual(t, row.ephemeral, false)
		connect.AssertEqual(t, row.ownerUserId == nil && row.memberId == nil, true)
		connect.AssertEqual(t, row.sponsorUserId != nil && *row.sponsorUserId == self.creatorUserId, true)
		connect.AssertEqual(t, row.authKeyId != nil && *row.authKeyId == authKeyId, true)
		connect.AssertEqual(t, row.adminRefused && row.strictExp, true)
		taggedOrEphemeral, err := ClientTaggedOrEphemeral(self.ctx, result.ClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, taggedOrEphemeral, true)
		connect.AssertEqual(t, ClientRefusesAdmin(self.ctx, self.networkId, result.ClientId), true)

		// the sidecar's own token at api: its window client, which inherits its marks
		enrolledSession := session.NewLocalClientSession(self.ctx, "192.0.2.9:1", byJwt)
		defer enrolledSession.Cancel()
		child, err := AuthNetworkClient(&AuthNetworkClientArgs{
			SourceClientId: &result.ClientId,
			Description:    "window",
			DeviceSpec:     "team sidecar",
		}, enrolledSession)
		if err != nil || child.Error != nil || child.ClientId == nil || child.ByClientJwt == nil {
			t.Fatalf("the tagged sidecar's window client: %v %+v", err, child)
		}
		connect.AssertEqual(t, child.AdminRefused && child.StrictExp, true)
		childJwt, err := session.ParseByJwt(self.ctx, *child.ByClientJwt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, childJwt.UserId, self.ownerOfRecordId)
		connect.AssertEqual(t, childJwt.MemberId == nil, true)
		connect.AssertEqual(t, childJwt.RootClientId != nil && *childJwt.RootClientId == result.ClientId, true)
		if err := session.ValidateByJwtState(self.ctx, childJwt, true); err != nil {
			t.Fatalf("the window client's credential: %v", err)
		}
	})
}

// A team's untagged ephemeral enrollment is its creator's own member device
// (§7.7, §8.4): the token and the owner row both name the creator's member_id,
// never its user id, its close capability is returned once with its lease, and
// its owner row marks it ephemeral, which the exchange reads to give it no
// member's recovery scope, its evs access being §8.6's ceiling that its
// admission sets. The member's removal ends it with the member's devices. A
// tagged ephemeral key, which a Member cannot make (§7.5's own devices), an
// Admin of the team makes: its client is tagged and ephemeral, sponsored by the
// Admin, and the network's: its token names the team's owner of record, and it
// and its credential outlive the member's removal and its sponsor's
// (QD-1194).
func TestTeamEphemeralEnrollmentIsItsCreatorsMemberDevice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTeamEnrollFixture(t, NetworkMemberRoleMember)
		bearer, _ := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{Ephemeral: true})

		result, err := EnrollNetworkClient(self.ctx, teamEnrollArgs(t, bearer), "192.0.2.9:1")
		if err != nil {
			t.Fatalf("the team's ephemeral enrollment: %v", err)
		}
		byJwt := requireTeamEnrollCredential(t, self.ctx, result)
		if byJwt.MemberId == nil || *byJwt.MemberId != self.creator.MemberId {
			t.Fatalf("the ephemeral credential names member %v, not the creator's %s", byJwt.MemberId, self.creator.MemberId)
		}
		row := readTeamEnrollOwnerRow(t, self.ctx, result.ClientId)
		connect.AssertEqual(t, row.ephemeral, true)
		connect.AssertEqual(t, row.tagged, false)
		connect.AssertEqual(t, row.ownerUserId != nil && *row.ownerUserId == self.creatorUserId, true)
		if row.memberId == nil || *row.memberId != self.creator.MemberId {
			t.Fatalf("the ephemeral client's owner row names member %v, not the creator's membership %s", row.memberId, self.creator.MemberId)
		}
		connect.AssertEqual(t, row.sponsorUserId == nil, true)
		taggedOrEphemeral, err := ClientTaggedOrEphemeral(self.ctx, result.ClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, taggedOrEphemeral, true)

		capabilityBytes, err := base64.RawURLEncoding.DecodeString(result.Close)
		connect.AssertEqual(t, err, nil)
		var stored []byte
		server.Db(self.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(self.ctx, `SELECT close_sha256 FROM network_client_lease WHERE client_id = $1`, result.ClientId).Scan(&stored))
		})
		sum := sha256.Sum256(capabilityBytes)
		connect.AssertEqual(t, bytes.Equal(stored, sum[:]), true)

		// a tagged ephemeral key, which the Member is refused and an Admin makes
		if refused, err := CreateAuthKey(&CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1, Tags: []string{teamEnrollTagHex}, Ephemeral: true}, self.creatorSession); err == nil || refused != nil {
			t.Fatalf("the Member's tagged ephemeral key: %v %+v", err, refused)
		}
		sponsorUserId := server.NewId()
		Testing_CreateNetwork(self.ctx, server.NewId(), "sponsor-"+sponsorUserId.String(), sponsorUserId)
		sponsor := addTestSignInMember(t, self.ctx, self.networkId, sponsorUserId, NetworkMemberRoleAdmin, false)
		sponsorSignIn := ResolveNetworkSignIn(self.ctx, sponsorUserId, self.networkId, server.NowUtc())
		if sponsorSignIn == nil || sponsorSignIn.MemberId == nil || *sponsorSignIn.MemberId != sponsor.MemberId {
			t.Fatalf("the sponsor's sign-in: %+v", sponsorSignIn)
		}
		sponsorSession := session.NewLocalClientSession(self.ctx, "192.0.2.1:1", NewNetworkSignInByJwt(sponsorSignIn, false))
		defer sponsorSession.Cancel()
		taggedBearer, _ := newTeamEnrollAuthKey(t, sponsorSession, &CreateAuthKeyArgs{Tags: []string{teamEnrollTagHex}, Ephemeral: true})
		tagged, err := EnrollNetworkClient(self.ctx, teamEnrollArgs(t, taggedBearer), "192.0.2.9:1")
		if err != nil {
			t.Fatalf("the team's tagged ephemeral enrollment: %v", err)
		}
		taggedJwt := requireTeamEnrollCredential(t, self.ctx, tagged)
		connect.AssertEqual(t, taggedJwt.UserId, self.ownerOfRecordId)
		connect.AssertEqual(t, taggedJwt.MemberId == nil, true)
		taggedRow := readTeamEnrollOwnerRow(t, self.ctx, tagged.ClientId)
		connect.AssertEqual(t, taggedRow.tagged && taggedRow.ephemeral, true)
		connect.AssertEqual(t, taggedRow.ownerUserId == nil && taggedRow.memberId == nil, true)
		connect.AssertEqual(t, taggedRow.sponsorUserId != nil && *taggedRow.sponsorUserId == sponsorUserId, true)
		connect.AssertEqual(t, tagged.Close != "", true)

		// the member's removal deactivates its untagged ephemeral device, and its
		// credential with it
		denied := denyTeamEnrollMember(t, self.ctx, self.networkId, self.creator.MemberId)
		connect.AssertEqual(t, denied.DeactivatedClientCount, int64(1))
		connect.AssertEqual(t, readTeamEnrollClientActive(t, self.ctx, result.ClientId), false)
		if err := session.ValidateByJwtState(self.ctx, byJwt, true); err == nil {
			t.Fatal("the removed member's ephemeral device's credential still validates")
		}
		// the tagged agent's client stays past its sponsor's removal too (owner
		// decision 83), and so does its credential
		denyTeamEnrollMember(t, self.ctx, self.networkId, sponsor.MemberId)
		connect.AssertEqual(t, readTeamEnrollClientActive(t, self.ctx, tagged.ClientId), true)
		if err := session.ValidateByJwtState(self.ctx, taggedJwt, true); err != nil {
			t.Fatalf("the tagged ephemeral agent's credential past its sponsor's removal: %v", err)
		}
	})
}

// A key whose creator holds no live membership in the team enrolls nothing and
// answers 401 auth_key_invalid, never 500, spending nothing: a removed
// creator's tagged and ephemeral keys, a creator past its reauth_deadline, and
// a creator removed after its sign-in was read and before the mint's
// transaction, where the mint's own state check refuses.
func TestTeamEnrollmentOfAKeyWhoseCreatorLeftIsRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTeamEnrollFixture(t, NetworkMemberRoleAdmin)
		taggedBearer, taggedKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{Tags: []string{teamEnrollTagHex}})
		ephemeralBearer, ephemeralKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{Ephemeral: true})

		// the creator past its reauth_deadline signs in to nothing
		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(self.ctx, `UPDATE network_member SET reauth_deadline = $2 WHERE member_id = $1`, self.creator.MemberId, server.NowUtc().Add(-time.Minute)))
		})
		_, err := EnrollNetworkClient(self.ctx, teamEnrollArgs(t, taggedBearer), "192.0.2.9:1")
		requireTeamEnrollRefusal(t, err, http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid)
		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(self.ctx, `UPDATE network_member SET reauth_deadline = NULL WHERE member_id = $1`, self.creator.MemberId))
		})

		// removed between its sign-in and the mint: the mint's state check refuses
		removed := false
		t.Cleanup(func() { Testing_EnrollAfterSignIn = nil })
		Testing_EnrollAfterSignIn = func() {
			if !removed {
				removed = true
				denyTeamEnrollMember(t, self.ctx, self.networkId, self.creator.MemberId)
			}
		}
		_, err = EnrollNetworkClient(self.ctx, teamEnrollArgs(t, taggedBearer), "192.0.2.9:1")
		requireTeamEnrollRefusal(t, err, http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid)
		connect.AssertEqual(t, removed, true)
		Testing_EnrollAfterSignIn = nil

		// removed: each of its keys is refused before the mint
		for _, bearer := range []string{taggedBearer, ephemeralBearer} {
			_, err := EnrollNetworkClient(self.ctx, teamEnrollArgs(t, bearer), "192.0.2.9:1")
			requireTeamEnrollRefusal(t, err, http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid)
		}
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, taggedKeyId), 0)
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, ephemeralKeyId), 0)
		connect.AssertEqual(t, countTeamEnrollClients(t, self.ctx, self.networkId), 0)
	})
}

// A window client the token mints, its credential parsed.
func mintTeamEnrollWindowClient(t testing.TB, enrolledSession *session.ClientSession, sourceClientId server.Id) *session.ByJwt {
	t.Helper()
	child, err := AuthNetworkClient(&AuthNetworkClientArgs{
		SourceClientId: &sourceClientId,
		Description:    "window",
		DeviceSpec:     "team sidecar",
	}, enrolledSession)
	if err != nil || child.Error != nil || child.ByClientJwt == nil {
		t.Fatalf("the tagged sidecar's window client: %v %+v", err, child)
	}
	childJwt, err := session.ParseByJwt(enrolledSession.Ctx, *child.ByClientJwt)
	if err != nil {
		t.Fatal(err)
	}
	return childJwt
}

// A team's tagged agent outlives its sponsor's membership as a client and at
// api (owner decision 83; QD-1194): once the sponsor is removed, api keeps the
// client and its window client active, and its credential, which names the
// team's owner of record, still passes the state check, refreshes, and mints a
// window client, on SQL and on the revocation record alike, as a window
// client minted before the removal still does. The client's removal ends every
// one of them.
func TestTeamTaggedCredentialOutlivesItsSponsor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTeamEnrollFixture(t, NetworkMemberRoleAdmin)
		bearer, _ := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{Tags: []string{teamEnrollTagHex}})
		result, err := EnrollNetworkClient(self.ctx, teamEnrollArgs(t, bearer), "192.0.2.9:1")
		if err != nil {
			t.Fatal(err)
		}
		byJwt := requireTeamEnrollCredential(t, self.ctx, result)
		enrolledSession := session.NewLocalClientSession(self.ctx, "192.0.2.9:1", byJwt)
		defer enrolledSession.Cancel()
		windowBefore := mintTeamEnrollWindowClient(t, enrolledSession, result.ClientId)

		denied := denyTeamEnrollMember(t, self.ctx, self.networkId, self.creator.MemberId)
		connect.AssertEqual(t, denied.DeactivatedClientCount, int64(0))
		connect.AssertEqual(t, readTeamEnrollClientActive(t, self.ctx, result.ClientId), true)
		connect.AssertEqual(t, readTeamEnrollClientActive(t, self.ctx, *windowBefore.ClientId), true)
		if err := session.ValidateByJwtState(self.ctx, byJwt, true); err != nil {
			t.Fatalf("the tagged credential past its sponsor's removal: %v", err)
		}
		if err := session.ValidateByJwtState(self.ctx, windowBefore, true); err != nil {
			t.Fatalf("its window client's credential past its sponsor's removal: %v", err)
		}

		// the refresh, whose token names the owner of record again
		signed, err := session.RenewSessionCredential(enrolledSession, "team", false)
		if err != nil {
			t.Fatalf("the tagged credential's refresh past its sponsor's removal: %v", err)
		}
		renewed, err := session.ParseByJwt(self.ctx, signed)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, renewed.UserId, self.ownerOfRecordId)
		connect.AssertEqual(t, renewed.MemberId == nil, true)
		if err := session.ValidateByJwtState(self.ctx, renewed, true); err != nil {
			t.Fatalf("the renewed credential: %v", err)
		}
		// auth-client, a window client of its own
		windowAfter := mintTeamEnrollWindowClient(t, enrolledSession, result.ClientId)
		if err := session.ValidateByJwtState(self.ctx, windowAfter, true); err != nil {
			t.Fatalf("a window client minted past its sponsor's removal: %v", err)
		}

		// the revocation record answers the same, its first check the state query's
		func() {
			defer session.Testing_SetRevocationRecordEnabled(true)()
			for attempt := range 2 {
				for _, credential := range []*session.ByJwt{byJwt, renewed, windowBefore, windowAfter} {
					if err := session.ValidateByJwtState(self.ctx, credential, true); err != nil {
						t.Fatalf("check %d on the revocation record: %v", attempt, err)
					}
				}
			}
		}()

		// the client's removal ends its credentials and its window clients'
		removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: result.ClientId}, enrolledSession)
		if err != nil || removed.Error != nil {
			t.Fatalf("the tagged client's removal: %v %+v", err, removed)
		}
		for _, recordEnabled := range []bool{false, true} {
			func() {
				defer session.Testing_SetRevocationRecordEnabled(recordEnabled)()
				for _, credential := range []*session.ByJwt{byJwt, renewed, windowBefore, windowAfter} {
					if err := session.ValidateByJwtState(self.ctx, credential, true); err == nil {
						t.Fatalf("a removed tagged lineage's credential validates (record %t)", recordEnabled)
					}
				}
			}()
		}
	})
}

// A personal network's tagged and ephemeral enrollments are what they were:
// the token names the owner with no member_id, the tagged client's owner row
// has the owner for its sponsor, and the ephemeral client's names the owner's
// user id as its member.
func TestPersonalTaggedAndEphemeralEnrollmentsAreUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, ownerUserId := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, networkId, "personal-"+networkId.String(), ownerUserId)
		ownerSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", session.NewByJwt(networkId, ownerUserId, "personal-"+networkId.String(), false, false))
		defer ownerSession.Cancel()
		t.Cleanup(server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", networkId)))
		t.Cleanup(Testing_SetNetworkEnrollHasGenesis(func(ctx context.Context, networkId server.Id) (bool, error) {
			return true, nil
		}))

		taggedBearer, _ := newTeamEnrollAuthKey(t, ownerSession, &CreateAuthKeyArgs{Tags: []string{teamEnrollTagHex}})
		tagged, err := EnrollNetworkClient(ctx, teamEnrollArgs(t, taggedBearer), "192.0.2.9:1")
		if err != nil {
			t.Fatal(err)
		}
		taggedJwt := requireTeamEnrollCredential(t, ctx, tagged)
		connect.AssertEqual(t, taggedJwt.MemberId == nil, true)
		connect.AssertEqual(t, taggedJwt.UserId, ownerUserId)
		taggedRow := readTeamEnrollOwnerRow(t, ctx, tagged.ClientId)
		connect.AssertEqual(t, taggedRow.tagged, true)
		connect.AssertEqual(t, taggedRow.ownerUserId == nil && taggedRow.memberId == nil, true)
		connect.AssertEqual(t, taggedRow.sponsorUserId != nil && *taggedRow.sponsorUserId == ownerUserId, true)

		ephemeralBearer, _ := newTeamEnrollAuthKey(t, ownerSession, &CreateAuthKeyArgs{Ephemeral: true})
		ephemeral, err := EnrollNetworkClient(ctx, teamEnrollArgs(t, ephemeralBearer), "192.0.2.9:1")
		if err != nil {
			t.Fatal(err)
		}
		ephemeralJwt := requireTeamEnrollCredential(t, ctx, ephemeral)
		connect.AssertEqual(t, ephemeralJwt.MemberId == nil, true)
		connect.AssertEqual(t, ephemeralJwt.UserId, ownerUserId)
		ephemeralRow := readTeamEnrollOwnerRow(t, ctx, ephemeral.ClientId)
		connect.AssertEqual(t, ephemeralRow.ephemeral && !ephemeralRow.tagged, true)
		connect.AssertEqual(t, ephemeralRow.ownerUserId != nil && *ephemeralRow.ownerUserId == ownerUserId, true)
		connect.AssertEqual(t, ephemeralRow.memberId != nil && *ephemeralRow.memberId == ownerUserId, true)
	})
}
