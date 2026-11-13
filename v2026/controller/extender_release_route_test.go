package controller

// The release route (connect/EXTENDER.md R3; connect/EXTENDER-UPDATE2.md
// §1.1, §3.2): its additive outcome and retry time beside the fields an older
// client reads, capability eligibility from the operator's cohorts through the
// records the route signs, and the block report's outage rule. Database
// backed; every address is a documentation one and every key synthetic.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Installs an `extender.yml` with a fresh root key and the given directory
// block lines, and returns the root key set clients verify with.
func installTestReleaseConfig(t testing.TB, directoryLines ...string) *connect.ExtenderRootKeySet {
	t.Helper()
	rootKeySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rootPublicKey, err := connect.ExtenderPublicKeyFromSeed(rootKeySeed)
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{
		fmt.Sprintf("root_private_key_hex: %s", connect.ExtenderKeySeedHex(rootKeySeed)),
		"root_public_keys_hex:",
		fmt.Sprintf("  - %s", hex.EncodeToString(rootPublicKey)),
		fmt.Sprintf("network_host: %s", testExtenderNetworkHost),
		fmt.Sprintf("api_url: https://%s", testExtenderApiHost),
	}
	if 0 < len(directoryLines) {
		lines = append(lines, "directory:")
		lines = append(lines, directoryLines...)
	}
	installTestExtenderConfigYaml(t, strings.Join(lines, "\n"))
	return connect.NewExtenderRootKeySet(rootPublicKey)
}

// The cohort block of one capability over the keys given.
func testReleaseCohortLines(capability string, keyHexes ...string) []string {
	lines := []string{
		"  capability_cohorts:",
		"    - capability: " + capability,
		"      extender_public_keys_hex:",
	}
	for _, keyHex := range keyHexes {
		lines = append(lines, "        - "+keyHex)
	}
	return lines
}

// One active gated extender with one active v4 address the operator's probe
// reached just now.
func createTestReleaseExtender(t testing.TB, ctx context.Context, index int) (string, server.Id) {
	t.Helper()
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	now := server.NowUtc()
	extenderId := server.NewId()
	model.Testing_CreateNetworkExtender(ctx, &model.NetworkExtender{
		ExtenderId:  extenderId,
		NetworkId:   server.NewId(),
		ClientId:    server.NewId(),
		PublicKey:   publicKey,
		CreateTime:  now,
		TcpPort:     443,
		UdpPort:     443,
		DnsPort:     53,
		DnsTld:      connect.DefaultExtenderDnsTld,
		CountryCode: "us",
		Active:      true,
	}, []*model.NetworkExtenderAddress{{
		IpVersion:            4,
		Ip:                   netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", 10+index)),
		Carriers:             []string{connect.ExtenderCarrierTcp},
		ActivateTime:         now,
		LastProbeTime:        &now,
		LastProbeSuccessTime: &now,
		Active:               true,
	}})
	if err := model.SetNetworkExtenderDirectoryTier(ctx, extenderId, connect.ExtenderDirectoryTierGated, false); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(publicKey), extenderId
}

// A client of a fresh network as the route sees it: new, or trusted, past its
// probation.
func newTestReleaseSession(t testing.TB, ctx context.Context, trusted bool) *session.ClientSession {
	t.Helper()
	networkId := server.NewId()
	clientId := server.NewId()
	model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic device", "synthetic spec")
	if trusted {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_client SET create_time = $2 WHERE client_id = $1`,
				clientId,
				server.NowUtc().Add(-60*24*time.Hour),
			))
		})
	}
	clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId: networkId,
		UserId:    server.NewId(),
		ClientId:  &clientId,
	})
	t.Cleanup(clientSession.Cancel)
	clientSession.ClientAddress = "192.0.2.80:4500"
	return clientSession
}

func setTestReleaseExtenderActive(t testing.TB, ctx context.Context, extenderId server.Id, active bool) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_extender SET active = $2 WHERE extender_id = $1`, extenderId, active))
	})
}

// The bodies of a release's records, verified under the root key set, by key.
func testReleaseBodies(t testing.TB, rootKeys *connect.ExtenderRootKeySet, result *ExtenderReleaseResult) map[string]*protocol.ExtenderRecordBody {
	t.Helper()
	bodies := map[string]*protocol.ExtenderRecordBody{}
	for _, recordBase64 := range result.Records {
		record, err := connect.DecodeExtenderRecordBase64(recordBase64)
		if err != nil {
			t.Fatal(err)
		}
		body, err := rootKeys.VerifyRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		bodies[hex.EncodeToString(body.PublicKey)] = body
	}
	return bodies
}

func testReleaseJsonKeys(t testing.TB, result *ExtenderReleaseResult) map[string]any {
	t.Helper()
	resultBytes, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]any{}
	if err := json.Unmarshal(resultBytes, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

// The outcome and retry time are additive: the fields an older client reads
// keep their meaning, a refusal keeps its message, and a retry time is in
// whole seconds and omitted when there is none.
func TestExtenderReleaseRouteAnswersOutcomesBesideTheOldFields(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rootKeys := installTestReleaseConfig(t)
		keyHex, extenderId := createTestReleaseExtender(t, ctx, 0)
		clientSession := newTestReleaseSession(t, ctx, false)

		released, err := ExtenderRelease(&ExtenderReleaseArgs{}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if released.Outcome != connect.ExtenderReleaseOutcomeOk || released.Error != "" || released.RetryAfterSeconds != 0 ||
			released.Count != 1 || !released.Probation || len(released.Records) != 1 {
			t.Fatalf("release = %+v", released)
		}
		body := testReleaseBodies(t, rootKeys, released)[keyHex]
		if body == nil || body.DirectoryTier != connect.ExtenderDirectoryTierGated || len(body.RequiredCapabilities) != 0 {
			t.Fatalf("released record = %v", body)
		}
		fields := testReleaseJsonKeys(t, released)
		if fields["outcome"] != "ok" {
			t.Fatalf("json = %v, want outcome ok", fields)
		}
		if _, ok := fields["retry_after_seconds"]; ok {
			t.Fatalf("json = %v carries a retry time with none known", fields)
		}

		// its one record leaves the fleet: the epoch is spent, and the answer
		// says when it ends, rounded up
		setTestReleaseExtenderActive(t, ctx, extenderId, false)
		before := server.NowUtc()
		spent, err := ExtenderRelease(&ExtenderReleaseArgs{}, clientSession)
		after := server.NowUtc()
		if err != nil {
			t.Fatal(err)
		}
		epochMs := connect.ExtenderReleaseEpochTimeout.Milliseconds()
		epochEnd := time.UnixMilli((before.UnixMilli()/epochMs + 1) * epochMs)
		if spent.Outcome != connect.ExtenderReleaseOutcomeProbation || len(spent.Records) != 0 || spent.Error != "" ||
			spent.RetryAfterSeconds < int(epochEnd.Sub(after)/time.Second) || int(epochEnd.Sub(before)/time.Second)+1 < spent.RetryAfterSeconds {
			t.Fatalf("spent = %+v, want probation retrying at the epoch's end %s", spent, epochEnd)
		}
		if fields := testReleaseJsonKeys(t, spent); fields["retry_after_seconds"] == nil {
			t.Fatalf("json = %v has no retry time", fields)
		}

		// the identity's request limit: its message, and a whole window
		var limited *ExtenderReleaseResult
		for range 2 * connect.ExtenderReleaseIdentityRequestLimit {
			result, err := ExtenderRelease(&ExtenderReleaseArgs{}, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome == connect.ExtenderReleaseOutcomeRateLimited {
				limited = result
				break
			}
		}
		if limited == nil || limited.Error != connect.ErrExtenderReleaseIdentityLimited.Error() ||
			limited.RetryAfterSeconds != int((connect.ExtenderReleaseRequestWindow+time.Second)/time.Second) || len(limited.Records) != 0 {
			t.Fatalf("limited = %+v", limited)
		}

		// requests the route cannot answer
		invalid, err := ExtenderRelease(&ExtenderReleaseArgs{IpVersions: []int{5}}, newTestReleaseSession(t, ctx, false))
		if err != nil || invalid.Outcome != connect.ExtenderReleaseOutcomeUnavailable || invalid.Error != "ip_versions must be 4 or 6" {
			t.Fatalf("invalid family = %+v, %v", invalid, err)
		}
		noClient := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: server.NewId(), UserId: server.NewId()})
		t.Cleanup(noClient.Cancel)
		if result, err := ExtenderRelease(&ExtenderReleaseArgs{}, noClient); err != nil || result.Outcome != connect.ExtenderReleaseOutcomeUnavailable {
			t.Fatalf("no client = %+v, %v", result, err)
		}
	})
}

// Root cause guarded (§3.2): a client that cannot use a Phase B extender must
// never be handed one. A cohort member's record requires the capability once
// it carries a camouflage key; a client that does not declare the capability
// is dealt the rest of its partition, or told it is incompatible when nothing
// else is left, which charges nothing. Without the key the cohort is inert,
// and a capability the server does not know is one no client satisfies.
func TestExtenderReleaseRouteHonorsCapabilityCohorts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cohortKeyHex, _ := createTestReleaseExtender(t, ctx, 0)
		otherKeyHex, otherId := createTestReleaseExtender(t, ctx, 1)
		rootKeys := installTestReleaseConfig(t, testReleaseCohortLines("tcp-camouflage-v1", strings.ToUpper(cohortKeyHex))...)
		realityKey := bytes.Repeat([]byte{0x5a}, extenderRealityPublicKeySize)
		previousRealityKey := extenderRecordRealityPublicKey
		extenderRecordRealityPublicKey = func(extender *model.NetworkExtender) []byte {
			return realityKey
		}
		t.Cleanup(func() { extenderRecordRealityPublicKey = previousRealityKey })
		metrics := newExtenderReleaseMetrics(prometheus.NewRegistry())
		release := func(clientSession *session.ClientSession, capabilities ...string) *ExtenderReleaseResult {
			t.Helper()
			result, err := extenderRelease(&ExtenderReleaseArgs{Capabilities: capabilities}, clientSession, metrics)
			if err != nil {
				t.Fatal(err)
			}
			return result
		}

		legacy := release(newTestReleaseSession(t, ctx, true))
		legacyBodies := testReleaseBodies(t, rootKeys, legacy)
		if legacy.Outcome != connect.ExtenderReleaseOutcomeOk || len(legacyBodies) != 1 || legacyBodies[otherKeyHex] == nil {
			t.Fatalf("legacy release = %+v (%d records), want the other extender alone", legacy, len(legacyBodies))
		}
		if required := legacyBodies[otherKeyHex].RequiredCapabilities; len(required) != 0 {
			t.Fatalf("a record outside the cohort requires %v", required)
		}

		capable := release(newTestReleaseSession(t, ctx, true), "tcp-camouflage-v1", "synthetic-unknown-capability")
		capableBodies := testReleaseBodies(t, rootKeys, capable)
		if capable.Outcome != connect.ExtenderReleaseOutcomeOk || len(capableBodies) != 2 {
			t.Fatalf("capable release = %+v (%d records), want both", capable, len(capableBodies))
		}
		cohortBody := capableBodies[cohortKeyHex]
		if cohortBody == nil || !slices.Equal(cohortBody.RequiredCapabilities, []string{"tcp-camouflage-v1"}) || !bytes.Equal(cohortBody.RealityPublicKey, realityKey) {
			t.Fatalf("the cohort record = %v, want it to require tcp-camouflage-v1 beside its key", cohortBody)
		}

		// with only the cohort member left, a legacy client is incompatible,
		// and that answer charged nothing: declaring the capability later in
		// the epoch is dealt the record
		setTestReleaseExtenderActive(t, ctx, otherId, false)
		newClient := newTestReleaseSession(t, ctx, false)
		incompatible := release(newClient)
		if incompatible.Outcome != connect.ExtenderReleaseOutcomeIncompatible || len(incompatible.Records) != 0 ||
			incompatible.Error != "" || incompatible.RetryAfterSeconds != 0 {
			t.Fatalf("incompatible = %+v", incompatible)
		}
		upgraded := release(newClient, "tcp-camouflage-v1")
		if upgraded.Outcome != connect.ExtenderReleaseOutcomeOk || testReleaseBodies(t, rootKeys, upgraded)[cohortKeyHex] == nil {
			t.Fatalf("after upgrading = %+v", upgraded)
		}
		if got := testutil.ToFloat64(metrics.releases.WithLabelValues("incompatible", "any", "false")); got != 1 {
			t.Fatalf("incompatible releases counted %v, want 1", got)
		}
		if got := testutil.ToFloat64(metrics.releases.WithLabelValues("ok", "any", "true")); got != 2 {
			t.Fatalf("capable ok releases counted %v, want 2", got)
		}

		// no camouflage key in the record: the cohort requires nothing yet
		extenderRecordRealityPublicKey = previousRealityKey
		inert := release(newTestReleaseSession(t, ctx, false))
		inertBody := testReleaseBodies(t, rootKeys, inert)[cohortKeyHex]
		if inert.Outcome != connect.ExtenderReleaseOutcomeOk || inertBody == nil || len(inertBody.RequiredCapabilities) != 0 {
			t.Fatalf("without a camouflage key = %+v, want the cohort record with no requirement", inert)
		}

		// a misspelled capability withholds the cohort from everyone
		installTestReleaseConfig(t, testReleaseCohortLines("tcp-camoflage-v1", cohortKeyHex)...)
		withheld := release(newTestReleaseSession(t, ctx, false), "tcp-camouflage-v1", "tcp-camoflage-v1")
		if withheld.Outcome != connect.ExtenderReleaseOutcomeIncompatible || len(withheld.Records) != 0 {
			t.Fatalf("a misspelled cohort = %+v, want incompatible", withheld)
		}
	})
}

// A block report is recorded only while the operator's probe reaches the
// extender; one sent during an outage is not, and the route says so.
func TestExtenderBlockReportRouteRecordsOnlyWhileProbed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestReleaseConfig(t)
		keyHex, extenderId := createTestReleaseExtender(t, ctx, 0)
		clientSession := newTestReleaseSession(t, ctx, false)
		report := func(publicKeyHex string) *ExtenderBlockReportResult {
			t.Helper()
			result, err := ExtenderBlockReport(&ExtenderBlockReportArgs{PublicKeyHex: publicKeyHex}, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			return result
		}
		if result := report(keyHex); !result.Recorded || result.Error != "" {
			t.Fatalf("a report against a probed extender = %+v", result)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_extender_address SET consecutive_probe_failures = 1 WHERE extender_id = $1`, extenderId))
		})
		if result := report(keyHex); result.Recorded || result.Error != "" {
			t.Fatalf("a report against a failing extender = %+v, want not recorded", result)
		}
		otherKey := make([]byte, ed25519.PublicKeySize)
		if result := report(hex.EncodeToString(otherKey)); result.Recorded || result.Error != "" {
			t.Fatalf("a report against an unknown key = %+v", result)
		}
	})
}

// The route counts the network of the authenticated client, whatever the request
// says: a body has no field for a network, and the client id comes from the verified
// token (connect/EXTENDER-UPDATE2.md §1.3). Three clients of one network are one
// reporter even with a request that claims otherwise, and three networks are three.
func TestExtenderBlockReportRouteCountsTheCallersNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestReleaseConfig(t)
		settings := connect.DefaultExtenderBlockedStateSettings()
		blocked := func(extenderId server.Id, countryCode string) bool {
			t.Helper()
			return model.NetworkExtenderBlockedInCountry(
				ctx,
				extenderId,
				countryCode,
				settings.ReportThreshold,
				settings.ReportWindow,
				settings.ProbeWindow,
				server.NowUtc(),
			)
		}
		report := func(keyHex string, networkId server.Id) {
			t.Helper()
			clientId := model.Testing_CreateBlockReportClient(ctx, networkId)
			clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
				NetworkId: networkId,
				UserId:    server.NewId(),
				ClientId:  &clientId,
			})
			t.Cleanup(clientSession.Cancel)
			clientSession.ClientAddress = "192.0.2.80:4500"
			// a claim of another network in the body goes nowhere
			var args ExtenderBlockReportArgs
			body := fmt.Sprintf(`{"public_key_hex":%q,"network_id":%q}`, keyHex, server.NewId())
			if err := json.Unmarshal([]byte(body), &args); err != nil {
				t.Fatal(err)
			}
			result, err := ExtenderBlockReport(&args, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Recorded || result.Error != "" {
				t.Fatalf("a report against a probed extender = %+v", result)
			}
		}
		countryCode := func() string {
			clientIp, err := netip.ParseAddr("192.0.2.80")
			if err != nil {
				t.Fatal(err)
			}
			if ipInfo, err := server.GetIpInfo(clientIp); err == nil {
				return ipInfo.CountryCode
			}
			return ""
		}()

		oneKeyHex, oneId := createTestReleaseExtender(t, ctx, 0)
		networkId := server.NewId()
		for range settings.ReportThreshold {
			report(oneKeyHex, networkId)
		}
		if blocked(oneId, countryCode) {
			t.Fatal("three clients of one network made a block through the route")
		}

		threeKeyHex, threeId := createTestReleaseExtender(t, ctx, 1)
		for range settings.ReportThreshold {
			report(threeKeyHex, server.NewId())
		}
		if !blocked(threeId, countryCode) {
			t.Fatal("three networks made no block through the route")
		}
	})
}
