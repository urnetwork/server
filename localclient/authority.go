// Package localclient dispatches hosted client control through the platform's
// authenticated model/controller boundaries without a public API round trip.
package localclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// One hosted parent owns its derived clients and notification publisher.
// Credential state is locked only for a scalar copy; database/controller calls
// run outside that lock. Close follows the hosted device's completed join.
type Authority struct {
	networkId     server.Id
	userId        server.Id
	clientId      server.Id
	deviceId      server.Id
	apiUrl        string
	stateLock     sync.Mutex
	parentJwt     string
	notifications *model.ContractOriginNotifications
	cancel        context.CancelFunc
	closeOnce     sync.Once
	closed        atomic.Bool
	// the hosting process's appearance counter, nil when it has none; the
	// process owns and closes it
	appearances *model.ProviderAppearances
}

// Signature, audience, current client and credential rotation are checked now
// and again on every operation. A local address is never an authentication.
func New(ctx context.Context, token, apiUrl string) (*Authority, error) {
	ctx = jwt.WithStateQuerySource(ctx, jwt.StateQueryHostedBootstrap)
	return server.HandleError2(func() (*Authority, error) {
		claims, err := jwt.ParseByJwtForAudience(ctx, token, jwt.ByJwtAudienceApi)
		if err != nil {
			return nil, err
		}
		if err = jwt.ValidateByJwtState(ctx, claims, true); err != nil {
			return nil, err
		}
		base, err := url.Parse(strings.TrimRight(apiUrl, "/"))
		if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Scheme != "http" && base.Scheme != "https" {
			return nil, errors.New("local API origin is invalid")
		}
		publisherCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		return &Authority{networkId: claims.NetworkId, userId: claims.UserId, clientId: *claims.ClientId, deviceId: *claims.DeviceId, apiUrl: strings.TrimRight(apiUrl, "/"), parentJwt: token,
			notifications: model.NewContractOriginNotifications(publisherCtx, model.DefaultContractOriginNotificationSettings()), appearances: model.GetProviderAppearances(ctx), cancel: cancel}, nil
	}, func(err error) (*Authority, error) { return nil, err })
}

// Must be called only after the hosted device has joined every control owner.
func (self *Authority) Close() {
	self.closeOnce.Do(func() { self.closed.Store(true); self.cancel(); self.notifications.Close() })
}

func (self *Authority) token() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.parentJwt
}

func (self *Authority) authenticate(ctx context.Context, token string, parentOnly bool) (*session.ClientSession, error) {
	claims, err := self.parseClaimsPreflight(ctx, token, parentOnly)
	if err != nil {
		return nil, err
	}
	if *claims.ClientId == self.clientId {
		err = jwt.ValidateByJwtState(ctx, claims, true)
	} else {
		err = jwt.ValidateByJwtStateForParent(ctx, claims, self.clientId)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &connect.HttpStatusError{StatusCode: http.StatusUnauthorized}
	}
	return self.newSession(ctx, claims), nil
}

// Signature, audience and owning account/parent binding are local preflight.
// Child source ownership and live state are checked together by authenticate;
// parent-only mint instead validates live state in its writing transaction.
func (self *Authority) parseClaimsPreflight(ctx context.Context, token string, parentOnly bool) (*jwt.ByJwt, error) {
	if self.closed.Load() {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	claims, err := jwt.ParseByJwtForAudience(ctx, token, jwt.ByJwtAudienceApi)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &connect.HttpStatusError{StatusCode: http.StatusUnauthorized}
	}
	if claims.NetworkId != self.networkId || claims.UserId != self.userId || claims.ClientId == nil || claims.DeviceId == nil {
		return nil, &connect.HttpStatusError{StatusCode: http.StatusUnauthorized}
	}
	if *claims.ClientId == self.clientId {
		if *claims.DeviceId != self.deviceId {
			return nil, &connect.HttpStatusError{StatusCode: http.StatusUnauthorized}
		}
	} else if parentOnly {
		return nil, &connect.HttpStatusError{StatusCode: http.StatusUnauthorized}
	}
	return claims, nil
}

// The operation owns the session and must validate its claims before writing.
func (self *Authority) newSession(ctx context.Context, claims *jwt.ByJwt) *session.ClientSession {
	ctx = model.WithContractOriginNotifications(ctx, self.notifications)
	if self.appearances != nil {
		ctx = model.WithProviderAppearances(ctx, self.appearances)
	}
	return session.NewLocalClientSession(ctx, "0.0.0.0:0", claims)
}

// Restored window identities belong to the durable parent, not merely an
// in-memory minted set. The primary-key read also fences another hosted client
// in the same network; it never locks a shared financial row.
func (self *Authority) ownsChild(ctx context.Context, id server.Id) bool {
	var owned bool
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM network_client WHERE client_id=$1 AND source_client_id=$2 AND network_id=$3 AND active=true)`, id, self.clientId, self.networkId).Scan(&owned))
	})
	return owned
}

// Typed conversion keeps the existing wire schema without maintaining a
// second partial copy of discovery, credential or error result fields.
func convert[T any](value any) (*T, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var out T
	err = json.Unmarshal(b, &out)
	return &out, err
}

func (self *Authority) AuthNetworkClient(ctx context.Context, args *connect.AuthNetworkClientArgs) (*connect.AuthNetworkClientResult, error) {
	ctx = jwt.WithStateQuerySource(ctx, jwt.StateQueryHostedMint)
	return server.HandleError2(func() (*connect.AuthNetworkClientResult, error) {
		if args == nil || args.ClientId != nil || args.SourceClientId == nil || server.Id(*args.SourceClientId) != self.clientId {
			return nil, errors.New("local mint requires its owning parent")
		}
		claims, err := self.parseClaimsPreflight(ctx, self.token(), true)
		if err != nil {
			return nil, err
		}
		s := self.newSession(ctx, claims)
		defer s.Cancel()
		request, err := convert[model.AuthNetworkClientArgs](args)
		if err != nil {
			return nil, err
		}
		result, err := controller.AuthNetworkClientFromParent(request, s)
		if errors.Is(err, model.ErrClientParentInactive) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &connect.HttpStatusError{StatusCode: http.StatusUnauthorized}
		}
		if err != nil {
			return nil, err
		}
		return convert[connect.AuthNetworkClientResult](result)
	}, func(err error) (*connect.AuthNetworkClientResult, error) { return nil, err })
}

func (self *Authority) RemoveNetworkClient(ctx context.Context, args *connect.RemoveNetworkClientArgs) (*connect.RemoveNetworkClientResult, error) {
	ctx = jwt.WithStateQuerySource(ctx, jwt.StateQueryHostedRetire)
	return server.HandleError2(func() (*connect.RemoveNetworkClientResult, error) {
		if args == nil || server.Id(args.ClientId) == self.clientId || !self.ownsChild(ctx, server.Id(args.ClientId)) {
			return nil, errors.New("local retirement requires an owned derived client")
		}
		s, err := self.authenticate(ctx, self.token(), true)
		if err != nil {
			return nil, err
		}
		defer s.Cancel()
		result, err := model.RemoveNetworkClient(&model.RemoveNetworkClientArgs{ClientId: server.Id(args.ClientId)}, s)
		if err != nil {
			return nil, err
		}
		return convert[connect.RemoveNetworkClientResult](result)
	}, func(err error) (*connect.RemoveNetworkClientResult, error) { return nil, err })
}

func (self *Authority) ConnectControl(ctx context.Context, token string, args *connect.ConnectControlArgs) (*connect.ConnectControlResult, error) {
	ctx = jwt.WithStateQuerySource(ctx, jwt.StateQueryHostedControl)
	return server.HandleError2(func() (*connect.ConnectControlResult, error) {
		if args == nil {
			return nil, errors.New("local control request is absent")
		}
		s, err := self.authenticate(ctx, token, false)
		if err != nil {
			return nil, err
		}
		defer s.Cancel()
		result, err := controller.ConnectControl(&controller.ConnectControlArgs{Pack: args.Pack}, s)
		if err != nil {
			return nil, err
		}
		return convert[connect.ConnectControlResult](result)
	}, func(err error) (*connect.ConnectControlResult, error) {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, err
	})
}

func (self *Authority) FindProviders2(ctx context.Context, token string, args *connect.FindProviders2Args) (*connect.FindProviders2Result, error) {
	ctx = jwt.WithStateQuerySource(ctx, jwt.StateQueryHostedDiscovery)
	return server.HandleError2(func() (*connect.FindProviders2Result, error) {
		if args == nil {
			return nil, errors.New("local discovery request is absent")
		}
		s, err := self.authenticate(ctx, token, true)
		if err != nil {
			return nil, err
		}
		defer s.Cancel()
		request, err := convert[model.FindProviders2Args](args)
		if err != nil {
			return nil, err
		}
		result, err := model.FindProviders2(request, s)
		if err != nil {
			return nil, err
		}
		return convert[connect.FindProviders2Result](result)
	}, func(err error) (*connect.FindProviders2Result, error) { return nil, err })
}

func (self *Authority) requestPath(requestUrl string) (string, error) {
	if !strings.HasPrefix(requestUrl, self.apiUrl+"/") {
		return "", errors.New("local API request escaped its origin")
	}
	u, err := url.Parse(requestUrl)
	if err != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("local API request URL is invalid")
	}
	return strings.TrimPrefix(requestUrl, self.apiUrl), nil
}

// Only owned control operations are dispatched. Unsupported calls fail closed
// locally, never retry through HTTP under another authority.
func (self *Authority) Get(ctx context.Context, requestUrl, token string) ([]byte, error) {
	return server.HandleError2(func() ([]byte, error) {
		path, err := self.requestPath(requestUrl)
		if err != nil {
			return nil, err
		}
		ctx = jwt.WithStateQuerySource(ctx, authorityStateQuerySource("GET", path))
		if token == "" && strings.HasPrefix(path, "/key/") {
			token = self.token()
		}
		s, err := self.authenticate(ctx, token, true)
		if err != nil {
			return nil, err
		}
		defer s.Cancel()
		var result any
		switch path {
		case "/auth/refresh":
			var refreshed *controller.RefreshTokenResult
			refreshed, err = controller.RefreshToken(s)
			result = refreshed
			if err == nil && refreshed != nil && refreshed.Error == nil && refreshed.ByJwt != "" {
				self.stateLock.Lock()
				if self.parentJwt == token {
					self.parentJwt = refreshed.ByJwt
				}
				self.stateLock.Unlock()
			}
		case "/subscription/balance":
			result, err = controller.SubscriptionBalanceForStorefront("", s)
		case "/network/provider-locations":
			result, err = model.GetProviderLocations(s)
		case "/network/clients":
			result, err = model.GetNetworkClients(s)
		case "/network/peers":
			result, err = model.GetNetworkPeersForSession(s)
		default:
			parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
			if len(parts) < 2 || parts[0] != "key" {
				return nil, errors.New("unsupported local API operation")
			}
			id, parseErr := server.ParseId(parts[1])
			if parseErr != nil {
				return nil, parseErr
			}
			if len(parts) == 2 {
				result, err = controller.GetClientKey(&controller.GetClientKeyArgs{ClientId: id}, s)
			} else if len(parts) == 3 && parts[2] == "history" {
				result, err = controller.GetClientKeyHistory(&controller.GetClientKeyHistoryArgs{ClientId: id}, s)
			} else {
				return nil, errors.New("unsupported local key operation")
			}
		}
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	}, func(err error) ([]byte, error) { return nil, err })
}

func (self *Authority) Post(ctx context.Context, requestUrl string, body []byte, token string) ([]byte, error) {
	return server.HandleError2(func() ([]byte, error) {
		path, err := self.requestPath(requestUrl)
		if err != nil {
			return nil, err
		}
		ctx = jwt.WithStateQuerySource(ctx, authorityStateQuerySource("POST", path))
		if len(body) > 1024*1024 {
			return nil, errors.New("local control request exceeds bound")
		}
		if path == "/connect/control" {
			var args connect.ConnectControlArgs
			if err = json.Unmarshal(body, &args); err != nil {
				return nil, err
			}
			r, e := self.ConnectControl(ctx, token, &args)
			if e != nil {
				return nil, e
			}
			return json.Marshal(r)
		}
		s, err := self.authenticate(ctx, token, true)
		if err != nil {
			return nil, err
		}
		defer s.Cancel()
		var result any
		switch path {
		case "/network/find-providers2":
			var args model.FindProviders2Args
			if err = json.Unmarshal(body, &args); err == nil {
				result, err = model.FindProviders2(&args, s)
			}
		case "/network/find-provider-locations":
			var args model.FindLocationsArgs
			if err = json.Unmarshal(body, &args); err == nil {
				result, err = model.FindProviderLocations(&args, s)
			}
		case "/network/find-locations":
			var args model.FindLocationsArgs
			if err = json.Unmarshal(body, &args); err == nil {
				result, err = model.FindLocations(&args, s)
			}
		case "/network/auth-client":
			var args connect.AuthNetworkClientArgs
			if err = json.Unmarshal(body, &args); err == nil {
				result, err = self.AuthNetworkClient(ctx, &args)
			}
		case "/network/remove-client":
			var args connect.RemoveNetworkClientArgs
			if err = json.Unmarshal(body, &args); err == nil {
				result, err = self.RemoveNetworkClient(ctx, &args)
			}
		default:
			return nil, errors.New("unsupported local API operation")
		}
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	}, func(err error) ([]byte, error) { return nil, err })
}
