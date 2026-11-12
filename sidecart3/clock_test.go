package sidecart3

// The store clock (clock.go): its translation held on a fake store whose own clock is a second fake, so the
// schedule's clock and the store's part as they do on T3; then, under sidecar_localstack, on the local stack's
// MinIO and the private Redis (clock_store_test.go).

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// A schedule's Net and a store on a clock of its own, with the store clock between them.
type storeClockFixture struct {
	net        *sidecartest.Net
	storeClock *sidecartest.FakeClock
	store      *sidecartest.FakeBlobs
	clock      *StoreClock
	cluster    sidecar.BlobCluster
}

func newStoreClockFixture(t *testing.T) *storeClockFixture {
	t.Helper()
	net := sidecartest.NewNet(t.Context(), nil)
	t.Cleanup(net.Close)
	// the store's clock runs months past the schedule's, as real time runs past DefaultClockStart
	storeClock := sidecartest.NewFakeClock(net.Clock().Now().Add(311 * 24 * time.Hour))
	store := sidecartest.NewFakeBlobs(storeClock, sidecartest.NewFaultQueue(), nil, nil)
	clock := newStoreClock(net, DefaultStoreClockSettings(), storeClock)
	netClockNets.Store(net, &netClocks{store: storeClock, storeClock: clock})
	t.Cleanup(func() { netClockNets.Delete(net) })
	cluster, err := (&storeClockBlobs{blobs: store, clock: clock}).FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	return &storeClockFixture{net: net, storeClock: storeClock, store: store, clock: clock, cluster: cluster}
}

// What the store clock says key was last modified, through a Head.
func (self *storeClockFixture) lastModified(t *testing.T, key string) time.Time {
	t.Helper()
	info, err := self.cluster.Head(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return info.LastModified
}

// An object written before a 25 h Advance reads 25 h old and one written after it reads new, through Head, Get, a
// ranged Get and List, while the store's own clock moved a second.
func TestStoreClockReadsAgesOnTheScheduleClock(t *testing.T) {
	fixture := newStoreClockFixture(t)
	ctx := t.Context()
	start := fixture.net.Clock().Now()
	if _, err := fixture.cluster.PutIfNoneMatch(ctx, "head/before/_", []byte("before"), nil); err != nil {
		t.Fatal(err)
	}
	fixture.net.Clock().Advance(25 * time.Hour)
	fixture.storeClock.Advance(time.Second)
	if _, err := fixture.cluster.PutIfNoneMatch(ctx, "head/after/_", []byte("after"), nil); err != nil {
		t.Fatal(err)
	}
	now := fixture.net.Clock().Now()
	if got := fixture.lastModified(t, "head/before/_"); !got.Equal(start) || now.Sub(got) != 25*time.Hour {
		t.Errorf("the object written before reads %s, %s old", got, now.Sub(got))
	}
	if got := fixture.lastModified(t, "head/after/_"); !got.Equal(now) {
		t.Errorf("the object written after reads %s, not %s", got, now)
	}
	object, err := fixture.cluster.Get(ctx, "head/before/_")
	if err != nil {
		t.Fatal(err)
	}
	object.Body.Close()
	if !object.LastModified.Equal(start) {
		t.Errorf("Get reads %s", object.LastModified)
	}
	ranged, err := fixture.cluster.GetRange(ctx, "head/after/_", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	ranged.Body.Close()
	if !ranged.LastModified.Equal(now) {
		t.Errorf("GetRange reads %s", ranged.LastModified)
	}
	objects, _, err := fixture.cluster.List(ctx, "head/", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 || !objects[0].LastModified.Equal(now) || !objects[1].LastModified.Equal(start) {
		t.Errorf("List reads %+v", objects)
	}
	if untranslated := fixture.clock.Untranslated(); len(untranslated) != 0 {
		t.Errorf("untranslated %q", untranslated)
	}
	if writeTime, ok := fixture.clock.WriteTime(fixture.cluster.ClusterId(), "head/before/_"); !ok || !writeTime.Equal(start) {
		t.Errorf("WriteTime %s, %t", writeTime, ok)
	}
}

// An overwrite reads at its own time, and a refused write records none: a 412 leaves the object's time as it was.
func TestStoreClockOverwritesTakeTheirOwnTime(t *testing.T) {
	fixture := newStoreClockFixture(t)
	ctx := t.Context()
	etag, err := fixture.cluster.PutIfNoneMatch(ctx, "head/a/_", []byte("v1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture.net.Clock().Advance(time.Hour)
	if _, err := fixture.cluster.PutIfMatch(ctx, "head/a/_", "stale", []byte("refused"), nil); !errors.Is(err, sidecar.ErrBlobPrecondition) {
		t.Fatalf("a stale If-Match: %v", err)
	}
	if got := fixture.lastModified(t, "head/a/_"); !got.Equal(fixture.net.Clock().Now().Add(-time.Hour)) {
		t.Errorf("after a refused write the object reads %s", got)
	}
	fixture.net.Clock().Advance(time.Hour)
	if _, err := fixture.cluster.PutIfMatch(ctx, "head/a/_", etag, []byte("v2"), nil); err != nil {
		t.Fatal(err)
	}
	if got := fixture.lastModified(t, "head/a/_"); !got.Equal(fixture.net.Clock().Now()) {
		t.Errorf("the overwrite reads %s", got)
	}
}

// A write whose answer was lost is recorded with no ETag, and the first read showing its object takes the write's
// time, so an unknown outcome that landed ages from when it was sent.
func TestStoreClockGivesALostAnswerItsTime(t *testing.T) {
	fixture := newStoreClockFixture(t)
	ctx := t.Context()
	if _, err := fixture.store.Faults().Add(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, KeyPrefix: "head/lost/", Result: sidecartest.ApplyThenDrop}); err != nil {
		t.Fatal(err)
	}
	sent := fixture.net.Clock().Now()
	var unknown *sidecar.BlobUnknownError
	if _, err := fixture.cluster.PutIfNoneMatch(ctx, "head/lost/_", []byte("landed"), nil); !errors.As(err, &unknown) {
		t.Fatalf("the lost answer: %v", err)
	}
	fixture.net.Clock().Advance(26 * time.Hour)
	if got := fixture.lastModified(t, "head/lost/_"); !got.Equal(sent) {
		t.Errorf("the write that landed reads %s, not %s", got, sent)
	}
	if untranslated := fixture.clock.Untranslated(); len(untranslated) != 0 {
		t.Errorf("untranslated %q", untranslated)
	}
}

// An object no write through the store clock made keeps its age on the store's own clock, and is listed, so a run
// that read it fails rather than passing on an age the schedule never set.
func TestStoreClockListsAReadItCannotTranslate(t *testing.T) {
	fixture := newStoreClockFixture(t)
	ctx := t.Context()
	direct, err := fixture.store.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := direct.PutIfNoneMatch(ctx, "head/direct/_", []byte("direct"), nil); err != nil {
		t.Fatal(err)
	}
	fixture.storeClock.Advance(3 * time.Hour)
	if got := fixture.lastModified(t, "head/direct/_"); !got.Equal(fixture.net.Clock().Now().Add(-3 * time.Hour)) {
		t.Errorf("the object reads %s", got)
	}
	problems := storeClockProblems(fixture.net)
	if len(problems) != 1 || !strings.Contains(problems[0], "head/direct/_") {
		t.Errorf("problems %q", problems)
	}
}

// A DELETE naming a LastModified is judged on the schedule's clock: naming the object an overwrite two hours later
// replaced is refused though the store's own clock moved under a second, and naming the newest deletes it. A key
// the wrapper never wrote takes the time named, as its age on the store's clock.
func TestStoreClockJudgesANamedDeleteOnTheScheduleClock(t *testing.T) {
	fixture := newStoreClockFixture(t)
	ctx := t.Context()
	etag, err := fixture.cluster.PutIfNoneMatch(ctx, "seg/r/s", []byte("v1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	first := fixture.lastModified(t, "seg/r/s")
	fixture.net.Clock().Advance(2 * time.Hour)
	fixture.storeClock.Advance(100 * time.Millisecond)
	if _, err := fixture.cluster.PutIfMatch(ctx, "seg/r/s", etag, []byte("v2"), nil); err != nil {
		t.Fatal(err)
	}
	second := fixture.lastModified(t, "seg/r/s")
	if err := fixture.cluster.Delete(sidecar.WithBlobUnmodified(ctx, first), "seg/r/s"); !errors.Is(err, sidecar.ErrBlobPrecondition) {
		t.Fatalf("a DELETE naming the replaced object: %v", err)
	}
	if err := fixture.cluster.Delete(sidecar.WithBlobUnmodified(ctx, second), "seg/r/s"); err != nil {
		t.Fatalf("a DELETE naming the newest: %v", err)
	}
	if _, err := fixture.cluster.Head(ctx, "seg/r/s"); !errors.Is(err, sidecar.ErrBlobNotFound) {
		t.Errorf("after its DELETE: %v", err)
	}
	if _, ok := fixture.clock.WriteTime(fixture.cluster.ClusterId(), "seg/r/s"); ok {
		t.Error("a deleted key keeps its writes")
	}

	direct, err := fixture.store.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := direct.PutIfNoneMatch(ctx, "seg/r/direct", []byte("direct"), nil); err != nil {
		t.Fatal(err)
	}
	// stored an hour ago by the store's clock: a time named two hours back is before it, half an hour back after it
	fixture.storeClock.Advance(time.Hour)
	before := fixture.net.Clock().Now().Add(-2 * time.Hour)
	if err := fixture.cluster.Delete(sidecar.WithBlobUnmodified(ctx, before), "seg/r/direct"); !errors.Is(err, sidecar.ErrBlobPrecondition) {
		t.Errorf("naming a time before an unrecorded object, by its age: %v", err)
	}
	after := fixture.net.Clock().Now().Add(-30 * time.Minute)
	if err := fixture.cluster.Delete(sidecar.WithBlobUnmodified(ctx, after), "seg/r/direct"); err != nil {
		t.Errorf("naming a time after it: %v", err)
	}
	if _, err := direct.Head(ctx, "seg/r/direct"); !errors.Is(err, sidecar.ErrBlobNotFound) {
		t.Errorf("the unrecorded object after the DELETE that named it: %v", err)
	}
}

// What every T3 request's deadline header carries: the same span ahead of the store's now as of the schedule's.
func TestStoreTimeKeepsADeadlinesSpan(t *testing.T) {
	fixture := newStoreClockFixture(t)
	deadline := fixture.net.Clock().Now().Add(10 * time.Second)
	if got, want := StoreTime(fixture.net, deadline), fixture.storeClock.Now().Add(10*time.Second); !got.Equal(want) {
		t.Errorf("StoreTime %s, want %s", got, want)
	}
	fixture.net.Clock().Advance(time.Hour)
	past := fixture.net.Clock().Now().Add(-time.Second)
	if got, want := StoreTime(fixture.net, past), fixture.storeClock.Now().Add(-time.Second); !got.Equal(want) {
		t.Errorf("a past deadline %s, want %s", got, want)
	}
	if got := StoreTime(&sidecartest.Net{}, deadline); !got.Equal(deadline) {
		t.Errorf("a Net that is no T3 Net's: %s", got)
	}
}

// Advance moves the schedule's clock, and a Net with no store clock has nothing more to age.
func TestAdvanceMovesTheSchedulesClock(t *testing.T) {
	net := sidecartest.NewNet(t.Context(), nil)
	defer net.Close()
	start := net.Clock().Now()
	if err := Advance(context.Background(), net, 25*time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := net.Clock().Now().Sub(start); got != 25*time.Hour {
		t.Errorf("the clock moved %s", got)
	}
	if StoreClockOf(net) != nil {
		t.Error("a Net with no T3 host has a store clock")
	}
}

// FS-114 refuses the store clock: its settle cases run on MinIO's own clock (QD-264). And no case of it is flagged
// store_clock, which would ask for what it refuses.
func TestFs114RefusesTheStoreClock(t *testing.T) {
	catalog := fault.DefaultCatalog()
	for _, testCase := range catalog.Row("FS-114").Cases {
		err := storeClockCaseErr(testCase, true)
		if err == nil || !strings.Contains(err.Error(), "QD-264") || !strings.Contains(err.Error(), "store's own clock") {
			t.Errorf("%s with the store clock: %v", testCase, err)
		}
		if testCase.StoreClock {
			t.Errorf("%s is flagged store_clock", testCase)
		}
		if err := storeClockCaseErr(testCase, false); err != nil {
			t.Errorf("%s without it: %v", testCase, err)
		}
	}
}

// A case runs with the store clock exactly when catalog.yml flags it: a flagged case without it is refused, so it
// cannot pass on real time, and an unflagged one with it is refused, so its gate report cannot leave the
// translation out.
func TestAStoreClockCaseRunsWithTheStoreClockAndOnlyIt(t *testing.T) {
	catalog := fault.DefaultCatalog()
	flagged := catalog.Case("FS-59", "")
	unflagged := catalog.Case("FS-2", "")
	if flagged == nil || !flagged.StoreClock || unflagged == nil || unflagged.StoreClock {
		t.Fatalf("the fixture cases moved: %v %v", flagged, unflagged)
	}
	if err := storeClockCaseErr(flagged, false); err == nil || !strings.Contains(err.Error(), "Settings.StoreClock") {
		t.Errorf("a flagged case without the store clock: %v", err)
	}
	if err := storeClockCaseErr(unflagged, true); err == nil || !strings.Contains(err.Error(), "flag the case") {
		t.Errorf("an unflagged case with it: %v", err)
	}
	if err := storeClockCaseErr(flagged, true); err != nil {
		t.Errorf("a flagged case with it: %v", err)
	}
	if err := storeClockCaseErr(unflagged, false); err != nil {
		t.Errorf("an unflagged case without it: %v", err)
	}
}

// Every case catalog.yml flags store_clock runs on T3, where alone the store clock translates: a flag elsewhere says
// nothing a gate could read.
func TestEveryStoreClockCaseRunsOnT3(t *testing.T) {
	flaggedCount := 0
	for _, row := range fault.DefaultCatalog().Rows {
		for _, testCase := range row.Cases {
			if !testCase.StoreClock {
				continue
			}
			flaggedCount += 1
			if !slices.Contains(testCase.TwinTiers(), fault.TierT3) {
				t.Errorf("%s is flagged store_clock and runs on %v", testCase, testCase.TwinTiers())
			}
		}
	}
	if flaggedCount == 0 {
		t.Error("no case is flagged store_clock")
	}
}

// A streamed PUT is recorded as the others are.
func TestStoreClockRecordsAStreamedPut(t *testing.T) {
	fixture := newStoreClockFixture(t)
	ctx := t.Context()
	segmentCluster, err := (&storeClockBlobs{blobs: fixture.store, clock: fixture.clock}).SegmentCluster()
	if err != nil {
		t.Fatal(err)
	}
	sent := fixture.net.Clock().Now()
	sum := sha256.Sum256([]byte("segment"))
	if _, err := segmentCluster.PutStream(ctx, "seg/r/streamed", io.NopCloser(strings.NewReader("segment")), 7, sum[:]); err != nil {
		t.Fatal(err)
	}
	fixture.net.Clock().Advance(49 * time.Hour)
	info, err := segmentCluster.Head(ctx, "seg/r/streamed")
	if err != nil {
		t.Fatal(err)
	}
	if !info.LastModified.Equal(sent) {
		t.Errorf("the streamed segment reads %s", info.LastModified)
	}
}

// RunRow holds a registered case to its flag before it builds any Net: FS-6, which catalog.yml flags, refused
// without the store clock, and FS-2, which it does not, refused with it.
func TestRunRowHoldsACaseToItsStoreClockFlag(t *testing.T) {
	withClock := DefaultSettings()
	withClock.StoreClock = DefaultStoreClockSettings()
	unflagged := record(t, func(tb testing.TB) {
		RunRow(tb, "FS-2", "", RuleSettings(tb, withClock))
	})
	if !strings.Contains(unflagged.text(), "flag the case") {
		t.Errorf("FS-2 with the store clock: %s", unflagged.text())
	}
	flagged := record(t, func(tb testing.TB) {
		RunRow(tb, "FS-6", "", RuleSettings(tb, nil))
	})
	if !strings.Contains(flagged.text(), "Settings.StoreClock") {
		t.Errorf("FS-6 without the store clock: %s", flagged.text())
	}
}
