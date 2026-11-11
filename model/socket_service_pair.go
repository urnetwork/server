package model

// §6.10.1's pair, {svc_<listener_client_id>} field <dialer_client_id>, which an edge's or a ur/ Find
// writes (§9.6, D-SOCK-12) and CreateContract reads to issue a Service contract only for a pair a Find
// recorded (C9). Its value is the grant's expiry in milliseconds, 0 for none. A tls/ or smtp/ pair's
// field ttl is the lesser of the grant's remaining life and 15 min, renewed by the listener's accepted
// registration; a ur/ pair's is capped at 60 min and refreshed per Find (QD-656, §2.8's sock-3).
// Revocation or the last edge unlisten removes the key's pairs and leaves the floor field ~f, which no
// dialer's field is, refusing a pair written at an older position. Only the sockets store writes it.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The field's grant expiry and whether it is there.
func getSocketServicePair(ctx context.Context, client server.RedisClient, listenerClientId connect.Id, dialerClientId connect.Id) (uint64, bool, error) {
	value, err := client.HGet(ctx, socketPairKey(listenerClientId), socketHexId(dialerClientId)).Result()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	grantExpireMillis, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("socket pair: a malformed value")
	}
	return grantExpireMillis, true, nil
}

// CreateContract's one Redis read for §6.10.1: whether a Find recorded the pair, and the grant expiry
// it holds. ctx must carry the caller's deadline, since this runs on the no-retry deadline pool, and a
// ctx with none is an error. An error says nothing about the pair, and CreateContract then issues no
// Service contract.
func GetSocketServicePair(ctx context.Context, listenerClientId connect.Id, dialerClientId connect.Id) (grantExpireMillis uint64, found bool, returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("socket pair: %v", recovered)
			}
		}
	}()
	returnErr = server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		var err error
		grantExpireMillis, found, err = getSocketServicePair(ctx, client, listenerClientId, dialerClientId)
		return err
	}, server.OptRedisNoPreflight())
	return
}
