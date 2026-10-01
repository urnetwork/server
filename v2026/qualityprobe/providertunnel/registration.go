// Only failed local credential acquisition or control registration before this
// private tunnel has obtained a registered client can rule out measurement.
package providertunnel

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/urnetwork/connect/v2026"
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

// Failed args creation cannot contact a provider because no client exists yet.
// A later credential alone is not contact; successful construction ends proof.
func (self *providerRegistrationState) recordClientArgs(args *connect.MultiClientGeneratorClientArgs, err error) {
	if args == nil && err != nil {
		self.state.Or(providerRegistrationFailed)
	}
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
// and budget capabilities remain unchanged. Credential and construction results
// are observed before the window can use a returned client.
type providerRegistrationGenerator struct {
	*connect.ApiMultiClientGenerator
	registration *providerRegistrationState
	setup        *providerSetupState
}

// Observe the legacy entrypoint without changing its parent-context ownership.
func (self *providerRegistrationGenerator) NewClientArgs() (*connect.MultiClientGeneratorClientArgs, error) {
	setupCall := self.setup.beginCredential()
	args, err := self.ApiMultiClientGenerator.NewClientArgs()
	self.registration.recordClientArgs(args, err)
	self.setup.finishCredential(setupCall, args, err, nil)
	return args, err
}

// Preserve the caller's bounded credential acquisition and its exact error.
func (self *providerRegistrationGenerator) NewClientArgsContext(ctx context.Context) (*connect.MultiClientGeneratorClientArgs, error) {
	setupCall := self.setup.beginCredential()
	args, err := self.ApiMultiClientGenerator.NewClientArgsContext(ctx)
	self.registration.recordClientArgs(args, err)
	self.setup.finishCredential(setupCall, args, err, nil)
	return args, err
}

// Destination-aware windows keep the real generator's identity/reuse behavior.
func (self *providerRegistrationGenerator) NewClientArgsForDestination(destination connect.MultiHopId) (*connect.MultiClientGeneratorClientArgs, error) {
	setupCall := self.setup.beginCredential()
	args, err := self.ApiMultiClientGenerator.NewClientArgsForDestination(destination)
	self.registration.recordClientArgs(args, err)
	self.setup.finishCredential(setupCall, args, err, &destination)
	return args, err
}

// Observe the context-aware destination path before any constructor can run.
func (self *providerRegistrationGenerator) NewClientArgsForDestinationContext(ctx context.Context, destination connect.MultiHopId) (*connect.MultiClientGeneratorClientArgs, error) {
	setupCall := self.setup.beginCredential()
	args, err := self.ApiMultiClientGenerator.NewClientArgsForDestinationContext(ctx, destination)
	self.registration.recordClientArgs(args, err)
	self.setup.finishCredential(setupCall, args, err, &destination)
	return args, err
}

// Retain the legacy caller's single-context lifecycle contract.
func (self *providerRegistrationGenerator) NewClient(ctx context.Context, args *connect.MultiClientGeneratorClientArgs, settings *connect.ClientSettings) (*connect.Client, error) {
	return self.NewClientContext(ctx, ctx, args, settings)
}

// Observe, but do not alter, the real constructor's cause or cleanup ownership.
func (self *providerRegistrationGenerator) NewClientContext(ctx context.Context, callCtx context.Context, args *connect.MultiClientGeneratorClientArgs, settings *connect.ClientSettings) (*connect.Client, error) {
	setupCall := self.setup.beginConstructor(args)
	client, err := self.ApiMultiClientGenerator.NewClientContext(ctx, callCtx, args, settings)
	self.registration.record(client, err)
	self.setup.finishConstructor(setupCall, client, err)
	return client, err
}
