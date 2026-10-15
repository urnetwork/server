package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

type shadowCaptureTestOwner struct {
	mu        sync.Mutex
	snapshot  ArinShadowOwnerSnapshot
	available bool
}

func (o *shadowCaptureTestOwner) ArinShadowCurrentConnection() (ArinShadowOwnerSnapshot, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.snapshot, o.available
}

func captureFixture(t *testing.T) (*ArinShadowRecorder, *shadowCaptureTestOwner, ArinShadowCaptureFacts) {
	t.Helper()
	r, f := shadowFixture(t, false)
	now := time.Unix(f.Epoch, 0).UTC().Add(48 * time.Hour)
	r.clock = func() time.Time { return now }
	r.start = now.Add(-time.Second)
	f.At = time.Unix(f.Epoch, 0).UTC().Add(time.Hour)
	o := &shadowCaptureTestOwner{available: true, snapshot: ArinShadowOwnerSnapshot{ConnectionId: NewId(), ClientId: NewId(), HandlerId: NewId(), Address: netip.MustParseAddr("192.0.2.1"), At: now}}
	facts := ArinShadowCaptureFacts{ConnectionId: o.snapshot.ConnectionId, ClientId: o.snapshot.ClientId, HandlerId: o.snapshot.HandlerId, ObservedAt: now, Connected: true, Present: true, Actual: f}
	return r, o, facts
}

func TestArinCurrentCaptureOldLookupSeparateFromFreshOwner(t *testing.T) {
	r, o, f := captureFixture(t)
	if r.Observe("legacy", "192.0.2.1", f.Actual) {
		t.Fatal("natural-write90s fence unexpectedly widened")
	}
	rows, err := r.CaptureCurrent(context.Background(), []ArinShadowCaptureTarget{{ConnectionId: f.ConnectionId, Owner: o}}, func(ctx context.Context, ids []Id) ([]ArinShadowCaptureFacts, error) {
		if len(ids) != 1 || ids[0] != f.ConnectionId {
			t.Fatal("not exact keys")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded fact reader")
		}
		return []ArinShadowCaptureFacts{f}, nil
	})
	if err != nil || len(rows) != 1 || rows[0].reason != "qualified" || !rows[0].actualAt.Equal(f.Actual.At) || !rows[0].observedAt.Equal(f.ObservedAt) {
		t.Fatal("old immutable lookup not joined separately", err, rows)
	}
	if !rows[0].facts.verified {
		t.Fatal("positive subscriber control missing")
	}
}

func TestArinCurrentCaptureRejectsChangedMaskedAddressAndBinding(t *testing.T) {
	for _, which := range []string{"same_mask_address", "client", "handler", "closed", "epoch", "flag", "stale_read", "future_lookup", "pre_epoch", "missing", "duplicate", "extra"} {
		t.Run(which, func(t *testing.T) {
			r, o, f := captureFixture(t)
			rows, err := r.CaptureCurrent(context.Background(), []ArinShadowCaptureTarget{{ConnectionId: f.ConnectionId, Owner: o}}, func(context.Context, []Id) ([]ArinShadowCaptureFacts, error) {
				switch which {
				case "same_mask_address":
					o.snapshot.Address = netip.MustParseAddr("192.0.2.2") //same/29;exact owner changed
				case "client":
					f.ClientId = NewId()
				case "handler":
					f.HandlerId = NewId()
				case "closed":
					o.available = false
				case "epoch":
					f.Actual.Epoch++
				case "flag":
					f.Actual.Risk = !f.Actual.Risk
				case "stale_read":
					f.ObservedAt = f.ObservedAt.Add(-3 * time.Second)
				case "future_lookup":
					f.Actual.At = f.ObservedAt.Add(time.Minute)
				case "pre_epoch":
					f.Actual.At = time.Unix(f.Actual.Epoch-1, 0)
				case "missing":
					return nil, nil
				case "duplicate":
					return []ArinShadowCaptureFacts{f, f}, nil
				case "extra":
					f.ConnectionId = NewId()
				}
				return []ArinShadowCaptureFacts{f}, nil
			})
			if err == nil && (len(rows) != 1 || rows[0].reason == "qualified") {
				t.Fatal("unqualified capture accepted", rows)
			}
			if which == "same_mask_address" && rows[0].reason != "owner_changed" {
				t.Fatal("masked address was used as identity")
			}
		})
	}
}

func TestArinCurrentCaptureNeverLocksAcrossReaderAndClose(t *testing.T) {
	r, o, f := captureFixture(t)
	readEntered, release := make(chan struct{}), make(chan struct{})
	done := make(chan []arinShadowCapturedConnection, 1)
	go func() {
		rows, _ := r.CaptureCurrent(context.Background(), []ArinShadowCaptureTarget{{ConnectionId: f.ConnectionId, Owner: o}}, func(context.Context, []Id) ([]ArinShadowCaptureFacts, error) {
			close(readEntered)
			<-release
			return []ArinShadowCaptureFacts{f}, nil
		})
		done <- rows
	}()
	<-readEntered
	closed := make(chan struct{})
	go func() { r.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("recorder lock held across reader")
	}
	close(release)
	if rows := <-done; len(rows) != 1 || rows[0].reason != "lookup_unavailable" {
		t.Fatal("closed resources used", rows)
	}
}

func TestArinCurrentCaptureBoundsAndCancellation(t *testing.T) {
	r, o, f := captureFixture(t)
	calls := 0
	read := func(context.Context, []Id) ([]ArinShadowCaptureFacts, error) {
		calls++
		return []ArinShadowCaptureFacts{f}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.CaptureCurrent(ctx, []ArinShadowCaptureTarget{{f.ConnectionId, o}}, read); err == nil || calls != 0 {
		t.Fatal("canceled owner reached reader")
	}
	if _, err := r.CaptureCurrent(context.Background(), make([]ArinShadowCaptureTarget, 257), read); err == nil || calls != 0 {
		t.Fatal("oversized batch reached reader")
	}
	if _, err := r.CaptureCurrent(context.Background(), []ArinShadowCaptureTarget{{f.ConnectionId, o}, {f.ConnectionId, o}}, read); err == nil || calls != 0 {
		t.Fatal("duplicate key reached reader")
	}
	_, err := r.CaptureCurrent(context.Background(), []ArinShadowCaptureTarget{{f.ConnectionId, o}}, func(context.Context, []Id) ([]ArinShadowCaptureFacts, error) {
		r.clock = func() time.Time { return f.ObservedAt.Add(91 * time.Second) }
		return []ArinShadowCaptureFacts{f}, nil
	})
	if err == nil {
		t.Fatal("late batch accepted")
	}
}

func TestArinCurrentStreamSpansBatchesCountsOnceAndPrivate(t *testing.T) {
	r, o, f := captureFixture(t)
	s, err := r.NewCurrentCaptureStream(context.Background(), []string{"all", "us", "ca"})
	if err != nil {
		t.Fatal(err)
	}
	const n = 513
	if err = s.BeginProvider(ArinShadowCaptureProvider{ClientId: f.ClientId, ExpectedConnections: n, Buckets: []string{"all", "us"}, BaseQuality: true, BaseSpeed: true, ActiveQuality: true}); err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < n; offset += ArinShadowCaptureBatchLimit {
		var targets []ArinShadowCaptureTarget
		facts := map[Id]ArinShadowCaptureFacts{}
		for i := offset; i < min(n, offset+ArinShadowCaptureBatchLimit); i++ {
			id := captureNumberId(i + 1)
			snapshot := o.snapshot
			snapshot.ConnectionId = id
			targets = append(targets, ArinShadowCaptureTarget{id, &shadowCaptureTestOwner{snapshot: snapshot, available: true}})
			row := f
			row.ConnectionId = id
			facts[id] = row
		}
		if err = s.CaptureBatch(targets, func(ctx context.Context, ids []Id) ([]ArinShadowCaptureFacts, error) {
			out := make([]ArinShadowCaptureFacts, 0, len(ids))
			for _, id := range ids {
				out = append(out, facts[id])
			}
			return out, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.EndProvider(); err != nil {
		t.Fatal(err)
	}
	report, err := s.Finish(true, 1, n)
	if err != nil || !report.ObservationComplete || report.ActualMainCoverage || report.Providers != 1 || report.CapturedConnections != n || report.Buckets[0].CandidateQuality != 1 || report.Buckets[1].Providers != 0 {
		t.Fatal("incorrect streamed census", err, report)
	}
	if !report.EarliestLookupAt.Equal(f.Actual.At) || !report.LatestDurableReadAt.Equal(f.ObservedAt) {
		t.Fatal("lookup/capture clocks collapsed")
	}
	raw, _ := json.Marshal(report)
	for _, secret := range []string{"192.0.2.1", f.ClientId.String(), f.HandlerId.String(), f.ConnectionId.String()} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private capture leaked")
		}
	}
	if len(s.buckets) != 3 || s.current != nil {
		t.Fatal("population retained")
	}
}

func captureNumberId(n int) Id {
	var id Id
	id[12] = byte(n >> 24)
	id[13] = byte(n >> 16)
	id[14] = byte(n >> 8)
	id[15] = byte(n)
	return id
}

func TestArinCurrentStreamMissingRowsAndReadFailureAreIndeterminate(t *testing.T) {
	for _, failure := range []string{"truncated", "reader", "missing_owner", "wrong_provider", "missing_end_marker", "wrong_total"} {
		t.Run(failure, func(t *testing.T) {
			r, o, f := captureFixture(t)
			s, _ := r.NewCurrentCaptureStream(context.Background(), []string{"all"})
			provider := ArinShadowCaptureProvider{ClientId: f.ClientId, ExpectedConnections: 1, Buckets: []string{"all"}, BaseQuality: true, ActiveQuality: true, BaseSpeed: true, ActiveSpeed: true}
			if failure == "truncated" {
				provider.ExpectedConnections = 2
			}
			if failure == "wrong_provider" {
				provider.ClientId = NewId()
			}
			if err := s.BeginProvider(provider); err != nil {
				t.Fatal(err)
			}
			if failure == "missing_owner" {
				o.available = false
			}
			err := s.CaptureBatch([]ArinShadowCaptureTarget{{f.ConnectionId, o}}, func(context.Context, []Id) ([]ArinShadowCaptureFacts, error) {
				if failure == "reader" {
					return nil, errors.New("private error must not appear")
				}
				return []ArinShadowCaptureFacts{f}, nil
			})
			if err != nil && failure != "reader" {
				t.Fatal(err)
			}
			if err = s.EndProvider(); err != nil {
				t.Fatal(err)
			}
			providers := int64(1)
			if failure == "wrong_total" {
				providers = 2
			}
			report, err := s.Finish(failure != "missing_end_marker", providers, int64(provider.ExpectedConnections))
			if err != nil || report.ObservationComplete {
				t.Fatal("partial observation promoted", err, report)
			}
			if failure != "wrong_total" && failure != "missing_end_marker" && (report.Buckets[0].QualityRemoved != 0 || report.Buckets[0].QualityIndeterminate != 1 || report.Buckets[0].SpeedIndeterminate != 1) {
				t.Fatal("observation loss reported as policy loss", report)
			}
		})
	}
}

func TestArinCurrentStreamOrderingAndOuterBudget(t *testing.T) {
	for _, failure := range []string{"duplicate_connection", "duplicate_provider", "global_cap", "elapsed", "canceled", "unfinished"} {
		t.Run(failure, func(t *testing.T) {
			r, o, f := captureFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, _ := r.NewCurrentCaptureStream(ctx, []string{"all"})
			p := ArinShadowCaptureProvider{ClientId: f.ClientId, ExpectedConnections: 1, Buckets: []string{"all"}}
			if failure == "global_cap" {
				p.ExpectedConnections = ArinShadowCapturePopulationLimit + 1
				if s.BeginProvider(p) == nil {
					t.Fatal("global cap ignored")
				}
				return
			}
			if err := s.BeginProvider(p); err != nil {
				t.Fatal(err)
			}
			if failure == "unfinished" {
				if _, err := s.Finish(true, 0, 0); err == nil {
					t.Fatal("unfinished provider omitted")
				}
				return
			}
			read := func(context.Context, []Id) ([]ArinShadowCaptureFacts, error) { return []ArinShadowCaptureFacts{f}, nil }
			if err := s.CaptureBatch([]ArinShadowCaptureTarget{{f.ConnectionId, o}}, read); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "duplicate_connection":
				if s.CaptureBatch([]ArinShadowCaptureTarget{{f.ConnectionId, o}}, read) == nil {
					t.Fatal("duplicate consumed")
				}
			case "duplicate_provider":
				if err := s.EndProvider(); err != nil {
					t.Fatal(err)
				}
				if s.BeginProvider(p) == nil {
					t.Fatal("duplicate provider consumed")
				}
			case "elapsed":
				r.clock = func() time.Time { return f.ObservedAt.Add(91 * time.Second) }
				if s.EndProvider() == nil {
					t.Fatal("outer budget exceeded")
				}
			case "canceled":
				cancel()
				if s.EndProvider() == nil {
					t.Fatal("canceled stream completed")
				}
			}
		})
	}
}
