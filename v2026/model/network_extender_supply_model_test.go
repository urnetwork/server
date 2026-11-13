package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
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
