package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The measurement certificate records (lag.dev/PLAN.md C5-C10, A8-A10, T2):
// the request states, the flag that is set before an order, the refusal of
// the gated tier and canaries for an extender that ever held a certificate
// (A9), withdrawal, and the expiry every record signer reads (C10).

const testMeasurementZone = "extender.ur.example"

// The extender's one measurement name (N1), as the list these tables keep.
func testMeasurementNames(extenderId server.Id) []string {
	return []string{connect.ExtenderMeasurementName(connect.Id(extenderId), testMeasurementZone)}
}

// One activated open tier extender with a v4 address.
func testMeasurementActivate(t testing.TB, ctx context.Context, index int) *NetworkExtenderWithAddresses {
	t.Helper()
	sign, _ := testRecordSigner()
	activated := ActivateNetworkExtender(
		ctx,
		testExtenderActivation([]byte(fmt.Sprintf("extender-public-key-measurement-%03d", index)), 4, fmt.Sprintf("203.0.113.%d", index)),
		sign,
	)
	if activated == nil {
		t.Fatalf("the activation was not stored")
	}
	return activated
}

// A certificate row for the extender's name and the given key, issued at
// `now` for ninety days.
func testMeasurementCert(extenderId server.Id, publicKey []byte, now time.Time) *NetworkExtenderMeasurementCert {
	return &NetworkExtenderMeasurementCert{
		CertId:     server.NewId(),
		ExtenderId: extenderId,
		Names:      testMeasurementNames(extenderId),
		Csr:        append([]byte("csr-"), publicKey...),
		PublicKey:  publicKey,
		ChainPem:   "chain",
		SerialHex:  "1",
		NotBefore:  now,
		NotAfter:   now.Add(90 * 24 * time.Hour),
		IssueTime:  now,
		CapCounted: true,
		RenewTime:  now.Add(60 * 24 * time.Hour),
	}
}

// A request is pending until a certificate answers it, for the same names and
// the same key. A request is known by its names and key, so one for what is
// already held -- pending or issued -- changes nothing, whatever its bytes
// (A8). A new key is a new request.
func TestNetworkExtenderMeasurementRequestStates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		extenderId := testMeasurementActivate(t, ctx, 1).Extender.ExtenderId
		names := testMeasurementNames(extenderId)
		now := server.NowUtc().Truncate(time.Second)

		state := SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-a"), []byte("key-a"), names, now)
		connect.AssertEqual(t, state, NetworkExtenderMeasurementStatePending)
		measurement := Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
		connect.AssertEqual(t, measurement.State, NetworkExtenderMeasurementStatePending)
		if !slices.Equal(measurement.Names, names) {
			t.Fatalf("names = %v", measurement.Names)
		}
		connect.AssertEqual(t, measurement.PendingTime.UTC(), now)

		// the same request, signed anew, while it waits: nothing changes
		state = SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-a-again"), []byte("key-a"), names, now.Add(time.Minute))
		connect.AssertEqual(t, state, NetworkExtenderMeasurementStatePending)
		measurement = Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
		connect.AssertEqual(t, bytes.Equal(measurement.Csr, []byte("csr-a")), true)
		connect.AssertEqual(t, measurement.PendingTime.UTC(), now)
		connect.AssertEqual(t, measurement.RequestTime.UTC(), now)

		AddNetworkExtenderMeasurementCert(ctx, testMeasurementCert(extenderId, []byte("key-a"), now))
		connect.AssertEqual(t, Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, NetworkExtenderMeasurementStateIssued)

		// and again once it is issued: no new request, so no new order
		state = SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-a-again"), []byte("key-a"), names, now.Add(time.Hour))
		connect.AssertEqual(t, state, NetworkExtenderMeasurementStateIssued)
		measurement = Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
		connect.AssertEqual(t, measurement.State, NetworkExtenderMeasurementStateIssued)
		connect.AssertEqual(t, bytes.Equal(measurement.Csr, []byte("csr-a")), true)
		connect.AssertEqual(t, len(GetPendingNetworkExtenderMeasurements(ctx, now.Add(2*time.Hour), 10)), 0)

		// a rotated key needs a certificate of its own
		state = SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-b"), []byte("key-b"), names, now.Add(2*time.Hour))
		connect.AssertEqual(t, state, NetworkExtenderMeasurementStatePending)
		// while the first key's certificate is still what the extender may be
		// answered with if it goes back to that key
		state = SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-a"), []byte("key-a"), names, now.Add(3*time.Hour))
		connect.AssertEqual(t, state, NetworkExtenderMeasurementStateIssued)

		// a request refused as it arrives keeps what could be read of it, so
		// A8 names its key
		otherNames := []string{"other." + testMeasurementZone}
		RefuseNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-c"), []byte("key-c"), otherNames, "the request names another extender", now.Add(4*time.Hour))
		measurement = Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
		connect.AssertEqual(t, measurement.State, NetworkExtenderMeasurementStateRefused)
		connect.AssertEqual(t, measurement.Reason, "the request names another extender")
		connect.AssertEqual(t, bytes.Equal(measurement.Csr, []byte("csr-c")), true)
		connect.AssertEqual(t, bytes.Equal(measurement.PublicKey, []byte("key-c")), true)
		if !slices.Equal(measurement.Names, otherNames) {
			t.Fatalf("names = %v", measurement.Names)
		}

		// one that could not be read at all has nothing to keep
		RefuseNetworkExtenderMeasurementRequest(ctx, extenderId, nil, nil, nil, "the request is not readable", now.Add(5*time.Hour))
		measurement = Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
		connect.AssertEqual(t, measurement.State, NetworkExtenderMeasurementStateRefused)
		connect.AssertEqual(t, measurement.Reason, "the request is not readable")
		connect.AssertEqual(t, len(measurement.Csr), 0)
		connect.AssertEqual(t, len(measurement.PublicKey), 0)
		connect.AssertEqual(t, len(measurement.Names), 0)
	})
}

// The queue reads first the requests the weekly cap does not hold (C7):
// those for names with an unexpired certificate, which the CA renews outside
// its budget, as a rotated key's is. While the cap is spent, an issuance pass
// leaves every capped request pending, so a read that took the longest
// waiting first would find the same capped requests on every pass, and a
// backlog of them as long as the read would keep a request the cap does not
// hold from ever being read. Within each part, the longest waiting is first.
func TestNetworkExtenderMeasurementPendingReadsUncappedFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)

		// a backlog of new extenders, all longer waiting than the rotated key
		const limit = 4
		backlog := []server.Id{}
		for i := range limit + 2 {
			extenderId := testMeasurementActivate(t, ctx, 10+i).Extender.ExtenderId
			key := []byte(fmt.Sprintf("key-backlog-%d", i))
			SetNetworkExtenderMeasurementRequest(ctx, extenderId, append([]byte("csr-"), key...), key, testMeasurementNames(extenderId), now.Add(time.Duration(i-20)*time.Minute))
			backlog = append(backlog, extenderId)
		}
		// an extender with a live certificate rotates its key: its name has an
		// unexpired certificate
		rotating := testMeasurementActivate(t, ctx, 1).Extender.ExtenderId
		SetNetworkExtenderMeasurementRequest(ctx, rotating, []byte("csr-key-a"), []byte("key-a"), testMeasurementNames(rotating), now.Add(-time.Hour))
		connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, testMeasurementCert(rotating, []byte("key-a"), now.Add(-time.Hour))), true)
		connect.AssertEqual(t, SetNetworkExtenderMeasurementRequest(ctx, rotating, []byte("csr-key-b"), []byte("key-b"), testMeasurementNames(rotating), now), NetworkExtenderMeasurementStatePending)

		pending := GetPendingNetworkExtenderMeasurements(ctx, now, limit)
		connect.AssertEqual(t, len(pending), limit)
		connect.AssertEqual(t, pending[0].Measurement.ExtenderId, rotating)
		for i, target := range pending[1:] {
			connect.AssertEqual(t, target.Measurement.ExtenderId, backlog[i])
		}
	})
}

// A request keeps its place in the queue while its extender keeps waiting
// (C7), and the backoff of its failed orders, so asking again cannot hurry
// the CA. Another key starts afresh but keeps the place.
func TestNetworkExtenderMeasurementPendingKeepsItsPlaceAndBackoff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		first := testMeasurementActivate(t, ctx, 1).Extender.ExtenderId
		second := testMeasurementActivate(t, ctx, 2).Extender.ExtenderId
		now := server.NowUtc().Truncate(time.Second)

		SetNetworkExtenderMeasurementRequest(ctx, first, []byte("csr-1"), []byte("key-1"), testMeasurementNames(first), now)
		SetNetworkExtenderMeasurementRequest(ctx, second, []byte("csr-2"), []byte("key-2"), testMeasurementNames(second), now.Add(time.Minute))

		pending := GetPendingNetworkExtenderMeasurements(ctx, now.Add(3*time.Minute), 10)
		connect.AssertEqual(t, len(pending), 2)
		connect.AssertEqual(t, pending[0].Measurement.ExtenderId, first)
		connect.AssertEqual(t, pending[1].Measurement.ExtenderId, second)
		connect.AssertEqual(t, pending[0].Extender.ExtenderId, first)
		connect.AssertEqual(t, len(pending[0].Addresses), 1)

		retryTime := now.Add(time.Hour)
		connect.AssertEqual(t, DeferPendingNetworkExtenderMeasurement(ctx, first, []byte("csr-1"), "the order failed", retryTime, now), true)
		// a request that is not the one stored is not the one deferred
		connect.AssertEqual(t, DeferPendingNetworkExtenderMeasurement(ctx, first, []byte("csr-stale"), "the order failed", retryTime, now), false)

		pending = GetPendingNetworkExtenderMeasurements(ctx, now.Add(3*time.Minute), 10)
		connect.AssertEqual(t, len(pending), 1)
		connect.AssertEqual(t, pending[0].Measurement.ExtenderId, second)

		// the same request again keeps the backoff
		SetNetworkExtenderMeasurementRequest(ctx, first, []byte("csr-1-again"), []byte("key-1"), testMeasurementNames(first), now.Add(4*time.Minute))
		measurement := Testing_GetNetworkExtenderMeasurement(ctx, first)
		connect.AssertEqual(t, measurement.FailureCount, 1)
		connect.AssertEqual(t, measurement.RetryTime.UTC(), retryTime)

		// another key does not, and still waits from when the extender began
		SetNetworkExtenderMeasurementRequest(ctx, first, []byte("csr-1b"), []byte("key-1b"), testMeasurementNames(first), now.Add(5*time.Minute))
		measurement = Testing_GetNetworkExtenderMeasurement(ctx, first)
		connect.AssertEqual(t, measurement.FailureCount, 0)
		connect.AssertEqual(t, measurement.RetryTime == nil, true)
		connect.AssertEqual(t, measurement.PendingTime.UTC(), now)
		pending = GetPendingNetworkExtenderMeasurements(ctx, now.Add(6*time.Minute), 10)
		connect.AssertEqual(t, len(pending), 2)
		connect.AssertEqual(t, pending[0].Measurement.ExtenderId, first)

		// a refusal is of the request that was read, and only once
		connect.AssertEqual(t, RefusePendingNetworkExtenderMeasurement(ctx, first, []byte("csr-1"), "stale", now), false)
		connect.AssertEqual(t, RefusePendingNetworkExtenderMeasurement(ctx, first, []byte("csr-1b"), "the extender is not active", now), true)
		connect.AssertEqual(t, RefusePendingNetworkExtenderMeasurement(ctx, first, []byte("csr-1b"), "the extender is not active", now), false)
		// refused holds nothing: the same request again waits anew
		state := SetNetworkExtenderMeasurementRequest(ctx, first, []byte("csr-1b"), []byte("key-1b"), testMeasurementNames(first), now.Add(7*time.Minute))
		connect.AssertEqual(t, state, NetworkExtenderMeasurementStatePending)
		connect.AssertEqual(t, Testing_GetNetworkExtenderMeasurement(ctx, first).PendingTime.UTC(), now.Add(7*time.Minute))
	})
}

// The flag is set before an order, and only on an extender that may be
// measured at that instant (C5): never on a gated one, a canary, or an
// inactive one.
func TestFlagNetworkExtenderMeasurementEverIssuedOnlyInTheOpenTier(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		open := testMeasurementActivate(t, ctx, 1).Extender.ExtenderId
		gated := testMeasurementActivate(t, ctx, 2).Extender.ExtenderId
		canary := testMeasurementActivate(t, ctx, 3).Extender.ExtenderId
		inactive := testMeasurementActivate(t, ctx, 4).Extender.ExtenderId

		if err := SetNetworkExtenderDirectoryTier(ctx, gated, connect.ExtenderDirectoryTierGated, false); err != nil {
			t.Fatal(err)
		}
		if err := SetNetworkExtenderCanaryChannel(ctx, canary, connect.ExtenderChannelDns, false); err != nil {
			t.Fatal(err)
		}
		RecordNetworkExtenderProbeResult(ctx, inactive, 4, false, server.NowUtc(), 1, testRevocationSigner())

		connect.AssertEqual(t, FlagNetworkExtenderMeasurementEverIssued(ctx, open), true)
		connect.AssertEqual(t, FlagNetworkExtenderMeasurementEverIssued(ctx, gated), false)
		connect.AssertEqual(t, FlagNetworkExtenderMeasurementEverIssued(ctx, canary), false)
		connect.AssertEqual(t, FlagNetworkExtenderMeasurementEverIssued(ctx, inactive), false)

		everIssued, _ := Testing_GetNetworkExtenderMeasurementExposure(ctx, open)
		connect.AssertEqual(t, everIssued, true)
		for _, extenderId := range []server.Id{gated, canary, inactive} {
			everIssued, _ := Testing_GetNetworkExtenderMeasurementExposure(ctx, extenderId)
			connect.AssertEqual(t, everIssued, false)
		}
	})
}

// A9: once an extender held a certificate, the gated tier and every canary
// channel are refused, in the model and for any writer at all, and the
// override moves it and withdraws its certificates (C9).
func TestNetworkExtenderMeasurementRefusesGatedAndCanaryForAHolder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		extenderId := testMeasurementActivate(t, ctx, 1).Extender.ExtenderId
		names := testMeasurementNames(extenderId)
		now := server.NowUtc()
		SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-a"), []byte("key-a"), names, now)
		connect.AssertEqual(t, FlagNetworkExtenderMeasurementEverIssued(ctx, extenderId), true)
		cert := testMeasurementCert(extenderId, []byte("key-a"), now)
		AddNetworkExtenderMeasurementCert(ctx, cert)

		tierOf := func() (int, string) {
			stored := getTestMeasurementExtender(t, ctx, extenderId)
			return stored.DirectoryTier, stored.CanaryChannel
		}

		err := SetNetworkExtenderDirectoryTier(ctx, extenderId, connect.ExtenderDirectoryTierGated, false)
		if !errors.Is(err, ErrNetworkExtenderMeasurementExposure) {
			t.Fatalf("the gated tier was not refused: %v", err)
		}
		for _, channel := range []string{connect.ExtenderChannelDns, connect.ExtenderChannelGated} {
			err = SetNetworkExtenderCanaryChannel(ctx, extenderId, channel, false)
			if !errors.Is(err, ErrNetworkExtenderMeasurementExposure) {
				t.Fatalf("the %s canary was not refused: %v", channel, err)
			}
		}
		tier, channel := tierOf()
		connect.AssertEqual(t, tier, connect.ExtenderDirectoryTierOpen)
		connect.AssertEqual(t, channel, "")
		// moving within the open tier, or clearing a canary, is no exposure
		if err := SetNetworkExtenderDirectoryTier(ctx, extenderId, connect.ExtenderDirectoryTierOpen, false); err != nil {
			t.Fatal(err)
		}
		if err := SetNetworkExtenderCanaryChannel(ctx, extenderId, "", false); err != nil {
			t.Fatal(err)
		}

		// a writer that knows nothing of measurement is refused by the table
		for _, statement := range []string{
			`UPDATE network_extender SET directory_tier = 1 WHERE extender_id = $1`,
			`UPDATE network_extender SET canary_channel = 'dns' WHERE extender_id = $1`,
		} {
			if r := server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, statement, extenderId))
				})
			}); r == nil {
				t.Fatalf("%s was not refused", statement)
			}
		}
		tier, channel = tierOf()
		connect.AssertEqual(t, tier, connect.ExtenderDirectoryTierOpen)
		connect.AssertEqual(t, channel, "")
		if live := Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId); live[0].WithdrawTime != nil {
			t.Fatalf("a refused move withdrew the certificate")
		}
		connect.AssertEqual(t, Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, NetworkExtenderMeasurementStateIssued)

		// the override accepts the exposure, moves the extender, and
		// withdraws its certificate at once
		if err := SetNetworkExtenderDirectoryTier(ctx, extenderId, connect.ExtenderDirectoryTierGated, true); err != nil {
			t.Fatal(err)
		}
		tier, _ = tierOf()
		connect.AssertEqual(t, tier, connect.ExtenderDirectoryTierGated)
		everIssued, accepted := Testing_GetNetworkExtenderMeasurementExposure(ctx, extenderId)
		connect.AssertEqual(t, everIssued, true)
		connect.AssertEqual(t, accepted, true)
		certs := Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
		connect.AssertEqual(t, len(certs), 1)
		if certs[0].WithdrawTime == nil {
			t.Fatalf("the override did not withdraw the certificate")
		}
		connect.AssertEqual(t, certs[0].WithdrawReason, "the extender moved to the gated tier")
		measurement := Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
		connect.AssertEqual(t, measurement.State, NetworkExtenderMeasurementStateWithdrawn)
		connect.AssertEqual(t, measurement.Reason, "the extender moved to the gated tier")
		if !slices.Equal(measurement.Names, names) {
			t.Fatalf("the withdrawn request names %v", measurement.Names)
		}
		toRevoke := GetNetworkExtenderMeasurementCertsToRevoke(ctx, server.NowUtc(), 10)
		connect.AssertEqual(t, len(toRevoke), 1)
		connect.AssertEqual(t, toRevoke[0].CertId, cert.CertId)

		// every exposing change asks for the override again, an earlier
		// acceptance notwithstanding
		err = SetNetworkExtenderCanaryChannel(ctx, extenderId, connect.ExtenderChannelGated, false)
		if !errors.Is(err, ErrNetworkExtenderMeasurementExposure) {
			t.Fatalf("a canary without the override was not refused: %v", err)
		}
		if err := SetNetworkExtenderCanaryChannel(ctx, extenderId, connect.ExtenderChannelGated, true); err != nil {
			t.Fatal(err)
		}
		_, channel = tierOf()
		connect.AssertEqual(t, channel, connect.ExtenderChannelGated)
	})
}

// A9 at activation: an activation by a durable network asks for the gated
// tier, and an extender that ever held a certificate stays in the open tier
// instead, its record signed as open. Once the exposure is accepted the
// activation's tier applies again.
func TestActivateNetworkExtenderKeepsAHolderInTheOpenTier(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey := []byte("extender-public-key-measurement-001")
		extenderId := testMeasurementActivate(t, ctx, 1).Extender.ExtenderId
		connect.AssertEqual(t, FlagNetworkExtenderMeasurementEverIssued(ctx, extenderId), true)

		signedTiers := []int{}
		sign := func(extender *NetworkExtender, addresses []*NetworkExtenderAddress, issueTime time.Time) ([]byte, error) {
			signedTiers = append(signedTiers, extender.DirectoryTier)
			return []byte("record"), nil
		}
		activation := testExtenderActivation(publicKey, 4, "203.0.113.1")
		activation.DirectoryTier = connect.ExtenderDirectoryTierGated
		activated := ActivateNetworkExtender(ctx, activation, sign)
		connect.AssertEqual(t, activated.Extender.ExtenderId, extenderId)
		connect.AssertEqual(t, activated.Extender.DirectoryTier, connect.ExtenderDirectoryTierOpen)
		connect.AssertEqual(t, signedTiers[len(signedTiers)-1], connect.ExtenderDirectoryTierOpen)
		connect.AssertEqual(t, getTestMeasurementExtender(t, ctx, extenderId).DirectoryTier, connect.ExtenderDirectoryTierOpen)

		if err := SetNetworkExtenderDirectoryTier(ctx, extenderId, connect.ExtenderDirectoryTierGated, true); err != nil {
			t.Fatal(err)
		}
		activated = ActivateNetworkExtender(ctx, activation, sign)
		connect.AssertEqual(t, activated.Extender.DirectoryTier, connect.ExtenderDirectoryTierGated)

		// an extender that never held one follows the activation's tier
		otherActivation := testExtenderActivation([]byte("extender-public-key-measurement-002"), 4, "203.0.113.2")
		otherActivation.DirectoryTier = connect.ExtenderDirectoryTierGated
		connect.AssertEqual(t, ActivateNetworkExtender(ctx, otherActivation, sign).Extender.DirectoryTier, connect.ExtenderDirectoryTierGated)
	})
}

// A withdrawal (C9) stops a certificate counting as live, leaves the request
// for its names withdrawn until the extender asks again, and queues the
// certificate for revocation until the CA revokes it; an expired certificate
// needs no revocation. A request the extender made for other names since is
// another request, and stands.
func TestNetworkExtenderMeasurementWithdrawalQueuesARevocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		extenderId := testMeasurementActivate(t, ctx, 1).Extender.ExtenderId
		names := testMeasurementNames(extenderId)
		now := server.NowUtc()
		SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-a"), []byte("key-a"), names, now)
		expired := testMeasurementCert(extenderId, []byte("key-a"), now.Add(-100*24*time.Hour))
		AddNetworkExtenderMeasurementCert(ctx, expired)
		cert := testMeasurementCert(extenderId, []byte("key-a"), now)
		AddNetworkExtenderMeasurementCert(ctx, cert)

		live := GetLiveNetworkExtenderMeasurementCerts(ctx, now)
		connect.AssertEqual(t, len(live), 1)
		connect.AssertEqual(t, live[0].Cert.CertId, cert.CertId)
		connect.AssertEqual(t, live[0].Extender.ExtenderId, extenderId)
		connect.AssertEqual(t, len(live[0].Addresses), 1)

		connect.AssertEqual(t, WithdrawNetworkExtenderMeasurementCert(ctx, cert.CertId, "the extender is not active", now), true)
		connect.AssertEqual(t, WithdrawNetworkExtenderMeasurementCert(ctx, cert.CertId, "the extender is not active", now), false)
		WithdrawNetworkExtenderMeasurementCert(ctx, expired.CertId, "the extender is not active", now)
		connect.AssertEqual(t, len(GetLiveNetworkExtenderMeasurementCerts(ctx, now)), 0)
		measurement := Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
		connect.AssertEqual(t, measurement.State, NetworkExtenderMeasurementStateWithdrawn)
		connect.AssertEqual(t, measurement.Reason, "the extender is not active")
		if !slices.Equal(measurement.Names, names) {
			t.Fatalf("the withdrawn request names %v", measurement.Names)
		}

		toRevoke := GetNetworkExtenderMeasurementCertsToRevoke(ctx, now, 10)
		connect.AssertEqual(t, len(toRevoke), 1)
		connect.AssertEqual(t, toRevoke[0].CertId, cert.CertId)
		connect.AssertEqual(t, toRevoke[0].RevokeFailureCount, 0)

		DeferNetworkExtenderMeasurementCertRevocation(ctx, cert.CertId, now.Add(time.Hour))
		connect.AssertEqual(t, len(GetNetworkExtenderMeasurementCertsToRevoke(ctx, now, 10)), 0)
		toRevoke = GetNetworkExtenderMeasurementCertsToRevoke(ctx, now.Add(2*time.Hour), 10)
		connect.AssertEqual(t, len(toRevoke), 1)
		connect.AssertEqual(t, toRevoke[0].RevokeFailureCount, 1)
		MarkNetworkExtenderMeasurementCertRevoked(ctx, cert.CertId, now)
		connect.AssertEqual(t, len(GetNetworkExtenderMeasurementCertsToRevoke(ctx, now.Add(2*time.Hour), 10)), 0)

		// the revoked certificate is still an unexpired certificate for the
		// name, so a new one for it is a renewal to the CA (C7)
		connect.AssertEqual(t, HasUnexpiredNetworkExtenderMeasurementCertForNames(ctx, names, now), true)
		connect.AssertEqual(t, HasUnexpiredNetworkExtenderMeasurementCertForNames(ctx, names, now.Add(91*24*time.Hour)), false)

		// withdrawn holds nothing: the extender asks again and waits again
		state := SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-a"), []byte("key-a"), names, now.Add(time.Minute))
		connect.AssertEqual(t, state, NetworkExtenderMeasurementStatePending)

		// a request for other names is not the withdrawn certificate's
		other := testMeasurementActivate(t, ctx, 2).Extender.ExtenderId
		otherCert := testMeasurementCert(other, []byte("key-o"), now)
		AddNetworkExtenderMeasurementCert(ctx, otherCert)
		SetNetworkExtenderMeasurementRequest(ctx, other, []byte("csr-o"), []byte("key-o"), []string{"another.name.example"}, now)
		WithdrawNetworkExtenderMeasurementCert(ctx, otherCert.CertId, "the extender is not active", now)
		connect.AssertEqual(t, Testing_GetNetworkExtenderMeasurement(ctx, other).State, NetworkExtenderMeasurementStatePending)
	})
}

// A new certificate replaces the extender's live ones (C6): the old one is
// renewed, still live until it expires but no longer due for renewal. Only
// the certificates that counted spend the weekly cap (C7).
func TestAddNetworkExtenderMeasurementCertRenewsTheOldOne(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		extenderId := testMeasurementActivate(t, ctx, 1).Extender.ExtenderId
		names := testMeasurementNames(extenderId)
		now := server.NowUtc()
		connect.AssertEqual(t, HasUnexpiredNetworkExtenderMeasurementCertForNames(ctx, names, now), false)

		old := testMeasurementCert(extenderId, []byte("key-a"), now.Add(-61*24*time.Hour))
		old.RenewTime = now.Add(-time.Hour)
		AddNetworkExtenderMeasurementCert(ctx, old)
		connect.AssertEqual(t, HasUnexpiredNetworkExtenderMeasurementCertForNames(ctx, names, now), true)

		due := GetNetworkExtenderMeasurementCertsForRenewal(ctx, now, 10)
		connect.AssertEqual(t, len(due), 1)
		connect.AssertEqual(t, due[0].Cert.CertId, old.CertId)
		connect.AssertEqual(t, due[0].Extender.ExtenderId, extenderId)

		// a failed renewal waits and counts
		DeferNetworkExtenderMeasurementCertRenewal(ctx, old.CertId, now.Add(time.Hour))
		connect.AssertEqual(t, len(GetNetworkExtenderMeasurementCertsForRenewal(ctx, now, 10)), 0)
		due = GetNetworkExtenderMeasurementCertsForRenewal(ctx, now.Add(2*time.Hour), 10)
		connect.AssertEqual(t, len(due), 1)
		connect.AssertEqual(t, due[0].Cert.RenewFailureCount, 1)

		renewal := testMeasurementCert(extenderId, []byte("key-a"), now)
		renewal.CapCounted = false
		AddNetworkExtenderMeasurementCert(ctx, renewal)
		connect.AssertEqual(t, len(GetNetworkExtenderMeasurementCertsForRenewal(ctx, now.Add(2*time.Hour), 10)), 0)
		connect.AssertEqual(t, len(GetLiveNetworkExtenderMeasurementCerts(ctx, now)), 2)
		certs := Testing_GetNetworkExtenderMeasurementCerts(ctx, extenderId)
		connect.AssertEqual(t, certs[0].RenewedTime != nil, true)
		connect.AssertEqual(t, certs[1].RenewedTime == nil, true)

		// the old one counted sixty-one days ago, the renewal does not count
		connect.AssertEqual(t, CountNetworkExtenderMeasurementCertsCountedSince(ctx, now.Add(-7*24*time.Hour)), 0)
		connect.AssertEqual(t, CountNetworkExtenderMeasurementCertsCountedSince(ctx, now.Add(-62*24*time.Hour)), 1)
	})
}

// A request whose certificate ran out un-renewed goes back in the queue. One
// with a live certificate, and one that was withdrawn, are left as they are.
func TestReopenExpiredNetworkExtenderMeasurements(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)

		expired := testMeasurementActivate(t, ctx, 1).Extender.ExtenderId
		SetNetworkExtenderMeasurementRequest(ctx, expired, []byte("csr-1"), []byte("key-1"), testMeasurementNames(expired), now.Add(-100*24*time.Hour))
		AddNetworkExtenderMeasurementCert(ctx, testMeasurementCert(expired, []byte("key-1"), now.Add(-100*24*time.Hour)))

		live := testMeasurementActivate(t, ctx, 2).Extender.ExtenderId
		SetNetworkExtenderMeasurementRequest(ctx, live, []byte("csr-2"), []byte("key-2"), testMeasurementNames(live), now)
		AddNetworkExtenderMeasurementCert(ctx, testMeasurementCert(live, []byte("key-2"), now))

		withdrawn := testMeasurementActivate(t, ctx, 3).Extender.ExtenderId
		SetNetworkExtenderMeasurementRequest(ctx, withdrawn, []byte("csr-3"), []byte("key-3"), testMeasurementNames(withdrawn), now)
		withdrawnCert := testMeasurementCert(withdrawn, []byte("key-3"), now)
		AddNetworkExtenderMeasurementCert(ctx, withdrawnCert)
		WithdrawNetworkExtenderMeasurementCert(ctx, withdrawnCert.CertId, "the extender is not active", now)

		for _, extenderId := range []server.Id{expired, live} {
			connect.AssertEqual(t, Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, NetworkExtenderMeasurementStateIssued)
		}
		connect.AssertEqual(t, ReopenExpiredNetworkExtenderMeasurements(ctx, now), 1)
		connect.AssertEqual(t, ReopenExpiredNetworkExtenderMeasurements(ctx, now), 0)

		measurement := Testing_GetNetworkExtenderMeasurement(ctx, expired)
		connect.AssertEqual(t, measurement.State, NetworkExtenderMeasurementStatePending)
		connect.AssertEqual(t, measurement.PendingTime.UTC(), now)
		connect.AssertEqual(t, bytes.Equal(measurement.Csr, []byte("csr-1")), true)
		connect.AssertEqual(t, Testing_GetNetworkExtenderMeasurement(ctx, live).State, NetworkExtenderMeasurementStateIssued)
		connect.AssertEqual(t, Testing_GetNetworkExtenderMeasurement(ctx, withdrawn).State, NetworkExtenderMeasurementStateWithdrawn)
	})
}

// C10: every reader a record is signed from carries the latest expiry of the
// extender's live certificates that serve: their name is published (C8) and
// A8 has handed them to the extender for the key of its latest request. An
// issued certificate whose name is not yet published is none, so is one the
// extender has yet to read from A8, so is one whose publication was cleared
// until it is published again, and so is a withdrawn certificate.
func TestNetworkExtenderRecordReadersCarryTheMeasurementCertExpiry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		withCert := testMeasurementActivate(t, ctx, 1).Extender.ExtenderId
		withoutCert := testMeasurementActivate(t, ctx, 2).Extender.ExtenderId
		now := server.NowUtc().Truncate(time.Second)
		SetNetworkExtenderMeasurementRequest(ctx, withCert, []byte("csr-key-a"), []byte("key-a"), testMeasurementNames(withCert), now)
		cert := testMeasurementCert(withCert, []byte("key-a"), now)
		AddNetworkExtenderMeasurementCert(ctx, cert)

		// issued, and its name not yet published
		extender, _ := GetActiveNetworkExtenderForRecord(ctx, withCert)
		connect.AssertEqual(t, extender.MeasurementCertNotAfter == nil, true)
		for _, entry := range GetRandomActiveNetworkExtenders(ctx, 10, server.Id{}) {
			connect.AssertEqual(t, entry.Extender.MeasurementCertNotAfter == nil, true)
		}

		// published, and the extender has yet to read it
		connect.AssertEqual(t, PublishNetworkExtenderMeasurementNames(ctx, cert.Names, now), 1)
		extender, _ = GetActiveNetworkExtenderForRecord(ctx, withCert)
		connect.AssertEqual(t, extender.MeasurementCertNotAfter == nil, true)
		for _, entry := range GetRandomActiveNetworkExtenders(ctx, 10, server.Id{}) {
			connect.AssertEqual(t, entry.Extender.MeasurementCertNotAfter == nil, true)
		}

		// its poll read it (A8): the first time counts, once
		connect.AssertEqual(t, MarkNetworkExtenderMeasurementCertDelivered(ctx, cert.CertId, now), true)
		connect.AssertEqual(t, MarkNetworkExtenderMeasurementCertDelivered(ctx, cert.CertId, now.Add(time.Hour)), false)
		connect.AssertEqual(t, Testing_GetNetworkExtenderMeasurementCerts(ctx, withCert)[0].DeliverTime.UTC(), now)
		extender, _ = GetActiveNetworkExtenderForRecord(ctx, withCert)
		connect.AssertEqual(t, extender.MeasurementCertNotAfter.UTC(), cert.NotAfter)
		extender, _ = GetActiveNetworkExtenderForRecord(ctx, withoutCert)
		connect.AssertEqual(t, extender.MeasurementCertNotAfter == nil, true)

		signedNotAfters := map[server.Id]*time.Time{}
		sign := func(extender *NetworkExtender, addresses []*NetworkExtenderAddress, issueTime time.Time) ([]byte, error) {
			signedNotAfters[extender.ExtenderId] = extender.MeasurementCertNotAfter
			return []byte("record"), nil
		}
		connect.AssertEqual(t, PublishNetworkExtenderRecord(ctx, withCert, sign), true)
		connect.AssertEqual(t, signedNotAfters[withCert].UTC(), cert.NotAfter)

		bootstrap := GetRandomActiveNetworkExtenders(ctx, 10, server.Id{})
		connect.AssertEqual(t, len(bootstrap), 2)
		for _, entry := range bootstrap {
			if entry.Extender.ExtenderId == withCert {
				connect.AssertEqual(t, entry.Extender.MeasurementCertNotAfter.UTC(), cert.NotAfter)
			} else {
				connect.AssertEqual(t, entry.Extender.MeasurementCertNotAfter == nil, true)
			}
		}

		activated := ActivateNetworkExtender(
			ctx,
			testExtenderActivation([]byte("extender-public-key-measurement-001"), 4, "203.0.113.1"),
			sign,
		)
		connect.AssertEqual(t, activated.Extender.ExtenderId, withCert)
		connect.AssertEqual(t, activated.Extender.MeasurementCertNotAfter.UTC(), cert.NotAfter)

		// publication cleared, as when the name's records went missing: none
		// until the name is published again
		connect.AssertEqual(t, UnpublishNetworkExtenderMeasurementNames(ctx, cert.Names), 1)
		extender, _ = GetActiveNetworkExtenderForRecord(ctx, withCert)
		connect.AssertEqual(t, extender.MeasurementCertNotAfter == nil, true)
		for _, entry := range GetRandomActiveNetworkExtenders(ctx, 10, server.Id{}) {
			connect.AssertEqual(t, entry.Extender.MeasurementCertNotAfter == nil, true)
		}
		connect.AssertEqual(t, PublishNetworkExtenderMeasurementNames(ctx, cert.Names, now), 1)
		extender, _ = GetActiveNetworkExtenderForRecord(ctx, withCert)
		connect.AssertEqual(t, extender.MeasurementCertNotAfter.UTC(), cert.NotAfter)

		// a new key's request: the delivered certificate is for a key the
		// extender asked to replace, so none, and none while the new key's
		// certificate waits for the extender to read it
		SetNetworkExtenderMeasurementRequest(ctx, withCert, []byte("csr-key-b"), []byte("key-b"), testMeasurementNames(withCert), now)
		extender, _ = GetActiveNetworkExtenderForRecord(ctx, withCert)
		connect.AssertEqual(t, extender.MeasurementCertNotAfter == nil, true)
		rotated := testMeasurementCert(withCert, []byte("key-b"), now.Add(time.Minute))
		rotated.CapCounted = false
		connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, rotated), true)
		connect.AssertEqual(t, PublishNetworkExtenderMeasurementNames(ctx, rotated.Names, now), 1)
		extender, _ = GetActiveNetworkExtenderForRecord(ctx, withCert)
		connect.AssertEqual(t, extender.MeasurementCertNotAfter == nil, true)
		for _, entry := range GetRandomActiveNetworkExtenders(ctx, 10, server.Id{}) {
			connect.AssertEqual(t, entry.Extender.MeasurementCertNotAfter == nil, true)
		}
		connect.AssertEqual(t, MarkNetworkExtenderMeasurementCertDelivered(ctx, rotated.CertId, now.Add(time.Minute)), true)
		extender, _ = GetActiveNetworkExtenderForRecord(ctx, withCert)
		connect.AssertEqual(t, extender.MeasurementCertNotAfter.UTC(), rotated.NotAfter)

		for _, withdrawn := range []*NetworkExtenderMeasurementCert{cert, rotated} {
			WithdrawNetworkExtenderMeasurementCert(ctx, withdrawn.CertId, "the extender is not active", now)
		}
		extender, _ = GetActiveNetworkExtenderForRecord(ctx, withCert)
		connect.AssertEqual(t, extender.MeasurementCertNotAfter == nil, true)
	})
}

// The extender row as stored, the tier and the canary channel included.
func getTestMeasurementExtender(t testing.TB, ctx context.Context, extenderId server.Id) *NetworkExtender {
	t.Helper()
	var extender *NetworkExtender
	server.Tx(ctx, func(tx server.PgTx) {
		extender = getNetworkExtenderInTx(ctx, tx, extenderId)
	})
	if extender == nil {
		t.Fatalf("the extender %s is gone", extenderId)
	}
	return extender
}

// An owner's opt-out (lag.dev/PLAN.md C4, C9): every live certificate is
// withdrawn with the reason, the request becomes withdrawn with its
// certificate request dropped, and nothing is pending. A certificate an order
// brings back after the opt-out is stored withdrawn, queued for revocation,
// and never read by a record signer, and the request stays withdrawn.
// Opting out again changes nothing; a request without the flag puts the
// extender back in the queue, for a name that still has an unexpired
// certificate, so a renewal (C7). An extender the operator never activated is
// left alone.
func TestNetworkExtenderMeasurementOptOut(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		activated := testMeasurementActivate(t, ctx, 1)
		extenderId := activated.Extender.ExtenderId
		publicKey := activated.Extender.PublicKey
		names := testMeasurementNames(extenderId)
		reason := "the extender's owner opted out of measurement"
		now := server.NowUtc()

		SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-a"), []byte("key-a"), names, now)
		cert := testMeasurementCert(extenderId, []byte("key-a"), now)
		connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, cert), true)
		// a new key's request is pending when the owner opts out
		SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-b"), []byte("key-b"), names, now.Add(time.Minute))
		connect.AssertEqual(t, len(GetPendingNetworkExtenderMeasurements(ctx, now.Add(2*time.Minute), 10)), 1)

		connect.AssertEqual(t, OptOutNetworkExtenderMeasurement(ctx, publicKey, reason, now.Add(2*time.Minute)), 1)
		measurement := Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
		connect.AssertEqual(t, measurement.State, NetworkExtenderMeasurementStateWithdrawn)
		connect.AssertEqual(t, measurement.Reason, reason)
		connect.AssertEqual(t, len(measurement.Csr), 0)
		if !slices.Equal(measurement.Names, names) || !bytes.Equal(measurement.PublicKey, []byte("key-b")) {
			t.Fatalf("the withdrawn request is %v for %q", measurement.Names, measurement.PublicKey)
		}
		connect.AssertEqual(t, len(GetPendingNetworkExtenderMeasurements(ctx, now.Add(2*time.Minute), 10)), 0)
		connect.AssertEqual(t, len(GetLiveNetworkExtenderMeasurementCerts(ctx, now.Add(2*time.Minute))), 0)
		connect.AssertEqual(t, len(GetNetworkExtenderMeasurementCertsToRevoke(ctx, now.Add(2*time.Minute), 10)), 1)

		// an order that was in flight comes back after the opt-out
		late := testMeasurementCert(extenderId, []byte("key-b"), now.Add(3*time.Minute))
		connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, late), false)
		connect.AssertEqual(t, len(GetLiveNetworkExtenderMeasurementCerts(ctx, now.Add(3*time.Minute))), 0)
		connect.AssertEqual(t, Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, NetworkExtenderMeasurementStateWithdrawn)
		toRevoke := GetNetworkExtenderMeasurementCertsToRevoke(ctx, now.Add(3*time.Minute), 10)
		connect.AssertEqual(t, len(toRevoke), 2)
		connect.AssertEqual(t, toRevoke[1].CertId, late.CertId)
		connect.AssertEqual(t, toRevoke[1].WithdrawReason, reason)
		extender, _ := GetActiveNetworkExtenderForRecord(ctx, extenderId)
		connect.AssertEqual(t, extender.MeasurementCertNotAfter == nil, true)

		// the flag again changes nothing
		connect.AssertEqual(t, OptOutNetworkExtenderMeasurement(ctx, publicKey, reason, now.Add(4*time.Minute)), 0)
		connect.AssertEqual(t, Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, NetworkExtenderMeasurementStateWithdrawn)

		// a request without the flag is back in the queue, for a name with an
		// unexpired certificate
		state := SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-b2"), []byte("key-b"), names, now.Add(5*time.Minute))
		connect.AssertEqual(t, state, NetworkExtenderMeasurementStatePending)
		connect.AssertEqual(t, len(GetPendingNetworkExtenderMeasurements(ctx, now.Add(5*time.Minute), 10)), 1)
		connect.AssertEqual(t, HasUnexpiredNetworkExtenderMeasurementCertForNames(ctx, names, now.Add(5*time.Minute)), true)

		connect.AssertEqual(t, OptOutNetworkExtenderMeasurement(ctx, []byte("extender-public-key-never-activated"), reason, now), 0)
	})
}
