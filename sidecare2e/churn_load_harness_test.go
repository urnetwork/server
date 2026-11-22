//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The churn load case's parts that reach past the hosts and api's routes (churn_load_test.go), gathered in one file as
// the e2e conventions ask, since QD-1028's layout move will move what they reach:
//
//   - the limits: pro.yml's sidecar limits for each tier the case's networks run on, past the world's own, through
//     model's Testing_SetSidecarLimits, until the case's end: every save of the case's ACL savers publishes a base,
//     which §7.9's config_bases_per_min of one a minute would hold a minute each;
//   - the world's settings past its construction: the dns and certificates case's stack (newDnsCertsStack) builds the
//     world itself on DefaultWorldSettings, which keeps each sidecar's gkm resync at the sidecar's own hour; the case
//     sets the T3World's KeyResyncInterval before its first enrollment, as WorldSettings.KeyResyncInterval would have,
//     so a hint a chaos lost costs a device at most that interval (§6.8);
//   - the exchange's connection limit from one address (server's ConnectionRateLimitSettings.MaxTotalConnectionCount,
//     200 in production): every client of the World dials from 127.0.0.1, and the case's sidecars, the edges' clients
//     and the bridges' window clients pass 200 from it, as the it bridge case found (ItBridgeSettings'
//     ExchangeConnectionMaxCount, which NewItBridgeWorld sets before its World starts); the case's stack builds the
//     World itself, so the case raises the handler's settings and restarts the handler before its first network;
//   - api's rows of a team's members and of a network's active clients, which the case holds against each member's and
//     device's churn (world_team_state_test.go's reads, wrapped so a panicking model call reads as an error).
//
// Safe for concurrent use.

package sidecare2e

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Puts each tier's limit overrides on the process's parsed pro.yml, past what the world installed, until the returned
// pop: free for the Community column, pro and team for theirs. The pops run newest first, so the case pops these
// before the world's own.
func churnLoadInstallLimits(tierLimits map[model.NetworkTier]map[string]int64) func() {
	pops := []func(){}
	for _, tier := range []model.NetworkTier{model.NetworkTierFree, model.NetworkTierPro, model.NetworkTierTeam} {
		limits, ok := tierLimits[tier]
		if !ok || len(limits) == 0 {
			continue
		}
		pops = append(pops, model.Testing_SetSidecarLimits(tier, maps.Clone(limits)))
	}
	return func() {
		for index := len(pops) - 1; 0 <= index; index -= 1 {
			pops[index]()
		}
	}
}

// Sets each sidecar's gkm hint resync on the world's real keys from now on (the file's header). Called before the
// world's first enrollment, while nothing reads the setting.
func churnLoadSetKeyResyncInterval(world *World, interval time.Duration) {
	world.T3().Settings().KeyResyncInterval = interval
}

// Raises the exchange handler's connection limit from one address to count and restarts the handler on it, every
// client's transport dialing again (the file's header). Called before the world's first network.
func churnLoadRaiseExchangeConnections(ctx context.Context, world *World, count int) error {
	world.T3().Settings().Exchange.Handler.ConnectionRateLimitSettings.MaxTotalConnectionCount = count
	if err := world.T3().Exchange().RestartHandler(ctx); err != nil {
		return fmt.Errorf("the exchange's handler on a limit of %d connections: %w", count, err)
	}
	return nil
}

// The team's members as api's rows hold them, each member_id and its role; a panicking model call reads as an error.
func churnLoadMemberRoles(ctx context.Context, networkId server.Id) (roles map[server.Id]string, returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("the member rows' read panicked: %v", recovered)
		}
	}()
	return worldNetworkMemberRoles(ctx, networkId)
}

// The network's active clients as api holds them, by client_id; a panicking model call reads as an error.
func churnLoadActiveClientIds(ctx context.Context, networkId server.Id) (clientIds map[server.Id]bool, returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("the client rows' read panicked: %v", recovered)
		}
	}()
	return worldNetworkActiveClientIds(ctx, networkId)
}
