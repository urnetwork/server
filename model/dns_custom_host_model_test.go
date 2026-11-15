//go:build sidecar_pending

package model

// §12.8's custom hosts (dns_custom_host_model.go; DNS-M6-02, DNS-M7-03): refused without Team, off a verified
// domain or at an apex or system owner; strong mode pending and out of the map; weak mode only with its
// disclosure; each of certs' verdicts at setup and later with its admin event; removal and a domain's removal
// withdrawing rows; an owner's 65th host refused naming the limit; a verified domain's acme_token, fixed and
// shown, which certs' challenge route takes; and a strict-mode MX host mapped for mxedge with nothing published.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// A records world whose network is a team, or not, with a verified domain.
type testDnsCustomHostWorld struct {
	*testDnsRecordsWorld
	domain string
}

// Holds the world's network to Team, or not, for the test.
func testDnsSetTeam(t testing.TB, team map[server.Id]bool) {
	t.Helper()
	previous := dnsRequireTeamTierInTx
	dnsRequireTeamTierInTx = func(ctx context.Context, tx server.PgTx, networkId server.Id) error {
		if team[networkId] {
			return nil
		}
		return requireTeamTierOf(NetworkTierFree)
	}
	t.Cleanup(func() { dnsRequireTeamTierInTx = previous })
}

// A domain row of the network, verified or not, with an acme_token once verified.
func testDnsNetworkDomain(t testing.TB, ctx context.Context, networkId server.Id, domain string, verified bool) string {
	t.Helper()
	acmeToken := ""
	server.Tx(ctx, func(tx server.PgTx) {
		var verifyTime *time.Time
		var token *string
		if verified {
			now := server.NowUtc()
			verifyTime = &now
			minted, err := newNetworkDomainAcmeToken()
			server.Raise(err)
			acmeToken = minted
			token = &minted
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_domain (network_id, domain, challenge, verify_time, acme_token) VALUES ($1, $2, $3, $4, $5)`,
			networkId,
			domain,
			"challenge",
			verifyTime,
			token,
		))
	})
	return acmeToken
}

func newTestDnsCustomHostWorld(t testing.TB, ctx context.Context) *testDnsCustomHostWorld {
	t.Helper()
	world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
	testDnsSetTeam(t, map[server.Id]bool{world.networkId: true})
	domain := "corp" + strings.ReplaceAll(world.networkId.String()[24:], "-", "") + ".example"
	testDnsNetworkDomain(t, ctx, world.networkId, domain, true)
	return &testDnsCustomHostWorld{testDnsRecordsWorld: world, domain: domain}
}

// An add as the network's credential sends it.
func (self *testDnsCustomHostWorld) add(ctx context.Context, add *sidecar.DnsCustomHostAddRequest) (*sidecar.DnsCustomHostAnswer, error) {
	return AddDnsCustomHost(ctx, &DnsCustomHostRequest{Root: self.root, NetworkId: self.networkId, Add: add, ActorKind: AdminEventActorSystem})
}

// The network's row for host, nil for none.
func (self *testDnsCustomHostWorld) row(ctx context.Context, host string) *DnsCustomHostRow {
	var row *DnsCustomHostRow
	server.Db(ctx, func(conn server.PgConn) {
		if rows := getDnsCustomHostRowsInTx(ctx, conn, `host = $1`, host); 0 < len(rows) {
			row = rows[0]
		}
	})
	return row
}

// Whether the active map holds host.
func testDnsCustomHostMapped(ctx context.Context, host string) bool {
	for _, row := range DnsCustomHostActiveRows(ctx) {
		if row.Host == host {
			return true
		}
	}
	return false
}

// The results of the network's admin events of action, oldest first.
func testDnsAdminEventResults(ctx context.Context, networkId server.Id, action string) []string {
	results := []string{}
	for _, event := range GetNetworkAdminEvents(ctx, networkId, 0, 0) {
		if event.Action == action {
			results = append(results, event.Result)
		}
	}
	return results
}

// Refused, with nothing written: a network that is not a team, a host under no verified domain or under an
// unverified one, the zone's apex as owner, * and a system owner, an owner outside the label's zone, a mode
// that is neither, and a host under the root.
func TestDnsCustomHostAddRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		owner := "shop." + world.apex
		testDnsNetworkDomain(t, ctx, world.networkId, "pending"+world.domain, false)
		cases := []struct {
			name   string
			add    *sidecar.DnsCustomHostAddRequest
			status int
		}{
			{name: "unverified domain", add: &sidecar.DnsCustomHostAddRequest{Host: "www.pending" + world.domain, Owner: owner}, status: http.StatusForbidden},
			{name: "no domain", add: &sidecar.DnsCustomHostAddRequest{Host: "www.other.example", Owner: owner}, status: http.StatusForbidden},
			{name: "apex owner", add: &sidecar.DnsCustomHostAddRequest{Host: "www." + world.domain, Owner: world.apex}, status: http.StatusBadRequest},
			{name: "wildcard owner", add: &sidecar.DnsCustomHostAddRequest{Host: "www." + world.domain, Owner: "*." + world.apex}, status: http.StatusBadRequest},
			{name: "system owner", add: &sidecar.DnsCustomHostAddRequest{Host: "www." + world.domain, Owner: "b." + world.apex}, status: http.StatusBadRequest},
			{name: "burner owner", add: &sidecar.DnsCustomHostAddRequest{Host: "www." + world.domain, Owner: "maple-1234." + world.apex}, status: http.StatusBadRequest},
			{name: "deep owner", add: &sidecar.DnsCustomHostAddRequest{Host: "www." + world.domain, Owner: "a.shop." + world.apex}, status: http.StatusBadRequest},
			{name: "other zone", add: &sidecar.DnsCustomHostAddRequest{Host: "www." + world.domain, Owner: "shop.otherlabel." + world.root}, status: http.StatusBadRequest},
			{name: "mode", add: &sidecar.DnsCustomHostAddRequest{Host: "www." + world.domain, Owner: owner, Mode: "medium"}, status: http.StatusBadRequest},
			{name: "under the root", add: &sidecar.DnsCustomHostAddRequest{Host: "x." + strings.TrimSuffix(world.root, "."), Owner: owner}, status: http.StatusBadRequest},
			{name: "weak without disclosure", add: &sidecar.DnsCustomHostAddRequest{Host: "www." + world.domain, Owner: owner, Mode: sidecar.DnsCustomHostModeWeak}, status: http.StatusBadRequest},
			{name: "weak apex", add: &sidecar.DnsCustomHostAddRequest{Host: world.domain, Owner: owner, Mode: sidecar.DnsCustomHostModeWeak, WeakDisclosureAccepted: true}, status: http.StatusBadRequest},
		}
		for _, c := range cases {
			if _, err := world.add(ctx, c.add); testDnsApiError(t, err).Status != c.status {
				t.Errorf("%s: %v, want %d", c.name, err, c.status)
			}
		}
		notTeam := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		testDnsNetworkDomain(t, ctx, notTeam.networkId, "solo"+world.domain, true)
		_, err := AddDnsCustomHost(ctx, &DnsCustomHostRequest{
			Root:      notTeam.root,
			NetworkId: notTeam.networkId,
			Add:       &sidecar.DnsCustomHostAddRequest{Host: "www.solo" + world.domain, Owner: "shop." + notTeam.apex},
		})
		if apiError := testDnsApiError(t, err); apiError.Status != http.StatusPaymentRequired || apiError.Refusal == nil || apiError.Refusal.LimitKey != TeamTierLimitKey {
			t.Errorf("a network that is not a team: %v", apiError)
		}
		if rows := DnsCustomHostActiveRows(ctx); len(rows) != 0 || world.row(ctx, "www."+world.domain) != nil {
			t.Errorf("a refusal left rows %+v", rows)
		}
	})
}

// Strong mode, every apex's, commits pending and unmapped; certs' passing walk activates it, mapped with its
// chain checked; a later failure leaves it mapped with the flag cleared, an alarming event and the authority
// notice owed; a later pass sets the flag again. A failing first walk refuses the host, removing its row.
func TestDnsCustomHostStrongModeVerdicts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		owner := "shop." + world.apex
		for _, host := range []string{world.domain, "www." + world.domain} {
			answer, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: host, Owner: owner})
			if err != nil {
				t.Fatal(err)
			}
			if got := answer.CustomHost; got.State != sidecar.DnsCustomHostStatePending || got.CaaMode != sidecar.DnsCustomHostModeStrong || got.EffectiveCaaChainOk {
				t.Fatalf("%s added as %+v", host, got)
			}
			if testDnsCustomHostMapped(ctx, host) {
				t.Fatalf("pending %s is mapped", host)
			}
			// adding it again answers it as it stands
			if again, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: host, Owner: owner}); err != nil || again.CustomHost.Host != host {
				t.Fatalf("adding %s again: %+v, %v", host, again, err)
			}
		}

		apply := func(host string, pass bool) DnsCustomHostOutcome {
			outcome, err := DnsCustomHostResultApply(ctx, &DnsCustomHostResult{Host: host, Pass: pass})
			if err != nil {
				t.Fatalf("%s: %v", host, err)
			}
			return outcome
		}
		www := "www." + world.domain
		if outcome := apply(www, true); outcome != DnsCustomHostOutcomeActivated {
			t.Fatalf("a passing first walk: %s", outcome)
		}
		if row := world.row(ctx, www); row.State != sidecar.DnsCustomHostStateActive || !row.EffectiveCaaChainOk || !testDnsCustomHostMapped(ctx, www) {
			t.Fatalf("after a pass the row is %+v", row)
		}
		if outcome := apply(www, false); outcome != DnsCustomHostOutcomeExposed {
			t.Fatalf("a failing later check: %s", outcome)
		}
		if row := world.row(ctx, www); row.State != sidecar.DnsCustomHostStateActive || row.EffectiveCaaChainOk || !testDnsCustomHostMapped(ctx, www) {
			t.Fatalf("after a later failure the row is %+v", row)
		}
		owed := 0
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT count(*) FROM account_message_outbox WHERE network_id = $1 AND template_name = $2`,
				world.networkId,
				SecurityNetworkAuthorityTemplateName,
			).Scan(&owed))
		})
		if owed == 0 {
			t.Error("a later failure owed no authority notice")
		}
		if outcome := apply(www, true); outcome != DnsCustomHostOutcomeChecked {
			t.Fatalf("a passing later check: %s", outcome)
		}
		if row := world.row(ctx, www); !row.EffectiveCaaChainOk {
			t.Fatalf("after a later pass the row is %+v", row)
		}
		if outcome := apply(world.domain, false); outcome != DnsCustomHostOutcomeRefused {
			t.Fatalf("a failing first walk: %s", outcome)
		}
		if row := world.row(ctx, world.domain); row != nil {
			t.Fatalf("a refused host left %+v", row)
		}
		if _, err := DnsCustomHostResultApply(ctx, &DnsCustomHostResult{Host: world.domain, Pass: true}); !errors.Is(err, ErrDnsCustomHostUnknown) {
			t.Errorf("a verdict for a removed host: %v", err)
		}
		results := testDnsAdminEventResults(ctx, world.networkId, AdminEventActionDnsCustomHostCaa)
		want := []string{AdminEventResultOk, AdminEventResultDnsCustomHostExposed, AdminEventResultOk, AdminEventResultDnsCustomHostOperatorAlias}
		if strings.Join(results, ",") != strings.Join(want, ",") {
			t.Errorf("the verdicts' events are %v, want %v", results, want)
		}
	})
}

// Weak mode is taken with its disclosure, active and mapped at once, recorded as weak in its admin event, and
// certs judges it neither way; removal withdraws a row, and a domain's removal its hosts.
func TestDnsCustomHostWeakModeAndRemovals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		weak := "blog." + world.domain
		answer, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: weak, Owner: "shop." + world.apex, Mode: sidecar.DnsCustomHostModeWeak, WeakDisclosureAccepted: true})
		if err != nil {
			t.Fatal(err)
		}
		if answer.CustomHost.State != sidecar.DnsCustomHostStateActive || answer.Disclosure != sidecar.DnsCustomHostWeakDisclosure || !testDnsCustomHostMapped(ctx, weak) {
			t.Fatalf("weak mode answered %+v", answer)
		}
		if results := testDnsAdminEventResults(ctx, world.networkId, AdminEventActionDnsCustomHostAdd); len(results) != 1 || results[0] != AdminEventResultDnsCustomHostWeak {
			t.Errorf("weak mode's events %v", results)
		}
		if _, err := DnsCustomHostResultApply(ctx, &DnsCustomHostResult{Host: weak, Pass: false}); !errors.Is(err, ErrDnsCustomHostUnknown) {
			t.Errorf("a verdict for a weak host: %v", err)
		}
		removed, err := RemoveDnsCustomHost(ctx, &DnsCustomHostRequest{Root: world.root, NetworkId: world.networkId, Remove: &sidecar.DnsCustomHostRemoveRequest{Host: weak}})
		if err != nil || !removed.Removed || world.row(ctx, weak) != nil || testDnsCustomHostMapped(ctx, weak) {
			t.Fatalf("removal answered %+v, %v", removed, err)
		}
		if again, err := RemoveDnsCustomHost(ctx, &DnsCustomHostRequest{Root: world.root, NetworkId: world.networkId, Remove: &sidecar.DnsCustomHostRemoveRequest{Host: weak}}); err != nil || again.Removed {
			t.Fatalf("removing again answered %+v, %v", again, err)
		}
		for _, host := range []string{"a." + world.domain, "b." + world.domain} {
			if _, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: host, Owner: "shop." + world.apex}); err != nil {
				t.Fatal(err)
			}
		}
		removedCount := 0
		server.Tx(ctx, func(tx server.PgTx) {
			removedCount = DnsRemoveDomainCustomHostsInTx(ctx, tx, world.networkId, world.domain)
		})
		if removedCount != 2 || world.row(ctx, "a."+world.domain) != nil {
			t.Errorf("the domain's removal took %d hosts", removedCount)
		}
	})
}

// An owner maps at most CertifiedNameMaxCount custom hosts: the 65th is refused naming the limit, and another
// owner still takes one.
func TestDnsCustomHostOwnerLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		owner := "shop." + world.apex
		limit := dnsCustomHostOwnerMaxCount()
		if limit != 64 {
			t.Fatalf("the limit is %d", limit)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			for index := range limit {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO network_dns_custom_host (host, root, network_id, owner, state, caa_mode, create_time)
						VALUES ($1, $2, $3, $4, 'pending', 'strong', $5)
					`,
					fmt.Sprintf("h%d.%s", index, world.domain),
					world.root,
					world.networkId,
					owner,
					server.NowUtc(),
				))
			}
		})
		_, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: "over." + world.domain, Owner: owner})
		if apiError := testDnsApiError(t, err); apiError.Status != http.StatusConflict || !strings.Contains(apiError.Message, "certified_name_max_count") {
			t.Errorf("the 65th host: %v", apiError)
		}
		if _, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: "over." + world.domain, Owner: "other." + world.apex}); err != nil {
			t.Errorf("another owner: %v", err)
		}
	})
}

// QD-175: a domain's first verification mints its acme_token, the same at every read and shown in GET
// /network/dns as <token>.acme.<root>, and an unverified domain has none; certs' challenge route writes a
// verified token's target at the name itself in <root>'s own rows, and refuses any other token.
func TestDnsCustomDomainAcmeToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		t.Cleanup(Testing_SetDnsChallengesPerNameHour(10))
		networkId, ownerSession := newTestDomainTeam(t, ctx)
		domains := newTestDomains()
		added, err := domains.add(&NetworkDomainArgs{Domain: "tokens.example"}, ownerSession)
		if err != nil || added.Error != nil {
			t.Fatalf("add: %v %+v", err, added)
		}
		if _, err := domains.add(&NetworkDomainArgs{Domain: "unproven.example"}, ownerSession); err != nil {
			t.Fatal(err)
		}
		readTokens := func() map[string]*string {
			tokens := map[string]*string{}
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, `SELECT domain, acme_token FROM network_domain WHERE network_id = $1`, networkId)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var domain string
						var token *string
						server.Raise(result.Scan(&domain, &token))
						tokens[domain] = token
					}
				})
			})
			return tokens
		}
		if tokens := readTokens(); tokens["tokens.example"] != nil {
			t.Fatalf("an unverified domain has token %q", *tokens["tokens.example"])
		}
		domains.advance(time.Minute)
		domains.resolver.set("_urnetwork-challenge.tokens.example", added.Domain.TxtValue)
		if verified, err := domains.verify(&NetworkDomainArgs{Domain: "tokens.example"}, ownerSession); err != nil || verified.Error != nil {
			t.Fatalf("verify: %v %+v", err, verified)
		}
		tokens := readTokens()
		token := tokens["tokens.example"]
		if token == nil || len(*token) != 26 || *token != strings.ToLower(*token) || tokens["unproven.example"] != nil {
			t.Fatalf("the tokens after verification: %v", tokens)
		}
		// verifying again keeps it
		domains.advance(time.Minute)
		if _, err := domains.verify(&NetworkDomainArgs{Domain: "tokens.example"}, ownerSession); err != nil {
			t.Fatal(err)
		}
		if again := readTokens()["tokens.example"]; again == nil || *again != *token {
			t.Fatalf("the token moved from %s", *token)
		}

		root := testDnsRoot(t, ctx)
		label, err := EnsureNetworkLabel(ctx, networkId, root)
		if err != nil || label == "" {
			t.Fatal(err)
		}
		networkDns, err := GetNetworkDns(ctx, root, networkId)
		if err != nil {
			t.Fatal(err)
		}
		target := *token + ".acme." + strings.TrimSuffix(root, ".")
		if len(networkDns.CustomDomains) != 1 || networkDns.CustomDomains[0].Domain != "tokens.example" || networkDns.CustomDomains[0].AcmeChallengeTarget != target {
			t.Fatalf("GET /network/dns shows %+v", networkDns.CustomDomains)
		}

		written, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: target, Values: []string{"delegated-digest"}})
		if err != nil {
			t.Fatal(err)
		}
		served := testDnsSnapshotRows(t, ctx, root)[dnsservice.DnsRecordKey{Owner: target + ".", Type: dns.TypeTXT}]
		if served.Kind != dnsservice.DnsRecordKindChallenge || len(served.Rdata) != 1 || served.Rdata[0] != `"delegated-digest"` {
			t.Fatalf("serial %d serves %+v", written.Serial, served)
		}
		for _, name := range []string{"unknowntoken.acme." + root, "a." + target} {
			if _, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: name, Values: []string{"x"}}); !errors.Is(err, ErrDnsChallengeName) {
				t.Errorf("a challenge at %s: %v", name, err)
			}
		}
		// the rate holds for a target as for a label
		t.Cleanup(Testing_SetDnsChallengesPerNameHour(1))
		if _, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: target, Values: []string{"again"}}); !errors.Is(err, ErrDnsChallengeRate) {
			t.Errorf("a second write under a cap of one: %v", err)
		}
	})
}

// DNS-M7-03's strict mode: the customer's MX host, mapped to the network's mail host, is active at once for
// mxedge and never in the TLS map, and dns writes nothing for it, since only the customer's own signed zone
// carries strict mode's claim.
func TestDnsStrictMxHostPublishesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		before := world.serial(t, ctx)
		mxHost := "mx1." + world.domain
		answer, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: mxHost, Owner: "mx." + world.apex})
		if err != nil {
			t.Fatal(err)
		}
		if answer.CustomHost.State != sidecar.DnsCustomHostStateActive {
			t.Fatalf("a strict-mode MX host added as %+v", answer.CustomHost)
		}
		if after := world.serial(t, ctx); after != before {
			t.Errorf("a strict-mode MX host took serial %d", after)
		}
		if testDnsCustomHostMapped(ctx, mxHost) {
			t.Error("a strict-mode MX host is in the TLS map")
		}
		strict := DnsStrictMxHostRows(ctx, world.networkId)
		if len(strict) != 1 || strict[0].Host != mxHost || !strict[0].StrictMx() {
			t.Fatalf("mxedge's rows %+v", strict)
		}
		if _, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: "mx2." + world.domain, Owner: "mx." + world.apex, Mode: sidecar.DnsCustomHostModeWeak, WeakDisclosureAccepted: true}); testDnsApiError(t, err).Status != http.StatusBadRequest {
			t.Errorf("a weak strict-mode MX host: %v", err)
		}
	})
}
