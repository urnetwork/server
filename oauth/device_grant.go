package oauth

// RFC 8628's device authorization grant for ur-cli (TEAM-M6-08; D-TEAM-7, R-TEAM-6, §7.8): `ur login --device` and
// `ur admin --device` on a host with no browser, or none that reaches a loopback redirect, get the same one-command
// admin token PKCE's flow gives (urcli.go), with the same code-matched consent and cmd binding, since an agent can
// drive a browser.
//
//	authorize  POST /oauth/device_authorization: client_id ur-cli, scope, resource, dpop_jkt and the command as RFC
//	           9396 authorization_details beside its consent code, all checked as ur-cli's authorization request is
//	           (ValidateUrcliRequest). The answer: a device_code (32 random bytes), a user_code of eight letters from
//	           RFC 8628 §6.1's twenty, the verification_uri on the consent page's origin, expires_in and interval
//	consent    the consent page, signed in, asks POST /oauth/device_consent {user_code} for the command and the
//	           consent code to show side by side, with a receipt of this authorization for this user, as
//	           /oauth/consent answers PKCE's; a sign-in older than UrcliSettings.MaxAuthAge is answered
//	           reauthentication_required. Consent is never remembered
//	approve    POST /oauth/device_approve {user_code, consent_receipt, approve}: approval only with that receipt;
//	           approve false denies
//	poll       POST /oauth/token grant_type=urn:ietf:params:oauth:grant-type:device_code with client_id ur-cli, the
//	           device_code and a DPoP proof by the dpop_jkt key, its ath over the device code: authorization_pending,
//	           slow_down for a poll inside the interval (which then grows by SlowDownStep), access_denied,
//	           expired_token, or once the admin token, the authorization taken so it is redeemed once
//
// Authorizations wait between their steps in Redis (device_grant_store.go; QD-830, §2.8's api-11), the store api
// installs at startup (cli/api/main.go), so the device endpoint serves and the issuer's metadata names it. A process
// with no store answers temporarily_unavailable at each step and names no device endpoint, and ur-cli refuses the
// device grant there, naming why.
//
// Each write is one atomic step of the store: a poll's bookkeeping and the consent page's answer replace only the
// authorization they read (Swap), so neither undoes the other on any api instance, and redemption takes it once
// (Take). A code the store does not hold answers expired_token: once Redis drops a key it cannot tell an expired code
// from a taken or an unknown one, and each of them ends the grant. No device code, user code or token enters a log or
// an error. The verification page itself is the consent page's (mmm), which nothing here builds.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// RFC 8628 §3.4's grant type.
const DeviceCodeGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// RFC 8628 §6.1's user code alphabet: twenty consonants, no vowel to spell a word, none confused with another.
const deviceUserCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

// Tunables of the device grant.
type DeviceGrantSettings struct {
	// How long a device code lives: 10 minutes, a consent receipt's life, long enough to sign in on another device
	// and compare the code.
	CodeDuration time.Duration
	// The poll interval answered, and the least a poll waits: 5 s, RFC 8628 §3.2's default.
	PollInterval time.Duration
	// What a slow_down adds to the interval: 5 s, RFC 8628 §3.5's.
	SlowDownStep time.Duration
	// The user code's letters: 8, RFC 8628 §6.1's example, about 34 bits from twenty letters, against guessing
	// within the code's life.
	UserCodeLength int
	// The device code's random bytes: 32.
	DeviceCodeByteCount int
	// How many user codes a new authorization draws while each one drawn is another live authorization's: 4. Eight
	// places of twenty letters make a second draw rare and a fourth all but impossible. Non-positive draws once.
	UserCodeAttemptCount int
	// How many times the consent page's answer reads and writes an authorization that a poll wrote in between: 4. A
	// poll writes at most once a request, so the second attempt all but always lands. Non-positive attempts once.
	DecisionAttemptCount int
}

// The defaults, a fresh value each call.
func DefaultDeviceGrantSettings() *DeviceGrantSettings {
	return &DeviceGrantSettings{
		CodeDuration:         10 * time.Minute,
		PollInterval:         5 * time.Second,
		SlowDownStep:         5 * time.Second,
		UserCodeLength:       8,
		DeviceCodeByteCount:  32,
		UserCodeAttemptCount: 4,
		DecisionAttemptCount: 4,
	}
}

var (
	// The process installs no store for device authorizations (the file header).
	ErrDeviceGrantUnavailable = errors.New("temporarily_unavailable")
	// RFC 8628 §3.5's poll answers.
	ErrAuthorizationPending = errors.New("authorization_pending")
	ErrSlowDown             = errors.New("slow_down")
	ErrAccessDenied         = errors.New("access_denied")
	ErrExpiredToken         = errors.New("expired_token")
	// A store's refusal of a new authorization whose user code another live authorization holds; the grant draws
	// another code.
	ErrDeviceUserCodeInUse = errors.New("the user code is in use")
)

// One device authorization as it waits between its steps.
type DeviceAuthorization struct {
	// SHA-256 of the device code, its key; the code itself is never stored
	DeviceCodeSha256 [sha256.Size]byte
	// the user code's letters, without the hyphen
	UserCode string
	// the request as ValidateUrcliRequest checked it: client, scope, resource, dpop_jkt and the command's details
	ClientId             string
	Scopes               []string
	Resource             string
	DpopJkt              string
	AuthorizationDetails string
	// the command's digest, the token's cmd
	Cmd        string
	ExpireTime time.Time
	// the interval a poll must keep, grown by each slow_down
	Interval     time.Duration
	LastPollTime time.Time
	// set once the consent page approved or denied it
	Approved bool
	Denied   bool
	// the approving person's user, network and sign-in time
	UserId    server.Id
	NetworkId server.Id
	AuthTime  time.Time

	// what the store answered this authorization from, opaque, which its Swap compares; set by the store's reads and
	// never stored itself
	storeVersion []byte
}

// Where device authorizations wait. Each method is one atomic step, whatever api instance runs it, so a poll and the
// consent page never undo each other's write, and a code is redeemed once.
type DeviceAuthorizationStore interface {
	// Keeps a new authorization until its ExpireTime. A device code in use is refused, and a user code another live
	// authorization holds is refused with ErrDeviceUserCodeInUse, keeping nothing, so a user code names one
	// authorization.
	Put(ctx context.Context, authorization *DeviceAuthorization) error
	// The authorization of a device code's hash, with the version Swap compares, nil for none: never kept, past its
	// life, or taken.
	Get(ctx context.Context, deviceCodeSha256 [sha256.Size]byte) (*DeviceAuthorization, error)
	// The authorization of a user code, nil for none, as Get.
	GetByUserCode(ctx context.Context, userCode string) (*DeviceAuthorization, error)
	// Replaces read, an authorization this store's Get or GetByUserCode answered, with next, keeping its life, only while
	// the store holds read unchanged: false, writing nothing, once another write came between or it is gone. next keeps
	// read's device code, user code and ExpireTime.
	Swap(ctx context.Context, read *DeviceAuthorization, next *DeviceAuthorization) (bool, error)
	// Removes the authorization of a device code's hash and answers it, with its version, nil for none: of any number
	// of takes, at most one is answered it.
	Take(ctx context.Context, deviceCodeSha256 [sha256.Size]byte) (*DeviceAuthorization, error)
}

var deviceAuthorizationStore atomic.Pointer[DeviceAuthorizationStore]

// Installs the process's store for device authorizations; nil removes it, and the device grant answers
// temporarily_unavailable.
func SetDeviceAuthorizationStore(store DeviceAuthorizationStore) {
	if store == nil {
		deviceAuthorizationStore.Store(nil)
		return
	}
	deviceAuthorizationStore.Store(&store)
}

func currentDeviceAuthorizationStore() DeviceAuthorizationStore {
	store := deviceAuthorizationStore.Load()
	if store == nil {
		return nil
	}
	return *store
}

// Where the person enters the user code: the consent page's origin, whose device page shows the command.
func DeviceVerificationUri() string {
	return strings.TrimSuffix(AuthorizationEndpoint(), "/") + "/device"
}

// The device authorization endpoint, under the issuer.
func DeviceAuthorizationEndpoint() string {
	return fmt.Sprintf("%s/oauth/device_authorization", Issuer())
}

// A user code as the person types it, uppercase with no hyphen or space; false for one of other letters or length.
func normalizeDeviceUserCode(userCode string, settings *DeviceGrantSettings) (string, bool) {
	normalized := strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		if 'a' <= r && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}, userCode)
	if len(normalized) != settings.UserCodeLength {
		return "", false
	}
	for _, c := range normalized {
		if !strings.ContainsRune(deviceUserCodeAlphabet, c) {
			return "", false
		}
	}
	return normalized, true
}

// A new user code, uniform over the alphabet by rejection sampling.
func newDeviceUserCode(random io.Reader, settings *DeviceGrantSettings) (string, error) {
	code := make([]byte, 0, settings.UserCodeLength)
	draw := make([]byte, 1)
	// the largest multiple of the alphabet's size a byte holds, so every letter is equally likely
	limit := byte(256 - 256%len(deviceUserCodeAlphabet))
	for len(code) < settings.UserCodeLength {
		if _, err := io.ReadFull(random, draw); err != nil {
			return "", err
		}
		if draw[0] < limit {
			code = append(code, deviceUserCodeAlphabet[int(draw[0])%len(deviceUserCodeAlphabet)])
		}
	}
	return string(code), nil
}

// The user code as shown: two halves of letters, a hyphen between.
func formatDeviceUserCode(userCode string) string {
	half := len(userCode) / 2
	return userCode[:half] + "-" + userCode[half:]
}

// The answer of the device authorization endpoint (RFC 8628 §3.2).
type DeviceAuthorizationResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationUri string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// What the device authorization endpoint received.
type DeviceAuthorizationArgs struct {
	ClientId             string
	Scopes               []string
	Resource             string
	DpopJkt              string
	AuthorizationDetails string
}

// The request a device authorization stands for, as ur-cli's validator and the consent receipt read it: no redirect
// or PKCE, and a state naming the authorization, so a receipt of one admits no other.
func (self *DeviceAuthorization) authorizationRequest() *AuthorizationRequest {
	return &AuthorizationRequest{
		ClientId:             self.ClientId,
		ResponseType:         "device_code",
		Scopes:               self.Scopes,
		State:                "device:" + hex.EncodeToString(self.DeviceCodeSha256[:]),
		Resource:             self.Resource,
		DpopJkt:              self.DpopJkt,
		AuthorizationDetails: self.AuthorizationDetails,
	}
}

// Starts a device authorization (the file header).
func AuthorizeDevice(ctx context.Context, args *DeviceAuthorizationArgs) (*DeviceAuthorizationResponse, error) {
	return authorizeDeviceAt(ctx, args, DefaultDeviceGrantSettings(), rand.Reader, server.NowUtc())
}

func authorizeDeviceAt(ctx context.Context, args *DeviceAuthorizationArgs, settings *DeviceGrantSettings, random io.Reader, now time.Time) (*DeviceAuthorizationResponse, error) {
	store := currentDeviceAuthorizationStore()
	if store == nil {
		return nil, ErrDeviceGrantUnavailable
	}
	urcliAuthorization, err := ValidateUrcliRequest(&AuthorizationRequest{
		ClientId:             args.ClientId,
		Scopes:               args.Scopes,
		Resource:             args.Resource,
		DpopJkt:              args.DpopJkt,
		AuthorizationDetails: args.AuthorizationDetails,
	})
	if err != nil {
		return nil, err
	}
	resource, err := CanonicalResource(args.Resource)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidRequest, err)
	}
	deviceCodeBytes := make([]byte, settings.DeviceCodeByteCount)
	if _, err := io.ReadFull(random, deviceCodeBytes); err != nil {
		return nil, err
	}
	deviceCode := base64.RawURLEncoding.EncodeToString(deviceCodeBytes)
	authorization := &DeviceAuthorization{
		DeviceCodeSha256:     sha256.Sum256([]byte(deviceCode)),
		ClientId:             args.ClientId,
		Scopes:               args.Scopes,
		Resource:             resource,
		DpopJkt:              args.DpopJkt,
		AuthorizationDetails: args.AuthorizationDetails,
		Cmd:                  urcliAuthorization.Cmd,
		ExpireTime:           now.Add(settings.CodeDuration),
		Interval:             settings.PollInterval,
	}
	// a user code another live authorization holds is drawn again, so the code the person types names one
	for attempt := 1; ; attempt += 1 {
		authorization.UserCode, err = newDeviceUserCode(random, settings)
		if err != nil {
			return nil, err
		}
		err = store.Put(ctx, authorization)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrDeviceUserCodeInUse) || settings.UserCodeAttemptCount <= attempt {
			return nil, err
		}
	}
	return &DeviceAuthorizationResponse{
		DeviceCode:      deviceCode,
		UserCode:        formatDeviceUserCode(authorization.UserCode),
		VerificationUri: DeviceVerificationUri(),
		ExpiresIn:       int(settings.CodeDuration / time.Second),
		Interval:        int(settings.PollInterval / time.Second),
	}, nil
}

// The unexpired authorization of a user code the person typed, or why there is none.
func deviceAuthorizationOfUserCode(ctx context.Context, store DeviceAuthorizationStore, userCode string, settings *DeviceGrantSettings, now time.Time) (*DeviceAuthorization, error) {
	normalized, ok := normalizeDeviceUserCode(userCode, settings)
	if !ok {
		return nil, fmt.Errorf("%w: not a user code", ErrInvalidGrant)
	}
	authorization, err := store.GetByUserCode(ctx, normalized)
	if err != nil {
		return nil, err
	}
	if authorization == nil || !now.Before(authorization.ExpireTime) || authorization.Approved || authorization.Denied {
		return nil, fmt.Errorf("%w: no such waiting authorization", ErrInvalidGrant)
	}
	return authorization, nil
}

// Describes a device authorization to the consent page for userId, signed in at authTime: the command and its code
// with a receipt, or ErrUrcliReauthenticationRequired for a sign-in past MaxAuthAge.
func DescribeDeviceConsent(ctx context.Context, userCode string, userId server.Id, authTime time.Time) (*UrcliConsent, error) {
	return describeDeviceConsentAt(ctx, userCode, userId, authTime, DefaultDeviceGrantSettings(), server.NowUtc())
}

func describeDeviceConsentAt(ctx context.Context, userCode string, userId server.Id, authTime time.Time, settings *DeviceGrantSettings, now time.Time) (*UrcliConsent, error) {
	store := currentDeviceAuthorizationStore()
	if store == nil {
		return nil, ErrDeviceGrantUnavailable
	}
	authorization, err := deviceAuthorizationOfUserCode(ctx, store, userCode, settings, now)
	if err != nil {
		return nil, err
	}
	return DescribeUrcliConsent(authorization.authorizationRequest(), userId, authTime, now)
}

// The person's answer to a device authorization: approval with the receipt the consent page was given, a fresh
// sign-in and a full network; or a denial.
func DecideDevice(ctx context.Context, userCode string, approve bool, receipt string, user *AuthorizingUser) error {
	return decideDeviceAt(ctx, userCode, approve, receipt, user, DefaultDeviceGrantSettings(), server.NowUtc)
}

// The answer replaces only the authorization it read, so a poll's bookkeeping written in between is read again and
// the answer applied to it, never lost under the poll's write nor the poll's under it.
func decideDeviceAt(ctx context.Context, userCode string, approve bool, receipt string, user *AuthorizingUser, settings *DeviceGrantSettings, now func() time.Time) error {
	store := currentDeviceAuthorizationStore()
	if store == nil {
		return ErrDeviceGrantUnavailable
	}
	for attempt := 1; ; attempt += 1 {
		authorization, err := deviceAuthorizationOfUserCode(ctx, store, userCode, settings, now())
		if err != nil {
			return err
		}
		decided := *authorization
		if approve {
			if user.GuestMode {
				return errUnauthorizedGuestNetwork
			}
			if user.NetworkId == (server.Id{}) {
				return errUnauthorizedNoNetwork
			}
			if user.AuthTime.IsZero() || DefaultUrcliSettings().MaxAuthAge < now().Sub(user.AuthTime) {
				return ErrUrcliReauthenticationRequired
			}
			if err := verifyUrcliConsentReceipt(receipt, user.UserId, authorization.authorizationRequest(), now); err != nil {
				return err
			}
			decided.Approved = true
			decided.UserId = user.UserId
			decided.NetworkId = user.NetworkId
			decided.AuthTime = user.AuthTime
		} else {
			decided.Denied = true
		}
		swapped, err := store.Swap(ctx, authorization, &decided)
		if err != nil {
			return err
		}
		if swapped {
			return nil
		}
		if settings.DecisionAttemptCount <= attempt {
			return fmt.Errorf("%w: the authorization kept changing under the answer", ErrDeviceGrantUnavailable)
		}
	}
}

// What a device code poll sent.
type ExchangeDeviceCodeArgs struct {
	ClientId   string
	DeviceCode string
	Resource   string
}

// One poll of a device code (the file header): the admin token once approved, else RFC 8628's answer.
func ExchangeDeviceCode(ctx context.Context, args *ExchangeDeviceCodeArgs, proofs []string) (*TokenResponse, error) {
	return exchangeDeviceCodeAt(ctx, args, proofs, DefaultDeviceGrantSettings(), server.NowUtc())
}

func exchangeDeviceCodeAt(ctx context.Context, args *ExchangeDeviceCodeArgs, proofs []string, settings *DeviceGrantSettings, now time.Time) (*TokenResponse, error) {
	store := currentDeviceAuthorizationStore()
	if store == nil {
		return nil, ErrDeviceGrantUnavailable
	}
	if args.ClientId != UrcliClientId {
		return nil, fmt.Errorf("%w: the device grant is ur-cli's", ErrInvalidRequest)
	}
	deviceCodeSha256 := sha256.Sum256([]byte(args.DeviceCode))
	authorization, err := store.Get(ctx, deviceCodeSha256)
	if err != nil {
		return nil, err
	}
	// expired, taken or never issued: the store cannot tell which once its key is gone (the file header)
	if authorization == nil {
		return nil, ErrExpiredToken
	}
	if !now.Before(authorization.ExpireTime) {
		// a store that keeps a code past its life gives it up here, and the answer stands whatever the take answers
		store.Take(ctx, deviceCodeSha256)
		return nil, ErrExpiredToken
	}
	if authorization.Denied {
		if _, err := store.Take(ctx, deviceCodeSha256); err != nil {
			return nil, err
		}
		return nil, ErrAccessDenied
	}
	if !authorization.Approved {
		tooSoon := !authorization.LastPollTime.IsZero() && now.Sub(authorization.LastPollTime) < authorization.Interval
		polled := *authorization
		polled.LastPollTime = now
		if tooSoon {
			polled.Interval += settings.SlowDownStep
		}
		swapped, err := store.Swap(ctx, authorization, &polled)
		if err != nil {
			return nil, err
		}
		// a poll that raced another write, the consent page's answer or a second poll, slows down and records
		// nothing: the next poll reads what that write left
		if tooSoon || !swapped {
			return nil, ErrSlowDown
		}
		return nil, ErrAuthorizationPending
	}
	resource, err := CanonicalResource(args.Resource)
	if err != nil || resource != authorization.Resource {
		return nil, fmt.Errorf("%w: the resource is not the authorization's", ErrInvalidGrant)
	}
	if len(proofs) != 1 {
		return nil, fmt.Errorf("%w: the request does not carry exactly one proof", ErrInvalidDpopProof)
	}
	// the proof first, so a poll with a bad one spends nothing
	if err := session.VerifyDpopProof(ctx, &session.DpopRequest{
		Proof:         proofs[0],
		Method:        "POST",
		Uri:           TokenEndpoint(),
		Token:         args.DeviceCode,
		KeyThumbprint: authorization.DpopJkt,
		ReplayId:      authorization.UserId,
	}); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidDpopProof, err)
	}
	taken, err := store.Take(ctx, deviceCodeSha256)
	if err != nil {
		return nil, err
	}
	// of any number of polls one takes the code, and only as the approved authorization the proof was checked against
	if taken == nil || !taken.Approved || !bytes.Equal(taken.storeVersion, authorization.storeVersion) {
		return nil, fmt.Errorf("%w: the device code was redeemed", ErrInvalidGrant)
	}
	memberId, err := urcliTokenMemberId(ctx, taken.NetworkId, taken.UserId)
	if err != nil {
		return nil, err
	}
	urcliSettings := DefaultUrcliSettings()
	token, expiry, err := MintAdminToken(&MintAdminTokenArgs{
		NetworkId: taken.NetworkId,
		MemberId:  memberId,
		Scopes:    taken.Scopes,
		Jkt:       taken.DpopJkt,
		Cmd:       taken.Cmd,
		Duration:  urcliSettings.AdminTokenDuration,
	})
	if err != nil {
		return nil, err
	}
	return &TokenResponse{
		AccessToken: token,
		TokenType:   adminTokenResponseType,
		ExpiresIn:   int(time.Until(expiry).Seconds()),
		Scope:       FormatScope(taken.Scopes),
	}, nil
}
