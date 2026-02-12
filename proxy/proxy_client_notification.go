package main

import (
	"context"
	"time"

	"github.com/urnetwork/connect"
	// "github.com/urnetwork/glog"
	// "github.com/urnetwork/proxy"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	// "github.com/urnetwork/server/router"
)

type ProxyClientsFunction = func(proxyClients []*model.ProxyClient)

type proxyClientNotification struct {
	ctx context.Context

	settings *ProxySettings

	proxyClientsCallbacks *connect.CallbackList[ProxyClientsFunction]
}

func newProxyClientNotification(ctx context.Context, settings *ProxySettings) *proxyClientNotification {
	return &proxyClientNotification{
		ctx:                   ctx,
		settings:              settings,
		proxyClientsCallbacks: connect.NewCallbackList[ProxyClientsFunction](),
	}
}

func (self *proxyClientNotification) run() {
	nextChangeId := 0
	for {
		var proxyClients []*model.ProxyClient
		proxyClients, nextChangeId = model.GetProxyClientsSince(self.ctx, nextChangeId)
		if 0 < len(proxyClients) {
			self.proxyClients(proxyClients)
		}
		select {
		case <-self.ctx.Done():
			return
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
