package controller

// §13.15's certificate routes at M5 (TLS-M5-16; R-TLS-3, R-TLS-4, D-TLS-6):
//
//	POST /tls/cert-order       a service token, aud certs: the caller's CSR and attestation, queued
//	GET  /tls/cert-order/<id>  the same token: the caller's own order
//	GET  /tls/certs            a client token: its network's certificates and attestations
//	POST /tls/cert-revoke      a client token for its own certificate, a network admin's credential for any of its network's
//
// An order's subject is the token's and never the body's: its network, client
// and sidecar are the verified claims, and its identifier set, identity key
// and §8 grant come from the network's newest verified projection through
// this process's installed deps. Nothing here reaches a CA: a route writes or
// reads a row, and certs' taskworker chains do the rest (TLS-M5-17).
//
// What the routes run on is installed per process (SetTlsCerts), as a gate's
// states are. A process with nothing installed answers every route 503, and a
// subject the projection cannot answer for is 503 too, never an order with no
// grant check. An order the gate does not grant is 403, a CSR or attestation
// that does not check 400, and an order or certificate that is not the
// caller's 404, the same as one that does not exist.

import (
	"context"
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
	subject, err := deps.Subject(ctx, principal, server.NowUtc())
	if err != nil || subject == nil {
		// the projection cannot answer: no order is taken on a doubt
		glog.Infof("[tls]cert-order has no subject: %v\n", err)
		return nil, errTlsCertsUnavailable
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

// Whether a team member's network credential may revoke any certificate of
// its network: its role holds devices (§7.5), the permission over the
// network's clients and devices. A role that cannot be read holds nothing.
// Tests replace it.
var tlsCertRevokeMemberPermitted = func(ctx context.Context, networkId server.Id, memberId server.Id) bool {
	role, ok := model.MemberRole(ctx, networkId, memberId)
	return ok && model.RoleAndScopesPermit(role, nil, model.RolePermissionDevices)
}

// POST /tls/cert-revoke: marks a certificate revoking, which certs'
// revocation chain takes to its CA. A client token revokes its own client's
// certificate. A network credential, which names no client, revokes any of
// its network's: a personal network's owner's, or a team member's whose role
// holds devices; an API key, whose scopes this route does not read, none. A
// certificate of another network is not found. One already on its way out
// answers its state.
func TlsCertRevoke(args *sidecar.TlsCertRevokeArgs, clientSession *session.ClientSession) (*TlsCertRevokeResult, error) {
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
	case clientSession.ApiKeyAuthenticated:
		// an API key's scopes are not read here, so none reaches another client's certificate
		return nil, errTlsCertsRoleRefused
	case byJwt.MemberId != nil:
		if !tlsCertRevokeMemberPermitted(ctx, byJwt.NetworkId, *byJwt.MemberId) {
			return nil, errTlsCertsRoleRefused
		}
	}
	switch row.State {
	case sidecar.TlsCertStateRevoking, sidecar.TlsCertStateRevoked:
		return &TlsCertRevokeResult{State: row.State}, nil
	case sidecar.TlsCertStateIssued:
	default:
		return nil, errTlsCertsNotRevocable
	}
	moved, err := deps.Store.TransitionCert(ctx, &certsservice.CertsTransition{
		CertId: row.CertId,
		From:   []sidecar.TlsCertState{sidecar.TlsCertStateIssued},
		To:     sidecar.TlsCertStateRevoking,
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
