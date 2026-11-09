package model

// §6.8's hint state on server's Redis (PROTO-M1-07): sidecar's pushservice stores over server.Redis, for
// the topics api owns, core and team, its long-poll (PROTO-M1-10) and the residents' publishers
// (PROTO-M1-09). The keys are §2.8's connect-4 and api-6, every one under the client's tag {scp_<C>},
// and pushservice builds them, so api, residents and the owning services name one key one way.
//
// Every call runs on the deadline pool with no retry and no preflight, as the sockets store does: a hint
// script is no idempotent write, so it is never replayed, and a call that fails is a lost hint that the
// client's hourly resync covers. A call takes no PostgreSQL connection, so a caller inside a
// transaction may make it (connect/CODESTYLE.md's locking rules), and none acquires a second Redis
// connection while it holds one.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
)

var _ pushservice.PublisherRedis = (*SidecarPushRedis)(nil)

var (
	// A call whose keys share no one tag, which production's cluster answers CROSSSLOT, so this
	// refuses before Redis is asked (§22.4's lint at run time).
	errSidecarPushCrossSlot = errors.New("sidecar push: keys share no hash tag")
	// A second script under one name with another source.
	errSidecarPushScriptName = errors.New("sidecar push: a script name with two sources")
	// SidecarHint for a topic api does not own: its owner writes it, through its host's Push.
	ErrSidecarHintTopic = errors.New("sidecar push: api hints core and team only")
)

// The adapter's own tunables.
type SidecarPushRedisSettings struct {
	// The longest one call may take when its caller gave no deadline: 2 s, so a hint never holds an api
	// request on a slow node past what its caller allowed.
	CallTimeout time.Duration
}

// The defaults.
func DefaultSidecarPushRedisSettings() *SidecarPushRedisSettings {
	return &SidecarPushRedisSettings{
		CallTimeout: 2 * time.Second,
	}
}

// pushservice.PublisherRedis over server.Redis. Safe for concurrent use.
type SidecarPushRedis struct {
	settings *SidecarPushRedisSettings
	// *redis.Script by RedisScript.Name, each sending EVALSHA with a fallback to its source
	scripts sync.Map
}

// An adapter with settings, nil being the defaults.
func NewSidecarPushRedis(settings *SidecarPushRedisSettings) *SidecarPushRedis {
	if settings == nil {
		settings = DefaultSidecarPushRedisSettings()
	}
	return &SidecarPushRedis{
		settings: settings,
	}
}

// Runs callback on the deadline pool with no retry and no preflight, bounded by ctx's deadline or
// CallTimeout, and turns the wrapper's panics into errors.
func (self *SidecarPushRedis) redis(ctx context.Context, callback func(client server.RedisClient) error) (returnErr error) {
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, self.settings.CallTimeout)
		defer cancel()
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("sidecar push: %v", recovered)
			}
		}
	}()
	return server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		return callback(client)
	}, server.OptRedisNoPreflight())
}

// The go-redis script for one RedisScript, made once per name.
func (self *SidecarPushRedis) script(script *sidecar.RedisScript) (*redis.Script, error) {
	type namedScript struct {
		source string
		script *redis.Script
	}
	value, _ := self.scripts.LoadOrStore(script.Name, &namedScript{source: script.Source, script: redis.NewScript(script.Source)})
	named := value.(*namedScript)
	if named.source != script.Source {
		return nil, errSidecarPushScriptName
	}
	return named.script, nil
}

// A go-redis reply as the root's RedisReply. An integer stays an int64; a bulk or status reply is its
// bytes; nil is a nil reply.
func sidecarRedisReply(value any) (*sidecar.RedisReply, error) {
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
			reply, err := sidecarRedisReply(element)
			if err != nil {
				return nil, err
			}
			replies = append(replies, reply)
		}
		return &sidecar.RedisReply{Kind: sidecar.RedisReplyArray, Replies: replies}, nil
	}
	return nil, fmt.Errorf("sidecar push: a reply of %T", value)
}

// See sidecar.Redis.
func (self *SidecarPushRedis) RunScript(ctx context.Context, script *sidecar.RedisScript, keys []string, args []string) (*sidecar.RedisReply, error) {
	if !sidecar.RedisKeysShareTag(keys) {
		return nil, errSidecarPushCrossSlot
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
	err = self.redis(ctx, func(client server.RedisClient) error {
		value, err := redisScript.Run(ctx, client, keys, argv...).Result()
		if errors.Is(err, redis.Nil) {
			value, err = nil, nil
		}
		if err != nil {
			return err
		}
		reply, err = sidecarRedisReply(value)
		return err
	})
	if err != nil {
		return nil, err
	}
	return reply, nil
}

// See sidecar.Redis.
func (self *SidecarPushRedis) Get(ctx context.Context, key string) (value []byte, found bool, returnErr error) {
	returnErr = self.redis(ctx, func(client server.RedisClient) error {
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

// See sidecar.Redis. A ttl of zero leaves the key with no expiry.
func (self *SidecarPushRedis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl < 0 {
		return fmt.Errorf("sidecar push: a ttl of %v", ttl)
	}
	return self.redis(ctx, func(client server.RedisClient) error {
		return client.Set(ctx, key, value, ttl).Err()
	})
}

// See sidecar.Redis: SET NX, a poll's {scp_<C>}hb (§6.16).
func (self *SidecarPushRedis) SetIfAbsent(ctx context.Context, key string, value []byte, ttl time.Duration) (set bool, returnErr error) {
	if ttl < 0 {
		return false, fmt.Errorf("sidecar push: a ttl of %v", ttl)
	}
	returnErr = self.redis(ctx, func(client server.RedisClient) error {
		var err error
		set, err = client.SetNX(ctx, key, value, ttl).Result()
		return err
	})
	return
}

// See sidecar.Redis: an absent key is an empty map.
func (self *SidecarPushRedis) HashGetAll(ctx context.Context, key string) (fieldValues map[string][]byte, returnErr error) {
	returnErr = self.redis(ctx, func(client server.RedisClient) error {
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

// See sidecar.Redis: XRANGE after afterEntryId, exclusive, from the start when it is empty.
func (self *SidecarPushRedis) StreamRange(ctx context.Context, key string, afterEntryId string, count int64) (entries []*sidecar.RedisStreamEntry, returnErr error) {
	if count <= 0 {
		return nil, fmt.Errorf("sidecar push: a count of %d", count)
	}
	start := "-"
	if afterEntryId != "" {
		start = "(" + afterEntryId
	}
	returnErr = self.redis(ctx, func(client server.RedisClient) error {
		messages, err := client.XRangeN(ctx, key, start, "+", count).Result()
		if err != nil {
			return err
		}
		entries = make([]*sidecar.RedisStreamEntry, 0, len(messages))
		for _, message := range messages {
			fieldValues := make(map[string][]byte, len(message.Values))
			for field, value := range message.Values {
				text, ok := value.(string)
				if !ok {
					return fmt.Errorf("sidecar push: a stream value of %T", value)
				}
				fieldValues[field] = []byte(text)
			}
			entries = append(entries, &sidecar.RedisStreamEntry{Id: message.ID, FieldValues: fieldValues})
		}
		return nil
	})
	return
}

// The keyspace channel pattern of every client's {scp_<C>}n on db: the one family api's long-poll and
// the residents' publishers watch, through one shared subscription per process, never one per client.
func ScpHintKeyEventPattern(db int) string {
	return fmt.Sprintf("__keyspace@%d__:%s", db, pushservice.HintSeqKeyPattern)
}

// The key a keyspace channel names, or false for a channel that is no keyspace channel.
func ParseKeyspaceChannelKey(channel string) (string, bool) {
	rest, found := strings.CutPrefix(channel, "__keyspace@")
	if !found {
		return "", false
	}
	_, key, found := strings.Cut(rest, "__:")
	if !found || key == "" {
		return "", false
	}
	return key, true
}

// The client whose {scp_<C>}n a channel matching ScpHintKeyEventPattern names.
func ParseScpHintKeyEvent(channel string) (connect.Id, bool) {
	key, ok := ParseKeyspaceChannelKey(channel)
	if !ok {
		return connect.Id{}, false
	}
	return pushservice.ParseHintSeqKey(key)
}

// api's one hint writer for its own topics, made at first use.
var sidecarHintWriter = sync.OnceValues(func() (*pushservice.HintWriter, error) {
	return pushservice.NewHintWriter(NewSidecarPushRedis(nil), pushservice.DefaultHintWriterSettings(), nil, nil)
})

// Hints each client of a change to a topic api owns: core (§5; plan-CORE-core-hint-body) and team
// (§7; TEAM-M2-14's hints carry the log position n). One script per client, on its own slot. A lost
// hint is a client's resync away, so an error is the caller's to count, never to retry blindly: the
// script may have applied. Holds no PostgreSQL connection, so a caller in a transaction may call it.
func SidecarHint(ctx context.Context, clientIds []server.Id, hint *sidecar.ClientHint) error {
	if hint == nil || (hint.Topic != sidecar.PushTopicCore && hint.Topic != sidecar.PushTopicTeam) {
		return ErrSidecarHintTopic
	}
	writer, err := sidecarHintWriter()
	if err != nil {
		return err
	}
	connectClientIds := make([]connect.Id, len(clientIds))
	for index, clientId := range clientIds {
		connectClientIds[index] = connect.Id(clientId)
	}
	return writer.Hint(ctx, connectClientIds, hint)
}
