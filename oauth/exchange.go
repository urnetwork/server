package oauth

// The token exchange (§7.8, RFC 8693): a sidecar trades its client token for a
// 15-minute service token for evs, gkm, dns, certs or mail.
//
//	POST /oauth/token  grant_type=urn:ietf:params:oauth:grant-type:token-exchange
//	     subject_token=<by_client_jwt>  subject_token_type=urn:ur:params:oauth:token-type:client-jwt
//	     resource=<service URI>  scope=<within the service's>  DPoP: <proof by the ClientKey>
//
// The subject token is the credential, in the form, never as a bearer: the
// route stays Public, and a strict_exp bearer is refused there on every grant
// (api/route_authz.go). The exchange checks the token as session's one verifier
// does and refuses one past its exp whatever reject_expired says, checks the
// ClientKey's DPoP proof of this request (§7.4's rule, the jti single-use, htu
// the issuer's token endpoint), refuses an inactive or reaped client, and never
// issues the api's audience. Scopes come from the installed ExchangeScopeSource,
// §8's rows in the newest projection, which TEAM-M2-15 installs; until then the
// source grants no scope at all, so none is ever granted without a projection.
//
// Tokens are never stored. Each exchange is one line of api's 7-day service log
// with §2.8 api-11's fields, and never a token or a proof.

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const (
	// RFC 8693 §2.1's grant type
	ExchangeGrantType = "urn:ietf:params:oauth:grant-type:token-exchange"
	// §7.8's subject token type: a client's by_client_jwt
	ExchangeSubjectTokenType = "urn:ur:params:oauth:token-type:client-jwt"
	// RFC 8693 §3's type of what the exchange issues
	ExchangeIssuedTokenType = "urn:ietf:params:oauth:token-type:access_token"
)

var (
	// RFC 8707 §2: the resource names no service the exchange issues for, the
	// api's included
	ErrInvalidTarget = errors.New("invalid_target")
	// RFC 6749 §5.2: a scope the exchange will not grant
	ErrInvalidScope = errors.New("invalid_scope")
	// RFC 9449 §5: the request's DPoP proof does not show the client's key
	ErrInvalidDpopProof = errors.New("invalid_dpop_proof")
	// §19.10: a site-loss restore holds the exchange
	ErrRestoreRequired = errors.New("restore_required")
)

// What the exchange asks its scope source: who asks, for which service, and
// which scopes.
type ExchangeScopeRequest struct {
	NetworkId       server.Id
	ClientId        server.Id
	SubjectClientId server.Id
	// nil for a client bound to no sidecar
	SidecarId *server.Id
	Audience  string
	// as asked, each once
	Scopes []string
}

// What a scope source grants.
type ExchangeScopeGrant struct {
	// a subset of the request's, the claim scope
	Scopes []string
	// the log position the scopes came from, the claim proj_v; 0 for none
	ProjVersion uint64
	// the member the sidecar belongs to, nil for none
	MemberId *server.Id
	// who acts for the subject, nil for none
	Act *ServiceTokenActor
}

// Where the exchange's scopes come from: §8's rows in the newest projection
// (TEAM-M2-15). Grant refuses a scope it will not grant with an error wrapping
// ErrInvalidScope; any other error is the exchange's server error.
type ExchangeScopeSource interface {
	Grant(ctx context.Context, request *ExchangeScopeRequest) (*ExchangeScopeGrant, error)
}

// The source until a projection's is installed: it grants no scope at all, so a
// request for any is refused, and a request for none gets a token that grants
// nothing but names its client.
type noProjectionExchangeScopeSource struct{}

func (self noProjectionExchangeScopeSource) Grant(ctx context.Context, request *ExchangeScopeRequest) (*ExchangeScopeGrant, error) {
	if 0 < len(request.Scopes) {
		return nil, fmt.Errorf("%w: no projection grants scopes yet", ErrInvalidScope)
	}
	return &ExchangeScopeGrant{}, nil
}

// The installed source, nil for none.
var exchangeScopeSource atomic.Pointer[ExchangeScopeSource]

// Installs source as the exchange's, returning a function that restores the
// previous one. nil restores the no-projection source.
func SetExchangeScopeSource(source ExchangeScopeSource) func() {
	var previous *ExchangeScopeSource
	if source == nil {
		previous = exchangeScopeSource.Swap(nil)
	} else {
		previous = exchangeScopeSource.Swap(&source)
	}
	return func() {
		exchangeScopeSource.Store(previous)
	}
}

func currentExchangeScopeSource() ExchangeScopeSource {
	if source := exchangeScopeSource.Load(); source != nil {
		return *source
	}
	return noProjectionExchangeScopeSource{}
}

// The exchange's reads of server state, which tests replace.
var (
	// refuses an inactive, reaped or removed client, or a network or credential
	// that is gone
	exchangeValidateClient = func(ctx context.Context, byJwt *session.ByJwt) error {
		return session.ValidateByJwtState(ctx, byJwt, true)
	}
	// the ClientKey a proof must be signed by (§7.4)
	exchangeProofKey = model.ClientProofPublicKey
	// the sidecar the client is bound to, nil for none
	exchangeClientSidecarId = func(ctx context.Context, networkId server.Id, clientId server.Id) *server.Id {
		view := model.GetSidecarIdentityByClient(ctx, networkId, clientId)
		if view == nil {
			return nil
		}
		sidecarId := view.SidecarId
		return &sidecarId
	}
	// the §8 subject the client acts as
	exchangeClientSubject = model.GetNetworkClientAclSubject
	// api's service log
	exchangeLogf = glog.Infof
)

// One exchange request, as its form and DPoP header carry it.
type ExchangeArgs struct {
	SubjectToken     string
	SubjectTokenType string
	// every resource the form names; the exchange takes exactly one
	Resources []string
	Scope     string
	// every DPoP header the request carries; the exchange takes exactly one
	Proofs []string
}

// RFC 8693 §2.2.1's answer.
type ExchangeResult struct {
	AccessToken     string `json:"access_token"`
	IssuedTokenType string `json:"issued_token_type"`
	TokenType       string `json:"token_type"`
	ExpiresIn       int64  `json:"expires_in"`
	Scope           string `json:"scope,omitempty"`
}

// The service audience a resource names (RFC 8707): exactly
// https://<audience>.<WARP_DOMAIN>, the service's own origin, for one of the
// service audiences. The api's origin is refused by name: the exchange never
// issues the api audience.
func serviceAudienceForResource(resource string) (string, error) {
	domain, err := server.Domain()
	if err != nil {
		return "", fmt.Errorf("%w: no service domain", ErrInvalidTarget)
	}
	parsed, err := url.Parse(resource)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		strings.Contains(resource, "#") || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("%w: not a service origin", ErrInvalidTarget)
	}
	host := strings.ToLower(parsed.Host)
	if host == "api."+domain {
		return "", fmt.Errorf("%w: the api audience is never issued", ErrInvalidTarget)
	}
	audience, ok := strings.CutSuffix(host, "."+domain)
	if !ok || !IsServiceTokenAudience(audience) {
		return "", fmt.Errorf("%w: no such service", ErrInvalidTarget)
	}
	return audience, nil
}

// Whether scope is an RFC 6749 §3.3 scope-token: printable ASCII but space, "
// and \.
func isScopeToken(scope string) bool {
	if scope == "" {
		return false
	}
	for i := 0; i < len(scope); i += 1 {
		c := scope[i]
		if c < 0x21 || 0x7e < c || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// The scopes a form asks for, each once in the order first asked, and at most
// §7.8's 64 repo: scopes: a 65th is refused, not dropped.
func exchangeScopes(scope string) ([]string, error) {
	scopes := []string{}
	seen := map[string]bool{}
	for _, s := range ParseScope(scope) {
		if !isScopeToken(s) {
			return nil, fmt.Errorf("%w: not a scope", ErrInvalidScope)
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		scopes = append(scopes, s)
	}
	if ServiceTokenMaxRepoScopeCount < serviceTokenRepoScopeCount(scopes) {
		return nil, fmt.Errorf("%w: more than %d repo: scopes", ErrInvalidScope, ServiceTokenMaxRepoScopeCount)
	}
	return scopes, nil
}

// The client token's claims, verified as every route verifies a bearer, and
// refused past its exp whatever reject_expired says: an expired subject token
// buys nothing (§7.8).
func exchangeSubject(ctx context.Context, subjectToken string) (*session.ByJwt, error) {
	byJwt, err := session.ParseByJwtForAudience(ctx, subjectToken, session.ByJwtAudienceApi)
	if err != nil {
		return nil, fmt.Errorf("%w: the subject token does not verify", ErrInvalidGrant)
	}
	if byJwt.ClientId == nil {
		return nil, fmt.Errorf("%w: the subject token names no client", ErrInvalidGrant)
	}
	if byJwt.GuestMode {
		return nil, fmt.Errorf("%w: a guest's token", ErrInvalidGrant)
	}
	if byJwt.ExpiresAt == nil || !server.NowUtc().Before(byJwt.ExpiresAt.Time) {
		return nil, fmt.Errorf("%w: the subject token is expired", ErrInvalidGrant)
	}
	return byJwt, nil
}

// Exchanges a sidecar's client token for a service token, or refuses with an
// error wrapping ErrInvalidRequest, ErrInvalidTarget, ErrInvalidScope,
// ErrInvalidDpopProof, ErrInvalidGrant or ErrRestoreRequired. Any other error
// is the server's. A refusal's error names no token.
func Exchange(ctx context.Context, args *ExchangeArgs) (*ExchangeResult, error) {
	if args.SubjectTokenType != ExchangeSubjectTokenType || args.SubjectToken == "" {
		return nil, fmt.Errorf("%w: a client token is the only subject", ErrInvalidRequest)
	}
	if len(args.Resources) != 1 {
		return nil, fmt.Errorf("%w: exactly one resource", ErrInvalidTarget)
	}
	audience, err := serviceAudienceForResource(args.Resources[0])
	if err != nil {
		return nil, err
	}
	scopes, err := exchangeScopes(args.Scope)
	if err != nil {
		return nil, err
	}
	if server.CurrentSidecarConfig().Reseed() {
		// re-seed mode's admin tokens are not built yet (§19.10), so the
		// exchange holds everything while a restore runs
		return nil, fmt.Errorf("%w: a restore holds the exchange", ErrRestoreRequired)
	}

	byJwt, err := exchangeSubject(ctx, args.SubjectToken)
	if err != nil {
		return nil, err
	}
	networkId := byJwt.NetworkId
	clientId := *byJwt.ClientId

	// the ClientKey's proof of this request, before any state is read: its jti
	// is spent whatever follows
	if len(args.Proofs) != 1 {
		return nil, fmt.Errorf("%w: not exactly one proof", ErrInvalidDpopProof)
	}
	proofKey, err := exchangeProofKey(ctx, clientId)
	if err != nil {
		return nil, fmt.Errorf("%w: the client has no bound key", ErrInvalidDpopProof)
	}
	err = session.VerifyDpopProof(ctx, &session.DpopRequest{
		Proof:    args.Proofs[0],
		Method:   "POST",
		Uri:      TokenEndpoint(),
		Token:    args.SubjectToken,
		Key:      proofKey,
		ReplayId: clientId,
	})
	if err != nil {
		if errors.Is(err, session.ErrAuthUnavailable) {
			return nil, fmt.Errorf("the proof's replay store is unavailable: %w", err)
		}
		return nil, fmt.Errorf("%w: the proof does not show the client's key", ErrInvalidDpopProof)
	}

	if err := exchangeValidateClient(ctx, byJwt); err != nil {
		if errors.Is(err, session.ErrAuthUnavailable) {
			return nil, fmt.Errorf("the client's state is unavailable: %w", err)
		}
		return nil, fmt.Errorf("%w: the client is not active", ErrInvalidGrant)
	}
	subjectClientId, err := exchangeClientSubject(ctx, networkId, clientId)
	if err != nil {
		return nil, fmt.Errorf("%w: the client's subject is not known", ErrInvalidGrant)
	}
	sidecarId := exchangeClientSidecarId(ctx, networkId, clientId)

	grant, err := currentExchangeScopeSource().Grant(ctx, &ExchangeScopeRequest{
		NetworkId:       networkId,
		ClientId:        clientId,
		SubjectClientId: subjectClientId,
		SidecarId:       sidecarId,
		Audience:        audience,
		Scopes:          slices.Clone(scopes),
	})
	if err != nil {
		return nil, err
	}
	for _, granted := range grant.Scopes {
		if !slices.Contains(scopes, granted) {
			return nil, fmt.Errorf("the scope source granted a scope the request did not ask for")
		}
	}

	var subjectClaim *server.Id
	if subjectClientId != clientId {
		subjectClaim = &subjectClientId
	}
	accessToken, expiry, err := MintServiceToken(&MintServiceTokenArgs{
		NetworkId:       networkId,
		ClientId:        clientId,
		SubjectClientId: subjectClaim,
		SidecarId:       sidecarId,
		MemberId:        grant.MemberId,
		Audience:        audience,
		Scopes:          grant.Scopes,
		ProjVersion:     grant.ProjVersion,
		Act:             grant.Act,
	})
	if err != nil {
		return nil, err
	}

	// §2.8 api-11: client, subject client, sidecar, member, audience, scope,
	// projection version, act and the DPoP key, never a token or a proof
	act := ""
	if grant.Act != nil {
		act = grant.Act.Subject
	}
	exchangeLogf(
		"[oauth][exchange]network %s client %s subject %s sidecar %s member %s aud %s scope %q proj_v %d act %q dpop_jkt %s\n",
		networkId,
		clientId,
		subjectClientId,
		optionalIdString(sidecarId),
		optionalIdString(grant.MemberId),
		audience,
		FormatScope(grant.Scopes),
		grant.ProjVersion,
		act,
		session.DpopKeyThumbprint(ed25519.PublicKey(proofKey)),
	)

	expiresIn := int64(expiry.Sub(server.NowUtc()).Seconds())
	return &ExchangeResult{
		AccessToken:     accessToken,
		IssuedTokenType: ExchangeIssuedTokenType,
		TokenType:       "Bearer",
		ExpiresIn:       max(expiresIn, 0),
		Scope:           FormatScope(grant.Scopes),
	}, nil
}

func optionalIdString(id *server.Id) string {
	if id == nil {
		return "none"
	}
	return id.String()
}
