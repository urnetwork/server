package competition

import (
	"math"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/server/v2026"
)

func TestRunnerHeartbeatContract(t *testing.T) {
	if runnerHeartbeatInterval != 15*time.Second {
		t.Fatalf("runner heartbeat interval = %s, want 15s", runnerHeartbeatInterval)
	}

	want := time.Date(2026, time.August, 28, 12, 34, 56, 789000000, time.UTC)
	recordRunnerHeartbeat(want)
	defer recordRunnerHeartbeat(server.NowUtc())

	metric := &dto.Metric{}
	if err := competitionRunnerHeartbeatTimestamp.Write(metric); err != nil {
		t.Fatal(err)
	}
	got := metric.GetGauge().GetValue()
	wantSeconds := float64(want.UnixNano()) / float64(time.Second)
	if math.Abs(got-wantSeconds) > 0.000001 {
		t.Fatalf("runner heartbeat timestamp = %.9f, want %.9f", got, wantSeconds)
	}
}
