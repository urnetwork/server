// The production fleet adapter forwards identity-free lifecycle observations.
package fleetprobe

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

func TestRunFullForwardsActualProgress(t *testing.T) {
	var events []prober.Progress
	options := FullOptions{
		ProbeTimeout: time.Minute,
		IpEchoUrl:    "https://echo.probe.example/ip",
		Submit:       nopSubmitter{}, Attempts: nopAttempts{}, Concurrency: 1,
		ObserveProgress: func(event prober.Progress) { events = append(events, event) },
	}
	// A deliberately malformed synthetic id fails inside the real Open path,
	// before external I/O. It still entered/returned from one actual worker.
	summary, err := RunFull(context.Background(), []prober.Provider{{ClientId: "synthetic-invalid-id"}}, options)
	if err != nil || summary.Attempted != 1 || summary.Failed != 1 || !slices.Equal(events, []prober.Progress{prober.ProbeStarted, prober.ProbeFinished}) {
		t.Fatalf("full adapter hid actual worker: summary=%+v error=%v events=%v", summary, err, events)
	}
	events = nil
	options.ProbeTimeout = 0
	if _, err := RunFull(context.Background(), []prober.Provider{{ClientId: "synthetic-invalid-id"}}, options); err == nil || len(events) != 0 {
		t.Fatalf("invalid options fabricated worker activity: error=%v events=%v", err, events)
	}
}
