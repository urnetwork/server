package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The dashboard counts' capability and post-quantum series (FP2-PROTOCOL.md
// §8.1) against the database: each provider counts in every bucket its
// classified decision admits, under its labels and its post-quantum signal.
// A provider UDP alone kept out of Quality and Speed counts there as
// ipv4+tcp, and a provider that is not downgraded counts by its strict
// decision. Needs the failed_later_legs and pq_outcome columns of the
// evidence migration.
func TestCountProviderEgressCountsTheClassifiedCapabilityAndPostQuantumSignal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Capability Count City", "Capability Count Region", "United States", "us")
		pass := capabilityStatusTestRun{ok: true}
		positive := capabilityStatusTestRun{ok: true, pqOutcome: providerEgressPqOutcomeSqlPositive}
		negative := capabilityStatusTestRun{ok: true, pqOutcome: providerEgressPqOutcomeSqlNegative}
		ipv4Udp := capabilityStatusTestRun{failedLaterLegs: ProviderEgressFailedLaterLegIpv4Udp}
		ipv6Tcp := capabilityStatusTestRun{failedLaterLegs: ProviderEgressFailedLaterLegIpv6Tcp}
		connectProvider := func() *egressTestProvider {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			egressTestReliability(ctx, provider.clientId, 0, 1, 1)
			egressTestReliability(ctx, provider.clientId, 1, 1, 1)
			egressTestReliability(ctx, provider.clientId, 2, 1, 1)
			return provider
		}

		// v4-only, 10 of 20 failed only over HTTP/3: strict online only, class
		// D in every bucket; its newest post-quantum outcome is positive
		udpless := connectProvider()
		capabilityStatusTestRuns(ctx, udpless.clientId, []capabilityStatusTestRun{negative}, capabilityStatusTestRepeat(10, ipv4Udp), capabilityStatusTestRepeat(8, pass), []capabilityStatusTestRun{positive})
		// dual-stack, every run passes: class A; its newest outcome negative
		healthy := connectProvider()
		capabilityStatusTestRuns(ctx, healthy.clientId, []capabilityStatusTestRun{positive}, capabilityStatusTestRepeat(18, pass), []capabilityStatusTestRun{negative})
		// dual-stack, 5 of 20 failed only over IPv6: class B, no outcome
		ipv6less := connectProvider()
		capabilityStatusTestRuns(ctx, ipv6less.clientId, capabilityStatusTestRepeat(15, pass), capabilityStatusTestRepeat(5, ipv6Tcp))
		// v4-only and unprobed: online, advertised labels, no outcome
		connectProvider()

		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		capabilityStatusTestDualstack(ctx, healthy.clientId)
		capabilityStatusTestDualstack(ctx, ipv6less.clientId)

		counts := CountProviderEgress(ctx)
		want := newProviderEgressCapabilityCounts()
		wantPostQuantum := newProviderPostQuantumCounts()
		for _, bucket := range []string{RankModeQuality, RankModeSpeed, ProviderEgressBucketOnline} {
			want[bucket][ProviderCapabilityIpIpv4][ProviderCapabilityProtocolTcp] += 1
			wantPostQuantum[bucket][ProviderPostQuantumSignalPositive] += 1
			want[bucket][ProviderCapabilityIpDualstack][ProviderCapabilityProtocolDualproto] += 1
			wantPostQuantum[bucket][ProviderPostQuantumSignalNegative] += 1
			want[bucket][ProviderCapabilityIpIpv4][ProviderCapabilityProtocolDualproto] += 1
			wantPostQuantum[bucket][ProviderPostQuantumSignalNone] += 1
		}
		// the unprobed provider is online only
		want[ProviderEgressBucketOnline][ProviderCapabilityIpIpv4][ProviderCapabilityProtocolDualproto] += 1
		wantPostQuantum[ProviderEgressBucketOnline][ProviderPostQuantumSignalNone] += 1
		for _, bucket := range ProviderEgressBuckets {
			for _, ip := range ProviderEgressCapabilityIps {
				for _, protocol := range ProviderEgressCapabilityProtocols {
					if got := counts.CapabilityCounts[bucket][ip][protocol]; got != want[bucket][ip][protocol] {
						t.Errorf("%s %s+%s: %d, want %d", bucket, ip, protocol, got, want[bucket][ip][protocol])
					}
				}
			}
			for _, signal := range ProviderPostQuantumSignals {
				if got := counts.PostQuantumCounts[bucket][signal]; got != wantPostQuantum[bucket][signal] {
					t.Errorf("%s %s: %d, want %d", bucket, signal, got, wantPostQuantum[bucket][signal])
				}
			}
		}
		// the strict counts are unchanged: the UDP-less provider is no native
		// of the strict decision, the other two are
		if counts.BucketIndexCounts[RankModeSpeed] == nil {
			t.Fatal("no strict speed counts")
		}
		strictSpeed := int64(0)
		for _, count := range counts.BucketIndexCounts[RankModeSpeed] {
			strictSpeed += count
		}
		if strictSpeed != 2 {
			t.Errorf("strict speed holds %d providers, want 2", strictSpeed)
		}
	})
}
