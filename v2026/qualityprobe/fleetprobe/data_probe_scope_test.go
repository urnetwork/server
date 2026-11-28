package fleetprobe

import (
	"context"
	"errors"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
	"github.com/urnetwork/server/v2026/qualityprobe/providertunnel"
	"testing"
	"time"
)

func TestFullProberDataOnlyModeIsUrlScoped(t *testing.T) {
	prior := openProviderTunnel
	t.Cleanup(func() { openProviderTunnel = prior })
	for _, urlProbe := range []bool{false, true} {
		calls := 0
		stop := errors.New("captured before Open")
		openProviderTunnel = func(_ context.Context, cfg providertunnel.Config, _ connect.Id) (*providertunnel.Tunnel, error) {
			calls++
			if cfg.DataOnlyProbe != urlProbe {
				t.Errorf("URL=%t data-only=%t", urlProbe, cfg.DataOnlyProbe)
			}
			return nil, stop
		}
		p := NewFullProber(FullOptions{UrlProbe: urlProbe, TunnelConfig: providertunnel.Config{DataOnlyProbe: !urlProbe}, Pool: func() *egresshealth.Pool { return smallPool() }, ProbeTimeout: time.Minute})
		_, _, err := p.OpenProvider(context.Background(), prober.Provider{ClientId: connect.NewId().String()})
		if !errors.Is(err, stop) || calls != 1 {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	}
}
