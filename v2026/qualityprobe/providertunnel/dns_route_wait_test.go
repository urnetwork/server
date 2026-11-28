package providertunnel

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026"
)

func TestProviderRouteWaitBoundaries(t *testing.T) {
	for _, name := range []string{"ready", "live_replacement", "removed_snapshot", "failed_evaluation", "lost", "closed", "canceled", "independent_tunnel"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				lost, lose := context.WithCancelCause(t.Context())
				defer lose(context.Canceled)
				monitor := connect.NewRemoteUserNatMultiClientMonitorWithDefaults()
				first, second := connect.NewId(), connect.NewId()
				add := func(m *connect.RemoteUserNatMultiClientMonitor, id connect.Id, state connect.ProviderState) {
					m.AddProviderEvent(id, state, id, nil, connect.IpFamilyV4Only)
				}
				wantErr, wantElapsed := error(context.DeadlineExceeded), 3*time.Second
				switch name {
				case "ready":
					add(monitor, first, connect.ProviderStateAdded)
					wantErr, wantElapsed = nil, 0
				case "live_replacement":
					add(monitor, first, connect.ProviderStateAdded)
					add(monitor, second, connect.ProviderStateAdded)
					add(monitor, first, connect.ProviderStateRemoved)
					wantErr, wantElapsed = nil, 0
				case "removed_snapshot":
					add(monitor, first, connect.ProviderStateAdded)
					add(monitor, first, connect.ProviderStateRemoved)
				case "failed_evaluation":
					add(monitor, first, connect.ProviderStateInEvaluation)
					go func() { time.Sleep(time.Second); add(monitor, first, connect.ProviderStateEvaluationFailed) }()
				case "lost":
					wantErr, wantElapsed = ErrTunnelLost, time.Second
					go func() { time.Sleep(time.Second); lose(ErrTunnelLost) }()
				case "closed":
					wantErr, wantElapsed = ErrTunnelClosed, time.Second
					go func() { time.Sleep(time.Second); lose(ErrTunnelClosed) }()
				case "canceled":
					wantErr, wantElapsed = context.Canceled, time.Second
					go func() { time.Sleep(time.Second); cancel() }()
				case "independent_tunnel":
					other := connect.NewRemoteUserNatMultiClientMonitorWithDefaults()
					add(other, second, connect.ProviderStateAdded)
				}
				err := waitProviderRoute(ctx, lost, monitor)
				if !errors.Is(err, wantErr) || time.Since(start) != wantElapsed {
					t.Fatalf("route wait boundary changed: error=%v want=%v elapsed=%s want=%s", err, wantErr, time.Since(start), wantElapsed)
				}
			})
		})
	}
}
