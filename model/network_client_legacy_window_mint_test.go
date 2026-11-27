package model

// The old app window's mint (network_client_legacy_window_mint.go): a client
// token's request with only description and device_spec, the caller's own
// device's, is a child of the caller's own client; every other mint without a
// source from a client token is still refused (AUTHZ1.md). The first tests are
// pure; the Db tests drive AuthNetworkClient end to end.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Sets one request field to a value that is not its zero value.
func legacyWindowMintTestSetField(t *testing.T, field reflect.Value, name string) {
	t.Helper()
	switch field.Kind() {
	case reflect.String:
		field.SetString("synthetic")
	case reflect.Bool:
		field.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		field.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		field.SetUint(1)
	case reflect.Pointer:
		field.Set(reflect.New(field.Type().Elem()))
	case reflect.Slice:
		field.Set(reflect.MakeSlice(field.Type(), 1, 1))
	case reflect.Map:
		field.Set(reflect.MakeMap(field.Type()))
	default:
		t.Fatalf("request field %s has kind %s: teach this test to set it", name, field.Kind())
	}
}

// The old window's request, as app builds v2025.7.4 and older send it.
func legacyWindowMintTestArgs() *AuthNetworkClientArgs {
	return &AuthNetworkClientArgs{
		Description: "synthetic phone",
		DeviceSpec:  "synthetic spec",
	}
}

func TestLegacyWindowMintArgsAreOnlyDescriptionAndDeviceSpec(t *testing.T) {
	connect.AssertEqual(t, isLegacyWindowMintArgs(legacyWindowMintTestArgs()), true)
	connect.AssertEqual(t, isLegacyWindowMintArgs(&AuthNetworkClientArgs{DeviceSpec: "synthetic spec"}), true)
	connect.AssertEqual(t, isLegacyWindowMintArgs(&AuthNetworkClientArgs{Description: "synthetic phone"}), false)
	connect.AssertEqual(t, isLegacyWindowMintArgs(nil), false)

	// every other field, including one added later, makes it another request
	argsType := reflect.TypeOf(AuthNetworkClientArgs{})
	checked := 0
	for i := 0; i < argsType.NumField(); i += 1 {
		name := argsType.Field(i).Name
		if name == "Description" || name == "DeviceSpec" {
			continue
		}
		args := legacyWindowMintTestArgs()
		legacyWindowMintTestSetField(t, reflect.ValueOf(args).Elem().Field(i), name)
		if isLegacyWindowMintArgs(args) {
			t.Errorf("a request that sets %s was taken as the old window's", name)
		}
		checked += 1
	}
	if checked == 0 {
		t.Fatal("the request has no fields besides description and device_spec")
	}
}

// Synthetic specs in the old Android window's form, "<os version>
// <fingerprint>": one device on two os builds, and another device. None is a
// real device's.
const (
	legacyWindowMintTestAndroidSpec        = "14 examplebrand/exampleproduct_xx/exampledevice:14/AB1A.240101.001/X100XXU1AXA1:user/release-keys"
	legacyWindowMintTestAndroidUpdatedSpec = "16 examplebrand/exampleproduct_xx/exampledevice:16/CD2A.260101.002/X100XXU9CZB2:user/release-keys"
	legacyWindowMintTestOtherAndroidSpec   = "16 examplebrand/otherproduct_xx/otherdevice:16/CD2A.260101.002/Y200XXU9CZB2:user/release-keys"
)

func TestLegacyWindowDeviceSpecIsTheSameAndroidDeviceOnAnyOsBuild(t *testing.T) {
	for _, c := range []struct {
		name             string
		deviceSpec       string
		storedDeviceSpec string
		want             bool
	}{
		{name: "the stored spec", deviceSpec: legacyWindowMintTestAndroidSpec, storedDeviceSpec: legacyWindowMintTestAndroidSpec, want: true},
		{name: "a newer os build of the device", deviceSpec: legacyWindowMintTestAndroidUpdatedSpec, storedDeviceSpec: legacyWindowMintTestAndroidSpec, want: true},
		{name: "an older os build of the device", deviceSpec: legacyWindowMintTestAndroidSpec, storedDeviceSpec: legacyWindowMintTestAndroidUpdatedSpec, want: true},
		{name: "a security patch of the device", deviceSpec: "14 examplebrand/exampleproduct_xx/exampledevice:14/AB1A.240101.001/X100XXU2AXB1:user/release-keys", storedDeviceSpec: legacyWindowMintTestAndroidSpec, want: true},
		{name: "another device", deviceSpec: legacyWindowMintTestOtherAndroidSpec, storedDeviceSpec: legacyWindowMintTestAndroidSpec, want: false},
		{name: "another brand", deviceSpec: "16 otherbrand/exampleproduct_xx/exampledevice:16/CD2A.260101.002/X100XXU9CZB2:user/release-keys", storedDeviceSpec: legacyWindowMintTestAndroidSpec, want: false},
		{name: "another build type", deviceSpec: "16 examplebrand/exampleproduct_xx/exampledevice:16/CD2A.260101.002/X100XXU9CZB2:userdebug/test-keys", storedDeviceSpec: legacyWindowMintTestAndroidSpec, want: false},
		{name: "a stored spec in the current app's form", deviceSpec: legacyWindowMintTestAndroidUpdatedSpec, storedDeviceSpec: "16 Examplebrand Example Phone", want: false},
		{name: "specs in another form", deviceSpec: "after", storedDeviceSpec: "before", want: false},
		{name: "a fingerprint without an os version", deviceSpec: "examplebrand/exampleproduct_xx/exampledevice:16/CD2A.260101.002/X100XXU9CZB2:user/release-keys", storedDeviceSpec: legacyWindowMintTestAndroidSpec, want: false},
		{name: "a fingerprint missing a part", deviceSpec: "16 examplebrand/exampledevice:16/CD2A.260101.002/X100XXU9CZB2:user/release-keys", storedDeviceSpec: "14 examplebrand/exampledevice:14/AB1A.240101.001/X100XXU1AXA1:user/release-keys", want: false},
		{name: "a fingerprint with an empty part", deviceSpec: "16 examplebrand//exampledevice:16/CD2A.260101.002/X100XXU9CZB2:user/release-keys", storedDeviceSpec: "14 examplebrand//exampledevice:14/AB1A.240101.001/X100XXU1AXA1:user/release-keys", want: false},
		{name: "text after the fingerprint", deviceSpec: legacyWindowMintTestAndroidUpdatedSpec + " synthetic", storedDeviceSpec: legacyWindowMintTestAndroidSpec, want: false},
		{name: "no spec", deviceSpec: "", storedDeviceSpec: "", want: false},
	} {
		if got := isLegacyWindowDeviceSpecOfDevice(c.deviceSpec, c.storedDeviceSpec); got != c.want {
			t.Errorf("%s: %t, want %t", c.name, got, c.want)
		}
	}
}

func TestClientTokenMintSourceTakesTheOldWindowRequestAsAChildOfTheCaller(t *testing.T) {
	callerClientId := server.NewId()
	networkId := server.NewId()
	refusalsBefore := testutil.ToFloat64(networkClientTopLevelMintRefusals)

	args := legacyWindowMintTestArgs()
	refusal, legacyWindowMint := applyClientTokenMintSource(args, callerClientId, networkId, true, time.Unix(0, 0))
	if refusal != nil {
		t.Fatalf("the old window's request was refused: %+v", refusal)
	}
	connect.AssertEqual(t, legacyWindowMint, true)
	// its source is the caller's own client, and no other
	if args.SourceClientId == nil || *args.SourceClientId != callerClientId {
		t.Fatalf("the source is %v, want the caller %s", args.SourceClientId, callerClientId)
	}
	connect.AssertEqual(t, testutil.ToFloat64(networkClientTopLevelMintRefusals), refusalsBefore)
}

func TestClientTokenMintSourceRefusesEveryOtherRequestWithoutASource(t *testing.T) {
	callerClientId := server.NewId()
	networkId := server.NewId()
	proxyArgs := legacyWindowMintTestArgs()
	proxyArgs.ProxyConfig = &ProxyConfig{}
	rolesArgs := legacyWindowMintTestArgs()
	rolesArgs.Roles = []string{"synthetic"}
	principalArgs := legacyWindowMintTestArgs()
	principalArgs.Principal = "synthetic"
	provideArgs := legacyWindowMintTestArgs()
	provideArgs.ProvideIntent = true
	localeArgs := legacyWindowMintTestArgs()
	localeArgs.Locales = []string{"en-US"}

	for _, c := range []struct {
		name                    string
		args                    *AuthNetworkClientArgs
		legacyWindowMintAllowed bool
	}{
		{name: "a proxy", args: proxyArgs, legacyWindowMintAllowed: true},
		{name: "roles", args: rolesArgs, legacyWindowMintAllowed: true},
		{name: "a principal", args: principalArgs, legacyWindowMintAllowed: true},
		{name: "provide intent", args: provideArgs, legacyWindowMintAllowed: true},
		{name: "locales", args: localeArgs, legacyWindowMintAllowed: true},
		{name: "no device_spec", args: &AuthNetworkClientArgs{Description: "synthetic phone"}, legacyWindowMintAllowed: true},
		{name: "the old shape where it is not allowed", args: legacyWindowMintTestArgs(), legacyWindowMintAllowed: false},
	} {
		refusalsBefore := testutil.ToFloat64(networkClientTopLevelMintRefusals)
		refusal, legacyWindowMint := applyClientTokenMintSource(c.args, callerClientId, networkId, c.legacyWindowMintAllowed, time.Unix(0, 0))
		if refusal == nil || refusal.Message != clientTokenTopLevelMintRefusalMessage {
			t.Fatalf("%s: answered %+v, want the refusal %q", c.name, refusal, clientTokenTopLevelMintRefusalMessage)
		}
		connect.AssertEqual(t, legacyWindowMint, false)
		if c.args.SourceClientId != nil {
			t.Fatalf("%s: the refused request was given a source", c.name)
		}
		connect.AssertEqual(t, testutil.ToFloat64(networkClientTopLevelMintRefusals), refusalsBefore+1)
	}
}

func TestClientTokenMintSourceRefusesAnotherClientsSource(t *testing.T) {
	callerClientId := server.NewId()
	otherClientId := server.NewId()
	refusalsBefore := testutil.ToFloat64(networkClientTopLevelMintRefusals)

	args := legacyWindowMintTestArgs()
	args.SourceClientId = &otherClientId
	refusal, legacyWindowMint := applyClientTokenMintSource(args, callerClientId, server.NewId(), true, time.Unix(0, 0))
	if refusal == nil || refusal.Message != "Client does not exist." {
		t.Fatalf("a child of another client answered %+v", refusal)
	}
	connect.AssertEqual(t, legacyWindowMint, false)
	connect.AssertEqual(t, *args.SourceClientId, otherClientId)
	connect.AssertEqual(t, testutil.ToFloat64(networkClientTopLevelMintRefusals), refusalsBefore)

	// its own client as the source passes on to the child path unchanged
	args = legacyWindowMintTestArgs()
	args.SourceClientId = &callerClientId
	refusal, legacyWindowMint = applyClientTokenMintSource(args, callerClientId, server.NewId(), true, time.Unix(0, 0))
	if refusal != nil {
		t.Fatalf("a child of its own client answered %+v", refusal)
	}
	connect.AssertEqual(t, legacyWindowMint, false)
}

func TestLegacyWindowMintResultIsCounted(t *testing.T) {
	clientId := server.NewId()
	networkId := server.NewId()
	childClientId := server.NewId()
	mintsBefore := testutil.ToFloat64(networkClientLegacyWindowMints)
	refusalsBefore := testutil.ToFloat64(networkClientTopLevelMintRefusals)

	// a minted child
	observeLegacyWindowMintResult(clientId, networkId, &AuthNetworkClientResult{ClientId: &childClientId}, nil, time.Unix(0, 0))
	connect.AssertEqual(t, testutil.ToFloat64(networkClientLegacyWindowMints), mintsBefore+1)
	connect.AssertEqual(t, testutil.ToFloat64(networkClientTopLevelMintRefusals), refusalsBefore)

	// a device_spec that is not the caller's device's
	refusal := &AuthNetworkClientResult{Error: &AuthNetworkClientError{Message: clientTokenTopLevelMintRefusalMessage}}
	observeLegacyWindowMintResult(clientId, networkId, refusal, nil, time.Unix(0, 0))
	connect.AssertEqual(t, testutil.ToFloat64(networkClientLegacyWindowMints), mintsBefore+1)
	connect.AssertEqual(t, testutil.ToFloat64(networkClientTopLevelMintRefusals), refusalsBefore+1)

	// any other answer is not this path's to count
	inactive := &AuthNetworkClientResult{Error: &AuthNetworkClientError{Message: "Client does not exist."}}
	observeLegacyWindowMintResult(clientId, networkId, inactive, nil, time.Unix(0, 0))
	observeLegacyWindowMintResult(clientId, networkId, nil, ErrClientParentInactive, time.Unix(0, 0))
	connect.AssertEqual(t, testutil.ToFloat64(networkClientLegacyWindowMints), mintsBefore+1)
	connect.AssertEqual(t, testutil.ToFloat64(networkClientTopLevelMintRefusals), refusalsBefore+1)
}

func TestClientTokenSourcelessMintLogIsOneLinePerClientPerInterval(t *testing.T) {
	settings := defaultClientTokenSourcelessMintLogSettings()
	settings.Interval = time.Hour
	settings.MaxClientCount = 4
	log := newClientTokenSourcelessMintLog(settings)

	clientId := server.NewId()
	otherClientId := server.NewId()
	start := time.Unix(1_000_000, 0)
	connect.AssertEqual(t, log.due(clientId, start), true)
	connect.AssertEqual(t, log.due(clientId, start.Add(time.Second)), false)
	connect.AssertEqual(t, log.due(clientId, start.Add(time.Hour-time.Second)), false)
	// another client has its own line
	connect.AssertEqual(t, log.due(otherClientId, start.Add(time.Second)), true)
	// a full interval later the client gets its next line, and the interval
	// restarts from it
	connect.AssertEqual(t, log.due(clientId, start.Add(time.Hour)), true)
	connect.AssertEqual(t, log.due(clientId, start.Add(time.Hour+time.Minute)), false)

	// a non-positive interval logs every mint
	settings = defaultClientTokenSourcelessMintLogSettings()
	settings.Interval = 0
	log = newClientTokenSourcelessMintLog(settings)
	connect.AssertEqual(t, log.due(clientId, start), true)
	connect.AssertEqual(t, log.due(clientId, start), true)
}

func TestClientTokenSourcelessMintLogStaysBounded(t *testing.T) {
	settings := defaultClientTokenSourcelessMintLogSettings()
	settings.MaxClientCount = 2
	log := newClientTokenSourcelessMintLog(settings)
	start := time.Unix(1_000_000, 0)
	for i := 0; i < 10; i += 1 {
		connect.AssertEqual(t, log.due(server.NewId(), start), true)
		if settings.MaxClientCount < log.clientLogTimes.Len() {
			t.Fatalf("the log remembers %d clients, more than %d", log.clientLogTimes.Len(), settings.MaxClientCount)
		}
	}
}

// Reads a minted client's device, source, network and whether it is active.
func legacyWindowMintTestChild(ctx context.Context, childClientId server.Id) (deviceId server.Id, sourceClientId *server.Id, networkId server.Id, active bool) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT device_id, source_client_id, network_id, active FROM network_client WHERE client_id = $1`,
			childClientId,
		).Scan(&deviceId, &sourceClientId, &networkId, &active))
	})
	return
}

func TestAuthNetworkClientOldWindowRequestIsAChildOfTheCallersOwnClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, userSession := authClientTestNetwork(ctx, "test")
		// the caller's device spec is "before" (authClientTestClient)
		clientId, deviceId := authClientTestClient(ctx, t, userSession)
		clientSession := clientTokenTestSession(ctx, userSession, clientId, deviceId)

		mintsBefore := testutil.ToFloat64(networkClientLegacyWindowMints)
		refusalsBefore := testutil.ToFloat64(networkClientTopLevelMintRefusals)
		clientCountBefore, deviceCountBefore, _ := authClientTestNetworkCounts(ctx, networkId)

		result, err := AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "before",
				DeviceSpec:  "before",
			},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		if result == nil || result.Error != nil || result.ClientId == nil || result.ByClientJwt == nil {
			t.Fatalf("the old window's request answered %+v, want a child", result)
		}

		// a child of the caller, on the caller's device and network; no new
		// device and no top-level client
		childDeviceId, childSourceClientId, childNetworkId, childActive := legacyWindowMintTestChild(ctx, *result.ClientId)
		connect.AssertEqual(t, childDeviceId, deviceId)
		if childSourceClientId == nil || *childSourceClientId != clientId {
			t.Fatalf("the child's source is %v, want the caller %s", childSourceClientId, clientId)
		}
		connect.AssertEqual(t, childNetworkId, networkId)
		connect.AssertEqual(t, childActive, true)
		clientCount, deviceCount, _ := authClientTestNetworkCounts(ctx, networkId)
		connect.AssertEqual(t, clientCount, clientCountBefore+1)
		connect.AssertEqual(t, deviceCount, deviceCountBefore)

		connect.AssertEqual(t, testutil.ToFloat64(networkClientLegacyWindowMints), mintsBefore+1)
		connect.AssertEqual(t, testutil.ToFloat64(networkClientTopLevelMintRefusals), refusalsBefore)

		// its token removes it, as a child of its own
		removeResult, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: *result.ClientId}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removeResult.Error, nil)
		connect.AssertEqual(t, clientTokenTestActive(ctx, *result.ClientId), false)

		// and it retires with its parent, as every child does
		result, err = AuthNetworkClient(&AuthNetworkClientArgs{Description: "before", DeviceSpec: "before"}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		removeResult, err = RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: clientId}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removeResult.Error, nil)
		connect.AssertEqual(t, clientTokenTestActive(ctx, clientId), false)
		connect.AssertEqual(t, clientTokenTestActive(ctx, *result.ClientId), false)
	})
}

// An old Android build whose os updated after its device row was written
// sends its window's spec on the new os build, and was refused for good: its
// only mints are its window's, refused before the row is refreshed (support
// inbox 8183).
func TestAuthNetworkClientOldWindowRequestAfterAnOsUpdateIsAChildOfTheCallersOwnClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, userSession := authClientTestNetwork(ctx, "test")
		created, err := AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "synthetic phone",
				DeviceSpec:  legacyWindowMintTestAndroidSpec,
			},
			userSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, created.Error, nil)
		clientId := *created.ClientId
		var deviceId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT device_id FROM network_client WHERE client_id = $1`,
				clientId,
			).Scan(&deviceId))
		})
		clientSession := clientTokenTestSession(ctx, userSession, clientId, deviceId)
		deviceSpec := func() (deviceSpec string) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT device_spec FROM device WHERE device_id = $1`, deviceId).Scan(&deviceSpec))
			})
			return
		}

		mintsBefore := testutil.ToFloat64(networkClientLegacyWindowMints)
		refusalsBefore := testutil.ToFloat64(networkClientTopLevelMintRefusals)
		clientCountBefore, deviceCountBefore, _ := authClientTestNetworkCounts(ctx, networkId)

		result, err := AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "synthetic phone",
				DeviceSpec:  legacyWindowMintTestAndroidUpdatedSpec,
			},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		if result == nil || result.Error != nil || result.ClientId == nil || result.ByClientJwt == nil {
			t.Fatalf("the old window's request on the updated os answered %+v, want a child", result)
		}
		childDeviceId, childSourceClientId, childNetworkId, childActive := legacyWindowMintTestChild(ctx, *result.ClientId)
		connect.AssertEqual(t, childDeviceId, deviceId)
		if childSourceClientId == nil || *childSourceClientId != clientId {
			t.Fatalf("the child's source is %v, want the caller %s", childSourceClientId, clientId)
		}
		connect.AssertEqual(t, childNetworkId, networkId)
		connect.AssertEqual(t, childActive, true)
		clientCount, deviceCount, _ := authClientTestNetworkCounts(ctx, networkId)
		connect.AssertEqual(t, clientCount, clientCountBefore+1)
		connect.AssertEqual(t, deviceCount, deviceCountBefore)
		connect.AssertEqual(t, testutil.ToFloat64(networkClientLegacyWindowMints), mintsBefore+1)
		connect.AssertEqual(t, testutil.ToFloat64(networkClientTopLevelMintRefusals), refusalsBefore)
		// the mint refreshed the device's spec, so the next window matches it as is
		connect.AssertEqual(t, deviceSpec(), legacyWindowMintTestAndroidUpdatedSpec)

		// another device's spec is still refused, writes nothing and counts
		refusalsBefore = testutil.ToFloat64(networkClientTopLevelMintRefusals)
		mintsBefore = testutil.ToFloat64(networkClientLegacyWindowMints)
		clientCountBefore, deviceCountBefore, _ = authClientTestNetworkCounts(ctx, networkId)
		result, err = AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "synthetic phone",
				DeviceSpec:  legacyWindowMintTestOtherAndroidSpec,
			},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		if result == nil || result.Error == nil || result.Error.Message != clientTokenTopLevelMintRefusalMessage {
			t.Fatalf("another device's spec answered %+v, want the refusal %q", result, clientTokenTopLevelMintRefusalMessage)
		}
		if result.ClientId != nil || result.ByClientJwt != nil {
			t.Fatal("the refusal returned credentials")
		}
		clientCount, deviceCount, _ = authClientTestNetworkCounts(ctx, networkId)
		connect.AssertEqual(t, clientCount, clientCountBefore)
		connect.AssertEqual(t, deviceCount, deviceCountBefore)
		connect.AssertEqual(t, testutil.ToFloat64(networkClientTopLevelMintRefusals), refusalsBefore+1)
		connect.AssertEqual(t, testutil.ToFloat64(networkClientLegacyWindowMints), mintsBefore)
		connect.AssertEqual(t, deviceSpec(), legacyWindowMintTestAndroidUpdatedSpec)
	})
}

func TestAuthNetworkClientRefusesClientTokenMintsThatAreNotTheOldWindowsRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, userSession := authClientTestNetwork(ctx, "test")
		clientId, deviceId := authClientTestClient(ctx, t, userSession)
		otherClientId, _ := authClientTestClient(ctx, t, userSession)
		clientSession := clientTokenTestSession(ctx, userSession, clientId, deviceId)

		withRoles := &AuthNetworkClientArgs{Description: "before", DeviceSpec: "before", Roles: []string{"synthetic"}}
		withPrincipal := &AuthNetworkClientArgs{Description: "before", DeviceSpec: "before", Principal: "synthetic"}
		withProvideIntent := &AuthNetworkClientArgs{Description: "before", DeviceSpec: "before", ProvideIntent: true}
		withTimeZone := &AuthNetworkClientArgs{Description: "before", DeviceSpec: "before", TimeZone: "UTC"}
		withLocales := &AuthNetworkClientArgs{Description: "before", DeviceSpec: "before", Locale: "en-US", Locales: []string{"en-US"}}
		withProxy := &AuthNetworkClientArgs{Description: "before", DeviceSpec: "before", ProxyConfig: &ProxyConfig{}}

		for _, c := range []struct {
			name    string
			args    *AuthNetworkClientArgs
			message string
			counted bool
		}{
			{name: "another device's spec", args: &AuthNetworkClientArgs{Description: "before", DeviceSpec: "after"}, message: clientTokenTopLevelMintRefusalMessage, counted: true},
			{name: "no device spec", args: &AuthNetworkClientArgs{Description: "before"}, message: clientTokenTopLevelMintRefusalMessage, counted: true},
			{name: "roles", args: withRoles, message: clientTokenTopLevelMintRefusalMessage, counted: true},
			{name: "a principal", args: withPrincipal, message: clientTokenTopLevelMintRefusalMessage, counted: true},
			{name: "provide intent", args: withProvideIntent, message: clientTokenTopLevelMintRefusalMessage, counted: true},
			{name: "a time zone", args: withTimeZone, message: clientTokenTopLevelMintRefusalMessage, counted: true},
			{name: "locales", args: withLocales, message: clientTokenTopLevelMintRefusalMessage, counted: true},
			{name: "a proxy", args: withProxy, message: clientTokenTopLevelMintRefusalMessage, counted: true},
			{name: "a child of another client", args: &AuthNetworkClientArgs{SourceClientId: &otherClientId, Description: "before", DeviceSpec: "before"}, message: "Client does not exist.", counted: false},
		} {
			refusalsBefore := testutil.ToFloat64(networkClientTopLevelMintRefusals)
			mintsBefore := testutil.ToFloat64(networkClientLegacyWindowMints)
			clientCountBefore, deviceCountBefore, proxyCountBefore := authClientTestNetworkCounts(ctx, networkId)
			result, err := AuthNetworkClient(c.args, clientSession)
			connect.AssertEqual(t, err, nil)
			if result == nil || result.Error == nil || result.Error.Message != c.message {
				t.Fatalf("%s: answered %+v, want the refusal %q", c.name, result, c.message)
			}
			if result.ClientId != nil || result.ByClientJwt != nil {
				t.Fatalf("%s: the refusal returned credentials", c.name)
			}
			clientCount, deviceCount, proxyCount := authClientTestNetworkCounts(ctx, networkId)
			if clientCount != clientCountBefore || deviceCount != deviceCountBefore || proxyCount != proxyCountBefore {
				t.Fatalf("%s: created %d clients, %d devices and %d proxies", c.name, clientCount-clientCountBefore, deviceCount-deviceCountBefore, proxyCount-proxyCountBefore)
			}
			wantRefusals := refusalsBefore
			if c.counted {
				wantRefusals += 1
			}
			connect.AssertEqual(t, testutil.ToFloat64(networkClientTopLevelMintRefusals), wantRefusals)
			connect.AssertEqual(t, testutil.ToFloat64(networkClientLegacyWindowMints), mintsBefore)
		}
		// the device's spec is unchanged by the refused requests
		var deviceSpec string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT device_spec FROM device WHERE device_id = $1`, deviceId).Scan(&deviceSpec))
		})
		connect.AssertEqual(t, deviceSpec, "before")
	})
}

func TestAuthNetworkClientOldWindowRequestNeedsTheCallerActive(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, userSession := authClientTestNetwork(ctx, "test")
		clientId, deviceId := authClientTestClient(ctx, t, userSession)
		clientSession := clientTokenTestSession(ctx, userSession, clientId, deviceId)

		removeResult, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: clientId}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removeResult.Error, nil)

		// an inactive caller gets no child, by the old window's request just as
		// by naming itself as the source; a refusal raised by the token's state
		// check is no child either
		mint := func(args *AuthNetworkClientArgs) (result *AuthNetworkClientResult) {
			defer func() {
				if recover() != nil {
					result = nil
				}
			}()
			result, _ = AuthNetworkClient(args, clientSession)
			return
		}
		mintsBefore := testutil.ToFloat64(networkClientLegacyWindowMints)
		for _, args := range []*AuthNetworkClientArgs{
			{Description: "before", DeviceSpec: "before"},
			{SourceClientId: &clientId, Description: "before", DeviceSpec: "before"},
		} {
			clientCountBefore, deviceCountBefore, _ := authClientTestNetworkCounts(ctx, networkId)
			result := mint(args)
			if result != nil && (result.ClientId != nil || result.ByClientJwt != nil) {
				t.Fatalf("an inactive caller got a child: %+v", result)
			}
			clientCount, deviceCount, _ := authClientTestNetworkCounts(ctx, networkId)
			connect.AssertEqual(t, clientCount, clientCountBefore)
			connect.AssertEqual(t, deviceCount, deviceCountBefore)
		}
		connect.AssertEqual(t, testutil.ToFloat64(networkClientLegacyWindowMints), mintsBefore)
	})
}
