package model

// §6.10.1's pair, field <edge client_id> of {svc_<client_id>} for the end that is not urnetwork-edge's,
// which an edge's or a ur/ Find writes (§9.6, D-SOCK-12) and CreateContract reads to issue a Service
// contract only for a pair a Find recorded (C9). Every pair is keyed by its non-edge end, whichever end
// listens (QD-728): a tls/ or smtp/ Find, an edge dialing a sidecar's listener, writes field <edge dialer>
// of {svc_<sidecar listener>}, and a ur/ Find, a sidecar dialing an edge's ur/<service>, field
// <edge listener> of {svc_<sidecar dialer>}, so CreateContract's one read is field <edge> of
// {svc_<sidecar>} in every namespace. Its value is the grant's expiry in milliseconds, 0 for none. A tls/
// or smtp/ pair's field ttl is the lesser of the grant's remaining life and 15 min, renewed by the
// listener's accepted registration; a ur/ pair's is its service's grant, 60 min unless the service names
// more (smtpedge's 125, QD-190), written again at each Find and never shortened by a renewal (QD-656,
// §2.8's sock-3). The sidecar's removal, or a revocation or unlisten that leaves it no tls/ or smtp/ name,
// removes the key's pairs, a ur/ one with them, and leaves the floor field ~f, which no edge's field is,
// refusing a pair written at an older position. An edge's unlisten never reaches a ur/ pair, which ends
// at its ttl. Only the sockets store writes it.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Field fieldClientId of {svc_<keyClientId>}: its grant expiry and whether it is there.
func getSocketServicePair(ctx context.Context, client server.RedisClient, keyClientId connect.Id, fieldClientId connect.Id) (uint64, bool, error) {
	value, err := client.HGet(ctx, socketPairKey(keyClientId), socketHexId(fieldClientId)).Result()
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

// CreateContract's one Redis read for §6.10.1: whether a Find recorded the pair of pairClientId, the
// end that is not the edge, and edgeClientId, field edgeClientId of {svc_<pairClientId>}, and the grant
// expiry it holds. ctx must carry the caller's deadline, since this runs on the no-retry deadline pool,
// and a ctx with none is an error. An error says nothing about the pair, and CreateContract then issues
// no Service contract.
func GetSocketServicePair(ctx context.Context, pairClientId connect.Id, edgeClientId connect.Id) (grantExpireMillis uint64, found bool, returnErr error) {
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
		grantExpireMillis, found, err = getSocketServicePair(ctx, client, pairClientId, edgeClientId)
		return err
	}, server.OptRedisNoPreflight())
	return
}
