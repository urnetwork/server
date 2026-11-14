package evshost

// The per-client repo count's real scripts on the private test Redis (.stack/server-test.sh), as the lease route runs
// them (plan-EVS1-subscription-caps#2, QD-263, QD-749): {evsc_<client_id>}, a sorted set no fake holds, counted ahead
// of {evsl_<repo_id>}'s lease script. Each script takes its now as an argument, so no case here waits on the store's
// clock.

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarhost"
)

// What the tests read of a key that sidecar.Redis has no call for.
var (
	subscribeReposTestCardScript = &sidecar.RedisScript{Name: "evshost-test-zcard", Source: `return redis.call('ZCARD', KEYS[1])`}
	subscribeReposTestTtlScript  = &sidecar.RedisScript{Name: "evshost-test-pttl", Source: `return redis.call('PTTL', KEYS[1])`}
	subscribeReposTestTypeScript = &sidecar.RedisScript{Name: "evshost-test-type", Source: `return redis.call('TYPE', KEYS[1]).ok`}
)

// A test id that is label's alone.
func subscribeReposTestId(label string) sidecar.Id {
	var id sidecar.Id
	copy(id[:], fmt.Sprintf("%-16s", label))
	return id
}

// One client's count on the store, with the lease route's settings.
type subscribeReposTestCount struct {
	tb       testing.TB
	ctx      context.Context
	redis    sidecar.Redis
	settings *evsservice.EvsSettings
	key      string
}

func newSubscribeReposTestCount(tb testing.TB, client string) *subscribeReposTestCount {
	return &subscribeReposTestCount{
		tb:       tb,
		ctx:      tb.Context(),
		redis:    sidecarhost.NewRedis(nil),
		settings: evsservice.DefaultEvsSettings(),
		key:      evsservice.ClientRepoKey(subscribeReposTestId(client)),
	}
}

// ClientRepoPutScript at now for repo under limit, with the arguments serveSubscribe gives it: its reply.
func (self *subscribeReposTestCount) put(now time.Time, repo string, limit int64) int64 {
	self.tb.Helper()
	reply, err := self.redis.RunScript(self.ctx, evsservice.ClientRepoPutScript, []string{self.key}, []string{
		strconv.FormatInt(now.UnixMilli(), 10),
		repo,
		strconv.FormatInt(limit, 10),
		strconv.FormatInt(now.Add(-self.settings.LeaseLife).UnixMilli(), 10),
		strconv.FormatInt(self.settings.LeaseKeyTtl.Milliseconds(), 10),
	})
	if err != nil {
		self.tb.Fatal(err)
	}
	return reply.Integer
}

// An integer one of the test's reading scripts answers of the count's key.
func (self *subscribeReposTestCount) read(script *sidecar.RedisScript) int64 {
	self.tb.Helper()
	reply, err := self.redis.RunScript(self.ctx, script, []string{self.key}, nil)
	if err != nil {
		self.tb.Fatal(err)
	}
	return reply.Integer
}

// A fixed now, so the scores are the test's own.
func subscribeReposTestStart() time.Time {
	return time.UnixMilli(1793836800123)
}

// The cap at §20.4's 500: 500 repos are counted and the 501st refused, writing nothing; each of the 500 renews at
// the cap as a renewal and the count stays 500; a removal makes room for one.
func TestClientRepoCountRefusesThe501stRepoOnThePrivateRedis(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		count := newSubscribeReposTestCount(tb, "cap client")
		start := subscribeReposTestStart()
		for i := 0; i < 500; i += 1 {
			if got := count.put(start, fmt.Sprintf("repo%03d", i), 500); got != 1 {
				tb.Fatalf("repo %d of 500 answered %d", i+1, got)
			}
		}
		if got := count.put(start.Add(time.Second), "repo500", 500); got != 0 {
			tb.Fatalf("the 501st repo answered %d", got)
		}
		if got := count.read(subscribeReposTestCardScript); got != 500 {
			tb.Fatalf("the refusal left %d repos counted", got)
		}
		for i := 0; i < 500; i += 1 {
			if got := count.put(start.Add(time.Hour), fmt.Sprintf("repo%03d", i), 500); got != 2 {
				tb.Fatalf("the renewal of repo %d answered %d", i+1, got)
			}
		}
		if got := count.read(subscribeReposTestCardScript); got != 500 {
			tb.Fatalf("500 renewals left %d repos counted", got)
		}
		if got := count.put(start.Add(2*time.Hour), "repo500", 500); got != 0 {
			tb.Fatalf("the 501st repo answered %d after the renewals", got)
		}
		removed, err := count.redis.RunScript(count.ctx, evsservice.ClientRepoRemoveScript, []string{count.key}, []string{"repo007"})
		if err != nil || removed.Integer != 1 {
			tb.Fatalf("the removal answered %v %v", removed, err)
		}
		if again, err := count.redis.RunScript(count.ctx, evsservice.ClientRepoRemoveScript, []string{count.key}, []string{"repo007"}); err != nil || again.Integer != 0 {
			tb.Fatalf("a second removal answered %v %v", again, err)
		}
		if got := count.put(start.Add(2*time.Hour), "repo500", 500); got != 1 {
			tb.Fatalf("the repo after a removal answered %d", got)
		}
		if got := count.put(start.Add(2*time.Hour), "repo501", 500); got != 0 {
			tb.Fatalf("the repo past the cap again answered %d", got)
		}
		// a sorted set under the hash's ttl, and another client's count is its own key
		typeReply, err := count.redis.RunScript(count.ctx, subscribeReposTestTypeScript, []string{count.key}, nil)
		if err != nil || string(typeReply.Bulk) != "zset" {
			tb.Fatalf("the count is a %q %v, not a sorted set", typeReply.Bulk, err)
		}
		ttlMs, keyTtlMs := count.read(subscribeReposTestTtlScript), count.settings.LeaseKeyTtl.Milliseconds()
		if ttlMs <= keyTtlMs-time.Minute.Milliseconds() || keyTtlMs < ttlMs {
			tb.Fatalf("the count's ttl is %d ms, not the %d ms a write sets", ttlMs, keyTtlMs)
		}
		other := newSubscribeReposTestCount(tb, "other client")
		if got := other.put(start, "repo501", 500); got != 1 {
			tb.Fatalf("another client's first repo answered %d", got)
		}
	})
}

// A repo leaves the count a lease's life after its last subscribe or renewal: a millisecond before, it still fills
// the cap; at it, it is gone and another takes its place, while a repo renewed since stays. A count nobody renews
// empties, and its key goes with its last member.
func TestClientRepoCountDropsALapsedRepoOnThePrivateRedis(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		count := newSubscribeReposTestCount(tb, "lapse client")
		start := subscribeReposTestStart()
		life := count.settings.LeaseLife
		if got := count.put(start, "stale", 2); got != 1 {
			tb.Fatalf("the first repo answered %d", got)
		}
		if got := count.put(start, "kept", 2); got != 1 {
			tb.Fatalf("the second repo answered %d", got)
		}
		// kept is renewed a day on, stale is not
		if got := count.put(start.Add(24*time.Hour), "kept", 2); got != 2 {
			tb.Fatalf("the renewal answered %d", got)
		}
		if got := count.put(start.Add(life-time.Millisecond), "new", 2); got != 0 {
			tb.Fatalf("a third repo answered %d before the oldest lapsed", got)
		}
		if got := count.put(start.Add(life), "new", 2); got != 1 {
			tb.Fatalf("a third repo answered %d once the oldest lapsed", got)
		}
		if got := count.put(start.Add(life), "stale", 2); got != 0 {
			tb.Fatalf("the lapsed repo came back past the cap: %d", got)
		}
		if got := count.read(subscribeReposTestCardScript); got != 2 {
			tb.Fatalf("%d repos counted", got)
		}
		// a refusal long after every score lapsed drops them all, and Redis drops the empty set
		if got := count.put(start.Add(3*life), "late", 0); got != 0 {
			tb.Fatalf("a repo under a cap of 0 answered %d", got)
		}
		if got := count.read(subscribeReposTestCardScript); got != 0 {
			tb.Fatalf("%d lapsed repos still counted", got)
		}
		if ttlMs := count.read(subscribeReposTestTtlScript); ttlMs != -2 {
			tb.Fatalf("the emptied count's key is still there: PTTL %d", ttlMs)
		}
	})
}

// The per-repo cap is what it was with the count ahead of it: the lease script still counts a repo's distinct live
// holders, refuses a new one at the cap and lets a holder renew, each client's count taking the repo first on its
// own key.
func TestLeaseScriptCapsHoldersBehindTheClientRepoCountOnThePrivateRedis(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		redis := sidecarhost.NewRedis(nil)
		settings := evsservice.DefaultEvsSettings()
		start := subscribeReposTestStart()
		repoId := subscribeReposTestId("capped repo")
		lease := func(now time.Time, client string, holdersPerRepo int64) (counted int64, written int64) {
			tb.Helper()
			count := newSubscribeReposTestCount(tb, client)
			counted = count.put(now, sidecar.EvsKeyId(repoId), 500)
			holder := sidecar.EvsKeyId(subscribeReposTestId(client)) + "." + sidecar.EvsKeyId(subscribeReposTestId(client+" sidecar"))
			reply, err := redis.RunScript(ctx, evsservice.LeasePutScript, []string{evsservice.LeaseKey(repoId), evsservice.LeaseVersionKey(repoId)}, []string{
				strconv.FormatInt(now.UnixMilli(), 10),
				holder,
				strconv.FormatInt(holdersPerRepo, 10),
				strconv.FormatInt(settings.LeaseKeyTtl.Milliseconds(), 10),
				holder + "|*|1|", strconv.FormatInt(now.Add(settings.LeaseLife).UnixMilli(), 10),
			})
			if err != nil {
				tb.Fatal(err)
			}
			return counted, reply.Integer
		}
		for _, client := range []string{"holder one", "holder two"} {
			if counted, written := lease(start, client, 2); counted != 1 || written != 1 {
				tb.Fatalf("%s answered %d %d", client, counted, written)
			}
		}
		if counted, written := lease(start.Add(time.Minute), "holder three", 2); counted != 1 || written != 0 {
			tb.Fatalf("a third holder of two answered %d %d", counted, written)
		}
		if counted, written := lease(start.Add(time.Minute), "holder one", 2); counted != 2 || written != 1 {
			tb.Fatalf("a holder's renewal answered %d %d", counted, written)
		}
		leases, err := evsservice.ReadLeases(ctx, redis, repoId)
		if err != nil || len(leases) != 2 {
			tb.Fatalf("the repo holds %d leases %v", len(leases), err)
		}
		// holder two's lease lapses where holder one's, renewed a minute on, has not: a place is free
		if counted, written := lease(start.Add(settings.LeaseLife), "holder three", 2); counted != 2 || written != 1 {
			tb.Fatalf("a holder after a lapse answered %d %d", counted, written)
		}
	})
}
