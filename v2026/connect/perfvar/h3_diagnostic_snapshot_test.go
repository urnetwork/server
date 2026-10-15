package perfvar

import (
	"reflect"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// Every published field gets a distinct nonzero sentinel. Adding an endpoint
// counter without extending the diagnostic reducer must fail here instead of
// quietly reporting a zero that could be mistaken for absence of loss.
func TestH3DiagnosticCounterDeltasCoverEveryField(t *testing.T) {
	t.Run("receive", func(t *testing.T) {
		testH3DiagnosticCounterDeltas(t, subtractH3FullTunReceive, map[string]bool{
			"PackHandoffMaxCount": true, "PackHandoffMaxByteCount": true,
			"PackHandoffAdaptiveMaxDepth": true, "PackHandoffAdaptiveMaxByteCount": true,
			"AckRouteWriteMaxWait": true,
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
		want := after.Field(index)
		if !gauges[field] {
			want = h3DiagnosticSentinelDifference(t, before.Field(index), want)
		}
		if !reflect.DeepEqual(actual.Field(index).Interface(), want.Interface()) {
			t.Errorf("%s = %v, want %v", field, actual.Field(index).Interface(), want.Interface())
		}
	}
	for field := range gauges {
		if _, exists := before.Type().FieldByName(field); !exists {
			t.Errorf("gauge manifest names nonexistent field %s", field)
		}
	}
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
