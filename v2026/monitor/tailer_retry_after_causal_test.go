package monitor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

const retryAfterFixturePrefix = "[warpctl][loki-retry-after-v1] until="

type retryAfterChildStarts struct {
	lock        sync.Mutex
	pending     string
	starts      []time.Time
	cancel      context.CancelFunc
	startPrefix string
}

func (self *retryAfterChildStarts) Write(data []byte) (int, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.pending += string(data)
	for {
		line, rest, ok := strings.Cut(self.pending, "\n")
		if !ok {
			break
		}
		self.pending = rest
		prefix := self.startPrefix
		if prefix == "" {
			prefix = retryAfterFixturePrefix
		}
		if strings.HasPrefix(line, prefix) {
			self.starts = append(self.starts, time.Now())
			if len(self.starts) == 2 {
				self.cancel()
			}
		}
	}
	return len(data), nil
}

func (self *retryAfterChildStarts) snapshot() []time.Time {
	self.lock.Lock()
	defer self.lock.Unlock()
	return append([]time.Time(nil), self.starts...)
}

func retryAfterNativeTailer(t *testing.T, until time.Time, cancel context.CancelFunc) (*logTailer, *retryAfterChildStarts) {
	t.Helper()
	stderr := retryAfterFixturePrefix + until.UTC().Format(time.RFC3339Nano) + "\n" + query429Panic("synthetic-private-rejection")
	runner := warpctlHttp500NativeRunner(t, "", stderr, 2)
	starts := &retryAfterChildStarts{cancel: cancel}
	runner.operatorDiagnostics = starts
	runner.cfg.env = "synthetic"
	tailer := newLogTailer("fixture-service", &probeEnv{cfg: runner.cfg, runner: runner})
	return tailer, starts
}

// No retry calculation is mocked. The original runStream discards advice at
// its real child boundary and starts child two after one second. The fixed
// owner remains in its cancellation-aware wait and never starts that child.
func TestLogTailerRetryAfterDefersNextChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	tailer, starts := retryAfterNativeTailer(t, time.Now().Add(time.Minute), cancel)
	tailer.runStream(ctx)
	if got := len(starts.snapshot()); got != 1 {
		t.Fatalf("native child attempts = %d, want one before future admission", got)
	}
	_, restarts, _ := tailer.healthSnapshot()
	if restarts != 1 || ctx.Err() == nil {
		t.Fatal("the failed child or waiting owner lost cancellation/restart state")
	}
}

// The admitted deadline does not suppress recovery. The real owner starts its
// successor after expiry; the fixture's second start cancels further work.
func TestLogTailerRetryAfterResumesAtAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	until := time.Now().Add(1800 * time.Millisecond)
	tailer, starts := retryAfterNativeTailer(t, until, cancel)
	tailer.runStream(ctx)
	observed := starts.snapshot()
	if len(observed) != 2 || observed[1].Before(until) {
		t.Fatal("the successor was missing or started before explicit admission")
	}
}
