package server

// stats publishing to the warp grafana service.
// every service pushes the default prometheus registry to the stable local
// publish port on its host (`local_port` in vault grafana.yml), keyed by
// {env, service, block, host}. the grafana go front stamps the receive time
// and forwards to mimir, so series go stale when a service stops pushing.
// register service metrics with the default prometheus registry
// (see connect/transport.go for an example) and they are pushed
// automatically.
// see warp/grafana in the warp repo

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"

	"github.com/urnetwork/glog"
)

const statsPushInterval = 15 * time.Second

const defaultGrafanaLocalPort = 3100

var buildInfoGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Name:      "build_info",
		Help:      "Build info of the running service",
	},
	[]string{"version"},
)

func init() {
	prometheus.MustRegister(buildInfoGauge)
}

// StartStatsPusher pushes the default prometheus registry
// (go runtime, process, and service metrics) to the local grafana
// publish port every interval, until the context is done.
// a no-op when not running under warp
// or when grafana.yml is not in the vault
func StartStatsPusher(ctx context.Context) {
	env, err := Env()
	if err != nil {
		glog.Infof("[stats]not running under warp (%s). Not publishing stats.\n", err)
		return
	}
	service, err := Service()
	if err != nil {
		glog.Infof("[stats]not running under warp (%s). Not publishing stats.\n", err)
		return
	}
	block, err := Block()
	if err != nil {
		glog.Infof("[stats]not running under warp (%s). Not publishing stats.\n", err)
		return
	}
	host, err := Host()
	if err != nil {
		glog.Infof("[stats]not running under warp (%s). Not publishing stats.\n", err)
		return
	}

	grafanaResource, err := Vault.SimpleResource("grafana.yml")
	if err != nil {
		glog.Infof("[stats]no grafana.yml in the vault (%s). Not publishing stats.\n", err)
		return
	}
	localPort := defaultGrafanaLocalPort
	if localPorts := grafanaResource.Int("local_port"); 0 < len(localPorts) {
		localPort = localPorts[0]
	}

	if version, err := Version(); err == nil {
		buildInfoGauge.WithLabelValues(version).Set(1)
	}

	pusher := push.New(fmt.Sprintf("http://127.0.0.1:%d", localPort), service).
		Gatherer(prometheus.DefaultGatherer).
		Client(&http.Client{
			Timeout: 10 * time.Second,
		}).
		Grouping("env", env).
		Grouping("service", service).
		Grouping("block", block).
		Grouping("host", host)

	glog.Infof("[stats]publishing to http://127.0.0.1:%d\n", localPort)

	go HandleError(func() {
		pushOk := true
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(statsPushInterval):
			}

			if err := pusher.PushContext(ctx); err != nil {
				// log on state change only, e.g. during a grafana redeploy
				if pushOk {
					glog.Infof("[stats]push error (%s)\n", err)
				}
				pushOk = false
			} else {
				if !pushOk {
					glog.Infof("[stats]push ok\n")
				}
				pushOk = true
			}
		}
	})
}
