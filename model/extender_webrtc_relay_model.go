// The signaling relay of the peer-to-peer webrtc extender carrier
// (connect/EXTENDER.md S8). The operator's prober -- the api's activation
// probe and the taskworker's uptime task -- signals an extender's client
// through the exchange resident that holds it, which no api or taskworker
// process can reach directly. Two per-client Redis keys carry it, modeled on
// the client close requests (client_close_model.go):
//   - `{<clientId>}w_offer` is a set of pending offers for the client. Only the
//     prober adds to it, for the one extender it is testing, and its size is
//     bounded at the add. The process that hosts the client's resident takes
//     each offer exactly once (SPOP) on the set's sadd event, relays it to the
//     client from the control id as a flagged `ExchangeSignals`, and posts the
//     answer. Each add renews the set's ttl, which collects offers nobody
//     took.
//   - `{<clientId>}w_answer:<requestId>` is the list the resident pushes the
//     one answer of an offer onto, which the prober waits on with BLPOP. Lists
//     raise no keyspace event under Main's `Kg$sx`, so an answer costs no
//     notification. Its ttl collects an answer that came after the prober
//     stopped waiting.
//
// Both keys of a client share its slot, so each script runs on one node.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

const extenderWebRtcOfferKeySuffix = "w_offer"
const extenderWebRtcAnswerKeySuffix = "w_answer"

func extenderWebRtcOfferKey(clientId server.Id) string {
	return fmt.Sprintf("{%s}%s", clientId, extenderWebRtcOfferKeySuffix)
}

func extenderWebRtcAnswerKey(clientId server.Id, requestId server.Id) string {
	return fmt.Sprintf("{%s}%s:%s", clientId, extenderWebRtcAnswerKeySuffix, requestId)
}

// The psubscribe pattern for the keyspace channels of every client's pending
// webrtc offers on a db.
func ExtenderWebRtcOfferKeyEventPattern(db int) string {
	return fmt.Sprintf("__keyspace@%d__:{*}%s", db, extenderWebRtcOfferKeySuffix)
}

// The client id of a keyspace channel matching
// `ExtenderWebRtcOfferKeyEventPattern`.
func ParseExtenderWebRtcOfferKeyEvent(channel string) (server.Id, bool) {
	return parseClientKeyEvent(channel, extenderWebRtcOfferKeySuffix)
}

// Why a relay of one offer did not come back with an answer, as the resident
// posts it. A prober reads these; any other value is a relay failure too.
const (
	// the offer's deadline had passed when the resident took it
	ExtenderWebRtcRelayErrorExpired = "expired"
	// the offer, or the answer, was past the relay's size bound
	ExtenderWebRtcRelayErrorOversized = "oversized"
	// the offer did not decode
	ExtenderWebRtcRelayErrorMalformed = "malformed"
	// the client's offers came faster than the resident's rate allows
	ExtenderWebRtcRelayErrorRateLimited = "rate_limited"
	// the exchange was relaying as many offers as it takes at once
	ExtenderWebRtcRelayErrorBusy = "busy"
	// the client did not answer before the offer's deadline, or its answer
	// failed: an extender with no answerer, or one that predates the flag,
	// sends nothing back
	ExtenderWebRtcRelayErrorNoAnswer = "no_answer"
)

// One offer the prober relays to an extender's client.
type ExtenderWebRtcOffer struct {
	// the prober waits for the answer under this id
	RequestId server.Id `json:"r"`
	// when the prober stops waiting, unix milliseconds. The resident relays
	// nothing past it and bounds the exchange with the client by it.
	ExpireTimeMs int64 `json:"e"`
	// the dialer's SDP offer: the json of a webrtc session description
	Offer string `json:"o"`
}

// ExpireTimeMs as a time, in utc.
func (self *ExtenderWebRtcOffer) ExpireTime() time.Time {
	return time.UnixMilli(self.ExpireTimeMs).UTC()
}

// The resident's answer to one offer.
type ExtenderWebRtcAnswer struct {
	// the extender's SDP answer, the json of a webrtc session description;
	// empty when the relay failed
	Answer string `json:"a,omitempty"`
	// why the relay failed (ExtenderWebRtcRelayError...); empty with an answer
	Error string `json:"e,omitempty"`
}

// Adds one offer to the client's pending offers and renews their ttl, unless
// maxPendingCount are pending already, in one script on the client's slot.
// Returns the member the offer is held under, which the prober removes when it
// stops waiting before a resident took it, and whether it was added.
func AddExtenderWebRtcOffer(
	ctx context.Context,
	clientId server.Id,
	offer *ExtenderWebRtcOffer,
	maxPendingCount int,
	ttl time.Duration,
) (member string, added bool) {
	memberBytes, err := json.Marshal(offer)
	if err != nil {
		return "", false
	}
	member = string(memberBytes)
	server.Redis(ctx, func(r server.RedisClient) {
		result, err := r.Eval(
			ctx,
			`
			if tonumber(ARGV[2]) <= redis.call('SCARD', KEYS[1]) then
				return 0
			end
			redis.call('SADD', KEYS[1], ARGV[1])
			redis.call('PEXPIRE', KEYS[1], ARGV[3])
			return 1
			`,
			[]string{extenderWebRtcOfferKey(clientId)},
			member,
			maxPendingCount,
			max(1, ttl.Milliseconds()),
		).Int()
		server.Raise(err)
		added = result == 1
	})
	return
}

// Removes one pending offer, which the prober does when it stops waiting
// before a resident took it, so nothing relays an offer no one will use.
func RemoveExtenderWebRtcOffer(ctx context.Context, clientId server.Id, member string) {
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.SRem(ctx, extenderWebRtcOfferKey(clientId), member).Err())
	})
}

// Takes up to `limit` pending offers of the client. SPOP gives each offer to
// exactly one taker; one that does not decode is dropped.
func TakeExtenderWebRtcOffers(ctx context.Context, clientId server.Id, limit int) []*ExtenderWebRtcOffer {
	offers := []*ExtenderWebRtcOffer{}
	if limit <= 0 {
		return offers
	}
	server.RedisDoOnce(ctx, func(r server.RedisClient) {
		members, err := r.SPopN(ctx, extenderWebRtcOfferKey(clientId), int64(limit)).Result()
		if err == redis.Nil {
			return
		}
		server.Raise(err)
		for _, member := range members {
			offer := &ExtenderWebRtcOffer{}
			if err := json.Unmarshal([]byte(member), offer); err != nil {
				continue
			}
			offers = append(offers, offer)
		}
	})
	return offers
}

// Posts the one answer of an offer and sets its ttl, in one script on the
// client's slot.
func PutExtenderWebRtcAnswer(
	ctx context.Context,
	clientId server.Id,
	requestId server.Id,
	answer *ExtenderWebRtcAnswer,
	ttl time.Duration,
) {
	answerBytes, err := json.Marshal(answer)
	if err != nil {
		return
	}
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Eval(
			ctx,
			`
			redis.call('RPUSH', KEYS[1], ARGV[1])
			redis.call('PEXPIRE', KEYS[1], ARGV[2])
			return 0
			`,
			[]string{extenderWebRtcAnswerKey(clientId, requestId)},
			string(answerBytes),
			max(1, ttl.Milliseconds()),
		).Err())
	})
}

// Waits up to `timeout` for the answer of one offer, which BLPOP takes and so
// removes. Redis counts the wait in whole seconds: a timeout under a second
// waits a second, and a longer one is cut to its whole seconds. false when no
// answer came, or one came that does not decode.
func WaitExtenderWebRtcAnswer(
	ctx context.Context,
	clientId server.Id,
	requestId server.Id,
	timeout time.Duration,
) (answer *ExtenderWebRtcAnswer, ok bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		values, err := r.BLPop(ctx, max(time.Second, timeout), extenderWebRtcAnswerKey(clientId, requestId)).Result()
		if err == redis.Nil {
			return
		}
		server.Raise(err)
		// the key, then the value
		if len(values) != 2 {
			return
		}
		decoded := &ExtenderWebRtcAnswer{}
		if err := json.Unmarshal([]byte(values[1]), decoded); err != nil {
			return
		}
		answer = decoded
		ok = true
	}, server.OptNoRetry())
	return
}
