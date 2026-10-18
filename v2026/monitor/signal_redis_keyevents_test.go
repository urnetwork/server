package monitor

import (
	"context"
	"testing"
)

func TestRedisKeyEventsSignalSyntheticKeyEventDrift(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return "6380 KE 12\n6381 - 12\n6382 KE 12", nil
	}}
	alerts, err := NewRedisKeyEventsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "keyevent-config-drift")
}

func TestRedisKeyEventsSignalSyntheticClientScaledPubsub(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return "6380 KE 1001\n6381 KE 1001\n6382 KE 1001", nil
	}}
	alerts, err := NewRedisKeyEventsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "pubsub-conn-shape")
}
