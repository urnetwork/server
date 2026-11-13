package privateheapprofile

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
)

// The limit includes the serialized private header and sampled profile, not
// just the profile body. Only the one configured process can retain a response.
const maxRetainedResponseBytes = 2 << 20
const maxResponseHeaderBytes = 65536
const responseTTL = 10 * time.Minute

func validRequest(r Request) bool {
	if r.Schema == 1 {
		return r.Operation == "" && r.RequestID == ""
	}
	if r.Schema != 2 || len(r.RequestID) != 32 || (r.Operation != "capture" && r.Operation != "status" && r.Operation != "retrieve") {
		return false
	}
	for _, c := range r.RequestID {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

type retainedResponse struct {
	mu        sync.Mutex
	requestID string
	state     string
	wire      []byte
	expires   time.Time
	stop      chan struct{}
	workers   sync.WaitGroup
}

func (r *retainedResponse) begin(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requestID = id
	r.state = "capturing"
	if id == "" {
		r.state = "legacy_consumed"
	}
}

func (r *retainedResponse) failed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.requestID != "" {
		r.state = "capture_unavailable"
	}
}

func responseWire(identity Identity, requestID string, capture Capture) ([]byte, error) {
	header, err := json.Marshal(Response{Schema: 2, Status: "complete", Identity: identity, Capture: &capture, RequestID: requestID})
	if err != nil || len(header)+1 > maxResponseHeaderBytes || len(capture.Profile) > maxRetainedResponseBytes-len(header)-1 {
		return nil, errors.New("retained_response_bound")
	}
	// A fresh exact-sized backing store avoids retaining bytes.Buffer spare
	// capacity. The transient sampler buffer is released after handle returns.
	wire := make([]byte, len(header)+1+len(capture.Profile))
	copy(wire, header)
	wire[len(header)] = '\n'
	copy(wire[len(header)+1:], capture.Profile)
	return wire, nil
}

func (r *retainedResponse) keep(ctx context.Context, wire []byte, ttl time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx.Err() != nil || r.state != "capturing" || len(wire) == 0 || cap(wire) > maxRetainedResponseBytes {
		return false
	}
	r.wire = wire
	r.state = "available"
	r.expires = time.Now().Add(ttl)
	r.stop = make(chan struct{})
	stop := r.stop
	r.workers.Add(1)
	go func() {
		defer r.workers.Done()
		timer := time.NewTimer(ttl)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		case <-stop:
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.state == "available" {
			r.wire = nil
			r.state = "expired"
		}
		r.stop = nil
	}()
	return true
}

func (r *retainedResponse) expireLocked(now time.Time) {
	if r.state == "available" && !now.Before(r.expires) {
		r.wire = nil
		r.state = "expired"
		if r.stop != nil {
			close(r.stop)
			r.stop = nil
		}
	}
}

func (r *retainedResponse) stateLocked(id string) string {
	if r.state == "" {
		return "unused"
	}
	if r.requestID == "" {
		return "legacy_consumed"
	}
	if r.requestID != id {
		return "request_mismatch"
	}
	return r.state
}

func (r *retainedResponse) retrieve(id string, now time.Time) ([]byte, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)
	state := r.stateLocked(id)
	if state != "available" {
		return nil, state
	}
	wire := r.wire
	r.wire = nil
	r.state = "retrieved"
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
	return wire, "complete"
}

type AttemptStatus struct {
	Schema          int      `json:"schema"`
	Status          string   `json:"status"`
	Identity        Identity `json:"identity"`
	RequestID       string   `json:"request_id"`
	AttemptState    string   `json:"attempt_state"`
	RetainedBytes   int      `json:"retained_bytes"`
	ExpiresInMillis int64    `json:"expires_in_millis"`
}

func (s *Server) respondStatus(w io.Writer, id string) error {
	now := time.Now()
	r := &s.retained
	r.mu.Lock()
	r.expireLocked(now)
	value := AttemptStatus{Schema: 2, Status: "status", Identity: s.identity, RequestID: id, AttemptState: r.stateLocked(id)}
	if value.AttemptState == "available" {
		value.RetainedBytes = len(r.wire)
		value.ExpiresInMillis = r.expires.Sub(now).Milliseconds()
	}
	r.mu.Unlock()
	return json.NewEncoder(w).Encode(value)
}

func (s *Server) respondV2(w io.Writer, id, state string, capture *Capture) error {
	return json.NewEncoder(w).Encode(Response{Schema: 2, Status: state, Identity: s.identity, RequestID: id, Capture: capture})
}

// Close is called only after the sole socket/capture owner has joined, so no
// keep can add another expiry worker concurrently with this Wait.
func (r *retainedResponse) close() {
	r.mu.Lock()
	r.wire = nil
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
	if r.state != "" {
		r.state = "expired"
	}
	r.mu.Unlock()
	r.workers.Wait()
}
