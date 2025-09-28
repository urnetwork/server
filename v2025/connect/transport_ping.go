package main

import (
	"sync"
	"time"
)

// tracks receive ping times and computes the min of last n
// this allows the server to match the ping cadence of the client

type PingTracker struct {
	stateLock         sync.Mutex
	pingTimeouts      []time.Duration
	n                 int
	head              int
	latestReceiveTime time.Time
}

func NewPingTracker(n int) *PingTracker {
	if n < 1 {
		panic("[ping]n must be at least 1.")
	}
	return &PingTracker{
		pingTimeouts:      make([]time.Duration, 0, n),
		n:                 n,
		head:              0,
		latestReceiveTime: time.Now(),
	}
}

func (self *PingTracker) Receive() {
	self.latestReceiveTime = time.Now()
}

func (self *PingTracker) ReceivePing() time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	now := time.Now()
	pingTimeout := now.Sub(self.latestReceiveTime)
	self.latestReceiveTime = now
	if len(self.pingTimeouts) < self.n {
		self.pingTimeouts = append(self.pingTimeouts, pingTimeout)
		self.head = len(self.pingTimeouts) % self.n
	} else {
		self.pingTimeouts[self.head] = pingTimeout
		self.head = (self.head + 1) % self.n
	}
	return pingTimeout
}

func (self *PingTracker) MinPingTimeout() time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if len(self.pingTimeouts) == 0 {
		return 0
	}
	minPingTimeout := self.pingTimeouts[0]
	for _, pingTimeout := range self.pingTimeouts[1:] {
		minPingTimeout = min(minPingTimeout, pingTimeout)
	}
	return minPingTimeout
}
