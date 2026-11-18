package handlers

// api's end of an edge block's platform clients (TLS-M5-11; QD-812, QD-873, QD-1054; §6.10.1, §13.6, §19.4): the
// route server.PlatformClientSource asks, under server.InternalGatePathPrefix, taking only the `edge` class's
// bearer, verified by its hash in constant time (server.InternalGateVerifier), and answering every other request
// 403 before anything is read:
//
//	POST /internal/gate/edge-client  edge  {block, host, slot, index}
//	  -> {client_id, by_client_jwt, expire_time_millis, platform_url, sockets_token, sockets_token_expire_time_millis}
//
// The client id is derived from (environment, host, block, slot, index), the environment api's own, never chosen
// by the caller (server.DeriveEdgeClientId), and the client is created in sidecar.yml's urnetwork_edge network on
// first use (model.MintEdgeClient). api refuses, 403, a block config/<env>/edge.yml does not list, a host that is
// not among the block's, a slot at or past the key's SlotCount (2: a deploy's old and new process of a block on a
// host, QD-873) and an index at or past the block's K, so the bearer can mint only the environment's fixed edge
// clients, at most SlotCount times K a block and host. A body without a slot reads slot 0, as a block without
// WARP_SLOT does while warp's change rolls out. With no edge.yml that parses, no urnetwork_edge in sidecar.yml, or
// no such network, every call is answered 503 and nothing is created. edge.yml is reread every
// ConfigRereadInterval, and a read that fails keeps the last one that parsed.
//
// Beside the jwt the answer carries the client's 15-minute service token of audience sockets (QD-1054,
// oauth.MintEdgeClientSocketsToken), minted only once the jwt's mint has answered the client as urnetwork-edge's
// active one, so the checks above bound it as they bound the jwt. A process that cannot sign one, with no svc-
// key, answers 503: an edge block's every call to sockets needs it.
//
// Nothing here logs a bearer, a jwt or a token, and a refusal's log line names the part refused, never a host.
// Safe for concurrent use.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/router"
)

// The route by the name its refusals count under.
const internalGateRouteEdgeClient = "edge-client"

// Tunables of the route.
type InternalGateEdgeClientSettings struct {
	// The most bytes a request's body may hold: 4 KiB, a block, a host, a slot and an index many times over.
	RequestMaxByteCount int64
	// How long one read of edge.yml answers before the next: 60 s, §19.11's reread.
	ConfigRereadInterval time.Duration
	// What a 503 asks the block to wait before it asks again, as Retry-After in whole seconds: 5 s, its source's
	// RetryInterval.
	UnavailableRetryAfter time.Duration
	// The key's limits edge.yml is read under, the same as the blocks' sources: its SlotCount bounds the slot.
	// nil: server.DefaultEdgeClientKeySettings.
	Key *server.EdgeClientKeySettings
	// The mint's. nil: model.DefaultEdgeClientMintSettings.
	Mint *model.EdgeClientMintSettings
}

func DefaultInternalGateEdgeClientSettings() *InternalGateEdgeClientSettings {
	return &InternalGateEdgeClientSettings{
		RequestMaxByteCount:   4 * 1024,
		ConfigRereadInterval:  60 * time.Second,
		UnavailableRetryAfter: 5 * time.Second,
		Key:                   server.DefaultEdgeClientKeySettings(),
		Mint:                  model.DefaultEdgeClientMintSettings(),
	}
}

// What the handlers run on. Each nil takes api's own.
type InternalGateEdgeClientDeps struct {
	// The bearers' digests. nil: server.LoadInternalGateVerifier from api's vault, read once; a vault without the
	// file refuses every call 403.
	Verifier *server.InternalGateVerifier
	// Reads edge.yml. nil: server.LoadEdgeClientsConfig under the settings' Key.
	Config func() (*server.EdgeClientsConfig, error)
	// The urnetwork-edge network. nil: sidecar.yml's urnetwork_edge.
	EdgeNetwork func() (server.Id, bool)
	// api's environment. nil: WARP_ENV.
	Environment func() (string, error)
	// Mints a client's sockets token and says when it ends. nil: oauth.MintEdgeClientSocketsToken, under auth.yml's
	// newest svc- key.
	MintSocketsToken func(networkId server.Id, clientId server.Id) (string, time.Time, error)
	// nil: the defaults.
	Settings *InternalGateEdgeClientSettings
}

// See the file header.
type InternalGateEdgeClientHandlers struct {
	verifier         func() *server.InternalGateVerifier
	loadConfig       func() (*server.EdgeClientsConfig, error)
	edgeNetwork      func() (server.Id, bool)
	environment      func() (string, error)
	mintSocketsToken func(networkId server.Id, clientId server.Id) (string, time.Time, error)
	settings         *InternalGateEdgeClientSettings

	stateLock sync.Mutex
	// the last edge.yml that parsed, and when the next read is due
	config         *server.EdgeClientsConfig
	configReadTime time.Time
}

// The handlers on deps, nil taking api's own.
func NewInternalGateEdgeClientHandlers(deps *InternalGateEdgeClientDeps) *InternalGateEdgeClientHandlers {
	if deps == nil {
		deps = &InternalGateEdgeClientDeps{}
	}
	verifier := func() *server.InternalGateVerifier {
		return deps.Verifier
	}
	if deps.Verifier == nil {
		verifier = sync.OnceValue(func() *server.InternalGateVerifier {
			loaded, err := server.LoadInternalGateVerifier()
			if err != nil {
				// the error names the file and the part, never a bearer
				glog.Errorf("[internalgate]every edge client mint is refused: %v\n", err)
				return nil
			}
			return loaded
		})
	}
	settings := deps.Settings
	if settings == nil {
		settings = DefaultInternalGateEdgeClientSettings()
	}
	loadConfig := deps.Config
	if loadConfig == nil {
		loadConfig = func() (*server.EdgeClientsConfig, error) {
			return server.LoadEdgeClientsConfig(settings.Key)
		}
	}
	edgeNetwork := deps.EdgeNetwork
	if edgeNetwork == nil {
		edgeNetwork = func() (server.Id, bool) {
			return server.CurrentSidecarConfig().UrnetworkEdge()
		}
	}
	environment := deps.Environment
	if environment == nil {
		environment = server.Env
	}
	mintSocketsToken := deps.MintSocketsToken
	if mintSocketsToken == nil {
		mintSocketsToken = oauth.MintEdgeClientSocketsToken
	}
	return &InternalGateEdgeClientHandlers{
		verifier:         verifier,
		loadConfig:       loadConfig,
		edgeNetwork:      edgeNetwork,
		environment:      environment,
		mintSocketsToken: mintSocketsToken,
		settings:         settings,
	}
}

// The one route, its pattern the client's path (server.EdgeClientMintPath), so both ends name one route.
func (self *InternalGateEdgeClientHandlers) Routes() []*router.Route {
	return []*router.Route{
		router.NewRoute("POST", server.EdgeClientMintPath, self.PostEdgeClient),
	}
}

// Whether the request carries the edge class's bearer; otherwise it is answered 403 and counted, before anything is
// read.
func (self *InternalGateEdgeClientHandlers) authenticate(w http.ResponseWriter, r *http.Request) bool {
	refuse := func(refusal string) bool {
		quotaServiceRefusalCounter.WithLabelValues(internalGateRouteEdgeClient, refusal).Inc()
		http.Error(w, "Forbidden.", http.StatusForbidden)
		return false
	}
	verifier := self.verifier()
	if verifier == nil {
		return refuse("no_verify_file")
	}
	caller, ok := verifier.Authenticate(r)
	if !ok {
		return refuse("credential")
	}
	if caller != server.InternalGateCallerEdge {
		if glog.V(1) {
			glog.Infof("[internalgate]%s's bearer refused on %s\n", caller, internalGateRouteEdgeClient)
		}
		return refuse("class")
	}
	return true
}

// edge.yml as last read, reread once ConfigRereadInterval has passed. A read that fails keeps the last that
// parsed, and nil before any has.
func (self *InternalGateEdgeClientHandlers) currentConfig() *server.EdgeClientsConfig {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	now := server.NowUtc()
	if self.config != nil && now.Before(self.configReadTime.Add(self.settings.ConfigRereadInterval)) {
		return self.config
	}
	config, err := self.loadConfig()
	if err != nil {
		// the error names the file and the part
		glog.Infof("[internalgate]edge.yml was not read: %v\n", err)
		return self.config
	}
	self.config = config
	self.configReadTime = now
	return config
}

// Answers 503 with UnavailableRetryAfter, saying why in the log alone.
func (self *InternalGateEdgeClientHandlers) unavailable(w http.ResponseWriter, reason string) {
	if glog.V(1) {
		glog.Infof("[internalgate]%s unavailable: %s\n", internalGateRouteEdgeClient, reason)
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(self.settings.UnavailableRetryAfter/time.Second)))
	http.Error(w, "Unavailable.", http.StatusServiceUnavailable)
}

// POST /internal/gate/edge-client: one edge client's jwt and sockets token.
func (self *InternalGateEdgeClientHandlers) PostEdgeClient(w http.ResponseWriter, r *http.Request) {
	if !self.authenticate(w, r) {
		return
	}
	args := &server.EdgeClientMintArgs{}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, self.settings.RequestMaxByteCount))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(args); err != nil {
		http.Error(w, "Invalid request.", http.StatusBadRequest)
		return
	}
	config := self.currentConfig()
	if config == nil {
		self.unavailable(w, "no edge.yml")
		return
	}
	if err := config.Admits(args); err != nil {
		// which part, never the host or the block asked for
		quotaServiceRefusalCounter.WithLabelValues(internalGateRouteEdgeClient, "not_listed").Inc()
		if glog.V(1) {
			glog.Infof("[internalgate]%s: %v\n", internalGateRouteEdgeClient, err)
		}
		http.Error(w, "Forbidden.", http.StatusForbidden)
		return
	}
	environment, err := self.environment()
	if err != nil || environment == "" {
		self.unavailable(w, "no environment")
		return
	}
	networkId, ok := self.edgeNetwork()
	if !ok {
		self.unavailable(w, "sidecar.yml names no urnetwork_edge")
		return
	}
	mint, err := model.MintEdgeClient(r.Context(), networkId, &model.EdgeClientKey{
		Environment: environment,
		Block:       args.Block,
		Host:        args.Host,
		Slot:        args.Slot,
		Index:       args.Index,
	}, self.settings.Mint)
	if err != nil {
		if errors.Is(err, model.ErrEdgeClientRetired) {
			quotaServiceRefusalCounter.WithLabelValues(internalGateRouteEdgeClient, "retired").Inc()
			http.Error(w, "Forbidden.", http.StatusForbidden)
			return
		}
		// a store error is said by its kind alone
		reason := "the mint failed"
		if errors.Is(err, model.ErrEdgeClientNetworkMissing) {
			reason = "no urnetwork-edge network"
		}
		self.unavailable(w, reason)
		return
	}
	// only now, for the client the mint answered as urnetwork-edge's active one (QD-1054)
	socketsToken, socketsTokenExpireTime, err := self.mintSocketsToken(networkId, mint.ClientId)
	if err != nil || socketsToken == "" {
		// the error names a missing key or a claim, never a token
		self.unavailable(w, "no sockets token was signed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(&server.EdgeClientMintResult{
		ClientId:                     mint.ClientId.String(),
		ByClientJwt:                  mint.ByClientJwt,
		ExpireTimeMillis:             mint.ExpireTime.UnixMilli(),
		PlatformUrl:                  config.PlatformUrl(),
		SocketsToken:                 socketsToken,
		SocketsTokenExpireTimeMillis: socketsTokenExpireTime.UnixMilli(),
	}); err != nil {
		glog.Infof("[internalgate]could not write an edge client\n")
	}
}
