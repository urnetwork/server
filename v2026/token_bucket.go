package server

import (
	"sync"
	"time"
)

// A token bucket: up to Burst admissions at once, refilled at PerSecond.
// Admission never waits; a refused take spends nothing. A non-positive rate or
// burst admits none. Safe for concurrent use.
type TokenBucket struct {
	perSecond float64
	burst     float64

	stateLock sync.Mutex
	tokens    float64
	// the refill time of tokens, in unix nanos; zero before the first take
	refillNanos int64
}

// Starts full.
func NewTokenBucket(perSecond float64, burst int) *TokenBucket {
	return &TokenBucket{
		perSecond: perSecond,
		burst:     float64(burst),
		tokens:    float64(burst),
	}
}

// Admits one at now when a token is left, and spends it. A clock that steps
// back refills nothing.
func (self *TokenBucket) Take(now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.perSecond <= 0 || self.burst <= 0 {
		return false
	}
	nowNanos := now.UnixNano()
	if self.refillNanos != 0 && self.refillNanos < nowNanos {
		elapsed := float64(nowNanos-self.refillNanos) / float64(time.Second)
		self.tokens = min(self.burst, self.tokens+elapsed*self.perSecond)
	}
	if self.refillNanos < nowNanos {
		self.refillNanos = nowNanos
	}
	if self.tokens < 1 {
		return false
	}
	self.tokens -= 1
	return true
}
