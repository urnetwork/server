package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// The bypass-domain gate. These run without Postgres or Redis: the sign-in
// email lookup and the active-drain listing are fakes.

type fakeTestBalanceDrainSignIns struct {
	networkIdEmails map[server.Id][]string
	lookups         []server.Id
}

func (self *fakeTestBalanceDrainSignIns) emails(networkId server.Id) []string {
	self.lookups = append(self.lookups, networkId)
	return self.networkIdEmails[networkId]
}

func TestTestBalanceDrainBypassDomainCallerAllowed(t *testing.T) {
	testNetworkId := server.NewId()
	ssoNetworkId := server.NewId()
	realNetworkId := server.NewId()
	phoneNetworkId := server.NewId()
	subdomainNetworkId := server.NewId()
	suffixNetworkId := server.NewId()
	noAuthNetworkId := server.NewId()
	signIns := &fakeTestBalanceDrainSignIns{networkIdEmails: map[server.Id][]string{
		testNetworkId:      {"Acceptance-IB-20261003T120000Z-1a2b@Acceptance.Invalid"},
		ssoNetworkId:       {"someone@gmail.com", "ib-sso@acceptance.invalid"},
		realNetworkId:      {"someone@gmail.com"},
		phoneNetworkId:     {"+13125550100"},
		subdomainNetworkId: {"ib@sub.acceptance.invalid"},
		suffixNetworkId:    {"ib@acceptance.invalid.attacker.example"},
	}}
	defer Testing_SetTestBalanceDrainSignInEmails(signIns.emails)()
	Testing_SetTestBalanceDrainGate(nil, []string{"acceptance.invalid"})
	defer Testing_SetTestBalanceDrainAllowlist(nil)

	ctx := context.Background()
	connect.AssertEqual(t, TestBalanceDrainAllowed(ctx, testNetworkId), true)
	connect.AssertEqual(t, TestBalanceDrainAllowed(ctx, ssoNetworkId), true)
	for _, networkId := range []server.Id{realNetworkId, phoneNetworkId, subdomainNetworkId, suffixNetworkId, noAuthNetworkId} {
		if TestBalanceDrainAllowed(ctx, networkId) {
			t.Fatalf("network %s without a sign-in email on the bypass domain was allowed", networkId)
		}
	}
	// the lookup is only ever about the network being checked
	connect.AssertEqual(t, signIns.lookups, []server.Id{
		testNetworkId, ssoNetworkId, realNetworkId, phoneNetworkId, subdomainNetworkId, suffixNetworkId, noAuthNetworkId,
	})

	// refused before any database access
	drain, err := DrainTestBalance(ctx, realNetworkId, time.Minute)
	connect.AssertEqual(t, drain == nil, true)
	connect.AssertEqual(t, err, ErrTestBalanceDrainNotAllowed)
}

func TestTestBalanceDrainMalformedBypassListRefusesEveryone(t *testing.T) {
	testNetworkId := server.NewId()
	signIns := &fakeTestBalanceDrainSignIns{networkIdEmails: map[server.Id][]string{
		testNetworkId: {"ib@acceptance.invalid"},
	}}
	defer Testing_SetTestBalanceDrainSignInEmails(signIns.emails)()
	ctx := context.Background()

	for _, malformed := range [][]string{
		{"acceptance.invalid", "*.invalid"},
		{"acceptance.invalid", "ib@acceptance.invalid"},
		{"acceptance.invalid", ""},
		{"acceptance.invalid", "127.0.0.1:443"},
	} {
		Testing_SetTestBalanceDrainGate(nil, malformed)
		if TestBalanceDrainAllowed(ctx, testNetworkId) {
			t.Fatalf("malformed bypass list %q allowed a drain", malformed)
		}
	}
	Testing_SetTestBalanceDrainAllowlist(nil)
	connect.AssertEqual(t, len(signIns.lookups), 0)
}

func TestTestBalanceDrainVaultBypassDomainsFailClosed(t *testing.T) {
	testNetworkId := server.NewId()
	signIns := &fakeTestBalanceDrainSignIns{networkIdEmails: map[server.Id][]string{
		testNetworkId: {"ib@acceptance.invalid"},
	}}
	defer Testing_SetTestBalanceDrainSignInEmails(signIns.emails)()
	Testing_SetTestBalanceDrainAllowlist(nil)
	testBalanceDrainGates.snapshot.Store(nil)
	defer testBalanceDrainGates.snapshot.Store(nil)
	popAllowlist := server.Vault.PushSimpleResource(testBalanceDrainVaultResource, []byte("network_ids: []\n"))
	defer popAllowlist()
	ctx := context.Background()

	allowed := func(testsYaml string) bool {
		pop := server.Vault.PushSimpleResource(testsVaultResourceName, []byte(testsYaml))
		defer pop()
		testBalanceDrainGates.snapshot.Store(nil)
		return TestBalanceDrainAllowed(ctx, testNetworkId)
	}

	connect.AssertEqual(t, allowed("version: 1\nemail_verification:\n  bypass_domains: [acceptance.invalid]\n"), true)
	// one malformed entry disables the whole domain gate
	connect.AssertEqual(t, allowed("version: 1\nemail_verification:\n  bypass_domains: [acceptance.invalid, '*.invalid']\n"), false)
	connect.AssertEqual(t, allowed("version: 2\nemail_verification:\n  bypass_domains: [acceptance.invalid]\n"), false)
	connect.AssertEqual(t, allowed("version: 1\nemail_verification:\n  bypass_domains: []\n"), false)
	connect.AssertEqual(t, allowed("version: 1\nemail_verification:\n  bypass_domains: acceptance.invalid\n"), false)
	connect.AssertEqual(t, allowed("version: 1\nemail_verification:\n  bypass_domains: [other.invalid]\n"), false)
}

func TestTestBalanceDrainAllowlistStillWorksBesideDomains(t *testing.T) {
	listedNetworkId := server.NewId()
	unlistedNetworkId := server.NewId()
	signIns := &fakeTestBalanceDrainSignIns{networkIdEmails: map[server.Id][]string{
		listedNetworkId:   {"someone@gmail.com"},
		unlistedNetworkId: {"someone-else@gmail.com"},
	}}
	defer Testing_SetTestBalanceDrainSignInEmails(signIns.emails)()
	defer Testing_SetTestBalanceDrainAllowlist(nil)
	ctx := context.Background()

	Testing_SetTestBalanceDrainGate([]server.Id{listedNetworkId}, []string{"acceptance.invalid"})
	connect.AssertEqual(t, TestBalanceDrainAllowed(ctx, listedNetworkId), true)
	connect.AssertEqual(t, TestBalanceDrainAllowed(ctx, unlistedNetworkId), false)

	// a malformed domain list leaves the allowlist working
	Testing_SetTestBalanceDrainGate([]server.Id{listedNetworkId}, []string{"*"})
	connect.AssertEqual(t, TestBalanceDrainAllowed(ctx, listedNetworkId), true)
	connect.AssertEqual(t, TestBalanceDrainAllowed(ctx, unlistedNetworkId), false)

	// neither configured: off for everyone, without a lookup
	lookupCount := len(signIns.lookups)
	Testing_SetTestBalanceDrainGate(nil, nil)
	connect.AssertEqual(t, TestBalanceDrainAllowed(ctx, listedNetworkId), false)
	connect.AssertEqual(t, len(signIns.lookups), lookupCount)
}

func TestTestBalanceDrainDomainHotPathUsesOneSnapshot(t *testing.T) {
	drainedTestNetworkId := server.NewId()
	drainedRealNetworkId := server.NewId()
	otherNetworkId := server.NewId()
	signIns := &fakeTestBalanceDrainSignIns{networkIdEmails: map[server.Id][]string{
		drainedTestNetworkId: {"ib@acceptance.invalid"},
		drainedRealNetworkId: {"someone@gmail.com"},
	}}
	defer Testing_SetTestBalanceDrainSignInEmails(signIns.emails)()
	activeDrainedNetworkIds := []server.Id{drainedTestNetworkId, drainedRealNetworkId}
	listCount := 0
	previousList := testBalanceDrainActiveNetworkIds
	testBalanceDrainActiveNetworkIds = func(ctx context.Context, query server.PgCanQuery, now time.Time) []server.Id {
		listCount += 1
		return activeDrainedNetworkIds
	}
	defer func() { testBalanceDrainActiveNetworkIds = previousList }()
	Testing_SetTestBalanceDrainGate(nil, []string{"acceptance.invalid"})
	defer Testing_SetTestBalanceDrainAllowlist(nil)

	ctx := context.Background()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	// failingQuery: no per-network drain query for a domain-gated payer
	query := failingQuery{t: t}
	connect.AssertEqual(t, testBalanceDrainActive(ctx, query, drainedTestNetworkId, now), true)
	// a drained network that no longer passes the gate is not drained
	connect.AssertEqual(t, testBalanceDrainActive(ctx, query, drainedRealNetworkId, now), false)
	for i := 0; i < 100; i += 1 {
		connect.AssertEqual(t, testBalanceDrainActive(ctx, query, otherNetworkId, now.Add(time.Duration(i)*time.Millisecond)), false)
	}
	connect.AssertEqual(t, listCount, 1)
	connect.AssertEqual(t, signIns.lookups, []server.Id{drainedTestNetworkId, drainedRealNetworkId})

	// restored elsewhere: seen after the refresh interval
	activeDrainedNetworkIds = []server.Id{}
	connect.AssertEqual(t, testBalanceDrainActive(ctx, query, drainedTestNetworkId, now.Add(testBalanceDrainActiveRefresh)), false)
	connect.AssertEqual(t, listCount, 2)

	// the gate off: no listing at all
	Testing_SetTestBalanceDrainGate(nil, nil)
	connect.AssertEqual(t, testBalanceDrainActive(ctx, query, drainedTestNetworkId, now.Add(time.Hour)), false)
	connect.AssertEqual(t, listCount, 2)
}

// Requires Postgres and Redis (server.DefaultTestEnv). The live sign-in lookup
// reads the network's own password-auth record.
func TestTestBalanceDrainBypassDomainRoundTrip(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		testNetworkId := server.NewId()
		testUserId := server.NewId()
		realNetworkId := server.NewId()
		realUserId := server.NewId()
		Testing_CreateNetwork(ctx, testNetworkId, "ib-test-domain", testUserId)
		Testing_CreateNetwork(ctx, realNetworkId, "ib-real-domain", realUserId)
		testUserAuth := "acceptance-ib-" + testNetworkId.String() + "@acceptance.invalid"
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_user_auth_password SET user_auth = $2 WHERE user_id = $1`,
				testUserId,
				testUserAuth,
			))
		})
		balanceByteCount := ByteCount(1024 * 1024 * 1024)
		for _, networkId := range []server.Id{testNetworkId, realNetworkId} {
			AddBasicTransferBalance(ctx, networkId, balanceByteCount, server.NowUtc(), server.NowUtc().Add(24*time.Hour))
		}

		Testing_SetTestBalanceDrainGate(nil, []string{"acceptance.invalid"})
		defer Testing_SetTestBalanceDrainAllowlist(nil)

		connect.AssertEqual(t, TestBalanceDrainAllowed(ctx, testNetworkId), true)
		connect.AssertEqual(t, TestBalanceDrainAllowed(ctx, realNetworkId), false)
		_, err := DrainTestBalance(ctx, realNetworkId, time.Minute)
		connect.AssertEqual(t, err, ErrTestBalanceDrainNotAllowed)

		_, err = DrainTestBalance(ctx, testNetworkId, 5*time.Minute)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, IsTestBalanceDrainActive(ctx, testNetworkId), true)
		connect.AssertEqual(t, GetActiveTransferBalanceByteCount(ctx, testNetworkId), ByteCount(0))
		connect.AssertEqual(t, IsTestBalanceDrainActive(ctx, realNetworkId), false)
		connect.AssertEqual(t, GetActiveTransferBalanceByteCount(ctx, realNetworkId), balanceByteCount)

		connect.AssertEqual(t, RestoreTestBalance(ctx, testNetworkId), int64(1))
		connect.AssertEqual(t, IsTestBalanceDrainActive(ctx, testNetworkId), false)
		connect.AssertEqual(t, GetActiveTransferBalanceByteCount(ctx, testNetworkId), balanceByteCount)
	})
}
