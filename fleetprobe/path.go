// This file is the provider tunnel as an egresshealth.Path: the tunnel a probe
// rides, and a new one to the same provider when it dies.
package fleetprobe

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/operator-proxy/providertunnel"
)

// The part of a providertunnel.Tunnel a probe uses. It is an
// interface so re-creation can be tested without a provider.
type probeTunnel interface {
	HttpClientForHosts(timeout time.Duration, hosts []string) *http.Client
	Lost() context.Context
	Close() error
}

// Opens one tunnel to the probe's provider.
type tunnelOpener func(ctx context.Context) (probeTunnel, error)

// Opens tunnels to providerClientId with config, whose
// pins are first cut down to the hosts the probe dials. Every call is a new
// proxy device and a new contract, which is why a run re-creates a tunnel at
// most egresshealth.Options.TunnelRecreateAttempts times.
func providerTunnelOpener(config providertunnel.Config, providerClientId connect.Id, hosts []string) tunnelOpener {
	config.Pins = restrictPins(config.Pins, hosts)
	return func(ctx context.Context) (probeTunnel, error) {
		tunnel, err := providertunnel.Open(ctx, config, providerClientId)
		if err != nil {
			// never a typed nil in the interface
			return nil, err
		}
		return tunnel, nil
	}
}

// What a re-open gets once the probe has closed its path.
var errPathClosed = errors.New("fleetprobe: the probe is over; its tunnel is not re-created")

// One probe's egresshealth.Path: the current tunnel to one
// provider, its client, and the means to open another when it dies part-way
// through a run or a check (GEOMAP §11.3, "A retry re-creates the tunnel").
//
// A replaced tunnel is closed in the background -- its teardown can take a
// while, and the loads waiting on the new one should not -- and Close waits for
// every one of them, so a probe never leaves a tunnel behind.
//
// Safe for concurrent use. open, hosts and timeout are fixed at construction;
// the rest is guarded by stateLock, which is never held across a call into a
// tunnel.
type probePath struct {
	open    tunnelOpener
	hosts   []string
	timeout time.Duration

	stateLock sync.Mutex
	tunnel    probeTunnel
	client    *http.Client
	closed    bool
	reopened  int
	retiring  sync.WaitGroup
	errs      []error
}

// Opens the probe's first tunnel.
func openProbePath(ctx context.Context, open tunnelOpener, hosts []string, timeout time.Duration) (*probePath, error) {
	tunnel, err := open(ctx)
	if err != nil {
		return nil, err
	}
	return &probePath{
		open:    open,
		hosts:   hosts,
		timeout: timeout,
		tunnel:  tunnel,
		client:  tunnel.HttpClientForHosts(timeout, hosts),
	}, nil
}

// Implements egresshealth.Path: the current tunnel's client and loss signal.
// The pair is read under the lock, and the tunnel, an external object, is
// asked for its signal after it.
func (self *probePath) Current() (*http.Client, context.Context) {
	var tunnel probeTunnel
	var client *http.Client
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		tunnel, client = self.tunnel, self.client
	}()
	return client, tunnel.Lost()
}

// Opens a new tunnel to the same provider, with the same config, and
// makes it current. The dead one is retired in the background.
func (self *probePath) Reopen(ctx context.Context) error {
	closed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		closed = self.closed
	}()
	if closed {
		return errPathClosed
	}

	tunnel, err := self.open(ctx)
	if err != nil {
		return err
	}
	// Built before the lock is taken: the tunnel is an external object.
	client := tunnel.HttpClientForHosts(self.timeout, self.hosts)

	var retired probeTunnel
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			closed = true
			return
		}
		retired = self.tunnel
		self.tunnel = tunnel
		self.client = client
		self.reopened++
		self.retiring.Add(1)
	}()
	if closed {
		return errors.Join(errPathClosed, tunnel.Close())
	}

	go func() {
		defer self.retiring.Done()
		if err := retired.Close(); err != nil {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.errs = append(self.errs, err)
			}()
		}
	}()
	return nil
}

// Reports how many times the path re-created its tunnel.
func (self *probePath) reopens() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.reopened
}

// Closes the current tunnel and waits for every retired one. It is safe
// to call more than once; later calls return nil.
func (self *probePath) Close() error {
	var current probeTunnel
	alreadyClosed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			alreadyClosed = true
			return
		}
		self.closed = true
		current = self.tunnel
	}()
	if alreadyClosed {
		return nil
	}

	err := current.Close()
	self.retiring.Wait()
	var retireErrs []error
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		retireErrs = self.errs
	}()
	return errors.Join(append([]error{err}, retireErrs...)...)
}
