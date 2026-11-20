//go:build sidecar_pending

package model

// §14.6's Team custom domains, their model half, on a private database (SMTP-M9-34; D-SMTP-15, D-SMTP-5, §20.7,
// QD-1080, QD-1129): the gate's facts carry a network's verified domains, their withhold time and, while strict mode's
// envelope is on, as it is by default, the strict ones, which GET /mail/status names alike; POST /smtp/dkim-keys takes
// a selector under a verified domain only while
// the network may send From it, and writes the network's delegated _domainkey row with it; the delegated zone answers
// the network's own selectors and no other network's, so at a receiver's checker its mail passes DMARC at adkim=s and
// another network's fails; and removing or dropping the domain withdraws the delegation, so a network that verifies it
// next answers alone. Every name, key and address is synthetic.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// The world's custom domain, and the customer's DMARC record for it (§14.6: adkim=s).
const (
	testSmtpCustomDomain       = "brand.example"
	testSmtpCustomDomainPolicy = "v=DMARC1; p=reject; adkim=s; aspf=s"
)

// When the fixture's signatures are made: mox, whose verifier the checker links, reads x= against the wall clock.
var testSmtpCustomDomainSignTime = time.Date(2200, time.January, 1, 0, 0, 0, 0, time.UTC)

// Puts the custom-domain rows' tier and downgrade clock for the test: tier for every network, and the downgrade time
// downgradeTime when left.
func testSmtpCustomDomainClock(t testing.TB, tier NetworkTier, downgradeTime time.Time, left bool) {
	t.Helper()
	previousTier, previousDowngrade := smtpCustomDomainTierOf, smtpCustomDomainDowngradeTimeOf
	smtpCustomDomainTierOf = func(ctx context.Context, networkId server.Id) NetworkTier {
		return tier
	}
	smtpCustomDomainDowngradeTimeOf = func(ctx context.Context, networkId server.Id) (time.Time, NetworkTier, bool) {
		return downgradeTime, NetworkTierTeam, left
	}
	t.Cleanup(func() {
		smtpCustomDomainTierOf, smtpCustomDomainDowngradeTimeOf = previousTier, previousDowngrade
	})
}

// Verifies domain for the network, as POST /network/domain-verify leaves its row.
func testSmtpVerifyDomain(ctx context.Context, networkId server.Id, domain string) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_domain (network_id, domain, challenge, verify_time, check_time) VALUES ($1, $2, $3, $4, $4)`,
			networkId, domain, "challenge-"+server.NewId().String(), server.NowUtc(),
		))
	})
}

// A selector over a key of its own, as a sending sidecar holds one for a domain.
type testSmtpCustomDomainSigner struct {
	selector string
	domain   string
	key      *rsa.PrivateKey
}

func newTestSmtpCustomDomainSigner(t testing.TB, domain string) *testSmtpCustomDomainSigner {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &testSmtpCustomDomainSigner{selector: smtpTestSelector(), domain: domain, key: key}
}

func (self *testSmtpCustomDomainSigner) DkimSelector() string { return self.selector }
func (self *testSmtpCustomDomainSigner) DkimDomain() string   { return self.domain }
func (self *testSmtpCustomDomainSigner) SignDkim(digest []byte) ([]byte, error) {
	return rsa.SignPKCS1v15(nil, self.key, crypto.SHA256, digest)
}

// The signer's public key as POST /smtp/dkim-keys carries it.
func (self *testSmtpCustomDomainSigner) publicKey(t testing.TB) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&self.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// Publishes the signer's selector under its domain as caller, mail_send granted.
func testSmtpPublishSigner(t testing.TB, ctx context.Context, caller *SmtpDkimKeysCaller, signer *testSmtpCustomDomainSigner) (*sidecar.SmtpDkimKeysResult, error) {
	t.Helper()
	return PublishSmtpDkimKeys(ctx, caller, &sidecar.SmtpDkimKeysArgs{
		Publish: []*sidecar.SmtpDkimKeyPublish{{Selector: signer.selector, Domain: signer.domain, PublicKey: signer.publicKey(t)}},
	}, smtpTestAllowed, server.NowUtc(), DefaultSmtpApiSettings())
}

// The edge's view of root as both dns edges serve it.
func testSmtpCustomDomainView(t testing.TB, ctx context.Context, root string) *dnsservice.ZoneView {
	t.Helper()
	view, _ := (&testDnsRecordsWorld{root: root}).view(t, ctx)
	return view
}

// What the delegated zone answers for a TXT at name, and the TXT's text when it answers one.
func testSmtpDelegatedAnswer(t testing.TB, view *dnsservice.ZoneView, name string) (rcode int, text string) {
	t.Helper()
	name = dns.CanonicalName(name)
	apex, labelApex, ok := view.Delegated().Lookup(name)
	if !ok {
		return -1, ""
	}
	query := &dns.Msg{}
	query.SetQuestion(name, dns.TypeTXT)
	query.SetEdns0(1232, true)
	reply := dnsservice.AnswerDelegated(&dnsservice.DnsQuery{Message: query}, view, apex, labelApex)
	if reply == nil {
		t.Fatalf("no answer at %s", name)
	}
	if len(reply.Answer) == 1 {
		if txt, ok := reply.Answer[0].(*dns.TXT); ok {
			return reply.Rcode, strings.Join(txt.Txt, "")
		}
	}
	return reply.Rcode, ""
}

// Another network of the world's root, with mail on, and a sending sidecar of it.
func newTestSmtpOtherNetwork(t testing.TB, ctx context.Context, root string) (caller *SmtpDkimKeysCaller, domain string) {
	t.Helper()
	networkId, apex := testDnsNetwork(t, ctx, root)
	if _, err := DnsSetMailRecords(ctx, networkId, root, true); err != nil {
		t.Fatal(err)
	}
	return &SmtpDkimKeysCaller{NetworkId: networkId, ClientId: server.NewId(), SidecarId: server.NewId()}, strings.TrimSuffix(apex, ".")
}

// The gate's facts carry the verified domains and no unverified one, the withhold time from the downgrade clock, and
// the strict domains while strict mode's envelope is on, as it is by default: those at or above an active strict-mode
// MX host. GET /mail/status names the same set under the same settings, and none while the envelope is off.
func TestSmtpGateFactsCarryTheCustomDomains(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newSmtpApiWorld(t, ctx)
		testSmtpVerifyDomain(ctx, world.networkId, testSmtpCustomDomain)
		testSmtpVerifyDomain(ctx, world.networkId, "plain.example")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_domain (network_id, domain, challenge) VALUES ($1, 'pending.example', 'c')`, world.networkId))
			// the customer's own MX host for the custom domain, mapped to the network's mail host (§12.8 step 4)
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_dns_custom_host (host, root, network_id, owner, state, caa_mode, effective_caa_chain_ok, create_time)
					VALUES ($1, $2, $3, $4, $5, $6, false, $7)
				`,
				"mx1."+testSmtpCustomDomain, world.root, world.networkId, "mx."+world.apex, sidecar.DnsCustomHostStateActive, sidecar.DnsCustomHostModeStrong, server.NowUtc(),
			))
		})
		downgradeTime := server.NowUtc().Add(-13 * 24 * time.Hour).Truncate(time.Millisecond)
		testSmtpCustomDomainClock(t, NetworkTierPro, downgradeTime, true)

		facts, err := GetSmtpGateFacts(ctx, world.networkId, world.root)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(facts.VerifiedDomains, []string{testSmtpCustomDomain, "plain.example"}) {
			t.Fatalf("verified %v", facts.VerifiedDomains)
		}
		if !DefaultSmtpCustomDomainSettings().StrictEnvelope || !slices.Equal(facts.StrictDomains, []string{testSmtpCustomDomain}) {
			t.Fatalf("strict %v", facts.StrictDomains)
		}
		if want := downgradeTime.Add(DefaultSmtpCustomDomainSettings().DowngradeGrace); !facts.CustomDomainWithholdTime.Equal(want) {
			t.Fatalf("withhold at %v, want %v", facts.CustomDomainWithholdTime, want)
		}
		status, err := GetMailStatus(ctx, world.networkId)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(status.StrictDomains, facts.StrictDomains) {
			t.Fatalf("GET /mail/status names %v and the gate %v", status.StrictDomains, facts.StrictDomains)
		}

		// with the envelope off, neither names any, so every custom domain keeps b.<net>.<root> at both ends
		settings := DefaultSmtpCustomDomainSettings()
		settings.StrictEnvelope = false
		facts, err = GetSmtpGateFactsWithSettings(ctx, world.networkId, world.root, settings)
		if err != nil {
			t.Fatal(err)
		}
		if facts.StrictDomains == nil || len(facts.StrictDomains) != 0 {
			t.Fatalf("strict mode's envelope is off, yet the feed names %v", facts.StrictDomains)
		}
		status, err = GetMailStatusWithSettings(ctx, world.networkId, settings)
		if err != nil || status.StrictDomains == nil || len(status.StrictDomains) != 0 {
			t.Fatalf("strict mode's envelope is off, yet GET /mail/status names %+v, %v", status, err)
		}

		// a network that left no tier carries no withhold time
		testSmtpCustomDomainClock(t, NetworkTierTeam, time.Time{}, false)
		facts, err = GetSmtpGateFacts(ctx, world.networkId, world.root)
		if err != nil || !facts.CustomDomainWithholdTime.IsZero() {
			t.Fatalf("facts %+v, %v", facts, err)
		}
	})
}

func TestSmtpStrictCustomDomainsStandAtOrAboveAnMxHost(t *testing.T) {
	domains := []string{"brand.example", "mail.brand.example", "other.example", "brand.example.net"}
	cases := []struct {
		hosts []string
		want  []string
	}{
		{hosts: nil, want: []string{}},
		{hosts: []string{"mx.brand.example"}, want: []string{"brand.example"}},
		{hosts: []string{"mx.mail.brand.example"}, want: []string{"brand.example", "mail.brand.example"}},
		{hosts: []string{"other.example"}, want: []string{"other.example"}},
		// a host only ending in a domain's name is not under it
		{hosts: []string{"mx.notbrand.example", "mx.brand.example.net.example"}, want: []string{}},
	}
	for _, c := range cases {
		if got := smtpStrictCustomDomains(domains, c.hosts); !slices.Equal(got, c.want) {
			t.Errorf("%v: %v, want %v", c.hosts, got, c.want)
		}
	}
}

// A verified domain's selector is published on the team tier, its record at the network's apex, and the network's
// delegated row names the domain's _domainkey, so dns answers the selector under it; the same body again writes
// nothing.
func TestSmtpDkimKeysPublishUnderATeamCustomDomain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newSmtpApiWorld(t, ctx)
		testSmtpVerifyDomain(ctx, world.networkId, testSmtpCustomDomain)
		testSmtpCustomDomainClock(t, NetworkTierTeam, time.Time{}, false)
		signer := newTestSmtpCustomDomainSigner(t, testSmtpCustomDomain)
		result, err := testSmtpPublishSigner(t, ctx, world.caller, signer)
		if err != nil || !slices.Equal(result.Published, []string{signer.selector}) {
			t.Fatalf("publish %+v, %v", result, err)
		}
		key, err := FindSmtpDkimKey(ctx, signer.selector, testSmtpCustomDomain)
		if err != nil || key == nil || key.Domain != testSmtpCustomDomain {
			t.Fatalf("the row %+v, %v", key, err)
		}
		if !smtpTestServed(t, ctx, world.root, signer.selector+"._domainkey."+world.apex) {
			t.Fatal("the selector's record is not at the network's apex")
		}
		view := testSmtpCustomDomainView(t, ctx, world.root)
		if apexes := view.Delegated().Apexes(); !slices.Equal(apexes, []string{"_domainkey." + testSmtpCustomDomain + "."}) {
			t.Fatalf("delegated apexes %v", apexes)
		}
		rcode, text := testSmtpDelegatedAnswer(t, view, signer.selector+"._domainkey."+testSmtpCustomDomain)
		if rcode != dns.RcodeSuccess || !strings.HasPrefix(text, "v=DKIM1") {
			t.Fatalf("the selector under the delegated zone: %d %q", rcode, text)
		}
		serial := testDnsRootSerial(t, ctx, world.root)
		if _, err := testSmtpPublishSigner(t, ctx, world.caller, signer); err != nil || testDnsRootSerial(t, ctx, world.root) != serial {
			t.Fatalf("the same publish again: %v", err)
		}
	})
}

// A selector under a custom domain is refused whole, writing nothing, for a domain the network has not verified, for
// a verified name under the root, and past §20.7's 14 days off the team tier; inside them it is taken.
func TestSmtpDkimKeysCustomDomainRefusedUnverifiedOrWithheld(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newSmtpApiWorld(t, ctx)
		testSmtpVerifyDomain(ctx, world.networkId, testSmtpCustomDomain)
		underRoot := "sub." + world.domain
		testSmtpVerifyDomain(ctx, world.networkId, underRoot)
		refused := func(domain string) {
			t.Helper()
			signer := newTestSmtpCustomDomainSigner(t, domain)
			serial := testDnsRootSerial(t, ctx, world.root)
			_, err := testSmtpPublishSigner(t, ctx, world.caller, signer)
			apiErr := &SmtpApiError{}
			if !errors.As(err, &apiErr) || apiErr.Code != SmtpApiCodeDomain || apiErr.Status != http.StatusBadRequest {
				t.Fatalf("%s answered %v", domain, err)
			}
			if key, _ := FindSmtpDkimKey(ctx, signer.selector, domain); key != nil || testDnsRootSerial(t, ctx, world.root) != serial {
				t.Fatalf("a refused publish under %s wrote", domain)
			}
		}

		testSmtpCustomDomainClock(t, NetworkTierTeam, time.Time{}, false)
		refused("pending.example")
		refused(underRoot)

		// left team 15 days ago
		testSmtpCustomDomainClock(t, NetworkTierPro, server.NowUtc().Add(-15*24*time.Hour), true)
		refused(testSmtpCustomDomain)
		// a Pro network that never left a tier
		testSmtpCustomDomainClock(t, NetworkTierPro, time.Time{}, false)
		refused(testSmtpCustomDomain)
		// left team 13 days ago: still sending
		testSmtpCustomDomainClock(t, NetworkTierPro, server.NowUtc().Add(-13*24*time.Hour), true)
		if _, err := testSmtpPublishSigner(t, ctx, world.caller, newTestSmtpCustomDomainSigner(t, testSmtpCustomDomain)); err != nil {
			t.Fatalf("inside the 14 days: %v", err)
		}
		// the verified name under the root is in no delegated row: a root is never delegated out of itself
		if apexes := testSmtpCustomDomainView(t, ctx, world.root).Delegated().Apexes(); !slices.Equal(apexes, []string{"_domainkey." + testSmtpCustomDomain + "."}) {
			t.Fatalf("delegated %v", apexes)
		}
	})
}

// The delegated zone answers the network's own selectors alone: at a receiver's checker (MAIL-M7-08's), with the
// customer's _dmarc at adkim=s, mail signed under the network's selector passes DMARC on DKIM alone, while another
// network of the root, whose selector sits at its own apex and which may not publish under the domain, passes
// nothing for it.
func TestSmtpCustomDomainDelegationAlignsOnlyTheNetworksSelectors(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newSmtpApiWorld(t, ctx)
		testSmtpVerifyDomain(ctx, world.networkId, testSmtpCustomDomain)
		testSmtpCustomDomainClock(t, NetworkTierTeam, time.Time{}, false)
		owner := newTestSmtpCustomDomainSigner(t, testSmtpCustomDomain)
		if _, err := testSmtpPublishSigner(t, ctx, world.caller, owner); err != nil {
			t.Fatal(err)
		}

		otherCaller, otherDomain := newTestSmtpOtherNetwork(t, ctx, world.root)
		// the other network may not publish under the domain
		intruder := newTestSmtpCustomDomainSigner(t, testSmtpCustomDomain)
		if _, err := testSmtpPublishSigner(t, ctx, otherCaller, intruder); err == nil {
			t.Fatal("another network published a selector under the domain")
		}
		// a selector under its own name, with the same key, is its to publish
		ownName := &testSmtpCustomDomainSigner{selector: intruder.selector, domain: otherDomain, key: intruder.key}
		if _, err := testSmtpPublishSigner(t, ctx, otherCaller, ownName); err != nil {
			t.Fatal(err)
		}

		view := testSmtpCustomDomainView(t, ctx, world.root)
		rcode, ownerRecord := testSmtpDelegatedAnswer(t, view, owner.selector+"._domainkey."+testSmtpCustomDomain)
		if rcode != dns.RcodeSuccess || ownerRecord == "" {
			t.Fatalf("the network's selector: %d", rcode)
		}
		if rcode, text := testSmtpDelegatedAnswer(t, view, intruder.selector+"._domainkey."+testSmtpCustomDomain); rcode != dns.RcodeNameError || text != "" {
			t.Fatalf("another network's selector under the domain: %d %q", rcode, text)
		}

		// the receiver resolves the delegated zone's answers, and the envelope domain's SPF
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		zone := sidecartest.NewFakeMailZone(clock)
		zone.AddTxt("_dmarc."+testSmtpCustomDomain, testSmtpCustomDomainPolicy)
		zone.AddTxt(owner.selector+"._domainkey."+testSmtpCustomDomain, ownerRecord)
		envelopeDomain := sidecar.SmtpEnvelopeDomain(world.domain, "")
		zone.AddTxt(envelopeDomain, "v=spf1 ip4:192.0.2.0/24 -all")
		mailSettings := sidecar.DefaultMailAuthSettings()
		checker := sidecar.NewMailAuthChecker(sidecar.NewMailDnsStub(zone, clock, mailSettings), clock, mailSettings)
		check := func(signer *testSmtpCustomDomainSigner) *sidecar.MailAuthOutcome {
			t.Helper()
			wire, err := sidecar.AssembleSmtpMessage(&sidecar.SmtpAssembleArgs{
				Message: []byte("From: Brand <news@" + testSmtpCustomDomain + ">\r\nTo: someone@shop.example\r\nSubject: The catalog\r\n" +
					"Date: Sun, 08 Nov 2026 12:03:41 +0000\r\nMessage-ID: <aaaaaaaaaaaaaaaaaaaaaaaaaa@" + testSmtpCustomDomain + ">\r\n\r\nIt is out.\r\n"),
				HeaderFromDomain: testSmtpCustomDomain,
				MessageIdDomain:  testSmtpCustomDomain,
				Now:              testSmtpCustomDomainSignTime,
			})
			if err != nil {
				t.Fatal(err)
			}
			signed, err := sidecar.SmtpDkimSign(wire, &sidecar.SmtpDkimSignArgs{
				Signer:        signer,
				Issued:        &sidecar.SmtpIssuedHeaders{XUrRef: "1.ljnfuws2ljnfuws2ljnfuws2ljnfuws2li", FeedbackId: "n:t:gmztgmztgmztgmztgmztgmztgm:urnetwork"},
				SignTime:      testSmtpCustomDomainSignTime,
				ExpireTimeout: sidecar.DefaultSmtpSenderSettings().DkimExpireTimeout,
			})
			if err != nil {
				t.Fatal(err)
			}
			spki := sha256.Sum256([]byte("receiver"))
			outcome, err := checker.Check(ctx, &sidecar.MailAuthInput{
				Message:            bytes.NewReader(signed),
				Size:               int64(len(signed)),
				SenderIp:           netip.MustParseAddr("192.0.2.25"),
				EhloName:           "o1.out.sender.example",
				SniHost:            "mx." + world.domain,
				ReceiverSpkiSha256: spki[:],
				MailFrom:           []byte("bounces@" + envelopeDomain),
			})
			if err != nil {
				t.Fatal(err)
			}
			return outcome
		}
		passed := check(owner)
		if len(passed.Dkim) == 0 {
			t.Fatal("no signature was checked")
		}
		for _, result := range passed.Dkim {
			if result.GetResult() != protocol.DkimVerdict_DKIM_PASS || result.GetDomain() != testSmtpCustomDomain {
				t.Errorf("the network's selector: dkim %v", result)
			}
		}
		if passed.Dmarc.GetResult() != protocol.DmarcVerdict_DMARC_PASS || passed.Dmarc.GetAdkim() != "s" || passed.Dmarc.GetAlignment() != sidecar.MailDmarcAlignedDkim {
			t.Errorf("the network's selector: dmarc %v", passed.Dmarc)
		}
		failed := check(intruder)
		for _, result := range failed.Dkim {
			if result.GetResult() == protocol.DkimVerdict_DKIM_PASS {
				t.Errorf("another network's selector: dkim %v", result)
			}
		}
		if failed.Dmarc.GetResult() == protocol.DmarcVerdict_DMARC_PASS || failed.Dmarc.GetAlignment() != 0 {
			t.Errorf("another network's selector: dmarc %v", failed.Dmarc)
		}
	})
}

// Removing a verified domain withdraws it from the network's delegated row in the removal's own transaction, and
// dropping one after its failed days does the same, so a network that verifies the domain next answers its selectors
// alone rather than neither answering.
func TestSmtpCustomDomainRemovalWithdrawsTheDelegation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newSmtpApiWorld(t, ctx)
		testSmtpVerifyDomain(ctx, world.networkId, testSmtpCustomDomain)
		testSmtpVerifyDomain(ctx, world.networkId, "plain.example")
		testSmtpCustomDomainClock(t, NetworkTierTeam, time.Time{}, false)
		if _, err := testSmtpPublishSigner(t, ctx, world.caller, newTestSmtpCustomDomainSigner(t, testSmtpCustomDomain)); err != nil {
			t.Fatal(err)
		}
		if apexes := testSmtpCustomDomainView(t, ctx, world.root).Delegated().Apexes(); len(apexes) != 2 {
			t.Fatalf("delegated %v", apexes)
		}

		// a removal that rolls back withdraws nothing
		rolledBack := errors.New("the removal rolled back")
		err := dnsCatchRefusal(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_domain WHERE network_id = $1 AND domain = $2`, world.networkId, testSmtpCustomDomain))
				if !SmtpWithdrawCustomDomainDelegationInTx(ctx, tx, world.networkId) {
					t.Error("the withdrawal wrote nothing")
				}
				panic(dnsWriteRefusal{err: rolledBack})
			}, server.TxReadCommitted)
		})
		if !errors.Is(err, rolledBack) {
			t.Fatal(err)
		}
		if apexes := testSmtpCustomDomainView(t, ctx, world.root).Delegated().Apexes(); len(apexes) != 2 {
			t.Fatalf("after a rollback %v", apexes)
		}
		// the removal's own transaction
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_domain WHERE network_id = $1 AND domain = $2`, world.networkId, testSmtpCustomDomain))
			SmtpWithdrawCustomDomainDelegationInTx(ctx, tx, world.networkId)
		}, server.TxReadCommitted)
		if apexes := testSmtpCustomDomainView(t, ctx, world.root).Delegated().Apexes(); !slices.Equal(apexes, []string{"_domainkey.plain.example."}) {
			t.Fatalf("after the removal %v", apexes)
		}
		// nothing more to withdraw: no write
		serial := testDnsRootSerial(t, ctx, world.root)
		server.Tx(ctx, func(tx server.PgTx) {
			if SmtpWithdrawCustomDomainDelegationInTx(ctx, tx, world.networkId) {
				t.Error("a withdrawal with nothing to withdraw wrote")
			}
		}, server.TxReadCommitted)
		if testDnsRootSerial(t, ctx, world.root) != serial {
			t.Fatal("the serial moved")
		}

		// the next network to verify the domain answers its selectors alone
		nextCaller, _ := newTestSmtpOtherNetwork(t, ctx, world.root)
		testSmtpVerifyDomain(ctx, nextCaller.NetworkId, testSmtpCustomDomain)
		signer := newTestSmtpCustomDomainSigner(t, testSmtpCustomDomain)
		if _, err := testSmtpPublishSigner(t, ctx, nextCaller, signer); err != nil {
			t.Fatal(err)
		}
		view := testSmtpCustomDomainView(t, ctx, world.root)
		if rcode, text := testSmtpDelegatedAnswer(t, view, signer.selector+"._domainkey."+testSmtpCustomDomain); rcode != dns.RcodeSuccess || !strings.HasPrefix(text, "v=DKIM1") {
			t.Fatalf("the next network's selector: %d %q", rcode, text)
		}

		// a drop after the failed days withdraws it in the drop's transaction too
		domains := newTestDomains()
		domains.resolver.fail(errors.New("synthetic resolver failure"))
		failStart := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_domain SET check_time = $3, fail_time = $3 WHERE network_id = $1 AND domain = $2`,
				nextCaller.NetworkId, testSmtpCustomDomain, failStart.Add(-8*24*time.Hour),
			))
		})
		domains.recheck(ctx, failStart, nil)
		if GetNetworkDomain(ctx, nextCaller.NetworkId, testSmtpCustomDomain) != nil {
			t.Fatal("the domain outlived its failed days")
		}
		if rcode, _ := testSmtpDelegatedAnswer(t, testSmtpCustomDomainView(t, ctx, world.root), signer.selector+"._domainkey."+testSmtpCustomDomain); rcode != -1 {
			t.Fatalf("a dropped domain is still delegated: %d", rcode)
		}
	})
}
