package proxy

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// devicesLiveGauge is the number of proxy ids with an installed embedded
// device on this instance
var devicesLiveGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "devices_live",
		Help:      "Proxy ids with an installed embedded device on this instance",
	},
)

// prewarmedDevicesGauge is the devices pre-warmed at startup from the
// activity set (PROXYDRAIN1.md §3.3)
var prewarmedDevicesGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "prewarmed_devices",
		Help:      "Devices pre-warmed at startup from the recent-activity set",
	},
)

func init() {
	prometheus.MustRegister(devicesLiveGauge)
	prometheus.MustRegister(prewarmedDevicesGauge)
}

// StartActivityFlusher periodically records this instance's recently active
// proxy ids into the per-(host, block) activity set (PROXYDRAIN1.md §3.3),
// so a replacement instance can pre-warm them during a deploy. It also keeps
// the devices-live gauge current. Runs until ctx is done.
func StartActivityFlusher(
	ctx context.Context,
	proxyDeviceManager *ProxyDeviceManager,
	settings *ProxySettings,
) {
	if settings.ActivityFlushTimeout <= 0 {
		return
	}
	proxyHost := server.RequireHost()
	block := server.RequireBlock()

	go server.HandleError(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(settings.ActivityFlushTimeout):
			}

			devicesLiveGauge.Set(float64(proxyDeviceManager.DeviceCount()))

			// activity within one flush interval plus margin: every device
			// active since the previous flush is (re)recorded
			window := 2 * settings.ActivityFlushTimeout
			proxyIds := proxyDeviceManager.ActiveProxyIds(window)
			if len(proxyIds) == 0 {
				continue
			}
			server.HandleError(func() {
				model.TouchProxyClientActivity(ctx, proxyHost, block, server.NowUtc(), proxyIds...)
			})
		}
	})
}

// Prewarm opens devices for the proxy clients recorded active on this
// (host, block) within `PrewarmActivityWindow` — the clients the drained
// instance was serving — with bounded concurrency and per-worker jitter, and
// waits for each device's egress window (PROXYDRAIN1.md §3.3). By the time
// the deploy's conntrack flush moves the pinned wg clients over, their first
// packet hits a warm device instead of paying the cold start.
//
// Returns the number of devices that reached ready. Bounded overall by
// `PrewarmTimeout`; failures are logged and skipped (the lazy open path
// remains the fallback for anything not pre-warmed).
func Prewarm(
	ctx context.Context,
	proxyDeviceManager *ProxyDeviceManager,
	settings *ProxySettings,
) int {
	if !settings.EnablePrewarm {
		return 0
	}
	proxyHost := server.RequireHost()
	block := server.RequireBlock()

	prewarmCtx, prewarmCancel := context.WithTimeout(ctx, settings.PrewarmTimeout)
	defer prewarmCancel()

	proxyIds := model.GetActiveProxyClients(
		prewarmCtx,
		proxyHost,
		block,
		settings.PrewarmActivityWindow,
		server.NowUtc(),
	)
	if len(proxyIds) == 0 {
		glog.Infof("[proxy]prewarm: no recently active proxy clients\n")
		return 0
	}
	glog.Infof("[proxy]prewarm: %d recently active proxy clients\n", len(proxyIds))

	next := make(chan server.Id)
	go server.HandleError(func() {
		defer close(next)
		for _, proxyId := range proxyIds {
			select {
			case <-prewarmCtx.Done():
				return
			case next <- proxyId:
			}
		}
	})

	readyCount := atomic.Int64{}
	wg := sync.WaitGroup{}
	concurrency := max(1, settings.PrewarmConcurrency)
	for range concurrency {
		wg.Add(1)
		go server.HandleError(func() {
			defer wg.Done()
			for proxyId := range next {
				// jitter spreads the platform dials so the herd does not
				// synchronize on the db/api
				select {
				case <-prewarmCtx.Done():
					return
				case <-time.After(time.Duration(rand.Int63n(int64(100 * time.Millisecond)))):
				}
				pd, err := proxyDeviceManager.OpenProxyDevice(proxyId)
				if err != nil {
					glog.Infof("[proxy][%s]prewarm open err=%s\n", proxyId, err)
					continue
				}
				if pd.WaitForReady(prewarmCtx, settings.PrewarmReadyTimeout) {
					readyCount.Add(1)
					prewarmedDevicesGauge.Set(float64(readyCount.Load()))
					if glog.V(1) {
						glog.Infof("[proxy][%s]prewarm ready\n", proxyId)
					}
				} else {
					glog.Infof("[proxy][%s]prewarm not ready within %s\n", proxyId, settings.PrewarmReadyTimeout)
				}
			}
		})
	}
	wg.Wait()

	glog.Infof("[proxy]prewarm: %d/%d devices ready\n", readyCount.Load(), len(proxyIds))
	return int(readyCount.Load())
}
