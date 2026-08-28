package mcp

import (
	"context"
	"sync"
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
	mutex           sync.Mutex
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

	select {
	case identityConcurrency.semaphore <- struct{}{}:
	case <-ctx.Done():
		self.releaseIdentity(identity, identityConcurrency)
		return nil, ctx.Err()
	}

	select {
	case self.globalSemaphore <- struct{}{}:
	case <-ctx.Done():
		<-identityConcurrency.semaphore
		self.releaseIdentity(identity, identityConcurrency)
		return nil, ctx.Err()
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			<-self.globalSemaphore
			<-identityConcurrency.semaphore
			self.releaseIdentity(identity, identityConcurrency)
		})
	}, nil
}

func (self *fetchConcurrencyLimiter) retainIdentity(identity string) *fetchIdentityConcurrency {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	identityConcurrency := self.identities[identity]
	if identityConcurrency == nil {
		identityConcurrency = &fetchIdentityConcurrency{
			semaphore: make(chan struct{}, fetchMaxConcurrentCallsPerIdentity),
		}
		self.identities[identity] = identityConcurrency
	}
	identityConcurrency.users += 1
	return identityConcurrency
}

func (self *fetchConcurrencyLimiter) releaseIdentity(identity string, identityConcurrency *fetchIdentityConcurrency) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	identityConcurrency.users -= 1
	if identityConcurrency.users == 0 && self.identities[identity] == identityConcurrency {
		delete(self.identities, identity)
	}
}
