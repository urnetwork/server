package model

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Per-client data caps and usage for embedded clients (EMBED1.md).
//
// Usage: every top-level client's billable bytes are metered. Settlement adds
// the bytes of each contract its client paid for to a redis counter per
// (block, shard) (RecordClientDataUsage, a post after the settlement commit);
// the RollupClientDataUsage task drains closed blocks into
// network_client_data_usage, the client's usage per UTC calendar month. A
// child client's bytes count for its top-level client.
//
// Caps: a top-level client can have an optional monthly cap (resets at the
// start of each UTC calendar month) and an optional running-total cap (no
// automatic reset; the backend raises or clears it, or resets the counter).
// A cap of zero pauses the client. The running total counts from when the
// client's first cap request was made, or from its last reset.
//
// Enforcement: escrow admission (`createTransferEscrowInTx`) refuses positive
// contracts paid by a capped top-level client, or any of its child clients,
// with the ordinary "Insufficient balance" refusal. The capped set is an
// in-memory snapshot refreshed every `clientDataCapActiveRefresh`, so a payer
// in a network without a capped client adds no query. A cap takes effect
// within about a minute and a half of the usage that crosses it (block close,
// drain, snapshot refresh); a client can exceed a cap by that much traffic
// plus any contracts already open.

// a large but finite bound that keeps the sums far from int64 overflow
const MaxClientDataCapByteLimit = ByteCount(1) << 60

const DefaultClientDataCapListLimit = 100
const MaxClientDataCapListLimit = 1000

const ClientDataUsageBlockDuration = 30 * time.Second
const ClientDataUsageRollupInterval = 30 * time.Second

// One redis hash per (block, shard), sharded by client id like the
// reliability counters so a block's fleet-wide write load spreads across
// cluster slots. field = the 16 raw bytes of the paying client id,
// value = accumulated billable bytes.
const clientDataUsageShardCount = 32

// memory backstop; the rollup deletes a drained hash
const clientDataUsageRedisTtl = 15 * time.Minute

// monthly usage is kept for billing for a little over a year
const ClientDataUsageRetention = 400 * 24 * time.Hour

// a drained (block, shard) cannot reappear after its hash ttl
const clientDataUsageDrainRetention = 24 * time.Hour

const clientDataUsageReapLimit = 1000

const clientDataCapActiveRefresh = 5 * time.Second

const clientDataCapTopLevelClientIdMaxCount = 100_000

const (
	ClientDataCapReasonMonthly = "monthly"
	ClientDataCapReasonTotal   = "total"
)

var clientDataUsageRecordResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_client_data_usage_record_total",
	Help: "Billable byte records added to the per-client data usage meter, by result.",
}, []string{"result"})

var clientDataUsageDrainResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_client_data_usage_drain_total",
	Help: "Drained (block, shard) data usage hashes, by result.",
}, []string{"result"})

func init() {
	prometheus.MustRegister(clientDataUsageRecordResults, clientDataUsageDrainResults)
}

func clientDataCapMonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func clientDataCapMonthEnd(monthStart time.Time) time.Time {
	return monthStart.AddDate(0, 1, 0)
}

func clientDataCapLimitMessage(limit *ByteCount) string {
	if limit == nil {
		return ""
	}
	if *limit < 0 {
		return "A byte limit must be zero or more."
	}
	if MaxClientDataCapByteLimit < *limit {
		return fmt.Sprintf("A byte limit must be at most %d.", MaxClientDataCapByteLimit)
	}
	return ""
}

// mergeClientDataCapLimit applies one limit of a merge request: an unset limit
// keeps the current value, a set nil clears the cap and a set value sets it.
func mergeClientDataCapLimit(current *ByteCount, set bool, update *ByteCount) *ByteCount {
	if !set {
		return current
	}
	if update == nil {
		return nil
	}
	value := *update
	return &value
}

// clientDataCapReason reports the cap a client is at. The total wins when both
// are reached, since only it outlasts the month.
func clientDataCapReason(monthlyByteLimit *ByteCount, monthlyUsedByteCount ByteCount, totalByteLimit *ByteCount, totalUsedByteCount ByteCount) (capped bool, reason string) {
	if totalByteLimit != nil && *totalByteLimit <= totalUsedByteCount {
		return true, ClientDataCapReasonTotal
	}
	if monthlyByteLimit != nil && *monthlyByteLimit <= monthlyUsedByteCount {
		return true, ClientDataCapReasonMonthly
	}
	return false, ""
}

func encodeClientDataCapCursor(clientId server.Id) string {
	return base64.RawURLEncoding.EncodeToString(clientId.Bytes())
}

// decodeClientDataCapCursor accepts the empty cursor as the start of the list.
func decodeClientDataCapCursor(cursor string) (afterClientId server.Id, ok bool) {
	if cursor == "" {
		return server.Id{}, true
	}
	idBytes, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(idBytes) != 16 {
		return server.Id{}, false
	}
	afterClientId, err = server.IdFromBytes(idBytes)
	if err != nil {
		return server.Id{}, false
	}
	return afterClientId, true
}

func parseClientDataCapListLimit(raw string) (limit int, ok bool) {
	if raw == "" {
		return DefaultClientDataCapListLimit, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || MaxClientDataCapListLimit < limit {
		return 0, false
	}
	return limit, true
}

// SetClientDataCapArgs is a merge request: an omitted limit keeps its current
// value, null clears the cap and a number sets it. A nil pointer alone cannot
// tell omitted from null, so decoding records which limits the request named.
// In Go, a non-nil limit is a set; SetMonthlyByteLimit(nil) or
// SetTotalByteLimit(nil) clears.
type SetClientDataCapArgs struct {
	ClientId         server.Id  `json:"client_id"`
	MonthlyByteLimit *ByteCount `json:"monthly_byte_limit"`
	TotalByteLimit   *ByteCount `json:"total_byte_limit"`
	ResetTotal       bool       `json:"reset_total,omitempty"`

	monthlyByteLimitNamed bool
	totalByteLimitNamed   bool
}

func (self *SetClientDataCapArgs) UnmarshalJSON(data []byte) error {
	type setClientDataCapArgsFields SetClientDataCapArgs
	var fields setClientDataCapArgsFields
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var named map[string]json.RawMessage
	if err := json.Unmarshal(data, &named); err != nil {
		return err
	}
	*self = SetClientDataCapArgs(fields)
	_, self.monthlyByteLimitNamed = named["monthly_byte_limit"]
	_, self.totalByteLimitNamed = named["total_byte_limit"]
	return nil
}

func (self *SetClientDataCapArgs) SetMonthlyByteLimit(limit *ByteCount) {
	self.MonthlyByteLimit = limit
	self.monthlyByteLimitNamed = true
}

func (self *SetClientDataCapArgs) SetTotalByteLimit(limit *ByteCount) {
	self.TotalByteLimit = limit
	self.totalByteLimitNamed = true
}

func (self *SetClientDataCapArgs) monthlyByteLimitSet() bool {
	return self.monthlyByteLimitNamed || self.MonthlyByteLimit != nil
}

func (self *SetClientDataCapArgs) totalByteLimitSet() bool {
	return self.totalByteLimitNamed || self.TotalByteLimit != nil
}

type ClientDataCap struct {
	ClientId             server.Id  `json:"client_id"`
	MonthlyByteLimit     *ByteCount `json:"monthly_byte_limit"`
	MonthlyUsedByteCount ByteCount  `json:"monthly_used_byte_count"`
	MonthlyPeriodStart   time.Time  `json:"monthly_period_start"`
	MonthlyPeriodEnd     time.Time  `json:"monthly_period_end"`
	TotalByteLimit       *ByteCount `json:"total_byte_limit"`
	TotalUsedByteCount   ByteCount  `json:"total_used_byte_count"`
	// null when no cap request was ever made for the client
	TotalPeriodStart *time.Time `json:"total_period_start"`
	Capped           bool       `json:"capped"`
	CappedReason     string     `json:"capped_reason"`
}

type ClientDataCapError struct {
	Message string `json:"message"`
}

// On success the cap fields; on a refusal only `error`.
type ClientDataCapResult struct {
	*ClientDataCap
	Error *ClientDataCapError `json:"error,omitempty"`
}

type GetClientDataCapArgs struct {
	// optional for a client token
	ClientId string
}

type ListClientDataCapsArgs struct {
	Cursor string
	Limit  string
}

// Each client is the same object the single read returns.
type ListClientDataCapsResult struct {
	Clients    []*ClientDataCapResult `json:"clients"`
	NextCursor *string                `json:"next_cursor"`
	Error      *ClientDataCapError    `json:"error,omitempty"`
}

func clientDataCapErrorResult(message string) *ClientDataCapResult {
	return &ClientDataCapResult{Error: &ClientDataCapError{Message: message}}
}

const clientDataCapNetworkSessionMessage = "Requires the network's root token or an API key."

// clientDataCapNetworkSession accepts a network-level session: the network's
// root JWT or an API key session (session/client_session.go), neither of which
// carries a client id. Pro mode and other JWT-only claims are not read.
func clientDataCapNetworkSession(clientSession *session.ClientSession) bool {
	return clientSession != nil &&
		clientSession.ByJwt != nil &&
		clientSession.ByJwt.ClientId == nil &&
		clientSession.ByJwt.NetworkId != (server.Id{})
}

// a cap row as stored
type clientDataCapRow struct {
	clientId                 server.Id
	networkId                server.Id
	monthlyByteLimit         *ByteCount
	totalByteLimit           *ByteCount
	totalPeriodStart         time.Time
	totalUsedByteCount       ByteCount
	monthlyCappedPeriodStart *time.Time
	totalCapped              bool
}

// recomputeCapped derives the admission markers from the limits and usage.
func (self *clientDataCapRow) recomputeCapped(monthStart time.Time, monthlyUsedByteCount ByteCount) {
	if self.monthlyByteLimit != nil && *self.monthlyByteLimit <= monthlyUsedByteCount {
		self.monthlyCappedPeriodStart = &monthStart
	} else {
		self.monthlyCappedPeriodStart = nil
	}
	self.totalCapped = self.totalByteLimit != nil && *self.totalByteLimit <= self.totalUsedByteCount
}

func newClientDataCap(clientId server.Id, row *clientDataCapRow, monthlyUsedByteCount ByteCount, now time.Time) *ClientDataCap {
	monthStart := clientDataCapMonthStart(now)
	clientDataCap := &ClientDataCap{
		ClientId:             clientId,
		MonthlyUsedByteCount: monthlyUsedByteCount,
		MonthlyPeriodStart:   monthStart,
		MonthlyPeriodEnd:     clientDataCapMonthEnd(monthStart),
	}
	if row != nil {
		clientDataCap.MonthlyByteLimit = row.monthlyByteLimit
		clientDataCap.TotalByteLimit = row.totalByteLimit
		clientDataCap.TotalUsedByteCount = row.totalUsedByteCount
		totalPeriodStart := row.totalPeriodStart
		clientDataCap.TotalPeriodStart = &totalPeriodStart
	}
	clientDataCap.Capped, clientDataCap.CappedReason = clientDataCapReason(
		clientDataCap.MonthlyByteLimit,
		monthlyUsedByteCount,
		clientDataCap.TotalByteLimit,
		clientDataCap.TotalUsedByteCount,
	)
	return clientDataCap
}

// clientDataCapTopLevelClient finds the client in the network. A caller that
// requires a top-level client checks topLevel itself.
func clientDataCapTopLevelClient(ctx context.Context, query server.PgCanQuery, networkId server.Id, clientId server.Id) (topLevelClientId server.Id, topLevel bool, found bool) {
	result, err := query.Query(
		ctx,
		`
			SELECT COALESCE(source_client_id, client_id)
			FROM network_client
			WHERE
				client_id = $1 AND
				network_id = $2
		`,
		clientId,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&topLevelClientId))
			found = true
		}
	})
	return topLevelClientId, found && topLevelClientId == clientId, found
}

func readClientDataCapRow(ctx context.Context, query server.PgCanQuery, clientId server.Id, forUpdate bool) *clientDataCapRow {
	sql := `
		SELECT
			client_id,
			network_id,
			monthly_byte_limit,
			total_byte_limit,
			total_period_start,
			total_used_byte_count,
			monthly_capped_period_start,
			total_capped
		FROM network_client_data_cap
		WHERE client_id = $1
	`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	var row *clientDataCapRow
	result, err := query.Query(ctx, sql, clientId)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			row = &clientDataCapRow{}
			server.Raise(result.Scan(
				&row.clientId,
				&row.networkId,
				&row.monthlyByteLimit,
				&row.totalByteLimit,
				&row.totalPeriodStart,
				&row.totalUsedByteCount,
				&row.monthlyCappedPeriodStart,
				&row.totalCapped,
			))
		}
	})
	return row
}

func readClientDataUsage(ctx context.Context, query server.PgCanQuery, clientId server.Id, monthStart time.Time) ByteCount {
	usedByteCount := ByteCount(0)
	result, err := query.Query(
		ctx,
		`
			SELECT used_byte_count
			FROM network_client_data_usage
			WHERE
				client_id = $1 AND
				period_start = $2
		`,
		clientId,
		monthStart,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&usedByteCount))
		}
	})
	return usedByteCount
}

// SetClientDataCap merges a cap request for a top-level client of the caller's
// network (POST /network/client-data-cap).
// Refused unless the caller's network is Embed-enabled (network_embed_model.go).
func SetClientDataCap(setClientDataCap *SetClientDataCapArgs, clientSession *session.ClientSession) (*ClientDataCapResult, error) {
	if networkEmbedRefused(clientSession) {
		return clientDataCapErrorResult(NetworkEmbedNotEnabledMessage), nil
	}
	if !clientDataCapNetworkSession(clientSession) {
		return clientDataCapErrorResult(clientDataCapNetworkSessionMessage), nil
	}
	if setClientDataCap.ClientId == (server.Id{}) {
		return clientDataCapErrorResult("client_id is required."), nil
	}
	for _, limit := range []*ByteCount{setClientDataCap.MonthlyByteLimit, setClientDataCap.TotalByteLimit} {
		if message := clientDataCapLimitMessage(limit); message != "" {
			return clientDataCapErrorResult(message), nil
		}
	}

	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	clientId := setClientDataCap.ClientId

	var setResult *ClientDataCapResult
	server.Tx(ctx, func(tx server.PgTx) {
		setResult = nil
		now := server.NowUtc()

		_, topLevel, found := clientDataCapTopLevelClient(ctx, tx, networkId, clientId)
		if !found {
			setResult = clientDataCapErrorResult("Client not found in this network.")
			return
		}
		if !topLevel {
			setResult = clientDataCapErrorResult("Data caps apply to top-level clients.")
			return
		}

		// create-then-lock serializes concurrent first requests for a client,
		// so a merge never starts from a row another request is replacing
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_client_data_cap (
					client_id,
					network_id,
					total_period_start,
					create_time,
					update_time
				)
				VALUES ($1, $2, $3, $3, $3)
				ON CONFLICT (client_id) DO NOTHING
			`,
			clientId,
			networkId,
			now,
		))
		row := readClientDataCapRow(ctx, tx, clientId, true)
		if row == nil {
			server.Raise(fmt.Errorf("client data cap row missing after insert: %s", clientId))
		}

		row.monthlyByteLimit = mergeClientDataCapLimit(row.monthlyByteLimit, setClientDataCap.monthlyByteLimitSet(), setClientDataCap.MonthlyByteLimit)
		row.totalByteLimit = mergeClientDataCapLimit(row.totalByteLimit, setClientDataCap.totalByteLimitSet(), setClientDataCap.TotalByteLimit)
		if setClientDataCap.ResetTotal {
			row.totalUsedByteCount = 0
			row.totalPeriodStart = now
		}
		monthStart := clientDataCapMonthStart(now)
		monthlyUsedByteCount := readClientDataUsage(ctx, tx, clientId, monthStart)
		row.recomputeCapped(monthStart, monthlyUsedByteCount)

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_client_data_cap
				SET
					monthly_byte_limit = $2,
					total_byte_limit = $3,
					total_period_start = $4,
					total_used_byte_count = $5,
					monthly_capped_period_start = $6,
					total_capped = $7,
					update_time = $8
				WHERE client_id = $1
			`,
			clientId,
			row.monthlyByteLimit,
			row.totalByteLimit,
			row.totalPeriodStart,
			row.totalUsedByteCount,
			row.monthlyCappedPeriodStart,
			row.totalCapped,
			now,
		))

		setResult = &ClientDataCapResult{
			ClientDataCap: newClientDataCap(clientId, row, monthlyUsedByteCount, now),
		}
	})
	// this host enforces the change at once; others within the refresh
	clientDataCapActive.invalidate()
	return setResult, nil
}

// GetClientDataCap reads one client's caps and usage
// (GET /network/client-data-cap). A network session (root JWT or API key) reads
// any top-level client of its network; a client token reads its own, and a
// child client's token reads its top-level client's.
// Refused unless the caller's network is Embed-enabled (network_embed_model.go).
func GetClientDataCap(getClientDataCap *GetClientDataCapArgs, clientSession *session.ClientSession) (*ClientDataCapResult, error) {
	if networkEmbedRefused(clientSession) {
		return clientDataCapErrorResult(NetworkEmbedNotEnabledMessage), nil
	}
	if clientSession == nil || clientSession.ByJwt == nil || clientSession.ByJwt.NetworkId == (server.Id{}) {
		return clientDataCapErrorResult(clientDataCapNetworkSessionMessage), nil
	}
	var requestedClientId *server.Id
	if getClientDataCap.ClientId != "" {
		clientId, err := server.ParseId(getClientDataCap.ClientId)
		if err != nil {
			return clientDataCapErrorResult("Invalid client_id."), nil
		}
		requestedClientId = &clientId
	}

	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId

	var getResult *ClientDataCapResult
	server.Db(ctx, func(conn server.PgConn) {
		now := server.NowUtc()
		var targetClientId server.Id
		if clientSession.ByJwt.ClientId == nil {
			if requestedClientId == nil {
				getResult = clientDataCapErrorResult("client_id is required.")
				return
			}
			_, topLevel, found := clientDataCapTopLevelClient(ctx, conn, networkId, *requestedClientId)
			if !found {
				getResult = clientDataCapErrorResult("Client not found in this network.")
				return
			}
			if !topLevel {
				getResult = clientDataCapErrorResult("Data caps apply to top-level clients.")
				return
			}
			targetClientId = *requestedClientId
		} else {
			ownClientId := *clientSession.ByJwt.ClientId
			topLevelClientId, _, found := clientDataCapTopLevelClient(ctx, conn, networkId, ownClientId)
			if !found {
				getResult = clientDataCapErrorResult("Client not found in this network.")
				return
			}
			if requestedClientId != nil && *requestedClientId != ownClientId && *requestedClientId != topLevelClientId {
				getResult = clientDataCapErrorResult("A client token can only read its own data cap.")
				return
			}
			targetClientId = topLevelClientId
		}
		row := readClientDataCapRow(ctx, conn, targetClientId, false)
		monthlyUsedByteCount := readClientDataUsage(ctx, conn, targetClientId, clientDataCapMonthStart(now))
		getResult = &ClientDataCapResult{
			ClientDataCap: newClientDataCap(targetClientId, row, monthlyUsedByteCount, now),
		}
	})
	return getResult, nil
}

// ListClientDataCaps pages through the clients of the caller's network that
// have a cap set, in client id order (GET /network/client-data-caps).
// Refused unless the caller's network is Embed-enabled (network_embed_model.go).
func ListClientDataCaps(listClientDataCaps *ListClientDataCapsArgs, clientSession *session.ClientSession) (*ListClientDataCapsResult, error) {
	refuse := func(message string) (*ListClientDataCapsResult, error) {
		return &ListClientDataCapsResult{Error: &ClientDataCapError{Message: message}}, nil
	}
	if networkEmbedRefused(clientSession) {
		return refuse(NetworkEmbedNotEnabledMessage)
	}
	if !clientDataCapNetworkSession(clientSession) {
		return refuse(clientDataCapNetworkSessionMessage)
	}
	limit, ok := parseClientDataCapListLimit(listClientDataCaps.Limit)
	if !ok {
		return refuse(fmt.Sprintf("limit must be between 1 and %d.", MaxClientDataCapListLimit))
	}
	afterClientId, ok := decodeClientDataCapCursor(listClientDataCaps.Cursor)
	if !ok {
		return refuse("Invalid cursor.")
	}

	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	listResult := &ListClientDataCapsResult{
		Clients: []*ClientDataCapResult{},
	}
	server.Db(ctx, func(conn server.PgConn) {
		now := server.NowUtc()
		monthStart := clientDataCapMonthStart(now)
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_client_data_cap.client_id,
					network_client_data_cap.network_id,
					network_client_data_cap.monthly_byte_limit,
					network_client_data_cap.total_byte_limit,
					network_client_data_cap.total_period_start,
					network_client_data_cap.total_used_byte_count,
					COALESCE(network_client_data_usage.used_byte_count, 0)
				FROM network_client_data_cap
				INNER JOIN network_client ON
					network_client.client_id = network_client_data_cap.client_id
				LEFT JOIN network_client_data_usage ON
					network_client_data_usage.client_id = network_client_data_cap.client_id AND
					network_client_data_usage.period_start = $3
				WHERE
					network_client_data_cap.network_id = $1 AND
					(
						network_client_data_cap.monthly_byte_limit IS NOT NULL OR
						network_client_data_cap.total_byte_limit IS NOT NULL
					) AND
					$2 < network_client_data_cap.client_id
				ORDER BY network_client_data_cap.client_id
				LIMIT $4
			`,
			networkId,
			afterClientId,
			monthStart,
			limit+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &clientDataCapRow{}
				var monthlyUsedByteCount ByteCount
				server.Raise(result.Scan(
					&row.clientId,
					&row.networkId,
					&row.monthlyByteLimit,
					&row.totalByteLimit,
					&row.totalPeriodStart,
					&row.totalUsedByteCount,
					&monthlyUsedByteCount,
				))
				listResult.Clients = append(listResult.Clients, &ClientDataCapResult{
					ClientDataCap: newClientDataCap(row.clientId, row, monthlyUsedByteCount, now),
				})
			}
		})
	})
	if limit < len(listResult.Clients) {
		listResult.Clients = listResult.Clients[:limit]
		nextCursor := encodeClientDataCapCursor(listResult.Clients[limit-1].ClientId)
		listResult.NextCursor = &nextCursor
	}
	return listResult, nil
}

// The escrow admission snapshot: the top-level clients at a cap now, and their
// networks. Built from the capped markers (partial indexes), so the refresh
// reads only the capped rows.
type clientDataCapActiveSnapshot struct {
	loadTime   time.Time
	monthStart time.Time
	networkIds map[server.Id]bool
	clientIds  map[server.Id]bool
}

type clientDataCapActiveState struct {
	refreshLock sync.Mutex
	snapshot    atomic.Pointer[clientDataCapActiveSnapshot]
}

var clientDataCapActive = &clientDataCapActiveState{}

func (self *clientDataCapActiveState) stale(snapshot *clientDataCapActiveSnapshot, now time.Time) bool {
	return snapshot == nil ||
		clientDataCapActiveRefresh <= now.Sub(snapshot.loadTime) ||
		now.Before(snapshot.loadTime) ||
		!snapshot.monthStart.Equal(clientDataCapMonthStart(now))
}

func (self *clientDataCapActiveState) current(ctx context.Context, query server.PgCanQuery, now time.Time) *clientDataCapActiveSnapshot {
	snapshot := self.snapshot.Load()
	if !self.stale(snapshot, now) {
		return snapshot
	}
	if snapshot == nil {
		return self.refreshBlocking(ctx, query, now)
	}
	if refreshed, ok := self.tryRefresh(ctx, query, now); ok {
		return refreshed
	}
	// others keep using the stale snapshot during a refresh
	return snapshot
}

func (self *clientDataCapActiveState) refreshBlocking(ctx context.Context, query server.PgCanQuery, now time.Time) *clientDataCapActiveSnapshot {
	self.refreshLock.Lock()
	defer self.refreshLock.Unlock()
	return self.refresh(ctx, query, now)
}

func (self *clientDataCapActiveState) tryRefresh(ctx context.Context, query server.PgCanQuery, now time.Time) (*clientDataCapActiveSnapshot, bool) {
	if !self.refreshLock.TryLock() {
		return nil, false
	}
	defer self.refreshLock.Unlock()
	return self.refresh(ctx, query, now), true
}

// refresh must be called with refreshLock held.
func (self *clientDataCapActiveState) refresh(ctx context.Context, query server.PgCanQuery, now time.Time) *clientDataCapActiveSnapshot {
	if snapshot := self.snapshot.Load(); !self.stale(snapshot, now) {
		return snapshot
	}
	monthStart := clientDataCapMonthStart(now)
	snapshot := &clientDataCapActiveSnapshot{
		loadTime:   now,
		monthStart: monthStart,
		networkIds: map[server.Id]bool{},
		clientIds:  map[server.Id]bool{},
	}
	result, err := query.Query(
		ctx,
		`
			SELECT client_id, network_id
			FROM network_client_data_cap
			WHERE
				total_capped OR
				monthly_capped_period_start = $1 OR
				monthly_byte_limit = 0
		`,
		monthStart,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var clientId server.Id
			var networkId server.Id
			server.Raise(result.Scan(&clientId, &networkId))
			snapshot.clientIds[clientId] = true
			snapshot.networkIds[networkId] = true
		}
	})
	self.snapshot.Store(snapshot)
	return snapshot
}

func (self *clientDataCapActiveState) invalidate() {
	self.snapshot.Store(nil)
}

// A client's top-level client never changes (source_client_id is set at
// creation), so the mapping is cached for the process; only payers in a
// network with a capped client ever look it up.
var clientDataCapTopLevelClientIds sync.Map // client id -> top-level client id
var clientDataCapTopLevelClientIdCount atomic.Int64

func clientDataCapTopLevelClientId(ctx context.Context, query server.PgCanQuery, clientId server.Id) server.Id {
	if topLevelClientId, ok := clientDataCapTopLevelClientIds.Load(clientId); ok {
		return topLevelClientId.(server.Id)
	}
	topLevelClientId := clientId
	found := false
	result, err := query.Query(
		ctx,
		`
			SELECT COALESCE(source_client_id, client_id)
			FROM network_client
			WHERE client_id = $1
		`,
		clientId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&topLevelClientId))
			found = true
		}
	})
	if found {
		if clientDataCapTopLevelClientIdMaxCount < clientDataCapTopLevelClientIdCount.Add(1) {
			clientDataCapTopLevelClientIds.Clear()
			clientDataCapTopLevelClientIdCount.Store(1)
		}
		clientDataCapTopLevelClientIds.Store(clientId, topLevelClientId)
	}
	return topLevelClientId
}

// clientDataCapEscrowError is the escrow refusal for a paying client at a data
// cap: the same "Insufficient balance" a client sees for an empty balance
// (ContractError_InsufficientBalance). Zero-byte contracts keep working.
func clientDataCapEscrowError(
	ctx context.Context,
	query server.PgCanQuery,
	payerNetworkId server.Id,
	payerClientId server.Id,
	contractTransferByteCount ByteCount,
	now time.Time,
) error {
	if contractTransferByteCount <= 0 {
		return nil
	}
	snapshot := clientDataCapActive.current(ctx, query, now)
	if !snapshot.networkIds[payerNetworkId] {
		return nil
	}
	if !snapshot.clientIds[clientDataCapTopLevelClientId(ctx, query, payerClientId)] {
		return nil
	}
	return fmt.Errorf("Insufficient balance (%d).", 0)
}

func clientDataUsageBlockNumber(t time.Time) int64 {
	return t.UTC().UnixMilli() / int64(ClientDataUsageBlockDuration/time.Millisecond)
}

func clientDataUsageBlockStart(blockNumber int64) time.Time {
	return time.UnixMilli(blockNumber * int64(ClientDataUsageBlockDuration/time.Millisecond)).UTC()
}

func clientDataUsageShard(clientId server.Id) int {
	idBytes := clientId.Bytes()
	return int(idBytes[len(idBytes)-1]) % clientDataUsageShardCount
}

func clientDataUsageKey(blockNumber int64, shard int) string {
	return fmt.Sprintf("client_data_usage.%d.%d", blockNumber, shard)
}

// RecordClientDataUsage adds a paying client's billable bytes to the current
// block. It is best-effort: a redis error is counted and dropped, so metering
// can never fail a settlement. Call it with the current time; the rollup
// drains a block only after two full blocks have passed.
func RecordClientDataUsage(ctx context.Context, payerClientId server.Id, byteCount ByteCount, now time.Time) {
	if byteCount <= 0 {
		return
	}
	key := clientDataUsageKey(clientDataUsageBlockNumber(now), clientDataUsageShard(payerClientId))
	field := string(payerClientId.Bytes())
	var recordErr error
	server.HandleError(func() {
		server.Redis(ctx, func(r server.RedisClient) {
			// both commands target one hash key (one slot), so the transaction
			// is cluster-safe
			_, recordErr = r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.HIncrBy(ctx, key, field, byteCount)
				pipe.Expire(ctx, key, clientDataUsageRedisTtl)
				return nil
			})
		})
	}, func(err error) {
		recordErr = err
	})
	if recordErr != nil {
		clientDataUsageRecordResults.WithLabelValues("error").Inc()
		glog.V(1).Infof("[ncdc]record usage error: %s\n", recordErr)
		return
	}
	clientDataUsageRecordResults.WithLabelValues("ok").Inc()
}

// clientDataUsageRollupBlockNumbers returns the closed blocks that can still
// have a live hash: newer than the drained high-water mark and inside the hash
// ttl. A block is closed once two full blocks have passed since its start.
func clientDataUsageRollupBlockNumbers(currentBlockNumber int64, maxDrainedBlock int64, hasHighWater bool) []int64 {
	maxFinalBlockNumber := currentBlockNumber - 2
	retainedBlockCount := int64(clientDataUsageRedisTtl / ClientDataUsageBlockDuration)
	minBlockNumber := currentBlockNumber - retainedBlockCount - 1
	if hasHighWater && minBlockNumber < maxDrainedBlock+1 {
		minBlockNumber = maxDrainedBlock + 1
	}
	if maxFinalBlockNumber < minBlockNumber {
		return nil
	}
	blockNumbers := make([]int64, 0, maxFinalBlockNumber-minBlockNumber+1)
	for blockNumber := minBlockNumber; blockNumber <= maxFinalBlockNumber; blockNumber += 1 {
		blockNumbers = append(blockNumbers, blockNumber)
	}
	return blockNumbers
}

// parseClientDataUsageFields reads one drained hash. A malformed field is
// counted and skipped rather than failing the whole hash.
func parseClientDataUsageFields(fields map[string]string) map[server.Id]ByteCount {
	usage := map[server.Id]ByteCount{}
	for field, value := range fields {
		clientId, err := server.IdFromBytes([]byte(field))
		if err != nil {
			clientDataUsageDrainResults.WithLabelValues("malformed_field").Inc()
			continue
		}
		byteCount, err := strconv.ParseInt(value, 10, 64)
		if err != nil || byteCount <= 0 {
			clientDataUsageDrainResults.WithLabelValues("malformed_value").Inc()
			continue
		}
		usage[clientId] += byteCount
	}
	return usage
}

func clientDataUsageMaxDrainedBlock(ctx context.Context) (maxDrainedBlock int64, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT max_drained_block
				FROM network_client_data_usage_rollup
				WHERE singleton_id = 1
			`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&maxDrainedBlock))
				ok = true
			}
		})
	})
	return
}

// RollupClientDataUsage drains closed usage blocks into monthly usage and the
// cap markers. Each (block, shard) applies exactly once: its drain row commits
// with its usage, and the hash is deleted only after that commit, so a crash
// in between re-reads the hash and skips it.
func RollupClientDataUsage(ctx context.Context, now time.Time) {
	currentBlockNumber := clientDataUsageBlockNumber(now)
	maxDrainedBlock, hasHighWater := clientDataUsageMaxDrainedBlock(ctx)
	for _, blockNumber := range clientDataUsageRollupBlockNumbers(currentBlockNumber, maxDrainedBlock, hasHighWater) {
		for shard := 0; shard < clientDataUsageShardCount; shard += 1 {
			drainClientDataUsageShard(ctx, blockNumber, shard)
		}
	}

	// every block <= the last closed block is drained or was never written
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_client_data_usage_rollup (
					singleton_id,
					max_drained_block,
					update_time
				)
				VALUES (1, $1, $2)
				ON CONFLICT (singleton_id) DO UPDATE
				SET
					max_drained_block = GREATEST(network_client_data_usage_rollup.max_drained_block, $1),
					update_time = $2
			`,
			currentBlockNumber-2,
			server.NowUtc(),
		))
	})

	reapClientDataUsage(ctx, now)
}

func drainClientDataUsageShard(ctx context.Context, blockNumber int64, shard int) {
	key := clientDataUsageKey(blockNumber, shard)

	// a closed block has no writers, so a chunked scan is a consistent read.
	// Raise on error: an empty read would delete the hash and drop its usage.
	fields := map[string]string{}
	server.Redis(ctx, func(r server.RedisClient) {
		var cursor uint64
		for {
			kvs, nextCursor, err := r.HScan(ctx, key, cursor, "", 5000).Result()
			server.Raise(err)
			for i := 0; i+1 < len(kvs); i += 2 {
				fields[kvs[i]] = kvs[i+1]
			}
			if nextCursor == 0 {
				return
			}
			cursor = nextCursor
		}
	})
	if len(fields) == 0 {
		return
	}

	if usage := parseClientDataUsageFields(fields); 0 < len(usage) {
		applyClientDataUsageShard(ctx, blockNumber, shard, usage)
	}

	server.Redis(ctx, func(r server.RedisClient) {
		r.Del(ctx, key)
	})
}

func applyClientDataUsageShard(ctx context.Context, blockNumber int64, shard int, usage map[server.Id]ByteCount) {
	payerClientIds := make([]server.Id, 0, len(usage))
	for payerClientId := range usage {
		payerClientIds = append(payerClientIds, payerClientId)
	}
	blockStart := clientDataUsageBlockStart(blockNumber)
	monthStart := clientDataCapMonthStart(blockStart)

	server.Tx(ctx, func(tx server.PgTx) {
		now := server.NowUtc()
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_client_data_usage_drain (
					block_number,
					shard,
					drain_time
				)
				VALUES ($1, $2, $3)
				ON CONFLICT (block_number, shard) DO NOTHING
			`,
			blockNumber,
			shard,
			now,
		))
		if tag.RowsAffected() == 0 {
			// already applied before a crash; the caller deletes the hash
			clientDataUsageDrainResults.WithLabelValues("replay").Inc()
			return
		}

		// a child client's bytes count for its top-level client; a client
		// that no longer exists has no one to count for
		topLevelByteCounts := map[server.Id]ByteCount{}
		topLevelNetworkIds := map[server.Id]server.Id{}
		result, err := tx.Query(
			ctx,
			`
				SELECT
					client_id,
					COALESCE(source_client_id, client_id),
					network_id
				FROM network_client
				WHERE client_id = ANY($1)
			`,
			payerClientIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var payerClientId server.Id
				var topLevelClientId server.Id
				var networkId server.Id
				server.Raise(result.Scan(&payerClientId, &topLevelClientId, &networkId))
				topLevelByteCounts[topLevelClientId] += usage[payerClientId]
				topLevelNetworkIds[topLevelClientId] = networkId
			}
		})
		if len(topLevelByteCounts) == 0 {
			clientDataUsageDrainResults.WithLabelValues("no_clients").Inc()
			return
		}

		clientIds := make([]server.Id, 0, len(topLevelByteCounts))
		networkIds := make([]server.Id, 0, len(topLevelByteCounts))
		byteCounts := make([]int64, 0, len(topLevelByteCounts))
		for clientId, byteCount := range topLevelByteCounts {
			clientIds = append(clientIds, clientId)
			networkIds = append(networkIds, topLevelNetworkIds[clientId])
			byteCounts = append(byteCounts, byteCount)
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_client_data_usage (
					client_id,
					period_start,
					network_id,
					used_byte_count,
					update_time
				)
				SELECT
					usage.client_id,
					$1::timestamp,
					usage.network_id,
					usage.byte_count,
					$2::timestamp
				FROM unnest($3::uuid[], $4::uuid[], $5::bigint[]) AS usage(client_id, network_id, byte_count)
				ON CONFLICT (client_id, period_start) DO UPDATE
				SET
					used_byte_count = network_client_data_usage.used_byte_count + EXCLUDED.used_byte_count,
					update_time = EXCLUDED.update_time
			`,
			monthStart,
			now,
			clientIds,
			networkIds,
			byteCounts,
		))

		// the running total counts only blocks that started inside its period;
		// a marker set for an earlier month never replaces a later one
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_client_data_cap
				SET
					total_used_byte_count = network_client_data_cap.total_used_byte_count +
						CASE WHEN network_client_data_cap.total_period_start <= $2 THEN delta.byte_count ELSE 0 END,
					total_capped = network_client_data_cap.total_capped OR (
						network_client_data_cap.total_byte_limit IS NOT NULL AND
						network_client_data_cap.total_byte_limit <= network_client_data_cap.total_used_byte_count +
							CASE WHEN network_client_data_cap.total_period_start <= $2 THEN delta.byte_count ELSE 0 END
					),
					monthly_capped_period_start = CASE
						WHEN
							network_client_data_cap.monthly_byte_limit IS NOT NULL AND
							network_client_data_cap.monthly_byte_limit <= network_client_data_usage.used_byte_count
						THEN GREATEST(COALESCE(network_client_data_cap.monthly_capped_period_start, $3), $3)
						ELSE network_client_data_cap.monthly_capped_period_start
					END,
					update_time = $4
				FROM unnest($1::uuid[], $5::bigint[]) AS delta(client_id, byte_count)
				INNER JOIN network_client_data_usage ON
					network_client_data_usage.client_id = delta.client_id AND
					network_client_data_usage.period_start = $3
				WHERE network_client_data_cap.client_id = delta.client_id
			`,
			clientIds,
			blockStart,
			monthStart,
			now,
			byteCounts,
		))
		clientDataUsageDrainResults.WithLabelValues("ok").Inc()
	})
}

// reapClientDataUsage bounds the drain bookkeeping and the monthly usage
// history. Each run deletes at most `clientDataUsageReapLimit` rows of each.
func reapClientDataUsage(ctx context.Context, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network_client_data_usage_drain
				WHERE (block_number, shard) IN (
					SELECT block_number, shard
					FROM network_client_data_usage_drain
					WHERE drain_time < $1
					LIMIT $2
				)
			`,
			now.Add(-clientDataUsageDrainRetention),
			clientDataUsageReapLimit,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network_client_data_usage
				WHERE (client_id, period_start) IN (
					SELECT client_id, period_start
					FROM network_client_data_usage
					WHERE period_start < $1
					LIMIT $2
				)
			`,
			clientDataCapMonthStart(now.Add(-ClientDataUsageRetention)),
			clientDataUsageReapLimit,
		))
	})
}

// Testing_ResetClientDataCapState drops the admission snapshot and the
// top-level client cache so a test sees its own writes at once.
func Testing_ResetClientDataCapState() {
	clientDataCapActive.invalidate()
	clientDataCapTopLevelClientIds.Clear()
	clientDataCapTopLevelClientIdCount.Store(0)
}
