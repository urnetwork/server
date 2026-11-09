package sidecarhost

// ServiceHost.Redis on server's Redis (host_redis.go; §19.7, D-OPS-7): every method of sidecar.Redis
// over server's pools, so engines reach the cluster without importing go-redis.
//
// Two boundaries carry the calls. A call that is safe to apply twice takes server.Redis, which reruns
// it after a connection error as every idempotent caller in server does. A call that is not takes
// server.RedisDoOnce, whose pool neither retries a command in go-redis nor reruns the callback: go-redis
// retries a failed pipeline whole, so a lost answer would otherwise apply an Increment, a StreamAdd, a
// script or a compare twice. SetIfAbsent, XREADGROUP and XAUTOCLAIM take it too, since a second try
// answers differently from the first: a SET NX that applied reads as refused, and a read or claim that
// applied hands its entries to a consumer that never saw them until they idle out.
//
// Every call of two or more keys is checked against sidecar.RedisKeysShareTag before Redis is asked, and
// refused as production's cluster refuses it, CROSSSLOT, so a one-node store cannot pass what 3 masters
// refuse (§22.4's lint at run time). Lengths, counts and ttls that Redis would read as another operation
// are refused here too, as host_redis.go lists them.
//
// A call with no deadline in its ctx is bounded by RedisSettings.CallTimeout. Safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

var _ sidecar.Redis = (*Redis)(nil)

var (
	// What production's cluster answers a call whose keys share no hash tag, given before Redis is
	// asked. The text starts as the cluster's does, so a caller reads either alike.
	errRedisCrossSlot = errors.New("CROSSSLOT keys of one call share no hash tag (§22.4)")
	// A second script under one name with another source.
	errRedisScriptName = errors.New("sidecarhost redis: a script name with two sources")
)

var (
	// host_redis.go's lease renewal: replaces the value only while the key holds the expected one, with
	// no expiry for a ttl of zero.
	redisCompareAndSetScript = &sidecar.RedisScript{
		Name: "sidecarhost-compare-and-set",
		Source: `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
if ARGV[3] == '0' then
	redis.call('SET', KEYS[1], ARGV[2])
else
	redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
end
return 1`,
	}
	// host_redis.go's lease release, which another holder's newer lease survives.
	redisCompareAndDeleteScript = &sidecar.RedisScript{
		Name: "sidecarhost-compare-and-delete",
		Source: `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
redis.call('DEL', KEYS[1])
return 1`,
	}
	// INCR with EXPIRE NX in one step. The value comes back as GET's string, since INCR's Lua number is a
	// double and host_redis.go lets no large number through Lua.
	redisIncrementScript = &sidecar.RedisScript{
		Name: "sidecarhost-increment",
		Source: `
redis.call('INCR', KEYS[1])
redis.call('PEXPIRE', KEYS[1], ARGV[1], 'NX')
return redis.call('GET', KEYS[1])`,
	}
)

// The adapter's tunables.
type RedisSettings struct {
	// The longest one call may take when its caller's ctx sets no deadline, a blocked read's block on
	// top: 5 s, so a retry against a node that stopped answering ends instead of holding its caller for
	// server's minute of reruns. Not positive: no bound but ctx.
	CallTimeout time.Duration
}

// The defaults.
func DefaultRedisSettings() *RedisSettings {
	return &RedisSettings{
		CallTimeout: 5 * time.Second,
	}
}

// The two ways a call reaches Redis. Production's run on server's pools; a test may give its own.
type redisBoundary struct {
	// For a call that is safe to apply twice: server.Redis, which reruns it after a connection error.
	retrying func(ctx context.Context, callback func(client server.RedisClient) error) error
	// For a call that is not: server.RedisDoOnce, which never replays it.
	once func(ctx context.Context, callback func(client server.RedisClient) error) error
}

// Runs callback through server.Redis. An error the callback returns is raised, so a connection error
// reruns it and any other ends the call, and the wrapper's panics come back as errors.
func redisRetrying(ctx context.Context, callback func(client server.RedisClient) error) (returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = recoveredError(recovered)
		}
	}()
	server.Redis(ctx, func(client server.RedisClient) {
		if err := callback(client); err != nil {
			panic(err)
		}
	})
	return nil
}

// Runs callback through server.RedisDoOnce, which retries nothing, and returns the wrapper's panics as
// errors.
func redisOnce(ctx context.Context, callback func(client server.RedisClient) error) (returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = recoveredError(recovered)
		}
	}()
	server.RedisDoOnce(ctx, func(client server.RedisClient) {
		returnErr = callback(client)
	})
	return
}

// A recovered panic as an error.
func recoveredError(recovered any) error {
	if err, ok := recovered.(error); ok {
		return err
	}
	return fmt.Errorf("sidecarhost redis: %v", recovered)
}

// sidecar.Redis over server's Redis. NewRedis makes one.
type Redis struct {
	settings *RedisSettings
	boundary *redisBoundary
	// the process's one shared subscription (redis_key_events.go)
	keyEvents *redisKeyEvents
	// *redisNamedScript by sidecar.RedisScript.Name
	scripts sync.Map
}

// An adapter on server's pools and the process's one key-event subscription, with settings, nil being
// the defaults.
func NewRedis(settings *RedisSettings) *Redis {
	return newRedis(settings, &redisBoundary{retrying: redisRetrying, once: redisOnce}, processRedisKeyEvents())
}

func newRedis(settings *RedisSettings, boundary *redisBoundary, keyEvents *redisKeyEvents) *Redis {
	if settings == nil {
		settings = DefaultRedisSettings()
	}
	return &Redis{
		settings:  settings,
		boundary:  boundary,
		keyEvents: keyEvents,
	}
}

// ctx with the call's bound when it has none: CallTimeout, plus extra for a blocked read.
func (self *Redis) bounded(ctx context.Context, extra time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok || self.settings.CallTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, self.settings.CallTimeout+extra)
}

// A call safe to apply twice.
func (self *Redis) retrying(ctx context.Context, callback func(ctx context.Context, client server.RedisClient) error) error {
	ctx, cancel := self.bounded(ctx, 0)
	defer cancel()
	return self.boundary.retrying(ctx, func(client server.RedisClient) error {
		return callback(ctx, client)
	})
}

// A call never replayed, bounded with extra on top of CallTimeout.
func (self *Redis) once(ctx context.Context, extra time.Duration, callback func(ctx context.Context, client server.RedisClient) error) error {
	ctx, cancel := self.bounded(ctx, extra)
	defer cancel()
	return self.boundary.once(ctx, func(client server.RedisClient) error {
		return callback(ctx, client)
	})
}

// host_redis.go's rounding: up to the millisecond Redis keeps, so a positive ttl never reaches it as
// zero.
func redisRoundUp(ttl time.Duration) time.Duration {
	return (ttl + time.Millisecond - 1) / time.Millisecond * time.Millisecond
}

// The same in whole milliseconds, as a script's argument.
func redisMilliseconds(ttl time.Duration) string {
	return strconv.FormatInt(int64(redisRoundUp(ttl)/time.Millisecond), 10)
}

func checkRedisTtl(ttl time.Duration) error {
	if ttl < 0 {
		return fmt.Errorf("sidecarhost redis: a ttl of %v", ttl)
	}
	return nil
}

// A ttl Redis would take as a delete is the caller's bug.
func checkRedisPositiveTtl(ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("sidecarhost redis: a ttl of %v, which Redis would take as a delete", ttl)
	}
	return nil
}

func checkRedisCount(count int64) error {
	if count <= 0 {
		return fmt.Errorf("sidecarhost redis: a count of %d", count)
	}
	return nil
}

// §22.4's rule, at run time: a call of two or more keys shares one hash tag.
func checkRedisKeys(keys []string) error {
	if !sidecar.RedisKeysShareTag(keys) {
		return errRedisCrossSlot
	}
	return nil
}

// See sidecar.Redis.
func (self *Redis) Get(ctx context.Context, key string) (value []byte, found bool, returnErr error) {
	returnErr = self.retrying(ctx, func(ctx context.Context, client server.RedisClient) error {
		value, found = nil, false
		bytes, err := client.Get(ctx, key).Bytes()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		value, found = bytes, true
		return nil
	})
	return
}

// See sidecar.Redis: MGET, in keys' order, nil for an absent key or one of another kind.
func (self *Redis) GetMany(ctx context.Context, keys []string) (values [][]byte, returnErr error) {
	if err := checkRedisKeys(keys); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return [][]byte{}, nil
	}
	returnErr = self.retrying(ctx, func(ctx context.Context, client server.RedisClient) error {
		replies, err := client.MGet(ctx, keys...).Result()
		if err != nil {
			return err
		}
		if len(replies) != len(keys) {
			return fmt.Errorf("sidecarhost redis: MGET of %d keys answered %d", len(keys), len(replies))
		}
		values = make([][]byte, len(keys))
		for index, reply := range replies {
			switch reply := reply.(type) {
			case nil:
			case string:
				values[index] = []byte(reply)
			default:
				return fmt.Errorf("sidecarhost redis: an MGET value of %T", reply)
			}
		}
		return nil
	})
	return
}

// See sidecar.Redis: a ttl of zero leaves the key with no expiry, clearing one it had.
func (self *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := checkRedisTtl(ttl); err != nil {
		return err
	}
	return self.retrying(ctx, func(ctx context.Context, client server.RedisClient) error {
		return client.Set(ctx, key, value, redisRoundUp(ttl)).Err()
	})
}

// See sidecar.Redis: SET NX, never replayed, since a second try of one that applied reads as refused.
func (self *Redis) SetIfAbsent(ctx context.Context, key string, value []byte, ttl time.Duration) (set bool, returnErr error) {
	if err := checkRedisTtl(ttl); err != nil {
		return false, err
	}
	returnErr = self.once(ctx, 0, func(ctx context.Context, client server.RedisClient) error {
		var err error
		set, err = client.SetNX(ctx, key, value, redisRoundUp(ttl)).Result()
		return err
	})
	return
}

// See sidecar.Redis: one script, never replayed.
func (self *Redis) CompareAndSet(ctx context.Context, key string, expected []byte, value []byte, ttl time.Duration) (bool, error) {
	if err := checkRedisTtl(ttl); err != nil {
		return false, err
	}
	reply, err := self.RunScript(ctx, redisCompareAndSetScript, []string{key}, []string{string(expected), string(value), redisMilliseconds(ttl)})
	if err != nil {
		return false, err
	}
	return reply.Kind == sidecar.RedisReplyInteger && reply.Integer == 1, nil
}

// See sidecar.Redis: one script, never replayed.
func (self *Redis) CompareAndDelete(ctx context.Context, key string, expected []byte) (bool, error) {
	reply, err := self.RunScript(ctx, redisCompareAndDeleteScript, []string{key}, []string{string(expected)})
	if err != nil {
		return false, err
	}
	return reply.Kind == sidecar.RedisReplyInteger && reply.Integer == 1, nil
}

// See sidecar.Redis: INCR alone for a ttl of zero, else INCR and PEXPIRE NX in one script, never
// replayed either way.
func (self *Redis) Increment(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	if err := checkRedisTtl(ttl); err != nil {
		return 0, err
	}
	if ttl == 0 {
		var value int64
		err := self.once(ctx, 0, func(ctx context.Context, client server.RedisClient) error {
			var err error
			value, err = client.Incr(ctx, key).Result()
			return err
		})
		return value, err
	}
	reply, err := self.RunScript(ctx, redisIncrementScript, []string{key}, []string{redisMilliseconds(ttl)})
	if err != nil {
		return 0, err
	}
	if reply.Kind != sidecar.RedisReplyBulk {
		return 0, fmt.Errorf("sidecarhost redis: an increment answered %v", reply.Kind)
	}
	return strconv.ParseInt(string(reply.Bulk), 10, 64)
}

// See sidecar.Redis: absent keys are no error.
func (self *Redis) Delete(ctx context.Context, keys []string) error {
	if err := checkRedisKeys(keys); err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	return self.retrying(ctx, func(ctx context.Context, client server.RedisClient) error {
		return client.Del(ctx, keys...).Err()
	})
}

// See sidecar.Redis: PEXPIRE, refusing a ttl Redis would take as a delete.
func (self *Redis) Expire(ctx context.Context, key string, ttl time.Duration) (found bool, returnErr error) {
	if err := checkRedisPositiveTtl(ttl); err != nil {
		return false, err
	}
	returnErr = self.retrying(ctx, func(ctx context.Context, client server.RedisClient) error {
		var err error
		found, err = client.PExpire(ctx, key, redisRoundUp(ttl)).Result()
		return err
	})
	return
}

// See sidecar.Redis.
func (self *Redis) HashGet(ctx context.Context, key string, field string) (value []byte, found bool, returnErr error) {
	returnErr = self.retrying(ctx, func(ctx context.Context, client server.RedisClient) error {
		value, found = nil, false
		bytes, err := client.HGet(ctx, key, field).Bytes()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		value, found = bytes, true
		return nil
	})
	return
}

// See sidecar.Redis: an absent key is an empty map.
func (self *Redis) HashGetAll(ctx context.Context, key string) (fieldValues map[string][]byte, returnErr error) {
	returnErr = self.retrying(ctx, func(ctx context.Context, client server.RedisClient) error {
		values, err := client.HGetAll(ctx, key).Result()
		if err != nil {
			return err
		}
		fieldValues = make(map[string][]byte, len(values))
		for field, value := range values {
			fieldValues[field] = []byte(value)
		}
		return nil
	})
	return
}

// field, value pairs in field order, so a command is the same bytes every run.
func redisFieldValueArgs(fieldValues map[string][]byte) []any {
	args := make([]any, 0, 2*len(fieldValues))
	for _, field := range slices.Sorted(maps.Keys(fieldValues)) {
		args = append(args, field, fieldValues[field])
	}
	return args
}

// See sidecar.Redis: HSET, which also clears a written field's ttl. No fields is nothing to write.
func (self *Redis) HashSet(ctx context.Context, key string, fieldValues map[string][]byte) error {
	if len(fieldValues) == 0 {
		return nil
	}
	return self.retrying(ctx, func(ctx context.Context, client server.RedisClient) error {
		return client.HSet(ctx, key, redisFieldValueArgs(fieldValues)...).Err()
	})
}

// See sidecar.Redis: absent fields are no error.
func (self *Redis) HashDelete(ctx context.Context, key string, fields []string) error {
	if len(fields) == 0 {
		return nil
	}
	return self.retrying(ctx, func(ctx context.Context, client server.RedisClient) error {
		return client.HDel(ctx, key, fields...).Err()
	})
}

// See sidecar.Redis: HPEXPIRE, refusing a ttl Redis would take as a delete. An absent key or field is
// no error.
func (self *Redis) HashExpire(ctx context.Context, key string, fields []string, ttl time.Duration) error {
	if err := checkRedisPositiveTtl(ttl); err != nil {
		return err
	}
	if len(fields) == 0 {
		return nil
	}
	return self.retrying(ctx, func(ctx context.Context, client server.RedisClient) error {
		return client.HPExpire(ctx, key, redisRoundUp(ttl), fields...).Err()
	})
}

// The go-redis script for one RedisScript, made once per name, which sends EVALSHA and falls back to
// the source when the node answers NOSCRIPT.
type redisNamedScript struct {
	source string
	script *redis.Script
}

func (self *Redis) script(script *sidecar.RedisScript) (*redis.Script, error) {
	value, _ := self.scripts.LoadOrStore(script.Name, &redisNamedScript{source: script.Source, script: redis.NewScript(script.Source)})
	named := value.(*redisNamedScript)
	if named.source != script.Source {
		return nil, errRedisScriptName
	}
	return named.script, nil
}

// A go-redis reply as the root's RedisReply. An integer stays an int64, a bulk or status reply is its
// bytes, and nil is a nil reply.
func redisScriptReply(value any) (*sidecar.RedisReply, error) {
	switch value := value.(type) {
	case nil:
		return &sidecar.RedisReply{Kind: sidecar.RedisReplyNil}, nil
	case int64:
		return &sidecar.RedisReply{Kind: sidecar.RedisReplyInteger, Integer: value}, nil
	case string:
		return &sidecar.RedisReply{Kind: sidecar.RedisReplyBulk, Bulk: []byte(value)}, nil
	case []byte:
		return &sidecar.RedisReply{Kind: sidecar.RedisReplyBulk, Bulk: value}, nil
	case []any:
		replies := make([]*sidecar.RedisReply, 0, len(value))
		for _, element := range value {
			reply, err := redisScriptReply(element)
			if err != nil {
				return nil, err
			}
			replies = append(replies, reply)
		}
		return &sidecar.RedisReply{Kind: sidecar.RedisReplyArray, Replies: replies}, nil
	}
	return nil, fmt.Errorf("sidecarhost redis: a script reply of %T", value)
}

// See sidecar.Redis: by SHA-1 with a fallback to the source on NOSCRIPT, so a node that lost its
// scripts loads it again, and never replayed.
func (self *Redis) RunScript(ctx context.Context, script *sidecar.RedisScript, keys []string, args []string) (*sidecar.RedisReply, error) {
	if script == nil {
		return nil, errors.New("sidecarhost redis: no script")
	}
	if err := checkRedisKeys(keys); err != nil {
		return nil, err
	}
	redisScript, err := self.script(script)
	if err != nil {
		return nil, err
	}
	argv := make([]any, len(args))
	for index, arg := range args {
		argv[index] = arg
	}
	var reply *sidecar.RedisReply
	err = self.once(ctx, 0, func(ctx context.Context, client server.RedisClient) error {
		value, err := redisScript.Run(ctx, client, keys, argv...).Result()
		if errors.Is(err, redis.Nil) {
			value, err = nil, nil
		}
		if err != nil {
			return err
		}
		reply, err = redisScriptReply(value)
		return err
	})
	if err != nil {
		return nil, err
	}
	return reply, nil
}

// See sidecar.Redis: on the process's one shared subscription, never one per client.
func (self *Redis) AddKeyEventCallback(keyPattern string, callback sidecar.RedisKeyEventFunction) (remove func(), err error) {
	return self.keyEvents.AddCallback(keyPattern, callback)
}
