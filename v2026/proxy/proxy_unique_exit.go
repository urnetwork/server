package proxy

// The data plane of a unique-exit hosted proxy (HOSTED-PROXY-UNIQUE-EXIT.md §5,
// model/proxy_unique_exit_model.go):
//   - the device follows its exit assignment, and repins its location when the
//     provider changes (a replacement or a rotate);
//   - it confirms its exit through its own tunnel with the operator's /ip echo,
//     and the model compares the keyed address with the network's other
//     exits;
//   - repeated failed confirms report the exit unreachable, which the sweeper
//     replaces after the grace;
//   - the throughput share holds a pinned proxy to its provider's estimate
//     times UniqueExitSettings.ThroughputShare, so a scraper's load cannot take
//     a home's whole line.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

var proxyUniqueExitSharePoliced = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_proxy_unique_exit_share_policed_packets_total",
	Help: "WireGuard packets of unique-exit proxies dropped by the throughput share, by direction",
}, []string{"direction"})

func init() {
	prometheus.MustRegister(proxyUniqueExitSharePoliced)
}

// uniqueExitShare is a token bucket in bytes. A zero rate is unlimited: a
// provider with no throughput estimate has no share to hold it to.
type uniqueExitShare struct {
	mutex          sync.Mutex
	bytesPerSecond float64
	burst          float64
	tokens         float64
	last           time.Time
	now            func() time.Time
	sleep          func(ctx context.Context, d time.Duration) error
}

// the burst is a quarter second of the rate, never below the largest single
// read or write the proxy moves at once
const uniqueExitShareMinBurst = 64 * 1024

func newUniqueExitShare(now func() time.Time) *uniqueExitShare {
	return &uniqueExitShare{
		now: now,
		sleep: func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

func (self *uniqueExitShare) setRate(bytesPerSecond float64) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.bytesPerSecond = max(0, bytesPerSecond)
	self.burst = max(uniqueExitShareMinBurst, self.bytesPerSecond/4)
	self.tokens = self.burst
	self.last = self.now()
}

func (self *uniqueExitShare) refillLocked(now time.Time) {
	if elapsed := now.Sub(self.last); 0 < elapsed {
		self.tokens = min(self.burst, self.tokens+elapsed.Seconds()*self.bytesPerSecond)
	}
	self.last = now
}

// allow spends n bytes if the bucket holds them, without waiting: for packets,
// which are dropped rather than queued when over the share.
func (self *uniqueExitShare) allow(n int) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.bytesPerSecond <= 0 {
		return true
	}
	self.refillLocked(self.now())
	if float64(n) <= self.tokens {
		self.tokens -= float64(n)
		return true
	}
	return false
}

// reserve spends n bytes now and returns how long to wait until they are
// covered. The tokens may go negative, which makes later callers wait in turn.
func (self *uniqueExitShare) reserve(n int) time.Duration {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.bytesPerSecond <= 0 {
		return 0
	}
	self.refillLocked(self.now())
	self.tokens -= float64(n)
	if 0 <= self.tokens {
		return 0
	}
	return time.Duration(-self.tokens / self.bytesPerSecond * float64(time.Second))
}

// wait spends n bytes, waiting as long as the share needs.
func (self *uniqueExitShare) wait(ctx context.Context, n int) error {
	if d := self.reserve(n); 0 < d {
		return self.sleep(ctx, d)
	}
	return nil
}

// chunk is how much of a write to pace at once, or 0 when the share is
// unlimited (no rate yet, or no estimate) and the write goes out whole.
func (self *uniqueExitShare) chunk() int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.bytesPerSecond <= 0 {
		return 0
	}
	return max(1, int(self.burst))
}

// uniqueExitShareConn paces a proxied connection to the device's share. TCP
// flow control carries the pace back to the provider, so nothing is dropped.
type uniqueExitShareConn struct {
	net.Conn
	ctx   context.Context
	share *uniqueExitShare
}

func (self *uniqueExitShareConn) Read(b []byte) (int, error) {
	n, err := self.Conn.Read(b)
	if 0 < n {
		if waitErr := self.share.wait(self.ctx, n); waitErr != nil && err == nil {
			err = waitErr
		}
	}
	return n, err
}

func (self *uniqueExitShareConn) Write(b []byte) (int, error) {
	written := 0
	for written < len(b) {
		size := self.share.chunk()
		if size <= 0 {
			n, err := self.Conn.Write(b[written:])
			return written + n, err
		}
		end := min(len(b), written+size)
		if err := self.share.wait(self.ctx, end-written); err != nil {
			return written, err
		}
		n, err := self.Conn.Write(b[written:end])
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// The model calls the device makes, injectable for tests.
type proxyUniqueExitSource interface {
	GetProxyExitForDevice(ctx context.Context, proxyClientId server.Id) *model.ProxyExitForDevice
	ConfirmProxyExit(ctx context.Context, proxyClientId server.Id, assignmentId server.Id, observedKey []byte, now time.Time) string
	ReportProxyExitFailing(ctx context.Context, proxyClientId server.Id, assignmentId server.Id, reason string, now time.Time)
}

type modelProxyUniqueExitSource struct{}

func (modelProxyUniqueExitSource) GetProxyExitForDevice(ctx context.Context, proxyClientId server.Id) *model.ProxyExitForDevice {
	return model.GetProxyExitForDevice(ctx, proxyClientId)
}

func (modelProxyUniqueExitSource) ConfirmProxyExit(ctx context.Context, proxyClientId server.Id, assignmentId server.Id, observedKey []byte, now time.Time) string {
	return model.ConfirmProxyExit(ctx, proxyClientId, assignmentId, observedKey, now)
}

func (modelProxyUniqueExitSource) ReportProxyExitFailing(ctx context.Context, proxyClientId server.Id, assignmentId server.Id, reason string, now time.Time) {
	model.ReportProxyExitFailing(ctx, proxyClientId, assignmentId, reason, now)
}

// proxyUniqueExit is one device's loop. step does one pass and is what the
// tests drive; run repeats it.
type proxyUniqueExit struct {
	clientId    server.Id
	settings    *model.UniqueExitSettings
	source      proxyUniqueExitSource
	setLocation func(location *sdk.ConnectLocation)
	echo        func(ctx context.Context) (netip.Addr, error)
	share       *uniqueExitShare
	now         func() time.Time
	// the provider the device was created pinned to, which needs no repin
	initialProviderId *server.Id

	// owned by the loop
	providerClientId *server.Id
	assignmentId     server.Id
	lastConfirm      time.Time
	confirmFailures  int
}

func (self *proxyUniqueExit) step(ctx context.Context) {
	exit := self.source.GetProxyExitForDevice(ctx, self.clientId)
	if exit == nil {
		return
	}
	if self.providerClientId == nil || *self.providerClientId != exit.ProviderClientId {
		// the first step finds the device already pinned where it was created;
		// a repin would only rebuild the window
		created := self.providerClientId == nil && self.initialProviderId != nil && *self.initialProviderId == exit.ProviderClientId
		if !created {
			self.setLocation(model.UniqueExitPinnedLocation(exit.ProviderClientId, exit.CountryCode))
		}
		providerClientId := exit.ProviderClientId
		self.providerClientId = &providerClientId
		self.share.setRate(self.settings.ThroughputShare * float64(exit.EstimatedBytesPerSecond))
		self.lastConfirm = time.Time{}
		self.confirmFailures = 0
	}
	if self.assignmentId != exit.AssignmentId {
		self.assignmentId = exit.AssignmentId
		self.lastConfirm = time.Time{}
		self.confirmFailures = 0
	}
	if exit.State == "replacing" {
		// the exit failed: the sweeper moves the proxy, and the next step
		// follows it
		return
	}
	now := self.now()
	if !self.lastConfirm.IsZero() && now.Sub(self.lastConfirm) < self.settings.ConfirmInterval {
		return
	}
	ip, err := self.echo(ctx)
	if err != nil {
		self.confirmFailures += 1
		// report at the threshold, then again at each further threshold's
		// worth of failures: the report is idempotent, and the sweeper acts on
		// the first one after the grace
		if threshold := max(1, self.settings.UnreachableConfirmFailures); self.confirmFailures%threshold == 0 {
			self.source.ReportProxyExitFailing(ctx, self.clientId, self.assignmentId, model.ProxyExitReasonUnreachable, now)
		}
		return
	}
	self.confirmFailures = 0
	self.source.ConfirmProxyExit(ctx, self.clientId, self.assignmentId, model.UniqueExitEgressKey(ip), now)
	self.lastConfirm = now
}

func (self *proxyUniqueExit) run(ctx context.Context) {
	for {
		if r := server.HandleError(func() {
			self.step(ctx)
		}); r != nil {
			glog.Infof("[upe]device %s step: %v\n", self.clientId, r)
		}
		// a few seconds of jitter keeps a host's devices from polling together
		interval := self.settings.AssignmentPollInterval
		jitter := time.Duration(rand.Int64N(int64(max(time.Second, interval/4))))
		timer := time.NewTimer(interval + jitter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// the echo answers at most this many bytes
const uniqueExitEchoMaxBytes = 64 * 1024

var errUniqueExitEchoNoAddress = errors.New("the ip echo answered no address")

// uniqueExitEcho asks the operator's /ip echo, through this device's own
// tunnel, what address the exit egresses from. The echo is the endpoint the
// egress probe already uses for the same question.
func (self *ProxyDevice) uniqueExitEcho(ctx context.Context, apiUrl string, timeout time.Duration) (netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	transport := &http.Transport{
		DialContext:       self.dialTun,
		ForceAttemptHTTP2: false,
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: timeout}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(apiUrl, "/")+"/my-ip-info", nil)
	if err != nil {
		return netip.Addr{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return netip.Addr{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("the ip echo answered %d", response.StatusCode)
	}
	var info struct {
		Info struct {
			Ip string `json:"ip"`
		} `json:"info"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, uniqueExitEchoMaxBytes)).Decode(&info); err != nil {
		return netip.Addr{}, err
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(info.Info.Ip))
	if err != nil {
		return netip.Addr{}, errUniqueExitEchoNoAddress
	}
	return addr.Unmap(), nil
}

// the echo's own deadline: the device's first request also waits for its one
// window client to come up
const uniqueExitEchoTimeout = 20 * time.Second

// startUniqueExit runs the device's unique-exit loop until the device closes.
// initialProviderClientId is the provider the device was created pinned to.
func (self *ProxyDevice) startUniqueExit(apiUrl string, initialProviderClientId *server.Id) {
	share := newUniqueExitShare(time.Now)
	self.uniqueExitShare = share
	loop := &proxyUniqueExit{
		clientId: self.clientId,
		settings: model.GetUniqueExitSettings(),
		source:   modelProxyUniqueExitSource{},
		setLocation: func(location *sdk.ConnectLocation) {
			self.deviceLocal.SetConnectLocation(location)
		},
		echo: func(ctx context.Context) (netip.Addr, error) {
			return self.uniqueExitEcho(ctx, apiUrl, uniqueExitEchoTimeout)
		},
		share:             share,
		now:               server.NowUtc,
		initialProviderId: initialProviderClientId,
	}
	go loop.run(self.ctx)
}

// policeUniqueExitPacket spends one WireGuard packet from the share, and
// counts it when the share drops it. A device without unique exit allows all.
func (self *ProxyDevice) policeUniqueExitPacket(direction string, n int) bool {
	share := self.uniqueExitShare
	if share == nil || share.allow(n) {
		return true
	}
	proxyUniqueExitSharePoliced.WithLabelValues(direction).Inc()
	return false
}
