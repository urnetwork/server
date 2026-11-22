package model

// The way back for an address the uptime probes deactivated
// (network_extender_recovery_model.go), against the tables.

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// Spends an address's whole failure budget, as six failed uptime probes do.
func testDeactivateByProbes(ctx context.Context, extenderId server.Id, ipVersion int) {
	for range ExtenderTestingMaxConsecutiveProbeFailures {
		RecordNetworkExtenderProbeResult(
			ctx,
			extenderId,
			ipVersion,
			false,
			server.NowUtc(),
			ExtenderTestingMaxConsecutiveProbeFailures,
			testRevocationSigner(),
		)
	}
}

// The window of the tests: an address that worked in the last day.
func testRecoveryLastWorkedSince() time.Time {
	return server.NowUtc().Add(-24 * time.Hour)
}

// The addresses the probes deactivated are read for their return, with when
// each last worked and whether its extender is revoked; an address something
// else deactivated, one that last worked before the window, and an active one
// are not.
func TestGetNetworkExtenderRecoveryProbeTargetsReadsWhatTheProbesDeactivated(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sign, _ := testRecordSigner()

		revokedKey := []byte("extender-public-key-recovery-revoked")
		revoked := ActivateNetworkExtender(ctx, testExtenderActivation(revokedKey, 4, "192.0.2.41"), sign)
		testDeactivateByProbes(ctx, revoked.Extender.ExtenderId, 4)

		dualKey := []byte("extender-public-key-recovery-dual")
		dual := ActivateNetworkExtender(ctx, testExtenderActivation(dualKey, 4, "192.0.2.42"), sign)
		ActivateNetworkExtender(ctx, testExtenderActivation(dualKey, 6, "2001:db8::42"), sign)
		testDeactivateByProbes(ctx, dual.Extender.ExtenderId, 6)

		// deactivated, but not by the probes: its failure count is clean
		otherKey := []byte("extender-public-key-recovery-other")
		other := ActivateNetworkExtender(ctx, testExtenderActivation(otherKey, 4, "192.0.2.43"), sign)
		ActivateNetworkExtender(ctx, testExtenderActivation(otherKey, 6, "2001:db8::43"), sign)
		Testing_DeactivateNetworkExtenderAddress(ctx, other.Extender.ExtenderId, 6)

		// deactivated by the probes, but it last worked before the window
		oldKey := []byte("extender-public-key-recovery-old")
		old := ActivateNetworkExtender(ctx, testExtenderActivation(oldKey, 4, "192.0.2.44"), sign)
		testDeactivateByProbes(ctx, old.Extender.ExtenderId, 4)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_extender_address SET activate_time = $2 WHERE extender_id = $1`,
				old.Extender.ExtenderId,
				server.NowUtc().Add(-48*time.Hour),
			))
		})

		targets := GetNetworkExtenderRecoveryProbeTargets(
			ctx,
			testRecoveryLastWorkedSince(),
			ExtenderTestingMaxConsecutiveProbeFailures,
		)
		byExtender := map[server.Id]*NetworkExtenderRecoveryProbeTarget{}
		for _, target := range targets {
			byExtender[target.ExtenderId] = target
		}
		connect.AssertEqual(t, len(targets), 2)

		revokedTarget := byExtender[revoked.Extender.ExtenderId]
		if revokedTarget == nil {
			t.Fatal("the revoked extender's address is not read for its return")
		}
		connect.AssertEqual(t, revokedTarget.IpVersion, 4)
		connect.AssertEqual(t, revokedTarget.Ip, netip.MustParseAddr("192.0.2.41"))
		connect.AssertEqual(t, revokedTarget.NetworkId, revoked.Extender.NetworkId)
		// the client a webrtc address's return is probed through
		// (connect/EXTENDER.md S8)
		connect.AssertEqual(t, revokedTarget.ClientId, revoked.Extender.ClientId)
		connect.AssertEqual(t, string(revokedTarget.PublicKey), string(revokedKey))
		connect.AssertEqual(t, revokedTarget.RevokeTime != nil, true)
		connect.AssertEqual(t, revokedTarget.LastProbeTime != nil, true)
		connect.AssertEqual(t, slicesEqualStrings(revokedTarget.Carriers, []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic}), true)

		dualTarget := byExtender[dual.Extender.ExtenderId]
		if dualTarget == nil {
			t.Fatal("the lost family of an active extender is not read for its return")
		}
		connect.AssertEqual(t, dualTarget.IpVersion, 6)
		connect.AssertEqual(t, dualTarget.RevokeTime == nil, true)
		// it never passed an uptime probe, so it last worked at its activation
		stored := Testing_GetNetworkExtender(ctx, dual.Extender.ExtenderId)
		connect.AssertEqual(t, dualTarget.LastWorkTime.Equal(stored.Addresses[1].ActivateTime), true)
	})
}

func slicesEqualStrings(a []string, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}

// A failed probe for a return stamps the address and nothing else. A passing
// one restores the address and its revoked extender, and queues a record of
// its addresses issued after the revocation.
func TestRecordNetworkExtenderRecoveryProbeResultRestoresARevokedExtender(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sign, signed := testRecordSigner()
		publicKey := []byte("extender-public-key-recovery-restore")
		activated := ActivateNetworkExtender(ctx, testExtenderActivation(publicKey, 4, "192.0.2.51"), sign)
		extenderId := activated.Extender.ExtenderId
		testDeactivateByProbes(ctx, extenderId, 4)
		revoked := Testing_GetNetworkExtender(ctx, extenderId)
		revokeTime := *revoked.Extender.RevokeTime
		publishCount := len(Testing_GetNetworkExtenderPublishes(ctx))
		ip := netip.MustParseAddr("192.0.2.51")

		// a failure: only the stamp
		failedAt := server.NowUtc()
		outcome := RecordNetworkExtenderRecoveryProbeResult(ctx, extenderId, 4, ip, false, failedAt, testRecoveryLastWorkedSince(), ExtenderTestingMaxConsecutiveProbeFailures, sign)
		connect.AssertEqual(t, *outcome, NetworkExtenderRecoveryOutcome{})
		stored := Testing_GetNetworkExtender(ctx, extenderId)
		connect.AssertEqual(t, stored.Addresses[0].Active, false)
		connect.AssertEqual(t, stored.Addresses[0].ConsecutiveProbeFailures, ExtenderTestingMaxConsecutiveProbeFailures)
		connect.AssertEqual(t, stored.Addresses[0].LastProbeTime.Equal(failedAt.Truncate(time.Microsecond)), true)
		connect.AssertEqual(t, stored.Extender.Active, false)
		connect.AssertEqual(t, len(Testing_GetNetworkExtenderPublishes(ctx)), publishCount)

		// a success on another ip: nothing
		outcome = RecordNetworkExtenderRecoveryProbeResult(ctx, extenderId, 4, netip.MustParseAddr("192.0.2.52"), true, server.NowUtc(), testRecoveryLastWorkedSince(), ExtenderTestingMaxConsecutiveProbeFailures, sign)
		connect.AssertEqual(t, outcome.AddressRestored, false)
		connect.AssertEqual(t, Testing_GetNetworkExtender(ctx, extenderId).Extender.Active, false)

		// a success past the window: nothing
		outcome = RecordNetworkExtenderRecoveryProbeResult(ctx, extenderId, 4, ip, true, server.NowUtc(), server.NowUtc().Add(time.Hour), ExtenderTestingMaxConsecutiveProbeFailures, sign)
		connect.AssertEqual(t, outcome.AddressRestored, false)
		connect.AssertEqual(t, len(Testing_GetNetworkExtenderPublishes(ctx)), publishCount)

		// a success: restored
		restoredAt := server.NowUtc()
		outcome = RecordNetworkExtenderRecoveryProbeResult(ctx, extenderId, 4, ip, true, restoredAt, testRecoveryLastWorkedSince(), ExtenderTestingMaxConsecutiveProbeFailures, sign)
		connect.AssertEqual(t, outcome.AddressRestored, true)
		connect.AssertEqual(t, outcome.ExtenderRestored, true)
		connect.AssertEqual(t, outcome.RecordQueued, true)
		connect.AssertEqual(t, 0 <= outcome.RevokedDuration, true)

		stored = Testing_GetNetworkExtender(ctx, extenderId)
		connect.AssertEqual(t, stored.Extender.Active, true)
		connect.AssertEqual(t, stored.Extender.RevokeTime == nil, true)
		connect.AssertEqual(t, stored.Addresses[0].Active, true)
		connect.AssertEqual(t, stored.Addresses[0].ConsecutiveProbeFailures, 0)
		connect.AssertEqual(t, stored.Addresses[0].LastProbeSuccessTime.Equal(restoredAt.Truncate(time.Microsecond)), true)
		if stored.Extender.RecordIssueTime == nil || !revokeTime.Before(*stored.Extender.RecordIssueTime) {
			t.Fatalf("the restored record (%v) is not issued after the revocation (%v)", stored.Extender.RecordIssueTime, revokeTime)
		}

		publishes := Testing_GetNetworkExtenderPublishes(ctx)
		connect.AssertEqual(t, len(publishes), publishCount+1)
		last := publishes[len(publishes)-1]
		connect.AssertEqual(t, last.Kind, NetworkExtenderPublishKindRecord)
		connect.AssertEqual(t, string(last.Message), "record:[192.0.2.51]")
		connect.AssertEqual(t, signed()[len(signed())-1], "record:[192.0.2.51]")

		// it is an active address again, probed as any other
		targets := GetActiveNetworkExtenderProbeTargets(ctx)
		connect.AssertEqual(t, len(targets), 1)
		connect.AssertEqual(t, targets[0].ExtenderId, extenderId)

		// and a second success changes nothing
		outcome = RecordNetworkExtenderRecoveryProbeResult(ctx, extenderId, 4, ip, true, server.NowUtc(), testRecoveryLastWorkedSince(), ExtenderTestingMaxConsecutiveProbeFailures, sign)
		connect.AssertEqual(t, outcome.AddressRestored, false)
		connect.AssertEqual(t, len(Testing_GetNetworkExtenderPublishes(ctx)), publishCount+1)
	})
}

// A family restored on an active extender joins its record; a gated extender
// is restored without a publish row, since no open channel carries it.
func TestRecordNetworkExtenderRecoveryProbeResultRestoresAFamilyAndKeepsTheTier(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sign, _ := testRecordSigner()

		dualKey := []byte("extender-public-key-recovery-family")
		dual := ActivateNetworkExtender(ctx, testExtenderActivation(dualKey, 4, "192.0.2.61"), sign)
		ActivateNetworkExtender(ctx, testExtenderActivation(dualKey, 6, "2001:db8::61"), sign)
		testDeactivateByProbes(ctx, dual.Extender.ExtenderId, 6)
		publishCount := len(Testing_GetNetworkExtenderPublishes(ctx))

		outcome := RecordNetworkExtenderRecoveryProbeResult(ctx, dual.Extender.ExtenderId, 6, netip.MustParseAddr("2001:db8::61"), true, server.NowUtc(), testRecoveryLastWorkedSince(), ExtenderTestingMaxConsecutiveProbeFailures, sign)
		connect.AssertEqual(t, outcome.AddressRestored, true)
		connect.AssertEqual(t, outcome.ExtenderRestored, false)
		connect.AssertEqual(t, outcome.RecordQueued, true)
		publishes := Testing_GetNetworkExtenderPublishes(ctx)
		connect.AssertEqual(t, len(publishes), publishCount+1)
		connect.AssertEqual(t, string(publishes[len(publishes)-1].Message), "record:[192.0.2.61 2001:db8::61]")

		gatedKey := []byte("extender-public-key-recovery-gated")
		gated := ActivateNetworkExtender(ctx, testExtenderActivation(gatedKey, 4, "192.0.2.62"), sign)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_extender SET directory_tier = $2 WHERE extender_id = $1`,
				gated.Extender.ExtenderId,
				connect.ExtenderDirectoryTierGated,
			))
		})
		testDeactivateByProbes(ctx, gated.Extender.ExtenderId, 4)
		publishCount = len(Testing_GetNetworkExtenderPublishes(ctx))

		outcome = RecordNetworkExtenderRecoveryProbeResult(ctx, gated.Extender.ExtenderId, 4, netip.MustParseAddr("192.0.2.62"), true, server.NowUtc(), testRecoveryLastWorkedSince(), ExtenderTestingMaxConsecutiveProbeFailures, sign)
		connect.AssertEqual(t, outcome.AddressRestored, true)
		connect.AssertEqual(t, outcome.ExtenderRestored, true)
		connect.AssertEqual(t, outcome.RecordQueued, false)
		connect.AssertEqual(t, Testing_GetNetworkExtender(ctx, gated.Extender.ExtenderId).Extender.Active, true)
		connect.AssertEqual(t, len(Testing_GetNetworkExtenderPublishes(ctx)), publishCount)
	})
}

// An address something other than the probes deactivated keeps its clean
// failure count, and a probe for its return never restores it.
func TestRecordNetworkExtenderRecoveryProbeResultLeavesWhatTheProbesDidNotDeactivate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sign, _ := testRecordSigner()
		publicKey := []byte("extender-public-key-recovery-elsewhere")
		activated := ActivateNetworkExtender(ctx, testExtenderActivation(publicKey, 4, "192.0.2.71"), sign)
		ActivateNetworkExtender(ctx, testExtenderActivation(publicKey, 6, "2001:db8::71"), sign)
		Testing_DeactivateNetworkExtenderAddress(ctx, activated.Extender.ExtenderId, 6)

		outcome := RecordNetworkExtenderRecoveryProbeResult(ctx, activated.Extender.ExtenderId, 6, netip.MustParseAddr("2001:db8::71"), true, server.NowUtc(), testRecoveryLastWorkedSince(), ExtenderTestingMaxConsecutiveProbeFailures, sign)
		connect.AssertEqual(t, outcome.AddressRestored, false)
		stored := Testing_GetNetworkExtender(ctx, activated.Extender.ExtenderId)
		connect.AssertEqual(t, stored.Addresses[1].Active, false)
	})
}
