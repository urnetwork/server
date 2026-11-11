package quotahost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The Quota of a host with no Postgres (§19.4: evs, gkm, sockets, tlsedge, mxedge), by plan-QUOTA-host-reach's
// answer (QD-055): limits, the over-limit list, usage batches, a payer's draw order and the spent mark over
// api's internal surface, the transport; CountRate and the lease reservations straight to redis, the host's
// ServiceHost.Redis or this process's own. It runs until ctx ends or Drain.
//
// NewHttpQuotaApi is the transport: api's routes under server.InternalGatePathPrefix, each call on the host's
// own bearer (server.LoadInternalGateCredential, plan-TLS-internal-credentials' scheme), and NewBlockQuota
// builds the whole Quota as a block's main hands it to its host. A host with no transport passes nil: every
// call that needs api answers ErrQuotaApiUnavailable and the Quota is never Ready, the interim
// plan-QUOTA-host-reach's resolution gives, so a host whose bearer or api url is missing stays not ready on
// these calls. Tests pass a fake transport.
func NewApiQuota(ctx context.Context, transport QuotaApiTransport, redis model.QuotaRateRedis, clock sidecar.Clock, settings *QuotaSettings) (*HostQuota, error) {
	if transport == nil {
		transport = unavailableQuotaApi{}
	}
	if redis == nil {
		return nil, errors.New("quotahost: an api-backed Quota needs the host's Redis")
	}
	return newHostQuota(ctx, transport, redis, clock, settings)
}

// api's internal quota surface as a host reaches it: one call a read or write, each on the caller's
// credential. The calls are QuotaSource's.
type QuotaApiTransport interface {
	QuotaSource
}

// What a host with no transport answers.
var ErrQuotaApiUnavailable = errors.New("quotahost: api's internal quota surface is not reachable from this host")

// The transport of a host that has none: every call fails closed.
type unavailableQuotaApi struct{}

func (unavailableQuotaApi) DrawOrder(ctx context.Context, networkId server.Id) ([]*LeaseBalance, error) {
	return nil, ErrQuotaApiUnavailable
}

func (unavailableQuotaApi) MarkSpent(ctx context.Context, networkId server.Id) error {
	return ErrQuotaApiUnavailable
}

func (unavailableQuotaApi) LoadLimits(ctx context.Context, networkId server.Id) (*sidecar.QuotaLimits, error) {
	return nil, ErrQuotaApiUnavailable
}

func (unavailableQuotaApi) LoadOverLimit(ctx context.Context) (*sidecar.QuotaOverLimit, error) {
	return nil, ErrQuotaApiUnavailable
}

func (unavailableQuotaApi) ApplyUsageBatch(ctx context.Context, batch *sidecar.UsageBatch, carves []*model.UsageCarve) error {
	return ErrQuotaApiUnavailable
}

// The internal surface's routes on api (server/api/handlers/quota_service_handlers.go serves them), each under
// server.InternalGatePathPrefix. A path ending in / takes the network id after it.
const (
	// GET: the network's sidecar.QuotaLimits.
	QuotaApiLimitsPath = server.InternalGatePathPrefix + "limits/"
	// GET: QuotaApiOverLimit.
	QuotaApiOverLimitPath = server.InternalGatePathPrefix + "over-limit"
	// GET: QuotaApiDrawOrder.
	QuotaApiDrawOrderPath = server.InternalGatePathPrefix + "draw-order/"
	// POST, no body: marks the network spent.
	QuotaApiSpentPath = server.InternalGatePathPrefix + "spent/"
	// POST QuotaApiUsageBatch: QuotaApiUsageBatchResult.
	QuotaApiUsageBatchPath = server.InternalGatePathPrefix + "usage-batch"
)

// The over-limit list on the wire: the networks over their storage limit and the spent ones, apart, each with
// the refusal a host answers (sidecar.QuotaOverLimit, whose maps JSON cannot key by an id).
type QuotaApiOverLimit struct {
	Storage []*QuotaApiNetworkRefusal `json:"storage"`
	Spent   []*QuotaApiNetworkRefusal `json:"spent"`
}

type QuotaApiNetworkRefusal struct {
	NetworkId server.Id             `json:"network_id"`
	Refusal   *sidecar.QuotaRefusal `json:"refusal"`
}

func NewQuotaApiOverLimit(overLimit *sidecar.QuotaOverLimit) *QuotaApiOverLimit {
	refusals := func(networkIdRefusals map[connect.Id]*sidecar.QuotaRefusal) []*QuotaApiNetworkRefusal {
		networkRefusals := []*QuotaApiNetworkRefusal{}
		for networkId, refusal := range networkIdRefusals {
			networkRefusals = append(networkRefusals, &QuotaApiNetworkRefusal{NetworkId: server.Id(networkId), Refusal: refusal})
		}
		return networkRefusals
	}
	return &QuotaApiOverLimit{
		Storage: refusals(overLimit.StorageNetworkIdRefusals),
		Spent:   refusals(overLimit.SpentNetworkIdRefusals),
	}
}

// The list as the Quota answers it. A network listed twice in one half, or listed with no refusal, makes the
// answer unusable.
func (self *QuotaApiOverLimit) OverLimit() (*sidecar.QuotaOverLimit, error) {
	networkIdRefusals := func(networkRefusals []*QuotaApiNetworkRefusal) (map[connect.Id]*sidecar.QuotaRefusal, error) {
		refusals := map[connect.Id]*sidecar.QuotaRefusal{}
		for _, networkRefusal := range networkRefusals {
			if networkRefusal == nil || networkRefusal.Refusal == nil || networkRefusal.NetworkId == (server.Id{}) {
				return nil, errors.New("an over-limit entry with no network or refusal")
			}
			networkId := connect.Id(networkRefusal.NetworkId)
			if _, ok := refusals[networkId]; ok {
				return nil, errors.New("a network listed twice")
			}
			refusals[networkId] = networkRefusal.Refusal
		}
		return refusals, nil
	}
	storage, err := networkIdRefusals(self.Storage)
	if err != nil {
		return nil, err
	}
	spent, err := networkIdRefusals(self.Spent)
	if err != nil {
		return nil, err
	}
	return &sidecar.QuotaOverLimit{StorageNetworkIdRefusals: storage, SpentNetworkIdRefusals: spent}, nil
}

// A payer's draw order on the wire: LeaseBalance's, in escrowDrawLess's order.
type QuotaApiDrawOrder struct {
	Balances []*QuotaApiBalance `json:"balances"`
}

type QuotaApiBalance struct {
	BalanceId server.Id `json:"balance_id"`
	// balance_byte_count when it was read, 0 when it is negative
	CreditByteCount int64 `json:"credit_byte_count"`
	// a team_credit line, drawn last (D-QUOTA-9)
	Credit  bool      `json:"credit"`
	EndTime time.Time `json:"end_time"`
}

// A usage batch on the wire: sidecar.UsageBatch with the lease carves its host made (plan-QUOTA-lease-debits),
// which model.ApplyUsageBatch takes.
type QuotaApiUsageBatch struct {
	BatchId     server.Id             `json:"batch_id"`
	Producer    string                `json:"producer"`
	WindowStart time.Time             `json:"window_start"`
	Events      []*QuotaApiUsageEvent `json:"events"`
	Carves      []*QuotaApiUsageCarve `json:"carves"`
}

type QuotaApiUsageEvent struct {
	NetworkId         server.Id `json:"network_id"`
	Meter             string    `json:"meter"`
	TransferByteCount int64     `json:"transfer_byte_count"`
	StoredByteDelta   int64     `json:"stored_byte_delta"`
}

type QuotaApiUsageCarve struct {
	UsageId   server.Id `json:"usage_id"`
	NetworkId server.Id `json:"network_id"`
	Meter     string    `json:"meter"`
	BalanceId server.Id `json:"balance_id"`
	ByteCount int64     `json:"byte_count"`
}

type QuotaApiUsageBatchResult struct {
	// false when the batch id was applied before, so nothing applied twice
	Applied bool `json:"applied"`
}

func NewQuotaApiUsageBatch(batch *sidecar.UsageBatch, carves []*model.UsageCarve) *QuotaApiUsageBatch {
	wire := &QuotaApiUsageBatch{
		BatchId:     server.Id(batch.BatchId),
		Producer:    batch.Producer,
		WindowStart: batch.WindowStart,
		Events:      []*QuotaApiUsageEvent{},
		Carves:      []*QuotaApiUsageCarve{},
	}
	for _, event := range batch.Events {
		wire.Events = append(wire.Events, &QuotaApiUsageEvent{
			NetworkId:         server.Id(event.NetworkId),
			Meter:             event.Meter,
			TransferByteCount: event.TransferByteCount,
			StoredByteDelta:   event.StoredByteDelta,
		})
	}
	for _, carve := range carves {
		wire.Carves = append(wire.Carves, &QuotaApiUsageCarve{
			UsageId:   carve.UsageId,
			NetworkId: carve.NetworkId,
			Meter:     carve.Meter,
			BalanceId: carve.BalanceId,
			ByteCount: carve.ByteCount,
		})
	}
	return wire
}

// The batch and its carves as model.ApplyUsageBatch takes them, which judges what they say: this only converts,
// refusing a hole in either list.
func (self *QuotaApiUsageBatch) UsageBatch() (*sidecar.UsageBatch, []*model.UsageCarve, error) {
	batch := &sidecar.UsageBatch{
		BatchId:     connect.Id(self.BatchId),
		Producer:    self.Producer,
		WindowStart: self.WindowStart,
	}
	for _, event := range self.Events {
		if event == nil {
			return nil, nil, errors.New("a batch with a null event")
		}
		batch.Events = append(batch.Events, &sidecar.UsageEvent{
			NetworkId:         connect.Id(event.NetworkId),
			Meter:             event.Meter,
			TransferByteCount: event.TransferByteCount,
			StoredByteDelta:   event.StoredByteDelta,
		})
	}
	carves := []*model.UsageCarve{}
	for _, carve := range self.Carves {
		if carve == nil {
			return nil, nil, errors.New("a batch with a null carve")
		}
		carves = append(carves, &model.UsageCarve{
			UsageId:   carve.UsageId,
			NetworkId: carve.NetworkId,
			Meter:     carve.Meter,
			BalanceId: carve.BalanceId,
			ByteCount: carve.ByteCount,
		})
	}
	return batch, carves, nil
}

type QuotaApiSettings struct {
	// The most bytes one answer may hold; a longer one fails the call. 16 MiB, far past an over-limit list
	// of every network a host would hold in memory.
	MaxResponseByteCount int64
	// The most bytes of a refusal's body its error quotes.
	MaxRefusalByteCount int64
}

func DefaultQuotaApiSettings() *QuotaApiSettings {
	return &QuotaApiSettings{
		MaxResponseByteCount: 16 * 1024 * 1024,
		MaxRefusalByteCount:  256,
	}
}

// What a call answers when api refused it or its answer could not be read: the status and the call's route,
// never the bearer or a network id (I-13).
var ErrQuotaApiRefused = errors.New("quotahost: api refused an internal quota call")

// api's internal quota surface over HTTP at apiUrl (its scheme and host, such as https://api.<root>), each
// call carrying bearer, the host's own. client nil takes a client of its own; every call is bounded by the
// ctx the Quota passes (QuotaSettings' timeouts). Safe for concurrent use.
func NewHttpQuotaApi(apiUrl string, bearer string, client *http.Client, settings *QuotaApiSettings) (QuotaApiTransport, error) {
	parsed, err := url.Parse(apiUrl)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("quotahost: api's url is not one")
	}
	if bearer == "" {
		return nil, errors.New("quotahost: no bearer for api's internal surface")
	}
	if client == nil {
		client = &http.Client{}
	}
	if settings == nil {
		settings = DefaultQuotaApiSettings()
	}
	return &httpQuotaApi{
		baseUrl:  strings.TrimSuffix(apiUrl, "/"),
		bearer:   bearer,
		client:   client,
		settings: settings,
	}, nil
}

type httpQuotaApi struct {
	baseUrl  string
	bearer   string
	client   *http.Client
	settings *QuotaApiSettings
}

// Sends one call and decodes its JSON answer into result, which nil skips. route names the call in an error.
func (self *httpQuotaApi) call(ctx context.Context, method string, path string, route string, body any, result any) error {
	var bodyReader io.Reader
	if body != nil {
		bodyBytes, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(bodyBytes)
	}
	request, err := http.NewRequestWithContext(ctx, method, self.baseUrl+path, bodyReader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+self.bearer)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := self.client.Do(request)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			// the url names the network
			err = urlErr.Err
		}
		return fmt.Errorf("%w: %s: %w", ErrQuotaApiRefused, route, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		quoted, _ := io.ReadAll(io.LimitReader(response.Body, self.settings.MaxRefusalByteCount))
		return fmt.Errorf("%w: %s answered %d: %q", ErrQuotaApiRefused, route, response.StatusCode, strings.TrimSpace(string(quoted)))
	}
	if result == nil {
		// what little the answer holds, read so the connection is kept
		io.Copy(io.Discard, io.LimitReader(response.Body, self.settings.MaxRefusalByteCount))
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, self.settings.MaxResponseByteCount+1))
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrQuotaApiRefused, route, err)
	}
	if self.settings.MaxResponseByteCount < int64(len(data)) {
		return fmt.Errorf("%w: %s answered past %d bytes", ErrQuotaApiRefused, route, self.settings.MaxResponseByteCount)
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("%w: %s answered what does not parse: %w", ErrQuotaApiRefused, route, err)
	}
	return nil
}

func (self *httpQuotaApi) LoadLimits(ctx context.Context, networkId server.Id) (*sidecar.QuotaLimits, error) {
	limits := &sidecar.QuotaLimits{}
	if err := self.call(ctx, http.MethodGet, QuotaApiLimitsPath+networkId.String(), "limits", nil, limits); err != nil {
		return nil, err
	}
	if limits.Tier == "" {
		return nil, fmt.Errorf("%w: limits answered no tier", ErrQuotaApiRefused)
	}
	if limits.Limits == nil {
		limits.Limits = map[string]int64{}
	}
	return limits, nil
}

func (self *httpQuotaApi) LoadOverLimit(ctx context.Context) (*sidecar.QuotaOverLimit, error) {
	wire := &QuotaApiOverLimit{}
	if err := self.call(ctx, http.MethodGet, QuotaApiOverLimitPath, "over-limit", nil, wire); err != nil {
		return nil, err
	}
	overLimit, err := wire.OverLimit()
	if err != nil {
		return nil, fmt.Errorf("%w: over-limit: %w", ErrQuotaApiRefused, err)
	}
	return overLimit, nil
}

func (self *httpQuotaApi) DrawOrder(ctx context.Context, networkId server.Id) ([]*LeaseBalance, error) {
	wire := &QuotaApiDrawOrder{}
	if err := self.call(ctx, http.MethodGet, QuotaApiDrawOrderPath+networkId.String(), "draw-order", nil, wire); err != nil {
		return nil, err
	}
	balances := []*LeaseBalance{}
	for _, balance := range wire.Balances {
		if balance == nil || balance.BalanceId == (server.Id{}) {
			return nil, fmt.Errorf("%w: draw-order answered a balance with no id", ErrQuotaApiRefused)
		}
		balances = append(balances, &LeaseBalance{
			BalanceId:       balance.BalanceId,
			CreditByteCount: max(balance.CreditByteCount, 0),
			IsCredit:        balance.Credit,
			EndTime:         balance.EndTime,
		})
	}
	return balances, nil
}

func (self *httpQuotaApi) MarkSpent(ctx context.Context, networkId server.Id) error {
	return self.call(ctx, http.MethodPost, QuotaApiSpentPath+networkId.String(), "spent", nil, nil)
}

func (self *httpQuotaApi) ApplyUsageBatch(ctx context.Context, batch *sidecar.UsageBatch, carves []*model.UsageCarve) error {
	return self.call(ctx, http.MethodPost, QuotaApiUsageBatchPath, "usage-batch", NewQuotaApiUsageBatch(batch, carves), &QuotaApiUsageBatchResult{})
}

// The Quota a block's main hands its host when the block holds no Postgres: NewApiQuota over NewHttpQuotaApi at
// apiUrl, on caller's bearer from the block's vault (server.LoadInternalGateCredential), with this process's own
// Redis for CountRate and the reservations. An error says what is missing, never the bearer, and the block
// then passes no Quota and stays not ready, failing closed. It runs until ctx ends or Drain.
func NewBlockQuota(ctx context.Context, caller server.InternalGateCaller, apiUrl string, settings *QuotaSettings) (*HostQuota, error) {
	if apiUrl == "" {
		return nil, errors.New("quotahost: no api url for the internal quota surface")
	}
	bearer, err := server.LoadInternalGateCredential(caller)
	if err != nil {
		return nil, err
	}
	transport, err := NewHttpQuotaApi(apiUrl, bearer, nil, nil)
	if err != nil {
		return nil, err
	}
	if settings == nil {
		settings = DefaultQuotaSettings()
	}
	rateSettings := settings.Rate
	if rateSettings == nil {
		rateSettings = model.DefaultQuotaRateSettings()
	}
	return NewApiQuota(ctx, transport, model.NewQuotaRateServerRedis(rateSettings.CallTimeout), nil, settings)
}
