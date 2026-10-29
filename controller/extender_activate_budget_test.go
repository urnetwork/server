package controller

import (
	"context"
	"crypto/ed25519"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The activation budgets (connect/EXTENDER.md C2, support inbox 8083): what
// spends them, and which family's they are.

// The budget attempts recorded under one id, by action.
func testAccountActionAttempts(t testing.TB, ctx context.Context, id server.Id) map[string]int {
	t.Helper()
	attempts := map[string]int{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT action, COUNT(*) FROM network_user_action_attempt
			WHERE user_id = $1
			GROUP BY action
			`,
			id,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var action string
				var count int
				server.Raise(result.Scan(&action, &count))
				attempts[action] = count
			}
		})
	})
	return attempts
}

// The number of budget attempts recorded under one id, over every action.
func testAccountActionAttemptCount(t testing.TB, ctx context.Context, id server.Id) int {
	t.Helper()
	count := 0
	for _, actionCount := range testAccountActionAttempts(t, ctx, id) {
		count += actionCount
	}
	return count
}

// A tcp port nothing is bound to on either loopback family, so a probe of it
// is refused at once.
func testClosedTcpPort(t testing.TB) int {
	t.Helper()
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

// Each family has its own activation budget, of the same size as before
// (support inbox 8083). One budget for both let a dual-stack provider's failing
// family spend the other's: three restarts of a provider whose ipv4 posts
// failed spent the hour, and its ipv6 could not activate either. Here a client
// spends its whole ipv4 budget on probes that fail, is refused a seventh ipv4
// activation by a refusal that names the family, the window and the wait, and
// still activates ipv6.
func TestExtenderActivateBudgetsAreSeparatePerFamily(t *testing.T) {
	// the limits are unchanged, and now hold per family
	settings := DefaultExtenderActivateSettings()
	connect.AssertEqual(t, settings.ClientRateLimit, 6)
	connect.AssertEqual(t, settings.AccountRateLimit, 60)
	connect.AssertEqual(t, settings.RateLimitWindow, time.Hour)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		rootPrivateKey := installTestExtenderConfig(t, fixture.api)
		rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)

		failingArgs := fixture.activateArgs()
		failingArgs.TcpPort = testClosedTcpPort(t)
		failingArgs.Carriers = []string{connect.ExtenderCarrierTcp}

		v4Session := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		for i := range settings.ClientRateLimit {
			result, err := ExtenderActivate(failingArgs, v4Session)
			if err != nil {
				t.Fatal(err)
			}
			connect.AssertEqual(t, result.Activated, false)
			if !strings.Contains(result.Error, "tcp carrier") {
				t.Fatalf("ipv4 attempt %d was not refused by its probe: %q", i+1, result.Error)
			}
		}

		result, err := ExtenderActivate(failingArgs, v4Session)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, result.Activated, false)
		for _, want := range []string{
			"maximum number of attempts for activating an extender over IPv4",
			"in the last hour",
			"Please try again in ",
		} {
			if !strings.Contains(result.Error, want) {
				t.Errorf("the seventh ipv4 refusal %q does not say %q", result.Error, want)
			}
		}
		for _, notWant := range []string{"today", "later", "this action"} {
			if strings.Contains(result.Error, notWant) {
				t.Errorf("the seventh ipv4 refusal %q says %q", result.Error, notWant)
			}
		}

		// the same client, its ipv4 budget spent, activates ipv6
		v6Session := session.Testing_CreateClientSession(ctx, v4Session.ByJwt)
		v6Session.ClientAddress = fixture.clientAddress("::1")
		result, err = ExtenderActivate(fixture.activateArgs(), v6Session)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("ipv6 was refused once ipv4's budget was spent: %s", result.Error)
		}
		connect.AssertEqual(t, result.IpVersion, 6)
		body := verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		connect.AssertEqual(t, len(body.Addresses), 1)
		connect.AssertEqual(t, body.Addresses[0].Ip, "::1")

		// each family's budgets hold their own attempts: the six ipv4 probes,
		// and the one ipv6 activation. The refusal spent nothing.
		clientAttempts := testAccountActionAttempts(t, ctx, *v4Session.ByJwt.ClientId)
		accountAttempts := testAccountActionAttempts(t, ctx, v4Session.ByJwt.UserId)
		for _, c := range []struct {
			attempts map[string]int
			action   string
			want     int
		}{
			{attempts: clientAttempts, action: "extender_activate_v4", want: 6},
			{attempts: clientAttempts, action: "extender_activate_v6", want: 1},
			{attempts: accountAttempts, action: "extender_activate_account_v4", want: 6},
			{attempts: accountAttempts, action: "extender_activate_account_v6", want: 1},
		} {
			if c.attempts[c.action] != c.want {
				t.Errorf("%s holds %d attempts, want %d (all: %v)", c.action, c.attempts[c.action], c.want, c.attempts)
			}
		}
	})
}

// The budgets are spent at the probe, and nowhere before it (support inbox
// 8083). A request refused before anything dials the caller -- malformed
// arguments, an unreadable caller, an operator that cannot probe, a budget
// with no room -- spends nothing, while every activation that dials the caller
// spends one attempt of its client's budget and one of its account's, whether
// its probe then passes or not.
//
// The client's budget was spent before the account's was checked, so a client
// refused by its account's budget lost an attempt of its own for an activation
// that never ran.
func TestExtenderActivateSpendsItsBudgetsOnlyAtTheProbe(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderConfig(t, fixture.api)

		settings := &ExtenderActivateSettings{
			ClientRateLimit:  6,
			AccountRateLimit: 2,
			RateLimitWindow:  time.Hour,
		}
		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		clientId := *clientSession.ByJwt.ClientId
		userId := clientSession.ByJwt.UserId

		activate := func(args *ExtenderActivateArgs, clientSession *session.ClientSession) *ExtenderActivateResult {
			t.Helper()
			result, err := extenderActivate(args, clientSession, settings)
			if err != nil {
				t.Fatal(err)
			}
			return result
		}
		requireRefused := func(stage string, result *ExtenderActivateResult, wantError string) {
			t.Helper()
			if result.Activated || !strings.Contains(result.Error, wantError) {
				t.Fatalf("%s: activated %t, error %q, want a refusal naming %q", stage, result.Activated, result.Error, wantError)
			}
		}
		requireSpent := func(stage string, id server.Id, want int) {
			t.Helper()
			if count := testAccountActionAttemptCount(t, ctx, id); count != want {
				t.Errorf("%s: %d budget attempts are recorded, want %d (%v)", stage, count, want, testAccountActionAttempts(t, ctx, id))
			}
		}

		// refused before the probe: nothing is spent
		malformedKeyArgs := fixture.activateArgs()
		malformedKeyArgs.PublicKeyHex = "aabb"
		unknownCarrierArgs := fixture.activateArgs()
		unknownCarrierArgs.Carriers = []string{"tcp", "sctp"}
		outOfRangeArgs := fixture.activateArgs()
		outOfRangeArgs.DnsPorts = []int{65536}
		requireRefused("a malformed public key", activate(malformedKeyArgs, clientSession), "public key")
		requireRefused("an unknown carrier", activate(unknownCarrierArgs, clientSession), "unknown carrier")
		requireRefused("a dns port out of range", activate(outOfRangeArgs, clientSession), "dns_ports")

		unreadableSession := session.Testing_CreateClientSession(ctx, clientSession.ByJwt)
		unreadableSession.ClientAddress = "not-an-address.example:54321"
		requireRefused("an unreadable caller", activate(fixture.activateArgs(), unreadableSession), "not readable")

		// an operator with no root key cannot sign what it would probe for
		installTestExtenderConfigYaml(t, strings.Join([]string{
			"network_host: " + testExtenderNetworkHost,
			"api_url: https://" + testExtenderApiHost,
		}, "\n"))
		requireRefused("an unconfigured operator", activate(fixture.activateArgs(), clientSession), "root key")
		installTestExtenderConfig(t, fixture.api)

		requireSpent("the refusals before the probe, client", clientId, 0)
		requireSpent("the refusals before the probe, account", userId, 0)

		// a probe that fails spends one attempt of each, the family's
		failingArgs := fixture.activateArgs()
		failingArgs.TcpPort = testClosedTcpPort(t)
		failingArgs.Carriers = []string{connect.ExtenderCarrierTcp}
		requireRefused("a failed probe", activate(failingArgs, clientSession), "tcp carrier")
		requireSpent("a failed probe, client", clientId, 1)
		requireSpent("a failed probe, account", userId, 1)
		if attempts := testAccountActionAttempts(t, ctx, clientId); attempts["extender_activate_v4"] != 1 {
			t.Errorf("the failed ipv4 probe is not recorded under the client's ipv4 budget: %v", attempts)
		}
		if attempts := testAccountActionAttempts(t, ctx, userId); attempts["extender_activate_account_v4"] != 1 {
			t.Errorf("the failed ipv4 probe is not recorded under the account's ipv4 budget: %v", attempts)
		}

		// a probe that passes spends one attempt of each too
		if result := activate(fixture.activateArgs(), clientSession); !result.Activated {
			t.Fatalf("the activation was refused: %s", result.Error)
		}
		requireSpent("an activation, client", clientId, 2)
		requireSpent("an activation, account", userId, 2)

		// the account's budget of two is spent. Its clients are refused by it,
		// and the refusal spends none of their own.
		requireRefused(
			"the account's spent budget",
			activate(fixture.activateArgs(), clientSession),
			"maximum number of attempts",
		)
		requireSpent("the account's refusal, client", clientId, 2)
		otherSession := newTestExtenderAccountSession(ctx, clientSession)
		otherClientId := *otherSession.ByJwt.ClientId
		requireRefused(
			"the account's spent budget, another client",
			activate(fixture.activateArgs(), otherSession),
			"maximum number of attempts",
		)
		requireSpent("the account's refusal, another client", otherClientId, 0)
		requireSpent("the account's refusals, account", userId, 2)
	})
}

// A caller address that is an ipv4-mapped ipv6 address is the ipv4 address it
// maps, which is what the probe dials. The answer, the signed record and the
// stored address name it in its own family's form, never an ipv6 form under
// ipv4, and the attempt is ipv4's.
func TestExtenderActivateRecordsAMappedCallerUnderItsOwnFamily(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		rootPrivateKey := installTestExtenderConfig(t, fixture.api)
		rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)

		args := fixture.activateArgs()
		// the forward is what proves the family; it runs over tcp
		args.Carriers = []string{connect.ExtenderCarrierTcp}

		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("::ffff:127.0.0.1"))
		result, err := ExtenderActivate(args, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the mapped caller was refused: %s", result.Error)
		}
		if result.Ip != "127.0.0.1" || result.IpVersion != 4 {
			t.Errorf("the answer names %s as ipv%d, want 127.0.0.1 as ipv4", result.Ip, result.IpVersion)
		}
		body := verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		connect.AssertEqual(t, len(body.Addresses), 1)
		if body.Addresses[0].Ip != "127.0.0.1" || body.Addresses[0].IpVersion != 4 {
			t.Errorf("the record names %s as ipv%d, want 127.0.0.1 as ipv4", body.Addresses[0].Ip, body.Addresses[0].IpVersion)
		}
		stored := model.Testing_GetNetworkExtender(ctx, testExtenderIdForKey(ctx, t, fixture.publicKey))
		connect.AssertEqual(t, len(stored.Addresses), 1)
		if stored.Addresses[0].Ip.String() != "127.0.0.1" || stored.Addresses[0].IpVersion != 4 {
			t.Errorf("the stored address is %s as ipv%d, want 127.0.0.1 as ipv4", stored.Addresses[0].Ip, stored.Addresses[0].IpVersion)
		}
	})
}
