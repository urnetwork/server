package mcp

import (
	"context"
	"sync"
	"time"
)

const (
	fetchMaxConcurrentCalls            = 64
	fetchMaxConcurrentCallsPerIdentity = 2
)

type fetchIdentityConcurrency struct {
	semaphore chan struct{}
	users     int
}

type fetchConcurrencyLimiter struct {
	globalSemaphore chan struct{}
	stateLock       sync.Mutex
	identities      map[string]*fetchIdentityConcurrency
}

var defaultFetchConcurrencyLimiter = newFetchConcurrencyLimiter()

func newFetchConcurrencyLimiter() *fetchConcurrencyLimiter {
	return &fetchConcurrencyLimiter{
		globalSemaphore: make(chan struct{}, fetchMaxConcurrentCalls),
		identities:      map[string]*fetchIdentityConcurrency{},
	}
}

func (self *fetchConcurrencyLimiter) acquire(ctx context.Context, identity string) (func(), error) {
	identityConcurrency := self.retainIdentity(identity)

	identityWaitStart := time.Now()
	mcpFetchWaiters.WithLabelValues("per_identity").Inc()
	select {
	case identityConcurrency.semaphore <- struct{}{}:
	case <-ctx.Done():
		mcpFetchWaiters.WithLabelValues("per_identity").Dec()
		mcpFetchWaitSeconds.WithLabelValues("per_identity").Observe(time.Since(identityWaitStart).Seconds())
		mcpFetchAdmissionsTotal.WithLabelValues("per_identity", "canceled").Inc()
		self.releaseIdentity(identity, identityConcurrency)
		return nil, ctx.Err()
	}
	mcpFetchWaiters.WithLabelValues("per_identity").Dec()
	mcpFetchWaitSeconds.WithLabelValues("per_identity").Observe(time.Since(identityWaitStart).Seconds())
	mcpFetchAdmissionsTotal.WithLabelValues("per_identity", "admitted").Inc()
	mcpFetchConcurrency.WithLabelValues("per_identity_active").Inc()

	globalWaitStart := time.Now()
	mcpFetchWaiters.WithLabelValues("global").Inc()
	select {
	case self.globalSemaphore <- struct{}{}:
	case <-ctx.Done():
		mcpFetchWaiters.WithLabelValues("global").Dec()
		mcpFetchWaitSeconds.WithLabelValues("global").Observe(time.Since(globalWaitStart).Seconds())
		mcpFetchAdmissionsTotal.WithLabelValues("global", "canceled").Inc()
		<-identityConcurrency.semaphore
		mcpFetchConcurrency.WithLabelValues("per_identity_active").Dec()
		self.releaseIdentity(identity, identityConcurrency)
		return nil, ctx.Err()
	}
	mcpFetchWaiters.WithLabelValues("global").Dec()
	mcpFetchWaitSeconds.WithLabelValues("global").Observe(time.Since(globalWaitStart).Seconds())
	mcpFetchAdmissionsTotal.WithLabelValues("global", "admitted").Inc()
	mcpFetchConcurrency.WithLabelValues("global_active").Inc()

	var once sync.Once
	return func() {
		once.Do(func() {
			<-self.globalSemaphore
			mcpFetchConcurrency.WithLabelValues("global_active").Dec()
			<-identityConcurrency.semaphore
			mcpFetchConcurrency.WithLabelValues("per_identity_active").Dec()
			self.releaseIdentity(identity, identityConcurrency)
		})
	}, nil
}

func (self *fetchConcurrencyLimiter) retainIdentity(identity string) *fetchIdentityConcurrency {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	identityConcurrency := self.identities[identity]
	if identityConcurrency == nil {
		identityConcurrency = &fetchIdentityConcurrency{
			semaphore: make(chan struct{}, fetchMaxConcurrentCallsPerIdentity),
		}
		self.identities[identity] = identityConcurrency
		mcpFetchConcurrency.WithLabelValues("active_identities").Set(float64(len(self.identities)))
	}
	identityConcurrency.users += 1
	return identityConcurrency
}

func (self *fetchConcurrencyLimiter) releaseIdentity(identity string, identityConcurrency *fetchIdentityConcurrency) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	identityConcurrency.users -= 1
	if identityConcurrency.users == 0 && self.identities[identity] == identityConcurrency {
		delete(self.identities, identity)
		mcpFetchConcurrency.WithLabelValues("active_identities").Set(float64(len(self.identities)))
	}
}
