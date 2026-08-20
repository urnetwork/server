package connect

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	// "github.com/urnetwork/server/v2026/session"
	// "github.com/urnetwork/server/v2026/model"
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
	excluded        bool
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
	clientAddr, err := netip.ParseAddr(clientIp)
	if err != nil {
		return nil, err
	}
	clientIpHash := server.ClientIpHashForAddr(clientAddr)
	clientIpHashHex := hex.EncodeToString(clientIpHash[:])

	excluded := server.IsLimitExcludeAddr(clientAddr)

	return &ConnectionRateLimit{
		ctx:             ctx,
		clientIpHashHex: clientIpHashHex,
		excluded:        excluded,
		handlerId:       handlerId,
		settings:        settings,
	}, nil
}

// *important* disconnect must always be called, even if there is a rate limit error
func (self *ConnectionRateLimit) Connect() (err error, disconnect func()) {
	// the burst and total counters for one client ip share the per-ip hash
	// tag so they stay in a single tx pipeline on cluster, while counters for
	// different ips spread across slots. A previous format put every counter
	// under one shared `{connect}` tag (a single slot/node hot spot);
	// old-format keys carried ttls, so they expired on their own.
	burstKey := fmt.Sprintf(
		"{connect_%s}burst_%d",
		self.clientIpHashHex,
		server.NowUtc().Unix()/int64(self.settings.BurstDuration/time.Second),
	)
	totalKey := fmt.Sprintf(
		"{connect_%s}total_%s",
		self.clientIpHashHex,
		self.handlerId,
	)

	totalIncremented := false
	disconnect = func() {
		// note use an uncanceled context for cleanup
		cleanupCtx := context.Background()

		if totalIncremented {
			var err error
			var totalCount int64
			server.Redis(cleanupCtx, func(r server.RedisClient) {
				var totalCmd *redis.IntCmd
				r.Pipelined(cleanupCtx, func(pipe redis.Pipeliner) error {
					totalCmd = pipe.Decr(cleanupCtx, totalKey)
					// a decr that recreates an expired key must not leave it
					// without a ttl; nx never extends the connect-time expiry
					pipe.ExpireNX(cleanupCtx, totalKey, self.settings.TotalExpiration)
					return nil
				})
				totalCount, err = totalCmd.Result()
			})
			if err != nil {
				glog.Errorf("[t][%s]total could not decrement err = %s\n", self.clientIpHashHex, err)
			} else {
				if glog.V(1) {
					glog.Infof("[t][%s]total -1 @%d\n", self.clientIpHashHex, totalCount)
				}
			}
		}
	}

	var burstCount int64
	var totalCount int64
	server.Redis(self.ctx, func(r server.RedisClient) {
		var burstCmd *redis.IntCmd
		var totalCmd *redis.IntCmd
		r.TxPipelined(self.ctx, func(pipe redis.Pipeliner) error {
			burstCmd = pipe.Incr(self.ctx, burstKey)
			pipe.Expire(self.ctx, burstKey, self.settings.BurstDuration)

			totalCmd = pipe.Incr(self.ctx, totalKey)
			pipe.Expire(self.ctx, totalKey, self.settings.TotalExpiration)

			return nil
		})
		burstCount, err = burstCmd.Result()
		if err != nil {
			return
		}
		totalCount, err = totalCmd.Result()
		if err != nil {
			return
		}
		totalIncremented = true
	})
	if err != nil {
		return
	}

	if self.excluded {
		return
	}

	if glog.V(1) {
		glog.Infof("[t][%s]total +1 @%d\n", self.clientIpHashHex, totalCount)
	}

	if int64(self.settings.MaxTotalConnectionCount) < totalCount {
		delay := self.settings.TotalConnectionDelay
		if glog.V(1) {
			glog.Infof("[t][%s]total rate limit @%d (+%2.fs delay)\n", self.clientIpHashHex, totalCount, float64(delay/time.Millisecond)/1000.0)
		}
		select {
		case <-self.ctx.Done():
			err = fmt.Errorf("Done.")
			return
		case <-time.After(delay):
		}
		err = fmt.Errorf("Total connection count exceeded.")
		return
	}

	if int64(self.settings.BurstConnectionCount) < burstCount {
		// delay connections above the burst limit
		delay := time.Duration(burstCount-int64(self.settings.BurstConnectionCount)) * self.settings.BurstConnectionDelay
		if glog.V(1) {
			glog.Infof("[t][%s]burst rate limit @%d (+%.2fs delay)\n", self.clientIpHashHex, burstCount, float64(delay/time.Millisecond)/1000.0)
		}
		select {
		case <-self.ctx.Done():
			err = fmt.Errorf("Done.")
			return
		case <-time.After(delay):
		}
		err = fmt.Errorf("Burst connection count exceeded.")
		return
	}

	return
}
