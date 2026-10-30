package controller

// GET /network/provider-status: for each of the caller network's own provider
// clients, why it is or is not offered to clients and how often FindProviders2
// offered it per minute over the last hour. With client_id, the status of one
// client of the caller's network; with format=compact, the statuses without
// their English texts. Every answer carries an entity tag of its body, scoped
// to the caller's network and the query (ProviderStatusEntityScope).

import (
	"context"
	"net/url"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
)

// How long a network's provider admission and ranking is reused. The
// selection's own inputs refresh on the order of minutes (the reliability and
// rollup tasks, the score export), so a fresher answer would only cost more
// queries. The appearance histograms are read fresh on every call.
const providerStatusCacheTtl = 5 * time.Minute

// The cache keys of the admission and ranking: the network's answer, the
// caller's own client and one client. A key is renamed when what it holds
// gains a field (v2: total_clients and reason_code), so an entry an older
// build wrote is never read without them.
const (
	providerStatusNetworkCacheKey = "api_provider_status_v2"
	providerStatusCallerCacheKey  = "api_provider_status_caller_v2"
	providerStatusClientCacheKey  = "api_provider_status_client_v2"
)

// The forms of the answer (the format query parameter).
const (
	// every status with its English texts, the default
	ProviderStatusFormatFull = "full"
	// every status without its English texts (model.ProviderStatusCompact)
	ProviderStatusFormatCompact = "compact"
)

// The refusals of a query, in error.message.
const (
	ProviderStatusInvalidClientIdMessage = "Invalid client_id."
	ProviderStatusInvalidFormatMessage   = "Invalid format."
	// The one answer for every client id the caller may not read: an unknown
	// id, another network's client, a removed or child client, and a client
	// of the network that is not a provider (unless it is the caller's own).
	// It never tells them apart.
	ProviderStatusClientNotFoundMessage = "Provider client not found in this network."
)

// The query of GET /network/provider-status, as sent.
type GetProviderStatusArgs struct {
	// one client of the caller's network; "" for the network's provider
	// clients
	ClientId string
	// ProviderStatusFormatFull ("" too) or ProviderStatusFormatCompact
	Format string
}

// Why a query was refused.
type GetProviderStatusError struct {
	Message string `json:"message"`
}

// The statuses of the caller network's provider clients, with their
// appearance histograms.
type GetProviderStatusResult struct {
	// the caller's own client first when it is a provider, then the others
	// in client id order; with client_id, that one client
	Providers []*model.ProviderStatus `json:"providers"`
	// the network has more provider clients than one answer decides
	Truncated bool `json:"truncated,omitempty"`
	// how many provider clients the network has, the ones Providers draws
	// from, counted up to model.ProviderStatusMaxCountedClients. Only the
	// network's answer has it: one client's answer is never truncated, and
	// a count there would change that client's answer whenever another
	// client of the network starts or stops providing.
	TotalClients *int `json:"total_clients,omitempty"`
	// a refused query, with no providers
	Error *GetProviderStatusError `json:"error,omitempty"`
}

// GetProviderStatusResult in the compact form (format=compact): the same
// statuses in the same order, without their English texts.
type GetProviderStatusCompactResult struct {
	Providers    []*model.ProviderStatusCompact `json:"providers"`
	Truncated    bool                           `json:"truncated,omitempty"`
	TotalClients *int                           `json:"total_clients,omitempty"`
	Error        *GetProviderStatusError        `json:"error,omitempty"`
}

// The reads of an answer, replaced in the pure tests.
var (
	// the network's answer, which a query without client_id returns
	getNetworkProviderStatus = GetProviderStatus
	// whether a client id is a client of the network, and a provider client
	getProviderStatusClient = model.GetProviderStatusClient
	// the admission and ranking of one client of the caller's network
	getCachedClientProviderStatus = cachedClientProviderStatus
	// the appearance histograms, read fresh
	getProviderAppearanceHistograms = model.GetProviderAppearanceHistograms
	// the clock the appearance histograms are read at
	providerStatusNow = server.NowUtc
)

// Pins the clock the appearance histograms are read at, so the answers of one
// test read one window whenever it runs. restore puts the clock back.
func Testing_SetProviderStatusNow(now func() time.Time) (restore func()) {
	previous := providerStatusNow
	providerStatusNow = now
	return func() {
		providerStatusNow = previous
	}
}

// What an answer's entity tag is scoped to besides the caller's network: the
// query, in a canonical form. Spellings of one client id share it, as do no
// format and format=full, which answer the same; every other query has its
// own. A value that is not valid is kept as sent, escaped.
func ProviderStatusEntityScope(args *GetProviderStatusArgs) string {
	clientId := args.ClientId
	if parsedClientId, err := server.ParseId(clientId); err == nil {
		clientId = parsedClientId.String()
	}
	format := args.Format
	if format == "" {
		format = ProviderStatusFormatFull
	}
	query := url.Values{}
	query.Set("client_id", clientId)
	query.Set("format", format)
	return query.Encode()
}

// Tells the caller, for each of its network's own provider clients, how often
// FindProviders2 offered it per minute over the last hour, the numbers it was
// ranked by and the first reason holding it back.
func GetProviderStatus(clientSession *session.ClientSession) (*GetProviderStatusResult, error) {
	networkStatuses, err := router.CacheWithNetworkAuth(
		func(session *session.ClientSession) (*model.ProviderStatusesResult, error) {
			return model.GetNetworkProviderStatuses(session.Ctx, session.ByJwt.NetworkId), nil
		},
		providerStatusNetworkCacheKey,
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
			providerStatusCallerCacheKey,
			providerStatusCacheTtl,
		)(clientSession)
		if err != nil {
			return nil, err
		}
		statuses = append(callerStatuses.Providers, statuses...)
	}
	statuses = orderProviderStatuses(statuses, callerClientId)

	attachProviderAppearances(clientSession.Ctx, statuses)

	totalClients := networkStatuses.TotalClients
	return &GetProviderStatusResult{
		Providers:    statuses,
		Truncated:    networkStatuses.Truncated,
		TotalClients: &totalClients,
	}, nil
}

// The answer to a query: without client_id the network's answer, exactly as
// GetProviderStatus gives it; with client_id the status of that one client.
// Only the full form is answered here; GetProviderStatusCompact answers the
// compact one.
func GetProviderStatusWithArgs(args *GetProviderStatusArgs, clientSession *session.ClientSession) (*GetProviderStatusResult, error) {
	switch args.Format {
	case "", ProviderStatusFormatFull:
	default:
		return providerStatusRefused(ProviderStatusInvalidFormatMessage), nil
	}
	if args.ClientId == "" {
		return getNetworkProviderStatus(clientSession)
	}
	clientId, err := server.ParseId(args.ClientId)
	if err != nil {
		return providerStatusRefused(ProviderStatusInvalidClientIdMessage), nil
	}
	return getClientProviderStatus(clientSession, clientId)
}

// The answer to a query in the compact form: the statuses
// GetProviderStatusWithArgs gives, in the same order, without their English
// texts.
func GetProviderStatusCompact(args *GetProviderStatusArgs, clientSession *session.ClientSession) (*GetProviderStatusCompactResult, error) {
	result, err := GetProviderStatusWithArgs(
		&GetProviderStatusArgs{
			ClientId: args.ClientId,
		},
		clientSession,
	)
	if err != nil {
		return nil, err
	}
	return newGetProviderStatusCompactResult(result), nil
}

// The status of one client of the caller's network. Its admission and ranking
// are cached per network and client for as long as the network's answer, and
// its appearances are read fresh. The caller reads a client the network's
// answer lists, a provider client, and its own client; every other id answers
// ProviderStatusClientNotFoundMessage alike.
func getClientProviderStatus(clientSession *session.ClientSession, clientId server.Id) (*GetProviderStatusResult, error) {
	client, provider := getProviderStatusClient(clientSession.Ctx, clientSession.ByJwt.NetworkId, clientId)
	if !providerStatusClientReadable(client, provider, clientId, clientSession.ByJwt.ClientId) {
		return providerStatusRefused(ProviderStatusClientNotFoundMessage), nil
	}
	clientStatuses, err := getCachedClientProviderStatus(clientSession, clientId)
	if err != nil {
		return nil, err
	}
	statuses := clientStatuses.Providers
	if len(statuses) != 1 || statuses[0].ClientId != clientId {
		// removed since it was decided
		return providerStatusRefused(ProviderStatusClientNotFoundMessage), nil
	}

	attachProviderAppearances(clientSession.Ctx, statuses)

	return &GetProviderStatusResult{
		Providers: statuses,
	}, nil
}

// Whether the caller reads the status of a client: a client of its network
// that is a provider client, as the network's answer lists them, or its own
// client, which the network's answer also adds when it is truncated.
func providerStatusClientReadable(client bool, provider bool, clientId server.Id, callerClientId *server.Id) bool {
	if !client {
		return false
	}
	return provider || (callerClientId != nil && *callerClientId == clientId)
}

// The admission and ranking of one client of the caller's network, cached per
// network and client. A client the status does not decide is an empty list.
func cachedClientProviderStatus(clientSession *session.ClientSession, clientId server.Id) (*model.ProviderStatusesResult, error) {
	return router.CacheWithNetworkAuthInput(
		func(clientId server.Id, session *session.ClientSession) (*model.ProviderStatusesResult, error) {
			result := &model.ProviderStatusesResult{
				Providers: []*model.ProviderStatus{},
			}
			if status := model.GetClientProviderStatus(session.Ctx, session.ByJwt.NetworkId, clientId); status != nil {
				result.Providers = append(result.Providers, status)
			}
			return result, nil
		},
		providerStatusClientCacheKey,
		providerStatusCacheTtl,
	)(clientId, clientSession)
}

// A refused query: no providers, and why in error.message.
func providerStatusRefused(message string) *GetProviderStatusResult {
	return &GetProviderStatusResult{
		Providers: []*model.ProviderStatus{},
		Error: &GetProviderStatusError{
			Message: message,
		},
	}
}

// The compact form of result.
func newGetProviderStatusCompactResult(result *GetProviderStatusResult) *GetProviderStatusCompactResult {
	providers := make([]*model.ProviderStatusCompact, 0, len(result.Providers))
	for _, status := range result.Providers {
		providers = append(providers, model.NewProviderStatusCompact(status))
	}
	return &GetProviderStatusCompactResult{
		Providers:    providers,
		Truncated:    result.Truncated,
		TotalClients: result.TotalClients,
		Error:        result.Error,
	}
}

// Reads the appearance histograms of statuses fresh and adds them to the
// statuses. A failed read leaves them absent.
func attachProviderAppearances(ctx context.Context, statuses []*model.ProviderStatus) {
	clientIds := make([]server.Id, 0, len(statuses))
	for _, status := range statuses {
		clientIds = append(clientIds, status.ClientId)
	}
	histograms, err := getProviderAppearanceHistograms(ctx, clientIds, providerStatusNow())
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
