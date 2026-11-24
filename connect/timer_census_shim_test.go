// The census's wiring of the process's timer sweep, as newExchange and
// NewConnectHandler make it (timer_census_db_test.go).
package connect

import (
	"context"

	"github.com/urnetwork/server/internal/timersweep"
	"github.com/urnetwork/server/session"
)

func censusInstallExchangeSweep(exchange *Exchange) {
	exchange.timerSweep = timersweep.New(timersweep.DefaultTick)
}

func censusInstallAnnounceSweep(settings *ConnectionAnnounceSettings, exchange *Exchange) {
	settings.timerSweep = exchange.timerSweep
}

func censusStartLease(ctx context.Context, credential *session.ByJwt, exchange *Exchange, closeTransport func(int)) (*session.AuthorizationLease, error) {
	return session.Testing_StartConnectionAuthorizationLease(ctx, credential, exchange.timerSweep, func(context.Context, *session.ByJwt) error {
		return nil
	}, closeTransport)
}

func censusSweepArmedCount(exchange *Exchange) int {
	return exchange.timerSweep.ArmedCount()
}

func censusExpectsSweep() bool {
	return true
}
