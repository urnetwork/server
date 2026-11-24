package controller

// §6.8's sync routes (PROTO-M1-10), Client class on the strict_exp allowlist (api/route_authz.go). Each
// authenticates its own client id, so a reaped or revoked client is answered 401 by the router's wrapper
// and nothing here runs or writes for it.
//
//	GET /sidecar/sync/poll?cursor=      the long-poll: pushservice.LongPoll over the client's slot
//	GET /sidecar/sync/versions?cursor=  a reset paged at 48 versions (plan-PROTO-sync-routes-durable-versions)
//	GET /sidecar/sync/<topic>?since=    core only (plan-CORE-core-hint-body #2); any other topic 404s
//
// core also carries DESIGN-COMPUTE §16.6's authorization features (CACL-M11-03): the request's
// auth_features states the features the sidecar's release implements, which api keeps per sidecar,
// unsigned and for the turn-on prompt alone ({scf_<network_id>}, model/sidecar_features_model.go), and
// the answer names the features every service serving the network implements and, to an Owner's or
// Admin's device, the sidecars whose last stated features lack one.
//
// The long-poll runs only while sidecar.yml's push switch is on (§6's EnableSidecarPush), as the residents'
// publishers do: the process makes it at the first poll the switch admits and closes it at the first the
// switch refuses, so its one shared {scp_*}n subscription runs only while the gate is on.
//
// versions answers from {scp_<C>}ver, core from it or else api's clock in µs, so an hourly resync always
// finds core newer and fetches it. team and dns, which come from network_acl_head and the zone serial, are
// left out until their owners' state exists (TEAM's M2, DNS's M4), and a client keeps its own; this file
// says so rather than invent their state.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

var (
	// The push switch is off: answer 503 with the gate's Retry-After (server.WriteGateClosed).
	ErrSidecarSyncGateClosed = errors.New("sidecar sync: the push gate is closed")
)

// A refusal with its status, and Retry-After when it has one, as RaiseHttpError reads them.
type sidecarSyncRefusal struct {
	status            int
	message           string
	retryAfterSeconds int
}

func (self *sidecarSyncRefusal) Error() string {
	return fmt.Sprintf("%d %s", self.status, self.message)
}

func (self *sidecarSyncRefusal) RetryAfterSeconds() int {
	return self.retryAfterSeconds
}

// The process's long-poll, while the push switch is on.
var sidecarSyncLongPoll struct {
	stateLock sync.Mutex
	longPoll  *pushservice.LongPoll
}

// The long-poll if the push switch is on, making it if none runs, or ErrSidecarSyncGateClosed, closing
// one that does.
func sidecarSyncLongPollIfOpen() (*pushservice.LongPoll, error) {
	open := server.CurrentSidecarConfig().Push()
	sidecarSyncLongPoll.stateLock.Lock()
	defer sidecarSyncLongPoll.stateLock.Unlock()
	if !open {
		if sidecarSyncLongPoll.longPoll != nil {
			sidecarSyncLongPoll.longPoll.Close()
			sidecarSyncLongPoll.longPoll = nil
		}
		return nil, ErrSidecarSyncGateClosed
	}
	if sidecarSyncLongPoll.longPoll == nil {
		// the poll marker, and an ephemeral client's lease seen_time when it sets or errs (TEAM-M1-10), a
		// no-op for a client with no lease and in a binary without MG1
		longPoll, err := pushservice.NewLongPoll(context.Background(), model.NewSidecarPushRedis(nil), nil, nil, model.TouchClientLease)
		if err != nil {
			return nil, err
		}
		sidecarSyncLongPoll.longPoll = longPoll
	}
	return sidecarSyncLongPoll.longPoll, nil
}

// Closes the process's long-poll, if one runs. Tests call it between configurations.
func Testing_CloseSidecarSyncLongPoll() {
	sidecarSyncLongPoll.stateLock.Lock()
	defer sidecarSyncLongPoll.stateLock.Unlock()
	if sidecarSyncLongPoll.longPoll != nil {
		sidecarSyncLongPoll.longPoll.Close()
		sidecarSyncLongPoll.longPoll = nil
	}
}

// The authenticated client, as connect names it.
func sidecarSyncClientId(clientSession *session.ClientSession) (connect.Id, error) {
	if clientSession.ByJwt == nil || clientSession.ByJwt.ClientId == nil {
		return connect.Id{}, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
	}
	return connect.Id(*clientSession.ByJwt.ClientId), nil
}

// GET /sidecar/sync/poll?cursor=: one poll, held up to 25 s. ErrSidecarSyncGateClosed while the switch is
// off; 503 with Retry-After past the process's bound on held polls.
func SidecarSyncPoll(cursor []byte, clientSession *session.ClientSession) (*protocol.PollResponse, error) {
	clientId, err := sidecarSyncClientId(clientSession)
	if err != nil {
		return nil, err
	}
	longPoll, err := sidecarSyncLongPollIfOpen()
	if err != nil {
		return nil, err
	}
	response, err := longPoll.Poll(clientSession.Ctx, clientId, cursor)
	switch {
	case errors.Is(err, pushservice.ErrLongPollBusy):
		return nil, &sidecarSyncRefusal{
			status:            http.StatusServiceUnavailable,
			message:           "Too many held polls.",
			retryAfterSeconds: int(pushservice.DefaultLongPollSettings().BusyRetryAfter / time.Second),
		}
	case errors.Is(err, pushservice.ErrLongPollClosed):
		return nil, ErrSidecarSyncGateClosed
	case err != nil:
		return nil, err
	}
	return response, nil
}

// api's clock in µs, the core topic's version when ver holds none (plan-CORE-core-hint-body #3).
func sidecarSyncCoreClock() uint64 {
	return uint64(server.NowUtc().UnixMicro())
}

// The versions api answers for a client: ver's, with core from it or else api's clock.
func sidecarSyncTopicVersions(ctx context.Context, clientId connect.Id) (map[string]uint64, error) {
	fieldValues, err := model.NewSidecarPushRedis(nil).HashGetAll(ctx, pushservice.HintVersionsKey(clientId))
	if err != nil {
		return nil, err
	}
	topicVersions := pushservice.ParseHintVersions(fieldValues)
	if _, ok := topicVersions[sidecar.PushTopicCore]; !ok {
		topicVersions[sidecar.PushTopicCore] = sidecarSyncCoreClock()
	}
	return topicVersions, nil
}

// GET /sidecar/sync/versions?cursor=: one page, with reset set and no hints. A malformed cursor is 400.
func SidecarSyncVersions(cursor []byte, clientSession *session.ClientSession) (*protocol.PollResponse, error) {
	clientId, err := sidecarSyncClientId(clientSession)
	if err != nil {
		return nil, err
	}
	topicVersions, err := sidecarSyncTopicVersions(clientSession.Ctx, clientId)
	if err != nil {
		return nil, err
	}
	response, err := pushservice.VersionsPage(topicVersions, cursor)
	if errors.Is(err, pushservice.ErrVersionsCursor) {
		return nil, fmt.Errorf("%d Invalid cursor.", http.StatusBadRequest)
	}
	return response, err
}

// GET /sidecar/sync/core's answer (plan-CORE-core-hint-body #2). version is a decimal string, since
// versions pass what a JSON number holds exactly (D-PROTO-12).
type SidecarSyncCoreResult struct {
	Version string `json:"version"`
	// The floor api asserts, unsigned, which a device applies as min(this, its own secure_version)
	// and never persists (QD-032). api asserts none yet, so 0.
	MinSecureVersion uint32 `json:"min_secure_version"`
	// sidecar.yml's gates for the token's network.
	Gates []string `json:"gates"`
	// Whether api holds this client's sidecar revoked: a candidate the client verifies against the
	// config (§5.6), never authority.
	Revoked bool `json:"revoked"`
	// The authorization features every service serving the network implements (DESIGN-COMPUTE
	// §16.6; server.SidecarConfig.AuthFeaturesFor), by name in bit order: unsigned, for the
	// turn-on prompt alone, and none until operations name every service's in sidecar.yml.
	AuthFeatures []string `json:"auth_features"`
	// To an Owner's or Admin's device alone, absent to any other: the network's sidecars whose
	// last stated features lack one of AuthFeatures, each to pause at its turning-on until it
	// updates. Advisory, never authority.
	AuthFeaturesLacking *SidecarSyncCoreLackingResult `json:"auth_features_lacking,omitempty"`
}

// The sidecars a core answer lists as lacking a feature the services implement.
type SidecarSyncCoreLackingResult struct {
	// In sidecar id order, at most model.SidecarFeaturesSettings.ListedSidecarCount.
	Sidecars []*SidecarSyncCoreSidecarResult `json:"sidecars"`
	// More sidecars lack one than are listed.
	More bool `json:"more"`
}

// One listed sidecar.
type SidecarSyncCoreSidecarResult struct {
	SidecarId server.Id `json:"sidecar_id"`
	// What its last core sync stated, by name; none for a sidecar that never stated.
	AuthFeatures []string `json:"auth_features"`
}

// GET /sidecar/sync/<topic>?since=: core alone; any other topic is 404, served by its owner's routes.
func SidecarSyncTopic(topic string, since string, clientSession *session.ClientSession) (*SidecarSyncCoreResult, error) {
	return SidecarSyncTopicWithFeatures(topic, since, "", clientSession)
}

// Keeps the calling sidecar's stated features and answers what an Owner's or Admin's device is told of
// the others' (DESIGN-COMPUTE §16.6's turn on row). Advisory, so best effort: a store that fails is
// logged and the answer goes without the list, never failing the core state.
func sidecarSyncCoreFeatures(ctx context.Context, networkId server.Id, sidecarId server.Id, stated sidecar.AuthFeature, features sidecar.AuthFeature) *SidecarSyncCoreLackingResult {
	settings := model.DefaultSidecarFeaturesSettings()
	if err := model.SetSidecarStatedAuthFeatures(ctx, settings, networkId, sidecarId, stated); err != nil {
		glog.Warningf("[sync]%s could not keep sidecar %s's stated features: %v\n", networkId, sidecarId, err)
	}
	sequencer, err := model.CurrentConfigLogSequencer()
	if err != nil || settings.ListTimeout <= 0 {
		return nil
	}
	listCtx, cancel := context.WithTimeout(ctx, settings.ListTimeout)
	defer cancel()
	lacking, err := sequencer.SidecarFeaturesLacking(listCtx, settings, networkId, sidecarId, features)
	if err != nil {
		glog.Warningf("[sync]%s could not list the sidecars lacking a feature: %v\n", networkId, err)
		return nil
	}
	if lacking == nil {
		return nil
	}
	result := &SidecarSyncCoreLackingResult{Sidecars: []*SidecarSyncCoreSidecarResult{}, More: lacking.More}
	for _, listed := range lacking.Sidecars {
		result.Sidecars = append(result.Sidecars, &SidecarSyncCoreSidecarResult{SidecarId: listed.SidecarId, AuthFeatures: listed.AuthFeatures.Names()})
	}
	return result
}

// SidecarSyncTopic with auth_features' value: the features the calling sidecar's release implements,
// their names comma separated, empty for none, as an older release sends (DESIGN-COMPUTE §16.6). api
// keeps them per sidecar for the turn-on prompt alone ({scf_<network_id>}); a name that is no
// feature's states nothing.
func SidecarSyncTopicWithFeatures(topic string, since string, authFeatures string, clientSession *session.ClientSession) (*SidecarSyncCoreResult, error) {
	clientId, err := sidecarSyncClientId(clientSession)
	if err != nil {
		return nil, err
	}
	if topic != sidecar.PushTopicCore {
		return nil, fmt.Errorf("%d No such topic.", http.StatusNotFound)
	}
	if since != "" && !pushservice.CanonicalVersion(since) {
		return nil, fmt.Errorf("%d Invalid since.", http.StatusBadRequest)
	}
	topicVersions, err := sidecarSyncTopicVersions(clientSession.Ctx, clientId)
	if err != nil {
		return nil, err
	}
	networkId := clientSession.ByJwt.NetworkId
	config := server.CurrentSidecarConfig()
	gates := config.GatesFor(networkId)
	if gates == nil {
		gates = []string{}
	}
	features := config.AuthFeaturesFor(networkId)
	// a candidate only, so a binary without §5.15's tables says false and the client's own config decides
	revoked := false
	var lacking *SidecarSyncCoreLackingResult
	if server.SidecarMg1Linked() {
		if view := model.GetSidecarIdentityByClient(clientSession.Ctx, networkId, server.Id(clientId)); view != nil {
			revoked = strings.EqualFold(view.Status, model.SidecarIdentityStatusRevoked)
			// a revoked sidecar states nothing: its removal drops its entry
			if !revoked {
				lacking = sidecarSyncCoreFeatures(clientSession.Ctx, networkId, view.SidecarId, sidecar.ParseAuthFeatureNames(authFeatures), features)
			}
		}
	}
	return &SidecarSyncCoreResult{
		Version:             pushservice.FormatVersion(topicVersions[sidecar.PushTopicCore]),
		MinSecureVersion:    0,
		Gates:               gates,
		Revoked:             revoked,
		AuthFeatures:        features.Names(),
		AuthFeaturesLacking: lacking,
	}, nil
}
