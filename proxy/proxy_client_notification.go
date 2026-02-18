package main

import (
	"context"
	"time"

	"golang.org/x/exp/maps"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	// "github.com/urnetwork/proxy"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	// "github.com/urnetwork/server/router"
)

type ProxyClientsFunction = func(proxyClients []*model.ProxyClient)

type proxyClientNotification struct {
	ctx    context.Context
	cancel context.CancelFunc

	settings *ProxySettings

	proxyClientsCallbacks *connect.CallbackList[ProxyClientsFunction]
}

func newProxyClientNotification(ctx context.Context, settings *ProxySettings) *proxyClientNotification {
	cancelCtx, cancel := context.WithCancel(ctx)
	p := &proxyClientNotification{
		ctx:                   cancelCtx,
		cancel:                cancel,
		settings:              settings,
		proxyClientsCallbacks: connect.NewCallbackList[ProxyClientsFunction](),
	}
	go server.HandleError(p.run, cancel)
	return p
}

func (self *proxyClientNotification) run() {
	defer self.cancel()

	proxyHost := server.RequireHost()
	block := server.RequireBlock()

	monitor := connect.NewMonitor()

	go server.HandleError(func() {
		defer self.cancel()

		event, sub := server.Subscribe(self.ctx, model.ProxyClientChannel(proxyHost, block))
		defer sub()

		for {
			select {
			case <-self.ctx.Done():
				return
			case <-event:
				monitor.NotifyAll()
			}
		}
	})

	nextChangeId := int64(0)
	for {
		notify := monitor.NotifyChannel()
		proxyClients, maxChangeId, err := model.GetProxyClientsSince(
			self.ctx,
			proxyHost,
			block,
			nextChangeId,
		)
		if err != nil {
			glog.Infof("[pcn]err=%s\n", err)
		} else if 0 < len(proxyClients) {

			glog.Infof("[pcn]found %d new proxy clients (%d..%d)\n", len(proxyClients), nextChangeId, maxChangeId)
			self.proxyClients(maps.Values(proxyClients))
		}
		nextChangeId = maxChangeId + 1
		select {
		case <-self.ctx.Done():
			return
		case <-notify:
		case <-time.After(self.settings.NotificationTimeout):
		}
	}
}

func (self *proxyClientNotification) proxyClients(proxyClients []*model.ProxyClient) {
	for _, proxyClientsCallback := range self.proxyClientsCallbacks.Get() {
		server.HandleError(func() {
			proxyClientsCallback(proxyClients)
		})
	}
}

func (self *proxyClientNotification) AddProxyClientsCallback(proxyClientsCallback ProxyClientsFunction) func() {
	callbackId := self.proxyClientsCallbacks.Add(proxyClientsCallback)
	return func() {
		self.proxyClientsCallbacks.Remove(callbackId)
	}
}
