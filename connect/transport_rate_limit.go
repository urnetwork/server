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

func burstKey(clientIpHashHex string, timeBlock int64) string {
	return fmt.Sprintf("connect_burst_%s_%d", clientIpHashHex, timeBlock)
}

func totalKey(handlerId server.Id, clientIpHashHex string) string {
	return fmt.Sprintf("connect_total_%s_%s", handlerId, clientIpHashHex)
}

func DefaultConnectionRateLimitSettings() *ConnectionRateLimitSettings {
	return &ConnectionRateLimitSettings{
		BurstDuration:           60 * time.Second,
		BurstConnectionCount:    60,
		BurstConnectionDelay:    500 * time.Millisecond,
		MaxTotalConnectionCount: 40,
		TotalExpiration:         7 * 24 * time.Hour,
	}
}

type ConnectionRateLimitSettings struct {
	BurstDuration        time.Duration
	BurstConnectionCount int

	// linear scale by count in excess of burst
	BurstConnectionDelay time.Duration

	// max total connections per ip
	MaxTotalConnectionCount int
	TotalExpiration         time.Duration
}

// type ConnectionHandlerRateLimit struct {
// 	ctx context.Context
// 	cancel context.CancelFunc
// 	handlerId server.Id
// 	settings *ConnectionRateLimitSettings
// }

// func NewConnectionHandlerRateLimitWithDefaults(
// 	ctx context.Context,
// 	handlerId server.Id,
// ) *ConnectionHandlerRateLimit {
// 	return NewConnectionHandlerRateLimit(ctx, handlerId, DefaultConnectionRateLimitSettings())
// }

// func NewConnectionHandlerRateLimit(
// 	ctx context.Context,
// 	handlerId server.Id,
// 	settings *ConnectionRateLimitSettings,
// ) *ConnectionHandlerRateLimit {
// 	cancelCtx, cancel := context.WithCancel(ctx)
// 	return &ConnectionHandlerRateLimit{
// 		ctx: cancelCtx,
// 		cancel: cancel,
// 		handlerId: handlerId,
// 		settings: settings,
// 	}
// }

// func (self *ConnectionHandlerRateLimit) run() {
// 	// keep extending the handler rate limit total expiration

// 	totalKey := totalKey(self.handlerId, self.clientIpHashHex)

// 	for {
// 		select {
// 		case <- self.ctx.Done():
// 			return
// 		case <- time.After(self.settings.TotalExpiration / 4):
// 		}

// 		server.Redis(ctx, func(r server.RedisClient) {
// 			r.Expire(ctx, totalKey, self.settings.TotalExpiration).Err()
// 		})
// 	}
// }

// func (self *ConnectionHandlerRateLimit) Close() {
// 	self.cancel()
// }

type ConnectionRateLimit struct {
	ctx             context.Context
	clientIpHashHex string
	handlerId       server.Id
	settings        *ConnectionRateLimitSettings
}

func NewConnectionRateLimitWithDefaults(
	ctx context.Context,
	clientAddress string,
	handlerId server.Id,
) (*ConnectionRateLimit, error) {
	return NewConnectionRateLimit(ctx, clientAddress, handlerId, DefaultConnectionRateLimitSettings())
}

func NewConnectionRateLimit(
	ctx context.Context,
	clientAddress string,
	handlerId server.Id,
	settings *ConnectionRateLimitSettings,
) (*ConnectionRateLimit, error) {
	clientIp, _, err := model.SplitClientAddress(clientAddress)
	if err != nil {
		return nil, err
	}
	clientIpHash, err := model.ClientIpHash(clientIp)
	if err != nil {
		return nil, err
	}
	clientIpHashHex := hex.EncodeToString(clientIpHash)

	return &ConnectionRateLimit{
		ctx:             ctx,
		clientIpHashHex: clientIpHashHex,
		handlerId:       handlerId,
		settings:        settings,
	}, nil
}

func (self *ConnectionRateLimit) Connect() error {
	now := server.NowUtc()

	burstKey := burstKey(
		self.clientIpHashHex,
		now.Unix()/int64(self.settings.BurstDuration/time.Second),
	)
	totalKey := totalKey(self.handlerId, self.clientIpHashHex)

	var err error
	var burstCount int64
	var totalCount int64
	server.Redis(self.ctx, func(r server.RedisClient) {
		burstCount, err = r.Incr(self.ctx, burstKey).Result()
		if err != nil {
			return
		}
		if burstCount == 1 {
			// FIXME put this in an atomic lua script
			// for now it doesnt matter if a few keys linger
			err = r.Expire(self.ctx, burstKey, self.settings.BurstDuration).Err()
		}
		totalCount, err = r.Incr(self.ctx, totalKey).Result()
		if err != nil {
			return
		}
		if totalCount == 1 {
			// FIXME put this in an atomic lua script
			// for now it doesnt matter if a few keys linger
			err = r.Expire(self.ctx, totalKey, self.settings.TotalExpiration).Err()
		}
	})
	if err != nil {
		return err
	}

	if int64(self.settings.MaxTotalConnectionCount) < totalCount {
		return fmt.Errorf("Total connection count exceeded.")
	}

	if int64(self.settings.BurstConnectionCount) < burstCount {
		// delay connections above the burst limit
		delay := time.Duration(burstCount-int64(self.settings.BurstConnectionCount)) * self.settings.BurstConnectionDelay
		glog.Infof("[t][%s]rate limit @%d (+%dms delay)\n", self.clientIpHashHex, burstCount, delay/time.Millisecond)
		select {
		case <-self.ctx.Done():
			return fmt.Errorf("Done.")
		case <-time.After(delay):
			return nil
		}
	}

	return nil
}

func (self *ConnectionRateLimit) Disconnect() {
	totalKey := totalKey(self.handlerId, self.clientIpHashHex)

	server.Redis(self.ctx, func(r server.RedisClient) {
		r.Decr(self.ctx, totalKey).Err()
	})
}
