// Only a failed local control registration before this private tunnel has
// ever obtained a registered client can rule out provider measurement.
package providertunnel

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/urnetwork/connect"
)

const (
	providerRegistrationFailed uint32 = 1 << iota
	providerRegistrationSucceeded
)

// Monotonic, constant-size state owned by one TUN, safe for concurrent request
// and setup callbacks. One successful constructor permanently disables the
// new unknown shortcut; existing lost-path handling then owns disconnections.
type providerRegistrationState struct {
	state atomic.Uint32
}

// Consume one completed constructor before its client can enter the window.
func (self *providerRegistrationState) record(client *connect.Client, err error) {
	if client != nil {
		self.state.Or(providerRegistrationSucceeded)
		return
	}
	var localFailure interface{ LocalControlRegistrationFailure() bool }
	if errors.As(err, &localFailure) && localFailure.LocalControlRegistrationFailure() {
		self.state.Or(providerRegistrationFailed)
	}
}

// A successful return permanently outranks every earlier or later failure.
func (self *providerRegistrationState) unavailable() bool {
	return self != nil && self.state.Load() == providerRegistrationFailed
}

// Embed the real API generator so its optional migration, identity, cleanup
// and budget capabilities remain unchanged. Only construction results are
// observed, before the window can use the returned client.
type providerRegistrationGenerator struct {
	*connect.ApiMultiClientGenerator
	registration *providerRegistrationState
}

// Retain the legacy caller's single-context lifecycle contract.
func (self *providerRegistrationGenerator) NewClient(ctx context.Context, args *connect.MultiClientGeneratorClientArgs, settings *connect.ClientSettings) (*connect.Client, error) {
	return self.NewClientContext(ctx, ctx, args, settings)
}

// Observe, but do not alter, the real constructor's cause or cleanup ownership.
func (self *providerRegistrationGenerator) NewClientContext(ctx context.Context, callCtx context.Context, args *connect.MultiClientGeneratorClientArgs, settings *connect.ClientSettings) (*connect.Client, error) {
	client, err := self.ApiMultiClientGenerator.NewClientContext(ctx, callCtx, args, settings)
	self.registration.record(client, err)
	return client, err
}
