// Package privateprovidercapture retains a bounded, explicitly armed sample of
// completed ordinary group cache misses. It has no public endpoint or uploader.
package privateprovidercapture

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	MaxRecords     = 8
	SampleInterval = time.Minute
	CaptureWindow  = 10 * time.Minute
	RetentionTTL   = 15 * time.Minute
)

// Record contains only the group/caller cache key and finite request
// provenance. The caller is the selector's already-resolved country location,
// including its actual zero value; it is never inferred from a country code.
type Record struct {
	Sequence         uint64    `json:"sequence"`
	StartedUTC       time.Time `json:"started_utc"`
	CompletedUTC     time.Time `json:"completed_utc"`
	GroupID          string    `json:"group_id"`
	CallerLocationID string    `json:"caller_location_id"`
	RequestRank      string    `json:"request_rank"`
	LoadRank         string    `json:"load_rank"`
	Source           string    `json:"source"`
	IPFamily         string    `json:"ip_family"`
	RequestedGroups  int       `json:"requested_groups"`
	MissingTargets   int       `json:"missing_targets"`
}

type Snapshot struct {
	State           string    `json:"state"`
	CaptureID       string    `json:"capture_id"`
	ArmedUTC        time.Time `json:"armed_utc"`
	CaptureUntilUTC time.Time `json:"capture_until_utc"`
	ExpiresUTC      time.Time `json:"expires_utc"`
	Retained        int       `json:"retained"`
	RateLimited     uint64    `json:"rate_limited"`
	Contended       uint64    `json:"contended"`
	Records         []Record  `json:"records,omitempty"`
}

// Recorder is safe for concurrent use. Its API owner supplies the lifetime;
// the request path never waits for a lock, performs I/O, or starts a worker.
type Recorder struct {
	mu          sync.Mutex
	enabled     atomic.Bool
	contended   atomic.Uint64
	state       string
	id          string
	armed       time.Time
	last        time.Time
	count       int
	rateLimited uint64
	records     [MaxRecords]Record
	armNotify   chan struct{}
	cancel      context.CancelFunc
	done        chan struct{}
	now         func() time.Time
}

func NewRecorder(ctx context.Context) *Recorder {
	owned, cancel := context.WithCancel(ctx)
	r := &Recorder{state: "unarmed", armNotify: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{}), now: time.Now}
	go r.run(owned)
	return r
}

func (r *Recorder) run(ctx context.Context) {
	defer close(r.done)
	select {
	case <-ctx.Done():
		r.clear("closed")
		return
	case <-r.armNotify:
	}
	r.mu.Lock()
	duration := max(time.Duration(0), r.armed.Add(CaptureWindow).Sub(r.now()))
	r.mu.Unlock()
	select {
	case <-ctx.Done():
		r.clear("closed")
		return
	case <-time.After(duration):
		r.enabled.Store(false)
	}
	r.mu.Lock()
	duration = max(time.Duration(0), r.armed.Add(RetentionTTL).Sub(r.now()))
	r.mu.Unlock()
	select {
	case <-ctx.Done():
		r.clear("closed")
	case <-time.After(duration):
		r.clear("expired")
	}
}

func (r *Recorder) clear(state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled.Store(false)
	r.records = [MaxRecords]Record{}
	r.count = 0
	r.state = state
}

func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.cancel()
	<-r.done
	r.clear("closed")
}

func (r *Recorder) Enabled() bool { return r != nil && r.enabled.Load() }

// Arm is one-use for the process owner, even after expiry or consumption.
// The private socket validates native process identity before calling it.
func (r *Recorder) Arm(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !validCaptureID(id) {
		return "invalid_capture_id"
	}
	if r.state != "unarmed" {
		return "already_armed"
	}
	r.id, r.armed, r.state = id, r.now(), "armed"
	r.enabled.Store(true)
	r.armNotify <- struct{}{}
	return "armed"
}

func validCaptureID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && id == strings.ToLower(id)
}

func validLocationID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	compact := strings.ReplaceAll(id, "-", "")
	if len(compact) != 32 {
		return false
	}
	_, err := hex.DecodeString(compact)
	return err == nil && id == strings.ToLower(id)
}

func validRecord(record Record) bool {
	return validLocationID(record.GroupID) &&
		validLocationID(record.CallerLocationID) &&
		(record.RequestRank == "quality" || record.RequestRank == "speed") &&
		(record.LoadRank == "quality" || record.LoadRank == "speed") &&
		(record.Source == "primary_legacy" || record.Source == "alternate_legacy" || record.Source == "online_legacy") &&
		(record.IPFamily == "any" || record.IPFamily == "v4" || record.IPFamily == "dualstack") &&
		record.RequestedGroups > 0 && record.MissingTargets > 0 && !record.StartedUTC.IsZero()
}

// Offer accepts at most the first eligible completion each minute, up to eight
// for the single ten-minute arm. Refusal never changes the model response.
func (r *Recorder) Offer(record Record) bool {
	if !r.Enabled() {
		return false
	}
	if !r.mu.TryLock() {
		r.contended.Add(1)
		return false
	}
	defer r.mu.Unlock()
	now := r.now()
	if r.state != "armed" || !now.Before(r.armed.Add(CaptureWindow)) || r.count == MaxRecords {
		r.enabled.Store(false)
		return false
	}
	if record.StartedUTC.Before(r.armed) || record.StartedUTC.After(now) || !validRecord(record) {
		return false
	}
	if !r.last.IsZero() && now.Sub(r.last) < SampleInterval {
		r.rateLimited++
		return false
	}
	record.Sequence = uint64(r.count + 1)
	record.CompletedUTC = now.UTC()
	record.StartedUTC = record.StartedUTC.UTC()
	r.records[r.count] = record
	r.count++
	r.last = now
	if r.count == MaxRecords {
		r.enabled.Store(false)
	}
	return true
}

// Inspect emits no tuple until a one-use read. An empty or expired sample is
// unknown, never a healthy control. A read consumes before the socket write;
// transport failure therefore cannot authorize a second export.
func (r *Recorder) Inspect(id string, read bool) Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if r.state == "armed" && !now.Before(r.armed.Add(RetentionTTL)) {
		r.enabled.Store(false)
		r.records = [MaxRecords]Record{}
		r.count, r.state = 0, "expired"
	}
	if r.id != id {
		return Snapshot{State: "capture_id_mismatch"}
	}
	out := Snapshot{State: r.state, CaptureID: r.id, ArmedUTC: r.armed.UTC(), CaptureUntilUTC: r.armed.Add(CaptureWindow).UTC(), ExpiresUTC: r.armed.Add(RetentionTTL).UTC(), Retained: r.count, RateLimited: r.rateLimited, Contended: r.contended.Load()}
	if !read || r.state != "armed" {
		return out
	}
	if r.count == 0 {
		out.State = "no_sample_yet"
		return out
	}
	out.State = "complete"
	out.Records = make([]Record, r.count)
	copy(out.Records, r.records[:r.count])
	r.enabled.Store(false)
	r.records = [MaxRecords]Record{}
	r.count, r.state = 0, "consumed"
	return out
}

type contextKey struct{}

// Wrap binds only the natural provider route. No request body, header, account
// or client identity is retained, and request cancellation is preserved.
func (r *Recorder) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && request.URL.Path == "/network/find-providers2" && r.Enabled() {
			request = request.WithContext(context.WithValue(request.Context(), contextKey{}, r))
		}
		next.ServeHTTP(w, request)
	})
}

func FromContext(ctx context.Context) *Recorder {
	r, _ := ctx.Value(contextKey{}).(*Recorder)
	if !r.Enabled() {
		return nil
	}
	return r
}
