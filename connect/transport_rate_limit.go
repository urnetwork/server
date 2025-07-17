package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/golang/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const RateLimitBurstDuration = 60 * time.Second
const RateLimitBurstConnectionCount = 60

// linear scale by count in excess of burst
const RateLimitConnectionDelay = 500 * time.Millisecond

func ConnectionRateLimit(ctx context.Context, clientAddress string) error {
	clientIp, _, err := model.SplitClientAddress(clientAddress)
	if err != nil {
		return err
	}
	clientIpHash, err := model.ClientIpHash(clientIp)
	if err != nil {
		return err
	}
	clientIpHashHex := hex.EncodeToString(clientIpHash)

	now := server.NowUtc()

	timeBlock := now.Unix() / int64(RateLimitBurstDuration/time.Second)

	key := fmt.Sprintf("connect_rl_%s_%d", clientIpHashHex, timeBlock)

	var count int64
	server.Redis(ctx, func(r server.RedisClient) {
		count, err = r.Incr(ctx, key).Result()
		if err != nil {
			return
		}
		if count == 1 {
			// FIXME put this in an atomic lua script
			// for now it doesnt matter if a few keys linger
			err = r.Expire(ctx, key, RateLimitBurstDuration).Err()
		}
	})
	if err != nil {
		return err
	}

	if count <= RateLimitBurstConnectionCount {
		return nil
	}

	delay := time.Duration(count-RateLimitBurstConnectionCount) * RateLimitConnectionDelay
	glog.Infof("[t][%s]rate limit @%d\n", clientIpHashHex, count)
	select {
	case <-ctx.Done():
		return fmt.Errorf("Done.")
	case <-time.After(delay):
		return nil
	}
}
