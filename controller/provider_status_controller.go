package controller

// GET /network/provider-status: for each of the caller network's own provider
// clients, why it is or is not offered to clients and how often FindProviders2
// offered it per minute over the last hour.

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// How long a network's provider admission and ranking is reused. The
// selection's own inputs refresh on the order of minutes (the reliability and
// rollup tasks, the score export), so a fresher answer would only cost more
// queries. The appearance histograms are read fresh on every call.
const providerStatusCacheTtl = 5 * time.Minute

// The statuses of the caller network's provider clients, with their
// appearance histograms.
type GetProviderStatusResult struct {
	// the caller's own client first when it is a provider, then the others
	// in client id order
	Providers []*model.ProviderStatus `json:"providers"`
	// the network has more provider clients than one answer decides
	Truncated bool `json:"truncated,omitempty"`
}

// Tells the caller, for each of its network's own provider clients, how often
// FindProviders2 offered it per minute over the last hour, the numbers it was
// ranked by and the first reason holding it back.
func GetProviderStatus(clientSession *session.ClientSession) (*GetProviderStatusResult, error) {
	networkStatuses, err := router.CacheWithNetworkAuth(
		func(session *session.ClientSession) (*model.ProviderStatusesResult, error) {
			return model.GetNetworkProviderStatuses(session.Ctx, session.ByJwt.NetworkId), nil
		},
		"api_provider_status",
		providerStatusCacheTtl,
	)(clientSession)
	if err != nil {
		return nil, err
	}
	statuses := networkStatuses.Providers

	callerClientId := clientSession.ByJwt.ClientId
	if callerClientId != nil && networkStatuses.Truncated && !providerStatusesContain(statuses, *callerClientId) {
		// the caller's own client, for a network with more providers than one
		// answer decides; a client that is not a provider is an empty list
		callerStatuses, err := router.CacheWithAuth(
			func(session *session.ClientSession) (*model.ProviderStatusesResult, error) {
				result := &model.ProviderStatusesResult{
					Providers: []*model.ProviderStatus{},
				}
				if session.ByJwt.ClientId != nil {
					if status := model.GetClientProviderStatus(session.Ctx, session.ByJwt.NetworkId, *session.ByJwt.ClientId); status != nil {
						result.Providers = append(result.Providers, status)
					}
				}
				return result, nil
			},
			"api_provider_status_caller",
			providerStatusCacheTtl,
		)(clientSession)
		if err != nil {
			return nil, err
		}
		statuses = append(callerStatuses.Providers, statuses...)
	}
	statuses = orderProviderStatuses(statuses, callerClientId)

	clientIds := make([]server.Id, 0, len(statuses))
	for _, status := range statuses {
		clientIds = append(clientIds, status.ClientId)
	}
	histograms, err := model.GetProviderAppearanceHistograms(clientSession.Ctx, clientIds, server.NowUtc())
	if err != nil {
		// the admission and ranking still answer; the app shows the histogram
		// as unavailable
		if glog.V(1) {
			glog.Infof("[psc]provider appearance read failed: %s\n", err)
		}
	}
	for _, status := range statuses {
		status.Appearances = histograms[status.ClientId]
	}

	return &GetProviderStatusResult{
		Providers: statuses,
		Truncated: networkStatuses.Truncated,
	}, nil
}

// Whether statuses has one for clientId.
func providerStatusesContain(statuses []*model.ProviderStatus, clientId server.Id) bool {
	for _, status := range statuses {
		if status.ClientId == clientId {
			return true
		}
	}
	return false
}

// The caller's own client first, the rest in their order, each client once.
func orderProviderStatuses(statuses []*model.ProviderStatus, callerClientId *server.Id) []*model.ProviderStatus {
	ordered := make([]*model.ProviderStatus, 0, len(statuses))
	seen := map[server.Id]bool{}
	if callerClientId != nil {
		for _, status := range statuses {
			if status.ClientId == *callerClientId {
				ordered = append(ordered, status)
				seen[status.ClientId] = true
				break
			}
		}
	}
	for _, status := range statuses {
		if !seen[status.ClientId] {
			ordered = append(ordered, status)
			seen[status.ClientId] = true
		}
	}
	return ordered
}
