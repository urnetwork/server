package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/router"
	"bringyour.com/bringyour/session"
	"github.com/cenkalti/backoff/v4"
)

const peerToPeerRedisKeyPrefix = "peer_to_peer"
const peerToPeerObjectExpire = 60 * time.Second
const pollTimeout = 10 * time.Second

func peerToPeerOfferRedisKey(id string) string {
	return fmt.Sprintf("%s:%s:offer", peerToPeerRedisKeyPrefix, id)
}

func peerToPeerAnswerRedisKey(id string) string {
	return fmt.Sprintf("%s:%s:answer", peerToPeerRedisKeyPrefix, id)
}

func peerToPeerHandshakeSetSDP(redisKeyFunc func(string) string, w http.ResponseWriter, r *http.Request) {

	pathValues := router.GetPathValues(r)

	sessionID := pathValues[0]

	router.WrapWithInputRequireClient(
		func(sdp json.RawMessage, cs *session.ClientSession) (string, error) {
			ctx := cs.Ctx
			var err error
			sdpKey := redisKeyFunc(sessionID)
			bringyour.Redis(ctx, func(client bringyour.RedisClient) {
				pipeline := client.Pipeline()
				pipeline.Set(ctx, sdpKey, []byte(sdp), 0)
				pipeline.Expire(ctx, sdpKey, peerToPeerObjectExpire)
				_, err = pipeline.Exec(ctx)
			})

			return "", err

		},
		w,
		r,
	)

}

func PeerToPeerHandshakeOfferSetSDP(w http.ResponseWriter, r *http.Request) {
	peerToPeerHandshakeSetSDP(peerToPeerOfferRedisKey, w, r)
}

func PeerToPeerHandshakeAnswerSetSDP(w http.ResponseWriter, r *http.Request) {
	peerToPeerHandshakeSetSDP(peerToPeerAnswerRedisKey, w, r)
}

func peerToPeerHandshakeLongPollSDP(redisKeyFunc func(string) string, w http.ResponseWriter, r *http.Request) {

	pathValues := router.GetPathValues(r)

	sessionID := pathValues[0]

	router.WrapRequireClient(
		func(cs *session.ClientSession) (json.RawMessage, error) {
			ctx := cs.Ctx
			var err error

			sdpKey := redisKeyFunc(sessionID)

			var sdp json.RawMessage

			bringyour.Redis(ctx, func(client bringyour.RedisClient) {
				ctx, cancel := context.WithTimeout(cs.Ctx, pollTimeout)
				defer cancel()

				bo := backoff.NewExponentialBackOff()
				bo.InitialInterval = 50 * time.Millisecond
				bo.MaxInterval = 1 * time.Second
				bo.MaxElapsedTime = pollTimeout

				boWithContext := backoff.WithContext(bo, ctx)

				sdp, err = backoff.RetryWithData(
					func() ([]byte, error) {
						res := client.Get(ctx, sdpKey)
						return res.Bytes()
					},
					boWithContext,
				)

			})

			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				w.WriteHeader(http.StatusNoContent)
				return nil, nil
			}

			if err != nil {
				return nil, err
			}

			return sdp, err

		},
		w,
		r,
	)

}

func PeerToPeerHandshakeLongPollOfferSDP(w http.ResponseWriter, r *http.Request) {
	peerToPeerHandshakeLongPollSDP(peerToPeerOfferRedisKey, w, r)
}

func PeerToPeerHandshakeLongPollAnswerSDP(w http.ResponseWriter, r *http.Request) {
	peerToPeerHandshakeLongPollSDP(peerToPeerAnswerRedisKey, w, r)
}

func peerToPeerOfferPeerCandidatesRedisKey(id string) string {
	return fmt.Sprintf("%s:%s:offer:peer_candidates", peerToPeerRedisKeyPrefix, id)
}
func peerToPeerAnswerPeerCandidatesRedisKey(id string) string {
	return fmt.Sprintf("%s:%s:answer:peer_candidates", peerToPeerRedisKeyPrefix, id)
}

func peerToPeerHandshakeAddPeerCandidate(redisKeyFunc func(string) string, w http.ResponseWriter, r *http.Request) {

	pathValues := router.GetPathValues(r)

	sessionID := pathValues[0]

	router.WrapWithInputRequireClient(
		func(peerCandidate json.RawMessage, cs *session.ClientSession) (string, error) {
			ctx := cs.Ctx

			var err error

			bringyour.Redis(ctx, func(client bringyour.RedisClient) {
				pl := client.Pipeline()

				candidatesKey := redisKeyFunc(sessionID)

				pl.RPush(ctx, candidatesKey, []byte(peerCandidate))
				pl.ExpireNX(ctx, candidatesKey, peerToPeerObjectExpire)

				_, err = pl.Exec(ctx)
				if err != nil {
					return
				}
			})

			if err != nil {
				return "", err
			}

			return "", nil

		},
		w,
		r,
	)

}

func PeerToPeerHandshakeOfferAddPeerCandidate(w http.ResponseWriter, r *http.Request) {
	peerToPeerHandshakeAddPeerCandidate(peerToPeerOfferPeerCandidatesRedisKey, w, r)
}

func PeerToPeerHandshakeAnswerAddPeerCandidate(w http.ResponseWriter, r *http.Request) {
	peerToPeerHandshakeAddPeerCandidate(peerToPeerAnswerPeerCandidatesRedisKey, w, r)
}

func peerToPeerHandshakeLongPollNewPeerCandidates(redisKeyFunc func(string) string, w http.ResponseWriter, r *http.Request) {

	fromString := r.URL.Query().Get("from")

	from := uint64(0)
	if fromString != "" {
		var err error
		from, err = strconv.ParseUint(fromString, 10, 64)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	pathValues := router.GetPathValues(r)

	sessionID := pathValues[0]

	router.WrapRequireClient(
		func(cs *session.ClientSession) ([]json.RawMessage, error) {

			ctx, cancel := context.WithTimeout(cs.Ctx, pollTimeout)
			defer cancel()

			var err error

			var vals []json.RawMessage

			bringyour.Redis(ctx, func(client bringyour.RedisClient) {

				candidatesKey := redisKeyFunc(sessionID)

				bo := backoff.NewExponentialBackOff()
				bo.InitialInterval = 50 * time.Millisecond
				bo.MaxInterval = 1 * time.Second
				bo.MaxElapsedTime = pollTimeout

				boWithContext := backoff.WithContext(bo, ctx)

				vals, err = backoff.RetryWithData(
					func() ([]json.RawMessage, error) {

						slCmd := client.LRange(ctx, candidatesKey, int64(from), -1)
						err = slCmd.Err()
						if err != nil {
							return nil, err
						}

						for _, v := range slCmd.Val() {
							vals = append(vals, json.RawMessage(v))
						}

						if len(vals) == 0 {
							return nil, fmt.Errorf("no candidates")
						}

						return vals, nil

					},
					boWithContext,
				)

			})

			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				w.WriteHeader(http.StatusNoContent)
				return nil, nil
			}

			if err != nil {
				return nil, err
			}

			json.NewEncoder(os.Stdout).Encode(vals)

			return vals, nil

		},
		w,
		r,
	)

}

func PeerToPeerHandshakeOfferLongPollNewPeerCandidates(w http.ResponseWriter, r *http.Request) {
	peerToPeerHandshakeLongPollNewPeerCandidates(peerToPeerOfferPeerCandidatesRedisKey, w, r)
}

func PeerToPeerHandshakeAnswerLongPollNewPeerCandidates(w http.ResponseWriter, r *http.Request) {
	peerToPeerHandshakeLongPollNewPeerCandidates(peerToPeerAnswerPeerCandidatesRedisKey, w, r)
}
