package fleetprobe

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

func TestFullProberTimingWiresReturnedCallObserver(t *testing.T) {
	observed := 0
	p := NewFullProber(FullOptions{ObserveTiming: func(prober.ProbeTiming) { observed++ }})
	want := errors.New("synthetic opener failure")
	p.OpenProvider = func(context.Context, prober.Provider) (*http.Client, func() error, error) { return nil, nil, want }
	if err := p.ProbeOne(t.Context(), prober.Provider{ClientId: "synthetic"}); !errors.Is(err, want) || observed != 1 {
		t.Fatalf("fleet adapter lost timing or changed error: observed=%d err=%v", observed, err)
	}
}
