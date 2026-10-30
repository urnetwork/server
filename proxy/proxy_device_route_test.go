package proxy

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// publishRouteTestProviders reports count providers in the window, below its
// minimum, as a forming or degraded window does.
func publishRouteTestProviders(source *proxyDeviceReadinessTestSource, count int) {
	source.mutex.Lock()
	source.status.ProviderStateAdded = count
	status := source.status
	listener := source.listener
	source.mutex.Unlock()
	if listener != nil {
		listener.WindowStatusChanged(&status)
	}
}

// A dial waits only while the window has no provider at all: a window below
// its minimum still routes through the providers it holds. The wait ends with
// the first provider, the timeout, the caller, or the device.
func TestProxyDeviceWaitForRoute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deviceCtx, cancelDevice := context.WithCancel(context.Background())
		defer cancelDevice()

		ready := &ProxyDevice{ctx: deviceCtx, deviceState: newProxyDeviceReadinessTestSource(true)}
		start := time.Now()
		if !ready.WaitForRoute(context.Background(), time.Minute) || time.Since(start) != 0 {
			t.Fatal("a ready window did not route at once")
		}
		partialSource := newProxyDeviceReadinessTestSource(false)
		publishRouteTestProviders(partialSource, 1)
		partial := &ProxyDevice{ctx: deviceCtx, deviceState: partialSource}
		if !partial.WaitForRoute(context.Background(), time.Minute) || time.Since(start) != 0 {
			t.Fatal("a window below its minimum with a provider did not route at once")
		}

		formingSource := newProxyDeviceReadinessTestSource(false)
		forming := &ProxyDevice{ctx: deviceCtx, deviceState: formingSource}
		result := make(chan bool, 1)
		go func() {
			result <- forming.WaitForRoute(context.Background(), time.Minute)
		}()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		select {
		case <-result:
			t.Fatal("a window with no provider routed")
		default:
		}
		publishRouteTestProviders(formingSource, 1)
		if !<-result || time.Since(start) != 5*time.Second {
			t.Fatalf("the first provider did not end the wait (at %s)", time.Since(start))
		}
		if !formingSource.unsubscribed.Load() {
			t.Fatal("the wait retained its status listener")
		}

		never := &ProxyDevice{ctx: deviceCtx, deviceState: newProxyDeviceReadinessTestSource(false)}
		start = time.Now()
		if never.WaitForRoute(context.Background(), 30*time.Second) || time.Since(start) != 30*time.Second {
			t.Fatalf("a window that never forms did not give up at the timeout (at %s)", time.Since(start))
		}
		callerCtx, cancelCaller := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelCaller()
		start = time.Now()
		if never.WaitForRoute(callerCtx, 30*time.Second) || time.Since(start) != 2*time.Second {
			t.Fatalf("the caller's deadline did not end the wait (at %s)", time.Since(start))
		}
		cancelDevice()
		if never.WaitForRoute(context.Background(), 30*time.Second) {
			t.Fatal("a closed device routed")
		}
	})
}
