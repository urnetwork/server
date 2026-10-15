package work

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

type privatePassControl func(context.Context, string, *connect.ConnectControlArgs) (*connect.ConnectControlResult, error)

func (f privatePassControl) ConnectControl(ctx context.Context, jwt string, args *connect.ConnectControlArgs) (*connect.ConnectControlResult, error) {
	return f(ctx, jwt, args)
}

// This composed ownership control uses the production pass wrapper, URL drain,
// full-batch buffering, Scheduler, ProbeOne and actual API OOB owner. Only the
// tunnel's packet plane and controller body are replaced by a held cleanup
// executor. The separate Open fixture covers their real successful lifecycle.
func TestPrivateProviderPassControlJoinsCanceledCleanup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
		defer cancel()
		entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		resultCh := make(chan error, 1)
		var owner *model.ContractOriginNotifications
		go func() {
			defer close(returned)
			_, err := runWithProviderEgressControl(ctx, &providerEgressCredentials{}, func(local connect.NetworkClientControl) (*ProviderEgressProbeResult, error) {
				owner = local.(*providerEgressControl).notifications
				settings := connect.DefaultClientStrategySettings()
				settings.EnableNormal, settings.EnableResilient = false, false
				settings.AltUrl, settings.ExtenderDirectory, settings.InternalDohDomains = "", nil, nil
				settings.RequestTimeout = 5 * time.Second
				strategy := connect.NewClientStrategy(ctx, settings)
				defer strategy.Close()
				control := connect.NewApiOutOfBandControlWithLocalControl(ctx, strategy, "fixture-child", "https://no-network.invalid", privatePassControl(func(cleanupCtx context.Context, _ string, _ *connect.ConnectControlArgs) (*connect.ConnectControlResult, error) {
					if ctx.Err() == nil || cleanupCtx.Err() != nil {
						t.Error("cleanup did not retain an independent live deadline after cancellation")
					}
					if deadline, ok := cleanupCtx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
						t.Error("cleanup lost finite strategy deadline")
					}
					close(entered)
					<-release
					return &connect.ConnectControlResult{Pack: ""}, nil
				}))
				defer control.CloseAndWait(context.Background())
				pass, args, _ := testUrlProbePass()
				args.UrlProbe.Limit, args.UrlProbe.Concurrency = 1, 1
				selected := false
				pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
					if selected {
						return nil, nil
					}
					selected = true
					return testDueProviders("private-provider"), nil
				}
				pass.runFull = func(probeCtx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
					probe := &prober.Prober{
						Open: func(context.Context, string) (*http.Client, func() error, error) {
							return &http.Client{}, func() error {
								cancel()
								callback := make(chan error, 1)
								control.SendControlWithCtx(context.WithoutCancel(probeCtx), nil, func(_ []*protocol.Frame, err error) { callback <- err })
								if err := control.CloseAndWait(context.Background()); err != nil {
									return err
								}
								return <-callback
							}, nil
						},
						Health: func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
							return testHealthRun(1, 1), nil
						},
						HealthResults: options.HealthResults, Attempts: options.Attempts,
					}
					scheduler := &prober.Scheduler{Prober: probe, Concurrency: 1}
					return scheduler.Run(probeCtx, providers), nil
				}
				return pass.run(ctx, args)
			})
			resultCh <- err
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("cleanup was not admitted")
		}
		// Admission occurs after parent cancellation, so this Watch proves the
		// real notifier remains available to late controller work.
		watch := owner.Watch(server.NewId(), server.NewId())
		if watch == nil {
			t.Fatal("canceled pass closed its notifier before cleanup")
		}
		watch.Close()
		select {
		case <-returned:
			t.Fatal("pass returned before held OOB executor/callback")
		default:
		}
		unblock()
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Fatal("released pass did not terminally join")
		}
		if err := <-resultCh; !errors.Is(err, context.Canceled) {
			t.Fatalf("pass cancellation changed: %v", err)
		}
		if watch := owner.Watch(server.NewId(), server.NewId()); watch != nil {
			watch.Close()
			t.Fatal("notifier survived pass return")
		}
	})
}
