package controller

// §13.15's certificate routes at M5 (TLS-M5-16; R-TLS-3, R-TLS-4, D-TLS-6):
//
//	POST /tls/cert-order       a service token, aud certs: the caller's CSR and attestation, queued
//	GET  /tls/cert-order/<id>  the same token: the caller's own order
//	GET  /tls/certs            a client token: its network's certificates and attestations
//	POST /tls/cert-revoke      a client token for its own certificate, a network admin's credential or an API key whose scopes reach devices for any of its network's
//
// An order's subject is the token's and never the body's: its network, client
// and sidecar are the verified claims, and its identifier set, identity key
// and §8 grant come from the network's newest verified projection through
// this process's installed deps. Nothing here reaches a CA: a route writes or
// reads a row, and certs' taskworker chains do the rest (TLS-M5-17). So an
// order's status answers the chain its row keeps from the issuance, the same
// after a restart, and a revocation writes its reason into the row, where it
// waits for the chain that takes it to the CA (cert-1; QD-741).
//
// What the routes run on is installed per process (SetTlsCerts), as a gate's
// states are: api installs it only while sidecar.yml's certs switch is on
// (InstallTlsCerts, tls_certs_install.go; QD-846), with the subject read from
// the projection through AclGateView. A process with nothing installed answers every route 503, and a
// subject the projection cannot answer for is 503 too, never an order with no
// grant check. An order the gate does not grant is 403, a CSR or attestation
// that does not check 400, and an order or certificate that is not the
// caller's 404, the same as one that does not exist.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The audience a certificate order's service token names (§7.8).
const TlsCertsAudience = "certs"

// What a caller's token and its network's verified projection give its order.
type TlsCertSubject struct {
	// §13.8's set the caller's config gives it, sidecar.TlsIdentifierSet's answer.
	IdentifierSet []string
	// The caller's Ed25519 identity key, from the projection.
	IdentityPublicKey []byte
	// Whether §8's gate holds an active tls/ or smtp/ grant covering the set
	// for the caller.
	Granted bool
}

// What the routes run on. Every field is required.
type TlsCertsDeps struct {
	Service *certsservice.CertsService
	// The rows, for a revocation's mark: the store Service runs on.
	Store certsservice.CertsStore
	// Verifies a service token for TlsCertsAudience.
	Authenticator sidecar.Authenticator
	// The principal's subject from its network's newest verified projection at
	// now. An error is a projection that cannot answer, and the order waits.
	Subject func(ctx context.Context, principal *sidecar.Principal, now time.Time) (*TlsCertSubject, error)
}

var tlsCertsDeps atomic.Pointer[TlsCertsDeps]

// Installs what this process's /tls routes run on; nil, or deps missing a
// field, removes it, and every route answers unavailable.
func SetTlsCerts(deps *TlsCertsDeps) {
	if deps == nil || deps.Service == nil || deps.Store == nil || deps.Authenticator == nil || deps.Subject == nil {
		tlsCertsDeps.Store(nil)
		return
	}
	tlsCertsDeps.Store(deps)
}

var (
	errTlsCertsUnavailable   = fmt.Errorf("%d Certificates are temporarily unavailable.", http.StatusServiceUnavailable)
	errTlsCertsUnauthorized  = fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
	errTlsCertsNotGranted    = fmt.Errorf("%d No grant for the certificate's names.", http.StatusForbidden)
	errTlsCertsNotSidecar    = fmt.Errorf("%d Not a sidecar's token.", http.StatusForbidden)
	errTlsCertsRequest       = fmt.Errorf("%d Certificate request refused.", http.StatusBadRequest)
	errTlsCertsNotFound      = fmt.Errorf("%d Certificate not found.", http.StatusNotFound)
	errTlsCertsNotOwn        = fmt.Errorf("%d Not this client's certificate.", http.StatusForbidden)
	errTlsCertsRoleRefused   = fmt.Errorf("%d This credential cannot revoke another client's certificate.", http.StatusForbidden)
	errTlsCertsNotRevocable  = fmt.Errorf("%d Certificate cannot be revoked.", http.StatusConflict)
	errTlsCertsNoCredentials = fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
)

// The answer to an engine or store error: its refusal's status, and 503 for
// everything else, whose cause stays in the log.
func tlsCertsError(route string, err error) error {
	switch {
	case errors.Is(err, certsservice.ErrCertsNotGranted):
		return errTlsCertsNotGranted
	case errors.Is(err, certsservice.ErrCertsCsr), errors.Is(err, certsservice.ErrCertsAttestation), errors.Is(err, certsservice.ErrCertsRequest):
		return errTlsCertsRequest
	case errors.Is(err, certsservice.ErrCertsNotFound):
		return errTlsCertsNotFound
	case errors.Is(err, certsservice.ErrCertsNotRevocable):
		return errTlsCertsNotRevocable
	}
	glog.Infof("[tls]%s unavailable: %v\n", route, err)
	return errTlsCertsUnavailable
}

// The principal a service token names for aud certs: 401 for a token that
// will not authenticate, 503 where nothing can be said about it.
func tlsCertsPrincipal(ctx context.Context, deps *TlsCertsDeps, token string) (*sidecar.Principal, error) {
	if token == "" {
		return nil, errTlsCertsNoCredentials
	}
	principal, err := deps.Authenticator.Authenticate(ctx, TlsCertsAudience, token)
	if err == nil {
		err = principal.Validate()
	}
	if err == nil && principal.Audience != TlsCertsAudience {
		err = sidecar.ErrAuthenticationRefused
	}
	if err != nil {
		if errors.Is(err, sidecar.ErrAuthenticationRefused) {
			return nil, errTlsCertsUnauthorized
		}
		glog.Infof("[tls]authentication unavailable: %v\n", err)
		return nil, errTlsCertsUnavailable
	}
	return principal, nil
}

// POST /tls/cert-order: queues the token's sidecar's order for its own set.
func TlsCertOrder(ctx context.Context, token string, args *sidecar.TlsCertOrderArgs) (*sidecar.TlsCertOrderResult, error) {
	deps := tlsCertsDeps.Load()
	if deps == nil {
		return nil, errTlsCertsUnavailable
	}
	principal, err := tlsCertsPrincipal(ctx, deps, token)
	if err != nil {
		return nil, err
	}
	if principal.SidecarId == nil {
		return nil, errTlsCertsNotSidecar
	}
	if args == nil {
		return nil, errTlsCertsRequest
	}
	// a CSR naming one name under no URnetwork root is a Team custom host's, never certs' own (17.2.5, C9)
	if tlsCertOrderIsCustom(args) {
		return tlsCustomCertOrder(ctx, token, args)
	}
	subject, err := deps.Subject(ctx, principal, server.NowUtc())
	if err != nil || subject == nil {
		// the projection cannot answer: no order is taken on a doubt
		glog.Infof("[tls]cert-order has no subject: %v\n", err)
		return nil, errTlsCertsUnavailable
	}
	if !subject.Granted {
		// before the engine reads the request: a caller with no grant learns nothing of its CSR
		return nil, errTlsCertsNotGranted
	}
	result, err := deps.Service.Accept(ctx, &certsservice.CertsOrderRequest{
		NetworkId:         sidecar.Id(principal.NetworkId),
		ClientId:          sidecar.Id(principal.ClientId),
		SidecarId:         sidecar.Id(*principal.SidecarId),
		IdentifierSet:     subject.IdentifierSet,
		IdentityPublicKey: subject.IdentityPublicKey,
		Granted:           subject.Granted,
		Args:              args,
	})
	if err != nil {
		return nil, tlsCertsError("cert-order", err)
	}
	return result, nil
}

// GET /tls/cert-order/<id>: the token's client's own order.
func TlsCertOrderStatus(ctx context.Context, token string, certId server.Id) (*sidecar.TlsCertOrderStatus, error) {
	deps := tlsCertsDeps.Load()
	if deps == nil {
		return nil, errTlsCertsUnavailable
	}
	principal, err := tlsCertsPrincipal(ctx, deps, token)
	if err != nil {
		return nil, err
	}
	status, err := deps.Service.OrderStatus(ctx, sidecar.Id(principal.NetworkId), sidecar.Id(principal.ClientId), sidecar.Id(certId))
	if err != nil {
		return nil, tlsCertsError("cert-order status", err)
	}
	return status, nil
}

// GET /tls/certs: the bearer's network's certificates and attestations, for
// the CT watch and for a device checking that its row survived a restore.
func TlsCerts(clientSession *session.ClientSession) (*sidecar.TlsCertsResult, error) {
	deps := tlsCertsDeps.Load()
	if deps == nil {
		return nil, errTlsCertsUnavailable
	}
	result, err := deps.Service.Certs(clientSession.Ctx, sidecar.Id(clientSession.ByJwt.NetworkId))
	if err != nil {
		return nil, tlsCertsError("certs", err)
	}
	return result, nil
}

// POST /tls/cert-revoke's answer.
type TlsCertRevokeResult struct {
	State sidecar.TlsCertState `json:"state"`
}

// What POST /tls/cert-revoke's handler read of the API key its request
// authenticated with (QD-847): the key's id, and the scopes its row holds, nil
// for a legacy key, which has none of its own.
type TlsCertRevokeApiKey struct {
	ApiKeyId server.Id
	Scopes   []string
}

// Whether a team member's network credential may revoke any certificate of
// its network: its role holds devices (§7.5), the permission over the
// network's clients and devices. A role that cannot be read holds nothing.
// Tests replace it.
var tlsCertRevokeMemberPermitted = func(ctx context.Context, networkId server.Id, memberId server.Id) bool {
	role, ok := model.MemberRole(ctx, networkId, memberId)
	return ok && model.RoleAndScopesPermit(role, nil, model.RolePermissionDevices)
}

// Whether the API key the session authenticated with may revoke any
// certificate of its network (QD-847): the handler read the session's own
// key, its scopes reach devices (§7.8), and, since a team's key acts as its
// creator's membership, that member's role holds devices too. A key the
// handler read nothing of, or another key than the session's, reaches nothing.
func tlsCertRevokeApiKeyPermitted(clientSession *session.ClientSession, apiKey *TlsCertRevokeApiKey) bool {
	if apiKey == nil || clientSession.ApiKeyId == nil || apiKey.ApiKeyId != *clientSession.ApiKeyId {
		return false
	}
	if !model.ScopesPermit(apiKey.Scopes, model.RolePermissionDevices) {
		return false
	}
	byJwt := clientSession.ByJwt
	return byJwt.MemberId == nil || tlsCertRevokeMemberPermitted(clientSession.Ctx, byJwt.NetworkId, *byJwt.MemberId)
}

// POST /tls/cert-revoke: marks a certificate revoking with its reason,
// keyCompromise when the body says so and cessationOfOperation otherwise,
// which certs' revocation chain takes to its CA. A client token revokes its
// own client's certificate. A network credential, which names no client, revokes any of
// its network's: a personal network's owner's, a team member's whose role
// holds devices, or an API key's whose scopes reach devices, which the
// handler reads from the key's row and passes as apiKey (QD-847); an OAuth
// identity or an admin token, whose scopes this route does not read, none. A
// certificate of another network is not found. One already on its way out
// answers its state, and a word of compromise about one the CA has not taken
// yet replaces its reason.
func TlsCertRevoke(args *sidecar.TlsCertRevokeArgs, apiKey *TlsCertRevokeApiKey, clientSession *session.ClientSession) (*TlsCertRevokeResult, error) {
	deps := tlsCertsDeps.Load()
	if deps == nil {
		return nil, errTlsCertsUnavailable
	}
	if args == nil || args.CertId == (sidecar.Id{}) {
		return nil, errTlsCertsRequest
	}
	ctx := clientSession.Ctx
	row, err := deps.Store.GetCert(ctx, args.CertId)
	if err != nil {
		return nil, tlsCertsError("cert-revoke", err)
	}
	if row.NetworkId != sidecar.Id(clientSession.ByJwt.NetworkId) {
		return nil, errTlsCertsNotFound
	}
	byJwt := clientSession.ByJwt
	switch {
	case byJwt.ClientId != nil:
		if row.ClientId != sidecar.Id(*byJwt.ClientId) {
			return nil, errTlsCertsNotOwn
		}
	case clientSession.UnsignedIdentity || clientSession.AdminCommand != nil || clientSession.OauthClient != nil:
		// an OAuth identity's and an admin token's scopes are not read here, so neither reaches another client's certificate
		return nil, errTlsCertsRoleRefused
	case clientSession.ApiKeyAuthenticated:
		if !tlsCertRevokeApiKeyPermitted(clientSession, apiKey) {
			return nil, errTlsCertsRoleRefused
		}
	case byJwt.MemberId != nil:
		if !tlsCertRevokeMemberPermitted(ctx, byJwt.NetworkId, *byJwt.MemberId) {
			return nil, errTlsCertsRoleRefused
		}
	}
	reason := sidecar.TlsCertRevocationReasonCessationOfOperation
	if args.KeyCompromise {
		reason = sidecar.TlsCertRevocationReasonKeyCompromise
	}
	from := []sidecar.TlsCertState{sidecar.TlsCertStateIssued}
	switch row.State {
	case sidecar.TlsCertStateRevoked:
		return &TlsCertRevokeResult{State: row.State}, nil
	case sidecar.TlsCertStateRevoking:
		if !args.KeyCompromise || row.RevocationReason == reason {
			return &TlsCertRevokeResult{State: row.State}, nil
		}
		// the CA has not taken it yet, and is to hear of the compromise
		from = append(from, sidecar.TlsCertStateRevoking)
	case sidecar.TlsCertStateIssued:
	default:
		return nil, errTlsCertsNotRevocable
	}
	moved, err := deps.Store.TransitionCert(ctx, &certsservice.CertsTransition{
		CertId:           row.CertId,
		From:             from,
		To:               sidecar.TlsCertStateRevoking,
		At:               server.NowUtc(),
		RevocationReason: reason,
	})
	if err != nil {
		return nil, tlsCertsError("cert-revoke", err)
	}
	if !moved {
		// another pass moved it between the read and the write: answer what it is now
		row, err = deps.Store.GetCert(ctx, args.CertId)
		if err != nil {
			return nil, tlsCertsError("cert-revoke", err)
		}
		if row.State != sidecar.TlsCertStateRevoking && row.State != sidecar.TlsCertStateRevoked {
			return nil, errTlsCertsNotRevocable
		}
		return &TlsCertRevokeResult{State: row.State}, nil
	}
	return &TlsCertRevokeResult{State: sidecar.TlsCertStateRevoking}, nil
}

// §13.15's custom-domain routes (TLS-M6-05; DESIGN-DETAILS 17.2.5, QD-175), each under a certs service token whose
// claims name the caller and never the body:
//
//	POST /tls/cert-order           the custom branch: a CSR naming one name under no URnetwork root
//	GET  /tls/certs?work=1         an ordering desktop's accounts and work list
//	POST /tls/custom-claim         take or renew an account's lock
//	POST /tls/custom-progress      what the holder learned at the CA
//	POST /tls/cert-register        the chain of the row the caller holds the lock for
//	POST /tls/custom-account       create the account row; begin, end, abort or repair a key change
//	POST /tls/custom-challenge     a delegated DNS-01 value, which certs writes (QD-175, QD-1165)
//	GET  /tls/custom-challenge/<id> whether the dns edges serve it
//
// The caller is built from the token and the network's verified projection (TlsCustomDeps.Caller): its §8 publish
// grants by owner label and whether its user is an Owner or Admin. Every operation is model.TlsCustomStore's, one
// transaction each; a refusal is a *sidecar.TlsCustomError, whose body and status the router answers. Nothing here
// reaches a CA: the ordering desktops do, under the customer's own account. A process with nothing installed
// answers 503, and the custom branch of an order is chosen before certsservice.Accept is reached, so no custom CSR
// is ever queued for certs' own passes (17.3's C9). Nothing here reaches a zone either: certs' DNS credential is
// taskworker's alone (QD-027), so the challenge routes hand their writes and reads to the certs worker's command
// (tls_custom_challenge_work.go) through a receipt (model's tls_custom_challenge_model.go).

// What the custom routes run on, beside TlsCertsDeps. Every field but Challenges is required.
type TlsCustomDeps struct {
	Store *model.TlsCustomStore
	// The caller of a certs token at now, from its network's verified projection. An error is a projection that
	// cannot answer, and the route answers 503.
	Caller func(ctx context.Context, principal *sidecar.Principal, now time.Time) (*model.TlsCustomCaller, error)
	// certs' roots, under none of which a custom host sits: the custom branch's test.
	Roots []string
	// How long a device waits before asking for its custom order again.
	OrderRetryAfter time.Duration
	// The challenge handoff's tunables (QD-1165). nil takes model.DefaultTlsCustomChallengeSettings.
	Challenges *model.TlsCustomChallengeSettings
}

// The challenge handoff's tunables.
func (self *TlsCustomDeps) challengeSettings() *model.TlsCustomChallengeSettings {
	if self.Challenges != nil {
		return self.Challenges
	}
	return model.DefaultTlsCustomChallengeSettings()
}

var tlsCustomDeps atomic.Pointer[TlsCustomDeps]

// Installs what this process's custom routes run on; nil, deps missing a field, or challenge settings that do not
// validate remove it.
func SetTlsCustom(deps *TlsCustomDeps) {
	if deps == nil || deps.Store == nil || deps.Caller == nil || len(deps.Roots) == 0 || (deps.Challenges != nil && deps.Challenges.Validate() != nil) {
		tlsCustomDeps.Store(nil)
		return
	}
	tlsCustomDeps.Store(deps)
}

// The caller of a certs token: 401 for a token that will not authenticate, 503 where nothing can be said.
func tlsCustomCaller(ctx context.Context, token string) (*TlsCustomDeps, *model.TlsCustomCaller, error) {
	deps := tlsCertsDeps.Load()
	customDeps := tlsCustomDeps.Load()
	if deps == nil || customDeps == nil {
		return nil, nil, errTlsCertsUnavailable
	}
	principal, err := tlsCertsPrincipal(ctx, deps, token)
	if err != nil {
		return nil, nil, err
	}
	if principal.SidecarId == nil {
		return nil, nil, errTlsCertsNotSidecar
	}
	caller, err := customDeps.Caller(ctx, principal, server.NowUtc())
	if err != nil || caller == nil {
		glog.Infof("[tls]custom caller unavailable: %v\n", err)
		return nil, nil, errTlsCertsUnavailable
	}
	return customDeps, caller, nil
}

// A store error as the route answers it: a refusal's own, and 503 for anything else, whose cause stays in the log.
func tlsCustomError(route string, err error) error {
	var refusal *sidecar.TlsCustomError
	if errors.As(err, &refusal) {
		return refusal
	}
	glog.Infof("[tls]%s unavailable: %v\n", route, err)
	return errTlsCertsUnavailable
}

// Whether a CSR takes the custom branch of POST /tls/cert-order: one name under no URnetwork root.
func tlsCertOrderIsCustom(args *sidecar.TlsCertOrderArgs) bool {
	customDeps := tlsCustomDeps.Load()
	if args == nil {
		return false
	}
	roots := []string{}
	if customDeps != nil {
		roots = customDeps.Roots
	}
	_, custom := certsservice.CertsCustomCsrHost(args.Csr, roots)
	// with no roots installed a CSR for a network name could read as custom: no custom branch then
	return custom && customDeps != nil
}

// POST /tls/cert-order's custom branch (17.2.5).
func tlsCustomCertOrder(ctx context.Context, token string, args *sidecar.TlsCertOrderArgs) (*sidecar.TlsCertOrderResult, error) {
	customDeps, caller, err := tlsCustomCaller(ctx, token)
	if err != nil {
		return nil, err
	}
	result, err := customDeps.Store.QueueOrder(ctx, caller, args, server.NewId(), customDeps.OrderRetryAfter)
	if err != nil {
		return nil, tlsCustomError("cert-order custom", err)
	}
	return result, nil
}

// GET /tls/certs?work=1.
func TlsCertsWork(ctx context.Context, token string) (*sidecar.TlsCertsResult, error) {
	customDeps, caller, err := tlsCustomCaller(ctx, token)
	if err != nil {
		return nil, err
	}
	result, err := customDeps.Store.Work(ctx, caller)
	if err != nil {
		return nil, tlsCustomError("certs work", err)
	}
	return result, nil
}

// POST /tls/custom-claim.
func TlsCustomClaim(ctx context.Context, token string, args *sidecar.TlsCustomClaimArgs) (*sidecar.TlsCustomClaimResult, error) {
	customDeps, caller, err := tlsCustomCaller(ctx, token)
	if err != nil {
		return nil, err
	}
	result, err := customDeps.Store.Claim(ctx, caller, args)
	if err != nil {
		return nil, tlsCustomError("custom-claim", err)
	}
	return result, nil
}

// POST /tls/custom-progress: an order_invalid answers the claim as it stands, any other event an empty claim.
func TlsCustomProgress(ctx context.Context, token string, args *sidecar.TlsCustomProgressArgs) (*sidecar.TlsCustomClaimResult, error) {
	customDeps, caller, err := tlsCustomCaller(ctx, token)
	if err != nil {
		return nil, err
	}
	result, err := customDeps.Store.Progress(ctx, caller, args)
	if err != nil {
		return nil, tlsCustomError("custom-progress", err)
	}
	if result == nil {
		result = &sidecar.TlsCustomClaimResult{}
	}
	return result, nil
}

// POST /tls/cert-register's answer.
type TlsCertRegisterResult struct {
	State sidecar.TlsCertState `json:"state"`
}

// POST /tls/cert-register.
func TlsCertRegister(ctx context.Context, token string, args *sidecar.TlsCertRegisterArgs) (*TlsCertRegisterResult, error) {
	customDeps, caller, err := tlsCustomCaller(ctx, token)
	if err != nil {
		return nil, err
	}
	if err := customDeps.Store.Register(ctx, caller, args); err != nil {
		return nil, tlsCustomError("cert-register", err)
	}
	return &TlsCertRegisterResult{State: sidecar.TlsCertStateCustomIssued}, nil
}

// POST /tls/custom-account.
func TlsCustomAccountOp(ctx context.Context, token string, args *sidecar.TlsCustomAccountArgs) (*sidecar.TlsCustomAccount, error) {
	customDeps, caller, err := tlsCustomCaller(ctx, token)
	if err != nil {
		return nil, err
	}
	result, err := customDeps.Store.AccountOp(ctx, caller, args)
	if err != nil {
		return nil, tlsCustomError("custom-account", err)
	}
	return result, nil
}

// A challenge GET names no receipt of the caller's.
var errTlsCustomChallengeNotFound = fmt.Errorf("%d Challenge not found.", http.StatusNotFound)

// Whether value is a DNS-01 value: base64url, unpadded, of a SHA-256 digest (RFC 8555 §8.4).
func tlsCustomChallengeValueValid(value string) bool {
	if len(value) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return false
	}
	digest, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(digest) == sha256.Size
}

// A refused receipt's answer: 409 claim, 404 no_account and 429 budget with the window's wait as the custom routes
// answer them, and 503 for a write the DNS path refused otherwise or that was never made.
func tlsCustomChallengeRefusal(receipt *model.TlsCustomChallengeReceipt) error {
	refusal := func(status int, reason string) error {
		return &sidecar.TlsCustomError{Status: status, Refusal: sidecar.TlsCustomRefusal{Reason: reason, RetryAfterMs: tlsCustomChallengeMs(receipt.RetryAfter)}}
	}
	switch receipt.Refusal {
	case model.TlsCustomChallengeRefusalClaim:
		return refusal(http.StatusConflict, sidecar.TlsCustomRefusalClaim)
	case model.TlsCustomChallengeRefusalNoAccount:
		return refusal(http.StatusNotFound, sidecar.TlsCustomRefusalNoAccount)
	case model.TlsCustomChallengeRefusalBudget:
		return refusal(http.StatusTooManyRequests, sidecar.TlsCustomRefusalBudget)
	}
	glog.Infof("[tls]custom-challenge %s refused: %s\n", receipt.ChallengeId, receipt.Refusal)
	return errTlsCertsUnavailable
}

// Milliseconds of d from the answer, at least 1 for a positive wait, saturating.
func tlsCustomChallengeMs(d time.Duration) uint32 {
	if d <= 0 {
		return 0
	}
	ms := (d + time.Millisecond - 1) / time.Millisecond
	return uint32(min(ms, time.Duration(^uint32(0))))
}

// Reads the receipt again every pollInterval until done holds or wait passes: the receipt as last read, nil when it
// went past its retention. ctx's end ends the wait with its error.
func waitTlsCustomChallenge(ctx context.Context, store *model.TlsCustomStore, receipt *model.TlsCustomChallengeReceipt, wait time.Duration, pollInterval time.Duration, done func(*model.TlsCustomChallengeReceipt) bool) (*model.TlsCustomChallengeReceipt, error) {
	timeout := time.After(wait)
	for !done(receipt) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout:
			return receipt, nil
		case <-time.After(pollInterval):
		}
		next, err := store.ChallengeReceipt(ctx, receipt.ChallengeId)
		if err != nil || next == nil {
			return next, err
		}
		receipt = next
	}
	return receipt, nil
}

// POST /tls/custom-challenge (QD-175, QD-1165): the delegated DNS-01 value at the domain's <acme_token>.acme.<root>,
// which certs, the only writer, writes from taskworker. api captures the write as a receipt under the caller's claim
// on the account's lock, queuing the certs worker's command in the same transaction
// (model.TlsCustomStore.CaptureChallenge), then waits up to WriteWait for the serial the write took. A retry under
// the claim with the same value recovers the receipt, never a second write, and after a stale answer owes a new one.
// A wait that ends with the write still owed answers 503, the receipt kept for the retry.
func TlsCustomChallenge(ctx context.Context, token string, args *sidecar.TlsCustomChallengeArgs) (*sidecar.TlsCustomChallengeResult, error) {
	customDeps, caller, err := tlsCustomCaller(ctx, token)
	if err != nil {
		return nil, err
	}
	if args == nil || args.Domain == "" || !tlsCustomChallengeValueValid(args.Value) {
		return nil, errTlsCertsRequest
	}
	settings := customDeps.challengeSettings()
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	receipt, err := customDeps.Store.CaptureChallenge(ctx, caller, args.Domain, args.Value, settings, func(tx server.PgTx, challengeId server.Id) {
		scheduleTlsCustomChallengeWorkInTx(tx, owner, challengeId, time.Time{}, settings)
	})
	if err != nil {
		return nil, tlsCustomError("custom-challenge", err)
	}
	tlsCustomChallengeBarrier(TlsCustomChallengeBarrierCaptured, receipt.ChallengeId)
	receipt, err = waitTlsCustomChallenge(ctx, customDeps.Store, receipt, settings.WriteWait, settings.PollInterval, func(receipt *model.TlsCustomChallengeReceipt) bool {
		return receipt.State == model.TlsCustomChallengeStateWritten || receipt.State == model.TlsCustomChallengeStateRefused
	})
	switch {
	case err != nil:
		return nil, tlsCustomError("custom-challenge", err)
	case receipt == nil:
		return nil, errTlsCertsUnavailable
	case receipt.State == model.TlsCustomChallengeStateWritten:
		return &sidecar.TlsCustomChallengeResult{ChallengeId: receipt.Serial}, nil
	case receipt.State == model.TlsCustomChallengeStateRefused:
		return nil, tlsCustomChallengeRefusal(receipt)
	}
	// still owed: the receipt waits for the retry
	return nil, errTlsCertsUnavailable
}

// GET /tls/custom-challenge/<id> (QD-175, QD-1165): GET /dns/ready's answer for the serial a write took, every answer
// passed through, which the certs worker reads, since api holds no DNS credential. The caller's own receipt of its
// network and sidecar written at the serial answers, else 404. stale is final for the serial, and an answer younger
// than ReadyFreshness is answered as it is; otherwise the worker is asked for a fresh read and the route waits up to
// ReadyWait for it, answering waiting when none comes in time, as /dns/ready does when its hold ends.
func TlsCustomChallengeStatus(ctx context.Context, token string, challengeId uint64) (*sidecar.TlsCustomChallengeResult, error) {
	customDeps, caller, err := tlsCustomCaller(ctx, token)
	if err != nil {
		return nil, err
	}
	if challengeId == 0 {
		return nil, errTlsCustomChallengeNotFound
	}
	settings := customDeps.challengeSettings()
	answer := func(readiness string) *sidecar.TlsCustomChallengeResult {
		return &sidecar.TlsCustomChallengeResult{ChallengeId: challengeId, Readiness: readiness}
	}
	receipt, err := customDeps.Store.ChallengeReceiptBySerial(ctx, caller, challengeId)
	if err != nil {
		return nil, tlsCustomError("custom-challenge status", err)
	}
	if receipt == nil {
		return nil, errTlsCustomChallengeNotFound
	}
	if receipt.ReadyAnswer == model.TlsCustomChallengeReadyStale || (receipt.ReadyAnswer != "" && receipt.Now.Sub(receipt.ReadyTime) < settings.ReadyFreshness) {
		return answer(receipt.ReadyAnswer), nil
	}
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	requestTime, err := customDeps.Store.RequestChallengeReadiness(ctx, receipt.ChallengeId, challengeId, func(tx server.PgTx, challengeId server.Id) {
		scheduleTlsCustomChallengeWorkInTx(tx, owner, challengeId, time.Time{}, settings)
	})
	if err != nil {
		return nil, tlsCustomError("custom-challenge status", err)
	}
	if requestTime.IsZero() {
		// the receipt moved since the read: a stale answer recorded, a new write under the claim, or its retention's end
		receipt, err = customDeps.Store.ChallengeReceiptBySerial(ctx, caller, challengeId)
		switch {
		case err != nil:
			return nil, tlsCustomError("custom-challenge status", err)
		case receipt == nil:
			return nil, errTlsCustomChallengeNotFound
		case receipt.ReadyAnswer == model.TlsCustomChallengeReadyStale:
			return answer(receipt.ReadyAnswer), nil
		}
		return answer(string(certsservice.CertsDnsWaiting)), nil
	}
	tlsCustomChallengeBarrier(TlsCustomChallengeBarrierReadyAsked, receipt.ChallengeId)
	answered := func(receipt *model.TlsCustomChallengeReceipt) bool {
		return receipt.State != model.TlsCustomChallengeStateWritten || receipt.Serial != challengeId ||
			(receipt.ReadyAnswer != "" && !receipt.ReadyTime.Before(requestTime))
	}
	receipt, err = waitTlsCustomChallenge(ctx, customDeps.Store, receipt, settings.ReadyWait, settings.PollInterval, answered)
	switch {
	case err != nil:
		return nil, tlsCustomError("custom-challenge status", err)
	case receipt == nil || receipt.State != model.TlsCustomChallengeStateWritten || receipt.Serial != challengeId:
		// the receipt went, or a new write under the claim replaced this serial's
		return nil, errTlsCustomChallengeNotFound
	case receipt.ReadyAnswer != "" && (receipt.ReadyAnswer == model.TlsCustomChallengeReadyStale || !receipt.ReadyTime.Before(requestTime)):
		return answer(receipt.ReadyAnswer), nil
	}
	return answer(string(certsservice.CertsDnsWaiting)), nil
}
