package sidecart3

// A world's store clock (NewWorldStoreClock): the translation a Net's store clock makes, on a schedule's clock with no
// Net, held on a fake store whose own clock is a second fake.

import (
	"testing"
	"time"

	"github.com/urnetwork/sidecar/sidecartest"
)

// An object written before a 25 h Advance reads 25 h old and one written after it reads new, while the store's own
// clock moved a second, and the witness is called once.
func TestWorldStoreClockReadsAgesOnTheScheduleClock(t *testing.T) {
	ctx := t.Context()
	schedule := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	// the store's clock runs months past the schedule's, as real time runs past DefaultClockStart
	store := sidecartest.NewFakeClock(schedule.Now().Add(311 * 24 * time.Hour))
	witnessCount := 0
	storeClock, blobs, err := NewWorldStoreClock(schedule, store, DefaultStoreClockSettings(), sidecartest.NewFakeBlobs(store, sidecartest.NewFaultQueue(), nil, nil), func() {
		witnessCount += 1
	})
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	start := schedule.Now()
	if _, err := cluster.PutIfNoneMatch(ctx, "head/before/_", []byte("before"), nil); err != nil {
		t.Fatal(err)
	}
	schedule.Advance(25 * time.Hour)
	store.Advance(time.Second)
	if _, err := cluster.PutIfNoneMatch(ctx, "head/after/_", []byte("after"), nil); err != nil {
		t.Fatal(err)
	}
	now := schedule.Now()
	objects, _, err := cluster.List(ctx, "head/", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 || !objects[0].LastModified.Equal(now) || !objects[1].LastModified.Equal(start) {
		t.Errorf("List reads %+v", objects)
	}
	info, err := cluster.Head(ctx, "head/before/_")
	if err != nil {
		t.Fatal(err)
	}
	if now.Sub(info.LastModified) != 25*time.Hour {
		t.Errorf("the object written before reads %s old", now.Sub(info.LastModified))
	}
	if untranslated := storeClock.Untranslated(); len(untranslated) != 0 {
		t.Errorf("untranslated %q", untranslated)
	}
	if witnessCount != 1 {
		t.Errorf("the witness was called %d times", witnessCount)
	}
}

// A world's store clock ages no Redis family, since that catch-up holds a Net's requests on its barriers.
func TestWorldStoreClockRefusesRedisFamilies(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	settings := DefaultStoreClockSettings()
	settings.RedisFamilies = []string{"{gka_*}*"}
	if _, _, err := NewWorldStoreClock(clock, clock, settings, sidecartest.NewFakeBlobs(clock, sidecartest.NewFaultQueue(), nil, nil), nil); err == nil {
		t.Fatal("a world's store clock took Redis families")
	}
}
