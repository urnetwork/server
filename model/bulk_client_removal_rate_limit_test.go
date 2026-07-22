package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

// The quota is global, not per-network: two different networks draw from the
// same shared budget, and a request that would push the trailing-hour sum
// past MaxBulkClientRemovalsPerHour must be rejected outright (nothing
// recorded for a rejected request, matching CheckAndRecordAccountActionRateLimit's
// "only record real, admitted attempts" contract).
func TestCheckAndRecordBulkClientRemovalQuotaEnforcesGlobalCeiling(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkIdA := server.NewId()
		networkIdB := server.NewId()

		err := CheckAndRecordBulkClientRemovalQuota(ctx, networkIdA, MaxBulkClientRemovalsPerHour-1)
		assert.Equal(t, err, nil)

		// 1 more from a different network fits exactly at the ceiling
		err = CheckAndRecordBulkClientRemovalQuota(ctx, networkIdB, 1)
		assert.Equal(t, err, nil)

		// the budget is now fully spent; even a request for a single removal
		// must be rejected
		err = CheckAndRecordBulkClientRemovalQuota(ctx, networkIdA, 1)
		assert.NotEqual(t, err, nil)
	})
}

// A single request that alone exceeds the hourly ceiling must be rejected
// without partially recording anything.
func TestCheckAndRecordBulkClientRemovalQuotaRejectsOversizedSingleRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()

		err := CheckAndRecordBulkClientRemovalQuota(ctx, networkId, MaxBulkClientRemovalsPerHour+1)
		assert.NotEqual(t, err, nil)

		// nothing was recorded, so a request that fits the full ceiling must
		// still succeed afterward
		err = CheckAndRecordBulkClientRemovalQuota(ctx, networkId, MaxBulkClientRemovalsPerHour)
		assert.Equal(t, err, nil)
	})
}

// RemoveNetworkClients must actually surface the quota's rejection as an
// error to the caller, not just leave the quota mechanism itself correct in
// isolation. Pre-spending almost the whole global budget directly (cheap)
// and then issuing a small, real request that would tip it over avoids
// needing to construct a million-id request just to exercise this wiring.
func TestRemoveNetworkClientsSurfacesGlobalQuotaRejection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		otherNetworkId := server.NewId()
		networkId := server.NewId()

		err := CheckAndRecordBulkClientRemovalQuota(ctx, otherNetworkId, MaxBulkClientRemovalsPerHour-1)
		assert.Equal(t, err, nil)

		sess := &session.ClientSession{
			Ctx: ctx,
			ByJwt: &jwt.ByJwt{
				NetworkId: networkId,
			},
		}

		clientIdA := server.NewId()
		clientIdB := server.NewId()
		_, err = RemoveNetworkClients(&RemoveNetworkClientsArgs{
			ClientIds: []server.Id{clientIdA, clientIdB},
		}, sess)
		assert.NotEqual(t, err, nil)
	})
}
