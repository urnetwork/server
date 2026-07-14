package model

// provider_model.go — the per-provider statistics APIs:
//   GET  /stats/providers                  StatsProviders          (last 24h)
//   POST /stats/providers-last-n           StatsProvidersLastN     (last_n hours)
//   POST /stats/provider-last-n            StatsProvider           (single provider, last_n hours)
//   POST /stats/providers-overview-last-n  StatsProvidersOverview  (network aggregate)
//
// All figures are real aggregates over the caller network's provider clients
// (a provider is the `destination_id` of a transfer_contract). Data sources:
//   transfer bytes   transfer_contract ⋈ contract_close (settled bytes)
//   contracts/clients transfer_contract (by create_time)
//   payout            transfer_escrow_sweep ⋈ transfer_contract (net revenue)
//   uptime/events     network_client_connection (connect/disconnect)
//   search interest   search_provider_stats (see search_provider_stats_model.go)
//
// Reads are grouped by client in a single query per metric (never N+1) and are
// fronted by a short-TTL per-network cache at the handler layer.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

const (
	defaultStatsWindow = 24 * time.Hour
	// upper bound so a pathological last_n can't blow up the day/hour gap-fill
	maxStatsWindow = 366 * 24 * time.Hour
)

// resolveStatsWindow computes the lookback window from a last_n hours value
// (preferred) or a legacy lookback in days, clamped to [1 hour, maxStatsWindow].
func resolveStatsWindow(lastNHours float64, legacyLookbackDays int) time.Duration {
	var window time.Duration
	switch {
	case 0 < lastNHours:
		window = time.Duration(lastNHours * float64(time.Hour))
	case 0 < legacyLookbackDays:
		window = time.Duration(legacyLookbackDays) * 24 * time.Hour
	default:
		window = defaultStatsWindow
	}
	if window < time.Hour {
		window = time.Hour
	}
	if maxStatsWindow < window {
		window = maxStatsWindow
	}
	return window
}

// -----------------------------------------------------------------------------
// providers list: /stats/providers and /stats/providers-last-n
// -----------------------------------------------------------------------------

type StatsProvidersArgs struct {
	// LastN is the window in hours (spec: /stats/providers-last-n).
	LastN float64 `json:"last_n,omitempty"`
}

type StatsProvidersResult struct {
	CreatedTime time.Time        `json:"created_time"`
	Providers   []*ProviderStats `json:"providers"`
}

type ProviderStats struct {
	ClientId               server.Id         `json:"client_id"`
	Connected              bool              `json:"connected"`
	ConnectedEventsLast24h []*ConnectedEvent `json:"connected_events_last_24h"`
	UptimeLast24h          float64           `json:"uptime_last_24h"`
	TransferDataLast24h    float64           `json:"transfer_data_last_24h"`
	PayoutLast24h          float64           `json:"payout_last_24h"`
	SearchInterestLast24h  int               `json:"search_interest_last_24h"`
	ContractsLast24h       int               `json:"contracts_last_24h"`
	ClientsLast24h         int               `json:"clients_last_24h"`
}

type ConnectedEvent struct {
	EventTime time.Time `json:"event_time"`
	Connected bool      `json:"connected"`
}

// StatsProviders returns every provider in the caller network with a summary of
// the last 24 hours.
func StatsProviders(
	clientSession *session.ClientSession,
) (*StatsProvidersResult, error) {
	now := server.NowUtc()
	return statsProviders(clientSession, now.Add(-defaultStatsWindow), now)
}

// StatsProvidersLastN is StatsProviders over a caller-specified window.
func StatsProvidersLastN(
	args *StatsProvidersArgs,
	clientSession *session.ClientSession,
) (*StatsProvidersResult, error) {
	now := server.NowUtc()
	windowStart := now.Add(-resolveStatsWindow(args.LastN, 0))
	return statsProviders(clientSession, windowStart, now)
}

func statsProviders(
	clientSession *session.ClientSession,
	windowStart time.Time,
	now time.Time,
) (*StatsProvidersResult, error) {
	networkId := clientSession.ByJwt.NetworkId
	providers := []*ProviderStats{}

	// stats read: tolerates replica delay
	server.ReplicaDb(clientSession.Ctx, func(conn server.PgConn) {
		// enumerate the network's active provider clients
		order := []server.Id{}
		result, err := conn.Query(
			clientSession.Ctx,
			`
			SELECT network_client.client_id
			FROM network_client
			WHERE network_client.network_id = $1 AND network_client.active = true
				AND EXISTS (
					SELECT 1 FROM provide_key
					WHERE provide_key.client_id = network_client.client_id
				)
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				order = append(order, clientId)
			}
		})
		if len(order) == 0 {
			return
		}
		ids := idStrings(order)

		// transfer bytes (settled) per provider
		transferBytes := map[server.Id]int64{}
		result, err = conn.Query(
			clientSession.Ctx,
			`
			SELECT tc.destination_id, COALESCE(SUM(cc.used_transfer_byte_count), 0)
			FROM transfer_contract tc
			INNER JOIN contract_close cc ON
				cc.contract_id = tc.contract_id AND cc.party = 'destination'
			WHERE tc.destination_id = ANY($1::uuid[]) AND tc.close_time >= $2
			GROUP BY tc.destination_id
			`,
			ids,
			windowStart,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var byteCount int64
				server.Raise(result.Scan(&clientId, &byteCount))
				transferBytes[clientId] = byteCount
			}
		})

		// contracts + distinct served clients per provider
		contracts := map[server.Id]int{}
		clients := map[server.Id]int{}
		result, err = conn.Query(
			clientSession.Ctx,
			`
			SELECT destination_id, COUNT(*), COUNT(DISTINCT source_id)
			FROM transfer_contract
			WHERE destination_id = ANY($1::uuid[]) AND create_time >= $2
			GROUP BY destination_id
			`,
			ids,
			windowStart,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var contractCount int
				var clientCount int
				server.Raise(result.Scan(&clientId, &contractCount, &clientCount))
				contracts[clientId] = contractCount
				clients[clientId] = clientCount
			}
		})

		// payout (net revenue) per provider
		payoutNanoCents := map[server.Id]NanoCents{}
		result, err = conn.Query(
			clientSession.Ctx,
			`
			SELECT tc.destination_id, COALESCE(SUM(s.payout_net_revenue_nano_cents), 0)
			FROM transfer_escrow_sweep s
			INNER JOIN transfer_contract tc ON tc.contract_id = s.contract_id
			WHERE tc.destination_id = ANY($1::uuid[]) AND s.sweep_time >= $2
			GROUP BY tc.destination_id
			`,
			ids,
			windowStart,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var nanoCents NanoCents
				server.Raise(result.Scan(&clientId, &nanoCents))
				payoutNanoCents[clientId] = nanoCents
			}
		})

		// search interest per provider
		searchInterest := map[server.Id]int{}
		result, err = conn.Query(
			clientSession.Ctx,
			`
			SELECT client_id, COALESCE(SUM(match_count), 0)
			FROM search_provider_stats
			WHERE client_id = ANY($1::uuid[]) AND period_start >= $2
			GROUP BY client_id
			`,
			ids,
			windowStart,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var count int
				server.Raise(result.Scan(&clientId, &count))
				searchInterest[clientId] = count
			}
		})

		// connection intervals per provider -> uptime, events, current state
		intervals := queryConnectionIntervals(clientSession.Ctx, conn, ids, windowStart, now)

		for _, clientId := range order {
			ivals := intervals[clientId]
			providers = append(providers, &ProviderStats{
				ClientId:               clientId,
				Connected:              anyActive(ivals),
				ConnectedEventsLast24h: connectedEvents(ivals, windowStart, now),
				UptimeLast24h:          uptimeHours(ivals, windowStart, now),
				TransferDataLast24h:    bytesToGib(transferBytes[clientId]),
				PayoutLast24h:          NanoCentsToUsd(payoutNanoCents[clientId]),
				SearchInterestLast24h:  searchInterest[clientId],
				ContractsLast24h:       contracts[clientId],
				ClientsLast24h:         clients[clientId],
			})
		}
	})

	return &StatsProvidersResult{
		CreatedTime: server.NowUtc(),
		Providers:   providers,
	}, nil
}

// -----------------------------------------------------------------------------
// single provider drill-down: /stats/provider-last-n
// -----------------------------------------------------------------------------

type StatsProviderArgs struct {
	ClientId server.Id `json:"client_id"`
	// LastN is the window in hours (spec: /stats/provider-last-n).
	LastN float64 `json:"last_n,omitempty"`
	// Lookback (days) is accepted for backward compatibility with the legacy
	// /stats/provider-last-90 route; LastN takes precedence.
	Lookback int `json:"lookback,omitempty"`
}

type StatsProviderResult struct {
	Lookback       int                `json:"lookback"`
	CreatedTime    time.Time          `json:"created_time"`
	Uptime         map[string]float64 `json:"uptime"` // yyyy-mm-dd-hh -> minutes
	TransferData   map[string]float64 `json:"transfer_data"`
	Payout         map[string]float64 `json:"payout"`
	SearchInterest map[string]int     `json:"search_interest"`
	Contracts      map[string]int     `json:"contracts"`
	Clients        map[string]int     `json:"clients"`
	ClientDetails  []*ClientDetail    `json:"client_details"`
}

type ClientDetail struct {
	ClientId     server.Id          `json:"client_id"`
	TransferData map[string]float64 `json:"transfer_data"`
}

// StatsProviderLast90 is the legacy default-90-days entry point kept for the
// /stats/provider-last-90 route.
func StatsProviderLast90(
	args *StatsProviderArgs,
	clientSession *session.ClientSession,
) (*StatsProviderResult, error) {
	a := *args
	if a.LastN <= 0 && a.Lookback <= 0 {
		a.Lookback = 90
	}
	return StatsProvider(&a, clientSession)
}

// StatsProvider returns detailed per-day (and per-hour uptime) stats for a
// single provider client in the caller network, plus a per-served-client
// transfer breakdown.
func StatsProvider(
	args *StatsProviderArgs,
	clientSession *session.ClientSession,
) (*StatsProviderResult, error) {
	networkId := clientSession.ByJwt.NetworkId
	clientId := args.ClientId
	now := server.NowUtc()
	windowStart := now.Add(-resolveStatsWindow(args.LastN, args.Lookback))

	days := dayKeysBetween(windowStart, now)
	uptime := map[string]float64{}
	transferData := gapFilledFloatDays(days)
	payout := gapFilledFloatDays(days)
	searchInterest := gapFilledIntDays(days)
	contracts := gapFilledIntDays(days)
	clients := gapFilledIntDays(days)
	clientDetails := []*ClientDetail{}

	found := false
	// stats read: tolerates replica delay
	server.ReplicaDb(clientSession.Ctx, func(conn server.PgConn) {
		// ownership: the client must belong to the caller's network
		result, err := conn.Query(
			clientSession.Ctx,
			`
			SELECT EXISTS(
				SELECT 1 FROM network_client
				WHERE network_id = $1 AND client_id = $2
			)
			`,
			networkId,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&found))
			}
		})
		if !found {
			return
		}

		// transfer data (settled) per day
		result, err = conn.Query(
			clientSession.Ctx,
			`
			SELECT to_char(tc.close_time, 'YYYY-MM-DD') AS day, COALESCE(SUM(cc.used_transfer_byte_count), 0)
			FROM transfer_contract tc
			INNER JOIN contract_close cc ON
				cc.contract_id = tc.contract_id AND cc.party = 'destination'
			WHERE tc.destination_id = $1 AND tc.close_time >= $2
			GROUP BY day
			`,
			clientId,
			windowStart,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var day string
				var byteCount int64
				server.Raise(result.Scan(&day, &byteCount))
				transferData[day] = bytesToGib(byteCount)
			}
		})

		// payout (net revenue) per day
		result, err = conn.Query(
			clientSession.Ctx,
			`
			SELECT to_char(s.sweep_time, 'YYYY-MM-DD') AS day, COALESCE(SUM(s.payout_net_revenue_nano_cents), 0)
			FROM transfer_escrow_sweep s
			INNER JOIN transfer_contract tc ON tc.contract_id = s.contract_id
			WHERE tc.destination_id = $1 AND s.sweep_time >= $2
			GROUP BY day
			`,
			clientId,
			windowStart,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var day string
				var nanoCents NanoCents
				server.Raise(result.Scan(&day, &nanoCents))
				payout[day] = NanoCentsToUsd(nanoCents)
			}
		})

		// contracts + distinct served clients per day
		result, err = conn.Query(
			clientSession.Ctx,
			`
			SELECT to_char(create_time, 'YYYY-MM-DD') AS day, COUNT(*), COUNT(DISTINCT source_id)
			FROM transfer_contract
			WHERE destination_id = $1 AND create_time >= $2
			GROUP BY day
			`,
			clientId,
			windowStart,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var day string
				var contractCount int
				var clientCount int
				server.Raise(result.Scan(&day, &contractCount, &clientCount))
				contracts[day] = contractCount
				clients[day] = clientCount
			}
		})

		// search interest per day
		result, err = conn.Query(
			clientSession.Ctx,
			`
			SELECT to_char(period_start, 'YYYY-MM-DD') AS day, COALESCE(SUM(match_count), 0)
			FROM search_provider_stats
			WHERE client_id = $1 AND period_start >= $2
			GROUP BY day
			`,
			clientId,
			windowStart,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var day string
				var count int
				server.Raise(result.Scan(&day, &count))
				searchInterest[day] = count
			}
		})

		// uptime per hour (minutes), from connection intervals
		intervals := queryConnectionIntervals(clientSession.Ctx, conn, []string{clientId.String()}, windowStart, now)
		for hour, minutes := range uptimeByHour(intervals[clientId], windowStart, now) {
			uptime[hour] = minutes
		}

		// client details: transfer data per served (source) client per day
		details := map[server.Id]map[string]float64{}
		detailOrder := []server.Id{}
		result, err = conn.Query(
			clientSession.Ctx,
			`
			SELECT tc.source_id, to_char(tc.close_time, 'YYYY-MM-DD') AS day, COALESCE(SUM(cc.used_transfer_byte_count), 0)
			FROM transfer_contract tc
			INNER JOIN contract_close cc ON
				cc.contract_id = tc.contract_id AND cc.party = 'destination'
			WHERE tc.destination_id = $1 AND tc.close_time >= $2
			GROUP BY tc.source_id, day
			`,
			clientId,
			windowStart,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var sourceId server.Id
				var day string
				var byteCount int64
				server.Raise(result.Scan(&sourceId, &day, &byteCount))
				dayMap, ok := details[sourceId]
				if !ok {
					dayMap = map[string]float64{}
					details[sourceId] = dayMap
					detailOrder = append(detailOrder, sourceId)
				}
				dayMap[day] = bytesToGib(byteCount)
			}
		})
		for _, sourceId := range detailOrder {
			clientDetails = append(clientDetails, &ClientDetail{
				ClientId:     sourceId,
				TransferData: details[sourceId],
			})
		}
	})

	if !found {
		// the client is not in the caller's network: deny with HTTP 403. The
		// router's RaiseHttpError peels the leading status code off the error;
		// with-input handlers pass impl errors through unwrapped.
		return nil, fmt.Errorf("%d Not authorized to view this provider.", http.StatusForbidden)
	}

	return &StatsProviderResult{
		Lookback:       windowLookbackDays(windowStart, now),
		CreatedTime:    server.NowUtc(),
		Uptime:         uptime,
		TransferData:   transferData,
		Payout:         payout,
		SearchInterest: searchInterest,
		Contracts:      contracts,
		Clients:        clients,
		ClientDetails:  clientDetails,
	}, nil
}

// -----------------------------------------------------------------------------
// network aggregate overview: /stats/providers-overview-last-n
// (server extension — a network-wide time series across all of its providers)
// -----------------------------------------------------------------------------

type StatsProvidersOverviewArgs struct {
	// LastN is the window in hours; Lookback (days) is the legacy field kept
	// for the /stats/providers-overview-last-90 route. LastN takes precedence.
	LastN    float64 `json:"last_n,omitempty"`
	Lookback int     `json:"lookback,omitempty"`
}

type StatsProvidersOverviewResult struct {
	Lookback       int                `json:"lookback"`
	CreatedTime    time.Time          `json:"created_time"`
	Uptime         map[string]float64 `json:"uptime"` // yyyy-mm-dd -> avg hours per provider
	TransferData   map[string]float64 `json:"transfer_data"`
	Payout         map[string]float64 `json:"payout"`
	SearchInterest map[string]int     `json:"search_interest"`
	Contracts      map[string]int     `json:"contracts"`
	Clients        map[string]int     `json:"clients"`
}

// StatsProvidersOverviewLast90 is the legacy default-90-days entry point kept
// for the /stats/providers-overview-last-90 route.
func StatsProvidersOverviewLast90(
	clientSession *session.ClientSession,
) (*StatsProvidersOverviewResult, error) {
	return StatsProvidersOverview(
		&StatsProvidersOverviewArgs{Lookback: 90},
		clientSession,
	)
}

// StatsProvidersOverview returns a network-wide per-day time series aggregated
// across all of the caller network's providers.
func StatsProvidersOverview(
	args *StatsProvidersOverviewArgs,
	clientSession *session.ClientSession,
) (*StatsProvidersOverviewResult, error) {
	networkId := clientSession.ByJwt.NetworkId
	now := server.NowUtc()
	windowStart := now.Add(-resolveStatsWindow(args.LastN, args.Lookback))

	days := dayKeysBetween(windowStart, now)
	uptime := gapFilledFloatDays(days)
	transferData := gapFilledFloatDays(days)
	payout := gapFilledFloatDays(days)
	searchInterest := gapFilledIntDays(days)
	contracts := gapFilledIntDays(days)
	clients := gapFilledIntDays(days)

	// stats read: tolerates replica delay
	server.ReplicaDb(clientSession.Ctx, func(conn server.PgConn) {
		// enumerate the network's provider clients
		providerIds := []server.Id{}
		result, err := conn.Query(
			clientSession.Ctx,
			`
			SELECT network_client.client_id
			FROM network_client
			WHERE network_client.network_id = $1 AND network_client.active = true
				AND EXISTS (
					SELECT 1 FROM provide_key
					WHERE provide_key.client_id = network_client.client_id
				)
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				providerIds = append(providerIds, clientId)
			}
		})
		if len(providerIds) == 0 {
			return
		}
		ids := idStrings(providerIds)

		// transfer bytes per day (summed across providers)
		result, err = conn.Query(
			clientSession.Ctx,
			`
			SELECT to_char(tc.close_time, 'YYYY-MM-DD') AS day, COALESCE(SUM(cc.used_transfer_byte_count), 0)
			FROM transfer_contract tc
			INNER JOIN contract_close cc ON
				cc.contract_id = tc.contract_id AND cc.party = 'destination'
			WHERE tc.destination_id = ANY($1::uuid[]) AND tc.close_time >= $2
			GROUP BY day
			`,
			ids,
			windowStart,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var day string
				var byteCount int64
				server.Raise(result.Scan(&day, &byteCount))
				transferData[day] = bytesToGib(byteCount)
			}
		})

		// payout per day
		result, err = conn.Query(
			clientSession.Ctx,
			`
			SELECT to_char(s.sweep_time, 'YYYY-MM-DD') AS day, COALESCE(SUM(s.payout_net_revenue_nano_cents), 0)
			FROM transfer_escrow_sweep s
			INNER JOIN transfer_contract tc ON tc.contract_id = s.contract_id
			WHERE tc.destination_id = ANY($1::uuid[]) AND s.sweep_time >= $2
			GROUP BY day
			`,
			ids,
			windowStart,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var day string
				var nanoCents NanoCents
				server.Raise(result.Scan(&day, &nanoCents))
				payout[day] = NanoCentsToUsd(nanoCents)
			}
		})

		// contracts + distinct served clients per day
		result, err = conn.Query(
			clientSession.Ctx,
			`
			SELECT to_char(create_time, 'YYYY-MM-DD') AS day, COUNT(*), COUNT(DISTINCT source_id)
			FROM transfer_contract
			WHERE destination_id = ANY($1::uuid[]) AND create_time >= $2
			GROUP BY day
			`,
			ids,
			windowStart,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var day string
				var contractCount int
				var clientCount int
				server.Raise(result.Scan(&day, &contractCount, &clientCount))
				contracts[day] = contractCount
				clients[day] = clientCount
			}
		})

		// search interest per day
		result, err = conn.Query(
			clientSession.Ctx,
			`
			SELECT to_char(period_start, 'YYYY-MM-DD') AS day, COALESCE(SUM(match_count), 0)
			FROM search_provider_stats
			WHERE client_id = ANY($1::uuid[]) AND period_start >= $2
			GROUP BY day
			`,
			ids,
			windowStart,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var day string
				var count int
				server.Raise(result.Scan(&day, &count))
				searchInterest[day] = count
			}
		})

		// uptime per day: total provider-hours that day, averaged over providers
		intervals := queryConnectionIntervals(clientSession.Ctx, conn, ids, windowStart, now)
		dayHours := map[string]float64{}
		for _, ivals := range intervals {
			for _, interval := range ivals {
				addUptimeHoursPerDay(dayHours, interval, windowStart, now)
			}
		}
		providerCount := float64(len(providerIds))
		for day, hours := range dayHours {
			uptime[day] = hours / providerCount
		}
	})

	return &StatsProvidersOverviewResult{
		Lookback:       windowLookbackDays(windowStart, now),
		CreatedTime:    server.NowUtc(),
		Uptime:         uptime,
		TransferData:   transferData,
		Payout:         payout,
		SearchInterest: searchInterest,
		Contracts:      contracts,
		Clients:        clients,
	}, nil
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

const bytesPerGib = float64(1024 * 1024 * 1024)

func bytesToGib(byteCount int64) float64 {
	return float64(byteCount) / bytesPerGib
}

func idStrings(ids []server.Id) []string {
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	return strs
}

func dayKey(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

func hourKey(t time.Time) string {
	return t.UTC().Format("2006-01-02-15")
}

// dayKeysBetween returns the yyyy-mm-dd key for every UTC day intersecting
// [windowStart, now], inclusive.
func dayKeysBetween(windowStart time.Time, now time.Time) []string {
	days := []string{}
	t := windowStart.UTC().Truncate(24 * time.Hour)
	end := now.UTC()
	for !t.After(end) {
		days = append(days, dayKey(t))
		t = t.Add(24 * time.Hour)
	}
	return days
}

func gapFilledFloatDays(days []string) map[string]float64 {
	m := map[string]float64{}
	for _, day := range days {
		m[day] = 0
	}
	return m
}

func gapFilledIntDays(days []string) map[string]int {
	m := map[string]int{}
	for _, day := range days {
		m[day] = 0
	}
	return m
}

func windowLookbackDays(windowStart time.Time, now time.Time) int {
	return int(math.Ceil(now.Sub(windowStart).Hours() / 24))
}

// connectionInterval is one network_client_connection row overlapping the
// window: an active period [connectTime, disconnectTime|now].
type connectionInterval struct {
	connectTime    time.Time
	disconnectTime *time.Time
	connected      bool
}

func queryConnectionIntervals(
	ctx context.Context,
	conn server.PgConn,
	ids []string,
	windowStart time.Time,
	now time.Time,
) map[server.Id][]*connectionInterval {
	intervals := map[server.Id][]*connectionInterval{}
	result, err := conn.Query(
		ctx,
		`
		SELECT client_id, connect_time, disconnect_time, connected
		FROM network_client_connection
		WHERE client_id = ANY($1::uuid[])
			AND connect_time <= $3
			AND (disconnect_time IS NULL OR disconnect_time >= $2)
		ORDER BY client_id, connect_time
		`,
		ids,
		windowStart,
		now,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var clientId server.Id
			interval := &connectionInterval{}
			server.Raise(result.Scan(
				&clientId,
				&interval.connectTime,
				&interval.disconnectTime,
				&interval.connected,
			))
			intervals[clientId] = append(intervals[clientId], interval)
		}
	})
	return intervals
}

// activeRange returns the connected span clipped to [windowStart, now]. ok is
// false for a disconnected record or when there is no overlap.
func (interval *connectionInterval) activeRange(windowStart time.Time, now time.Time) (time.Time, time.Time, bool) {
	if !interval.connected {
		return time.Time{}, time.Time{}, false
	}
	start := interval.connectTime
	end := now
	if interval.disconnectTime != nil {
		end = *interval.disconnectTime
	}
	if start.Before(windowStart) {
		start = windowStart
	}
	if end.After(now) {
		end = now
	}
	if !start.Before(end) {
		return time.Time{}, time.Time{}, false
	}
	return start, end, true
}

func anyActive(intervals []*connectionInterval) bool {
	for _, interval := range intervals {
		if interval.connected && interval.disconnectTime == nil {
			return true
		}
	}
	return false
}

func uptimeHours(intervals []*connectionInterval, windowStart time.Time, now time.Time) float64 {
	var total time.Duration
	for _, interval := range intervals {
		if start, end, ok := interval.activeRange(windowStart, now); ok {
			total += end.Sub(start)
		}
	}
	return total.Hours()
}

// uptimeByHour distributes each connected span across yyyy-mm-dd-hh buckets as
// minutes-up.
func uptimeByHour(intervals []*connectionInterval, windowStart time.Time, now time.Time) map[string]float64 {
	minutes := map[string]float64{}
	for _, interval := range intervals {
		start, end, ok := interval.activeRange(windowStart, now)
		if !ok {
			continue
		}
		t := start
		for t.Before(end) {
			segEnd := t.Truncate(time.Hour).Add(time.Hour)
			if end.Before(segEnd) {
				segEnd = end
			}
			minutes[hourKey(t)] += segEnd.Sub(t).Minutes()
			t = segEnd
		}
	}
	return minutes
}

// addUptimeHoursPerDay distributes one connected span across yyyy-mm-dd buckets
// as hours-up, accumulating into dayHours.
func addUptimeHoursPerDay(dayHours map[string]float64, interval *connectionInterval, windowStart time.Time, now time.Time) {
	start, end, ok := interval.activeRange(windowStart, now)
	if !ok {
		return
	}
	t := start
	for t.Before(end) {
		segEnd := t.Truncate(24 * time.Hour).Add(24 * time.Hour)
		if end.Before(segEnd) {
			segEnd = end
		}
		dayHours[dayKey(t)] += segEnd.Sub(t).Hours()
		t = segEnd
	}
}

// connectedEvents reconstructs the connect/disconnect transition timeline
// within the window: a connect event at connect_time and a disconnect event at
// disconnect_time, whichever fall inside [windowStart, now].
func connectedEvents(intervals []*connectionInterval, windowStart time.Time, now time.Time) []*ConnectedEvent {
	events := []*ConnectedEvent{}
	for _, interval := range intervals {
		if !interval.connectTime.Before(windowStart) && !interval.connectTime.After(now) {
			events = append(events, &ConnectedEvent{
				EventTime: interval.connectTime,
				Connected: true,
			})
		}
		if interval.disconnectTime != nil &&
			!interval.disconnectTime.Before(windowStart) &&
			!interval.disconnectTime.After(now) {
			events = append(events, &ConnectedEvent{
				EventTime: *interval.disconnectTime,
				Connected: false,
			})
		}
	}
	sort.Slice(events, func(i, j int) bool {
		return events[i].EventTime.Before(events[j].EventTime)
	})
	return events
}
