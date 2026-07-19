package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type watchdog struct {
	ctx         context.Context
	pollTimeout time.Duration
	port        int
}

func newWatchdog(ctx context.Context, pollTimeout time.Duration, port int) *watchdog {
	w := &watchdog{
		ctx:         ctx,
		pollTimeout: pollTimeout,
		port:        port,
	}
	go server.HandleError(w.run, func() {
		os.Exit(1)
	})
	return w
}

func (self *watchdog) run() {
	for {
		select {
		case <-self.ctx.Done():
		case <-time.After(self.pollTimeout):
		}

		// FIXME read from config
		testNetworkId := server.RequireParseId("018c224b-909e-d3f3-b0bf-f40b7e11c5d7")
		testUserId := server.RequireParseId("018c224b-909e-d3f3-b0bf-f40b2f8f8cfd")

		success := func() bool {
			testCtx, testCancel := context.WithCancel(self.ctx)
			defer testCancel()

			// The teardown below MUST run on a context independent of self.ctx.
			// On a graceful shutdown self.ctx — and therefore testCtx — is already
			// canceled by the time these deferred removals run, so a testCtx-based
			// delete fails immediately and leaks the just-created top-level client
			// as active. The proxy redeploys often and a single poll can take
			// minutes (120s x up to 5 attempts), so SIGTERM lands mid-poll
			// routinely; that leak is what accumulated thousands of "proxy
			// watchdog" clients in the test network and tripped the peer valve
			// (model.NetworkPeersEnabled). context.Background keeps teardown alive
			// through shutdown; the timeout bounds it so a stuck delete can't hang
			// the process.
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cleanupCancel()

			testSession := &session.ClientSession{
				Ctx:           testCtx,
				Cancel:        testCancel,
				ClientAddress: "0.0.0.0:0",
				Header:        map[string][]string{},
				ByJwt: &jwt.ByJwt{
					NetworkId: testNetworkId,
					UserId:    testUserId,
				},
			}

			result, err := model.AuthNetworkClient(
				&model.AuthNetworkClientArgs{
					Description: "proxy watchdog",
					DeviceSpec:  "proxy watchdog",
				},
				testSession,
			)
			if err != nil {
				panic(err)
			}

			clientId := *result.ClientId
			cleanupSession := &session.ClientSession{
				Ctx:           cleanupCtx,
				ClientAddress: "0.0.0.0:0",
				Header:        map[string][]string{},
				ByJwt: &jwt.ByJwt{
					NetworkId: testNetworkId,
					UserId:    testUserId,
				},
			}
			defer model.RemoveNetworkClient(
				&model.RemoveNetworkClientArgs{
					ClientId: clientId,
				},
				cleanupSession,
			)

			proxyDeviceConfig := &model.ProxyDeviceConfig{
				InitialDeviceState: &model.ProxyDeviceState{
					Location: model.GetConnectLocationForCountryCode(self.ctx, "us"),
				},
			}
			proxyDeviceConfig.ClientId = clientId
			model.CreateProxyDeviceConfig(self.ctx, proxyDeviceConfig)
			defer model.RemoveProxyDeviceConfig(cleanupCtx, proxyDeviceConfig.ProxyId)
			signedProxyId := model.SignProxyId(proxyDeviceConfig.ProxyId)

			listenIpv4, _, listenPort := server.RequireListenIpPort(self.port)

			httpProxyUrl, err := url.Parse(fmt.Sprintf("http://%s:%d", listenIpv4, listenPort))
			if err != nil {
				panic(err)
			}

			proxyConnectHeader := http.Header{}
			proxyConnectHeader.Add("Proxy-Authorization", fmt.Sprintf("Bearer %s", signedProxyId))

			proxyHttpClient := &http.Client{
				Timeout: 120 * time.Second,
				Transport: &http.Transport{
					Proxy:              http.ProxyURL(httpProxyUrl),
					ProxyConnectHeader: proxyConnectHeader,
					DisableKeepAlives:  true,
				},
			}

			n := 5
			success := false
			for i := range n {
				r, err := http.NewRequestWithContext(self.ctx, "GET", "https://api.bringyour.com/hello", nil)
				if err != nil {
					panic(err)
				}
				response, err := proxyHttpClient.Do(r)
				if err == nil && response.StatusCode == http.StatusOK {
					glog.Infof("[proxy][%d/%d]watchdog poll success\n", i+1, n)
					success = true
					break
				}
				if i+1 < n {
					glog.Infof("[proxy][%d/%d]watchdog poll failed err=%s. Will retry ...\n", i+1, n, err)
				} else {
					glog.Infof("[proxy][%d/%d]watchdog poll failed err=%s. Exiting.\n", i+1, n, err)
				}
			}
			return success
		}()
		select {
		case <-self.ctx.Done():
			return
		default:
		}
		if !success {
			os.Exit(1)
		}
	}
}
