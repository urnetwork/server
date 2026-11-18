package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/lagdrift"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// Tests of the drift test's routes (lag.dev/DRIFTTEST.md P, E1-E3, K3, K5,
// W8, T4): the Embed gate, the token, its runs and their limits, the binding
// to client_ip, origins and the demo token. Processing and verdicts are in
// lag_drift_process_test.go.

// Where the fixture's extenders are, around a visitor in Berlin: near, mid
// and far bands, and one beyond them (E2).
var lagDriftTestCities = []struct {
	city        string
	countryCode string
	latitude    float64
	longitude   float64
}{
	{"Berlin", "de", 52.52, 13.40},
	{"Leipzig", "de", 51.34, 12.37},
	{"Hamburg", "de", 53.55, 10.00},
	{"Dresden", "de", 51.05, 13.74},
	{"Szczecin", "pl", 53.43, 14.55},
	{"Prague", "cz", 50.08, 14.43},
	{"Warsaw", "pl", 52.23, 21.01},
	{"Stockholm", "se", 59.33, 18.07},
	{"Paris", "fr", 48.86, 2.35},
	{"London", "gb", 51.50, -0.12},
	{"Rome", "it", 41.90, 12.50},
	{"Kyiv", "ua", 50.45, 30.52},
	{"Madrid", "es", 40.42, -3.70},
	{"Lisbon", "pt", 38.72, -9.14},
	{"Cairo", "eg", 30.04, 31.24},
	{"Dubai", "ae", 25.20, 55.27},
	{"Lagos", "ng", 6.52, 3.38},
	{"New York", "us", 40.71, -74.00},
	{"Mumbai", "in", 19.08, 72.88},
	{"Toronto", "ca", 43.65, -79.38},
	{"Sydney", "au", -33.87, 151.21},
}

// The visitors' addresses: a Berlin block and a Lyon block of the hand-built
// GeoLite2 City database, and a Berlin block of IPv6.
const (
	lagDriftTestVisitorIp       = "198.51.100.30"
	lagDriftTestVisitorAddress  = "198.51.100.30:52000"
	lagDriftTestNeighborAddress = "198.51.100.99:52000"
	lagDriftTestLyonAddress     = "203.0.113.7:52000"
	lagDriftTestVisitorIpv6     = "[2001:db8:1::30]:52000"
	// IPv6 from France, as a dual-stack visitor's on a VPN in Germany over
	// IPv4 only
	lagDriftTestLyonIpv6 = "[2001:db8:2::7]:52000"
	// Africa, whose k at 99% is above the textbook's (V11)
	lagDriftTestCairoIp      = "100.64.20.5"
	lagDriftTestCairoAddress = "100.64.20.5:52000"
	// a German city the place list has no point for
	lagDriftTestLeipzigIp      = "100.64.30.5"
	lagDriftTestLeipzigAddress = "100.64.30.5:52000"
)

var lagDriftTestLeipzigBlock = lagTestCityBlock{
	countryCode: "de", country: "Germany", countryGeonameId: 2921044,
	region: "Saxony", regionGeonameId: 2842566,
	city: "Leipzig", cityGeonameId: 2879139,
	latitude: 51.34, longitude: 12.37,
}

var lagDriftTestCairoBlock = lagTestCityBlock{
	countryCode: "eg", country: "Egypt", countryGeonameId: 357994,
	region: "Cairo Governorate", regionGeonameId: 360631,
	city: "Cairo", cityGeonameId: 360630,
	latitude: 30.06, longitude: 31.25,
}

var lagTestLyon = lagTestCityBlock{
	countryCode: "fr", country: "France", countryGeonameId: 3017382,
	region: "Auvergne-Rhone-Alpes", regionGeonameId: 11071625,
	city: "Lyon", cityGeonameId: 2996944,
	latitude: 45.76, longitude: 4.84,
}

// Square borders for the countries the verdicts judge (G1): Germany, France
// and Poland, each a box around the real one, so Berlin is inside Germany
// and Madrid far outside it.
func lagDriftTestBorders(t testing.TB) *lagdrift.Borders {
	t.Helper()
	box := func(code string, minLat float64, minLon float64, maxLat float64, maxLon float64) lagdrift.Country {
		return lagdrift.Country{
			Code: code,
			Polygons: [][][]lagdrift.Point{{{
				{Lat: minLat, Lon: minLon},
				{Lat: minLat, Lon: maxLon},
				{Lat: maxLat, Lon: maxLon},
				{Lat: maxLat, Lon: minLon},
			}}},
		}
	}
	countries := []lagdrift.Country{
		box("de", 47.3, 5.9, 55.0, 15.0),
		box("fr", 42.3, -4.8, 51.1, 5.9),
		box("pl", 49.0, 15.0, 54.8, 24.1),
	}
	// written and read back as the deployment's file is (G1)
	var file bytes.Buffer
	if err := lagdrift.WriteBorders(&file, "ne-admin0-10m-test", countries); err != nil {
		t.Fatal(err)
	}
	borders, err := lagdrift.ReadBorders(&file)
	if err != nil {
		t.Fatal(err)
	}
	return borders
}

// One drift test environment: the extender config, the extenders, the
// visitor's GeoLite2 and place list, the borders, and an Embed network with
// its credential.
type lagDriftTestFixture struct {
	ctx            context.Context
	settings       *LagSettings
	rootPublicKey  ed25519.PublicKey
	networkId      server.Id
	networkSession *session.ClientSession
	// by extender id
	extenders map[server.Id]*lagTestExtender
	places    map[server.Id]*model.LagPlace
}

func newLagDriftTestFixture(t testing.TB, ctx context.Context) *lagDriftTestFixture {
	t.Helper()
	fixture := &lagDriftTestFixture{
		ctx:       ctx,
		settings:  DefaultLagSettings(),
		extenders: map[server.Id]*lagTestExtender{},
		places:    map[server.Id]*model.LagPlace{},
	}
	t.Cleanup(Testing_SetLagSettings(fixture.settings))
	fixture.rootPublicKey = installLagTestExtenderConfig(t, true)
	addressPlaces := map[string]*model.LagPlace{}
	useLagAddressPlaces(t, addressPlaces)
	for i, city := range lagDriftTestCities {
		place := lagTestPlace(city.city, city.countryCode, city.latitude, city.longitude)
		extender := newLagServingTestExtender(t, ctx, addressPlaces, place, fmt.Sprintf("192.0.2.%d", 10+i))
		fixture.extenders[extender.extenderId] = extender
		fixture.places[extender.extenderId] = place
	}
	useLagTestCityDb(t, map[string]lagTestCityBlock{
		"198.51.100.0/24": lagTestBerlinNorth,
		"203.0.113.0/24":  lagTestLyon,
		"2001:db8:1::/48": lagTestBerlinNorth,
		"2001:db8:2::/48": lagTestLyon,
		"100.64.30.0/24":  lagDriftTestLeipzigBlock,
		"100.64.20.0/24":  lagDriftTestCairoBlock,
	})
	t.Cleanup(model.Testing_PushPlaces(lagTestPlaces))
	t.Cleanup(Testing_SetLagDriftBorders(lagDriftTestBorders(t)))

	fixture.networkId = server.NewId()
	userId := server.NewId()
	model.Testing_CreateNetwork(ctx, fixture.networkId, "drift-test-"+fixture.networkId.String()[:8], userId)
	model.Testing_EnableNetworkEmbed(ctx, fixture.networkId)
	fixture.networkSession = session.NewLocalClientSession(
		ctx,
		"203.0.113.200:443",
		session.NewByJwt(fixture.networkId, userId, "drift-test-network", false, false),
	)
	t.Cleanup(fixture.networkSession.Cancel)
	return fixture
}

// The widget's session from an address: no credential (P1).
func (self *lagDriftTestFixture) widgetSession(t testing.TB, clientAddress string) *session.ClientSession {
	t.Helper()
	clientSession := session.NewLocalClientSession(self.ctx, clientAddress, nil)
	t.Cleanup(clientSession.Cancel)
	return clientSession
}

// Allocates a drift test for the visitor's address and answers the token.
func (self *lagDriftTestFixture) allocate(t testing.TB, now time.Time, args *LagDriftTestArgs) *LagDriftTestResult {
	t.Helper()
	if args == nil {
		args = &LagDriftTestArgs{ClientIp: lagDriftTestVisitorIp}
	}
	result, err := lagDriftTestAllocate(args, self.networkSession, now, self.settings)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// Starts a run as the frame does. A pin to client_ip's family is followed as
// when the pinned origin can't be reached: the same start once more, sent
// unpinned (P5).
func (self *lagDriftTestFixture) start(t testing.TB, token string, clientAddress string, parentOrigin string, now time.Time) *LagDriftStartResult {
	t.Helper()
	args := &LagDriftStartArgs{Token: token, ParentOrigin: parentOrigin}
	for {
		result, err := lagDriftTestStart(args, self.widgetSession(t, clientAddress), now, self.settings)
		if err != nil {
			t.Fatal(err)
		}
		if result.Pin == "" || args.Unpinned {
			return result
		}
		args.Unpinned = true
	}
}

func (self *lagDriftTestFixture) finish(t testing.TB, token string, run int, stop string, now time.Time) *LagDriftFinishResult {
	t.Helper()
	result, err := lagDriftTestFinish(
		&LagDriftFinishArgs{Token: token, Run: run, Stop: stop},
		self.widgetSession(t, lagDriftTestVisitorAddress),
		now,
		self.settings,
	)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (self *lagDriftTestFixture) widgetResult(t testing.TB, token string, now time.Time) *LagDriftWidgetResult {
	t.Helper()
	result, err := lagDriftTestResult(
		&LagDriftResultArgs{Token: token},
		self.widgetSession(t, lagDriftTestVisitorAddress),
		now,
		self.settings,
	)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (self *lagDriftTestFixture) read(t testing.TB, driftTestId server.Id, now time.Time) *LagDriftTestView {
	t.Helper()
	view, err := lagDriftTestGet(
		&LagDriftTestGetArgs{DriftTestId: driftTestId.String()},
		self.networkSession,
		now,
		self.settings,
	)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func assertLagDriftRefused(t testing.TB, lagError *LagError, code string) {
	t.Helper()
	if lagError == nil || lagError.Code != code {
		t.Fatalf("want a %s refusal, got %+v", code, lagError)
	}
}

// A network route's refusal: its status and its code.
func assertLagDriftStatus(t testing.TB, err error, status int, code string) *lagDriftStatusError {
	t.Helper()
	var statusError *lagDriftStatusError
	if !errors.As(err, &statusError) || statusError.status != status || statusError.lagError.Code != code {
		t.Fatalf("want a %d %s, got %v", status, code, err)
	}
	return statusError
}

func assertLagDrift400(t testing.TB, err error) {
	t.Helper()
	assertLagDriftStatus(t, err, http.StatusBadRequest, LagDriftErrorCodeInvalid)
}

// The drift routes are Embed APIs (decision 3, P1): a network that never had
// Embed, or had it disabled, is refused with 403 and the Embed message before
// any other work, a client token is refused with 403, and an Embed network
// allocates. Another network's test is a 404.
func TestLagDriftEmbedGate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc()

		allocated := fixture.allocate(t, now, nil)

		// never enabled: refused first, even for a request that would be a 400
		otherNetworkId := server.NewId()
		otherSession := session.NewLocalClientSession(ctx, "203.0.113.201:443", session.NewByJwt(otherNetworkId, server.NewId(), "other", false, false))
		defer otherSession.Cancel()
		_, err := lagDriftTestAllocate(&LagDriftTestArgs{}, otherSession, now, fixture.settings)
		refused := assertLagDriftStatus(t, err, http.StatusForbidden, LagDriftErrorCodeEmbed)
		connect.AssertEqual(t, refused.lagError.Message, model.NetworkEmbedNotEnabledMessage)
		_, err = lagDriftTestGet(&LagDriftTestGetArgs{DriftTestId: allocated.DriftTestId.String()}, otherSession, now, fixture.settings)
		assertLagDriftStatus(t, err, http.StatusForbidden, LagDriftErrorCodeEmbed)

		// disabled
		connect.AssertEqual(t, model.DisableNetworkEmbed(ctx, fixture.networkId), nil)
		_, err = lagDriftTestAllocate(&LagDriftTestArgs{ClientIp: lagDriftTestVisitorIp}, fixture.networkSession, now, fixture.settings)
		assertLagDriftStatus(t, err, http.StatusForbidden, LagDriftErrorCodeEmbed)
		_, err = lagDriftTestGet(&LagDriftTestGetArgs{DriftTestId: allocated.DriftTestId.String()}, fixture.networkSession, now, fixture.settings)
		assertLagDriftStatus(t, err, http.StatusForbidden, LagDriftErrorCodeEmbed)
		// and its tokens start no more runs (P3)
		assertLagDriftRefused(t, fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", now).Error, LagDriftErrorCodeExpired)
		model.Testing_EnableNetworkEmbed(ctx, fixture.networkId)
		connect.AssertEqual(t, fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", now).Error, (*LagError)(nil))

		// a client token of the Embed network
		clientSession := session.NewLocalClientSession(ctx, "203.0.113.202:443",
			session.NewByJwt(fixture.networkId, server.NewId(), "client", false, false).Client(server.NewId(), server.NewId()))
		defer clientSession.Cancel()
		_, err = lagDriftTestAllocate(&LagDriftTestArgs{ClientIp: lagDriftTestVisitorIp}, clientSession, now, fixture.settings)
		assertLagDriftStatus(t, err, http.StatusForbidden, LagDriftErrorCodeClientToken)
		_, err = lagDriftTestGet(&LagDriftTestGetArgs{DriftTestId: allocated.DriftTestId.String()}, clientSession, now, fixture.settings)
		assertLagDriftStatus(t, err, http.StatusForbidden, LagDriftErrorCodeClientToken)

		// another Embed network can't read this one's test, nor anyone an id
		// that names none
		model.Testing_EnableNetworkEmbed(ctx, otherNetworkId)
		_, err = lagDriftTestGet(&LagDriftTestGetArgs{DriftTestId: allocated.DriftTestId.String()}, otherSession, now, fixture.settings)
		assertLagDriftStatus(t, err, http.StatusNotFound, LagDriftErrorCodeNotFound)
		_, err = lagDriftTestGet(&LagDriftTestGetArgs{DriftTestId: server.NewId().String()}, fixture.networkSession, now, fixture.settings)
		assertLagDriftStatus(t, err, http.StatusNotFound, LagDriftErrorCodeNotFound)
		_, err = lagDriftTestGet(&LagDriftTestGetArgs{DriftTestId: "nope"}, fixture.networkSession, now, fixture.settings)
		assertLagDrift400(t, err)
	})
}

// A network route's refusal reaches the caller as its status with the refusal
// as a JSON body, `{"error": {"code", "message"}}`, and a limit adds
// retry_after_ms and Retry-After in whole seconds, rounded up (P5). Pure.
func TestLagDriftStatusResponses(t *testing.T) {
	respond := func(err error) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		router.RaiseHttpError(err, recorder)
		return recorder
	}
	body := func(recorder *httptest.ResponseRecorder) *LagError {
		var parsed struct {
			Error *LagError `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &parsed); err != nil {
			t.Fatalf("not a json refusal: %q", recorder.Body.String())
		}
		return parsed.Error
	}

	recorder := respond(lagDriftInvalid("client_ip is required."))
	connect.AssertEqual(t, recorder.Code, http.StatusBadRequest)
	connect.AssertEqual(t, recorder.Header().Get("Content-Type"), "application/json")
	connect.AssertEqual(t, body(recorder).Code, LagDriftErrorCodeInvalid)
	connect.AssertEqual(t, body(recorder).Message, "client_ip is required.")

	recorder = respond(&lagDriftStatusError{
		status:   http.StatusForbidden,
		lagError: lagDriftRefusal(LagDriftErrorCodeEmbed, model.NetworkEmbedNotEnabledMessage),
	})
	connect.AssertEqual(t, recorder.Code, http.StatusForbidden)
	connect.AssertEqual(t, body(recorder).Code, LagDriftErrorCodeEmbed)
	connect.AssertEqual(t, body(recorder).Message, model.NetworkEmbedNotEnabledMessage)

	limited := lagDriftLimited(model.LagRateLimitResult{RetryAfter: 2100 * time.Millisecond}, "Too many.")
	recorder = respond(&lagDriftStatusError{status: http.StatusTooManyRequests, lagError: limited})
	connect.AssertEqual(t, recorder.Code, http.StatusTooManyRequests)
	connect.AssertEqual(t, recorder.Header().Get("Retry-After"), "3")
	connect.AssertEqual(t, body(recorder).Code, LagErrorCodeLimit)
	connect.AssertEqual(t, body(recorder).RetryAfterMs, int64(2100))
}

// An allocation answers a token of 32 random bytes, stored only as its
// SHA-256 (P2, K5), and the visitor's address only as the peppered hash of
// its /29 (K3), with the defaults and the bounds of P2.
func TestLagDriftAllocate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)

		result := fixture.allocate(t, now, &LagDriftTestArgs{ClientIp: lagDriftTestVisitorIp, Reference: "signup-8812"})
		if !strings.HasPrefix(result.Token, "dt1_") || len(result.Token) != 47 {
			t.Fatalf("token %q is not dt1_ and 43 characters", result.Token)
		}
		tokenBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(result.Token, "dt1_"))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(tokenBytes), 32)
		connect.AssertEqual(t, result.EmbedUrl, "https://lag.dev/?t="+result.Token)
		expiresAt, err := time.Parse(time.RFC3339, result.ExpiresAt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, expiresAt.Equal(now.Add(300*time.Second).Truncate(time.Second)), true)

		test := model.GetLagDriftTest(ctx, result.DriftTestId)
		digest := sha256.Sum256([]byte(result.Token))
		connect.AssertEqual(t, test.TokenHash, digest[:])
		connect.AssertEqual(t, test.NetworkId, fixture.networkId)
		connect.AssertEqual(t, test.Confidence, 0.99)
		connect.AssertEqual(t, test.Reference, "signup-8812")
		connect.AssertEqual(t, test.ClientIpFamily, 4)
		// the binding is to the /29, by the server's peppered hash (K3)
		clientIpHash := server.ClientIpHashForAddr(netip.MustParseAddr(lagDriftTestVisitorIp))
		connect.AssertEqual(t, test.ClientIpHash, clientIpHash[:])
		// the token and the address are in no table
		tableText := lagTestTableText(t, ctx, "lag_drift_test")
		for _, secret := range []string{result.Token, strings.TrimPrefix(result.Token, "dt1_"), lagDriftTestVisitorIp} {
			if strings.Contains(tableText, secret) {
				t.Fatalf("lag_drift_test holds %q", secret)
			}
		}

		read := fixture.read(t, result.DriftTestId, now)
		connect.AssertEqual(t, read.State, LagDriftStateCreated)
		connect.AssertEqual(t, read.Reference, "signup-8812")
		connect.AssertEqual(t, read.Result, (*LagDriftResult)(nil))
		connect.AssertEqual(t, len(read.Runs), 0)
		// a token never started expires
		connect.AssertEqual(t, fixture.read(t, result.DriftTestId, expiresAt).State, LagDriftStateExpired)

		// the options
		confidence := 0.95
		ttlSeconds := 60
		result = fixture.allocate(t, now, &LagDriftTestArgs{
			ClientIp:      " 2001:db8:1::30 ",
			ParentOrigins: []string{"https://Example.com:443/", "https://example.com", "http://localhost:3000"},
			Confidence:    &confidence,
			TtlSeconds:    &ttlSeconds,
		})
		test = model.GetLagDriftTest(ctx, result.DriftTestId)
		connect.AssertEqual(t, test.Confidence, 0.95)
		connect.AssertEqual(t, test.ClientIpFamily, 6)
		connect.AssertEqual(t, test.ParentOrigins, []string{"https://example.com", "http://localhost:3000"})
		expiresAt, _ = time.Parse(time.RFC3339, result.ExpiresAt)
		connect.AssertEqual(t, expiresAt.Equal(now.Add(time.Minute).Truncate(time.Second)), true)

		// the bounds; 0.999 is refused (V11)
		half := 0.5
		tooConfident := 0.999
		longReference := strings.Repeat("x", 129)
		tooShort := 59
		tooLong := 3601
		for _, args := range []*LagDriftTestArgs{
			nil,
			{},
			{ClientIp: "not an address"},
			{ClientIp: "0.0.0.0"},
			{ClientIp: lagDriftTestVisitorIp, Confidence: &half},
			{ClientIp: lagDriftTestVisitorIp, Confidence: &tooConfident},
			{ClientIp: lagDriftTestVisitorIp, Reference: longReference},
			{ClientIp: lagDriftTestVisitorIp, TtlSeconds: &tooShort},
			{ClientIp: lagDriftTestVisitorIp, TtlSeconds: &tooLong},
			{ClientIp: lagDriftTestVisitorIp, ParentOrigins: []string{"http://example.com"}},
			{ClientIp: lagDriftTestVisitorIp, ParentOrigins: []string{"https://example.com/page"}},
		} {
			_, err := lagDriftTestAllocate(args, fixture.networkSession, now, fixture.settings)
			assertLagDrift400(t, err)
		}
		_, err = lagDriftTestAllocate(&LagDriftTestArgs{ClientIp: lagDriftTestVisitorIp, Reference: strings.Repeat("é", 128)}, fixture.networkSession, now, fixture.settings)
		connect.AssertEqual(t, err, nil)

		// a token narrows the network's origins, never widens them (P3)
		connect.AssertEqual(t, model.SetLagDriftNetworkOrigins(ctx, fixture.networkId, []string{"https://example.com", "https://shop.example.com"}), nil)
		fixture.allocate(t, now, &LagDriftTestArgs{ClientIp: lagDriftTestVisitorIp, ParentOrigins: []string{"https://shop.example.com"}})
		_, err = lagDriftTestAllocate(&LagDriftTestArgs{ClientIp: lagDriftTestVisitorIp, ParentOrigins: []string{"https://other.example"}}, fixture.networkSession, now, fixture.settings)
		assertLagDrift400(t, err)
	})
}

// Allocations are limited per network (P7): a 429 with the rest of the window
// to wait.
func TestLagDriftAllocateLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		fixture.settings.Drift.AllocateLimit = 2
		now := server.NowUtc()
		fixture.allocate(t, now, nil)
		fixture.allocate(t, now, nil)
		_, err := lagDriftTestAllocate(&LagDriftTestArgs{ClientIp: lagDriftTestVisitorIp}, fixture.networkSession, now, fixture.settings)
		refused := assertLagDriftStatus(t, err, http.StatusTooManyRequests, LagErrorCodeLimit)
		if refused.lagError.RetryAfterMs <= 0 || time.Minute.Milliseconds() < refused.lagError.RetryAfterMs {
			t.Fatalf("retry after %d ms, want within the minute", refused.lagError.RetryAfterMs)
		}
		if refused.RetryAfterSeconds() < 1 || 60 < refused.RetryAfterSeconds() {
			t.Fatalf("Retry-After %d s, want within the minute", refused.RetryAfterSeconds())
		}
	})
}

// The operator's controls (P3, ops runbook): the kill switch stops every
// allocation and start, the demo's too; a network's drift switch stops its
// allocations and the runs of the tokens it holds, and reads go on; a revoked
// token starts no more runs, and its client_ip hash goes at once.
func TestLagDriftOperatorControls(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc()

		// the kill switch
		allocated := fixture.allocate(t, now, nil)
		off := false
		fixture.settings.Drift.Enabled = &off
		_, err := lagDriftTestAllocate(&LagDriftTestArgs{ClientIp: lagDriftTestVisitorIp}, fixture.networkSession, now, fixture.settings)
		assertLagDriftStatus(t, err, http.StatusServiceUnavailable, LagErrorCodeUnavailable)
		assertLagDriftRefused(t, fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", now).Error, LagErrorCodeUnavailable)
		assertLagDriftRefused(t, fixture.start(t, LagDriftDemoToken, lagDriftTestVisitorAddress, "", now).Error, LagErrorCodeUnavailable)
		connect.AssertEqual(t, fixture.read(t, allocated.DriftTestId, now).State, LagDriftStateCreated)
		fixture.settings.Drift.Enabled = nil

		// the network's drift switch
		connect.AssertEqual(t, model.SetLagDriftNetworkDisabled(ctx, fixture.networkId, true), nil)
		connect.AssertEqual(t, model.GetLagDriftNetwork(ctx, fixture.networkId).Disabled, true)
		_, err = lagDriftTestAllocate(&LagDriftTestArgs{ClientIp: lagDriftTestVisitorIp}, fixture.networkSession, now, fixture.settings)
		assertLagDriftStatus(t, err, http.StatusForbidden, LagDriftErrorCodeDisabled)
		assertLagDriftRefused(t, fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", now).Error, LagDriftErrorCodeExpired)
		connect.AssertEqual(t, fixture.read(t, allocated.DriftTestId, now).State, LagDriftStateCreated)
		// the switch keeps the network's origins, and back on, both hold
		connect.AssertEqual(t, model.SetLagDriftNetworkOrigins(ctx, fixture.networkId, []string{"https://example.com"}), nil)
		connect.AssertEqual(t, model.SetLagDriftNetworkDisabled(ctx, fixture.networkId, false), nil)
		network := model.GetLagDriftNetwork(ctx, fixture.networkId)
		connect.AssertEqual(t, network.Disabled, false)
		connect.AssertEqual(t, network.ParentOrigins, []string{"https://example.com"})
		connect.AssertEqual(t, fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "https://example.com", now).Run, 1)
		connect.AssertEqual(t, model.SetLagDriftNetworkDisabled(ctx, server.NewId(), true), model.ErrNetworkNotFound)

		// revoking a token
		revoked := fixture.allocate(t, now, nil)
		connect.AssertEqual(t, model.RevokeLagDriftTest(ctx, revoked.DriftTestId, now), true)
		connect.AssertEqual(t, model.GetLagDriftTest(ctx, revoked.DriftTestId).ClientIpHash, []byte(nil))
		assertLagDriftRefused(t, fixture.start(t, revoked.Token, lagDriftTestVisitorAddress, "https://example.com", now).Error, LagDriftErrorCodeExpired)
		connect.AssertEqual(t, fixture.read(t, revoked.DriftTestId, now).State, LagDriftStateExpired)
		connect.AssertEqual(t, model.RevokeLagDriftTest(ctx, server.NewId(), now), false)
	})
}

// Decodes and verifies a start's tickets under the root key, as an extender
// does (M3).
func verifyLagDriftTickets(t testing.TB, fixture *lagDriftTestFixture, result *LagDriftStartResult, now time.Time) []*protocol.LagTicketBody {
	t.Helper()
	bodies := []*protocol.LagTicketBody{}
	for _, ticket := range result.Tickets {
		bodies = append(bodies, verifyLagTestTicket(t, ticket.Ticket, fixture.rootPublicKey, now))
	}
	return bodies
}

// A run is a lag track of kind drift (E1) with 12 extenders and 4 spares
// chosen around the start's place (E2), each ticketed for pings only, with a
// prompt report (E3, E4). The ticket's client is the drift test, and the
// track keeps the start address's client fields and nothing of the address.
func TestLagDriftStart(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		allocated := fixture.allocate(t, now, nil)

		result := fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "https://example.com", now)
		connect.AssertEqual(t, result.Error, (*LagError)(nil))
		connect.AssertEqual(t, *result.DriftTestId, allocated.DriftTestId)
		connect.AssertEqual(t, result.Demo, false)
		connect.AssertEqual(t, result.Run, 1)
		connect.AssertEqual(t, result.RunsLeft, 5)
		connect.AssertEqual(t, result.MeasureMs, 12000)
		connect.AssertEqual(t, result.Confidence, 0.99)
		connect.AssertEqual(t, result.Ip.CountryCode, "de")
		connect.AssertEqual(t, result.Ip.CountryName, "Germany")
		connect.AssertEqual(t, result.Ip.City, "Berlin")
		// the radar centers on the city's own point
		connect.AssertEqual(t, *result.Ip.Latitude, 52.5196)
		connect.AssertEqual(t, *result.Ip.Longitude, 13.4069)

		connect.AssertEqual(t, len(result.Tickets), 16)
		positions := []int{}
		cities := map[int]string{}
		for _, ticket := range result.Tickets {
			positions = append(positions, ticket.Position)
			cities[ticket.Position] = fixture.places[ticket.ExtenderId].City
			connect.AssertEqual(t, ticket.Host, ticket.ExtenderId.String()+"."+lagTestMeasurementZone)
			connect.AssertEqual(t, ticket.Location.CountryCode, fixture.places[ticket.ExtenderId].CountryCode)
		}
		connect.AssertEqual(t, positions, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15})
		// near: two in Germany first, then the nearest; mid by the ring
		// around the IP place; far spread by bearing; spares nearest first
		// (E2)
		connect.AssertEqual(t, []string{cities[0], cities[1], cities[2], cities[3]}, []string{"Berlin", "Leipzig", "Szczecin", "Dresden"})
		mid := []string{cities[4], cities[5], cities[6], cities[7]}
		slices.Sort(mid)
		connect.AssertEqual(t, mid, []string{"Kyiv", "Lisbon", "London", "Stockholm"})
		for _, position := range []int{8, 9, 10, 11} {
			if !slices.Contains([]string{"Cairo", "Dubai", "Lagos", "New York", "Mumbai", "Toronto"}, cities[position]) {
				t.Fatalf("far position %d is %s", position, cities[position])
			}
		}
		connect.AssertEqual(t, []string{cities[12], cities[13]}, []string{"Hamburg", "Prague"})

		expiresAt, err := time.Parse(time.RFC3339, result.ExpiresAt)
		connect.AssertEqual(t, err, nil)
		for i, body := range verifyLagDriftTickets(t, fixture, result, now) {
			connect.AssertEqual(t, body.ClientId, allocated.DriftTestId.Bytes())
			connect.AssertEqual(t, body.TrackId, result.TrackId.Bytes())
			connect.AssertEqual(t, body.TrackPosition, uint32(result.Tickets[i].Position))
			connect.AssertEqual(t, body.ExpireTimeMs, uint64(expiresAt.UnixMilli()))
			connect.AssertEqual(t, body.PingIntervalMs, uint32(250))
			// any load bounded to nothing on every extender version (E3)
			connect.AssertEqual(t, body.DownloadMs, uint32(1))
			connect.AssertEqual(t, body.UploadMs, uint32(1))
			connect.AssertEqual(t, body.MaxStreams, uint32(1))
			connect.AssertEqual(t, body.MaxDownloadBytes, uint64(1))
			connect.AssertEqual(t, body.MaxUploadBytes, uint64(1))
			// reported at once, and reported (E4)
			connect.AssertEqual(t, body.ReportPromptly, true)
			connect.AssertEqual(t, body.NoReport, false)
			// stored for the report (A7)
			stored := model.GetLagTicket(ctx, server.RequireIdFromBytes(body.TicketId))
			connect.AssertEqual(t, stored.TrackId, *result.TrackId)
			connect.AssertEqual(t, stored.ClientId, allocated.DriftTestId)
			connect.AssertEqual(t, stored.NetworkId, fixture.networkId)
		}

		// the track (E1)
		track := model.GetLagTrack(ctx, *result.TrackId)
		connect.AssertEqual(t, track.ClientId, allocated.DriftTestId)
		connect.AssertEqual(t, track.NetworkId, fixture.networkId)
		var kind string
		var driftTestId server.Id
		var driftRun int
		var countryCode, city string
		var latitude float64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT kind, drift_test_id, drift_run, client_country_code, client_city, client_latitude FROM lag_track WHERE track_id = $1`,
				*result.TrackId,
			).Scan(&kind, &driftTestId, &driftRun, &countryCode, &city, &latitude))
		})
		connect.AssertEqual(t, kind, model.LagTrackKindDrift)
		connect.AssertEqual(t, driftTestId, allocated.DriftTestId)
		connect.AssertEqual(t, driftRun, 1)
		connect.AssertEqual(t, countryCode, "de")
		connect.AssertEqual(t, city, "Berlin")
		connect.AssertEqual(t, latitude, 52.5196)
		for _, table := range []string{"lag_track", "lag_ticket", "lag_drift_run", "lag_drift_test"} {
			if strings.Contains(lagTestTableText(t, ctx, table), "198.51.100.") {
				t.Fatalf("%s holds the visitor's address", table)
			}
		}

		run := model.GetLagDriftRuns(ctx, allocated.DriftTestId)[0]
		connect.AssertEqual(t, run.State, model.LagDriftRunStateStarted)
		connect.AssertEqual(t, *run.ClientIpMatches, true)
		connect.AssertEqual(t, run.IpFamily, 4)
		connect.AssertEqual(t, run.IpCountryCode, "de")
		connect.AssertEqual(t, run.IpCountryName, "Germany")
		connect.AssertEqual(t, len(run.Extenders), 16)
		for _, extender := range run.Extenders {
			// GeoLite2's 20 km for the address the place came from (V3)
			connect.AssertEqual(t, extender.PlaceErrorKm, 20.0)
		}

		read := fixture.read(t, allocated.DriftTestId, now)
		connect.AssertEqual(t, read.State, LagDriftStateActive)
		connect.AssertEqual(t, *read.FirstStartedAt, lagDriftTime(now))
		connect.AssertEqual(t, *read.RetestUntil, lagDriftTime(now.Add(15*time.Minute)))
		connect.AssertEqual(t, len(read.Runs), 1)
		connect.AssertEqual(t, read.Runs[0].Extenders, 12)
		connect.AssertEqual(t, *read.Runs[0].ClientIpMatches, true)
	})
}

// A token runs one run at a time and at most 6, its first within its expiry
// and its retests within 15 minutes of the first start (decision 4, P5). The
// last run it allows deletes its client_ip hash (K3), as does the sweep for a
// token whose time is up.
func TestLagDriftTokenRuns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)

		// an unknown token, a malformed one, and one never started in time
		for _, token := range []string{"dt1_" + strings.Repeat("A", 43), "dt1_short", "not-a-token"} {
			assertLagDriftRefused(t, fixture.start(t, token, lagDriftTestVisitorAddress, "", now).Error, LagDriftErrorCodeExpired)
		}
		expired := fixture.allocate(t, now, nil)
		assertLagDriftRefused(t, fixture.start(t, expired.Token, lagDriftTestVisitorAddress, "", now.Add(300*time.Second)).Error, LagDriftErrorCodeExpired)

		allocated := fixture.allocate(t, now, nil)
		at := now
		for run := 1; run <= 6; run += 1 {
			result := fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", at)
			connect.AssertEqual(t, result.Error, (*LagError)(nil))
			connect.AssertEqual(t, result.Run, run)
			connect.AssertEqual(t, result.RunsLeft, 6-run)
			// one at a time: another start while it measures is busy, and
			// after the sixth, closed
			busy := fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", at.Add(time.Second)).Error
			if run < 6 {
				assertLagDriftRefused(t, busy, LagDriftErrorCodeBusy)
			} else {
				assertLagDriftRefused(t, busy, LagDriftErrorCodeClosed)
			}
			// its finish lets a retest start
			finished := fixture.finish(t, allocated.Token, run, LagDriftStopConverged, at.Add(4*time.Second))
			connect.AssertEqual(t, finished.Run, run)
			connect.AssertEqual(t, finished.RunState, model.LagDriftRunStateProcessing)
			// finish is idempotent and keeps its first stop
			again := fixture.finish(t, allocated.Token, run, LagDriftStopCap, at.Add(5*time.Second))
			connect.AssertEqual(t, again.RunState, model.LagDriftRunStateProcessing)
			at = at.Add(5 * time.Second)
		}
		connect.AssertEqual(t, model.GetLagDriftRuns(ctx, allocated.DriftTestId)[0].Stop, LagDriftStopConverged)
		// out of runs: closed, and the hash is gone with the last run
		assertLagDriftRefused(t, fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", at).Error, LagDriftErrorCodeClosed)
		connect.AssertEqual(t, model.GetLagDriftTest(ctx, allocated.DriftTestId).ClientIpHash, []byte(nil))
		_, err := lagDriftTestFinish(&LagDriftFinishArgs{Token: allocated.Token, Run: 7, Stop: LagDriftStopCap}, fixture.widgetSession(t, lagDriftTestVisitorAddress), at, fixture.settings)
		assertLagDrift400(t, err)
		_, err = lagDriftTestFinish(&LagDriftFinishArgs{Token: allocated.Token, Run: 1, Stop: "lucky"}, fixture.widgetSession(t, lagDriftTestVisitorAddress), at, fixture.settings)
		assertLagDrift400(t, err)

		// a run that never finishes stops measuring after processing_after
		other := fixture.allocate(t, now, nil)
		connect.AssertEqual(t, fixture.start(t, other.Token, lagDriftTestVisitorAddress, "", now).Run, 1)
		assertLagDriftRefused(t, fixture.start(t, other.Token, lagDriftTestVisitorAddress, "", now.Add(29*time.Second)).Error, LagDriftErrorCodeBusy)
		connect.AssertEqual(t, fixture.start(t, other.Token, lagDriftTestVisitorAddress, "", now.Add(30*time.Second)).Run, 2)
		// the retest window closes 15 minutes after the first start
		fixture.finish(t, other.Token, 2, LagDriftStopCap, now.Add(40*time.Second))
		assertLagDriftRefused(t, fixture.start(t, other.Token, lagDriftTestVisitorAddress, "", now.Add(15*time.Minute)).Error, LagDriftErrorCodeClosed)
		connect.AssertNotEqual(t, model.GetLagDriftTest(ctx, other.DriftTestId).ClientIpHash, []byte(nil))
		connect.AssertEqual(t, model.RemoveClosedLagDriftClientIpHashes(ctx, now.Add(15*time.Minute), fixture.settings.Drift.MaxRuns), 2)
		connect.AssertEqual(t, model.GetLagDriftTest(ctx, other.DriftTestId).ClientIpHash, []byte(nil))
		connect.AssertEqual(t, model.GetLagDriftTest(ctx, expired.DriftTestId).ClientIpHash, []byte(nil))
	})
}

// A run started from another address than client_ip is measured, but marked
// client_ip_matches false; one from the other family is null (K3). The
// comparison is by the peppered hash of the /29 (/56).
func TestLagDriftClientIpBinding(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc()
		matches := func(clientAddress string) *bool {
			allocated := fixture.allocate(t, now, nil)
			result := fixture.start(t, allocated.Token, clientAddress, "", now)
			connect.AssertEqual(t, result.Error, (*LagError)(nil))
			return model.GetLagDriftRuns(ctx, allocated.DriftTestId)[0].ClientIpMatches
		}
		connect.AssertEqual(t, *matches(lagDriftTestVisitorAddress), true)
		// the same /29
		connect.AssertEqual(t, *matches("198.51.100.25:1"), true)
		connect.AssertEqual(t, *matches(lagDriftTestNeighborAddress), false)
		connect.AssertEqual(t, *matches(lagDriftTestLyonAddress), false)
		connect.AssertEqual(t, matches(lagDriftTestVisitorIpv6), (*bool)(nil))
	})
}

// Parent origins (W5, P3): with no list any https origin and an unknown one
// are accepted; with the network's list, only its origins; and a token
// narrows the network's list.
func TestLagDriftOrigins(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc()
		startAt := func(args *LagDriftTestArgs, parentOrigin string) *LagError {
			allocated := fixture.allocate(t, now, args)
			return fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, parentOrigin, now).Error
		}
		for _, origin := range []string{"", "null", "https://example.com", "https://anywhere.example/page?x=1", "https://EXAMPLE.com:443", "http://localhost:5173", "http://127.0.0.1:8080/page"} {
			connect.AssertEqual(t, startAt(nil, origin), (*LagError)(nil))
		}
		for _, origin := range []string{"http://example.com", "file:///x", "https://", "example.com", "http://localhost.example"} {
			assertLagDriftRefused(t, startAt(nil, origin), LagDriftErrorCodeOrigin)
		}

		connect.AssertEqual(t, model.SetLagDriftNetworkOrigins(ctx, fixture.networkId, []string{"https://example.com", "https://shop.example.com"}), nil)
		connect.AssertEqual(t, startAt(nil, "https://shop.example.com/cart"), (*LagError)(nil))
		assertLagDriftRefused(t, startAt(nil, "https://other.example"), LagDriftErrorCodeOrigin)
		assertLagDriftRefused(t, startAt(nil, ""), LagDriftErrorCodeOrigin)
		narrowed := &LagDriftTestArgs{ClientIp: lagDriftTestVisitorIp, ParentOrigins: []string{"https://shop.example.com"}}
		connect.AssertEqual(t, startAt(narrowed, "https://shop.example.com"), (*LagError)(nil))
		assertLagDriftRefused(t, startAt(narrowed, "https://example.com"), LagDriftErrorCodeOrigin)

		connect.AssertEqual(t, model.SetLagDriftNetworkOrigins(ctx, fixture.networkId, nil), nil)
		connect.AssertEqual(t, model.GetLagDriftNetworkOrigins(ctx, fixture.networkId), []string(nil))
		connect.AssertEqual(t, startAt(nil, "https://other.example"), (*LagError)(nil))
		connect.AssertEqual(t, model.SetLagDriftNetworkOrigins(ctx, server.NewId(), []string{"https://example.com"}), model.ErrNetworkNotFound)
	})
}

// Runs are limited per /24 (/48) apart from lag.dev's own track limits (P7),
// and a busy refusal is not a run.
func TestLagDriftStartLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		fixture.settings.Drift.StartLimit = 2
		now := server.NowUtc()
		for range 2 {
			allocated := fixture.allocate(t, now, nil)
			connect.AssertEqual(t, fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", now).Error, (*LagError)(nil))
		}
		allocated := fixture.allocate(t, now, nil)
		// another address of the /24
		refused := fixture.start(t, allocated.Token, lagDriftTestNeighborAddress, "", now).Error
		assertLagDriftRefused(t, refused, LagErrorCodeLimit)
		if refused.RetryAfterMs <= 0 || time.Hour.Milliseconds() < refused.RetryAfterMs {
			t.Fatalf("retry after %d ms, want within the hour", refused.RetryAfterMs)
		}
		// another /24 is its own
		connect.AssertEqual(t, fixture.start(t, allocated.Token, lagDriftTestLyonAddress, "", now).Error, (*LagError)(nil))
	})
}

// A start over client_ip's other family is pinned to client_ip's (K6, P5):
// it answers the API's origin in that family and nothing else, using no run
// and no count of the start limit. Sent unpinned, it runs as an other-family
// run (K3). A start over client_ip's family, and the demo's, are never
// pinned, and nor is a token that can't start, which answers as an unknown
// one does. Every start's token read counts first, at twice the start limit.
func TestLagDriftStartPinned(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		fixture.settings.Drift.StartLimit = 1
		now := server.NowUtc().Truncate(time.Millisecond)
		start := func(token string, clientAddress string, unpinned bool, at time.Time) *LagDriftStartResult {
			result, err := lagDriftTestStart(
				&LagDriftStartArgs{Token: token, Unpinned: unpinned},
				fixture.widgetSession(t, clientAddress),
				at,
				fixture.settings,
			)
			if err != nil {
				t.Fatal(err)
			}
			return result
		}

		// client_ip over IPv4, the start over IPv6
		allocated := fixture.allocate(t, now, nil)
		pinned := start(allocated.Token, lagDriftTestVisitorIpv6, false, now)
		connect.AssertEqual(t, pinned.Pin, "https://api-v4.bringyour.com")
		pinnedJson, err := json.Marshal(pinned)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, string(pinnedJson), `{"pin":"https://api-v4.bringyour.com"}`)
		view := fixture.read(t, allocated.DriftTestId, now)
		connect.AssertEqual(t, len(view.Runs), 0)
		connect.AssertEqual(t, view.State, LagDriftStateCreated)

		// unpinned, the start goes ahead from the other family: the pin
		// counted no start, so the /48's one start is this one
		unpinned := start(allocated.Token, lagDriftTestVisitorIpv6, true, now)
		connect.AssertEqual(t, unpinned.Pin, "")
		connect.AssertEqual(t, unpinned.Error, (*LagError)(nil))
		connect.AssertEqual(t, unpinned.Run, 1)
		connect.AssertEqual(t, fixture.runView(t, allocated.DriftTestId, 1, now).ClientIpMatches, (*bool)(nil))
		other := fixture.allocate(t, now, nil)
		assertLagDriftRefused(t, start(other.Token, lagDriftTestVisitorIpv6, true, now).Error, LagErrorCodeLimit)

		// client_ip over IPv6, the start over IPv4
		allocated = fixture.allocate(t, now, &LagDriftTestArgs{ClientIp: "2001:db8:1::30"})
		connect.AssertEqual(t, start(allocated.Token, lagDriftTestVisitorAddress, false, now).Pin, "https://api-v6.bringyour.com")

		// a token that can't start answers as an unknown one does, never
		// with a pin that would say it exists and in which family: expired,
		// revoked, or its network switched off; each from a /48 of its own
		ttlSeconds := 60
		expiring := fixture.allocate(t, now, &LagDriftTestArgs{ClientIp: lagDriftTestVisitorIp, TtlSeconds: &ttlSeconds})
		assertLagDriftRefused(t, start(expiring.Token, "[2001:db8:4::1]:52000", false, now.Add(2*time.Minute)).Error, LagDriftErrorCodeExpired)
		revoked := fixture.allocate(t, now, nil)
		connect.AssertEqual(t, model.RevokeLagDriftTest(ctx, revoked.DriftTestId, now), true)
		assertLagDriftRefused(t, start(revoked.Token, "[2001:db8:5::1]:52000", false, now).Error, LagDriftErrorCodeExpired)
		assertLagDriftRefused(t, start("dt1_unknown", "[2001:db8:6::1]:52000", false, now).Error, LagDriftErrorCodeExpired)
		switchedOff := fixture.allocate(t, now, nil)
		connect.AssertEqual(t, model.SetLagDriftNetworkDisabled(ctx, fixture.networkId, true), nil)
		assertLagDriftRefused(t, start(switchedOff.Token, "[2001:db8:7::1]:52000", false, now).Error, LagDriftErrorCodeExpired)
		connect.AssertEqual(t, model.SetLagDriftNetworkDisabled(ctx, fixture.networkId, false), nil)

		// the token reads count before the token is read: twice the start
		// limit from a /48, whatever the tokens
		for range 2 {
			assertLagDriftRefused(t, start("dt1_unknown", "[2001:db8:8::1]:52000", false, now).Error, LagDriftErrorCodeExpired)
		}
		assertLagDriftRefused(t, start("dt1_unknown", "[2001:db8:8::1]:52000", false, now).Error, LagErrorCodeLimit)

		// the demo is never pinned
		demo := start(LagDriftDemoToken, lagDriftTestLyonIpv6, false, now)
		connect.AssertEqual(t, demo.Pin, "")
		connect.AssertEqual(t, demo.Demo, true)
	})
}

// A run's k and k_early (V11): the calibration's k at the token's confidence
// for client_ip's continent, here the defaults, and k_early from it, answered
// by start and recorded on the run. The demo's are for its own address's
// continent.
func TestLagDriftStartK(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		// Africa's k at 99% is above the textbook's, and so its k_early above 4
		connect.AssertEqual(t, lagdrift.KForConfidence(0.99) < lagdrift.DefaultK(0.99, "AF"), true)
		connect.AssertEqual(t, lagdrift.EarlyK < lagdrift.KEarly(lagdrift.DefaultK(0.99, "AF")), true)
		for _, check := range []struct {
			clientIp      string
			clientAddress string
			confidence    float64
			k             float64
		}{
			{lagDriftTestVisitorIp, lagDriftTestVisitorAddress, 0.99, lagdrift.DefaultK(0.99, "EU")},
			{lagDriftTestVisitorIp, lagDriftTestVisitorAddress, 0.95, lagdrift.DefaultK(0.95, "EU")},
			{lagDriftTestCairoIp, lagDriftTestCairoAddress, 0.99, lagdrift.DefaultK(0.99, "AF")},
		} {
			confidence := check.confidence
			allocated := fixture.allocate(t, now, &LagDriftTestArgs{ClientIp: check.clientIp, Confidence: &confidence})
			start := fixture.start(t, allocated.Token, check.clientAddress, "", now)
			connect.AssertEqual(t, start.Error, (*LagError)(nil))
			connect.AssertEqual(t, start.K, check.k)
			connect.AssertEqual(t, start.KEarly, lagdrift.KEarly(check.k))
			run := model.GetLagDriftRuns(ctx, allocated.DriftTestId)[0]
			connect.AssertEqual(t, *run.K, check.k)
			connect.AssertEqual(t, *run.KEarly, lagdrift.KEarly(check.k))
		}

		demo := fixture.start(t, LagDriftDemoToken, lagDriftTestCairoAddress, "", now)
		connect.AssertEqual(t, demo.K, lagdrift.DefaultK(0.99, "AF"))
		connect.AssertEqual(t, demo.KEarly, lagdrift.KEarly(demo.K))
	})
}

// start answers as the IP place's point the point the run's fits measure
// from (V5, V10): the city's own point the track keeps, else the middle of
// the box around client_ip's country, so the frame's fits and the server's
// start from one place.
func TestLagDriftStartOrigin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)

		allocated := fixture.allocate(t, now, nil)
		start := fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", now)
		connect.AssertEqual(t, start.Ip.City, "Berlin")
		connect.AssertEqual(t, *start.Ip.Latitude, 52.5196)
		connect.AssertEqual(t, *start.Ip.Longitude, 13.4069)

		// Leipzig has no point of its own: Germany, with no city, at the
		// middle of its box, 47.3 to 55.0 north and 5.9 to 15.0 east
		allocated = fixture.allocate(t, now, &LagDriftTestArgs{ClientIp: lagDriftTestLeipzigIp})
		start = fixture.start(t, allocated.Token, lagDriftTestLeipzigAddress, "", now)
		connect.AssertEqual(t, start.Error, (*LagError)(nil))
		connect.AssertEqual(t, start.Ip.CountryCode, "de")
		connect.AssertEqual(t, start.Ip.CountryName, "Germany")
		connect.AssertEqual(t, start.Ip.City, "")
		connect.AssertEqual(t, start.Ip.Region, "")
		if math.Abs(*start.Ip.Latitude-51.15) > 0.01 || math.Abs(*start.Ip.Longitude-10.45) > 0.01 {
			t.Fatalf("the origin is %v, %v, want the middle of Germany's box", *start.Ip.Latitude, *start.Ip.Longitude)
		}
	})
}

// The demo token (W8) runs for anyone with tickets as a run's, carrying
// NoReport, answers demo with no drift test, and stores nothing but its rate
// limit counter. finish and result refuse it.
func TestLagDriftDemo(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		fixture.settings.Drift.DemoStartsPerHour = 2
		now := server.NowUtc().Truncate(time.Millisecond)
		countRows := func() int {
			count := 0
			server.Db(ctx, func(conn server.PgConn) {
				for _, table := range []string{"lag_track", "lag_ticket", "lag_result", "lag_drift_test", "lag_drift_run"} {
					var tableCount int
					server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&tableCount))
					count += tableCount
				}
			})
			return count
		}
		before := countRows()

		result := fixture.start(t, LagDriftDemoToken, lagDriftTestVisitorAddress, "https://anywhere.example", now)
		connect.AssertEqual(t, result.Error, (*LagError)(nil))
		connect.AssertEqual(t, result.Demo, true)
		connect.AssertEqual(t, result.DriftTestId, (*server.Id)(nil))
		connect.AssertEqual(t, result.TrackId, (*server.Id)(nil))
		connect.AssertEqual(t, result.Run, 0)
		connect.AssertEqual(t, result.Ip.CountryCode, "de")
		connect.AssertEqual(t, len(result.Tickets), 16)
		tracks := map[server.Id]bool{}
		for _, body := range verifyLagDriftTickets(t, fixture, result, now) {
			connect.AssertEqual(t, body.NoReport, true)
			connect.AssertEqual(t, body.ReportPromptly, false)
			connect.AssertEqual(t, body.MaxStreams, uint32(1))
			connect.AssertEqual(t, body.MaxDownloadBytes, uint64(1))
			ticketId := server.RequireIdFromBytes(body.TicketId)
			connect.AssertEqual(t, model.GetLagTicket(ctx, ticketId), (*model.LagTicket)(nil))
			// each demo ticket's track is derived from it, so a report of it
			// is known without anything stored
			trackId := server.RequireIdFromBytes(body.TrackId)
			connect.AssertEqual(t, trackId, lagDriftDemoTrackId(ticketId))
			tracks[trackId] = true
		}
		connect.AssertEqual(t, len(tracks), 16)
		connect.AssertEqual(t, countRows(), before)

		// limited per /24 (/48)
		connect.AssertEqual(t, fixture.start(t, LagDriftDemoToken, lagDriftTestNeighborAddress, "", now).Error, (*LagError)(nil))
		assertLagDriftRefused(t, fixture.start(t, LagDriftDemoToken, lagDriftTestVisitorAddress, "", now).Error, LagErrorCodeLimit)
		// any https page, but only one
		assertLagDriftRefused(t, fixture.start(t, LagDriftDemoToken, lagDriftTestLyonAddress, "http://example.com", now).Error, LagDriftErrorCodeOrigin)

		_, err := lagDriftTestFinish(&LagDriftFinishArgs{Token: LagDriftDemoToken, Run: 1, Stop: LagDriftStopCap}, fixture.widgetSession(t, lagDriftTestVisitorAddress), now, fixture.settings)
		assertLagDrift400(t, err)
		_, err = lagDriftTestResult(&LagDriftResultArgs{Token: LagDriftDemoToken}, fixture.widgetSession(t, lagDriftTestVisitorAddress), now, fixture.settings)
		assertLagDrift400(t, err)
	})
}

// Without the borders no run starts (G1); the demo token, whose verdict is
// the frame's own, still does.
func TestLagDriftStartWithoutBorders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc()
		allocated := fixture.allocate(t, now, nil)
		// a deployment without the file
		defer Testing_SetLagDriftBorders(nil)()

		assertLagDriftRefused(t, fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", now).Error, LagErrorCodeUnavailable)
		connect.AssertEqual(t, fixture.start(t, LagDriftDemoToken, lagDriftTestVisitorAddress, "", now).Error, (*LagError)(nil))
	})
}

// The widget's result answers only P5's fields: never the drift country, a
// distance or the fit (decision 7).
func TestLagDriftWidgetResultFields(t *testing.T) {
	verdict := LagDriftVerdictDisagree
	widget := &LagDriftWidgetResult{
		State:         LagDriftStateActive,
		Run:           2,
		RunState:      model.LagDriftRunStateComplete,
		Verdict:       &verdict,
		IpCountryName: "Germany",
		RunsInVerdict: []int{1, 2},
		RetestAllowed: true,
		RunsLeft:      4,
	}
	widgetJson, err := json.Marshal(widget)
	connect.AssertEqual(t, err, nil)
	fields := map[string]any{}
	connect.AssertEqual(t, json.Unmarshal(widgetJson, &fields), nil)
	keys := []string{}
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	connect.AssertEqual(t, keys, []string{"ip_country_name", "retest_allowed", "run", "run_state", "runs_in_verdict", "runs_left", "state", "verdict"})
	for _, forbidden := range []string{"fr", "910", "drift", "distance", "margin", "ellipse", "latitude", "longitude"} {
		if strings.Contains(string(widgetJson), forbidden) {
			t.Fatalf("the widget's result names %q: %s", forbidden, widgetJson)
		}
	}
}

// Origins as compared (W5, P3). Pure.
func TestLagDriftParseOrigin(t *testing.T) {
	for text, want := range map[string]string{
		"":                             "",
		"null":                         "",
		"https://example.com":          "https://example.com",
		"https://Example.COM/":         "https://example.com",
		"https://example.com:443":      "https://example.com",
		"https://example.com:8443":     "https://example.com:8443",
		"https://[2001:db8::1]:8443/":  "https://[2001:db8::1]:8443",
		"https://example.com/page?q=1": "https://example.com",
		"http://localhost:3000":        "http://localhost:3000",
		"http://LOCALHOST":             "http://localhost",
		"http://127.0.0.1:8080/":       "http://127.0.0.1:8080",
		"http://localhost:80":          "http://localhost",
	} {
		origin, ok := lagDriftParseOrigin(text, false)
		if !ok || origin != want {
			t.Errorf("%q parsed as %q, %t; want %q", text, origin, ok, want)
		}
	}
	for _, text := range []string{"http://example.com", "http://192.168.1.2", "http://[::1]:3000", "example.com", "https://", "https://user@example.com", "ftp://example.com"} {
		if _, ok := lagDriftParseOrigin(text, false); ok {
			t.Errorf("%q parsed", text)
		}
	}
	// an allocation's origins are origins, not pages
	for _, text := range []string{"https://example.com/page", "https://example.com/?q=1", "https://example.com/#x"} {
		if _, ok := lagDriftParseOrigin(text, true); ok {
			t.Errorf("%q parsed strictly", text)
		}
	}
	origins, err := ParseLagDriftOrigins([]string{"https://a.example", "https://A.example/", "https://b.example"})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, origins, []string{"https://a.example", "https://b.example"})
	tooMany := []string{}
	for i := range 17 {
		tooMany = append(tooMany, fmt.Sprintf("https://%d.example", i))
	}
	_, err = ParseLagDriftOrigins(tooMany)
	connect.AssertNotEqual(t, err, nil)
}

// E2's choice (lag.dev/DRIFTTEST.md E2): the near band nearest, the mid band
// by expected information over a ring of plausible places around the IP
// place, and the far band spread by bearing, with a short mid band's slots
// passing to it. A retest prefers extenders the window has not used, within
// each band, and falls back to repeats where a band has no others. The choice
// is the same for the same pool and place. An address placed nowhere gets the
// sample's spread. Names pinned to the run's family come first in each band
// (K6). Pure.
func TestLagDriftChoose(t *testing.T) {
	set := &lagServingSet{byId: map[server.Id]*lagServingExtender{}}
	byCity := map[string]*lagServingExtender{}
	for _, city := range lagDriftTestCities {
		extender := &lagServingExtender{
			ExtenderMeasurementHost: &ExtenderMeasurementHost{ExtenderId: server.NewId()},
			Place:                   lagTestPlace(city.city, city.countryCode, city.latitude, city.longitude),
		}
		set.extenders = append(set.extenders, extender)
		set.byId[extender.ExtenderId] = extender
		byCity[city.city] = extender
	}
	drift := DefaultLagDriftSettings()
	driftModel := model.DefaultLagDriftCalibration().Model()
	berlin := &lagDriftAddrPlace{CountryCode: "de", Point: &lagDriftPoint{latitude: 52.5196, longitude: 13.4069}}
	cities := func(chosen []*lagDriftExtender) []string {
		names := []string{}
		for _, choice := range chosen {
			names = append(names, choice.extender.Place.City)
		}
		return names
	}

	first := lagDriftChoose(set, berlin, nil, drift, 4, driftModel, nil)
	connect.AssertEqual(t, len(first), 16)
	connect.AssertEqual(t, cities(first)[:4], []string{"Berlin", "Leipzig", "Szczecin", "Dresden"})
	// the ring takes extenders toward Iberia, Scandinavia and the east,
	// where spreading by bearing took the nearest in each gap (London, Rome,
	// Stockholm, Warsaw)
	firstMid := cities(first)[4:8]
	slices.Sort(firstMid)
	connect.AssertEqual(t, firstMid, []string{"Kyiv", "Lisbon", "London", "Stockholm"})
	for _, toward := range [][]string{{"Lisbon", "Madrid"}, {"Stockholm"}, {"Kyiv", "Warsaw"}} {
		if !slices.ContainsFunc(firstMid, func(city string) bool { return slices.Contains(toward, city) }) {
			t.Fatalf("the mid band %v has none of %v", firstMid, toward)
		}
	}
	firstFar := cities(first)[8:12]
	slices.Sort(firstFar)
	connect.AssertEqual(t, firstFar, []string{"Cairo", "Lagos", "Mumbai", "Toronto"})
	connect.AssertEqual(t, cities(first)[12:], []string{"Hamburg", "Prague", "Warsaw", "Paris"})
	// the same pool and place choose the same
	connect.AssertEqual(t, cities(lagDriftChoose(set, berlin, nil, drift, 4, driftModel, nil)), cities(first))
	for i, choice := range first {
		// the run's 12 from 0, the spares from 12
		connect.AssertEqual(t, choice.position, i)
	}

	// a retest: the near band's unused extenders first, two in the IP
	// country, then the nearest repeats
	used := map[server.Id]bool{}
	for _, choice := range first[:12] {
		used[choice.extender.ExtenderId] = true
	}
	retest := lagDriftChoose(set, berlin, used, drift, 4, driftModel, nil)
	connect.AssertEqual(t, cities(retest)[:4], []string{"Hamburg", "Berlin", "Prague", "Szczecin"})
	for _, choice := range retest[4:8] {
		if used[choice.extender.ExtenderId] {
			t.Fatalf("the retest's mid band %v repeats %s", cities(retest)[4:8], choice.extender.Place.City)
		}
	}
	// the far band's two unused, then repeats
	retestFar := cities(retest)[8:12]
	for _, city := range []string{"Dubai", "New York"} {
		if !slices.Contains(retestFar, city) {
			t.Fatalf("the retest's far band %v lacks the unused %s", retestFar, city)
		}
	}

	// an extender an operator excluded is never chosen
	drift.ExcludedExtenders = []string{byCity["Berlin"].ExtenderId.String(), "not-an-id"}
	excluded := lagDriftChoose(lagDriftWithout(set, drift.Excluded()), berlin, nil, drift, 4, driftModel, nil)
	connect.AssertEqual(t, cities(excluded)[:4], []string{"Leipzig", "Dresden", "Szczecin", "Hamburg"})

	// a mid band of two hands its other two slots to the far band
	drift.ExcludedExtenders = []string{}
	for _, city := range []string{"Warsaw", "Paris", "London", "Rome", "Kyiv", "Lisbon"} {
		drift.ExcludedExtenders = append(drift.ExcludedExtenders, byCity[city].ExtenderId.String())
	}
	short := lagDriftChoose(lagDriftWithout(set, drift.Excluded()), berlin, nil, drift, 4, driftModel, nil)
	shortMid := cities(short)[4:6]
	slices.Sort(shortMid)
	connect.AssertEqual(t, shortMid, []string{"Madrid", "Stockholm"})
	shortFar := cities(short)[6:12]
	slices.Sort(shortFar)
	connect.AssertEqual(t, shortFar, []string{"Cairo", "Dubai", "Lagos", "Mumbai", "New York", "Toronto"})
	drift.ExcludedExtenders = nil

	// an address placed nowhere: a spread across countries
	unplaced := lagDriftChoose(set, &lagDriftAddrPlace{}, nil, drift, 4, driftModel, nil)
	connect.AssertEqual(t, len(unplaced), 16)
	countries := map[string]bool{}
	for _, choice := range unplaced {
		countries[choice.extender.Place.CountryCode] = true
	}
	if len(countries) < 12 {
		t.Fatalf("an unplaced choice spans %d countries", len(countries))
	}

	// names pinned to the run's family first within each band (K6): every
	// extender but Berlin's and Leipzig's has its IPv4 name, and none in the
	// far band, which fills with plain names
	publishTime := server.NowUtc()
	far := []string{"Cairo", "Dubai", "Lagos", "New York", "Mumbai", "Toronto"}
	for i, extender := range set.extenders {
		extender.Name = fmt.Sprintf("%s.m.example", extender.ExtenderId)
		extender.Ips = []netip.Addr{netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", 10+i))}
		if city := extender.Place.City; city == "Berlin" || city == "Leipzig" || slices.Contains(far, city) {
			continue
		}
		extender.Ipv4Name = fmt.Sprintf("%s.ipv4.m.example", extender.ExtenderId)
		extender.FamilyPublishTime = &publishTime
		extender.FamilyServing = true
	}
	pinned := lagDriftChoose(set, berlin, nil, drift, 4, driftModel, nil)
	connect.AssertEqual(t, cities(pinned)[:4], []string{"Dresden", "Hamburg", "Szczecin", "Prague"})
	for _, choice := range pinned[:8] {
		connect.AssertEqual(t, choice.pinned, true)
	}
	for _, city := range cities(pinned)[8:12] {
		if !slices.Contains(far, city) {
			t.Fatalf("the far band %v has %s", cities(pinned)[8:12], city)
		}
	}
	connect.AssertEqual(t, lagDriftHost(byCity["Dresden"], 4), byCity["Dresden"].Ipv4Name)
	connect.AssertEqual(t, lagDriftHost(byCity["Dresden"], 6), byCity["Dresden"].Name)
	connect.AssertEqual(t, lagDriftHost(byCity["Berlin"], 4), byCity["Berlin"].Name)
	// a run over IPv6, whose names none has, chooses as without them
	connect.AssertEqual(t, cities(lagDriftChoose(set, berlin, nil, drift, 6, driftModel, nil)), cities(first))

	// a retest takes the unused plain names, Berlin and Leipzig, before the
	// pinned ones it has used
	pinnedUsed := map[server.Id]bool{}
	for _, choice := range pinned[:12] {
		pinnedUsed[choice.extender.ExtenderId] = true
	}
	pinnedRetest := lagDriftChoose(set, berlin, pinnedUsed, drift, 4, driftModel, nil)
	connect.AssertEqual(t, cities(pinnedRetest)[:2], []string{"Berlin", "Leipzig"})
}

// A run's tickets name each extender's host pinned to the run's family where
// it has one, and its measurement name where it hasn't (K6). Pure.
func TestLagDriftTicketHosts(t *testing.T) {
	installLagTestExtenderConfig(t, true)
	signer, refusal := newLagTicketSigner()
	if refusal != nil {
		t.Fatalf("no signer: %+v", refusal)
	}
	publishTime := server.NowUtc()
	extender := func(name string, ipv4Name string, ipv6Name string) *lagServingExtender {
		host := &ExtenderMeasurementHost{
			ExtenderId: server.NewId(),
			PublicKey:  make([]byte, 32),
			Name:       name,
			Ips:        []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8:10::1")},
			Ipv4Name:   ipv4Name,
			Ipv6Name:   ipv6Name,
		}
		if ipv4Name != "" || ipv6Name != "" {
			host.FamilyPublishTime = &publishTime
			host.FamilyServing = true
		}
		return &lagServingExtender{
			ExtenderMeasurementHost: host,
			Place:                   lagTestPlace("Berlin", "de", 52.52, 13.40),
		}
	}
	chosen := []*lagDriftExtender{
		{extender: extender("a.m.example", "a.ipv4.m.example", "a.ipv6.m.example"), position: 0},
		{extender: extender("b.m.example", "", ""), position: 1},
	}
	for family, want := range map[int][]string{
		4: {"a.ipv4.m.example", "b.m.example"},
		6: {"a.ipv6.m.example", "b.m.example"},
	} {
		_, tickets, _, err := signer.signDrift(server.NewId(), server.NewId(), server.NewId(), chosen, family, false, server.NowUtc(), DefaultLagSettings())
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, []string{tickets[0].Host, tickets[1].Host}, want)
	}
}

// A start over IPv4 names the IPv4 host of each extender whose certificate
// carries its family-pinned names, and E2 takes those first in each band; the
// rest keep their measurement names (K6).
func TestLagDriftStartPinnedHosts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		pinnedHosts := map[server.Id]string{}
		for extenderId, extender := range fixture.extenders {
			if city := fixture.places[extenderId].City; city == "Berlin" || city == "Leipzig" {
				continue
			}
			addLagTestCert(t, ctx, extender, ExtenderMeasurementNames(extenderId, lagTestMeasurementZone)...)
			pinnedHosts[extenderId] = ExtenderMeasurementFamilyName(extenderId, lagTestMeasurementZone, 4)
		}
		allocated := fixture.allocate(t, now, nil)
		start := fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", now)
		connect.AssertEqual(t, start.Error, (*LagError)(nil))
		for _, ticket := range start.Tickets {
			if host, ok := pinnedHosts[ticket.ExtenderId]; ok {
				connect.AssertEqual(t, ticket.Host, host)
			} else {
				connect.AssertEqual(t, strings.Contains(ticket.Host, ".ipv4."), false)
			}
			if ticket.Position < 4 {
				if _, ok := pinnedHosts[ticket.ExtenderId]; !ok {
					t.Fatalf("the near band names the plain-named %s", ticket.Host)
				}
			}
		}
	})
}

// lag.yml's drift block is read over the design's defaults (P7): the keys it
// sets, the defaults for the rest and for a value that is not positive, the
// kill switch on unless set false, and the exclusions by id. Pure.
func TestLagDriftSettings(t *testing.T) {
	read := func(body string) *LagDriftSettings {
		pop := server.Vault.PushSimpleResource("lag.yml", []byte(body))
		defer pop()
		return &readLagSettings().Drift
	}
	defaults := read("guest_ttl: 12h\n")
	connect.AssertEqual(t, defaults.On(), true)
	connect.AssertEqual(t, defaults.CalibrationOn(), true)
	connect.AssertEqual(t, defaults.MaxRuns, 6)
	connect.AssertEqual(t, defaults.WindowRuns, 3)
	connect.AssertEqual(t, defaults.ReportWait(), 20*time.Second)
	connect.AssertEqual(t, defaults.ConfidenceDefault, 0.99)
	connect.AssertEqual(t, defaults.StopAreaTolerance, 1.17)
	connect.AssertEqual(t, defaults.DemoStartsPerHour, 30)
	connect.AssertEqual(t, len(defaults.Excluded()), 0)

	extenderId := server.NewId()
	drift := read("drift:\n  enabled: false\n  excluded_extenders: [" + extenderId.String() + ", not-an-id]\n  report_wait_ms: 5000\n  max_runs: 0\n  confidence_default: 0.5\n")
	connect.AssertEqual(t, drift.On(), false)
	connect.AssertEqual(t, drift.Excluded(), map[server.Id]bool{extenderId: true})
	connect.AssertEqual(t, drift.ReportWait(), 5*time.Second)
	connect.AssertEqual(t, drift.MaxRuns, 6)
	connect.AssertEqual(t, drift.ConfidenceDefault, 0.99)
	connect.AssertEqual(t, read("drift:\n  enabled: true\n").On(), true)
	connect.AssertEqual(t, read("drift:\n  calibration_enabled: false\n").CalibrationOn(), false)

	// the pinned origins (K6, P5)
	connect.AssertEqual(t, defaults.PinOrigin(4), "https://api-v4.bringyour.com")
	connect.AssertEqual(t, defaults.PinOrigin(6), "https://api-v6.bringyour.com")
	pinned := read("drift:\n  pin_origin_ipv4: https://api4.example.com/\n  pin_origin_ipv6: \" \"\n")
	connect.AssertEqual(t, pinned.PinOrigin(4), "https://api4.example.com")
	connect.AssertEqual(t, pinned.PinOrigin(6), "https://api-v6.bringyour.com")

	// a lag.yml that doesn't read leaves drift tests off, never on by the
	// defaults, whatever it meant to say (P7)
	for _, body := range []string{"drift:\n  enabled: false\n  max_runs: [\n", "drift:\n  enabled: maybe\n", "guest_ttl: [\n"} {
		connect.AssertEqual(t, read(body).On(), false)
	}
}

// The drift counters' labels as the dashboard reads them (P7, ops runbook):
// a run's outcome with the browser's stop, none without a finish, and the
// time from its finish only for a run the frame finished; a verdict by
// network and verdict, and by verdict and reason. Pure.
func TestLagDriftMetrics(t *testing.T) {
	counted := testutil.ToFloat64(lagDriftRunOutcomesCounter.WithLabelValues(LagDriftRunOutcomeCounted, LagDriftStopConverged))
	none := testutil.ToFloat64(lagDriftRunOutcomesCounter.WithLabelValues(LagDriftRunOutcomeReportsMissing, "none"))
	observations := testutil.CollectAndCount(lagDriftProcessingHistogram)
	finishToComplete := 1500 * time.Millisecond
	countLagDriftRunCompleted(LagDriftRunOutcomeCounted, LagDriftStopConverged, &finishToComplete)
	countLagDriftRunCompleted(LagDriftRunOutcomeReportsMissing, "", nil)
	connect.AssertEqual(t, testutil.ToFloat64(lagDriftRunOutcomesCounter.WithLabelValues(LagDriftRunOutcomeCounted, LagDriftStopConverged)), counted+1)
	connect.AssertEqual(t, testutil.ToFloat64(lagDriftRunOutcomesCounter.WithLabelValues(LagDriftRunOutcomeReportsMissing, "none")), none+1)
	connect.AssertEqual(t, testutil.CollectAndCount(lagDriftProcessingHistogram), observations)

	networkId := server.NewId()
	reasons := testutil.ToFloat64(lagDriftVerdictReasonsCounter.WithLabelValues(LagDriftVerdictDisagree, LagDriftReasonRegionOutside))
	countLagDriftVerdict(networkId, LagDriftVerdictDisagree, LagDriftReasonRegionOutside)
	connect.AssertEqual(t, testutil.ToFloat64(lagDriftVerdictsCounter.WithLabelValues(networkId.String(), LagDriftVerdictDisagree)), 1.0)
	connect.AssertEqual(t, testutil.ToFloat64(lagDriftVerdictReasonsCounter.WithLabelValues(LagDriftVerdictDisagree, LagDriftReasonRegionOutside)), reasons+1)

	at := time.Unix(1790000000, 0)
	SetLagDriftSweepTimestamp(at)
	connect.AssertEqual(t, testutil.ToFloat64(lagDriftSweepTimestampGauge), float64(at.Unix()))
}

// The drift country (V6, G3): a fit in an area two countries claim reports the
// claimant that is the IP country, though the borders name the other first;
// elsewhere, the country containing the fit, the nearest within 300 km, or
// none at sea. Pure.
func TestLagDriftCountryOf(t *testing.T) {
	square := func(code string, minLat float64, minLon float64, maxLat float64, maxLon float64) lagdrift.Country {
		return lagdrift.Country{Code: code, Polygons: [][][]lagdrift.Point{{{
			{Lat: minLat, Lon: minLon}, {Lat: minLat, Lon: maxLon}, {Lat: maxLat, Lon: maxLon}, {Lat: maxLat, Lon: minLon},
		}}}}
	}
	// both claim the strip between longitudes 9 and 10
	borders, err := lagdrift.NewBorders("test", []lagdrift.Country{
		square("aa", 0, 0, 10, 10),
		square("bb", 0, 9, 10, 20),
	})
	connect.AssertEqual(t, err, nil)
	loaded := &lagDriftLoadedBorders{borders: borders}
	disputed := lagdrift.Point{Lat: 5, Lon: 9.5}

	connect.AssertEqual(t, borders.CountryAt(disputed, lagdrift.DriftCountryWithinKm), "aa")
	connect.AssertEqual(t, lagDriftCountryOf(loaded, borders.Country("bb"), disputed), "bb")
	connect.AssertEqual(t, lagDriftCountryOf(loaded, borders.Country("aa"), disputed), "aa")
	// outside the IP country: the country holding the fit
	connect.AssertEqual(t, lagDriftCountryOf(loaded, borders.Country("aa"), lagdrift.Point{Lat: 5, Lon: 15}), "bb")
	// near one, and at sea
	connect.AssertEqual(t, lagDriftCountryOf(loaded, nil, lagdrift.Point{Lat: 11, Lon: 5}), "aa")
	connect.AssertEqual(t, lagDriftCountryOf(loaded, nil, lagdrift.Point{Lat: -30, Lon: -30}), "")
	connect.AssertEqual(t, lagDriftCountryOf(nil, nil, disputed), "")
}
