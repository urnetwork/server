package connect

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"

	"github.com/urnetwork/connect"
)

// Every labeled move metric pushes under the stats pusher's grouping labels
// (env, service, block, host and instance; see grafana.go). The push client
// refuses a push whose metric carries a grouping label as its own, and the
// refusal repeats on every later push of the process, so every metric of the
// process goes stale. A metric without labels cannot collide.
func TestMoveMetricsPushUnderTheStatsGrouping(t *testing.T) {
	var pushCount atomic.Int32
	pushServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer pushServer.Close()

	// one series of each labeled vector, under label values no other test uses
	series := []struct {
		vector      *prometheus.MetricVec
		labelValues []string
	}{
		{movesSentCounter.MetricVec, []string{"push-test", "push-test", "push-test", "push-test"}},
		{movesWouldSendCounter.MetricVec, []string{"push-test", "push-test", "push-test", "push-test"}},
		{moveBudgetRefusedCounter.MetricVec, []string{"push-test"}},
		{moveSendErrorsCounter.MetricVec, []string{"push-test", "push-test"}},
		{moverTicksCounter.MetricVec, []string{"push-test"}},
		{moveLoadPublishesCounter.MetricVec, []string{"push-test"}},
		{drainMovesSentCounter.MetricVec, []string{"push-test"}},
		{residentMovesArrivedCounter.MetricVec, []string{"push-test", "push-test"}},
		{residentMovesExpiredCounter.MetricVec, []string{"push-test"}},
		{residentMoveTargetBreakerOpenGauge.MetricVec, []string{"push-test", "push-test", "push-test"}},
		{drainRefusalsCounter.MetricVec, []string{"push-test"}},
	}
	registry := prometheus.NewRegistry()
	for _, s := range series {
		metric, err := s.vector.GetMetricWithLabelValues(s.labelValues...)
		if err != nil {
			t.Fatal(err)
		}
		switch m := metric.(type) {
		case prometheus.Counter:
			m.Add(0)
		case prometheus.Gauge:
			m.Set(0)
		}
		defer s.vector.DeleteLabelValues(s.labelValues...)
		registry.MustRegister(s.vector)
	}

	pusher := push.New(pushServer.URL, "connect").
		Gatherer(registry).
		Grouping("env", "test").
		Grouping("service", "connect").
		Grouping("block", "push-test").
		Grouping("host", "push-test").
		Grouping("instance", "push-test")
	// the refusal is per push, so a second push must succeed too
	for range 2 {
		connect.AssertEqual(t, nil, pusher.Push())
	}
	connect.AssertEqual(t, int32(2), pushCount.Load())
}
