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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The per-host activation budget (connect#230, support inbox 8148), against
// the database: what a fleet can activate at once, how often one address is
// probed, and that the budgets of an activation are spent one spend at a time.

// How far past the moment a budget has room its refusal may ask a host to
// wait (BudgetRetryAfterJitter), in seconds.
var testBudgetRetryAfterJitterSeconds = int(DefaultExtenderActivateSettings().BudgetRetryAfterJitter / time.Second)

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

// A host the uptime probes revoked still counts as proven until 7 days after
// its last pass (connect#230): a fleet that went down is the fleet whose restart
// must not queue. Six failed probes leave its address inactive and its extender
// revoked (model.RecordNetworkExtenderProbeResult), and neither touches the
// history of its passes or the address it passed from, which are what a proven
// host is read from. Here 10 hosts that passed 6 days ago and were revoked 5 days
// ago raise the account's limit to 60 + 6 × 10 = 120 to the attempt, one that
// last passed 8 days ago does not, and a revoked host's post from its own
// address is not held off by that address's spent budget.
func TestExtenderActivateCountsHostsTheUptimeProbesRevoked(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		settings := DefaultExtenderActivateSettings()
		service := newExtenderActivateService(settings)
		service.testProbes = testFakeExtenderProbes(nil)

		account := newTestExtenderAccount(ctx)
		const hostCount = 10
		hosts := []*testExtenderHost{}
		extenderIds := []server.Id{}
		for i := range hostCount {
			host := account.newHost(t, ctx)
			result := testActivateHost(t, ctx, service, host, testExtenderFleetIp(t, i, 4))
			if !result.Activated || result.ExtenderId == nil {
				t.Fatalf("host %d did not activate: %q", i+1, result.Error)
			}
			hosts = append(hosts, host)
			extenderIds = append(extenderIds, *result.ExtenderId)
		}
		shiftTestExtenderActivationsBack(t, ctx, 6*24*time.Hour)

		probeTime := server.NowUtc().Add(-5 * 24 * time.Hour)
		for i, extenderId := range extenderIds {
			var outcome *model.NetworkExtenderProbeOutcome
			for range 6 {
				outcome = model.RecordNetworkExtenderProbeResult(
					ctx,
					extenderId,
					4,
					false,
					probeTime,
					6,
					func(extender *model.NetworkExtender, issueTime time.Time) ([]byte, error) {
						return []byte("revocation"), nil
					},
				)
			}
			if !outcome.AddressDeactivated || !outcome.ExtenderRevoked {
				t.Fatalf("the probes did not revoke host %d: %+v", i+1, outcome)
			}
		}
		// went down too, but its last pass has left the 7 days
		seedTestProvenExtenderHost(t, ctx, account.newHost(t, ctx), map[int]string{
			4: testExtenderFleetIp(t, hostCount, 4),
		}, server.NowUtc().Add(-8*24*time.Hour))

		// the account has spent its floor, and the first host's address its
		// budget: the first host comes back all the same
		const accountAction = model.AccountActionExtenderActivateAccountV4
		attemptTime := server.NowUtc().Add(-time.Minute)
		seedTestAccountActionAttempts(t, ctx, account.userId, accountAction, settings.AccountRateLimit, attemptTime)
		firstIp := testExtenderFleetIp(t, 0, 4)
		seedTestAccountActionAttempts(
			t,
			ctx,
			settings.addressBudgetId(netip.MustParseAddr(firstIp)),
			model.AccountActionExtenderActivateAddressV4,
			settings.AddressRateLimit,
			attemptTime,
		)
		if result := testActivateHost(t, ctx, service, hosts[0], firstIp); !result.Activated {
			t.Fatalf("a revoked host was refused at its restart past the floor: %q", result.Error)
		}

		// the floor and the first host's attempt are spent: spend up to one
		// short of the limit, 119 of 120
		seedTestAccountActionAttempts(
			t,
			ctx,
			account.userId,
			accountAction,
			settings.AccountRateLimitPerProvenHost*hostCount-2,
			attemptTime,
		)
		if result := testActivateHost(t, ctx, service, hosts[1], testExtenderFleetIp(t, 1, 4)); !result.Activated {
			t.Fatalf("the account's 120th attempt was refused: %q", result.Error)
		}
		refused := testActivateHost(t, ctx, service, hosts[2], testExtenderFleetIp(t, 2, 4))
		if refused.Activated || !strings.Contains(refused.Error, "your account's extenders over IPv4") {
			t.Fatalf("the account's 121st attempt was not refused by its budget: activated %t, %q", refused.Activated, refused.Error)
		}
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
		if refused.RetryAfterSeconds <= 50*60 || 60*60+testBudgetRetryAfterJitterSeconds < refused.RetryAfterSeconds {
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

// One address is probed at most 6 times an hour per family for keys new to
// it, whatever clients and accounts post from it (connect#230), since the
// address is what the probe dials. Before, the budgets were the client's and
// the account's, so K accounts posting from one address had it probed up to
// K × 60 times an hour. Another /32 is another address, the families are
// apart, an ipv6 address is its /64, and the refusal spends nothing. A key
// that passed at the address spends a budget of its own there instead
// (TestExtenderActivateKeepsAProvenKeysAddressBudgetItsOwn), so every post
// here is of a key new to the address.
func TestExtenderActivateBudgetsEachAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		// seven clients over two accounts, each with a key of its own
		firstAccount := newTestExtenderAccount(ctx)
		secondAccount := newTestExtenderAccount(ctx)
		hosts := []*testExtenderHost{}
		for range 4 {
			hosts = append(hosts, firstAccount.newHost(t, ctx))
		}
		for range 3 {
			hosts = append(hosts, secondAccount.newHost(t, ctx))
		}
		seventhHost := hosts[6]

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
			if refused.RetryAfterSeconds <= 50*60 || 60*60+testBudgetRetryAfterJitterSeconds < refused.RetryAfterSeconds {
				t.Errorf("%s: the refusal asks for %d seconds, want the rest of the hour", name, refused.RetryAfterSeconds)
			}
			connect.AssertEqual(t, testTotalAccountActionAttemptCount(t, ctx), attemptCount)
		}

		// six posts from one address, over two accounts and six clients
		const ipv4 = "198.51.100.7"
		for i, host := range hosts[:6] {
			requireActivated(fmt.Sprintf("post %d", i+1), host, ipv4)
		}
		requireAddressRefused("the seventh post", seventhHost, ipv4, "IPv4")
		requireActivated("a post from another /32", seventhHost, "198.51.100.8")

		// ipv6 has a budget of its own, kept by /64, where no key has passed
		for i, host := range hosts[:6] {
			requireActivated(fmt.Sprintf("ipv6 post %d", i+1), host, fmt.Sprintf("2001:db8:7::%d", i+1))
		}
		requireAddressRefused("the seventh post from the /64", seventhHost, "2001:db8:7:0:ffff::1", "IPv6")
		requireActivated("a post from another /64", seventhHost, "2001:db8:7:1::1")
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
				if result.Activated || !strings.Contains(result.Error, "the tcp carrier failed after ") {
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
			if refused.RetryAfterSeconds <= 25*60 || 30*60+testBudgetRetryAfterJitterSeconds < refused.RetryAfterSeconds {
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

// A busy api and each budget's refusal are counted where they ended
// (urnetwork_extender_activations_total, connect#230): stage busy with why,
// and stage budget with the budget that refused -- the client's, the
// address's, the account's. Before, both refusals were answered around the
// count, so neither a fleet's busy refusals nor which budget held it back
// showed on the extenders dashboard.
func TestExtenderActivateCountsBusyAndBudgetRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		metrics := newExtenderActivationMetrics(prometheus.NewRegistry())
		settings := DefaultExtenderActivateSettings()
		settings.metrics = metrics
		settings.ClientRateLimit = 1
		settings.AddressRateLimit = 1
		settings.AccountRateLimit = 2
		settings.MaxConcurrentProbes = 1
		settings.BusyRetryAfterMin = 90 * time.Second
		settings.BusyRetryAfterMax = 90 * time.Second
		service := newExtenderActivateService(settings)

		// the first probe holds the one slot until it is let go
		probeStarted := make(chan struct{}, 8)
		probeRelease := make(chan struct{})
		holdFirst := &atomic.Bool{}
		holdFirst.Store(true)
		service.testProbes = func(clientIp netip.Addr, ipVersion int) *extenderActivationProbes {
			probes := testFakeExtenderProbes(nil)(clientIp, ipVersion)
			passCarrier := probes.carrier
			probes.carrier = func(ctx context.Context, connectMode connect.ExtenderConnectMode, port int) error {
				if holdFirst.CompareAndSwap(true, false) {
					probeStarted <- struct{}{}
					<-probeRelease
				}
				return passCarrier(ctx, connectMode, port)
			}
			return probes
		}

		account := newTestExtenderAccount(ctx)
		firstHost := account.newHost(t, ctx)
		secondHost := account.newHost(t, ctx)
		thirdHost := account.newHost(t, ctx)

		held := make(chan *ExtenderActivateResult, 1)
		go func() {
			result, err := service.activate(firstHost.activateArgs(), firstHost.session(ctx, "198.51.100.1"))
			if err != nil {
				t.Error(err)
			}
			held <- result
		}()
		<-probeStarted
		busy := testActivateHost(t, ctx, service, secondHost, "198.51.100.2")
		connect.AssertEqual(t, busy.Activated, false)
		connect.AssertEqual(t, busy.RetryAfterSeconds, 90)
		close(probeRelease)
		if result := <-held; result == nil || !result.Activated {
			t.Fatalf("the held activation was refused: %+v", result)
		}

		// the first client's second post, its own budget spent
		clientRefused := testActivateHost(t, ctx, service, firstHost, "198.51.100.1")
		// another client from the first client's address, its budget spent
		addressRefused := testActivateHost(t, ctx, service, secondHost, "198.51.100.1")
		// a second activation of the account spends its floor of 2, and a
		// third client of it is refused by the account
		if result := testActivateHost(t, ctx, service, secondHost, "198.51.100.2"); !result.Activated {
			t.Fatalf("the account's second activation was refused: %q", result.Error)
		}
		accountRefused := testActivateHost(t, ctx, service, thirdHost, "198.51.100.3")
		for _, refused := range []*ExtenderActivateResult{clientRefused, addressRefused, accountRefused} {
			if refused.Activated || refused.RetryAfterSeconds <= 0 {
				t.Fatalf("expected a budget refusal with a wait, got activated %t, %q, %d", refused.Activated, refused.Error, refused.RetryAfterSeconds)
			}
		}

		for _, c := range []struct {
			stage   string
			outcome string
			want    float64
		}{
			{stage: "busy", outcome: "probes", want: 1},
			{stage: "budget", outcome: "client", want: 1},
			{stage: "budget", outcome: "address", want: 1},
			{stage: "budget", outcome: "account", want: 1},
			{stage: "store", outcome: "activated", want: 2},
		} {
			if got := testutil.ToFloat64(metrics.activations.WithLabelValues(c.stage, c.outcome, "ipv4")); got != c.want {
				t.Errorf("%s %s ipv4 counted %v, want %v", c.stage, c.outcome, got, c.want)
			}
		}
	})
}

// A public extender relays posts to the api from its own address, so any
// account's posts through it spend that address's budget. Six junk posts an
// hour through an extender, from another account, used to refuse the
// extender's own re-activation from its own address for the rest of the
// hour, renewably (connect#230). A key its network already proved at the
// address now spends a budget of its own there, which no other post can: the
// junk posts leave its re-activation alone, the other account posting that
// same key from the address is refused for the key's network, and the key's
// own posts are held to 6 an hour.
func TestExtenderActivateKeepsAProvenKeysAddressBudgetItsOwn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		const extenderIp = "198.51.100.50"
		owner := newTestExtenderAccount(ctx)
		ownerHost := owner.newHost(t, ctx)
		seedTestProvenExtenderHost(t, ctx, ownerHost, map[int]string{4: extenderIp}, server.NowUtc().Add(-2*time.Hour))
		secondOwnerHost := owner.newHost(t, ctx)
		other := newTestExtenderAccount(ctx)
		otherHost := other.newHost(t, ctx)
		otherSecondHost := other.newHost(t, ctx)

		// another account's junk posts through the extender, each with a key
		// the extender does not hold, so each probe fails after its spend
		junkService := newExtenderActivateService(DefaultExtenderActivateSettings())
		junkService.testProbes = testFakeExtenderProbes(errors.New("the extender answered with another identity key"))
		for i := range 6 {
			junk := testActivateHost(t, ctx, junkService, &testExtenderHost{
				byJwt:     otherHost.byJwt,
				publicKey: owner.newHost(t, ctx).publicKey,
			}, extenderIp)
			if junk.Activated || !strings.Contains(junk.Error, "another identity key") {
				t.Fatalf("junk post %d: activated %t, %q", i+1, junk.Activated, junk.Error)
			}
		}

		service := newExtenderActivateService(DefaultExtenderActivateSettings())
		service.testProbes = testFakeExtenderProbes(nil)
		if result := testActivateHost(t, ctx, service, ownerHost, extenderIp); !result.Activated {
			t.Fatalf("the extender's re-activation from its own address was refused: %q", result.Error)
		}

		// the other account posting the extender's own key from its address,
		// from a client with its own budget whole, is refused for the key's
		// network before any budget
		theirs := testActivateHost(t, ctx, service, &testExtenderHost{
			byJwt:     otherSecondHost.byJwt,
			publicKey: ownerHost.publicKey,
		}, extenderIp)
		if !testKeyOtherNetworkRefusal(theirs) {
			t.Fatalf("another account's post of the extender's key from its address: activated %t, %q", theirs.Activated, theirs.Error)
		}

		// the key's own budget at the address: five more over two clients of
		// its network, and the seventh is refused by it
		for i, host := range []*testExtenderHost{ownerHost, ownerHost, secondOwnerHost, secondOwnerHost, secondOwnerHost} {
			result := testActivateHost(t, ctx, service, &testExtenderHost{byJwt: host.byJwt, publicKey: ownerHost.publicKey}, extenderIp)
			if !result.Activated {
				t.Fatalf("re-activation %d of the extender from its own address was refused: %q", i+2, result.Error)
			}
		}
		seventh := testActivateHost(t, ctx, service, &testExtenderHost{byJwt: secondOwnerHost.byJwt, publicKey: ownerHost.publicKey}, extenderIp)
		if seventh.Activated || !strings.Contains(seventh.Error, "re-activating an extender from its own address over IPv4") {
			t.Fatalf("the seventh re-activation in the hour: activated %t, %q", seventh.Activated, seventh.Error)
		}
		if seventh.RetryAfterSeconds <= 0 {
			t.Errorf("the seventh re-activation's refusal asks for no wait")
		}
	})
}

// The key's network is checked first, then the probe guard, then the budgets.
// Another network's post of a stored key takes no probe slot and records no
// budget attempt, however many it posts, so posts relayed through an extender
// with its own key cannot spend its address's budget. With no probe slot free
// the post still reads as the key's refusal, not as a busy api.
func TestExtenderActivateChecksTheKeysNetworkBeforeTheGuardAndTheBudgets(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		const extenderIp = "198.51.100.51"
		owner := newTestExtenderAccount(ctx)
		ownerHost := owner.newHost(t, ctx)
		seedTestProvenExtenderHost(t, ctx, ownerHost, map[int]string{4: extenderIp}, server.NowUtc().Add(-2*time.Hour))
		other := newTestExtenderAccount(ctx)
		otherHost := other.newHost(t, ctx)
		theirPost := &testExtenderHost{byJwt: otherHost.byJwt, publicKey: ownerHost.publicKey}

		noSlotSettings := DefaultExtenderActivateSettings()
		noSlotSettings.MaxConcurrentProbes = 0
		noSlotService := newExtenderActivateService(noSlotSettings)
		if refused := testActivateHost(t, ctx, noSlotService, theirPost, extenderIp); !testKeyOtherNetworkRefusal(refused) {
			t.Fatalf("with no probe slot free: activated %t, %q", refused.Activated, refused.Error)
		}

		settings := DefaultExtenderActivateSettings()
		service := newExtenderActivateService(settings)
		probes := newTestCountingProbes()
		service.testProbes = probes.probes(nil)
		attemptCount := testTotalAccountActionAttemptCount(t, ctx)
		for i := range settings.AddressRateLimit + 1 {
			if refused := testActivateHost(t, ctx, service, theirPost, extenderIp); !testKeyOtherNetworkRefusal(refused) {
				t.Fatalf("post %d of another network's key: activated %t, %q", i+1, refused.Activated, refused.Error)
			}
		}
		if got := testTotalAccountActionAttemptCount(t, ctx); got != attemptCount {
			t.Fatalf("the refused posts recorded %d budget attempts", got-attemptCount)
		}
		if got := probes.count(4); got != 0 {
			t.Fatalf("the refused posts were probed %d times", got)
		}

		// the address's budget is whole for the other account's own key
		if result := testActivateHost(t, ctx, service, otherHost, extenderIp); !result.Activated {
			t.Fatalf("the other account's own key was refused: %q", result.Error)
		}
		if got := probes.count(4); got != 1 {
			t.Fatalf("the other account's own key was probed %d times", got)
		}
	})
}

// The attempts one activation records for its budgets -- the client's, the
// caller address's, the account's -- share no create_time (connect#230). They
// were all recorded at the spend's one time, which joined the address's hashed
// row to the client's and the account's exactly.
func TestExtenderActivateRecordsItsBudgetAttemptsAtTimesOfTheirOwn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		account := newTestExtenderAccount(ctx)
		host := account.newHost(t, ctx)
		service := newExtenderActivateService(DefaultExtenderActivateSettings())
		service.testProbes = testFakeExtenderProbes(nil)
		if result := testActivateHost(t, ctx, service, host, "198.51.100.9"); !result.Activated {
			t.Fatalf("the activation was refused: %q", result.Error)
		}

		actionTimes := map[string]time.Time{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT action, create_time FROM network_user_action_attempt`)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var action string
					var createTime time.Time
					server.Raise(result.Scan(&action, &createTime))
					actionTimes[action] = createTime
				}
			})
		})
		connect.AssertEqual(t, len(actionTimes), 3)
		seenTimes := map[time.Time]string{}
		for action, createTime := range actionTimes {
			if otherAction, ok := seenTimes[createTime]; ok {
				t.Fatalf("the %s and %s attempts share the create_time %s", action, otherAction, createTime)
			}
			seenTimes[createTime] = action
		}
	})
}
