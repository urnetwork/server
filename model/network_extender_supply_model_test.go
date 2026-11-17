package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The extender supply counts (monitor/SIGNALS.md §28.5): the keys first
// activated in the trailing day, and the extenders revoked in it that have not
// activated again. Every address is an RFC 5737 documentation address and
// every id is generated, so nothing here names anything real.
func TestCountExtenderSupplyCountsTheTrailingDay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		ago := func(duration time.Duration) time.Time {
			return now.Add(-duration)
		}
		create := func(name string, createTime time.Time, revokeTime *time.Time) {
			publicKey := []byte("extender-stats-" + name)
			Testing_CreateNetworkExtender(
				ctx,
				&NetworkExtender{
					ExtenderId:  server.NewId(),
					NetworkId:   testExtenderNetworkId(publicKey),
					ClientId:    server.NewId(),
					PublicKey:   publicKey,
					CreateTime:  createTime,
					TcpPort:     443,
					UdpPort:     443,
					DnsPort:     53,
					DnsTld:      connect.DefaultExtenderDnsTld,
					CountryCode: "au",
					Active:      revokeTime == nil,
					RevokeTime:  revokeTime,
				},
				nil,
			)
		}
		revokedAt := func(duration time.Duration) *time.Time {
			revokeTime := ago(duration)
			return &revokeTime
		}

		// created in the day, still active: created
		create("supply-new", ago(2*time.Hour), nil)
		// created and revoked in the day, the churn the review found: both
		create("supply-new-revoked", ago(20*time.Hour), revokedAt(time.Hour))
		// created before the day, revoked in it: a loss only
		create("supply-old-revoked", ago(72*time.Hour), revokedAt(10*time.Hour))
		// created and revoked before the day: neither
		create("supply-old-gone", ago(96*time.Hour), revokedAt(48*time.Hour))
		// created before the day, still active: neither
		create("supply-old", ago(30*time.Hour), nil)
		// revoked a minute before the day began: neither
		create("supply-edge", ago(50*time.Hour), revokedAt(ExtenderSupplyWindow+time.Minute))
		// revoked in the day, then activated again: the activation reuses the
		// key's row and clears revoke_time, so the directory lost nothing
		create("supply-back", ago(50*time.Hour), revokedAt(3*time.Hour))
		testStatsActivate(ctx, "supply-back", 4, "192.0.2.40", "au", nil)

		count := CountExtenderSupply(ctx, now)
		if count.Created != 2 || count.Revoked != 2 {
			t.Fatalf("supply = %+v, want 2 created and 2 revoked", count)
		}

		// the window ends at its instant: a count five hours back leaves out
		// what happened since and takes in the day before, which holds the
		// edge's revocation
		earlier := CountExtenderSupply(ctx, ago(5*time.Hour))
		if earlier.Created != 1 || earlier.Revoked != 2 {
			t.Fatalf("supply five hours ago = %+v, want 1 created and 2 revoked", earlier)
		}
	})
}

// The pool's concentration (sn WHITEPAPER §8.6.3): the networks holding an
// active extender, and what the network holding the most holds, by extenders
// and by pool units. A unit is an extender's active ipv4 address's /29, else
// its active ipv6 address's /48 (§8.6.2), so extenders in one block share one
// unit, and an extender whose ipv4 address lapsed is placed by its ipv6. An
// inactive extender, and an active one with no active address, hold nothing.
// Every address is an RFC 5737 or RFC 3849 documentation address and every id
// is generated.
func TestCountExtenderConcentrationReadsThePoolsUnits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		if concentration := CountExtenderConcentration(ctx); concentration != (ExtenderConcentration{}) {
			t.Fatalf("an empty pool reads %+v", concentration)
		}

		address := testExtenderCacheAddress
		create := func(name string, networkId server.Id, active bool, addresses ...*NetworkExtenderAddress) {
			Testing_CreateNetworkExtender(
				ctx,
				&NetworkExtender{
					ExtenderId:  server.NewId(),
					NetworkId:   networkId,
					ClientId:    server.NewId(),
					PublicKey:   []byte("extender-concentration-" + name),
					CreateTime:  server.NowUtc(),
					TcpPort:     443,
					UdpPort:     443,
					DnsPort:     53,
					DnsTld:      connect.DefaultExtenderDnsTld,
					CountryCode: "us",
					Active:      active,
				},
				addresses,
			)
		}

		fleetNetworkId := server.NewId()
		// four extenders in one /29 hold one unit; a fifth in the next /29 a
		// second, and an ipv6-only one its /48 a third
		create("fleet-a", fleetNetworkId, true, address("192.0.2.1", true), address("2001:db8:1:1::1", true))
		create("fleet-b", fleetNetworkId, true, address("192.0.2.2", true))
		create("fleet-c", fleetNetworkId, true, address("192.0.2.3", true))
		create("fleet-d", fleetNetworkId, true, address("192.0.2.7", true))
		create("fleet-e", fleetNetworkId, true, address("192.0.2.8", true))
		create("fleet-f", fleetNetworkId, true, address("2001:db8:2::1", true))

		homeNetworkId := server.NewId()
		// its ipv4 address lapsed, so its ipv6 /48 is its unit
		create("home-a", homeNetworkId, true, address("198.51.100.1", false), address("2001:db8:3::1", true))
		create("home-b", homeNetworkId, true, address("198.51.100.9", true))

		// neither holds anything: revoked, and active with no active address
		create("gone", server.NewId(), false, address("203.0.113.1", true))
		create("lapsed", server.NewId(), true, address("203.0.113.9", false))

		want := ExtenderConcentration{
			NetworkCount:            2,
			TopNetworkExtenderCount: 6,
			UnitCount:               5,
			TopNetworkUnitCount:     3,
		}
		if concentration := CountExtenderConcentration(ctx); concentration != want {
			t.Fatalf("concentration = %+v, want %+v", concentration, want)
		}
	})
}
