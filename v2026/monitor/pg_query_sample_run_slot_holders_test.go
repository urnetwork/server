// Synthetic signal owners exercise the real pool and immutable receipt handoff.
// The independent wire decoder also compiles against the pre-observation source.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Caller keys can be adversarial; only registry-owned spellings may persist.
type pgHolderSignalControl struct {
	Signal
	key     string
	entered chan struct{}
	finish  chan struct{}
}

// Use a caller-controlled override only for the privacy control.
func (self *pgHolderSignalControl) Key() string {
	if self.key != "" {
		return self.key
	}
	return self.Signal.Key()
}

// A finite test barrier holds an actual Signal.Run owner until explicit release.
func (self *pgHolderSignalControl) Run(ctx context.Context, _ SignalSettings) (Alerts, error) {
	close(self.entered)
	select {
	case <-ctx.Done():
	case <-self.finish:
	}
	return nil, nil
}

// Join before returning the release so the next snapshot cannot race cleanup.
func startPgHolderControl(t *testing.T, slots *runSlotPool, signal Signal, key string) func() {
	t.Helper()
	owner := &pgHolderSignalControl{Signal: signal, key: key, entered: make(chan struct{}), finish: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := runSignalInSlot(t.Context(), slots, owner, syntheticSettings(nil))
		done <- err
	}()
	<-owner.entered
	var once sync.Once
	return func() {
		once.Do(func() {
			close(owner.finish)
			if err := <-done; err != nil {
				t.Error("synthetic holder failed its owned signal turn")
			}
		})
	}
}

// Decode only the documented public-key/age projection, independently of its
// production Go types. Missing observations must reach a causal assertion.
type pgHolderWire struct {
	Schema      int                   `json:"schema"`
	Enqueued    *pgHolderSnapshotWire `json:"enqueued"`
	BeforeGrant *pgHolderSnapshotWire `json:"before_grant"`
}

// Null ages and offsets are distinct from known zero elapsed time.
type pgHolderSnapshotWire struct {
	Status     string              `json:"status"`
	CapturedNs *int64              `json:"captured_ns"`
	Active     int                 `json:"active"`
	Holders    []pgHolderEntryWire `json:"holders"`
}

// Only a registry key or the fixed unknown spelling belongs here.
type pgHolderEntryWire struct {
	SignalKey  string `json:"signal_key"`
	GrantAgeNs *int64 `json:"grant_age_ns"`
}

// Accept a real receipt body or the same private observation before persistence.
func readPgHolderWire(t *testing.T, raw []byte) *pgHolderWire {
	t.Helper()
	var envelope struct {
		Transport struct {
			Holders *pgHolderWire `json:"run_slot_holders"`
		} `json:"transport_observation"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		t.Fatal("holder projection is not valid JSON")
	}
	return envelope.Transport.Holders
}

// Serialize the exact canceled request without manufacturing a durable receipt.
func readPgHolderObservation(t *testing.T, observation *pgSampleTransportObservation) *pgHolderWire {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"transport_observation": observation.receipt})
	if err != nil {
		t.Fatal("holder observation did not encode")
	}
	return readPgHolderWire(t, raw)
}

// The enqueue snapshot contains four distinct real owners. After one returns,
// the pre-grant snapshot contains the other three and excludes the new sampler.
func TestPgRunSlotHoldersRetainExactOwnersAndGrantAges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slots := newRunSlotPool(runLoopMaxConcurrentSignals)
		owners := []Signal{NewHostLoadSignal(), NewHostPressureSignal(), NewPgCapacitySignal(), NewRedisMemorySignal()}
		var releases []func()
		for _, owner := range owners {
			release := startPgHolderControl(t, slots, owner, "")
			defer release()
			releases = append(releases, release)
			time.Sleep(time.Second)
		}
		signal, calls := pgRunSlotTimingFixture(t)
		done := make(chan error, 1)
		go func() {
			_, err := runSignalInSlot(t.Context(), slots, signal, syntheticSettings(nil))
			done <- err
		}()
		synctest.Wait()
		if slots.active != 4 || len(slots.waiting) != 1 || *calls != 0 {
			t.Fatal("fixture did not retain four owners before the sampler queued")
		}
		time.Sleep(5 * time.Second)
		releases[1]()
		if err := <-done; err != nil {
			t.Fatal("sampler did not retain its real receipt after grant")
		}
		control, raw := pgTransportReadControlReceipt(t, signal.env)
		if !control.Complete || control.Samples != 12 || *calls != 1 || slots.active != 3 {
			t.Fatal("holder observation changed source completion or retained permits")
		}
		wire := readPgHolderWire(t, raw)
		if wire == nil || wire.Schema != 1 || wire.Enqueued == nil || wire.BeforeGrant == nil {
			t.Fatal("PG receipt omitted the actual global-holder snapshots")
		}
		if wire.Enqueued.Status != "ok" || wire.Enqueued.Active != 4 || len(wire.Enqueued.Holders) != 4 || wire.Enqueued.CapturedNs == nil || *wire.Enqueued.CapturedNs != 0 || wire.BeforeGrant.Status != "ok" || wire.BeforeGrant.Active != 3 || len(wire.BeforeGrant.Holders) != 3 || wire.BeforeGrant.CapturedNs == nil || *wire.BeforeGrant.CapturedNs != (5*time.Second).Nanoseconds() {
			t.Fatal("holder snapshot lost its exact bounded capture boundary")
		}
		for i, owner := range owners {
			holder := wire.Enqueued.Holders[i]
			if holder.SignalKey != owner.Key() || holder.GrantAgeNs == nil || *holder.GrantAgeNs != (time.Duration(4-i)*time.Second).Nanoseconds() {
				t.Fatal("enqueue projection did not identify the actual grant and its age")
			}
		}
		for i, original := range []int{0, 2, 3} {
			holder := wire.BeforeGrant.Holders[i]
			if holder.SignalKey != owners[original].Key() || holder.GrantAgeNs == nil || *holder.GrantAgeNs != (time.Duration(9-original)*time.Second).Nanoseconds() {
				t.Fatal("pre-grant projection retained the released owner or lost a live one")
			}
		}
		// Releasing later owners must not mutate either already captured slice.
		for _, release := range releases {
			release()
		}
		_, after := pgTransportReadControlReceipt(t, signal.env)
		if string(after) != string(raw) || slots.active != 0 {
			t.Fatal("later release changed immutable evidence or leaked capacity")
		}
	})
}

// Exact grant/cancel overlap is forced inside the existing attempt clock, after
// granted=true and before ready closes. Either select arm must return only this
// sampler's permit; the three prior owners remain visible in a later receipt.
func TestPgRunSlotHoldersGrantCancellationRemovesOnlyItsOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slots := newRunSlotPool(runLoopMaxConcurrentSignals)
		var releases []func()
		for _, owner := range []Signal{NewHostLoadSignal(), NewHostPressureSignal(), NewPgCapacitySignal(), NewRedisMemorySignal()} {
			release := startPgHolderControl(t, slots, owner, "")
			defer release()
			releases = append(releases, release)
		}
		signal, calls := pgRunSlotTimingFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := runSignalInSlot(ctx, slots, signal, syntheticSettings(nil))
			done <- err
		}()
		synctest.Wait()
		if len(slots.waiting) != 1 {
			t.Fatal("fixture did not queue the cancellable request")
		}
		waiter := slots.waiting[0]
		observation := pgSampleTransportFromContext(waiter.ctx)
		forced := false
		observation.localClock = func() time.Time {
			if waiter.granted {
				forced = true
				cancel()
			}
			return time.Now()
		}
		releases[0]()
		if err := <-done; !errors.Is(err, context.Canceled) || !forced || *calls != 0 || slots.active != 3 || len(slots.waiting) != 0 {
			t.Fatal("forced grant/cancel overlap lost another owner's capacity")
		}
		wire := readPgHolderObservation(t, observation)
		if wire == nil || wire.BeforeGrant == nil || wire.BeforeGrant.Active != 3 || len(wire.BeforeGrant.Holders) != 3 {
			t.Fatal("canceled grant lost its real pre-grant snapshot")
		}
		next, _ := pgRunSlotTimingFixture(t)
		if _, err := runSignalInSlot(t.Context(), slots, next, syntheticSettings(nil)); err != nil {
			t.Fatal("capacity was not available after joined cancellation")
		}
		_, raw := pgTransportReadControlReceipt(t, next.env)
		nextWire := readPgHolderWire(t, raw)
		if nextWire == nil || nextWire.Enqueued.Active != 3 || len(nextWire.Enqueued.Holders) != 3 {
			t.Fatal("canceled owner survived into the next request")
		}
		for _, holder := range nextWire.Enqueued.Holders {
			if holder.SignalKey == NewPgQuerySampleSignal().Key() || holder.SignalKey == NewHostLoadSignal().Key() {
				t.Fatal("released grant was attributed to a later request")
			}
		}
	})
}

// Cancellation before any release preserves a complete enqueue snapshot and
// a missing pre-grant snapshot. It creates no new completed or sampled turn.
func TestPgRunSlotHoldersUnadmittedCancellationKeepsGrantUnknown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slots := newRunSlotPool(1)
		release := startPgHolderControl(t, slots, NewHostLoadSignal(), "")
		defer release()
		signal, calls := pgRunSlotTimingFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := runSignalInSlot(ctx, slots, signal, syntheticSettings(nil)); done <- err }()
		synctest.Wait()
		if len(slots.waiting) != 1 {
			t.Fatal("fixture did not queue its request")
		}
		observation := pgSampleTransportFromContext(slots.waiting[0].ctx)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) || *calls != 0 {
			t.Fatal("unadmitted cancellation reached source execution")
		}
		wire := readPgHolderObservation(t, observation)
		if wire == nil || wire.Enqueued == nil || wire.Enqueued.Active != 1 || wire.BeforeGrant != nil || slots.active != 1 {
			t.Fatal("unadmitted request invented a grant or removed the prior owner")
		}
	})
}

// Even syntactically plausible caller labels must be catalog members. An
// arbitrarily long key and a private-looking ordinary key both become unknown.
func TestPgRunSlotHoldersWithholdUnregisteredCallerKeys(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slots := newRunSlotPool(runLoopMaxConcurrentSignals)
		for _, key := range []string{"private-holder-canary", strings.Repeat("private-oversized-canary", 100)} {
			release := startPgHolderControl(t, slots, NewHostLoadSignal(), key)
			defer release()
		}
		signal, _ := pgRunSlotTimingFixture(t)
		if _, err := runSignalInSlot(t.Context(), slots, signal, syntheticSettings(nil)); err != nil {
			t.Fatal("unknown-key control did not complete")
		}
		_, raw := pgTransportReadControlReceipt(t, signal.env)
		wire := readPgHolderWire(t, raw)
		if wire == nil || len(wire.Enqueued.Holders) != 2 || len(wire.BeforeGrant.Holders) != 2 || strings.Contains(string(raw), "private-holder") || strings.Contains(string(raw), "private-oversized") {
			t.Fatal("caller-controlled label escaped the fixed holder projection")
		}
		for _, snapshot := range []*pgHolderSnapshotWire{wire.Enqueued, wire.BeforeGrant} {
			for _, holder := range snapshot.Holders {
				if holder.SignalKey != "unknown" || holder.GrantAgeNs == nil {
					t.Fatal("unknown identity became a public key or lost known ownership")
				}
			}
		}
	})
}

// Direct observations do not participate in the global pool and cannot claim
// an empty pool, enqueue, grant or holder ages from their normal 40s budget.
func TestPgRunSlotHoldersDirectProbeKeepsSnapshotsAbsent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		signal, _ := pgRunSlotTimingFixture(t)
		if _, err := signal.Run(t.Context(), syntheticSettings(nil)); err != nil {
			t.Fatal("direct control did not complete")
		}
		_, raw := pgTransportReadControlReceipt(t, signal.env)
		if readPgHolderWire(t, raw) != nil || strings.Contains(string(raw), "run_slot_holders") {
			t.Fatal("direct probe invented global holder authority")
		}
	})
}

// A custom larger pool keeps its own existing admission behavior, but the
// fixed four-holder projection must not silently truncate known occupancy.
func TestPgRunSlotHoldersUnsupportedOccupancyStaysUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slots := newRunSlotPool(5)
		var releases []func()
		for range 5 {
			release := startPgHolderControl(t, slots, NewHostLoadSignal(), "")
			defer release()
			releases = append(releases, release)
		}
		signal, _ := pgRunSlotTimingFixture(t)
		done := make(chan error, 1)
		go func() { _, err := runSignalInSlot(t.Context(), slots, signal, syntheticSettings(nil)); done <- err }()
		synctest.Wait()
		if len(slots.waiting) != 1 || slots.active != 5 {
			t.Fatal("fixture did not reach the custom pool's existing capacity")
		}
		releases[0]()
		if err := <-done; err != nil || slots.active != 4 {
			t.Fatal("diagnostic bound changed the custom pool's admission")
		}
		_, raw := pgTransportReadControlReceipt(t, signal.env)
		wire := readPgHolderWire(t, raw)
		if wire == nil || wire.Enqueued.Status != "unavailable" || wire.Enqueued.Active != 5 || wire.Enqueued.Holders != nil || wire.BeforeGrant.Status != "unavailable" || wire.BeforeGrant.Holders != nil {
			t.Fatal("incomplete bounded bookkeeping became a complete holder census")
		}
	})
}

// A regressing injected observation clock must not become negative grant ages
// or a healthy empty snapshot. The original enqueue facts remain unchanged.
func TestPgRunSlotHoldersInvalidClockRetainsOccupancyWithoutAges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slots := newRunSlotPool(2)
		release := startPgHolderControl(t, slots, NewHostLoadSignal(), "")
		defer release()
		otherRelease := startPgHolderControl(t, slots, NewHostPressureSignal(), "")
		defer otherRelease()
		signal, _ := pgRunSlotTimingFixture(t)
		done := make(chan error, 1)
		go func() { _, err := runSignalInSlot(t.Context(), slots, signal, syntheticSettings(nil)); done <- err }()
		synctest.Wait()
		if len(slots.waiting) != 1 {
			t.Fatal("fixture did not queue its clock-controlled request")
		}
		observation := pgSampleTransportFromContext(slots.waiting[0].ctx)
		observation.localClock = func() time.Time { return observation.runSlotRequestedAt.Add(-time.Nanosecond) }
		release()
		if err := <-done; err != nil {
			t.Fatal("diagnostic clock altered source execution")
		}
		_, raw := pgTransportReadControlReceipt(t, signal.env)
		wire := readPgHolderWire(t, raw)
		if wire == nil || wire.Enqueued.Status != "ok" || wire.BeforeGrant == nil || wire.BeforeGrant.Status != "clock_invalid" || wire.BeforeGrant.CapturedNs != nil || wire.BeforeGrant.Active != 1 || len(wire.BeforeGrant.Holders) != 1 {
			t.Fatal("invalid observation clock became authoritative holder timing")
		}
		for _, holder := range wire.BeforeGrant.Holders {
			if holder.GrantAgeNs != nil {
				t.Fatal("invalid snapshot retained a grant age")
			}
		}
	})
}

// Worst-size fixed labels and durations fit the existing receipt cap. Test the
// owning encoder too: added metadata must never bypass its total byte bound.
func TestPgRunSlotHoldersFixedSchemaFitsExistingReceiptBound(t *testing.T) {
	longest := ""
	for _, signal := range NewSignals() {
		if len(signal.Key()) > len(longest) {
			longest = signal.Key()
		}
	}
	if len(longest) == 0 || len(longest) > 64 {
		t.Fatal("catalog keys exceeded the reviewed public-label bound")
	}
	maximum := int64(9223372036854775807)
	snapshot := &pgHolderSnapshotWire{Status: "ok", CapturedNs: &maximum, Active: 4, Holders: []pgHolderEntryWire{}}
	for range 4 {
		snapshot.Holders = append(snapshot.Holders, pgHolderEntryWire{SignalKey: longest, GrantAgeNs: &maximum})
	}
	wire := &pgHolderWire{Schema: 1, Enqueued: snapshot, BeforeGrant: snapshot}
	projection, err := json.MarshalIndent(wire, "", "  ")
	if err != nil || len(projection) > 4096 {
		t.Fatal("fixed holder projection exceeded its finite encoding bound")
	}
	raw, err := json.Marshal(map[string]any{"schema": 1, "transport_observation": map[string]any{"run_slot_holders": wire}})
	var receipt pgQuerySampleReceipt
	if err != nil || json.Unmarshal(raw, &receipt) != nil {
		t.Fatal("holder receipt fixture did not decode")
	}
	public, private, err := pgSampleEncodeReceipts(&receipt)
	if err != nil || len(public)+len(private) > pgSampleReceiptBytes || readPgHolderWire(t, public) == nil {
		t.Fatal("holder metadata was lost or exceeded the existing receipt byte cap")
	}
	receipt.Qualifiers = []string{strings.Repeat("x", pgSampleReceiptBytes)}
	if _, _, err := pgSampleEncodeReceipts(&receipt); err == nil {
		t.Fatal("holder metadata bypassed the unchanged total receipt cap")
	}
}
