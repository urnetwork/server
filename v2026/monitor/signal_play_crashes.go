package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	oauthjwt "golang.org/x/oauth2/jwt"
)

const (
	playReportingAPIBase        = "https://playdeveloperreporting.googleapis.com"
	playReportingOAuthScope     = "https://www.googleapis.com/auth/playdeveloperreporting"
	playCrashStateVersion       = 1
	playCrashIssueLookback      = 48 * time.Hour
	playCrashStateRetention     = 14 * 24 * time.Hour
	playCrashFreshnessTolerance = 72 * time.Hour
	playCrashAlertLimit         = 50
	playCrashIssueReadLimit     = 10_000
	playCrashMetricRowLimit     = 10_000
)

var playPackagePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(?:\.[A-Za-z][A-Za-z0-9_]*)+$`)

type playReportingClientFactory func(context.Context, GooglePlayReportingSettings) (*providerHTTP, error)

// NewPlayCrashesSignal implements SIGNALS.md §20.1 (`play-crashes`). It reads
// Google Play's aggregate Android-vitals crash issues and a bounded sanitized
// sample for each newly advancing issue/version group. The optional credential
// is deliberately checked before any config validation or network access.
func NewPlayCrashesSignal() Signal {
	return newPlayCrashesSignal(playReportingAPIBase, newGooglePlayReportingClient)
}

type playCrashesSignal struct {
	apiBaseURL    string
	clientFactory playReportingClientFactory
}

func newPlayCrashesSignal(apiBaseURL string, clientFactory playReportingClientFactory) *playCrashesSignal {
	return &playCrashesSignal{apiBaseURL: strings.TrimRight(apiBaseURL, "/"), clientFactory: clientFactory}
}

func (*playCrashesSignal) Number() string         { return "20.1" }
func (*playCrashesSignal) Key() string            { return "play-crashes" }
func (*playCrashesSignal) ID() string             { return "mobile/play-crashes" }
func (*playCrashesSignal) Name() string           { return "Google Play crash reports" }
func (*playCrashesSignal) Cadence() time.Duration { return 30 * time.Minute }

func (s *playCrashesSignal) Run(ctx context.Context, settings SignalSettings) (Alerts, error) {
	settings = settings.withDefaults()
	configured := settings.GooglePlay
	if !configured.Enabled {
		return nil, nil
	}
	if err := validateGooglePlayReportingSettings(configured); err != nil {
		return nil, newProviderVisibilityError(providerAuthenticationClass, providerErrorText(err))
	}
	if s == nil || s.clientFactory == nil || strings.TrimSpace(s.apiBaseURL) == "" {
		return nil, fmt.Errorf("google play reporting client is unavailable")
	}
	stateLock, err := lockProviderState(ctx, settings.StateDir, s.Key())
	if err != nil {
		return nil, err
	}
	defer stateLock.Close()
	client, err := s.clientFactory(ctx, configured)
	if err != nil {
		return nil, newProviderVisibilityError(providerAuthenticationClass, "google play authentication: "+providerErrorText(err))
	}

	now := settings.Now().UTC()
	issueStart, issueEnd := playCrashInterval(now)
	state := playCrashState{Groups: map[string]playCrashCursor{}}
	if _, err := loadProviderState(settings.StateDir, s.Key(), playCrashStateVersion, &state); err != nil {
		return nil, err
	}
	if state.Groups == nil {
		state.Groups = map[string]playCrashCursor{}
	}
	if err := validatePlayCrashState(state); err != nil {
		return nil, err
	}

	metric, err := s.crashMetric(ctx, client, configured.PackageName)
	if err != nil {
		return nil, providerDataError("google play crash metric", err)
	}
	if metric.FreshThrough.After(now.Add(24 * time.Hour)) {
		return nil, providerDataError("google play crash metric", fmt.Errorf("freshness boundary is implausibly in the future"))
	}
	issues, err := s.crashIssues(ctx, client, configured.PackageName, issueStart, issueEnd)
	if err != nil {
		return nil, providerDataError("google play crash issues", err)
	}

	alerts := s.metricAlerts(settings, metric, len(issues))
	next := playCrashState{Groups: map[string]playCrashCursor{}}
	for key, cursor := range state.Groups {
		if cursor.LastReportTime.After(now.Add(-playCrashStateRetention)) {
			next.Groups[key] = cursor
		}
	}

	type advancingIssue struct {
		issue      playErrorIssue
		key        string
		count      int64
		users      int64
		at         time.Time
		correction bool
	}
	advancing := make([]advancingIssue, 0, len(issues))
	seenIssueGroups := map[string]struct{}{}
	for _, issue := range issues {
		parsed, err := validatePlayIssue(configured.PackageName, issue)
		if err != nil {
			return nil, providerDataError("google play crash issue", err)
		}
		if parsed.at.Before(issueStart) || !parsed.at.Before(issueEnd) {
			return nil, providerDataError("google play crash issue", fmt.Errorf("report hour is outside the requested interval"))
		}
		if _, exists := seenIssueGroups[parsed.key]; exists {
			return nil, providerDataError("google play crash issues", fmt.Errorf("provider repeated an issue/version group"))
		}
		seenIssueGroups[parsed.key] = struct{}{}
		cursor := playCrashCursor{
			LastReportTime: parsed.at,
			ReportCount:    parsed.count,
			Fingerprint:    playIssueFingerprint(issue),
			WindowEnd:      issueEnd,
		}
		previous, exists := state.Groups[parsed.key]
		next.Groups[parsed.key] = cursor
		advanced := !exists || parsed.at.After(previous.LastReportTime) ||
			(parsed.at.Equal(previous.LastReportTime) && parsed.count > previous.ReportCount)
		// Counts and even the latest included report hour can fall naturally
		// when the next whole-hour query boundary ages data out of this moving
		// window. A backward replacement is a correction only when it revises
		// the exact same query window; root-cause metadata changes remain visible
		// across windows because they cannot be explained by ordinary aging.
		sameWindow := !previous.WindowEnd.IsZero() && issueEnd.Equal(previous.WindowEnd)
		corrected := exists && !advanced && (cursor.Fingerprint != previous.Fingerprint ||
			(sameWindow && (!parsed.at.Equal(previous.LastReportTime) || parsed.count != previous.ReportCount)))
		if advanced || corrected {
			advancing = append(advancing, advancingIssue{
				issue: issue, key: parsed.key, count: parsed.count, users: parsed.users, at: parsed.at, correction: corrected,
			})
		}
	}
	sort.SliceStable(advancing, func(i, j int) bool {
		if advancing[i].count != advancing[j].count {
			return advancing[i].count > advancing[j].count
		}
		return advancing[i].key < advancing[j].key
	})

	visible := min(len(advancing), playCrashAlertLimit)
	for _, item := range advancing[:visible] {
		report, err := s.sampleReport(
			ctx, client, configured.PackageName,
			issueStart, issueEnd, item.issue,
		)
		if err != nil {
			return nil, providerDataError("google play crash sample", err)
		}
		alerts = append(alerts, playIssueAlert(settings, configured.PackageName, item.issue, report, item.count, item.users, item.at, metric, item.correction))
	}
	if visible < len(advancing) {
		alerts = append(alerts, playIssueOverflowAlert(settings, configured.PackageName, len(advancing), visible))
	}

	if err := saveProviderState(settings.StateDir, s.Key(), playCrashStateVersion, next); err != nil {
		return nil, err
	}
	return alerts, nil
}

// Google requires error issue/report search intervals to be aligned to whole
// UTC hours. Use the most recent complete boundary; the next 30-minute cadence
// overlaps it, so reports processed during the partial hour are not lost.
func playCrashInterval(now time.Time) (time.Time, time.Time) {
	end := now.UTC().Truncate(time.Hour)
	return end.Add(-playCrashIssueLookback), end
}

func validateGooglePlayReportingSettings(settings GooglePlayReportingSettings) error {
	if settings.LoadError != nil {
		return fmt.Errorf("google play reporting credential: %w", settings.LoadError)
	}
	missing := make([]string, 0, 5)
	for name, value := range map[string]string{
		"package_name":   settings.PackageName,
		"client_email":   settings.ClientEmail,
		"private_key":    settings.PrivateKey,
		"private_key_id": settings.PrivateKeyID,
		"token_uri":      settings.TokenURL,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		return fmt.Errorf("google play reporting credential is incomplete: missing %s", strings.Join(missing, ", "))
	}
	if !playPackagePattern.MatchString(settings.PackageName) {
		return fmt.Errorf("google play reporting package_name is invalid")
	}
	address, err := mail.ParseAddress(settings.ClientEmail)
	if err != nil || address.Address != settings.ClientEmail {
		return fmt.Errorf("google play reporting client_email is invalid")
	}
	if !providerResourceIDPattern.MatchString(settings.PrivateKeyID) {
		return fmt.Errorf("google play reporting private_key_id is invalid")
	}
	parsed, err := url.Parse(settings.TokenURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("google play reporting token_uri must be an HTTPS URL")
	}
	return nil
}

func newGooglePlayReportingClient(ctx context.Context, settings GooglePlayReportingSettings) (*providerHTTP, error) {
	base := &http.Client{Timeout: 30 * time.Second, CheckRedirect: providerSameOriginRedirect}
	authContext := context.WithValue(ctx, oauth2.HTTPClient, base)
	config := &oauthjwt.Config{
		Email:        settings.ClientEmail,
		PrivateKey:   []byte(settings.PrivateKey),
		PrivateKeyID: settings.PrivateKeyID,
		Scopes:       []string{playReportingOAuthScope},
		TokenURL:     settings.TokenURL,
	}
	client := config.Client(authContext)
	client.Timeout = 30 * time.Second
	client.CheckRedirect = providerSameOriginRedirect
	return newProviderHTTP(client), nil
}

type playCrashState struct {
	Groups map[string]playCrashCursor `json:"groups"`
}

type playCrashCursor struct {
	LastReportTime time.Time `json:"last_report_time"`
	ReportCount    int64     `json:"report_count"`
	Fingerprint    string    `json:"fingerprint"`
	// WindowEnd distinguishes a provider correction from ordinary aging of
	// the moving 48-hour query. It is optional for compatibility with cursors
	// written before the monitor recorded the exact search boundary.
	WindowEnd time.Time `json:"window_end,omitempty"`
}

func validatePlayCrashState(state playCrashState) error {
	for key, cursor := range state.Groups {
		if strings.TrimSpace(key) == "" || cursor.LastReportTime.IsZero() || cursor.ReportCount < 0 {
			return fmt.Errorf("load play-crashes cursor data: invalid issue watermark")
		}
		if !cursor.WindowEnd.IsZero() &&
			(cursor.WindowEnd.Minute() != 0 || cursor.WindowEnd.Second() != 0 || cursor.WindowEnd.Nanosecond() != 0 ||
				!cursor.LastReportTime.Before(cursor.WindowEnd)) {
			return fmt.Errorf("load play-crashes cursor data: invalid issue window")
		}
		if cursor.Fingerprint != "" {
			decoded, err := hex.DecodeString(cursor.Fingerprint)
			if err != nil || len(decoded) != sha256.Size {
				return fmt.Errorf("load play-crashes cursor data: invalid issue fingerprint")
			}
		}
	}
	return nil
}

func playIssueFingerprint(issue playErrorIssue) string {
	value := strings.Join([]string{
		issue.Type,
		issue.Cause,
		issue.Location,
		issue.LastAppVersion.VersionCode,
		issue.LastOSVersion.APILevel,
	}, "\x00")
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

type playErrorIssue struct {
	Name                string         `json:"name"`
	Type                string         `json:"type"`
	Cause               string         `json:"cause"`
	Location            string         `json:"location"`
	DistinctUsers       string         `json:"distinctUsers"`
	ErrorReportCount    string         `json:"errorReportCount"`
	LastErrorReportTime string         `json:"lastErrorReportTime"`
	IssueURI            string         `json:"issueUri"`
	LastAppVersion      playAppVersion `json:"lastAppVersion"`
	LastOSVersion       playOSVersion  `json:"lastOsVersion"`
	SampleErrorReports  []string       `json:"sampleErrorReports"`
}

type playAppVersion struct {
	VersionCode string `json:"versionCode"`
}

type playOSVersion struct {
	APILevel string `json:"apiLevel"`
}

type playErrorReport struct {
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Issue       string         `json:"issue"`
	ReportText  string         `json:"reportText"`
	EventTime   string         `json:"eventTime"`
	AppVersion  playAppVersion `json:"appVersion"`
	OSVersion   playOSVersion  `json:"osVersion"`
	DeviceModel struct {
		MarketingName string `json:"marketingName"`
	} `json:"deviceModel"`
}

type playIssueValidation struct {
	key   string
	count int64
	users int64
	at    time.Time
}

func validatePlayIssue(packageName string, issue playErrorIssue) (playIssueValidation, error) {
	if issue.Type != "CRASH" {
		return playIssueValidation{}, fmt.Errorf("google play issue %q has unexpected type %q", providerEvidence(issue.Name), providerEvidence(issue.Type))
	}
	issueID, err := playResourceID(packageName, issue.Name)
	if err != nil {
		return playIssueValidation{}, fmt.Errorf("google play issue resource: %w", err)
	}
	if len(issue.SampleErrorReports) > 1 {
		return playIssueValidation{}, fmt.Errorf("google play issue %s returned multiple sample reports", issueID)
	}
	version := strings.TrimSpace(issue.LastAppVersion.VersionCode)
	if version == "" {
		version = "unknown"
	} else if _, err := parseProviderNonnegativeInt("google play version code", version); err != nil {
		return playIssueValidation{}, err
	}
	count, err := parseProviderNonnegativeInt("google play error report count", issue.ErrorReportCount)
	if err != nil {
		return playIssueValidation{}, err
	}
	if count == 0 {
		return playIssueValidation{}, fmt.Errorf("google play crash issue has zero error reports")
	}
	users, err := parseProviderNonnegativeInt("google play distinct users", issue.DistinctUsers)
	if err != nil {
		return playIssueValidation{}, err
	}
	at, err := parsePlayTimestamp(issue.LastErrorReportTime)
	if err != nil {
		return playIssueValidation{}, fmt.Errorf("google play issue %s last report time: %w", issueID, err)
	}
	return playIssueValidation{
		key: issueID + "|" + version, count: count, users: users, at: at,
	}, nil
}

func parseProviderNonnegativeInt(label, value string) (int64, error) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s is not a nonnegative integer", label)
	}
	return parsed, nil
}

func parsePlayTimestamp(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid timestamp")
}

func playResourceID(packageName, resource string) (string, error) {
	prefix := "apps/" + packageName + "/"
	if !strings.HasPrefix(resource, prefix) {
		return "", fmt.Errorf("resource does not belong to configured package")
	}
	id := strings.TrimSpace(resource[strings.LastIndex(resource, "/")+1:])
	if !providerResourceIDPattern.MatchString(id) {
		return "", fmt.Errorf("resource has an invalid identifier")
	}
	return id, nil
}

func (s *playCrashesSignal) crashIssues(
	ctx context.Context,
	client *providerHTTP,
	packageName string,
	start time.Time,
	end time.Time,
) ([]playErrorIssue, error) {
	endpoint := s.apiBaseURL + "/v1beta1/apps/" + url.PathEscape(packageName) + "/errorIssues:search"
	baseQuery := url.Values{
		"filter":                 {"errorIssueType = CRASH"},
		"orderBy":                {"errorReportCount desc"},
		"pageSize":               {"1000"},
		"sampleErrorReportLimit": {"1"},
	}
	addPlayInterval(baseQuery, start.UTC(), end.UTC())

	var result []playErrorIssue
	pageToken := ""
	pagination := providerPagination{limit: 100}
	for {
		query := cloneURLValues(baseQuery)
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		var response struct {
			ErrorIssues   []playErrorIssue `json:"errorIssues"`
			NextPageToken string           `json:"nextPageToken"`
		}
		if err := client.json(ctx, http.MethodGet, endpoint+"?"+query.Encode(), "google play crash issues", nil, nil, &response); err != nil {
			return nil, err
		}
		result = append(result, response.ErrorIssues...)
		if len(result) > playCrashIssueReadLimit {
			return nil, fmt.Errorf("google play crash issues exceeded %d rows", playCrashIssueReadLimit)
		}
		more, err := pagination.next(response.NextPageToken)
		if err != nil {
			return nil, fmt.Errorf("google play crash issues: %w", err)
		}
		if !more {
			break
		}
		pageToken = strings.TrimSpace(response.NextPageToken)
	}
	return result, nil
}

func (s *playCrashesSignal) sampleReport(
	ctx context.Context,
	client *providerHTTP,
	packageName string,
	start time.Time,
	end time.Time,
	issue playErrorIssue,
) (*playErrorReport, error) {
	if len(issue.SampleErrorReports) == 0 {
		return nil, nil
	}
	reportID, err := playResourceID(packageName, issue.SampleErrorReports[0])
	if err != nil {
		return nil, fmt.Errorf("google play sample report resource: %w", err)
	}
	endpoint := s.apiBaseURL + "/v1beta1/apps/" + url.PathEscape(packageName) + "/errorReports:search"
	query := url.Values{
		"filter":   {"errorReportId = " + reportID},
		"pageSize": {"1"},
	}
	addPlayInterval(query, start.UTC(), end.UTC())
	var response struct {
		ErrorReports  []playErrorReport `json:"errorReports"`
		NextPageToken string            `json:"nextPageToken"`
	}
	if err := client.json(ctx, http.MethodGet, endpoint+"?"+query.Encode(), "google play crash sample", nil, nil, &response); err != nil {
		return nil, err
	}
	if len(response.ErrorReports) == 0 {
		return nil, nil
	}
	if len(response.ErrorReports) != 1 || strings.TrimSpace(response.NextPageToken) != "" {
		return nil, fmt.Errorf("google play crash sample returned an ambiguous result")
	}
	report := response.ErrorReports[0]
	if report.Type != "CRASH" {
		return nil, fmt.Errorf("google play sample report has unexpected type %q", providerEvidence(report.Type))
	}
	if report.Issue != "" && report.Issue != issue.Name {
		return nil, fmt.Errorf("google play sample report belongs to a different issue")
	}
	actualReportID, err := playResourceID(packageName, report.Name)
	if err != nil {
		return nil, fmt.Errorf("google play sample report resource: %w", err)
	}
	if actualReportID != reportID {
		return nil, fmt.Errorf("google play crash sample returned a different report")
	}
	if report.EventTime != "" {
		if _, err := parsePlayTimestamp(report.EventTime); err != nil {
			return nil, fmt.Errorf("google play sample report event time: %w", err)
		}
	}
	if report.AppVersion.VersionCode != "" {
		if _, err := parseProviderNonnegativeInt("google play sample version code", report.AppVersion.VersionCode); err != nil {
			return nil, err
		}
	}
	if report.OSVersion.APILevel != "" {
		if _, err := parseProviderNonnegativeInt("google play sample OS API level", report.OSVersion.APILevel); err != nil {
			return nil, err
		}
	}
	return &report, nil
}

func addPlayInterval(values url.Values, start, end time.Time) {
	add := func(prefix string, value time.Time) {
		values.Set(prefix+".year", strconv.Itoa(value.Year()))
		values.Set(prefix+".month", strconv.Itoa(int(value.Month())))
		values.Set(prefix+".day", strconv.Itoa(value.Day()))
		values.Set(prefix+".hours", strconv.Itoa(value.Hour()))
		values.Set(prefix+".minutes", strconv.Itoa(value.Minute()))
		values.Set(prefix+".seconds", strconv.Itoa(value.Second()))
		values.Set(prefix+".timeZone.id", "UTC")
	}
	add("interval.startTime", start)
	add("interval.endTime", end)
}

func cloneURLValues(source url.Values) url.Values {
	cloned := make(url.Values, len(source))
	for key, values := range source {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

type playDateTime struct {
	Year     int `json:"year"`
	Month    int `json:"month"`
	Day      int `json:"day"`
	Hours    int `json:"hours,omitempty"`
	Minutes  int `json:"minutes,omitempty"`
	Seconds  int `json:"seconds,omitempty"`
	TimeZone struct {
		ID string `json:"id"`
	} `json:"timeZone"`
}

func (value playDateTime) time() (time.Time, error) {
	if value.Year == 0 || value.Month == 0 || value.Day == 0 || strings.TrimSpace(value.TimeZone.ID) == "" {
		return time.Time{}, fmt.Errorf("incomplete provider date-time")
	}
	location, err := time.LoadLocation(value.TimeZone.ID)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid provider time zone")
	}
	parsed := time.Date(value.Year, time.Month(value.Month), value.Day, value.Hours, value.Minutes, value.Seconds, 0, location)
	year, month, day := parsed.Date()
	if year != value.Year || int(month) != value.Month || day != value.Day ||
		parsed.Hour() != value.Hours || parsed.Minute() != value.Minutes || parsed.Second() != value.Seconds {
		return time.Time{}, fmt.Errorf("invalid provider date-time")
	}
	return parsed, nil
}

type playCrashMetric struct {
	Available         bool
	FreshThrough      time.Time
	LatestPeriod      time.Time
	CrashRate         float64
	UserPerceivedRate float64
	DistinctUsers     float64
}

func (s *playCrashesSignal) crashMetric(ctx context.Context, client *providerHTTP, packageName string) (playCrashMetric, error) {
	endpoint := s.apiBaseURL + "/v1beta1/apps/" + url.PathEscape(packageName) + "/crashRateMetricSet"
	var metadata struct {
		FreshnessInfo struct {
			Freshnesses []struct {
				AggregationPeriod string       `json:"aggregationPeriod"`
				LatestEndTime     playDateTime `json:"latestEndTime"`
			} `json:"freshnesses"`
		} `json:"freshnessInfo"`
	}
	if err := client.json(ctx, http.MethodGet, endpoint, "google play crash freshness", nil, nil, &metadata); err != nil {
		return playCrashMetric{}, err
	}
	var end playDateTime
	for _, freshness := range metadata.FreshnessInfo.Freshnesses {
		if freshness.AggregationPeriod == "DAILY" {
			end = freshness.LatestEndTime
			break
		}
	}
	freshThrough, err := end.time()
	if err != nil {
		// Missing freshness is a provider-observability condition, not malformed
		// crash data. Return an unavailable metric so Run emits a structured alert.
		return playCrashMetric{}, nil
	}
	start := freshThrough.AddDate(0, 0, -8)
	request := playMetricRequest{
		TimelineSpec: playTimelineSpec{
			AggregationPeriod: "DAILY",
			StartTime:         playDateTimeFromTime(start, "America/Los_Angeles"),
			EndTime:           playDateTimeFromTime(freshThrough, "America/Los_Angeles"),
		},
		Metrics:  []string{"crashRate", "userPerceivedCrashRate", "distinctUsers"},
		PageSize: 1000,
	}
	queryEndpoint := endpoint + ":query"
	pagination := providerPagination{limit: 100}
	latest := playCrashMetric{FreshThrough: freshThrough.UTC()}
	periods := map[time.Time]struct{}{}
	rowCount := 0
	for {
		body, err := json.Marshal(request)
		if err != nil {
			return playCrashMetric{}, fmt.Errorf("google play crash metric request: %w", err)
		}
		var response playMetricResponse
		headers := http.Header{"Content-Type": {"application/json"}}
		if err := client.json(ctx, http.MethodPost, queryEndpoint, "google play crash metric", headers, body, &response); err != nil {
			return playCrashMetric{}, err
		}
		for _, row := range response.Rows {
			rowCount++
			if rowCount > playCrashMetricRowLimit {
				return playCrashMetric{}, fmt.Errorf("google play crash metric exceeded %d rows", playCrashMetricRowLimit)
			}
			observedAt, err := row.StartTime.time()
			if err != nil {
				return playCrashMetric{}, fmt.Errorf("google play crash metric period: %w", err)
			}
			if row.AggregationPeriod != "DAILY" {
				return playCrashMetric{}, fmt.Errorf("google play crash metric returned unexpected aggregation %q", providerEvidence(row.AggregationPeriod))
			}
			if observedAt.Before(start) || !observedAt.Before(freshThrough) {
				return playCrashMetric{}, fmt.Errorf("google play crash metric returned a period outside the requested timeline")
			}
			periodKey := observedAt.UTC()
			if _, exists := periods[periodKey]; exists {
				return playCrashMetric{}, fmt.Errorf("google play crash metric repeated a daily period")
			}
			periods[periodKey] = struct{}{}
			parsed, err := parsePlayMetricRow(row)
			if err != nil {
				return playCrashMetric{}, err
			}
			if !latest.Available || observedAt.After(latest.LatestPeriod) {
				latest.Available = true
				latest.LatestPeriod = observedAt.UTC()
				latest.CrashRate = parsed.CrashRate
				latest.UserPerceivedRate = parsed.UserPerceivedRate
				latest.DistinctUsers = parsed.DistinctUsers
			}
		}
		more, err := pagination.next(response.NextPageToken)
		if err != nil {
			return playCrashMetric{}, fmt.Errorf("google play crash metric: %w", err)
		}
		if !more {
			break
		}
		request.PageToken = strings.TrimSpace(response.NextPageToken)
	}
	return latest, nil
}

type playMetricRequest struct {
	TimelineSpec playTimelineSpec `json:"timelineSpec"`
	Metrics      []string         `json:"metrics"`
	PageSize     int              `json:"pageSize"`
	PageToken    string           `json:"pageToken,omitempty"`
}

type playTimelineSpec struct {
	AggregationPeriod string       `json:"aggregationPeriod"`
	StartTime         playDateTime `json:"startTime"`
	EndTime           playDateTime `json:"endTime"`
}

type playMetricResponse struct {
	Rows          []playMetricRow `json:"rows"`
	NextPageToken string          `json:"nextPageToken"`
}

type playMetricRow struct {
	AggregationPeriod string       `json:"aggregationPeriod"`
	StartTime         playDateTime `json:"startTime"`
	Metrics           []struct {
		Metric       string `json:"metric"`
		DecimalValue struct {
			Value string `json:"value"`
		} `json:"decimalValue"`
	} `json:"metrics"`
}

func playDateTimeFromTime(value time.Time, zone string) playDateTime {
	location, err := time.LoadLocation(zone)
	if err == nil {
		value = value.In(location)
	}
	result := playDateTime{Year: value.Year(), Month: int(value.Month()), Day: value.Day()}
	result.TimeZone.ID = zone
	return result
}

func parsePlayMetricRow(row playMetricRow) (playCrashMetric, error) {
	values := map[string]float64{}
	for _, metric := range row.Metrics {
		if _, duplicate := values[metric.Metric]; duplicate {
			return playCrashMetric{}, fmt.Errorf("google play crash metric row repeated %q", providerEvidence(metric.Metric))
		}
		raw := strings.TrimSpace(metric.DecimalValue.Value)
		if raw == "" {
			raw = "0"
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return playCrashMetric{}, fmt.Errorf("google play crash metric %q is invalid", providerEvidence(metric.Metric))
		}
		values[metric.Metric] = value
	}
	for _, required := range []string{"crashRate", "userPerceivedCrashRate", "distinctUsers"} {
		if _, exists := values[required]; !exists {
			return playCrashMetric{}, fmt.Errorf("google play crash metric row is missing %s", required)
		}
	}
	return playCrashMetric{
		CrashRate: values["crashRate"], UserPerceivedRate: values["userPerceivedCrashRate"], DistinctUsers: values["distinctUsers"],
	}, nil
}

func (s *playCrashesSignal) metricAlerts(settings SignalSettings, metric playCrashMetric, issueCount int) Alerts {
	now := settings.Now().UTC()
	if metric.FreshThrough.IsZero() {
		return Alerts{playVisibilityAlert(settings, "play-crash-data-unobservable",
			"Google Play did not publish DAILY crash freshness metadata",
			"The reporting API authenticated, but it did not identify a complete daily crash boundary. An empty issue response therefore cannot be interpreted as zero crashes.",
			"daily_freshness=missing",
			"Confirm the Play Developer Reporting API is enabled, the service account has View app information (read-only), and Android vitals has enough processed data; then rerun the probe.")}
	}
	if now.Sub(metric.FreshThrough) > playCrashFreshnessTolerance {
		return Alerts{playVisibilityAlert(settings, "play-crash-data-stale",
			"Google Play crash data is stale",
			"The provider's own DAILY freshness boundary is older than the permitted reporting lag, so recent Android stability is unknown even if issue search succeeds.",
			fmt.Sprintf("fresh_through=%s lag=%s", metric.FreshThrough.Format(time.RFC3339), now.Sub(metric.FreshThrough).Round(time.Minute)),
			"Check Google Play reporting status and API permissions, retain the last good cursor, and rerun after freshness advances.")}
	}
	if !metric.Available {
		return Alerts{playVisibilityAlert(settings, "play-crash-data-unobservable",
			"Google Play returned no crash-rate rows",
			"Freshness metadata exists, but the matching metric query returned no explicit value. Empty rows may reflect low volume, processing, or visibility and are not proof of zero crashes.",
			fmt.Sprintf("fresh_through=%s metric_rows=0 issue_groups=%d", metric.FreshThrough.Format(time.RFC3339), issueCount),
			"Compare Android vitals in Play Console, verify data sharing and reporting permissions, and rerun after the provider publishes a metric row.")}
	}
	return nil
}

func playVisibilityAlert(settings SignalSettings, class, symptom, mechanism, observed, action string) Alert {
	return Alert{
		SignalNumber: "20.1", SignalKey: "play-crashes", SignalID: "mobile/play-crashes", SignalName: "Google Play crash reports",
		Severity: SeverityWarn, Class: class, Target: "google-play/" + settings.GooglePlay.PackageName,
		Environment: settings.Environment, ObservedAt: settings.Now(), Sustain: 1,
		Symptom: symptom, Mechanism: mechanism,
		Baseline: "Google Play publishes a recent explicit crash metric value and issue search can be interpreted against that visibility boundary.",
		Observed: observed, Action: action,
		Verify:   "The next probe run authenticates, freshness advances within 72 hours, and the API returns an explicit metric row (including an explicit zero when there were no observed crashes).",
		Playbook: "SIGNALS.md §20.1",
	}
}

func playIssueAlert(
	settings SignalSettings,
	packageName string,
	issue playErrorIssue,
	report *playErrorReport,
	count int64,
	users int64,
	at time.Time,
	metric playCrashMetric,
	correction bool,
) Alert {
	issueID, _ := playResourceID(packageName, issue.Name)
	version := strings.TrimSpace(issue.LastAppVersion.VersionCode)
	if version == "" {
		version = "unknown"
	}
	evidenceParts := []string{
		fmt.Sprintf("cause=%s", providerEvidence(issue.Cause)),
		fmt.Sprintf("location=%s", providerEvidence(issue.Location)),
		fmt.Sprintf("last_os_api=%s", providerEvidence(issue.LastOSVersion.APILevel)),
	}
	if uri := safeGooglePlayIssueURI(issue.IssueURI); uri != "" {
		evidenceParts = append(evidenceParts, "play_console="+uri)
	}
	if report != nil {
		evidenceParts = append(evidenceParts,
			fmt.Sprintf("sample_event=%s sample_version=%s sample_device=%s sample_os_api=%s", providerEvidence(report.EventTime), providerEvidence(report.AppVersion.VersionCode), providerEvidence(report.DeviceModel.MarketingName), providerEvidence(report.OSVersion.APILevel)),
		)
		if stack := providerEvidence(report.ReportText); stack != "" {
			evidenceParts = append(evidenceParts, "sanitized_sample:\n"+stack)
		}
	} else {
		evidenceParts = append(evidenceParts, "sanitized_sample=[provider supplied no sample report]")
	}
	metricContext := "latest_daily_metric=unavailable"
	if metric.Available {
		metricContext = fmt.Sprintf(
			"latest_daily_period=%s crash_rate_decimal=%g user_perceived_crash_rate_decimal=%g distinct_users=%g",
			metric.LatestPeriod.Format("2006-01-02"), metric.CrashRate, metric.UserPerceivedRate, metric.DistinctUsers,
		)
	}
	class := "play-crash-issue"
	symptom := fmt.Sprintf("Google Play crash issue %s advanced on Android version code %s", issueID, version)
	mechanism := "Android vitals grouped one or more newly observed fatal reports under the same likely root cause and app version. The provider-sanitized sample identifies the failing frame without treating device identifiers as alert data."
	if correction {
		class = "play-crash-correction"
		symptom = fmt.Sprintf("Google Play revised crash issue %s on Android version code %s", issueID, version)
		mechanism = "Google Play revised the issue's report hour, rolling-window count, or root-cause metadata without a forward occurrence. The watermark records the replacement so the correction is visible once and is not mistaken for a new crash."
	}
	return Alert{
		SignalNumber: "20.1", SignalKey: "play-crashes", SignalID: "mobile/play-crashes", SignalName: "Google Play crash reports",
		Severity: SeverityWarn, Class: class, Target: "google-play/" + packageName + "/" + issueID,
		Frame: "version=" + version, Environment: settings.Environment, ObservedAt: settings.Now(), Sustain: 1,
		Symptom:   symptom,
		Mechanism: mechanism,
		Baseline:  "No new fatal crash issue occurrence is observed for a released Android version; an explicit zero metric is distinct from absent provider data.",
		Observed:  fmt.Sprintf("reports_in_48h=%d estimated_distinct_users_in_48h=%d last_report_hour=%s %s", count, users, at.Format(time.RFC3339), metricContext),
		Evidence:  strings.Join(evidenceParts, "\n"),
		Context:   "Counts cover the moving 48-hour overlap and are not lifetime totals. The local issue/version cursor prevents duplicate alerts while a later report hour, correction, or higher count at the same hour advances the group.",
		Action:    "Symbolicate the bounded sample against the named version, reproduce the top crashing frame, identify whether the app, embedded SDK, device/OS, or server contract owns it, and fix that root cause. Prioritize issue groups by affected users and recurrence; do not suppress the monitor merely because Play grouping changes.",
		Verify:    "Ship the owning fix, then confirm this issue/version stops advancing through the 48-hour overlap and the explicit crash-rate series remains current.",
		Playbook:  "SIGNALS.md §20.1",
	}
}

func safeGooglePlayIssueURI(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() != "play.google.com" || parsed.User != nil || parsed.Fragment != "" {
		return ""
	}
	return providerEvidence(parsed.String())
}

func playIssueOverflowAlert(settings SignalSettings, packageName string, total, included int) Alert {
	return Alert{
		SignalNumber: "20.1", SignalKey: "play-crashes", SignalID: "mobile/play-crashes", SignalName: "Google Play crash reports",
		Severity: SeverityWarn, Class: "play-crash-overflow", Target: "google-play/" + packageName,
		Environment: settings.Environment, ObservedAt: settings.Now(), Sustain: 1,
		Symptom:   fmt.Sprintf("Google Play returned %d advancing crash groups; detailed alerts were bounded to %d", total, included),
		Mechanism: "A broad crash wave exceeded the monitor's per-run evidence cap. Every observed cursor was persisted, but downloading an unbounded sample per issue would amplify a provider or application incident.",
		Baseline:  fmt.Sprintf("At most %d crash issue/version groups advance in one 30-minute cadence.", playCrashAlertLimit),
		Observed:  fmt.Sprintf("advancing_groups=%d detailed_groups=%d", total, included),
		Action:    "Use Play Console to rank the remaining groups by distinct users and common release/device boundaries, then address the shared root cause before widening evidence collection.",
		Verify:    "The next runs remain within the detail cap and each emitted issue group stops advancing after its owning fix.",
		Playbook:  "SIGNALS.md §20.1",
	}
}
