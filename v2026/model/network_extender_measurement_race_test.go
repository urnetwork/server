package model

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The dns chain's withdrawal judged again where it writes (lag.dev/PLAN.md C9,
// amendment 24): WithdrawNetworkExtenderMeasurementCertIf locks the extender
// row and reads it in the withdrawing transaction, so an extender that
// activates after the sweep's read, right at the end of its away grace, keeps
// its certificate, and an activation and a withdrawal of one extender run one
// after the other.

// The away grace (controller.ExtenderMeasurementAwayGrace, amendment 23),
// which this package cannot import.
const testMeasurementRaceAwayGrace = 7 * 24 * time.Hour

// How the dns chain judges an extender at `now`
// (controller.ExtenderMeasurementWithdrawReason), for the extenders these tests
// make, all in the open tier and no canary: gone, or deactivated and no longer
// away.
func testMeasurementRaceReasonFor(now time.Time) func(extender *NetworkExtender) string {
	return func(extender *NetworkExtender) string {
		switch {
		case extender == nil:
			return "the extender is gone"
		case !extender.Active && (extender.RevokeTime == nil || !now.Before(extender.RevokeTime.Add(testMeasurementRaceAwayGrace))):
			return "the extender is not active"
		}
		return ""
	}
}

// One activated extender, `index` as testMeasurementActivate takes it, with
// its request and the live certificate that answers it, issued at `now`.
func createTestMeasurementRaceCert(
	t testing.TB,
	ctx context.Context,
	index int,
	now time.Time,
) (server.Id, *NetworkExtenderMeasurementCert) {
	t.Helper()
	extenderId := testMeasurementActivate(t, ctx, index).Extender.ExtenderId
	key := []byte(fmt.Sprintf("key-race-%d", index))
	SetNetworkExtenderMeasurementRequest(ctx, extenderId, append([]byte("csr-"), key...), key, testMeasurementNames(extenderId), now)
	cert := testMeasurementCert(extenderId, key, now)
	connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, cert), true)
	connect.AssertEqual(t, Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, NetworkExtenderMeasurementStateIssued)
	return extenderId, cert
}

// The extender's probes fail at `revokeTime`, which deactivates it then, as
// the uptime task does.
func deactivateTestMeasurementRaceExtender(t testing.TB, ctx context.Context, extenderId server.Id, revokeTime time.Time) {
	t.Helper()
	outcome := RecordNetworkExtenderProbeResult(ctx, extenderId, 4, false, revokeTime, 1, testRevocationSigner())
	connect.AssertEqual(t, outcome.ExtenderRevoked, true)
}

// The extender still holds its one certificate, live and queued for no
// revocation, and its request is exactly `request`.
func assertTestMeasurementRaceCertKept(
	t testing.TB,
	ctx context.Context,
	extenderId server.Id,
	request *NetworkExtenderMeasurement,
	now time.Time,
) {
	t.Helper()
	certs := Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
	connect.AssertEqual(t, len(certs), 1)
	connect.AssertEqual(t, certs[0].WithdrawTime == nil, true)
	connect.AssertEqual(t, certs[0].WithdrawReason, "")
	for _, cert := range GetNetworkExtenderMeasurementCertsToRevoke(ctx, now, 10) {
		if cert.ExtenderId == extenderId {
			t.Fatalf("the certificate %s is queued for revocation", cert.CertId)
		}
	}
	if kept := Testing_GetNetworkExtenderMeasurement(ctx, extenderId); !reflect.DeepEqual(kept, request) {
		t.Fatalf("the request changed to %+v from %+v", kept, request)
	}
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

// The sweep's read finds an extender deactivated for the whole away grace, and
// it activates before the withdrawal: judged as it is then, active, it keeps
// its certificate, and its request stays as it was (amendment 24).
func TestNetworkExtenderMeasurementWithdrawIfJudgesTheExtenderAsItIsNow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		reasonFor := testMeasurementRaceReasonFor(now)
		extenderId, cert := createTestMeasurementRaceCert(t, ctx, 1, now)
		deactivateTestMeasurementRaceExtender(t, ctx, extenderId, now.Add(-testMeasurementRaceAwayGrace-time.Minute))

		// the sweep's read
		live := GetLiveNetworkExtenderMeasurementCerts(ctx, now)
		connect.AssertEqual(t, len(live), 1)
		connect.AssertEqual(t, live[0].Cert.CertId, cert.CertId)
		connect.AssertEqual(t, reasonFor(live[0].Extender), "the extender is not active")
		request := Testing_GetNetworkExtenderMeasurement(ctx, extenderId)

		// back before the withdrawal
		testMeasurementActivate(t, ctx, 1)

		judged := []*NetworkExtender{}
		withdrawn := WithdrawNetworkExtenderMeasurementCertIf(ctx, cert.CertId, now, func(extender *NetworkExtender) string {
			judged = append(judged, extender)
			return reasonFor(extender)
		})
		connect.AssertEqual(t, withdrawn, 0)
		connect.AssertEqual(t, 0 < len(judged), true)
		extender := judged[len(judged)-1]
		connect.AssertEqual(t, extender.ExtenderId, extenderId)
		connect.AssertEqual(t, extender.Active, true)
		connect.AssertEqual(t, extender.RevokeTime == nil, true)
		assertTestMeasurementRaceCertKept(t, ctx, extenderId, request, now)
	})
}

// What must go still goes, with the reason its extender is judged by in the
// withdrawing transaction. Of two extenders the sweep's read finds deactivated
// for the whole grace, one is withdrawn as not active, and one whose row is
// gone since the read as gone, each request with it, and both are queued for
// revocation. One still away keeps its certificate, and a certificate already
// withdrawn is not withdrawn again.
func TestNetworkExtenderMeasurementWithdrawIfWithdrawsWithTheReasonItJudges(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		reasonFor := testMeasurementRaceReasonFor(now)
		left, leftCert := createTestMeasurementRaceCert(t, ctx, 1, now)
		gone, goneCert := createTestMeasurementRaceCert(t, ctx, 2, now)
		away, awayCert := createTestMeasurementRaceCert(t, ctx, 3, now)
		for _, extenderId := range []server.Id{left, gone} {
			deactivateTestMeasurementRaceExtender(t, ctx, extenderId, now.Add(-testMeasurementRaceAwayGrace-time.Minute))
		}
		deactivateTestMeasurementRaceExtender(t, ctx, away, now.Add(-testMeasurementRaceAwayGrace+time.Minute))

		// the sweep's read
		readReasons := map[server.Id]string{}
		for _, target := range GetLiveNetworkExtenderMeasurementCerts(ctx, now) {
			readReasons[target.Cert.ExtenderId] = reasonFor(target.Extender)
		}
		connect.AssertEqual(t, len(readReasons), 3)
		connect.AssertEqual(t, readReasons[left], "the extender is not active")
		connect.AssertEqual(t, readReasons[gone], "the extender is not active")
		connect.AssertEqual(t, readReasons[away], "")
		awayRequest := Testing_GetNetworkExtenderMeasurement(ctx, away)

		// gone before the withdrawal
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_extender WHERE extender_id = $1`, gone))
		})

		connect.AssertEqual(t, WithdrawNetworkExtenderMeasurementCertIf(ctx, leftCert.CertId, now, reasonFor), 1)
		connect.AssertEqual(t, WithdrawNetworkExtenderMeasurementCertIf(ctx, goneCert.CertId, now, reasonFor), 1)
		connect.AssertEqual(t, WithdrawNetworkExtenderMeasurementCertIf(ctx, awayCert.CertId, now, reasonFor), 0)
		connect.AssertEqual(t, WithdrawNetworkExtenderMeasurementCertIf(ctx, leftCert.CertId, now, reasonFor), 0)

		for extenderId, reason := range map[server.Id]string{
			left: "the extender is not active",
			gone: "the extender is gone",
		} {
			certs := Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
			connect.AssertEqual(t, len(certs), 1)
			connect.AssertEqual(t, certs[0].WithdrawTime.UTC(), now)
			connect.AssertEqual(t, certs[0].WithdrawReason, reason)
			measurement := Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
			connect.AssertEqual(t, measurement.State, NetworkExtenderMeasurementStateWithdrawn)
			connect.AssertEqual(t, measurement.Reason, reason)
		}
		toRevoke := []server.Id{}
		for _, cert := range GetNetworkExtenderMeasurementCertsToRevoke(ctx, now, 10) {
			toRevoke = append(toRevoke, cert.CertId)
		}
		connect.AssertEqual(t, len(toRevoke), 2)
		connect.AssertEqual(t, slices.Contains(toRevoke, leftCert.CertId), true)
		connect.AssertEqual(t, slices.Contains(toRevoke, goneCert.CertId), true)
		assertTestMeasurementRaceCertKept(t, ctx, away, awayRequest, now)
	})
}

// An activation is in flight when the withdrawal runs, its extender row
// written and not yet committed. The withdrawal waits for it, reads the
// extender it committed, active, and withdraws nothing.
func TestNetworkExtenderMeasurementWithdrawIfWaitsForAnActivation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		extenderId, cert := createTestMeasurementRaceCert(t, ctx, 1, now)
		deactivateTestMeasurementRaceExtender(t, ctx, extenderId, now.Add(-testMeasurementRaceAwayGrace-time.Minute))
		request := Testing_GetNetworkExtenderMeasurement(ctx, extenderId)

		var group sync.WaitGroup
		defer group.Wait()
		activationHold := newTestMeasurementRaceHold()
		defer activationHold.let()

		activated := goTestMeasurementRace(t, &group, func() *NetworkExtenderWithAddresses {
			return ActivateNetworkExtender(
				ctx,
				testExtenderActivation([]byte("extender-public-key-measurement-001"), 4, "203.0.113.1"),
				func(*NetworkExtender, []*NetworkExtenderAddress, time.Time) ([]byte, error) {
					// the signer runs in the activation's transaction, after
					// it wrote the extender row
					activationHold.hold()
					return []byte("record"), nil
				},
			)
		})
		activationHold.await(t)
		withdrawn := goTestMeasurementRace(t, &group, func() int {
			return WithdrawNetworkExtenderMeasurementCertIf(ctx, cert.CertId, now, testMeasurementRaceReasonFor(now))
		})
		awaitTestMeasurementRaceLockWait(t, ctx)
		activationHold.let()

		connect.AssertEqual(t, receiveTestMeasurementRace(t, activated).Extender.Active, true)
		connect.AssertEqual(t, receiveTestMeasurementRace(t, withdrawn), 0)
		assertTestMeasurementRaceCertKept(t, ctx, extenderId, request, now)
	})
}

// A withdrawal is in flight when the extender activates, the extender judged
// under its lock, deactivated for the whole grace, and not yet committed. The
// activation waits for it and comes after it: the certificate is withdrawn as
// not active, and the extender is active again, a return after the
// withdrawal, which asks again (C9).
func TestNetworkExtenderMeasurementActivationWaitsForWithdrawIf(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		reasonFor := testMeasurementRaceReasonFor(now)
		extenderId, cert := createTestMeasurementRaceCert(t, ctx, 1, now)
		deactivateTestMeasurementRaceExtender(t, ctx, extenderId, now.Add(-testMeasurementRaceAwayGrace-time.Minute))

		var group sync.WaitGroup
		defer group.Wait()
		withdrawalHold := newTestMeasurementRaceHold()
		defer withdrawalHold.let()

		withdrawn := goTestMeasurementRace(t, &group, func() int {
			return WithdrawNetworkExtenderMeasurementCertIf(ctx, cert.CertId, now, func(extender *NetworkExtender) string {
				// judged under the extender's lock
				withdrawalHold.hold()
				return reasonFor(extender)
			})
		})
		withdrawalHold.await(t)
		activated := goTestMeasurementRace(t, &group, func() *NetworkExtenderWithAddresses {
			sign, _ := testRecordSigner()
			return ActivateNetworkExtender(
				ctx,
				testExtenderActivation([]byte("extender-public-key-measurement-001"), 4, "203.0.113.1"),
				sign,
			)
		})
		awaitTestMeasurementRaceLockWait(t, ctx)
		withdrawalHold.let()

		connect.AssertEqual(t, receiveTestMeasurementRace(t, withdrawn), 1)
		connect.AssertEqual(t, receiveTestMeasurementRace(t, activated).Extender.Active, true)
		certs := Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
		connect.AssertEqual(t, len(certs), 1)
		connect.AssertEqual(t, certs[0].WithdrawTime.UTC(), now)
		connect.AssertEqual(t, certs[0].WithdrawReason, "the extender is not active")
		measurement := Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
		connect.AssertEqual(t, measurement.State, NetworkExtenderMeasurementStateWithdrawn)
		connect.AssertEqual(t, measurement.Reason, "the extender is not active")
		extender := getTestMeasurementExtender(t, ctx, extenderId)
		connect.AssertEqual(t, extender.Active, true)
		connect.AssertEqual(t, extender.RevokeTime == nil, true)
	})
}

// An extender holds two live certificates while a renewal overlaps the one it
// replaces, and both go on the one judgment (amendment 24): withdrawn one at
// a time, an activation between the two would leave the renewal serving with
// its request withdrawn. The renewal, read next, is withdrawn already.
func TestNetworkExtenderMeasurementWithdrawIfWithdrawsTheWholeExtender(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		reasonFor := testMeasurementRaceReasonFor(now)
		extenderId, cert := createTestMeasurementRaceCert(t, ctx, 1, now.Add(-time.Hour))
		renewal := testMeasurementCert(extenderId, []byte("key-race-1"), now)
		connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, renewal), true)
		deactivateTestMeasurementRaceExtender(t, ctx, extenderId, now.Add(-testMeasurementRaceAwayGrace-time.Minute))

		connect.AssertEqual(t, WithdrawNetworkExtenderMeasurementCertIf(ctx, cert.CertId, now, reasonFor), 2)
		certs := Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
		connect.AssertEqual(t, len(certs), 2)
		for _, withdrawnCert := range certs {
			connect.AssertEqual(t, withdrawnCert.WithdrawTime != nil, true)
			connect.AssertEqual(t, withdrawnCert.WithdrawReason, "the extender is not active")
		}
		connect.AssertEqual(t, Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, NetworkExtenderMeasurementStateWithdrawn)
		connect.AssertEqual(t, WithdrawNetworkExtenderMeasurementCertIf(ctx, renewal.CertId, now, reasonFor), 0)
	})
}
