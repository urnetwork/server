package model

// A client token mints only children of its own client (AUTHZ1.md, server
// 587bacf44). App builds v2025.7.4 and older mint every VPN window client with
// their client token and only the device's description and device_spec, with
// no source_client_id: the server of their day took the source from the token
// (connect 3d0509f0 v2025/ip_remote_multi_client.go NewClientArgs; builds from
// v2025.7.23 send source_client_id). AUTHZ1 refused that request as a
// top-level mint, so those builds could no longer connect at all (support
// inbox 8183).
//
// The old window's exact request, whose device_spec is the caller's own
// device's, is now made a child of the caller's own client, as if it had named
// the caller as its source: the path AUTHZ1 already allows, through the same
// checks, so it adds no capability. Every other mint without a source from a
// client token is refused with the same message as before.
//
// Both outcomes are counted, and logged with the client and its network at
// most once per client per interval. The refusal of a request that is not the
// old window's does no database work; the old window's request is checked
// against the caller's device inside the mint's own transaction.
//
// The old Android window's device_spec is "<os version> <Build.FINGERPRINT>"
// (android v2025.6.30 MainApplication.kt deviceSpec), and every os update
// changes it. The device row keeps the spec of the device's last accepted
// mint, and an old build's only mints are its window's, which this check
// gates before the row is refreshed (network_client_model.go). So an exact
// comparison refused, for good, every old Android build whose os had updated
// since its device row was written: of 21 callers refused on 2026-11-26, 18
// had sent window specs of their own device on a newer os build. A spec of
// the same Android device, its brand, product, device, type and tags, is the
// caller's device's; its accepted mint then refreshes the row as every mint
// does.

import (
	"context"
	"reflect"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// The answer to a client token's mint without a source (AUTHZ1).
const clientTokenTopLevelMintRefusalMessage = "A client token cannot create a top-level client."

var networkClientTopLevelMintRefusals = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "network_client",
	Name:      "top_level_mint_refusals_total",
	Help:      "Client-token mints without a source_client_id refused because a client token cannot create a top-level client (AUTHZ1), including old-window requests whose device_spec is not the caller's device's",
})

var networkClientLegacyWindowMints = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "network_client",
	Name:      "legacy_window_mints_total",
	Help:      "Window clients minted for app builds v2025.7.4 and older: a client token's request with only description and device_spec, the caller's device's, made a child of the caller's own client",
})

// Registers both counters with the default registry.
func init() {
	prometheus.MustRegister(networkClientTopLevelMintRefusals, networkClientLegacyWindowMints)
}

// The log of client-token mints without a source.
type clientTokenSourcelessMintLogSettings struct {
	// The least time between two lines for one client; non-positive logs every mint.
	Interval time.Duration
	// The most clients remembered; a full set is forgotten at once, which costs
	// at most one extra line per client.
	MaxClientCount int64
}

// An hour keeps a stuck device's retries (several a second) to one line an hour
// while the log still names every client and network of a day.
func defaultClientTokenSourcelessMintLogSettings() *clientTokenSourcelessMintLogSettings {
	return &clientTokenSourcelessMintLogSettings{
		Interval:       time.Hour,
		MaxClientCount: 16 * 1024,
	}
}

// The per-client limit of log lines for one outcome. Safe for concurrent use;
// two racing mints of one client may both log.
type clientTokenSourcelessMintLog struct {
	settings       *clientTokenSourcelessMintLogSettings
	clientLogTimes *boundedClientMap[time.Time]
}

func newClientTokenSourcelessMintLog(settings *clientTokenSourcelessMintLogSettings) *clientTokenSourcelessMintLog {
	return &clientTokenSourcelessMintLog{
		settings:       settings,
		clientLogTimes: newBoundedClientMap[time.Time](settings.MaxClientCount),
	}
}

// Reports whether a line for the client is due at now, and if so records now
// as its last line.
func (self *clientTokenSourcelessMintLog) due(clientId server.Id, now time.Time) bool {
	if 0 < self.settings.Interval {
		if lastLogTime, ok := self.clientLogTimes.Load(clientId); ok && now.Sub(lastLogTime) < self.settings.Interval {
			return false
		}
	}
	self.clientLogTimes.Store(clientId, now)
	return true
}

var clientTokenTopLevelMintRefusalLogs = newClientTokenSourcelessMintLog(defaultClientTokenSourcelessMintLogSettings())
var clientTokenLegacyWindowMintLogs = newClientTokenSourcelessMintLog(defaultClientTokenSourcelessMintLogSettings())

// The old window's request: a device_spec, a description, and nothing else.
// Any other field set, including one added to the request later, makes it
// another request.
func isLegacyWindowMintArgs(authClient *AuthNetworkClientArgs) bool {
	if authClient == nil || authClient.DeviceSpec == "" {
		return false
	}
	authClientValue := reflect.ValueOf(*authClient)
	authClientType := authClientValue.Type()
	for i := 0; i < authClientValue.NumField(); i += 1 {
		switch authClientType.Field(i).Name {
		case "Description", "DeviceSpec":
			continue
		}
		if !authClientValue.Field(i).IsZero() {
			return false
		}
	}
	return true
}

// Applies AUTHZ1 to a client token's mint without a client_id. A mint without a
// source is refused, unless legacyWindowMintAllowed and it is the old window's
// request: then its source is set to the caller's own client in place, so every
// later step, the controller's included, treats it as the child mint it is. A
// source other than the caller is refused as a client that does not exist.
// No database work.
func applyClientTokenMintSource(
	authClient *AuthNetworkClientArgs,
	callerClientId server.Id,
	networkId server.Id,
	legacyWindowMintAllowed bool,
	now time.Time,
) (refusal *AuthNetworkClientError, legacyWindowMint bool) {
	if authClient.SourceClientId == nil {
		if legacyWindowMintAllowed && isLegacyWindowMintArgs(authClient) {
			sourceClientId := callerClientId
			authClient.SourceClientId = &sourceClientId
			return nil, true
		}
		observeClientTokenTopLevelMintRefusal(callerClientId, networkId, "the request is not the old window's", now)
		return &AuthNetworkClientError{Message: clientTokenTopLevelMintRefusalMessage}, false
	}
	if *authClient.SourceClientId != callerClientId {
		return &AuthNetworkClientError{Message: "Client does not exist."}, false
	}
	return nil, false
}

func observeClientTokenTopLevelMintRefusal(clientId server.Id, networkId server.Id, reason string, now time.Time) {
	networkClientTopLevelMintRefusals.Inc()
	if clientTokenTopLevelMintRefusalLogs.due(clientId, now) {
		glog.Infof("[authz]refused a mint without a source from the client token of client %s, network %s: %s (one line per client an hour)\n", clientId, networkId, reason)
	}
}

// Counts and logs the outcome of the old window's request once its mint has
// answered: a minted child, or the refusal of a device_spec that is not the
// caller's device's. Other answers, such as an inactive caller, are not this
// path's to count.
func observeLegacyWindowMintResult(
	clientId server.Id,
	networkId server.Id,
	authClientResult *AuthNetworkClientResult,
	authClientError error,
	now time.Time,
) {
	if authClientError != nil || authClientResult == nil {
		return
	}
	if authClientResult.Error == nil {
		if authClientResult.ClientId == nil {
			return
		}
		networkClientLegacyWindowMints.Inc()
		if clientTokenLegacyWindowMintLogs.due(clientId, now) {
			glog.Infof("[authz]minted an old app window's client as a child of client %s, network %s (one line per client an hour)\n", clientId, networkId)
		}
		return
	}
	if authClientResult.Error.Message == clientTokenTopLevelMintRefusalMessage {
		observeClientTokenTopLevelMintRefusal(clientId, networkId, "its device_spec is not the caller's device's", now)
	}
}

// The old window sends its own device's spec, so a request whose spec is not
// the caller's device's is not the old window's. Read inside the mint's
// transaction, on the device the source row just named.
func legacyWindowMintDeviceSpecMatchesInTx(ctx context.Context, tx server.PgTx, deviceId server.Id, deviceSpec string) bool {
	matches := false
	result, err := tx.Query(
		ctx,
		`
			SELECT device_spec
			FROM device
			WHERE device_id = $1
		`,
		deviceId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var storedDeviceSpec *string
			server.Raise(result.Scan(&storedDeviceSpec))
			matches = storedDeviceSpec != nil && isLegacyWindowDeviceSpecOfDevice(deviceSpec, *storedDeviceSpec)
		}
	})
	return matches
}

// Whether the old window's spec is that of the device whose row holds
// storedDeviceSpec: the same spec, or both the old Android form of one device
// on another os build. Any other pair, including one side in another form, is
// another device's.
func isLegacyWindowDeviceSpecOfDevice(deviceSpec string, storedDeviceSpec string) bool {
	if deviceSpec == "" {
		return false
	}
	if deviceSpec == storedDeviceSpec {
		return true
	}
	deviceIdentity, ok := legacyWindowAndroidDeviceIdentity(deviceSpec)
	if !ok {
		return false
	}
	storedDeviceIdentity, ok := legacyWindowAndroidDeviceIdentity(storedDeviceSpec)
	return ok && deviceIdentity == storedDeviceIdentity
}

// The part of an old Android window's spec, "<os version> <fingerprint>", that
// an os update leaves as it was. A fingerprint is
// "<brand>/<product>/<device>:<release>/<build id>/<incremental>:<type>/<tags>"
// and holds no whitespace (Android CDD 3.2.2), so the identity is
// "<brand>/<product>/<device>:<type>/<tags>". False for any other form.
func legacyWindowAndroidDeviceIdentity(deviceSpec string) (string, bool) {
	osVersion, fingerprint, found := strings.Cut(deviceSpec, " ")
	if !found || osVersion == "" || strings.ContainsAny(fingerprint, " \t\r\n") {
		return "", false
	}
	sections := strings.Split(fingerprint, ":")
	if len(sections) != 3 {
		return "", false
	}
	for i, wantPartCount := range []int{3, 3, 2} {
		parts := strings.Split(sections[i], "/")
		if len(parts) != wantPartCount {
			return "", false
		}
		for _, part := range parts {
			if part == "" {
				return "", false
			}
		}
	}
	return sections[0] + ":" + sections[2], true
}
