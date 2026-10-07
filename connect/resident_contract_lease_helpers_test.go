// Adapts existing lifecycle controls and seeds complete synthetic Redis leases.
package connect

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Existing boolean controls keep their behavior under a distant source lease.
// Tests of actual expiry use explicit deadlines instead of this adapter.
func residentContractReadForTest(read func(context.Context, server.Id, server.Id) bool) func(context.Context, server.Id, server.Id) residentContractAllowance {
	return func(ctx context.Context, source, destination server.Id) residentContractAllowance {
		if !read(ctx, source, destination) {
			return residentContractAllowance{}
		}
		return residentContractAllowance{active: true, validUntil: time.Now().Add(time.Hour)}
	}
}

// Preserves source errors, cancellation barriers and positive/negative results.
func residentContractSourceForTest(read func(context.Context, server.Id, server.Id) (bool, error)) func(context.Context, server.Id, server.Id) (model.ContractHoleStatus, time.Time, error) {
	return func(ctx context.Context, source, destination server.Id) (model.ContractHoleStatus, time.Time, error) {
		active, err := read(ctx, source, destination)
		if !active {
			return model.ContractHoleNegative, time.Time{}, err
		}
		return model.ContractHolePositive, time.Now().Add(time.Hour), err
	}
}

// Positive counts require matching expiring membership. Infinite member scores
// represent legacy contracts; malformed scalar fixtures remain malformed.
func residentPacketSeedHole(ctx context.Context, client server.RedisClient, source, destination server.Id, value string) {
	countKey := residentPacketHoleKey(source, destination)
	memberKey := strings.TrimSuffix(countKey, ":count") + ":members"
	server.Raise(client.Del(ctx, memberKey).Err())
	server.Raise(client.Set(ctx, countKey, value, model.ContractHoleTtl).Err())
	count, err := strconv.Atoi(value)
	if err != nil || count <= 0 || count > 8192 || strconv.Itoa(count) != value {
		return
	}
	for range count {
		server.Raise(client.Do(ctx, "ZADD", memberKey, "+inf", server.NewId().String()).Err())
	}
	server.Raise(client.PExpire(ctx, memberKey, model.ContractHoleTtl).Err())
}
