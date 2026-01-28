package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

type watchdog struct {
	ctx         context.Context
	pollTimeout time.Duration
}

func newWatchdog(ctx context.Context, pollTimeout time.Duration) *watchdog {
	w := &watchdog{
		ctx:         ctx,
		pollTimeout: pollTimeout,
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

		testNetworkId := server.RequireParseId("018c224b-909e-d3f3-b0bf-f40b7e11c5d7")
		testUserId := server.RequireParseId("018c224b-909e-d3f3-b0bf-f40b2f8f8cfd")

		success := func() bool {
			testCtx, testCancel := context.WithCancel(self.ctx)
			defer testCancel()

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
			defer model.RemoveNetworkClient(
				&model.RemoveNetworkClientArgs{
					ClientId: clientId,
				},
				testSession,
			)

			proxyDeviceConfig := &model.ProxyDeviceConfig{
				InitialDeviceState: &model.ProxyDeviceState{
					Location: model.GetConnectLocationForCountryCode(self.ctx, "us"),
				},
			}
			proxyDeviceConfig.ClientId = clientId
			model.CreateProxyDeviceConfig(self.ctx, proxyDeviceConfig)
			defer model.RemoveProxyDeviceConfig(testCtx, proxyDeviceConfig.ProxyId)
			signedProxyId := model.SignProxyId(proxyDeviceConfig.ProxyId)

			httpProxyUrl, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", ListenHttpPort))
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
