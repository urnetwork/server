package monitor

import (
	"context"
	"crypto/ecdsa"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
)

const (
	appleReportingAPIBase          = "https://api.appstoreconnect.apple.com"
	appleCrashStateVersion         = 1
	appleCrashProcessingOverlap    = 7 * 24 * time.Hour
	appleCrashFreshnessTolerance   = 72 * time.Hour
	appleCrashProcessedRetention   = 30 * 24 * time.Hour
	appleCrashInstancesPerRunLimit = 60
	appleCrashSegmentsPerInstance  = 1_000
	appleCrashRowsPerInstance      = 200_000
	appleCrashDatesPerInstance     = 400
	appleCrashVersionsPerDate      = 10_000
	appleReportRequestReadLimit    = 1_000
	appleReportReadLimit           = 10_000
	appleCrashInstanceReadLimit    = 10_000
)

type appleReportingClients struct {
	api      *providerHTTP
	download *providerHTTP
}

type appleReportingClientFactory func(AppleReportingSettings, func() time.Time) (appleReportingClients, error)

// NewAppleCrashesSignal implements SIGNALS.md §20.2 (`apple-crashes`). It
// consumes App Store Connect's privacy-preserving App Crashes aggregate. Apple
// does not expose stack traces in this report, so the signal groups bounded
// evidence by event date and app version and preserves correction semantics.
func NewAppleCrashesSignal() Signal {
	return newAppleCrashesSignal(appleReportingAPIBase, newAppleReportingClients)
}

type appleCrashesSignal struct {
	apiBaseURL    string
	clientFactory appleReportingClientFactory
}

func newAppleCrashesSignal(apiBaseURL string, clientFactory appleReportingClientFactory) *appleCrashesSignal {
	return &appleCrashesSignal{apiBaseURL: strings.TrimRight(apiBaseURL, "/"), clientFactory: clientFactory}
}

func (*appleCrashesSignal) Number() string         { return "20.2" }
func (*appleCrashesSignal) Key() string            { return "apple-crashes" }
func (*appleCrashesSignal) ID() string             { return "mobile/apple-crashes" }
func (*appleCrashesSignal) Name() string           { return "Apple crash reports" }
func (*appleCrashesSignal) Cadence() time.Duration { return 6 * time.Hour }

func (s *appleCrashesSignal) Run(ctx context.Context, settings SignalSettings) (Alerts, error) {
	settings = settings.withDefaults()
	configured := settings.AppleReporting
	if !configured.Enabled {
		return nil, nil
	}
	if err := validateAppleReportingSettings(configured); err != nil {
		return nil, newProviderVisibilityError(providerAuthenticationClass, providerErrorText(err))
	}
	if s == nil || s.clientFactory == nil || strings.TrimSpace(s.apiBaseURL) == "" {
		return nil, fmt.Errorf("apple reporting client is unavailable")
	}
	stateLock, err := lockProviderState(ctx, settings.StateDir, s.Key())
	if err != nil {
		return nil, err
	}
	defer stateLock.Close()
	clients, err := s.clientFactory(configured, settings.Now)
	if err != nil {
		return nil, newProviderVisibilityError(providerAuthenticationClass, "apple reporting authentication: "+providerErrorText(err))
	}
	if clients.api == nil || clients.download == nil {
		return nil, fmt.Errorf("apple reporting clients are unavailable")
	}

	state := appleCrashState{
		Partitions: map[string]appleCrashPartition{},
		Processed:  map[string]string{},
	}
	loaded, err := loadProviderState(settings.StateDir, s.Key(), appleCrashStateVersion, &state)
	if err != nil {
		return nil, providerDataError("apple analytics report requests", err)
	}
	state.initialize()
	if err := validateAppleCrashState(state); err != nil {
		return nil, err
	}

	requests, err := appleListResources[appleReportRequest](
		ctx, clients.api, s.apiBaseURL,
		s.apiBaseURL+"/v1/apps/"+url.PathEscape(configured.AppID)+"/analyticsReportRequests?filter%5BaccessType%5D=ONGOING&limit=200",
		"apple analytics report requests", appleReportRequestReadLimit,
	)
	if err != nil {
		return nil, providerDataError("apple analytics reports", err)
	}
	request, requestAlert := selectAppleReportRequest(settings, requests)
	if requestAlert != nil {
		return Alerts{*requestAlert}, nil
	}

	reports, err := appleListResources[appleReport](
		ctx, clients.api, s.apiBaseURL,
		s.apiBaseURL+"/v1/analyticsReportRequests/"+url.PathEscape(request.ID)+"/reports?limit=200",
		"apple analytics reports", appleReportReadLimit,
	)
	if err != nil {
		return nil, providerDataError("apple crash report instances", err)
	}
	report, reportAlert := selectAppleCrashReport(settings, reports)
	if reportAlert != nil {
		return Alerts{*reportAlert}, nil
	}

	instances, err := appleListResources[appleReportInstance](
		ctx, clients.api, s.apiBaseURL,
		s.apiBaseURL+"/v1/analyticsReports/"+url.PathEscape(report.ID)+"/instances?filter%5Bgranularity%5D=DAILY&limit=200",
		"apple crash report instances", appleCrashInstanceReadLimit,
	)
	if err != nil {
		return nil, providerDataError("apple crash report instances", err)
	}
	validated, err := validateAppleInstances(instances)
	if err != nil {
		return nil, providerDataError("apple crash report instances", err)
	}
	if len(validated) == 0 {
		return Alerts{appleVisibilityAlert(settings, "apple-crash-data-unobservable",
			"Apple has not published a DAILY App Crashes instance",
			"The ongoing request and report exist, but the instances response is empty. Apple may still be generating the first report, or privacy and data availability may prevent publication; this is not proof of zero crashes.",
			"daily_instances=0",
			"Wait for the documented initial report-generation window, verify the ongoing request remains active, and compare App Analytics before rerunning.")}, nil
	}

	now := settings.Now().UTC()
	latest := validated[len(validated)-1]
	if latest.processingTime.After(now.Add(24 * time.Hour)) {
		return nil, providerDataError("apple crash report instances", fmt.Errorf("newest processingDate is implausibly in the future"))
	}
	alerts := Alerts{}
	if now.Sub(latest.processingTime) > appleCrashFreshnessTolerance {
		alerts = append(alerts, appleVisibilityAlert(settings, "apple-crash-data-stale",
			"Apple App Crashes data is stale",
			"The newest provider processing date is older than three days, so recent Apple-platform crash state is unknown even though the API remains reachable.",
			fmt.Sprintf("latest_processing_date=%s lag=%s", latest.ProcessingDate, now.Sub(latest.processingTime).Round(time.Hour)),
			"Check whether the ongoing request stopped due to inactivity and whether App Store Connect Analytics is delayed; restore report generation before interpreting crash counts."))
	}

	selected := selectNewAppleInstances(validated, state)
	backlog := 0
	if len(selected) > appleCrashInstancesPerRunLimit {
		backlog = len(selected) - appleCrashInstancesPerRunLimit
		selected = selected[:appleCrashInstancesPerRunLimit]
	}
	next := state.clone()
	touched := map[string]struct{}{}
	for _, instance := range selected {
		partitions, rowCount, err := s.downloadAppleInstance(ctx, clients, configured.AppID, instance)
		if err != nil {
			return nil, providerDataError("apple crash report instance", err)
		}
		next.Processed[instance.ID] = instance.ProcessingDate
		recordAppleInstanceVisibility(&next, instance.ProcessingDate, rowCount)
		if rowCount == 0 {
			continue
		}
		applyApplePartitions(&next, partitions, touched)
	}
	pruneAppleState(&next, latest.processingTime)

	touchedDates := make([]string, 0, len(touched))
	for eventDate := range touched {
		touchedDates = append(touchedDates, eventDate)
	}
	sort.Strings(touchedDates)
	for _, eventDate := range touchedDates {
		alerts = append(alerts, compareApplePartition(settings, eventDate, state.Partitions[eventDate], next.Partitions[eventDate])...)
	}
	if next.LatestEmptyProcessingDate != "" {
		alerts = append(alerts, appleVisibilityAlert(settings, "apple-crash-privacy-suppressed",
			"Apple published an App Crashes instance without observable rows",
			"The provider returned no aggregate crash rows for its latest processed instance. Apple only reports opted-in data and suppresses low-volume groups below its privacy threshold, so the result cannot be interpreted as zero crashes.",
			"latest_empty_processing_date="+next.LatestEmptyProcessingDate,
			"Compare App Store Connect Analytics and independent client telemetry. Keep the probe enabled; a later non-empty processing instance will clear this visibility alert."))
	}
	if backlog > 0 {
		alerts = append(alerts, appleBacklogAlert(settings, len(selected), backlog))
	}

	if len(selected) != 0 || !loaded {
		if err := saveProviderState(settings.StateDir, s.Key(), appleCrashStateVersion, next); err != nil {
			return nil, err
		}
	}
	return alerts, nil
}

func validateAppleReportingSettings(settings AppleReportingSettings) error {
	if settings.LoadError != nil {
		return fmt.Errorf("apple reporting credential: %w", settings.LoadError)
	}
	missing := make([]string, 0, 4)
	for name, value := range map[string]string{
		"app_id":      settings.AppID,
		"issuer_id":   settings.IssuerID,
		"key_id":      settings.KeyID,
		"private_key": settings.PrivateKey,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		return fmt.Errorf("apple reporting credential is incomplete: missing %s", strings.Join(missing, ", "))
	}
	if parsed, err := strconv.ParseInt(settings.AppID, 10, 64); err != nil || parsed <= 0 {
		return fmt.Errorf("apple reporting app_id must be a positive integer")
	}
	if !providerResourceIDPattern.MatchString(settings.IssuerID) {
		return fmt.Errorf("apple reporting issuer_id is invalid")
	}
	if !providerResourceIDPattern.MatchString(settings.KeyID) {
		return fmt.Errorf("apple reporting key_id is invalid")
	}
	return nil
}

type appleAuthDoer struct {
	client   *http.Client
	key      *ecdsa.PrivateKey
	issuerID string
	keyID    string
	now      func() time.Time
}

func (d *appleAuthDoer) Do(request *http.Request) (*http.Response, error) {
	token, err := appleReportingJWT(d.key, d.issuerID, d.keyID, d.now())
	if err != nil {
		return nil, err
	}
	cloned := request.Clone(request.Context())
	cloned.Header = request.Header.Clone()
	cloned.Header.Set("Authorization", "Bearer "+token)
	return d.client.Do(cloned)
}

func newAppleReportingClients(settings AppleReportingSettings, now func() time.Time) (appleReportingClients, error) {
	key, err := gojwt.ParseECPrivateKeyFromPEM([]byte(settings.PrivateKey))
	if err != nil {
		return appleReportingClients{}, fmt.Errorf("parse App Store Connect private key: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	base := &http.Client{Timeout: 30 * time.Second, CheckRedirect: providerSameOriginRedirect}
	download := &http.Client{Timeout: 30 * time.Second, CheckRedirect: providerHTTPSDownloadRedirect}
	return appleReportingClients{
		api: newProviderHTTP(&appleAuthDoer{
			client: base, key: key, issuerID: settings.IssuerID, keyID: settings.KeyID, now: now,
		}),
		download: newProviderHTTP(download),
	}, nil
}

func appleReportingJWT(key *ecdsa.PrivateKey, issuerID, keyID string, now time.Time) (string, error) {
	if key == nil {
		return "", fmt.Errorf("App Store Connect private key is unavailable")
	}
	token := gojwt.NewWithClaims(gojwt.SigningMethodES256, gojwt.MapClaims{
		"iss": issuerID,
		"iat": now.Unix(),
		"exp": now.Add(15 * time.Minute).Unix(),
		"aud": "appstoreconnect-v1",
	})
	token.Header["kid"] = keyID
	token.Header["typ"] = "JWT"
	signed, err := token.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("sign App Store Connect token: %w", err)
	}
	return signed, nil
}

type appleCrashState struct {
	Partitions                map[string]appleCrashPartition `json:"partitions"`
	Processed                 map[string]string              `json:"processed_instances"`
	LastProcessingDate        string                         `json:"last_processing_date,omitempty"`
	LatestEmptyProcessingDate string                         `json:"latest_empty_processing_date,omitempty"`
}

func (state *appleCrashState) initialize() {
	if state.Partitions == nil {
		state.Partitions = map[string]appleCrashPartition{}
	}
	if state.Processed == nil {
		state.Processed = map[string]string{}
	}
	for date, partition := range state.Partitions {
		partition.initialize()
		state.Partitions[date] = partition
	}
}

func (state appleCrashState) clone() appleCrashState {
	result := appleCrashState{
		Partitions: map[string]appleCrashPartition{}, Processed: map[string]string{},
		LastProcessingDate: state.LastProcessingDate, LatestEmptyProcessingDate: state.LatestEmptyProcessingDate,
	}
	for key, value := range state.Processed {
		result.Processed[key] = value
	}
	for date, partition := range state.Partitions {
		result.Partitions[date] = partition.clone()
	}
	return result
}

func validateAppleCrashState(state appleCrashState) error {
	validateDate := func(label, value string, optional bool) error {
		if value == "" && optional {
			return nil
		}
		if _, err := time.Parse("2006-01-02", value); err != nil {
			return fmt.Errorf("load apple-crashes cursor data: invalid %s", label)
		}
		return nil
	}
	if err := validateDate("last processing date", state.LastProcessingDate, true); err != nil {
		return err
	}
	if err := validateDate("empty processing date", state.LatestEmptyProcessingDate, true); err != nil {
		return err
	}
	for id, date := range state.Processed {
		if !validAppleResourceID(id) {
			return fmt.Errorf("load apple-crashes cursor data: invalid processed instance")
		}
		if err := validateDate("processed instance date", date, false); err != nil {
			return err
		}
	}
	for eventDate, partition := range state.Partitions {
		if err := validateDate("event date", eventDate, false); err != nil {
			return err
		}
		if err := validateDate("partition processing date", partition.ProcessingDate, false); err != nil {
			return err
		}
		if eventDate > partition.ProcessingDate {
			return fmt.Errorf("load apple-crashes cursor data: event date follows processing date")
		}
		for version, group := range partition.Groups {
			if strings.TrimSpace(version) == "" || group.Crashes < 0 || group.UniqueDevices < 0 {
				return fmt.Errorf("load apple-crashes cursor data: invalid crash group")
			}
			for _, values := range []map[string]int64{group.Devices, group.Platforms} {
				for label, count := range values {
					if strings.TrimSpace(label) == "" || count < 0 {
						return fmt.Errorf("load apple-crashes cursor data: invalid breakdown")
					}
				}
			}
		}
	}
	return nil
}

type appleCrashPartition struct {
	ProcessingDate string                     `json:"processing_date"`
	Groups         map[string]appleCrashGroup `json:"groups"`
}

func (partition *appleCrashPartition) initialize() {
	if partition.Groups == nil {
		partition.Groups = map[string]appleCrashGroup{}
	}
	for version, group := range partition.Groups {
		group.initialize()
		partition.Groups[version] = group
	}
}

func (partition appleCrashPartition) clone() appleCrashPartition {
	result := appleCrashPartition{ProcessingDate: partition.ProcessingDate, Groups: map[string]appleCrashGroup{}}
	for version, group := range partition.Groups {
		result.Groups[version] = group.clone()
	}
	return result
}

type appleCrashGroup struct {
	Crashes       int64            `json:"crashes"`
	UniqueDevices int64            `json:"unique_devices_row_sum"`
	Devices       map[string]int64 `json:"devices"`
	Platforms     map[string]int64 `json:"platform_versions"`
}

func (group *appleCrashGroup) initialize() {
	if group.Devices == nil {
		group.Devices = map[string]int64{}
	}
	if group.Platforms == nil {
		group.Platforms = map[string]int64{}
	}
}

func (group appleCrashGroup) clone() appleCrashGroup {
	result := appleCrashGroup{Crashes: group.Crashes, UniqueDevices: group.UniqueDevices, Devices: map[string]int64{}, Platforms: map[string]int64{}}
	for key, value := range group.Devices {
		result.Devices[key] = value
	}
	for key, value := range group.Platforms {
		result.Platforms[key] = value
	}
	return result
}

type appleReportRequest struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	Attributes struct {
		AccessType             string `json:"accessType"`
		StoppedDueToInactivity bool   `json:"stoppedDueToInactivity"`
	} `json:"attributes"`
}

type appleReport struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	Attributes struct {
		Name     string `json:"name"`
		Category string `json:"category"`
	} `json:"attributes"`
}

type appleReportInstance struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	Attributes struct {
		Granularity    string `json:"granularity"`
		ProcessingDate string `json:"processingDate"`
	} `json:"attributes"`
	ProcessingDate string    `json:"-"`
	processingTime time.Time `json:"-"`
}

type appleReportSegment struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	Attributes struct {
		Checksum    string `json:"checksum"`
		SizeInBytes int64  `json:"sizeInBytes"`
		URL         string `json:"url"`
	} `json:"attributes"`
}

func appleListResources[T any](ctx context.Context, client *providerHTTP, apiBaseURL, endpoint, label string, maxItems int) ([]T, error) {
	if maxItems <= 0 {
		return nil, fmt.Errorf("%s: invalid resource limit", label)
	}
	result := []T{}
	next := endpoint
	pagination := providerPagination{limit: 100}
	for next != "" {
		var response struct {
			Data  []T `json:"data"`
			Links struct {
				Next string `json:"next"`
			} `json:"links"`
		}
		if err := client.json(ctx, http.MethodGet, next, label, nil, nil, &response); err != nil {
			return nil, err
		}
		if len(response.Data) > maxItems-len(result) {
			return nil, fmt.Errorf("%s exceeded %d resources", label, maxItems)
		}
		result = append(result, response.Data...)
		if strings.TrimSpace(response.Links.Next) == "" {
			break
		}
		more, err := pagination.next(response.Links.Next)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		if !more {
			break
		}
		next, err = providerNextURL(apiBaseURL, response.Links.Next)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
	}
	return result, nil
}

func selectAppleReportRequest(settings SignalSettings, requests []appleReportRequest) (appleReportRequest, *Alert) {
	sort.SliceStable(requests, func(i, j int) bool { return requests[i].ID < requests[j].ID })
	var stopped bool
	for _, request := range requests {
		if request.Type != "analyticsReportRequests" || request.Attributes.AccessType != "ONGOING" {
			continue
		}
		if request.Attributes.StoppedDueToInactivity {
			stopped = true
			continue
		}
		if validAppleResourceID(request.ID) {
			return request, nil
		}
	}
	if stopped {
		alert := appleVisibilityAlert(settings, "apple-crash-request-stopped",
			"Apple stopped the ongoing Analytics Report request due to inactivity",
			"App Store Connect suspends ongoing report generation when instances are not retrieved regularly. Existing credentials may still authenticate while all new crash visibility has stopped.",
			"ongoing_request=stoppedDueToInactivity",
			"An App Store Connect Admin must create a new ONGOING analytics report request for this app; after that, the Sales and Reports monitor key can resume read-only downloads.")
		return appleReportRequest{}, &alert
	}
	alert := appleVisibilityAlert(settings, "apple-crash-request-missing",
		"Apple has no active ongoing Analytics Report request for the app",
		"Apple does not generate Analytics Reports until an Admin creates an ONGOING request. A read-only reporting key cannot create that prerequisite, and an empty request list is not zero crashes.",
		"active_ongoing_requests=0",
		"Use an App Store Connect Admin identity once to create an ONGOING analytics report request for the configured app, then keep the monitor key at Sales and Reports access.")
	return appleReportRequest{}, &alert
}

func selectAppleCrashReport(settings SignalSettings, reports []appleReport) (appleReport, *Alert) {
	matches := make([]appleReport, 0, 2)
	for _, report := range reports {
		if report.Type == "analyticsReports" && validAppleResourceID(report.ID) && strings.HasPrefix(report.Attributes.Name, "App Crashes") {
			matches = append(matches, report)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		iExact := matches[i].Attributes.Name == "App Crashes"
		jExact := matches[j].Attributes.Name == "App Crashes"
		if iExact != jExact {
			return iExact
		}
		return matches[i].ID < matches[j].ID
	})
	if len(matches) != 0 {
		return matches[0], nil
	}
	alert := appleVisibilityAlert(settings, "apple-crash-report-missing",
		"Apple has not generated the App Crashes analytics report",
		"The ongoing request is active, but its report catalog has no name beginning with App Crashes. Initial generation can take one to two days, and unavailable or privacy-suppressed data must not be read as zero crashes.",
		fmt.Sprintf("reports=%d app_crash_reports=0", len(reports)),
		"Wait through Apple's initial report-generation window, then verify App Crashes is present in App Store Connect and rerun this probe.")
	return appleReport{}, &alert
}

func validAppleResourceID(value string) bool {
	value = strings.TrimSpace(value)
	return providerResourceIDPattern.MatchString(value)
}

func validateAppleInstances(instances []appleReportInstance) ([]appleReportInstance, error) {
	result := make([]appleReportInstance, 0, len(instances))
	seen := map[string]struct{}{}
	for _, instance := range instances {
		if instance.Type != "analyticsReportInstances" || instance.Attributes.Granularity != "DAILY" {
			return nil, fmt.Errorf("apple crash report returned an unexpected instance type or granularity")
		}
		if !validAppleResourceID(instance.ID) {
			return nil, fmt.Errorf("apple crash report returned an invalid instance identifier")
		}
		if _, exists := seen[instance.ID]; exists {
			return nil, fmt.Errorf("apple crash report repeated an instance identifier")
		}
		seen[instance.ID] = struct{}{}
		processing, err := time.Parse("2006-01-02", instance.Attributes.ProcessingDate)
		if err != nil {
			return nil, fmt.Errorf("apple crash report instance has invalid processingDate")
		}
		instance.ProcessingDate = instance.Attributes.ProcessingDate
		instance.processingTime = processing.UTC()
		result = append(result, instance)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].ProcessingDate != result[j].ProcessingDate {
			return result[i].ProcessingDate < result[j].ProcessingDate
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func selectNewAppleInstances(instances []appleReportInstance, state appleCrashState) []appleReportInstance {
	threshold := instances[len(instances)-1].processingTime.Add(-appleCrashProcessingOverlap)
	if state.LastProcessingDate != "" {
		if previous, err := time.Parse("2006-01-02", state.LastProcessingDate); err == nil {
			threshold = previous.Add(-appleCrashProcessingOverlap)
		}
	}
	selected := make([]appleReportInstance, 0, len(instances))
	for _, instance := range instances {
		if instance.processingTime.Before(threshold) {
			continue
		}
		if state.Processed[instance.ID] == instance.ProcessingDate {
			continue
		}
		selected = append(selected, instance)
	}
	return selected
}

func recordAppleInstanceVisibility(state *appleCrashState, processingDate string, rowCount int) {
	previousLatest := state.LastProcessingDate
	if previousLatest == "" || processingDate > previousLatest {
		state.LastProcessingDate = processingDate
	}
	if rowCount == 0 {
		// A late-discovered older empty instance cannot make current provider
		// visibility unknown after a newer non-empty instance was observed.
		if (previousLatest == "" || processingDate >= previousLatest) &&
			(state.LatestEmptyProcessingDate == "" || processingDate >= state.LatestEmptyProcessingDate) {
			state.LatestEmptyProcessingDate = processingDate
		}
		return
	}
	if processingDate >= state.LatestEmptyProcessingDate {
		state.LatestEmptyProcessingDate = ""
	}
}

func applyApplePartitions(state *appleCrashState, partitions map[string]appleCrashPartition, touched map[string]struct{}) {
	for eventDate, partition := range partitions {
		previous, exists := state.Partitions[eventDate]
		// Apple corrections are ordered by processingDate. An instance can be
		// discovered late inside the overlap, but it must never roll a partition
		// back after a newer processing instance has already been committed.
		if exists && partition.ProcessingDate < previous.ProcessingDate {
			continue
		}
		state.Partitions[eventDate] = partition
		touched[eventDate] = struct{}{}
	}
}

func (s *appleCrashesSignal) downloadAppleInstance(
	ctx context.Context,
	clients appleReportingClients,
	appID string,
	instance appleReportInstance,
) (map[string]appleCrashPartition, int, error) {
	segments, err := appleListResources[appleReportSegment](
		ctx, clients.api, s.apiBaseURL,
		s.apiBaseURL+"/v1/analyticsReportInstances/"+url.PathEscape(instance.ID)+"/segments?limit=200",
		"apple crash report segments", appleCrashSegmentsPerInstance,
	)
	if err != nil {
		return nil, 0, err
	}
	if len(segments) > appleCrashSegmentsPerInstance {
		return nil, 0, fmt.Errorf("apple crash report instance exceeded %d segments", appleCrashSegmentsPerInstance)
	}
	sort.SliceStable(segments, func(i, j int) bool { return segments[i].ID < segments[j].ID })
	partitions := map[string]appleCrashPartition{}
	rows := 0
	seenSegments := map[string]struct{}{}
	for _, segment := range segments {
		if segment.Type != "analyticsReportSegments" || !validAppleResourceID(segment.ID) {
			return nil, 0, fmt.Errorf("apple crash report returned an invalid segment")
		}
		if _, exists := seenSegments[segment.ID]; exists {
			return nil, 0, fmt.Errorf("apple crash report repeated a segment identifier")
		}
		seenSegments[segment.ID] = struct{}{}
		if segment.Attributes.SizeInBytes < 0 || segment.Attributes.SizeInBytes > providerSegmentLimit {
			return nil, 0, fmt.Errorf("apple crash report segment has invalid size")
		}
		if err := validateAppleSegmentURL(s.apiBaseURL, segment.Attributes.URL); err != nil {
			return nil, 0, err
		}
		downloadHeaders := http.Header{"Accept-Encoding": {"identity"}}
		compressed, err := clients.download.bytes(ctx, http.MethodGet, segment.Attributes.URL, "apple crash report download", downloadHeaders, nil, providerSegmentLimit)
		if err != nil {
			return nil, 0, err
		}
		if int64(len(compressed)) != segment.Attributes.SizeInBytes {
			return nil, 0, fmt.Errorf("apple crash report segment size mismatch")
		}
		if err := verifyProviderChecksum(compressed, segment.Attributes.Checksum); err != nil {
			return nil, 0, fmt.Errorf("apple crash report segment: %w", err)
		}
		expanded, err := expandProviderGzip(compressed)
		if err != nil {
			return nil, 0, fmt.Errorf("apple crash report segment: %w", err)
		}
		segmentRows, err := parseAppleCrashTSV(expanded, appID, instance.ProcessingDate, partitions, appleCrashRowsPerInstance-rows)
		if err != nil {
			return nil, 0, fmt.Errorf("apple crash report segment: %w", err)
		}
		rows += segmentRows
		if rows > appleCrashRowsPerInstance {
			return nil, 0, fmt.Errorf("apple crash report instance exceeded %d rows", appleCrashRowsPerInstance)
		}
	}
	return partitions, rows, nil
}

func validateAppleSegmentURL(apiBaseURL, raw string) error {
	candidate, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || candidate.Host == "" || candidate.User != nil || candidate.Fragment != "" {
		return fmt.Errorf("apple crash report segment has an invalid download URL")
	}
	if candidate.Scheme == "https" {
		return nil
	}
	// Synthetic httptest servers use loopback HTTP. Production's API origin is
	// HTTPS, so this exception cannot admit an insecure provider URL there.
	base, baseErr := url.Parse(apiBaseURL)
	if baseErr == nil && base.Scheme == "http" && candidate.Scheme == "http" && candidate.Host == base.Host {
		return nil
	}
	return fmt.Errorf("apple crash report segment download URL must use HTTPS")
}

func parseAppleCrashTSV(data []byte, appID, processingDate string, partitions map[string]appleCrashPartition, maxRows int) (int, error) {
	reader := csv.NewReader(strings.NewReader(string(data)))
	reader.Comma = '\t'
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err == io.EOF {
		return 0, fmt.Errorf("report is missing its header")
	}
	if err != nil {
		return 0, fmt.Errorf("read report header: %w", err)
	}
	indexes := map[string]int{}
	for index, name := range header {
		name = strings.TrimSpace(strings.TrimPrefix(name, "\ufeff"))
		if _, duplicate := indexes[name]; duplicate {
			return 0, fmt.Errorf("report has duplicate column %q", providerEvidence(name))
		}
		indexes[name] = index
	}
	required := []string{"Date", "App Apple Identifier", "App Version", "Device", "Platform Version", "Crashes", "Unique Devices"}
	for _, name := range required {
		if _, exists := indexes[name]; !exists {
			return 0, fmt.Errorf("report is missing column %q", name)
		}
	}
	maxIndex := 0
	for _, name := range required {
		maxIndex = max(maxIndex, indexes[name])
	}
	rows := 0
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read report row: %w", err)
		}
		if rows >= maxRows {
			return 0, fmt.Errorf("apple crash report exceeded %d rows", appleCrashRowsPerInstance)
		}
		if len(record) <= maxIndex {
			return 0, fmt.Errorf("report row has too few columns")
		}
		eventDate := strings.TrimSpace(record[indexes["Date"]])
		if _, err := time.Parse("2006-01-02", eventDate); err != nil {
			return 0, fmt.Errorf("report row has invalid Date")
		}
		if eventDate > processingDate {
			return 0, fmt.Errorf("report row Date follows its processingDate")
		}
		if strings.TrimSpace(record[indexes["App Apple Identifier"]]) != appID {
			return 0, fmt.Errorf("report row belongs to a different App Apple Identifier")
		}
		version := providerLabel(record[indexes["App Version"]])
		if version == "" {
			version = "unknown"
		}
		crashes, err := parseProviderNonnegativeInt("apple crash count", record[indexes["Crashes"]])
		if err != nil {
			return 0, err
		}
		unique, err := parseProviderNonnegativeInt("apple unique-device count", record[indexes["Unique Devices"]])
		if err != nil {
			return 0, err
		}
		if unique > crashes {
			return 0, fmt.Errorf("apple unique-device count exceeds crash count")
		}
		device := providerLabel(record[indexes["Device"]])
		platform := providerLabel(record[indexes["Platform Version"]])
		partition := partitions[eventDate]
		if partition.Groups == nil && len(partitions) >= appleCrashDatesPerInstance {
			return 0, fmt.Errorf("apple crash report exceeded %d event dates", appleCrashDatesPerInstance)
		}
		partition.ProcessingDate = processingDate
		partition.initialize()
		group := partition.Groups[version]
		if group.Devices == nil && len(partition.Groups) >= appleCrashVersionsPerDate {
			return 0, fmt.Errorf("apple crash report exceeded %d versions for one event date", appleCrashVersionsPerDate)
		}
		group.initialize()
		if err := addAppleCount(&group.Crashes, crashes); err != nil {
			return 0, err
		}
		if err := addAppleCount(&group.UniqueDevices, unique); err != nil {
			return 0, err
		}
		device = firstNonempty(device, "unknown")
		platform = firstNonempty(platform, "unknown")
		deviceCrashes := group.Devices[device]
		if err := addAppleCount(&deviceCrashes, crashes); err != nil {
			return 0, err
		}
		group.Devices[device] = deviceCrashes
		platformCrashes := group.Platforms[platform]
		if err := addAppleCount(&platformCrashes, crashes); err != nil {
			return 0, err
		}
		group.Platforms[platform] = platformCrashes
		partition.Groups[version] = group
		partitions[eventDate] = partition
		rows++
	}
	return rows, nil
}

func addAppleCount(total *int64, value int64) error {
	if value < 0 || *total > math.MaxInt64-value {
		return fmt.Errorf("apple crash report count overflow")
	}
	*total += value
	return nil
}

func compareApplePartition(settings SignalSettings, eventDate string, previous, current appleCrashPartition) Alerts {
	previous.initialize()
	current.initialize()
	versions := make([]string, 0, len(current.Groups)+len(previous.Groups))
	seen := map[string]struct{}{}
	for version := range current.Groups {
		seen[version] = struct{}{}
		versions = append(versions, version)
	}
	for version := range previous.Groups {
		if _, exists := seen[version]; !exists {
			versions = append(versions, version)
		}
	}
	sort.Strings(versions)
	alerts := Alerts{}
	for _, version := range versions {
		before, beforeExists := previous.Groups[version]
		after, afterExists := current.Groups[version]
		if afterExists && after.Crashes == 0 && (!beforeExists || before.Crashes == 0) {
			continue
		}
		if beforeExists && afterExists && appleCrashGroupsEqual(before, after) {
			continue
		}
		alerts = append(alerts, appleGroupAlert(settings, eventDate, version, previous.ProcessingDate, current.ProcessingDate, before, beforeExists, after, afterExists))
	}
	return alerts
}

func appleCrashGroupsEqual(left, right appleCrashGroup) bool {
	if left.Crashes != right.Crashes || left.UniqueDevices != right.UniqueDevices || len(left.Devices) != len(right.Devices) || len(left.Platforms) != len(right.Platforms) {
		return false
	}
	for key, value := range left.Devices {
		if right.Devices[key] != value {
			return false
		}
	}
	for key, value := range left.Platforms {
		if right.Platforms[key] != value {
			return false
		}
	}
	return true
}

func appleGroupAlert(
	settings SignalSettings,
	eventDate string,
	version string,
	previousProcessing string,
	currentProcessing string,
	before appleCrashGroup,
	beforeExists bool,
	after appleCrashGroup,
	afterExists bool,
) Alert {
	class := "apple-crash-group"
	symptom := fmt.Sprintf("Apple observed crashes for app version %s on %s", version, eventDate)
	mechanism := "Apple aggregated opted-in crash events for one app version and event date. Device and platform rows identify the affected release boundary without exposing an individual device or a stack trace."
	if beforeExists {
		class = "apple-crash-correction"
		symptom = fmt.Sprintf("Apple revised crash data for app version %s on %s", version, eventDate)
		mechanism = "A newer Apple processing instance replaced the older rows for this event date. The monitor used replacement semantics instead of summing instances, so the delta is a late arrival or provider correction rather than double counting."
	}
	current := "suppressed_or_absent"
	evidence := "current_group=[absent from the replacement partition]"
	if afterExists {
		current = fmt.Sprintf("crashes=%d unique_devices_row_sum=%d", after.Crashes, after.UniqueDevices)
		evidence = fmt.Sprintf("top_devices_by_crashes=%s\ntop_platform_versions_by_crashes=%s", formatAppleBreakdown(after.Devices), formatAppleBreakdown(after.Platforms))
	}
	prior := "none"
	if beforeExists {
		prior = fmt.Sprintf("crashes=%d unique_devices_row_sum=%d", before.Crashes, before.UniqueDevices)
	}
	return Alert{
		SignalNumber: "20.2", SignalKey: "apple-crashes", SignalID: "mobile/apple-crashes", SignalName: "Apple crash reports",
		Severity: SeverityWarn, Class: class, Target: "app-store/" + settings.AppleReporting.AppID,
		Frame: "date=" + eventDate + " version=" + version, Environment: settings.Environment, ObservedAt: settings.Now(), Sustain: 1,
		Symptom: symptom, Mechanism: mechanism,
		Baseline: "No new opted-in aggregate crash group is published for a released Apple app version; absent privacy-threshold rows remain unknown rather than zero.",
		Observed: fmt.Sprintf("previous={%s processing_date=%s} current={%s processing_date=%s}", prior, firstNonempty(previousProcessing, "none"), current, currentProcessing),
		Evidence: evidence,
		Context:  "Apple's Unique Devices value is summed across device/platform rows only as bounded context and may double-count a device across rows. App Crashes is complete within five days and includes only opted-in data with at least five users in the applicable privacy group. This aggregate API does not contain a crash stack.",
		Action:   "Correlate the app version, event date, and top device/OS rows with symbolicated client crash telemetry. Fix the owning app, embedded SDK, OS-specific interaction, or server contract; obtain detailed stacks through the separately instrumented iOS crash path rather than inferring a frame from aggregate counts.",
		Verify:   "After shipping the owning fix, later processing instances stop adding this version/date group, any correction is applied by replacement, and provider freshness remains within three days.",
		Playbook: "SIGNALS.md §20.2",
	}
}

func formatAppleBreakdown(values map[string]int64) string {
	type item struct {
		name  string
		count int64
	}
	items := make([]item, 0, len(values))
	for name, count := range values {
		items = append(items, item{name: name, count: count})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].count != items[j].count {
			return items[i].count > items[j].count
		}
		return items[i].name < items[j].name
	})
	items = items[:min(len(items), 5)]
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, fmt.Sprintf("%s:%d", item.name, item.count))
	}
	return strings.Join(parts, ", ")
}

func pruneAppleState(state *appleCrashState, latest time.Time) {
	threshold := latest.Add(-appleCrashProcessedRetention).Format("2006-01-02")
	for id, date := range state.Processed {
		if date < threshold {
			delete(state.Processed, id)
		}
	}
	for eventDate, partition := range state.Partitions {
		if partition.ProcessingDate < threshold {
			delete(state.Partitions, eventDate)
		}
	}
}

func appleVisibilityAlert(settings SignalSettings, class, symptom, mechanism, observed, action string) Alert {
	return Alert{
		SignalNumber: "20.2", SignalKey: "apple-crashes", SignalID: "mobile/apple-crashes", SignalName: "Apple crash reports",
		Severity: SeverityWarn, Class: class, Target: "app-store/" + settings.AppleReporting.AppID,
		Environment: settings.Environment, ObservedAt: settings.Now(), Sustain: 1,
		Symptom: symptom, Mechanism: mechanism,
		Baseline: "An active ONGOING request publishes a recent DAILY App Crashes instance with an observable aggregate or an explicitly understood privacy boundary.",
		Observed: observed, Action: action,
		Verify:   "The next probe authenticates, finds the active App Crashes report, downloads and validates every segment without forwarding Authorization to its signed URL, and advances to a current processing date.",
		Playbook: "SIGNALS.md §20.2",
	}
}

func appleBacklogAlert(settings SignalSettings, processed, remaining int) Alert {
	return Alert{
		SignalNumber: "20.2", SignalKey: "apple-crashes", SignalID: "mobile/apple-crashes", SignalName: "Apple crash reports",
		Severity: SeverityWarn, Class: "apple-crash-backlog", Target: "app-store/" + settings.AppleReporting.AppID,
		Environment: settings.Environment, ObservedAt: settings.Now(), Sustain: 1,
		Symptom:   fmt.Sprintf("Apple crash ingestion has %d report instances still queued after processing %d", remaining, processed),
		Mechanism: "A long monitor outage or provider replay exceeded the bounded per-run download cap. The cursor commits only validated instances in chronological order, leaving the rest for the next run without summing replacement partitions.",
		Baseline:  fmt.Sprintf("No more than %d unprocessed daily instances accumulate between six-hour cadences.", appleCrashInstancesPerRunLimit),
		Observed:  fmt.Sprintf("processed_instances=%d remaining_instances=%d", processed, remaining),
		Action:    "Keep the monitor running and confirm the backlog shrinks on each cadence. If it grows, investigate App Store Connect latency and local state persistence rather than raising the download bound blindly.",
		Verify:    "Subsequent runs drain the backlog to zero, keep checksum/size validation intact, and publish the newest processing date.",
		Playbook:  "SIGNALS.md §20.2",
	}
}
