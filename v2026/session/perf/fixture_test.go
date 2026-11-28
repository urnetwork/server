package perf

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// One network and its admin user, created through the model's fixture.
type perfNetwork struct {
	id   server.Id
	user server.Id
	name string
}

// A signed credential and the claims it carries.
type credential struct {
	claims *session.ByJwt
	jwt    string
}

var networkCounter atomic.Int64

func (self *harness) newNetwork(prefix string) *perfNetwork {
	network := &perfNetwork{id: server.NewId(), user: server.NewId()}
	network.name = fmt.Sprintf("perf%s%d", prefix, networkCounter.Add(1))
	model.Testing_CreateNetwork(self.ctx, network.id, network.name, network.user)
	return network
}

// The untagged network claims an API key authenticates as.
func (self *perfNetwork) apiKeyClaims() *session.ByJwt {
	return session.NewByJwt(self.id, self.user, self.name, false, false)
}

// A client session as the api builds one for an API key request.
func (self *harness) apiKeySession(network *perfNetwork) *session.ClientSession {
	clientSession := session.NewLocalClientSession(self.ctx, "192.0.2.10:1", network.apiKeyClaims())
	clientSession.ApiKeyAuthenticated = true
	return clientSession
}

func (self *harness) credentialSession(presented *credential) *session.ClientSession {
	return session.NewLocalClientSession(self.ctx, "192.0.2.11:1", presented.claims)
}

// A password sign-in: an independent session (MintNetworkSession).
func (self *harness) signIn(network *perfNetwork) (*credential, error) {
	claims := session.NewByJwt(network.id, network.user, network.name, false, false)
	signed, err := session.MintNetworkSession(self.ctx, claims, "password")
	if err != nil {
		return nil, err
	}
	return self.signed(signed)
}

// The credential a signed token carries, as a server reads it back.
func (self *harness) signed(signed string) (*credential, error) {
	claims, err := self.parse(signed)
	if err != nil {
		return nil, err
	}
	return &credential{claims: claims, jwt: signed}, nil
}

func (self *harness) parse(signed string) (*session.ByJwt, error) {
	return session.ParseByJwt(self.ctx, signed)
}

func proxyArgs() *model.AuthNetworkClientArgs {
	return &model.AuthNetworkClientArgs{
		Description: "perf proxy",
		DeviceSpec:  "proxy",
		ProxyConfig: &model.ProxyConfig{
			InitialDeviceState: &model.ExtendedProxyDeviceState{
				ProxyDeviceState: model.ProxyDeviceState{
					Location: &sdk.ConnectLocation{
						ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true},
					},
				},
			},
		},
	}
}

var errClientRefused = errors.New("auth-client refused")

// POST /network/auth-client through the model: a top-level client, or a hosted
// proxy with its proxy rows. An API key session gives the client its own
// session; a signed network credential's client inherits it.
func (self *harness) authClient(clientSession *session.ClientSession, proxy bool) (*credential, server.Id, string, error) {
	args := &model.AuthNetworkClientArgs{Description: "perf client", DeviceSpec: "perf"}
	if proxy {
		args = proxyArgs()
	}
	var result *model.AuthNetworkClientResult
	err := routeCall(func() error {
		var err error
		result, err = model.AuthNetworkClient(args, clientSession)
		return err
	})
	if err != nil {
		return nil, server.Id{}, "", err
	}
	if result.Error != nil || result.ClientId == nil || result.ByClientJwt == nil {
		message := "no client"
		if result.Error != nil {
			message = result.Error.Message
		}
		return nil, server.Id{}, message, fmt.Errorf("%w: %s", errClientRefused, message)
	}
	claims, err := self.parse(*result.ByClientJwt)
	if err != nil {
		return nil, server.Id{}, "", err
	}
	return &credential{claims: claims, jwt: *result.ByClientJwt}, *result.ClientId, "", nil
}

// A hosted derived child, as a proxy's local authority mints one: a child row
// under the parent client, inheriting the parent's session.
func (self *harness) childMint(parent *credential) (*credential, server.Id, error) {
	clientSession := self.credentialSession(parent)
	defer clientSession.Cancel()
	source := *parent.claims.ClientId
	var result *model.AuthNetworkClientResult
	err := routeCall(func() error {
		var err error
		result, err = model.AuthNetworkClientFromParent(&model.AuthNetworkClientArgs{
			SourceClientId: &source,
			Description:    "perf child",
			DeviceSpec:     "child",
		}, clientSession)
		return err
	})
	if err != nil {
		return nil, server.Id{}, err
	}
	if result.Error != nil || result.ClientId == nil || result.ByClientJwt == nil {
		message := "no client"
		if result.Error != nil {
			message = result.Error.Message
		}
		return nil, server.Id{}, fmt.Errorf("%w: %s", errClientRefused, message)
	}
	claims, err := self.parse(*result.ByClientJwt)
	if err != nil {
		return nil, server.Id{}, err
	}
	return &credential{claims: claims, jwt: *result.ByClientJwt}, *result.ClientId, nil
}

func (self *harness) removeClient(clientSession *session.ClientSession, clientId server.Id) error {
	var result *model.RemoveNetworkClientResult
	err := routeCall(func() error {
		var err error
		result, err = model.RemoveNetworkClient(&model.RemoveNetworkClientArgs{ClientId: clientId}, clientSession)
		return err
	})
	if err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("remove-client: %s", result.Error.Message)
	}
	return nil
}

// A hosted proxy device opening its client: MintHostedSession.
func (self *harness) hostedMint(network *perfNetwork, deviceId, clientId server.Id) (*credential, error) {
	claims := session.NewByJwt(network.id, network.user, network.name, false, false).Client(deviceId, clientId)
	signed, err := session.MintHostedSession(self.ctx, claims)
	if err != nil {
		return nil, err
	}
	return self.signed(signed)
}

// /auth/network-refresh or /auth/refresh of a credential.
func (self *harness) refresh(presented *credential, network *perfNetwork) (*credential, error) {
	clientSession := self.credentialSession(presented)
	defer clientSession.Cancel()
	signed, err := session.RenewSessionCredential(clientSession, network.name, false, nil)
	if err != nil {
		return nil, err
	}
	return self.signed(signed)
}

var errCodeRefused = errors.New("auth code refused")

// /auth/code-create by a signed-in network, then /auth/code-login: each
// redemption is an independent session whose origin is the creator's.
func (self *harness) codeCreate(creator *credential) (string, error) {
	clientSession := self.credentialSession(creator)
	defer clientSession.Cancel()
	var result *model.AuthCodeCreateResult
	err := routeCall(func() error {
		var err error
		result, err = model.AuthCodeCreate(&model.AuthCodeCreateArgs{DurationMinutes: 60, Uses: 1}, clientSession)
		return err
	})
	if err != nil {
		return "", err
	}
	if result.Error != nil || result.AuthCode == "" {
		message := "no code"
		if result.Error != nil {
			message = result.Error.Message
		}
		return "", fmt.Errorf("%w: %s", errCodeRefused, message)
	}
	return result.AuthCode, nil
}

func (self *harness) codeLogin(code string) (*credential, error) {
	clientSession := session.NewLocalClientSession(self.ctx, "192.0.2.12:1", nil)
	defer clientSession.Cancel()
	requestId := server.NewId()
	var result *model.AuthCodeLoginResult
	err := routeCall(func() error {
		var err error
		result, err = model.AuthCodeLogin(&model.AuthCodeLoginArgs{RequestId: &requestId, AuthCode: code}, clientSession)
		return err
	})
	if err != nil {
		return nil, err
	}
	if result.Error != nil || result.ByJwt == "" {
		message := "no credential"
		if result.Error != nil {
			message = result.Error.Message
		}
		return nil, fmt.Errorf("%w: %s", errCodeRefused, message)
	}
	claims, err := self.parse(result.ByJwt)
	if err != nil {
		return nil, err
	}
	return &credential{claims: claims, jwt: result.ByJwt}, nil
}

// API request authentication exactly as the api's router runs it: the bearer
// header, the signature and claims, the state check and the last-use sample.
func (self *harness) apiAuth(signed string) error {
	request := httptest.NewRequest(http.MethodGet, "http://api.local/network/clients", nil)
	request.Header.Set("Authorization", "Bearer "+signed)
	clientSession, err := session.NewClientSessionFromRequest(request)
	if err != nil {
		return err
	}
	defer clientSession.Cancel()
	return clientSession.Auth(request)
}

// connect's periodic lease recheck of an admitted client transport.
func (self *harness) leaseCheck(claims *session.ByJwt) error {
	ctx, cancel := context.WithTimeout(self.ctx, 30*time.Second)
	defer cancel()
	return session.ValidateByJwtState(ctx, claims, claims.ClientId != nil)
}

// A FIFO of created clients, removed once they reach their lifetime.
type clientQueue struct {
	lock    sync.Mutex
	entries []queuedClient
}

type queuedClient struct {
	created  time.Time
	network  *perfNetwork
	clientId server.Id
	remover  *session.ClientSession
}

func (self *clientQueue) push(entry queuedClient) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.entries = append(self.entries, entry)
}

// The oldest client at least lifetime old, if any.
func (self *clientQueue) popDue(lifetime time.Duration) (queuedClient, bool) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if len(self.entries) == 0 || time.Since(self.entries[0].created) < lifetime {
		return queuedClient{}, false
	}
	entry := self.entries[0]
	self.entries = self.entries[1:]
	return entry, true
}

func (self *clientQueue) size() int {
	self.lock.Lock()
	defer self.lock.Unlock()
	return len(self.entries)
}

// A panic a model call raised, as a route handler sees it: the api router's
// recover answers it 500 with the generic message (router.go), whatever error
// it carried. Its class is "panic:" and the carried error's class.
type routePanic struct {
	err error
}

func (self *routePanic) Error() string { return "route panic: " + self.err.Error() }
func (self *routePanic) Unwrap() error { return self.err }

// Calls fn as a route handler body: a panic comes back as a *routePanic.
func routeCall(fn func() error) (returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err, ok := recovered.(error)
			if !ok {
				err = fmt.Errorf("%v", recovered)
			}
			returnErr = &routePanic{err: err}
		}
	}()
	return fn()
}
