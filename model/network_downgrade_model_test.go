//go:build sidecar_pending

package model

// §20.7's downgrade job on Postgres and Redis (network_downgrade_model.go; QUOTA-M6-05), FS-51 at the model: a
// Team lapse goes read-only at the first pass, changes nothing more on day 13, and on day 14 loses its newest
// user RRsets past the limit, its custom web hosts and its standing names over tls_endpoints, with MX, TLSA and
// the strict-mode MX host kept so mail still arrives; a rerun writes nothing; a renewal inside the 14 days
// cancels and one after them releases, oldest first under a lower tier's limit; a Pro lapse takes the same path;
// the limit is shared across roots; a killed pass is finished by the next; an abuse kill is never this job's to
// overwrite or lift; and the two limits are dark while enforce_sidecar_limits is off. The effects themselves are
// DNS-M6-07's and TLS-M5-13's, whose own tests hold them; evs's gate, which refuses on the over-limit list read
// here, is EVSB-M2-15's.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
)

const testDowngradeDay = 24 * time.Hour

// A network with a zone, a verified domain and Team's custom hosts, on limits a tier at a time: Community 2
// entries, 1 endpoint and 1 GiB; Pro 4 entries and 2 endpoints; Team 100 entries, 10 endpoints and 100 GiB.
type testDowngradeWorld struct {
	*testDnsCustomHostWorld
	// what the fake projection lists as published
	names []*TlsPublishedName
}

func newTestDowngradeWorld(t testing.TB, ctx context.Context) *testDowngradeWorld {
	t.Helper()
	t.Cleanup(testingSetTeamProConfig())
	t.Cleanup(Testing_SetEscrowDrawOrder(true))
	t.Cleanup(Testing_SetEnforceSidecarLimits(true))
	t.Cleanup(Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"dns_entries": 2, "tls_endpoints": 1, "storage": 1 << 30}))
	t.Cleanup(Testing_SetSidecarLimits(NetworkTierPro, map[string]int64{"dns_entries": 4, "tls_endpoints": 2, "storage": 10 << 30}))
	t.Cleanup(Testing_SetSidecarLimits(NetworkTierTeam, map[string]int64{"dns_entries": 100, "tls_endpoints": 10, "storage": 100 << 30}))
	world := &testDowngradeWorld{testDnsCustomHostWorld: newTestDnsCustomHostWorld(t, ctx)}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team (network_id) VALUES ($1)`, world.networkId))
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_acl_head (network_id, version, chain, has_document) VALUES ($1, 1, $2, true)`,
			world.networkId,
			[]byte{1},
		))
	})
	SetTlsPublishedNamesSource(func(ctx context.Context, networkId server.Id, now time.Time) ([]*TlsPublishedName, error) {
		if networkId != world.networkId {
			return []*TlsPublishedName{}, nil
		}
		return world.names, nil
	})
	t.Cleanup(func() { SetTlsPublishedNamesSource(nil) })
	return world
}

// A published name of the world's network: standing with a zero expiry.
func (self *testDowngradeWorld) name(host string, port int, expireTime time.Time) *TlsPublishedName {
	return &TlsPublishedName{NetworkId: self.networkId, Host: host, Port: port, Label: "label", Audience: protocol.AclAudience_ACL_AUDIENCE_PUBLIC, ExpireTime: expireTime}
}

// The fixtures of a Team in use: mail's records and a TLSA, five user RRsets h1 to h5 oldest first, a weak and a
// strong web host and a strict-mode MX host, two standing names, one that expires and the system publish, and
// 5 GiB stored with storage_over as Team left it.
func (self *testDowngradeWorld) fill(t testing.TB, ctx context.Context) {
	t.Helper()
	if _, err := DnsSetMailRecords(ctx, self.networkId, self.root, true); err != nil {
		t.Fatal(err)
	}
	tlsa := dnsservice.DnsRecordSet{Owner: "_25._tcp.mx." + self.apex, Type: dns.TypeTLSA, Ttl: 300, Kind: dnsservice.DnsRecordKindKey, Rdata: []string{"3 1 1 " + strings.Repeat("ab", 32)}}
	if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: self.root, NetworkId: self.networkId, Upserts: []dnsservice.DnsRecordSet{tlsa}}); err != nil {
		t.Fatal(err)
	}
	self.writeAged(t, ctx, "h1", "h2", "h3", "h4", "h5")
	if _, err := self.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: "a." + self.domain, Owner: "blog." + self.apex, Mode: sidecar.DnsCustomHostModeWeak, WeakDisclosureAccepted: true}); err != nil {
		t.Fatal(err)
	}
	self.strongHost(t, ctx, "s."+self.domain, "shop."+self.apex)
	if _, err := DnsCustomHostResultApply(ctx, &DnsCustomHostResult{Host: "s." + self.domain, Pass: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := self.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: "mx." + self.domain, Owner: "mx." + self.apex}); err != nil {
		t.Fatal(err)
	}
	self.names = []*TlsPublishedName{
		self.name("blog", 443, time.Time{}),
		self.name("shop", 443, time.Time{}),
		self.name("tmp", 443, server.NowUtc().Add(365*testDowngradeDay)),
		self.name("mta-sts", 443, time.Time{}),
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_quota_state (network_id, stored_byte_count, storage_over) VALUES ($1, $2, false)`,
			self.networkId,
			int64(5)<<30,
		))
	})
}

// A Team period's grant that ended, grace included, at end.
func (self *testDowngradeWorld) teamGrantEnded(t testing.TB, ctx context.Context, end time.Time) *TeamGrant {
	t.Helper()
	return testingAddTeamGrant(t, ctx, self.networkId, end.Add(-ProGrantGrace).AddDate(0, -1, 0), end.Add(-ProGrantGrace))
}

// A Team period's grant in its window now: the renewal.
func (self *testDowngradeWorld) teamRenewed(t testing.TB, ctx context.Context) {
	t.Helper()
	start, end := testingTeamPeriod()
	testingAddTeamGrant(t, ctx, self.networkId, start, end)
	connect.AssertEqual(t, NetworkTierOfFresh(ctx, self.networkId), NetworkTierTeam)
}

// One whole pass at now.
func testDowngradePass(t testing.TB, ctx context.Context, now time.Time) ApplyNetworkDowngradesResult {
	t.Helper()
	result, err := ApplyNetworkDowngrades(ctx, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.LastNetworkId != nil {
		t.Fatalf("the pass stopped short %+v", result)
	}
	return result
}

// What a pass changed, apart from how many networks it read.
func testDowngradeEffects(result ApplyNetworkDowngradesResult) ApplyNetworkDowngradesResult {
	result.LookedCount = 0
	result.LastNetworkId = nil
	return result
}

// The custom hosts' states, by host.
func (self *testDowngradeWorld) customHostStates(ctx context.Context) map[string]string {
	states := map[string]string{}
	for _, label := range []string{"a", "s", "mx"} {
		if row := self.row(ctx, label+"."+self.domain); row != nil {
			states[label] = row.State
		}
	}
	return states
}

// The zone's rows that are no user's: mail's, the key's and the system's.
func (self *testDowngradeWorld) systemLines(ctx context.Context) []string {
	lines := []string{}
	for _, line := range self.zoneLines(ctx) {
		if !strings.Contains(line, " "+string(dnsservice.DnsRecordKindUser)+" ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// The hosts the network's kills hold, by reason, from its rows, and the hosts its key holds.
func (self *testDowngradeWorld) kills(t testing.TB, ctx context.Context) (rowHostReasons map[string]string, keyHosts []string) {
	t.Helper()
	rowHostReasons = map[string]string{}
	for host, row := range tlsSuspensionTestRows(t, ctx, self.networkId) {
		rowHostReasons[host] = row.Reason
	}
	entries, _ := tlsSuspensionTestKey(t, ctx, self.networkId)
	for host := range entries {
		keyHosts = append(keyHosts, host)
	}
	slices.Sort(keyHosts)
	return
}

func (self *testDowngradeWorld) storageOver(t testing.TB, ctx context.Context) bool {
	t.Helper()
	state, err := GetNetworkQuotaState(ctx, self.networkId)
	if err != nil || state == nil {
		t.Fatalf("quota state %+v %v", state, err)
	}
	return state.StorageOver
}

// FS-51: a Team whose period ended. The first pass sets storage read-only and nothing else; day 13 changes
// nothing; day 14 withholds the newest RRsets past the limit, the web hosts and the standing names over the
// limit, while MX, TLSA, the strict-mode MX host, the name that expires and the system publish stay; a rerun
// writes nothing; and a renewal on day 20 releases everything and opens storage.
func TestApplyNetworkDowngradesTeamLapse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDowngradeWorld(t, ctx)
		world.fill(t, ctx)
		lapse := world.teamGrantEnded(t, ctx, server.NowUtc().Add(-time.Hour)).EndTime
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, world.networkId), NetworkTierFree)
		systemLines := world.systemLines(ctx)
		customHostMap := world.customHostMap(t, ctx)

		// at once, with no clock: storage, and nothing else
		result := testDowngradePass(t, ctx, lapse.Add(time.Hour))
		connect.AssertEqual(t, result.LookedCount, 1)
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{StorageOverCorrected: 1})
		connect.AssertEqual(t, world.storageOver(t, ctx), true)
		overLimit, err := GetQuotaOverLimit(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if refusal := overLimit.StorageNetworkIdRefusals[connect.Id(world.networkId)]; refusal == nil || refusal.LimitKey != quotaStorageLimitKey {
			t.Fatalf("the over-limit list %+v", overLimit.StorageNetworkIdRefusals)
		}
		// frozen by the tier, sign-in and data kept
		if err := RequireTeamTier(ctx, world.networkId); err == nil {
			t.Fatal("a lapsed Team passed the Team gate")
		}

		// day 13 changes nothing
		serial := world.serial(t, ctx)
		result = testDowngradePass(t, ctx, lapse.Add(14*testDowngradeDay-time.Minute))
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{})
		connect.AssertEqual(t, world.serial(t, ctx), serial)
		if withheld, _ := world.withheldHosts(t, ctx); len(withheld) != 0 {
			t.Fatalf("day 13 withheld %v", withheld)
		}
		connect.AssertEqual(t, world.customHostStates(ctx), map[string]string{"a": sidecar.DnsCustomHostStateActive, "s": sidecar.DnsCustomHostStateActive, "mx": sidecar.DnsCustomHostStateActive})
		if rows, keyHosts := world.kills(t, ctx); len(rows) != 0 || len(keyHosts) != 0 {
			t.Fatalf("day 13 killed %v %v", rows, keyHosts)
		}

		// day 14
		result = testDowngradePass(t, ctx, lapse.Add(14*testDowngradeDay))
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{DnsWithheldCount: 1, CustomHostsWithheldCount: 2, TlsSuspendedCount: 2})
		if withheld, served := world.withheldHosts(t, ctx); !slices.Equal(withheld, []string{"h3", "h4", "h5"}) || !slices.Equal(served, []string{"h1", "h2"}) {
			t.Fatalf("withheld %v served %v", withheld, served)
		}
		// nothing deleted, and growth still counted
		connect.AssertEqual(t, DnsEntryCount(ctx, world.networkId), int64(5))
		// no inbound address broken: mail's records, the TLSA and the strict-mode MX host as they were
		connect.AssertEqual(t, world.systemLines(ctx), systemLines)
		connect.AssertEqual(t, world.customHostStates(ctx), map[string]string{"a": DnsCustomHostStateWithheld, "s": DnsCustomHostStateWithheld, "mx": sidecar.DnsCustomHostStateActive})
		if withheldMap := world.customHostMap(t, ctx); withheldMap.Version != customHostMap.Version+1 {
			t.Fatalf("the map after withholding %+v", withheldMap)
		}
		connect.AssertEqual(t, testDnsCustomHostMapped(ctx, "a."+world.domain), false)
		rows, keyHosts := world.kills(t, ctx)
		connect.AssertEqual(t, rows, map[string]string{"blog": TlsSuspensionReasonQuotaDowngrade, "shop": TlsSuspensionReasonQuotaDowngrade})
		connect.AssertEqual(t, keyHosts, []string{"blog", "shop"})

		// a rerun is idempotent
		serial = world.serial(t, ctx)
		_, triggerCount := tlsSuspensionTestKey(t, ctx, world.networkId)
		result = testDowngradePass(t, ctx, lapse.Add(15*testDowngradeDay))
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{})
		connect.AssertEqual(t, world.serial(t, ctx), serial)
		_, triggerCountAfter := tlsSuspensionTestKey(t, ctx, world.networkId)
		connect.AssertEqual(t, triggerCountAfter, triggerCount)

		// a renewal on day 20 releases what was withheld
		world.teamRenewed(t, ctx)
		result = testDowngradePass(t, ctx, lapse.Add(20*testDowngradeDay))
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{DnsReleasedCount: 1, CustomHostsReleasedCount: 2, TlsLiftedCount: 2, StorageOverCorrected: 1})
		if withheld, _ := world.withheldHosts(t, ctx); len(withheld) != 0 {
			t.Fatalf("after the renewal withheld %v", withheld)
		}
		connect.AssertEqual(t, world.customHostStates(ctx), map[string]string{"a": sidecar.DnsCustomHostStateActive, "s": sidecar.DnsCustomHostStateActive, "mx": sidecar.DnsCustomHostStateActive})
		if rows, keyHosts := world.kills(t, ctx); len(rows) != 0 || len(keyHosts) != 0 {
			t.Fatalf("after the renewal killed %v %v", rows, keyHosts)
		}
		connect.AssertEqual(t, world.storageOver(t, ctx), false)
		connect.AssertEqual(t, world.systemLines(ctx), systemLines)
		result = testDowngradePass(t, ctx, lapse.Add(21*testDowngradeDay))
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{})
	})
}

// A renewal on day 10 cancels the clock: a pass on day 15 of the lapse finds a Team and withholds nothing.
func TestApplyNetworkDowngradesRenewalInsideTheGraceCancels(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDowngradeWorld(t, ctx)
		world.fill(t, ctx)
		lapse := world.teamGrantEnded(t, ctx, server.NowUtc().Add(-time.Hour)).EndTime
		result := testDowngradePass(t, ctx, lapse.Add(9*testDowngradeDay))
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{StorageOverCorrected: 1})
		world.teamRenewed(t, ctx)
		serial := world.serial(t, ctx)
		result = testDowngradePass(t, ctx, lapse.Add(15*testDowngradeDay))
		// storage opens with the renewal, and nothing was withheld to release
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{StorageOverCorrected: 1})
		connect.AssertEqual(t, world.serial(t, ctx), serial)
		if withheld, _ := world.withheldHosts(t, ctx); len(withheld) != 0 {
			t.Fatalf("withheld %v", withheld)
		}
		connect.AssertEqual(t, world.customHostStates(ctx)["a"], sidecar.DnsCustomHostStateActive)
		if rows, _ := world.kills(t, ctx); len(rows) != 0 {
			t.Fatalf("killed %v", rows)
		}
	})
}

// A lapsed Team that buys Pro after its 14 days is brought to Pro's limits: the oldest withheld RRsets are
// served first up to Pro's dns_entries, one standing name fits Pro's tls_endpoints beside the one that expires,
// and the web hosts stay withheld, since custom domains are Team only.
func TestApplyNetworkDowngradesReleasesInOrderUnderALowerTier(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDowngradeWorld(t, ctx)
		world.fill(t, ctx)
		now := server.NowUtc()
		lapse := world.teamGrantEnded(t, ctx, now.Add(-20*testDowngradeDay)).EndTime
		result := testDowngradePass(t, ctx, now)
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{DnsWithheldCount: 1, CustomHostsWithheldCount: 2, TlsSuspendedCount: 2, StorageOverCorrected: 1})

		testingAddProSubscriptionBalance(ctx, world.networkId, now.Add(-time.Hour), now.Add(30*testDowngradeDay))
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, world.networkId), NetworkTierPro)
		downgradeTime, fromTier, left := NetworkDowngradeTime(ctx, world.networkId)
		connect.AssertEqual(t, left, true)
		connect.AssertEqual(t, fromTier, NetworkTierTeam)
		connect.AssertEqual(t, downgradeTime, lapse)

		result = testDowngradePass(t, ctx, now)
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{DnsReleasedCount: 1, TlsLiftedCount: 1, StorageOverCorrected: 1})
		if withheld, served := world.withheldHosts(t, ctx); !slices.Equal(withheld, []string{"h5"}) || !slices.Equal(served, []string{"h1", "h2", "h3", "h4"}) {
			t.Fatalf("withheld %v served %v", withheld, served)
		}
		connect.AssertEqual(t, world.customHostStates(ctx), map[string]string{"a": DnsCustomHostStateWithheld, "s": DnsCustomHostStateWithheld, "mx": sidecar.DnsCustomHostStateActive})
		rows, keyHosts := world.kills(t, ctx)
		connect.AssertEqual(t, rows, map[string]string{"shop": TlsSuspensionReasonQuotaDowngrade})
		connect.AssertEqual(t, keyHosts, []string{"shop"})
		connect.AssertEqual(t, world.storageOver(t, ctx), false)
	})
}

// A Pro lapse takes the same path: on day 13 only storage, on day 14 the newest RRsets past Community's limit and
// the standing names over it. It has no custom host to withhold.
func TestApplyNetworkDowngradesProLapse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDowngradeWorld(t, ctx)
		world.writeAged(t, ctx, "h1", "h2", "h3", "h4")
		world.names = []*TlsPublishedName{world.name("blog", 443, time.Time{}), world.name("blog", 8443, time.Time{}), world.name("shop", 443, time.Time{})}
		now := server.NowUtc()
		lapse := now.Add(-time.Hour)
		testingAddProSubscriptionBalance(ctx, world.networkId, lapse.AddDate(0, -1, 0), lapse)
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, world.networkId), NetworkTierFree)

		result := testDowngradePass(t, ctx, lapse.Add(13*testDowngradeDay))
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{})
		result = testDowngradePass(t, ctx, lapse.Add(14*testDowngradeDay))
		// blog's two ports pass the one endpoint by themselves, so blog goes and shop's one fits
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{DnsWithheldCount: 1, TlsSuspendedCount: 1})
		rows, keyHosts := world.kills(t, ctx)
		connect.AssertEqual(t, rows, map[string]string{"blog": TlsSuspensionReasonQuotaDowngrade})
		connect.AssertEqual(t, keyHosts, []string{"blog"})
		if withheld, served := world.withheldHosts(t, ctx); !slices.Equal(withheld, []string{"h3", "h4"}) || !slices.Equal(served, []string{"h1", "h2"}) {
			t.Fatalf("withheld %v served %v", withheld, served)
		}
		testDnsAdminResultsContain(t, ctx, world.networkId, AdminEventResultDnsWithheld)

		// Pro again: everything back
		testingAddProSubscriptionBalance(ctx, world.networkId, now.Add(-time.Minute), now.Add(30*testDowngradeDay))
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, world.networkId), NetworkTierPro)
		result = testDowngradePass(t, ctx, lapse.Add(20*testDowngradeDay))
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{DnsReleasedCount: 1, TlsLiftedCount: 1})
		if withheld, _ := world.withheldHosts(t, ctx); len(withheld) != 0 {
			t.Fatalf("after Pro withheld %v", withheld)
		}
	})
}

// dns_entries counts a network's RRsets in every root, so the newest go whichever root holds them: with a limit
// of 2 over two roots, each root keeps the one RRset of its own among the network's two oldest.
func TestApplyNetworkDowngradesSharesTheLimitAcrossRoots(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDowngradeWorld(t, ctx)
		otherRoot := testDnsRoot(t, ctx)
		otherLabel, err := EnsureNetworkLabel(ctx, world.networkId, otherRoot)
		if err != nil {
			t.Fatal(err)
		}
		otherApex := otherLabel + "." + otherRoot
		start := server.NowUtc().Add(-time.Hour)
		write := func(root string, apex string, host string, age int) {
			t.Helper()
			owner := host + "." + apex
			_, err := WriteDnsRecords(ctx, &DnsRecordsWrite{
				Root:    root,
				Caller:  &DnsRecordsCaller{NetworkId: world.networkId, ActorKind: AdminEventActorSystem},
				Request: testDnsUpsert(owner, "TXT", `"`+host+`"`),
			})
			if err != nil {
				t.Fatal(err)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE network_dns_record SET create_time = $3 WHERE root = $1 AND owner = $2`,
					root, owner, start.Add(time.Duration(age)*time.Minute),
				))
			})
		}
		write(world.root, world.apex, "h1", 0)
		write(otherRoot, otherApex, "g1", 1)
		write(world.root, world.apex, "h2", 2)
		write(otherRoot, otherApex, "g2", 3)

		lapse := server.NowUtc().Add(-time.Hour)
		testingAddProSubscriptionBalance(ctx, world.networkId, lapse.AddDate(0, -1, 0), lapse)
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, world.networkId), NetworkTierFree)
		result := testDowngradePass(t, ctx, lapse.Add(14*testDowngradeDay))
		connect.AssertEqual(t, result.DnsWithheldCount, 2)
		withheld := []string{}
		server.Db(ctx, func(conn server.PgConn) {
			for _, root := range []string{world.root, otherRoot} {
				for _, row := range getDnsZoneRowsInTx(ctx, conn, root, world.networkId) {
					if row.set.Kind == dnsservice.DnsRecordKindUser && row.withheld {
						withheld = append(withheld, row.set.Owner)
					}
				}
			}
		})
		slices.Sort(withheld)
		expected := []string{"g2." + otherApex, "h2." + world.apex}
		slices.Sort(expected)
		connect.AssertEqual(t, withheld, expected)
	})
}

// A killed pass resumes: a page that came back full names where the next starts, and each network is done
// once; a kill whose row a killed pass wrote without its key gets its key; and a lift killed after the key went
// is finished from the row.
func TestApplyNetworkDowngradesResumesAKilledPass(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDowngradeWorld(t, ctx)
		world.fill(t, ctx)
		now := server.NowUtc()
		world.teamGrantEnded(t, ctx, now.Add(-20*testDowngradeDay))

		// a second network, lapsed from Pro, with RRsets of its own
		otherId, otherApex := testDnsNetwork(t, ctx, world.root)
		world.abuse.Set(connect.Id(otherId), sidecar.AbuseStandingStandard, true)
		for index, host := range []string{"o1", "o2", "o3"} {
			owner := host + "." + otherApex
			_, err := WriteDnsRecords(ctx, &DnsRecordsWrite{
				Root:    world.root,
				Caller:  &DnsRecordsCaller{NetworkId: otherId, ActorKind: AdminEventActorSystem},
				Request: testDnsUpsert(owner, "TXT", `"`+host+`"`),
			})
			if err != nil {
				t.Fatal(err)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE network_dns_record SET create_time = $3 WHERE root = $1 AND owner = $2`,
					world.root, owner, now.Add(-time.Hour).Add(time.Duration(index)*time.Minute),
				))
			})
		}
		testingAddProSubscriptionBalance(ctx, otherId, now.Add(-50*testDowngradeDay), now.Add(-20*testDowngradeDay))

		// the first pass was killed after it wrote blog's row and before its key
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_tls_suspension (network_id, host, reason) VALUES ($1, 'blog', $2)`,
				world.networkId,
				TlsSuspensionReasonQuotaDowngrade,
			))
		})

		settings := DefaultNetworkDowngradeSettings()
		settings.PageCount = 1
		total := ApplyNetworkDowngradesResult{}
		var after *server.Id
		pageCount := 0
		for {
			result, err := ApplyNetworkDowngradesWithSettings(ctx, now, after, settings)
			if err != nil {
				t.Fatal(err)
			}
			pageCount += 1
			total.LookedCount += result.LookedCount
			total.add(&result)
			total.ErrorCount += result.ErrorCount
			if result.LastNetworkId == nil {
				break
			}
			after = result.LastNetworkId
			if 10 < pageCount {
				t.Fatal("the pass never ended")
			}
		}
		connect.AssertEqual(t, total.LookedCount, 2)
		// blog's row stood, so only shop's is new, and both hold their key
		connect.AssertEqual(t, testDowngradeEffects(total), ApplyNetworkDowngradesResult{DnsWithheldCount: 2, CustomHostsWithheldCount: 2, TlsSuspendedCount: 1, StorageOverCorrected: 1})
		rows, keyHosts := world.kills(t, ctx)
		connect.AssertEqual(t, rows, map[string]string{"blog": TlsSuspensionReasonQuotaDowngrade, "shop": TlsSuspensionReasonQuotaDowngrade})
		connect.AssertEqual(t, keyHosts, []string{"blog", "shop"})

		// a lift killed after shop's key went and before its row
		world.teamRenewed(t, ctx)
		value, err := tlsSuspensionEntryValue("shop", TlsSuspensionReasonQuotaDowngrade, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			keys := []string{sidecar.TlsKillKey(sidecar.Id(world.networkId)), sidecar.TlsKillTriggerKey(sidecar.Id(world.networkId))}
			if err := tlsSuspensionExpireScript.Run(ctx, r, keys, "shop", string(value)).Err(); err != nil {
				t.Fatal(err)
			}
		})
		result := testDowngradePass(t, ctx, now)
		connect.AssertEqual(t, result.TlsLiftedCount, 2)
		if rows, keyHosts := world.kills(t, ctx); len(rows) != 0 || len(keyHosts) != 0 {
			t.Fatalf("after the renewal killed %v %v", rows, keyHosts)
		}
	})
}

// A kill an abuse case holds on a host is never this job's: a pass does not take it over, a renewal's pass does
// not lift it, and a case's kill set over the job's own keeps both its row and its key through the release.
func TestApplyNetworkDowngradesLeavesAnAbuseKill(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDowngradeWorld(t, ctx)
		world.fill(t, ctx)
		now := server.NowUtc()
		world.teamGrantEnded(t, ctx, now.Add(-20*testDowngradeDay))
		abuseReason := "abuse:case"
		if err := SetTlsSuspension(ctx, &sidecar.TlsSuspendArgs{NetworkId: sidecar.Id(world.networkId), Host: "blog", Reason: abuseReason}); err != nil {
			t.Fatal(err)
		}
		result := testDowngradePass(t, ctx, now)
		connect.AssertEqual(t, result.TlsSuspendedCount, 1)
		rows, _ := world.kills(t, ctx)
		connect.AssertEqual(t, rows, map[string]string{"blog": abuseReason, "shop": TlsSuspensionReasonQuotaDowngrade})

		// the case takes shop over too
		if err := SetTlsSuspension(ctx, &sidecar.TlsSuspendArgs{NetworkId: sidecar.Id(world.networkId), Host: "shop", Reason: abuseReason}); err != nil {
			t.Fatal(err)
		}
		world.teamRenewed(t, ctx)
		result = testDowngradePass(t, ctx, now)
		connect.AssertEqual(t, result.TlsLiftedCount, 0)
		rows, keyHosts := world.kills(t, ctx)
		connect.AssertEqual(t, rows, map[string]string{"blog": abuseReason, "shop": abuseReason})
		connect.AssertEqual(t, keyHosts, []string{"blog", "shop"})
		entries, _ := tlsSuspensionTestKey(t, ctx, world.networkId)
		connect.AssertEqual(t, entries["shop"].Reason, abuseReason)
	})
}

// dns_entries and tls_endpoints are dark while enforce_sidecar_limits is off: nothing is withheld for them and
// what was is served again, while custom hosts follow the tier. A network whose names cannot be read keeps its
// kills as they stood and is counted as not finished.
func TestApplyNetworkDowngradesDarkLimitsAndUnreadNames(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDowngradeWorld(t, ctx)
		world.fill(t, ctx)
		now := server.NowUtc()
		world.teamGrantEnded(t, ctx, now.Add(-20*testDowngradeDay))

		restore := Testing_SetEnforceSidecarLimits(false)
		result := testDowngradePass(t, ctx, now)
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{CustomHostsWithheldCount: 2, StorageOverCorrected: 1})
		restore()

		// no source: the DNS half still runs, the kills do not
		SetTlsPublishedNamesSource(nil)
		result = testDowngradePass(t, ctx, now)
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{DnsWithheldCount: 1, ErrorCount: 1})
		SetTlsPublishedNamesSource(func(ctx context.Context, networkId server.Id, now time.Time) ([]*TlsPublishedName, error) {
			return nil, errors.New("not loaded")
		})
		result = testDowngradePass(t, ctx, now)
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{ErrorCount: 1})
		SetTlsPublishedNamesSource(func(ctx context.Context, networkId server.Id, now time.Time) ([]*TlsPublishedName, error) {
			return world.names, nil
		})
		result = testDowngradePass(t, ctx, now)
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{TlsSuspendedCount: 2})

		// the switch off again serves what the limits withheld
		defer Testing_SetEnforceSidecarLimits(false)()
		result = testDowngradePass(t, ctx, now)
		connect.AssertEqual(t, testDowngradeEffects(result), ApplyNetworkDowngradesResult{DnsReleasedCount: 1, TlsLiftedCount: 2})
		connect.AssertEqual(t, world.customHostStates(ctx)["a"], DnsCustomHostStateWithheld)
	})
}

// Which hosts a downgrade kills: none while the names fit or the limit is none; past it, hosts with a standing
// name from the end of host order until the rest fit, a host's ports counted each; never a host whose names all
// expire, the system publish, every host at once or a glob.
func TestTlsDowngradeSuspendHosts(t *testing.T) {
	settings := DefaultTlsSuspensionSettings()
	expires := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	name := func(host string, port int, expireTime time.Time) *TlsPublishedName {
		return &TlsPublishedName{Host: host, Port: port, ExpireTime: expireTime}
	}
	names := []*TlsPublishedName{
		name("a", 443, time.Time{}),
		// one grant under two served names is one endpoint
		name("a", 443, time.Time{}),
		name("b", 443, time.Time{}),
		name("b", 8443, time.Time{}),
		name("c", 443, time.Time{}),
		name("tmp", 443, expires),
		name("mta-sts", 443, time.Time{}),
	}
	connect.AssertEqual(t, len(tlsDowngradeSuspendHosts(names, -1, settings)), 0)
	connect.AssertEqual(t, len(tlsDowngradeSuspendHosts(names, 5, settings)), 0)
	// tmp stays; a fits; b's two do not; c does
	connect.AssertEqual(t, tlsDowngradeSuspendHosts(names, 3, settings), []string{"b"})
	connect.AssertEqual(t, tlsDowngradeSuspendHosts(names, 2, settings), []string{"b", "c"})
	connect.AssertEqual(t, tlsDowngradeSuspendHosts(names, 0, settings), []string{"a", "b", "c"})

	globs := []*TlsPublishedName{
		name("*", 443, time.Time{}),
		name("*-preview", 443, time.Time{}),
		name("@", 443, time.Time{}),
		name("mta-sts", 8443, time.Time{}),
	}
	connect.AssertEqual(t, tlsDowngradeSuspendHosts(globs, 0, settings), []string{"@"})
}
