//go:build sidecar_pending

package model

// §12.8's CAA probe, dns side (dns_caa_canary_model.go; DNS-M6-05) on Postgres: the canary's exact form with a
// copy of *'s records added and removed with it; refused at an @ and at an owner no strong-mode custom host
// maps to; a CNAME at the owner answered as the lint's refusal; a second probe on a target refused while a
// stale republish takes a new nonce and serial; expiry at an hour with its copy; and FS-32's apex host, whose
// canary sits at its host owner.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// A canary publish at owner as the route builds it, with a fresh nonce, replacing replaces.
func testDnsCaaCanaryPut(t testing.TB, owner string, replaces string) *dnsservice.DnsCaaCanaryPut {
	t.Helper()
	nonce, err := dnsservice.NewDnsCaaCanaryNonce()
	if err != nil {
		t.Fatal(err)
	}
	rdata, err := dnsservice.DnsCaaCanaryRdata([]string{`0 issue "ca.example; accounturi=https://ca.example/acct/7"`}, nonce, &dnsservice.DefaultDnsSettings().CaaCanary)
	if err != nil {
		t.Fatal(err)
	}
	return &dnsservice.DnsCaaCanaryPut{Owner: owner, Rdata: rdata, Nonce: nonce, Replaces: replaces, Ttl: 60}
}

// The zone's rows at owner, by kind.
func testDnsRowsAt(t testing.TB, ctx context.Context, root string, networkId server.Id, owner string) map[dnsservice.DnsRecordKind]*dnsservice.DnsRecordSet {
	t.Helper()
	sets := map[dnsservice.DnsRecordKind]*dnsservice.DnsRecordSet{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, row := range getDnsZoneRowsInTx(ctx, conn, root, networkId) {
			if row.set.Owner == owner {
				set := row.set
				sets[set.Kind] = &set
			}
		}
	})
	return sets
}

// A strong-mode custom host of the world mapped to owner, pending as certs finds it.
func (self *testDnsCustomHostWorld) strongHost(t testing.TB, ctx context.Context, host string, owner string) {
	t.Helper()
	if _, err := self.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: host, Owner: owner}); err != nil {
		t.Fatal(err)
	}
}

// The canary is the walk's set plus 0 urcanary "<nonce>", TTL 60, kind canary, written with a copy of *'s
// records at an owner that held nothing, in one write; its delete takes both in one write; neither is an entry.
func TestDnsCaaCanaryWritesItsFormAndCopy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		if _, err := EnsureNetworkZone(ctx, world.networkId, world.root); err != nil {
			t.Fatal(err)
		}
		owner := "shop." + world.apex
		world.strongHost(t, ctx, "www."+world.domain, owner)
		put := testDnsCaaCanaryPut(t, owner, "")
		before := world.serial(t, ctx)
		result, err := DnsPutCaaCanary(ctx, world.root, put)
		if err != nil || result.Serial != before+1 {
			t.Fatalf("%+v %v", result, err)
		}
		rows := testDnsRowsAt(t, ctx, world.root, world.networkId, owner)
		canary := rows[dnsservice.DnsRecordKindCanary]
		if canary == nil || canary.Type != dns.TypeCAA || canary.Ttl != 60 || !slices.Equal(canary.Rdata, put.Rdata) {
			t.Fatalf("canary %+v", canary)
		}
		if !strings.HasSuffix(canary.Rdata[len(canary.Rdata)-1], `urcanary "`+put.Nonce+`"`) || !strings.HasPrefix(canary.Rdata[len(canary.Rdata)-1], "0 ") {
			t.Fatalf("the canary record %q", canary.Rdata)
		}
		if rows[dnsservice.DnsRecordKindCopy] == nil {
			t.Fatal("no copy of *'s records beside the canary")
		}
		// one journal row carried both
		journal := testDnsJournal(t, ctx, world.root, before)
		if len(journal) != 1 || len(journal[0].Upserts) != 2 {
			t.Fatalf("journal %+v", journal)
		}
		if count := DnsEntryCount(ctx, world.networkId); count != 0 {
			t.Errorf("%d entries", count)
		}
		deleted, err := DnsDeleteCaaCanary(ctx, world.root, owner)
		if err != nil || deleted.Serial != result.Serial+1 {
			t.Fatalf("delete %+v %v", deleted, err)
		}
		if rows := testDnsRowsAt(t, ctx, world.root, world.networkId, owner); len(rows) != 0 {
			t.Fatalf("left %v", rows)
		}
		if _, err := DnsDeleteCaaCanary(ctx, world.root, owner); !errors.Is(err, dnsservice.ErrDnsWriteEmpty) {
			t.Fatalf("a delete of nothing: %v", err)
		}
	})
}

// Refused, with nothing written: the zone's @, an alias's @, an owner no custom host maps to, a weak-mode
// host's owner, and a scope's @ the projection lists; and a CNAME at the owner is the lint's refusal.
func TestDnsCaaCanaryRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		world.strongHost(t, ctx, "www."+world.domain, "shop."+world.apex)
		if _, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: "weak." + world.domain, Owner: "blog." + world.apex, Mode: sidecar.DnsCustomHostModeWeak, WeakDisclosureAccepted: true}); err != nil {
			t.Fatal(err)
		}
		testDnsScopeSource(t, world.networkId, "eng")
		before := world.serial(t, ctx)
		for _, owner := range []string{world.apex, "other." + world.apex, "blog." + world.apex, "eng." + world.apex, "x.other.example."} {
			if _, err := DnsPutCaaCanary(ctx, world.root, testDnsCaaCanaryPut(t, owner, "")); !errors.Is(err, dnsservice.ErrDnsCaaCanaryRefused) {
				t.Errorf("%s: %v", owner, err)
			}
		}
		// a CNAME at the owner: the lint refuses the canary, which is the probe's failing verdict
		cnamed := "cdn." + world.apex
		world.strongHost(t, ctx, "cdn."+world.domain, cnamed)
		if _, err := world.write(ctx, testDnsUpsert(cnamed, "CNAME", "edge.provider.example.")); err != nil {
			t.Fatal(err)
		}
		before = world.serial(t, ctx)
		if _, err := DnsPutCaaCanary(ctx, world.root, testDnsCaaCanaryPut(t, cnamed, "")); !errors.Is(err, dnsservice.ErrDnsCaaCanaryLint) {
			t.Errorf("a CNAME at the owner: %v", err)
		}
		if after := world.serial(t, ctx); after != before {
			t.Errorf("a refusal took serial %d", after)
		}
	})
}

// One probe a target at a time: a second publish is busy, one naming another nonce too; the probe's own
// republish after a stale ready names its canary and takes a new nonce at a new serial.
func TestDnsCaaCanaryOneProbeATarget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		owner := "shop." + world.apex
		world.strongHost(t, ctx, "www."+world.domain, owner)
		world.strongHost(t, ctx, "store."+world.domain, owner)
		first := testDnsCaaCanaryPut(t, owner, "")
		written, err := DnsPutCaaCanary(ctx, world.root, first)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DnsPutCaaCanary(ctx, world.root, testDnsCaaCanaryPut(t, owner, "")); !errors.Is(err, dnsservice.ErrDnsCaaCanaryBusy) {
			t.Errorf("a second probe: %v", err)
		}
		if _, err := DnsPutCaaCanary(ctx, world.root, testDnsCaaCanaryPut(t, owner, strings.Repeat("0", 32))); !errors.Is(err, dnsservice.ErrDnsCaaCanaryBusy) {
			t.Errorf("replacing another nonce: %v", err)
		}
		// FS-32's restored stale: the republish replaces the probe's own canary
		republish := testDnsCaaCanaryPut(t, owner, first.Nonce)
		rewritten, err := DnsPutCaaCanary(ctx, world.root, republish)
		if err != nil || rewritten.Serial <= written.Serial {
			t.Fatalf("republish %+v %v", rewritten, err)
		}
		canary := testDnsRowsAt(t, ctx, world.root, world.networkId, owner)[dnsservice.DnsRecordKindCanary]
		if nonce := dnsservice.DnsCaaCanaryNonce(canary.Rdata); nonce != republish.Nonce || nonce == first.Nonce {
			t.Fatalf("the canary carries %q", nonce)
		}
	})
}

// taskworker's chain removes a canary past its hour through the journal, its copy with it.
func TestDnsCaaCanaryExpiresAtAnHour(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		owner := "shop." + world.apex
		world.strongHost(t, ctx, "www."+world.domain, owner)
		if _, err := DnsPutCaaCanary(ctx, world.root, testDnsCaaCanaryPut(t, owner, "")); err != nil {
			t.Fatal(err)
		}
		if removed, _, err := RemoveExpiredDnsChallenges(ctx, server.NowUtc().Add(59*time.Minute)); err != nil || removed != 0 {
			t.Fatalf("before the hour: %d %v", removed, err)
		}
		if removed, _, err := RemoveExpiredDnsChallenges(ctx, server.NowUtc().Add(61*time.Minute)); err != nil || removed != 1 {
			t.Fatalf("after the hour: %d %v", removed, err)
		}
		if rows := testDnsRowsAt(t, ctx, world.root, world.networkId, owner); len(rows) != 0 {
			t.Fatalf("left %v", rows)
		}
	})
}

// FS-32's apex host: a custom host at the domain's apex maps to a host owner, where its canary sits, never the
// zone's @.
func TestDnsCaaCanaryForAnApexHostSitsAtItsHostOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		owner := "apex." + world.apex
		world.strongHost(t, ctx, world.domain, owner)
		if _, err := DnsPutCaaCanary(ctx, world.root, testDnsCaaCanaryPut(t, owner, "")); err != nil {
			t.Fatal(err)
		}
		if testDnsRowsAt(t, ctx, world.root, world.networkId, owner)[dnsservice.DnsRecordKindCanary] == nil {
			t.Fatal("no canary at the apex host's owner")
		}
		for _, set := range testDnsRowsAt(t, ctx, world.root, world.networkId, world.apex) {
			if set.Type == dns.TypeCAA {
				t.Fatal("a CAA at the zone's @")
			}
		}
	})
}
