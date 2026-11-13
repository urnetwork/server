package sidecarhost

// ServiceHost.Authenticator on server/oauth and server/session (host_auth.go; §7.8, §9.15): a service token
// verified offline against the svc- key set this process mounts (oauth.VerifyServiceToken), and, on a block
// given one, a Client-class token for that block's audience, sockets', verified by session's ClientTokenVerifier
// (plan-SOCK-client-auth, QD-040): offline against the by jwt keyring's mounted public keys, then against the
// token's revocation record alone, never SQL. Either maps its claims onto sidecar.Principal.
//
// Refusals and unavailability stay apart, so an engine answers 401 only for a token that will not
// authenticate and unavailable for everything else:
//
//   - an empty audience, an empty token, a token a verifier refuses (signature, kid, issuer, audience,
//     expiry, missing claims, a record's tombstone or rotation) and a principal that Principal.Validate
//     refuses wrap sidecar.ErrAuthenticationRefused;
//   - a process with no usable verifier, and an audience that is no service audience and not the
//     block's Client-class one, answer an error that does not;
//   - a Client-class check that cannot answer yet, a record miss outside its grace or a store outage,
//     answers a *sidecar.AuthenticationUnavailableError carrying the wait the check names.
//
// A Client-class Principal names the token's network and client, its sidecar_id claim as SidecarId (nil for
// an urnetwork-edge platform client, which binds no sidecar), its member_id, no scopes and no log position:
// the engine reads the caller's §8 subject from SidecarId and the projection, never from the token's scopes.
// Safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/session"
)

var _ sidecar.Authenticator = (*Authenticator)(nil)

// A token audience no verifier here serves: a Client-class audience on a block given no verifier for it.
var errAuthenticatorAudienceUnavailable = errors.New("sidecarhost auth: no verifier for this audience on this host")

// What verifies a block's Client-class tokens: session's ClientTokenVerifier, or a test's.
type ClientTokenVerifier interface {
	// The by client jwt jwtSigned once verified and its client not revoked. A refusal wraps
	// session.ErrClientTokenRefused; a check that cannot answer is a *session.ClientTokenUnavailableError.
	Verify(ctx context.Context, jwtSigned string) (*session.ByJwt, error)
}

var _ ClientTokenVerifier = (*session.ClientTokenVerifier)(nil)

// Verifies service tokens, and one audience's Client-class tokens when it was given a verifier for them.
// NewAuthenticator and NewClientTokenAuthenticator make one.
type Authenticator struct {
	verify func(token string, audience string) (*oauth.ServiceTokenClaims, error)
	// The Client-class audience clientTokens serves, "" for none.
	clientAudience string
	clientTokens   ClientTokenVerifier
}

// An authenticator on the process's service token verifier, which answers every Client-class audience
// unavailable.
func NewAuthenticator() *Authenticator {
	return &Authenticator{verify: oauth.VerifyServiceToken}
}

// An authenticator on the process's service token verifier that verifies audience's Client-class tokens with
// clientTokens: sockets' block passes socketsservice.SocketsAudience and session's ClientTokenVerifier.
func NewClientTokenAuthenticator(audience string, clientTokens ClientTokenVerifier) *Authenticator {
	return &Authenticator{verify: oauth.VerifyServiceToken, clientAudience: audience, clientTokens: clientTokens}
}

// A claim's id as the root's.
func authPrincipalId(id *server.Id) *connect.Id {
	if id == nil {
		return nil
	}
	connectId := connect.Id(*id)
	return &connectId
}

// See sidecar.Authenticator.
func (self *Authenticator) Authenticate(ctx context.Context, audience string, jwt string) (*sidecar.Principal, error) {
	if audience == "" {
		return nil, fmt.Errorf("%w: no audience", sidecar.ErrAuthenticationRefused)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if self.clientTokens != nil && audience == self.clientAudience {
		return self.authenticateClientToken(ctx, audience, jwt)
	}
	if !oauth.IsServiceTokenAudience(audience) {
		return nil, fmt.Errorf("%w: %q", errAuthenticatorAudienceUnavailable, audience)
	}
	if jwt == "" {
		return nil, fmt.Errorf("%w: no token", sidecar.ErrAuthenticationRefused)
	}
	claims, err := self.verify(jwt, audience)
	if err != nil {
		if errors.Is(err, oauth.ErrInvalidServiceToken) {
			return nil, fmt.Errorf("%w: %w", sidecar.ErrAuthenticationRefused, err)
		}
		// no verifier: nothing is known about the token
		return nil, fmt.Errorf("sidecarhost auth: %w", err)
	}
	principal := &sidecar.Principal{
		NetworkId:       connect.Id(claims.NetworkId),
		ClientId:        connect.Id(claims.ClientId),
		SubjectClientId: authPrincipalId(claims.SubjectClientId),
		SidecarId:       authPrincipalId(claims.SidecarId),
		MemberId:        authPrincipalId(claims.MemberId),
		Audience:        claims.Audience(),
		Scopes:          claims.Scopes(),
		ProjVersion:     claims.ProjVersion,
	}
	if claims.IssuedAt != nil {
		principal.IssuedAt = claims.IssuedAt.Time
	}
	if claims.ExpiresAt != nil {
		principal.ExpiresAt = claims.ExpiresAt.Time
	}
	if err := principal.Validate(); err != nil {
		return nil, err
	}
	if principal.Audience != audience {
		return nil, fmt.Errorf("%w: another audience", sidecar.ErrAuthenticationRefused)
	}
	return principal, nil
}

// A Client-class token for audience, the block's own (QD-040).
func (self *Authenticator) authenticateClientToken(ctx context.Context, audience string, jwt string) (*sidecar.Principal, error) {
	if jwt == "" {
		return nil, fmt.Errorf("%w: no token", sidecar.ErrAuthenticationRefused)
	}
	byJwt, err := self.clientTokens.Verify(ctx, jwt)
	if err != nil {
		unavailable := &session.ClientTokenUnavailableError{}
		switch {
		case errors.Is(err, session.ErrClientTokenRefused):
			return nil, fmt.Errorf("%w: %w", sidecar.ErrAuthenticationRefused, err)
		case errors.As(err, &unavailable):
			return nil, &sidecar.AuthenticationUnavailableError{RetryAfter: unavailable.RetryAfter, Err: err}
		}
		// nothing is known about the token
		return nil, fmt.Errorf("sidecarhost auth: %w", err)
	}
	if byJwt == nil || byJwt.ClientId == nil {
		return nil, fmt.Errorf("%w: no client", sidecar.ErrAuthenticationRefused)
	}
	principal := &sidecar.Principal{
		NetworkId: connect.Id(byJwt.NetworkId),
		ClientId:  connect.Id(*byJwt.ClientId),
		SidecarId: authPrincipalId(byJwt.SidecarId),
		MemberId:  authPrincipalId(byJwt.MemberId),
		Audience:  audience,
	}
	if byJwt.IssuedAt != nil {
		principal.IssuedAt = byJwt.IssuedAt.Time
	}
	if byJwt.ExpiresAt != nil {
		principal.ExpiresAt = byJwt.ExpiresAt.Time
	}
	if err := principal.Validate(); err != nil {
		return nil, err
	}
	return principal, nil
}
