package proxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/exp/maps"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	// "github.com/urnetwork/proxy"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	// "github.com/urnetwork/server/router"
)

// this is used to migrate from older configurations that mistakenly used the fully qualified host
const DebugWatchFqHost = true

// ProxyClientsFunction delivers newly changed proxy clients. A non-nil error
// means the delivery failed and the same clients will be delivered again on
// the next poll (the notification does not advance past them).
type ProxyClientsFunction = func(proxyClients []*model.ProxyClient) error

// ProxyClientsFullSyncFunction delivers the full set of proxy clients for the
// watched hosts/block, read from the db starting at syncStartTime. Used to
// reconcile state that must be bounded by the current set (e.g. wg peers).
type ProxyClientsFullSyncFunction = func(proxyClients []*model.ProxyClient, syncStartTime time.Time)

type proxyClientNotification struct {
	ctx    context.Context
	cancel context.CancelFunc

	settings *ProxySettings

	proxyClientsCallbacks *connect.CallbackList[ProxyClientsFunction]
	fullSyncCallbacks     *connect.CallbackList[ProxyClientsFullSyncFunction]
}

func NewProxyClientNotification(ctx context.Context, settings *ProxySettings) *proxyClientNotification {
	cancelCtx, cancel := context.WithCancel(ctx)
	p := &proxyClientNotification{
		ctx:                   cancelCtx,
		cancel:                cancel,
		settings:              settings,
		proxyClientsCallbacks: connect.NewCallbackList[ProxyClientsFunction](),
		fullSyncCallbacks:     connect.NewCallbackList[ProxyClientsFullSyncFunction](),
	}
	go server.HandleError(p.run, cancel)
	return p
}

func (self *proxyClientNotification) run() {
	defer self.cancel()

	proxyHost := server.RequireHost()
	block := server.RequireBlock()

	hosts := []string{proxyHost}

	if DebugWatchFqHost && !strings.Contains(proxyHost, ".") {
		fqProxyHost := fmt.Sprintf("%s.%s", proxyHost, server.RequireDomain())
		hosts = append(hosts, fqProxyHost)
		go server.HandleError(func() {
			self.watch(fqProxyHost, block)
		})
	}

	go server.HandleError(func() {
		self.watch(proxyHost, block)
	})

	go server.HandleError(func() {
		self.reconcile(hosts, block)
	})

	select {
	case <-self.ctx.Done():
	}
}

// reconcile periodically reads the full set of proxy clients for the watched
// hosts and delivers it to the full sync callbacks. This heals clients that
// were dropped from an earlier delivery (e.g. a partial wg restore) and lets
// the wg server remove peers whose proxy client no longer exists.
func (self *proxyClientNotification) reconcile(hosts []string, block string) {
	if self.settings.ReconcileTimeout <= 0 {
		return
	}
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.ReconcileTimeout):
		}

		syncStartTime := server.NowUtc()
		allProxyClients := map[server.Id]*model.ProxyClient{}
		ok := true
		for _, host := range hosts {
			proxyClients, _, err := model.GetProxyClientsSince(self.ctx, host, block, 0)
			if err != nil {
				// never reconcile from partial data, since the full sync is
				// used to remove stale state
				glog.Infof("[proxy]reconcile err=%s (skipping round)\n", err)
				ok = false
				break
			}
			maps.Copy(allProxyClients, proxyClients)
		}
		if !ok {
			continue
		}
		glog.Infof("[proxy]reconcile %d proxy clients\n", len(allProxyClients))
		self.fullSyncProxyClients(maps.Values(allProxyClients), syncStartTime)
	}
}

func (self *proxyClientNotification) watch(host string, block string) {
	monitor := connect.NewMonitor()

	go server.HandleError(func() {
		defer self.cancel()

		event, unsub := server.Subscribe(self.ctx, model.ProxyClientChannel(host, block))
		defer unsub()

		for {
			select {
			case <-self.ctx.Done():
				return
			case <-event:
				// coealsce remaining events into a single event
				func() {
					rateLimit := time.After(self.settings.EventRateLimitTimeout)
					for {
						select {
						case <-self.ctx.Done():
							return
						case <-event:
						case <-rateLimit:
							return
						}
					}
				}()
				func() {
					for {
						select {
						case <-self.ctx.Done():
							return
						case <-event:
						default:
							return
						}
					}
				}()
				select {
				case <-self.ctx.Done():
					return
				default:
					glog.V(2).Infof("[proxy]notify\n")
					monitor.NotifyAll()
				}
			}
		}
	})

	nextChangeId := int64(0)
	for {
		notify := monitor.NotifyChannel()
		proxyClients, maxChangeId, err := model.GetProxyClientsSince(
			self.ctx,
			host,
			block,
			nextChangeId,
		)
		if err != nil {
			glog.Infof("[proxy]err=%s\n", err)
		} else if 0 < len(proxyClients) {
			glog.Infof("[proxy]found %d new proxy clients (%d..%d)\n", len(proxyClients), nextChangeId, maxChangeId)
			if err := self.proxyClients(maps.Values(proxyClients)); err == nil {
				nextChangeId = maxChangeId + 1
			} else {
				// do not advance past a failed delivery;
				// the same range is retried on the next poll
				glog.Infof("[proxy]proxy clients callback err=%s (will retry)\n", err)
			}
		}
		select {
		case <-self.ctx.Done():
			return
		case <-notify:
		case <-time.After(self.settings.NotificationTimeout):
		}
	}
}

func (self *proxyClientNotification) proxyClients(proxyClients []*model.ProxyClient) (returnErr error) {
	proxyClientsCallbacks := self.proxyClientsCallbacks.Get()
	if len(proxyClientsCallbacks) == 0 {
		// the watch may win the race with callback registration at startup.
		// Failing the delivery retries it on the next poll instead of
		// consuming the change range with no listener.
		return fmt.Errorf("no proxy clients callbacks registered")
	}
	for _, proxyClientsCallback := range proxyClientsCallbacks {
		var err error
		if r := server.HandleError(func() {
			err = proxyClientsCallback(proxyClients)
		}); r != nil {
			err = errors.Join(err, fmt.Errorf("proxy clients callback panic: %v", r))
		}
		returnErr = errors.Join(returnErr, err)
	}
	return
}

func (self *proxyClientNotification) fullSyncProxyClients(proxyClients []*model.ProxyClient, syncStartTime time.Time) {
	for _, fullSyncCallback := range self.fullSyncCallbacks.Get() {
		server.HandleError(func() {
			fullSyncCallback(proxyClients, syncStartTime)
		})
	}
}

func (self *proxyClientNotification) AddProxyClientsCallback(proxyClientsCallback ProxyClientsFunction) func() {
	callbackId := self.proxyClientsCallbacks.Add(proxyClientsCallback)
	return func() {
		self.proxyClientsCallbacks.Remove(callbackId)
	}
}

func (self *proxyClientNotification) AddProxyClientsFullSyncCallback(fullSyncCallback ProxyClientsFullSyncFunction) func() {
	callbackId := self.fullSyncCallbacks.Add(fullSyncCallback)
	return func() {
		self.fullSyncCallbacks.Remove(callbackId)
	}
}
