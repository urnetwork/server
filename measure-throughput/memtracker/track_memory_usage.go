package memtracker

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/jedib0t/go-pretty/v6/progress"
)

type UsageAtTime struct {
	SecondsSinceStart float64
	Bytes             uint64
}

type MemoryUsage struct {
	mu      sync.Mutex
	history []UsageAtTime
}

func (m *MemoryUsage) GetHistory() []UsageAtTime {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.history
}

func Start(ctx context.Context, pw progress.Writer) *MemoryUsage {

	tracker := &progress.Tracker{
		Message: "Memory Usage (mb)",
		Total:   0,
	}

	pw.AppendTracker(tracker)
	tracker.Start()

	usage := &MemoryUsage{}

	go func() {
		timer := time.NewTicker(1 * time.Second)
		startTime := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:

				var memStats runtime.MemStats
				runtime.ReadMemStats(&memStats)

				tracker.SetValue(int64(memStats.Sys / 1024 / 1024))

				usage.mu.Lock()
				usage.history = append(usage.history, UsageAtTime{
					SecondsSinceStart: time.Since(startTime).Seconds(),
					Bytes:             memStats.Sys,
				})
				usage.mu.Unlock()
			}
		}
	}()

	return usage

}
