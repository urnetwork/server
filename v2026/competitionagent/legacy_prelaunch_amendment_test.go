//go:build linux

// One-hop amendments replace only the recovery executable/service authority.
// Signed synthetic admission and original cleanup remain the composed path.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyPrelaunchSshdListKeysDoNotCollideWithRoutingAuthority(t *testing.T) {
	authorization := LegacyPrelaunchAuthorization{IngressWrapper: Pin{Path: "/synthetic/fenced-wrapper"}, AuthorizedKeys: Pin{Path: "/synthetic/authorized-keys"}}
	content := []byte("hostkey /synthetic/host-one\nhostkey /synthetic/host-two\nlistenaddress 192.0.2.1:22\nlistenaddress 192.0.2.2:22\nforcecommand /usr/bin/sudo -n -- /synthetic/fenced-wrapper\nauthorizedkeysfile /synthetic/authorized-keys\nauthorizedkeyscommand none\n")
	// This is the exact former parser predicate, not a scheduler-dependent
	// reproduction: the ordinary second hostkey deterministically rejected.
	old := map[string]string{}
	oldRejected := false
	for _, line := range strings.Split(strings.TrimSuffix(string(content), "\n"), "\n") {
		key, value, found := strings.Cut(line, " ")
		if !found || old[key] != "" {
			oldRejected = true
			break
		}
		old[key] = value
	}
	if !oldRejected {
		t.Fatal("regression fixture did not reproduce the original duplicate-list failure")
	}
	if err := validateLegacyPrelaunchSshdProjection(content, authorization, int64(len(content))); err != nil {
		t.Fatal("legitimate unrelated lists broke exact routing proof", err)
	}
	for _, fault := range []string{"duplicate-force", "duplicate-keys", "duplicate-command", "empty-first-force", "empty-first-keys", "empty-first-command", "missing-force", "missing-keys", "missing-command", "empty-force", "empty-keys", "empty-command", "wrong-force", "malformed", "bound"} {
		candidate := append([]byte(nil), content...)
		maximum := int64(len(candidate)) + 1024
		switch fault {
		case "duplicate-force":
			candidate = append(candidate, []byte("forcecommand /usr/bin/sudo -n -- /synthetic/fenced-wrapper\n")...)
		case "duplicate-keys":
			candidate = append(candidate, []byte("authorizedkeysfile /synthetic/authorized-keys\n")...)
		case "duplicate-command":
			candidate = append(candidate, []byte("authorizedkeyscommand none\n")...)
		case "empty-first-force":
			candidate = append([]byte("forcecommand \n"), candidate...)
		case "empty-first-keys":
			candidate = append([]byte("authorizedkeysfile \n"), candidate...)
		case "empty-first-command":
			candidate = append([]byte("authorizedkeyscommand \n"), candidate...)
		case "missing-force":
			candidate = []byte(strings.ReplaceAll(string(candidate), "forcecommand /usr/bin/sudo -n -- /synthetic/fenced-wrapper\n", ""))
		case "missing-keys":
			candidate = []byte(strings.ReplaceAll(string(candidate), "authorizedkeysfile /synthetic/authorized-keys\n", ""))
		case "missing-command":
			candidate = []byte(strings.ReplaceAll(string(candidate), "authorizedkeyscommand none\n", ""))
		case "empty-force":
			candidate = []byte(strings.ReplaceAll(string(candidate), "forcecommand /usr/bin/sudo -n -- /synthetic/fenced-wrapper\n", "forcecommand \n"))
		case "empty-keys":
			candidate = []byte(strings.ReplaceAll(string(candidate), "authorizedkeysfile /synthetic/authorized-keys\n", "authorizedkeysfile \n"))
		case "empty-command":
			candidate = []byte(strings.ReplaceAll(string(candidate), "authorizedkeyscommand none\n", "authorizedkeyscommand \n"))
		case "wrong-force":
			candidate = []byte(strings.ReplaceAll(string(candidate), "-- /synthetic/fenced-wrapper", "-- /synthetic/foreign-wrapper"))
		case "malformed":
			candidate = append(candidate, []byte("malformed\n")...)
		case "bound":
			maximum = int64(len(candidate)) - 1
		}
		if validateLegacyPrelaunchSshdProjection(candidate, authorization, maximum) == nil {
			t.Fatal("routing authority or bounded output was weakened", fault)
		}
	}
}

func TestLegacyPrelaunchOneHopReauthenticatesPriorAndStartingAfterCleanup(t *testing.T) {
	for _, fault := range []string{"prior", "starting"} {
		fixture, agent, authorization, path, digest, startedPath, started, prior := amendmentFixture(t)
		clock := recoveryClockBytes(t, fixture)
		fixture.cleaner.before = func(context.Context, cleanupPlan) error {
			mutationPath, content := authorization.PriorAuthorization.Path, prior
			if fault == "starting" {
				mutationPath, content = startedPath, started
			}
			writeRecoveryTestAuthority(t, fixture, mutationPath, append(append([]byte(nil), content...), '\n'))
			return nil
		}
		if agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, &legacyPrelaunchTestHost{}) == nil {
			t.Fatal("changed prior/starting proof published a closure", fault)
		}
		state, err := agent.loadState(authorization.RunId)
		if err != nil || state.State != "accepted" || !state.RecoveryActive || !state.CleanupVerified || !state.StopCleanupVerified || state.ExecutionStarted || fixture.cleaner.count() != 1 || fixture.runner.launchCount() != 1 || !bytes.Equal(clock, recoveryClockBytes(t, fixture)) {
			t.Fatal("post-cleanup authority mutation released custody or changed the clock", fault, err)
		}
		if _, err := os.Stat(filepath.Join(agent.stateDirectory(authorization.RunId), "legacy-prelaunch-closed.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("post-cleanup mutation retained a false closure", fault, err)
		}
	}
}

func TestLegacyPrelaunchNativeIngressReauthenticatesPriorBeforeObservation(t *testing.T) {
	fixture, agent, authorization, path, digest, _, _, prior := amendmentFixture(t)
	writeRecoveryTestAuthority(t, fixture, authorization.PriorAuthorization.Path, append(prior, '\n'))
	host := &legacyPrelaunchNativeHost{agent: agent, authorization: authorization, authorizationPath: path, digest: digest}
	if err := host.ingress(t.Context()); err == nil || !strings.Contains(err.Error(), "prior authority") {
		t.Fatal("native ingress reached executable/SSH proof after prior authority changed", err)
	}
}

func amendmentFixture(t *testing.T) (*agentTestFixture, *Agent, LegacyPrelaunchAuthorization, string, string, string, []byte, []byte) {
	t.Helper()
	fixture, agent, prior, priorPath, priorDigest := newLegacyPrelaunchTestFixture(t)
	if agent.closeLegacyPrelaunch(t.Context(), priorPath, prior, priorDigest, &legacyPrelaunchTestHost{closeErr: errors.New("synthetic original ingress parser failure")}) == nil {
		t.Fatal("starting boundary was not retained")
	}
	startedPath := filepath.Join(agent.stateDirectory(prior.RunId), "legacy-prelaunch-started.json")
	started, err := os.ReadFile(startedPath)
	if err != nil {
		t.Fatal(err)
	}
	oldBytes, err := os.ReadFile(priorPath)
	if err != nil {
		t.Fatal(err)
	}
	amended := prior
	amended.RecoveryAgent = Pin{Path: filepath.Join(fixture.config.BaseDirectory, "new-recovery"), Sha256: strings.Repeat("e", 64)}
	amended.RecoveryService.Sha256 = strings.Repeat("f", 64)
	amended.PriorAuthorization = &Pin{Path: priorPath, Sha256: priorDigest}
	amended.RetainedStartedSha256 = digestDockerBytes(started)
	path := filepath.Join(fixture.config.BaseDirectory, "amended-authorization.json")
	content, err := json.Marshal(amended)
	if err != nil {
		t.Fatal(err)
	}
	writeRecoveryTestAuthority(t, fixture, path, content)
	return fixture, agent, amended, path, digestDockerBytes(content), startedPath, started, oldBytes
}

func TestLegacyPrelaunchOneHopAmendmentResumesExactRetainedAuthority(t *testing.T) {
	fixture, agent, authorization, path, digest, startedPath, started, prior := amendmentFixture(t)
	clock := recoveryClockBytes(t, fixture)
	if err := agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, &legacyPrelaunchTestHost{}); err != nil {
		t.Fatal("explicit replacement could not resume original cleanup", err)
	}
	after, err := os.ReadFile(startedPath)
	if err != nil || !bytes.Equal(after, started) {
		t.Fatal("original starting receipt was rewritten", err)
	}
	oldAuth, err := os.ReadFile(authorization.PriorAuthorization.Path)
	if err != nil || !bytes.Equal(oldAuth, prior) {
		t.Fatal("original authorization was rewritten", err)
	}
	closedBytes, err := os.ReadFile(filepath.Join(agent.stateDirectory(authorization.RunId), "legacy-prelaunch-closed.json"))
	closed := legacyPrelaunchClosed{}
	if err != nil || decodeStrict(closedBytes, &closed) != nil || closed.AuthorizationSha256 != digest || closed.PriorAuthorizationSha256 != authorization.PriorAuthorization.Sha256 || closed.RetainedStartedSha256 != digestDockerBytes(started) {
		t.Fatal("closure omitted explicit new/prior/starting provenance", err)
	}
	state, err := agent.loadState(authorization.RunId)
	if err != nil || state.State != "failed" || state.RecoveryActive || state.ExecutionStarted || !state.LaunchAttempted || !state.CleanupVerified || !state.StopCleanupVerified || fixture.runner.launchCount() != 1 || fixture.cleaner.count() != 1 || !bytes.Equal(clock, recoveryClockBytes(t, fixture)) {
		t.Fatal("one-hop resume changed execution/clock/cleanup authority", err)
	}
}

func TestLegacyPrelaunchOneHopRejectsMutatedAuthorityMissingStartingAndChains(t *testing.T) {
	for _, fault := range []string{"clock", "run", "state", "boot", "approval", "wrapper", "prior-roles", "starting-pin", "missing-starting", "prior-pin", "chain", "missing-prior"} {
		fixture, agent, authorization, path, _, startedPath, _, _ := amendmentFixture(t)
		switch fault {
		case "clock":
			authorization.ClockSha256 = strings.Repeat("f", 64)
		case "run":
			authorization.RunId = strings.Repeat("f", 64)
		case "state":
			authorization.StateSha256 = strings.Repeat("f", 64)
		case "boot":
			authorization.BootId = "44444444-4444-4444-8444-444444444444"
		case "approval":
			authorization.ApprovalSha256 = strings.Repeat("f", 64)
		case "wrapper":
			authorization.IngressWrapper.Sha256 = strings.Repeat("f", 64)
		case "prior-roles":
			authorization.PriorControlRoles = []LegacyPrelaunchControlRole{{Agent: authorization.RecoveryAgent, Config: authorization.OriginalConfig}}
		case "starting-pin":
			authorization.RetainedStartedSha256 = strings.Repeat("a", 64)
		case "missing-starting":
			if err := os.Rename(startedPath, startedPath+".retained-not-selected"); err != nil {
				t.Fatal(err)
			}
		case "prior-pin":
			authorization.PriorAuthorization.Sha256 = strings.Repeat("f", 64)
		case "missing-prior":
			authorization.PriorAuthorization = nil
		case "chain":
			priorBytes, err := os.ReadFile(authorization.PriorAuthorization.Path)
			if err != nil {
				t.Fatal(err)
			}
			prior := LegacyPrelaunchAuthorization{}
			if decodeStrict(priorBytes, &prior) != nil {
				t.Fatal("bad synthetic prior")
			}
			prior.PriorAuthorization = &Pin{Path: path, Sha256: strings.Repeat("c", 64)}
			prior.RetainedStartedSha256 = strings.Repeat("d", 64)
			changed, err := json.Marshal(prior)
			if err != nil {
				t.Fatal(err)
			}
			writeRecoveryTestAuthority(t, fixture, authorization.PriorAuthorization.Path, changed)
			authorization.PriorAuthorization.Sha256 = digestDockerBytes(changed)
		}
		content, err := json.Marshal(authorization)
		if err != nil {
			t.Fatal(err)
		}
		writeRecoveryTestAuthority(t, fixture, path, content)
		if agent.closeLegacyPrelaunch(t.Context(), path, authorization, digestDockerBytes(content), &legacyPrelaunchTestHost{}) == nil || fixture.cleaner.count() != 0 || fixture.runner.launchCount() != 1 {
			t.Fatal("amendment widened or fabricated original custody", fault)
		}
	}
}

func TestLegacyPrelaunchOptionalAmendmentKeepsHistoricalCanonicalBytes(t *testing.T) {
	_, _, authorization, _, _ := newLegacyPrelaunchTestFixture(t)
	// Reconstruct the historical type by its original explicit field order,
	// excluding only the two new optional fields. JSON must remain byte-exact.
	names := []string{"Schema", "Kind", "ApprovalSha256", "HostId", "RunId", "StateSha256", "ManifestSha256", "CleanupPlanSha256", "ClockSha256", "BootId", "OriginalConfig", "OriginalRecoveryServiceArchive", "RecoveryAgent", "RecoveryService", "IngressWrapper", "OriginalIngressWrapper", "OriginalIngressWrapperArchive", "AuthorizedKeys", "Sshd", "ControlUser", "OwnerStartDenial", "PriorControlRoles"}
	typeOf := reflect.TypeOf(authorization)
	fields := []reflect.StructField{}
	for _, name := range names {
		field, ok := typeOf.FieldByName(name)
		if !ok {
			t.Fatal("historical field missing", name)
		}
		fields = append(fields, field)
	}
	historical := reflect.New(reflect.StructOf(fields)).Elem()
	current := reflect.ValueOf(authorization)
	for index, name := range names {
		historical.Field(index).Set(current.FieldByName(name))
	}
	oldBytes, err := json.Marshal(historical.Interface())
	if err != nil {
		t.Fatal(err)
	}
	newBytes, err := json.Marshal(authorization)
	if err != nil || !bytes.Equal(oldBytes, newBytes) || bytes.Contains(newBytes, []byte("prior_authorization")) || bytes.Contains(newBytes, []byte("retained_started_sha256")) {
		t.Fatal("optional amendment changed historical authorization canonical bytes", err)
	}
	oldClosed := legacyPrelaunchClosed{Schema: 1, Kind: "synthetic-closed"}
	encoded, err := json.Marshal(oldClosed)
	if err != nil || bytes.Contains(encoded, []byte("prior_authorization_sha256")) || bytes.Contains(encoded, []byte("retained_started_sha256")) {
		t.Fatal("new closed provenance changed ordinary canonical fields", err)
	}
}
