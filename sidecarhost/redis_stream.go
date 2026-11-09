package sidecarhost

// ServiceHost.Redis's streams (host_redis.go): §18.7's {wakeq_<shard>}, read through one consumer group
// by §19.4's push sender, and a publisher's hint stream (§6.8). XADD, XREADGROUP and XAUTOCLAIM change
// what the next call sees, so they are never replayed; XRANGE, XGROUP CREATE and XACK are.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// A go-redis stream entry as the root's. Every value is a bulk string, so binary values survive.
func redisStreamEntries(messages []redis.XMessage) ([]*sidecar.RedisStreamEntry, error) {
	entries := make([]*sidecar.RedisStreamEntry, 0, len(messages))
	for _, message := range messages {
		fieldValues := make(map[string][]byte, len(message.Values))
		for field, value := range message.Values {
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("sidecarhost redis: a stream value of %T", value)
			}
			fieldValues[field] = []byte(text)
		}
		entries = append(entries, &sidecar.RedisStreamEntry{Id: message.ID, FieldValues: fieldValues})
	}
	return entries, nil
}

// See sidecar.Redis: XADD with MAXLEN ~ maxLength, none for zero, never replayed.
func (self *Redis) StreamAdd(ctx context.Context, key string, maxLength int64, fieldValues map[string][]byte) (entryId string, returnErr error) {
	if maxLength < 0 {
		return "", fmt.Errorf("sidecarhost redis: a stream length of %d", maxLength)
	}
	if len(fieldValues) == 0 {
		return "", errors.New("sidecarhost redis: a stream entry with no fields")
	}
	args := &redis.XAddArgs{
		Stream: key,
		ID:     "*",
		Values: redisFieldValueArgs(fieldValues),
	}
	if 0 < maxLength {
		args.MaxLen = maxLength
		args.Approx = true
	}
	returnErr = self.once(ctx, 0, func(ctx context.Context, client server.RedisClient) error {
		var err error
		entryId, err = client.XAdd(ctx, args).Result()
		return err
	})
	return
}

// See sidecar.Redis: XRANGE after afterEntryId, exclusive, from the start when it is empty.
func (self *Redis) StreamRange(ctx context.Context, key string, afterEntryId string, count int64) (entries []*sidecar.RedisStreamEntry, returnErr error) {
	if err := checkRedisCount(count); err != nil {
		return nil, err
	}
	start := "-"
	if afterEntryId != "" {
		start = "(" + afterEntryId
	}
	returnErr = self.retrying(ctx, func(ctx context.Context, client server.RedisClient) error {
		messages, err := client.XRangeN(ctx, key, start, "+", count).Result()
		if err != nil {
			return err
		}
		entries, err = redisStreamEntries(messages)
		return err
	})
	return
}

// See sidecar.Redis: XGROUP CREATE MKSTREAM at $, and an existing group is no error.
func (self *Redis) StreamGroupCreate(ctx context.Context, key string, group string) error {
	return self.retrying(ctx, func(ctx context.Context, client server.RedisClient) error {
		err := client.XGroupCreateMkStream(ctx, key, group, "$").Err()
		if err != nil && strings.HasPrefix(err.Error(), "BUSYGROUP") {
			return nil
		}
		return err
	})
}

// See sidecar.Redis: XREADGROUP of entries no consumer has been given. A block of zero sends no BLOCK,
// which Redis would read as waiting forever. Never replayed.
func (self *Redis) StreamGroupRead(ctx context.Context, key string, group string, consumer string, count int64, block time.Duration) (entries []*sidecar.RedisStreamEntry, returnErr error) {
	if err := checkRedisCount(count); err != nil {
		return nil, err
	}
	if block < 0 {
		return nil, fmt.Errorf("sidecarhost redis: a block of %v", block)
	}
	args := &redis.XReadGroupArgs{
		Group:    group,
		Consumer: consumer,
		Streams:  []string{key, ">"},
		Count:    count,
		// go-redis sends BLOCK for any block that is not negative
		Block: -1,
	}
	if 0 < block {
		args.Block = redisRoundUp(block)
	}
	returnErr = self.once(ctx, block, func(ctx context.Context, client server.RedisClient) error {
		entries = []*sidecar.RedisStreamEntry{}
		streams, err := client.XReadGroup(ctx, args).Result()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, stream := range streams {
			read, err := redisStreamEntries(stream.Messages)
			if err != nil {
				return err
			}
			entries = append(entries, read...)
		}
		return nil
	})
	return
}

// See sidecar.Redis: XACK. A missing group or key acknowledges nothing and is no error.
func (self *Redis) StreamGroupAck(ctx context.Context, key string, group string, entryIds []string) error {
	if len(entryIds) == 0 {
		return nil
	}
	return self.retrying(ctx, func(ctx context.Context, client server.RedisClient) error {
		return client.XAck(ctx, key, group, entryIds...).Err()
	})
}

// See sidecar.Redis: XAUTOCLAIM from cursor, "0-0" when it is empty, and "0-0" back once the pending
// list is done. An entry a trim removed is neither returned nor kept pending. Never replayed, since a
// claim resets the entries' idle time.
func (self *Redis) StreamGroupClaim(ctx context.Context, key string, group string, consumer string, minIdle time.Duration, cursor string, count int64) (entries []*sidecar.RedisStreamEntry, nextCursor string, returnErr error) {
	if err := checkRedisCount(count); err != nil {
		return nil, "", err
	}
	if minIdle < 0 {
		return nil, "", fmt.Errorf("sidecarhost redis: an idle time of %v", minIdle)
	}
	if cursor == "" {
		cursor = "0-0"
	}
	args := &redis.XAutoClaimArgs{
		Stream:   key,
		Group:    group,
		Consumer: consumer,
		MinIdle:  redisRoundUp(minIdle),
		Start:    cursor,
		Count:    count,
	}
	returnErr = self.once(ctx, 0, func(ctx context.Context, client server.RedisClient) error {
		messages, start, err := client.XAutoClaim(ctx, args).Result()
		if err != nil {
			return err
		}
		entries, err = redisStreamEntries(messages)
		nextCursor = start
		return err
	})
	return
}
