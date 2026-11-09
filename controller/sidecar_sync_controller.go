package controller

// §6.8's sync routes (PROTO-M1-10), Client class on the strict_exp allowlist (api/route_authz.go). Each
// authenticates its own client id, so a reaped or revoked client is answered 401 by the router's wrapper
// and nothing here runs or writes for it.
//
//	GET /sidecar/sync/poll?cursor=      the long-poll: pushservice.LongPoll over the client's slot
//	GET /sidecar/sync/versions?cursor=  a reset paged at 48 versions (plan-PROTO-sync-routes-durable-versions)
//	GET /sidecar/sync/<topic>?since=    core only (plan-CORE-core-hint-body #2); any other topic 404s
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
		// no lease table yet (TEAM-M1-10): the poll marker alone, and no row to touch
		longPoll, err := pushservice.NewLongPoll(context.Background(), model.NewSidecarPushRedis(nil), nil, nil, nil)
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
}

// GET /sidecar/sync/<topic>?since=: core alone; any other topic is 404, served by its owner's routes.
func SidecarSyncTopic(topic string, since string, clientSession *session.ClientSession) (*SidecarSyncCoreResult, error) {
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
	gates := server.CurrentSidecarConfig().GatesFor(networkId)
	if gates == nil {
		gates = []string{}
	}
	// a candidate only, so a binary without §5.15's tables says false and the client's own config decides
	revoked := false
	if server.SidecarMg1Linked() {
		if view := model.GetSidecarIdentityByClient(clientSession.Ctx, networkId, server.Id(clientId)); view != nil {
			revoked = strings.EqualFold(view.Status, model.SidecarIdentityStatusRevoked)
		}
	}
	return &SidecarSyncCoreResult{
		Version:          pushservice.FormatVersion(topicVersions[sidecar.PushTopicCore]),
		MinSecureVersion: 0,
		Gates:            gates,
		Revoked:          revoked,
	}, nil
}
