package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/oauth2/jwt"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Credentials are controller-private because only the external provider
// adapters consume them. The shared non-secret policy and persistence objects
// live in model/analytics_model.go.
type analyticsCredentials struct {
	Google struct {
		ServiceAccountJSON string `yaml:"service_account_json"`
	} `yaml:"google_search_console"`
	Bing struct {
		APIKey string `yaml:"api_key"`
	} `yaml:"bing_webmaster"`
	Yandex struct {
		ClientID     string `yaml:"client_id"`
		ClientSecret string `yaml:"client_secret"`
		OAuthToken   string `yaml:"oauth_token"`
		UserID       string `yaml:"user_id"`
	} `yaml:"yandex_webmaster"`
}

func loadAnalyticsCredentials() (*analyticsCredentials, error) {
	resource, err := server.Vault.SimpleResource("analytics.yml")
	if err != nil {
		return &analyticsCredentials{}, nil
	}
	var credentials analyticsCredentials
	if err := resource.UnmarshalYamlE(&credentials); err != nil {
		return nil, err
	}
	return &credentials, nil
}

func cleanSecret(value string) string {
	return strings.TrimSpace(value)
}

var emailPattern = regexp.MustCompile(`(?i)\b[a-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+\b`)
var phonePattern = regexp.MustCompile(`(?:\+?[0-9][0-9 .()\-]{7,}[0-9])`)
var ipTokenPattern = regexp.MustCompile(`(?i)(?:\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b|\b[0-9a-f]{0,4}:[0-9a-f:]{2,}\b)`)

func normalizeSpaces(value string) string {
	return strings.Join(strings.FieldsFunc(value, unicode.IsSpace), " ")
}

func truncateRunes(value string, maximum int) string {
	if maximum <= 0 || utf8.RuneCountInString(value) <= maximum {
		return value
	}
	runes := []rune(value)
	return string(runes[:maximum])
}

func privacySafeQuery(config *model.AnalyticsConfig, value string, impressions float64) (string, bool) {
	if math.IsNaN(impressions) || math.IsInf(impressions, 0) || impressions < float64(config.Search.MinimumImpressions) {
		return "", false
	}
	if !utf8.ValidString(value) {
		return "", false
	}
	value = normalizeSpaces(strings.TrimSpace(value))
	if value == "" {
		return "", false
	}
	if config.Search.RedactEmailAddresses {
		value = emailPattern.ReplaceAllString(value, "[redacted-email]")
	}
	if config.Search.RedactIPAddresses {
		value = ipTokenPattern.ReplaceAllStringFunc(value, func(token string) string {
			trimmed := strings.Trim(token, "[](){}.,;")
			if net.ParseIP(trimmed) != nil {
				return strings.Replace(token, trimmed, "[redacted-ip]", 1)
			}
			return token
		})
	}
	if config.Search.RedactPhoneNumbers {
		value = phonePattern.ReplaceAllString(value, "[redacted-phone]")
	}
	value = truncateRunes(value, config.Search.MaximumQueryRunes)
	return value, value != ""
}

func normalizePath(config *model.AnalyticsConfig, site model.AnalyticsSiteConfig, value string) (string, bool) {
	if value == "" {
		return "", true
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", false
	}
	if parsed.IsAbs() {
		origin, err := url.Parse(site.Origin)
		if err != nil || !strings.EqualFold(parsed.Hostname(), origin.Hostname()) {
			return "", false
		}
	}
	path := parsed.Path
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		return "", false
	}
	if config.WebViews.NormalizeTrailingSlash && path != "/" {
		path = strings.TrimRight(path, "/")
	}
	path = truncateRunes(path, 1024)
	return path, true
}

const maximumProviderResponseBytes = 64 << 20

type providerError struct {
	Provider string
	Code     string
	Auth     bool
}

func (err *providerError) Error() string {
	return fmt.Sprintf("%s provider error: %s", err.Provider, err.Code)
}

func errorCode(err error) string {
	var providerErr *providerError
	if errors.As(err, &providerErr) {
		return providerErr.Code
	}
	return "request_failed"
}

func isAuthError(err error) bool {
	var providerErr *providerError
	return errors.As(err, &providerErr) && providerErr.Auth
}

// decodeResponse never includes request URLs, response bodies, or credential
// material in errors. Provider-specific callers classify non-success bodies
// when the status code alone is ambiguous.
func decodeResponse(provider string, response *http.Response, output any) error {
	defer response.Body.Close()
	if response.StatusCode < 200 || 300 <= response.StatusCode {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return &providerError{
			Provider: provider,
			Code:     fmt.Sprintf("http_%d", response.StatusCode),
			Auth:     response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden,
		}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maximumProviderResponseBytes+1))
	if err := decoder.Decode(output); err != nil {
		return &providerError{Provider: provider, Code: "invalid_json"}
	}
	return nil
}

func doRequest(provider string, client *http.Client, request *http.Request, output any) error {
	response, err := client.Do(request)
	if err != nil {
		// net/http errors commonly render the full URL. Never wrap that error:
		// Bing authenticates in its query string.
		return &providerError{Provider: provider, Code: "request_failed"}
	}
	return decodeResponse(provider, response, output)
}

const googleWebmastersReadonlyScope = "https://www.googleapis.com/auth/webmasters.readonly"
const googleSearchAnalyticsEndpoint = "https://www.googleapis.com/webmasters/v3/sites/"
const googleRowLimit = 25000

type googleQueryRequest struct {
	StartDate       string   `json:"startDate"`
	EndDate         string   `json:"endDate"`
	Dimensions      []string `json:"dimensions"`
	SearchType      string   `json:"type"`
	AggregationType string   `json:"aggregationType"`
	DataState       string   `json:"dataState"`
	RowLimit        int      `json:"rowLimit"`
	StartRow        int      `json:"startRow"`
}

type googleQueryResponse struct {
	Rows []struct {
		Keys        []string `json:"keys"`
		Clicks      float64  `json:"clicks"`
		Impressions float64  `json:"impressions"`
		Position    float64  `json:"position"`
	} `json:"rows"`
}

func googleClient(ctx context.Context, serviceAccountJSON string) (*http.Client, error) {
	var serviceAccount struct {
		Type         string `json:"type"`
		ClientEmail  string `json:"client_email"`
		PrivateKey   string `json:"private_key"`
		PrivateKeyID string `json:"private_key_id"`
		TokenURI     string `json:"token_uri"`
	}
	if err := json.Unmarshal([]byte(serviceAccountJSON), &serviceAccount); err != nil ||
		serviceAccount.Type != "service_account" || serviceAccount.ClientEmail == "" ||
		serviceAccount.PrivateKey == "" {
		return nil, &providerError{Provider: "google", Code: "invalid_credentials", Auth: true}
	}
	if serviceAccount.TokenURI == "" {
		serviceAccount.TokenURI = "https://oauth2.googleapis.com/token"
	}
	tokenURI, err := url.Parse(serviceAccount.TokenURI)
	if err != nil || tokenURI.Scheme != "https" ||
		(tokenURI.Hostname() != "oauth2.googleapis.com" && tokenURI.Hostname() != "accounts.google.com") {
		return nil, &providerError{Provider: "google", Code: "invalid_credentials", Auth: true}
	}
	jwtConfig := &jwt.Config{
		Email:        serviceAccount.ClientEmail,
		PrivateKey:   []byte(serviceAccount.PrivateKey),
		PrivateKeyID: serviceAccount.PrivateKeyID,
		Scopes:       []string{googleWebmastersReadonlyScope},
		TokenURL:     serviceAccount.TokenURI,
	}
	client := jwtConfig.Client(ctx)
	client.Timeout = 45 * time.Second
	return client, nil
}

func fetchGoogleDay(
	ctx context.Context,
	client *http.Client,
	config *model.AnalyticsConfig,
	site model.AnalyticsSiteConfig,
	searchType string,
	day time.Time,
) ([]model.WebSearchAnalyticsRow, int, error) {
	day = day.UTC()
	property := site.Properties.GoogleSearchConsole
	endpoint := googleSearchAnalyticsEndpoint + url.PathEscape(property) + "/searchAnalytics/query"
	rows := []model.WebSearchAnalyticsRow{}
	rejected := 0
	for startRow := 0; ; startRow += googleRowLimit {
		payload := googleQueryRequest{
			StartDate: day.Format("2006-01-02"),
			EndDate:   day.Format("2006-01-02"),
			// Country/device fan-out multiplies query cardinality without
			// helping the requested search-term report. Page-view geography is
			// collected independently at the edge.
			Dimensions:      []string{"date", "query", "page"},
			SearchType:      searchType,
			AggregationType: "auto",
			DataState:       "final",
			RowLimit:        googleRowLimit,
			StartRow:        startRow,
		}
		body, err := json.Marshal(&payload)
		if err != nil {
			return nil, rejected, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, rejected, &providerError{Provider: "google", Code: "request_build_failed"}
		}
		request.Header.Set("Content-Type", "application/json")
		var response googleQueryResponse
		if err := doRequest("google", client, request, &response); err != nil {
			return nil, rejected, err
		}
		if len(response.Rows) == 0 {
			break
		}
		for _, source := range response.Rows {
			if len(source.Keys) != 3 {
				rejected++
				continue
			}
			periodStart, err := time.Parse("2006-01-02", source.Keys[0])
			if err != nil {
				rejected++
				continue
			}
			query, ok := privacySafeQuery(config, source.Keys[1], source.Impressions)
			if !ok {
				rejected++
				continue
			}
			path, ok := normalizePath(config, site, source.Keys[2])
			if !ok {
				rejected++
				continue
			}
			row := model.WebSearchAnalyticsRow{
				Provider:    "google",
				Site:        site.Name,
				PeriodStart: periodStart.UTC(),
				PeriodEnd:   periodStart.UTC().Add(24 * time.Hour),
				SearchType:  searchType,
				Query:       query,
				Path:        path,
				Clicks:      source.Clicks,
				Impressions: source.Impressions,
				Position:    source.Position,
			}
			if !model.ValidWebSearchAnalyticsRow(row) {
				rejected++
				continue
			}
			rows = append(rows, row)
		}
		if len(response.Rows) < googleRowLimit {
			break
		}
		if startRow+googleRowLimit >= 50000 {
			// Search Console documents a maximum of 50K rows per day and
			// search type. Do not issue requests that cannot return more data.
			break
		}
	}
	return rows, rejected, nil
}

func googleWindow(config *model.AnalyticsConfig, state model.WebSearchAnalyticsIngestState, now time.Time) (time.Time, time.Time, error) {
	end := now.UTC().Add(-config.FinalizedDelay()).Truncate(24 * time.Hour)
	if end.After(now.UTC()) {
		return time.Time{}, time.Time{}, fmt.Errorf("google finalized window is in the future")
	}
	start := end.Add(-config.InitialLookback())
	if !state.CursorTime.IsZero() {
		start = state.CursorTime.UTC().Add(-config.Overlap())
	}
	start = start.Truncate(24 * time.Hour)
	if start.After(end) {
		start = end
	}
	return start, end, nil
}

var bingDatePattern = regexp.MustCompile(`^/Date\((-?[0-9]+)(?:[+-][0-9]+)?\)/$`)

type bingQueryResponse struct {
	Rows []struct {
		Clicks            float64 `json:"Clicks"`
		Impressions       float64 `json:"Impressions"`
		AverageClick      float64 `json:"AvgClickPosition"`
		AverageImpression float64 `json:"AvgImpressionPosition"`
		Date              string  `json:"Date"`
		Query             string  `json:"Query"`
	} `json:"d"`
}

type bingErrorResponse struct {
	ErrorCode int    `json:"ErrorCode"`
	Message   string `json:"Message"`
}

func parseBingDate(value string) (time.Time, bool) {
	match := bingDatePattern.FindStringSubmatch(value)
	if len(match) != 2 {
		return time.Time{}, false
	}
	millis, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	providerTime := time.UnixMilli(millis).UTC()
	day, err := time.Parse("2006-01-02", providerTime.Format("2006-01-02"))
	return day.UTC(), err == nil
}

func fetchBing(
	ctx context.Context,
	client *http.Client,
	config *model.AnalyticsConfig,
	providerConfig model.AnalyticsProviderConfig,
	site model.AnalyticsSiteConfig,
	apiKey string,
) ([]model.WebSearchAnalyticsRow, int, error) {
	endpoint := strings.TrimRight(providerConfig.Endpoint, "/") + "/GetQueryStats"
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() != "ssl.bing.com" {
		return nil, 0, &providerError{Provider: "bing", Code: "invalid_endpoint"}
	}
	query := parsed.Query()
	query.Set("siteUrl", site.Properties.BingWebmaster)
	query.Set("apikey", apiKey)
	parsed.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, 0, &providerError{Provider: "bing", Code: "request_build_failed"}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, &providerError{Provider: "bing", Code: "request_failed"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || 300 <= response.StatusCode {
		var providerResponse bingErrorResponse
		_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&providerResponse)
		auth := response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || providerResponse.ErrorCode == 3
		code := "http_" + strconv.Itoa(response.StatusCode)
		if providerResponse.ErrorCode != 0 {
			code = "api_" + strconv.Itoa(providerResponse.ErrorCode)
		}
		return nil, 0, &providerError{Provider: "bing", Code: code, Auth: auth}
	}
	var decoded bingQueryResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maximumProviderResponseBytes)).Decode(&decoded); err != nil {
		return nil, 0, &providerError{Provider: "bing", Code: "invalid_json"}
	}
	rows := make([]model.WebSearchAnalyticsRow, 0, len(decoded.Rows))
	rejected := 0
	for _, source := range decoded.Rows {
		day, ok := parseBingDate(source.Date)
		if !ok {
			rejected++
			continue
		}
		query, ok := privacySafeQuery(config, source.Query, source.Impressions)
		if !ok {
			rejected++
			continue
		}
		position := source.AverageImpression
		if position == 0 {
			position = source.AverageClick
		}
		row := model.WebSearchAnalyticsRow{
			Provider:    "bing",
			Site:        site.Name,
			PeriodStart: day,
			PeriodEnd:   day.Add(24 * time.Hour),
			SearchType:  "web",
			Query:       query,
			Clicks:      source.Clicks,
			Impressions: source.Impressions,
			Position:    position,
		}
		if !model.ValidWebSearchAnalyticsRow(row) {
			rejected++
			continue
		}
		rows = append(rows, row)
	}
	return rows, rejected, nil
}

const yandexWebmasterEndpoint = "https://api.webmaster.yandex.net/v4"

type yandexQueryResponse struct {
	Queries []struct {
		QueryText  string             `json:"query_text"`
		Indicators map[string]float64 `json:"indicators"`
	} `json:"queries"`
	DateFrom string `json:"date_from"`
	DateTo   string `json:"date_to"`
	Count    int    `json:"count"`
}

func fetchYandex(
	ctx context.Context,
	client *http.Client,
	config *model.AnalyticsConfig,
	site model.AnalyticsSiteConfig,
	userID string,
	oauthToken string,
	now time.Time,
) ([]model.WebSearchAnalyticsRow, int, time.Time, error) {
	dateTo := now.UTC().Add(-config.FinalizedDelay()).Truncate(24 * time.Hour)
	dateFrom := dateTo.AddDate(0, 0, -7)
	base := yandexWebmasterEndpoint + "/user/" + url.PathEscape(userID) + "/hosts/" +
		url.PathEscape(site.Properties.YandexHostID) + "/search-queries/popular"
	rows := []model.WebSearchAnalyticsRow{}
	rejected := 0
	for offset := 0; ; offset += 500 {
		parsed, _ := url.Parse(base)
		query := parsed.Query()
		query.Set("order_by", "TOTAL_SHOWS")
		query.Add("query_indicator", "TOTAL_SHOWS")
		query.Add("query_indicator", "TOTAL_CLICKS")
		query.Add("query_indicator", "AVG_SHOW_POSITION")
		query.Set("device_type_indicator", "ALL")
		query.Set("date_from", dateFrom.Format("2006-01-02"))
		query.Set("date_to", dateTo.Format("2006-01-02"))
		query.Set("offset", strconv.Itoa(offset))
		query.Set("limit", "500")
		parsed.RawQuery = query.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
		if err != nil {
			return nil, rejected, dateTo, &providerError{Provider: "yandex", Code: "request_build_failed"}
		}
		request.Header.Set("Authorization", "OAuth "+oauthToken)
		var response yandexQueryResponse
		if err := doRequest("yandex", client, request, &response); err != nil {
			return nil, rejected, dateTo, err
		}
		periodStart, err := time.Parse("2006-01-02", response.DateFrom)
		if err != nil {
			periodStart = dateFrom
		}
		periodEndDay, err := time.Parse("2006-01-02", response.DateTo)
		if err != nil {
			periodEndDay = dateTo
		}
		periodEnd := periodEndDay.UTC().Add(24 * time.Hour)
		for _, source := range response.Queries {
			impressions := source.Indicators["TOTAL_SHOWS"]
			queryText, ok := privacySafeQuery(config, source.QueryText, impressions)
			if !ok {
				rejected++
				continue
			}
			row := model.WebSearchAnalyticsRow{
				Provider:    "yandex",
				Site:        site.Name,
				PeriodStart: periodStart.UTC(),
				PeriodEnd:   periodEnd,
				SearchType:  "web",
				Query:       queryText,
				Clicks:      source.Indicators["TOTAL_CLICKS"],
				Impressions: impressions,
				Position:    source.Indicators["AVG_SHOW_POSITION"],
			}
			if !model.ValidWebSearchAnalyticsRow(row) {
				rejected++
				continue
			}
			rows = append(rows, row)
		}
		if len(response.Queries) < 500 || response.Count <= offset+len(response.Queries) {
			break
		}
		if 3000 <= offset+500 {
			break
		}
	}
	return rows, rejected, dateTo, nil
}

const maximumManualImportBytes int64 = 50 << 20

type manualImportResult struct {
	ObjectsAccepted int
	ObjectsSkipped  int
	RowsAccepted    int
	RowsRejected    int
	RowsRemoved     int
}

func canonicalHeader(value string) string {
	value = strings.TrimPrefix(value, "\ufeff")
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer(" ", "_", "-", "_", ".", "_").Replace(value)
	switch value {
	case "property":
		return "site"
	case "day":
		return "date"
	case "keyword", "search_term":
		return "query"
	case "page", "landing_page", "url":
		return "path"
	case "country":
		return "region"
	case "type":
		return "search_type"
	case "shows":
		return "impressions"
	case "avg_position":
		return "position"
	default:
		return value
	}
}

func parseManualTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return parsed.UTC(), true
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC().Truncate(24 * time.Hour), true
	}
	return time.Time{}, false
}

func parseManualCSV(config *model.AnalyticsConfig, sites map[string]model.AnalyticsSiteConfig, content []byte) ([]model.WebSearchAnalyticsRow, int, error) {
	reader := csv.NewReader(bytes.NewReader(content))
	reader.FieldsPerRecord = -1
	headers, err := reader.Read()
	if err != nil {
		return nil, 0, fmt.Errorf("manual import header: %w", err)
	}
	columns := map[string]int{}
	for index, header := range headers {
		columns[canonicalHeader(header)] = index
	}
	for _, required := range []string{"site", "date", "query", "clicks", "impressions"} {
		if _, ok := columns[required]; !ok {
			return nil, 0, fmt.Errorf("manual import missing %s column", required)
		}
	}
	value := func(record []string, name string) string {
		index, ok := columns[name]
		if !ok || len(record) <= index {
			return ""
		}
		return strings.TrimSpace(record[index])
	}
	rows := []model.WebSearchAnalyticsRow{}
	rejected := 0
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			rejected++
			continue
		}
		site, ok := sites[value(record, "site")]
		if !ok {
			rejected++
			continue
		}
		day, ok := parseManualTime(value(record, "date"))
		if !ok {
			rejected++
			continue
		}
		clicks, clickErr := strconv.ParseFloat(value(record, "clicks"), 64)
		impressions, impressionErr := strconv.ParseFloat(value(record, "impressions"), 64)
		position := 0.0
		var positionErr error
		if rawPosition := value(record, "position"); rawPosition != "" {
			position, positionErr = strconv.ParseFloat(rawPosition, 64)
		}
		if clickErr != nil || impressionErr != nil || positionErr != nil {
			rejected++
			continue
		}
		query, ok := privacySafeQuery(config, value(record, "query"), impressions)
		if !ok {
			rejected++
			continue
		}
		landingPath, ok := normalizePath(config, site, value(record, "path"))
		if !ok {
			rejected++
			continue
		}
		searchType := value(record, "search_type")
		if searchType == "" {
			searchType = "web"
		}
		row := model.WebSearchAnalyticsRow{
			Provider:    "baidu",
			Site:        site.Name,
			PeriodStart: day,
			PeriodEnd:   day.Add(24 * time.Hour),
			SearchType:  truncateRunes(searchType, 32),
			Query:       query,
			Path:        landingPath,
			Region:      truncateRunes(value(record, "region"), 16),
			Device:      truncateRunes(value(record, "device"), 32),
			Clicks:      clicks,
			Impressions: impressions,
			Position:    position,
		}
		if !model.ValidWebSearchAnalyticsRow(row) {
			rejected++
			continue
		}
		rows = append(rows, row)
	}
	return rows, rejected, nil
}

func ingestBaiduObjects(ctx context.Context, config *model.AnalyticsConfig, provider model.AnalyticsProviderConfig) (manualImportResult, error) {
	result := manualImportResult{}
	store, ok := server.LoadBlobStore()
	if !ok {
		return result, &providerError{Provider: "baidu", Code: "blob_store_unavailable"}
	}
	env, err := server.Env()
	if err != nil {
		env = "local"
	}
	prefix := path.Join(store.Prefix(), env, strings.Trim(provider.ImportPrefix, "/")) + "/"
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return result, &providerError{Provider: "baidu", Code: "list_failed"}
	}
	sites := map[string]model.AnalyticsSiteConfig{}
	for _, site := range config.Sites {
		sites[site.Name] = site
	}
	for _, object := range objects {
		if !strings.HasSuffix(strings.ToLower(object.Key), ".csv") {
			continue
		}
		if object.Size < 0 || maximumManualImportBytes < object.Size {
			result.RowsRejected++
			continue
		}
		reader, err := store.Get(ctx, object.Key)
		if err != nil {
			return result, &providerError{Provider: "baidu", Code: "read_failed"}
		}
		content, readErr := io.ReadAll(io.LimitReader(reader, maximumManualImportBytes+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || maximumManualImportBytes < int64(len(content)) {
			return result, &providerError{Provider: "baidu", Code: "read_failed"}
		}
		sum := sha256.Sum256(content)
		contentHash := hex.EncodeToString(sum[:])
		if model.WebSearchAnalyticsManualObjectProcessed(ctx, "baidu", object.Key, contentHash) {
			result.ObjectsSkipped++
			continue
		}
		rows, rejected, err := parseManualCSV(config, sites, content)
		if err != nil {
			return result, &providerError{Provider: "baidu", Code: "invalid_csv"}
		}
		rows, volumeRejected := model.EnforceWebSearchAnalyticsVolumeCap(config.Search, rows)
		rejected += volumeRejected
		if 0 < rejected {
			ingestRows.WithLabelValues("*", "baidu", "rejected").Add(float64(rejected))
		}
		persisted := model.PersistWebSearchAnalyticsRows(ctx, config.Search, rows)
		result.RowsRemoved += int(model.TrimExcessWebSearchAnalyticsRows(ctx, config.Search, rows))
		accepted, _ := recordPersistResults(persisted)
		for _, persistedRow := range persisted {
			if persistedRow.Changed {
				emitSearchRow(config, persistedRow.Row)
			}
		}
		model.MarkWebSearchAnalyticsManualObjectProcessed(ctx, "baidu", object.Key, contentHash, accepted, rejected)
		result.ObjectsAccepted++
		result.RowsAccepted += accepted
		result.RowsRejected += rejected
	}
	return result, nil
}

var searchClicks = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "web_search",
		Name:      "clicks_total",
		Help:      "Positive deltas in privacy-filtered webmaster click aggregates",
	},
	[]string{"site", "engine", "path"},
)

var searchImpressions = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "web_search",
		Name:      "impressions_total",
		Help:      "Positive deltas in privacy-filtered webmaster impression aggregates",
	},
	[]string{"site", "engine", "path"},
)

var ingestRows = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "web_search",
		Name:      "ingest_rows_total",
		Help:      "Webmaster rows accepted, rejected, unchanged, or skipped",
	},
	[]string{"site", "engine", "result"},
)

var ingestLastSuccess = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "web_search",
		Name:      "ingest_last_success_timestamp_seconds",
		Help:      "Unix timestamp of the most recent successful webmaster import",
	},
	[]string{"site", "engine"},
)

var ingestProviderRuns = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "web_search",
		Name:      "provider_runs_total",
		Help:      "Webmaster provider attempts by privacy-safe outcome code",
	},
	[]string{"engine", "result"},
)

func init() {
	prometheus.MustRegister(
		searchClicks,
		searchImpressions,
		ingestRows,
		ingestLastSuccess,
		ingestProviderRuns,
	)
}

func recordPersistResults(results []model.WebSearchAnalyticsPersistResult) (accepted int, unchanged int) {
	for _, result := range results {
		path := result.Row.Path
		if path == "" {
			path = "(none)"
		}
		if result.Changed {
			accepted++
			ingestRows.WithLabelValues(result.Row.Site, result.Row.Provider, "accepted").Inc()
			if 0 < result.ClicksIncrease {
				searchClicks.WithLabelValues(result.Row.Site, result.Row.Provider, path).Add(result.ClicksIncrease)
			}
			if 0 < result.ImpressionIncrease {
				searchImpressions.WithLabelValues(result.Row.Site, result.Row.Provider, path).Add(result.ImpressionIncrease)
			}
		} else {
			unchanged++
			ingestRows.WithLabelValues(result.Row.Site, result.Row.Provider, "unchanged").Inc()
		}
	}
	return
}

const cleanupBatchSize = 10000

var analyticsLogWriter io.Writer = os.Stdout
var analyticsLogLock sync.Mutex

type searchRowLog struct {
	Event       string  `json:"event"`
	PrivacySafe bool    `json:"privacy_safe"`
	Engine      string  `json:"engine"`
	Site        string  `json:"site"`
	PeriodStart string  `json:"period_start"`
	SearchType  string  `json:"search_type"`
	Query       string  `json:"query"`
	Path        string  `json:"path"`
	Region      string  `json:"region,omitempty"`
	Device      string  `json:"device,omitempty"`
	Clicks      float64 `json:"clicks"`
	Impressions float64 `json:"impressions"`
	Position    float64 `json:"position"`
}

type providerLog struct {
	Event       string `json:"event"`
	PrivacySafe bool   `json:"privacy_safe"`
	Engine      string `json:"engine"`
	Site        string `json:"site,omitempty"`
	Result      string `json:"result"`
}

func writeAnalyticsLog(value any) {
	body, err := json.Marshal(value)
	if err != nil {
		return
	}
	analyticsLogLock.Lock()
	defer analyticsLogLock.Unlock()
	_, _ = fmt.Fprintln(analyticsLogWriter, string(body))
}

func emitSearchRow(config *model.AnalyticsConfig, row model.WebSearchAnalyticsRow) {
	if !config.Search.EmitPrivacyFilteredLogs || row.Query == "" {
		return
	}
	writeAnalyticsLog(searchRowLog{
		Event:       "web_search_query",
		PrivacySafe: true,
		Engine:      row.Provider,
		Site:        row.Site,
		PeriodStart: row.PeriodStart.UTC().Format(time.RFC3339),
		SearchType:  row.SearchType,
		Query:       row.Query,
		Path:        row.Path,
		Region:      row.Region,
		Device:      row.Device,
		Clicks:      row.Clicks,
		Impressions: row.Impressions,
		Position:    row.Position,
	})
}

func emitProviderResult(provider string, site string, result string) {
	ingestProviderRuns.WithLabelValues(provider, result).Inc()
	writeAnalyticsLog(providerLog{
		Event:       "web_search_ingest",
		PrivacySafe: true,
		Engine:      provider,
		Site:        site,
		Result:      result,
	})
}

func isDue(state model.WebSearchAnalyticsIngestState, now time.Time, interval time.Duration) bool {
	return state.LastAttempt.IsZero() || !now.Before(state.LastAttempt.Add(interval))
}

func recordRows(ctx context.Context, config *model.AnalyticsConfig, result *model.WebSearchAnalyticsResult, provider string, site string, rows []model.WebSearchAnalyticsRow, rejected int) {
	rows, volumeRejected := model.EnforceWebSearchAnalyticsVolumeCap(config.Search, rows)
	rejected += volumeRejected
	persisted := model.PersistWebSearchAnalyticsRows(ctx, config.Search, rows)
	result.RowsRemoved += int(model.TrimExcessWebSearchAnalyticsRows(ctx, config.Search, rows))
	accepted, unchanged := recordPersistResults(persisted)
	result.RowsAccepted += accepted
	result.RowsUnchanged += unchanged
	result.RowsRejected += rejected
	for _, persistedRow := range persisted {
		if persistedRow.Changed {
			emitSearchRow(config, persistedRow.Row)
		}
	}
	if 0 < rejected {
		ingestRows.WithLabelValues(site, provider, "rejected").Add(float64(rejected))
	}
}

func runProviderRows(ctx context.Context, config *model.AnalyticsConfig, result *model.WebSearchAnalyticsResult, provider string, site string, rows []model.WebSearchAnalyticsRow, rejected int) {
	recordRows(ctx, config, result, provider, site, rows, rejected)
}

func RunWebSearchAnalytics(ctx context.Context, now time.Time) (*model.WebSearchAnalyticsResult, error) {
	config, err := model.LoadAnalyticsConfig()
	if err != nil {
		return nil, err
	}
	result := &model.WebSearchAnalyticsResult{}
	if !config.Enabled || !config.Search.Enabled {
		return result, nil
	}
	// Retention runs before any external request, so a slow or unavailable
	// provider can never starve database cleanup.
	expiredRemoved := model.RemoveExpiredWebSearchAnalyticsRows(ctx, now.Add(-config.Retention()), cleanupBatchSize)
	belowVolumeRemoved := model.RemoveWebSearchAnalyticsRowsBelowVolume(ctx, config.Search, cleanupBatchSize)
	result.RowsRemoved = int(expiredRemoved + belowVolumeRemoved)
	if 0 < expiredRemoved || 0 < belowVolumeRemoved {
		glog.Infof(
			"[analytics]removed %d expired and %d below-volume web search rows\n",
			expiredRemoved,
			belowVolumeRemoved,
		)
	}
	credentials, err := loadAnalyticsCredentials()
	if err != nil {
		// A malformed optional vault file must not stop unrelated taskworker
		// tasks. Treat every provider as missing auth for this run.
		credentials = &analyticsCredentials{}
		emitProviderResult("all", "", "invalid_vault")
	}

	runGoogle(ctx, config, credentials, result, now.UTC())
	runBing(ctx, config, credentials, result, now.UTC())
	runYandex(ctx, config, credentials, result, now.UTC())
	runBaidu(ctx, config, result, now.UTC())

	return result, nil
}

func runGoogle(ctx context.Context, config *model.AnalyticsConfig, credentials *analyticsCredentials, result *model.WebSearchAnalyticsResult, now time.Time) {
	provider := config.Providers.Google
	if !provider.Enabled || provider.Mode != "api" {
		return
	}
	secret := cleanSecret(credentials.Google.ServiceAccountJSON)
	if secret == "" {
		result.ProvidersSkipped++
		emitProviderResult("google", "", "missing_auth")
		return
	}
	client, err := googleClient(ctx, secret)
	if err != nil {
		result.ProvidersSkipped++
		emitProviderResult("google", "", "invalid_auth")
		return
	}
	for _, site := range config.Sites {
		if site.Properties.GoogleSearchConsole == "" {
			continue
		}
		for _, searchType := range provider.SearchTypes {
			state := model.GetWebSearchAnalyticsIngestState(ctx, "google", site.Name, searchType)
			if !isDue(state, now, provider.IntervalDuration()) {
				continue
			}
			result.ProvidersAttempted++
			model.MarkWebSearchAnalyticsIngestAttempt(ctx, "google", site.Name, searchType, now)
			start, end, err := googleWindow(config, state, now)
			if err == nil {
				for day := start; day.Before(end); day = day.Add(24 * time.Hour) {
					rows, rejected, fetchErr := fetchGoogleDay(ctx, client, config, site, searchType, day)
					if fetchErr != nil {
						err = fetchErr
						break
					}
					runProviderRows(ctx, config, result, "google", site.Name, rows, rejected)
				}
			}
			if err != nil {
				code := errorCode(err)
				if isAuthError(err) {
					code = "invalid_auth"
				}
				model.MarkWebSearchAnalyticsIngestError(ctx, "google", site.Name, searchType, now, code)
				emitProviderResult("google", site.Name, code)
				continue
			}
			model.MarkWebSearchAnalyticsIngestSuccess(ctx, "google", site.Name, searchType, now, end)
			ingestLastSuccess.WithLabelValues(site.Name, "google").Set(float64(now.Unix()))
			emitProviderResult("google", site.Name, "success")
		}
	}
}

func runBing(ctx context.Context, config *model.AnalyticsConfig, credentials *analyticsCredentials, result *model.WebSearchAnalyticsResult, now time.Time) {
	provider := config.Providers.Bing
	if !provider.Enabled || provider.Mode != "api" || provider.Protocol != "rest" {
		return
	}
	apiKey := cleanSecret(credentials.Bing.APIKey)
	if apiKey == "" {
		result.ProvidersSkipped++
		emitProviderResult("bing", "", "missing_auth")
		return
	}
	client := &http.Client{Timeout: 45 * time.Second}
	for _, site := range config.Sites {
		if site.Properties.BingWebmaster == "" {
			continue
		}
		state := model.GetWebSearchAnalyticsIngestState(ctx, "bing", site.Name, "web")
		if !isDue(state, now, provider.IntervalDuration()) {
			continue
		}
		result.ProvidersAttempted++
		model.MarkWebSearchAnalyticsIngestAttempt(ctx, "bing", site.Name, "web", now)
		rows, rejected, err := fetchBing(ctx, client, config, provider, site, apiKey)
		if err != nil {
			code := errorCode(err)
			if isAuthError(err) {
				code = "invalid_auth"
			}
			model.MarkWebSearchAnalyticsIngestError(ctx, "bing", site.Name, "web", now, code)
			emitProviderResult("bing", site.Name, code)
			continue
		}
		runProviderRows(ctx, config, result, "bing", site.Name, rows, rejected)
		model.MarkWebSearchAnalyticsIngestSuccess(ctx, "bing", site.Name, "web", now, now)
		ingestLastSuccess.WithLabelValues(site.Name, "bing").Set(float64(now.Unix()))
		emitProviderResult("bing", site.Name, "success")
	}
}

func runYandex(ctx context.Context, config *model.AnalyticsConfig, credentials *analyticsCredentials, result *model.WebSearchAnalyticsResult, now time.Time) {
	provider := config.Providers.Yandex
	if !provider.Enabled || provider.Mode != "api" {
		return
	}
	userID := cleanSecret(credentials.Yandex.UserID)
	token := cleanSecret(credentials.Yandex.OAuthToken)
	if userID == "" || token == "" {
		result.ProvidersSkipped++
		emitProviderResult("yandex", "", "missing_auth")
		return
	}
	client := &http.Client{Timeout: 45 * time.Second}
	for _, site := range config.Sites {
		if site.Properties.YandexHostID == "" {
			continue
		}
		state := model.GetWebSearchAnalyticsIngestState(ctx, "yandex", site.Name, "web")
		if !isDue(state, now, provider.IntervalDuration()) {
			continue
		}
		result.ProvidersAttempted++
		model.MarkWebSearchAnalyticsIngestAttempt(ctx, "yandex", site.Name, "web", now)
		rows, rejected, cursor, err := fetchYandex(ctx, client, config, site, userID, token, now)
		if err != nil {
			code := errorCode(err)
			if isAuthError(err) {
				code = "invalid_auth"
			}
			model.MarkWebSearchAnalyticsIngestError(ctx, "yandex", site.Name, "web", now, code)
			emitProviderResult("yandex", site.Name, code)
			continue
		}
		runProviderRows(ctx, config, result, "yandex", site.Name, rows, rejected)
		model.MarkWebSearchAnalyticsIngestSuccess(ctx, "yandex", site.Name, "web", now, cursor)
		ingestLastSuccess.WithLabelValues(site.Name, "yandex").Set(float64(now.Unix()))
		emitProviderResult("yandex", site.Name, "success")
	}
}

func runBaidu(ctx context.Context, config *model.AnalyticsConfig, result *model.WebSearchAnalyticsResult, now time.Time) {
	provider := config.Providers.Baidu
	if !provider.Enabled || provider.Mode != "manual_import" {
		return
	}
	state := model.GetWebSearchAnalyticsIngestState(ctx, "baidu", "*", "manual")
	if !isDue(state, now, provider.IntervalDuration()) {
		return
	}
	result.ProvidersAttempted++
	model.MarkWebSearchAnalyticsIngestAttempt(ctx, "baidu", "*", "manual", now)
	manualResult, err := ingestBaiduObjects(ctx, config, provider)
	if err != nil {
		code := errorCode(err)
		model.MarkWebSearchAnalyticsIngestError(ctx, "baidu", "*", "manual", now, code)
		emitProviderResult("baidu", "", code)
		return
	}
	result.RowsAccepted += manualResult.RowsAccepted
	result.RowsRejected += manualResult.RowsRejected
	result.RowsRemoved += manualResult.RowsRemoved
	model.MarkWebSearchAnalyticsIngestSuccess(ctx, "baidu", "*", "manual", now, now)
	for _, site := range config.Sites {
		ingestLastSuccess.WithLabelValues(site.Name, "baidu").Set(float64(now.Unix()))
	}
	emitProviderResult("baidu", "", "success")
}
