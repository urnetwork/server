package monitor

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/md5"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
)

func TestAppleCrashesMissingCredentialNoops(t *testing.T) {
	created := 0
	signal := newAppleCrashesSignal("http://invalid", func(AppleReportingSettings, func() time.Time) (appleReportingClients, error) {
		created++
		return appleReportingClients{}, fmt.Errorf("must not run")
	})
	alerts, err := signal.Run(context.Background(), SignalSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 || created != 0 {
		t.Fatalf("missing credential ran provider: alerts=%v clients=%d", alerts, created)
	}
}

func TestAppleListResourcesRejectsProviderCardinalityBeyondBound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeTestJSON(t, writer, map[string]any{"data": []any{
			map[string]any{"type": "analyticsReportRequests", "id": "one"},
			map[string]any{"type": "analyticsReportRequests", "id": "two"},
		}})
	}))
	defer server.Close()
	_, err := appleListResources[appleReportRequest](context.Background(), newProviderHTTP(server.Client()), server.URL, server.URL, "synthetic resources", 1)
	if err == nil || !strings.Contains(err.Error(), "exceeded 1 resources") {
		t.Fatalf("resource cardinality error = %v", err)
	}
}

func TestAppleCrashCursorNeverRollsBackForLateOlderInstance(t *testing.T) {
	state := appleCrashState{
		LastProcessingDate: "2026-09-02",
		Partitions: map[string]appleCrashPartition{
			"2026-08-30": {
				ProcessingDate: "2026-09-02",
				Groups:         map[string]appleCrashGroup{"1.0": {Crashes: 5}},
			},
		},
	}
	state.initialize()
	touched := map[string]struct{}{}
	applyApplePartitions(&state, map[string]appleCrashPartition{
		"2026-08-30": {
			ProcessingDate: "2026-09-01",
			Groups:         map[string]appleCrashGroup{"1.0": {Crashes: 2}},
		},
	}, touched)
	if got := state.Partitions["2026-08-30"].Groups["1.0"].Crashes; got != 5 {
		t.Fatalf("late older instance rolled partition back to %d crashes", got)
	}
	if len(touched) != 0 {
		t.Fatalf("older partition marked touched: %v", touched)
	}

	recordAppleInstanceVisibility(&state, "2026-09-01", 0)
	if state.LatestEmptyProcessingDate != "" {
		t.Fatalf("older empty instance re-armed privacy marker: %q", state.LatestEmptyProcessingDate)
	}
	recordAppleInstanceVisibility(&state, "2026-09-03", 0)
	if state.LastProcessingDate != "2026-09-03" || state.LatestEmptyProcessingDate != "2026-09-03" {
		t.Fatalf("new empty instance was not recorded: %+v", state)
	}

	state.Partitions["2026-07-01"] = appleCrashPartition{ProcessingDate: "2026-07-02"}
	pruneAppleState(&state, time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC))
	if _, exists := state.Partitions["2026-07-01"]; exists {
		t.Fatal("old replacement partition was retained indefinitely")
	}
}

func TestAppleCrashesPaginatesRetriesReplacesCorrectionsAndOmitsDownloadAuth(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	includeCorrection := false
	badCorrectionChecksum := false
	requestAttempts := 0
	downloadCalls := 0
	apiCalls := 0
	key := appleSyntheticPrivateKey()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(request.URL.Path, "/download/") {
			downloadCalls++
			if authorization := request.Header.Get("Authorization"); authorization != "" {
				t.Errorf("signed segment download received Authorization %q", authorization)
			}
			if request.Header.Get("Accept-Encoding") != "identity" {
				t.Errorf("signed segment requested Accept-Encoding %q", request.Header.Get("Accept-Encoding"))
			}
			segmentID := strings.TrimPrefix(request.URL.Path, "/download/")
			data, ok := appleFixtureSegment(segmentID)
			if !ok {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			writer.Header().Set("Content-Type", "application/gzip")
			_, _ = writer.Write(data)
			return
		}

		apiCalls++
		authorization := request.Header.Get("Authorization")
		if !strings.HasPrefix(authorization, "Bearer ") {
			t.Errorf("API request has no bearer token: %q", authorization)
		}
		switch request.URL.Path {
		case "/v1/apps/6741000606/analyticsReportRequests":
			requestAttempts++
			if requestAttempts == 1 {
				writer.Header().Set("Retry-After", "0")
				writer.WriteHeader(http.StatusTooManyRequests)
				return
			}
			if request.URL.Query().Get("cursor") == "" {
				writeTestJSON(t, writer, map[string]any{
					"data": []any{}, "links": map[string]any{"next": server.URL + request.URL.Path + "?cursor=request-next"},
				})
				return
			}
			writeTestJSON(t, writer, map[string]any{"data": []any{map[string]any{
				"type": "analyticsReportRequests", "id": "request-1",
				"attributes": map[string]any{"accessType": "ONGOING", "stoppedDueToInactivity": false},
			}}, "links": map[string]any{}})
		case "/v1/analyticsReportRequests/request-1/reports":
			if request.URL.Query().Get("cursor") == "" {
				writeTestJSON(t, writer, map[string]any{
					"data":  []any{map[string]any{"type": "analyticsReports", "id": "sessions", "attributes": map[string]any{"name": "App Sessions", "category": "APP_USAGE"}}},
					"links": map[string]any{"next": server.URL + request.URL.Path + "?cursor=report-next"},
				})
				return
			}
			writeTestJSON(t, writer, map[string]any{"data": []any{map[string]any{
				"type": "analyticsReports", "id": "crash-report", "attributes": map[string]any{"name": "App Crashes Expanded", "category": "PERFORMANCE"},
			}}, "links": map[string]any{}})
		case "/v1/analyticsReports/crash-report/instances":
			instances := []any{
				appleInstanceFixture("instance-old", "2026-09-01"),
				appleInstanceFixture("instance-current", "2026-09-02"),
			}
			if includeCorrection {
				instances = append(instances, appleInstanceFixture("instance-correction", "2026-09-03"))
			}
			writeTestJSON(t, writer, map[string]any{"data": instances, "links": map[string]any{}})
		case "/v1/analyticsReportInstances/instance-old/segments":
			writeTestJSON(t, writer, appleSegmentsFixture(server.URL, []string{"old"}, false))
		case "/v1/analyticsReportInstances/instance-current/segments":
			writeTestJSON(t, writer, appleSegmentsFixture(server.URL, []string{"current-a", "current-b"}, false))
		case "/v1/analyticsReportInstances/instance-correction/segments":
			writeTestJSON(t, writer, appleSegmentsFixture(server.URL, []string{"correction"}, badCorrectionChecksum))
		default:
			t.Errorf("unexpected API request %s", request.URL.String())
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	apiClient := newProviderHTTP(&appleAuthDoer{
		client: server.Client(), key: key, issuerID: "issuer-1", keyID: "key-1", now: func() time.Time { return now },
	})
	apiClient.wait = func(context.Context, time.Duration) error { return nil }
	downloadClient := newProviderHTTP(server.Client())
	downloadClient.wait = func(context.Context, time.Duration) error { return nil }
	signal := newAppleCrashesSignal(server.URL, func(AppleReportingSettings, func() time.Time) (appleReportingClients, error) {
		return appleReportingClients{api: apiClient, download: downloadClient}, nil
	})
	settings := appleCrashTestSettings(now, t.TempDir())
	settings.Now = func() time.Time { return now }

	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "apple-crash-group")
	if !strings.Contains(alert.Observed, "crashes=5") || strings.Contains(alert.Observed, "crashes=8") {
		t.Fatalf("Apple instances were summed instead of replaced: %s", alert.Markdown())
	}
	for _, want := range []string{"date=2026-08-31 version=1.2.3", "iPhone:4", "iPad:1", "iOS 19.0:4"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Errorf("Apple Markdown missing %q:\n%s", want, alert.Markdown())
		}
	}
	if requestAttempts != 3 {
		t.Fatalf("request retry/pagination attempts=%d, want 3", requestAttempts)
	}
	firstDownloads := downloadCalls
	if firstDownloads != 3 {
		t.Fatalf("downloaded segments=%d, want all 3", firstDownloads)
	}

	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 || downloadCalls != firstDownloads {
		t.Fatalf("processed instances were not deduplicated: alerts=%+v downloads=%d", alerts, downloadCalls)
	}

	includeCorrection = true
	badCorrectionChecksum = true
	now = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	_, err = signal.Run(context.Background(), settings)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("bad correction checksum error = %v", err)
	}
	if strings.Contains(err.Error(), "signature=archive-secret") {
		t.Fatalf("signed URL leaked in checksum error: %v", err)
	}

	badCorrectionChecksum = false
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	corrected := requireAlertClass(t, alerts, "apple-crash-correction")
	if !strings.Contains(corrected.Observed, "previous={crashes=5") || !strings.Contains(corrected.Observed, "current={crashes=2") {
		t.Fatalf("replacement correction missing exact before/after: %s", corrected.Markdown())
	}
	if apiCalls == 0 {
		t.Fatal("synthetic API was never called")
	}
}

func TestAppleCrashesPrivacySuppressedInstanceRemainsObservable(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	headerOnly := appleGzipFixture("Date\tApp Name\tApp Apple Identifier\tApp Version\tDevice\tPlatform Version\tCrashes\tUnique Devices\n")
	checksum := md5.Sum(headerOnly) // #nosec G401 -- mirrors Apple's documented report checksum.
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/apps/6741000606/analyticsReportRequests":
			writeTestJSON(t, writer, map[string]any{"data": []any{map[string]any{
				"type": "analyticsReportRequests", "id": "request-1", "attributes": map[string]any{"accessType": "ONGOING", "stoppedDueToInactivity": false},
			}}})
		case "/v1/analyticsReportRequests/request-1/reports":
			writeTestJSON(t, writer, map[string]any{"data": []any{map[string]any{
				"type": "analyticsReports", "id": "crash-report", "attributes": map[string]any{"name": "App Crashes", "category": "APP_USAGE"},
			}}})
		case "/v1/analyticsReports/crash-report/instances":
			writeTestJSON(t, writer, map[string]any{"data": []any{appleInstanceFixture("empty-instance", "2026-09-02")}})
		case "/v1/analyticsReportInstances/empty-instance/segments":
			writeTestJSON(t, writer, map[string]any{"data": []any{map[string]any{
				"type": "analyticsReportSegments", "id": "empty-segment", "attributes": map[string]any{
					"checksum": hex.EncodeToString(checksum[:]), "sizeInBytes": len(headerOnly), "url": server.URL + "/empty-download?signature=private",
				},
			}}})
		case "/empty-download":
			if request.Header.Get("Authorization") != "" {
				t.Errorf("Authorization forwarded to signed URL")
			}
			writer.Header().Set("Content-Type", "application/gzip")
			_, _ = writer.Write(headerOnly)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := newProviderHTTP(server.Client())
	signal := newAppleCrashesSignal(server.URL, func(AppleReportingSettings, func() time.Time) (appleReportingClients, error) {
		return appleReportingClients{api: client, download: client}, nil
	})
	settings := appleCrashTestSettings(now, t.TempDir())
	for run := 1; run <= 2; run++ {
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "apple-crash-privacy-suppressed")
		if !strings.Contains(alert.Mechanism, "not be interpreted as zero") {
			t.Fatalf("empty Apple result lost privacy qualification: %s", alert.Markdown())
		}
	}
}

func TestAppleCrashesMissingOngoingRequestIsStructured(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writeTestJSON(t, writer, map[string]any{"data": []any{}})
	}))
	defer server.Close()
	client := newProviderHTTP(server.Client())
	signal := newAppleCrashesSignal(server.URL, func(AppleReportingSettings, func() time.Time) (appleReportingClients, error) {
		return appleReportingClients{api: client, download: client}, nil
	})
	alerts, err := signal.Run(context.Background(), appleCrashTestSettings(now, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "apple-crash-request-missing")
	if !strings.Contains(alert.Action, "Admin") || !strings.Contains(alert.Action, "Sales and Reports") {
		t.Fatalf("request setup ownership is incomplete: %s", alert.Markdown())
	}
}

func TestAppleReportingJWTClaimsAndFactory(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	key := appleSyntheticPrivateKey()
	tokenString, err := appleReportingJWT(key, "issuer-1", "key-1", now)
	if err != nil {
		t.Fatal(err)
	}
	parser := gojwt.NewParser(gojwt.WithTimeFunc(func() time.Time { return now }))
	token, err := parser.Parse(tokenString, func(token *gojwt.Token) (any, error) {
		if token.Method != gojwt.SigningMethodES256 {
			t.Errorf("JWT method = %v", token.Method)
		}
		return &key.PublicKey, nil
	})
	if err != nil || !token.Valid {
		t.Fatalf("parse signed token: %v", err)
	}
	claims := token.Claims.(gojwt.MapClaims)
	if claims["iss"] != "issuer-1" || claims["aud"] != "appstoreconnect-v1" || tokenKeyID(token) != "key-1" {
		t.Fatalf("unexpected JWT claims/header: %#v %#v", claims, token.Header)
	}
	iat, _ := claims.GetIssuedAt()
	exp, _ := claims.GetExpirationTime()
	if iat == nil || exp == nil || exp.Time.Sub(iat.Time) != 15*time.Minute {
		t.Fatalf("JWT lifetime iat=%v exp=%v", iat, exp)
	}

	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
	clients, err := newAppleReportingClients(AppleReportingSettings{
		IssuerID: "issuer-1", KeyID: "key-1", PrivateKey: string(privatePEM),
	}, func() time.Time { return now })
	if err != nil || clients.api == nil || clients.download == nil {
		t.Fatalf("factory clients=%+v err=%v", clients, err)
	}
}

func TestParseAppleCrashTSVRejectsMalformedAndForeignRows(t *testing.T) {
	base := "Date\tApp Name\tApp Apple Identifier\tApp Version\tDevice\tPlatform Version\tCrashes\tUnique Devices\n"
	for name, data := range map[string]string{
		"missing column": "Date\tApp Apple Identifier\tCrashes\n2026-09-01\t6741000606\t1\n",
		"foreign app":    base + "2026-09-01\tUR\t999\t1.0\tiPhone\tiOS 19\t1\t1\n",
		"invalid count":  base + "2026-09-01\tUR\t6741000606\t1.0\tiPhone\tiOS 19\tnan\t1\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseAppleCrashTSV([]byte(data), "6741000606", "2026-09-02", map[string]appleCrashPartition{}, appleCrashRowsPerInstance)
			if err == nil {
				t.Fatal("malformed report was accepted")
			}
		})
	}
}

func appleCrashTestSettings(now time.Time, stateDir string) SignalSettings {
	return SignalSettings{
		Environment: "synthetic", StateDir: stateDir, Now: func() time.Time { return now },
		AppleReporting: AppleReportingSettings{
			Enabled: true, AppID: "6741000606", IssuerID: "issuer-1", KeyID: "key-1", PrivateKey: "synthetic",
		},
	}
}

func appleSyntheticPrivateKey() *ecdsa.PrivateKey {
	curve := elliptic.P256()
	d := big.NewInt(1)
	x, y := curve.ScalarBaseMult(d.Bytes())
	return &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y}, D: d}
}

func appleInstanceFixture(id, processingDate string) map[string]any {
	return map[string]any{
		"type": "analyticsReportInstances", "id": id,
		"attributes": map[string]any{"granularity": "DAILY", "processingDate": processingDate},
	}
}

func appleSegmentsFixture(baseURL string, ids []string, badChecksum bool) map[string]any {
	data := make([]any, 0, len(ids))
	for _, id := range ids {
		compressed, _ := appleFixtureSegment(id)
		digest := md5.Sum(compressed) // #nosec G401 -- mirrors Apple's documented report checksum.
		checksum := hex.EncodeToString(digest[:])
		if badChecksum {
			checksum = strings.Repeat("0", md5.Size*2)
		}
		data = append(data, map[string]any{
			"type": "analyticsReportSegments", "id": "segment-" + id,
			"attributes": map[string]any{
				"checksum": checksum, "sizeInBytes": len(compressed), "url": baseURL + "/download/" + id + "?signature=archive-secret",
			},
		})
	}
	return map[string]any{"data": data, "links": map[string]any{}}
}

func appleFixtureSegment(id string) ([]byte, bool) {
	header := "Date\tApp Name\tApp Apple Identifier\tApp Version\tDevice\tPlatform Version\tCrashes\tUnique Devices\n"
	rows := map[string]string{
		"old":        "2026-08-31\tURnetwork\t6741000606\t1.2.3\tiPhone\tiOS 19.0\t3\t3\n",
		"current-a":  "2026-08-31\tURnetwork\t6741000606\t1.2.3\tiPhone\tiOS 19.0\t4\t4\n",
		"current-b":  "2026-08-31\tURnetwork\t6741000606\t1.2.3\tiPad\tiPadOS 19.0\t1\t1\n",
		"correction": "2026-08-31\tURnetwork\t6741000606\t1.2.3\tiPhone\tiOS 19.0\t2\t2\n",
	}
	row, exists := rows[id]
	if !exists {
		return nil, false
	}
	return appleGzipFixture(header + row), true
}

func appleGzipFixture(value string) []byte {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	writer.Header.ModTime = time.Unix(0, 0).UTC()
	_, _ = writer.Write([]byte(value))
	_ = writer.Close()
	return buffer.Bytes()
}
