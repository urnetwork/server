package controller

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The per-host activation budget (connect#230, support inbox 8148), against
// the database: what a fleet can activate at once, how often one address is
// probed, and that the budgets of an activation are spent one spend at a time.

// The address of host i of a fleet on one family: the ipv4 hosts spread over
// the three documentation /24s in turn, and each ipv6 host in a /48 of its
// own.
func testExtenderFleetIp(t testing.TB, i int, ipVersion int) string {
	t.Helper()
	if ipVersion == 6 {
		return fmt.Sprintf("2001:db8:%x::1", i+1)
	}
	blocks := []string{"192.0.2.", "198.51.100.", "203.0.113."}
	hostNumber := i/len(blocks) + 1
	if 254 < hostNumber {
		t.Fatalf("no documentation ipv4 address for host %d", i)
	}
	return fmt.Sprintf("%s%d", blocks[i%len(blocks)], hostNumber)
}

// Stores host as an extender of its account that passed activation at
// `activateTime` on each family given, from that address, as an activation
// stores one (model.ActivateNetworkExtender): the extender, its address of
// each family and the history row of each activation. The addresses are left
// inactive, as the uptime probes leave a fleet that went down.
func seedTestProvenExtenderHost(
	t testing.TB,
	ctx context.Context,
	host *testExtenderHost,
	ipVersionIps map[int]string,
	activateTime time.Time,
) {
	t.Helper()
	extenderId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_extender (
				extender_id,
				network_id,
				client_id,
				public_key,
				create_time,
				active
			)
			VALUES ($1, $2, $3, $4, $5, false)
			`,
			extenderId,
			host.byJwt.NetworkId,
			*host.byJwt.ClientId,
			host.publicKey,
			activateTime.UTC(),
		))
		for ipVersion, ip := range ipVersionIps {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO network_extender_address (
					extender_id,
					ip_version,
					ip,
					carriers,
					activate_time,
					active
				)
				VALUES ($1, $2, $3, 'tcp', $4, false)
				`,
				extenderId,
				ipVersion,
				netip.MustParseAddr(ip),
				activateTime.UTC(),
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO network_extender_activation (
					activation_id,
					extender_id,
					activate_time,
					ip_version
				)
				VALUES ($1, $2, $3, $4)
				`,
				server.NewId(),
				extenderId,
				activateTime.UTC(),
				ipVersion,
			))
		}
	})
}

// Moves every budget attempt and every stored activation back by `by`, which
// is what the clock moving on by `by` does to them.
func shiftTestExtenderActivationsBack(t testing.TB, ctx context.Context, by time.Duration) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		for _, update := range []string{
			`UPDATE network_user_action_attempt SET create_time = create_time - make_interval(secs => $1)`,
			`UPDATE network_extender_activation SET activate_time = activate_time - make_interval(secs => $1)`,
			`UPDATE network_extender_address SET activate_time = activate_time - make_interval(secs => $1)`,
		} {
			server.RaisePgResult(tx.Exec(ctx, update, by.Seconds()))
		}
	})
}

// Records `count` attempts of `action` by `id` at `createTime`.
func seedTestAccountActionAttempts(
	t testing.TB,
	ctx context.Context,
	id server.Id,
	action string,
	count int,
	createTime time.Time,
) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_user_action_attempt (
				network_user_action_attempt_id,
				user_id,
				action,
				create_time
			)
			SELECT gen_random_uuid(), $1, $2, $3 FROM generate_series(1, $4)
			`,
			id,
			action,
			createTime.UTC(),
			count,
		))
	})
}

// Every budget attempt recorded, over every id and action.
func testTotalAccountActionAttemptCount(t testing.TB, ctx context.Context) int {
	t.Helper()
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_user_action_attempt`).Scan(&count))
	})
	return count
}

// Fake probes that count the activations they probe, by family.
type testCountingProbes struct {
	stateLock       sync.Mutex
	ipVersionCounts map[int]int
}

func newTestCountingProbes() *testCountingProbes {
	return &testCountingProbes{
		ipVersionCounts: map[int]int{},
	}
}

// The probes of the activation service: each passes, or fails with
// `carrierErr`.
func (self *testCountingProbes) probes(carrierErr error) func(clientIp netip.Addr, ipVersion int) *extenderActivationProbes {
	fakeProbes := testFakeExtenderProbes(carrierErr)
	return func(clientIp netip.Addr, ipVersion int) *extenderActivationProbes {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.ipVersionCounts[ipVersion] += 1
		}()
		return fakeProbes(clientIp, ipVersion)
	}
}

func (self *testCountingProbes) count(ipVersion int) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.ipVersionCounts[ipVersion]
}

// One activation of `host` from `ip`.
func testActivateHost(
	t testing.TB,
	ctx context.Context,
	service *extenderActivateService,
	host *testExtenderHost,
	ip string,
) *ExtenderActivateResult {
	t.Helper()
	result, err := service.activate(host.activateArgs(), host.session(ctx, ip))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// A fleet whose hosts have all passed activation restarts at once
// (connect#230). An account's limit is its floor, 60 an hour per family, plus 6
// for each of its hosts that passed between an hour and 7 days ago, so 100
// proven dual-stack hosts re-activate both families in the same minute.
// Before, the limit was 60 an hour per family whatever the hosts had proven,
// and the 61st host of each family was refused for an hour. The ipv4 hosts
// share the three documentation /24s, of which 32 hosts each count, so ipv4's
// limit is 60 + 6 × 96 = 636; each ipv6 host is in a /48 of its own.
func TestExtenderActivateRestartsAProvenFleetAtOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		account := newTestExtenderAccount(ctx)
		const hostCount = 100
		hosts := []*testExtenderHost{}
		// passed two hours ago, and since gone down
		provenTime := server.NowUtc().Add(-2 * time.Hour)
		for i := range hostCount {
			host := account.newHost(t, ctx)
			hosts = append(hosts, host)
			seedTestProvenExtenderHost(t, ctx, host, map[int]string{
				4: testExtenderFleetIp(t, i, 4),
				6: testExtenderFleetIp(t, i, 6),
			}, provenTime)
		}

		service := newExtenderActivateService(DefaultExtenderActivateSettings())
		probes := newTestCountingProbes()
		service.testProbes = probes.probes(nil)
		for i, host := range hosts {
			for _, ipVersion := range []int{4, 6} {
				result := testActivateHost(t, ctx, service, host, testExtenderFleetIp(t, i, ipVersion))
				if !result.Activated {
					t.Fatalf("host %d of the proven fleet was refused ipv%d at its restart: %q", i+1, ipVersion, result.Error)
				}
			}
		}
		connect.AssertEqual(t, probes.count(4), hostCount)
		connect.AssertEqual(t, probes.count(6), hostCount)
	})
}

// A new fleet ramps up an hour at a time (connect#230). In its first hour the
// account has its floor of 60 an hour: the 61st host is refused, with the wait
// until the hour's first attempt leaves the window in `retry_after_seconds`.
// An hour on, the 60 that passed are proven hosts, the limit is 60 + 6 × 60 =
// 420, and the other 240 hosts all activate. Before, the second hour admitted
// 60 again, and a fleet of 300 took five hours to activate.
func TestExtenderActivateRampsANewFleet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		account := newTestExtenderAccount(ctx)
		const hostCount = 300
		hosts := []*testExtenderHost{}
		for range hostCount {
			hosts = append(hosts, account.newHost(t, ctx))
		}
		activate := func(service *extenderActivateService, i int) *ExtenderActivateResult {
			return testActivateHost(t, ctx, service, hosts[i], testExtenderFleetIp(t, i, 4))
		}

		probes := newTestCountingProbes()
		service := newExtenderActivateService(DefaultExtenderActivateSettings())
		service.testProbes = probes.probes(nil)
		for i := range 60 {
			if result := activate(service, i); !result.Activated {
				t.Fatalf("host %d was refused in the fleet's first hour: %q", i+1, result.Error)
			}
		}
		refused := activate(service, 60)
		if refused.Activated || !strings.Contains(refused.Error, "activating your account's extenders over IPv4") {
			t.Fatalf("host 61 was not refused by the account's floor: activated %t, %q", refused.Activated, refused.Error)
		}
		if refused.RetryAfterSeconds <= 50*60 || 60*60 < refused.RetryAfterSeconds {
			t.Errorf("the refusal of host 61 asks for %d seconds, want the rest of the hour", refused.RetryAfterSeconds)
		}
		connect.AssertEqual(t, probes.count(4), 60)

		// an hour and a minute on. A process's count of proven hosts would have
		// long expired, so the hour starts with a service of its own
		shiftTestExtenderActivationsBack(t, ctx, 61*time.Minute)
		service = newExtenderActivateService(DefaultExtenderActivateSettings())
		service.testProbes = probes.probes(nil)
		for i := 60; i < hostCount; i += 1 {
			if result := activate(service, i); !result.Activated {
				t.Fatalf("host %d was refused in the fleet's second hour: %q", i+1, result.Error)
			}
		}
		connect.AssertEqual(t, probes.count(4), hostCount)
	})
}

// One address is probed at most 6 times an hour per family, whatever clients
// and accounts post from it (connect#230), since the address is what the probe
// dials. Before, the budgets were the client's and the account's, so K
// accounts posting from one address had it probed up to K × 60 times an hour.
// Another /32 is another address, the families are apart, an ipv6 address is
// its /64, and the refusal spends nothing.
func TestExtenderActivateBudgetsEachAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		firstAccount := newTestExtenderAccount(ctx)
		secondAccount := newTestExtenderAccount(ctx)
		firstHost := firstAccount.newHost(t, ctx)
		secondHost := firstAccount.newHost(t, ctx)
		thirdHost := secondAccount.newHost(t, ctx)

		service := newExtenderActivateService(DefaultExtenderActivateSettings())
		service.testProbes = testFakeExtenderProbes(nil)
		requireActivated := func(name string, host *testExtenderHost, ip string) {
			t.Helper()
			if result := testActivateHost(t, ctx, service, host, ip); !result.Activated {
				t.Fatalf("%s from %s was refused: %q", name, ip, result.Error)
			}
		}
		requireAddressRefused := func(name string, host *testExtenderHost, ip string, family string) {
			t.Helper()
			attemptCount := testTotalAccountActionAttemptCount(t, ctx)
			refused := testActivateHost(t, ctx, service, host, ip)
			if refused.Activated || !strings.Contains(refused.Error, "activating an extender from this address over "+family) {
				t.Fatalf("%s from %s was not refused by the address's budget: activated %t, %q", name, ip, refused.Activated, refused.Error)
			}
			if refused.RetryAfterSeconds <= 50*60 || 60*60 < refused.RetryAfterSeconds {
				t.Errorf("%s: the refusal asks for %d seconds, want the rest of the hour", name, refused.RetryAfterSeconds)
			}
			connect.AssertEqual(t, testTotalAccountActionAttemptCount(t, ctx), attemptCount)
		}

		// six posts from one address, over two accounts and three clients
		const ipv4 = "198.51.100.7"
		for i, host := range []*testExtenderHost{firstHost, firstHost, firstHost, secondHost, secondHost, thirdHost} {
			requireActivated(fmt.Sprintf("post %d", i+1), host, ipv4)
		}
		requireAddressRefused("the seventh post", thirdHost, ipv4, "IPv4")
		requireActivated("a post from another /32", thirdHost, "198.51.100.8")

		// ipv6 has a budget of its own, kept by /64
		for i, post := range []struct {
			host *testExtenderHost
			ip   string
		}{
			{host: thirdHost, ip: "2001:db8:7::1"},
			{host: firstHost, ip: "2001:db8:7::2"},
			{host: firstHost, ip: "2001:db8:7::3"},
			{host: firstHost, ip: "2001:db8:7::4"},
			{host: secondHost, ip: "2001:db8:7::5"},
			{host: secondHost, ip: "2001:db8:7::6"},
		} {
			requireActivated(fmt.Sprintf("ipv6 post %d", i+1), post.host, post.ip)
		}
		requireAddressRefused("the seventh post from the /64", thirdHost, "2001:db8:7:0:ffff::1", "IPv6")
		requireActivated("a post from another /64", thirdHost, "2001:db8:7:1::1")
	})
}

// Hosts whose probes fail never raise the account's limit (connect#230): a
// proven host is one with a stored activation, and only a passed required
// test stores one. A fleet of failing hosts is held at the floor of 60 an hour
// in its first hour and again in its second, and nothing of it is stored.
func TestExtenderActivateFailingHostsDoNotRaiseTheLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		account := newTestExtenderAccount(ctx)
		const hostCount = 100
		hosts := []*testExtenderHost{}
		for range hostCount {
			hosts = append(hosts, account.newHost(t, ctx))
		}

		probes := newTestCountingProbes()
		requireHeldAtTheFloor := func(hour string, firstHost int) {
			t.Helper()
			service := newExtenderActivateService(DefaultExtenderActivateSettings())
			service.testProbes = probes.probes(errors.New("connection refused"))
			for i := firstHost; i < firstHost+60; i += 1 {
				result := testActivateHost(t, ctx, service, hosts[i], testExtenderFleetIp(t, i, 4))
				if result.Activated || !strings.Contains(result.Error, "tcp carrier did not answer") {
					t.Fatalf("%s: host %d was not refused by its probe: activated %t, %q", hour, i+1, result.Activated, result.Error)
				}
			}
			refused := testActivateHost(t, ctx, service, hosts[firstHost+60], testExtenderFleetIp(t, firstHost+60, 4))
			if refused.Activated || !strings.Contains(refused.Error, "activating your account's extenders over IPv4") {
				t.Fatalf("%s: host %d was not refused at the floor: activated %t, %q", hour, firstHost+61, refused.Activated, refused.Error)
			}
		}

		requireHeldAtTheFloor("the first hour", 0)
		connect.AssertEqual(t, probes.count(4), 60)
		shiftTestExtenderActivationsBack(t, ctx, 61*time.Minute)
		requireHeldAtTheFloor("the second hour", hostCount-61)
		connect.AssertEqual(t, probes.count(4), 120)

		storedCount := 0
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT COUNT(*) FROM network_extender WHERE network_id = $1`,
				account.networkId,
			).Scan(&storedCount))
		})
		connect.AssertEqual(t, storedCount, 0)
	})
}

// The account's limit stops at its ceiling of 1200 an hour per family
// (connect#230), however many hosts it has proven, and an ipv4 /24 counts at
// most 32 of them. 500 proven ipv6 hosts, each in a /48 of its own, would raise
// ipv6 to 60 + 6 × 500 = 3060, and the ceiling holds it at 1200; 100 proven
// ipv4 hosts over the three documentation /24s count 96, for 636. The refusal
// past either limit waits for the hour's oldest attempt to leave the window.
func TestExtenderActivateHoldsTheAccountAtItsCeiling(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		account := newTestExtenderAccount(ctx)
		provenTime := server.NowUtc().Add(-2 * time.Hour)
		for i := range 500 {
			ipVersionIps := map[int]string{
				6: testExtenderFleetIp(t, i, 6),
			}
			if i < 100 {
				ipVersionIps[4] = testExtenderFleetIp(t, i, 4)
			}
			seedTestProvenExtenderHost(t, ctx, account.newHost(t, ctx), ipVersionIps, provenTime)
		}

		service := newExtenderActivateService(DefaultExtenderActivateSettings())
		service.testProbes = testFakeExtenderProbes(nil)
		// the hour's other attempts, the oldest of which leaves the window in
		// half an hour
		attemptTime := server.NowUtc().Add(-30 * time.Minute)
		for _, c := range []struct {
			ipVersion int
			limit     int
			action    string
			family    string
			lastIp    string
			overIp    string
		}{
			{
				ipVersion: 6,
				limit:     1200,
				action:    model.AccountActionExtenderActivateAccountV6,
				family:    "IPv6",
				lastIp:    "2001:db8:ffff::1",
				overIp:    "2001:db8:fffe::1",
			},
			{
				ipVersion: 4,
				limit:     636,
				action:    model.AccountActionExtenderActivateAccountV4,
				family:    "IPv4",
				lastIp:    "192.0.2.250",
				overIp:    "192.0.2.251",
			},
		} {
			seedTestAccountActionAttempts(t, ctx, account.userId, c.action, c.limit-1, attemptTime)
			if result := testActivateHost(t, ctx, service, account.newHost(t, ctx), c.lastIp); !result.Activated {
				t.Fatalf("ipv%d: activation %d of the hour was refused: %q", c.ipVersion, c.limit, result.Error)
			}
			refused := testActivateHost(t, ctx, service, account.newHost(t, ctx), c.overIp)
			if refused.Activated || !strings.Contains(refused.Error, "activating your account's extenders over "+c.family) {
				t.Fatalf(
					"ipv%d: activation %d of the hour was not refused by the account's limit: activated %t, %q",
					c.ipVersion, c.limit+1, refused.Activated, refused.Error)
			}
			if refused.RetryAfterSeconds <= 25*60 || 30*60 < refused.RetryAfterSeconds {
				t.Errorf("ipv%d: the refusal asks for %d seconds, want the half hour until the oldest attempt leaves", c.ipVersion, refused.RetryAfterSeconds)
			}
		}
	})
}

// Two posts at the limit spend it once (connect#230). The budgets of an
// activation are spent one spend at a time, each reading what the one before
// it spent. Before, two posts could both read the account's 59 of 60 and both
// spend, and both were probed; at the address budget, which bounds what one
// address is sent, any number of accounts posting at once could have it
// probed past its 6 an hour.
func TestExtenderActivateSpendsTheLastAttemptOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		account := newTestExtenderAccount(ctx)
		firstHost := account.newHost(t, ctx)
		secondHost := account.newHost(t, ctx)
		seedTestAccountActionAttempts(
			t,
			ctx,
			account.userId,
			model.AccountActionExtenderActivateAccountV4,
			59,
			server.NowUtc().Add(-30*time.Minute),
		)

		service := newExtenderActivateService(DefaultExtenderActivateSettings())
		probes := newTestCountingProbes()
		service.testProbes = probes.probes(nil)

		// the first spend to read its budgets holds there until the second has
		// read them too, which nothing kept it from, or is waiting for the first
		reads := make(chan struct{}, 2)
		lockWaits := make(chan struct{}, 2)
		var readCount atomic.Int32
		model.Testing_AccountActionBudgetsBeforeSpend = func() {
			if readCount.Add(1) == 1 {
				select {
				case <-reads:
				case <-lockWaits:
				case <-time.After(30 * time.Second):
					t.Errorf("the second spend neither read its budgets nor waited for the first")
				}
			} else {
				reads <- struct{}{}
			}
		}
		model.Testing_AccountActionBudgetsLockWait = func() {
			lockWaits <- struct{}{}
		}
		defer func() {
			model.Testing_AccountActionBudgetsBeforeSpend = nil
			model.Testing_AccountActionBudgetsLockWait = nil
		}()

		results := make([]*ExtenderActivateResult, 2)
		var postWait sync.WaitGroup
		for i, post := range []struct {
			host *testExtenderHost
			ip   string
		}{
			{host: firstHost, ip: "198.51.100.1"},
			{host: secondHost, ip: "198.51.100.2"},
		} {
			postWait.Add(1)
			go func() {
				defer postWait.Done()
				result, err := service.activate(post.host.activateArgs(), post.host.session(ctx, post.ip))
				if err != nil {
					t.Errorf("post %d: %s", i+1, err)
					return
				}
				results[i] = result
			}()
		}
		postWait.Wait()

		activatedCount := 0
		refusal := ""
		for i, result := range results {
			if result == nil {
				t.Fatalf("post %d has no answer", i+1)
			}
			if result.Activated {
				activatedCount += 1
			} else {
				refusal = result.Error
			}
		}
		if activatedCount != 1 {
			t.Fatalf("%d of the two posts at the limit were activated, want one (refusal %q)", activatedCount, refusal)
		}
		if !strings.Contains(refusal, "activating your account's extenders over IPv4") {
			t.Errorf("the post past the limit was refused by %q, want the account's limit", refusal)
		}
		connect.AssertEqual(t, probes.count(4), 1)
		attempts := testAccountActionAttempts(t, ctx, account.userId)
		connect.AssertEqual(t, attempts[model.AccountActionExtenderActivateAccountV4], 60)
	})
}
