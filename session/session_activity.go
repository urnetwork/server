// Last use is optional presentation data; it never grants or extends authority.
package session

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
)

const sessionUseInterval = 60 * time.Second
const sessionUseBudget = 100 * time.Millisecond
const sessionUseCacheLimit = 8192

var sessionUseDrops = prometheus.NewCounter(prometheus.CounterOpts{Name: "urnetwork_session_observation_drops_total", Help: "Optional session observations dropped or unavailable"})

func init() { prometheus.MustRegister(sessionUseDrops) }

type sessionUseThrottle struct {
	stateLock sync.Mutex
	attempts  map[[2]server.Id]int64
}

var sessionUseAttempts = sessionUseThrottle{attempts: map[[2]server.Id]int64{}}

func (self *sessionUseThrottle) admit(networkId, sid server.Id, now int64) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	key := [2]server.Id{networkId, sid}
	previous, ok := self.attempts[key]
	if ok && now-previous < sessionUseInterval.Milliseconds() {
		return false
	}
	if len(self.attempts) >= sessionUseCacheLimit {
		for key, value := range self.attempts {
			if now-value >= sessionUseInterval.Milliseconds() {
				delete(self.attempts, key)
			}
		}
		if len(self.attempts) >= sessionUseCacheLimit {
			return false
		}
	}
	self.attempts[key] = now
	return true
}

const sessionSampleLua = `
local now=tonumber(ARGV[1]);local sid=ARGV[2];local observed=tonumber(ARGV[3]);local accept=tonumber(redis.call('ZSCORE',KEYS[1],sid)) or 0
if redis.call('EXISTS',KEYS[3])==1 or accept<=now then return 0 end
local old=redis.call('GET',KEYS[2])
if old then local ok,value=pcall(cjson.decode,old);if ok and tonumber(value.unix_time) and observed<tonumber(value.unix_time)+60 then return 0 end end
redis.call('SET',KEYS[2],ARGV[4]);redis.call('PEXPIREAT',KEYS[2],accept);return 1
`

// The hash-only task context intentionally cannot produce a usage observation.
func (self *ClientSession) LastUsedObservation(now time.Time) *SessionLastUsed {
	if self.clientAddressHash != nil {
		return nil
	}
	info := self.ClientInfo
	if info.DeviceType == "" {
		info = UnknownClientInfo()
	}
	observation := &SessionLastUsed{UnixTime: now.Unix(), DeviceType: info.DeviceType, AppVersion: info.AppVersion}
	if ip, _, err := self.ParseClientIpPort(); err == nil {
		if place, err := server.GetIpInfo(ip); err == nil && place != nil {
			observation.City = place.City
			observation.Region = place.Region
			observation.Country = place.Country
			observation.CountryCode = place.CountryCode
		}
	}
	return observation
}

// Called after successful admission and after owned SQL connections are released.
// A failed optional sample cannot convert valid authentication into a rejection.
func (self *ClientSession) ObserveAuthenticatedUse() {
	if self.ByJwt == nil || self.ByJwt.SessionId == nil || self.clientAddressHash != nil {
		return
	}
	defer func() {
		if recover() != nil {
			sessionUseDrops.Inc()
		}
	}()
	now := server.NowUtc()
	sid := *self.ByJwt.SessionId
	if !sessionUseAttempts.admit(self.ByJwt.NetworkId, sid, now.UnixMilli()) {
		return
	}
	observation := self.LastUsedObservation(now)
	if observation == nil {
		return
	}
	if err := writeSessionObservation(self.Ctx, self.ByJwt.NetworkId, sid, *observation, now); err != nil {
		sessionUseDrops.Inc()
	}
}
func writeSessionObservation(ctx context.Context, networkId, sid server.Id, observation SessionLastUsed, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, sessionUseBudget)
	defer cancel()
	encoded, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	return server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		return r.Eval(ctx, sessionSampleLua, []string{SessionKey(networkId, "z"), SessionKey(networkId, "u:"+sid.String()), SessionMarkerKey(networkId, sid)}, now.UnixMilli(), sid.String(), observation.UnixTime, encoded).Err()
	})
}
