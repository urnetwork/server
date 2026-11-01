package model

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A request withdrawn or refused because its extender was not active is asked
// again once the extender is active again and may be measured
// (lag.dev/PLAN.md C9, amendment 24), with the request it holds and by the
// rules of the same request carried again
// (SetNetworkExtenderMeasurementRequest), and nothing else is.

// The reason the controller gives an extender that is not active
// (controller.ExtenderMeasurementInactiveReason), which this package cannot
// import.
const testReopenInactiveReason = "the extender is not active"

// The text of a time column, "none" for NULL.
func testReopenTime(t *time.Time) string {
	if t == nil {
		return "none"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// Every column of a request, so a test can tell whether a pass changed it.
func testReopenRow(measurement *NetworkExtenderMeasurement) string {
	return fmt.Sprintf(
		"csr %x key %x names %v requested %s %s",
		measurement.Csr,
		measurement.PublicKey,
		measurement.Names,
		testReopenTime(&measurement.RequestTime),
		testReopenQueueRow(measurement),
	)
}

// The columns of a request that a request asked again holds as the same
// request carried again would: all but its CSR's bytes and its request time,
// which stay those of the activation that last carried it.
func testReopenQueueRow(measurement *NetworkExtenderMeasurement) string {
	return fmt.Sprintf(
		"%s %q pending %s retry %s failures %d updated %s",
		measurement.State,
		measurement.Reason,
		testReopenTime(measurement.PendingTime),
		testReopenTime(measurement.RetryTime),
		measurement.FailureCount,
		testReopenTime(&measurement.UpdateTime),
	)
}

// One active extender with a request for its name under its own key, and a
// certificate for that request issued at `issueTime` for ninety days.
func testReopenHolding(t testing.TB, ctx context.Context, index int, issueTime time.Time) (server.Id, *NetworkExtenderMeasurementCert) {
	t.Helper()
	extenderId := testMeasurementActivate(t, ctx, index).Extender.ExtenderId
	key := []byte(fmt.Sprintf("key-reopen-%d", index))
	SetNetworkExtenderMeasurementRequest(ctx, extenderId, append([]byte("csr-"), key...), key, testMeasurementNames(extenderId), issueTime)
	cert := testMeasurementCert(extenderId, key, issueTime)
	if !AddNetworkExtenderMeasurementCert(ctx, cert) {
		t.Fatalf("the certificate is not live")
	}
	return extenderId, cert
}

// The extender's probes fail at its one address at `probeTime`, which
// deactivates it then, as the uptime task does.
func testReopenDeactivate(t testing.TB, ctx context.Context, extenderId server.Id, probeTime time.Time) {
	t.Helper()
	RecordNetworkExtenderProbeResult(ctx, extenderId, 4, false, probeTime, 1, testRevocationSigner())
	if getTestMeasurementExtender(t, ctx, extenderId).Active {
		t.Fatalf("the extender was not deactivated")
	}
}

// One extender that held a certificate and left: deactivated eight days
// before `now`, past the away grace, so the dns chain withdrew its
// certificate as not active, and its request with it (C9, amendment 23).
func testReopenLeft(t testing.TB, ctx context.Context, index int, now time.Time) server.Id {
	t.Helper()
	extenderId, cert := testReopenHolding(t, ctx, index, now.Add(-30*24*time.Hour))
	testReopenDeactivate(t, ctx, extenderId, now.Add(-8*24*time.Hour))
	if !WithdrawNetworkExtenderMeasurementCert(ctx, cert.CertId, testReopenInactiveReason, now.Add(-time.Hour)) {
		t.Fatalf("the certificate was not withdrawn")
	}
	return extenderId
}

// A rule that lets every extender be measured, so a test judges the request
// rules alone.
func testReopenMayAskAll(*NetworkExtender, []*NetworkExtenderAddress) bool {
	return true
}

// A request withdrawn because its extender left, or refused for it (as an
// issuance pass refused one before amendment 23), is asked again once the
// extender is back, and left exactly as the extender's activation carrying the
// same request again leaves its twin: pending from then, with the backoff of
// its failed orders, or issued at once while a live certificate still answers
// it. Its CSR and request time stay those of the activation that last carried
// it. It is back in the queue, and is not asked again twice.
func TestReopenReturnedNetworkExtenderMeasurementsAsksAgainLikeTheSameRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)

		cases := []struct {
			name  string
			setup func(index int) server.Id
			state string
		}{
			{
				name: "withdrawn",
				setup: func(index int) server.Id {
					return testReopenLeft(t, ctx, index, now)
				},
				state: NetworkExtenderMeasurementStatePending,
			},
			{
				// it waited, an order failed, and the request was refused when
				// it left
				name: "refused",
				setup: func(index int) server.Id {
					extenderId := testMeasurementActivate(t, ctx, index).Extender.ExtenderId
					key := []byte(fmt.Sprintf("key-reopen-%d", index))
					csr := append([]byte("csr-"), key...)
					SetNetworkExtenderMeasurementRequest(ctx, extenderId, csr, key, testMeasurementNames(extenderId), now.Add(-3*time.Hour))
					connect.AssertEqual(t, DeferPendingNetworkExtenderMeasurement(ctx, extenderId, csr, "the order failed", now.Add(-2*time.Hour), now.Add(-150*time.Minute)), true)
					testReopenDeactivate(t, ctx, extenderId, now.Add(-2*time.Hour))
					connect.AssertEqual(t, RefusePendingNetworkExtenderMeasurement(ctx, extenderId, csr, testReopenInactiveReason, now.Add(-time.Hour)), true)
					return extenderId
				},
				state: NetworkExtenderMeasurementStatePending,
			},
			{
				// it came back while the dns chain was withdrawing its
				// certificates: the renewed one is withdrawn, and its renewal,
				// for the same name and key, still answers the request
				name: "withdrawn with a live renewal",
				setup: func(index int) server.Id {
					extenderId, renewed := testReopenHolding(t, ctx, index, now.Add(-70*24*time.Hour))
					renewal := testMeasurementCert(extenderId, renewed.PublicKey, now.Add(-10*24*time.Hour))
					renewal.CapCounted = false
					connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, renewal), true)
					testReopenDeactivate(t, ctx, extenderId, now.Add(-8*24*time.Hour))
					connect.AssertEqual(t, WithdrawNetworkExtenderMeasurementCert(ctx, renewed.CertId, testReopenInactiveReason, now.Add(-time.Hour)), true)
					return extenderId
				},
				state: NetworkExtenderMeasurementStateIssued,
			},
		}

		returnedIds := []server.Id{}
		twinIds := []server.Id{}
		befores := []*NetworkExtenderMeasurement{}
		for i, c := range cases {
			returnedId := c.setup(1 + 2*i)
			twinId := c.setup(2 + 2*i)
			before := Testing_GetNetworkExtenderMeasurement(ctx, returnedId)
			if before.Reason != testReopenInactiveReason || len(before.Csr) == 0 {
				t.Fatalf("%s: the request reads %q with csr %x", c.name, before.Reason, before.Csr)
			}
			// both are back, with activations that carry no request
			testMeasurementActivate(t, ctx, 1+2*i)
			testMeasurementActivate(t, ctx, 2+2*i)
			connect.AssertEqual(t, testReopenRow(Testing_GetNetworkExtenderMeasurement(ctx, returnedId)), testReopenRow(before))
			// the twin's activation carries the request it holds again
			held := Testing_GetNetworkExtenderMeasurement(ctx, twinId)
			connect.AssertEqual(t, SetNetworkExtenderMeasurementRequest(ctx, twinId, held.Csr, held.PublicKey, held.Names, now), c.state)
			returnedIds = append(returnedIds, returnedId)
			twinIds = append(twinIds, twinId)
			befores = append(befores, before)
		}

		connect.AssertEqual(t, ReopenReturnedNetworkExtenderMeasurements(ctx, testReopenInactiveReason, now, testReopenMayAskAll), len(cases))

		for i, c := range cases {
			reopened := Testing_GetNetworkExtenderMeasurement(ctx, returnedIds[i])
			connect.AssertEqual(t, reopened.State, c.state)
			if got, want := testReopenQueueRow(reopened), testReopenQueueRow(Testing_GetNetworkExtenderMeasurement(ctx, twinIds[i])); got != want {
				t.Fatalf("%s: asked again as %s, and carried again as %s", c.name, got, want)
			}
			// the request it holds, not a new one
			connect.AssertEqual(t, bytes.Equal(reopened.Csr, befores[i].Csr), true)
			connect.AssertEqual(t, bytes.Equal(reopened.PublicKey, befores[i].PublicKey), true)
			connect.AssertEqual(t, reopened.RequestTime.UTC(), befores[i].RequestTime.UTC())
		}
		withdrawn := Testing_GetNetworkExtenderMeasurement(ctx, returnedIds[0])
		connect.AssertEqual(t, withdrawn.Reason, "")
		connect.AssertEqual(t, withdrawn.PendingTime.UTC(), now)
		connect.AssertEqual(t, withdrawn.UpdateTime.UTC(), now)
		// the same name and key keep the backoff of their failed orders
		connect.AssertEqual(t, Testing_GetNetworkExtenderMeasurement(ctx, returnedIds[1]).FailureCount, 1)

		queued := []server.Id{}
		for _, target := range GetPendingNetworkExtenderMeasurements(ctx, now, 100) {
			queued = append(queued, target.Measurement.ExtenderId)
		}
		for _, returnedId := range returnedIds[:2] {
			if !slices.Contains(queued, returnedId) {
				t.Fatalf("%s asked again is not in the queue", returnedId)
			}
		}

		connect.AssertEqual(t, ReopenReturnedNetworkExtenderMeasurements(ctx, testReopenInactiveReason, now.Add(time.Minute), testReopenMayAskAll), 0)
	})
}

// Nothing else is asked again: a request whose extender is still away, or is
// gone; one an owner's opt-out dropped; one withdrawn for the gated tier or a
// canary; one refused for any other reason, by the issuance pass or as it
// arrived; one without its CSR, whatever its reason; and a pending one, which
// keeps its place in the queue whatever its reason says. Each is left exactly
// as it was.
func TestReopenReturnedNetworkExtenderMeasurementsLeavesEveryOtherRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)

		away := testReopenLeft(t, ctx, 1, now)

		// a request for an extender that is no longer in the directory
		gone := server.NewId()
		goneKey := []byte("key-reopen-gone")
		SetNetworkExtenderMeasurementRequest(ctx, gone, append([]byte("csr-"), goneKey...), goneKey, testMeasurementNames(gone), now.Add(-30*24*time.Hour))
		goneCert := testMeasurementCert(gone, goneKey, now.Add(-30*24*time.Hour))
		connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, goneCert), true)
		connect.AssertEqual(t, WithdrawNetworkExtenderMeasurementCert(ctx, goneCert.CertId, testReopenInactiveReason, now.Add(-time.Hour)), true)

		optedOut, _ := testReopenHolding(t, ctx, 2, now.Add(-30*24*time.Hour))
		connect.AssertEqual(t, OptOutNetworkExtenderMeasurement(ctx, getTestMeasurementExtender(t, ctx, optedOut).PublicKey, "the extender's owner opted out of measurement", now.Add(-time.Hour)), 1)

		gated, _ := testReopenHolding(t, ctx, 3, now.Add(-30*24*time.Hour))
		if err := SetNetworkExtenderDirectoryTier(ctx, gated, connect.ExtenderDirectoryTierGated, true); err != nil {
			t.Fatal(err)
		}
		canary, _ := testReopenHolding(t, ctx, 4, now.Add(-30*24*time.Hour))
		if err := SetNetworkExtenderCanaryChannel(ctx, canary, connect.ExtenderChannelDns, true); err != nil {
			t.Fatal(err)
		}

		refused := testMeasurementActivate(t, ctx, 5).Extender.ExtenderId
		SetNetworkExtenderMeasurementRequest(ctx, refused, []byte("csr-key-reopen-5"), []byte("key-reopen-5"), testMeasurementNames(refused), now.Add(-2*time.Hour))
		connect.AssertEqual(t, RefusePendingNetworkExtenderMeasurement(ctx, refused, []byte("csr-key-reopen-5"), "the extender has no public inbound address", now.Add(-time.Hour)), true)
		refusedOnArrival := testMeasurementActivate(t, ctx, 6).Extender.ExtenderId
		RefuseNetworkExtenderMeasurementRequest(ctx, refusedOnArrival, []byte("csr-key-reopen-6"), []byte("key-reopen-6"), []string{"other." + testMeasurementZone}, "the request names another extender", now.Add(-time.Hour))

		// back, with the reason that is asked again for, and no CSR to ask with
		withoutCsr := testReopenLeft(t, ctx, 7, now)
		testMeasurementActivate(t, ctx, 7)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_extender_measurement SET csr = NULL WHERE extender_id = $1`, withoutCsr))
		})

		waiting := testMeasurementActivate(t, ctx, 8).Extender.ExtenderId
		SetNetworkExtenderMeasurementRequest(ctx, waiting, []byte("csr-key-reopen-8"), []byte("key-reopen-8"), testMeasurementNames(waiting), now.Add(-2*time.Hour))
		NoteNetworkExtenderMeasurementWait(ctx, waiting, []byte("csr-key-reopen-8"), testReopenInactiveReason, now.Add(-time.Hour))

		untouched := []struct {
			name       string
			extenderId server.Id
			state      string
			reason     string
		}{
			{"away", away, NetworkExtenderMeasurementStateWithdrawn, testReopenInactiveReason},
			{"gone", gone, NetworkExtenderMeasurementStateWithdrawn, testReopenInactiveReason},
			{"opted out", optedOut, NetworkExtenderMeasurementStateWithdrawn, "the extender's owner opted out of measurement"},
			{"gated", gated, NetworkExtenderMeasurementStateWithdrawn, "the extender moved to the gated tier"},
			{"canary", canary, NetworkExtenderMeasurementStateWithdrawn, "the extender became a canary"},
			{"refused", refused, NetworkExtenderMeasurementStateRefused, "the extender has no public inbound address"},
			{"refused on arrival", refusedOnArrival, NetworkExtenderMeasurementStateRefused, "the request names another extender"},
			{"without its csr", withoutCsr, NetworkExtenderMeasurementStateWithdrawn, testReopenInactiveReason},
			{"waiting", waiting, NetworkExtenderMeasurementStatePending, testReopenInactiveReason},
		}
		befores := map[server.Id]string{}
		for _, u := range untouched {
			before := Testing_GetNetworkExtenderMeasurement(ctx, u.extenderId)
			if before.State != u.state || before.Reason != u.reason {
				t.Fatalf("%s: the request reads %s %q", u.name, before.State, before.Reason)
			}
			befores[u.extenderId] = testReopenRow(before)
		}
		// every one of them but the away one and the gone one is active
		for _, extenderId := range []server.Id{optedOut, gated, canary, refused, refusedOnArrival, withoutCsr, waiting} {
			connect.AssertEqual(t, getTestMeasurementExtender(t, ctx, extenderId).Active, true)
		}

		connect.AssertEqual(t, ReopenReturnedNetworkExtenderMeasurements(ctx, testReopenInactiveReason, now, testReopenMayAskAll), 0)
		for _, u := range untouched {
			if after := testReopenRow(Testing_GetNetworkExtenderMeasurement(ctx, u.extenderId)); after != befores[u.extenderId] {
				t.Fatalf("%s: changed from %s to %s", u.name, befores[u.extenderId], after)
			}
		}
	})
}

// Only an extender that may be measured now is asked for (C9, amendment 24).
// mayAsk judges each with its extender and active addresses as read, and again
// under their locks in the transaction that asks for it, and one it refuses at
// either is left exactly as it was: withdrawn, which its extender reads, so it
// drops what it holds. Asked for and then refused by the issuance pass, it
// would read only pending and refused.
func TestReopenReturnedNetworkExtenderMeasurementsAsksOnlyWhatMayBeMeasured(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)

		admitted := testReopenLeft(t, ctx, 1, now)
		refused := testReopenLeft(t, ctx, 2, now)
		changed := testReopenLeft(t, ctx, 3, now)
		extenderIds := []server.Id{admitted, refused, changed}
		befores := map[server.Id]string{}
		for i, extenderId := range extenderIds {
			// back, with an activation that carries no request
			testMeasurementActivate(t, ctx, 1+i)
			befores[extenderId] = testReopenRow(Testing_GetNetworkExtenderMeasurement(ctx, extenderId))
		}

		// a rule that lets none be measured asks for none, and is given each
		// extender as it is, active with its one address
		judged := map[server.Id]int{}
		connect.AssertEqual(t, ReopenReturnedNetworkExtenderMeasurements(
			ctx,
			testReopenInactiveReason,
			now,
			func(extender *NetworkExtender, addresses []*NetworkExtenderAddress) bool {
				if extender == nil || !extender.Active || len(addresses) != 1 || !addresses[0].Active {
					t.Errorf("judged %+v with %d active addresses", extender, len(addresses))
					return false
				}
				judged[extender.ExtenderId] += 1
				return false
			},
		), 0)
		for _, extenderId := range extenderIds {
			connect.AssertEqual(t, judged[extenderId], 1)
			connect.AssertEqual(t, testReopenRow(Testing_GetNetworkExtenderMeasurement(ctx, extenderId)), befores[extenderId])
		}

		// one may be measured, one may not, and one may be as read but no
		// longer when it is asked for, as when it activates in between with
		// its tcp port moved
		judged = map[server.Id]int{}
		mayAsk := func(extender *NetworkExtender, addresses []*NetworkExtenderAddress) bool {
			if extender == nil {
				return false
			}
			judged[extender.ExtenderId] += 1
			switch extender.ExtenderId {
			case admitted:
				return true
			case changed:
				return judged[changed] == 1
			default:
				return false
			}
		}
		connect.AssertEqual(t, ReopenReturnedNetworkExtenderMeasurements(ctx, testReopenInactiveReason, now, mayAsk), 1)
		connect.AssertEqual(t, Testing_GetNetworkExtenderMeasurement(ctx, admitted).State, NetworkExtenderMeasurementStatePending)
		for _, extenderId := range []server.Id{refused, changed} {
			connect.AssertEqual(t, testReopenRow(Testing_GetNetworkExtenderMeasurement(ctx, extenderId)), befores[extenderId])
		}
		// one refused as read is not judged again; the others are, before
		// they are asked for
		connect.AssertEqual(t, judged[refused], 1)
		for _, extenderId := range []server.Id{admitted, changed} {
			if judged[extenderId] < 2 {
				t.Fatalf("%s was judged %d times", extenderId, judged[extenderId])
			}
		}
	})
}
