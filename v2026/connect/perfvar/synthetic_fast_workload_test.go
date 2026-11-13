// Synthetic body measurement: bounded streaming origins and a joined
// controller. The integration adapter supplies the real native/TCP/H1 dial.
package perfvar

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Duration is a measurement window, not a body-completion deadline. A finite
// body is complete only after exact length, validated bytes and EOF/trailer.
type syntheticFastSettings struct {
	Upload                   bool             `json:"upload,omitempty"`
	RunDrainTimeout          time.Duration    `json:"run_drain_timeout_ns"`
	FixtureDrainTimeout      time.Duration    `json:"fixture_drain_timeout_ns"`
	OverallTimeout           time.Duration    `json:"overall_timeout_ns"`
	StartupTimeout           time.Duration    `json:"startup_timeout_ns"`
	HeaderTimeout            time.Duration    `json:"header_timeout_ns"`
	BodyStallCount           int              `json:"body_stall_count"`
	BodyStallEveryBytes      int64            `json:"body_stall_every_bytes"`
	Duration                 time.Duration    `json:"duration_ns"`
	SampleInterval           time.Duration    `json:"sample_interval_ns"`
	InitialStreams           int              `json:"initial_streams"`
	MaximumStreams           int              `json:"maximum_streams"`
	Adaptive                 bool             `json:"adaptive"`
	Warm                     bool             `json:"warm"`
	Tls                      bool             `json:"tls"`
	LoadedLatency            bool             `json:"loaded_latency"`
	BytesPerStream           int64            `json:"bytes_per_stream"`
	OfferedBitsPerSecond     int64            `json:"offered_bits_per_second"`
	AccessRoundTrip          time.Duration    `json:"access_round_trip_ns"`
	LossProbability          float64          `json:"loss_probability"`
	StallBeforeHeaders       [8]time.Duration `json:"stall_before_headers_ns"`
	StallAfterBytes          int64            `json:"stall_after_bytes"`
	StallBodyDuration        time.Duration    `json:"stall_body_duration_ns"`
	OldProviderFallback      bool             `json:"old_provider_fallback"`
	RequireDistinctProviders int              `json:"require_distinct_providers"`
}

// No100Mbps fixture ceiling: the default offered path is2Gbps in each direction.
func defaultSyntheticFastSettings() syntheticFastSettings {
	return syntheticFastSettings{RunDrainTimeout: 5 * time.Second, FixtureDrainTimeout: 30 * time.Second, OverallTimeout: 60 * time.Second, StartupTimeout: 15 * time.Second, HeaderTimeout: 20 * time.Second, BodyStallCount: 1, Duration: 10 * time.Second, SampleInterval: 500 * time.Millisecond,
		InitialStreams: 1, MaximumStreams: 8, Adaptive: true, Tls: true,
		OfferedBitsPerSecond: 2_000_000_000, AccessRoundTrip: 40 * time.Millisecond,
		StallAfterBytes: -1}
}

// Bounds cover all retained reports and payload buffers independently of rate.
func (self syntheticFastSettings) validate() error {
	if self.RunDrainTimeout <= 0 || 30*time.Second < self.RunDrainTimeout || self.FixtureDrainTimeout <= 0 || 60*time.Second < self.FixtureDrainTimeout || self.OverallTimeout <= 0 || 120*time.Second < self.OverallTimeout || self.StartupTimeout <= 0 || 60*time.Second < self.StartupTimeout || self.HeaderTimeout <= 0 || 60*time.Second < self.HeaderTimeout || self.BodyStallCount < 0 || 16 < self.BodyStallCount || self.StallAfterBytes < -1 || self.BodyStallEveryBytes < 0 || self.BodyStallCount > 1 && self.BodyStallEveryBytes == 0 || self.Duration < 5*time.Second || 30*time.Second < self.Duration || self.SampleInterval < 250*time.Millisecond || time.Second < self.SampleInterval ||
		self.InitialStreams < 1 || self.MaximumStreams < self.InitialStreams || 8 < self.MaximumStreams || self.BytesPerStream < 0 ||
		self.OfferedBitsPerSecond <= 0 || 20_000_000_000 < self.OfferedBitsPerSecond || self.AccessRoundTrip < 0 || time.Second < self.AccessRoundTrip ||
		self.LossProbability < 0 || 1 <= self.LossProbability || self.StallBodyDuration < 0 || 30*time.Second < self.StallBodyDuration ||
		self.RequireDistinctProviders < 0 || 4 < self.RequireDistinctProviders {
		return errors.New("invalid bounded synthetic Fast settings")
	}
	for _, stall := range self.StallBeforeHeaders {
		if stall < 0 || 30*time.Second < stall {
			return errors.New("invalid synthetic header stall")
		}
	}
	return nil
}

// Ramp only at sample boundaries. Pending zero-byte streams can expose an
// independent healthy origin; a plateau stops growth after a probing increase.
func syntheticFastNextStreams(current, maximum int, adaptive bool, intervalBytes, previousBytes int64, pendingZero int) int {
	if !adaptive || current >= maximum {
		return current
	}
	if pendingZero > 0 || previousBytes < 0 && intervalBytes > 0 || previousBytes > 0 && intervalBytes*100 >= previousBytes*105 {
		return min(maximum, current*2)
	}
	return current
}

// A32KiB deterministic block is generated once per worker. Compression and
// caches are disabled; validation uses the same stream-specific block offset.
func syntheticFastBlock(stream int) []byte {
	value := uint64(stream + 1)
	block := make([]byte, 32*1024)
	for index := range block {
		value ^= value << 13
		value ^= value >> 7
		value ^= value << 17
		block[index] = byte(value)
	}
	return block
}

// Fragmentation and short reads cannot change byte identity or alignment.
func syntheticFastValidateChunk(block, chunk []byte, offset int64) bool {
	for len(chunk) > 0 {
		start := int(offset % int64(len(block)))
		count := min(len(chunk), len(block)-start)
		if !bytes.Equal(chunk[:count], block[start:start+count]) {
			return false
		}
		chunk, offset = chunk[count:], offset+int64(count)
	}
	return true
}

// Handler gates expose real accepted requests; they never fabricate transport
// or provider causality. All zero-byte waits observe both request and run owner.
type syntheticFastOrigin struct {
	runs                          [5]atomic.Pointer[syntheticFastOriginRun]
	settings                      syntheticFastSettings
	ctx                           context.Context
	started, finished             atomic.Int64
	streamStarted, streamFinished [8]atomic.Int64
	beforeHeadersForTest          func(context.Context, int) error
	providerForRequestForTest     func(*http.Request) int
}

func syntheticFastWait(ctx, run context.Context, duration time.Duration) error {
	if duration == 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-run.Done():
		return run.Err()
	case <-time.After(duration):
		return nil
	}
}

// Streaming writes retain one32KiB block per handler. Flush keeps first-byte
// timing distinct from a response buffered until handler completion.
func (self *syntheticFastOrigin) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	self.started.Add(1)
	var leaveRun func()
	defer func() {
		self.finished.Add(1)
		if leaveRun != nil {
			leaveRun()
		}
	}()
	runOwner := self.ctx
	var ownedRun *syntheticFastOriginRun
	if text := request.URL.Query().Get("run"); text != "" {
		ordinal, parseErr := strconv.Atoi(text)
		if parseErr != nil || ordinal < 0 || 5 <= ordinal {
			http.Error(writer, "invalid run token", http.StatusBadRequest)
			return
		}
		run := self.runs[ordinal].Load()
		if run == nil {
			http.Error(writer, "unregistered run", http.StatusBadRequest)
			return
		}
		leave, accepted := run.enter()
		if !accepted {
			panic(http.ErrAbortHandler)
		}
		leaveRun = leave
		runOwner = run.ctx
		ownedRun = run
	}
	if ownedRun != nil && request.ProtoMajor == 1 {
		if connection, ok := request.Context().Value(syntheticFastOriginRawConnKey{}).(io.Closer); ok {
			// The callback owns only this HTTP/1 response's raw connection.
			// TLS.Close may write close_notify for another five seconds.
			interrupted := make(chan struct{})
			stopInterrupt := context.AfterFunc(runOwner, func() {
				_ = connection.Close()
				close(interrupted)
			})
			defer func() {
				if !stopInterrupt() {
					<-interrupted
				}
			}()
		}
	}
	writer.Header().Set("Cache-Control", "no-store, no-cache")
	writer.Header().Set("Content-Type", "application/octet-stream")
	if request.URL.Path == "/" {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(writer, syntheticFastFrontend)
		return
	}
	if request.URL.Path == "/loaded-latency" {
		writer.Header().Set("Content-Length", strconv.Itoa(len(syntheticFastSmallBody)))
		_, _ = writer.Write(syntheticFastSmallBody[:])
		return
	}
	if request.URL.Path == "/latency" {
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	stream, err := strconv.Atoi(strings.TrimPrefix(request.URL.Path, "/bulk/"))
	method := http.MethodGet
	if self.settings.Upload {
		method = http.MethodPost
	}
	if request.Method != method || !strings.HasPrefix(request.URL.Path, "/bulk/") || err != nil || stream < 0 || 8 <= stream {
		http.Error(writer, "invalid synthetic stream", http.StatusBadRequest)
		return
	}
	if ownedRun != nil && self.providerForRequestForTest != nil {
		provider := self.providerForRequestForTest(request)
		if 0 <= provider && provider < 7 {
			ownedRun.providerMask.Or(uint32(1) << provider)
			ownedRun.streamProviderMasks[stream].Or(uint32(1) << provider)
		} else {
			ownedRun.providerUnknown.Add(1)
		}
	}
	self.streamStarted[stream].Add(1)
	defer self.streamFinished[stream].Add(1)
	if self.beforeHeadersForTest != nil {
		if err := self.beforeHeadersForTest(request.Context(), stream); err != nil {
			return
		}
	}
	if syntheticFastWait(request.Context(), runOwner, self.settings.StallBeforeHeaders[stream]) != nil {
		return
	}
	if self.settings.Upload {
		self.receiveUpload(writer, request, ownedRun, stream)
		return
	}
	writer.Header().Set("Trailer", "X-Synthetic-Complete")
	flusher, ok := writer.(http.Flusher)
	if !ok {
		http.Error(writer, "streaming required", http.StatusInternalServerError)
		return
	}
	writer.WriteHeader(http.StatusOK)
	flusher.Flush()
	block := syntheticFastBlock(stream)
	written, stalls := int64(0), 0
	for self.settings.BytesPerStream == 0 || written < self.settings.BytesPerStream {
		select {
		case <-runOwner.Done():
			return
		case <-request.Context().Done():
			return
		default:
		}
		threshold := self.settings.StallAfterBytes + int64(stalls)*self.settings.BodyStallEveryBytes
		if stalls < self.settings.BodyStallCount && self.settings.StallAfterBytes >= 0 && written >= threshold {
			stalls++
			if syntheticFastWait(request.Context(), runOwner, self.settings.StallBodyDuration) != nil {
				return
			}
		}
		blockStart := int(written % int64(len(block)))
		count := len(block) - blockStart
		threshold = self.settings.StallAfterBytes + int64(stalls)*self.settings.BodyStallEveryBytes
		if stalls < self.settings.BodyStallCount && self.settings.StallAfterBytes >= 0 && written < threshold {
			count = int(min(int64(count), threshold-written))
		}
		if self.settings.BytesPerStream > 0 {
			count = int(min(int64(count), self.settings.BytesPerStream-written))
		}
		count, err := writer.Write(block[blockStart : blockStart+count])
		written += int64(count)
		if err != nil {
			return
		}
		flusher.Flush()
	}
	writer.Header().Set("X-Synthetic-Complete", "true")
}

type syntheticFastFlow struct {
	Phase                                                                 syntheticFastPhaseSnapshot `json:"phase"`
	Stream                                                                int                        `json:"stream"`
	ProviderMask                                                          uint32                     `json:"provider_mask"`
	Bytes                                                                 int64                      `json:"validated_bytes"`
	StartNs, HeaderFirstByteNs, FirstPayloadNs, LastPayloadNs, ReturnedNs int64
	HeaderTtfbNs, PayloadTtfbNs                                           int64
	OriginAttempts, OriginFinished                                        int64
	Status                                                                int    `json:"http_status"`
	ConnectionReused                                                      bool   `json:"connection_reused"`
	FiniteBodyComplete                                                    bool   `json:"finite_body_complete"`
	Stop                                                                  string `json:"stop"`
	Error                                                                 string `json:"error,omitempty"`
}

type syntheticFastFlowState struct {
	errorAtNs            int64
	ownerCanceledAtError bool
	bytes, first, last   atomic.Int64
	result               syntheticFastFlow
	err                  error
}

type syntheticFastSample struct {
	AtNs                   int64 `json:"at_ns"`
	ValidatedBytes         int64 `json:"validated_bytes"`
	LaunchedStreams        int   `json:"launched_streams"`
	PendingZeroByteStreams int   `json:"pending_zero_byte_streams"`
}

type syntheticFastReport struct {
	Direction                                              string                       `json:"direction"`
	UploadValidation                                       *syntheticFastUploadSnapshot `json:"upload_origin_validation,omitempty"`
	OwnerStartUnixNs                                       int64                        `json:"owner_start_unix_ns,string"`
	MeasurementStartUnixNs                                 int64                        `json:"measurement_start_unix_ns,string"`
	MeasurementEndUnixNs                                   int64                        `json:"measurement_end_unix_ns,string"`
	workerOwner                                            *syntheticFastWorkerOwner
	WorkerJoinState                                        string                `json:"worker_join_state"`
	Valid                                                  bool                  `json:"valid"`
	OverallElapsedNs                                       int64                 `json:"overall_elapsed_ns"`
	WholeRunMbps                                           float64               `json:"whole_run_mbps"`
	Schema                                                 string                `json:"schema"`
	Settings                                               syntheticFastSettings `json:"settings"`
	Flows                                                  []syntheticFastFlow   `json:"flows"`
	Samples                                                []syntheticFastSample `json:"samples"`
	LoadedProbeStartNs                                     []int64               `json:"loaded_probe_start_ns,omitempty"`
	LoadedCanceledProbeNs                                  []int64               `json:"loaded_canceled_probe_ns,omitempty"`
	LoadedFirstPayloadNs                                   []int64               `json:"loaded_first_payload_ns,omitempty"`
	LoadedLatencyNs                                        []int64               `json:"loaded_latency_ns,omitempty"`
	ValidatedBytes                                         int64                 `json:"validated_bytes"`
	MeasurementBytes                                       int64                 `json:"measurement_bytes"`
	FirstPayloadNs, LastPayloadNs, MeasurementNs, JoinedNs int64
	ActiveMbps, WholeMeasurementMbps, WholeJoinedMbps      float64
	Stop                                                   string `json:"stop"`
	Joined                                                 bool   `json:"joined"`
	WarmupNs                                               int64  `json:"warmup_ns"`
	LoadedLatencyError                                     string `json:"loaded_latency_error,omitempty"`
}

// A timed-out controller retains this exact terminal receipt. Its adapter
// closes the transport/fixture first and must then consume the bounded drain.
type syntheticFastWorkerOwner struct {
	done        chan struct{}
	completedAt time.Time
}

func (self *syntheticFastWorkerOwner) join(ctx context.Context) error {
	completed := func() error {
		if deadline, ok := ctx.Deadline(); ok && self.completedAt.After(deadline) {
			return context.DeadlineExceeded
		}
		return nil
	}
	select {
	case <-self.done:
		return completed()
	default:
	}
	select {
	case <-self.done:
		return completed()
	case <-ctx.Done():
		select {
		case <-self.done:
			return completed()
		default:
			return ctx.Err()
		}
	}
}

// Each read validates real response bytes. Intentional measurement cancellation
// is recorded separately from EOF, malformed protocol and a parent cancellation.
func syntheticFastDownload(ctx context.Context, client *http.Client, target string, stream int, started time.Time, state *syntheticFastFlowState) {
	result := syntheticFastFlow{Stream: stream, StartNs: int64(time.Since(started))}
	captureConnections, _ := ctx.Value(syntheticFastPlacementContextKey{}).(bool)
	phase := &syntheticFastPhaseObserver{origin: started, captureConnections: captureConnections}
	var headerNs atomic.Int64
	var reused atomic.Bool
	defer func() {
		result.Phase = phase.snapshot()
		result.Bytes = state.bytes.Load()
		result.FirstPayloadNs = state.first.Load()
		result.LastPayloadNs = state.last.Load()
		result.ReturnedNs = int64(time.Since(started))
		result.HeaderFirstByteNs = headerNs.Load()
		result.ConnectionReused = reused.Load()
		if result.HeaderFirstByteNs > 0 {
			result.HeaderTtfbNs = result.HeaderFirstByteNs - result.StartNs
		}
		if result.FirstPayloadNs > 0 {
			result.PayloadTtfbNs = result.FirstPayloadNs - result.StartNs
		}
		state.result = result
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		state.err = err
		state.errorAtNs = int64(time.Since(started))
		state.ownerCanceledAtError = ctx.Err() != nil
		result.Error = err.Error()
		return
	}
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() { headerNs.CompareAndSwap(0, max(1, int64(time.Since(started)))) },
		GotConn:              func(info httptrace.GotConnInfo) { reused.Store(info.Reused) },
	}
	phase.install(trace)
	request = request.WithContext(context.WithValue(httptrace.WithClientTrace(request.Context(), trace), syntheticFastPhaseContextKey{}, phase))
	response, err := client.Do(request)
	if err != nil {
		state.err = err
		state.errorAtNs = int64(time.Since(started))
		state.ownerCanceledAtError = ctx.Err() != nil
		result.Error = err.Error()
		return
	}
	defer response.Body.Close()
	result.Status = response.StatusCode
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Encoding") != "" || response.Header.Get("Cache-Control") != "no-store, no-cache" {
		result.Error = "synthetic HTTP status/encoding/cache contract changed"
		return
	}
	buffer, block := make([]byte, 32*1024), syntheticFastBlock(stream)
	offset := int64(0)
	for {
		count, readErr := response.Body.Read(buffer)
		if count > 0 {
			if !syntheticFastValidateChunk(block, buffer[:count], offset) {
				result.Error = "synthetic body mismatch"
				return
			}
			now := max(1, int64(time.Since(started)))
			state.first.CompareAndSwap(0, now)
			state.last.Store(now)
			offset += int64(count)
			state.bytes.Store(offset)
		}
		if readErr == io.EOF {
			result.FiniteBodyComplete = response.Trailer.Get("X-Synthetic-Complete") == "true"
			if !result.FiniteBodyComplete {
				result.Error = "unexpected body EOF without completion trailer"
			}
			return
		}
		if readErr != nil {
			state.err = readErr
			state.errorAtNs = int64(time.Since(started))
			state.ownerCanceledAtError = ctx.Err() != nil
			result.Error = readErr.Error()
			return
		}
	}
}

// The adapter owns origins/transport; this controller owns every download and
// latency worker, joins them before returning, and retains at most121 samples.
func measureSyntheticFast(ctx context.Context, client *http.Client, urlForStream func(int) string, settings syntheticFastSettings, sample func(), originRuns ...*syntheticFastOriginRun) (syntheticFastReport, error) {
	report := syntheticFastReport{Schema: "perfvar-synthetic-fast-v5-phase1", Direction: "download", Settings: settings}
	var originRun *syntheticFastOriginRun
	var upload *syntheticFastUploadRun
	if settings.Upload {
		if len(originRuns) != 1 || originRuns[0] == nil || originRuns[0].upload == nil {
			return report, errors.New("upload requires its exact owned origin run")
		}
		originRun, upload = originRuns[0], originRuns[0].upload
		report.Direction, report.Schema = "upload", "perfvar-synthetic-fast-upload-v1"
	}
	if err := settings.validate(); err != nil {
		return report, err
	}
	overallStarted := time.Now()
	report.OwnerStartUnixNs = overallStarted.UnixNano()
	parent := ctx
	ctx, stopOverall := context.WithTimeout(ctx, settings.OverallTimeout)
	defer stopOverall()
	if settings.Warm {
		warmStarted := time.Now()
		warmCtx, cancelWarm := context.WithTimeout(ctx, settings.StartupTimeout)
		defer cancelWarm()
		for index := range 4 {
			request, _ := http.NewRequestWithContext(warmCtx, http.MethodGet, syntheticFastLatencyUrl(urlForStream(index)), nil)
			response, err := client.Do(request)
			if err != nil {
				return report, err
			}
			response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				return report, errors.New("warmup latency status changed")
			}
		}
		report.WarmupNs = int64(time.Since(warmStarted))
		cancelWarm()
	}
	started := time.Now()
	report.MeasurementStartUnixNs = started.UnixNano()
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if upload != nil {
		upload.start(workCtx, started)
	}
	var workers sync.WaitGroup
	states := make([]*syntheticFastFlowState, 0, 8)
	launch := func(count int) {
		for len(states) < count {
			index, state := len(states), &syntheticFastFlowState{}
			states = append(states, state)
			if upload != nil {
				upload.register(index, state)
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				if upload != nil {
					syntheticFastUpload(workCtx, client, urlForStream(index), index, started, state, settings.BytesPerStream)
				} else {
					syntheticFastDownload(workCtx, client, urlForStream(index), index, started, state)
				}
			}()
		}
	}
	launch(settings.InitialStreams)
	var loadedLatencyNs, loadedFirstPayloadNs, loadedProbeStartNs, loadedCanceledProbeNs []int64
	var loadedLatencyError string
	if settings.LoadedLatency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-workCtx.Done():
					return
				case <-time.After(settings.SampleInterval):
				}
				probeStarted := time.Now()
				loadedProbeStartNs = append(loadedProbeStartNs, int64(probeStarted.Sub(started)))
				first, complete, err := syntheticFastSmallGet(workCtx, client, syntheticFastLatencyUrl(urlForStream(0)))
				if err != nil {
					if workCtx.Err() == nil {
						loadedLatencyError = err.Error()
					} else {
						loadedCanceledProbeNs = append(loadedCanceledProbeNs, int64(time.Since(probeStarted)))
					}
					return
				}
				loadedFirstPayloadNs = append(loadedFirstPayloadNs, int64(first))
				loadedLatencyNs = append(loadedLatencyNs, int64(complete))
			}
		}()
	}
	window := time.NewTimer(settings.Duration)
	defer window.Stop()
	ticker := time.NewTicker(settings.SampleInterval)
	defer ticker.Stop()
	previousTotal, previousInterval := int64(0), int64(-1)
loop:
	for {
		select {
		case <-ctx.Done():
			if errors.Is(parent.Err(), context.Canceled) {
				report.Stop = "parent_canceled"
			} else {
				report.Stop = "overall_deadline"
			}
			break loop
		case <-window.C:
			report.Stop = "measurement_window"
			break loop
		case <-ticker.C:
			total, pending := int64(0), 0
			for _, state := range states {
				total += state.bytes.Load()
				if state.first.Load() == 0 {
					pending++
				}
			}
			report.Samples = append(report.Samples, syntheticFastSample{AtNs: int64(time.Since(started)), ValidatedBytes: total, LaunchedStreams: len(states), PendingZeroByteStreams: pending})
			if sample != nil {
				sample()
			}
			next := syntheticFastNextStreams(len(states), settings.MaximumStreams, settings.Adaptive, total-previousTotal, previousInterval, pending)
			previousInterval, previousTotal = total-previousTotal, total
			launch(next)
		}
	}
	if ctx.Err() != nil {
		if errors.Is(parent.Err(), context.Canceled) {
			report.Stop = "parent_canceled"
		} else {
			report.Stop = "overall_deadline"
		}
	}
	if upload != nil {
		report.MeasurementNs, report.MeasurementEndUnixNs, report.MeasurementBytes = upload.measurementBoundary()
	} else {
		report.MeasurementNs = int64(time.Since(started))
		report.MeasurementEndUnixNs = time.Now().UnixNano()
		for _, state := range states {
			report.MeasurementBytes += state.bytes.Load()
		}
	}
	cancel()
	owner := &syntheticFastWorkerOwner{done: make(chan struct{})}
	report.workerOwner = owner
	go func() {
		workers.Wait()
		if originRun != nil {
			// The existing bounded owner retains this drain if the first join expires.
			_ = originRun.sealAndWait(context.Background())
		}
		owner.completedAt = time.Now()
		close(owner.done)
	}()
	var resultErr error
	joinDeadline := time.Now().Add(settings.RunDrainTimeout)
	if overallDeadline, ok := ctx.Deadline(); ok && overallDeadline.Before(joinDeadline) {
		joinDeadline = overallDeadline
	}
	joinCtx, stopJoin := context.WithDeadline(context.Background(), joinDeadline)
	joinErr := owner.join(joinCtx)
	stopJoin()
	if joinErr != nil {
		report.WorkerJoinState = "UNSAFE_WORKERS_OWNED_AT_JOIN_DEADLINE"
		if report.Stop == "measurement_window" {
			report.Stop = "worker_join_deadline_or_cancel"
		}
		resultErr = errors.Join(resultErr, joinErr)
	} else {
		report.Joined, report.JoinedNs = true, int64(time.Since(started))
		report.WorkerJoinState = "WORKERS_JOINED"
		report.LoadedProbeStartNs, report.LoadedCanceledProbeNs = loadedProbeStartNs, loadedCanceledProbeNs
		report.LoadedFirstPayloadNs = loadedFirstPayloadNs
		report.LoadedLatencyNs, report.LoadedLatencyError = loadedLatencyNs, loadedLatencyError
		if settings.LoadedLatency && (loadedLatencyError != "" || len(loadedLatencyNs) == 0) {
			resultErr = errors.Join(resultErr, fmt.Errorf("mandatory loaded small GET: completed=%d error=%s", len(loadedLatencyNs), loadedLatencyError))
		}
	}
	// A simultaneous completion cannot turn a deadline-crossing join into a
	// valid window. No mutable worker result is read before its terminal edge.
	if ctx.Err() != nil {
		resultErr = errors.Join(resultErr, ctx.Err())
		if report.Joined && report.Stop == "measurement_window" {
			report.Stop = "overall_deadline_or_cancel_after_join"
		}
	}
	if upload != nil && report.Joined {
		value, validationErr := upload.snapshot()
		report.UploadValidation = &value
		resultErr = errors.Join(resultErr, validationErr)
	}
	for index, state := range states {
		if !report.Joined {
			flow := syntheticFastFlow{Stream: index, Bytes: state.bytes.Load(), FirstPayloadNs: state.first.Load(), LastPayloadNs: state.last.Load(), Stop: "UNSAFE_WORKER_NOT_JOINED", Error: "immutable partial snapshot; worker still owned"}
			report.ValidatedBytes += flow.Bytes
			report.Flows = append(report.Flows, flow)
			continue
		}
		flow := state.result
		if upload != nil {
			// Final origin accounting is read only after both worker domains join.
			flow.Bytes, flow.FirstPayloadNs, flow.LastPayloadNs = state.bytes.Load(), state.first.Load(), state.last.Load()
			if flow.FirstPayloadNs > 0 {
				flow.PayloadTtfbNs = flow.FirstPayloadNs - flow.StartNs
			}
		}
		if flow.Error != "" {
			if report.Stop == "measurement_window" && state.ownerCanceledAtError && state.errorAtNs >= report.MeasurementNs && (errors.Is(state.err, context.Canceled) || errors.Is(state.err, context.DeadlineExceeded)) {
				flow.Stop = "measurement_window"
			} else {
				flow.Stop = "error"
				resultErr = errors.Join(resultErr, fmt.Errorf("stream%d: %s", flow.Stream, flow.Error))
			}
		} else {
			flow.Stop = "finite_body_complete"
		}
		if flow.FiniteBodyComplete && settings.BytesPerStream > 0 && flow.Bytes != settings.BytesPerStream {
			resultErr = errors.Join(resultErr, errors.New("finite synthetic body length changed"))
		}
		report.ValidatedBytes += flow.Bytes
		if flow.FirstPayloadNs > 0 && (report.FirstPayloadNs == 0 || flow.FirstPayloadNs < report.FirstPayloadNs) {
			report.FirstPayloadNs = flow.FirstPayloadNs
		}
		report.LastPayloadNs = max(report.LastPayloadNs, flow.LastPayloadNs)
		report.Flows = append(report.Flows, flow)
	}
	if report.Stop == "parent_canceled" || report.Stop == "overall_deadline" {
		resultErr = errors.Join(resultErr, ctx.Err())
	}
	if report.LastPayloadNs > report.FirstPayloadNs {
		report.ActiveMbps = float64(report.ValidatedBytes) * 8 * 1000 / float64(report.LastPayloadNs-report.FirstPayloadNs)
	}
	if report.MeasurementNs > 0 {
		report.WholeMeasurementMbps = float64(report.MeasurementBytes) * 8 * 1000 / float64(report.MeasurementNs)
	}
	if report.JoinedNs > 0 {
		report.WholeJoinedMbps = float64(report.ValidatedBytes) * 8 * 1000 / float64(report.JoinedNs)
	}
	if report.MeasurementBytes == 0 && report.Stop == "measurement_window" {
		resultErr = errors.Join(resultErr, errors.New("measurement ended without payload"))
	}
	report.OverallElapsedNs = int64(time.Since(overallStarted))
	if report.OverallElapsedNs > 0 {
		report.WholeRunMbps = float64(report.ValidatedBytes) * 8 * 1000 / float64(report.OverallElapsedNs)
	}
	report.Valid = resultErr == nil && report.Joined && report.Stop == "measurement_window" && report.MeasurementBytes > 0
	return report, resultErr
}

// A finite 16-KiB object separates payload first-byte and completion latency.
// The startup-only /latency endpoint keeps its old 204 contract.
var syntheticFastSmallBody = func() (body [16 * 1024]byte) {
	for i := range body {
		body[i] = byte(i)
	}
	return
}()

func syntheticFastSmallGet(ctx context.Context, client *http.Client, latencyURL string) (time.Duration, time.Duration, error) {
	address := strings.Replace(latencyURL, "/latency", "/loaded-latency", 1)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return 0, 0, err
	}
	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		return 0, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("small GET status %d", response.StatusCode)
	}
	var body [16 * 1024]byte
	if _, err := io.ReadFull(response.Body, body[:1]); err != nil {
		return 0, 0, err
	}
	first := time.Since(started)
	if _, err := io.ReadFull(response.Body, body[1:]); err != nil {
		return first, 0, err
	}
	var extra [1]byte
	n, err := io.ReadFull(response.Body, extra[:])
	if n != 0 || err != io.EOF {
		return first, 0, fmt.Errorf("small GET missing exact EOF: bytes=%d error=%v", n, err)
	}
	if !bytes.Equal(body[:], syntheticFastSmallBody[:]) {
		return first, 0, errors.New("small GET corrupt body")
	}
	return first, time.Since(started), nil
}

// Leave one connection for the mandatory probe while all eight bulk streams
// are active. This removes client-pool waiting from the network latency guard.
func syntheticFastConfigureHttpPool(transport *http.Transport, settings syntheticFastSettings) {
	maximum := 8
	if settings.LoadedLatency {
		maximum++
	}
	transport.MaxConnsPerHost, transport.MaxIdleConnsPerHost, transport.MaxIdleConns = maximum, maximum, 32
}
