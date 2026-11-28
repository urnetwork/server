package privateprovidercapture

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func syntheticRecord(now time.Time) Record {
	return Record{StartedUTC: now, GroupID: fmt.Sprintf("%08x-0000-0000-0000-%012x", 1, 1), CallerLocationID: fmt.Sprintf("%08x-0000-0000-0000-%012x", 2, 2), RequestRank: "quality", LoadRank: "speed", Source: "alternate_legacy", IPFamily: "any", RequestedGroups: 1, MissingTargets: 2}
}

// A deterministic clock drives exactly the same admission/expiry transitions;
// no scheduling or short timeout supplies the primary proof.
func syntheticRecorder() (*Recorder, *time.Time) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	r := &Recorder{state: "unarmed", armNotify: make(chan struct{}, 1), now: func() time.Time { return now }}
	return r, &now
}

func TestProviderCaptureUnarmedAndInvalidSamples(t *testing.T) {
	r, now := syntheticRecorder()
	if r.Offer(syntheticRecord(*now)) || r.Enabled() {
		t.Fatal("unarmed owner captured")
	}
	if r.Arm("not-an-operator-capture-id") != "invalid_capture_id" {
		t.Fatal("invalid arm accepted")
	}
	if r.Arm(strings.Repeat("a", 32)) != "armed" {
		t.Fatal("valid arm failed")
	}
	for _, change := range []func(*Record){
		func(s *Record) { s.GroupID = "private.example" },
		func(s *Record) { s.CallerLocationID = "private.example" },
		func(s *Record) { s.RequestRank = "unknown" },
		func(s *Record) { s.Source = "unknown" },
		func(s *Record) { s.IPFamily = "v6" },
		func(s *Record) { s.StartedUTC = now.Add(-time.Second) },
		func(s *Record) { s.StartedUTC = now.Add(time.Second) },
		func(s *Record) { s.RequestedGroups = 0 },
		func(s *Record) { s.MissingTargets = 0 },
	} {
		sample := syntheticRecord(*now)
		change(&sample)
		if r.Offer(sample) {
			t.Fatal("invalid sample accepted")
		}
	}
	if r.count != 0 || !r.Offer(syntheticRecord(*now)) {
		t.Fatal("invalid sample consumed the valid slot")
	}
}

func TestProviderCapturePreservesObservedZeroGroupAndCaller(t *testing.T) {
	r, now := syntheticRecorder()
	id := strings.Repeat("a", 32)
	r.Arm(id)
	sample := syntheticRecord(*now)
	sample.GroupID = "00000000-0000-0000-0000-000000000000"
	sample.CallerLocationID = sample.GroupID
	if !r.Offer(sample) {
		t.Fatal("actual zero-valued cache key was silently discarded")
	}
	read := r.Inspect(id, true)
	if len(read.Records) != 1 || read.Records[0].GroupID != sample.GroupID || read.Records[0].CallerLocationID != sample.CallerLocationID {
		t.Fatal("observed zero key was replaced")
	}
}

func TestProviderCaptureRateCapAndStatusPrivacy(t *testing.T) {
	r, now := syntheticRecorder()
	id := strings.Repeat("a", 32)
	r.Arm(id)
	for i := range MaxRecords {
		if !r.Offer(syntheticRecord(*now)) {
			t.Fatalf("sample %d refused", i)
		}
		if r.Offer(syntheticRecord(*now)) {
			t.Fatal("same-minute sample accepted")
		}
		*now = now.Add(SampleInterval)
	}
	status := r.Inspect(id, false)
	if status.Retained != MaxRecords || status.Records != nil || r.Enabled() {
		t.Fatal("cap or tuple-free status failed")
	}
	if r.Offer(syntheticRecord(*now)) {
		t.Fatal("ninth sample accepted")
	}
	read := r.Inspect(id, true)
	if read.State != "complete" || len(read.Records) != MaxRecords {
		t.Fatal("bounded sample unavailable")
	}
	for i, record := range read.Records {
		if record.Sequence != uint64(i+1) || record.CompletedUTC.Sub(record.StartedUTC) != 0 {
			t.Fatal("sample provenance changed")
		}
	}
	if second := r.Inspect(id, true); second.State != "consumed" || second.Records != nil || r.count != 0 || r.records != ([MaxRecords]Record{}) {
		t.Fatal("second read retained or returned tuples")
	}
	if r.Arm(strings.Repeat("b", 32)) != "already_armed" {
		t.Fatal("consumed owner rearmed")
	}
}

func TestProviderCaptureWindowAndExpiryRemainUnknown(t *testing.T) {
	r, now := syntheticRecorder()
	id := strings.Repeat("a", 32)
	r.Arm(id)
	if empty := r.Inspect(id, true); empty.State != "no_sample_yet" || empty.Records != nil {
		t.Fatal("no sample became a healthy claim")
	}
	if wrong := r.Inspect(strings.Repeat("b", 32), true); wrong.State != "capture_id_mismatch" || wrong.Records != nil {
		t.Fatal("capture mismatch consumed")
	}
	if !r.Offer(syntheticRecord(*now)) {
		t.Fatal("valid sample refused")
	}
	*now = now.Add(CaptureWindow)
	if r.Offer(syntheticRecord(*now)) || r.Enabled() {
		t.Fatal("window admitted late completion")
	}
	if status := r.Inspect(id, false); status.Retained != 1 {
		t.Fatal("window end discarded unexpired evidence")
	}
	*now = now.Add(RetentionTTL - CaptureWindow)
	if expired := r.Inspect(id, true); expired.State != "expired" || expired.Records != nil || r.records != ([MaxRecords]Record{}) {
		t.Fatal("expiry leaked a tuple")
	}
	if r.Arm(id) != "already_armed" {
		t.Fatal("expired owner rearmed")
	}
}

func TestProviderCaptureDoesNotWaitForContendedOwner(t *testing.T) {
	r, now := syntheticRecorder()
	r.Arm(strings.Repeat("a", 32))
	r.mu.Lock()
	accepted := r.Offer(syntheticRecord(*now))
	r.mu.Unlock()
	if accepted || r.contended.Load() != 1 || r.count != 0 {
		t.Fatal("contended path was not a zero-wait refusal")
	}
}

func TestProviderCaptureLifecycleAndIndependentOwners(t *testing.T) {
	r := NewRecorder(context.Background())
	other := NewRecorder(context.Background())
	defer other.Close()
	id := strings.Repeat("a", 32)
	r.Arm(id)
	other.Arm(id)
	if !r.Offer(syntheticRecord(time.Now())) || !other.Offer(syntheticRecord(time.Now())) {
		t.Fatal("owners unexpectedly shared capacity")
	}
	r.Close()
	r.Close()
	if r.Enabled() || r.Inspect(id, true).State != "closed" || r.records != ([MaxRecords]Record{}) {
		t.Fatal("closed owner retained tuples")
	}
	if !other.Enabled() || other.Inspect(id, false).Retained != 1 {
		t.Fatal("closing one owner changed another")
	}
}

func TestProviderCaptureBindsOnlyNaturalRouteContext(t *testing.T) {
	r := NewRecorder(context.Background())
	defer r.Close()
	r.Arm(strings.Repeat("a", 32))
	for _, testCase := range []struct {
		method, path string
		present      bool
	}{
		{method: http.MethodPost, path: "/network/find-providers2", present: true},
		{method: http.MethodGet, path: "/network/find-providers2", present: false},
		{method: http.MethodPost, path: "/network/find-providers", present: false},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		request := httptest.NewRequest(testCase.method, testCase.path, nil).WithContext(ctx)
		called := false
		r.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			called = true
			if (FromContext(request.Context()) != nil) != testCase.present {
				t.Fatal("wrong route received capture owner")
			}
			cancel()
			if request.Context().Err() == nil {
				t.Fatal("wrapper lost request cancellation")
			}
		})).ServeHTTP(httptest.NewRecorder(), request)
		cancel()
		if !called {
			t.Fatal("wrapper blocked route")
		}
	}
}
