package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestRedisBuffersSignalSyntheticClientBufferGrowth(t *testing.T) {
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if strings.Contains(command, "for p in") {
			return "6380 1000000000 10000000000 600000000 300000000 10", nil
		}
		return "", nil
	}}
	alerts, err := NewRedisBuffersSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "client-buffers")
}
