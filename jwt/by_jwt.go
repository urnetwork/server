package jwt

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// see https://github.com/golang-jwt/jwt
// see https://golang-jwt.github.io/jwt/usage/create/

var byJwtTlsKeyPaths = sync.OnceValue(func() []string {
	jwt := server.Vault.RequireSimpleResource("jwt.yml")
	return jwt.RequireStringList("tls_key_paths")
})

const (
	// Reverted from 24h to 30 days by team decision.
	//
	// The one-day value bounded stale authorization for a client that stays
	// online indefinitely, which is a real thing to want. It rested on
	// "conforming clients rotate at half-life" -- and that is true of the sdk
	// only since the half-life refresh landed on 2026-08-06. It is not true of
	// anything else holding a token: an app build older than that, or any client
	// issued a token it does not refresh.
	//
	// Those clients do not fail loudly at the deadline. They stay connected and
	// keep accepting connections while carrying nothing, so they look healthy
	// from every angle except an egress probe. Measured on beta: 95% probe
	// success under 12h of client age, 14% at 20-24h, and 0% past 24h across 270
	// attempts -- a fleet-wide blackhole on a 24h clock, invisible to everything
	// that was not probing egress.
	//
	// 30 days does not fix the non-refreshing client, it only makes the deadline
	// rare enough to notice. The durable protections are elsewhere and stay in
	// place: the sdk's half-life rotation, and the server refusing to advertise
	// a provider whose egress health has aged out (ProviderEgressHealthMaxAge).
	// Shortening this again should wait until a client that ignores the deadline
	// is the exception rather than the rule.
	expiryDuration = 30 * 24 * time.Hour
	clockLeeway    = 30 * time.Second

	ByJwtIssuer          = "urnetwork:byjwt"
	ByJwtAudienceApi     = "urnetwork:api"
	ByJwtAudienceConnect = "urnetwork:connect"
)

// the first key (most recent version) is used to sign new JWTs
var byPrivateKeys = sync.OnceValue(func() []crypto.PrivateKey {
	keys := []crypto.PrivateKey{}
	glog.Infof("[jwt]paths: %s", byJwtTlsKeyPaths())
	errs := []error{}
	for _, jwtTlsKeyPath := range byJwtTlsKeyPaths() {
		// `ResourcePaths` returns the version paths in descending order
		// hence the `paths[0]` will be the most recent version
		paths, err := server.Vault.ResourcePaths(jwtTlsKeyPath)
		if err != nil {
			errs = append(errs, err)
		} else {
			for _, path := range paths {
				bytes, err := os.ReadFile(path)
				if err != nil {
					panic(err)
				}
				block, _ := pem.Decode(bytes)

				keyPathErrs := []error{}
				if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
					glog.Errorf("[jwt]loaded ec key \"%s\"\n", path)
					keys = append(keys, key)
				} else {
					if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
						glog.Errorf("[jwt]loaded pkcs8 key \"%s\"\n", path)
						keys = append(keys, key)
					} else {
						keyPathErrs = append(keyPathErrs, err)
						if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
							glog.Errorf("[jwt]loaded pkcs1 key \"%s\"\n", path)
							keys = append(keys, key)
						} else {
							keyPathErrs = append(keyPathErrs, err)
							err = errors.Join(keyPathErrs...)
							glog.Errorf("[jwt]could not load key \"%s\". err = %s\n", path, err)
							errs = append(errs, err)
						}
					}
				}
			}
		}
	}
	if len(keys) == 0 {
		panic(errors.Join(errs...))
	}
	return keys
})

func byRsaSigningKey() *rsa.PrivateKey {
	for _, key := range byPrivateKeys() {
		switch v := key.(type) {
		case *rsa.PrivateKey:
			return v
		}
	}
	return nil
}

func byEcdsaSigningKey() *ecdsa.PrivateKey {
	for _, key := range byPrivateKeys() {
		switch v := key.(type) {
		case *ecdsa.PrivateKey:
			return v
		}
	}
	return nil
}

// jwtKid is a stable key id for a public key: the base64url (no padding) SHA-256
// of its PKIX DER encoding. The signer and verifier derive the same id from the
// same key, so it is used as the JOSE `kid` header to select the exact
// verification key instead of trying every key.
func jwtKid(publicKey crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// publicKey returns the public key for a loaded private key.
func publicKey(byPrivateKey crypto.PrivateKey) crypto.PublicKey {
	return byPrivateKey.(interface{ Public() crypto.PublicKey }).Public()
}

// byPublicKeysByKid maps each loaded key's kid to its public key, for O(1)
// verification-key lookup from a token's `kid` header.
var byPublicKeysByKid = sync.OnceValue(func() map[string]crypto.PublicKey {
	publicKeysByKid := map[string]crypto.PublicKey{}
	for _, byPrivateKey := range byPrivateKeys() {
		kid, err := jwtKid(publicKey(byPrivateKey))
		if err != nil {
			glog.Errorf("[jwt]could not compute kid: %s\n", err)
			continue
		}
		publicKeysByKid[kid] = publicKey(byPrivateKey)
	}
	return publicKeysByKid
})

// byPublicKeys is the full set of verification keys, newest first (same order as
// byPrivateKeys). It is the fallback when a token has no recognized `kid`.
var byPublicKeys = sync.OnceValue(func() []gojwt.VerificationKey {
	publicKeys := []gojwt.VerificationKey{}
	for _, byPrivateKey := range byPrivateKeys() {
		publicKeys = append(publicKeys, publicKey(byPrivateKey))
	}
	return publicKeys
})

// the bringyour authorization model is:
// Network
//
//	User
//	  Client
//
// Trust verification happens at the user level.
// A client is always tied to a user.
type ByJwt struct {
	NetworkId   server.Id  `json:"network_id,omitempty"`
	NetworkName string     `json:"network_name,omitempty"`
	UserId      server.Id  `json:"user_id,omitempty"`
	CreateTime  time.Time  `json:"create_time,omitempty"`
	DeviceId    *server.Id `json:"device_id,omitempty"`
	ClientId    *server.Id `json:"client_id,omitempty"`
	// Deprecated: always false for new tokens. Field kept for backward compat with existing guest JWTs.
	GuestMode bool `json:"guest_mode,omitempty"`
	Pro       bool `json:"pro,omitempty"`
	// identity roles and principal, assigned at client or auth code creation.
	// The values have no meaning to the network.
	Roles     []string `json:"roles,omitempty"`
	Principal string   `json:"principal,omitempty"`
	gojwt.RegisteredClaims
}

func newRegisteredClaims(userId server.Id) gojwt.RegisteredClaims {
	now := server.NowUtc()
	return gojwt.RegisteredClaims{
		Issuer:    ByJwtIssuer,
		Subject:   userId.String(),
		Audience:  gojwt.ClaimStrings{ByJwtAudienceApi, ByJwtAudienceConnect},
		ExpiresAt: gojwt.NewNumericDate(now.Add(expiryDuration)),
		NotBefore: gojwt.NewNumericDate(now),
		IssuedAt:  gojwt.NewNumericDate(now),
		ID:        server.NewId().String(),
	}
}

func NewByJwt(
	networkId server.Id,
	userId server.Id,
	networkName string,
	guestMode bool,
	pro bool,
) *ByJwt {
	if networkId == (server.Id{}) {
		panic(fmt.Errorf("network_id must be set"))
	}
	if userId == (server.Id{}) {
		panic(fmt.Errorf("user_id must be set"))
	}

	// glog.Infof("Creating ByJwt for network_id=%s, user_id=%s, guest_mode=%v, pro=%v", networkId, userId, guestMode, pro)

	return NewByJwtWithCreateTime(
		networkId,
		userId,
		networkName,
		server.NowUtc(),
		guestMode,
		pro,
	)
}

func NewByJwtWithCreateTime(
	networkId server.Id,
	userId server.Id,
	networkName string,
	createTime time.Time,
	guestMode bool,
	pro bool,
) *ByJwt {
	if networkId == (server.Id{}) {
		panic(fmt.Errorf("network_id must be set"))
	}
	if userId == (server.Id{}) {
		panic(fmt.Errorf("user_id must be set"))
	}

	return &ByJwt{
		NetworkId:   networkId,
		UserId:      userId,
		NetworkName: networkName,
		GuestMode:   guestMode,
		Pro:         pro,
		// round here so that the string representation in the jwt does not lose information
		CreateTime:       server.CodecTime(createTime),
		RegisteredClaims: newRegisteredClaims(userId),
	}
}

// rejectMissingExpiration gates the registered-claims hardening. Tokens
// minted before the hardening carry none of the registered claims
// (exp/iss/aud/sub/jti/nbf/iat), so rejecting on absence cuts off every
// credential issued before the cutover at once. Until vault auth.yml sets
// reject_missing_expiration: true, absent claims are tolerated and only the
// claims present on a token are validated, giving the fleet time to
// re-issue credentials through normal refresh. The vault value is read once
// per process; changing it requires a restart.
var rejectMissingExpirationOverride atomic.Pointer[bool]

var vaultRejectMissingExpiration = sync.OnceValue(loadRejectMissingExpiration)

func loadRejectMissingExpiration() bool {
	return loadAuthSettingBool("reject_missing_expiration")
}

func rejectMissingExpiration() bool {
	if value := rejectMissingExpirationOverride.Load(); value != nil {
		return *value
	}
	return vaultRejectMissingExpiration()
}

// Testing_SetRejectMissingExpiration forces the gate for tests, bypassing the
// vault setting. The returned pop restores the previous override.
func Testing_SetRejectMissingExpiration(value bool) func() {
	previous := rejectMissingExpirationOverride.Swap(&value)
	return func() {
		rejectMissingExpirationOverride.Store(previous)
	}
}

// rejectExpired gates enforcement of a token's expiration when one is
// present. Pre-hardening deploys never checked expiry, so clients hold
// credentials whose exp passed long ago and refresh on their own schedule.
// Until vault auth.yml sets reject_expired: true, a passed exp is tolerated
// and logged, giving those clients time to refresh. Independent of
// reject_missing_expiration: that gate decides whether exp must be present,
// this one decides whether a passed exp is fatal.
var rejectExpiredOverride atomic.Pointer[bool]

var vaultRejectExpired = sync.OnceValue(loadRejectExpired)

func loadRejectExpired() bool {
	return loadAuthSettingBool("reject_expired")
}

func rejectExpired() bool {
	if value := rejectExpiredOverride.Load(); value != nil {
		return *value
	}
	return vaultRejectExpired()
}

// Testing_SetRejectExpired forces the reject_expired gate for tests,
// bypassing the vault setting. The returned pop restores the previous
// override.
func Testing_SetRejectExpired(value bool) func() {
	previous := rejectExpiredOverride.Swap(&value)
	return func() {
		rejectExpiredOverride.Store(previous)
	}
}

// loadAuthSettingBool reads a bool key from vault auth.yml. A missing file or
// key means false.
func loadAuthSettingBool(key string) bool {
	authResource, err := server.Vault.SimpleResource("auth.yml")
	if err != nil {
		return false
	}
	if values := authResource.Bool(key); len(values) == 1 {
		return values[0]
	}
	return false
}

// AuthRejectionCause is the bounded reason a credential was refused. A
// rejection is driven entirely by client-supplied input, so it is counted
// rather than logged: a per-occurrence log line at default verbosity hands
// any client — or any retry loop — a way to spam the logs. The counter is
// the lossless rate signal; the matching detail line is emitted at V(1) for
// diagnosis, where the operator opts into the volume.
type AuthRejectionCause string

const (
	AuthRejectionSignature          AuthRejectionCause = "signature"
	AuthRejectionExpired            AuthRejectionCause = "expired"
	AuthRejectionNotYetValid        AuthRejectionCause = "not_yet_valid"
	AuthRejectionUsedBeforeIssued   AuthRejectionCause = "used_before_issued"
	AuthRejectionMissingClaims      AuthRejectionCause = "missing_claims"
	AuthRejectionInvalidClaims      AuthRejectionCause = "invalid_claims"
	AuthRejectionUnresolvedIdentity AuthRejectionCause = "unresolved_identity"
	AuthRejectionMissingToken       AuthRejectionCause = "missing_token"
	AuthRejectionClientRequired     AuthRejectionCause = "client_required"
	AuthRejectionNoActiveRow        AuthRejectionCause = "no_active_row"
	AuthRejectionCredentialRotated  AuthRejectionCause = "credential_rotated"
)

// AuthLegacyAcceptCause is the bounded reason a credential was accepted only
// because a migration gate is off. Each counter going to zero is the signal
// that the matching auth.yml gate can be flipped on.
type AuthLegacyAcceptCause string

const (
	AuthLegacyMissingExpiration AuthLegacyAcceptCause = "missing_expiration"
	AuthLegacyExpired           AuthLegacyAcceptCause = "expired"
)

var authRejectionCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "auth",
		Name:      "jwt_rejections_total",
		Help:      "Credential rejections partitioned by a bounded cause class",
	},
	[]string{"cause"},
)

var authLegacyAcceptCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "auth",
		Name:      "jwt_legacy_accepts_total",
		Help:      "Credentials accepted only because an auth.yml migration gate is off, by cause",
	},
	[]string{"cause"},
)

func init() {
	prometheus.MustRegister(authRejectionCounter, authLegacyAcceptCounter)
}

// rejectByJwt counts a rejection, emits its detail at V(1), and returns the
// opaque caller-facing error. The detail never reaches the default log level:
// see AuthRejectionCause.
func rejectByJwt(cause AuthRejectionCause, detailFormat string, args ...any) error {
	authRejectionCounter.WithLabelValues(string(cause)).Inc()
	if glog.V(1) {
		glog.Infof("[jwt]reject %s: %s\n", cause, fmt.Sprintf(detailFormat, args...))
	}
	return errors.New("Could not verify signed token.")
}

// rejectByJwtClaims is rejectByJwt for the claims-shape failures, which
// return a distinct caller-facing message.
func rejectByJwtClaims(cause AuthRejectionCause, detailFormat string, args ...any) error {
	authRejectionCounter.WithLabelValues(string(cause)).Inc()
	if glog.V(1) {
		glog.Infof("[jwt]reject %s: %s\n", cause, fmt.Sprintf(detailFormat, args...))
	}
	return errors.New("Invalid signed token claims.")
}

func ParseByJwt(ctx context.Context, jwtSigned string) (*ByJwt, error) {
	return ParseByJwtForAudience(ctx, jwtSigned, ByJwtAudienceApi)
}

// ParseByJwtForAudience verifies the signature and the registered lifetime
// and identity claims. API and connect use distinct expected audiences even
// though current client credentials are deliberately minted for both.
//
// Tokens minted before the claims hardening carry no registered claims.
// While the auth.yml reject_missing_expiration gate is off, absent claims
// are tolerated and only the claims present on a token are validated; while
// the reject_expired gate is off, a present-but-passed expiration is
// tolerated too. A wrong claim value (issuer, audience, subject, not-before)
// is rejected in every mode, and the signature is always enforced.
//
// The claims policy is enforced manually below rather than with parser
// options because gojwt validates exp whenever it is present, with no option
// to tolerate it; the parser layer checks decoding, the signature, and the
// signing method only.
func ParseByJwtForAudience(ctx context.Context, jwtSigned string, audience string) (*ByJwt, error) {
	if audience == "" {
		return nil, errors.New("Missing JWT audience.")
	}
	rejectMissing := rejectMissingExpiration()
	parserOptions := []gojwt.ParserOption{
		gojwt.WithValidMethods([]string{"ES256", "ES384", "ES512", "RS512"}),
		gojwt.WithoutClaimsValidation(),
	}

	// select the verification key by the token's `kid` header when present and
	// recognized; otherwise fall back to trying every key. kid is only an
	// optimization, so an absent or unknown kid never changes which tokens
	// verify. parse directly into the ByJwt struct (which is a gojwt.Claims via
	// its embedded RegisteredClaims) to avoid a MapClaims map plus a json
	// marshal/unmarshal round-trip on every request.
	keyFunc := func(token *gojwt.Token) (any, error) {
		if kid, ok := token.Header["kid"].(string); ok {
			if matchedPublicKey, ok := byPublicKeysByKid()[kid]; ok {
				return matchedPublicKey, nil
			}
		}
		return gojwt.VerificationKeySet{Keys: byPublicKeys()}, nil
	}

	byJwt := &ByJwt{}
	_, err := gojwt.ParseWithClaims(jwtSigned, byJwt, keyFunc, parserOptions...)
	if err != nil {
		// the caller-facing error stays opaque; the reason (malformed token
		// vs bad signature) is only visible server-side
		return nil, rejectByJwt(AuthRejectionSignature, "parse err = %v", err)
	}

	// lifetime claims, validated whenever present with gojwt's comparison and
	// leeway semantics. The gates decide whether an absent exp
	// (reject_missing_expiration) or a passed exp (reject_expired) is fatal;
	// not-before and issued-at violations are fatal in every mode.
	now := server.NowUtc()
	if byJwt.ExpiresAt == nil {
		if rejectMissing {
			return nil, rejectByJwtClaims(AuthRejectionMissingClaims, "missing expiration")
		}
		// gauge of remaining legacy-credential traffic, for deciding when to
		// flip reject_missing_expiration on
		authLegacyAcceptCounter.WithLabelValues(string(AuthLegacyMissingExpiration)).Inc()
	} else if now.After(byJwt.ExpiresAt.Time.Add(clockLeeway)) {
		if rejectExpired() {
			return nil, rejectByJwt(AuthRejectionExpired, "token is expired")
		}
		// gauge of stale-credential traffic, for deciding when to flip
		// reject_expired on
		authLegacyAcceptCounter.WithLabelValues(string(AuthLegacyExpired)).Inc()
	}
	if byJwt.NotBefore != nil && now.Before(byJwt.NotBefore.Time.Add(-clockLeeway)) {
		return nil, rejectByJwt(AuthRejectionNotYetValid, "token is not valid yet")
	}
	if byJwt.IssuedAt != nil && now.Before(byJwt.IssuedAt.Time.Add(-clockLeeway)) {
		return nil, rejectByJwt(AuthRejectionUsedBeforeIssued, "token used before issued")
	}

	// identity claims
	if rejectMissing {
		if byJwt.Issuer != ByJwtIssuer ||
			!slices.Contains(byJwt.Audience, audience) ||
			byJwt.Subject == "" || byJwt.Subject != byJwt.UserId.String() ||
			byJwt.IssuedAt == nil || byJwt.NotBefore == nil ||
			byJwt.ID == "" || byJwt.CreateTime.IsZero() {
			return nil, rejectByJwtClaims(AuthRejectionMissingClaims, "incomplete registered claims")
		}
	} else {
		// legacy tokens predate all identity claims, so each is validated
		// only when present
		if byJwt.Issuer != "" && byJwt.Issuer != ByJwtIssuer {
			return nil, rejectByJwtClaims(AuthRejectionInvalidClaims, "issuer mismatch")
		}
		if 0 < len(byJwt.Audience) && !slices.Contains(byJwt.Audience, audience) {
			return nil, rejectByJwtClaims(AuthRejectionInvalidClaims, "audience mismatch")
		}
		if byJwt.Subject != "" && byJwt.Subject != byJwt.UserId.String() {
			return nil, rejectByJwtClaims(AuthRejectionInvalidClaims, "subject mismatch")
		}
	}

	err = fixByJwt(ctx, byJwt)
	if err != nil {
		authRejectionCounter.WithLabelValues(string(AuthRejectionUnresolvedIdentity)).Inc()
		return nil, err
	}

	return byJwt, nil
}

// ValidateByJwtState binds a cryptographically valid token to current account
// and client state. Password resets invalidate older credentials through
// credential_change_time, and removed clients stop working immediately.
func ValidateByJwtState(ctx context.Context, byJwt *ByJwt, requireClient bool) (returnErr error) {
	if byJwt == nil {
		authRejectionCounter.WithLabelValues(string(AuthRejectionMissingToken)).Inc()
		return errors.New("Missing signed token.")
	}
	if requireClient && (byJwt.ClientId == nil || byJwt.DeviceId == nil) {
		authRejectionCounter.WithLabelValues(string(AuthRejectionClientRequired)).Inc()
		return errors.New("Client credential required.")
	}

	valid := false
	var credentialChangeTime time.Time
	server.Db(ctx, func(conn server.PgConn) {
		if byJwt.ClientId == nil {
			result, err := conn.Query(ctx, `
				SELECT network_user.credential_change_time
				FROM network_user
				INNER JOIN network ON
					network.admin_user_id = network_user.user_id AND
					network.network_id = $2
				WHERE network_user.user_id = $1
			`, byJwt.UserId, byJwt.NetworkId)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&credentialChangeTime))
					valid = true
				}
			})
			return
		}

		result, err := conn.Query(ctx, `
			SELECT network_user.credential_change_time
			FROM network_user
			INNER JOIN network ON
				network.admin_user_id = network_user.user_id AND
				network.network_id = $2
			INNER JOIN network_client ON
				network_client.client_id = $3 AND
				network_client.network_id = network.network_id AND
				network_client.active = true
			WHERE
				network_user.user_id = $1 AND
				($4::uuid IS NULL OR network_client.device_id = $4)
		`, byJwt.UserId, byJwt.NetworkId, *byJwt.ClientId, byJwt.DeviceId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&credentialChangeTime))
				valid = true
			}
		})
	})

	// the caller-facing message stays the same for both branches; the split
	// (row gone/inactive vs credential rotation) is visible in the counter,
	// and with ids at V(1)
	if !valid {
		authRejectionCounter.WithLabelValues(string(AuthRejectionNoActiveRow)).Inc()
		if glog.V(1) {
			glog.Infof("[jwt]reject %s: network=%s user=%s client=%v\n",
				AuthRejectionNoActiveRow, byJwt.NetworkId, byJwt.UserId, byJwt.ClientId)
		}
		return errors.New("Signed token is no longer active.")
	}
	if byJwt.CreateTime.Before(credentialChangeTime) {
		authRejectionCounter.WithLabelValues(string(AuthRejectionCredentialRotated)).Inc()
		if glog.V(1) {
			glog.Infof("[jwt]reject %s: rotated at %s, token created %s (user=%s)\n",
				AuthRejectionCredentialRotated, credentialChangeTime, byJwt.CreateTime, byJwt.UserId)
		}
		return errors.New("Signed token is no longer active.")
	}
	return nil
}

func ParseByJwtUnverified(ctx context.Context, jwtStr string) (*ByJwt, error) {
	// parse claims straight into the ByJwt struct, same as ParseByJwt
	byJwt := &ByJwt{}
	_, _, err := gojwt.NewParser().ParseUnverified(jwtStr, byJwt)
	if err != nil {
		return nil, err
	}

	err = fixByJwt(ctx, byJwt)
	if err != nil {
		return nil, err
	}

	return byJwt, nil
}

// func (self *ByJwt) Sign() string {
// 	claimsJson, err := json.Marshal(self)
// 	if err != nil {
// 		panic(err)
// 	}

// 	claims := &gojwt.MapClaims{}
// 	err = json.Unmarshal(claimsJson, claims)
// 	if err != nil {
// 		panic(err)
// 	}

// 	token := gojwt.NewWithClaims(gojwt.SigningMethodRS512, claims)

// 	jwtSigned, err := token.SignedString(bySigningKey())
// 	if err != nil {
// 		panic(err)
// 	}

// 	return jwtSigned
// }

func (self *ByJwt) Sign() string {
	return sign(self)
}

// the client jwt inherits the session roles and principal by default;
// callers that assign client-specific values set them on the returned jwt
// before signing
func (self *ByJwt) Client(deviceId server.Id, clientId server.Id) *ByJwt {
	return &ByJwt{
		NetworkId:        self.NetworkId,
		UserId:           self.UserId,
		NetworkName:      self.NetworkName,
		CreateTime:       self.CreateTime,
		GuestMode:        self.GuestMode,
		Pro:              self.Pro,
		Roles:            self.Roles,
		Principal:        self.Principal,
		DeviceId:         &deviceId,
		ClientId:         &clientId,
		RegisteredClaims: newRegisteredClaims(self.UserId),
	}
}

func (self *ByJwt) User() *ByJwt {
	return &ByJwt{
		NetworkId:        self.NetworkId,
		UserId:           self.UserId,
		NetworkName:      self.NetworkName,
		CreateTime:       self.CreateTime,
		GuestMode:        self.GuestMode,
		Pro:              self.Pro,
		Roles:            self.Roles,
		Principal:        self.Principal,
		RegisteredClaims: newRegisteredClaims(self.UserId),
	}
}

// in some cases, the byJwt might be corrupt
// in some of those cases, we can still recover it
func fixByJwt(ctx context.Context, byJwt *ByJwt) error {
	if byJwt.NetworkId == (server.Id{}) {
		// the NetworkId is missing (FIXME why?)
		// it can be recovered from the client id or the user id

		if byJwt.UserId != (server.Id{}) {
			var cachedValue string
			key := jwtNetworkIdByUserIdKey(byJwt.UserId)
			server.Redis(ctx, func(r server.RedisClient) {
				cachedValue, _ = r.Get(ctx, key).Result()
			})
			if cachedValue != "" {
				networkId, err := server.ParseId(cachedValue)
				if err == nil {
					byJwt.NetworkId = networkId
					glog.V(1).Infof("[jwt]fixed network_id with user_id (cached)\n")
				}
			} else {
				networkId, err := getNetworkIdForUser(ctx, byJwt.UserId)
				if err == nil {
					byJwt.NetworkId = networkId
					glog.V(1).Infof("[jwt]fixed network_id with user_id\n")
					storeCtx := context.Background()
					ttl := 15 * time.Minute
					go server.HandleError(func() {
						server.Redis(storeCtx, func(r server.RedisClient) {
							// ignore the error
							r.SetNX(storeCtx, key, networkId.String(), ttl).Err()
						})
					})
				}
			}
		} else if byJwt.ClientId != nil {
			var cachedValue string
			key := jwtNetworkIdByClientIdKey(*byJwt.ClientId)
			server.Redis(ctx, func(r server.RedisClient) {
				cachedValue, _ = r.Get(ctx, key).Result()
			})
			if cachedValue != "" {
				networkId, err := server.ParseId(cachedValue)
				if err == nil {
					byJwt.NetworkId = networkId
					glog.V(1).Infof("[jwt]fixed network_id with client_id (cached)\n")
				}
			} else {
				networkId, err := getNetworkIdForClient(ctx, *byJwt.ClientId)
				if err == nil {
					byJwt.NetworkId = networkId
					glog.V(1).Infof("[jwt]fixed network_id with client_id\n")
					storeCtx := context.Background()
					ttl := 15 * time.Minute
					go server.HandleError(func() {
						server.Redis(storeCtx, func(r server.RedisClient) {
							// ignore the error
							r.SetNX(storeCtx, key, networkId.String(), ttl).Err()
						})
					})
				}
			}
		}

		if byJwt.NetworkId == (server.Id{}) {
			return fmt.Errorf("Missing network_id")
		}
	}

	if byJwt.UserId == (server.Id{}) {
		return fmt.Errorf("Missing user_id")
	}

	return nil
}

func jwtNetworkIdByUserIdKey(userId server.Id) string {
	return fmt.Sprintf("jwt_network_id_u_%s", userId)
}

func getNetworkIdForUser(ctx context.Context, userId server.Id) (networkId server.Id, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
			    network_id
			FROM network
			WHERE admin_user_id = $1
			`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId))
			} else {
				returnErr = fmt.Errorf("network_id not found for user_id=%s", userId)
			}
		})
	})
	return
}

func jwtNetworkIdByClientIdKey(clientId server.Id) string {
	return fmt.Sprintf("jwt_network_id_c_%s", clientId)
}

func getNetworkIdForClient(ctx context.Context, clientId server.Id) (networkId server.Id, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
			    network_id
			FROM network_client
			WHERE client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId))
			} else {
				returnErr = fmt.Errorf("network_id not found for client_id=%s", clientId)
			}
		})
	})
	return
}

func LoadByJwtFromClientId(ctx context.Context, clientId server.Id) (byJwt *ByJwt, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
			    network.network_id,
			    network.admin_user_id,
			    network.network_name,
			    network_client.device_id,
			    network_client.principal,
			    EXISTS (
			        SELECT 1 FROM transfer_balance
			        WHERE transfer_balance.network_id = network.network_id
			            AND transfer_balance.pro = true
			            AND transfer_balance.start_time <= $2
			            AND $2 < transfer_balance.end_time
			    ) AS pro

			FROM network_client

			INNER JOIN network ON network.network_id = network_client.network_id

			WHERE
			    network_client.client_id = $1
		    `,
			clientId,
			server.NowUtc(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var networkId server.Id
				var userId server.Id
				var networkName string
				var deviceId server.Id
				var principal string
				var pro bool
				server.Raise(result.Scan(
					&networkId,
					&userId,
					&networkName,
					&deviceId,
					&principal,
					&pro,
				))

				guestMode := false

				byJwt = NewByJwt(
					networkId,
					userId,
					networkName,
					guestMode,
					pro,
				).Client(deviceId, clientId)
				byJwt.Principal = principal
			} else {
				returnErr = fmt.Errorf("Client not found.")
			}
		})

		if byJwt == nil {
			return
		}

		result, err = conn.Query(
			ctx,
			`
			SELECT role FROM network_client_role
			WHERE client_id = $1
			ORDER BY role
		    `,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var role string
				server.Raise(result.Scan(&role))
				byJwt.Roles = append(byJwt.Roles, role)
			}
		})
	})
	return
}

func sign(claims gojwt.Claims) string {
	var signingMethod gojwt.SigningMethod
	var key any

	if ecdsaKey := byEcdsaSigningKey(); ecdsaKey != nil {
		switch bitLen := ecdsaKey.Curve.Params().N.BitLen(); bitLen {
		case 256:
			signingMethod = gojwt.SigningMethodES256
		case 384:
			signingMethod = gojwt.SigningMethodES384
		case 512:
			signingMethod = gojwt.SigningMethodES512
		default:
			panic(fmt.Errorf("Unsupported ECDSA bit len %d", bitLen))
		}
		key = ecdsaKey
	} else if rsaKey := byRsaSigningKey(); rsaKey != nil {
		if bitLen := rsaKey.N.BitLen(); 2048 <= bitLen {
			signingMethod = gojwt.SigningMethodRS512
		} else {
			panic(fmt.Errorf("Unsupported RSA bit len %d", bitLen))
		}
		key = rsaKey
	} else {
		panic(fmt.Errorf("No signing key found"))
	}
	token := gojwt.NewWithClaims(signingMethod, claims)
	// tag the token with the signing key's kid so the verifier can select the
	// exact key instead of trying all of them
	if kid, err := jwtKid(publicKey(key)); err == nil {
		token.Header["kid"] = kid
	}
	jwtSigned, err := token.SignedString(key)
	if err != nil {
		panic(err)
	}
	return jwtSigned
}

// Testing_NormalizeClaims fills the registered claims and CreateTime of a
// hand-built ByJwt fixture, leaving explicitly set fields as-is. Tests
// commonly construct sessions from a bare &ByJwt{NetworkId, UserId} literal;
// tokens minted or derived from such a fixture (AuthNetworkClient derives
// by-client jwts via Client, which copies CreateTime) must survive
// ParseByJwt's full claims validation and ValidateByJwtState's
// credential_change_time comparison, both of which reject a zero CreateTime.
// Production mint paths go through NewByJwt and never need this.
func Testing_NormalizeClaims(byJwt *ByJwt) {
	if byJwt.CreateTime.IsZero() {
		byJwt.CreateTime = server.CodecTime(server.NowUtc())
	}
	if byJwt.Subject == "" {
		byJwt.RegisteredClaims = newRegisteredClaims(byJwt.UserId)
	}
}
