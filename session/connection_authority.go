// Connection authority is short lived. Stream grants bind immutable endpoint
// generations and never borrow a later login's authority for an old stream.
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/urnetwork/server"
	"time"
)

type ConnectionAuthority struct {
	ClientId     server.Id  `json:"client_id"`
	Generation   server.Id  `json:"generation"`
	NetworkId    server.Id  `json:"network_id"`
	SessionId    *server.Id `json:"session_id"`
	LeaseVersion uint32     `json:"lease_version"`
	Deadline     int64      `json:"deadline"`
}

func connectionAuthorityKeys(clientId server.Id) []string {
	prefix := fmt.Sprintf("{ca_%s}", clientId)
	return []string{prefix + "z", prefix + "h"}
}

const connectionAuthorityPublishLua = `
local now=tonumber(ARGV[1]);local deadline=tonumber(ARGV[3])
local old=redis.call('ZRANGEBYSCORE',KEYS[1],'-inf',now)
for _,id in ipairs(old) do redis.call('HDEL',KEYS[2],id) end
redis.call('ZREMRANGEBYSCORE',KEYS[1],'-inf',now)
if deadline<=now then return 0 end
redis.call('ZADD',KEYS[1],deadline,ARGV[2]);redis.call('HSET',KEYS[2],ARGV[2],ARGV[4])
local top=redis.call('ZREVRANGE',KEYS[1],0,0,'WITHSCORES');for i=1,2 do redis.call('PEXPIREAT',KEYS[i],tonumber(top[2])+300000) end
return 1
`

func PublishConnectionAuthority(ctx context.Context, credential *ByJwt, generation server.Id, version uint32, deadline time.Time) error {
	if credential.ClientId == nil {
		return sessionError("invalid_request")
	}
	value := ConnectionAuthority{ClientId: *credential.ClientId, Generation: generation, NetworkId: credential.NetworkId, SessionId: credential.SessionId, LeaseVersion: version, Deadline: deadline.UnixMilli()}
	encoded, _ := json.Marshal(value)
	return server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		return r.Eval(ctx, connectionAuthorityPublishLua, connectionAuthorityKeys(value.ClientId), server.NowUtc().UnixMilli(), generation.String(), value.Deadline, string(encoded)).Err()
	})
}
func RetireConnectionAuthority(ctx context.Context, clientId, generation server.Id) error {
	return server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		return r.Eval(ctx, `redis.call('ZREM',KEYS[1],ARGV[1]);redis.call('HDEL',KEYS[2],ARGV[1]);return 1`, connectionAuthorityKeys(clientId), generation.String()).Err()
	})
}

// With a generation, only that original endpoint may renew an existing grant.
func ReadConnectionAuthority(ctx context.Context, clientId server.Id, generation *server.Id, now time.Time) (*ConnectionAuthority, error) {
	var raw string
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		gen := ""
		if generation != nil {
			gen = generation.String()
		}
		var err error
		raw, err = r.Eval(ctx, `local id=ARGV[1];if id=='' then local top=redis.call('ZREVRANGEBYSCORE',KEYS[1],'+inf','('..ARGV[2],'LIMIT',0,1);id=top[1] or '' end;return redis.call('HGET',KEYS[2],id) or ''`, connectionAuthorityKeys(clientId), gen, now.UnixMilli()).Text()
		return err
	})
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	var value ConnectionAuthority
	if err = json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, err
	}
	if value.ClientId != clientId || value.Deadline <= now.UnixMilli() {
		return nil, nil
	}
	return &value, nil
}
