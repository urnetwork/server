package oauth

// Service tokens (§7.8): what the token exchange mints for a sidecar to call
// evs, gkm, dns, certs or mail, and what those services verify offline.
//
// A service token is an ES256 JWT under the svc- key set, never the mcp set, the
// admin set or the ByJwt keys, so it is no credential anywhere else. It lives 15
// minutes (owner decision 2), names one service audience and never the api's,
// and carries §7.8's claims: scope (at most 64 repo: scopes), proj_v (the log
// position its scopes came from), network_id, ur_client_id, and where they
// apply ur_subject_client_id, ur_sidecar_id, member_id and act. Tokens are
// never stored (§2.8 api-11).
//
// The issuer verifies with its own svc- keys. A non-issuer block mounts only the
// svc- set's public jwks and the issuer that signs with it, the vault file
// oauth-jwks-svc.json (§19.4), and loads nothing of auth.yml.

import (
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
)

const (
	// §7.8 and owner decision 2: a service token lives 15 minutes
	ServiceTokenDuration = 15 * time.Minute
	// §7.8: at most 64 repo: scopes, keeping the bearer under the lb's 8 KiB
	// header line
	ServiceTokenMaxRepoScopeCount = 64
	// the prefix of an evs or gkm capability scope, repo:<id>:<role>
	ServiceTokenRepoScopePrefix = "repo:"
	// the prefix of an unadmitted device's own recovery wraps scope at gkm,
	// recovery:<member_id>
	ServiceTokenRecoveryScopePrefix = "recovery:"
)

// The audiences a service token may name (§7.8). The api's is never one.
var serviceTokenAudiences = map[string]bool{
	"evs":   true,
	"gkm":   true,
	"dns":   true,
	"certs": true,
	"mail":  true,
}

// Whether audience is one a service token may name.
func IsServiceTokenAudience(audience string) bool {
	return serviceTokenAudiences[audience]
}

// The vault file a non-issuer block mounts: the svc- set's public jwks and the
// issuer (§19.4).
const ServiceJwksResourceName = "oauth-jwks-svc.json"

// A token that is no service token for the audience: a bad signature, another
// key set's kid, another issuer or audience, a passed exp, or missing claims.
var ErrInvalidServiceToken = errors.New("invalid service token")

// RFC 8693 §4.1's actor: who acts for the subject, when someone does.
type ServiceTokenActor struct {
	Subject string `json:"sub"`
}

// The claims of a service token. Ids are the server's ids; an absent pointer is
// an absent claim.
type ServiceTokenClaims struct {
	// space delimited
	Scope string `json:"scope,omitempty"`
	// the log position n the scopes came from (§7.9); 0 for none
	ProjVersion uint64 `json:"proj_v"`
	// the network the token belongs to, whose rev/ objects apply (§7.8)
	NetworkId server.Id `json:"network_id"`
	// the client that exchanged its token
	ClientId server.Id `json:"ur_client_id"`
	// the §8 subject the client acts as, absent when it acts as itself
	SubjectClientId *server.Id `json:"ur_subject_client_id,omitempty"`
	// the sidecar the client is bound to
	SidecarId *server.Id `json:"ur_sidecar_id,omitempty"`
	// the member the sidecar belongs to
	MemberId *server.Id         `json:"member_id,omitempty"`
	Act      *ServiceTokenActor `json:"act,omitempty"`

	gojwt.RegisteredClaims
}

// The token's scopes.
func (self *ServiceTokenClaims) Scopes() []string {
	return ParseScope(self.Scope)
}

// The one audience the token names.
func (self *ServiceTokenClaims) Audience() string {
	if len(self.RegisteredClaims.Audience) != 1 {
		return ""
	}
	return self.RegisteredClaims.Audience[0]
}

// What a service token says.
type MintServiceTokenArgs struct {
	NetworkId       server.Id
	ClientId        server.Id
	SubjectClientId *server.Id
	SidecarId       *server.Id
	MemberId        *server.Id
	Audience        string
	Scopes          []string
	ProjVersion     uint64
	Act             *ServiceTokenActor
}

// Counts the repo: scopes among scopes.
func serviceTokenRepoScopeCount(scopes []string) int {
	count := 0
	for _, scope := range scopes {
		if strings.HasPrefix(scope, ServiceTokenRepoScopePrefix) {
			count += 1
		}
	}
	return count
}

// The checks every service token meets, minted or verified.
func validateServiceTokenClaims(claims *ServiceTokenClaims) error {
	if claims.NetworkId == (server.Id{}) {
		return errors.New("no network")
	}
	if claims.ClientId == (server.Id{}) {
		return errors.New("no client")
	}
	for _, claimId := range []*server.Id{claims.SubjectClientId, claims.SidecarId, claims.MemberId} {
		if claimId != nil && *claimId == (server.Id{}) {
			return errors.New("a zero id in a claim")
		}
	}
	if !IsServiceTokenAudience(claims.Audience()) {
		return errors.New("not a service audience")
	}
	if ServiceTokenMaxRepoScopeCount < serviceTokenRepoScopeCount(claims.Scopes()) {
		return fmt.Errorf("more than %d repo: scopes", ServiceTokenMaxRepoScopeCount)
	}
	return nil
}

// Mints a signed service token under the svc- set's newest key, 15 minutes from
// now. The caller has already authorized every claim; this only encodes the
// decision.
func MintServiceToken(args *MintServiceTokenArgs) (string, time.Time, error) {
	signingKey, err := SigningKeyOf(SignerKeySetService)
	if err != nil {
		return "", time.Time{}, err
	}
	now := server.NowUtc()
	expiry := now.Add(ServiceTokenDuration)
	claims := &ServiceTokenClaims{
		Scope:           FormatScope(args.Scopes),
		ProjVersion:     args.ProjVersion,
		NetworkId:       args.NetworkId,
		ClientId:        args.ClientId,
		SubjectClientId: args.SubjectClientId,
		SidecarId:       args.SidecarId,
		MemberId:        args.MemberId,
		Act:             args.Act,
		RegisteredClaims: gojwt.RegisteredClaims{
			Issuer:    Issuer(),
			Subject:   args.ClientId.String(),
			Audience:  gojwt.ClaimStrings{args.Audience},
			IssuedAt:  gojwt.NewNumericDate(now),
			NotBefore: gojwt.NewNumericDate(now),
			ExpiresAt: gojwt.NewNumericDate(expiry),
			ID:        server.NewId().String(),
		},
	}
	if err := validateServiceTokenClaims(claims); err != nil {
		return "", time.Time{}, fmt.Errorf("service token: %w", err)
	}
	token := gojwt.NewWithClaims(gojwt.SigningMethodES256, claims)
	token.Header["kid"] = signingKey.Kid
	signed, err := token.SignedString(signingKey.PrivateKey)
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, expiry, nil
}

// The document a non-issuer block mounts: a jwk set (RFC 7517 §5, whose
// readers ignore the member they do not know) with the issuer beside its keys.
type PublicJwksDocument struct {
	Issuer string `json:"issuer"`
	Keys   []*Jwk `json:"keys"`
}

// Verifies service tokens against the svc- set's public keys and the issuer that
// signs with them. Safe for concurrent use.
type ServiceTokenVerifier struct {
	issuer string
	keys   map[string]*ecdsa.PublicKey
	now    func() time.Time
}

// A verifier of document's keys. Each must be an svc- key whose kid is its
// prefix and thumbprint; any other refuses the whole document, so a mistaken
// mount verifies nothing rather than another set's tokens.
func NewServiceTokenVerifier(document *PublicJwksDocument) (*ServiceTokenVerifier, error) {
	if document == nil || document.Issuer == "" || strings.HasSuffix(document.Issuer, "/") {
		return nil, errors.New("the jwks names no issuer")
	}
	keys := map[string]*ecdsa.PublicKey{}
	for _, jwk := range document.Keys {
		if SignerKeySetOfKid(jwk.Kid) != SignerKeySetService {
			return nil, fmt.Errorf("jwk %s is not an svc- key", jwk.Kid)
		}
		if jwk.Alg != signerAlgEs256 {
			return nil, fmt.Errorf("jwk %s is not ES256", jwk.Kid)
		}
		publicKey, err := jwk.PublicKey()
		if err != nil {
			return nil, err
		}
		thumbprint, err := SignerKid(publicKey)
		if err != nil {
			return nil, err
		}
		if jwk.Kid != SignerKeySetService.KidPrefix()+thumbprint {
			return nil, fmt.Errorf("jwk %s is not its key's thumbprint", jwk.Kid)
		}
		keys[jwk.Kid] = publicKey
	}
	if len(keys) == 0 {
		return nil, errors.New("the jwks holds no key")
	}
	return &ServiceTokenVerifier{
		issuer: document.Issuer,
		keys:   keys,
		now:    server.NowUtc,
	}, nil
}

// The claims of token, a service token of this verifier's issuer for audience,
// one of IsServiceTokenAudience's. Every registered claim is checked: alg ES256,
// an svc- kid this verifier holds, iss, exactly the one audience, exp and iat
// present and the exp not passed. A failure wraps ErrInvalidServiceToken.
func (self *ServiceTokenVerifier) Verify(token string, audience string) (*ServiceTokenClaims, error) {
	if !IsServiceTokenAudience(audience) {
		return nil, fmt.Errorf("%w: %q is not a service audience", ErrInvalidServiceToken, audience)
	}
	claims := &ServiceTokenClaims{}
	keyFunc := func(token *gojwt.Token) (any, error) {
		kid, ok := token.Header["kid"].(string)
		if !ok {
			return nil, errors.New("no kid")
		}
		publicKey, ok := self.keys[kid]
		if !ok || SignerKeySetOfKid(kid) != SignerKeySetService {
			return nil, fmt.Errorf("kid %s is no service key", kid)
		}
		return publicKey, nil
	}
	_, err := gojwt.ParseWithClaims(
		token,
		claims,
		keyFunc,
		gojwt.WithValidMethods([]string{signerAlgEs256}),
		gojwt.WithIssuer(self.issuer),
		gojwt.WithAudience(audience),
		gojwt.WithExpirationRequired(),
		gojwt.WithIssuedAt(),
		gojwt.WithTimeFunc(self.now),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidServiceToken, err)
	}
	if claims.IssuedAt == nil {
		return nil, fmt.Errorf("%w: no iat", ErrInvalidServiceToken)
	}
	if err := validateServiceTokenClaims(claims); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidServiceToken, err)
	}
	return claims, nil
}

// The issuer's verifier of its own svc- set.
func issuerServiceTokenVerifier() (*ServiceTokenVerifier, error) {
	return NewServiceTokenVerifier(&PublicJwksDocument{
		Issuer: Issuer(),
		Keys:   PublicJwks(SignerKeySetService).Keys,
	})
}

// The process's verifier: a non-issuer block's mounted oauth-jwks-svc.json, read
// once, else the issuer's own svc- set.
var defaultServiceTokenVerifier = sync.OnceValues(func() (*ServiceTokenVerifier, error) {
	resource, err := server.Vault.SimpleResource(ServiceJwksResourceName)
	if err != nil {
		if errors.Is(err, server.ErrResourceNotFound) && !errors.Is(err, server.ErrResourceUnavailable) {
			return issuerServiceTokenVerifier()
		}
		return nil, err
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, err
	}
	document := &PublicJwksDocument{}
	if err := json.Unmarshal(data, document); err != nil {
		return nil, fmt.Errorf("%s: %w", ServiceJwksResourceName, err)
	}
	return NewServiceTokenVerifier(document)
})

// A verifier tests install in place of the process's.
var serviceTokenVerifierOverride atomic.Pointer[ServiceTokenVerifier]

// The claims of token, a service token for audience, by the process's verifier
// (§7.8). This is what server's ServiceHost.Authenticator wraps for evs and gkm
// (OPS-M1-04). A process with no usable verifier refuses every token, with an
// error that is not ErrInvalidServiceToken, so the engine answers unavailable.
func VerifyServiceToken(token string, audience string) (*ServiceTokenClaims, error) {
	verifier, err := currentServiceTokenVerifier()
	if err != nil {
		return nil, fmt.Errorf("no service token verifier: %w", err)
	}
	return verifier.Verify(token, audience)
}

func currentServiceTokenVerifier() (*ServiceTokenVerifier, error) {
	if override := serviceTokenVerifierOverride.Load(); override != nil {
		return override, nil
	}
	return defaultServiceTokenVerifier()
}

// Installs verifier in place of the process's until the returned pop.
func Testing_SetServiceTokenVerifier(verifier *ServiceTokenVerifier) func() {
	previous := serviceTokenVerifierOverride.Swap(verifier)
	return func() {
		serviceTokenVerifierOverride.Store(previous)
	}
}

// The verifier's clock, for a test.
func (self *ServiceTokenVerifier) Testing_SetNow(now func() time.Time) {
	self.now = now
}
