//go:build sidecar_pending

package model

// §7.6's verified domains and discovery's lookup (TEAM-M6-06), on Postgres
// with MG6, the resolver and the clock injected.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A resolver that answers from a table, an error when set, and can hold every
// lookup at a barrier.
type testDomainResolver struct {
	stateLock sync.Mutex
	txtValues map[string][]string
	err       error
	lookups   []string
	// when set, each lookup announces itself here and waits for release
	arrived chan string
	release chan struct{}
}

func newTestDomainResolver() *testDomainResolver {
	return &testDomainResolver{txtValues: map[string][]string{}}
}

func (self *testDomainResolver) set(name string, txtValues ...string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.txtValues[name] = txtValues
}

func (self *testDomainResolver) fail(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.err = err
}

func (self *testDomainResolver) lookupCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.lookups)
}

func (self *testDomainResolver) LookupTxt(ctx context.Context, name string) ([]string, error) {
	if self.arrived != nil {
		self.arrived <- name
		select {
		case <-self.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lookups = append(self.lookups, name)
	if self.err != nil {
		return nil, self.err
	}
	return self.txtValues[name], nil
}

// The domain routes over a fake clock and resolver, the tier check passed.
type testDomains struct {
	*networkDomains
	stateLock sync.Mutex
	clock     time.Time
	resolver  *testDomainResolver
}

func newTestDomains() *testDomains {
	self := &testDomains{
		clock:    time.Date(2026, time.March, 2, 9, 0, 0, 0, time.UTC),
		resolver: newTestDomainResolver(),
	}
	self.networkDomains = &networkDomains{
		settings:        DefaultNetworkDomainSettings(),
		now:             self.nowTime,
		requireTeamTier: func(ctx context.Context, networkId server.Id) error { return nil },
		resolver:        func() NetworkDomainResolver { return self.resolver },
	}
	return self
}

func (self *testDomains) nowTime() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.clock
}

func (self *testDomains) advance(d time.Duration) time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clock = self.clock.Add(d)
	return self.clock
}

// A member's network credential in the team, of the role.
func newTestTeamMemberSession(t testing.TB, ctx context.Context, networkId server.Id, role NetworkMemberRole, pending bool) *session.ClientSession {
	t.Helper()
	userId := server.NewId()
	Testing_CreateNetwork(ctx, server.NewId(), "member-"+userId.String(), userId)
	member := addTestSignInMember(t, ctx, networkId, userId, role, pending)
	byJwt := session.NewByJwt(networkId, userId, "team", false, false)
	byJwt.MemberId = &member.MemberId
	clientSession := session.NewLocalClientSession(ctx, "192.0.2.30:1", byJwt)
	t.Cleanup(clientSession.Cancel)
	return clientSession
}

// A team and its Owner's credential.
func newTestDomainTeam(t testing.TB, ctx context.Context) (networkId server.Id, ownerSession *session.ClientSession) {
	t.Helper()
	networkId, _ = newTestSignInTeam(t, ctx, false)
	ownerSession = newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleOwner, false)
	return
}

// The network's admin event actions, oldest first.
func testAdminEventActions(ctx context.Context, networkId server.Id) []string {
	actions := []string{}
	for _, event := range GetNetworkAdminEvents(ctx, networkId, 0, 0) {
		actions = append(actions, event.Action)
	}
	return actions
}

// A domain is lowercase ASCII labels with no trailing dot; an address, a
// single label, an empty label and anything outside letters, digits and inner
// hyphens are refused.
func TestNormalizeNetworkDomain(t *testing.T) {
	for domain, want := range map[string]string{
		"team.example":          "team.example",
		" Team.Example. ":       "team.example",
		"a-b.c0.example":        "a-b.c0.example",
		"xn--bcher-kva.example": "xn--bcher-kva.example",
	} {
		got, err := NormalizeNetworkDomain(domain)
		if err != nil || got != want {
			t.Errorf("%q normalized to %q %v, want %q", domain, got, err, want)
		}
	}
	for _, refused := range []string{
		"",
		"example",
		"192.0.2.7",
		"team..example",
		".team.example",
		"-team.example",
		"team-.example",
		"team_x.example",
		"team.example/path",
		"user@team.example",
		"bücher.example",
		"*.team.example",
		strings.Repeat("a", 64) + ".example",
		strings.Repeat("a.", 120) + "example",
	} {
		if got, err := NormalizeNetworkDomain(refused); err == nil {
			t.Errorf("%q passed as %q", refused, got)
		}
	}
	connect.AssertEqual(t, NetworkDomainChallengeName("team.example"), "_urnetwork-challenge.team.example")
}

// The Owner adds a domain and is answered its TXT record; the record not yet
// published leaves it unverified; published, the domain verifies; removed, it
// is gone. Each change writes its admin event, and adding and verifying each
// owe Owners and Admins the notice, which names no domain.
func TestNetworkDomainAddVerifyRemove(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, ownerSession := newTestDomainTeam(t, ctx)
		domains := newTestDomains()

		added, err := domains.add(&NetworkDomainArgs{Domain: "Team.Example."}, ownerSession)
		if err != nil || added.Error != nil || added.Domain == nil {
			t.Fatalf("add: %v %+v", err, added)
		}
		connect.AssertEqual(t, added.Domain.Domain, "team.example")
		connect.AssertEqual(t, added.Domain.TxtName, "_urnetwork-challenge.team.example")
		connect.AssertEqual(t, added.Domain.Verified, false)
		if len(added.Domain.TxtValue) != 43 {
			t.Fatalf("the challenge is %d characters", len(added.Domain.TxtValue))
		}
		// adding it again answers the same record and changes nothing
		again, err := domains.add(&NetworkDomainArgs{Domain: "team.example"}, ownerSession)
		if err != nil || again.Error != nil || again.Domain.TxtValue != added.Domain.TxtValue {
			t.Fatalf("add again: %v %+v", err, again)
		}
		connect.AssertEqual(t, len(GetNetworkVerifiedDomains(ctx, networkId)), 0)

		// no record yet
		notFound, err := domains.verify(&NetworkDomainArgs{Domain: "team.example"}, ownerSession)
		if err != nil || notFound.Error == nil || notFound.Domain == nil || notFound.Domain.Verified {
			t.Fatalf("verify with no record: %v %+v", err, notFound)
		}
		connect.AssertEqual(t, domains.resolver.lookupCount(), 1)
		// a second ask within the interval makes no lookup
		throttled, err := domains.verify(&NetworkDomainArgs{Domain: "team.example"}, ownerSession)
		if err != nil || throttled.Error == nil {
			t.Fatalf("verify again at once: %v %+v", err, throttled)
		}
		connect.AssertEqual(t, domains.resolver.lookupCount(), 1)

		// another value at the name proves nothing; the challenge among others does
		domains.advance(time.Minute)
		domains.resolver.set("_urnetwork-challenge.team.example", "v=spf1 -all", "another-value")
		wrong, err := domains.verify(&NetworkDomainArgs{Domain: "team.example"}, ownerSession)
		if err != nil || wrong.Error == nil || wrong.Domain.Verified {
			t.Fatalf("verify with another value: %v %+v", err, wrong)
		}
		verifyTime := domains.advance(time.Minute)
		domains.resolver.set("_urnetwork-challenge.team.example", "v=spf1 -all", " "+added.Domain.TxtValue+" ")
		verified, err := domains.verify(&NetworkDomainArgs{Domain: "team.example"}, ownerSession)
		if err != nil || verified.Error != nil || !verified.Domain.Verified {
			t.Fatalf("verify: %v %+v", err, verified)
		}
		if verified.Domain.VerifyTime == nil || !verified.Domain.VerifyTime.Equal(verifyTime) || verified.Domain.FailTime != nil {
			t.Fatalf("the verified domain: %+v", verified.Domain)
		}
		connect.AssertEqual(t, GetNetworkVerifiedDomains(ctx, networkId), []string{"team.example"})
		// verifying a verified domain answers it with no lookup
		lookups := domains.resolver.lookupCount()
		still, err := domains.verify(&NetworkDomainArgs{Domain: "team.example"}, ownerSession)
		if err != nil || still.Error != nil || !still.Domain.Verified || domains.resolver.lookupCount() != lookups {
			t.Fatalf("verify a verified domain: %v %+v", err, still)
		}

		notices := unsentTestNotices(t, ctx, networkId, SecurityNetworkAuthorityTemplateName)
		if len(notices) == 0 {
			t.Fatal("no notice is owed")
		}
		actions := []any{}
		for _, notice := range notices {
			if strings.Contains(notice.templateJson, "team.example") || strings.Contains(notice.templateJson, added.Domain.TxtValue) {
				t.Fatalf("the notice names the domain: %s", notice.templateJson)
			}
			for _, change := range notice.changes(t) {
				actions = append(actions, change["action"])
			}
		}
		connect.AssertEqual(t, actions, []any{SecurityNetworkAuthorityDomain, SecurityNetworkAuthorityDomainVerified})

		// a domain never added verifies nothing and removes nothing
		missing, err := domains.verify(&NetworkDomainArgs{Domain: "other.example"}, ownerSession)
		if err != nil || missing.Error == nil || missing.Domain != nil {
			t.Fatalf("verify a domain never added: %v %+v", err, missing)
		}
		none, err := domains.remove(&NetworkDomainArgs{Domain: "other.example"}, ownerSession)
		if err != nil || none.Removed {
			t.Fatalf("remove a domain never added: %v %+v", err, none)
		}
		removed, err := domains.remove(&NetworkDomainArgs{Domain: "team.example"}, ownerSession)
		if err != nil || !removed.Removed {
			t.Fatalf("remove: %v %+v", err, removed)
		}
		if GetNetworkDomain(ctx, networkId, "team.example") != nil {
			t.Fatal("the removed domain's row is still there")
		}
		connect.AssertEqual(t, testAdminEventActions(ctx, networkId), []string{
			AdminEventActionDomainAdd,
			AdminEventActionDomainVerify,
			AdminEventActionDomainRemove,
		})
		if err := VerifyNetworkAdminEventChain(ctx, networkId); err != nil {
			t.Fatal(err)
		}

		// a domain that does not parse is an error result, and writes nothing
		invalid, err := domains.add(&NetworkDomainArgs{Domain: "192.0.2.7"}, ownerSession)
		if err != nil || invalid.Error == nil {
			t.Fatalf("add an address: %v %+v", err, invalid)
		}
	})
}

// The domain routes are the Owner's: an Admin, a Member, a pending Owner, a
// client token and a personal network's owner are refused 403 with nothing
// written, a network outside the team tier gets the QuotaRefusal, and a
// network holds at most its limit.
func TestNetworkDomainRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, ownerSession := newTestDomainTeam(t, ctx)
		domains := newTestDomains()
		args := &NetworkDomainArgs{Domain: "team.example"}

		clientSession := newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleOwner, false)
		clientId := server.NewId()
		clientSession.ByJwt.ClientId = &clientId
		personalUserId, personalNetworkId := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, personalNetworkId, "personal-"+personalNetworkId.String(), personalUserId)
		personalSession := session.NewLocalClientSession(ctx, "192.0.2.31:1", session.NewByJwt(personalNetworkId, personalUserId, "personal", false, false))
		defer personalSession.Cancel()
		for label, refusedSession := range map[string]*session.ClientSession{
			"admin":         newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleAdmin, false),
			"auditor":       newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleAuditor, false),
			"member":        newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleMember, false),
			"pending owner": newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleOwner, true),
			"client token":  clientSession,
			"personal":      personalSession,
		} {
			for route, call := range map[string]func(*NetworkDomainArgs, *session.ClientSession) (*NetworkDomainResult, error){
				"add": domains.add, "verify": domains.verify, "remove": domains.remove,
			} {
				result, err := call(args, refusedSession)
				if result != nil || err == nil || !strings.HasPrefix(err.Error(), "403 ") {
					t.Errorf("%s by %s answered %+v %v, want 403", route, label, result, err)
				}
			}
		}
		if GetNetworkDomain(ctx, networkId, "team.example") != nil || GetNetworkDomain(ctx, personalNetworkId, "team.example") != nil {
			t.Fatal("a refused caller added a domain")
		}
		connect.AssertEqual(t, len(testAdminEventActions(ctx, networkId)), 0)
		connect.AssertEqual(t, domains.resolver.lookupCount(), 0)

		// outside the team tier, through the real tier check
		outside := newTestDomains()
		outside.requireTeamTier = RequireTeamTier
		result, err := outside.add(args, ownerSession)
		refusal := &QuotaRefusalError{}
		if result != nil || !errors.As(err, &refusal) || !refusal.Refusal.Upgrade || refusal.Refusal.LimitKey != TeamTierLimitKey {
			t.Fatalf("outside the tier answered %+v %v", result, err)
		}

		// the limit
		domains.settings.DomainLimit = 2
		for _, domain := range []string{"one.example", "two.example"} {
			if result, err := domains.add(&NetworkDomainArgs{Domain: domain}, ownerSession); err != nil || result.Error != nil {
				t.Fatalf("add %s: %v %+v", domain, err, result)
			}
		}
		over, err := domains.add(&NetworkDomainArgs{Domain: "three.example"}, ownerSession)
		if err != nil || over.Error == nil || GetNetworkDomain(ctx, networkId, "three.example") != nil {
			t.Fatalf("add past the limit: %v %+v", err, over)
		}
	})
}

// One network per verified domain under a race: two teams publish their
// challenges at the same name, both lookups find theirs before either claims,
// and exactly one verifies. The other is told another network holds the
// domain, stays unverified, and verifies once the holder removes it.
func TestOneNetworkPerDomainUnderARace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		domains := newTestDomains()
		type team struct {
			networkId    server.Id
			ownerSession *session.ClientSession
			challenge    string
			result       *NetworkDomainResult
			err          error
		}
		teams := []*team{{}, {}}
		txtValues := []string{}
		for _, team := range teams {
			team.networkId, team.ownerSession = newTestDomainTeam(t, ctx)
			added, err := domains.add(&NetworkDomainArgs{Domain: "shared.example"}, team.ownerSession)
			if err != nil || added.Error != nil {
				t.Fatalf("add: %v %+v", err, added)
			}
			team.challenge = added.Domain.TxtValue
			txtValues = append(txtValues, team.challenge)
		}
		if teams[0].challenge == teams[1].challenge {
			t.Fatal("two networks share a challenge")
		}
		domains.resolver.set("_urnetwork-challenge.shared.example", txtValues...)

		// the barrier: both lookups are in flight before either answers
		domains.resolver.arrived = make(chan string, len(teams))
		domains.resolver.release = make(chan struct{})
		var wg sync.WaitGroup
		for _, racingTeam := range teams {
			wg.Add(1)
			go func() {
				defer wg.Done()
				racingTeam.result, racingTeam.err = domains.verify(&NetworkDomainArgs{Domain: "shared.example"}, racingTeam.ownerSession)
			}()
		}
		for range teams {
			<-domains.resolver.arrived
		}
		close(domains.resolver.release)
		wg.Wait()

		var winner, loser *team
		for _, team := range teams {
			if team.err != nil || team.result == nil || team.result.Domain == nil {
				t.Fatalf("verify: %v %+v", team.err, team.result)
			}
			if team.result.Domain.Verified {
				if winner != nil {
					t.Fatal("two networks verified one domain")
				}
				winner = team
			} else {
				loser = team
			}
		}
		if winner == nil || loser == nil {
			t.Fatal("no network verified the domain")
		}
		if loser.result.Error == nil || loser.result.Error.Message != networkDomainHeldMessage {
			t.Fatalf("the loser was answered %+v", loser.result.Error)
		}
		connect.AssertEqual(t, GetNetworkVerifiedDomains(ctx, winner.networkId), []string{"shared.example"})
		connect.AssertEqual(t, len(GetNetworkVerifiedDomains(ctx, loser.networkId)), 0)
		var verifiedCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_domain WHERE domain = $1 AND verify_time IS NOT NULL`, "shared.example").Scan(&verifiedCount))
		})
		connect.AssertEqual(t, verifiedCount, 1)
		connect.AssertEqual(t, testAdminEventActions(ctx, loser.networkId), []string{AdminEventActionDomainAdd})

		// the holder lets go, and the other's proof then holds
		domains.resolver.arrived = nil
		if removed, err := domains.remove(&NetworkDomainArgs{Domain: "shared.example"}, winner.ownerSession); err != nil || !removed.Removed {
			t.Fatalf("remove: %v %+v", err, removed)
		}
		domains.advance(time.Minute)
		after, err := domains.verify(&NetworkDomainArgs{Domain: "shared.example"}, loser.ownerSession)
		if err != nil || after.Error != nil || !after.Domain.Verified {
			t.Fatalf("verify after the holder removed it: %v %+v", err, after)
		}
	})
}

// A verified domain with an IdP, for the recheck and discovery tests.
func newTestVerifiedDomain(t testing.TB, ctx context.Context, domains *testDomains, domain string) (networkId server.Id, ownerSession *session.ClientSession, idpId server.Id) {
	t.Helper()
	networkId, ownerSession = newTestDomainTeam(t, ctx)
	added, err := domains.add(&NetworkDomainArgs{Domain: domain}, ownerSession)
	if err != nil || added.Error != nil {
		t.Fatalf("add: %v %+v", err, added)
	}
	domains.resolver.set(NetworkDomainChallengeName(domain), added.Domain.TxtValue)
	verified, err := domains.verify(&NetworkDomainArgs{Domain: domain}, ownerSession)
	if err != nil || verified.Error != nil || !verified.Domain.Verified {
		t.Fatalf("verify: %v %+v", err, verified)
	}
	idpId = server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_idp (idp_id, network_id, issuer, client_id, client_auth) VALUES ($1, $2, $3, $4, $5)`,
			idpId, networkId, "https://idp.example", "synthetic-client", NetworkIdpClientAuthPrivateKeyJwt,
		))
	})
	return
}

// 7 failed days on a fake clock: a verified domain is looked up once a day; a
// check that does not find its record, or cannot look it up, starts a failing
// run; a check that finds it ends the run; and a run that reaches 7 days drops
// the domain, with the system's admin event, after which discovery routes
// nothing for it.
func TestNetworkDomainDroppedAfterSevenFailedDays(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		domains := newTestDomains()
		networkId, _, idpId := newTestVerifiedDomain(t, ctx, domains, "team.example")
		challenge := GetNetworkDomain(ctx, networkId, "team.example").Challenge
		routes := func() bool {
			found := NetworkSsoIdpOfEmail(ctx, "someone@team.example")
			if found != nil && *found != idpId {
				t.Fatalf("discovery routes to %s", *found)
			}
			return found != nil
		}
		connect.AssertEqual(t, routes(), true)

		// within the day nothing is due
		lookups := domains.resolver.lookupCount()
		result := domains.recheck(ctx, domains.advance(23*time.Hour), nil)
		connect.AssertEqual(t, result.CheckedCount, 0)
		connect.AssertEqual(t, domains.resolver.lookupCount(), lookups)

		// a day on, the record is found: no failing run
		result = domains.recheck(ctx, domains.advance(time.Hour), nil)
		connect.AssertEqual(t, result.CheckedCount, 1)
		connect.AssertEqual(t, result.FailedCount, 0)
		if row := GetNetworkDomain(ctx, networkId, "team.example"); row == nil || row.FailTime != nil {
			t.Fatalf("after a check that found the record: %+v", row)
		}

		// the record goes: a failing run starts, ends when the record is back
		domains.resolver.set("_urnetwork-challenge.team.example")
		failStart := domains.advance(24 * time.Hour)
		result = domains.recheck(ctx, failStart, nil)
		connect.AssertEqual(t, result.FailedCount, 1)
		connect.AssertEqual(t, result.DroppedCount, 0)
		if row := GetNetworkDomain(ctx, networkId, "team.example"); row == nil || row.FailTime == nil || !row.FailTime.Equal(failStart) {
			t.Fatalf("after a failed check: %+v", row)
		}
		domains.resolver.set("_urnetwork-challenge.team.example", challenge)
		result = domains.recheck(ctx, domains.advance(3*24*time.Hour), nil)
		connect.AssertEqual(t, result.FailedCount, 0)
		if row := GetNetworkDomain(ctx, networkId, "team.example"); row == nil || row.FailTime != nil {
			t.Fatalf("after the record came back: %+v", row)
		}

		// a lookup that errors is a failed check too, and starts a new run
		domains.resolver.fail(errors.New("synthetic resolver failure"))
		failStart = domains.advance(24 * time.Hour)
		result = domains.recheck(ctx, failStart, nil)
		connect.AssertEqual(t, result.FailedCount, 1)
		// failed on each of the next six days, the domain still routes
		for day := 1; day <= 6; day += 1 {
			result = domains.recheck(ctx, domains.advance(24*time.Hour), nil)
			connect.AssertEqual(t, result.CheckedCount, 1)
			connect.AssertEqual(t, result.FailedCount, 1)
			connect.AssertEqual(t, result.DroppedCount, 0)
			row := GetNetworkDomain(ctx, networkId, "team.example")
			if row == nil || row.FailTime == nil || !row.FailTime.Equal(failStart) {
				t.Fatalf("day %d of the failing run: %+v", day, row)
			}
			connect.AssertEqual(t, routes(), true)
		}
		// the seventh day's failed check drops it
		result = domains.recheck(ctx, domains.advance(24*time.Hour), nil)
		connect.AssertEqual(t, result.DroppedCount, 1)
		if GetNetworkDomain(ctx, networkId, "team.example") != nil {
			t.Fatal("the domain outlived 7 failed days")
		}
		connect.AssertEqual(t, routes(), false)
		actions := testAdminEventActions(ctx, networkId)
		connect.AssertEqual(t, actions[len(actions)-1], AdminEventActionDomainDrop)
		events := GetNetworkAdminEvents(ctx, networkId, 0, 0)
		if drop := events[len(events)-1]; drop.ActorKind != AdminEventActorSystem || drop.ActorId != nil {
			t.Fatalf("the drop's actor: %+v", drop)
		}
		// nothing is left to check
		lookups = domains.resolver.lookupCount()
		domains.recheck(ctx, domains.advance(24*time.Hour), nil)
		connect.AssertEqual(t, domains.resolver.lookupCount(), lookups)
	})
}

// A domain added and never verified goes 7 days after it was added, with no
// lookup, and not a moment before.
func TestUnverifiedNetworkDomainExpires(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, ownerSession := newTestDomainTeam(t, ctx)
		domains := newTestDomains()
		if added, err := domains.add(&NetworkDomainArgs{Domain: "pending.example"}, ownerSession); err != nil || added.Error != nil {
			t.Fatalf("add: %v %+v", err, added)
		}
		result := domains.recheck(ctx, domains.advance(7*24*time.Hour-time.Second), nil)
		connect.AssertEqual(t, result.DroppedCount, 0)
		if GetNetworkDomain(ctx, networkId, "pending.example") == nil {
			t.Fatal("an unverified domain went before its 7 days")
		}
		result = domains.recheck(ctx, domains.advance(time.Second), nil)
		connect.AssertEqual(t, result.DroppedCount, 1)
		connect.AssertEqual(t, result.CheckedCount, 0)
		connect.AssertEqual(t, domains.resolver.lookupCount(), 0)
		if GetNetworkDomain(ctx, networkId, "pending.example") != nil {
			t.Fatal("an unverified domain outlived its 7 days")
		}
		connect.AssertEqual(t, testAdminEventActions(ctx, networkId), []string{AdminEventActionDomainAdd, AdminEventActionDomainDrop})
	})
}

// A pass examines at most its batch and hands on a cursor; the next pass from
// the cursor takes the rest, and a pass that reaches the end hands on none.
func TestNetworkDomainRecheckResumesFromItsCursor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		domains := newTestDomains()
		for _, domain := range []string{"one.example", "two.example", "three.example"} {
			newTestVerifiedDomain(t, ctx, domains, domain)
		}
		domains.settings.RecheckBatchCount = 2
		now := domains.advance(24 * time.Hour)
		lookups := domains.resolver.lookupCount()
		first := domains.recheck(ctx, now, nil)
		connect.AssertEqual(t, first.CheckedCount, 2)
		if first.After == nil {
			t.Fatal("a pass that stopped at its bound handed on no cursor")
		}
		second := domains.recheck(ctx, now, first.After)
		connect.AssertEqual(t, second.CheckedCount, 1)
		if second.After != nil {
			t.Fatalf("a pass that reached the end handed on %+v", second.After)
		}
		connect.AssertEqual(t, domains.resolver.lookupCount(), lookups+3)
	})
}

// Discovery's lookup answers alike for every address it does not route, and
// reads only the domain: an address that does not parse, a domain no network
// added, one added and unverified, one removed, and a verified one whose team
// has no IdP all answer nothing; a verified domain with an IdP answers that
// IdP for any local part, whether or not a user holds the address.
func TestSsoDiscoveryAnswersAlikeForUnknownDomains(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		domains := newTestDomains()
		_, _, idpId := newTestVerifiedDomain(t, ctx, domains, "routed.example")

		_, pendingOwner := newTestDomainTeam(t, ctx)
		if added, err := domains.add(&NetworkDomainArgs{Domain: "unverified.example"}, pendingOwner); err != nil || added.Error != nil {
			t.Fatalf("add: %v %+v", err, added)
		}
		// verified, its team with no IdP
		noIdpNetworkId, noIdpOwner := newTestDomainTeam(t, ctx)
		added, err := domains.add(&NetworkDomainArgs{Domain: "noidp.example"}, noIdpOwner)
		if err != nil || added.Error != nil {
			t.Fatalf("add: %v %+v", err, added)
		}
		domains.resolver.set("_urnetwork-challenge.noidp.example", added.Domain.TxtValue)
		if verified, err := domains.verify(&NetworkDomainArgs{Domain: "noidp.example"}, noIdpOwner); err != nil || !verified.Domain.Verified {
			t.Fatalf("verify: %v %+v", err, verified)
		}
		connect.AssertEqual(t, GetNetworkVerifiedDomains(ctx, noIdpNetworkId), []string{"noidp.example"})
		// a user that holds an address at a domain no network added
		existingUserId := server.NewId()
		existingUserAuth := Testing_CreateNetwork(ctx, server.NewId(), "existing-"+existingUserId.String(), existingUserId)

		for _, email := range []string{
			"",
			"not-an-address",
			"@routed.example",
			"someone@",
			"someone@unknown.example",
			"someone@unverified.example",
			"someone@noidp.example",
			"someone@sub.routed.example",
			"someone@routed.example.evil.example",
			existingUserAuth,
		} {
			if found := NetworkSsoIdpOfEmail(ctx, email); found != nil {
				t.Errorf("%q routes to %s", email, *found)
			}
		}
		for _, email := range []string{"someone@routed.example", "nobody-with-this-address@ROUTED.example", "a@b@routed.example."} {
			found := NetworkSsoIdpOfEmail(ctx, email)
			if found == nil || *found != idpId {
				t.Errorf("%q routes to %v, want the team's IdP", email, found)
			}
		}
	})
}

// A removed domain stops routing at once, and the oldest of a team's IdPs is
// the one a domain routes to.
func TestRemovedDomainStopsRouting(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		domains := newTestDomains()
		networkId, ownerSession, idpId := newTestVerifiedDomain(t, ctx, domains, "team.example")
		laterIdpId := server.NewId()
		for !idpId.Less(laterIdpId) {
			laterIdpId = server.NewId()
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_idp (idp_id, network_id, issuer, client_id, client_auth) VALUES ($1, $2, $3, $4, $5)`,
				laterIdpId, networkId, "https://idp2.example", "synthetic-client", NetworkIdpClientAuthPrivateKeyJwt,
			))
		})
		found := NetworkSsoIdpOfEmail(ctx, "someone@team.example")
		if found == nil || *found != idpId {
			t.Fatalf("routes to %v, want the oldest IdP", found)
		}
		if removed, err := domains.remove(&NetworkDomainArgs{Domain: "team.example"}, ownerSession); err != nil || !removed.Removed {
			t.Fatalf("remove: %v %+v", err, removed)
		}
		if found := NetworkSsoIdpOfEmail(ctx, "someone@team.example"); found != nil {
			t.Fatalf("a removed domain routes to %s", *found)
		}
	})
}
