// The platform owns the prober identity and already has model access. Its
// private tunnels need not compete for a public API handler to mint clients.
package work

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

var egressProbeCredentialsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork", Subsystem: "egress_probe", Name: "internal_credentials_total",
	Help: "Completed internal prober credential operations; not public HTTP calls or measured provider coverage",
}, []string{"operation", "result"})

func init() {
	for _, operation := range []string{"mint", "retire"} {
		for _, result := range []string{"ok", "error", "timeout", "canceled"} {
			egressProbeCredentialsTotal.WithLabelValues(operation, result)
		}
	}
	prometheus.MustRegister(egressProbeCredentialsTotal)
}

// Immutable scalar ownership, copied from the stored singleton, not task
// arguments or the provider under test. Function fields are per-owner seams
// for deterministic tests; production uses the ordinary JWT/model boundaries.
type providerEgressCredentials struct {
	children        sync.Map // only identities minted by this immutable owner
	networkId       server.Id
	userId          server.Id
	clientId        server.Id
	parentJwt       string
	parse           func(context.Context, string, string) (*jwt.ByJwt, error)
	validate        func(context.Context, *jwt.ByJwt, bool) error
	mint            func(*model.AuthNetworkClientArgs, *session.ClientSession) (*model.AuthNetworkClientResult, error)
	retire          func(*model.RemoveNetworkClientArgs, *session.ClientSession) (*model.RemoveNetworkClientResult, error)
	captureResident func(context.Context, server.Id, server.Id) (*model.NetworkClientResidentRetirement, error)
	removeResident  func(context.Context, *model.NetworkClientResidentRetirement) (bool, error)
}

func newProviderEgressCredentials(identity *model.ProberIdentity) (*providerEgressCredentials, error) {
	if identity == nil || identity.NetworkId == nil || identity.UserId == nil || identity.ClientId == nil || identity.ByClientJwt == "" ||
		*identity.NetworkId == (server.Id{}) || *identity.UserId == (server.Id{}) || *identity.ClientId == (server.Id{}) {
		return nil, errors.New("provider egress prober identity is incomplete")
	}
	return &providerEgressCredentials{
		networkId: *identity.NetworkId, userId: *identity.UserId, clientId: *identity.ClientId, parentJwt: identity.ByClientJwt,
		parse: jwt.ParseByJwtForAudience, validate: jwt.ValidateByJwtState,
		mint: model.AuthNetworkClient, retire: model.RemoveNetworkClient,
		captureResident: model.CaptureResidentForClientRetirement, removeResident: model.RemoveCapturedResidentForClient,
	}, nil
}

func (self *providerEgressCredentials) parentSession(ctx context.Context) (*session.ClientSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	claims, err := self.parse(ctx, self.parentJwt, jwt.ByJwtAudienceApi)
	if err != nil {
		return nil, err
	}
	if claims == nil || claims.NetworkId != self.networkId || claims.UserId != self.userId || claims.ClientId == nil ||
		*claims.ClientId != self.clientId || claims.DeviceId == nil {
		return nil, errors.New("provider egress parent credential does not match stored identity")
	}
	// Preserve immediate revocation and credential-rotation checks on every
	// call. A cached bootstrap token is not permission to revive a dead client.
	if err := self.validate(ctx, claims, true); err != nil {
		return nil, err
	}
	return session.NewLocalClientSession(ctx, "0.0.0.0:0", claims), nil
}

func recordProviderEgressCredentialResult(operation string, err error) {
	if operation != "mint" && operation != "retire" {
		return
	}
	result := "ok"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		result = "timeout"
	case errors.Is(err, context.Canceled):
		result = "canceled"
	case err != nil:
		result = "error"
	}
	egressProbeCredentialsTotal.WithLabelValues(operation, result).Inc()
}

func (self *providerEgressCredentials) AuthNetworkClient(ctx context.Context, args *connect.AuthNetworkClientArgs) (result *connect.AuthNetworkClientResult, returnErr error) {
	defer func() { recordProviderEgressCredentialResult("mint", returnErr) }()
	return server.HandleError2(func() (*connect.AuthNetworkClientResult, error) {
		if args == nil || args.ClientId != nil || args.SourceClientId == nil || server.Id(*args.SourceClientId) != self.clientId {
			return nil, errors.New("provider egress mint requires its stored source client")
		}
		parent, err := self.parentSession(ctx)
		if err != nil {
			return nil, err
		}
		defer parent.Cancel()
		source := self.clientId
		minted, err := self.mint(&model.AuthNetworkClientArgs{
			SourceClientId: &source, Description: model.ProberClientDescription, DeviceSpec: model.ProberClientDeviceSpec,
		}, parent)
		if err != nil {
			return nil, err
		}
		if minted == nil {
			return nil, errors.New("provider egress mint returned no result")
		}
		if minted.Error != nil {
			return nil, fmt.Errorf("provider egress mint refused: %s", minted.Error.Message)
		}
		if minted.ClientId == nil || *minted.ClientId == self.clientId || minted.ByClientJwt == nil || *minted.ByClientJwt == "" {
			return nil, errors.New("provider egress mint returned no derived credential")
		}
		self.children.Store(*minted.ClientId, struct{}{})
		return &connect.AuthNetworkClientResult{ByClientJwt: *minted.ByClientJwt}, nil
	}, func(err error) (*connect.AuthNetworkClientResult, error) {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, err
	})
}

func (self *providerEgressCredentials) RemoveNetworkClient(ctx context.Context, args *connect.RemoveNetworkClientArgs) (result *connect.RemoveNetworkClientResult, returnErr error) {
	defer func() { recordProviderEgressCredentialResult("retire", returnErr) }()
	return server.HandleError2(func() (*connect.RemoveNetworkClientResult, error) {
		if args == nil || args.ClientId == (connect.Id{}) || server.Id(args.ClientId) == self.clientId {
			return nil, errors.New("provider egress cannot retire its parent client")
		}
		parent, err := self.parentSession(ctx)
		if err != nil {
			return nil, err
		}
		defer parent.Cancel()
		// The existing model's transactional network fence prevents this
		// authority from revoking another network's identity.
		retired, err := self.retire(&model.RemoveNetworkClientArgs{ClientId: server.Id(args.ClientId)}, parent)
		if err != nil {
			return nil, err
		}
		if retired == nil {
			return nil, errors.New("provider egress retirement returned no result")
		}
		if retired.Error != nil {
			return nil, fmt.Errorf("provider egress retirement refused: %s", retired.Error.Message)
		}
		self.children.Delete(server.Id(args.ClientId))
		return &connect.RemoveNetworkClientResult{}, nil
	}, func(err error) (*connect.RemoveNetworkClientResult, error) {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, err
	})
}
