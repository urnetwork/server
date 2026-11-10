package controller

// POST /network/enroll (§7.8, TEAM-M1-04): Public, its credential the auth key
// the body carries. Limited per caller address and per auth key before the
// mint, each counted in the shared IP rate-limit histories by the address's
// hash, never the address in the clear (I-13), and, for the key, by its id. A
// caller whose address cannot be read is limited by its key alone.
//
// Nothing here logs or answers the body's auth key: a refusal carries a fixed
// message and code (model.NetworkEnrollRefusal), and the router logs a body's
// type, never its value (D-TEAM-22).

import (
	"fmt"
	"net/http"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Tunables of enrollment's limits.
type NetworkEnrollSettings struct {
	// One caller address's enrollments in AddressLookback, whatever the keys:
	// 120 in 10 min, an office or a CI runner pool behind one address.
	// Non-positive limits nothing.
	AddressLimit    int
	AddressLookback time.Duration
	// One auth key's enrollments in KeyLookback, whatever the addresses: 600 in
	// 10 min, a reusable CI key starting a fleet. Non-positive limits nothing.
	KeyLimit    int
	KeyLookback time.Duration
}

func DefaultNetworkEnrollSettings() *NetworkEnrollSettings {
	return &NetworkEnrollSettings{
		AddressLimit:    120,
		AddressLookback: 10 * time.Minute,
		KeyLimit:        600,
		KeyLookback:     10 * time.Minute,
	}
}

// The prefix of enrollment's IP rate-limit histories.
const networkEnrollRateLimitKeyPrefix = "network_enroll."

func networkEnrollRateLimited(lookback time.Duration) *model.NetworkEnrollRefusal {
	return &model.NetworkEnrollRefusal{
		Status:     http.StatusTooManyRequests,
		Code:       "rate_limited",
		Message:    "Too many enrollments. Try again later.",
		RetryAfter: int(lookback / time.Second),
	}
}

// Counts one enrollment against the caller's address and against the key, and
// refuses past either limit.
func limitNetworkEnroll(clientSession *session.ClientSession, authKeyId *server.Id, settings *NetworkEnrollSettings) error {
	rateLimitClient, err := server.NewRateLimitClient(clientSession.ClientAddress)
	if err != nil {
		clientAddressHash, _, err := clientSession.ClientAddressHashPort()
		if err == nil {
			rateLimitClient = server.NewStoredRateLimitClient(clientAddressHash)
		} else {
			rateLimitClient = nil
		}
	}
	now := server.NowUtc()
	if rateLimitClient != nil && 0 < settings.AddressLimit {
		_, allowed, err := server.CheckIpRateLimitAttempt(
			clientSession.Ctx,
			rateLimitClient,
			"",
			now,
			server.IpRateLimitAttemptSettings{
				KeyPrefix:       networkEnrollRateLimitKeyPrefix,
				AddressLookback: settings.AddressLookback,
				// the history holds this attempt and refuses at its limit
				AddressLimit:   settings.AddressLimit + 1,
				GlobalLookback: settings.AddressLookback,
				GlobalLimit:    settings.AddressLimit + 1,
			},
		)
		if err != nil {
			return fmt.Errorf("enroll rate limit: %w", err)
		}
		if !allowed {
			return networkEnrollRateLimited(settings.AddressLookback)
		}
	}
	if authKeyId != nil && 0 < settings.KeyLimit {
		keyClient := rateLimitClient
		if keyClient == nil {
			keyClient = server.NewStoredRateLimitClient([32]byte{})
		}
		_, allowed, err := server.CheckIpRateLimitAttempt(
			clientSession.Ctx,
			keyClient,
			"enroll_key_"+authKeyId.String(),
			now,
			server.IpRateLimitAttemptSettings{
				KeyPrefix:       networkEnrollRateLimitKeyPrefix,
				AddressLookback: settings.KeyLookback,
				AddressLimit:    settings.KeyLimit + 1,
				GlobalLookback:  settings.KeyLookback,
				GlobalLimit:     settings.KeyLimit + 1,
			},
		)
		if err != nil {
			return fmt.Errorf("enroll rate limit: %w", err)
		}
		if !allowed {
			return networkEnrollRateLimited(settings.KeyLookback)
		}
	}
	return nil
}

// POST /network/enroll: the limits, then the mint (model.EnrollNetworkClient).
func NetworkEnroll(enroll *model.NetworkEnrollArgs, clientSession *session.ClientSession) (*model.NetworkEnrollResult, error) {
	return networkEnrollWithSettings(enroll, clientSession, DefaultNetworkEnrollSettings())
}

func networkEnrollWithSettings(enroll *model.NetworkEnrollArgs, clientSession *session.ClientSession, settings *NetworkEnrollSettings) (*model.NetworkEnrollResult, error) {
	// a key that does not parse has no id to count, and the mint refuses it
	var authKeyId *server.Id
	if parsedId, _, err := model.ParseAuthKeyBearer(enroll.AuthKey); err == nil {
		authKeyId = &parsedId
	}
	if err := limitNetworkEnroll(clientSession, authKeyId, settings); err != nil {
		return nil, err
	}
	return model.EnrollNetworkClient(clientSession.Ctx, enroll, clientSession.ClientAddress)
}
