package perfvar

import (
	"reflect"
	"strings"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
)

// Every published field gets a distinct nonzero sentinel. Adding an endpoint
// counter without extending the diagnostic reducer must fail here instead of
// quietly reporting a zero that could be mistaken for absence of loss. A
// nested snapshot behind a pointer is a field of fields: each of those gets a
// sentinel as well, and a gauge among them is named by its dotted path.
func TestH3DiagnosticCounterDeltasCoverEveryField(t *testing.T) {
	t.Run("receive", func(t *testing.T) {
		testH3DiagnosticCounterDeltas(t, subtractH3FullTunReceive, map[string]bool{
			"PackHandoffMaxCount": true, "PackHandoffMaxByteCount": true,
			"PackHandoffAdaptiveMaxDepth": true, "PackHandoffAdaptiveMaxByteCount": true,
			"AckRouteWriteMaxWait": true, "AckRouteWriteFailures.MaxWait": true,
		})
	})
	t.Run("recovery", func(t *testing.T) {
		testH3DiagnosticCounterDeltas(t, subtractH3FullTunRecovery, map[string]bool{
			"UnreliableFlightMaximumWaitDuration": true,
			"UnreliableFlightMaximumByteCount":    true, "UnreliableFlightMaximumLimitByteCount": true,
			"UnreliableFlightMaximumMessageCount": true, "UnreliableFlightMaximumMessageLimit": true,
			"RouteUnacknowledgedDuration": true, "RouteRetainedItemCount": true,
			"ReliableLaneLongestAckGap": true, "ReliableLaneStallOnsetInterval": true,
			"ReliableLaneStallOnsetOutstanding": true, "ReliableLaneStallOnsetOffset": true,
			"ReliableAdmissionByteLimitMinimum": true, "UnreliableCarrierLastAckAge": true,
		})
	})
	t.Run("quic", func(t *testing.T) {
		testH3DiagnosticCounterDeltas(t, subtractH3FullTunQuic, map[string]bool{"CurrentMtu": true})
	})
	t.Run("tcp", func(t *testing.T) {
		testH3DiagnosticCounterDeltas(t, subtractH3FullTunTcp, nil)
	})
	t.Run("provider-congestion", func(t *testing.T) {
		testH3DiagnosticCounterDeltas(t, subtractH3FullTunProviderCongestion, nil)
	})
	t.Run("lanes", func(t *testing.T) {
		testH3DiagnosticCounterDeltas(t, subtractH3FullTunLanes, nil)
	})
}

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
		want := h3DiagnosticSentinelDelta(t, field, before.Field(index), after.Field(index), gauges)
		if !reflect.DeepEqual(actual.Field(index).Interface(), want.Interface()) {
			t.Errorf("%s = %v, want %v", field, actual.Field(index).Interface(), want.Interface())
		}
	}
	for field := range gauges {
		if !h3DiagnosticFieldExists(before.Type(), field) {
			t.Errorf("gauge manifest names nonexistent field %s", field)
		}
	}
}

// A gauge keeps its end value and a counter is its end minus its start. A
// nested snapshot is reduced field by field under its dotted path.
func h3DiagnosticSentinelDelta(
	t *testing.T,
	path string,
	before reflect.Value,
	after reflect.Value,
	gauges map[string]bool,
) reflect.Value {
	t.Helper()
	if gauges[path] {
		return after
	}
	if after.Kind() != reflect.Pointer {
		return h3DiagnosticSentinelDifference(t, before, after)
	}
	result := reflect.New(after.Type().Elem())
	for index := range after.Elem().NumField() {
		field := after.Elem().Type().Field(index).Name
		result.Elem().Field(index).Set(h3DiagnosticSentinelDelta(
			t,
			path+"."+field,
			before.Elem().Field(index),
			after.Elem().Field(index),
			gauges,
		))
	}
	return result
}

// Resolves a dotted gauge path through nested snapshots.
func h3DiagnosticFieldExists(snapshotType reflect.Type, path string) bool {
	for _, name := range strings.Split(path, ".") {
		if snapshotType.Kind() == reflect.Pointer {
			snapshotType = snapshotType.Elem()
		}
		if snapshotType.Kind() != reflect.Struct {
			return false
		}
		field, ok := snapshotType.FieldByName(name)
		if !ok {
			return false
		}
		snapshotType = field.Type
	}
	return true
}

func setH3DiagnosticSentinel(t *testing.T, value reflect.Value, seed int64) {
	t.Helper()
	switch value.Kind() {
	case reflect.Uint64:
		value.SetUint(uint64(seed))
	case reflect.Int64:
		value.SetInt(seed)
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
	case reflect.Pointer:
		// a nested snapshot, whose own fields must each be classified
		if value.Type().Elem().Kind() != reflect.Struct {
			t.Fatalf("unclassified diagnostic field type %s", value.Type())
		}
		snapshot := reflect.New(value.Type().Elem())
		for index := range snapshot.Elem().NumField() {
			setH3DiagnosticSentinel(t, snapshot.Elem().Field(index), seed*int64(index+1))
		}
		value.Set(snapshot)
	default:
		t.Fatalf("unclassified diagnostic field type %s", value.Type())
	}
}

func h3DiagnosticSentinelDifference(t *testing.T, before, after reflect.Value) reflect.Value {
	t.Helper()
	result := reflect.New(after.Type()).Elem()
	switch after.Kind() {
	case reflect.Uint64:
		result.SetUint(after.Uint() - before.Uint())
	case reflect.Int64:
		result.SetInt(after.Int() - before.Int())
	case reflect.Array:
		for index := range after.Len() {
			result.Index(index).Set(h3DiagnosticSentinelDifference(t, before.Index(index), after.Index(index)))
		}
	case reflect.Map:
		result.Set(reflect.MakeMap(after.Type()))
		for _, key := range after.MapKeys() {
			result.SetMapIndex(key, h3DiagnosticSentinelDifference(t, before.MapIndex(key), after.MapIndex(key)))
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
