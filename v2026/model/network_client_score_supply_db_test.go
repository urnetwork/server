// The supply publisher lease against Redis: the claim script decides as the
// model the pure tests share between simulated taskworkers (supplyTestLease),
// and processes claiming through Redis leave one publisher, the newest. Every
// value below is synthetic.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Each case starts from a lease held for leaseTestHeld, or none, and makes
// one claim. The script and its model name the same holder, and renew the
// lease to the full ttl exactly when the claim takes or keeps it for the
// claimant.
func TestClientScoreSupplyPublisherLeaseScriptMatchesItsModel(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const leaseTestHeld = 30 * time.Second
		newer := server.NowUtc()
		older := newer.Add(-10 * time.Minute)
		olderLease := clientScoreSupplyLease(older, "taskworker-a")
		newerLease := clientScoreSupplyLease(newer, "taskworker-b")
		for _, c := range []struct {
			name       string
			held       string
			lapsed     bool
			lease      string
			measuredAt time.Time
			want       string
		}{
			{name: "absent", held: "", lease: olderLease, measuredAt: older, want: olderLease},
			{name: "lapsed", held: newerLease, lapsed: true, lease: olderLease, measuredAt: older, want: olderLease},
			{name: "an older holder yields", held: olderLease, lease: newerLease, measuredAt: newer, want: newerLease},
			{name: "a newer holder keeps", held: newerLease, lease: olderLease, measuredAt: older, want: newerLease},
			{name: "the holder renews", held: newerLease, lease: newerLease, measuredAt: newer, want: newerLease},
			{name: "a claim as new keeps the incumbent", held: newerLease, lease: clientScoreSupplyLease(newer, "taskworker-c"), measuredAt: newer, want: newerLease},
			{name: "a longer time is newer", held: "99999999999999:taskworker-x", lease: newerLease, measuredAt: newer, want: "99999999999999:taskworker-x"},
			{name: "a shorter time is older", held: "999:taskworker-x", lease: olderLease, measuredAt: older, want: olderLease},
			{name: "an unreadable lease passes", held: "unreadable", lease: olderLease, measuredAt: older, want: olderLease},
			{name: "a lease without a time passes", held: ":taskworker-x", lease: olderLease, measuredAt: older, want: olderLease},
			{name: "a lease with a broken time passes", held: "12ab:taskworker-x", lease: olderLease, measuredAt: older, want: olderLease},
		} {
			renewed := c.want == c.lease

			server.Redis(ctx, func(r server.RedisClient) {
				err := r.Del(ctx, clientScoreSupplyPublisherKey).Err()
				server.Raise(err)
				if c.held != "" {
					held := leaseTestHeld
					if c.lapsed {
						held = time.Millisecond
					}
					err := r.Set(ctx, clientScoreSupplyPublisherKey, c.held, held).Err()
					server.Raise(err)
				}
			})
			if c.lapsed {
				time.Sleep(20 * time.Millisecond)
			}
			holder, err := claimClientScoreSupplyPublisher(ctx, c.lease, c.measuredAt)
			if err != nil {
				t.Fatalf("%s: claim: %v", c.name, err)
			}
			if holder != c.want {
				t.Fatalf("%s: Redis answered the holder %q, want %q", c.name, holder, c.want)
			}
			server.Redis(ctx, func(r server.RedisClient) {
				value, err := r.Get(ctx, clientScoreSupplyPublisherKey).Result()
				server.Raise(err)
				ttl, err := r.PTTL(ctx, clientScoreSupplyPublisherKey).Result()
				server.Raise(err)
				if value != c.want {
					t.Fatalf("%s: Redis holds %q, want %q", c.name, value, c.want)
				}
				if renewed && (ttl <= leaseTestHeld || clientScoreSupplyPublisherLeaseTtl < ttl) {
					t.Fatalf("%s: a lease the claim took or kept has %s left, want up to %s", c.name, ttl, clientScoreSupplyPublisherLeaseTtl)
				}
				if !renewed && (ttl <= 0 || leaseTestHeld < ttl) {
					t.Fatalf("%s: a lease the claim left to its holder has %s left, want its own %s at most", c.name, ttl, leaseTestHeld)
				}
			})

			clock := newSupplyTestClock(newer)
			model := &supplyTestLease{now: clock.now}
			if c.held != "" {
				model.value, model.expiresAt = c.held, clock.now().Add(leaseTestHeld)
				if c.lapsed {
					model.expiresAt = clock.now()
				}
			}
			modelHolder, err := model.claim(ctx, c.lease, c.measuredAt)
			if err != nil || modelHolder != c.want {
				t.Fatalf("%s: the model answered the holder %q (%v), want %q", c.name, modelHolder, err, c.want)
			}
			if modelRenewed := model.expiresAt.Equal(clock.now().Add(clientScoreSupplyPublisherLeaseTtl)); modelRenewed != renewed {
				t.Fatalf("%s: the model renewed the lease %t, want %t", c.name, modelRenewed, renewed)
			}
		}
	})
}

// Two taskworkers claim through Redis. The only measurement publishes; the
// export of a newer one takes the lease at once, and the first process stops
// at its next scrape.
func TestClientScoreSupplyProcessesShareOneRedisLease(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clock := newSupplyTestClock(server.NowUtc())
		process := func(publisher string) *clientScoreSupplyCollector {
			return &clientScoreSupplyCollector{
				now:       clock.now,
				publisher: publisher,
				claim:     claimClientScoreSupplyPublisher,
				errorf: func(format string, args ...any) {
					t.Errorf("%s: unexpected claim failure: "+format, append([]any{publisher}, args...)...)
				},
				infof: func(string, ...any) {},
			}
		}
		older := process("taskworker-a")
		newer := process("taskworker-b")
		olderMeasurement := supplyTestMeasurement(clock.now(), 4, "de", "kr")
		older.publish(ctx, olderMeasurement)
		clock.advance(supplyTestScrapeInterval)
		if series, measuredAt := supplyTestScrape(t, older); series != supplyTestSeriesOf(2) || measuredAt != olderMeasurement.measuredAt.Unix() {
			t.Fatalf("the only measurement published %d series measured at %d", series, measuredAt)
		}
		if series, _ := supplyTestScrape(t, newer); series != 0 {
			t.Fatalf("a process without a measurement published %d series", series)
		}

		clock.advance(time.Minute)
		newerMeasurement := supplyTestMeasurement(clock.now(), 5, "de", "kr", "au")
		newer.publish(ctx, newerMeasurement)
		server.Redis(ctx, func(r server.RedisClient) {
			value, err := r.Get(ctx, clientScoreSupplyPublisherKey).Result()
			server.Raise(err)
			if value != clientScoreSupplyLease(newerMeasurement.measuredAt, "taskworker-b") {
				t.Fatalf("after the newer export the lease is %q", value)
			}
		})
		clock.advance(supplyTestScrapeInterval)
		if series, _ := supplyTestScrape(t, older); series != 0 {
			t.Fatalf("one scrape after a newer measurement appeared, the older still published %d series", series)
		}
		if series, measuredAt := supplyTestScrape(t, newer); series != supplyTestSeriesOf(3) || measuredAt != newerMeasurement.measuredAt.Unix() {
			t.Fatalf("the newer measurement published %d series measured at %d", series, measuredAt)
		}
	})
}
