package sidecarhost

// ServiceHost.Authenticator on server/oauth (host_auth.go; §7.8): a service token verified offline
// against the svc- key set this process mounts (oauth.VerifyServiceToken), its claims mapped onto
// sidecar.Principal.
//
// Refusals and unavailability stay apart, so an engine answers 401 only for a token that will not
// authenticate and unavailable for everything else:
//
//   - an empty audience, an empty token, a token the verifier refuses (signature, kid, issuer, audience,
//     expiry, missing claims) and a principal that Principal.Validate refuses wrap
//     sidecar.ErrAuthenticationRefused;
//   - a process with no usable verifier, and an audience that is no service audience, answer an error
//     that does not.
//
// sockets' callers hold Client-class tokens, which carry no service audience. plan-SOCK-client-auth
// settles how they verify (a verify-only by_jwt keyring, the revocation record's no-SQL check and its
// grace, a sidecar_id claim), and none of that is built here, so until it is every such call answers
// unavailable, never an unauthenticated allow. Safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/oauth"
)

var _ sidecar.Authenticator = (*Authenticator)(nil)

// A token audience no verifier here serves: Client-class tokens for sockets (plan-SOCK-client-auth).
var errAuthenticatorAudienceUnavailable = errors.New("sidecarhost auth: no verifier for this audience on this host")

// Verifies service tokens. NewAuthenticator makes one.
type Authenticator struct {
	verify func(token string, audience string) (*oauth.ServiceTokenClaims, error)
}

// An authenticator on the process's service token verifier.
func NewAuthenticator() *Authenticator {
	return &Authenticator{verify: oauth.VerifyServiceToken}
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
