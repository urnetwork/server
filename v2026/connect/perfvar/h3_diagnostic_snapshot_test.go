// Diagnostic interval reducers retain every published counter and explicit gauge.
package perfvar

import (
	"reflect"
	"strings"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// Nested failure counters are intervals; their maximum remains an end gauge.
func TestH3DiagnosticReceiveCounterDeltasCoverEveryField(t *testing.T) {
	testH3DiagnosticCounterDeltas(t, subtractH3FullTunReceive, map[string]bool{
		"PackHandoffMaxCount": true, "PackHandoffMaxByteCount": true,
		"PackHandoffAdaptiveMaxDepth": true, "PackHandoffAdaptiveMaxByteCount": true,
		"AckRouteWriteMaxWait": true, "AckRouteWriteFailures.MaxWait": true,
	})
}

// Recovery gauges describe end state rather than monotonic interval counts.
func TestH3DiagnosticRecoveryCounterDeltasCoverEveryField(t *testing.T) {
	testH3DiagnosticCounterDeltas(t, subtractH3FullTunRecovery, map[string]bool{
		"UnreliableFlightMaximumWaitDuration": true,
		"UnreliableFlightMaximumByteCount":    true, "UnreliableFlightMaximumLimitByteCount": true,
		"UnreliableFlightMaximumMessageCount": true, "UnreliableFlightMaximumMessageLimit": true,
		"RouteUnacknowledgedDuration": true, "RouteRetainedItemCount": true,
		"ReliableLaneLongestAckGap": true, "ReliableLaneStallOnsetInterval": true,
		"ReliableLaneStallOnsetOutstanding": true, "ReliableLaneStallOnsetOffset": true,
		"ReliableAdmissionByteLimitMinimum": true, "UnreliableCarrierLastAckAge": true,
	})
}

// Current MTU is a gauge; packet event fields are cumulative counters.
func TestH3DiagnosticQuicCounterDeltasCoverEveryField(t *testing.T) {
	testH3DiagnosticCounterDeltas(t, subtractH3FullTunQuic, map[string]bool{"CurrentMtu": true})
}

// Every TCP event field participates in the interval.
func TestH3DiagnosticTcpCounterDeltasCoverEveryField(t *testing.T) {
	testH3DiagnosticCounterDeltas(t, subtractH3FullTunTcp, nil)
}

// Provider admission failures remain complete interval evidence.
func TestH3DiagnosticProviderCongestionCounterDeltasCoverEveryField(t *testing.T) {
	testH3DiagnosticCounterDeltas(t, subtractH3FullTunProviderCongestion, nil)
}

// Every lane event field participates in the interval.
func TestH3DiagnosticLaneCounterDeltasCoverEveryField(t *testing.T) {
	testH3DiagnosticCounterDeltas(t, subtractH3FullTunLanes, nil)
}

// Every published leaf gets a nonzero sentinel. Unknown kinds still fail closed.
func testH3DiagnosticCounterDeltas[T any](t *testing.T, subtract func(T, T) T, gauges map[string]bool) {
	t.Helper()
	var start, end T
	before, after := reflect.ValueOf(&start).Elem(), reflect.ValueOf(&end).Elem()
	for index := range before.NumField() {
		setH3DiagnosticSentinel(t, before.Field(index), 11*int64(index+1))
		setH3DiagnosticSentinel(t, after.Field(index), 37*int64(index+1))
	}
	actual := reflect.ValueOf(subtract(start, end))
	for index := range before.NumField() {
		field := before.Type().Field(index).Name
		want := h3DiagnosticSentinelDifference(t, before.Field(index), after.Field(index), gauges, field)
		if !reflect.DeepEqual(actual.Field(index).Interface(), want.Interface()) {
			t.Errorf("%s = %v, want %v", field, actual.Field(index).Interface(), want.Interface())
		}
	}
	for field := range gauges {
		fieldType := before.Type()
		for _, name := range strings.Split(field, ".") {
			for fieldType.Kind() == reflect.Pointer {
				fieldType = fieldType.Elem()
			}
			if fieldType.Kind() != reflect.Struct {
				t.Fatalf("gauge manifest path %s crosses non-struct %s", field, fieldType)
			}
			member, exists := fieldType.FieldByName(name)
			if !exists {
				t.Fatalf("gauge manifest names nonexistent field %s", field)
			}
			fieldType = member.Type
		}
		if fieldType.Kind() != reflect.Uint64 && fieldType.Kind() != reflect.Int64 {
			t.Fatalf("gauge manifest must name a numeric leaf, not %s (%s)", field, fieldType)
		}
	}
}

// Optional structs are populated, not skipped; unsupported leaves remain errors.
func setH3DiagnosticSentinel(t *testing.T, value reflect.Value, seed int64) {
	t.Helper()
	switch value.Kind() {
	case reflect.Uint64:
		value.SetUint(uint64(seed))
	case reflect.Int64:
		value.SetInt(seed)
	case reflect.Pointer:
		value.Set(reflect.New(value.Type().Elem()))
		setH3DiagnosticSentinel(t, value.Elem(), seed)
	case reflect.Struct:
		for index := range value.NumField() {
			setH3DiagnosticSentinel(t, value.Field(index), seed*int64(index+1))
		}
	case reflect.Array:
		for index := range value.Len() {
			setH3DiagnosticSentinel(t, value.Index(index), seed*int64(index+1))
		}
	case reflect.Map:
		value.Set(reflect.MakeMap(value.Type()))
		for index, carrier := range []clientconnect.TransportType{clientconnect.TransportTypeH1, clientconnect.TransportTypeH3} {
			entry := reflect.New(value.Type().Elem()).Elem()
			setH3DiagnosticSentinel(t, entry, seed*int64(index+1))
			value.SetMapIndex(reflect.ValueOf(carrier), entry)
		}
	default:
		t.Fatalf("unclassified diagnostic field type %s", value.Type())
	}
}

// Only explicitly named numeric gauges bypass recursive counter subtraction.
func h3DiagnosticSentinelDifference(t *testing.T, before, after reflect.Value, gauges map[string]bool, field string) reflect.Value {
	t.Helper()
	if gauges[field] {
		return after
	}
	result := reflect.New(after.Type()).Elem()
	switch after.Kind() {
	case reflect.Uint64:
		result.SetUint(after.Uint() - before.Uint())
	case reflect.Int64:
		result.SetInt(after.Int() - before.Int())
	case reflect.Pointer:
		if !after.IsNil() {
			baseline := reflect.New(after.Type().Elem()).Elem()
			if !before.IsNil() {
				baseline = before.Elem()
			}
			result.Set(reflect.New(after.Type().Elem()))
			result.Elem().Set(h3DiagnosticSentinelDifference(t, baseline, after.Elem(), gauges, field))
		}
	case reflect.Struct:
		for index := range after.NumField() {
			name := field + "." + after.Type().Field(index).Name
			result.Field(index).Set(h3DiagnosticSentinelDifference(t, before.Field(index), after.Field(index), gauges, name))
		}
	case reflect.Array:
		for index := range after.Len() {
			result.Index(index).Set(h3DiagnosticSentinelDifference(t, before.Index(index), after.Index(index), gauges, field))
		}
	case reflect.Map:
		result.Set(reflect.MakeMap(after.Type()))
		for _, key := range after.MapKeys() {
			baseline := before.MapIndex(key)
			if !baseline.IsValid() {
				baseline = reflect.New(after.Type().Elem()).Elem()
			}
			result.SetMapIndex(key, h3DiagnosticSentinelDifference(t, baseline, after.MapIndex(key), gauges, field))
		}
	default:
		t.Fatalf("unclassified diagnostic field type %s", after.Type())
	}
	return result
}

// A carrier first used during measurement has a zero baseline, and snapshots
// own their maps so later callers cannot change the evidence being compared.
func TestH3DiagnosticAckCarrierDeltasOwnNewCarrierMaps(t *testing.T) {
	start := clientconnect.ClientReceiveStatsSnapshot{
		AckRouteWriteCountByTransport:   map[clientconnect.TransportType]uint64{clientconnect.TransportTypeH1: 10},
		AckRouteWriteWaitByTransport:    map[clientconnect.TransportType]time.Duration{clientconnect.TransportTypeH1: time.Second},
		AckRouteWriteTimeoutByTransport: map[clientconnect.TransportType]uint64{clientconnect.TransportTypeH1: 2},
	}
	end := clientconnect.ClientReceiveStatsSnapshot{
		AckRouteWriteCountByTransport:   map[clientconnect.TransportType]uint64{clientconnect.TransportTypeH1: 15, clientconnect.TransportTypeH3: 7},
		AckRouteWriteWaitByTransport:    map[clientconnect.TransportType]time.Duration{clientconnect.TransportTypeH1: 3 * time.Second, clientconnect.TransportTypeH3: 4 * time.Second},
		AckRouteWriteTimeoutByTransport: map[clientconnect.TransportType]uint64{clientconnect.TransportTypeH1: 5, clientconnect.TransportTypeH3: 8},
	}
	delta := subtractH3FullTunReceive(start, end)
	if delta.AckRouteWriteCountByTransport[clientconnect.TransportTypeH1] != 5 ||
		delta.AckRouteWriteCountByTransport[clientconnect.TransportTypeH3] != 7 ||
		delta.AckRouteWriteWaitByTransport[clientconnect.TransportTypeH1] != 2*time.Second ||
		delta.AckRouteWriteWaitByTransport[clientconnect.TransportTypeH3] != 4*time.Second ||
		delta.AckRouteWriteTimeoutByTransport[clientconnect.TransportTypeH1] != 3 ||
		delta.AckRouteWriteTimeoutByTransport[clientconnect.TransportTypeH3] != 8 {
		t.Fatalf("ACK carrier deltas were erased or misattributed: %+v", delta)
	}
	delta.AckRouteWriteCountByTransport[clientconnect.TransportTypeH1] = 99
	delta.AckRouteWriteWaitByTransport[clientconnect.TransportTypeH1] = 99 * time.Second
	delta.AckRouteWriteTimeoutByTransport[clientconnect.TransportTypeH1] = 99
	if start.AckRouteWriteCountByTransport[clientconnect.TransportTypeH1] != 10 || end.AckRouteWriteCountByTransport[clientconnect.TransportTypeH1] != 15 ||
		start.AckRouteWriteWaitByTransport[clientconnect.TransportTypeH1] != time.Second || end.AckRouteWriteWaitByTransport[clientconnect.TransportTypeH1] != 3*time.Second ||
		start.AckRouteWriteTimeoutByTransport[clientconnect.TransportTypeH1] != 2 || end.AckRouteWriteTimeoutByTransport[clientconnect.TransportTypeH1] != 5 {
		t.Fatal("diagnostic delta aliases a lifetime snapshot")
	}
}

// Directly reaches the reducer even when the generic sentinel cannot populate a
// new shape. A nil endpoint stays absent; present output owns its copied fields.
func TestH3DiagnosticAckFailureDeltasPreserveNilAndOwnership(t *testing.T) {
	start := clientconnect.AckRouteWriteFailureStatsSnapshot{
		ErrorCount: 28, RouteTimeoutCount: 2, CallerCanceledCount: 3, CallerDeadlineCount: 5,
		SelectorClosedCount: 7, OtherCount: 11, BlockedCount: 13,
		WaitDuration: 31 * time.Second, MaxWait: 19 * time.Second,
	}
	end := clientconnect.AckRouteWriteFailureStatsSnapshot{
		ErrorCount: 43, RouteTimeoutCount: 3, CallerCanceledCount: 5, CallerDeadlineCount: 8,
		SelectorClosedCount: 11, OtherCount: 16, BlockedCount: 20,
		WaitDuration: 73 * time.Second, MaxWait: 23 * time.Second,
	}
	for _, c := range []struct {
		name       string
		start, end *clientconnect.AckRouteWriteFailureStatsSnapshot
		want       *clientconnect.AckRouteWriteFailureStatsSnapshot
	}{
		{name: "absent"},
		{name: "first failure", end: &end, want: &end},
		{name: "end unavailable", start: &start},
		{name: "both present", start: &start, end: &end, want: &clientconnect.AckRouteWriteFailureStatsSnapshot{
			ErrorCount: 15, RouteTimeoutCount: 1, CallerCanceledCount: 2, CallerDeadlineCount: 3,
			SelectorClosedCount: 4, OtherCount: 5, BlockedCount: 7,
			WaitDuration: 42 * time.Second, MaxWait: 23 * time.Second,
		}},
		{name: "no new failure", start: &start, end: &start},
	} {
		var beforeStart, beforeEnd clientconnect.AckRouteWriteFailureStatsSnapshot
		if c.start != nil {
			beforeStart = *c.start
		}
		if c.end != nil {
			beforeEnd = *c.end
		}
		got := subtractH3FullTunReceive(
			clientconnect.ClientReceiveStatsSnapshot{AckRouteWriteFailures: c.start},
			clientconnect.ClientReceiveStatsSnapshot{AckRouteWriteFailures: c.end},
		).AckRouteWriteFailures
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: AckRouteWriteFailures = %+v, want %+v", c.name, got, c.want)
		}
		if got != nil {
			if got == c.start || got == c.end {
				t.Fatalf("%s: diagnostic delta aliases a lifetime snapshot", c.name)
			}
			*got = clientconnect.AckRouteWriteFailureStatsSnapshot{ErrorCount: 999}
		}
		if (c.start != nil && *c.start != beforeStart) || (c.end != nil && *c.end != beforeEnd) {
			t.Fatalf("%s: reducer or result mutation changed a lifetime snapshot", c.name)
		}
	}
}

// Connect publishes ACK route failure totals only after a failed write. A
// first failure inside the interval reduces against a zero baseline, an
// interval without a new failure has no totals, and the delta owns its value.
func TestH3DiagnosticAckRouteWriteFailureDeltas(t *testing.T) {
	first := &clientconnect.AckRouteWriteFailureStatsSnapshot{
		ErrorCount:        3,
		RouteTimeoutCount: 2,
		OtherCount:        1,
		BlockedCount:      2,
		WaitDuration:      3 * time.Second,
		MaxWait:           2 * time.Second,
	}
	later := &clientconnect.AckRouteWriteFailureStatsSnapshot{
		ErrorCount:          5,
		RouteTimeoutCount:   3,
		CallerCanceledCount: 1,
		OtherCount:          1,
		BlockedCount:        3,
		WaitDuration:        5 * time.Second,
		MaxWait:             4 * time.Second,
	}
	receive := func(failures *clientconnect.AckRouteWriteFailureStatsSnapshot) clientconnect.ClientReceiveStatsSnapshot {
		return clientconnect.ClientReceiveStatsSnapshot{AckRouteWriteFailures: failures}
	}

	if delta := subtractH3FullTunReceive(receive(nil), receive(nil)).AckRouteWriteFailures; delta != nil {
		t.Fatalf("an interval without any failure reported totals %+v", *delta)
	}
	delta := subtractH3FullTunReceive(receive(nil), receive(first)).AckRouteWriteFailures
	if delta == nil || *delta != *first {
		t.Fatalf("first failures against a zero baseline = %+v, want %+v", delta, *first)
	}
	if delta == first {
		t.Fatal("the interval delta aliases the lifetime snapshot")
	}
	if delta := subtractH3FullTunReceive(receive(first), receive(first)).AckRouteWriteFailures; delta != nil {
		t.Fatalf("an interval without a new failure reported totals %+v", *delta)
	}
	want := clientconnect.AckRouteWriteFailureStatsSnapshot{
		ErrorCount:          2,
		RouteTimeoutCount:   1,
		CallerCanceledCount: 1,
		BlockedCount:        1,
		WaitDuration:        2 * time.Second,
		MaxWait:             4 * time.Second,
	}
	if delta := subtractH3FullTunReceive(receive(first), receive(later)).AckRouteWriteFailures; delta == nil || *delta != want {
		t.Fatalf("later failures = %+v, want %+v", delta, want)
	}
}
