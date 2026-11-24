package handlers

// The SN25 validator rate class (owner decision 2026-11-24). A validator whose
// activation the reserved upload admission admits mints a short-lived class
// token here, and sends it beside its bearer on every api request; the lb
// front charges those requests to the validator rate class
// (nginx.LimitIdentity.Validator). This route itself runs under the normal
// identity limits.

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// A minted class token and its expiry.
type SnValidatorClassResult struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

var snValidatorClassMints = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "sn",
	Name:      "validator_class_mints_total",
	Help:      "SN25 validator rate class tokens asked for at POST /sn/validator-class by an authenticated client, by outcome: minted, refused_403 (the admission refused the validator, or the request), refused_503 (this instance's admission is not current, or the token could not be minted), refused_other (any other refusal)",
}, []string{"outcome"})

func init() {
	prometheus.MustRegister(snValidatorClassMints)
}

// The production route over the API lifecycle's reserved upload lane.
func SnValidatorClassWithReserved(reserved SnReservedAttemptUploadLane) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveSnValidatorClass(w, r, reserved, session.MintValidatorClass, time.Now)
	}
}

// The outcome label of a mint's answer, from the status its error declares.
func snValidatorClassOutcome(err error) string {
	if err == nil {
		return "minted"
	}
	message := err.Error()
	if len(message) < 4 || message[3] != ' ' {
		return "refused_other"
	}
	switch status, _ := strconv.Atoi(message[:3]); status {
	case http.StatusForbidden:
		return "refused_403"
	case http.StatusServiceUnavailable:
		return "refused_503"
	default:
		return "refused_other"
	}
}

// Dependencies are per call, so tests replace the minting and the clock.
func serveSnValidatorClass(w http.ResponseWriter, r *http.Request, reservedLane SnReservedAttemptUploadLane, mint func(session.ValidatorClassClaims) (string, error), now func() time.Time) {
	router.WrapRequireClient(router.PrivateErrors(func(clientSession *session.ClientSession) (result *SnValidatorClassResult, resultErr error) {
		defer clientSession.Cancel()
		defer func() {
			outcome := snValidatorClassOutcome(resultErr)
			// neither a result nor an error: the impl panicked
			if result == nil && resultErr == nil {
				outcome = "refused_other"
			}
			snValidatorClassMints.WithLabelValues(outcome).Inc()
		}()
		ctx, cancel := context.WithTimeout(clientSession.Ctx, 10*time.Second)
		defer cancel()
		byJwt := clientSession.ByJwt
		if byJwt == nil || byJwt.NetworkId == (server.Id{}) || byJwt.ClientId == nil || *byJwt.ClientId == (server.Id{}) {
			return nil, errors.New("401 Client authentication required.")
		}
		// The wrapper authenticated this exact bearer; its bytes are the
		// session the consent binds.
		authorizations := r.Header.Values("Authorization")
		if len(authorizations) != 1 || !strings.HasPrefix(authorizations[0], "Bearer ") {
			return nil, errors.New("401 Exact bearer authentication required.")
		}
		intents := r.Header.Values(protocol.ValidatorClassIntentHeader)
		if len(intents) != 1 {
			return nil, errors.New("400 One validator class consent is required.")
		}
		var reserved *controller.StReservedAttemptUpload
		starting := false
		if reservedLane != nil {
			reserved, starting = reservedLane.Admission()
		}
		// As the upload route: a lane not running here says nothing about the
		// validator, so the lb tries a sibling.
		if reserved == nil && starting {
			return nil, errors.New("503 Validator reserved upload is not running on this instance.")
		}
		if reserved == nil {
			return nil, errors.New("403 Validator reserved upload is unavailable or ambiguous.")
		}
		class, err := reserved.Class(ctx, strings.TrimPrefix(authorizations[0], "Bearer "), intents[0])
		if err != nil {
			return nil, err
		}
		issued := now()
		expires := issued.Add(session.ValidatorClassMaximumLifetime)
		if class.ValidUntil.Before(expires) {
			expires = class.ValidUntil
		}
		// whole seconds, never past the admission's expiry
		expires = time.Unix(expires.Unix(), 0)
		if !issued.Before(expires) {
			return nil, errors.New("503 Validator staging authority is not current on this instance.")
		}
		token, err := mint(session.ValidatorClassClaims{
			ClientId:   *byJwt.ClientId,
			NetworkId:  byJwt.NetworkId,
			Hotkey:     hex.EncodeToString(class.Owner.Hotkey[:]),
			Activation: hex.EncodeToString(class.Activation[:]),
			IssuedAt:   issued.Unix(),
			ExpiresAt:  expires.Unix(),
		})
		if err != nil {
			return nil, router.PrivateCause(http.StatusServiceUnavailable, "Validator class unavailable.", err)
		}
		return &SnValidatorClassResult{Token: token, ExpiresAt: expires.UTC()}, nil
	}), w, r)
}
