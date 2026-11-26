package model

// provider_model.go — the per-provider statistics APIs:
//   GET  /stats/providers                  StatsProviders          (last 24h)
//   POST /stats/providers-last-n           StatsProvidersLastN     (last_n hours)
//   POST /stats/provider-last-n            StatsProvider           (single provider, last_n hours)
//   POST /stats/providers-overview-last-n  StatsProvidersOverview  (network aggregate)
//
// All figures are real aggregates over the caller network's provider clients.
// Data sources:
//   transfer bytes   transfer_contract ⋈ contract_close (settled bytes)
//   contracts/clients transfer_contract (by create_time)
//   payout            transfer_escrow_sweep.provider_payouts (settled client shares)
//   uptime/events     network_client_connection (connect/disconnect)
//   search interest   search_provider_stats (see search_provider_stats_model.go)
//
// Reads are grouped by client in a single query per metric (never N+1) and are
// fronted by a short-TTL per-network cache at the handler layer.
// Payouts require the original sweep network as well as current client
// ownership; the provider view does not change historical account balances.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
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

// Settled transfer bytes per provider: the destination reports of the
// requested providers' contracts closed in the window. The closed contracts
// are selected first, and the reports are read only through contract_close's
// complete primary key, scoped to those contracts, even when a large provider
// array makes a whole-history join look inexpensive.
//
// Until 2026-11-18 each eligible contract's report was found by its own
// primary-key descent, about five index pages per contract. The largest
// networks close more than ten million contracts a day, and that read alone
// ran past the provider list's 2-minute fill budget. The eligible contracts
// are now grouped by the first four bytes of their id: contract ids lead with
// their creation millisecond (connect.NewId), so a group is about a minute of
// creation and one key range of the index. Each group's reports are read by
// one primary-key scan over its keys, which visits a leaf page once for all of
// its keys, and are matched back to their contracts by id. Ids without the
// time prefix group less tightly; the totals are the same either way.
//
// Since the contract tables were partitioned by day (2026-11-25), PostgreSQL
// plans each partition on its own, and the provider array is read through a
// lateral subquery (providerStatsProviderArraySql). A group's lookup also
// bounds the id by the first and last id of its prefix, so only the
// partition that holds the group runs: the array alone prunes nothing, and
// every partition of contract_close sorted and searched every group's array,
// six partitions on 2026-11-26 and one more each day until the legacy
// partition returns. A group whose prefix spans midnight runs two.
const providerStatsTransferBytesSQL = `
	WITH eligible AS MATERIALIZED (
		SELECT contract.contract_id, contract.destination_id
		FROM ` + providerStatsProviderArraySql + `
		CROSS JOIN LATERAL (
			SELECT contract_id, destination_id
			FROM transfer_contract
			WHERE destination_id = ANY(provider.destination_ids) AND close_time >= $2
			OFFSET 0
		) contract
	), id_range AS MATERIALIZED (
		SELECT
			array_agg(contract_id) AS contract_ids,
			(id_prefix || '000000000000000000000000')::uuid AS first_contract_id,
			(id_prefix || 'ffffffffffffffffffffffff')::uuid AS last_contract_id
		FROM (
			SELECT contract_id, encode(substr(uuid_send(contract_id), 1, 4), 'hex') AS id_prefix
			FROM eligible
		) eligible_prefix
		GROUP BY id_prefix
	), report AS MATERIALIZED (
		SELECT cc.contract_id, cc.used_transfer_byte_count
		FROM id_range
		CROSS JOIN LATERAL (
			SELECT contract_id, used_transfer_byte_count
			FROM contract_close
			WHERE contract_id = ANY(id_range.contract_ids)
				AND contract_id BETWEEN id_range.first_contract_id AND id_range.last_contract_id
				AND party = 'destination'
			OFFSET 0
		) cc
	)
	SELECT eligible.destination_id, COALESCE(SUM(report.used_transfer_byte_count), 0)
	FROM report
	JOIN eligible ON eligible.contract_id = report.contract_id
	GROUP BY eligible.destination_id
`

// The provider array $1 of a per-provider contract read, as the relation
// provider with the one column destination_ids. A read takes its providers'
// contracts from a lateral subquery, `destination_id =
// ANY(provider.destination_ids)`, and both subqueries end in OFFSET 0, so
// neither is pulled up and the array reaches the contract scans as an
// executor parameter.
//
// PostgreSQL estimates `destination_id = ANY(array)` from each partition's
// statistics, element by element. Since the contract tables were partitioned
// by day (2026-11-25) a day is planned on its own, and a day's statistics hold
// about 130,000 distinct destinations while the largest networks pass 100,000
// to 300,000 providers. Such a read then expected half or more of each filled
// day to match, and read the days whole with a sequential scan: 172M rows for
// each of a provider list fill's two contract reads on 2026-11-26, where the
// network's contracts were 0 to 7% of a day, and the reads ran 40 s to past
// the fill's 2-minute budget. The plain table was planned whole, and its
// destination index stayed the cheaper read. An array that reaches a scan as
// an executor parameter is estimated at ten elements whatever its length, so
// every partition is read through its destination index, reading only the
// network's contracts. Callers pass the ids in uuid order
// (providerStatsIdStrings): the index scan of each partition sorts the array
// first, which is then a pass over sorted input.
const providerStatsProviderArraySql = `(SELECT $1::uuid[] AS destination_ids OFFSET 0) provider`

// Contracts opened in the window and the distinct clients they served, per
// provider. The window start bounds the contract id as well ($3,
// providerStatsCreatedFromContractId), so the partitions minted before it are
// not read: a contract created in the window has an id of its creation
// millisecond, within ContractIdCreateTimeMargin.
const providerStatsContractsSQL = `
	SELECT contract.destination_id, COUNT(*), COUNT(DISTINCT contract.source_id)
	FROM ` + providerStatsProviderArraySql + `
	CROSS JOIN LATERAL (
		SELECT destination_id, source_id
		FROM transfer_contract
		WHERE destination_id = ANY(provider.destination_ids) AND create_time >= $2 AND contract_id >= $3
		OFFSET 0
	) contract
	GROUP BY contract.destination_id
`

// The network overview's settled transfer bytes per close day, summed over its
// providers. Its providers' contracts are read as the provider list's are
// (providerStatsProviderArraySql); each contract's report is found by its key,
// in the one partition of contract_close that holds it.
const providerStatsOverviewTransferBytesSQL = `
	SELECT to_char(contract.close_time, 'YYYY-MM-DD') AS day, COALESCE(SUM(cc.used_transfer_byte_count), 0)
	FROM ` + providerStatsProviderArraySql + `
	CROSS JOIN LATERAL (
		SELECT contract_id, close_time
		FROM transfer_contract
		WHERE destination_id = ANY(provider.destination_ids) AND close_time >= $2
		OFFSET 0
	) contract
	INNER JOIN contract_close cc ON
		cc.contract_id = contract.contract_id AND cc.party = 'destination'
	GROUP BY day
`

// The network overview's contracts opened per day and the distinct clients
// they served, bounded as providerStatsContractsSQL is.
const providerStatsOverviewContractsSQL = `
	SELECT to_char(contract.create_time, 'YYYY-MM-DD') AS day, COUNT(*), COUNT(DISTINCT contract.source_id)
	FROM ` + providerStatsProviderArraySql + `
	CROSS JOIN LATERAL (
		SELECT create_time, source_id
		FROM transfer_contract
		WHERE destination_id = ANY(provider.destination_ids) AND create_time >= $2 AND contract_id >= $3
		OFFSET 0
	) contract
	GROUP BY day
`

// The smallest contract id a contract created at or after windowStart has
// (ContractIdCreateTimeRange).
func providerStatsCreatedFromContractId(windowStart time.Time) server.Id {
	lower, _ := ContractIdCreateTimeRange(windowStart, windowStart)
	return lower
}

// The provider ids of a per-provider contract read in uuid order, which is the
// order of their canonical text (CODESTYLE.md, Identifiers).
func providerStatsIdStrings(ids []server.Id) []string {
	idStrs := idStrings(ids)
	slices.Sort(idStrs)
	return idStrs
}

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
	PayoutAsset            string            `json:"payout_asset"`
	PayoutBasis            string            `json:"payout_basis"`
	SnPayoutIncluded       bool              `json:"sn_payout_included"`
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
	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId

	// enumerate the network's active top-level provider clients
	order := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, providerStatsClientsSQL, networkId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				order = append(order, clientId)
			}
		})
	})
	if len(order) == 0 {
		return &StatsProvidersResult{
			CreatedTime: server.NowUtc(),
			Providers:   []*ProviderStats{},
		}, nil
	}
	ids := providerStatsIdStrings(order)

	// Three reads, each on its own pooled primary connection: the payout
	// attribution (it reads the network's sweeps, not its provider list), the
	// contract counts with the search and connection reads, and the transfer
	// bytes on this goroutine. For the largest networks each takes tens of
	// seconds; in sequence they passed the list's 2-minute fill budget
	// (2026-11-18), and together they take as long as the slowest. Payout
	// authority and metrics stay on the primary: replica lag must not make a
	// retained earning boundary look absent when policy is removed. Every read
	// is joined before this returns, a raise included, and a raise here first
	// cancels the others.
	readsCtx, cancelReads := context.WithCancel(ctx)
	payout := startProviderStatsRead(readsCtx, func(conn server.PgConn) (map[server.Id]map[string]NanoCents, error) {
		return queryProviderPayoutStats(readsCtx, conn, networkId, windowStart, now)
	})
	defer payout.wait()
	activity := startProviderStatsRead(readsCtx, func(conn server.PgConn) (*providerStatsActivity, error) {
		return queryProviderStatsActivity(readsCtx, conn, ids, windowStart, now), nil
	})
	defer activity.wait()
	defer cancelReads()

	// transfer bytes (settled) per provider
	transferBytes := map[server.Id]int64{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, providerStatsTransferBytesSQL, ids, windowStart)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var byteCount int64
				server.Raise(result.Scan(&clientId, &byteCount))
				transferBytes[clientId] = byteCount
			}
		})
	})
	reads, err := activity.result()
	if err != nil {
		return nil, err
	}

	// Validate whole network allocations before selecting current providers.
	clientDayPayouts, err := payout.result()
	if err != nil {
		return nil, err
	}
	payoutNanoCents := map[server.Id]NanoCents{}
	for _, clientId := range order {
		for _, amount := range clientDayPayouts[clientId] {
			if math.MaxInt64-payoutNanoCents[clientId] < amount {
				return nil, fmt.Errorf("provider payout total exceeds nanocent range for client %s", clientId)
			}
			payoutNanoCents[clientId] += amount
		}
	}

	providers := []*ProviderStats{}
	for _, clientId := range order {
		ivals := reads.intervals[clientId]
		providers = append(providers, &ProviderStats{
			ClientId:               clientId,
			Connected:              anyActive(ivals),
			ConnectedEventsLast24h: connectedEvents(ivals, windowStart, now),
			UptimeLast24h:          uptimeHours(ivals, windowStart, now),
			TransferDataLast24h:    bytesToGib(transferBytes[clientId]),
			PayoutLast24h:          NanoCentsToUsd(payoutNanoCents[clientId]),
			PayoutAsset:            "USDC",
			PayoutBasis:            "legacy_escrow_credit_by_sweep_time_not_transfer_confirmation",
			SearchInterestLast24h:  reads.searchInterest[clientId],
			ContractsLast24h:       reads.contracts[clientId],
			ClientsLast24h:         reads.clients[clientId],
		})
	}

	return &StatsProvidersResult{
		CreatedTime: server.NowUtc(),
		Providers:   providers,
	}, nil
}

// The provider list's per-provider activity apart from transfer bytes.
type providerStatsActivity struct {
	contracts      map[server.Id]int
	clients        map[server.Id]int
	searchInterest map[server.Id]int
	intervals      map[server.Id][]*connectionInterval
}

func queryProviderStatsActivity(
	ctx context.Context,
	conn server.PgConn,
	ids []string,
	windowStart time.Time,
	now time.Time,
) *providerStatsActivity {
	activity := &providerStatsActivity{
		contracts:      map[server.Id]int{},
		clients:        map[server.Id]int{},
		searchInterest: map[server.Id]int{},
	}

	// contracts + distinct served clients per provider
	result, err := conn.Query(ctx, providerStatsContractsSQL, ids, windowStart, providerStatsCreatedFromContractId(windowStart))
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var clientId server.Id
			var contractCount int
			var clientCount int
			server.Raise(result.Scan(&clientId, &contractCount, &clientCount))
			activity.contracts[clientId] = contractCount
			activity.clients[clientId] = clientCount
		}
	})

	// search interest per provider
	result, err = conn.Query(
		ctx,
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
			activity.searchInterest[clientId] = count
		}
	})

	// connection intervals per provider -> uptime, events, current state
	activity.intervals = queryConnectionIntervals(ctx, conn, ids, windowStart, now)
	return activity
}

// One read on its own pooled primary connection, started before its caller's
// other reads and joined before the caller returns. The read's raise, such as
// the db layer's stop when its context ends, is raised again in the goroutine
// that takes the result.
type providerStatsRead[R any] struct {
	done   chan struct{}
	value  R
	err    error
	raised any
}

func startProviderStatsRead[R any](ctx context.Context, read func(server.PgConn) (R, error)) *providerStatsRead[R] {
	self := &providerStatsRead[R]{done: make(chan struct{})}
	go func() {
		defer close(self.done)
		defer func() {
			self.raised = recover()
		}()
		server.Db(ctx, func(conn server.PgConn) {
			self.value, self.err = read(conn)
		})
	}()
	return self
}

// Waits for the read and returns its result, raising its raise again.
func (self *providerStatsRead[R]) result() (R, error) {
	<-self.done
	if self.raised != nil {
		panic(self.raised)
	}
	return self.value, self.err
}

// Waits for the read without taking its result.
func (self *providerStatsRead[R]) wait() {
	<-self.done
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
	var returnErr error
	// Keep payout-boundary authority on the same primary owner as its metrics.
	server.Db(clientSession.Ctx, func(conn server.PgConn) {
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

		// The snapshot's original network owns this client's daily revenue.
		clientDayPayouts, err := queryProviderPayoutStats(clientSession.Ctx, conn, networkId, windowStart, now)
		if err != nil {
			returnErr = err
			return
		}
		for day, amount := range clientDayPayouts[clientId] {
			payout[day] = NanoCentsToUsd(amount)
		}

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
	if returnErr != nil {
		return nil, returnErr
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
	var returnErr error

	// Keep payout-boundary authority on the same primary owner as its metrics.
	server.Db(clientSession.Ctx, func(conn server.PgConn) {
		// enumerate the network's active top-level provider clients
		providerIds := []server.Id{}
		result, err := conn.Query(
			clientSession.Ctx,
			providerStatsClientsSQL,
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
		ids := providerStatsIdStrings(providerIds)

		// transfer bytes per day (summed across providers)
		result, err = conn.Query(
			clientSession.Ctx,
			providerStatsOverviewTransferBytesSQL,
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

		// Sum exact shares for the same visible providers as the other metrics.
		// Historical network account totals remain independent of this view.
		clientDayPayouts, err := queryProviderPayoutStats(clientSession.Ctx, conn, networkId, windowStart, now)
		if err != nil {
			returnErr = err
			return
		}
		dayPayouts := map[string]NanoCents{}
		for _, clientId := range providerIds {
			for day, amount := range clientDayPayouts[clientId] {
				if math.MaxInt64-dayPayouts[day] < amount {
					returnErr = fmt.Errorf("provider payout total exceeds nanocent range for network %s on %s", networkId, day)
					return
				}
				dayPayouts[day] += amount
			}
		}
		for day, amount := range dayPayouts {
			payout[day] = NanoCentsToUsd(amount)
		}

		// contracts + distinct served clients per day
		result, err = conn.Query(
			clientSession.Ctx,
			providerStatsOverviewContractsSQL,
			ids,
			windowStart,
			providerStatsCreatedFromContractId(windowStart),
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
	if returnErr != nil {
		return nil, returnErr
	}

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
