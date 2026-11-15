//go:build sidecar_pending

package model

// §12.5's aliases (dns_alias_model.go; DNS-M6-01, reading 11): a claim refused for a name a row holds in each
// state, a screened name, an unverified network, one on probation and a name that is not the network's; an
// alias answering only its challenge until certified, then its label's records with its own mx through the
// edge's answer path, a write to the label reaching it with no second write; a release's CAA and revocation,
// the alias still answering, then its tombstone at 90 days on a fake clock with the name never free again and
// RenameNetwork refused it; a change retiring the old alias; and a network's deletion leaving no CAA behind.

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// A records world whose network may claim an alias, and its name.
func newTestDnsAliasWorld(t testing.TB, ctx context.Context, standing sidecar.AbuseStanding, verified bool) (*testDnsRecordsWorld, string) {
	t.Helper()
	world := newTestDnsRecordsWorld(t, ctx, standing, verified)
	name := getTestNetworkName(t, ctx, world.networkId)
	return world, name
}

// The network's name as it stands.
func getTestNetworkName(t testing.TB, ctx context.Context, networkId server.Id) string {
	t.Helper()
	name := ""
	server.Db(ctx, func(conn server.PgConn) {
		name = getNetworkNameInTx(ctx, conn, networkId)
	})
	if name == "" {
		t.Fatal("the network has no name")
	}
	return name
}

// A claim of name by the world's network.
func (self *testDnsRecordsWorld) claimAlias(ctx context.Context, name string) (*sidecar.DnsAliasAnswer, error) {
	return ClaimDnsAlias(ctx, &DnsAliasRequest{Root: self.root, NetworkId: self.networkId, Name: name, ActorKind: AdminEventActorSystem})
}

// A release of name by the world's network.
func (self *testDnsRecordsWorld) releaseAlias(ctx context.Context, name string) (*sidecar.DnsAliasAnswer, error) {
	return ReleaseDnsAlias(ctx, &DnsAliasRequest{Root: self.root, NetworkId: self.networkId, Name: name, ActorKind: AdminEventActorSystem})
}

// The world's label, below its root.
func (self *testDnsRecordsWorld) label() string {
	return strings.TrimSuffix(self.apex, "."+self.root)
}

// The edge's view of the world's root, its own records reconciled so the view answers, and a healthy set
// of one edge and one router interface.
func (self *testDnsRecordsWorld) view(t testing.TB, ctx context.Context) (*dnsservice.ZoneView, *dnsservice.HealthySet) {
	t.Helper()
	settings := testDnsRootSettings(t, "")
	settings.Roots = []string{self.root}
	settings.RootRecords = map[string]*dnsservice.DnsRootRecordSettings{self.root: {ReportMx: "rpt.report.example"}}
	if err := DnsReconcileRootRecords(ctx, settings); err != nil {
		t.Fatal(err)
	}
	snapshot, err := OpenDnsSnapshot(ctx, self.root)
	if err != nil {
		t.Fatal(err)
	}
	view, err := dnsservice.NewZoneView(settings, snapshot)
	snapshot.Close()
	if err != nil {
		t.Fatal(err)
	}
	router := dnsservice.DnsLbInterface{Name: "lb-2", Ipv4: netip.MustParseAddr("192.0.2.2"), Router: true}
	healthy := dnsservice.NewHealthySet(
		[]dnsservice.DnsLbInterface{{Name: "lb-1", Ipv4: netip.MustParseAddr("192.0.2.1")}, router},
		[]dnsservice.DnsLbInterface{router},
	)
	return view, healthy
}

func requireDnsViewAnswer(t testing.TB, view *dnsservice.ZoneView, healthy *dnsservice.HealthySet, name string, rrType uint16, wantRcode int, want ...string) {
	t.Helper()
	rcode, got := testDnsViewAnswer(t, view, healthy, name, rrType)
	slices.Sort(want)
	if rcode != wantRcode || !slices.Equal(got, want) {
		t.Errorf("%s %s: %s %v, want %s %v", name, dns.TypeToString[rrType], dns.RcodeToString[rcode], got, dns.RcodeToString[wantRcode], want)
	}
}

// Holds a name for another network in dns_label, in kind and state.
func testHoldDnsName(t testing.TB, ctx context.Context, root string, name string, kind string, state string) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO dns_label (root, label, network_id, kind, state, state_time) VALUES ($1, $2, $3, $4, $5, $6)`,
			root,
			name,
			server.NewId(),
			kind,
			state,
			server.NowUtc(),
		))
	})
}

// A claim is refused, writing nothing: a name a row holds as a label, a live alias, a retiring one or a
// tombstone, in any root; a name that is not the network's; a name the label rules or the screen refuse; an
// unverified network; one on probation or suspended; and a standing not loaded.
func TestDnsAliasClaimRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, held := range []struct {
			kind  string
			state string
		}{
			{kind: DnsLabelKindLabel, state: DnsLabelStateLive},
			{kind: DnsLabelKindAlias, state: DnsLabelStateLive},
			{kind: DnsLabelKindAlias, state: DnsLabelStateRetiring},
			{kind: DnsLabelKindAlias, state: DnsLabelStateTombstoned},
			{kind: DnsLabelKindLabel, state: DnsLabelStateTombstoned},
		} {
			world, name := newTestDnsAliasWorld(t, ctx, sidecar.AbuseStandingStandard, true)
			testHoldDnsName(t, ctx, world.root, name, held.kind, held.state)
			before := world.serial(t, ctx)
			_, err := world.claimAlias(ctx, name)
			if apiError := testDnsApiError(t, err); apiError.Status != http.StatusConflict {
				t.Errorf("a name held as %s %s: %v", held.kind, held.state, apiError)
			}
			if after := world.serial(t, ctx); after != before {
				t.Errorf("a refused claim took serial %d", after)
			}
		}

		world, name := newTestDnsAliasWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := world.claimAlias(ctx, name+"x"); testDnsApiError(t, err).Status != http.StatusConflict {
			t.Errorf("a name not the network's: %v", err)
		}
		if _, err := world.claimAlias(ctx, "Bad_Name"); testDnsApiError(t, err).Status != http.StatusBadRequest {
			t.Errorf("a name the rules refuse: %v", err)
		}

		// the label rules pass a word-number name, which §21.6's screen keeps for burners
		screened, _ := newTestDnsAliasWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		wordNumberName := "aliasscreen-4821"
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network SET network_name = $2 WHERE network_id = $1`, screened.networkId, wordNumberName))
		})
		if _, err := screened.claimAlias(ctx, wordNumberName); testDnsApiError(t, err).Status != http.StatusForbidden {
			t.Errorf("a word-number name: %v", err)
		}
		// and refuse a brand's name outright
		brandName := "paypal-" + strings.ReplaceAll(screened.networkId.String()[24:], "-", "")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network SET network_name = $2 WHERE network_id = $1`, screened.networkId, brandName))
		})
		if _, err := screened.claimAlias(ctx, brandName); testDnsApiError(t, err).Status != http.StatusBadRequest {
			t.Errorf("a brand's name: %v", err)
		}

		unverified, unverifiedName := newTestDnsAliasWorld(t, ctx, sidecar.AbuseStandingStandard, false)
		apiError := testDnsApiError(t, func() error { _, err := unverified.claimAlias(ctx, unverifiedName); return err }())
		if apiError.Status != http.StatusPaymentRequired || apiError.Refusal == nil || apiError.Refusal.LimitKey != DnsVerificationLimitKey {
			t.Errorf("an unverified network: %v", apiError)
		}
		for _, standing := range []sidecar.AbuseStanding{sidecar.AbuseStandingProbation, sidecar.AbuseStandingRestricted, sidecar.AbuseStandingSuspended} {
			refused, refusedName := newTestDnsAliasWorld(t, ctx, standing, true)
			if _, err := refused.claimAlias(ctx, refusedName); testDnsApiError(t, err).Status != http.StatusForbidden {
				t.Errorf("standing %v: %v", standing, err)
			}
		}
		unloaded, unloadedName := newTestDnsAliasWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		unloaded.abuse.Unload(connect.Id(unloaded.networkId))
		if _, err := unloaded.claimAlias(ctx, unloadedName); testDnsApiError(t, err).Status != http.StatusServiceUnavailable {
			t.Errorf("an unloaded standing: %v", err)
		}
	})
}

// Reading 11 through the edge's own answer path: claimed, the alias answers its challenge and nothing of the
// label's; certified, it answers the label's records under its own apex with its own mail host, which answers
// the router interfaces; a write to the label's zone after reaches the alias with no write of its own; and the
// claim is idempotent.
func TestDnsAliasAnswersItsLabelOnceCertified(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		t.Cleanup(Testing_SetDnsChallengesPerNameHour(10))
		world, name := newTestDnsAliasWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		world.limits.set(DnsEntriesLimitKey, 10)
		if _, err := DnsSetMailRecords(ctx, world.networkId, world.root, true); err != nil {
			t.Fatal(err)
		}
		answer, err := world.claimAlias(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if answer.State != sidecar.DnsAliasStateLive || answer.Certified || answer.Disclosure != sidecar.DnsAliasDisclosure {
			t.Fatalf("the claim answered %+v", answer)
		}
		again, err := world.claimAlias(ctx, name)
		if err != nil || again.Name != answer.Name {
			t.Fatalf("claiming again answered %+v, %v", again, err)
		}
		alias := name + "." + world.root
		if _, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: world.root, Name: alias, Values: []string{"alias-key-authorization"}}); err != nil {
			t.Fatal(err)
		}
		view, healthy := world.view(t, ctx)
		requireDnsViewAnswer(t, view, healthy, "_acme-challenge."+alias, dns.TypeTXT, dns.RcodeSuccess, `"alias-key-authorization"`)
		requireDnsViewAnswer(t, view, healthy, alias, dns.TypeMX, dns.RcodeSuccess)
		requireDnsViewAnswer(t, view, healthy, "mx."+alias, dns.TypeA, dns.RcodeNameError)

		if _, err := DnsAliasCertified(ctx, world.root, world.networkId, name, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		if result, err := DnsAliasCertified(ctx, world.root, world.networkId, name, server.NowUtc()); err != nil || result != nil {
			t.Fatalf("certifying again wrote %+v, %v", result, err)
		}
		view, healthy = world.view(t, ctx)
		requireDnsViewAnswer(t, view, healthy, alias, dns.TypeMX, dns.RcodeSuccess, "10 mx."+alias)
		requireDnsViewAnswer(t, view, healthy, world.apex, dns.TypeMX, dns.RcodeSuccess, "10 mx."+world.apex)
		requireDnsViewAnswer(t, view, healthy, "mx."+alias, dns.TypeA, dns.RcodeSuccess, "192.0.2.2")
		requireDnsViewAnswer(t, view, healthy, "maple-1234."+alias, dns.TypeMX, dns.RcodeSuccess, "10 mx."+alias)
		requireDnsViewAnswer(t, view, healthy, "_dmarc."+alias, dns.TypeTXT, dns.RcodeSuccess, `"`+dnsDmarcPolicy+`"`)
		requireDnsViewAnswer(t, view, healthy, "_acme-challenge."+alias, dns.TypeTXT, dns.RcodeSuccess, `"alias-key-authorization"`)

		if _, err := world.write(ctx, testDnsUpsert("shop."+world.apex, "A", "11.22.33.44")); err != nil {
			t.Fatal(err)
		}
		view, healthy = world.view(t, ctx)
		requireDnsViewAnswer(t, view, healthy, "shop."+alias, dns.TypeA, dns.RcodeSuccess, "11.22.33.44")
		// the alias's own apex takes no user write: it is a view of the label's zone
		if _, err := world.write(ctx, testDnsUpsert("shop."+alias, "A", "11.22.33.45")); err == nil {
			t.Error("a write under the alias's apex was taken")
		}
		zone, err := GetNetworkDns(ctx, world.root, world.networkId)
		if err != nil {
			t.Fatal(err)
		}
		if zone.Alias != name || !zone.AliasCertified {
			t.Errorf("GET /network/dns shows alias %q certified %v", zone.Alias, zone.AliasCertified)
		}
		for _, set := range zone.RecordSets {
			if set.Kind == string(dnsservice.DnsRecordKindAlias) {
				t.Errorf("GET /network/dns shows the alias row %+v", set)
			}
		}
	})
}

// An issued certificate of the network for {label, alias}, and one for the label alone.
func testDnsAliasCertificates(t testing.TB, ctx context.Context, world *testDnsRecordsWorld, alias string) (aliasCertId server.Id, labelCertId server.Id) {
	t.Helper()
	insert := func(aliases []string) server.Id {
		identifierSet, err := sidecar.TlsIdentifierSet(strings.TrimSuffix(world.root, "."), world.label(), "", aliases)
		if err != nil {
			t.Fatal(err)
		}
		identifierSetSha256, err := sidecar.TlsIdentifierSetHash(identifierSet)
		if err != nil {
			t.Fatal(err)
		}
		certId := server.NewId()
		spki := sha256.Sum256(certId.Bytes())
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_tls_cert (
						cert_id, network_id, client_id, sidecar_id, identifier_set_sha256, spki_sha256, attestation, state, state_time
					) VALUES ($1, $2, $3, $4, $5, $6, $7, 'issued', $8)
				`,
				certId,
				world.networkId,
				server.NewId(),
				server.NewId(),
				identifierSetSha256[:],
				spki[:],
				[]byte{0x01},
				server.NowUtc(),
			))
		})
		return certId
	}
	return insert([]string{alias}), insert(nil)
}

// A certificate's state.
func testDnsCertState(t testing.TB, ctx context.Context, certId server.Id) string {
	t.Helper()
	state := ""
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT state FROM network_tls_cert WHERE cert_id = $1`, certId).Scan(&state))
	})
	return state
}

// D-DNS-10 on a fake clock: a release retires the alias under <root>'s CAA issue ";", marks the certificate
// naming it for revocation and leaves the label's alone, and the alias keeps answering; nothing tombstones
// before 90 days, and at 90 days every row at or below its apex leaves in one write and the name tombstones,
// never claimable or a network's name again.
func TestDnsAliasReleaseRetiresThenTombstones(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world, name := newTestDnsAliasWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := DnsSetMailRecords(ctx, world.networkId, world.root, true); err != nil {
			t.Fatal(err)
		}
		if _, err := world.claimAlias(ctx, name); err != nil {
			t.Fatal(err)
		}
		if _, err := DnsAliasCertified(ctx, world.root, world.networkId, name, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		aliasCertId, labelCertId := testDnsAliasCertificates(t, ctx, world, name)
		released, err := world.releaseAlias(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if released.State != sidecar.DnsAliasStateRetiring || released.TombstoneTime == nil {
			t.Fatalf("the release answered %+v", released)
		}
		if state := testDnsCertState(t, ctx, aliasCertId); state != "revoking" {
			t.Errorf("the alias's certificate is %s", state)
		}
		if state := testDnsCertState(t, ctx, labelCertId); state != "issued" {
			t.Errorf("the label's certificate is %s", state)
		}
		if again, err := world.releaseAlias(ctx, name); err != nil || again.State != sidecar.DnsAliasStateRetiring {
			t.Fatalf("releasing again answered %+v, %v", again, err)
		}
		alias := name + "." + world.root
		view, healthy := world.view(t, ctx)
		requireDnsViewAnswer(t, view, healthy, alias, dns.TypeCAA, dns.RcodeSuccess, `0 issue ";"`)
		requireDnsViewAnswer(t, view, healthy, alias, dns.TypeMX, dns.RcodeSuccess, "10 mx."+alias)
		// a retiring alias takes no certificate and no challenge
		if _, err := DnsAliasCertified(ctx, world.root, world.networkId, name, server.NowUtc()); err == nil {
			t.Error("a retiring alias was certified")
		}
		if _, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: world.root, Name: alias, Values: []string{"late"}}); err == nil {
			t.Error("a retiring alias took a challenge")
		}

		now := server.NowUtc()
		if count, _, err := TombstoneRetiredDnsAliases(ctx, now.Add(89*24*time.Hour)); err != nil || count != 0 {
			t.Fatalf("at 89 days %d tombstoned, %v", count, err)
		}
		before := world.serial(t, ctx)
		if count, _, err := TombstoneRetiredDnsAliases(ctx, now.Add(90*24*time.Hour+time.Minute)); err != nil || count != 1 {
			t.Fatalf("at 90 days %d tombstoned, %v", count, err)
		}
		requireDnsSerial(t, ctx, world.root, before+1)
		for key := range testDnsSnapshotRows(t, ctx, world.root) {
			if dnsOwnerAtOrBelow(key.Owner, alias) {
				t.Errorf("the tombstone left %s %s", key.Owner, dns.TypeToString[key.Type])
			}
		}
		view, healthy = world.view(t, ctx)
		requireDnsViewAnswer(t, view, healthy, alias, dns.TypeMX, dns.RcodeNameError)
		if _, kind, err := ResolveNetworkLabel(ctx, world.root, name, false); err == nil {
			t.Errorf("a tombstoned alias resolves as %s", kind)
		}

		// never anyone's again: not by a claim, not as a name
		other, _ := newTestDnsAliasWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		otherNetworkId, otherSession := testRenameNetwork(t, ctx, "dns-alias-renamer-"+strings.ReplaceAll(other.networkId.String()[24:], "-", ""))
		requireRenameRefused(t, RenameNetwork(otherSession, &RenameNetworkArgs{NewName: name}), RenameNetworkRefusalNotAvailable)
		if networkNameOf(ctx, otherNetworkId) == name {
			t.Error("the tombstoned alias became a network's name")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network SET network_name = $2 WHERE network_id = $1`, other.networkId, name+"-x"))
		})
		if _, err := world.claimAlias(ctx, name); err == nil {
			t.Error("the tombstoned alias was claimed again")
		}
	})
}

// A claim of a new name, after the network's name changed, retires the old alias in the same transaction:
// one live alias at a time, the old one's certificate marked for revocation.
func TestDnsAliasChangeRetiresTheOld(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world, name := newTestDnsAliasWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := world.claimAlias(ctx, name); err != nil {
			t.Fatal(err)
		}
		aliasCertId, _ := testDnsAliasCertificates(t, ctx, world, name)
		renamed := name + "-two"
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network SET network_name = $2 WHERE network_id = $1`, world.networkId, renamed))
		})
		answer, err := world.claimAlias(ctx, renamed)
		if err != nil {
			t.Fatal(err)
		}
		if answer.State != sidecar.DnsAliasStateLive {
			t.Fatalf("the change answered %+v", answer)
		}
		states := map[string]string{}
		server.Db(ctx, func(conn server.PgConn) {
			for _, row := range getNetworkDnsAliasRowsInTx(ctx, conn, world.root, world.networkId) {
				states[row.name] = row.state
			}
		})
		if states[name] != DnsLabelStateRetiring || states[renamed] != DnsLabelStateLive || len(states) != 2 {
			t.Errorf("the aliases stand %v", states)
		}
		if state := testDnsCertState(t, ctx, aliasCertId); state != "revoking" {
			t.Errorf("the old alias's certificate is %s", state)
		}
		zone, err := GetNetworkDns(ctx, world.root, world.networkId)
		if err != nil || zone.Alias != renamed {
			t.Fatalf("GET /network/dns shows %+v, %v", zone, err)
		}
	})
}

// A network's deletion tombstones its retiring alias at once, and the next pass takes the CAA the release had
// written at its apex.
func TestDnsAliasNetworkDeletionLeavesNoCaa(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world, name := newTestDnsAliasWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := world.claimAlias(ctx, name); err != nil {
			t.Fatal(err)
		}
		if _, err := world.releaseAlias(ctx, name); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(DnsWithdrawNetworkInTx(ctx, tx, world.networkId))
		}, server.TxReadCommitted)
		caa := dnsservice.DnsRecordKey{Owner: name + "." + world.root, Type: dns.TypeCAA}
		if _, held := testDnsSnapshotRows(t, ctx, world.root)[caa]; !held {
			t.Fatal("the release's CAA is not served")
		}
		if _, _, err := TombstoneRetiredDnsAliases(ctx, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		if _, held := testDnsSnapshotRows(t, ctx, world.root)[caa]; held {
			t.Error("a deleted network's alias kept its CAA")
		}
	})
}
