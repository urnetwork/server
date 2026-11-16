package proxy

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Closes the hosted devices of a client removed through the api
// (CLIENTCLOSE.md section 5). After a removal commits, the api sets the
// client's `{<clientId>}c_removed` flag (model.SetClientsRemoved), and the
// flag's keyspace event reaches every proxy process. Before this, a removed
// client's device served until its 90-minute idle timeout.

var (
	clientRemovedSubscribesCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "client_removed_subscribes_total",
		Help:      "Subscriptions and resubscriptions to the removed client key events; each checks the clients of the installed devices",
	})
	clientRemovedDeviceClosesCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "client_removed_device_closes_total",
		Help:      "Installed devices closed because their client was removed through the api, by what found the removal",
	}, []string{"trigger"})
	clientRemovedReadFailuresCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "proxy",
		Name:      "client_removed_read_failures_total",
		Help:      "Removed flag reads that failed; their devices wait for the next subscription or their idle timeout",
	})
)

func init() {
	prometheus.MustRegister(
		clientRemovedSubscribesCounter,
		clientRemovedDeviceClosesCounter,
		clientRemovedReadFailuresCounter,
	)
}

func DefaultClientRemovedSettings() *ClientRemovedSettings {
	return &ClientRemovedSettings{
		ResubscribeTimeout:      5 * time.Second,
		TopologyRefreshInterval: 60 * time.Second,
		CheckConcurrency:        16,
		CheckTimeout:            5 * time.Second,
	}
}

type ClientRemovedSettings struct {
	// backoff between failed (re)subscribes
	ResubscribeTimeout time.Duration
	// how often the subscription re-checks the cluster master set (a moved
	// slot emits events on the new owner)
	TopologyRefreshInterval time.Duration
	// the most removed flag reads in flight in one (re)subscribe check
	CheckConcurrency int
	// bounds each removed flag read
	CheckTimeout time.Duration

	// The key event subscription and the flag read. Nil uses Redis; tests
	// replace them.
	subscribe       func(ctx context.Context, topologyRefresh time.Duration) (<-chan server.RedisMessage, <-chan struct{}, func(), error)
	isClientRemoved func(ctx context.Context, clientId server.Id) bool
}

// Subscribes to the removed flag events of every client on the configured db,
// on every master node. A raise is returned as an error, so that the
// subscriber retries instead of ending for the life of the process.
func subscribeClientRemovedKeyEvents(
	ctx context.Context,
	topologyRefresh time.Duration,
) (messages <-chan server.RedisMessage, done <-chan struct{}, unsub func(), returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			returnErr = fmt.Errorf("client removed subscribe raised: %v", r)
		}
	}()
	return server.SubscribeKeyEvents(
		ctx,
		topologyRefresh,
		model.ClientRemovedKeyEventPattern(server.RedisDb()),
	)
}

// One per device manager. It holds the subscription to every client's
// removed flag events and cancels the installed devices of a removed client,
// as an idle device is canceled; each device's own worker then uninstalls
// and closes it.
//
// Every (re)established subscription checks the flag of each installed
// device's client: events published while unsubscribed are gone, and a
// device has no other prompt backstop.
type clientRemovedSubscriber struct {
	ctx      context.Context
	manager  *ProxyDeviceManager
	settings *ClientRemovedSettings

	subscribe       func(ctx context.Context, topologyRefresh time.Duration) (<-chan server.RedisMessage, <-chan struct{}, func(), error)
	isClientRemoved func(ctx context.Context, clientId server.Id) bool

	// closed when the subscriber has stopped, after its context ends
	done chan struct{}
}

func newClientRemovedSubscriber(
	ctx context.Context,
	manager *ProxyDeviceManager,
	settings *ClientRemovedSettings,
) *clientRemovedSubscriber {
	subscriber := &clientRemovedSubscriber{
		ctx:             ctx,
		manager:         manager,
		settings:        settings,
		subscribe:       settings.subscribe,
		isClientRemoved: settings.isClientRemoved,
		done:            make(chan struct{}),
	}
	if subscriber.subscribe == nil {
		subscriber.subscribe = subscribeClientRemovedKeyEvents
	}
	if subscriber.isClientRemoved == nil {
		subscriber.isClientRemoved = model.IsClientRemoved
	}
	go server.HandleError(func() {
		defer close(subscriber.done)
		subscriber.run()
	})
	return subscriber
}

func (self *clientRemovedSubscriber) Done() <-chan struct{} {
	return self.done
}

// Supervises the subscription: (re)subscribe, check the installed devices'
// clients for removals the gap may have hidden, cancel devices on removal
// events until the subscription ends, repeat. Errors back off; the loop only
// exits with the manager.
func (self *clientRemovedSubscriber) run() {
	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}

		messages, done, unsub, err := self.subscribe(self.ctx, self.settings.TopologyRefreshInterval)
		if err != nil {
			if self.ctx.Err() == nil {
				glog.Infof("[proxy]client removed subscribe err=%s\n", err)
			}
		} else {
			clientRemovedSubscribesCounter.Inc()
			self.drain(messages, done, unsub)
		}

		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.ResubscribeTimeout):
		}
	}
}

// Dispatches the subscription's events until it ends. Events published before
// it began are gone, so it checks the installed devices' clients alongside,
// and joins that check before it unsubscribes.
func (self *clientRemovedSubscriber) drain(
	messages <-chan server.RedisMessage,
	done <-chan struct{},
	unsub func(),
) {
	defer unsub()

	checkCtx, checkCancel := context.WithCancel(self.ctx)
	checkDone := make(chan struct{})
	go server.HandleError(func() {
		defer close(checkDone)
		self.check(checkCtx)
	})
	defer func() {
		checkCancel()
		<-checkDone
	}()

	for {
		select {
		case <-self.ctx.Done():
			return
		case <-done:
			return
		case message, ok := <-messages:
			if !ok {
				return
			}
			self.dispatch(message.Channel, message.Payload)
		}
	}
}

// Routes one keyspace notification of a removed flag. `event` is the
// command's event name (the message payload). Only the SET of a removal marks
// the client removed; the SET's own ttl (`expire`), the flag's expiry
// (`expired`) and a delete (`del`) do not. Canceling a device never blocks, so
// the dispatch needs no worker.
func (self *clientRemovedSubscriber) dispatch(channel string, event string) {
	if event != "set" {
		return
	}
	clientId, ok := model.ParseClientRemovedKeyEvent(channel)
	if !ok {
		return
	}
	self.cancelClientDevices(clientId, "event")
}

// Reads the removed flag of each installed device's client, at most
// CheckConcurrency at a time, and cancels the devices of the removed ones.
func (self *clientRemovedSubscriber) check(ctx context.Context) {
	clientIds := self.manager.deviceClientIds()
	if len(clientIds) == 0 {
		return
	}
	next := make(chan server.Id, len(clientIds))
	for _, clientId := range clientIds {
		next <- clientId
	}
	close(next)

	var checkers sync.WaitGroup
	for range min(max(1, self.settings.CheckConcurrency), len(clientIds)) {
		checkers.Add(1)
		go server.HandleError(func() {
			defer checkers.Done()
			for clientId := range next {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if removed, _ := self.readClientRemoved(ctx, clientId); removed {
					self.cancelClientDevices(clientId, "check")
				}
			}
		})
	}
	checkers.Wait()
}

// Reads the removed flag of a device's client once the device is installed.
// A dial whose session was minted before the removal committed can install
// its device after the removal's event found nothing to cancel. Installed
// first, the device is canceled by any later event, and this read finds an
// earlier removal.
func (self *clientRemovedSubscriber) checkInstalledDevice(pd *ProxyDevice) {
	if removed, _ := self.readClientRemoved(pd.ctx, pd.clientId); removed {
		self.cancelClientDevices(pd.clientId, "install")
	}
}

// One flag read, bounded by CheckTimeout. A failed read counts as not
// removed: the next (re)subscribe reads the flag again, and the idle timeout
// remains.
func (self *clientRemovedSubscriber) readClientRemoved(ctx context.Context, clientId server.Id) (removed bool, ok bool) {
	readCtx, readCancel := context.WithTimeout(ctx, self.settings.CheckTimeout)
	defer readCancel()
	defer func() {
		if r := recover(); r != nil {
			if ctx.Err() == nil {
				clientRemovedReadFailuresCounter.Inc()
				if glog.V(1) {
					glog.Infof("[proxy][%s]client removed read err=%v\n", clientId, r)
				}
			}
			removed = false
			ok = false
		}
	}()
	return self.isClientRemoved(readCtx, clientId), true
}

// Removals are client driven: counted, and logged only at V(1).
func (self *clientRemovedSubscriber) cancelClientDevices(clientId server.Id, trigger string) {
	canceledCount := self.manager.cancelClientDevices(clientId)
	if 0 < canceledCount {
		clientRemovedDeviceClosesCounter.WithLabelValues(trigger).Add(float64(canceledCount))
		if glog.V(1) {
			glog.Infof("[proxy][%s]client removed; closing %d devices (%s)\n", clientId, canceledCount, trigger)
		}
	}
}
