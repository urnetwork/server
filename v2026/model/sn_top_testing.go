package model

import (
	"context"

	"github.com/urnetwork/server/v2026"
)

// Testing_InsertSnTrailHops writes trail hop rows as a complete trail does.
func Testing_InsertSnTrailHops(ctx context.Context, hops []*SnTrailHop) {
	server.Tx(ctx, func(tx server.PgTx) {
		insertSnTrailHopsInTx(ctx, tx, hops)
	})
}

// Testing_AddVerifyCohortMembers admits providers to a settlement epoch's
// verify cohort.
func Testing_AddVerifyCohortMembers(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64, clientIds []server.Id) {
	keys := newVerifyCohortKeys(deploymentKey, epoch)
	server.Redis(ctx, func(r server.RedisClient) {
		for _, clientId := range clientIds {
			server.Raise(r.SAdd(ctx, keys.epoch, clientId.String()).Err())
			server.Raise(r.SAdd(ctx, keys.lifetime, clientId.String()).Err())
		}
	})
}
