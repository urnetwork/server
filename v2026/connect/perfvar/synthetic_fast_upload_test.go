// Streaming uploads count only bytes validated by their exact owned origin run.
package perfvar

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// The fixed eight-stream projection is independent of client write completion.
type syntheticFastUploadSnapshot struct {
	Started           [8]bool   `json:"started"`
	Finished          [8]bool   `json:"finished"`
	FiniteComplete    [8]bool   `json:"finite_complete"`
	ValidatedBytes    [8]int64  `json:"validated_bytes"`
	Errors            [8]string `json:"errors"`
	InvalidAdmissions int       `json:"invalid_admissions"`
}

// One bounded publication lock serializes actual validation with the window cutoff.
// It never covers a body read, callback, response write or worker join.
type syntheticFastUploadRun struct {
	stateLock sync.Mutex
	ctx       context.Context
	started   time.Time
	states    [8]*syntheticFastFlowState
	receipt   syntheticFastUploadSnapshot
}

// Called before publishing any of this measurement's HTTP workers.
func (self *syntheticFastUploadRun) start(ctx context.Context, started time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ctx, self.started = ctx, started
}

// A controller-created stream is the only identity an origin can claim.
func (self *syntheticFastUploadRun) register(stream int, state *syntheticFastFlowState) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.states[stream] = state
}

// Duplicate HTTP attempts cannot replay a stream into its byte denominator.
func (self *syntheticFastUploadRun) begin(stream int) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if stream < 0 || len(self.states) <= stream || self.states[stream] == nil || self.receipt.Started[stream] {
		self.receipt.InvalidAdmissions++
		return false
	}
	// A known first request may arrive after its own measurement was canceled.
	if self.ctx.Err() != nil {
		return false
	}
	self.receipt.Started[stream] = true
	return true
}

// Publish only after validating the complete read at its original stream offset.
func (self *syntheticFastUploadRun) validated(stream, count int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	state := self.states[stream]
	now := max(1, int64(time.Since(self.started)))
	state.first.CompareAndSwap(0, now)
	state.last.Store(now)
	state.bytes.Add(int64(count))
	self.receipt.ValidatedBytes[stream] += int64(count)
}

// Real EOF and validation errors remain distinct from a canceled streaming body.
func (self *syntheticFastUploadRun) finish(stream int, complete bool, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.receipt.Finished[stream] = true
	self.receipt.FiniteComplete[stream] = complete
	if err != nil {
		self.receipt.Errors[stream] = err.Error()
	}
}

// Measurement and byte publication share a boundary; drain bytes stay outside it.
func (self *syntheticFastUploadRun) measurementBoundary() (int64, int64, int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	end, total := time.Now(), int64(0)
	for _, bytes := range self.receipt.ValidatedBytes {
		total += bytes
	}
	return int64(end.Sub(self.started)), end.UnixNano(), total
}

// Read after the controller has joined both HTTP workers and origin handlers.
func (self *syntheticFastUploadRun) snapshot() (syntheticFastUploadSnapshot, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var err error
	if self.receipt.InvalidAdmissions != 0 {
		err = errors.New("upload origin rejected an unknown or duplicate stream")
	}
	for stream := range self.receipt.Started {
		if self.receipt.Started[stream] != self.receipt.Finished[stream] || self.receipt.Errors[stream] != "" {
			err = errors.Join(err, fmt.Errorf("upload origin stream%d: joined=%t error=%s", stream, self.receipt.Finished[stream], self.receipt.Errors[stream]))
		}
	}
	return self.receipt, err
}

// HTTP transport reads are offered bytes only and never update measured counters.
type syntheticFastUploadBody struct {
	ctx           context.Context
	block         []byte
	offset, limit int64
	closed        atomic.Bool
}

// A single reusable block follows normal HTTP request-body backpressure.
func (self *syntheticFastUploadBody) Read(buffer []byte) (int, error) {
	if err := self.ctx.Err(); err != nil {
		return 0, err
	}
	if self.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	if self.limit > 0 && self.offset == self.limit {
		return 0, io.EOF
	}
	count := min(len(buffer), len(self.block)-int(self.offset%int64(len(self.block))))
	if self.limit > 0 {
		count = int(min(int64(count), self.limit-self.offset))
	}
	copy(buffer[:count], self.block[int(self.offset%int64(len(self.block))):][:count])
	self.offset += int64(count)
	return count, nil
}

// Close is safe alongside the transport's reader and retains no worker.
func (self *syntheticFastUploadBody) Close() error {
	self.closed.Store(true)
	return nil
}

// A complete finite response requires validated EOF; a timer supplies no receipt.
func (self *syntheticFastOrigin) receiveUpload(writer http.ResponseWriter, request *http.Request, run *syntheticFastOriginRun, stream int) {
	if run == nil || run.upload == nil || !run.upload.begin(stream) {
		http.Error(writer, "upload run identity refused", http.StatusConflict)
		return
	}
	upload := run.upload
	var validationErr error
	complete := false
	defer func() { upload.finish(stream, complete, validationErr) }()
	if request.Header.Get("Content-Encoding") != "" || request.Header.Get("Content-Type") != "application/octet-stream" ||
		self.settings.BytesPerStream > 0 && request.ContentLength != self.settings.BytesPerStream ||
		self.settings.BytesPerStream == 0 && request.ContentLength >= 0 {
		validationErr = errors.New("upload request framing changed")
		http.Error(writer, validationErr.Error(), http.StatusBadRequest)
		return
	}
	buffer, block := make([]byte, 32*1024), syntheticFastBlock(stream)
	offset, stalls := int64(0), 0
	for {
		threshold := self.settings.StallAfterBytes + int64(stalls)*self.settings.BodyStallEveryBytes
		if stalls < self.settings.BodyStallCount && self.settings.StallAfterBytes >= 0 && offset >= threshold {
			stalls++
			if syntheticFastWait(request.Context(), run.ctx, self.settings.StallBodyDuration) != nil {
				return
			}
		}
		count, readErr := request.Body.Read(buffer)
		if count > 0 {
			if !syntheticFastValidateChunk(block, buffer[:count], offset) || self.settings.BytesPerStream > 0 && offset+int64(count) > self.settings.BytesPerStream {
				validationErr = errors.New("upload body identity or length mismatch")
				http.Error(writer, validationErr.Error(), http.StatusBadRequest)
				return
			}
			offset += int64(count)
			upload.validated(stream, count)
		}
		if readErr == io.EOF {
			if self.settings.BytesPerStream <= 0 || offset != self.settings.BytesPerStream {
				if upload.ctx.Err() == nil {
					validationErr = errors.New("upload ended without its exact finite body")
				}
				return
			}
			complete = true
			writer.Header().Set("X-Synthetic-Complete", "true")
			writer.Header().Set("X-Synthetic-Upload-Bytes", strconv.FormatInt(offset, 10))
			writer.Header().Set("X-Synthetic-Stream", strconv.Itoa(stream))
			writer.Header().Set("Content-Length", "0")
			writer.WriteHeader(http.StatusOK)
			return
		}
		if readErr != nil {
			if upload.ctx.Err() == nil {
				validationErr = fmt.Errorf("upload body read: %w", readErr)
			}
			return
		}
	}
}

// The origin owns byte validation; this worker owns the exact POST and response.
func syntheticFastUpload(ctx context.Context, client *http.Client, target string, stream int, started time.Time, state *syntheticFastFlowState, limit int64) {
	result := syntheticFastFlow{Stream: stream, StartNs: int64(time.Since(started))}
	captureConnections, _ := ctx.Value(syntheticFastPlacementContextKey{}).(bool)
	phase := &syntheticFastPhaseObserver{origin: started, captureConnections: captureConnections}
	var headerNs atomic.Int64
	var reused atomic.Bool
	defer func() {
		result.Phase = phase.snapshot()
		result.ReturnedNs, result.HeaderFirstByteNs = int64(time.Since(started)), headerNs.Load()
		result.ConnectionReused = reused.Load()
		if result.HeaderFirstByteNs > 0 {
			result.HeaderTtfbNs = result.HeaderFirstByteNs - result.StartNs
		}
		state.result = result
	}()
	body := &syntheticFastUploadBody{ctx: ctx, block: syntheticFastBlock(stream), limit: limit}
	defer body.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if err == nil {
		request.ContentLength = -1
		if limit > 0 {
			request.ContentLength = limit
		}
		request.Header.Set("Content-Type", "application/octet-stream")
		trace := &httptrace.ClientTrace{
			GotFirstResponseByte: func() { headerNs.CompareAndSwap(0, max(1, int64(time.Since(started)))) },
			GotConn:              func(info httptrace.GotConnInfo) { reused.Store(info.Reused) },
		}
		phase.install(trace)
		request = request.WithContext(context.WithValue(httptrace.WithClientTrace(request.Context(), trace), syntheticFastPhaseContextKey{}, phase))
		var response *http.Response
		response, err = client.Do(request)
		if err == nil {
			defer response.Body.Close()
			result.Status = response.StatusCode
			bytes, readErr := io.ReadAll(io.LimitReader(response.Body, 2))
			if readErr != nil {
				err = readErr
			} else if response.StatusCode != http.StatusOK || response.Header.Get("Content-Encoding") != "" || response.Header.Get("Cache-Control") != "no-store, no-cache" ||
				response.Header.Get("X-Synthetic-Complete") != "true" || response.Header.Get("X-Synthetic-Stream") != strconv.Itoa(stream) ||
				limit <= 0 || response.Header.Get("X-Synthetic-Upload-Bytes") != strconv.FormatInt(limit, 10) || len(bytes) != 0 {
				err = errors.New("upload finite response identity or completion mismatch")
			} else {
				result.FiniteBodyComplete = true
			}
		}
	}
	if err != nil {
		state.err, state.errorAtNs, state.ownerCanceledAtError = err, int64(time.Since(started)), ctx.Err() != nil
		result.Error = err.Error()
	}
}
