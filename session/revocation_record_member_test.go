//go:build sidecar_pending

package session

// A team member's tagged credentials on the Redis revocation record (owner
// decision 561, QD-1013), on Postgres with MG6: the SQL fallback projects a
// joined, admitted membership as m:<member_id> and its user's cutoff as
// u:<user_id>, so api's record path and sockets' Client-class verifier answer
// the member's tokens with no SQL, and a removal, a membership of another
// user, a passed reauth deadline and a sign-out-all refuse them from the
// record alone. A pending member is never projected, a reauth deadline's move
// drops the projection, and a removal racing a projection keeps its tombstone.

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A member of a fresh team, with an active top-level client of the team and
// that client's tagged credential.
type testingRecordMember struct {
	team     *testMemberTeam
	userId   server.Id
	memberId server.Id
	client   *ByJwt
}

func newTestingRecordMember(t testing.TB, ctx context.Context, pending bool, reauthDeadline *time.Time) *testingRecordMember {
	t.Helper()
	team := newTestMemberTeam(t, ctx)
	userId, _ := newTestMemberUser(t, ctx)
	memberId := team.join(t, ctx, userId, pending, reauthDeadline)
	return &testingRecordMember{
		team:     team,
		userId:   userId,
		memberId: memberId,
		client:   team.memberClient(t, ctx, userId, memberId),
	}
}

// A new active top-level client of the team and the member's tagged
// credential for it, whose root claim is the client itself.
func (self *testMemberTeam) memberClient(t testing.TB, ctx context.Context, userId server.Id, memberId server.Id) *ByJwt {
	t.Helper()
	clientId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id,device_id,description,create_time,auth_time,active) VALUES($1,$2,$3,'synthetic',$4,$4,true)`, clientId, self.networkId, self.deviceId, server.NowUtc()))
	})
	return self.credential(userId, memberId).Client(self.deviceId, clientId)
}

// Ends the membership as TEAM's writers do: its tombstone in every record of
// the network, then its row, in one transaction.
func (self *testingRecordMember) remove(t testing.TB, ctx context.Context) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SELECT member_id FROM network_member WHERE member_id = $1 FOR UPDATE`, self.memberId))
		server.Raise(RevokeMemberRecords(ctx, map[server.Id][]server.Id{self.team.networkId: {self.memberId}}))
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_member WHERE member_id = $1`, self.memberId))
	})
}

// Moves the membership's reauth deadline as an IdP sign-in does: its
// projection dropped from every record of the network, then the row.
func (self *testingRecordMember) moveReauthDeadline(t testing.TB, ctx context.Context, reauthDeadline time.Time) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(DropMemberRecordProjections(ctx, self.team.networkId, []server.Id{self.memberId}))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_member SET reauth_deadline = $2 WHERE member_id = $1`, self.memberId, reauthDeadline))
	})
}

// The record key of the member's client credential.
func (self *testingRecordMember) key() string {
	return revocationRecordKey(DefaultRevocationRecordSettings(), self.team.networkId, *self.client.RootClientId)
}

// Each shard of the network whose field differs from want: its value, or
// "absent".
func testingRevocationShardsDiffer(ctx context.Context, t testing.TB, networkId server.Id, field string, want string) []string {
	t.Helper()
	differ := []string{}
	for shard := range DefaultRevocationRecordSettings().ShardCount {
		value, ok := testingRevocationField(ctx, t, revocationRecordShardKey(networkId, shard), field)
		if !ok {
			value = "absent"
		}
		if value != want {
			differ = append(differ, strconv.Itoa(shard)+"="+value)
		}
	}
	return differ
}

// Runs check and fails the test unless it is refused as inactive with no state
// query, the record's own answer.
func testingRecordRefusesWithoutQuery(t testing.TB, name string, check func() error) {
	t.Helper()
	before := testingAllClientStateQueries()
	if err := check(); !errors.Is(err, ErrByJwtInactive) {
		t.Errorf("%s: want refused as inactive, got %v", name, err)
	}
	if queries := testingAllClientStateQueries() - before; queries != 0 {
		t.Errorf("%s: the record's refusal ran %.0f state queries", name, queries)
	}
}

// A projected member's tokens pass with no SQL: one state query projects the
// membership and its user's cutoff into the client's record, api's record path
// then answers with none, and sockets' verifier passes the token with every
// Postgres acquisition refused. A member's network credential, keyed by its
// session, projects the same way.
func TestRevocationRecordAnswersAProjectedMemberWithNoSql(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := WithStateQuerySource(t.Context(), StateQueryConnectH1)
		deadline := server.NowUtc().Add(time.Hour)
		member := newTestingRecordMember(t, ctx, false, &deadline)

		before := testingAllClientStateQueries()
		for attempt := range 3 {
			if err := ValidateByJwtState(ctx, member.client, true); err != nil {
				t.Fatalf("check %d refused the member's client: %v", attempt, err)
			}
		}
		if queries := testingAllClientStateQueries() - before; queries != 1 {
			t.Errorf("three checks ran %.0f state queries, want only the first", queries)
		}
		var reauthDeadline time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT reauth_deadline FROM network_member WHERE member_id = $1`, member.memberId).Scan(&reauthDeadline))
		})
		if value, _ := testingRevocationField(ctx, t, member.key(), revocationMemberField(member.memberId)); value != revocationMembershipValue(member.userId, &reauthDeadline) {
			t.Errorf("the membership projected as %q", value)
		}
		if value, ok := testingRevocationField(ctx, t, member.key(), revocationUserField(member.userId)); !ok || value != "0" {
			t.Errorf("the member user's cutoff projected as %q (present %v), want the epoch's 0", value, ok)
		}
		// a member credential writes neither the network's admin nor its cutoff
		for _, field := range []string{revocationFieldUser, revocationFieldCredential} {
			if value, ok := testingRevocationField(ctx, t, member.key(), field); ok {
				t.Errorf("a member's projection wrote %s = %q", field, value)
			}
		}
		// both projected member fields lapse with the positive ttl
		var fieldTtls []int64
		err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			var err error
			fieldTtls, err = r.HPTTL(ctx, member.key(), revocationMemberField(member.memberId), revocationUserField(member.userId)).Result()
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, fieldTtl := range fieldTtls {
			if fieldTtl <= 0 || DefaultRevocationRecordSettings().PositiveTtl.Milliseconds() < fieldTtl {
				t.Errorf("projected member field ttls %v, want at most %s", fieldTtls, DefaultRevocationRecordSettings().PositiveTtl)
			}
		}

		test := newClientTokenTestVerifier(t, nil)
		byJwt, err := test.verifyWithoutSql(t, ctx, member.client.Testing_Sign())
		if err != nil {
			t.Fatalf("sockets refused a projected member: %v", err)
		}
		if byJwt.MemberId == nil || *byJwt.MemberId != member.memberId {
			t.Fatalf("claims %+v", byJwt)
		}

		account := member.team.credential(member.userId, member.memberId)
		sessionId := server.NewId()
		account.SessionId = &sessionId
		accountBefore := testingStateQueries(StateQueryConnectH1, false)
		for attempt := range 2 {
			if err := ValidateByJwtState(ctx, account, false); err != nil {
				t.Fatalf("check %d refused the member's network credential: %v", attempt, err)
			}
		}
		if queries := testingStateQueries(StateQueryConnectH1, false) - accountBefore; queries != 1 {
			t.Errorf("two network credential checks ran %.0f state queries, want only the first", queries)
		}
	})
}

// A removal tombstones the membership in every record of the network before
// it commits: api's record path and sockets both refuse the member's token
// with no SQL, sockets within the grace of its last valid check as well.
func TestRevocationRecordRefusesARemovedMember(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		member := newTestingRecordMember(t, ctx, false, nil)
		if err := ValidateByJwtState(ctx, member.client, true); err != nil {
			t.Fatal(err)
		}
		test := newClientTokenTestVerifier(t, nil)
		if _, err := test.verifyWithoutSql(t, ctx, member.client.Testing_Sign()); err != nil {
			t.Fatal(err)
		}

		member.remove(t, ctx)
		if differ := testingRevocationShardsDiffer(ctx, t, member.team.networkId, revocationMemberField(member.memberId), revocationTombstone); len(differ) != 0 {
			t.Fatalf("shards that do not refuse the removed membership: %v", differ)
		}
		testingRecordRefusesWithoutQuery(t, "the removed member's client", func() error {
			return ValidateByJwtState(ctx, member.client, true)
		})
		if _, err := test.verifyWithoutSql(t, ctx, member.client.Testing_Sign()); !errors.Is(err, ErrClientTokenRefused) || !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("sockets answered a removed member: %v", err)
		}
		// within the grace, with the member user's cutoff lapsed too, the
		// tombstone still refuses
		testingLapseRevocationField(ctx, t, member.client, revocationUserField(member.userId))
		if _, err := test.verifyWithoutSql(t, ctx, member.client.Testing_Sign()); !errors.Is(err, ErrClientTokenRefused) {
			t.Fatalf("sockets answered a removed member in the grace: %v", err)
		}
	})
}

// A membership names its user: a token naming the member_id of another user's
// membership is refused from the record, by api and by sockets.
func TestRevocationRecordRefusesAMembershipOfAnotherUser(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		member := newTestingRecordMember(t, ctx, false, nil)
		if err := ValidateByJwtState(ctx, member.client, true); err != nil {
			t.Fatal(err)
		}
		otherUserId, _ := newTestMemberUser(t, ctx)
		other := *member.client
		other.UserId = otherUserId
		other.Subject = otherUserId.String()
		testingRecordRefusesWithoutQuery(t, "another user's membership", func() error {
			return ValidateByJwtState(ctx, &other, true)
		})
		test := newClientTokenTestVerifier(t, nil)
		if _, err := test.verifyWithoutSql(t, ctx, other.Testing_Sign()); !errors.Is(err, ErrClientTokenRefused) {
			t.Fatalf("sockets answered another user's membership: %v", err)
		}
		// the member itself still passes
		if _, err := test.verifyWithoutSql(t, ctx, member.client.Testing_Sign()); err != nil {
			t.Fatal(err)
		}
	})
}

// A projected membership's reauth deadline refuses once it passes: sockets at
// its clock, and api's record path once the deadline the record holds is
// past, each with no SQL.
func TestRevocationRecordRefusesAMemberPastItsReauthDeadline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		deadline := server.NowUtc().Add(time.Hour)
		member := newTestingRecordMember(t, ctx, false, &deadline)
		if err := ValidateByJwtState(ctx, member.client, true); err != nil {
			t.Fatal(err)
		}
		test := newClientTokenTestVerifier(t, nil)
		test.now = deadline.Add(-time.Minute)
		if _, err := test.verifyWithoutSql(t, ctx, member.client.Testing_Sign()); err != nil {
			t.Fatalf("sockets refused a member before its deadline: %v", err)
		}
		test.now = deadline
		if _, err := test.verifyWithoutSql(t, ctx, member.client.Testing_Sign()); !errors.Is(err, ErrClientTokenRefused) || !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("sockets answered a member at its reauth deadline: %v", err)
		}

		// a projection made before an earlier deadline that has since passed
		passed := server.NowUtc().Add(-time.Minute)
		err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			return r.HSet(ctx, member.key(), revocationMemberField(member.memberId), revocationMembershipValue(member.userId, &passed)).Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		testingRecordRefusesWithoutQuery(t, "a member past the deadline its record holds", func() error {
			return ValidateByJwtState(ctx, member.client, true)
		})
	})
}

// A sign-out-all raises the member user's cutoff in every record of the team
// before it commits: a token created before it is refused with no SQL, by api
// and by sockets, and one created after passes.
func TestRevocationRecordRefusesAMemberAfterASignOutAll(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		member := newTestingRecordMember(t, ctx, false, nil)
		if err := ValidateByJwtState(ctx, member.client, true); err != nil {
			t.Fatal(err)
		}
		cutoff := member.client.CreateTime.Add(time.Second)
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(RotateCredentialRecordsInTx(ctx, tx, member.userId, cutoff))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time = $2 WHERE user_id = $1`, member.userId, cutoff))
		})
		want := strconv.FormatInt(cutoff.UnixMicro(), 10)
		if differ := testingRevocationShardsDiffer(ctx, t, member.team.networkId, revocationUserField(member.userId), want); len(differ) != 0 {
			t.Fatalf("shards without the member user's cutoff: %v", differ)
		}
		before := testingAllClientStateQueries()
		if err := ValidateByJwtState(ctx, member.client, true); !errors.Is(err, ErrByJwtInactive) {
			t.Errorf("a member token created before the sign-out-all: %v", err)
		}
		if queries := testingAllClientStateQueries() - before; queries != 0 {
			t.Errorf("the rotation's refusal ran %.0f state queries", queries)
		}
		test := newClientTokenTestVerifier(t, nil)
		if _, err := test.verifyWithoutSql(t, ctx, member.client.Testing_Sign()); !errors.Is(err, ErrClientTokenRefused) || !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("sockets answered a member token created before the sign-out-all: %v", err)
		}

		fresh := *member.client
		fresh.CreateTime = cutoff.Add(time.Millisecond)
		before = testingAllClientStateQueries()
		if err := ValidateByJwtState(ctx, &fresh, true); err != nil {
			t.Fatalf("a member token created after the sign-out-all: %v", err)
		}
		if queries := testingAllClientStateQueries() - before; queries != 0 {
			t.Errorf("a valid check after the rotation ran %.0f state queries", queries)
		}
		if _, err := test.verifyWithoutSql(t, ctx, fresh.Testing_Sign()); err != nil {
			t.Fatalf("sockets refused a member token created after the sign-out-all: %v", err)
		}
	})
}

// A JIT pending member reaches nothing and is never projected: api refuses it
// through SQL, its record holds nothing for it, and sockets answers its token
// unavailable, never valid.
func TestRevocationRecordNeverProjectsAPendingMember(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		member := newTestingRecordMember(t, ctx, true, nil)
		for attempt := range 2 {
			if err := ValidateByJwtState(ctx, member.client, true); !errors.Is(err, ErrByJwtInactive) {
				t.Fatalf("check %d answered a pending member: %v", attempt, err)
			}
		}
		for _, field := range []string{revocationMemberField(member.memberId), revocationUserField(member.userId), revocationClientField(*member.client.ClientId)} {
			if value, ok := testingRevocationField(ctx, t, member.key(), field); ok {
				t.Errorf("a pending member's record holds %s = %q", field, value)
			}
		}
		test := newClientTokenTestVerifier(t, nil)
		_, err := test.verifyWithoutSql(t, ctx, member.client.Testing_Sign())
		if retryAfter := clientTokenRetryAfter(t, err); retryAfter != DefaultClientTokenVerifierSettings().MissRetryAfter {
			t.Fatalf("sockets asked a pending member for %s", retryAfter)
		}
	})
}

// An IdP sign-in's deadline move drops the membership's projection from every
// record of the network before it commits, so the next check reads SQL and
// projects the new deadline; a removed membership's tombstone stays.
func TestRevocationRecordReauthDeadlineMoveDropsTheProjection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		deadline := server.NowUtc().Add(time.Hour)
		member := newTestingRecordMember(t, ctx, false, &deadline)
		if err := ValidateByJwtState(ctx, member.client, true); err != nil {
			t.Fatal(err)
		}
		later := deadline.Add(7 * 24 * time.Hour)
		member.moveReauthDeadline(t, ctx, later)
		if differ := testingRevocationShardsDiffer(ctx, t, member.team.networkId, revocationMemberField(member.memberId), "absent"); len(differ) != 0 {
			t.Fatalf("shards that still hold the moved membership: %v", differ)
		}
		before := testingAllClientStateQueries()
		for range 2 {
			if err := ValidateByJwtState(ctx, member.client, true); err != nil {
				t.Fatal(err)
			}
		}
		if queries := testingAllClientStateQueries() - before; queries != 1 {
			t.Errorf("two checks after the move ran %.0f state queries, want one", queries)
		}
		var reauthDeadline time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT reauth_deadline FROM network_member WHERE member_id = $1`, member.memberId).Scan(&reauthDeadline))
		})
		if value, _ := testingRevocationField(ctx, t, member.key(), revocationMemberField(member.memberId)); value != revocationMembershipValue(member.userId, &reauthDeadline) {
			t.Errorf("the moved membership projected as %q", value)
		}

		member.remove(t, ctx)
		if err := DropMemberRecordProjections(ctx, member.team.networkId, []server.Id{member.memberId}); err != nil {
			t.Fatal(err)
		}
		if differ := testingRevocationShardsDiffer(ctx, t, member.team.networkId, revocationMemberField(member.memberId), revocationTombstone); len(differ) != 0 {
			t.Fatalf("a drop removed the tombstone: %v", differ)
		}
	})
}

// A member refused for its reauth deadline is not remembered, on either state
// path: once an IdP sign-in moves the deadline on, the same credential is
// answered again rather than refused from memory.
func TestMemberRefusedForItsReauthDeadlineIsNotRemembered(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, recordEnabled := range []bool{false, true} {
			func() {
				defer Testing_SetRevocationRecordEnabled(recordEnabled)()
				ctx := t.Context()
				passed := server.NowUtc().Add(-time.Minute)
				member := newTestingRecordMember(t, ctx, false, &passed)
				for attempt := range 2 {
					if err := ValidateByJwtState(ctx, member.client, true); !errors.Is(err, ErrByJwtInactive) {
						t.Fatalf("record %v, check %d answered a member past its deadline: %v", recordEnabled, attempt, err)
					}
				}
				member.moveReauthDeadline(t, ctx, server.NowUtc().Add(time.Hour))
				if err := ValidateByJwtState(ctx, member.client, true); err != nil {
					t.Fatalf("record %v: the member after its deadline moved on: %v", recordEnabled, err)
				}
			}()
		}
	})
}

// FS case for owner decision 561: a removal racing a projection. The fallback
// reads SQL while the member is joined, then the removal writes its tombstone
// and commits, then the projection lands: it leaves the tombstone, so the
// removed member's token is refused from the record, by api and by sockets,
// from the next check on. A projection that lands after the tombstone lapsed
// was read before the removal, so the freshness bound refuses it.
func TestRevocationRecordMemberRemovalRacingAProjectionKeepsItsTombstone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		settings := DefaultRevocationRecordSettings()
		member := newTestingRecordMember(t, ctx, false, nil)

		removed := false
		raceCtx := context.WithValue(ctx, revocationRecordBeforeProjectionKey{}, func() {
			member.remove(t, ctx)
			removed = true
		})
		// the in-flight check read SQL before the removal and answers for that
		// read
		if err := ValidateByJwtState(raceCtx, member.client, true); err != nil {
			t.Fatalf("the check that read before the removal: %v", err)
		}
		if !removed {
			t.Fatal("the removal did not run between the fallback's read and its projection")
		}
		if differ := testingRevocationShardsDiffer(ctx, t, member.team.networkId, revocationMemberField(member.memberId), revocationTombstone); len(differ) != 0 {
			t.Fatalf("the projection replaced the tombstone: %v", differ)
		}
		// the rest of the projection landed, and the tombstone refuses whatever
		// else it holds
		if _, ok := testingRevocationField(ctx, t, member.key(), revocationClientField(*member.client.ClientId)); !ok {
			t.Error("the racing projection wrote no client field")
		}
		testingRecordRefusesWithoutQuery(t, "the removed member after the race", func() error {
			return ValidateByJwtState(ctx, member.client, true)
		})
		test := newClientTokenTestVerifier(t, nil)
		if _, err := test.verifyWithoutSql(t, ctx, member.client.Testing_Sign()); !errors.Is(err, ErrClientTokenRefused) || !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("sockets answered the removed member after the race: %v", err)
		}

		// the tombstone lapses; a projection read before the removal is stale
		testingLapseRevocationField(ctx, t, member.client, revocationMemberField(member.memberId))
		facts := &revocationRecordFacts{member: true, clientActive: true, deviceId: member.client.DeviceId, rootMatches: true}
		projectRevocationRecord(ctx, settings, member.client, *member.client.RootClientId, server.NowUtc().Add(-2*settings.Freshness), facts)
		if value, ok := testingRevocationField(ctx, t, member.key(), revocationMemberField(member.memberId)); ok {
			t.Fatalf("a projection read before the removal landed after its tombstone lapsed: %q", value)
		}
		// SQL refuses what the record no longer holds
		if err := ValidateByJwtState(ctx, member.client, true); !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("the removed member once its tombstone lapsed: %v", err)
		}
	})
}
