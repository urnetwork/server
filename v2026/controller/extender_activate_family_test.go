package controller

import (
	"context"
	"crypto/ed25519"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The family an activation is for (connect/EXTENDER.md C2, support inbox
// 8083): an extender posts each family's activation to that family's api host,
// and the operator probes the family the request arrived on.

// The family a host claims is the family suffix of an api service label, with
// or without an env prefix, a port, a trailing dot or upper case. Every other
// host claims none.
func TestExtenderActivateHostIpVersion(t *testing.T) {
	cases := []struct {
		host string
		want int
	}{
		{host: "api-v4.bringyour.com", want: 4},
		{host: "api-v6.bringyour.com", want: 6},
		{host: "api-v4.example", want: 4},
		{host: "g2-api-v6.bringyour.com", want: 6},
		{host: "main-api-v4.ur.network", want: 4},
		{host: "api-v4.bringyour.com:443", want: 4},
		{host: "API-V6.BringYour.com.", want: 6},
		{host: " api-v4.bringyour.com ", want: 4},

		{host: "", want: 0},
		{host: "api.bringyour.com", want: 0},
		{host: "g2-api.bringyour.com", want: 0},
		{host: "api.example:8443", want: 0},
		{host: "connect-v4.bringyour.com", want: 0},
		{host: "alt-v6.bringyour.com", want: 0},
		{host: "api-v5.bringyour.com", want: 0},
		{host: "api-v4-x.bringyour.com", want: 0},
		{host: "v4.bringyour.com", want: 0},
		{host: "api-v4", want: 0},
		{host: "localhost:8080", want: 0},
		{host: "192.0.2.1", want: 0},
		{host: "192.0.2.1:443", want: 0},
		{host: "[2001:db8::1]:443", want: 0},
		{host: "[2001:db8::1]", want: 0},
		{host: "2001:db8::1", want: 0},
	}
	for _, c := range cases {
		if got := extenderActivateHostIpVersion(c.host); got != c.want {
			t.Errorf("%q claims ipv%d, want ipv%d", c.host, got, c.want)
		}
	}
}

// An activation that arrives on a family other than the one its host is for
// is refused with both families named, before anything is probed, spent or
// stored. Before, an ipv4 activation that an alt path carried over ipv6 was
// probed and stored as the caller's ipv6 address and answered as activated,
// and the extender took it for its ipv4 (connect#229). connect fc06cc7d
// removed that path; the operator refuses whatever path does it next.
func TestExtenderActivateRefusesAnActivationThatArrivedOnTheOtherFamily(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderConfig(t, fixture.api)

		cases := []struct {
			host       string
			loopbackIp string
			wantError  string
		}{
			{
				host:       "api-v4.example",
				loopbackIp: "::1",
				wantError:  "the activation for ipv4 arrived over ipv6",
			},
			{
				host:       "g2-api-v4.example:443",
				loopbackIp: "::1",
				wantError:  "the activation for ipv4 arrived over ipv6",
			},
			{
				host:       "api-v6.example",
				loopbackIp: "127.0.0.1",
				wantError:  "the activation for ipv6 arrived over ipv4",
			},
		}
		for _, c := range cases {
			clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress(c.loopbackIp))
			clientSession.Host = c.host
			result, err := ExtenderActivate(fixture.activateArgs(), clientSession)
			if err != nil {
				t.Fatal(err)
			}
			if result.Activated || !strings.Contains(result.Error, c.wantError) {
				t.Errorf("%s from %s: activated %t, error %q, want a refusal %q", c.host, c.loopbackIp, result.Activated, result.Error, c.wantError)
			}
			// nothing is recorded under either family
			if result.Ip != "" || result.IpVersion != 0 || result.Record != "" {
				t.Errorf("%s from %s: the refusal answers for %q ipv%d", c.host, c.loopbackIp, result.Ip, result.IpVersion)
			}
			if count := testAccountActionAttemptCount(t, ctx, *clientSession.ByJwt.ClientId); count != 0 {
				t.Errorf("%s from %s: the refusal spent %d of the client's budget", c.host, c.loopbackIp, count)
			}
			if count := testAccountActionAttemptCount(t, ctx, clientSession.ByJwt.UserId); count != 0 {
				t.Errorf("%s from %s: the refusal spent %d of the account's budget", c.host, c.loopbackIp, count)
			}
		}
		connect.AssertEqual(t, len(model.GetRandomActiveNetworkExtenders(ctx, 8, server.Id{})), 0)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderPublishes(ctx)), 0)
	})
}

// The happy path is unchanged: an activation posted to each family's host and
// arriving on that family activates that family, the second adds its address
// to the same record, and a host that claims no family -- the plain api url of
// an operator without family hosts -- activates whichever family it arrived
// on. Each spends its own family's budget.
func TestExtenderActivateOnTheFamilyHosts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		rootPrivateKey := installTestExtenderConfig(t, fixture.api)
		rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)

		v4Session := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		v4Session.Host = "api-v4.example"
		v6Session := session.Testing_CreateClientSession(ctx, v4Session.ByJwt)
		v6Session.ClientAddress = fixture.clientAddress("::1")
		v6Session.Host = "api-v6.example"
		plainSession := session.Testing_CreateClientSession(ctx, v4Session.ByJwt)
		plainSession.ClientAddress = fixture.clientAddress("::1")
		plainSession.Host = testExtenderApiHost

		for _, c := range []struct {
			clientSession *session.ClientSession
			wantIp        string
			wantIpVersion int
		}{
			{clientSession: v4Session, wantIp: "127.0.0.1", wantIpVersion: 4},
			{clientSession: v6Session, wantIp: "::1", wantIpVersion: 6},
			{clientSession: plainSession, wantIp: "::1", wantIpVersion: 6},
		} {
			result, err := ExtenderActivate(fixture.activateArgs(), c.clientSession)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Activated {
				t.Fatalf("%s from %s was refused: %s", c.clientSession.Host, c.wantIp, result.Error)
			}
			connect.AssertEqual(t, result.Ip, c.wantIp)
			connect.AssertEqual(t, result.IpVersion, c.wantIpVersion)
		}

		stored := model.Testing_GetNetworkExtender(ctx, testExtenderIdForKey(ctx, t, fixture.publicKey))
		storedIps := []string{}
		for _, address := range stored.Addresses {
			storedIps = append(storedIps, address.Ip.String())
		}
		slices.Sort(storedIps)
		if !slices.Equal(storedIps, []string{"127.0.0.1", "::1"}) {
			t.Fatalf("stored addresses = %v, want both families", storedIps)
		}

		clientAttempts := testAccountActionAttempts(t, ctx, *v4Session.ByJwt.ClientId)
		connect.AssertEqual(t, clientAttempts["extender_activate_v4"], 1)
		connect.AssertEqual(t, clientAttempts["extender_activate_v6"], 2)

		// the record of the last activation names both families
		result, err := ExtenderActivate(fixture.activateArgs(), v4Session)
		if err != nil {
			t.Fatal(err)
		}
		body := verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		recordIps := []string{}
		for _, address := range body.Addresses {
			recordIps = append(recordIps, address.Ip)
		}
		slices.Sort(recordIps)
		if !slices.Equal(recordIps, []string{"127.0.0.1", "::1"}) {
			t.Fatalf("record addresses = %v, want both families", recordIps)
		}
	})
}
