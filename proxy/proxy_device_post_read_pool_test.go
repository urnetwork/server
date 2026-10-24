// Actual TUN reads own pooled copies until the SDK consumes them. A context
// installed only on ProxyDevice forces its exact post-read activity refusal;
// the real TUN and SDK retain their ordinary independent fixture contexts.
package proxy

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect"
)

// ProxyDevice.Run is the sole consumer of this context. Subscription setup
// uses DeviceLocal's separate context; no constructor receives this wrapper.
type proxyPostReadContext struct {
	context.Context
	done     chan struct{}
	onCheck  func(int32)
	refuseAt int32
	checks   atomic.Int32
	once     sync.Once
}

func (self *proxyPostReadContext) Done() <-chan struct{} {
	check := self.checks.Add(1)
	if self.onCheck != nil {
		self.onCheck(check)
	}
	if check == self.refuseAt {
		self.close()
	}
	return self.done
}

func (self *proxyPostReadContext) Err() error {
	select {
	case <-self.done:
		return context.Canceled
	default:
		return nil
	}
}

func (self *proxyPostReadContext) close() {
	self.once.Do(func() { close(self.done) })
}

// Reuse the existing fully owned ProxyDevice fixture, replacing its generic
// test TUN with the actual hosted IPv4 settings before any packet is emitted.
func newProxyPostReadFixture(t *testing.T) *ProxyDevice {
	t.Helper()
	device := budgetTestProxyDevice(t, context.Background())
	if err := device.tun.Close(); err != nil {
		t.Fatal(err)
	}
	tun, err := connect.CreateTunWithResolver(device.ctx, proxyDeviceTunSettings(device.settings), nil)
	if err != nil {
		t.Fatal(err)
	}
	device.tun = tun
	return device
}

// These actual UDP writes queue three finite packets inside gVisor. They do
// not use a host socket, live route, synthetic ReadBatch or pooled producer.
func queueProxyPostReadPackets(t *testing.T, device *ProxyDevice) int {
	t.Helper()
	connection, err := device.tun.DialContext(context.Background(), "udp4", "198.51.100.9:443")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	packets := [][]byte{[]byte("first"), bytes.Repeat([]byte{0x52}, 300), bytes.Repeat([]byte{0x63}, 700)}
	for _, packet := range packets {
		if count, err := connection.Write(packet); err != nil || count != len(packet) {
			t.Fatalf("actual TUN datagram write: bytes=%d error=%v", count, err)
		}
	}
	return len(packets)
}

// Counters reconcile caller-owned roots and all size classes; a violation
// check independently rejects duplicate returns hidden by a net count.
type proxyPostReadPool struct {
	taken, returned, roots, violations uint64
	bytes                              connect.ByteCount
}

func proxyPostReadPoolNow() proxyPostReadPool {
	taken, returned, _ := connect.MessagePoolCounts()
	return proxyPostReadPool{taken: taken, returned: returned, roots: connect.MessagePoolOutstandingCount(), bytes: connect.MessagePoolOutstandingByteCount(), violations: connect.MessagePoolViolationCount()}
}

func (self proxyPostReadPool) require(t *testing.T, expectedTaken, expectedReturned uint64, marker string) {
	t.Helper()
	after := proxyPostReadPoolNow()
	held := after.roots - self.roots
	if after.taken-self.taken != expectedTaken || after.returned-self.returned != expectedReturned || after.roots != self.roots+expectedTaken-expectedReturned || after.violations != self.violations {
		t.Fatalf("%s: taken=%d returned=%d held_roots=%d violations=%d; want taken=%d returned=%d", marker, after.taken-self.taken, after.returned-self.returned, held, after.violations-self.violations, expectedTaken, expectedReturned)
	}
	if expectedTaken == expectedReturned && after.bytes != self.bytes {
		t.Fatalf("%s: root bytes=%d want baseline=%d", marker, after.bytes, self.bytes)
	}
}

// Install last: neither the actual TUN nor any SDK goroutine sees this Done
// counter. Deferred Run cancellation still closes the real fixture lifetime.
func installProxyPostReadContext(device *ProxyDevice, refuseAt int32, onCheck func(int32)) *proxyPostReadContext {
	ctx := &proxyPostReadContext{Context: device.ctx, done: make(chan struct{}), refuseAt: refuseAt, onCheck: onCheck}
	previousCancel := device.cancel
	device.ctx = ctx
	device.cancel = func() { ctx.close(); previousCancel() }
	return ctx
}

// OLD reaches the second check with exactly three newly caller-owned roots,
// then exits leaking all three. Only that reconciliation assertion is RED.
func TestProxyDeviceRunPostReadRefusalReturnsEveryPacket(t *testing.T) {
	device := newProxyPostReadFixture(t)
	packetCount := uint64(queueProxyPostReadPackets(t, device))
	before := proxyPostReadPoolNow()
	ctx := installProxyPostReadContext(device, 2, func(check int32) {
		switch check {
		case 1:
			before.require(t, 0, 0, "PROXY_POST_READ_PREMISE_FIRST_CHECK")
		case 2:
			before.require(t, packetCount, 0, "PROXY_POST_READ_PREMISE_SUCCESSFUL_BATCH")
		}
	})
	device.Run()
	if ctx.checks.Load() != 2 {
		t.Fatalf("activity-check ordering changed: got=%d want=2", ctx.checks.Load())
	}
	before.require(t, packetCount, packetCount, "PROXY_RUN_POST_READ_POOL_LEAK")
}

// Normal SDK admission consumes every original even when no route accepts it.
// The next top-of-loop check proves send returned before this cancellation.
func TestProxyDeviceRunNormalBatchConsumesEveryPacketOnce(t *testing.T) {
	device := newProxyPostReadFixture(t)
	packetCount := uint64(queueProxyPostReadPackets(t, device))
	before := proxyPostReadPoolNow()
	ctx := installProxyPostReadContext(device, 3, func(check int32) {
		switch check {
		case 1:
			before.require(t, 0, 0, "PROXY_POST_READ_NORMAL_FIRST_CHECK")
		case 2:
			before.require(t, packetCount, 0, "PROXY_POST_READ_NORMAL_OWNED_BATCH")
		case 3:
			before.require(t, packetCount, packetCount, "PROXY_POST_READ_NORMAL_CONSUMED_BATCH")
		}
	})
	device.Run()
	if ctx.checks.Load() != 3 {
		t.Fatalf("normal send did not reach next read boundary: checks=%d", ctx.checks.Load())
	}
	before.require(t, packetCount, packetCount, "PROXY_POST_READ_NORMAL_DOUBLE_RETURN")
}

// Refusing before ReadBatch never takes a pooled copy of queued gVisor data.
func TestProxyDeviceRunBeforeReadCancellationOwnsNothing(t *testing.T) {
	device := newProxyPostReadFixture(t)
	queueProxyPostReadPackets(t, device)
	before := proxyPostReadPoolNow()
	ctx := installProxyPostReadContext(device, 1, func(check int32) {
		if check != 1 {
			t.Fatalf("unexpected activity check before canceled read: %d", check)
		}
		before.require(t, 0, 0, "PROXY_POST_READ_CANCELED_BEFORE_READ")
	})
	device.Run()
	if ctx.checks.Load() != 1 {
		t.Fatalf("canceled Run reached ReadBatch: checks=%d", ctx.checks.Load())
	}
	before.require(t, 0, 0, "PROXY_POST_READ_CANCELED_OWNER")
}

// The actual empty-slice contract is zero/nil with no owner. A closed TUN
// returns zero/error; Run exits before its second activity check or any send.
func TestProxyDeviceRunEmptyAndErrorReadOwnNothing(t *testing.T) {
	device := newProxyPostReadFixture(t)
	before := proxyPostReadPoolNow()
	if count, err := device.tun.ReadBatch(nil); count != 0 || err != nil {
		t.Fatalf("empty actual ReadBatch changed: count=%d error=%v", count, err)
	}
	before.require(t, 0, 0, "PROXY_POST_READ_EMPTY_OWNER")
	if err := device.tun.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := installProxyPostReadContext(device, 99, func(check int32) {
		if check != 1 {
			t.Fatalf("failed actual read reached second activity check: %d", check)
		}
	})
	device.Run()
	if ctx.checks.Load() != 1 {
		t.Fatalf("error read check count=%d want=1", ctx.checks.Load())
	}
	before.require(t, 0, 0, "PROXY_POST_READ_ERROR_OWNER")
}
