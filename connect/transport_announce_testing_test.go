// Announcements without a handler's reconcile, for tests only. A
// ConnectHandler's reconcile retires the connected rows under its handler id
// that it does not hold, so an announcement built here under a live handler's
// id has its live row retired; tests use handler ids of their own.
package connect

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// NewConnectionAnnounce with the default test config and settings.
func NewConnectionAnnounceWithDefaults(
	ctx context.Context,
	cancel context.CancelFunc,
	networkId server.Id,
	clientId server.Id,
	clientAddress string,
	handlerId server.Id,
	announceTimeout time.Duration,
) *ConnectionAnnounce {
	return NewConnectionAnnounce(
		ctx,
		cancel,
		networkId,
		clientId,
		clientAddress,
		handlerId,
		announceTimeout,
		DefaultTestConfig(),
		DefaultConnectionAnnounceSettings(),
	)
}

// NewConnectionAnnounce announces a legacy, family-agnostic connection
// (intent 0). Family-pinned transports use NewConnectionAnnounceWithIpFamily.
func NewConnectionAnnounce(
	ctx context.Context,
	cancel context.CancelFunc,
	networkId server.Id,
	clientId server.Id,
	clientAddress string,
	handlerId server.Id,
	announceTimeout time.Duration,
	testConfig *TestConfig,
	settings *ConnectionAnnounceSettings,
) *ConnectionAnnounce {
	return NewConnectionAnnounceWithIpFamily(
		ctx,
		cancel,
		networkId,
		clientId,
		clientAddress,
		0,
		"",
		handlerId,
		announceTimeout,
		testConfig,
		settings,
	)
}

// NewConnectionAnnounceWithIpFamily is NewConnectionAnnounce with the address
// family the transport declared it intends to prove (0, 4 or 6) and the
// client's app version from its auth.
func NewConnectionAnnounceWithIpFamily(
	ctx context.Context,
	cancel context.CancelFunc,
	networkId server.Id,
	clientId server.Id,
	clientAddress string,
	ipFamilyIntent int,
	appVersion string,
	handlerId server.Id,
	announceTimeout time.Duration,
	testConfig *TestConfig,
	settings *ConnectionAnnounceSettings,
) *ConnectionAnnounce {
	return newConnectionAnnounceWithReconcile(
		ctx,
		cancel,
		networkId,
		clientId,
		clientAddress,
		ipFamilyIntent,
		appVersion,
		handlerId,
		nil,
		announceTimeout,
		testConfig,
		settings,
	)
}
