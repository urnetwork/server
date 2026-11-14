//go:build sidecar_pending

package quotahost

// FS-47's quota clauses (§20.7, FI-13 (c); QUOTA-M2-13) on T3: what evs's gate stands on, read from
// Postgres and counted in Redis by the real scripts. The storage row at the exact limit: a network one byte
// under its limit is not refused, the byte that reaches it is, with the limit and the count the refusal
// carries, and a batch retried moves the count once. The clamp: a delta past the count leaves zero, the
// drift counted, and the nightly pass's absolute write corrects it to what the listing shows. Spent: a
// refused lease marks the network, and the over-limit list carries it. Then evs's QuotaGate over this Quota,
// at the limit and while spent: an ordinary upload refused, a re-seed's passed, and twelve erasers asking for
// closure uploads at one instant in a fixed hostile order, paced through the real {sq_} script: R are taken in
// the first second and every eraser has had three turns within 10 s (DESIGN-DETAILS 13.6).
//
// The push kinds at the limit run in evsservice's quota_gate_test.go (EVSB-M2-15), and the CLOSURE rows are
// EVS1-M2-20's.

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/quotasuite"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const fs47T3StorageLimit = int64(1000)

// A counter's value over every label, from the process's registry.
func t3CounterValue(name string) float64 {
	families, err := prometheus.DefaultGatherer.Gather()
	server.Raise(err)
	total := float64(0)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			total += metric.GetCounter().GetValue()
		}
	}
	return total
}

func fs47T3StoredBatch(networkId connect.Id, storedByteDelta int64) *sidecar.UsageBatch {
	return &sidecar.UsageBatch{
		BatchId:  connect.NewId(),
		Producer: sidecar.QuotaProducerEvs,
		Events:   []*sidecar.UsageEvent{{NetworkId: networkId, Meter: sidecar.QuotaMeterEvs, StoredByteDelta: storedByteDelta}},
	}
}

func fs47T3StorageRefusal(t *testing.T, ctx context.Context, target *quotasuite.Target, networkId connect.Id) *sidecar.QuotaRefusal {
	t.Helper()
	overLimit, err := target.Quota.OverLimit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return overLimit.StorageNetworkIdRefusals[networkId]
}

// A network of the tier whose storage limit is fs47T3StorageLimit, with a balance, so it is not spent.
func fs47T3Network(t *testing.T, target *quotasuite.Target) connect.Id {
	t.Helper()
	networkId := connect.NewId()
	if err := target.SetBalances(networkId, []*sidecartest.QuotaBalance{{ByteCount: 1 << 30}}); err != nil {
		t.Fatal(err)
	}
	return networkId
}

// The storage row at the exact limit; answers the network, left at its limit.
func fs47T3ExactLimit(t *testing.T, ctx context.Context, target *quotasuite.Target, oracle *sidecartest.QuotaOracle) connect.Id {
	networkId := fs47T3Network(t, target)
	if err := target.Quota.ReportUsage(ctx, fs47T3StoredBatch(networkId, fs47T3StorageLimit-1)); err != nil {
		t.Fatal(err)
	}
	if refusal := fs47T3StorageRefusal(t, ctx, target, networkId); refusal != nil {
		t.Fatalf("one byte under the limit refused: %+v", refusal)
	}
	last := fs47T3StoredBatch(networkId, 1)
	for range 2 {
		// the second is the batch retried: its delta moves the count once
		if err := target.Quota.ReportUsage(ctx, last); err != nil {
			t.Fatal(err)
		}
		refusal := fs47T3StorageRefusal(t, ctx, target, networkId)
		if refusal == nil || refusal.LimitKey != "storage" || refusal.Limit != fs47T3StorageLimit || refusal.Used != fs47T3StorageLimit {
			t.Fatalf("at the exact limit: %+v", refusal)
		}
	}
	if err := oracle.CheckStored(ctx, networkId, &sidecartest.QuotaStoredListing{SegmentByteCount: fs47T3StorageLimit}); err != nil {
		t.Fatal(err)
	}
	return networkId
}

// The clamp at zero and the nightly pass's correction.
func fs47T3ClampAndDriftCorrection(t *testing.T, ctx context.Context, target *quotasuite.Target, oracle *sidecartest.QuotaOracle) {
	networkId := fs47T3Network(t, target)
	if err := target.Quota.ReportUsage(ctx, fs47T3StoredBatch(networkId, 300)); err != nil {
		t.Fatal(err)
	}
	drift := t3CounterValue("urnetwork_quota_stored_drift_bytes_total")
	// deletes of objects whose uploads' deltas a dropped batch lost: more than the count holds
	if err := target.Quota.ReportUsage(ctx, fs47T3StoredBatch(networkId, -5000)); err != nil {
		t.Fatal(err)
	}
	if err := oracle.CheckStored(ctx, networkId, &sidecartest.QuotaStoredListing{}); err != nil {
		t.Fatalf("a delta past the count: %v", err)
	}
	if got := t3CounterValue("urnetwork_quota_stored_drift_bytes_total") - drift; got != 4700 {
		t.Fatalf("drift counted %v, want the 4700 the clamp cut", got)
	}
	// the listing shows the network at its limit: the nightly write corrects the count, and the refusal follows
	listing := &sidecartest.QuotaStoredListing{SegmentByteCount: 700, PackByteCount: 200, ParcelByteCount: 60, BatchRecordByteCount: 40}
	if err := model.SetNetworkStoredByteCount(ctx, server.Id(networkId), listing.StoredByteCount()); err != nil {
		t.Fatal(err)
	}
	if err := oracle.CheckStored(ctx, networkId, listing); err != nil {
		t.Fatal(err)
	}
	if refusal := fs47T3StorageRefusal(t, ctx, target, networkId); refusal == nil || refusal.Used != fs47T3StorageLimit {
		t.Fatalf("after the correction to the limit: %+v", refusal)
	}
	listing.PackByteCount = 0
	if err := model.SetNetworkStoredByteCount(ctx, server.Id(networkId), listing.StoredByteCount()); err != nil {
		t.Fatal(err)
	}
	if err := oracle.CheckStored(ctx, networkId, listing); err != nil {
		t.Fatal(err)
	}
	if refusal := fs47T3StorageRefusal(t, ctx, target, networkId); refusal != nil {
		t.Fatalf("under the limit after a pack went: %+v", refusal)
	}
}

// evs's gate over the Quota, its first read done.
func fs47T3Gate(t *testing.T, ctx context.Context, target *quotasuite.Target, overNetworkId connect.Id) *evsservice.QuotaGate {
	t.Helper()
	gate, err := evsservice.NewQuotaGate(ctx, &sidecar.ServiceHost{Quota: target.Quota, Clock: target.Clock}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gate.Close)
	for !gate.Over(overNetworkId) {
		_, added := target.Clock.WatchTimers()
		if gate.Over(overNetworkId) {
			break
		}
		select {
		case <-added:
		case <-time.After(30 * time.Second):
			t.Fatal("the gate never read the over-limit list")
		}
	}
	return gate
}

// Twelve erasers at the limit ask at one instant in a fixed order, the last to come first, and ask again
// when their refusals say: the times each was served in span.
func fs47T3Stampede(t *testing.T, ctx context.Context, target *quotasuite.Target, gate *evsservice.QuotaGate, networkId connect.Id, span time.Duration) (start time.Time, served [][]time.Time) {
	const eraserCount = 12
	// rate windows are fixed, turning on the minute, so the run starts on one
	target.Clock.AdvanceTo(target.Clock.Now().Truncate(time.Minute).Add(time.Minute))
	start = target.Clock.Now()
	erasers := make([]connect.Id, eraserCount)
	nextAsks := make([]time.Time, eraserCount)
	served = make([][]time.Time, eraserCount)
	for i := range erasers {
		erasers[i] = connect.NewId()
		nextAsks[i] = start
	}
	step := 50 * time.Millisecond
	for target.Clock.Now().Sub(start) < span {
		now := target.Clock.Now()
		for i := eraserCount - 1; 0 <= i; i -= 1 {
			if now.Before(nextAsks[i]) {
				continue
			}
			refusal, err := gate.ChargeClosure(ctx, networkId, erasers[i], 1024, connect.NewId())
			if err != nil {
				t.Fatal(err)
			}
			if refusal == nil {
				served[i] = append(served[i], now)
				continue
			}
			if refusal.RetryAfterMs == 0 {
				t.Fatalf("a refusal with no retry_after_ms: %+v", refusal)
			}
			nextAsks[i] = now.Add(time.Duration(refusal.RetryAfterMs) * time.Millisecond)
		}
		target.Clock.Advance(step)
	}
	return
}

// FS-47's quota clauses on Postgres and Redis.
func TestFs47T3QuotaClauses(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		defer model.Testing_SetSidecarLimits(model.NetworkTierFree, map[string]int64{"storage": fs47T3StorageLimit})()
		ctx := t.Context()
		target := newT3Target(ctx)(t)
		oracle := newT3Oracle(ctx)

		limitNetworkId := fs47T3ExactLimit(t, ctx, target, oracle)
		fs47T3ClampAndDriftCorrection(t, ctx, target, oracle)

		// spent: a lease refused for want of a balance marks the network
		spentNetworkId := connect.NewId()
		lease, refusal, err := target.Quota.Lease(ctx, spentNetworkId, sidecar.QuotaMeterTls)
		if err != nil || lease != nil || refusal == nil {
			t.Fatalf("a lease with no balance: %v, %+v, %v", lease, refusal, err)
		}
		overLimit, err := target.Quota.OverLimit(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if overLimit.SpentNetworkIdRefusals[spentNetworkId] == nil || overLimit.StorageNetworkIdRefusals[spentNetworkId] != nil {
			t.Fatalf("the spent network in the list: %+v", overLimit)
		}

		// the closure rates, as pro.yml would give them
		closureLimits := map[string]int64{
			"evs_closure_uploads_per_s":   4,
			"evs_closure_uploads_per_min": 240,
			"evs_closure_bytes_per_day":   1024 * 1024 * 1024,
			"evs_closure_bytes_floor":     4 * 1024 * 1024,
		}
		for _, networkId := range []connect.Id{limitNetworkId, spentNetworkId} {
			if err := target.SetLimits(networkId, &sidecar.QuotaLimits{Tier: "team", Limits: closureLimits}); err != nil {
				t.Fatal(err)
			}
		}
		gate := fs47T3Gate(t, ctx, target, limitNetworkId)
		for _, networkId := range []connect.Id{limitNetworkId, spentNetworkId} {
			if !gate.Over(networkId) {
				t.Fatalf("the gate reads %s as under its limits", networkId)
			}
			if refusal := gate.CheckUpload(networkId, false); refusal == nil {
				t.Fatal("an ordinary upload passed at the limit")
			}
			if refusal := gate.CheckUpload(networkId, true); refusal != nil {
				t.Fatalf("a re-seed upload refused at the limit: %+v", refusal)
			}
		}
		// while spent a closure upload is counted and passes, as at the limit
		if refusal, err := gate.ChargeClosure(ctx, spentNetworkId, connect.NewId(), 1024, connect.NewId()); err != nil || refusal != nil {
			t.Fatalf("a closure upload while spent: %+v, %v", refusal, err)
		}

		span := 10 * time.Second
		start, served := fs47T3Stampede(t, ctx, target, gate, limitNetworkId, span)
		firstSecond := 0
		for i, times := range served {
			if len(times) < 3 {
				t.Errorf("eraser %d had %d turns in %s, want three", i, len(times), span)
			}
			for _, at := range times {
				if at.Sub(start) < time.Second {
					firstSecond += 1
				}
			}
		}
		if firstSecond != 4 {
			t.Errorf("%d uploads taken in the first second, want R = 4", firstSecond)
		}
		total := 0
		for _, times := range served {
			total += len(times)
		}
		if 4*int(span/time.Second) < total {
			t.Errorf("%d uploads in %s at R = 4 a second", total, span)
		}
	})
}
