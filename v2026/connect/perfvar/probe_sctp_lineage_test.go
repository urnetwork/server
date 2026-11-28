//go:build acklineagetrace

package perfvar

import (
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	clientconnect "github.com/urnetwork/connect/v2026"
)

const probeSctpCapacity = 16384

// Exact source-format matching reads only bounded numeric arguments. Names
// become a local hash; private SACK chunks, errors and packet bytes are never
// inspected, formatted or retained. Float RTT values are logged in milliseconds
// by SCTP and are converted to integer nanoseconds here.
type probeSctpSchema struct {
	stage       string
	format      string
	columns     []string
	argumentLen int
	indices     [7]int
	boolMask    uint8
	millisMask  uint8
}

func newProbeSctpSchema(stage, format string, columns ...string) probeSctpSchema {
	schema := probeSctpSchema{stage: stage, format: "[pion:sctp]" + format, columns: columns, argumentLen: len(columns) + 1}
	for index := range columns {
		schema.indices[index] = index + 1
	}
	return schema
}

var probeSctpSchemas = func() []probeSctpSchema {
	result := []probeSctpSchema{
		newProbeSctpSchema("cwnd-initial", "[%s] updated cwnd=%d ssthresh=%d inflight=%d (INI)", "cwnd_bytes", "ssthresh_bytes", "inflight_bytes"),
		newProbeSctpSchema("rwnd-initial", "[%s] initial rwnd=%d", "rwnd_bytes"),
		newProbeSctpSchema("data-send", "[%s] sending ppi=%d tsn=%d ssn=%d sent=%d len=%d (%v,%v)", "ppi", "tsn", "ssn", "send_count", "bytes", "begin_fragment", "end_fragment"),
		newProbeSctpSchema("data-receive", "[%s] DATA: tsn=%d immediateSack=%v len=%d", "tsn", "immediate_sack", "bytes"),
		newProbeSctpSchema("sack-receive", "[%s] SACK: cumTSN=%d a_rwnd=%d", "cumulative_tsn", "advertised_rwnd_bytes"),
		newProbeSctpSchema("sack-cumulative-advance", "[%s] SACK: cumTSN advanced: %d -> %d", "previous_tsn", "cumulative_tsn"),
		newProbeSctpSchema("sack-gap", "[%s] tsn=%d has been sacked", "tsn"),
		newProbeSctpSchema("sack-old", "[%s] SACK Cumulative ACK %v is older than ACK point %v", "received_tsn", "current_tsn"),
		newProbeSctpSchema("cwnd-slow-start", "[%s] updated cwnd=%d ssthresh=%d acked=%d (SS)", "cwnd_bytes", "ssthresh_bytes", "acked_bytes"),
		newProbeSctpSchema("cwnd-avoidance", "[%s] updated cwnd=%d ssthresh=%d acked=%d (CA)", "cwnd_bytes", "ssthresh_bytes", "acked_bytes"),
		newProbeSctpSchema("cwnd-fast-recovery", "[%s] updated cwnd=%d ssthresh=%d inflight=%d (FR)", "cwnd_bytes", "ssthresh_bytes", "inflight_bytes"),
		newProbeSctpSchema("cwnd-rto", "[%s] updated cwnd=%d ssthresh=%d inflight=%d (RTO)", "cwnd_bytes", "ssthresh_bytes", "inflight_bytes"),
		newProbeSctpSchema("cwnd-unchanged", "[%s] cwnd did not grow: cwnd=%d ssthresh=%d acked=%d FR=%v pending=%d", "cwnd_bytes", "ssthresh_bytes", "acked_bytes", "fast_recovery", "pending_chunks"),
		newProbeSctpSchema("rtt-sack", "[%s] SACK: measured-rtt=%f srtt=%f new-rto=%f", "rtt_nanos", "srtt_nanos", "rto_nanos"),
		newProbeSctpSchema("rtt-heartbeat", "[%s] HB RTT: measured=%.3fms srtt=%.3fms rto=%.3fms", "rtt_nanos", "srtt_nanos", "rto_nanos"),
		newProbeSctpSchema("inflight-empty", "[%s] SACK: no more packet in-flight (pending=%d)", "pending_chunks"),
		newProbeSctpSchema("pending-drained", "[%s] all pending data have been sent, notify writable"),
		newProbeSctpSchema("t3-start-data", "[%s] T3-rtx timer start (pt1)"),
		newProbeSctpSchema("t3-start-ack", "[%s] T3-rtx timer start (pt2)"),
		newProbeSctpSchema("t3-start-post-ack", "[%s] T3-rtx timer start (pt3)"),
		newProbeSctpSchema("t3-timeout", "[%s] T3-rtx timed out: nRtos=%d cwnd=%d ssthresh=%d", "timeout_count", "cwnd_bytes", "ssthresh_bytes"),
		newProbeSctpSchema("retransmit-fast", "[%s] fast-retransmit: tsn=%d sent=%d htna=%d", "tsn", "send_count", "recovery_exit_tsn"),
		newProbeSctpSchema("retransmit", "[%s] retransmitting tsn=%d ssn=%d sent=%d", "tsn", "ssn", "send_count"),
		newProbeSctpSchema("rack-timer-loss", "[%s] RACK timer: mark lost tsn=%d", "tsn"),
		newProbeSctpSchema("rack-ack-loss", "[%s] RACK: mark lost tsn=%d (sent=%v, delivered=%v, reoWnd=%v)", "tsn", "reorder_window_nanos"),
		newProbeSctpSchema("rack-duplicate", "[%s] RACK: DSACK/dupTSN seen, inflate reoWnd to %v", "reorder_window_nanos"),
		newProbeSctpSchema("pto-probe", "[%s] PTO fired: probe tsn=%d", "tsn"),
		newProbeSctpSchema("sack-scheduled", "[%s] sending SACK: %s"),
	}
	for index := range result {
		schema := &result[index]
		switch schema.stage {
		case "data-send":
			schema.boolMask = 1<<5 | 1<<6
		case "data-receive":
			schema.boolMask = 1 << 1
		case "cwnd-unchanged":
			schema.boolMask = 1 << 3
		case "rtt-sack", "rtt-heartbeat":
			schema.millisMask = 7
		case "rack-ack-loss":
			// The time.Time arguments are intentionally not inspected.
			schema.argumentLen, schema.indices[1] = 5, 4
		case "sack-scheduled":
			// The private chunk/stringer is not invoked. This proves scheduling,
			// not successful serialization, physical write or peer delivery.
			schema.argumentLen = 2
		}
	}
	return result
}()

var probeSctpSchemaByFormat = func() map[string]uint8 {
	result := make(map[string]uint8, len(probeSctpSchemas))
	for index, schema := range probeSctpSchemas {
		result[schema.format] = uint8(index)
	}
	return result
}()

type probeSctpEvent struct {
	atNano      int64
	association uint64
	values      [7]int64
	schema      uint8
}

type probeSctpRecorder struct {
	next        atomic.Uint64
	malformed   atomic.Uint64
	configured  atomic.Uint64
	unsupported atomic.Uint64
	stageCounts [32]atomic.Uint64
	events      [probeSctpCapacity]probeSctpEvent
	published   [probeSctpCapacity]atomic.Bool
}

func probeSctpAssociationHash(name any) (uint64, bool) {
	value, ok := name.(string)
	if !ok || len(value) == 0 || len(value) > 64 {
		return 0, false
	}
	hash := uint64(14695981039346656037)
	for index := range len(value) {
		hash = (hash ^ uint64(value[index])) * 1099511628211
	}
	return hash, true
}

func probeSctpInteger(value any) (int64, bool) {
	// Reflection accepts SCTP's named uint32/int types without invoking any
	// user-defined Error/String/As methods or retaining the original argument.
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return reflected.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		unsigned := reflected.Uint()
		if unsigned <= math.MaxInt64 {
			return int64(unsigned), true
		}
	}
	return 0, false
}

func probeSctpValue(value any, boolean, milliseconds bool) (int64, bool) {
	if boolean {
		flag, ok := value.(bool)
		if flag {
			return 1, ok
		}
		return 0, ok
	}
	if milliseconds {
		millis, ok := value.(float64)
		if !ok || math.IsNaN(millis) || math.IsInf(millis, 0) || millis < 0 || millis >= float64(math.MaxInt64)/1e6 {
			return 0, false
		}
		return int64(millis * 1e6), true
	}
	return probeSctpInteger(value)
}

func (self *probeSctpRecorder) observeFormat(format string, args []any) {
	// Do not inspect arguments of unselected formats. Limiting the format
	// length also bounds lookup work for accidental arbitrary string input.
	if self == nil || len(format) > 200 {
		return
	}
	index, selected := probeSctpSchemaByFormat[format]
	if !selected {
		return
	}
	schema := &probeSctpSchemas[index]
	if len(args) != schema.argumentLen {
		self.malformed.Add(1)
		return
	}
	association, ok := probeSctpAssociationHash(args[0])
	if !ok {
		self.malformed.Add(1)
		return
	}
	event := probeSctpEvent{schema: index, association: association}
	for field := range schema.columns {
		value, valid := probeSctpValue(args[schema.indices[field]], schema.boolMask&(1<<field) != 0, schema.millisMask&(1<<field) != 0)
		if !valid {
			self.malformed.Add(1)
			return
		}
		event.values[field] = value
	}
	event.atNano = time.Now().UnixNano()
	self.stageCounts[index].Add(1)
	self.observe(event)
}

func (self *probeSctpRecorder) observe(event probeSctpEvent) {
	index := self.next.Add(1) - 1
	if index < probeSctpCapacity {
		self.events[index] = event
		self.published[index].Store(true)
	}
}

func (self *probeSctpRecorder) counts() (reserved, overflow, unpublished uint64) {
	reserved = self.next.Load()
	if probeSctpCapacity < reserved {
		overflow = reserved - probeSctpCapacity
	}
	for index := uint64(0); index < min(reserved, probeSctpCapacity); index++ {
		if !self.published[index].Load() {
			unpublished++
		}
	}
	return
}

func (self *probeSctpRecorder) dump(t testing.TB, role string) {
	reserved, overflow, unpublished := self.counts()
	columns := make(map[string][]string, len(probeSctpSchemas))
	counts := make(map[string]uint64, len(probeSctpSchemas))
	for index, schema := range probeSctpSchemas {
		columns[schema.stage] = schema.columns
		counts[schema.stage] = self.stageCounts[index].Load()
	}
	header, _ := json.Marshal(map[string]any{
		"kind": "probe-sctp-header", "role": role, "capacity": probeSctpCapacity,
		"reserved_prefix": reserved, "overflow": overflow, "unpublished": unpublished,
		"malformed": self.malformed.Load(), "configured": self.configured.Load(), "unsupported": self.unsupported.Load(),
		"first_events": true, "baseline_eligible": false, "recorder_bytes": unsafe.Sizeof(*self),
		"snapshot_before_carrier_teardown": true, "columns": columns, "stage_counts": counts,
		"association_identity": "process-local-name-hash", "values": "signed-integer-strings; booleans=0/1",
		"limitation": "SACK scheduling is not delivery; TLR budget is not directly observed",
	})
	t.Logf("[probe-sctp] %s", header)
	for ordinal := uint64(0); ordinal < min(reserved, probeSctpCapacity); ordinal++ {
		if !self.published[ordinal].Load() {
			continue
		}
		event := self.events[ordinal]
		schema := &probeSctpSchemas[event.schema]
		var values [7]string
		for index := range schema.columns {
			values[index] = strconv.FormatInt(event.values[index], 10)
		}
		row, _ := json.Marshal(struct {
			Kind        string   `json:"kind"`
			Role        string   `json:"role"`
			Ordinal     uint64   `json:"ordinal"`
			Stage       string   `json:"stage"`
			AtNano      int64    `json:"at_unix_nano,string"`
			Association uint64   `json:"association,string"`
			Values      []string `json:"values"`
		}{"probe-sctp-event", role, ordinal, schema.stage, event.atNano, event.association, values[:len(schema.columns)]})
		t.Logf("[probe-sctp] %s", row)
	}
	if overflow != 0 || unpublished != 0 || self.malformed.Load() != 0 || self.unsupported.Load() != 0 || self.configured.Load() == 0 {
		t.Errorf("SCTP evidence incomplete: role=%s reserved=%d overflow=%d unpublished=%d malformed=%d configured=%d unsupported=%d",
			role, reserved, overflow, unpublished, self.malformed.Load(), self.configured.Load(), self.unsupported.Load())
	}
	for _, stage := range []string{"data-send", "data-receive", "sack-receive", "pending-drained"} {
		if counts[stage] == 0 {
			t.Errorf("SCTP evidence missing required stage: role=%s stage=%s", role, stage)
		}
	}
}

type probeSctpLogger struct {
	clientconnect.Logger
	recorder *probeSctpRecorder
	verbose  probeSctpVerbose
}

type probeSctpVerbose struct{ recorder *probeSctpRecorder }

func (self *probeSctpVerbose) Enabled() bool { return true }
func (self *probeSctpVerbose) Info(...any)   {}
func (self *probeSctpVerbose) Infof(format string, args ...any) {
	self.recorder.observeFormat(format, args)
}

func (self *probeSctpLogger) V(level int32) clientconnect.Verbose {
	if level == 2 {
		return &self.verbose
	}
	return self.Logger.V(level)
}

func (self *probeSctpLogger) Infof(format string, args ...any) {
	// Pion's unstructured Trace/Debug/Info paths share this format and cannot
	// be distinguished by level. Drop that opaque string form locally; never
	// promote it into global verbose output or inspect a message Stringer.
	if format == "[pion:%s]%s" {
		return
	}
	self.Logger.Infof(format, args...)
}

func (self *probeSctpRecorder) configure(settings *clientconnect.ClientSettings) {
	if settings.WebRtcSettings == nil {
		self.unsupported.Add(1)
		return
	}
	webRtc := *settings.WebRtcSettings
	original := webRtc.Log
	if original == nil {
		original = settings.Log
	}
	if original == nil {
		original = clientconnect.DefaultLogger()
	}
	webRtc.Log = &probeSctpLogger{Logger: original, recorder: self, verbose: probeSctpVerbose{recorder: self}}
	settings.WebRtcSettings = &webRtc
	self.configured.Add(1)
}

func newProbeSctpLineageTrace() *perfvarProgressTrace {
	trace := newAckReplayTrace()
	recorder := &probeSctpRecorder{}
	trace.configureForTest = recorder.configure
	ackDump := trace.dumpForTest
	trace.dumpForTest = func(t testing.TB, role string) {
		ackDump(t, role)
		recorder.dump(t, role)
	}
	return trace
}

// Every selected format gets its real argument shape; opaque arguments prove
// we neither inspect skipped fields nor format unselected logs.
type unreadableProbeSctpValue struct{}

func (unreadableProbeSctpValue) String() string { panic("SCTP diagnostic formatted opaque value") }
func (unreadableProbeSctpValue) Error() string  { panic("SCTP diagnostic formatted opaque error") }

func TestProbeSctpMetadataFormats(t *testing.T) {
	if len(probeSctpSchemas) > len(probeSctpRecorder{}.stageCounts) || len(probeSctpSchemaByFormat) != len(probeSctpSchemas) {
		t.Fatal("SCTP schema exceeds bounds or has duplicate formats")
	}
	type nativeInteger uint32
	for index, schema := range probeSctpSchemas {
		t.Run(schema.stage, func(t *testing.T) {
			recorder := &probeSctpRecorder{}
			args := make([]any, schema.argumentLen)
			args[0] = "local-association"
			for index := 1; index < len(args); index++ {
				args[index] = unreadableProbeSctpValue{}
			}
			var want [7]int64
			for field := range schema.columns {
				args[schema.indices[field]], want[field] = nativeInteger(42), 42
				if schema.boolMask&(1<<field) != 0 {
					args[schema.indices[field]], want[field] = true, 1
				}
				if schema.millisMask&(1<<field) != 0 {
					args[schema.indices[field]], want[field] = float64(1.25), 1250000
				}
			}
			recorder.observeFormat(schema.format, args)
			if recorder.next.Load() != 1 || recorder.malformed.Load() != 0 ||
				recorder.events[0].schema != uint8(index) || recorder.events[0].values != want || recorder.events[0].atNano == 0 {
				t.Fatalf("numeric metadata mismatch for stage=%s", schema.stage)
			}
			if got := testing.AllocsPerRun(100, func() { recorder.next.Store(0); recorder.observeFormat(schema.format, args) }); got != 0 {
				t.Fatalf("numeric observation allocated %g objects", got)
			}
		})
	}
	recorder := &probeSctpRecorder{}
	recorder.observeFormat("unselected", []any{unreadableProbeSctpValue{}})
	var disabled *probeSctpRecorder
	disabled.observeFormat(probeSctpSchemas[0].format, []any{unreadableProbeSctpValue{}})
	if recorder.next.Load() != 0 || recorder.malformed.Load() != 0 {
		t.Fatal("unselected logging inspected arguments")
	}
}

func TestProbeSctpRejectsMalformedNumericArguments(t *testing.T) {
	for _, value := range []any{nil, unreadableProbeSctpValue{}, "not-numeric", uint64(math.MaxUint64)} {
		if _, ok := probeSctpInteger(value); ok {
			t.Fatal("opaque or out-of-range number accepted")
		}
	}
	for _, value := range []any{nil, -1.0, math.NaN(), math.Inf(1), float64(math.MaxInt64)} {
		if _, ok := probeSctpValue(value, false, true); ok {
			t.Fatal("invalid millisecond number accepted")
		}
	}
	recorder := &probeSctpRecorder{}
	recorder.observeFormat(probeSctpSchemas[0].format, nil)
	recorder.observeFormat(probeSctpSchemas[0].format, []any{unreadableProbeSctpValue{}, 1, 2, 3})
	recorder.observeFormat(probeSctpSchemas[0].format, []any{"local", unreadableProbeSctpValue{}, 2, 3})
	if recorder.next.Load() != 0 || recorder.malformed.Load() != 3 {
		t.Fatal("malformed selected events were not explicit")
	}
}

func TestProbeSctpRecorderBoundedConcurrentPublication(t *testing.T) {
	recorder := &probeSctpRecorder{}
	var workers sync.WaitGroup
	for worker := range 4 {
		workers.Go(func() {
			for index := range probeSctpCapacity / 4 {
				recorder.observe(probeSctpEvent{association: uint64(worker*probeSctpCapacity/4 + index)})
			}
		})
	}
	workers.Wait()
	if reserved, overflow, unpublished := recorder.counts(); reserved != probeSctpCapacity || overflow != 0 || unpublished != 0 {
		t.Fatalf("SCTP recorder counts=%d/%d/%d", reserved, overflow, unpublished)
	}
	var seen [probeSctpCapacity]bool
	for _, event := range recorder.events {
		if event.association >= probeSctpCapacity || seen[event.association] {
			t.Fatal("concurrent publication lost an event")
		}
		seen[event.association] = true
	}
	recorder.observe(probeSctpEvent{})
	if _, overflow, _ := recorder.counts(); overflow != 1 {
		t.Fatal("overflow not explicit")
	}
	recorder = &probeSctpRecorder{}
	recorder.next.Store(1)
	if _, _, unpublished := recorder.counts(); unpublished != 1 {
		t.Fatal("unpublished event not explicit")
	}
}

type probeSctpTestLog struct{ info, warning, errors, verbose atomic.Uint64 }

func (self *probeSctpTestLog) Info(...any)                   { self.info.Add(1) }
func (self *probeSctpTestLog) Infof(string, ...any)          { self.info.Add(1) }
func (self *probeSctpTestLog) Warningf(string, ...any)       { self.warning.Add(1) }
func (self *probeSctpTestLog) Errorf(string, ...any)         { self.errors.Add(1) }
func (self *probeSctpTestLog) V(int32) clientconnect.Verbose { return probeSctpTestVerbose{self} }

type probeSctpTestVerbose struct{ log *probeSctpTestLog }

func (self probeSctpTestVerbose) Enabled() bool        { return false }
func (self probeSctpTestVerbose) Info(...any)          { self.log.verbose.Add(1) }
func (self probeSctpTestVerbose) Infof(string, ...any) { self.log.verbose.Add(1) }

func TestProbeSctpLoggerIsEndpointLocal(t *testing.T) {
	base := &probeSctpTestLog{}
	settings := clientconnect.DefaultClientSettings()
	settings.WebRtcSettings.Log = base
	shared := settings.WebRtcSettings
	recorder := &probeSctpRecorder{}
	recorder.configure(settings)
	logger := settings.WebRtcSettings.Log
	if settings.WebRtcSettings == shared || shared.Log != base || settings.Log != nil ||
		recorder.configured.Load() != 1 || !logger.V(2).Enabled() || base.V(2).Enabled() || logger.V(1).Enabled() {
		t.Fatal("SCTP logging escaped its endpoint settings")
	}
	logger.Info("preserved")
	logger.Infof("preserved")
	logger.Warningf("preserved")
	logger.Errorf("preserved")
	logger.V(1).Infof("preserved")
	logger.V(2).Infof("unselected", unreadableProbeSctpValue{})
	logger.V(2).Info(unreadableProbeSctpValue{})
	logger.Infof("[pion:%s]%s", "sctp", unreadableProbeSctpValue{})
	logger.V(2).Infof(probeSctpSchemas[0].format, "local", uint32(1191), uint32(4764), 1163)
	if base.info.Load() != 2 || base.warning.Load() != 1 || base.errors.Load() != 1 ||
		base.verbose.Load() != 1 || recorder.next.Load() != 1 || recorder.malformed.Load() != 0 {
		t.Fatal("SCTP logger changed ordinary delegation or emitted verbose output")
	}
	if got := testing.AllocsPerRun(100, func() { _ = logger.V(2).Enabled() }); got != 0 {
		t.Fatalf("logger level check allocated %g objects", got)
	}
	settings.WebRtcSettings = nil
	recorder.configure(settings)
	if recorder.unsupported.Load() != 1 {
		t.Fatal("missing SCTP settings not explicit")
	}
}

func TestProbeSctpTraceConfigurationIsOptIn(t *testing.T) {
	t.Setenv("CONNECT_PERFVAR_PROGRESS_TRACE", "")
	settings := clientconnect.DefaultClientSettings()
	before := settings.WebRtcSettings
	newPerfvarProgressTrace().configure(settings)
	if settings.WebRtcSettings != before || settings.WebRtcSettings.Log != nil {
		t.Fatal("disabled trace attached native SCTP logging")
	}
	ordinary := newAckReplayTrace()
	ordinary.configure(settings)
	if ordinary.configureForTest != nil || settings.WebRtcSettings != before || settings.WebRtcSettings.Log != nil {
		t.Fatal("ordinary ACK trace attached native SCTP logging")
	}
	trace := newProbeSctpLineageTrace()
	trace.configure(settings)
	platform := clientconnect.DefaultPlatformTransportSettings()
	trace.configurePlatform(platform)
	if trace.configureForTest == nil || trace.observeForTest == nil || trace.dumpForTest == nil ||
		settings.WebRtcSettings == before || settings.WebRtcSettings.Log == nil ||
		settings.SendBufferSettings.ProgressObserver == nil || settings.ReceiveBufferSettings.ProgressObserver == nil ||
		settings.StreamManagerSettings.StreamBufferSettings.P2pTransportSettings.ProgressObserver == nil || platform.ProgressObserver == nil {
		t.Fatal("native diagnostic replaced an existing progress boundary")
	}
}
