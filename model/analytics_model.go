package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

type WebSearchAnalyticsRow struct {
	Provider    string
	Site        string
	PeriodStart time.Time
	PeriodEnd   time.Time
	SearchType  string
	Query       string
	Path        string
	Region      string
	Device      string
	Clicks      float64
	Impressions float64
	Position    float64
}

// WebSearchAnalyticsConfig is the shared storage/privacy policy used by both
// the provider ingestion layer and the model layer.
type WebSearchAnalyticsConfig struct {
	Enabled                  bool   `yaml:"enabled"`
	Retain                   string `yaml:"retain"`
	MinimumImpressions       int    `yaml:"minimum_impressions"`
	MaximumRowsPerSitePeriod int    `yaml:"maximum_rows_per_site_period"`
	MaximumQueryRunes        int    `yaml:"maximum_query_runes"`
	RedactEmailAddresses     bool   `yaml:"redact_email_addresses"`
	RedactPhoneNumbers       bool   `yaml:"redact_phone_numbers"`
	RedactIPAddresses        bool   `yaml:"redact_ip_addresses"`
	EmitPrivacyFilteredLogs  bool   `yaml:"emit_privacy_filtered_query_logs"`
	AcceptTermsFromReferrers bool   `yaml:"accept_query_terms_from_referrer_urls"`
}

type AnalyticsConfig struct {
	Enabled    bool                      `yaml:"enabled"`
	Taskworker AnalyticsTaskworkerConfig `yaml:"taskworker"`
	Sites      []AnalyticsSiteConfig     `yaml:"sites"`
	WebViews   AnalyticsWebViewsConfig   `yaml:"web_views"`
	Search     WebSearchAnalyticsConfig  `yaml:"search"`
	Providers  AnalyticsProvidersConfig  `yaml:"providers"`
}

type AnalyticsTaskworkerConfig struct {
	Interval        string `yaml:"interval"`
	InitialLookback string `yaml:"initial_lookback"`
	Overlap         string `yaml:"overlap"`
	FinalizedDelay  string `yaml:"finalized_delay"`
	RetryBackoff    string `yaml:"retry_backoff"`
}

type AnalyticsSiteConfig struct {
	Name       string                  `yaml:"name"`
	Origin     string                  `yaml:"origin"`
	Properties AnalyticsSiteProperties `yaml:"properties"`
}

type AnalyticsSiteProperties struct {
	GoogleSearchConsole string `yaml:"google_search_console"`
	BingWebmaster       string `yaml:"bing_webmaster"`
	YandexHostID        string `yaml:"yandex_host_id"`
	BaiduSite           string `yaml:"baidu_site"`
}

type AnalyticsWebViewsConfig struct {
	Enabled                bool `yaml:"enabled"`
	NormalizeTrailingSlash bool `yaml:"normalize_trailing_slash"`
}

type AnalyticsProviderConfig struct {
	Enabled      bool     `yaml:"enabled"`
	Mode         string   `yaml:"mode"`
	Interval     string   `yaml:"interval"`
	Protocol     string   `yaml:"protocol"`
	Endpoint     string   `yaml:"endpoint"`
	ImportPrefix string   `yaml:"import_prefix"`
	SearchTypes  []string `yaml:"search_types"`
}

type AnalyticsProvidersConfig struct {
	Google AnalyticsProviderConfig `yaml:"google_search_console"`
	Bing   AnalyticsProviderConfig `yaml:"bing_webmaster"`
	Yandex AnalyticsProviderConfig `yaml:"yandex_webmaster"`
	Baidu  AnalyticsProviderConfig `yaml:"baidu_search_resource_platform"`
}

func NewDefaultAnalyticsConfig() *AnalyticsConfig {
	config := &AnalyticsConfig{}
	config.applyDefaults()
	return config
}

func LoadAnalyticsConfig() (*AnalyticsConfig, error) {
	resource, err := server.Config.SimpleResource("analytics.yml")
	if err != nil {
		return &AnalyticsConfig{}, nil
	}
	var config AnalyticsConfig
	if err := resource.UnmarshalYamlE(&config); err != nil {
		return nil, err
	}
	config.applyDefaults()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &config, nil
}

func (config *AnalyticsConfig) applyDefaults() {
	if config.Taskworker.Interval == "" {
		config.Taskworker.Interval = "1h"
	}
	if config.Taskworker.InitialLookback == "" {
		config.Taskworker.InitialLookback = "90d"
	}
	if config.Taskworker.Overlap == "" {
		config.Taskworker.Overlap = "72h"
	}
	if config.Taskworker.FinalizedDelay == "" {
		config.Taskworker.FinalizedDelay = "72h"
	}
	if config.Taskworker.RetryBackoff == "" {
		config.Taskworker.RetryBackoff = "15m"
	}
	if config.Search.Retain == "" {
		config.Search.Retain = "400d"
	}
	if config.Search.MinimumImpressions <= 0 {
		config.Search.MinimumImpressions = WebSearchAnalyticsAbsoluteMinimumImpressions
	}
	if config.Search.MaximumRowsPerSitePeriod <= 0 {
		config.Search.MaximumRowsPerSitePeriod = 5000
	}
	if config.Search.MaximumQueryRunes <= 0 {
		config.Search.MaximumQueryRunes = 160
	}
	if config.Providers.Google.Interval == "" {
		config.Providers.Google.Interval = config.Taskworker.Interval
	}
	if config.Providers.Bing.Interval == "" {
		config.Providers.Bing.Interval = "24h"
	}
	if config.Providers.Yandex.Interval == "" {
		config.Providers.Yandex.Interval = "24h"
	}
	if config.Providers.Baidu.Interval == "" {
		config.Providers.Baidu.Interval = "24h"
	}
	if config.Providers.Bing.Endpoint == "" {
		config.Providers.Bing.Endpoint = "https://ssl.bing.com/webmaster/api.svc/json"
	}
	if config.Providers.Baidu.ImportPrefix == "" {
		config.Providers.Baidu.ImportPrefix = "analytics/search/baidu/"
	}
	if len(config.Providers.Google.SearchTypes) == 0 {
		config.Providers.Google.SearchTypes = []string{"web"}
	}
}

func (config *AnalyticsConfig) Validate() error {
	if config.Search.MinimumImpressions < WebSearchAnalyticsAbsoluteMinimumImpressions {
		return fmt.Errorf(
			"analytics search.minimum_impressions must be at least %d",
			WebSearchAnalyticsAbsoluteMinimumImpressions,
		)
	}
	if config.Search.MaximumRowsPerSitePeriod < 1 || 10000 < config.Search.MaximumRowsPerSitePeriod {
		return fmt.Errorf("analytics search.maximum_rows_per_site_period must be between 1 and 10000")
	}
	durations := map[string]string{
		"taskworker.interval":         config.Taskworker.Interval,
		"taskworker.initial_lookback": config.Taskworker.InitialLookback,
		"taskworker.overlap":          config.Taskworker.Overlap,
		"taskworker.finalized_delay":  config.Taskworker.FinalizedDelay,
		"taskworker.retry_backoff":    config.Taskworker.RetryBackoff,
		"search.retain":               config.Search.Retain,
		"providers.google.interval":   config.Providers.Google.Interval,
		"providers.bing.interval":     config.Providers.Bing.Interval,
		"providers.yandex.interval":   config.Providers.Yandex.Interval,
		"providers.baidu.interval":    config.Providers.Baidu.Interval,
	}
	for name, value := range durations {
		duration, err := server.ParseDurationExtended(value)
		if err != nil || duration <= 0 {
			return fmt.Errorf("analytics %s must be a positive duration: %q", name, value)
		}
	}
	seenSites := map[string]bool{}
	for _, site := range config.Sites {
		if site.Name == "" || site.Origin == "" {
			return fmt.Errorf("analytics sites require name and origin")
		}
		if seenSites[site.Name] {
			return fmt.Errorf("analytics site %q is repeated", site.Name)
		}
		seenSites[site.Name] = true
	}
	allowedSearchTypes := map[string]bool{
		"web": true, "image": true, "video": true, "news": true,
		"discover": true, "googleNews": true,
	}
	for _, searchType := range config.Providers.Google.SearchTypes {
		if !allowedSearchTypes[searchType] {
			return fmt.Errorf("analytics google search type %q is unsupported", searchType)
		}
	}
	if config.Search.AcceptTermsFromReferrers {
		return fmt.Errorf("analytics must not accept search terms from referrer URLs")
	}
	return nil
}

func analyticsDuration(value string) time.Duration {
	duration, err := server.ParseDurationExtended(value)
	if err != nil {
		panic(err)
	}
	return duration
}

func (config *AnalyticsConfig) ScheduleInterval() time.Duration {
	return analyticsDuration(config.Taskworker.Interval)
}

func (config *AnalyticsConfig) InitialLookback() time.Duration {
	return analyticsDuration(config.Taskworker.InitialLookback)
}

func (config *AnalyticsConfig) Overlap() time.Duration {
	return analyticsDuration(config.Taskworker.Overlap)
}

func (config *AnalyticsConfig) FinalizedDelay() time.Duration {
	return analyticsDuration(config.Taskworker.FinalizedDelay)
}

func (config *AnalyticsConfig) Retention() time.Duration {
	return analyticsDuration(config.Search.Retain)
}

func (config AnalyticsProviderConfig) IntervalDuration() time.Duration {
	return analyticsDuration(config.Interval)
}

type WebSearchAnalyticsPersistResult struct {
	Row                WebSearchAnalyticsRow
	Changed            bool
	ClicksIncrease     float64
	ImpressionIncrease float64
}

type WebSearchAnalyticsResult struct {
	ProvidersAttempted int `json:"providers_attempted"`
	ProvidersSkipped   int `json:"providers_skipped"`
	RowsAccepted       int `json:"rows_accepted"`
	RowsRejected       int `json:"rows_rejected"`
	RowsUnchanged      int `json:"rows_unchanged"`
	RowsRemoved        int `json:"rows_removed"`
}

const WebSearchAnalyticsAbsoluteMinimumImpressions = 10

func ValidWebSearchAnalyticsRow(row WebSearchAnalyticsRow) bool {
	return row.Provider != "" && row.Site != "" && !row.PeriodStart.IsZero() &&
		row.PeriodEnd.After(row.PeriodStart) && row.SearchType != "" && row.Query != "" &&
		0 <= row.Clicks && WebSearchAnalyticsAbsoluteMinimumImpressions <= row.Impressions && 0 <= row.Position &&
		!math.IsNaN(row.Clicks) && !math.IsInf(row.Clicks, 0) &&
		!math.IsNaN(row.Impressions) && !math.IsInf(row.Impressions, 0) &&
		!math.IsNaN(row.Position) && !math.IsInf(row.Position, 0)
}

const webSearchAnalyticsTimeLayout = "2006-01-02T15:04:05Z"

func truncateWebSearchAnalyticsRunes(value string, maximum int) string {
	runes := []rune(value)
	if maximum <= 0 || len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum])
}

func rowKey(row WebSearchAnalyticsRow) string {
	canonical := strings.Join([]string{
		row.Provider,
		row.Site,
		row.PeriodStart.UTC().Format(webSearchAnalyticsTimeLayout),
		row.PeriodEnd.UTC().Format(webSearchAnalyticsTimeLayout),
		row.SearchType,
		row.Query,
		row.Path,
		row.Region,
		row.Device,
	}, "\x00")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// EnforceWebSearchAnalyticsVolumeCap is the second cardinality bound after the impression
// floor. It keeps the highest-volume query rows for each provider/site/period
// and prevents a search engine export from turning its long tail into an
// unbounded database workload.
func EnforceWebSearchAnalyticsVolumeCap(
	config WebSearchAnalyticsConfig,
	rows []WebSearchAnalyticsRow,
) ([]WebSearchAnalyticsRow, int) {
	if config.MinimumImpressions < WebSearchAnalyticsAbsoluteMinimumImpressions {
		config.MinimumImpressions = WebSearchAnalyticsAbsoluteMinimumImpressions
	}
	if config.MaximumRowsPerSitePeriod < 1 {
		return nil, len(rows)
	}
	groups := map[string][]WebSearchAnalyticsRow{}
	order := []string{}
	dropped := 0
	for _, row := range rows {
		if !ValidWebSearchAnalyticsRow(row) || row.Impressions < float64(config.MinimumImpressions) {
			dropped++
			continue
		}
		key := strings.Join([]string{
			row.Provider,
			row.Site,
			row.PeriodStart.UTC().Format(webSearchAnalyticsTimeLayout),
			row.PeriodEnd.UTC().Format(webSearchAnalyticsTimeLayout),
			row.SearchType,
		}, "\x00")
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], row)
	}
	kept := make([]WebSearchAnalyticsRow, 0, len(rows))
	for _, key := range order {
		group := groups[key]
		sort.SliceStable(group, func(i int, j int) bool {
			if group[i].Impressions == group[j].Impressions {
				return group[j].Clicks < group[i].Clicks
			}
			return group[j].Impressions < group[i].Impressions
		})
		limit := config.MaximumRowsPerSitePeriod
		if limit < len(group) {
			dropped += len(group) - limit
			group = group[:limit]
		}
		kept = append(kept, group...)
	}
	return kept, dropped
}

func PersistWebSearchAnalyticsRows(
	ctx context.Context,
	config WebSearchAnalyticsConfig,
	rows []WebSearchAnalyticsRow,
) []WebSearchAnalyticsPersistResult {
	if config.MinimumImpressions < WebSearchAnalyticsAbsoluteMinimumImpressions {
		config.MinimumImpressions = WebSearchAnalyticsAbsoluteMinimumImpressions
	}
	results := make([]WebSearchAnalyticsPersistResult, 0, len(rows))
	server.Tx(ctx, func(tx server.PgTx) {
		for _, row := range rows {
			if !ValidWebSearchAnalyticsRow(row) || row.Impressions < float64(config.MinimumImpressions) {
				continue
			}
			key := rowKey(row)
			var oldClicks float64
			var oldImpressions float64
			var oldPosition float64
			err := tx.QueryRow(
				ctx,
				`SELECT clicks, impressions, average_position
				 FROM web_search_analytics
				 WHERE row_key = $1
				 FOR UPDATE`,
				key,
			).Scan(&oldClicks, &oldImpressions, &oldPosition)
			isNew := errors.Is(err, pgx.ErrNoRows)
			if err != nil && !isNew {
				panic(err)
			}
			changed := isNew || oldClicks != row.Clicks || oldImpressions != row.Impressions || oldPosition != row.Position
			if changed {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO web_search_analytics (
						row_key, provider, site, period_start, period_end,
						search_type, query, path, region, device,
						clicks, impressions, average_position, update_time
					 ) VALUES (
						$1, $2, $3, $4, $5,
						$6, $7, $8, $9, $10,
						$11, $12, $13, $14
					 )
					 ON CONFLICT (row_key) DO UPDATE SET
						clicks = EXCLUDED.clicks,
						impressions = EXCLUDED.impressions,
						average_position = EXCLUDED.average_position,
						update_time = EXCLUDED.update_time`,
					key,
					row.Provider,
					row.Site,
					row.PeriodStart.UTC(),
					row.PeriodEnd.UTC(),
					row.SearchType,
					row.Query,
					row.Path,
					row.Region,
					row.Device,
					row.Clicks,
					row.Impressions,
					row.Position,
					server.NowUtc(),
				))
			}
			results = append(results, WebSearchAnalyticsPersistResult{
				Row:                row,
				Changed:            changed,
				ClicksIncrease:     math.Max(0, row.Clicks-oldClicks),
				ImpressionIncrease: math.Max(0, row.Impressions-oldImpressions),
			})
		}
	})
	return results
}

type webSearchAnalyticsRowGroup struct {
	Provider    string
	Site        string
	PeriodStart time.Time
	PeriodEnd   time.Time
	SearchType  string
}

// TrimExcessWebSearchAnalyticsRows applies the configured top-N cap to rows
// already in the database. EnforceWebSearchAnalyticsVolumeCap bounds the current fetch; this companion cleanup
// also removes older rows that fell out of that top-N set on a later fetch.
func TrimExcessWebSearchAnalyticsRows(
	ctx context.Context,
	config WebSearchAnalyticsConfig,
	rows []WebSearchAnalyticsRow,
) int64 {
	if config.MaximumRowsPerSitePeriod <= 0 || len(rows) == 0 {
		return 0
	}
	groups := map[string]webSearchAnalyticsRowGroup{}
	for _, row := range rows {
		key := strings.Join([]string{
			row.Provider,
			row.Site,
			row.PeriodStart.UTC().Format(webSearchAnalyticsTimeLayout),
			row.PeriodEnd.UTC().Format(webSearchAnalyticsTimeLayout),
			row.SearchType,
		}, "\x00")
		groups[key] = webSearchAnalyticsRowGroup{
			Provider:    row.Provider,
			Site:        row.Site,
			PeriodStart: row.PeriodStart.UTC(),
			PeriodEnd:   row.PeriodEnd.UTC(),
			SearchType:  row.SearchType,
		}
	}
	var removed int64
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		for _, group := range groups {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`WITH excess AS (
					SELECT row_key
					FROM web_search_analytics
					WHERE provider = $1
						AND site = $2
						AND period_start = $3
						AND period_end = $4
						AND search_type = $5
					ORDER BY impressions DESC, clicks DESC, row_key
					OFFSET $6
				 )
				 DELETE FROM web_search_analytics analytics
				 USING excess
				 WHERE analytics.row_key = excess.row_key`,
				group.Provider,
				group.Site,
				group.PeriodStart,
				group.PeriodEnd,
				group.SearchType,
				config.MaximumRowsPerSitePeriod,
			))
			removed += tag.RowsAffected()
		}
	})
	return removed
}

type WebSearchAnalyticsIngestState struct {
	LastAttempt time.Time
	LastSuccess time.Time
	CursorTime  time.Time
}

func GetWebSearchAnalyticsIngestState(ctx context.Context, provider string, site string, stream string) WebSearchAnalyticsIngestState {
	state := WebSearchAnalyticsIngestState{}
	server.Db(ctx, func(conn server.PgConn) {
		err := conn.QueryRow(
			ctx,
			`SELECT last_attempt, last_success, cursor_time
			 FROM web_search_ingest_state
			 WHERE provider = $1 AND site = $2 AND stream = $3`,
			provider,
			site,
			stream,
		).Scan(&state.LastAttempt, &state.LastSuccess, &state.CursorTime)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			panic(err)
		}
	})
	return state
}

func MarkWebSearchAnalyticsIngestAttempt(ctx context.Context, provider string, site string, stream string, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO web_search_ingest_state (
				provider, site, stream, last_attempt, update_time
			 ) VALUES ($1, $2, $3, $4, $4)
			 ON CONFLICT (provider, site, stream) DO UPDATE SET
				last_attempt = EXCLUDED.last_attempt,
				update_time = EXCLUDED.update_time`,
			provider,
			site,
			stream,
			now.UTC(),
		))
	})
}

func MarkWebSearchAnalyticsIngestSuccess(ctx context.Context, provider string, site string, stream string, now time.Time, cursor time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO web_search_ingest_state (
				provider, site, stream, last_attempt, last_success, cursor_time, last_error, update_time
			 ) VALUES ($1, $2, $3, $4, $4, $5, '', $4)
			 ON CONFLICT (provider, site, stream) DO UPDATE SET
				last_attempt = EXCLUDED.last_attempt,
				last_success = EXCLUDED.last_success,
				cursor_time = EXCLUDED.cursor_time,
				last_error = '',
				update_time = EXCLUDED.update_time`,
			provider,
			site,
			stream,
			now.UTC(),
			cursor.UTC(),
		))
	})
}

func MarkWebSearchAnalyticsIngestError(ctx context.Context, provider string, site string, stream string, now time.Time, code string) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO web_search_ingest_state (
				provider, site, stream, last_attempt, last_error, update_time
			 ) VALUES ($1, $2, $3, $4, $5, $4)
			 ON CONFLICT (provider, site, stream) DO UPDATE SET
				last_attempt = EXCLUDED.last_attempt,
				last_error = EXCLUDED.last_error,
				update_time = EXCLUDED.update_time`,
			provider,
			site,
			stream,
			now.UTC(),
			truncateWebSearchAnalyticsRunes(code, 64),
		))
	})
}

func WebSearchAnalyticsManualObjectProcessed(ctx context.Context, provider string, objectKey string, contentSHA256 string) bool {
	processed := false
	server.Db(ctx, func(conn server.PgConn) {
		var value int
		err := conn.QueryRow(
			ctx,
			`SELECT 1 FROM web_search_manual_import
			 WHERE provider = $1 AND object_key = $2 AND content_sha256 = $3`,
			provider,
			objectKey,
			contentSHA256,
		).Scan(&value)
		if err == nil {
			processed = true
		} else if !errors.Is(err, pgx.ErrNoRows) {
			panic(err)
		}
	})
	return processed
}

func MarkWebSearchAnalyticsManualObjectProcessed(ctx context.Context, provider string, objectKey string, contentSHA256 string, rowsAccepted int, rowsRejected int) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO web_search_manual_import (
				provider, object_key, content_sha256, rows_accepted, rows_rejected, process_time
			 ) VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (provider, object_key, content_sha256) DO NOTHING`,
			provider,
			objectKey,
			contentSHA256,
			rowsAccepted,
			rowsRejected,
			server.NowUtc(),
		))
	})
}

func RemoveExpiredWebSearchAnalyticsRows(ctx context.Context, cutoff time.Time, limit int) int64 {
	if limit <= 0 {
		limit = 10000
	}
	var removed int64
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`WITH expired AS (
				SELECT row_key
				FROM web_search_analytics
				WHERE period_end < $1
				ORDER BY period_end
				LIMIT $2
			 )
			 DELETE FROM web_search_analytics analytics
			 USING expired
			 WHERE analytics.row_key = expired.row_key`,
			cutoff.UTC(),
			limit,
		))
		removed = tag.RowsAffected()
	})
	return removed
}

// RemoveWebSearchAnalyticsRowsBelowVolume re-applies the current privacy/cardinality floor to
// historical data. This matters when operators raise minimum_impressions: the
// old lower-volume rows are removed on the next task runs instead of lingering
// until the retention cutoff.
func RemoveWebSearchAnalyticsRowsBelowVolume(ctx context.Context, config WebSearchAnalyticsConfig, limit int) int64 {
	if config.MinimumImpressions < WebSearchAnalyticsAbsoluteMinimumImpressions {
		config.MinimumImpressions = WebSearchAnalyticsAbsoluteMinimumImpressions
	}
	if limit <= 0 {
		limit = 10000
	}
	var removed int64
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`WITH below_volume AS (
				SELECT row_key
				FROM web_search_analytics
				WHERE impressions < $1
				ORDER BY impressions, period_end
				LIMIT $2
			 )
			 DELETE FROM web_search_analytics analytics
			 USING below_volume
			 WHERE analytics.row_key = below_volume.row_key`,
			config.MinimumImpressions,
			limit,
		))
		removed = tag.RowsAffected()
	})
	return removed
}
