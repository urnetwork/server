package work

import (
	"context"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The dns chain's withdrawal judged again where it writes, end to end
// (lag.dev/PLAN.md C9, amendment 24): an extender that activates while a dns
// pass runs, right at the end of its away grace, keeps its certificate, and
// what must go still goes, with the reason its extender has when it is
// withdrawn.

// The extender activates again from the IPv4 address `ip`, as
// reactivateTestMeasurementExtender does, and its transaction calls `hold`
// once it has written the extender row: the signer runs in the transaction,
// after the upsert.
func activateTestMeasurementRaceExtender(
	ctx context.Context,
	index int,
	ip string,
	hold func(),
) *model.NetworkExtenderWithAddresses {
	return model.ActivateNetworkExtender(ctx, &model.NetworkExtenderActivation{
		NetworkId:   testMeasurementExtenderNetworkId(index),
		ClientId:    server.NewId(),
		PublicKey:   testMeasurementExtenderPublicKey(index),
		TcpPort:     controller.ExtenderMeasurementPort,
		UdpPort:     443,
		DnsPort:     53,
		DnsTld:      connect.DefaultExtenderDnsTld,
		CountryCode: "de",
		IpVersion:   4,
		Ip:          netip.MustParseAddr(ip),
		Carriers:    []string{connect.ExtenderCarrierTcp},
	}, func(*model.NetworkExtender, []*model.NetworkExtenderAddress, time.Time) ([]byte, error) {
		hold()
		return []byte("record"), nil
	})
}

// A point inside a transaction where it holds, its locks taken, until the
// test lets it go on, so another transaction runs against what it locked.
// Only the first pass holds, since a transaction that is retried runs its
// callback again.
type testMeasurementRaceHold struct {
	held        chan struct{}
	release     chan struct{}
	holdOnce    sync.Once
	releaseOnce sync.Once
}

func newTestMeasurementRaceHold() *testMeasurementRaceHold {
	return &testMeasurementRaceHold{
		held:    make(chan struct{}),
		release: make(chan struct{}),
	}
}

// Called inside the transaction.
func (self *testMeasurementRaceHold) hold() {
	self.holdOnce.Do(func() {
		close(self.held)
		<-self.release
	})
}

// Waits until the transaction holds.
func (self *testMeasurementRaceHold) await(t testing.TB) {
	t.Helper()
	select {
	case <-self.held:
	case <-time.After(30 * time.Second):
		t.Fatalf("the transaction did not reach its hold")
	}
}

// Lets the transaction go on. A test defers it too, so a failure never leaves
// a transaction holding.
func (self *testMeasurementRaceHold) let() {
	self.releaseOnce.Do(func() {
		close(self.release)
	})
}

// Runs `call` on its own goroutine, counted in `group`, and returns the
// channel its result arrives on. A call that panics, as a database error
// raises, fails the test rather than the process, and the channel closes with
// no result.
func goTestMeasurementRace[T any](t testing.TB, group *sync.WaitGroup, call func() T) <-chan T {
	results := make(chan T, 1)
	group.Add(1)
	go func() {
		defer group.Done()
		returned := false
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("%v", r)
			}
			if !returned {
				close(results)
			}
		}()
		result := call()
		returned = true
		results <- result
	}()
	return results
}

// The result of a call goTestMeasurementRace runs, once it returns.
func receiveTestMeasurementRace[T any](t testing.TB, results <-chan T) T {
	t.Helper()
	result, ok := <-results
	if !ok {
		t.Fatalf("the call did not return")
	}
	return result
}

// Waits until a statement in the test's database waits for a lock, as one
// does that wants a row another transaction holds. It gives up well inside
// the lock timeout of the waiting transaction (server.PgLockTimeout), so the
// test lets the holder go on before the waiter fails.
func awaitTestMeasurementRaceLockWait(t testing.TB, ctx context.Context) {
	t.Helper()
	endTime := time.Now().Add(server.PgLockTimeout / 2)
	for {
		waiting := 0
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT COUNT(*)
				FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock'
				`,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&waiting))
				}
			})
		})
		if 0 < waiting {
			return
		}
		if endTime.Before(time.Now()) {
			t.Fatalf("nothing waited for a lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An extender deactivated for the whole away grace activates as a dns pass
// runs: the pass reads it inactive, and the activation has written the
// extender row and not yet committed when the pass withdraws. The withdrawal
// waits for the activation, judges the extender it committed, active, and
// withdraws nothing (amendment 24). The name stays published with the
// certificate the extender held, its request stays as it was, and nothing is
// revoked or ordered.
func TestExtenderMeasurementWithdrawalWaitsForAnActivation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		returning := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		requestTestMeasurementCert(t, ctx, returning, server.NowUtc())
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 1)
		connect.AssertEqual(t, runTestMeasurementDns(t, ctx).Names, 1)

		deactivateTestMeasurementExtender(t, ctx, returning, server.NowUtc().Add(-controller.ExtenderMeasurementAwayGrace-time.Minute))
		request := model.Testing_GetNetworkExtenderMeasurement(ctx, returning)

		var group sync.WaitGroup
		defer group.Wait()
		activationHold := newTestMeasurementRaceHold()
		defer activationHold.let()

		activated := goTestMeasurementRace(t, &group, func() *model.NetworkExtenderWithAddresses {
			return activateTestMeasurementRaceExtender(ctx, 1, "203.0.113.1", activationHold.hold)
		})
		activationHold.await(t)
		passed := goTestMeasurementRace(t, &group, func() *ExtenderMeasurementDnsResult {
			clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
			defer clientSession.Cancel()
			result, err := ExtenderMeasurementDns(&ExtenderMeasurementDnsArgs{}, clientSession)
			server.Raise(err)
			return result
		})
		awaitTestMeasurementRaceLockWait(t, ctx)
		activationHold.let()

		connect.AssertEqual(t, receiveTestMeasurementRace(t, activated).Extender.Active, true)
		dnsResult := receiveTestMeasurementRace(t, passed)
		connect.AssertEqual(t, dnsResult.Withdrawn, 0)
		connect.AssertEqual(t, dnsResult.Names, 1)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{
			testMeasurementName(returning) + " A": {"203.0.113.1"},
		})
		certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, returning)
		connect.AssertEqual(t, len(certs), 1)
		connect.AssertEqual(t, certs[0].WithdrawTime == nil, true)
		connect.AssertEqual(t, certs[0].PublishTime != nil, true)
		if kept := model.Testing_GetNetworkExtenderMeasurement(ctx, returning); !reflect.DeepEqual(kept, request) {
			t.Fatalf("the request changed to %+v from %+v", kept, request)
		}

		result := runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Revoked+result.Renewed+result.Issued+result.Refused+result.Failed, 0)
		connect.AssertEqual(t, env.ca.issuedCount(), 1)
		connect.AssertEqual(t, len(env.ca.revokedReasons()), 0)
		assertTestMeasurementBatches(t, env)
	})
}

// What must go still goes, each certificate with the reason its extender has
// when it is withdrawn: one deactivated for the whole away grace as not
// active, and one whose row is gone as gone, each request with it. Both leave
// the zone in the same pass and are revoked by the next issuance pass; an
// extender that stays keeps its own.
func TestExtenderMeasurementWithdrawalStillWithdrawsWhatMustGo(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		left := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		gone := createTestMeasurementExtender(t, ctx, 2, "203.0.113.2")
		staying := createTestMeasurementExtender(t, ctx, 3, "203.0.113.3")
		for _, extenderId := range []server.Id{left, gone, staying} {
			requestTestMeasurementCert(t, ctx, extenderId, server.NowUtc())
		}
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 3)
		connect.AssertEqual(t, runTestMeasurementDns(t, ctx).Names, 3)

		deactivateTestMeasurementExtender(t, ctx, left, server.NowUtc().Add(-controller.ExtenderMeasurementAwayGrace-time.Minute))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_extender WHERE extender_id = $1`, gone))
		})

		dnsResult := runTestMeasurementDns(t, ctx)
		connect.AssertEqual(t, dnsResult.Withdrawn, 2)
		connect.AssertEqual(t, dnsResult.Names, 1)
		assertTestMeasurementAddressRecords(t, env, map[string][]string{
			testMeasurementName(staying) + " A": {"203.0.113.3"},
		})
		for extenderId, reason := range map[server.Id]string{
			left: "the extender is not active",
			gone: "the extender is gone",
		} {
			certs := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
			connect.AssertEqual(t, len(certs), 1)
			connect.AssertEqual(t, certs[0].WithdrawTime != nil, true)
			connect.AssertEqual(t, certs[0].WithdrawReason, reason)
			measurement := model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
			connect.AssertEqual(t, measurement.State, model.NetworkExtenderMeasurementStateWithdrawn)
			connect.AssertEqual(t, measurement.Reason, reason)
		}
		stayingCerts := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, staying)
		connect.AssertEqual(t, len(stayingCerts), 1)
		connect.AssertEqual(t, stayingCerts[0].WithdrawTime == nil, true)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, staying).State, model.NetworkExtenderMeasurementStateIssued)

		result := runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Revoked, 2)
		connect.AssertEqual(t, result.Issued+result.Renewed, 0)
		connect.AssertEqual(t, len(env.ca.revokedReasons()), 2)
		assertTestMeasurementBatches(t, env)
	})
}
