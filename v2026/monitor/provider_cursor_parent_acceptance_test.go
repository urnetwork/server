// Public custom crash signals cannot consume cursors for an ended parent.
package monitor

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Close synchronously ends the owner after every complete terminal byte was read.
type cursorParentBody struct {
	*bytes.Reader
	onClose  func()
	readErr  error
	closeErr error
}

// Cancellation-like child errors do not change the actual parent context.
func (self *cursorParentBody) Read(buffer []byte) (int, error) {
	if self.readErr != nil {
		return 0, self.readErr
	}
	return self.Reader.Read(buffer)
}

// There is no race, sleep or goroutine between complete read and cancellation.
func (self *cursorParentBody) Close() error {
	if self.onClose != nil {
		self.onClose()
	}
	return self.closeErr
}

// The entire envelope is captured, including all groups and processing markers.
type cursorParentSnapshot struct {
	exists bool
	data   []byte
	object any
}

// Directories and lock files may exist; only cursor JSON is acceptance evidence.
func snapshotCursorParent(t *testing.T, directory, key string) cursorParentSnapshot {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, "provider-reports", key+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return cursorParentSnapshot{}
	}
	if err != nil {
		t.Fatal("could not read complete synthetic cursor")
	}
	var object any
	if json.Unmarshal(data, &object) != nil {
		t.Fatal("synthetic cursor envelope is invalid")
	}
	return cursorParentSnapshot{exists: true, data: data, object: object}
}

// Comparing every byte plus the full object catches consumption beyond watermarks.
func requireCursorParentSnapshot(t *testing.T, before, after cursorParentSnapshot) {
	t.Helper()
	if before.exists != after.exists || !bytes.Equal(before.data, after.data) || !reflect.DeepEqual(before.object, after.object) {
		t.Fatal("ended parent changed the complete persistent crash cursor")
	}
}

// One memory-only source serves identical rejected and later accepted snapshots.
type cursorParentFixture struct {
	kind           string
	count          int
	stage          string
	empty          bool
	suppressed     bool
	childFailure   string
	cancel         context.CancelFunc
	terminalCloses int
	factoryCalls   int
	calls          map[string]int
}

// Every body is fully valid before Close cancellation is armed.
func (self *cursorParentFixture) response(t *testing.T, value any, data []byte, terminal bool) *http.Response {
	t.Helper()
	if data == nil {
		var err error
		data, err = json.Marshal(value)
		if err != nil {
			t.Fatal("could not encode synthetic provider response")
		}
	}
	body := &cursorParentBody{Reader: bytes.NewReader(data)}
	if terminal {
		body.onClose = func() {
			self.terminalCloses++
			if self.cancel != nil {
				self.cancel()
			}
		}
	}
	status := http.StatusOK
	switch self.childFailure {
	case "read":
		body.readErr = context.Canceled
	case "close":
		body.closeErr = context.DeadlineExceeded
	case "status":
		status = http.StatusUnauthorized
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}
}

// Exact methods/paths and complete protocol fixtures keep setup failures distinct.
func (self *cursorParentFixture) do(t *testing.T, request *http.Request) (*http.Response, error) {
	t.Helper()
	self.calls[request.Method+" "+request.URL.Path]++
	if self.childFailure == "do" {
		return nil, context.Canceled
	}
	if self.kind == "play" {
		prefix := "/v1beta1/apps/com.example.app/"
		switch request.Method + " " + request.URL.Path {
		case "GET " + prefix + "crashRateMetricSet":
			if self.empty {
				return self.response(t, map[string]any{}, nil, false), nil
			}
			return self.response(t, map[string]any{"freshnessInfo": map[string]any{"freshnesses": []any{map[string]any{
				"aggregationPeriod": "DAILY", "latestEndTime": map[string]any{"year": 2026, "month": 9, "day": 2, "timeZone": map[string]any{"id": "America/Los_Angeles"}},
			}}}}, nil, false), nil
		case "POST " + prefix + "crashRateMetricSet:query":
			return self.response(t, playMetricResponseFixture("2026-09-01", ""), nil, false), nil
		case "GET " + prefix + "errorIssues:search":
			if self.empty {
				return self.response(t, map[string]any{"errorIssues": []any{}}, nil, true), nil
			}
			return self.response(t, map[string]any{"errorIssues": []any{map[string]any{
				"name": "apps/com.example.app/issue-7", "type": "CRASH", "cause": "java.lang.IllegalStateException", "location": "network.ur.Main.start",
				"distinctUsers": "2", "errorReportCount": fmt.Sprint(self.count), "lastErrorReportTime": "2026-09-02T10:00:00Z",
				"issueUri": "https://play.google.com/console/issues/7?account=cursor-private", "lastAppVersion": map[string]any{"versionCode": "103"},
				"lastOsVersion": map[string]any{"apiLevel": "36"}, "sampleErrorReports": []string{"apps/com.example.app/report-9"},
			}}}, nil, false), nil
		case "GET " + prefix + "errorReports:search":
			return self.response(t, map[string]any{"errorReports": []any{map[string]any{
				"name": "apps/com.example.app/report-9", "type": "CRASH", "issue": "apps/com.example.app/issue-7", "eventTime": "2026-09-02T10:00:00Z",
				"reportText": "Fatal alice@example.com 123e4567-e89b-12d3-a456-426614174000 https://errors.example/trace?session=cursor-private",
				"appVersion": map[string]any{"versionCode": "103"}, "osVersion": map[string]any{"apiLevel": "36"}, "deviceModel": map[string]any{"marketingName": "Pixel Synthetic"},
			}}}, nil, true), nil
		}
	} else {
		instance, date, segment := "instance-old", "2026-09-01", "old"
		if self.stage == "current" {
			instance, date, segment = "instance-current", "2026-09-02", "current-a"
		}
		if self.stage == "correction" {
			instance, date, segment = "instance-correction", "2026-09-03", "correction"
		}
		if request.Method != http.MethodGet {
			t.Fatal("Apple source changed its method")
		}
		switch request.URL.Path {
		case "/v1/apps/6741000606/analyticsReportRequests":
			return self.response(t, map[string]any{"data": []any{map[string]any{"type": "analyticsReportRequests", "id": "request-1", "attributes": map[string]any{"accessType": "ONGOING", "stoppedDueToInactivity": false}}}}, nil, false), nil
		case "/v1/analyticsReportRequests/request-1/reports":
			return self.response(t, map[string]any{"data": []any{map[string]any{"type": "analyticsReports", "id": "crash-report", "attributes": map[string]any{"name": "App Crashes Expanded", "category": "PERFORMANCE"}}}}, nil, false), nil
		case "/v1/analyticsReports/crash-report/instances":
			if self.empty {
				return self.response(t, map[string]any{"data": []any{}}, nil, true), nil
			}
			return self.response(t, map[string]any{"data": []any{appleInstanceFixture(instance, date)}}, nil, false), nil
		case "/v1/analyticsReportInstances/" + instance + "/segments":
			return self.response(t, appleSegmentIDsFixture([]string{segment}), nil, false), nil
		case "/v1/analyticsReportSegments/segment-" + segment:
			detail := appleSegmentDetailFixture("https://reports.example.invalid", "segment-"+segment, false)
			data := self.appleData(t, segment)
			digest := md5.Sum(data) // #nosec G401 -- synthetic documented segment checksum.
			attributes := detail["data"].(map[string]any)["attributes"].(map[string]any)
			attributes["sizeInBytes"], attributes["checksum"] = len(data), hex.EncodeToString(digest[:])
			return self.response(t, detail, nil, false), nil
		case "/download/" + segment:
			if request.Header.Get("Authorization") != "" || request.Header.Get("Accept-Encoding") != "identity" {
				t.Fatal("segment auth/compression contract changed")
			}
			return self.response(t, nil, self.appleData(t, segment), true), nil
		}
	}
	t.Fatal("public crash signal used an unexpected synthetic method/path")
	return nil, errors.New("unexpected synthetic request")
}

// Empty published instances still have a valid complete checksum/gzip/TSV frame.
func (self *cursorParentFixture) appleData(t *testing.T, segment string) []byte {
	t.Helper()
	if self.suppressed {
		return appleGzipFixture("Date\tApp Name\tApp Apple Identifier\tApp Version\tDevice\tPlatform Version\tCrashes\tUnique Devices\n")
	}
	data, ok := appleFixtureSegment(segment)
	if !ok {
		t.Fatal("synthetic compressed segment is unavailable")
	}
	return data
}

// Factories inject an in-memory Doer without loading credentials or opening sockets.
func newCursorParentControl(t *testing.T, kind string) (*cursorParentFixture, Signal, SignalSettings) {
	t.Helper()
	fixture := &cursorParentFixture{kind: kind, count: 3, calls: map[string]int{}}
	client := newProviderHTTP(providerDoerFunc(func(request *http.Request) (*http.Response, error) { return fixture.do(t, request) }))
	client.wait = func(context.Context, time.Duration) error { return nil }
	now := time.Date(2026, 9, 2, 12, 34, 56, 0, time.UTC)
	if kind == "play" {
		signal := newPlayCrashesSignal("https://reports.example.invalid", func(context.Context, GooglePlayReportingSettings) (*providerHTTP, error) {
			fixture.factoryCalls++
			return client, nil
		})
		return fixture, signal, playCrashTestSettings(now, t.TempDir())
	}
	signal := newAppleCrashesSignal("https://reports.example.invalid", func(AppleReportingSettings, func() time.Time) (appleReportingClients, error) {
		fixture.factoryCalls++
		return appleReportingClients{api: client, download: client}, nil
	})
	return fixture, signal, appleCrashTestSettings(now, t.TempDir())
}

// Render public formats only; never expose the complete private cursor snapshot.
func requireCursorParentPrivacy(t *testing.T, alerts Alerts) {
	t.Helper()
	var jsonl bytes.Buffer
	if alerts.WriteJSONL(&jsonl) != nil {
		t.Fatal("could not encode synthetic alerts")
	}
	for _, alert := range alerts {
		for _, text := range []string{alert.Markdown(), jsonl.String()} {
			for _, forbidden := range []string{"alice@example.com", "123e4567-e89b-12d3-a456-426614174000", "session=cursor-private", "account=cursor-private", "signature=archive-secret"} {
				if strings.Contains(text, forbidden) {
					t.Fatal("public crash output exposed private fixture data")
				}
			}
		}
	}
}

// A real accepted public call seeds state; rejected source is replayed unchanged.
func exerciseCursorParentLateClose(t *testing.T, kind string, preexisting bool) {
	t.Helper()
	fixture, signal, settings := newCursorParentControl(t, kind)
	if preexisting {
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal("prior live crash report was not accepted")
		}
		class := "play-crash-issue"
		if kind == "apple" {
			class = "apple-crash-group"
		}
		requireAlertClass(t, alerts, class)
	}
	if kind == "play" {
		if preexisting {
			fixture.count = 4
		}
	} else {
		fixture.stage = "current"
	}
	// Prove this exact advancing payload valid without consuming the target cursor.
	controlSettings := settings
	controlSettings.StateDir = t.TempDir()
	controlAlerts, controlErr := signal.Run(context.Background(), controlSettings)
	if controlErr != nil {
		t.Fatal("advancing payload failed its independent live public control")
	}
	controlClass := "play-crash-issue"
	if kind == "apple" {
		controlClass = "apple-crash-group"
	}
	requireAlertClass(t, controlAlerts, controlClass)
	requireCursorParentPrivacy(t, controlAlerts)
	before := snapshotCursorParent(t, settings.StateDir, signal.Key())
	if before.exists != preexisting {
		t.Fatal("cursor prerequisite did not match prior acceptance")
	}
	closes := fixture.terminalCloses
	fixture.calls = map[string]int{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture.cancel = cancel
	alerts, err := signal.Run(ctx, settings)
	if fixture.terminalCloses != closes+1 || ctx.Err() != context.Canceled {
		t.Fatal("valid terminal body Close did not establish cancellation")
	}
	want := 4
	if kind == "apple" {
		want = 6
	}
	total := 0
	for _, n := range fixture.calls {
		total += n
		if n != 1 {
			t.Fatal("valid terminal source unexpectedly repeated or retried")
		}
	}
	if total != want {
		t.Fatal("public fixture did not complete every exact source path")
	}
	// File assertion first makes causal RED specifically about persistent acceptance.
	requireCursorParentSnapshot(t, before, snapshotCursorParent(t, settings.StateDir, signal.Key()))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatal("ended crash parent published a verdict")
	}
	fixture.cancel = nil
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal("live replay of rejected crash data failed")
	}
	class := "play-crash-issue"
	if kind == "apple" {
		class = "apple-crash-group"
		if preexisting {
			class = "apple-crash-correction"
		}
	}
	requireAlertClass(t, alerts, class)
	requireCursorParentPrivacy(t, alerts)
	if !snapshotCursorParent(t, settings.StateDir, signal.Key()).exists {
		t.Fatal("live accepted replay did not create its complete cursor")
	}
	closes = fixture.terminalCloses
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal("accepted identical replay failed")
	}
	for _, alert := range alerts {
		if alert.Class == "play-crash-issue" || alert.Class == "play-crash-correction" || alert.Class == "apple-crash-group" || alert.Class == "apple-crash-correction" {
			t.Fatal("identical accepted source did not deduplicate")
		}
	}
	if closes != fixture.terminalCloses {
		t.Fatal("deduplicated report downloaded another sample")
	}
	if kind == "play" {
		fixture.count = 2
	} else {
		fixture.stage = "correction"
	}
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal("valid later correction failed")
	}
	class = "play-crash-correction"
	if kind == "apple" {
		class = "apple-crash-correction"
	}
	requireAlertClass(t, alerts, class)
	if kind == "apple" {
		var accepted, afterOlder appleCrashState
		if _, err := loadProviderState(settings.StateDir, signal.Key(), appleCrashStateVersion, &accepted); err != nil {
			t.Fatal("could not inspect accepted Apple partitions")
		}
		fixture.stage = "old"
		alerts, err = signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal("late older instance was not handled")
		}
		for _, alert := range alerts {
			if alert.Class == "apple-crash-group" || alert.Class == "apple-crash-correction" {
				t.Fatal("late overlap rolled back accepted newer Apple partition")
			}
		}
		if _, err := loadProviderState(settings.StateDir, signal.Key(), appleCrashStateVersion, &afterOlder); err != nil {
			t.Fatal("could not inspect late overlap result")
		}
		if !reflect.DeepEqual(accepted.Partitions, afterOlder.Partitions) || accepted.LastProcessingDate != afterOlder.LastProcessingDate || accepted.LatestEmptyProcessingDate != afterOlder.LatestEmptyProcessingDate {
			t.Fatal("late overlap changed newer partition/visibility authority")
		}
	}
}

// All existing Play group/window/fingerprint fields must remain unchanged.
func TestPlayCrashCursorLateParentPreservesWholeState(t *testing.T) {
	exerciseCursorParentLateClose(t, "play", true)
}

// A first Play cursor JSON remains absent despite the existing lock directory.
func TestPlayCrashCursorLateParentCreatesNoFirstJson(t *testing.T) {
	exerciseCursorParentLateClose(t, "play", false)
}

// All Apple processed/partition/visibility members must remain unchanged.
func TestAppleCrashCursorLateParentPreservesWholeState(t *testing.T) {
	exerciseCursorParentLateClose(t, "apple", true)
}

// A first Apple cursor JSON cannot consume a rejected instance.
func TestAppleCrashCursorLateParentCreatesNoFirstJson(t *testing.T) {
	exerciseCursorParentLateClose(t, "apple", false)
}

// Cancellation-like child failures retain existing classes with a live parent.
func exerciseCursorParentChildFailure(t *testing.T, kind string) {
	t.Helper()
	for _, mode := range []string{"do", "read", "close", "status"} {
		fixture, signal, settings := newCursorParentControl(t, kind)
		fixture.childFailure = mode
		alerts, err := signal.Run(context.Background(), settings)
		var visibility *providerVisibilityError
		if err == nil || !errors.As(err, &visibility) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || len(alerts) != 0 {
			t.Fatal("live parent acquired a child cancellation verdict")
		}
		if snapshotCursorParent(t, settings.StateDir, signal.Key()).exists {
			t.Fatal("failed source created cursor JSON")
		}
		if mode == "do" {
			calls := 0
			for _, n := range fixture.calls {
				calls += n
			}
			if calls != providerRequestAttempts {
				t.Fatal("child source failure changed retry bounds")
			}
		}
		fixture.childFailure = ""
		alerts, err = signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal("live parent could not retry valid source")
		}
		class := "play-crash-issue"
		if kind == "apple" {
			class = "apple-crash-group"
		}
		requireAlertClass(t, alerts, class)
	}
}

// Play does not mistake a canceled child for the actual parent.
func TestPlayCrashCursorChildFailureKeepsLiveParent(t *testing.T) {
	exerciseCursorParentChildFailure(t, "play")
}

// Apple does not mistake a canceled child for the actual parent.
func TestAppleCrashCursorChildFailureKeepsLiveParent(t *testing.T) {
	exerciseCursorParentChildFailure(t, "apple")
}

// Empty provider visibility must also respect the final public emission boundary.
func exerciseCursorParentEmpty(t *testing.T, kind string) {
	t.Helper()
	fixture, signal, settings := newCursorParentControl(t, kind)
	fixture.empty = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture.cancel = cancel
	alerts, err := signal.Run(ctx, settings)
	if fixture.terminalCloses != 1 {
		t.Fatal("empty provider response did not reach Close")
	}
	requireCursorParentSnapshot(t, cursorParentSnapshot{}, snapshotCursorParent(t, settings.StateDir, signal.Key()))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatal("ended empty response emitted visibility or cursor")
	}
	fixture.cancel = nil
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal("live empty source visibility failed")
	}
	class := "play-crash-data-unobservable"
	if kind == "apple" {
		class = "apple-crash-data-unobservable"
	}
	requireAlertClass(t, alerts, class)
}

// Missing Play metric freshness is unknown rather than healthy zero.
func TestPlayCrashCursorEmptyVisibilityRespectsParent(t *testing.T) {
	exerciseCursorParentEmpty(t, "play")
}

// Missing Apple instances are unknown rather than healthy zero.
func TestAppleCrashCursorEmptyVisibilityRespectsParent(t *testing.T) {
	exerciseCursorParentEmpty(t, "apple")
}

// A known ended parent cannot enter source collection or cursor creation.
func TestCrashCursorAlreadyEndedParentDoesNotEnterProvider(t *testing.T) {
	for _, kind := range []string{"play", "apple"} {
		fixture, signal, settings := newCursorParentControl(t, kind)
		ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
		defer cancel()
		alerts, err := signal.Run(ctx, settings)
		if !errors.Is(err, context.DeadlineExceeded) || len(alerts) != 0 || len(fixture.calls) != 0 || fixture.factoryCalls != 0 || snapshotCursorParent(t, settings.StateDir, signal.Key()).exists {
			t.Fatal("ended parent entered custom provider acceptance")
		}
	}
}

// A real published empty instance retains privacy visibility and deduplicates IDs.
func TestAppleCrashCursorPublishedEmptyRespectsParentAndVisibility(t *testing.T) {
	fixture, signal, settings := newCursorParentControl(t, "apple")
	fixture.stage, fixture.suppressed = "current", true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture.cancel = cancel
	alerts, err := signal.Run(ctx, settings)
	if fixture.terminalCloses != 1 {
		t.Fatal("valid published empty instance did not complete its terminal download")
	}
	requireCursorParentSnapshot(t, cursorParentSnapshot{}, snapshotCursorParent(t, settings.StateDir, signal.Key()))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatal("ended empty instance published privacy/cursor state")
	}
	fixture.cancel = nil
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal("live empty instance failed")
	}
	requireAlertClass(t, alerts, "apple-crash-privacy-suppressed")
	closes := fixture.terminalCloses
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil || fixture.terminalCloses != closes {
		t.Fatal("accepted empty instance did not deduplicate its download")
	}
	requireAlertClass(t, alerts, "apple-crash-privacy-suppressed")
	fixture.stage, fixture.suppressed = "correction", false
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal("newer nonempty instance failed after privacy suppression")
	}
	requireAlertClass(t, alerts, "apple-crash-group")
	for _, alert := range alerts {
		if alert.Class == "apple-crash-privacy-suppressed" {
			t.Fatal("newer observed rows retained stale privacy suppression")
		}
	}
}
