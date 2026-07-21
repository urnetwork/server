package main

// network impairment for a provider's platform connection.
//
// A provider dials its websocket to the exchange through impairConn, which
// applies bandwidth (token bucket), one-way latency + jitter, and a loss
// model (an occasional retransmit-sized stall) to the bytes it carries. The
// connect service measures latency (ws ping RTT) and speed (timed transfer)
// over this same connection, so the server-side scores reflect the impairment
// with no fake reporting, and the rate limiter's backpressure produces
// realistic queuing under load.
//
// Only the provider side is impaired (clients are unimpaired in v1), and only
// the write direction carries the latency delay — a ping's pong is delayed by
// ~one-way latency, so the measured RTT tracks it. Parameters are read from
// an atomic pointer the fleet dynamics loop swaps, so conditions can change
// over the run (the good/degraded regime modulation).
//
// The model is deliberately cheap (inline, no per-conn goroutine) so a fleet
// of 100k connections is affordable; it trades emulation fidelity for scale,
// which is the right call for comparing algorithms rather than reproducing a
// specific network.

import (
	"math"
	mathrand "math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type impairParams struct {
	latency   time.Duration
	jitter    time.Duration
	bytesPerS int64
	loss      float64
}

// tokenBucket is a simple byte-rate limiter. Not safe for concurrent use.
type tokenBucket struct {
	tokens   float64
	lastFill time.Time
}

func (self *tokenBucket) take(n int, bytesPerS int64) time.Duration {
	if bytesPerS <= 0 {
		return 0
	}
	now := time.Now()
	if self.lastFill.IsZero() {
		self.lastFill = now
		self.tokens = float64(bytesPerS) // start with ~1s burst
	}
	elapsed := now.Sub(self.lastFill).Seconds()
	self.lastFill = now
	self.tokens += elapsed * float64(bytesPerS)
	// cap the burst at 1s worth
	if maxTokens := float64(bytesPerS); maxTokens < self.tokens {
		self.tokens = maxTokens
	}
	self.tokens -= float64(n)
	if 0 <= self.tokens {
		return 0
	}
	// wait for the deficit to refill
	deficit := -self.tokens
	return time.Duration(deficit / float64(bytesPerS) * float64(time.Second))
}

type impairConn struct {
	net.Conn
	params *atomic.Pointer[impairParams]

	writeLock     sync.Mutex
	writeBucket   tokenBucket
	writeRand     *mathrand.Rand
	lastWriteTime time.Time

	readLock   sync.Mutex
	readBucket tokenBucket
}

func newImpairConn(conn net.Conn, params *atomic.Pointer[impairParams], seed int64) *impairConn {
	return &impairConn{
		Conn:      conn,
		params:    params,
		writeRand: mathrand.New(mathrand.NewSource(seed)),
	}
}

func (self *impairConn) Write(p []byte) (int, error) {
	params := self.params.Load()

	var delay time.Duration
	func() {
		self.writeLock.Lock()
		defer self.writeLock.Unlock()

		// bandwidth: rate-limit every write
		delay += self.writeBucket.take(len(p), params.bytesPerS)

		// latency: charge one-way latency + jitter only on a NEW burst — the
		// first write after an idle gap of at least the latency. Consecutive
		// writes within a burst (a bulk transfer streamed as many small frames)
		// pipeline behind that one latency, instead of each frame paying it,
		// which would collapse throughput to latency x frame-count.
		now := time.Now()
		if self.lastWriteTime.IsZero() || params.latency <= now.Sub(self.lastWriteTime) {
			delay += params.latency
			if 0 < params.jitter {
				delay += time.Duration(self.writeRand.Int63n(int64(params.jitter)))
			}
			// loss: an occasional retransmit-sized stall, only at burst start
			if 0 < params.loss && self.writeRand.Float64() < params.loss {
				stall := params.latency * 3
				if stall <= 0 {
					stall = 50 * time.Millisecond
				}
				delay += stall
			}
		}
		self.lastWriteTime = now
	}()

	if 0 < delay {
		self.sleep(delay)
	}
	return self.Conn.Write(p)
}

func (self *impairConn) Read(p []byte) (int, error) {
	n, err := self.Conn.Read(p)
	if 0 < n {
		params := self.params.Load()
		var delay time.Duration
		func() {
			self.readLock.Lock()
			defer self.readLock.Unlock()
			delay = self.readBucket.take(n, params.bytesPerS)
		}()
		if 0 < delay {
			self.sleep(delay)
		}
	}
	return n, err
}

func (self *impairConn) sleep(d time.Duration) {
	// cap a single sleep so a pathological param can't wedge the conn
	if maxSleep := 5 * time.Second; maxSleep < d {
		d = maxSleep
	}
	time.Sleep(d)
}

// baseParams and degradedParams derive the two regimes for a provider entry.
func baseParams(entry ProviderEntry) *impairParams {
	return &impairParams{
		latency:   time.Duration(entry.LatencyMillis * float64(time.Millisecond)),
		jitter:    time.Duration(entry.JitterMillis * float64(time.Millisecond)),
		bytesPerS: entry.BandwidthBps,
		loss:      entry.Loss,
	}
}

func degradedParams(entry ProviderEntry) *impairParams {
	loss := entry.Loss + entry.DegradedLossAdd
	if 1 < loss {
		loss = 1
	}
	return &impairParams{
		latency:   time.Duration(entry.LatencyMillis * entry.DegradedLatencyScale * float64(time.Millisecond)),
		jitter:    time.Duration(entry.JitterMillis * entry.DegradedLatencyScale * float64(time.Millisecond)),
		bytesPerS: int64(math.Max(1, float64(entry.BandwidthBps)*entry.DegradedBandwidthScale)),
		loss:      loss,
	}
}
