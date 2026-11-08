package server

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"math"
	"net"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"
)

// sidecar.ClockCheck on chronyd (§19.6's clocks row, D-OPS-20, D-EVS-23). chronyd runs on every host
// that runs evs, gkm or taskworker and serves the services network its estimate of true time, while
// it only slews the system clock toward that estimate. So what this process reads can trail what
// chronyd answers, and the bound that matters is that gap plus how far chronyd may be from UTC: the
// measured offset plus the reply's root distance, RFC 5905 §4's maximum error, which is what
// `chronyc tracking` gives as a clock's absolute bound.
//
// The offset is a magnitude, and the root distance is taken from this process, so its own round trip
// counts as one more hop of root delay, as RFC 5905's root_dist() counts it. Half that round trip is
// how far the measured offset itself can be wrong, so a process stalled mid exchange cannot measure
// its way to a smaller bound than the truth. On a healthy host this is microseconds.
//
// Each second sends one packet, on a socket of its own, so a reply to an earlier query cannot reach a
// later one. A reply must echo that query's random transmit timestamp. Anything else is not a reply:
// it changes nothing, and the second it arrived in counts as one with no reply.
//
// Sure is a promise about now, so reads judge the time for themselves and do not lean on the timer
// that closes the channel: a timer that runs late cannot stretch a known offset past UnknownTimeout.
//
// This file links no sidecar package, so no service gains one by importing server (sidecar_require.go).
// Its methods use only standard types, so the type meets sidecar.ClockCheck as it stands.

// NTP's wire format (RFC 5905 §7.3), which are facts rather than settings.
const (
	ntpPort            = "123"
	ntpPacketByteCount = 48

	ntpVersion    = 4
	ntpModeClient = 3
	ntpModeServer = 4

	// the leap indicator's alarm condition: this clock is not synchronised
	ntpLeapUnsynchronised = 3
	// stratum 0 is a kiss code or unspecified, and 16 unsynchronised, so only those between are usable
	ntpStratumUnsynchronised = 16

	ntpRootDelayIndex       = 4
	ntpRootDispersionIndex  = 8
	ntpOriginTimestampIndex = 24
	ntpReceiveIndex         = 32
	ntpTransmitIndex        = 40

	// seconds from NTP's epoch, 1900, to Unix's, 1970
	ntpUnixEpochOffset = 2208988800
)

// The offset as monitor reads it. A process that has no bound exports +Inf, so one comparison covers
// §19.8's "past 25 ms or unknown", and a NaN would not do, since warp's grafana front drops those
// samples (warp/grafana/push.go), leaving a gap that reads as nothing wrong.
const chronyClockOffsetMetricName = "urnetwork_clock_offset_seconds"

// How the check is tuned. §19.6 sets every default, and §11.8's margins are built on them.
type ChronyClockCheckSettings struct {
	// Where chronyd answers, as host:port: warp's --add-host name for the services docker network
	// (warp/warpctl/run.go, servicesDockerNetwork), which is services.yml's services_docker_network.
	// A name that does not resolve leaves the check unknown.
	ChronyAddress string
	// Between one packet and the next. A reply has until the next is due. Non-positive sends none,
	// so the check stays unknown rather than spin.
	QueryInterval time.Duration
	// How long a reply's offset stands with no later reply. At that age the offset is unknown.
	// Non-positive: an offset never stands.
	UnknownTimeout time.Duration
	// The largest offset Sure allows, inclusive. §11.8's fence and §11.7's collection margins assume
	// at most 50 ms in each clock they involve, so raising this breaks them.
	SureOffset time.Duration
}

// §19.6's clocks row. warpservices is the network name warp's README gives.
func DefaultChronyClockCheckSettings() *ChronyClockCheckSettings {
	return &ChronyClockCheckSettings{
		ChronyAddress:  net.JoinHostPort("warpservices", ntpPort),
		QueryInterval:  1 * time.Second,
		UnknownTimeout: 3 * time.Second,
		SureOffset:     50 * time.Millisecond,
	}
}

// Time as the check reads it: the system's in a process, a schedule's in a test. sidecartest's Clock
// has this method set.
type chronyClock interface {
	Now() time.Time
	After(timeout time.Duration) <-chan time.Time
	AfterFunc(timeout time.Duration, function func()) (stop func() bool)
}

// The system's clock. Its times carry a monotonic reading, so ages stay true across a step of the
// wall clock, and the NTP timestamps made from them use the wall clock, as a measurement must.
type chronySystemClock struct{}

func (self chronySystemClock) Now() time.Time {
	return time.Now()
}

func (self chronySystemClock) After(timeout time.Duration) <-chan time.Time {
	return time.After(timeout)
}

func (self chronySystemClock) AfterFunc(timeout time.Duration, function func()) (stop func() bool) {
	return time.AfterFunc(timeout, function).Stop
}

// What a query needs of its socket: a connected UDP socket in a process.
type chronyConn interface {
	Write(packet []byte) (int, error)
	Read(packet []byte) (int, error)
	SetDeadline(deadline time.Time) error
	Close() error
}

// Runs until its ctx ends, and goes unknown then, so ctx must outlive the work that reads it: an
// engine's drain still deletes. One to a process: its gauge has no labels, so a second one alive
// panics, as prometheus's MustRegister does, and a host replacing one waits on Done first. Safe for
// concurrent use.
type ChronyClockCheck struct {
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	clock      chronyClock
	dial       func(ctx context.Context) (chronyConn, error)
	registerer prometheus.Registerer
	gauge      prometheus.GaugeFunc
	settings   *ChronyClockCheckSettings
	// cancels the timer that ends the newest reply's stand. Only run's goroutine touches this.
	stopLapse func() bool

	stateLock sync.Mutex
	offset    time.Duration
	known     bool
	// when a known offset stops being known
	knownUntil time.Time
	sure       bool
	// open through one sure stretch, closed at its end, and closed for as long as the clock is unsure
	unsureChannel chan struct{}
}

// Starts unknown, and asks chronyd at once. A nil settings takes the defaults.
func NewChronyClockCheck(ctx context.Context, settings *ChronyClockCheckSettings) *ChronyClockCheck {
	if settings == nil {
		settings = DefaultChronyClockCheckSettings()
	}
	dial := func(ctx context.Context) (chronyConn, error) {
		dialer := net.Dialer{}
		return dialer.DialContext(ctx, "udp", settings.ChronyAddress)
	}
	return newChronyClockCheck(ctx, chronySystemClock{}, dial, prometheus.DefaultRegisterer, settings)
}

// Closed once the check has ended and released its gauge.
func (self *ChronyClockCheck) Done() <-chan struct{} {
	return self.done
}

func newChronyClockCheck(
	ctx context.Context,
	clock chronyClock,
	dial func(ctx context.Context) (chronyConn, error),
	registerer prometheus.Registerer,
	settings *ChronyClockCheckSettings,
) *ChronyClockCheck {
	cancelCtx, cancel := context.WithCancel(ctx)
	unsureChannel := make(chan struct{})
	close(unsureChannel)
	self := &ChronyClockCheck{
		ctx:           cancelCtx,
		cancel:        cancel,
		done:          make(chan struct{}),
		clock:         clock,
		dial:          dial,
		registerer:    registerer,
		settings:      settings,
		unsureChannel: unsureChannel,
	}
	self.gauge = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: chronyClockOffsetMetricName,
			Help: "The most this process's clock may be from true time, +Inf while unknown (§19.6)",
		},
		func() float64 {
			offset, known := self.Offset()
			if !known {
				return math.Inf(1)
			}
			return offset.Seconds()
		},
	)
	registerer.MustRegister(self.gauge)
	go self.run()
	return self
}

// The most this clock may be from true time, zero and false while there is no such bound.
func (self *ChronyClockCheck) Offset() (time.Duration, bool) {
	now := self.clock.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lapseWithLock(now)
	return self.offset, self.known
}

// Whether the offset is known and within SureOffset.
func (self *ChronyClockCheck) Sure() bool {
	now := self.clock.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lapseWithLock(now)
	return self.sure
}

// The current sure stretch's channel, which closes the moment it ends. Closed already while unsure.
func (self *ChronyClockCheck) UnsureChannel() <-chan struct{} {
	now := self.clock.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lapseWithLock(now)
	return self.unsureChannel
}

func (self *ChronyClockCheck) setWithLock(offset time.Duration, known bool) {
	self.offset = offset
	self.known = known
	sure := known && offset <= self.settings.SureOffset
	if sure == self.sure {
		return
	}
	self.sure = sure
	if sure {
		self.unsureChannel = make(chan struct{})
	} else {
		close(self.unsureChannel)
	}
}

// Ends a known offset that has reached its age, whichever of a read and the timer gets there first.
func (self *ChronyClockCheck) lapseWithLock(now time.Time) {
	if self.known && !now.Before(self.knownUntil) {
		self.setWithLock(0, false)
	}
}

func (self *ChronyClockCheck) run() {
	defer func() {
		self.cancel()
		if self.stopLapse != nil {
			self.stopLapse()
		}
		// nothing checks this clock any more
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.setWithLock(0, false)
		}()
		self.registerer.Unregister(self.gauge)
		close(self.done)
	}()
	if self.settings.QueryInterval <= 0 {
		glog.Warningf("[clock]unsure: no query interval, so chronyd is never asked\n")
		return
	}
	// The first state and each turn are logged, with what the last query met, since §19.10's runbook
	// for an unsure clock starts from why. At most once a query, and from here so that no read waits
	// on a log.
	logged := false
	loggedSure := false
	for {
		issued := self.clock.Now()
		outcome := self.query()
		var offset time.Duration
		var known bool
		var sure bool
		func() {
			now := self.clock.Now()
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.lapseWithLock(now)
			offset, known, sure = self.offset, self.known, self.sure
		}()
		if !logged || sure != loggedSure {
			logged = true
			loggedSure = sure
			switch {
			case sure:
				glog.Infof("[clock]sure, offset %v (%s)\n", offset, outcome)
			case known:
				glog.Warningf("[clock]unsure, offset %v past %v (%s)\n", offset, self.settings.SureOffset, outcome)
			default:
				glog.Warningf("[clock]unsure, offset unknown (%s)\n", outcome)
			}
		}
		// the next packet is due an interval after this one, however long its reply took
		select {
		case <-self.ctx.Done():
			return
		case <-self.clock.After(self.settings.QueryInterval - self.clock.Now().Sub(issued)):
		}
	}
}

// One packet out, then what comes back until the next is due. With no usable reply nothing changes
// here: the timer the last reply set is what ends the offset, so a query that blocks cannot delay it.
// Returns what the query met, for the log.
func (self *ChronyClockCheck) query() string {
	dialCtx, dialCancel := context.WithTimeout(self.ctx, self.settings.QueryInterval)
	defer dialCancel()
	conn, err := self.dial(dialCtx)
	if err != nil {
		return "no socket: " + err.Error()
	}
	defer conn.Close()
	// a read blocked at shutdown ends with the check
	stopClose := context.AfterFunc(self.ctx, func() {
		conn.Close()
	})
	defer stopClose()

	request := make([]byte, ntpPacketByteCount)
	request[0] = ntpVersion<<3 | ntpModeClient
	// chronyd echoes this as the reply's origin timestamp and reads nothing else in it, so a random
	// value ties the reply to this query, and t1 stays here
	rand.Read(request[ntpTransmitIndex:])
	origin := binary.BigEndian.Uint64(request[ntpTransmitIndex:])

	transmitted := self.clock.Now()
	if err := conn.SetDeadline(transmitted.Add(self.settings.QueryInterval)); err != nil {
		return "no deadline: " + err.Error()
	}
	if _, err := conn.Write(request); err != nil {
		return "no send: " + err.Error()
	}
	packet := make([]byte, ntpPacketByteCount)
	for {
		byteCount, err := conn.Read(packet)
		received := self.clock.Now()
		if err != nil {
			return "no reply"
		}
		reply, ok := parseChronyReply(packet[:byteCount], origin)
		if !ok {
			continue
		}
		if !reply.synchronised {
			self.observe(0, false, received)
			return "an unsynchronised reply"
		}
		self.observe(reply.offset(transmitted, received), true, received)
		return "a reply"
	}
}

// Takes a reply's word. A synchronised reply stands for UnknownTimeout, and an unsynchronised one ends
// any offset at once. Called only from run's goroutine.
func (self *ChronyClockCheck) observe(offset time.Duration, known bool, received time.Time) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.knownUntil = received.Add(self.settings.UnknownTimeout)
		self.setWithLock(offset, known)
	}()
	if self.stopLapse != nil {
		self.stopLapse()
		self.stopLapse = nil
	}
	if known {
		self.stopLapse = self.clock.AfterFunc(self.settings.UnknownTimeout, func() {
			now := self.clock.Now()
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			// a timer a later reply was too late to stop finds knownUntil moved on, and does nothing
			self.lapseWithLock(now)
		})
	}
}

// What chronyd said in one reply to one query.
type chronyReply struct {
	synchronised   bool
	rootDelay      time.Duration
	rootDispersion time.Duration
	// when chronyd took the query and sent this reply, by its estimate of true time: RFC 5905 §8's
	// t2 and t3
	receiveTimestamp  uint64
	transmitTimestamp uint64
}

// Reads packet as the answer to the query that sent origin. false means it is not that answer: late,
// stray or not NTP at all, and it says nothing about this clock.
func parseChronyReply(packet []byte, origin uint64) (*chronyReply, bool) {
	if len(packet) < ntpPacketByteCount {
		return nil, false
	}
	leap := packet[0] >> 6
	version := packet[0] >> 3 & 7
	mode := packet[0] & 7
	if version != ntpVersion || mode != ntpModeServer {
		return nil, false
	}
	if binary.BigEndian.Uint64(packet[ntpOriginTimestampIndex:]) != origin {
		return nil, false
	}
	stratum := packet[1]
	receiveTimestamp := binary.BigEndian.Uint64(packet[ntpReceiveIndex:])
	transmitTimestamp := binary.BigEndian.Uint64(packet[ntpTransmitIndex:])
	return &chronyReply{
		// one that answers this query and cannot be measured is as bad as one saying it is lost
		synchronised: leap != ntpLeapUnsynchronised &&
			0 < stratum && stratum < ntpStratumUnsynchronised &&
			receiveTimestamp != 0 && transmitTimestamp != 0,
		rootDelay:         ntpShortDuration(binary.BigEndian.Uint32(packet[ntpRootDelayIndex:])),
		rootDispersion:    ntpShortDuration(binary.BigEndian.Uint32(packet[ntpRootDispersionIndex:])),
		receiveTimestamp:  receiveTimestamp,
		transmitTimestamp: transmitTimestamp,
	}, true
}

// The most this clock may be from true time, by a query sent at transmitted and answered at received
// on this clock: |θ| + (δ + root delay) / 2 + root dispersion, RFC 5905 §8's θ and δ. Rounds up.
func (self *chronyReply) offset(transmitted time.Time, received time.Time) time.Duration {
	t1 := ntpTimestamp(transmitted)
	t4 := ntpTimestamp(received)
	// θ doubled, so nothing is lost before the one division below
	doubledOffset := ntpDifference(self.receiveTimestamp, t1) + ntpDifference(self.transmitTimestamp, t4)
	// negative only if a clock moved during the exchange, which is error all the same
	roundTrip := ntpDifference(t4, t1) - ntpDifference(self.transmitTimestamp, self.receiveTimestamp)
	return (doubledOffset.Abs()+roundTrip.Abs()+self.rootDelay+1)/2 + self.rootDispersion
}

// t in NTP's timestamp format: 32 bits of seconds since 1900 and 32 of fraction. The seconds wrap at
// each era's end, the first in 2036, which ntpDifference allows for.
func ntpTimestamp(t time.Time) uint64 {
	seconds := uint64(t.Unix() + ntpUnixEpochOffset)
	fraction := (uint64(t.Nanosecond())<<32 + uint64(time.Second)/2) / uint64(time.Second)
	return seconds<<32 | fraction
}

// a - b. Exact across an era's end while the two are within 68 years (RFC 5905 §6).
func ntpDifference(a uint64, b uint64) time.Duration {
	difference := int64(a - b)
	seconds := difference >> 32
	fraction := uint64(difference) & math.MaxUint32
	return time.Duration(seconds)*time.Second +
		time.Duration((fraction*uint64(time.Second)+1<<31)>>32)
}

// NTP's short format: 16 bits of seconds and 16 of fraction. Rounds up, since this feeds a bound.
func ntpShortDuration(short uint32) time.Duration {
	return time.Duration((uint64(short)*uint64(time.Second) + math.MaxUint16) >> 16)
}
