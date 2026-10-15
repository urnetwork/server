// The capability probe's whole pass over a real tunnel, from its selection to
// the evidence row. An external test: capprobe imports providertunnel.
package providertunnel_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/capprobe"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/providertunnel"
)

// The value of one of the capability probe's counters, by its name and its
// labels, from the registry the probe registers with.
func capabilityProbeCounterValue(tb testing.TB, name string, labels map[string]string) float64 {
	tb.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		tb.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			matched := len(metric.GetLabel()) == len(labels)
			for _, pair := range metric.GetLabel() {
				if labels[pair.GetName()] != pair.GetValue() {
					matched = false
				}
			}
			if matched {
				return metric.GetCounter().GetValue()
			}
		}
	}
	tb.Fatalf("no counter %s %v", name, labels)
	return 0
}

// Makes clientId a provider the capability probe selects: connected with one
// valid location, and with recent URL successes that the egress rules admit
// to Quality and Speed.
func capabilityProbeSelectable(tb testing.TB, ctx context.Context, clientId server.Id) {
	tb.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx,
			`INSERT INTO network_client_location_reliability (client_id, update_block_number, country_location_id, client_address_hash_count, location_count, connected) VALUES ($1, 0, $2, 1, 1, true)
			ON CONFLICT (client_id) DO UPDATE SET country_location_id = EXCLUDED.country_location_id, client_address_hash_count = 1, location_count = 1, connected = true`,
			clientId, server.NewId(),
		))
	})
	now := server.NowUtc().Truncate(time.Microsecond)
	destination := egresshealth.Destination{Name: "synthetic-document", Class: egresshealth.ClassSite, Url: "https://document.example/"}
	for index := range 5 {
		measuredAt := now.Add(-time.Duration(index+1) * time.Minute)
		model.SetProviderEgressHealth(ctx, &model.ProviderEgressHealth{
			RunId:      server.NewId(),
			ClientId:   clientId,
			MeasuredAt: measuredAt,
			OKCount:    1,
			Total:      1,
			UrlProbeEvidence: &egresshealth.UrlProbeEvidence{
				PolicyVersion: egresshealth.UrlProbePolicyVersion, Policy: egresshealth.DefaultUrlProbePolicy(),
				Destination: destination, MeasuredAt: measuredAt, ContentMatcherVersion: 1,
				Security:   []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: measuredAt, TlsAuthenticated: true}},
				StatusCode: 200, ByteCount: 32, WireByteCount: 32, WireSampleByteCount: 31, BodyComplete: true,
				RequestWritten: true, FirstByteReceived: true, TtfbMillis: 10, BodyMillis: 1, BodyBitsPerSecond: 248000,
				ContentClassification: "content", PerformanceClassification: "insufficient_sample",
			},
		})
	}
}

// Re-review finding 1: nothing showed that the capability probe can record a
// modern provider at all. The probe's real pass, SelectPass and FinishPass,
// selects a provider that runs the current app's opportunistic sessions on a
// local platform, measures it through capprobe's own tunnel opener and the
// real Open path with the in-process credential and control authority, and
// records it modern: a session its responder negotiated with the
// post-quantum hybrid group, and acknowledgements that advertise a window. If
// the probe's sessions or the reading broke, every measurement would end
// without evidence, and once the gate is on every provider would leave
// Quality and Speed.
func TestCapabilityProbeRecordsAModernProviderThroughARealTunnel(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		stack, closeStack := providertunnel.NewCapabilityTestStack(tb, ctx)
		defer closeStack()
		defer providertunnel.UseHermeticTunnelClientSettings()()
		capabilityProbeSelectable(tb, ctx, stack.ProviderClientId)

		settings := capprobe.DefaultSettings()
		settings.Enabled = true
		settings.Concurrency = 1
		settings.Limit = 1
		selection, err := capprobe.SelectPass(ctx, settings, capprobe.Sweep{})
		if err != nil || !selection.NeedsTunnels() {
			tb.Fatalf("the pass did not select the provider: %v", err)
		}
		modernLabels := map[string]string{"outcome": "recorded_modern"}
		resizableLabels := map[string]string{"capability": "window", "reading": "resizable"}
		modernBefore := capabilityProbeCounterValue(tb, "urnetwork_capability_probe_providers_total", modernLabels)
		resizableBefore := capabilityProbeCounterValue(tb, "urnetwork_capability_probe_readings_total", resizableLabels)
		postQuantumBefore := 0.0
		for _, reading := range []string{"post_quantum_sealed", "post_quantum_unsealed"} {
			postQuantumBefore += capabilityProbeCounterValue(tb, "urnetwork_capability_probe_readings_total", map[string]string{"capability": "e2e", "reading": reading})
		}

		_, result, err := capprobe.FinishPass(ctx, settings, selection, providertunnel.Config{
			ApiUrl:            stack.ApiUrl,
			PlatformUrl:       stack.PlatformUrl,
			ByJwt:             stack.SourceJwt,
			ClientId:          connect.Id(stack.SourceClientId),
			DeviceDescription: model.ProberClientDescription,
			DeviceSpec:        model.ProberClientDeviceSpec,
			Version:           server.RequireVersion(),
			ClientCredentials: stack.Credentials(),
			ClientControl:     stack.Control(),
		})
		if err != nil || result.Started != 1 || result.Recorded != 1 {
			tb.Fatalf("the pass returned %+v, %v; want one provider measured and recorded", result, err)
		}

		var postQuantum, windowResizable bool
		var keyExchange string
		var maxReceiveWindowByteCount int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx,
				`SELECT post_quantum, window_resizable, key_exchange, max_receive_window_byte_count FROM provider_modern_evidence WHERE client_id = $1`,
				stack.ProviderClientId,
			).Scan(&postQuantum, &windowResizable, &keyExchange, &maxReceiveWindowByteCount))
		})
		if !postQuantum || !windowResizable || keyExchange != "X25519MLKEM768" || maxReceiveWindowByteCount <= 0 {
			tb.Fatalf("recorded post_quantum=%t window_resizable=%t key_exchange=%q max_receive_window_byte_count=%d, want a modern row", postQuantum, windowResizable, keyExchange, maxReceiveWindowByteCount)
		}
		if modern := capabilityProbeCounterValue(tb, "urnetwork_capability_probe_providers_total", modernLabels) - modernBefore; modern != 1 {
			tb.Fatalf("counted %v recorded_modern measurements, want 1", modern)
		}
		if resizable := capabilityProbeCounterValue(tb, "urnetwork_capability_probe_readings_total", resizableLabels) - resizableBefore; resizable != 1 {
			tb.Fatalf("counted %v resizable window readings, want 1", resizable)
		}
		postQuantumAfter := 0.0
		for _, reading := range []string{"post_quantum_sealed", "post_quantum_unsealed"} {
			postQuantumAfter += capabilityProbeCounterValue(tb, "urnetwork_capability_probe_readings_total", map[string]string{"capability": "e2e", "reading": reading})
		}
		if postQuantumAfter-postQuantumBefore != 1 {
			tb.Fatalf("counted %v post-quantum session readings, want 1", postQuantumAfter-postQuantumBefore)
		}
		if apiCalls := stack.ApiCalls(); apiCalls != 0 {
			tb.Fatalf("the control API was used %d times", apiCalls)
		}
	})
}
