//go:build sidecar_pending

package quotahost

// QUOTA-M10-01's api half on T3 (§20.6's adds, purge and drop rows; §20.7's stored, count, limit, closure, allow and
// wait rows; D-QUOTA-3, D-QUOTA-12, R-QUOTA-7; FS-71, FS-90, FS-92, FS-112's inputs): what format 2's jobs report,
// applied by NewLocalQuota's parts over the private stack's Postgres and Redis, with FI-13's oracle read after every
// step. The evs jobs decide what to report and are EVSB's and EVS2's to test (evsservice's purge, evacuation, sweep
// and drop tests on T2); here each schedule sends the batches those jobs send at each of its steps, as its producer
// sends them, and holds the count to them:
//
//	adds      evs's batches: an accepted push's record length, at the limit too, and the transfer it debits
//	purge     the committing evs process's negative delta for a kind-5 segment it deleted at seg/, once however often
//	          a lost answer makes it resend; the sweep's DELETE of the same id answers 204 and reports nothing
//	staged    taskworker's negative delta for a staged segment its evacuation dropped with its batch, 23 to 25 h
//	          after its push, whose purge found nothing at seg/ and reported nothing
//	observe   D-TEST-12's observe mode reports nothing, so replaced content counts until deletes turn on, and the
//	          turn-on pass, a sweep, reports nothing either: the nightly pass catches what only it freed
//	drop      taskworker's drop batches, each id from (scan id, cursor range): two instances racing one id apply it
//	          once, a resumed instance's resend is a no-op, a held object's move to held is reported once and its
//	          later DELETE never; service_usage_batch keeps an id 24 h, past which a resend would apply again, the
//	          hazard EVSB-M10-06's cursor must stay inside and the nightly pass corrects
//	rates     evs_drops_per_day a network and UTC day and evs_drop_previews_per_day a repo and UTC day through
//	          ServiceHost.Quota.CountRate, each 429 past its limit with retry_after_ms to 00:00 UTC, dark while
//	          enforce_sidecar_limits is off
//	closure   FS-112's inputs: an eraser at evs_closure_bytes_floor, the run starting 10 min before 00:00 UTC, its
//	          closure bytes and uploads counted, and the allowance's and pacing's retry_after_ms giving the finish its
//	          eraser computes, which agrees with the fake clock's to within one turn, a restart's replay counting
//	          nothing
//
// Between nightly passes the count is the last night's moved by each reported delta once; at each nightly pass it is
// the listing's formula (FI-13 (c)), which a step that freed bytes and reported none leaves above the count's until
// that pass. The nightly writes are the closing job's (fs60_count_t3_test.go's reading): the pass that makes them from
// LISTs runs on T2.

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/quotasuite"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const format2UsageBalanceByteCount = int64(1 << 30)

// One network's stored bytes as a schedule moves them.
type format2UsageNetwork struct {
	networkId connect.Id
	// what LISTs and heads would show of the network's stored bytes now
	listing *sidecartest.QuotaStoredListing
	// what network_quota_state must hold: the last night's count moved by each delta reported, once
	count int64
}

// A T3 target, FI-13's oracle over it, and an evs producer's and a taskworker producer's window ids.
type format2UsageWorld struct {
	t      *testing.T
	ctx    context.Context
	target *quotasuite.Target
	oracle *sidecartest.QuotaOracle

	evsInstance        [16]byte
	evsWindowSeq       uint64
	taskworkerInstance [16]byte
	taskworkerSeq      uint64
}

func newFormat2UsageWorld(t *testing.T, ctx context.Context) *format2UsageWorld {
	t.Helper()
	world := &format2UsageWorld{t: t, ctx: ctx, target: newT3Target(ctx)(t), oracle: newT3Oracle(ctx)}
	if _, err := rand.Read(world.evsInstance[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(world.taskworkerInstance[:]); err != nil {
		t.Fatal(err)
	}
	return world
}

// A network with one in-window balance and nothing counted.
func (self *format2UsageWorld) network() *format2UsageNetwork {
	self.t.Helper()
	network := &format2UsageNetwork{networkId: connect.NewId(), listing: &sidecartest.QuotaStoredListing{}}
	if err := self.target.SetBalances(network.networkId, []*sidecartest.QuotaBalance{{ByteCount: format2UsageBalanceByteCount}}); err != nil {
		self.t.Fatal(err)
	}
	self.oracle.Grant(network.networkId, format2UsageBalanceByteCount)
	return network
}

// The UTC minute the clock stands in: a batch's window.
func (self *format2UsageWorld) window() time.Time {
	return self.target.Clock.Now().UTC().Truncate(time.Minute)
}

// evs's next window for the network: transfer the network's callers read and a stored-byte delta of its repos.
func (self *format2UsageWorld) evsBatch(network *format2UsageNetwork, transferByteCount int64, storedByteDelta int64) *sidecar.UsageBatch {
	self.evsWindowSeq += 1
	return &sidecar.UsageBatch{
		BatchId:     sidecar.NewUsageBatchId(self.evsInstance, self.evsWindowSeq),
		Producer:    sidecar.QuotaProducerEvs,
		WindowStart: self.window(),
		Events: []*sidecar.UsageEvent{{
			NetworkId: network.networkId, Meter: sidecar.QuotaMeterEvs, TransferByteCount: transferByteCount, StoredByteDelta: storedByteDelta,
		}},
	}
}

// taskworker's evacuation batcher's next window: one free of staged content.
func (self *format2UsageWorld) evacuationBatch(network *format2UsageNetwork, storedByteDelta int64) *sidecar.UsageBatch {
	self.taskworkerSeq += 1
	return self.taskworkerBatch(sidecar.NewUsageBatchId(self.taskworkerInstance, self.taskworkerSeq), network, storedByteDelta)
}

// The drop job's batch for one checkpoint: its id from the job's scan and cursor range, not its instance.
func (self *format2UsageWorld) dropBatch(scanId connect.Id, cursorFrom string, cursorTo string, network *format2UsageNetwork, storedByteDelta int64) *sidecar.UsageBatch {
	return self.taskworkerBatch(sidecar.NewDropUsageBatchId(scanId, []byte(cursorFrom), []byte(cursorTo)), network, storedByteDelta)
}

// taskworker's frees carry a stored-byte delta on evs's meter and no transfer (§20.6).
func (self *format2UsageWorld) taskworkerBatch(batchId connect.Id, network *format2UsageNetwork, storedByteDelta int64) *sidecar.UsageBatch {
	return &sidecar.UsageBatch{
		BatchId:     batchId,
		Producer:    sidecar.QuotaProducerTaskworker,
		WindowStart: self.window(),
		Events:      []*sidecar.UsageEvent{{NetworkId: network.networkId, Meter: sidecar.QuotaMeterEvs, StoredByteDelta: storedByteDelta}},
	}
}

// Sends batch sendCount times, as a producer whose answers were lost resends it under its id, and moves each named
// network's count by its delta once, metering its transfer once.
func (self *format2UsageWorld) report(what string, batch *sidecar.UsageBatch, sendCount int, networks ...*format2UsageNetwork) {
	self.t.Helper()
	for range sendCount {
		if err := self.target.Quota.ReportUsage(self.ctx, batch); err != nil {
			self.t.Fatalf("%s: %v", what, err)
		}
	}
	self.applied(batch, networks...)
	self.check(what, networks...)
}

// What batch moves, once: each named network's count by its events' deltas, and their transfer metered.
func (self *format2UsageWorld) applied(batch *sidecar.UsageBatch, networks ...*format2UsageNetwork) {
	for _, event := range batch.Events {
		for _, network := range networks {
			if network.networkId != event.NetworkId {
				continue
			}
			network.count = max(0, network.count+event.StoredByteDelta)
			if 0 < event.TransferByteCount {
				self.oracle.Meter(network.networkId, event.Meter, event.TransferByteCount)
			}
		}
	}
}

// The closing job's write for the night: each network's count is its listing's formula (FI-13 (c)).
func (self *format2UsageWorld) night(what string, networks ...*format2UsageNetwork) {
	self.t.Helper()
	for _, network := range networks {
		if err := model.SetNetworkStoredByteCount(self.ctx, server.Id(network.networkId), network.listing.StoredByteCount()); err != nil {
			self.t.Fatalf("%s: %v", what, err)
		}
		network.count = network.listing.StoredByteCount()
		if err := self.oracle.CheckStored(self.ctx, network.networkId, network.listing); err != nil {
			self.t.Fatalf("%s: %v", what, err)
		}
	}
	self.check(what, networks...)
}

// FI-13 (a)'s upper bound over every network, and each named network's count as its deltas moved it.
func (self *format2UsageWorld) check(what string, networks ...*format2UsageNetwork) {
	self.t.Helper()
	if err := self.oracle.Check(self.ctx); err != nil {
		self.t.Fatalf("%s: %v", what, err)
	}
	for _, network := range networks {
		state, err := model.GetNetworkQuotaState(self.ctx, server.Id(network.networkId))
		if err != nil {
			self.t.Fatalf("%s: %v", what, err)
		}
		count := int64(0)
		if state != nil {
			count = state.StoredByteCount
		}
		if count != network.count {
			self.t.Fatalf("%s: network %s counts %d stored, want %d", what, network.networkId, count, network.count)
		}
	}
}

// FI-13 (a)'s lower bound once the debits flushed: every metered byte debited.
func (self *format2UsageWorld) settled(what string) {
	self.t.Helper()
	if err := self.target.Settle(); err != nil {
		self.t.Fatalf("%s: %v", what, err)
	}
	if err := self.oracle.CheckSettled(self.ctx); err != nil {
		self.t.Fatalf("%s: %v", what, err)
	}
}

// The storage refusal evs's gate reads for the network (QuotaGate), nil while it is under its limit.
func (self *format2UsageWorld) storageRefusal(network *format2UsageNetwork) *sidecar.QuotaRefusal {
	self.t.Helper()
	overLimit, err := self.target.Quota.OverLimit(self.ctx)
	if err != nil {
		self.t.Fatal(err)
	}
	return overLimit.StorageNetworkIdRefusals[network.networkId]
}

// Each case's switches: enforce_sidecar_limits on, a fresh limit snapshot after it, and Community's storage at
// storageByteCount.
func format2UsageRun(t *testing.T, storageByteCount int64, run func(ctx context.Context)) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		// the process's limit snapshot is dropped at the end, so the next test reads limits as a fresh process does
		defer model.Testing_SetNetworkSidecarLimitSettings(model.DefaultNetworkSidecarLimitSettings())()
		defer model.Testing_SetSidecarLimits(model.NetworkTierFree, map[string]int64{"storage": storageByteCount})()
		run(t.Context())
	})
}

// FS-71's api half: at the exact storage limit a push that only removes a versionless file is accepted and counts its
// record, the purge's free of the replaced kind-5 segment applies once and takes the network under its limit, the
// sweep's 204 for the same id counts nothing twice, and a staged candidate replaced 24 h after its push is subtracted
// once, by evacuation, its purge having found nothing at seg/; the night's count is the listing's.
func TestFormat2UsageT3Fs71FreesMoveTheCountOnce(t *testing.T) {
	format2UsageRun(t, 10000, func(ctx context.Context) {
		world := newFormat2UsageWorld(t, ctx)
		network := world.network()

		// the night before: x's version 1 on its kind-5 segment s1 (3,000 B) and 5,000 B of other segments at seg/,
		// and push u1's record still in its batch, 2,000 B with its staged segment t1's 1,500 B: exactly the limit
		network.listing.SegmentByteCount = 8000
		network.listing.BatchRecordByteCount = 2000
		world.night("the night before", network)
		refusal := world.storageRefusal(network)
		if refusal == nil || refusal.LimitKey != "storage" || refusal.Limit != 10000 || refusal.Used != 10000 {
			t.Fatalf("at the exact limit the storage refusal is %+v", refusal)
		}
		// an overwrite adding payload is refused there by evs's gate, which uploads nothing and reports nothing

		// u2 removes x, adding no payload: accepted at the limit, its 200 B record counts and its upload is read
		network.listing.BatchRecordByteCount += 200
		world.report("u2's record at the limit", world.evsBatch(network, 200, 200), 1, network)
		if world.storageRefusal(network) == nil {
			t.Fatal("a record past the limit took the network under it")
		}

		// a minute later u2's process purges s1 at seg/ and reports it, resending once after a lost answer
		world.target.Clock.Advance(time.Minute)
		network.listing.SegmentByteCount -= 3000
		world.report("the purge of s1", world.evsBatch(network, 0, -3000), 2, network)
		if refusal := world.storageRefusal(network); refusal != nil {
			t.Fatalf("7,200 stored of 10,000 still refused: %+v", refusal)
		}
		// evacuation's sweep of u2's record DELETEs s1 again, answered 204, and reports nothing
		world.check("the sweep's 204 for s1", network)

		// 24 h after u1, u3 replaces t1's version: its 300 B record counts; its purge, t1 being over 23 h old, DELETEs at
		// seg/, where t1 is not yet, and reports nothing
		world.target.Clock.Advance(24 * time.Hour)
		network.listing.BatchRecordByteCount += 300
		world.report("u3's record", world.evsBatch(network, 300, 300), 1, network)

		// at 25 h evacuation empties u1's minute: its record goes to a parcel less t1, which it drops, and taskworker
		// reports t1 once, resending once after a lost answer
		world.target.Clock.Advance(time.Hour)
		network.listing.BatchRecordByteCount -= 2000
		network.listing.ParcelByteCount += 500
		world.report("t1 dropped at evacuation", world.evacuationBatch(network, -1500), 2, network)

		world.night("the next night", network)
		world.settled("the run's reads")
	})
}

// FS-71's observe clause (D-TEST-12): while the purge observes, the replaced content stays and nothing reports it, so
// it counts; the turn-on pass deletes it as a sweep does, reporting nothing, and the next night's count lets it go.
func TestFormat2UsageT3Fs71ObserveCountsReplacedContentUntilDeletesTurnOn(t *testing.T) {
	format2UsageRun(t, 1<<30, func(ctx context.Context) {
		world := newFormat2UsageWorld(t, ctx)
		network := world.network()
		network.listing.SegmentByteCount = 6000
		world.night("the night before", network)

		// u2 replaces x under observe: its record counts, and its purge deletes nothing and reports nothing
		network.listing.BatchRecordByteCount += 200
		world.report("u2's record", world.evsBatch(network, 200, 200), 1, network)
		world.target.Clock.Advance(24 * time.Hour)
		world.night("a night under observe: x's replaced segment still listed and counted", network)

		// deletes turn on: the turn-on pass deletes the 3,000 B segment observe kept, and reports nothing
		network.listing.SegmentByteCount -= 3000
		world.check("after the turn-on pass, before the night", network)
		if want := network.listing.StoredByteCount() + 3000; network.count != want {
			t.Fatalf("the count %d before the night, want the turn-on's 3,000 B still in it: %d", network.count, want)
		}
		world.target.Clock.Advance(24 * time.Hour)
		world.night("the night after the turn-on", network)
		world.settled("the run's reads")
	})
}

// FS-90's accounting: two instances racing the take report the same checkpoint's batch, whose id hashes the scan and
// cursor range, and it applies once; another instance resuming from that checkpoint resends it and nothing moves; the
// next range's batch is its own. service_usage_batch keeps the id 24 h: inside that a resend is a no-op, past it the
// same id applies again, which is why EVSB-M10-06 advances its cursor first, and the next night corrects the count.
func TestFormat2UsageT3Fs90FreedBytesApplyOnceAcrossInstances(t *testing.T) {
	format2UsageRun(t, 1<<30, func(ctx context.Context) {
		world := newFormat2UsageWorld(t, ctx)
		network := world.network()
		network.listing.SegmentByteCount = 8000
		world.night("the night before", network)
		scanId := connect.NewId()

		// d1 and d2, 2,000 B, deleted by both instances, each id once or found gone: one checkpoint's batch from each
		first := world.dropBatch(scanId, "cursor 0", "cursor 1", network, -2000)
		var wait sync.WaitGroup
		errs := make([]error, 2)
		for i := range errs {
			wait.Go(func() {
				errs[i] = world.target.Quota.ReportUsage(ctx, first)
			})
		}
		wait.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatal(err)
		}
		network.listing.SegmentByteCount -= 2000
		world.applied(first, network)
		world.check("two instances racing one checkpoint's batch", network)

		// a third instance resumes from the same checkpoint after the lease lapsed and reports its range again
		world.target.Clock.Advance(5 * time.Minute)
		world.report("a resumed instance's resend", first, 1)
		world.check("a resumed instance's resend", network)

		// the next range, 1,000 B more
		second := world.dropBatch(scanId, "cursor 1", "cursor 2", network, -1000)
		network.listing.SegmentByteCount -= 1000
		world.report("the next range", second, 1, network)

		// the dedupe horizon: an hour inside 24 h of its apply the id is still kept, so a resend is a no-op. The host
		// that sent it has long forgotten the batch (its plan horizon), so only service_usage_batch's row answers
		world.target.Clock.Advance(23 * time.Hour)
		if _, err := model.RemoveExpiredServiceUsage(ctx, world.target.Clock.Now(), 1000); err != nil {
			t.Fatal(err)
		}
		world.report("a resend within 24 h", second, 1)
		world.check("a resend within 24 h", network)
		// past it the id is gone, and a job stalled with its cursor unadvanced would subtract the range again: the host
		// has forgotten the last resend too, an hour being past its plan horizon
		world.target.Clock.Advance(time.Hour + time.Minute)
		if _, err := model.RemoveExpiredServiceUsage(ctx, world.target.Clock.Now(), 1000); err != nil {
			t.Fatal(err)
		}
		world.report("a resend past 24 h", second, 1, network)
		if want := network.listing.StoredByteCount() - 1000; network.count != want {
			t.Fatalf("the count %d past the horizon, want the range subtracted twice: %d", network.count, want)
		}
		world.night("the night corrects it", network)
		world.settled("the run")
	})
}

// FS-92's accounting: a hold placed before the DROP keeps what the drop would delete, and the job's move of those
// objects to held is reported once, the count falling then; when the hold ends the job DELETEs them and reports
// nothing, the count unmoved. The held network's count and bill equal a no-hold twin's at every night.
func TestFormat2UsageT3Fs92HeldBytesLeaveTheCountOnceAtMoveToHeld(t *testing.T) {
	format2UsageRun(t, 1<<30, func(ctx context.Context) {
		world := newFormat2UsageWorld(t, ctx)
		held := world.network()
		twin := world.network()
		for _, network := range []*format2UsageNetwork{held, twin} {
			network.listing.SegmentByteCount = 8000
		}
		world.night("the night before", held, twin)

		// the held network's job moves d1 and d2 to held: they stay listed, kept only by the hold; the twin's deletes them
		held.listing.HeldOnlyByteCount += 2000
		world.report("the move to held", world.dropBatch(connect.NewId(), "", "cursor 1", held, -2000), 1, held)
		twin.listing.SegmentByteCount -= 2000
		world.report("the twin's deletes", world.dropBatch(connect.NewId(), "", "cursor 1", twin, -2000), 1, twin)
		world.target.Clock.Advance(24 * time.Hour)
		world.night("a night under the hold", held, twin)
		if held.count != twin.count {
			t.Fatalf("held %d, twin %d: the hold shows in the count", held.count, twin.count)
		}

		// the hold ends and the job DELETEs the held objects, reporting nothing
		held.listing.SegmentByteCount -= 2000
		held.listing.HeldOnlyByteCount -= 2000
		world.check("the DELETEs after the hold", held, twin)
		world.target.Clock.Advance(24 * time.Hour)
		world.night("the night after the hold", held, twin)
		if held.count != twin.count {
			t.Fatalf("held %d, twin %d after the hold", held.count, twin.count)
		}
		world.settled("the run")
	})
}

// evs_drops_per_day through ServiceHost.Quota.CountRate (§11.7's refused row; D-QUOTA-12): drops count a network and
// UTC day under their push ids; past the limit a drop gets 429 naming the key, retry_after_ms to 00:00 UTC and, below
// Team, an upgrade, counting nothing, and the same push id asked again is judged afresh, passing once the day turns.
// A cancel never asks (evs's countDrop), and the refusal stops nothing else of the network. Dark while
// enforce_sidecar_limits is off: neither counted nor refused.
func TestFormat2UsageT3DropsPerDayThroughCountRate(t *testing.T) {
	format2UsageRun(t, 1<<30, func(ctx context.Context) {
		world := newFormat2UsageWorld(t, ctx)
		clock := world.target.Clock
		midnight := clock.Now().UTC().Truncate(24 * time.Hour).Add(48 * time.Hour)
		clock.AdvanceTo(midnight.Add(-10 * time.Minute))
		networkId := connect.NewId()
		if err := world.target.SetLimits(networkId, &sidecar.QuotaLimits{Tier: "free", Limits: map[string]int64{"evs_drops_per_day": 2, "evs_drop_previews_per_day": 5}}); err != nil {
			t.Fatal(err)
		}
		drop := func(pushId connect.Id) *sidecar.QuotaRefusal {
			t.Helper()
			refusal, err := world.target.Quota.CountRate(ctx, networkId, map[string]int64{sidecar.QuotaRateKeyDrop: 1}, pushId)
			if err != nil {
				t.Fatal(err)
			}
			return refusal
		}
		first, second, third := connect.NewId(), connect.NewId(), connect.NewId()
		if drop(first) != nil || drop(second) != nil {
			t.Fatal("the day's two drops refused")
		}
		// a resent push's drop counts nothing
		if refusal := drop(first); refusal != nil {
			t.Fatalf("a resent drop refused: %+v", refusal)
		}
		refusal := drop(third)
		if refusal == nil || refusal.LimitKey != "evs_drops_per_day" || refusal.Limit != 2 || refusal.Used != 2 || !refusal.Upgrade ||
			refusal.RetryAfterMs != uint32((10*time.Minute).Milliseconds()) {
			t.Fatalf("past the day's drops: %+v", refusal)
		}
		if refusal := drop(third); refusal == nil || refusal.Used != 2 {
			t.Fatalf("the refused drop asked again: %+v", refusal)
		}
		// the network's other rates pass while its drops refuse
		preview, err := world.target.Quota.CountRate(ctx, networkId, map[string]int64{sidecar.QuotaRateKeyDropPreview(connect.NewId()): 1}, connect.NewId())
		if err != nil || preview != nil {
			t.Fatalf("a preview while drops refuse: %+v, %v", preview, err)
		}
		// 00:00 UTC: the same push's drop passes
		clock.AdvanceTo(midnight)
		if refusal := drop(third); refusal != nil {
			t.Fatalf("the drop after 00:00 UTC: %+v", refusal)
		}

		// a Team network's refusal names no upgrade: no higher tier lifts it
		teamId := connect.NewId()
		if err := world.target.SetLimits(teamId, &sidecar.QuotaLimits{Tier: "team", Limits: map[string]int64{"evs_drops_per_day": 1}}); err != nil {
			t.Fatal(err)
		}
		if refusal, err := world.target.Quota.CountRate(ctx, teamId, map[string]int64{sidecar.QuotaRateKeyDrop: 1}, connect.NewId()); err != nil || refusal != nil {
			t.Fatalf("a Team network's first drop: %+v, %v", refusal, err)
		}
		if refusal, err := world.target.Quota.CountRate(ctx, teamId, map[string]int64{sidecar.QuotaRateKeyDrop: 1}, connect.NewId()); err != nil || refusal == nil || refusal.Upgrade {
			t.Fatalf("a Team network past its drops: %+v, %v", refusal, err)
		}

		// dark: with the switch off, drops neither count nor refuse, and nothing reaches the network's counter
		darkId := connect.NewId()
		if err := world.target.SetLimits(darkId, &sidecar.QuotaLimits{Tier: "free", Limits: map[string]int64{"evs_drops_per_day": 1}}); err != nil {
			t.Fatal(err)
		}
		func() {
			defer model.Testing_SetEnforceSidecarLimits(false)()
			for range 3 {
				if refusal, err := world.target.Quota.CountRate(ctx, darkId, map[string]int64{sidecar.QuotaRateKeyDrop: 1}, connect.NewId()); err != nil || refusal != nil {
					t.Fatalf("a dark drop: %+v, %v", refusal, err)
				}
			}
		}()
		if format2UsageRateCounted(t, ctx, darkId, sidecar.QuotaRateKeyDrop) {
			t.Fatal("a dark drop reached the network's counter")
		}
		if refusal, err := world.target.Quota.CountRate(ctx, darkId, map[string]int64{sidecar.QuotaRateKeyDrop: 1}, connect.NewId()); err != nil || refusal != nil {
			t.Fatalf("the first drop counted once the switch is on: %+v, %v", refusal, err)
		}
	})
}

// Whether {sq_<network_id>}rate:<key>, the counter quota_rate_script.go keeps for key, holds anything.
func format2UsageRateCounted(t *testing.T, ctx context.Context, networkId connect.Id, key string) bool {
	t.Helper()
	exists := int64(0)
	server.Redis(ctx, func(client server.RedisClient) {
		var err error
		exists, err = client.Exists(ctx, "{sq_"+server.Id(networkId).String()+"}rate:"+key).Result()
		server.Raise(err)
	})
	return 0 < exists
}

// evs_drop_previews_per_day through ServiceHost.Quota.CountRate (§11.13's preview row): a repo's dry runs a UTC day,
// each repo apart, the same on every tier, so a refusal names no upgrade.
func TestFormat2UsageT3DropPreviewsPerRepoAndDay(t *testing.T) {
	format2UsageRun(t, 1<<30, func(ctx context.Context) {
		world := newFormat2UsageWorld(t, ctx)
		clock := world.target.Clock
		midnight := clock.Now().UTC().Truncate(24 * time.Hour).Add(48 * time.Hour)
		clock.AdvanceTo(midnight.Add(-time.Hour))
		networkId := connect.NewId()
		if err := world.target.SetLimits(networkId, &sidecar.QuotaLimits{Tier: "free", Limits: map[string]int64{"evs_drop_previews_per_day": 1}}); err != nil {
			t.Fatal(err)
		}
		preview := func(repoId connect.Id) *sidecar.QuotaRefusal {
			t.Helper()
			refusal, err := world.target.Quota.CountRate(ctx, networkId, map[string]int64{sidecar.QuotaRateKeyDropPreview(repoId): 1}, connect.NewId())
			if err != nil {
				t.Fatal(err)
			}
			return refusal
		}
		repoA, repoB := connect.NewId(), connect.NewId()
		if preview(repoA) != nil {
			t.Fatal("repo A's first preview refused")
		}
		refusal := preview(repoA)
		if refusal == nil || refusal.LimitKey != "evs_drop_previews_per_day" || refusal.Limit != 1 || refusal.Used != 1 || refusal.Upgrade ||
			refusal.RetryAfterMs != uint32(time.Hour.Milliseconds()) {
			t.Fatalf("repo A's second preview: %+v", refusal)
		}
		if refusal := preview(repoB); refusal != nil {
			t.Fatalf("repo B's preview counted on repo A's: %+v", refusal)
		}
		clock.AdvanceTo(midnight)
		if refusal := preview(repoA); refusal != nil {
			t.Fatalf("repo A's preview after 00:00 UTC: %+v", refusal)
		}
	})
}

// When the last of objectCount closure uploads of byteCount each commits, the allowance a UTC day having refused at
// now: §11.13's wait row as the eraser's device states it (sidecar's indexEraseFinish), whole uploads a day from the
// next 00:00 UTC up to the allowance, plus a pacing turn for each upload of the last day.
func fs112T3Finish(now time.Time, objectCount int, byteCount int64, allowance int64, turn time.Duration) time.Time {
	perDay := max(1, int(allowance/byteCount))
	days := (objectCount + perDay - 1) / perDay
	lastDay := objectCount - (days-1)*perDay
	midnight := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	return midnight.Add(time.Duration(days-1)*24*time.Hour + time.Duration(lastDay)*turn)
}

// FS-112's inputs at the api (§20.7's closure, allow and wait rows; owner decision 146): an eraser at
// evs_closure_bytes_floor, its configured allowance below it, erases eight full i/ re-keys starting 10 min before
// 00:00 UTC. Each ?closure=1 upload counts its bytes and its turn; the pacing's 429 names its seat; the allowance's
// names evs_closure_bytes_per_day at the floor, what the eraser used, and retry_after_ms to the next 00:00 UTC. The
// finish the eraser computes from that first refusal names the UTC day and turn at which its last upload passes on
// the fake clock, within one turn. A restart mid-run that re-asks an upload already counted counts nothing.
func TestFormat2UsageT3Fs112ClosureNumbersGiveTheErasersFinish(t *testing.T) {
	format2UsageRun(t, 1<<30, func(ctx context.Context) {
		world := newFormat2UsageWorld(t, ctx)
		clock := world.target.Clock
		midnight := clock.Now().UTC().Truncate(24 * time.Hour).Add(48 * time.Hour)
		clock.AdvanceTo(midnight.Add(-10 * time.Minute))
		networkId := connect.NewId()
		floor := int64(4 * 1024 * 1024)
		if err := world.target.SetLimits(networkId, &sidecar.QuotaLimits{Tier: "free", Limits: map[string]int64{
			"evs_closure_bytes_per_day":   1024 * 1024,
			"evs_closure_bytes_floor":     floor,
			"evs_closure_uploads_per_s":   4,
			"evs_closure_uploads_per_min": 60,
		}}); err != nil {
			t.Fatal(err)
		}
		eraserId := connect.NewId()
		uploadByteCount := int64(sidecar.QuotaClosureUploadMaxByteCount)
		turn := 250 * time.Millisecond
		ask := func(debitId connect.Id) *sidecar.QuotaRefusal {
			t.Helper()
			refusal, err := world.target.Quota.CountRate(ctx, networkId, map[string]int64{
				sidecar.QuotaRateKeyClosureUpload(eraserId): 1,
				sidecar.QuotaRateKeyClosureBytes(eraserId):  uploadByteCount,
			}, debitId)
			if err != nil {
				t.Fatal(err)
			}
			return refusal
		}

		const objectCount = 8
		var finish time.Time
		var lastPass time.Time
		allowanceRefusals := 0
		for object := range objectCount {
			debitId := connect.NewId()
			for {
				refusal := ask(debitId)
				if refusal == nil {
					lastPass = clock.Now()
					break
				}
				switch refusal.LimitKey {
				case "evs_closure_uploads_per_s":
					// its turn: the pacing names its seat, at most a turn away with one eraser asking
					if refusal.RetryAfterMs == 0 || uint32(turn.Milliseconds()) < refusal.RetryAfterMs || refusal.Limit != 4 {
						t.Fatalf("object %d's pacing refusal: %+v", object, refusal)
					}
				case "evs_closure_bytes_per_day":
					nextMidnight := clock.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
					if refusal.Limit != floor || refusal.Used != 3*uploadByteCount || refusal.Upgrade ||
						!clock.Now().Add(time.Duration(refusal.RetryAfterMs)*time.Millisecond).Equal(nextMidnight) {
						t.Fatalf("object %d's allowance refusal at %s: %+v", object, clock.Now(), refusal)
					}
					if allowanceRefusals == 0 {
						finish = fs112T3Finish(clock.Now(), objectCount-object, uploadByteCount, refusal.Limit, turn)
					}
					allowanceRefusals += 1
				default:
					t.Fatalf("object %d refused by %+v", object, refusal)
				}
				clock.Advance(time.Duration(refusal.RetryAfterMs) * time.Millisecond)
			}
			if object == 4 {
				// the eraser restarts and re-asks the upload whose answer it lost: counted already, it counts nothing
				if refusal := ask(debitId); refusal != nil {
					t.Fatalf("the restarted eraser's replay: %+v", refusal)
				}
			}
		}
		if allowanceRefusals != 2 {
			t.Fatalf("%d allowance refusals over two UTC days of three uploads", allowanceRefusals)
		}
		if lastPass.Before(midnight.Add(24*time.Hour)) || lastPass.Sub(finish).Abs() > turn {
			t.Fatalf("the last upload passed at %s, the eraser's finish %s", lastPass, finish)
		}
	})
}
