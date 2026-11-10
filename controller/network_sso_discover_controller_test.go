//go:build sidecar_pending

package controller

// §7.6's discovery (TEAM-M6-06) against the fake OIDC provider, on Postgres
// with MG6: a verified domain's address begins a real flow at its team's IdP,
// and everything else answers alike.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Writes a domain row for the team, verified or not, as the routes would.
func testWriteDomainRow(t testing.TB, team *testSsoTeam, networkId server.Id, domain string, verified bool) {
	t.Helper()
	var verifyTime *time.Time
	if verified {
		now := server.NowUtc()
		verifyTime = &now
	}
	server.Tx(team.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			team.ctx,
			`INSERT INTO network_domain (network_id, domain, challenge, verify_time, check_time) VALUES ($1, $2, $3, $4, $4)`,
			networkId, domain, "synthetic-challenge", verifyTime,
		))
	})
}

// Discovery answers alike for unknown domains: an address that does not
// parse, a domain no network added, an unverified one, a verified one whose
// team has no IdP and an existing user's address at none of them all answer
// the same bytes, while a verified domain's address begins a flow at its
// team's IdP that the callback finishes, linked to the caller's own sign-in.
func TestSsoDiscoverRoutesOnlyAVerifiedDomain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		team := newTestSsoTeam(t)
		settings := DefaultNetworkSsoDiscoverSettings()
		settings.AddressLimit = 0
		testWriteDomainRow(t, team, team.teamId, "routed.example", true)
		testWriteDomainRow(t, team, team.teamId, "unverified.example", false)
		noIdpTeamId := server.NewId()
		model.Testing_CreateNetwork(team.ctx, noIdpTeamId, "team-"+noIdpTeamId.String(), server.NewId())
		server.Tx(team.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(team.ctx, `INSERT INTO network_team(network_id) VALUES($1)`, noIdpTeamId))
		})
		testWriteDomainRow(t, team, noIdpTeamId, "noidp.example", true)
		existingUserId := server.NewId()
		existingUserAuth := model.Testing_CreateNetwork(team.ctx, server.NewId(), "existing-account", existingUserId)

		discover := func(email string, clientSession *session.ClientSession) (*NetworkSsoDiscoverResult, error) {
			if clientSession == nil {
				clientSession = session.NewLocalClientSession(team.ctx, "192.0.2.51:1", nil)
				defer clientSession.Cancel()
			}
			return networkSsoDiscoverWithSettings(&NetworkSsoDiscoverArgs{Email: email}, clientSession, settings)
		}
		answers := map[string]bool{}
		for _, email := range []string{
			"",
			"not-an-address",
			"someone@unknown.example",
			"someone@unverified.example",
			"someone@noidp.example",
			"someone@sub.routed.example",
			existingUserAuth,
		} {
			result, err := discover(email, nil)
			if err != nil {
				t.Fatalf("%q: %v", email, err)
			}
			wire, err := json.Marshal(result)
			connect.AssertEqual(t, err, nil)
			answers[string(wire)] = true
		}
		if len(answers) != 1 || !answers["{}"] {
			t.Fatalf("unrouted addresses answer apart: %v", answers)
		}

		// a verified domain's address, whoever holds it, begins a flow at the IdP
		passwordUserId, personalNetworkId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(team.ctx, personalNetworkId, "password-account", passwordUserId)
		passwordSession := session.NewLocalClientSession(team.ctx, "192.0.2.52:1", session.NewByJwt(personalNetworkId, passwordUserId, "personal", false, false))
		defer passwordSession.Cancel()
		routed, err := discover("nobody-holds-this@Routed.Example", passwordSession)
		if err != nil || !strings.HasPrefix(routed.AuthorizationUrl, team.provider.server.URL+"/authorize?") {
			t.Fatalf("a verified domain: %v %+v", err, routed)
		}
		state, code := team.provider.authorize(t, routed.AuthorizationUrl, "employee-9", "employee@routed.example")
		finished := team.callback(t, state, code)
		if finished.Error != nil || !finished.Linked || !finished.Pending || finished.NetworkId == nil || *finished.NetworkId != team.teamId {
			t.Fatalf("the flow discovery began: %+v", finished)
		}
		// with no sign-in of the caller's own, the identity of another person links to nobody
		unlinked, err := discover("someone@routed.example", nil)
		connect.AssertEqual(t, err, nil)
		state, code = team.provider.authorize(t, unlinked.AuthorizationUrl, "employee-10", "other@routed.example")
		if finished := team.callback(t, state, code); finished.Error == nil || finished.ByJwt != "" {
			t.Fatalf("an unlinked identity signed in through discovery: %+v", finished)
		}

		// while no flow can be kept, every address is answered 503 alike
		model.SetNetworkSsoFlowStore(nil)
		for _, email := range []string{"someone@routed.example", "someone@unknown.example", ""} {
			result, err := discover(email, nil)
			if result != nil || err == nil || !strings.HasPrefix(err.Error(), "503 ") {
				t.Errorf("%q with no flow store answered %+v %v", email, result, err)
			}
		}
	})
}

// A caller address is limited whatever it asks about: past the limit every
// address it names, routed or not, answers 429, and another caller is served.
func TestSsoDiscoverIsLimitedPerAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		team := newTestSsoTeam(t)
		testWriteDomainRow(t, team, team.teamId, "routed.example", true)
		settings := DefaultNetworkSsoDiscoverSettings()
		settings.AddressLimit = 2
		// addresses of this run's own, so no other run's history counts, in
		// two networks, since addresses are limited by prefix
		idBytes := server.NewId().Bytes()
		group := func(i int) uint16 { return uint16(idBytes[i])<<8 | uint16(idBytes[i+1]) }
		callerAddress := fmt.Sprintf("[2001:db8:%x:%x::1]:1", group(12), group(14))
		otherAddress := fmt.Sprintf("[2001:db8:%x:%x::1]:1", group(12)^0xffff, group(14))
		discover := func(address string, email string) error {
			clientSession := session.NewLocalClientSession(team.ctx, address, nil)
			defer clientSession.Cancel()
			_, err := networkSsoDiscoverWithSettings(&NetworkSsoDiscoverArgs{Email: email}, clientSession, settings)
			return err
		}
		connect.AssertEqual(t, discover(callerAddress, "someone@unknown.example"), nil)
		connect.AssertEqual(t, discover(callerAddress, "someone@routed.example"), nil)
		for _, email := range []string{"someone@routed.example", "someone@unknown.example"} {
			if err := discover(callerAddress, email); err == nil || !strings.HasPrefix(err.Error(), "429 ") {
				t.Fatalf("%q past the limit answered %v", email, err)
			}
		}
		connect.AssertEqual(t, discover(otherAddress, "someone@unknown.example"), nil)
	})
}
