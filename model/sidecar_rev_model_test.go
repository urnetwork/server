package model

// rev/'s writer and sweep (sidecar_rev_model.go; §7.8), on T2's FakeBlobs and a
// fake clock, with a stand-in signature in place of oauth.SignRevocation's
// (oauth/revocation_object_test.go runs the real one end to end). No Postgres.

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The stand-in's signature part.
const testRevSignature = "test-signature"

// A compact JWS shape around payload, as oauth.SignRevocation writes one.
func testRevSign(payload []byte) ([]byte, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"test","typ":"ur-rev+jwt"}`))
	return []byte(header + "." + base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(testRevSignature))), nil
}

func testRevVerify(object []byte) ([]byte, error) {
	parts := strings.Split(string(object), ".")
	if len(parts) != 3 {
		return nil, errors.New("not a jws")
	}
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if string(signature) != testRevSignature {
		return nil, errors.New("not signed")
	}
	return base64.RawURLEncoding.DecodeString(parts[1])
}

type testRevStore struct {
	clock   *sidecartest.FakeClock
	blobs   *sidecartest.FakeBlobs
	cluster sidecar.BlobCluster
	store   *SidecarRevStore
}

func newTestRevStore(t testing.TB) *testRevStore {
	t.Helper()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	blobs := sidecartest.NewFakeBlobs(clock, nil, nil, nil)
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	settings := DefaultSidecarRevSettings()
	settings.Clock = clock
	return &testRevStore{
		clock:   clock,
		blobs:   blobs,
		cluster: cluster,
		store:   NewSidecarRevStore(cluster, testRevSign, settings),
	}
}

// The record the key holds, read through the root's reader; nil for none.
func (self *testRevStore) held(t testing.TB, revocation *SidecarRevocation) *sidecar.RevocationRecord {
	t.Helper()
	record, err := sidecar.ReadRevocation(context.Background(), self.cluster, testRevVerify,
		sidecar.Id(revocation.NetworkId), revocation.Kind, sidecar.Id(revocation.SubjectId))
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func newTestRevocation(at time.Time) *SidecarRevocation {
	return &SidecarRevocation{
		NetworkId: server.NewId(),
		Kind:      sidecar.RevocationKindClient,
		SubjectId: server.NewId(),
		NotBefore: sidecar.RevocationNotBefore(at),
		Reason:    sidecar.RevocationReasonRemoved,
	}
}

// A write creates the object; a later not_before raises it; an earlier one
// leaves it, since not_before is never lowered.
func TestSidecarRevWriteCreatesRaisesNeverLowers(t *testing.T) {
	ctx := context.Background()
	s := newTestRevStore(t)
	revocation := newTestRevocation(s.clock.Now())
	if err := s.store.Write(ctx, revocation); err != nil {
		t.Fatal(err)
	}
	held := s.held(t, revocation)
	if held == nil || !held.NotBefore.Equal(revocation.NotBefore) || held.Reason != sidecar.RevocationReasonRemoved {
		t.Fatalf("created %+v", held)
	}

	s.clock.Advance(time.Minute)
	raise := *revocation
	raise.NotBefore = sidecar.RevocationNotBefore(s.clock.Now())
	raise.Reason = sidecar.RevocationReasonRevoked
	if err := s.store.Write(ctx, &raise); err != nil {
		t.Fatal(err)
	}
	if held := s.held(t, revocation); !held.NotBefore.Equal(raise.NotBefore) || held.Reason != sidecar.RevocationReasonRevoked {
		t.Fatalf("raised %+v", held)
	}

	putCount := s.blobs.RequestCount("meta-1", "put")
	if err := s.store.Write(ctx, revocation); err != nil {
		t.Fatal(err)
	}
	if held := s.held(t, revocation); !held.NotBefore.Equal(raise.NotBefore) {
		t.Fatalf("lowered to %v", held.NotBefore)
	}
	if s.blobs.RequestCount("meta-1", "put") != putCount {
		t.Fatal("a write at an earlier not_before sent a PUT")
	}
}

// §7.8's raise with a lost response: the If-Match PUT applies and its answer is
// lost, so the write reads again and finds its own not_before there.
func TestSidecarRevRaiseWithLostResponse(t *testing.T) {
	ctx := context.Background()
	s := newTestRevStore(t)
	revocation := newTestRevocation(s.clock.Now())
	if err := s.store.Write(ctx, revocation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.blobs.Faults().Add(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, KeyPrefix: "rev/", Result: sidecartest.ApplyThenDrop}); err != nil {
		t.Fatal(err)
	}
	s.clock.Advance(time.Minute)
	raise := *revocation
	raise.NotBefore = sidecar.RevocationNotBefore(s.clock.Now())
	if err := s.store.Write(ctx, &raise); err != nil {
		t.Fatal(err)
	}
	if held := s.held(t, revocation); !held.NotBefore.Equal(raise.NotBefore) {
		t.Fatalf("after a lost response the object holds %v", held.NotBefore)
	}
	if unfired := s.blobs.Faults().Unfired(); len(unfired) != 0 {
		t.Fatalf("unfired %v", unfired)
	}
	// one PUT applied and was lost, and the read after it ended the write
	if count := s.blobs.RequestCount("meta-1", "put"); count != 2 {
		t.Fatalf("%d PUTs", count)
	}
}

// A create's lost response and a racing creator's 412 both resolve by reading.
func TestSidecarRevCreateRacesResolve(t *testing.T) {
	ctx := context.Background()
	s := newTestRevStore(t)
	revocation := newTestRevocation(s.clock.Now())
	if _, err := s.blobs.Faults().Add(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, KeyPrefix: "rev/", Result: sidecartest.ApplyThenDrop}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.Write(ctx, revocation); err != nil {
		t.Fatal(err)
	}
	if held := s.held(t, revocation); held == nil || !held.NotBefore.Equal(revocation.NotBefore) {
		t.Fatalf("after a lost create %+v", held)
	}

	// another writer creates first, with a later not_before: this write ends
	// at the 412's read, leaving the later one
	other := newTestRevocation(s.clock.Now())
	later := *other
	later.NotBefore = other.NotBefore.Add(time.Minute)
	Testing_SidecarRevBeforePut = func(key string) {
		Testing_SidecarRevBeforePut = nil
		if err := s.store.Write(ctx, &later); err != nil {
			t.Error(err)
		}
	}
	defer func() { Testing_SidecarRevBeforePut = nil }()
	if err := s.store.Write(ctx, other); err != nil {
		t.Fatal(err)
	}
	if held := s.held(t, other); !held.NotBefore.Equal(later.NotBefore) {
		t.Fatalf("a racing create lowered not_before to %v", held.NotBefore)
	}
}

// A store that fails leaves the write to its task, which reads again.
func TestSidecarRevWriteFailureReturnsError(t *testing.T) {
	ctx := context.Background()
	s := newTestRevStore(t)
	if _, err := s.blobs.Faults().Add(&sidecartest.FaultPlan{Op: sidecartest.FaultOpGet, KeyPrefix: "rev/", Result: sidecartest.FailBeforeApply}); err != nil {
		t.Fatal(err)
	}
	revocation := newTestRevocation(s.clock.Now())
	if err := s.store.Write(ctx, revocation); err == nil {
		t.Fatal("a failed read reported a write")
	}
	if err := s.store.Write(ctx, revocation); err != nil {
		t.Fatal(err)
	}
}

// §7.8's life row: an object goes 20 minutes after its last write and not
// before, and a raise starts its 20 minutes again.
func TestSidecarRevSweepDeletesTwentyMinutesAfterLastWrite(t *testing.T) {
	ctx := context.Background()
	s := newTestRevStore(t)
	first := newTestRevocation(s.clock.Now())
	second := newTestRevocation(s.clock.Now())
	for _, revocation := range []*SidecarRevocation{first, second} {
		if err := s.store.Write(ctx, revocation); err != nil {
			t.Fatal(err)
		}
	}
	s.clock.Advance(10 * time.Minute)
	raise := *second
	raise.NotBefore = sidecar.RevocationNotBefore(s.clock.Now())
	if err := s.store.Write(ctx, &raise); err != nil {
		t.Fatal(err)
	}

	s.clock.Advance(10*time.Minute - time.Second)
	if deleted, err := s.store.Sweep(ctx); err != nil || deleted != 0 {
		t.Fatalf("at 19:59 deleted %d: %v", deleted, err)
	}
	s.clock.Advance(time.Second)
	if deleted, err := s.store.Sweep(ctx); err != nil || deleted != 1 {
		t.Fatalf("at 20:00 deleted %d: %v", deleted, err)
	}
	if s.held(t, first) != nil {
		t.Fatal("the first object outlived its 20 minutes")
	}
	if s.held(t, second) == nil {
		t.Fatal("the raised object went 20 minutes after its first write")
	}
	s.clock.Advance(10 * time.Minute)
	if deleted, err := s.store.Sweep(ctx); err != nil || deleted != 1 || s.held(t, second) != nil {
		t.Fatalf("20 minutes after the raise deleted %d: %v", deleted, err)
	}
}

// A raise between the sweep's LIST and its delete stays: the sweep reads the
// key again before it deletes.
func TestSidecarRevSweepKeepsARaiseSinceItsList(t *testing.T) {
	ctx := context.Background()
	s := newTestRevStore(t)
	revocation := newTestRevocation(s.clock.Now())
	if err := s.store.Write(ctx, revocation); err != nil {
		t.Fatal(err)
	}
	s.clock.Advance(20 * time.Minute)
	if _, err := s.blobs.Faults().Add(&sidecartest.FaultPlan{
		Op:        sidecartest.FaultOpList,
		KeyPrefix: "rev/",
		Result:    sidecartest.Delay,
		Delay:     sidecartest.DelayInNetwork,
	}); err != nil {
		t.Fatal(err)
	}
	swept := make(chan int, 1)
	go func() {
		deleted, err := s.store.Sweep(ctx)
		if err != nil {
			t.Error(err)
		}
		swept <- deleted
	}()
	// the LIST is parked, its client timeout the one timer
	if err := s.clock.AwaitTimers(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if delivered := s.blobs.DeliverDelayed("meta-1", "rev/"); delivered != 1 {
		t.Fatalf("%d parked", delivered)
	}
	// the LIST has answered with the old object; raise it before the sweep's HEAD
	raise := *revocation
	raise.NotBefore = sidecar.RevocationNotBefore(s.clock.Now())
	if err := s.store.Write(ctx, &raise); err != nil {
		t.Fatal(err)
	}
	if deleted := <-swept; deleted != 0 {
		// the HEAD may have run first, which deletes; the raise then created
		// again, and the object still stands either way
		if s.held(t, revocation) == nil {
			t.Fatal("a raise was lost to the sweep")
		}
		return
	}
	if held := s.held(t, revocation); held == nil || !held.NotBefore.Equal(raise.NotBefore) {
		t.Fatalf("after the sweep %+v", held)
	}
}

// The process's writer: none installed, the task fails and keeps every
// revocation for its retry.
func TestSidecarRevTaskWithoutWriterRetries(t *testing.T) {
	SetSidecarRevStore(nil)
	ctx := context.Background()
	clientSession := session.NewLocalClientSession(ctx, "", nil)
	defer clientSession.Cancel()
	_, err := WriteSidecarRevTask(&WriteSidecarRevArgs{Revocations: []*SidecarRevocation{newTestRevocation(server.NowUtc())}}, clientSession)
	if err == nil {
		t.Fatal("a task with no writer reported its objects landed")
	}
	if _, err := SweepSidecarRevocations(ctx); err == nil {
		t.Fatal("a sweep with no writer reported success")
	}

	s := newTestRevStore(t)
	SetSidecarRevStore(s.store)
	defer SetSidecarRevStore(nil)
	revocation := newTestRevocation(s.clock.Now())
	if _, err := WriteSidecarRevTask(&WriteSidecarRevArgs{Revocations: []*SidecarRevocation{revocation}}, clientSession); err != nil {
		t.Fatal(err)
	}
	if s.held(t, revocation) == nil {
		t.Fatal("the task did not land its object")
	}
}

// The object the writer lands reads through the root's checker: a token of the
// removed client issued before not_before is void, one after it honored.
func TestSidecarRevObjectVoidsAtTheChecker(t *testing.T) {
	ctx := context.Background()
	s := newTestRevStore(t)
	issuedAt := s.clock.Now().Truncate(time.Second)
	s.clock.Advance(time.Second)
	revocation := newTestRevocation(s.clock.Now())
	if err := s.store.Write(ctx, revocation); err != nil {
		t.Fatal(err)
	}
	settings := sidecar.DefaultRevocationSettings()
	settings.Clock = s.clock
	checker := sidecar.NewRevocationChecker(s.blobs, testRevVerify, settings)
	principal := &sidecar.Principal{
		NetworkId: [16]byte(revocation.NetworkId),
		ClientId:  [16]byte(revocation.SubjectId),
		Audience:  "gkm",
		IssuedAt:  issuedAt,
	}
	if err := checker.Check(ctx, principal); !errors.Is(err, sidecar.ErrRevocationVoidsToken) {
		t.Fatalf("a removed client's token: %v", err)
	}
	principal.IssuedAt = revocation.NotBefore
	s.clock.Advance(10 * time.Second)
	if err := checker.Check(ctx, principal); err != nil {
		t.Fatalf("a token issued at not_before: %v", err)
	}
}

// What a payload the writer holds reads as, signed or not.
func TestSidecarRevHeldRecordReadsUnverified(t *testing.T) {
	revocation := newTestRevocation(time.Unix(1767225600, 0))
	payload, err := sidecar.EncodeRevocationPayload(&sidecar.RevocationRecord{
		NetworkId: sidecar.Id(revocation.NetworkId),
		Kind:      revocation.Kind,
		SubjectId: sidecar.Id(revocation.SubjectId),
		NotBefore: revocation.NotBefore,
		Reason:    revocation.Reason,
		Time:      revocation.NotBefore,
	})
	if err != nil {
		t.Fatal(err)
	}
	object, _ := testRevSign(payload)
	if held, ok := sidecarRevHeldRecord(object); !ok || !held.NotBefore.Equal(revocation.NotBefore) {
		t.Fatalf("held %+v %v", held, ok)
	}
	for _, bad := range [][]byte{[]byte("x"), []byte("a.b.c"), payload} {
		if _, ok := sidecarRevHeldRecord(bad); ok {
			t.Errorf("read %q", bad)
		}
	}
}
