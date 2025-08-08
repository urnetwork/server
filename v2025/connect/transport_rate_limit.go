package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/golang/glog"

	"github.com/urnetwork/server/v2025"
	// "github.com/urnetwork/server/v2025/session"
	// "github.com/urnetwork/server/v2025/model"
)

func DefaultConnectionRateLimitSettings() *ConnectionRateLimitSettings {
	return &ConnectionRateLimitSettings{
		BurstDuration:           60 * time.Second,
		BurstConnectionCount:    200,
		BurstConnectionDelay:    500 * time.Millisecond,
		MaxTotalConnectionCount: 200,
		TotalConnectionDelay:    30 * time.Second,
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
	TotalConnectionDelay    time.Duration
	TotalExpiration         time.Duration
}

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
	clientIp, _, err := server.SplitClientAddress(clientAddress)
	if err != nil {
		return nil, err
	}
	clientIpHash, err := server.ClientIpHash(clientIp)
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

// *important* disconnect must always be called, even if there is a rate limit error
func (self *ConnectionRateLimit) Connect() (err error, disconnect func()) {
	burstKey := fmt.Sprintf(
		"connect_burst_%s_%d",
		self.clientIpHashHex,
		server.NowUtc().Unix()/int64(self.settings.BurstDuration/time.Second),
	)
	totalKey := fmt.Sprintf(
		"connect_total_%s_%s",
		self.handlerId,
		self.clientIpHashHex,
	)

	totalIncremented := false
	disconnect = func() {
		// note use an uncanceled context for cleanup
		cleanupCtx := context.Background()

		if totalIncremented {
			var err error
			var totalCount int64
			server.Redis(cleanupCtx, func(r server.RedisClient) {
				totalCount, err = r.Decr(cleanupCtx, totalKey).Result()
			})
			if err != nil {
				glog.Errorf("[t][%s]total could not decrement err = %s\n", self.clientIpHashHex, err)
			} else {
				glog.V(1).Infof("[t][%s]total -1 @%d\n", self.clientIpHashHex, totalCount)
			}
		}
	}

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
			r.Expire(self.ctx, burstKey, self.settings.BurstDuration).Err()
		}
		totalCount, err = r.Incr(self.ctx, totalKey).Result()
		if err != nil {
			return
		}
		totalIncremented = true
		if totalCount == 1 {
			// FIXME put this in an atomic lua script
			// for now it doesnt matter if a few keys linger
			r.Expire(self.ctx, totalKey, self.settings.TotalExpiration).Err()
		}
	})
	if err != nil {
		return
	}

	glog.V(1).Infof("[t][%s]total +1 @%d\n", self.clientIpHashHex, totalCount)

	if int64(self.settings.MaxTotalConnectionCount) < totalCount {
		delay := self.settings.TotalConnectionDelay
		glog.Infof("[t][%s]total rate limit @%d (+%2.fs delay)\n", self.clientIpHashHex, totalCount, float64(delay/time.Millisecond)/1000.0)
		select {
		case <-self.ctx.Done():
			err = fmt.Errorf("Done.")
		case <-time.After(delay):
		}
		return
		err = fmt.Errorf("Total connection count exceeded.")
		return
	}

	if int64(self.settings.BurstConnectionCount) < burstCount {
		// delay connections above the burst limit
		delay := time.Duration(burstCount-int64(self.settings.BurstConnectionCount)) * self.settings.BurstConnectionDelay
		glog.Infof("[t][%s]burst rate limit @%d (+%.2fs delay)\n", self.clientIpHashHex, burstCount, float64(delay/time.Millisecond)/1000.0)
		select {
		case <-self.ctx.Done():
			err = fmt.Errorf("Done.")
		case <-time.After(delay):
		}
		return
	}

	return
}
